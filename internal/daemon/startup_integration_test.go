// internal/daemon/startup_integration_test.go
//
// 交接窗口集成测试。
//
// 验证 daemon 启动时 startup catch-up 关闭 stop→collect→start 数据窗口：
// 在 monitor ready barrier 之前「注入」的数据（即手工 collect 完成后、watcher/poller
// ready 前产生的差量），由 startup catch-up 入库，无需等待第二次源变化。
//
// 确定性保证：
//   - 临时 HOME / data_dir / 数据源（t.TempDir），不触碰真实用户环境。
//   - ready barrier 用 analyzer 生产级 Ready() channel 控制：测试显式等待
//     <-a.Ready() 再做断言，不 sleep。
//   - 「注入数据」用 ExecuteFunc 返回预设消息并写入真实 usage DB（复用 persistClientBatch
//     的入库语义）；「不需要第二次源变化」= 测试在 ready 后不修改任何数据源文件，
//     若 DB 出现预期消息，必来自 startup catch-up 的 Submit。
//   - bounded 等待用 channel / 轮询 runtime-state / 轮询 Submit 记录，无裸 time.Sleep。
//
// 覆盖：
//   - SQLite cursor client（OpenCode/ZCode）的 incremental catch-up。
//   - Claude / WorkBuddy / AutoClaw JSONL 的无日期全扫 catch-up。
//   - Codex state incremental + rollout full scan 双请求 catch-up。
//   - router cursor 的 Source=router incremental catch-up。
//   - catch-up 与 ready 后实时事件两种先后（catch-up 先 / 实时事件先均最终入库）。
//   - 单项失败仍继续且 status/errors 可观察（final state=failed + 准确 failures 计数）。
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === 集成测试辅助 ===

// catchUpCall 记录一次 Submit 的关键签名，用于断言请求矩阵与顺序。
type catchUpCall struct {
	client      string
	source      string
	incremental bool
}

// recordingExecute 构造一个 ExecuteFunc：按 (client, source, incremental) 三元组查 payload，
// 命中则把预设消息写入真实 usage DB（模拟 persistClientBatch 的入库效果），并记录每次调用。
//
// 与生产 engine.RunCollect 的关系：生产路径由 collector 读源→RunCollect→事务 UpsertMessages。
// 这里把「读源 + 入库」合并进 ExecuteFunc，目的是在 daemon 级别确定性验证 coordinator 驱动的
// catch-up 闭合交接窗口，而不重放五种 collector 的解析格式（格式正确性由 collector/engine
// 各自的测试覆盖）。failOn 命中的调用返回指定错误，模拟单项采集失败。
type recordingExecute struct {
	mu       sync.Mutex
	calls    []catchUpCall
	usageDB  *db.DB
	payloads map[string][]model.Message // key = sourceKey(client,source,incr) → 注入的消息
	failOn   map[string]error           // key = sourceKey → 该请求返回错误
	callN    int32
}

func sourceKey(client, source string, incr bool) string {
	return fmt.Sprintf("%s|%s|%v", client, source, incr)
}

func (e *recordingExecute) execute(ctx context.Context, client string, req collector.CollectRequest) error {
	atomic.AddInt32(&e.callN, 1)
	e.mu.Lock()
	e.calls = append(e.calls, catchUpCall{client: client, source: req.Source, incremental: req.Incremental})
	key := sourceKey(client, req.Source, req.Incremental)
	msgs := e.payloads[key]
	fail := e.failOn[key]
	e.mu.Unlock()

	// 模拟单项失败（不计入 DB，但仍记录调用顺序）。
	if fail != nil {
		return fail
	}
	// 入库：把预设消息持久化到 usage DB（对应 persistClientBatch 的 UpsertMessages）。
	if e.usageDB != nil && len(msgs) > 0 {
		if err := persistTestMessages(ctx, e.usageDB, msgs); err != nil {
			return err
		}
	}
	return nil
}

func (e *recordingExecute) snapshot() []catchUpCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]catchUpCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// persistTestMessages 把消息写入真实 usage DB（复用 db.UpsertMessages 的事务语义）。
// 这是 engine.persistClientBatch 的最小入库等价：单事务 UpsertMessages + Commit。
func persistTestMessages(ctx context.Context, usageDB *db.DB, msgs []model.Message) error {
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启写事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := db.UpsertMessages(ctx, tx, msgs); err != nil {
		return fmt.Errorf("保存 messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交写事务: %w", err)
	}
	return nil
}

