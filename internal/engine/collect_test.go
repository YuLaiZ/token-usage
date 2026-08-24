package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// fixedCollector 固定结构：每次 Collect 返回预置 result，递增 calls。
type fixedCollector struct {
	name   string
	result collector.CollectResult
	err    error
	calls  int
}

func (c *fixedCollector) Name() string { return c.name }
func (c *fixedCollector) SyncSources() []string {
	return []string{"test_source"}
}
func (c *fixedCollector) Collect(_ context.Context, _ collector.CollectRequest, _ *slog.Logger) (collector.CollectResult, error) {
	c.calls++
	return c.result, c.err
}

// msg 是构造 model.Message 的便捷 helper（减少测试样板）。
func msg(id, client, date string) model.Message {
	return model.Message{ID: id, Client: client, Date: date}
}

func collectTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// levelCaptureHandler 收集全部日志记录及其级别，供断言日志级别行为。
type levelCaptureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *levelCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *levelCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *levelCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *levelCaptureHandler) WithGroup(name string) slog.Handler       { return h }

// hasRecordAt 断言指定 msg 是否以恰好该级别记录过。
func (h *levelCaptureHandler) hasRecordAt(level slog.Level, message string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && r.Message == message {
			return true
		}
	}
	return false
}

func testDeps(enabled bool, collectors ...collector.Collector) *Deps {
	clients := make(map[string]config.Client, len(collectors))
	for _, c := range collectors {
		clients[c.Name()] = config.Client{Enabled: enabled}
	}
	return &Deps{cfg: &config.Config{Clients: clients}, collectors: collectors}
}

// fixedResultCollector 返回固定 result 的 collector，用一次性构造减少样板。
func fixedResultCollector(name string, result collector.CollectResult) *fixedCollector {
	return &fixedCollector{name: name, result: result}
}

func TestRunCollect_FailurePersistsOneErrorPerDate(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := &fixedCollector{name: "claude", err: errors.New("boom")}
	dates := []string{"2026-06-22", "2026-06-23"}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: dates}, true, false)
	if result.Complete() || result.Err == nil {
		t.Fatalf("collector failure must fail: %+v", result)
	}
	got, _ := db.GetErrors(usageDB, db.ErrorFilter{Dates: dates, Source: "claude", Unresolved: true})
	if len(got) != 2 {
		t.Fatalf("errors = %+v", got)
	}
}

func TestRunCollect_NormalSuccessResolvesHistoricalErrors(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordErrorsByDate(context.Background(), usageDB, []string{"2026-06-23"}, "claude", "old failure", "")
	c := &fixedCollector{name: "claude"} // 空数据仍是一次完整成功扫描
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 0 {
		t.Fatalf("stale errors not resolved: %+v", remaining)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT session_count FROM collection_log
		WHERE date = '2026-06-23' AND source = 'claude'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("zero-data success must be marked collected: count=%d err=%v", count, err)
	}
}

func TestRunCollect_MultiDateUsesPerDateMessageCount(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{
			msg("m1", model.ClientClaudeCode, "2026-06-22"),
			msg("m2", model.ClientClaudeCode, "2026-06-22"),
			msg("m3", model.ClientClaudeCode, "2026-06-23"),
		},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-22", "2026-06-23"}}, true, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	for date, want := range map[string]int{"2026-06-22": 2, "2026-06-23": 1} {
		var got int
		if err := usageDB.QueryRow(`SELECT session_count FROM collection_log WHERE date=? AND source='claude'`, date).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count=%d want=%d err=%v", date, got, want, err)
		}
	}
}

func TestRunCollect_AllDisabledIsNotComplete(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	result := RunCollect(context.Background(), testDeps(false, &fixedCollector{name: "claude"}),
		usageDB, collectTestLogger(), io.Discard, "",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if result.Attempted != 0 || result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if err := ValidateResult("", result); err == nil {
		t.Fatal("all-disabled collect must return non-zero result")
	}
}

func TestRunCollect_CancelledContextDoesNotMarkOrResolve(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordErrorsByDate(context.Background(), usageDB, []string{"2026-06-23"}, "claude", "old failure", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("partial", model.ClientClaudeCode, "2026-06-23")},
	})
	result := RunCollect(ctx, testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if result.Complete() || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("cancelled result = %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 1 {
		t.Fatalf("cancelled collect resolved history: %+v", remaining)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled collect marked completion: count=%d err=%v", count, err)
	}
}

