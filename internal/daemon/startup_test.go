// internal/daemon/startup_test.go
package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// loadEffectiveConfigForTest 复用 runtimecfg.LoadEffectiveConfig（daemon 测试已有模式）。
// 生产 daemon 代码不 import runtimecfg；测试辅助可用。
func loadEffectiveConfigForTest(cfgPath, home string) (*config.Config, error) {
	return runtimecfg.LoadEffectiveConfig(cfgPath, runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         "linux",
		DefaultPaths: runtimecfg.NewStandardProvider(),
	})
}

// === daemon startup coordinator 与 runtime-state ===
//
// 测试覆盖：
//   - 四类数据源 catch-up request 矩阵（catchUpRequestsFor）；
//   - Codex 恰好生成两个串行请求（state incremental → rollout full scan），incremental 失败也执行 full scan；
//   - coordinator 请求顺序：client 名升序、每个 client 的 router 请求在全部 client-source 之后、
//     前序失败不跳过后续请求、多 client 输入顺序任意时执行顺序按 client 名稳定；
//   - ready barrier → 写 ready state(pending) → catch-up → running/succeeded/failed 阶段序列；
//   - ready 前 state 发布失败：不 Submit catch-up、回传 fatal error、cancel analyzer、完成 goroutine 回收；
//   - running/final state 写失败仍执行全部 catch-up / 继续 monitor，后续写入仍尝试；
//   - 单项采集失败继续累计准确数量；
//   - cancel 后无新增 submit/state 写入。

// ---- catchUpRequestsFor：四类数据源 request 矩阵 ----

// TestCatchUpRequestsFor_FourSourceMatrix 验证请求矩阵：
//   - opencode/zcode：单请求 Incremental=true（SQLite cursor 继续）
//   - claude/workbuddy/autoclaw：单请求无日期扫描现存 JSONL（Incremental=false，Dates 空，
//     ScanExistingJSONL=true——现存 JSONL 全扫的显式合同）
//   - codex：两个请求——先 Incremental=true（state cursor），再无日期全扫 rollout JSONL
//     （ScanExistingJSONL=true）
//   - router：不在本函数产出，由 coordinator 单独处理（Source=router, Incremental=true）
func TestCatchUpRequestsFor_FourSourceMatrix(t *testing.T) {
	cases := []struct {
		client   string
		wantLen  int
		wantIncr []bool // 按返回顺序的 Incremental 值
		wantScan []bool // 按返回顺序的 ScanExistingJSONL 值
	}{
		{"opencode", 1, []bool{true}, []bool{false}},
		{"zcode", 1, []bool{true}, []bool{false}},
		{"claude", 1, []bool{false}, []bool{true}},
		{"workbuddy", 1, []bool{false}, []bool{true}},
		{"autoclaw", 1, []bool{false}, []bool{true}},
		{"codex", 2, []bool{true, false}, []bool{false, true}}, // state incremental → rollout full scan
	}
	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			reqs := catchUpRequestsFor(tc.client)
			if len(reqs) != tc.wantLen {
				t.Fatalf("%s: got %d requests, want %d (reqs=%v)", tc.client, len(reqs), tc.wantLen, reqs)
			}
			for i, want := range tc.wantIncr {
				if reqs[i].Incremental != want {
					t.Errorf("%s req[%d].Incremental = %v, want %v", tc.client, i, reqs[i].Incremental, want)
				}
				if reqs[i].ScanExistingJSONL != tc.wantScan[i] {
					t.Errorf("%s req[%d].ScanExistingJSONL = %v, want %v", tc.client, i, reqs[i].ScanExistingJSONL, tc.wantScan[i])
				}
				// client-source 请求 Source 必为空或 client（不能是 router）
				if reqs[i].Source == collector.CollectSourceRouter {
					t.Errorf("%s req[%d].Source = router, want client-source", tc.client, i)
				}
			}
			// 无日期扫描请求的 Dates 必为空（claude/workbuddy/autoclaw/codex full scan）
			if tc.client == "claude" || tc.client == "workbuddy" || tc.client == "autoclaw" {
				if len(reqs[0].Dates) != 0 {
					t.Errorf("%s: Dates should be empty for no-date scan, got %v", tc.client, reqs[0].Dates)
				}
			}
		})
	}
}

