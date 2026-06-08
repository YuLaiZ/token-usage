// internal/analyzer/analyzer_test.go
package analyzer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// ===== 测试用例 =====

// TestAnalyzer_RunNoMonitorsReturnsError 守护：无任何监控时 Run 应立即返回 error，
// 而非 warn 后空转等 ctx.Done（使 launchd 能感知配置错误并告警/重启，而非静默空转）。
// 有监控的优雅关闭路径由 integration_test 覆盖。
func TestAnalyzer_RunNoMonitorsReturnsError(t *testing.T) {
	a := New(noopExecute, nil) // 无 watchers/pollers

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error when no monitors configured, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run blocked (would idle on ctx.Done with no monitors); expected immediate error")
	}
}

func TestAnalyzer_StopTwice(t *testing.T) {
	a := New(noopExecute, nil)

	// Stop 两次不应 panic
	a.Stop()
	a.Stop()
}

func TestSetupFromConfig_WatchersAndPollers(t *testing.T) {
	tests := []struct {
		name         string
		cfgTemplate  string   // 用 %s 占位 data_dir 等路径
		preCreate    []string // 相对 tmpDir 预创建的文件（如 codex state_*.sqlite），使 Glob 能匹配到
		wantWatchers int
		wantPollers  int
	}{
		{
			name: "claude enabled with cc_switch router",
			cfgTemplate: `
data_dir = "%s"
[clients.claude]
enabled = true
router = "cc_switch"
[clients.claude.paths]
projects_dir = "%s/claude"
[routers.cc_switch]
db_path = "%s/cc-switch.db"
[daemon]
poll_interval = 1
`,
			wantWatchers: 1, // claude watcher
			wantPollers:  1, // cc-switch poller
		},
		{
			name: "opencode enabled",
			cfgTemplate: `
data_dir = "%s"
[clients.opencode]
enabled = true
[clients.opencode.paths]
db = "%s/opencode.db"
[daemon]
poll_interval = 1
`,
			wantWatchers: 0,
			wantPollers:  1, // opencode poller
		},
		{
			name: "codex enabled with state files",
			cfgTemplate: `
data_dir = "%s"
[clients.codex]
enabled = true
[clients.codex.paths]
sessions_dir = "%s/codex/sessions"
state_dir = "%s/codex/state"
[daemon]
poll_interval = 1
`,
			preCreate:    []string{"codex/state/state_v1.sqlite"}, // 预创建 state 文件，使 setupFromConfig 的 Glob 命中
			wantWatchers: 1,                                       // codex watcher（sessions_dir）
			wantPollers:  1,                                       // Glob 命中 state_v1.sqlite → 建 1 个 codex state poller
		},
		{
			name: "all disabled",
			cfgTemplate: `
data_dir = "%s"
[clients.claude]
enabled = false
[clients.opencode]
enabled = false
[clients.codex]
enabled = false
[clients.workbuddy]
enabled = false
[clients.zcode]
enabled = false
[daemon]
poll_interval = 1
`,
			wantWatchers: 0,
			wantPollers:  0,
		},
		{
			// 通用化回归：非 claude client（codex）配 cc_switch router 时，
			// daemon 也应为其建立 cc-switch.db poller 并触发 codex 采集。
			// 通用化前写死 claude，此用例下不会建 router poller（wantPollers 仅 1）。
			name: "codex enabled with cc_switch router",
			cfgTemplate: `
data_dir = "%s"
[clients.codex]
enabled = true
router = "cc_switch"
[clients.codex.paths]
sessions_dir = "%s/codex/sessions"
state_dir = "%s/codex/state"
[routers.cc_switch]
db_path = "%s/cc-switch.db"
[daemon]
poll_interval = 1
`,
			preCreate:    []string{"codex/state/state_v1.sqlite"}, // codex state poller 需要 Glob 命中
			wantWatchers: 1,                                       // codex sessions_dir watcher
			wantPollers:  2,                                       // codex state poller + cc_switch router poller
		},
		{
			name: "zcode enabled with db",
			cfgTemplate: `
data_dir = "%s"
[clients.zcode]
enabled = true
[clients.zcode.paths]
db = "%s/zcode.db"
[daemon]
poll_interval = 1
`,
			wantWatchers: 0,
			wantPollers:  1, // zcode SQLite poller
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			// 预创建文件（如 codex state_*.sqlite），使 setupFromConfig 的 filepath.Glob 能匹配到，
			// 否则 codex state poller 创建分支永远无法被测试覆盖
			for _, rel := range tt.preCreate {
				full := filepath.Join(tmpDir, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatalf("mkdir for %s: %v", rel, err)
				}
				if err := os.WriteFile(full, []byte("sqlite"), 0644); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}

			cfgContent := strings.ReplaceAll(tt.cfgTemplate, "%s", tmpDir)
			cfgPath := filepath.Join(tmpDir, "config.toml")
			os.WriteFile(cfgPath, []byte(cfgContent), 0644)

			cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

			a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

			if len(a.jsonlWatchers) != tt.wantWatchers {
				t.Errorf("jsonlWatchers = %d, want %d", len(a.jsonlWatchers), tt.wantWatchers)
			}
			if len(a.sqlitePollers) != tt.wantPollers {
				t.Errorf("sqlitePollers = %d, want %d", len(a.sqlitePollers), tt.wantPollers)
			}
		})
	}
}