func TestRunCollect_MarkCollectedFailureIsNotComplete(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON collection_log
		BEGIN SELECT RAISE(ABORT, 'forced mark failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := RunCollect(context.Background(),
		testDeps(true, &fixedCollector{name: "claude"}), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if result.Complete() || result.Err == nil {
		t.Fatalf("collection_log failure must fail: %+v", result)
	}
}

func TestRunCollect_ResolveFailureIsNotComplete(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordError(context.Background(), usageDB, "2026-06-23", "claude", "old", "")
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_resolve BEFORE UPDATE OF resolved ON collection_errors
		WHEN NEW.resolved = 1
		BEGIN SELECT RAISE(ABORT, 'forced resolve failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := RunCollect(context.Background(),
		testDeps(true, &fixedCollector{name: "claude"}), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if result.Complete() || result.Err == nil {
		t.Fatalf("resolve failure must fail: %+v", result)
	}
}

func TestRunCollect_OneCollectorFailureMakesAggregateIncomplete(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	failed := &fixedCollector{name: "claude", err: errors.New("claude failed")}
	succeeded := &fixedCollector{name: "codex"}
	result := RunCollect(context.Background(), testDeps(true, failed, succeeded), usageDB,
		collectTestLogger(), io.Discard, "",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if result.Complete() || result.Attempted != 2 || result.Succeeded != 1 ||
		result.Err == nil || !strings.Contains(result.Err.Error(), "claude failed") {
		t.Fatalf("partial success must fail aggregate: %+v", result)
	}
}

// TestRunCollect_HeartbeatLoggedAtDebug：每次采集必打的「开始采集」/「采集完成」
// 是预期心跳，多触发源叠加下单日可达万级，必须以 Debug 而非 Info 记录。
func TestRunCollect_HeartbeatLoggedAtDebug(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	handler := &levelCaptureHandler{}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelDebug, "collection started") {
		t.Error("开始采集 必须以 Debug 记录")
	}
	if handler.hasRecordAt(slog.LevelInfo, "collection started") {
		t.Error("开始采集 不得以 Info 记录")
	}
	if !handler.hasRecordAt(slog.LevelDebug, "collection completed") {
		t.Error("采集完成 必须以 Debug 记录")
	}
	if handler.hasRecordAt(slog.LevelInfo, "collection completed") {
		t.Error("采集完成 不得以 Info 记录")
	}
}

// TestRunCollect_RouterHeartbeatLoggedAtDebug：router 专用路径的「router 采集完成」
// 与 client 路径心跳同级语义，必须以 Debug 记录。
func TestRunCollect_RouterHeartbeatLoggedAtDebug(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{{
			RequestID: "r1", MessageID: "m1", RouterName: "cc_switch", AppType: "claude",
		}},
	}
	deps := &Deps{
		cfg:     &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	handler := &levelCaptureHandler{}
	result := RunCollect(context.Background(), deps, usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelDebug, "router collection completed") {
		t.Error("router 采集完成 必须以 Debug 记录")
	}
	if handler.hasRecordAt(slog.LevelInfo, "router collection completed") {
		t.Error("router 采集完成 不得以 Info 记录")
	}
}

// TestRunCollect_SkipCollectedStaysInfo（反向断言）：「已采集，跳过」仅 CLI 手工
// collect 以 skipCollected 触发（daemon 恒为 false），频次天然低，保持 Info 不降级。
func TestRunCollect_SkipCollectedStaysInfo(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := db.MarkCollected(context.Background(), usageDB, "2026-07-01", "claude", 0); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{})
	handler := &levelCaptureHandler{}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-01"}}, false, true /*skipCollected*/)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelInfo, "already collected, skipping") {
		t.Error("已采集，跳过 必须保持 Info")
	}
	if handler.hasRecordAt(slog.LevelDebug, "already collected, skipping") {
		t.Error("已采集，跳过 不应降为 Debug")
	}
}

func TestValidateCollectResult_UnknownClient(t *testing.T) {
	err := ValidateResult("typo", Result{})
	if err == nil || !strings.Contains(err.Error(), "未知客户端") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateCollectResult_ExplicitDisabledClient(t *testing.T) {
	err := ValidateResult("claude", Result{Matched: true})
	if err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("err = %v", err)
	}
}

func TestGroupByDateSource(t *testing.T) {
	errors := []model.CollectionError{
		{ID: 1, Date: "2026-06-09", Source: "claude"},
		{ID: 2, Date: "2026-06-09", Source: "claude"},
		{ID: 3, Date: "2026-06-08", Source: "codex"},
	}

	groups := groupByDateSource(errors)
	if len(groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(groups))
	}

	if groups[1] != (retryGroup{date: "2026-06-09", source: "claude"}) {
		t.Fatalf("groups not deterministic: %+v", groups)
	}
}

func TestRunRetry_ReturnsErrorWhenRetryFails(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if err := db.RecordError(context.Background(), usageDB, "2026-06-23", "claude", "boom", ""); err != nil {
		t.Fatal(err)
	}
	failed := &fixedCollector{name: "claude", err: errors.New("still broken")}
	var out bytes.Buffer
	err = RunRetryWithDeps(testDeps(true, failed), usageDB, "claude", collectTestLogger(), &out)
	if err == nil {
		t.Fatal("expected retry failure")
	}
	remaining, err := db.GetUnresolvedErrors(usageDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].RetryCount != 1 || remaining[0].Resolved {
		t.Fatalf("unexpected remaining errors: %+v", remaining)
	}
}