// TestCatchUpRequestsFor_CodexTwoSerialIncrementalThenFullScan Codex 严格顺序：
// 第一请求 Incremental=true（推进 state cursor），第二请求无日期全扫 rollout。
// 第一失败也必须继续第二（coordinator 层面验证，这里只断言请求顺序与语义）。
func TestCatchUpRequestsFor_CodexTwoSerialIncrementalThenFullScan(t *testing.T) {
	reqs := catchUpRequestsFor("codex")
	if len(reqs) != 2 {
		t.Fatalf("codex: want 2 requests, got %d", len(reqs))
	}
	if !reqs[0].Incremental {
		t.Errorf("codex req[0]: Incremental=false, want true (state cursor advance)")
	}
	if reqs[0].ScanExistingJSONL {
		t.Errorf("codex req[0]: ScanExistingJSONL=true, want false (state incremental 非 JSONL 全扫)")
	}
	if reqs[1].Incremental {
		t.Errorf("codex req[1]: Incremental=true, want false (rollout full scan)")
	}
	if !reqs[1].ScanExistingJSONL {
		t.Errorf("codex req[1]: ScanExistingJSONL=false, want true (rollout full scan)")
	}
	if len(reqs[1].Dates) != 0 {
		t.Errorf("codex req[1]: Dates should be empty (full scan), got %v", reqs[1].Dates)
	}
}

// TestCatchUpRequestsFor_ScanExistingJSONLSingleProducer 入口区分的负向锚点：
// ScanExistingJSONL 是 daemon startup catch-up 现存 JSONL 全扫的显式合同，
// 唯一置 true 的生产点是本函数（codex rollout 全扫 + claude/workbuddy/autoclaw 全扫）。
// 非 catch-up 入口（CLI collect Dates 形态、collect all、collect retry、ChangedFile 事件、
// SQLite poller Incremental 形态）全部不得携带该标志——它们的 CollectRequest 构造点为
// 无标志字面量（internal/cli/collect.go、internal/engine/retry.go、internal/analyzer/），
// 行为级断言见 engine 层 RunCollect 请求透传测试。
func TestCatchUpRequestsFor_ScanExistingJSONLSingleProducer(t *testing.T) {
	for _, client := range []string{"opencode", "zcode", "unknown-client"} {
		for _, req := range catchUpRequestsFor(client) {
			if req.ScanExistingJSONL {
				t.Errorf("%s: ScanExistingJSONL=true, want false (SQLite cursor 类 client 无 JSONL 全扫)", client)
			}
		}
	}
}

// TestCatchUpRequestsFor_UnknownClientEmpty 未知 client 返回空切片（不 panic）。
func TestCatchUpRequestsFor_UnknownClientEmpty(t *testing.T) {
	reqs := catchUpRequestsFor("unknown-client")
	if len(reqs) != 0 {
		t.Fatalf("unknown client: want 0 requests, got %d (%v)", len(reqs), reqs)
	}
}

// ---- coordinator 请求顺序 ----

// fakeSubmitRecorder 记录每个 Submit 调用的 (client, req.Source, req.Incremental)，
// 并按配置返回错误，用于断言 coordinator 的请求顺序与失败计数。
type fakeSubmitRecorder struct {
	mu      sync.Mutex
	calls   []submitCall
	failOn  map[int]error // 第 i 次调用（0-based）返回的错误
	callCnt int32
}
type submitCall struct {
	client      string
	source      string
	incremental bool
}

func (r *fakeSubmitRecorder) submit(ctx context.Context, client string, req collector.CollectRequest) error {
	idx := int(atomic.AddInt32(&r.callCnt, 1)) - 1
	r.mu.Lock()
	r.calls = append(r.calls, submitCall{client: client, source: req.Source, incremental: req.Incremental})
	r.mu.Unlock()
	if e, ok := r.failOn[idx]; ok {
		return e
	}
	return nil
}

func (r *fakeSubmitRecorder) snapshot() []submitCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]submitCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// fakeStateRecorder 记录每次 WriteRuntimeState 的 state，并可注入失败。
type fakeStateRecorder struct {
	mu     sync.Mutex
	writes []runmetaState
	failOn map[int]error // 第 i 次写入（0-based）失败
	writeN int32
}

func (r *fakeStateRecorder) write(st runmetaState) error {
	idx := int(atomic.AddInt32(&r.writeN, 1)) - 1
	r.mu.Lock()
	r.writes = append(r.writes, st)
	r.mu.Unlock()
	if e, ok := r.failOn[idx]; ok {
		return e
	}
	return nil
}

func (r *fakeStateRecorder) snapshot() []runmetaState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]runmetaState, len(r.writes))
	copy(out, r.writes)
	return out
}

// stateWriterFunc（生产侧定义，测试通过方法值适配）。
// fakeStateRecorder.write 直接作为 stateWriterFunc 注入。

