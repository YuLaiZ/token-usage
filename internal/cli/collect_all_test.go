package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// callProbeCollector 记录自身被调用次数，可选注入 error。
type callProbeCollector struct {
	name      string
	err       error
	calls     int
	onCollect func()
}

func (c *callProbeCollector) Name() string          { return c.name }
func (c *callProbeCollector) SyncSources() []string { return nil }
func (c *callProbeCollector) Collect(ctx context.Context, req collector.CollectRequest, log *slog.Logger) (collector.CollectResult, error) {
	c.calls++
	if c.onCollect != nil {
		c.onCollect()
	}
	return collector.CollectResult{}, c.err
}

// callProbeRouter 记录自身被调用次数，可选注入 error。
type callProbeRouter struct {
	name  string
	err   error
	calls int
}

func (r *callProbeRouter) Name() string { return r.name }
func (r *callProbeRouter) Capabilities() collector.RouterCapabilities {
	return collector.RouterCapabilities{}
}
func (r *callProbeRouter) SyncSource() string { return "probe_router_source_" + r.name }
func (r *callProbeRouter) CollectLogs(ctx context.Context, req collector.RouterCollectRequest, log *slog.Logger) (collector.RouterCollectResult, error) {
	r.calls++
	return collector.RouterCollectResult{}, r.err
}

// orderedProbe 给每次调用打一个全局序号（共享 *int），用于阶段顺序断言。
type orderedProbe struct {
	name  string
	err   error
	mu    *sync.Mutex
	seq   *[]int
	myTag int // 写入 seq 的标识
}

func newOrderedProbe(name string, err error, mu *sync.Mutex, seq *[]int, myTag int) *orderedProbe {
	return &orderedProbe{name: name, err: err, mu: mu, seq: seq, myTag: myTag}
}

// 实现 collector.Collector
func (o *orderedProbe) Name() string          { return o.name }
func (o *orderedProbe) SyncSources() []string { return nil }
func (o *orderedProbe) Collect(ctx context.Context, req collector.CollectRequest, log *slog.Logger) (collector.CollectResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	*o.seq = append(*o.seq, o.myTag)
	return collector.CollectResult{}, o.err
}

// orderedRouterAdapter 实现 collector.RouterAdapter，把调用顺序写入共享 seq。
type orderedRouterAdapter struct {
	name  string
	err   error
	mu    *sync.Mutex
	seq   *[]int
	myTag int
}

func (o *orderedRouterAdapter) Name() string { return o.name }
func (o *orderedRouterAdapter) Capabilities() collector.RouterCapabilities {
	return collector.RouterCapabilities{}
}
func (o *orderedRouterAdapter) SyncSource() string { return "ordered_router_" + o.name }
func (o *orderedRouterAdapter) CollectLogs(ctx context.Context, req collector.RouterCollectRequest, log *slog.Logger) (collector.RouterCollectResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	*o.seq = append(*o.seq, o.myTag)
	return collector.RouterCollectResult{}, o.err
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestCollectAll_MessagesBeforeRouter 阶段 A（messages）全部完成后才进入阶段 B（router）。
// 用 seq 记录调用顺序：messages 调用序号都应小于 router 调用序号。
func TestCollectAll_MessagesBeforeRouter(t *testing.T) {
	var mu sync.Mutex
	var seq []int
	// tags: 1=claude messages, 2=opencode messages, 3=claude router, 4=opencode router
	fc1 := newOrderedProbe("claude", nil, &mu, &seq, 1)
	fc2 := newOrderedProbe("opencode", nil, &mu, &seq, 2)
	ra1 := &orderedRouterAdapter{name: "cc_switch", mu: &mu, seq: &seq, myTag: 3}
	ra2 := &orderedRouterAdapter{name: "cc_switch_b", mu: &mu, seq: &seq, myTag: 4}

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true, Router: "cc_switch"},
			"opencode": {Enabled: true, Router: "cc_switch_b"},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch":   {DBPath: "ignored"},
			"cc_switch_b": {DBPath: "ignored"},
		},
	}
	deps := engine.NewDepsWithCollectors(cfg,
		[]collector.Collector{fc1, fc2},
		map[string]collector.RouterAdapter{"cc_switch": ra1, "cc_switch_b": ra2},
	)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude", "opencode"})
	if err != nil {
		t.Fatalf("runCollectAll: %v", err)
	}
	if len(seq) != 4 {
		t.Fatalf("期望 4 次调用，实际 %d: %v", len(seq), seq)
	}
	// 期望顺序: 1,2 (messages) 然后 3,4 (router)
	if seq[0] != 1 || seq[1] != 2 || seq[2] != 3 || seq[3] != 4 {
		t.Errorf("messages 阶段未在 router 之前全部完成: %v", seq)
	}
}