func TestRunRetry_ManualRetryBeyondThreeCanResolve(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if err := db.RecordError(context.Background(), usageDB, "2026-06-23", "claude", "boom", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := db.GetUnresolvedErrors(usageDB)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed unresolved error: %v, %+v", err, rows)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.IncrementRetryCountByDateSource(context.Background(), usageDB, "2026-06-23", "claude"); err != nil {
			t.Fatal(err)
		}
	}

	succeeded := &fixedCollector{name: "claude"} // 无新数据也是成功的重采
	if err := RunRetryWithDeps(testDeps(true, succeeded), usageDB, "claude", collectTestLogger(), io.Discard); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.GetUnresolvedErrors(usageDB)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("successful fourth manual retry must resolve error: %v, %+v", err, remaining)
	}
}

func TestRunRetry_UnknownClientReturnsError(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	err = RunRetryWithDeps(testDeps(true, &fixedCollector{name: "claude"}), usageDB, "typo", collectTestLogger(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "未知客户端") {
		t.Fatalf("unknown client must fail, got %v", err)
	}
}

func TestRunRetry_DisabledCollectorDoesNotIncrement(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if err := db.RecordError(context.Background(), usageDB, "2026-06-23", "claude", "boom", ""); err != nil {
		t.Fatal(err)
	}
	err = RunRetryWithDeps(testDeps(false, &fixedCollector{name: "claude"}),
		usageDB, "claude", collectTestLogger(), io.Discard)
	if err == nil {
		t.Fatal("disabled collector retry must fail")
	}
	remaining, err := db.GetUnresolvedErrors(usageDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].RetryCount != 0 {
		t.Fatalf("non-attempt must not increment retry_count: %+v", remaining)
	}
}

// TestRunCollect_ZeroSessionsMessage 语义：空结果消息改为"消息/API 请求"
// （设计文档 行为）。先红：当前固定输出"采集 %d 个会话"。
func TestRunCollect_ZeroSessionsMessage(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := &fixedCollector{name: "claude"}
	var out bytes.Buffer
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), &out, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if !result.Complete() {
		t.Fatalf("zero-session collect must succeed: %+v", result)
	}
	got := out.String()
	if !strings.Contains(got, "采集 0 条消息/API 请求") {
		t.Fatalf("zero-session output must contain '采集 0 条消息/API 请求', got: %q", got)
	}
	if strings.Contains(got, "采集 0 个会话") {
		t.Fatalf("zero-session output must not contain '采集 0 个会话', got: %q", got)
	}
}

// TestRunCollect_CancelledCollectorErrorNotPersisted 边界修复：
// collector 因 ctx 取消返回错误（如 OpenCode QueryContext 在 daemon 关闭时返回
// context.Canceled）时，取消不是采集故障——禁止落错误记录、禁止标记成功。
func TestRunCollect_CancelledCollectorErrorNotPersisted(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fixedCollector{name: "claude", err: context.Canceled}
	result := RunCollect(ctx, testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if result.Complete() || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("cancelled collector must surface cancellation, not success: %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 0 {
		t.Fatalf("cancellation must not persist as collection error: %+v", remaining)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancellation must not mark collection_log: count=%d err=%v", count, err)
	}
}

// TestRunCollect_RouterFailureDegradesWhenCtxActive：router 失败但 ctx 未取消时
// （真实降级，非 daemon 关闭），必须保留"降级 warn + 继续落客户端数据 +
// 标记成功 + 解决历史错误"语义。
func TestRunCollect_RouterFailureDegradesWhenCtxActive(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordErrorsByDate(context.Background(), usageDB, []string{"2026-06-23"}, "claude", "old failure", "")
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: filepath.Join(t.TempDir(), "missing.db")},
		},
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	deps := &Deps{
		cfg:        cfg,
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": collector.NewCCSwitchAdapter("cc_switch", cfg.Routers["cc_switch"], cfg)},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if !result.Complete() {
		t.Fatalf("router degradation must not fail collect: %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 0 {
		t.Fatalf("historical errors must be resolved on degraded success: %+v", remaining)
	}
}

// fakeRouter 测试用 RouterAdapter：记录所有调用 request，支持确定性复现
// router 路径相关分支（ctx 取消、router-only、router backfill 等）。
type fakeRouter struct {
	result collector.RouterCollectResult
	err    error
	calls  []collector.RouterCollectRequest
	name   string
}

func newFakeRouter() *fakeRouter { return &fakeRouter{name: "cc_switch"} }

func (f *fakeRouter) Name() string { return f.name }
func (f *fakeRouter) SyncSource() string {
	return collector.SyncSourceCCSwitchRouter
}
func (f *fakeRouter) Capabilities() collector.RouterCapabilities {
	return collector.RouterCapabilities{Provider: true, Model: true}
}
func (f *fakeRouter) CollectLogs(_ context.Context, req collector.RouterCollectRequest, _ *slog.Logger) (collector.RouterCollectResult, error) {
	f.calls = append(f.calls, req)
	return f.result, f.err
}