// buildEnabledClients 构造一个含若干启用 client 的 config（路径非空，让 enabled 成立）。
// routerClients 指明哪些 client 配了 router（router name 固定 "cc_switch"）。
func buildEnabledClients(t *testing.T, tmpDir string, clients []string, routerClients map[string]bool) *config.Config {
	t.Helper()
	for _, c := range clients {
		dir := filepath.Join(tmpDir, c)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", c, err)
		}
	}
	// 用 TOML 构造，再用现有 runtimecfg.LoadEffectiveConfig 解析（保持与 daemon_test 一致）。
	cfgPath := filepath.Join(tmpDir, "config.toml")
	content := `data_dir = "` + tmpDir + `"
[daemon]
poll_interval = 1
[log]
level = "info"
dir = "` + tmpDir + `/logs"
max_days = 7
`
	for _, c := range clients {
		content += `
[clients.` + c + `]
enabled = true
`
		if routerClients[c] {
			content += `router = "cc_switch"
`
		}
		content += `[clients.` + c + `.paths]
`
		switch c {
		case "claude", "workbuddy":
			content += `projects_dir = "` + filepath.Join(tmpDir, c) + `"` + "\n"
		case "codex":
			content += `sessions_dir = "` + filepath.Join(tmpDir, c) + `"` + "\n"
			content += `state_dir = "` + filepath.Join(tmpDir, c) + `"` + "\n"
		case "autoclaw":
			content += `sessions_dir = "` + filepath.Join(tmpDir, c) + `"` + "\n"
		default: // opencode, zcode
			content += `db = "` + filepath.Join(tmpDir, c, c+".db") + `"` + "\n"
		}
	}
	if len(routerClients) > 0 {
		content += `
[routers.cc_switch]
db_path = "` + filepath.Join(tmpDir, "router.db") + `"
`
	}
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadEffectiveConfigForTest(cfgPath, tmpDir)
	if err != nil {
		t.Fatalf("load effective config: %v", err)
	}
	return cfg
}

// TestCoordinator_RequestOrder_ClientNameAsc 多 client 输入顺序任意时，执行顺序按 client 名升序。
func TestCoordinator_RequestOrder_ClientNameAsc(t *testing.T) {
	tmpDir := t.TempDir()
	// 故意用非字典序的 client 列表构造，断言 coordinator 内部排序后按 client 名升序执行。
	cfg := buildEnabledClients(t, tmpDir, []string{"zcode", "claude", "opencode"}, nil)

	rec := &fakeSubmitRecorder{}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 123, "inst-1", nil)

	// 直接驱动 catch-up（不经 ready 等待），用测试专用入口观察请求顺序。
	coord.runCatchUp(context.Background())

	got := rec.snapshot()
	// client-source 顺序应为 claude → opencode → zcode。
	var clientOrder []string
	for _, c := range got {
		clientOrder = append(clientOrder, c.client)
	}
	wantOrder := []string{"claude", "opencode", "zcode"}
	if len(clientOrder) != len(wantOrder) {
		t.Fatalf("client calls = %v, want %v", clientOrder, wantOrder)
	}
	for i, w := range wantOrder {
		if clientOrder[i] != w {
			t.Fatalf("client call[%d] = %s, want %s (full order: %v)", i, clientOrder[i], w, clientOrder)
		}
	}
}