// TestCollectAll_SingleClientOnly --client 限定时只调用指定 client。
func TestCollectAll_SingleClientOnly(t *testing.T) {
	fc1 := &callProbeCollector{name: "claude"}
	fc2 := &callProbeCollector{name: "opencode"}
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true},
			"opencode": {Enabled: true},
		},
	}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fc1, fc2}, nil)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude"})
	if err != nil {
		t.Fatalf("runCollectAll: %v", err)
	}
	if fc1.calls != 1 {
		t.Errorf("claude 期望调用 1 次，实际 %d", fc1.calls)
	}
	if fc2.calls != 0 {
		t.Errorf("opencode 不应被调用，实际 %d", fc2.calls)
	}
}

func TestCollectAll_ContextCancellationStopsRemainingClients(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := &callProbeCollector{name: "claude", err: context.Canceled, onCollect: cancel}
	second := &callProbeCollector{name: "opencode"}
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude":   {Enabled: true},
		"opencode": {Enabled: true},
	}}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{first, second}, nil)

	err := runCollectAll(ctx, deps, cfg, openTestDB(t), slog.Default(), io.Discard, []string{"claude", "opencode"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后应返回 context.Canceled，got %v", err)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Errorf("取消后不应继续后续 client，calls=%d/%d", first.calls, second.calls)
	}
}

// TestCollectAll_MessagesFailureDoesNotSkipRouter 某 client 的 messages 阶段失败时，
// 其 router 阶段仍被尝试（数据库无 messages 时回填 0 条）。
func TestCollectAll_MessagesFailureDoesNotSkipRouter(t *testing.T) {
	fcFail := &callProbeCollector{name: "claude", err: fmt.Errorf("messages 失败")}
	ra := &callProbeRouter{name: "cc_switch"}
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "x"}},
	}
	deps := engine.NewDepsWithCollectors(cfg,
		[]collector.Collector{fcFail},
		map[string]collector.RouterAdapter{"cc_switch": ra},
	)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude"})
	if err == nil {
		t.Fatal("期望 runCollectAll 返回 error（messages 失败），实际 nil")
	}
	if ra.calls != 1 {
		t.Errorf("messages 失败时 router 仍应被调用 1 次，实际 %d", ra.calls)
	}
}

// TestCollectAll_NoRouterSkipsBackfill client 未配 router 时阶段 B 跳过（不算错误）。
func TestCollectAll_NoRouterSkipsBackfill(t *testing.T) {
	fc := &callProbeCollector{name: "claude"}
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fc}, nil)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude"})
	if err != nil {
		t.Fatalf("无 router 不应报错，实际: %v", err)
	}
}

// TestCollectAll_MultiClientPartialFailContinues 一个 client messages 失败不阻断其他。
// 最终错误应包含所有失败来源（messages + router）。
func TestCollectAll_MultiClientPartialFailContinues(t *testing.T) {
	fcFail := &callProbeCollector{name: "claude", err: fmt.Errorf("claude messages 失败")}
	fcOK := &callProbeCollector{name: "opencode"}
	raClaudeFail := &callProbeRouter{name: "cc_switch_a", err: fmt.Errorf("claude router 失败")}
	raOpencodeFail := &callProbeRouter{name: "cc_switch_b", err: fmt.Errorf("opencode router 失败")}
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true, Router: "cc_switch_a"},
			"opencode": {Enabled: true, Router: "cc_switch_b"},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch_a": {DBPath: "x"},
			"cc_switch_b": {DBPath: "x"},
		},
	}
	deps := engine.NewDepsWithCollectors(cfg,
		[]collector.Collector{fcFail, fcOK},
		map[string]collector.RouterAdapter{"cc_switch_a": raClaudeFail, "cc_switch_b": raOpencodeFail},
	)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude", "opencode"})
	if err == nil {
		t.Fatal("期望返回 error（多处失败），实际 nil")
	}
	msg := err.Error()
	for _, want := range []string{"claude", "opencode", "messages", "router"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error 应含 %q，实际 %q", want, msg)
		}
	}
	if fcOK.calls != 1 {
		t.Errorf("opencode messages 应调用 1 次，实际 %d", fcOK.calls)
	}
	if raClaudeFail.calls != 1 {
		t.Errorf("claude router 应调用 1 次，实际 %d", raClaudeFail.calls)
	}
}

// TestCollectAll_EmptyClients 非零退出。
func TestCollectAll_EmptyClients(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{}}
	deps := engine.NewDepsWithCollectors(cfg, nil, nil)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{})
	if err == nil {
		t.Fatal("期望空 client 列表返回 error")
	}
	if !strings.Contains(err.Error(), "没有已启用的客户端") {
		t.Errorf("error 应含 '没有已启用的客户端'，实际 %q", err.Error())
	}
}