// TestRunCollect_RouterCancelledAbortsCollect：router 因 ctx 取消失败后立即返回。
// 断言取消不被误判为完整成功：不写 collection_log、不解决历史错误。
func TestRunCollect_RouterCancelledAbortsCollect(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordErrorsByDate(context.Background(), usageDB, []string{"2026-06-23"}, "claude", "old failure", "")
	ctx, cancel := context.WithCancel(context.Background())
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	router := newFakeRouter()
	router.err = context.Canceled
	deps := &Deps{
		cfg:        &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": cancelRouter{router: router, cancel: cancel}},
	}
	result := RunCollect(ctx, deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true, false)
	if result.Complete() || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("router cancellation must abort collect, not mark success: %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 1 {
		t.Fatalf("aborted collect must not resolve history: %+v", remaining)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("aborted collect must not mark collection_log: count=%d err=%v", count, err)
	}
}

// cancelRouter 在 CollectLogs 内同步取消 ctx 并委托给内嵌 router，
// 用于确定性复现"ctx 在 router 调用期间被并发取消"的分支。
type cancelRouter struct {
	router *fakeRouter
	cancel context.CancelFunc
}

func (c cancelRouter) Name() string       { return c.router.Name() }
func (c cancelRouter) SyncSource() string { return c.router.SyncSource() }
func (c cancelRouter) Capabilities() collector.RouterCapabilities {
	return c.router.Capabilities()
}
func (c cancelRouter) CollectLogs(ctx context.Context, req collector.RouterCollectRequest, l *slog.Logger) (collector.RouterCollectResult, error) {
	if c.cancel != nil {
		c.cancel()
	}
	return c.router.CollectLogs(ctx, req, l)
}

// reqCapturingCollector 捕获最后一次 Collect 收到的 req.Dates 长度。
type reqCapturingCollector struct {
	name    string
	result  collector.CollectResult
	err     error
	lastLen *atomic.Int32
	calls   *atomic.Int32
}

func (c *reqCapturingCollector) Name() string { return c.name }
func (c *reqCapturingCollector) SyncSources() []string {
	return []string{"test_source"}
}
func (c *reqCapturingCollector) Collect(_ context.Context, req collector.CollectRequest, _ *slog.Logger) (collector.CollectResult, error) {
	if c.calls != nil {
		c.calls.Add(1)
	}
	if c.lastLen != nil {
		c.lastLen.Store(int32(len(req.Dates)))
	}
	return c.result, c.err
}

// TestFilterUncollected：filterUncollected 保留 dates 中不在 collected 集合内的日期。
func TestFilterUncollected(t *testing.T) {
	if got := filterUncollected([]string{"2026-07-01", "2026-07-02"}, nil); len(got) != 2 {
		t.Fatalf("empty collected should pass all: %v", got)
	}
	got := filterUncollected([]string{"2026-07-01", "2026-07-02", "2026-07-03"}, []string{"2026-07-02"})
	want := []string{"2026-07-01", "2026-07-03"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("filterUncollected = %v, want %v", got, want)
	}
}

// TestRunCollect_SkipCollectedFiltersCollectedDates：skipCollected=true 时，
// 已在 collection_log 的 date 不传给 collector。
func TestRunCollect_SkipCollectedFiltersCollectedDates(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := db.MarkCollected(context.Background(), usageDB, "2026-07-01", "claude", 0); err != nil {
		t.Fatal(err)
	}
	var hit atomic.Int32
	c := &reqCapturingCollector{name: "claude", lastLen: &hit,
		result: collector.CollectResult{
			Messages: []model.Message{msg("s1", model.ClientClaudeCode, "2026-07-02")},
		}}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-01", "2026-07-02"}},
		false /*recordError*/, true /*skipCollected*/)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if got, want := hit.Load(), int32(1); got != want {
		t.Fatalf("collector got %d dates, want %d (已采集 date 应被过滤)", got, want)
	}
}

// TestRunCollect_SkipCollectedAllCollectedSkipsAndSucceeds：全部 date 已采集时，
// collector 不执行，但该 client 仍计 Attempted/Succeeded。
func TestRunCollect_SkipCollectedAllCollectedSkipsAndSucceeds(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	for _, d := range []string{"2026-07-01", "2026-07-02"} {
		if err := db.MarkCollected(context.Background(), usageDB, d, "claude", 1); err != nil {
			t.Fatal(err)
		}
	}
	var hit atomic.Int32
	var calls atomic.Int32
	c := &reqCapturingCollector{name: "claude", lastLen: &hit, calls: &calls}
	var out bytes.Buffer
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), &out, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-01", "2026-07-02"}},
		false, true)
	if !result.Complete() {
		t.Fatalf("all-collected skip must be Complete: %+v", result)
	}
	if calls.Load() != 0 {
		t.Fatalf("collector must not run when all dates collected: calls=%d", calls.Load())
	}
}