// TestSetupFromConfig_RouterPollerTriggersConfiguredClient 验证 router poller 触发的是
// 声明该 router 的 client（codex），而非写死的 claude。
// 表驱动用例只数 poller 数量，无法证明"cc-switch.db poller 的 clientName 是 codex 而非 claude"，
// 故新增独立测试直接检查 poller 字段。
func TestSetupFromConfig_RouterPollerTriggersConfiguredClient(t *testing.T) {
	tmpDir := t.TempDir()
	ccSwitchPath := filepath.Join(tmpDir, "cc-switch.db")
	cfgContent := fmt.Sprintf(`
data_dir = "%s"
[clients.codex]
enabled = true
router = "cc_switch"
[clients.codex.paths]
sessions_dir = "%s/codex/sessions"
state_dir = "%s/codex/state"
[routers.cc_switch]
db_path = "%s"
[daemon]
poll_interval = 1
`, tmpDir, tmpDir, tmpDir, ccSwitchPath)
	cfgPath := filepath.Join(tmpDir, "config.toml")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

	// 找到 dbPath == cc-switch.db 的 poller，验证 clientName == "codex"
	var found bool
	for _, p := range a.sqlitePollers {
		if p.dbPath == ccSwitchPath {
			found = true
			if p.clientName != "codex" {
				t.Errorf("router poller clientName = %q, want codex（应触发声明 router 的 client，非写死 claude）", p.clientName)
			}
		}
	}
	if !found {
		t.Fatal("未找到 cc-switch.db poller，router 通用化未生效")
	}
}

// TestSetupFromConfig_RequestSemantics 行为：按数据源注册正确的 CollectRequest。
// - JSONL watcher：ChangedFile 由 watcher 运行期动态填充（构造期无固定 request，只校验 watcher 存在）。
// - OpenCode/ZCode/Codex state SQLite poller：固定 Incremental=true, Source=""。
// - CC Switch router poller：固定 Source=router, Incremental=true。
func TestSetupFromConfig_RequestSemantics(t *testing.T) {
	tmpDir := t.TempDir()
	ccSwitchPath := filepath.Join(tmpDir, "cc-switch.db")
	stateDir := filepath.Join(tmpDir, "codex", "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state_v1.sqlite"), []byte("x"), 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// claude 配 cc_switch router（router poller 触发 claude 采集）
	cfgContent := fmt.Sprintf(`
data_dir = "%s"
[clients.claude]
enabled = true
router = "cc_switch"
[clients.claude.paths]
projects_dir = "%s/claude"
[clients.codex]
enabled = true
[clients.codex.paths]
sessions_dir = "%s/codex/sessions"
state_dir = "%s"
[clients.opencode]
enabled = true
[clients.opencode.paths]
db = "%s/opencode.db"
[clients.zcode]
enabled = true
[clients.zcode.paths]
db = "%s/zcode.db"
[clients.workbuddy]
enabled = true
[clients.workbuddy.paths]
projects_dir = "%s/workbuddy"
[routers.cc_switch]
db_path = "%s"
[daemon]
poll_interval = 1
`, tmpDir, tmpDir, tmpDir, stateDir, tmpDir, tmpDir, tmpDir, ccSwitchPath)
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

	// JSONL watchers：claude、codex、workbuddy（3 个）。
	// 不校验固定 request（ChangedFile 由运行期动态填充），只校验数量。
	if got, want := len(a.jsonlWatchers), 3; got != want {
		t.Fatalf("jsonlWatchers = %d, want %d", got, want)
	}

	// SQLite pollers：opencode(1) + zcode(1) + codex state(1) + cc_switch router(1) = 4
	if got, want := len(a.sqlitePollers), 4; got != want {
		t.Fatalf("sqlitePollers = %d, want %d", got, want)
	}

	for _, p := range a.sqlitePollers {
		switch p.clientName {
		case "opencode", "zcode", "codex":
			// client 源 SQLite poller：Incremental=true, Source=""
			if !p.request.Incremental {
				t.Errorf("client poller[%s] request.Incremental = false, want true", p.clientName)
			}
			if p.request.Source != "" {
				t.Errorf("client poller[%s] request.Source = %q, want empty", p.clientName, p.request.Source)
			}
		case "claude":
			// CC Switch router poller：Source=router, Incremental=true
			if p.request.Source != collector.CollectSourceRouter {
				t.Errorf("router poller[%s] request.Source = %q, want %q", p.clientName, p.request.Source, collector.CollectSourceRouter)
			}
			if !p.request.Incremental {
				t.Errorf("router poller[%s] request.Incremental = false, want true", p.clientName)
			}
		default:
			t.Errorf("unexpected poller clientName %q", p.clientName)
		}
	}
}