// TestCoordinator_RequestOrder_RouterAfterClientSource 每个 client 的 router 请求在该 client
// 全部 client-source 请求之后；前序失败不改顺序、不跳过。
// 用带 router 的 codex：Codex state incremental → Codex rollout full scan → Codex router incremental。
func TestCoordinator_RequestOrder_RouterAfterClientSource(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"codex"}, map[string]bool{"codex": true})

	rec := &fakeSubmitRecorder{}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 123, "inst-1", nil)
	coord.runCatchUp(context.Background())

	got := rec.snapshot()
	// 固定顺序：codex incr(client) → codex fullscan(client) → codex router
	want := []struct {
		client      string
		source      string
		incremental bool
	}{
		{"codex", collector.CollectSourceClient, true},  // state incremental
		{"codex", collector.CollectSourceClient, false}, // rollout full scan
		{"codex", collector.CollectSourceRouter, true},  // router incremental
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %d (%+v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].client != w.client || got[i].source != w.source || got[i].incremental != w.incremental {
			t.Fatalf("call[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestCoordinator_RequestOrder_FailureDoesNotSkip 前序请求失败不跳过当前 client 后续请求、
// router 请求或后续 client。失败数按失败请求计数（Codex 两个请求：incremental 失败也执行 full scan）。
func TestCoordinator_RequestOrder_FailureDoesNotSkip(t *testing.T) {
	tmpDir := t.TempDir()
	// claude + codex(router)。codex 第 0 请求（state incremental）失败。
	cfg := buildEnabledClients(t, tmpDir, []string{"claude", "codex"}, map[string]bool{"codex": true})

	rec := &fakeSubmitRecorder{
		failOn: map[int]error{1: errors.New("codex state incr boom")}, // call idx 0=claude,1=codex incr
	}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 123, "inst-1", nil)
	failures := coord.runCatchUp(context.Background())

	got := rec.snapshot()
	// 期望全部 5 个请求都被执行：claude(1) + codex incr(1) + codex fullscan(1) + codex router(1) = 4
	// 顺序：claude → codex incr → codex fullscan → codex router
	if len(got) != 4 {
		t.Fatalf("executed calls = %d (%+v), want 4 (no skip on failure)", len(got), got)
	}
	wantSeq := []string{"claude", "codex", "codex", "codex"}
	for i, w := range wantSeq {
		if got[i].client != w {
			t.Fatalf("call[%d].client = %s, want %s", i, got[i].client, w)
		}
	}
	// 失败计数 = 1（只有 codex state incremental 失败）
	if failures != 1 {
		t.Fatalf("failures = %d, want 1 (only codex state incremental failed)", failures)
	}
}

// TestCoordinator_RequestOrder_AllClientsThenNotGlobalRouter 不采用「全部 client 请求完成后再统一 router」：
// router 请求紧跟各自 client。用 codex(router) + opencode(router) 验证：
// codex source*2 → codex router → opencode source → opencode router（不出现 codex source*2 → opencode source → codex router → opencode router）。
func TestCoordinator_RequestOrder_AllClientsThenNotGlobalRouter(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"codex", "opencode"}, map[string]bool{"codex": true, "opencode": true})

	rec := &fakeSubmitRecorder{}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 123, "inst-1", nil)
	coord.runCatchUp(context.Background())

	got := rec.snapshot()
	// 期望：codex incr → codex fullscan → codex router → opencode → opencode router
	want := []struct {
		client string
		source string
	}{
		{"codex", collector.CollectSourceClient},
		{"codex", collector.CollectSourceClient},
		{"codex", collector.CollectSourceRouter},
		{"opencode", collector.CollectSourceClient},
		{"opencode", collector.CollectSourceRouter},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %d (%+v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].client != w.client || got[i].source != w.source {
			t.Fatalf("call[%d] = {client:%s source:%s}, want {client:%s source:%s}", i, got[i].client, got[i].source, w.client, w.source)
		}
	}
}

// ---- startupCoordinator.run：ready barrier → state 阶段 → fatal channel → 降级 ----

// statePhase 常量（生产侧定义 phasePending/phaseRunning/phaseSucceeded/phaseFailed）。

// TestCoordinator_Run_HappyPath_PhaseSequence ready state(pending) → running → succeeded 完整序列。
// 无采集失败 → final=succeeded, failures=0。
func TestCoordinator_Run_HappyPath_PhaseSequence(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{}
	stateRec := &fakeStateRecorder{}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 4242, "inst-happy", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	// 在 close ready 前不应有任何 state 写入（所有 monitor ready 前没有 state）。
	if snaps := stateRec.snapshot(); len(snaps) != 0 {
		t.Fatalf("state written before ready: %+v", snaps)
	}
	close(ready)

	// 等 coordinator 收尾（catch-up 同步执行后返回）。
	<-done

	snaps := stateRec.snapshot()
	// 期望 3 次写入：pending → running → succeeded。
	if len(snaps) != 3 {
		t.Fatalf("state writes = %d (%+v), want 3 (pending/running/succeeded)", len(snaps), snaps)
	}
	// pending：monitor_ready=true, catch_up=pending, failures=0, pid/instanceID 准确。
	if snaps[0].monitorReady != true || snaps[0].catchUp != phasePending || snaps[0].catchUpFailures != 0 {
		t.Errorf("pending state = %+v, want {monitorReady:true catchUp:pending failures:0}", snaps[0])
	}
	if snaps[0].pid != 4242 || snaps[0].instanceID != "inst-happy" {
		t.Errorf("pending state pid/instanceID = %d/%q, want 4242/inst-happy", snaps[0].pid, snaps[0].instanceID)
	}
	// running：monitor_ready=true, catch_up=running。
	if snaps[1].catchUp != phaseRunning || snaps[1].monitorReady != true {
		t.Errorf("running state = %+v, want catchUp=running monitorReady=true", snaps[1])
	}
	// succeeded：catch_up=succeeded, failures=0。
	if snaps[2].catchUp != phaseSucceeded || snaps[2].catchUpFailures != 0 {
		t.Errorf("final state = %+v, want catchUp=succeeded failures=0", snaps[2])
	}
	// happy path 不应回传 fatal。
	select {
	case err := <-fatal:
		t.Fatalf("happy path should not send fatal, got %v", err)
	default:
	}
}