// TestRunCollect_SkipCollectedFalseRecollectsAll：skipCollected=false 时全部 date 都传给 collector。
func TestRunCollect_SkipCollectedFalseRecollectsAll(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := db.MarkCollected(context.Background(), usageDB, "2026-07-01", "claude", 0); err != nil {
		t.Fatal(err)
	}
	var hit atomic.Int32
	c := &reqCapturingCollector{name: "claude", lastLen: &hit}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-01", "2026-07-02"}},
		false, false /*skipCollected=false=force/daemon 语义*/)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if got, want := hit.Load(), int32(2); got != want {
		t.Fatalf("skipCollected=false should pass all dates: hit=%d want %d", got, want)
	}
}

// TestRunCollect_SkipCollectedFailureDoesNotRecordCollectedDate（评审 C1）：
// 去重后写阶段失败只能记录 cdates 范围内的错误。
func TestRunCollect_SkipCollectedFailureDoesNotRecordCollectedDate(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := db.MarkCollected(context.Background(), usageDB, "2026-07-01", "claude", 1); err != nil {
		t.Fatal(err)
	}
	c := &fixedCollector{name: "claude", err: errors.New("boom")}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-01", "2026-07-02"}},
		true /*recordError*/, true /*skipCollected*/)
	if result.Complete() {
		t.Fatal("expected failure")
	}
	got, _ := db.GetErrors(usageDB, db.ErrorFilter{Dates: []string{"2026-07-01", "2026-07-02"}, Source: "claude", Unresolved: true})
	if len(got) != 1 || got[0].Date != "2026-07-02" {
		t.Fatalf("expected only 2026-07-02 error, got %+v", got)
	}
}

// =====：router/client 路径事务化 =====