// TestAnalyzer_DifferentFilesNotDropped 行为：同一 client 依次 Submit 不同 ChangedFile
// 与一次 router 请求，三次都必须执行（不被 analyzer 二次抑制丢弃）。
// 旧版 per-client suppressWindow 会吞掉窗口内第二次同 client 触发，本用例守护此回归。
func TestAnalyzer_DifferentFilesNotDropped(t *testing.T) {
	type captured struct {
		client string
		req    collector.CollectRequest
	}
	results := make(chan captured, 8)
	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		results <- captured{client: client, req: req}
		return nil
	}
	a := New(execute, nil)
	enableSubmitForTest(t, a)

	ctx := context.Background()
	// 依次 Submit：同 client 不同 ChangedFile、再 Submit 一次 router 请求
	if err := a.Submit(ctx, "codex", collector.CollectRequest{ChangedFile: "/a.jsonl"}); err != nil {
		t.Fatalf("Submit[0]: %v", err)
	}
	if err := a.Submit(ctx, "codex", collector.CollectRequest{ChangedFile: "/b.jsonl"}); err != nil {
		t.Fatalf("Submit[1]: %v", err)
	}
	if err := a.Submit(ctx, "codex", collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true}); err != nil {
		t.Fatalf("Submit[2]: %v", err)
	}

	close(results)
	var got []captured
	for r := range results {
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("期望采集 3 次（不同 ChangedFile 与 router 均不丢弃），实际 %d: %+v", len(got), got)
	}
}

// TestAnalyzer_ClientAndRouterNotDropped 行为：client 请求与 router 请求不互相吞掉。
func TestAnalyzer_ClientAndRouterNotDropped(t *testing.T) {
	type captured struct {
		client string
		req    collector.CollectRequest
	}
	results := make(chan captured, 8)
	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		results <- captured{client: client, req: req}
		return nil
	}
	a := New(execute, nil)
	enableSubmitForTest(t, a)

	ctx := context.Background()
	if err := a.Submit(ctx, "claude", collector.CollectRequest{Incremental: true}); err != nil {
		t.Fatalf("Submit[0]: %v", err)
	}
	if err := a.Submit(ctx, "claude", collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true}); err != nil {
		t.Fatalf("Submit[1]: %v", err)
	}
	if err := a.Submit(ctx, "claude", collector.CollectRequest{ChangedFile: "/x.jsonl"}); err != nil {
		t.Fatalf("Submit[2]: %v", err)
	}

	close(results)
	var got []captured
	for r := range results {
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("期望 client/router/ChangedFile 三种请求都执行（%d），实际 %d: %+v", 3, len(got), got)
	}
}

// TestAnalyzer_RepeatedIncrementalRequestNotDropped 行为：连续两次 Incremental 请求
// 必须都执行。SQLitePoller 调回调后推进自己的 mtime，若 analyzer 丢弃第二次请求，
// 该批源数据可能永远没有后续触发。
func TestAnalyzer_RepeatedIncrementalRequestNotDropped(t *testing.T) {
	var n atomic.Int32
	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		n.Add(1)
		return nil
	}
	a := New(execute, nil)
	enableSubmitForTest(t, a)

	ctx := context.Background()
	if err := a.Submit(ctx, "zcode", collector.CollectRequest{Incremental: true}); err != nil {
		t.Fatalf("Submit[0]: %v", err)
	}
	if err := a.Submit(ctx, "zcode", collector.CollectRequest{Incremental: true}); err != nil {
		t.Fatalf("Submit[1]: %v", err)
	}

	if got := n.Load(); got != 2 {
		t.Fatalf("期望两次 Incremental 都执行（%d），实际 %d", 2, got)
	}
}