// TestCoordinator_Run_ReadyStateFailure_SendsFatalNoCatchUp 初次 ready/pending state 写失败：
// 不 Submit catch-up、回传 fatal error、cancel 后 coordinator goroutine 退出（不泄漏）。
func TestCoordinator_Run_ReadyStateFailure_SendsFatalNoCatchUp(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{}
	stateRec := &fakeStateRecorder{
		failOn: map[int]error{0: errors.New("write ready state boom")}, // 第 0 次写入（pending）失败
	}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 1, "inst-fatal", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	close(ready)

	// 应回传 fatal error。
	select {
	case err := <-fatal:
		if err == nil || err.Error() != "write ready state boom" {
			t.Fatalf("fatal = %v, want \"write ready state boom\"", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for fatal after ready state write failure")
	}

	// coordinator 应已退出（goroutine 回收）。
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator goroutine did not exit after fatal")
	}

	// 不应 Submit 任何 catch-up（ready state 发布失败不得开始 catch-up）。
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("catch-up submitted after ready state failure: %+v", calls)
	}
}

// TestCoordinator_Run_RunningStateFailure_StillCatchUp running state 写失败仍执行全部 catch-up，
// 且尝试写 final state：失败不停 daemon、继续执行、后续写入仍尝试。
func TestCoordinator_Run_RunningStateFailure_StillCatchUp(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"codex"}, nil) // codex = 2 client-source 请求

	rec := &fakeSubmitRecorder{}
	stateRec := &fakeStateRecorder{
		failOn: map[int]error{1: errors.New("running write boom")}, // 第 1 次写入（running）失败
	}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 7, "inst-r", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	close(ready)
	<-done

	// running 失败不应回传 fatal。
	select {
	case err := <-fatal:
		t.Fatalf("running state failure should NOT send fatal, got %v", err)
	default:
	}
	// 全部 2 个 client-source 请求仍执行（running 写失败不中断 catch-up）。
	if calls := rec.snapshot(); len(calls) != 2 {
		t.Fatalf("catch-up calls = %d, want 2 (running failure must not skip catch-up)", len(calls))
	}
	// 仍尝试写 final state（3 次写入：pending 成功、running 失败、final 成功）。
	snaps := stateRec.snapshot()
	if len(snaps) != 3 {
		t.Fatalf("state writes = %d, want 3 (running failed but pending+final attempted)", len(snaps))
	}
	// final 应为 succeeded（无采集失败）。
	if snaps[2].catchUp != phaseSucceeded {
		t.Errorf("final state = %+v, want catchUp=succeeded", snaps[2])
	}
}

// TestCoordinator_Run_FinalStateFailure_DaemonContinues final state 写失败 daemon 仍继续（不 fatal），
// 且 catch-up 已全部执行；采集有失败时 collection 事实仍保留（failures 计数由 catch-up 返回值决定，
// 与 state 写入失败无关）。
func TestCoordinator_Run_FinalStateFailure_DaemonContinues(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{
		failOn: map[int]error{0: errors.New("collect boom")}, // 单项采集失败
	}
	stateRec := &fakeStateRecorder{
		failOn: map[int]error{2: errors.New("final write boom")}, // final 写失败
	}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 9, "inst-f", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	close(ready)
	<-done

	// final 写失败不 fatal。
	select {
	case err := <-fatal:
		t.Fatalf("final state failure should NOT send fatal, got %v", err)
	default:
	}
	// 单项采集失败 → final state 的 catch_up 应为 failed、failures=1（由 catch-up 返回值决定，
	// 即使 final 写失败也仍尝试写带正确 failures 的 failed state）。
	snaps := stateRec.snapshot()
	if len(snaps) != 3 {
		t.Fatalf("state writes = %d, want 3", len(snaps))
	}
	// final（第 3 次写入，即便返回错误也记录了尝试的目标值）。
	last := snaps[2]
	if last.catchUp != phaseFailed || last.catchUpFailures != 1 {
		t.Errorf("final state = %+v, want catchUp=failed failures=1 (采集失败计数准确)", last)
	}
}