// TestCollectAll_ErrorOrderStable 多 client 失败时，错误按 client+stage 稳定排序。
func TestCollectAll_ErrorOrderStable(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true},
			"opencode": {Enabled: true},
			"codex":    {Enabled: true},
		},
	}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{
		&callProbeCollector{name: "claude", err: fmt.Errorf("e")},
		&callProbeCollector{name: "codex", err: fmt.Errorf("e")},
		&callProbeCollector{name: "opencode", err: fmt.Errorf("e")},
	}, nil)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard,
		[]string{"claude", "codex", "opencode"})
	if err == nil {
		t.Fatal("期望返回 error")
	}
	msg := err.Error()
	idxClaude := strings.Index(msg, "claude")
	idxCodex := strings.Index(msg, "codex")
	idxOpencode := strings.Index(msg, "opencode")
	if !(idxClaude < idxCodex && idxCodex < idxOpencode) {
		t.Errorf("错误顺序非字典序稳定: claude=%d codex=%d opencode=%d\n%s",
			idxClaude, idxCodex, idxOpencode, msg)
	}
}

// TestCollectAll_ConfiguredRouterAdapterMissingFailsRouterStage 覆盖旧盲区：
// client 在配置层声明了 router（cc.Router != ""）但 adapter 装配失败
// （deps.RouterFor 返回 nil，例如 db_path 无效导致 NewDeps 未注册 adapter）。
// 修复前：clientHasRouter 用 deps.RouterFor 判定 → 静默跳过 router 阶段，用户看不到错误。
// 修复后：clientHasRouter 用配置层判定 → 进入阶段 B，RunRouterBackfill 返回 error，
//
//	计入 router 阶段失败汇总；messages 阶段不受影响（成功）。
func TestCollectAll_ConfiguredRouterAdapterMissingFailsRouterStage(t *testing.T) {
	fc := &callProbeCollector{name: "claude"}
	// 关键：cfg.Clients["claude"].Router 非空，但 NewDepsWithCollectors 不传对应 adapter
	// （模拟生产中 db_path 无效导致 NewDeps 未注册 adapter 的情形）。
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: ""}},
	}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fc}, nil) // 无 router adapter
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(), io.Discard, []string{"claude"})
	if err == nil {
		t.Fatal("期望 router 阶段失败汇总返回 error（adapter 未装配），实际 nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude/router") {
		t.Errorf("error 应含 'claude/router' 阶段失败，实际 %q", msg)
	}
	if !strings.Contains(msg, "未配置 router") {
		t.Errorf("error 应含 RunRouterBackfill 的 '未配置 router' 文案，实际 %q", msg)
	}
	if fc.calls != 1 {
		t.Errorf("messages 阶段应正常执行 1 次，实际 %d", fc.calls)
	}
}

// TestCollectAll_ConfiguredRouterBackfillErrorAggregates 覆盖另一种 adapter 装配成功
// 但 backfill 失败的情形：阶段 B 调用 RunRouterBackfill 返回 error，
// 计入 router 阶段失败；messages 阶段成功不受影响；多 client 时二者独立汇总。
func TestCollectAll_ConfiguredRouterBackfillErrorAggregates(t *testing.T) {
	fcClaude := &callProbeCollector{name: "claude"}
	fcOpencode := &callProbeCollector{name: "opencode"}
	raFail := &callProbeRouter{name: "cc_switch", err: fmt.Errorf("router backfill 失败")}
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true, Router: "cc_switch"},
			"opencode": {Enabled: true}, // 未配 router，应跳过且不算错误
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "x"}},
	}
	deps := engine.NewDepsWithCollectors(cfg,
		[]collector.Collector{fcClaude, fcOpencode},
		map[string]collector.RouterAdapter{"cc_switch": raFail},
	)
	usageDB := openTestDB(t)

	err := runCollectAll(context.Background(), deps, cfg, usageDB, slog.Default(),
		io.Discard, []string{"claude", "opencode"})
	if err == nil {
		t.Fatal("期望 router 阶段失败汇总返回 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude/router") {
		t.Errorf("error 应含 'claude/router' 阶段失败，实际 %q", msg)
	}
	if strings.Contains(msg, "opencode/router") {
		t.Errorf("opencode 未配 router 不应出现在 router 阶段失败，实际 %q", msg)
	}
	if raFail.calls != 1 {
		t.Errorf("claude router 应被调用 1 次，实际 %d", raFail.calls)
	}
	if fcClaude.calls != 1 || fcOpencode.calls != 1 {
		t.Errorf("两个 client 的 messages 阶段都应正常执行，claude=%d opencode=%d",
			fcClaude.calls, fcOpencode.calls)
	}
}