// TestSerialSubmit_ConcurrencyIsOne 行为：不同 CollectRequest 经 Submit 仍由 collectMu 串行执行，
// 最大并发仍为 1。
func TestSerialSubmit_ConcurrencyIsOne(t *testing.T) {
	var maxConcurrent int32
	var currentConcurrent int32

	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		c := atomic.AddInt32(&currentConcurrent, 1)
		// 更新最大并发度
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if c <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, c) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // 模拟采集耗时
		atomic.AddInt32(&currentConcurrent, -1)
		return nil
	}

	a := New(execute, nil)
	enableSubmitForTest(t, a)
	var wg sync.WaitGroup
	ctx := context.Background()
	// 用不同 ChangedFile / Incremental 请求并发 Submit，验证 collectMu 仍串行
	for i := 0; i < 10; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			req := collector.CollectRequest{ChangedFile: fmt.Sprintf("/f%d.jsonl", i)}
			if i%2 == 0 {
				req = collector.CollectRequest{Incremental: true}
			}
			_ = a.Submit(ctx, "test", req)
		}()
	}
	wg.Wait()

	if atomic.LoadInt32(&maxConcurrent) != 1 {
		t.Errorf("maxConcurrent = %d, want 1", atomic.LoadInt32(&maxConcurrent))
	}
}

// TestAnalyzer_RunWaitsForInFlightCollect 行为 守护关闭语义完整性：
// 关闭时即使 in-flight 采集慢于 debounce.Stop 的 stopTimeout（被超时放弃等待），
// analyzer.Run 也必须等待该 request-aware 回调真正结束才返回——否则外层 defer cleanup()
// 会关闭 usageDB，残留的采集 goroutine 向已关闭的 DB 写入，静默丢数据。
//
// 构造：stopTimeout 调小到 100ms，让阻塞的 collectFunc 不被 debounce.Wait 到；
// 触发一次 in-flight 采集后 cancel，断言 Run 不在采集完成前返回；放行后才返回。
func TestAnalyzer_RunWaitsForInFlightCollect(t *testing.T) {
	origTimeout := stopTimeout
	stopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stopTimeout = origTimeout })

	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, "claude", "proj")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	var finished int32
	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		startedOnce.Do(func() { close(started) })
		<-release
		atomic.StoreInt32(&finished, 1)
		return nil
	}

	// ready 现在等待生产 Ready() barrier（与 daemon 走同一协议），
	// 不再向 NewFromConfig 传测试专用 onReady。
	var expected int32

	cfgContent := `
data_dir = "` + tmpDir + `"
[clients.claude]
enabled = true
[clients.claude.paths]
projects_dir = "` + filepath.Join(tmpDir, "claude") + `"
[daemon]
poll_interval = 1
[log]
level = "info"
dir = "` + tmpDir + `/logs"
max_days = 7
`
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

	a := NewFromConfig(cfg, execute, nil, 50*time.Millisecond)
	atomic.StoreInt32(&expected, int32(len(a.jsonlWatchers)+len(a.sqlitePollers)))

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { a.Run(ctx); close(runDone) }()

	// 等待生产 Ready() barrier。
	select {
	case <-a.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for watcher ready")
	}

	// 触发一次 in-flight 采集
	if err := os.WriteFile(filepath.Join(claudeDir, "s.jsonl"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for in-flight collect to start")
	}

	cancel()

	// 修复前：debounce.Stop(100ms) 超时放弃等待慢回调后，Run 不等 in-flight 采集即返回；
	// 修复后：Run 等待 in-flight 采集，runDone 不应在此窗口关闭
	select {
	case <-runDone:
		t.Fatal("Run returned before in-flight collect finished (collectWg not honored)")
	case <-time.After(500 * time.Millisecond):
		// 预期：Run 仍在等待 in-flight 采集
	}

	// 放行采集完成，Run 应随后返回
	close(release)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after in-flight collect finished")
	}
	if atomic.LoadInt32(&finished) != 1 {
		t.Fatal("collectFunc did not finish")
	}
}