// TestCoordinator_Run_SingleFailureAccurateCount 单项采集失败继续累计准确数量：
// 多个请求，部分失败，final failures 计数 = 失败请求数。
func TestCoordinator_Run_SingleFailureAccurateCount(t *testing.T) {
	tmpDir := t.TempDir()
	// codex(2 client-source) + opencode(1) + claude(1) = 4 client-source 请求。
	cfg := buildEnabledClients(t, tmpDir, []string{"codex", "opencode", "claude"}, nil)

	rec := &fakeSubmitRecorder{
		// codex incr(idx0) 成功, codex fullscan(idx1) 失败, opencode(idx2) 失败, claude(idx3) 成功
		failOn: map[int]error{1: errors.New("f1"), 2: errors.New("f2")},
	}
	stateRec := &fakeStateRecorder{}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 11, "inst-c", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	close(ready)
	<-done

	calls := rec.snapshot()
	if len(calls) != 4 {
		t.Fatalf("catch-up calls = %d, want 4 (no skip)", len(calls))
	}
	snaps := stateRec.snapshot()
	// final = failed, failures = 2
	last := snaps[len(snaps)-1]
	if last.catchUp != phaseFailed || last.catchUpFailures != 2 {
		t.Errorf("final state = %+v, want catchUp=failed failures=2", last)
	}
}

// TestCoordinator_Run_CancelBeforeReady_NoStateWrite ctx 在 ready 前/时取消：不写 state、不 Submit。
func TestCoordinator_Run_CancelBeforeReady_NoStateWrite(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{}
	stateRec := &fakeStateRecorder{}
	ready := make(chan struct{}) // 不 close

	coord := newStartupCoordinator(cfg, rec.submit, stateRec.write, 12, "inst-cancel", nil)

	ctx, cancel := context.WithCancel(context.Background())
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	cancel() // 在 ready 前取消

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator did not exit after cancel before ready")
	}
	if snaps := stateRec.snapshot(); len(snaps) != 0 {
		t.Errorf("state written after cancel-before-ready: %+v", snaps)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("catch-up submitted after cancel: %+v", calls)
	}
	select {
	case <-fatal:
		t.Error("cancel-before-ready should not send fatal")
	default:
	}
}