// TestRunCollect_ClientTxnFailureAbortsAllWrites：任一本地写失败时
// 数据和 cursor 均不推进。在 sync_state 建 BEFORE INSERT trigger RAISE(ABORT)，
// 执行 Incremental Collect，断言 messages/sessions/collection_log/sync_state 均为 0。
func TestRunCollect_ClientTxnFailureAbortsAllWrites(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_cursor BEFORE INSERT ON sync_state
		BEGIN SELECT RAISE(ABORT, 'cursor fail'); END`); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages:    []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
		NextCursors: map[string]model.SyncCursor{"test_source": {Value: 100}},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Incremental: true}, false, false)
	if result.Complete() || result.Err == nil {
		t.Fatalf("cursor write failure must fail: %+v", result)
	}
	// 验证事务原子性：messages/sessions/collection_log/sync_state 全部为 0
	for table, want := range map[string]int{
		"messages": 0, "sessions": 0, "collection_log": 0, "sync_state": 0,
	} {
		var got int
		if err := usageDB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, got, want, err)
		}
	}
}

// TestRunCollect_CursorAdvancedOnSuccess：业务数据成功提交后 cursor 更新。
func TestRunCollect_CursorAdvancedOnSuccess(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages:    []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
		NextCursors: map[string]model.SyncCursor{"test_source": {Value: 100, ID: "abc"}},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Incremental: true}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	cursors, err := db.GetSyncCursors(context.Background(), usageDB, "claude", []string{"test_source"})
	if err != nil {
		t.Fatal(err)
	}
	if cursors["test_source"].Value != 100 || cursors["test_source"].ID != "abc" {
		t.Fatalf("cursor not advanced: %+v", cursors["test_source"])
	}
}

// TestRunCollect_PartialErrorPersistsDataWithoutCompletionProgress：
// 部分源失败时保留已成功解析的数据，但不能写完成日志、解决旧错误或推进 cursor，
// 否则后续普通采集会跳过仍有缺口的日期/增量区间。
func TestRunCollect_PartialErrorPersistsDataWithoutCompletionProgress(t *testing.T) {
	ctx := context.Background()
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	const date = "2026-06-23"
	if err := db.RecordErrorsByDate(ctx, usageDB, []string{date}, "claude", "old failure", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSyncCursors(ctx, usageDB, "claude", map[string]model.SyncCursor{
		"test_source": {Value: 50, ID: "old"},
	}); err != nil {
		t.Fatal(err)
	}

	partialErr := errors.New("one source file is unreadable")
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("partial-ok", model.ClientClaudeCode, date)},
		NextCursors: map[string]model.SyncCursor{
			"test_source": {Value: 100, ID: "new"},
		},
		PartialErr: partialErr,
	})
	result := RunCollect(ctx, testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Incremental: true}, true, false)
	if result.Complete() || result.Succeeded != 0 || !errors.Is(result.Err, partialErr) {
		t.Fatalf("partial result = %+v", result)
	}

	var messageCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`,
		model.ClientClaudeCode, "partial-ok").Scan(&messageCount); err != nil || messageCount != 1 {
		t.Fatalf("成功部分应落库: count=%d err=%v", messageCount, err)
	}
	var logCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log
		WHERE date=? AND source='claude'`, date).Scan(&logCount); err != nil || logCount != 0 {
		t.Fatalf("部分失败不能标记完成: count=%d err=%v", logCount, err)
	}
	cursors, err := db.GetSyncCursors(ctx, usageDB, "claude", []string{"test_source"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cursors["test_source"]; got.Value != 50 || got.ID != "old" {
		t.Fatalf("部分失败不能推进 cursor: %+v", got)
	}
	unresolved, err := db.GetErrors(usageDB, db.ErrorFilter{
		Dates: []string{date}, Source: "claude", Unresolved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("旧错误应保持未解决且新增部分失败错误: %+v", unresolved)
	}
}

// TestRunCollect_DuplicateMessageCount：双源/重复 ID 按 (client,id,date) 去重计数。
func TestRunCollect_DuplicateMessageCount(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 同一 message 出现两次（模拟双源合并后重复），应只计 1 条
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{
			msg("dup", model.ClientClaudeCode, "2026-06-23"),
			msg("dup", model.ClientClaudeCode, "2026-06-23"),
			msg("other", model.ClientClaudeCode, "2026-06-23"),
		},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var got int
	if err := usageDB.QueryRow(`SELECT session_count FROM collection_log WHERE date='2026-06-23' AND source='claude'`).Scan(&got); err != nil || got != 2 {
		t.Fatalf("dedup count=%d want 2 err=%v", got, err)
	}
}

// TestRunCollect_CliEmptyResultMarksCountZero：CLI 空结果以 count=0 标记请求日期。
// 普通收集在 skipCollected 时跳过，--force 重新执行。
func TestRunCollect_CliEmptyResultMarksCountZero(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := &fixedCollector{name: "claude"}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-10"}}, true, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var got int
	if err := usageDB.QueryRow(`SELECT session_count FROM collection_log WHERE date='2026-07-10' AND source='claude'`).Scan(&got); err != nil || got != 0 {
		t.Fatalf("empty result count=%d want 0 err=%v", got, err)
	}
}

// TestRunCollect_ChangedFileCrossDayLogsPerDate：ChangedFile 跨日消息分别写 collection_log。
func TestRunCollect_ChangedFileCrossDayLogsPerDate(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{
			msg("m1", model.ClientClaudeCode, "2026-07-09"),
			msg("m2", model.ClientClaudeCode, "2026-07-10"),
		},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: "/some/file.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// 两个 date 各记一条
	for date, want := range map[string]int{"2026-07-09": 1, "2026-07-10": 1} {
		var got int
		if err := usageDB.QueryRow(`SELECT session_count FROM collection_log WHERE date=? AND source='claude'`, date).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count=%d want=%d err=%v", date, got, want, err)
		}
	}
}

// TestRunCollect_RouterLateArrivalOnlyChangesRouterFields：
// router late arrival 只改 router_* 字段，不改 message.provider。
func TestRunCollect_RouterLateArrivalOnlyChangesRouterFields(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 先写入一条 client 消息，provider="client_provider"
	c1 := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{
			{ID: "m1", Client: model.ClientClaudeCode, Date: "2026-06-23", Provider: "client_provider"},
		},
	})
	if r := RunCollect(context.Background(), testDeps(true, c1), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false); !r.Complete() {
		t.Fatalf("first collect failed: %+v", r)
	}
	// Source=router 路径：router 重新采集返回 late-arrival 日志（message_id 已对齐）
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{{
			RequestID: "r1", MessageID: "m1", RouterName: "cc_switch",
			AppType: "claude", ProviderName: "router_provider", Model: "router_model",
		}},
	}
	deps := &Deps{
		cfg:     &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var provider, routerProvider, routerModel string
	if err := usageDB.QueryRow(`SELECT provider, router_provider, router_model FROM messages WHERE id='m1' AND client=?`, model.ClientClaudeCode).
		Scan(&provider, &routerProvider, &routerModel); err != nil {
		t.Fatal(err)
	}
	if provider != "client_provider" {
		t.Fatalf("message.provider changed: %q", provider)
	}
	if routerProvider != "router_provider" || routerModel != "router_model" {
		t.Fatalf("router fields not backfilled: provider=%q model=%q", routerProvider, routerModel)
	}
	// router 路径不写 collection_log
	var logCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log`).Scan(&logCount); err != nil || logCount != 1 {
		t.Fatalf("router path must not touch collection_log (1 from client collect): %d", logCount)
	}
}

// TestRunCollect_SourceRouterDoesNotCallClientCollector：
// Source=router 不调用 client collector、不改 token。
func TestRunCollect_SourceRouterDoesNotCallClientCollector(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 预置 client 消息带 token
	c1 := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{{
			ID: "m1", Client: model.ClientClaudeCode, Date: "2026-06-23",
			Provider: "client_p", InputTokens: 100,
		}},
	})
	if r := RunCollect(context.Background(), testDeps(true, c1), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false); !r.Complete() {
		t.Fatalf("seed collect failed: %+v", r)
	}
	// Source=router 路径：不应调用 client collector
	clientCollector := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{{
			ID: "m1", Client: model.ClientClaudeCode, Date: "2026-06-23",
			InputTokens: 999, // 如果被调用，token 会被改
		}},
	})
	// 预置 router 日志
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{{
		RequestID: "r1", MessageID: "m1", RouterName: "cc_switch",
		AppType: "claude", ProviderName: "rp",
	}}); err != nil {
		t.Fatal(err)
	}
	router := newFakeRouter()
	deps := &Deps{
		cfg:        &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		collectors: []collector.Collector{clientCollector},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if clientCollector.calls != 0 {
		t.Fatalf("Source=router must not call client collector: calls=%d", clientCollector.calls)
	}
	var inputTokens int64
	if err := usageDB.QueryRow(`SELECT input_tokens FROM messages WHERE id='m1' AND client=?`, model.ClientClaudeCode).
		Scan(&inputTokens); err != nil || inputTokens != 100 {
		t.Fatalf("token changed by router path: %d", inputTokens)
	}
}