// ===== AutoClaw JSONL watcher 装配回归 =====

// TestSetupFromConfig_AutoClawJSONLWatcher autoclaw enabled + 显式 sessions_dir 时
// 应创建名为 autoclaw 的 JSONLWatcher，且不创建 SQLitePoller（AutoClaw 是 JSONL 型）。
func TestSetupFromConfig_AutoClawJSONLWatcher(t *testing.T) {
	tmpDir := t.TempDir()
	cfgTemplate := `
data_dir = "%s"
[clients.autoclaw]
enabled = true
[clients.autoclaw.paths]
sessions_dir = "%s/autoclaw/agents"
[daemon]
poll_interval = 1
`
	cfgContent := strings.ReplaceAll(cfgTemplate, "%s", tmpDir)
	cfgPath := filepath.Join(tmpDir, "config.toml")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)
	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

	// 应有 1 个 autoclaw JSONL watcher
	var found bool
	for _, w := range a.jsonlWatchers {
		if w.clientName == "autoclaw" {
			found = true
		}
	}
	if !found {
		t.Errorf("jsonlWatchers 应含 clientName=autoclaw 的 watcher, got %v", watcherClientNames(a.jsonlWatchers))
	}
	// 不应有 autoclaw SQLite poller（AutoClaw 数据源是 JSONL）
	for _, p := range a.sqlitePollers {
		if p.clientName == "autoclaw" {
			t.Errorf("autoclaw 不应建 SQLitePoller（JSONL 型），但找到 clientName=autoclaw 的 poller")
		}
	}
}

// TestSetupFromConfig_AutoClawDisabled_NoWatcher autoclaw disabled 时不创建 watcher。
func TestSetupFromConfig_AutoClawDisabled_NoWatcher(t *testing.T) {
	tmpDir := t.TempDir()
	cfgTemplate := `
data_dir = "%s"
[clients.autoclaw]
enabled = false
[clients.autoclaw.paths]
sessions_dir = "%s/autoclaw/agents"
[daemon]
poll_interval = 1
`
	cfgContent := strings.ReplaceAll(cfgTemplate, "%s", tmpDir)
	cfgPath := filepath.Join(tmpDir, "config.toml")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)
	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

	for _, w := range a.jsonlWatchers {
		if w.clientName == "autoclaw" {
			t.Errorf("autoclaw disabled 时不应创建 watcher")
		}
	}
}

// TestSetupFromConfig_AutoClawDefaultPathBackfill 用户只写 enabled=true 不配 sessions_dir，
// 经 LoadEffectiveConfig（含 ApplyDefaults）后 sessions_dir 回填为 ~/.openclaw-autoclaw/agents，
// setupFromConfig 正常创建 autoclaw JSONLWatcher。
func TestSetupFromConfig_AutoClawDefaultPathBackfill(t *testing.T) {
	tmpDir := t.TempDir()
	cfgTemplate := `
data_dir = "%s"
[clients.autoclaw]
enabled = true
[daemon]
poll_interval = 1
`
	cfgContent := strings.ReplaceAll(cfgTemplate, "%s", tmpDir)
	cfgPath := filepath.Join(tmpDir, "config.toml")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	// home=tmpDir，ApplyDefaults 回填 sessions_dir = tmpDir/.openclaw-autoclaw/agents
	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

	sessionsDir := cfg.Clients["autoclaw"].Paths["sessions_dir"]
	wantSuffix := filepath.Join(".openclaw-autoclaw", "agents")
	if sessionsDir == "" {
		t.Fatalf("ApplyDefaults 应回填 autoclaw sessions_dir，实际为空")
	}
	if !strings.HasSuffix(sessionsDir, wantSuffix) {
		t.Errorf("回填 sessions_dir = %q, 应以 %q 结尾", sessionsDir, wantSuffix)
	}

	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)
	var found bool
	for _, w := range a.jsonlWatchers {
		if w.clientName == "autoclaw" {
			found = true
		}
	}
	if !found {
		t.Errorf("默认路径回填后应创建 autoclaw watcher, got %v", watcherClientNames(a.jsonlWatchers))
	}
}

// watcherClientNames 收集 watcher 的 clientName 列表（错误信息可读）。
func watcherClientNames(ws []*JSONLWatcher) []string {
	names := make([]string, 0, len(ws))
	for _, w := range ws {
		names = append(names, w.clientName)
	}
	return names
}