// countMessagesByClient 统计 usage DB 中某 client 显示名的 messages 行数。
// 用于断言「注入的数据由 catch-up 入库」。
func countMessagesByClient(t *testing.T, usageDB *db.DB, client string) int {
	t.Helper()
	var n int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = ?`, client).Scan(&n); err != nil {
		t.Fatalf("count messages for %s: %v", client, err)
	}
	return n
}

// waitForSubmitCalls bounded 轮询：等待 recordingExecute.calls 达到 want 个或超时。
// 替代裸 time.Sleep：ready 后 coordinator 同步串行 Submit，轮询其记录长度即可确定性同步。
func waitForSubmitCalls(t *testing.T, e *recordingExecute, want int, timeout time.Duration) []catchUpCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := e.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return e.snapshot()
}

// waitForRuntimeStateCatchUp 轮询磁盘 runtime-state 直到 catch_up 到达目标阶段。
func waitForRuntimeStateCatchUp(t *testing.T, statePath string, wantPhase string, timeout time.Duration) *runmetaRuntimeStateOnDisk {
	t.Helper()
	return waitForRuntimeState(t, statePath, timeout, func(st *runmetaRuntimeStateOnDisk) bool {
		return st.MonitorReady && st.CatchUp == wantPhase
	})
}

// === TestStartupCatchUp_*：交接窗口确定性集成测试 ===

// TestStartupCatchUp_AllSourceTypes_ReadyBeforeInject 验证 ready 前注入的数据可被补采：
// daemon 启动 → watcher/poller ready → coordinator 顺序 Submit 各 client/source 的 catch-up
// 请求 → 注入数据入库。关键：ready 后不再修改数据源，DB 出现的消息必来自 catch-up
// （证明「不需第二次源变化」）。
//
// 覆盖的 catch-up 请求矩阵（catchUpRequestsFor + routerCatchUpRequest）：
//   - opencode: client-source Incremental=true（SQLite cursor）
//   - zcode: client-source Incremental=true（SQLite cursor）
//   - claude: client-source 无日期全扫（Incremental=false，ScanExistingJSONL=true）
//   - workbuddy: client-source 无日期全扫（Incremental=false，ScanExistingJSONL=true）
//   - autoclaw: client-source 无日期全扫（Incremental=false，ScanExistingJSONL=true）
//   - codex: 两个 client-source 请求——先 Incremental=true（state cursor），再全扫（false，
//     ScanExistingJSONL=true）
//   - router: codex + opencode 各一个 Source=router Incremental=true
//
// 单个 analyzer 同时挂 claude/codex/workbuddy/autoclaw watcher + opencode/zcode/codex-state/router poller，
// ready barrier 在全部 monitor 初始化后关闭，coordinator 随后串行 Submit 全部 catch-up。
func TestStartupCatchUp_AllSourceTypes_ReadyBeforeInject(t *testing.T) {
	tmpDir := t.TempDir()
	// 启用全部六类 client + codex/opencode 配 cc_switch router（router catch-up 也覆盖）。
	// buildConfigWithCodexState 预创建 codex state 文件使 state poller 能建（monitor > 0）。
	cfg := buildConfigWithCodexState(t, tmpDir,
		[]string{"claude", "codex", "opencode", "workbuddy", "zcode", "autoclaw"},
		map[string]bool{"codex": true, "opencode": true})

	// 真实 usage DB。
	usageDB, err := db.Open(filepath.Join(tmpDir, "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	defer usageDB.Close()

	// 预设「ready 前注入的数据」：每个 catch-up 请求三元组对应一批消息。
	exec := &recordingExecute{
		usageDB: usageDB,
		payloads: map[string][]model.Message{
			// SQLite cursor client（Incremental=true）。
			sourceKey("opencode", collector.CollectSourceClient, true): {
				{ID: "oc-1", Client: model.ClientOpenCode, Date: "2026-07-29", SessionID: "s-oc", TotalTokens: 10},
			},
			sourceKey("zcode", collector.CollectSourceClient, true): {
				{ID: "zc-1", Client: model.ClientZCode, Date: "2026-07-29", SessionID: "s-zc", TotalTokens: 20},
			},
			// JSONL client 无日期全扫（Incremental=false）。
			sourceKey("claude", collector.CollectSourceClient, false): {
				{ID: "cl-1", Client: model.ClientClaudeCode, Date: "2026-07-29", SessionID: "s-cl", TotalTokens: 30},
			},
			sourceKey("workbuddy", collector.CollectSourceClient, false): {
				{ID: "wb-1", Client: model.ClientWorkBuddy, Date: "2026-07-29", SessionID: "s-wb", TotalTokens: 40},
			},
			// AutoClaw JSONL 无日期全扫（Incremental=false）。
			sourceKey("autoclaw", collector.CollectSourceClient, false): {
				{ID: "ac-1", Client: model.ClientZhipuAutoClaw, Date: "2026-07-29", SessionID: "s-ac", TotalTokens: 70},
			},
			// Codex 两个请求：state Incremental=true，rollout 全扫 false。
			sourceKey("codex", collector.CollectSourceClient, true): {
				{ID: "cx-st", Client: model.ClientCodexCLI, Date: "2026-07-29", SessionID: "s-cx-st", TotalTokens: 50},
			},
			sourceKey("codex", collector.CollectSourceClient, false): {
				{ID: "cx-ro", Client: model.ClientCodexCLI, Date: "2026-07-29", SessionID: "s-cx-ro", TotalTokens: 60},
			},
			// Router（Source=router, Incremental=true）。
			sourceKey("codex", collector.CollectSourceRouter, true): {
				{ID: "cx-rt", Client: model.ClientCodexCLI, Date: "2026-07-29", SessionID: "s-cx-rt", TotalTokens: 70},
			},
			sourceKey("opencode", collector.CollectSourceRouter, true): {
				{ID: "oc-rt", Client: model.ClientOpenCode, Date: "2026-07-29", SessionID: "s-oc-rt", TotalTokens: 80},
			},
		},
	}

	a := analyzer.NewFromConfig(cfg, exec.execute, nil, 100*time.Millisecond)

	// 正常 state writer：写到真实临时 state 文件。
	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a, okWrite, 4242, "inst-catchup", nil)
	}()

	// ready-gated：等 ready barrier 关闭（确定性同步点，不 sleep）。
	select {
	case <-a.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for analyzer ready barrier")
	}

	// ready 后不再修改任何数据源。等 coordinator 串行 Submit 全部 catch-up 请求。
	// 期望请求数：claude(1)+codex(2)+opencode(1)+workbuddy(1)+zcode(1)+autoclaw(1) = 7 client-source
	//          + codex router(1) + opencode router(1) = 9。
	const wantCalls = 9
	got := waitForSubmitCalls(t, exec, wantCalls, 5*time.Second)
	if len(got) < wantCalls {
		cancel()
		t.Fatalf("catch-up calls = %d, want %d (got=%+v)", len(got), wantCalls, got)
	}

	// 等待 runtime-state 落到 succeeded（catch-up 全部成功）。
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 3*time.Second); st == nil {
		cancel()
		t.Fatalf("runtime-state 未到 succeeded，calls=%+v", got)
	}

	cancel()
	<-done

	// 断言所有 client 的注入消息均入库，无需第二次源变化。
	if n := countMessagesByClient(t, usageDB, model.ClientClaudeCode); n != 1 {
		t.Errorf("claude messages = %d, want 1", n)
	}
	if n := countMessagesByClient(t, usageDB, model.ClientOpenCode); n != 2 { // oc client(1) + oc router(1)
		t.Errorf("opencode messages = %d, want 2 (client + router)", n)
	}
	if n := countMessagesByClient(t, usageDB, model.ClientZCode); n != 1 {
		t.Errorf("zcode messages = %d, want 1", n)
	}
	if n := countMessagesByClient(t, usageDB, model.ClientWorkBuddy); n != 1 {
		t.Errorf("workbuddy messages = %d, want 1", n)
	}
	if n := countMessagesByClient(t, usageDB, model.ClientZhipuAutoClaw); n != 1 {
		t.Errorf("autoclaw messages = %d, want 1", n)
	}
	if n := countMessagesByClient(t, usageDB, model.ClientCodexCLI); n != 3 { // state(1) + rollout(1) + router(1)
		t.Errorf("codex messages = %d, want 3 (state + rollout + router)", n)
	}
}

// TestStartupCatchUp_RealtimeEventBeforeCatchUp 覆盖 ready 后实时事件
// 可能早于 catch-up 获得串行锁（设计文档：允许重复采集，保证最终覆盖）。
//
// 构造：手动经 a.Submit 提交一个「实时事件」请求，与 catch-up 并发争串行锁；
// 无论谁先，两者最终都入库（幂等 upsert 保证最终覆盖，不丢事件）。
func TestStartupCatchUp_RealtimeEventBeforeCatchUp(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	usageDB, err := db.Open(filepath.Join(tmpDir, "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	defer usageDB.Close()

	// ExecuteFunc 按「调用签名」决定入库哪条消息：
	//   - catch-up 请求（Incremental=false, 无 ChangedFile）→ cl-catchup
	//   - 实时事件（ChangedFile 非空）→ cl-realtime
	// 两条都经 execute 入库，验证两种请求共用同一串行入口、最终都落库（与先后无关）。
	catchUpMsg := model.Message{ID: "cl-catchup", Client: model.ClientClaudeCode, Date: "2026-07-29", SessionID: "s-catchup", TotalTokens: 30}
	realtimeMsg := model.Message{ID: "cl-realtime", Client: model.ClientClaudeCode, Date: "2026-07-29", SessionID: "s-realtime", TotalTokens: 31}
	exec := &recordingExecute{
		usageDB: usageDB,
		payloads: map[string][]model.Message{
			sourceKey("claude", collector.CollectSourceClient, false): {catchUpMsg},
		},
	}

	// 包装 execute：实时事件（ChangedFile 非空）入库 realtimeMsg。
	origExecute := exec.execute
	wrapExecute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		if req.ChangedFile != "" {
			// 实时事件：直接入库 realtimeMsg（catch-up payload 表不命中 ChangedFile 签名）。
			exec.mu.Lock()
			exec.calls = append(exec.calls, catchUpCall{client: client, source: req.Source, incremental: req.Incremental})
			exec.mu.Unlock()
			atomic.AddInt32(&exec.callN, 1)
			return persistTestMessages(ctx, exec.usageDB, []model.Message{realtimeMsg})
		}
		return origExecute(ctx, client, req)
	}

	a2 := analyzer.NewFromConfig(cfg, wrapExecute, nil, 100*time.Millisecond)

	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a2, okWrite, 1, "inst-race", nil)
	}()

	// 等 ready。
	select {
	case <-a2.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for ready")
	}

	// ready 后立即手动提交一个「实时事件」（ChangedFile 模式）。
	// 它与 coordinator 的 catch-up 并发争串行锁：无论先后，两条消息最终都入库。
	realtimeDone := make(chan error, 1)
	go func() {
		realtimeDone <- a2.Submit(ctx, "claude", collector.CollectRequest{ChangedFile: "/tmp/injected.jsonl"})
	}()

	// 等 catch-up（claude 一个 client-source）+ 实时事件都执行完。
	// 期望总 calls >= 2（catch-up 1 + realtime 1）。
	got := waitForSubmitCalls(t, exec, 2, 5*time.Second)
	if len(got) < 2 {
		cancel()
		t.Fatalf("calls = %d, want >=2 (catch-up + realtime), got=%+v", len(got), got)
	}

	// 关键：等 catch-up 完成（runtime-state succeeded 保证 catch-up 持久化已落盘）再 cancel，
	// 避免 catch-up 持久化进行中 cancel 导致 context canceled。
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 5*time.Second); st == nil {
		cancel()
		t.Fatal("runtime-state 未到 succeeded（catch-up 未完成）")
	}

	// 等实时事件 Submit 返回（它也经串行锁，可能在 catch-up 之后才拿到锁）。
	select {
	case err := <-realtimeDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("realtime submit err = %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("realtime submit did not return")
	}

	cancel()
	<-done

	// 无论先后，两条消息都最终入库：catch-up 与实时事件共用串行入口，不丢事件。
	if n := countMessagesByClient(t, usageDB, model.ClientClaudeCode); n != 2 {
		t.Errorf("claude messages = %d, want 2 (catch-up + realtime 各一条)", n)
	}
}

// TestStartupCatchUp_PartialFailureContinuesAndObservable 验证部分失败可观察：
// 单项采集失败时 coordinator 继续剩余请求，final runtime-state=failed 且 failures 计数准确。
// 模拟：codex state incremental（第一个请求）失败，rollout full scan 仍执行；router 也执行；
// final state 记录 catch_up=failed + failures=1。
func TestStartupCatchUp_PartialFailureContinuesAndObservable(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildConfigWithCodexState(t, tmpDir,
		[]string{"codex"}, map[string]bool{"codex": true})

	usageDB, err := db.Open(filepath.Join(tmpDir, "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	defer usageDB.Close()

	exec := &recordingExecute{
		usageDB: usageDB,
		payloads: map[string][]model.Message{
			sourceKey("codex", collector.CollectSourceClient, false): {
				{ID: "cx-ro", Client: model.ClientCodexCLI, Date: "2026-07-29", SessionID: "s-ro", TotalTokens: 60},
			},
			sourceKey("codex", collector.CollectSourceRouter, true): {
				{ID: "cx-rt", Client: model.ClientCodexCLI, Date: "2026-07-29", SessionID: "s-rt", TotalTokens: 70},
			},
		},
		// 第一个请求（state Incremental=true）失败：模拟单项采集故障。
		failOn: map[string]error{
			sourceKey("codex", collector.CollectSourceClient, true): errors.New("codex state read boom"),
		},
	}

	a := analyzer.NewFromConfig(cfg, exec.execute, nil, 100*time.Millisecond)

	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a, okWrite, 7, "inst-fail", nil)
	}()

	select {
	case <-a.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for ready")
	}

	// codex 三个请求都应执行（state incr 失败不跳过 rollout + router）。
	got := waitForSubmitCalls(t, exec, 3, 5*time.Second)
	if len(got) < 3 {
		cancel()
		t.Fatalf("calls = %d, want 3 (partial failure must not skip), got=%+v", len(got), got)
	}

	// final state 应为 failed + failures=1（status/errors 可观察）。
	st := waitForRuntimeStateCatchUp(t, statePath, phaseFailed, 3*time.Second)
	if st == nil {
		cancel()
		t.Fatalf("runtime-state 未到 failed，calls=%+v", got)
	}
	if st.CatchUpFailures != 1 {
		t.Errorf("catch_up_failures = %d, want 1 (only codex state incremental failed)", st.CatchUpFailures)
	}

	cancel()
	<-done

	// 失败的请求不入库（codex state 增量消息缺失），但 rollout + router 仍入库。
	if n := countMessagesByClient(t, usageDB, model.ClientCodexCLI); n != 2 {
		t.Errorf("codex messages = %d, want 2 (rollout + router; state incremental failed)", n)
	}
}

// TestStartupCatchUp_RequestOrderDeterministic 验证请求顺序稳定：
// ready 后 coordinator 按 client 名升序、每个 client 先全部 client-source 再 router。
// 用 codex(router) + opencode(router) + claude 验证稳定顺序（不受 map 迭代顺序影响）。
func TestStartupCatchUp_RequestOrderDeterministic(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildConfigWithCodexState(t, tmpDir,
		[]string{"claude", "codex", "opencode"},
		map[string]bool{"codex": true, "opencode": true})

	usageDB, err := db.Open(filepath.Join(tmpDir, "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	defer usageDB.Close()

	exec := &recordingExecute{usageDB: usageDB}

	a := analyzer.NewFromConfig(cfg, exec.execute, nil, 100*time.Millisecond)

	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a, okWrite, 1, "inst-order", nil)
	}()

	select {
	case <-a.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for ready")
	}

	// claude(1) + codex(2 client + 1 router) + opencode(1 client + 1 router) = 6.
	const wantCalls = 6
	got := waitForSubmitCalls(t, exec, wantCalls, 5*time.Second)
	if len(got) < wantCalls {
		cancel()
		t.Fatalf("calls = %d, want %d, got=%+v", len(got), wantCalls, got)
	}

	cancel()
	<-done

	// 期望顺序：claude(client) → codex(client incr) → codex(client fullscan) → codex(router)
	//        → opencode(client incr) → opencode(router)。
	want := []catchUpCall{
		{"claude", collector.CollectSourceClient, false},
		{"codex", collector.CollectSourceClient, true},
		{"codex", collector.CollectSourceClient, false},
		{"codex", collector.CollectSourceRouter, true},
		{"opencode", collector.CollectSourceClient, true},
		{"opencode", collector.CollectSourceRouter, true},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestStartupCatchUp_NoSecondSourceChangeNeeded 验证无需第二次源变化：
// 在 ready barrier 关闭前「注入」数据源（手工 collect 后、watcher/poller ready 前的差量），
// daemon 启动后 catch-up 把它入库——证明不需要第二次源变化。
//
// 与 TestStartupCatchUp_AllSourceTypes 的区别：这里聚焦「注入时机在 ready 前」，
// 用 opencode 单 client + 在 a.Ready() 关闭前预先放好 payload，断言 ready 后立即入库。
func TestStartupCatchUp_NoSecondSourceChangeNeeded(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"opencode"}, nil)

	usageDB, err := db.Open(filepath.Join(tmpDir, "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	defer usageDB.Close()

	// 「ready 前注入的数据」：opencode SQLite cursor 增量差量。
	const msgID = "oc-handoff"
	exec := &recordingExecute{
		usageDB: usageDB,
		payloads: map[string][]model.Message{
			sourceKey("opencode", collector.CollectSourceClient, true): {
				{ID: msgID, Client: model.ClientOpenCode, Date: "2026-07-29", SessionID: "s-handoff", TotalTokens: 99},
			},
		},
	}

	a := analyzer.NewFromConfig(cfg, exec.execute, nil, 100*time.Millisecond)

	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a, okWrite, 1, "inst-handoff", nil)
	}()

	// 等 ready barrier 关闭（「ready 前」的注入已完成：payload 已在 ExecuteFunc 内就绪）。
	select {
	case <-a.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for ready")
	}

	// ready 后【不修改任何数据源】。等 catch-up 把 ready 前注入的数据入库。
	got := waitForSubmitCalls(t, exec, 1, 5*time.Second)
	if len(got) < 1 {
		cancel()
		t.Fatalf("catch-up 未在 ready 后执行 (calls=%d)", len(got))
	}

	// 等 runtime-state succeeded。
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 3*time.Second); st == nil {
		cancel()
		t.Fatal("runtime-state 未到 succeeded")
	}

	cancel()
	<-done

	// 关键断言：ready 前注入的数据，由 catch-up 入库（无需第二次源变化）。
	if n := countMessagesByClient(t, usageDB, model.ClientOpenCode); n != 1 {
		t.Errorf("opencode messages = %d, want 1 (ready 前注入的数据应由 catch-up 入库)", n)
	}
}

// buildConfigWithCodexState 预创建 codex state_*.sqlite 文件后构造 config，
// 使 analyzer.setupFromConfig 的 Glob 命中从而建 codex state poller（保证 monitor 数 > 0）。
// buildEnabledClients 已为 codex 配 state_dir= tmpDir/codex，故 state 文件落在 tmpDir/codex/state/ 下。
// clients/routerClients 透传给 buildEnabledClients。
func buildConfigWithCodexState(t *testing.T, tmpDir string, clients []string, routerClients map[string]bool) *config.Config {
	t.Helper()
	// 预创建 codex state 文件（state_dir 在 buildEnabledClients 里是 tmpDir/codex，
	// setupFromConfig 的 Glob 匹配 state_dir/state_*.sqlite，故文件需在 tmpDir/codex/state/ 下）。
	if hasCodex(clients) {
		stateDir := filepath.Join(tmpDir, "codex", "state")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatalf("mkdir codex state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "state_5.sqlite"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write codex state: %v", err)
		}
	}
	return buildEnabledClients(t, tmpDir, clients, routerClients)
}

func hasCodex(clients []string) bool {
	for _, c := range clients {
		if c == "codex" {
			return true
		}
	}
	return false
}