// TestCoordinator_Run_CancelAfterReadyStopsCatchUp ctx 在 catch-up 中途取消：
// 正在跑的 Submit 经其 ctx 退出，coordinator 不再发起后续 Submit/写 final state（cancel 后无新增写入）。
// 用阻塞 Submit 模拟「catch-up 进行中」，cancel 后断言无新增 submit/state。
func TestCoordinator_Run_CancelAfterReadyStopsCatchUp(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude", "codex"}, nil)

	submitEntered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	submittedClients := int32(0)
	blockingSubmit := func(ctx context.Context, client string, req collector.CollectRequest) error {
		atomic.AddInt32(&submittedClients, 1)
		enteredOnce.Do(func() { close(submitEntered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	stateRec := &fakeStateRecorder{}
	ready := make(chan struct{})

	coord := newStartupCoordinator(cfg, blockingSubmit, stateRec.write, 13, "inst-c2", nil)

	ctx, cancel := context.WithCancel(context.Background())
	fatal := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		coord.run(ctx, ready, fatal)
		close(done)
	}()

	close(ready)
	<-submitEntered // 第一个 Submit 进入（claude，阻塞）；此时 ready state(pending)+running 已写

	// 记录 cancel 前的 state 写入数（pending + running = 2）。
	writesBeforeCancel := len(stateRec.snapshot())

	// 取消 child ctx：阻塞的 Submit 经 ctx.Done() 退出。
	cancel()

	// 等待 coordinator 退出。
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator did not exit after cancel during catch-up")
	}

	// 放行 release（防止泄漏）。
	close(release)

	// cancel 后不应发起新 Submit：只应有第一个进入的 claude（其它 client/codex 请求未发起）。
	// 注意：runCatchUp 串行 Submit，第一个返回后才会尝试第二个；cancel 后第二个不会发起。
	n := atomic.LoadInt32(&submittedClients)
	if n != 1 {
		t.Errorf("submitted clients after cancel = %d, want 1 (cancel must stop further submits)", n)
	}

	// cancel 后不应有新增 state 写入：
	// runCatchUp 返回后第 6 步被 ctx.Err() gate 拦截，final state 不写。
	writesAfter := len(stateRec.snapshot())
	if writesAfter != writesBeforeCancel {
		t.Errorf("state writes after cancel = %d, want %d (cancel must stop further state writes)",
			writesAfter, writesBeforeCancel)
	}
}

// ---- 集成：daemon.runAnalyzer 贯通 coordinator → runmeta.WriteRuntimeState 写磁盘 ----

// TestRun_Integration_RuntimeStateWrittenAfterReady daemon.Run（带 claude client + 真实 DB）
// 跑到 monitor ready 后，coordinator 应把 runtime-state 写到磁盘：
//   - PID/instanceID 与本次启动一致；
//   - monitor_ready=true；
//   - catch_up 最终为 succeeded（claude catch-up 无文件→空结果成功）。
//
// 用 eventually 风格轮询读 state 文件（短间隔 + deadline，替代裸 time.Sleep）：
// ready barrier 关闭是异步事件，集成层无确定性 channel 可等，只能轮询磁盘 state。
func TestRun_Integration_RuntimeStateWrittenAfterReady(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)
	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	pidPath := filepath.Join(tmpDir, "token-usage.pid")

	ctx, cancel := context.WithCancel(context.Background())
	// 后台跑 daemon；在验证完 state 后取消。
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, RunOptions{OpenResources: openTestResources(t, nil), InstanceID: "inst-int"})
	}()

	// eventually 轮询：读到 monitor_ready=true 的 state 或 deadline 超时。
	readyState := waitForRuntimeState(t, statePath, 5*time.Second,
		func(st *runmetaRuntimeStateOnDisk) bool { return st.MonitorReady })
	if readyState == nil {
		cancel()
		t.Fatal("timeout: runtime-state 未在 ready 后写入磁盘")
	}

	// ready state 的 PID/instanceID 准确：与 PID 文件一致且 instanceID=inst-int。
	pidFromFile, pidInst, err := readPIDFileOnDisk(pidPath)
	if err != nil {
		cancel()
		t.Fatalf("read pid file: %v", err)
	}
	if readyState.InstanceID != "inst-int" || readyState.PID != pidFromFile {
		cancel()
		t.Errorf("ready state pid/instanceID = %d/%q, want %d/inst-int", readyState.PID, readyState.InstanceID, pidFromFile)
	}
	if pidInst != "inst-int" {
		cancel()
		t.Errorf("pid file instanceID = %q, want inst-int", pidInst)
	}
	if !readyState.MonitorReady {
		cancel()
		t.Errorf("ready state monitor_ready = false, want true")
	}

	cancel()
	<-done
}

// ---- 集成测试辅助：读磁盘 state/PID ----