// TestRunCollect_DaemonClientPathSkipsRouterCollectLogs：
// daemon client 路径（ChangedFile/Incremental）不调用 router CollectLogs。
func TestRunCollect_DaemonClientPathSkipsRouterCollectLogs(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	router := newFakeRouter()
	deps := &Deps{
		cfg:        &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
	// ChangedFile 路径（daemon 触发）
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: "/some/file.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if len(router.calls) != 0 {
		t.Fatalf("daemon client path must not call router CollectLogs: calls=%d", len(router.calls))
	}
}

// TestRunCollect_CliDatesFetchesRouterLogsThenBackfills：
// CLI 模式（有 Dates）先落 raw_router_logs 再回填 messages。
func TestRunCollect_CliDatesFetchesRouterLogsThenBackfills(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{{
			RequestID: "r1", MessageID: "m1", RouterName: "cc_switch",
			AppType: "claude", ProviderName: "router_p", Model: "router_m",
		}},
	}
	deps := &Deps{
		cfg:        &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if len(router.calls) != 1 {
		t.Fatalf("CLI Dates mode must call router once: calls=%d", len(router.calls))
	}
	// raw_router_logs 已落库
	var logCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM raw_router_logs WHERE router_name='cc_switch'`).Scan(&logCount); err != nil || logCount != 1 {
		t.Fatalf("raw_router_logs not persisted: %d", logCount)
	}
	// messages 已被 backfill
	var routerProvider string
	if err := usageDB.QueryRow(`SELECT router_provider FROM messages WHERE id='m1' AND client=?`, model.ClientClaudeCode).
		Scan(&routerProvider); err != nil || routerProvider != "router_p" {
		t.Fatalf("router_provider not backfilled: %q", routerProvider)
	}
}

// TestRunCollect_ProviderAliasOnlyChangesRouterProvider：
// 配置 provider alias 后断言只改 RouterProvider，不改 Message.Provider。
func TestRunCollect_ProviderAliasOnlyChangesRouterProvider(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{{
			ID: "m1", Client: model.ClientClaudeCode, Date: "2026-06-23",
			Provider: "original",
		}},
	})
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{{
			RequestID: "r1", MessageID: "m1", RouterName: "cc_switch",
			AppType: "claude", ProviderName: "raw_provider", Model: "m",
		}},
	}
	deps := &Deps{
		cfg: &config.Config{
			Clients:         map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
			ProviderAliases: map[string]string{"raw_provider": "alias_provider"},
		},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var provider, routerProvider string
	if err := usageDB.QueryRow(`SELECT provider, router_provider FROM messages WHERE id='m1' AND client=?`, model.ClientClaudeCode).
		Scan(&provider, &routerProvider); err != nil {
		t.Fatal(err)
	}
	if provider != "original" {
		t.Fatalf("message.provider changed by alias: %q", provider)
	}
	if routerProvider != "alias_provider" {
		t.Fatalf("router_provider should be aliased: %q", routerProvider)
	}
}

// TestRunCollect_CtxCancelBeforePersistDoesNotRecordError：
// ctx 取消：立即 rollback，不调用 RecordErrorsByDate。
func TestRunCollect_CtxCancelBeforePersistDoesNotRecordError(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	ctx, cancel := context.WithCancel(context.Background())
	// 在 persist 前取消
	cancel()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	result := RunCollect(ctx, testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true /*recordError*/, false)
	if result.Complete() || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("cancelled must surface: %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 0 {
		t.Fatalf("ctx cancel must not record error: %+v", remaining)
	}
}

// TestRunCollect_CtxCancelDuringTxnDoesNotRecordError (行为 补充)：
// collector 成功返回数据后、persistClientBatch 期间 ctx 被取消时，
// 事务失败但不得记为采集故障（与读阶段取消同策略）。
type cancelAfterCollectCollector struct {
	name   string
	cancel context.CancelFunc
}

func (c *cancelAfterCollectCollector) Name() string { return c.name }
func (c *cancelAfterCollectCollector) SyncSources() []string {
	return []string{"test_source"}
}
func (c *cancelAfterCollectCollector) Collect(_ context.Context, _ collector.CollectRequest, _ *slog.Logger) (collector.CollectResult, error) {
	// 在返回数据后立即取消 ctx，模拟"collector 返回后、persist 前 ctx 被并发取消"
	if c.cancel != nil {
		c.cancel()
	}
	return collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	}, nil
}

func TestRunCollect_CtxCancelDuringTxnDoesNotRecordError(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := &cancelAfterCollectCollector{name: "claude", cancel: cancel}
	result := RunCollect(ctx, testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, true /*recordError*/, false)
	if result.Complete() {
		t.Fatalf("cancelled must not be Complete: %+v", result)
	}
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if len(remaining) != 0 {
		t.Fatalf("ctx cancel during txn must not record error: %+v", remaining)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_log`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ctx cancel during txn must not mark collection_log: %d", count)
	}
}