// waitForRuntimeState 是 eventually 风格轮询 helper：以短间隔轮询磁盘 state 文件，
// 直到 cond 返回 true 或 deadline 超时。返回命中的 state（超时返回 nil）。
// 用于集成测试等待 ready barrier 关闭后 coordinator 写出的 state（无确定性 channel 可等）。
func waitForRuntimeState(t *testing.T, path string, timeout time.Duration, cond func(*runmetaRuntimeStateOnDisk) bool) *runmetaRuntimeStateOnDisk {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := readRuntimeStateOnDisk(path); err == nil && cond(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// runmetaRuntimeStateOnDisk 是从磁盘读出的 runtime-state（用于集成测试断言）。
type runmetaRuntimeStateOnDisk struct {
	PID             int
	InstanceID      string
	MonitorReady    bool
	CatchUp         string
	CatchUpFailures int
}

func readRuntimeStateOnDisk(path string) (*runmetaRuntimeStateOnDisk, error) {
	st, err := runmeta.ReadRuntimeState(path)
	if err != nil {
		return nil, err
	}
	return &runmetaRuntimeStateOnDisk{
		PID:             st.PID,
		InstanceID:      st.InstanceID,
		MonitorReady:    st.MonitorReady,
		CatchUp:         st.CatchUp,
		CatchUpFailures: st.CatchUpFailures,
	}, nil
}

func readPIDFileOnDisk(path string) (int, string, error) {
	return runmeta.ReadPIDFile(path)
}

// ---- daemon 级 fatal→cancel→return 路径（runAnalyzerWithCoordinator）----

// TestRunAnalyzerWithCoordinator_ReadyStateFailure_CancelsAnalyzer daemon 级 fatal 路径：
// 注入始终失败的 state writer，runAnalyzerWithCoordinator 应：
//   - 回传 coordinator 的 fatal error（ready state 发布失败）；
//   - 立即 cancel child ctx 使阻塞的 Analyzer.Run 进入 shutdown 并返回（不 hang）；
//   - 完成 coordinator goroutine 回收；
//   - 不 Submit 任何 catch-up。
//
// 用真实 analyzer（claude watcher）+ 失败 state writer，验证 fatal 主动打断 a.Run 的正确性。
func TestRunAnalyzerWithCoordinator_ReadyStateFailure_CancelsAnalyzer(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{}
	// 构造一个真实 analyzer：Submit 走 recorder，Ready barrier 在 claude watcher 初始化后关闭。
	a := buildTestAnalyzer(t, cfg, rec.submit)

	// 始终失败的 state writer：模拟 ready/pending state 写盘失败。
	failWrite := stateWriterFunc(func(st runmetaState) error {
		return errors.New("disk write fatal")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- runAnalyzerWithCoordinator(ctx, cfg, a, failWrite, 99, "inst-daemon-fatal", nil)
	}()

	select {
	case err := <-done:
		if err == nil || err.Error() != "disk write fatal" {
			t.Fatalf("runAnalyzerWithCoordinator err = %v, want \"disk write fatal\"", err)
		}
		// 必须快速返回（fatal 主动 cancel a.Run，而非等 ctx 超时）。
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("fatal path took %v, want < 3s (should cancel analyzer, not hang)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAnalyzerWithCoordinator hung: fatal did not cancel Analyzer.Run")
	}

	// 不应 Submit 任何 catch-up（ready state 发布失败不得开始 catch-up）。
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("catch-up submitted after fatal: %+v", calls)
	}
}

// TestRunAnalyzerWithCoordinator_HappyPath daemon 级 happy path：
// 正常 state writer，Analyzer.Run 在 ctx 取消后返回，无 fatal。
//
// ready-gated（不 sleep）：等 ready barrier 关闭后，coordinator 同步串行执行 catch-up；
// 通过轮询 rec.snapshot() 长度确认 catch-up 已执行（claude=1 个 client-source 请求）后再 cancel，
// 保证 cancel 前采集事实已落定，避免裸 time.Sleep 带来的时序竞态。
func TestRunAnalyzerWithCoordinator_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)

	rec := &fakeSubmitRecorder{}
	a := buildTestAnalyzer(t, cfg, rec.submit)

	// 正常 state writer：写到真实临时 state 文件。
	statePath := filepath.Join(tmpDir, "token-usage.runtime.json")
	okWrite := stateWriterFunc(func(st runmetaState) error {
		return writeRuntimeStateToPath(statePath, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runAnalyzerWithCoordinator(ctx, cfg, a, okWrite, 100, "inst-daemon-ok", nil) }()

	// ready-gated：ready 后 coordinator 同步执行 catch-up（claude 1 个 client-source 请求）。
	// 轮询 rec 直到记录到该请求，确认 catch-up 已执行后再 cancel——确定性同步，无裸 sleep。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := rec.snapshot(); len(calls) < 1 {
		cancel()
		t.Fatalf("catch-up 未在 ready 后执行（rec.calls=%d），ready barrier 可能未关闭", len(calls))
	}

	cancel() // catch-up 已落定后再取消 Analyzer.Run。

	select {
	case err := <-done:
		// happy path：ctx 取消后 a.Run 优雅退出（nil）或 ctx.Err；无 fatal。
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("happy path err = %v, want nil or context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAnalyzerWithCoordinator hung on happy path")
	}

	// claude 的 catch-up 应已执行（恰好 1 个 client-source 请求）。
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Errorf("catch-up calls = %d, want 1 (claude client-source)", len(calls))
	}
}

// buildTestAnalyzer 构造一个真实 analyzer（与 runAnalyzer 装配一致），Submit 走注入的 submit。
// 复用 analyzer.NewFromConfig：cfg 必须含至少一个已启用 client（这里用 claude JSONL watcher）。
func buildTestAnalyzer(t *testing.T, cfg *config.Config, submit SubmitFunc) *analyzer.Analyzer {
	t.Helper()
	execute := analyzer.ExecuteFunc(func(ctx context.Context, client string, req collector.CollectRequest) error {
		return submit(ctx, client, req)
	})
	return analyzer.NewFromConfig(cfg, execute, nil, 100*time.Millisecond)
}

// writeRuntimeStateToPath 把 runmetaState 写到指定路径（适配 runmeta.WriteRuntimeState）。
func writeRuntimeStateToPath(path string, st runmetaState) error {
	return runmeta.WriteRuntimeState(path, runmeta.RuntimeState{
		PID:             st.pid,
		InstanceID:      st.instanceID,
		MonitorReady:    st.monitorReady,
		CatchUp:         st.catchUp,
		CatchUpFailures: st.catchUpFailures,
	})
}