// TestRunCollect_DaemonFailureBeforeMessagesUsesCurrentDate：
// daemon 在产出消息前失败时错误记录归当前本地日期。
func TestRunCollect_DaemonFailureBeforeMessagesUsesCurrentDate(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := &fixedCollector{name: "claude", err: errors.New("daemon read failure")}
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Incremental: true}, true /*recordError*/, false)
	if result.Complete() {
		t.Fatal("expected failure")
	}
	today := time.Now().Format("2006-01-02")
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Dates: []string{today}, Source: "claude", Unresolved: true})
	if len(remaining) != 1 {
		t.Fatalf("expected 1 error at today=%s, got %+v", today, remaining)
	}
}

// TestRunCollect_RouterOnlyPathTxnFailureRecordsError (补充)：
// router-only 路径本地事务失败时，事务必须先回滚再记录错误，
// 避免 RecordErrorsByDate 在 tx 持锁时开新事务死锁。
func TestRunCollect_RouterOnlyPathTxnFailureRecordsError(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 在 raw_router_logs 建 trigger 强制写入失败
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_router_log BEFORE INSERT ON raw_router_logs
		BEGIN SELECT RAISE(ABORT, 'router log forced fail'); END`); err != nil {
		t.Fatal(err)
	}
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{{
			RequestID: "r1", MessageID: "m1", RouterName: "cc_switch", AppType: "claude",
		}},
	}
	deps := &Deps{
		cfg:     &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, true /*recordError*/, false)
	if result.Complete() || result.Err == nil {
		t.Fatalf("router-only txn failure must fail: %+v", result)
	}
	// 错误被正确记录（未死锁）
	remaining, _ := db.GetErrors(usageDB, db.ErrorFilter{Source: "claude", Unresolved: true})
	if len(remaining) != 1 {
		t.Fatalf("router-only failure must record 1 error, got %+v", remaining)
	}
}

// TestRunCollect_ManualRetryUsesDatesNoCursorAdvance：
// 手工重试使用 Dates，不推进 daemon cursor。
func TestRunCollect_ManualRetryUsesDatesNoCursorAdvance(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 预置 cursor
	if err := db.SetSyncCursors(context.Background(), usageDB, "claude", map[string]model.SyncCursor{
		"test_source": {Value: 50, ID: "old"},
	}); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-06-23")},
	})
	// Dates 模式（手工重试），Incremental=false
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-23"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	cursors, err := db.GetSyncCursors(context.Background(), usageDB, "claude", []string{"test_source"})
	if err != nil {
		t.Fatal(err)
	}
	// cursor 不应被推进（保持 50/old）
	if cursors["test_source"].Value != 50 || cursors["test_source"].ID != "old" {
		t.Fatalf("cursor advanced by manual retry: %+v", cursors["test_source"])
	}
}

// TestMessageCounts_DeduplicatesByClientIDDate (行为 helper)：
// messageCounts 按 (client,id,date) 去重计数。
func TestMessageCounts_DeduplicatesByClientIDDate(t *testing.T) {
	messages := []model.Message{
		msg("m1", "claude", "2026-06-23"),
		msg("m1", "claude", "2026-06-23"), // 重复
		msg("m2", "claude", "2026-06-23"),
		msg("m1", "codex", "2026-06-23"),  // 不同 client 不去重
		msg("m1", "claude", "2026-06-24"), // 不同 date 不去重
	}
	counts := messageCounts(messages)
	want := map[string]int{
		"2026-06-23": 3, // m1(claude) + m2(claude) + m1(codex)
		"2026-06-24": 1,
	}
	if len(counts) != len(want) {
		t.Fatalf("counts len=%d want=%d: %+v", len(counts), len(want), counts)
	}
	for d, w := range want {
		if got := counts[d]; got != w {
			t.Fatalf("counts[%s]=%d want %d", d, got, w)
		}
	}
}

// TestDatesToMark_MergesRequestAndActualDates：
// datesToMark 合并 req.Dates 与实际消息日期（去重排序）。
func TestDatesToMark_MergesRequestAndActualDates(t *testing.T) {
	req := collector.CollectRequest{Dates: []string{"2026-06-23", "2026-06-24"}}
	counts := map[string]int{"2026-06-25": 1} // 实际消息在请求外的日期
	got := datesToMark(req, counts)
	want := []string{"2026-06-23", "2026-06-24", "2026-06-25"}
	if len(got) != len(want) || !sort.StringsAreSorted(got) {
		t.Fatalf("datesToMark=%v want sorted %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("datesToMark[%d]=%q want %q", i, got[i], w)
		}
	}
}
