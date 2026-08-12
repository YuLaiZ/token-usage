package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// helper_runner_test.go 校验后台 helper 编排逻辑（平台无关，macOS 可跑）。
//
// 覆盖 helper 自动化的关键路径与失败分支：
//   - 成功路径（备份 → MoveFileEx → 校验 → 重启 → result）；
//   - 父进程等待失败；
//   - daemon 停止后意外运行；
//   - MoveFileEx 失败 → 回滚；
//   - 替换后 hash 校验失败 → 回滚；
//   - daemon 启动失败 → 回滚 + 重启旧；
//   - 计划校验失败（不写 result）；
//   - wasRunning=false 不重启。
//
// backup/move/verify 全部在 t.TempDir() 真实文件上运作（fileMover 用 os.Rename
// 模拟 MoveFileEx(REPLACE_EXISTING) 的原子替换语义），保证 hash 校验与文件状态一致。

// realFileMover 用 os.Rename 实现原子替换（与 MoveFileEx(REPLACE_EXISTING) 语义对齐），
// 使 helper_runner 测试在真实 FS 上一致运作。
type realFileMover struct{}

func (realFileMover) MoveReplace(from, to string) error { return os.Rename(from, to) }

// errorFileMover 始终返回预置错误（模拟 MoveFileEx 失败）。
type errorFileMover struct{ err error }

func (e errorFileMover) MoveReplace(from, to string) error { return e.err }

// setupHelperFixture 在 t.TempDir() 下构造一份完整的 helper 替换现场：
//   - target（旧二进制）、stage（新二进制）、helper.exe、plan 文件；
//
// 返回 (runner 依赖集合, paths, plan) 便于各用例覆盖个别 seam 后装配 runner。
type helperFixture struct {
	dir     string
	paths   helperPaths
	plan    helperPlan
	parent  *fakeParentWaiter
	mover   FileMover
	result  *fakeResultWriter
	sess    *fakeControlSession
	mgr     *fakeControlManager
	cfgLoad *recordingConfigLoader
}

func newHelperFixture(t *testing.T, wasRunning bool) *helperFixture {
	t.Helper()
	dir := t.TempDir()
	const base = "token-usage"
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	paths := deriveHelperPaths(dir, base, nonce)

	oldContent := []byte("old-official-binary")
	newContent := []byte("new-official-binary-v2")
	if err := os.WriteFile(paths.Target, oldContent, 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(paths.Stage, newContent, 0o755); err != nil {
		t.Fatalf("write stage: %v", err)
	}
	if err := os.WriteFile(paths.Helper, []byte("helper-fake"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: base,
		OldSHA256:      sumHex(oldContent),
		NewSHA256:      sumHex(newContent),
		WasRunning:     wasRunning,
		Parent:         ProcessIdentity{PID: 4242, CreationTime: 0xabcdef},
	}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	sess := &fakeControlSession{}
	mgr := &fakeControlManager{session: sess}
	return &helperFixture{
		dir:     dir,
		paths:   paths,
		plan:    plan,
		parent:  &fakeParentWaiter{},
		mover:   realFileMover{},
		result:  newFakeResultWriter(),
		sess:    sess,
		mgr:     mgr,
		cfgLoad: &recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}},
	}
}

// runner 装配 helperRunner（用 fixture 的 seam）。
func (f *helperFixture) runner(t *testing.T) *helperRunner {
	t.Helper()
	r, err := NewHelperRunner(f.parent, f.mover, f.result, f.mgr, f.cfgLoad.load)
	if err != nil {
		t.Fatalf("NewHelperRunner: %v", err)
	}
	return r
}

// readResult 读取 fixture resultWriter 记录的 result JSON。
func (f *helperFixture) readResult(t *testing.T) helperResult {
	t.Helper()
	data, ok := f.result.written[f.paths.Result]
	if !ok {
		t.Fatalf("未写入 result 文件 %s", f.paths.Result)
	}
	var res helperResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("解析 result 失败: %v", err)
	}
	return res
}

// TestHelperRunner_SuccessWasRunning 成功路径 + wasRunning=true：
// 备份 → 移动 → 校验 → 重启 daemon → 成功 result。target 变为新内容。
func TestHelperRunner_SuccessWasRunning(t *testing.T) {
	f := newHelperFixture(t, true)
	r := f.runner(t)

	if err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	// target 已替换为新版本。
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("new-official-binary-v2")) {
		t.Fatalf("target 内容应为新版本，got %q", string(got))
	}
	// stage 已被移动（不存在）。
	if _, err := os.Stat(f.paths.Stage); !os.IsNotExist(err) {
		t.Errorf("stage 应已被移动走")
	}
	// backup 仍存在（成功后由 cleanup 删除，helper 本身不删）。
	if _, err := os.Stat(f.paths.Backup); err != nil {
		t.Errorf("backup 应仍存在（待 cleanup 删除）: %v", err)
	}
	// daemon 被重启（用 target 路径）。
	if f.sess.startCalls != 1 {
		t.Errorf("wasRunning 应 Start 一次，startCalls=%d", f.sess.startCalls)
	}
	if f.sess.lastStartBinPath != f.paths.Target {
		t.Errorf("应用 target 启动 %q，实际 %q", f.paths.Target, f.sess.lastStartBinPath)
	}
	// 成功 result。
	res := f.readResult(t)
	if !res.Success {
		t.Fatalf("result 应 Success=true，got %+v", res)
	}
}

// TestHelperRunner_SuccessNotRunning wasRunning=false：不重启 daemon。
func TestHelperRunner_SuccessNotRunning(t *testing.T) {
	f := newHelperFixture(t, false)
	r := f.runner(t)

	if err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if f.sess.startCalls != 0 {
		t.Errorf("wasRunning=false 不应 Start，startCalls=%d", f.sess.startCalls)
	}
	res := f.readResult(t)
	if !res.Success {
		t.Fatalf("result 应 Success=true，got %+v", res)
	}
}

// TestHelperRunner_ParentWaitFails 父进程等待失败 → 失败 result，不移动。
func TestHelperRunner_ParentWaitFails(t *testing.T) {
	f := newHelperFixture(t, true)
	f.parent.err = errors.New("parent still alive")
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("父进程等待失败应返回 error")
	}
	// target 仍是旧版本。
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("old-official-binary")) {
		t.Fatalf("父进程等待失败不应替换 target，got %q", string(got))
	}
	res := f.readResult(t)
	if res.Success {
		t.Fatal("result 应 Success=false")
	}
	if res.Error == "" {
		t.Error("失败 result 应携带 Error")
	}
}

// TestHelperRunner_DaemonRunningAfterStop daemon 停止后意外运行 → 安全失败，不替换。
func TestHelperRunner_DaemonRunningAfterStop(t *testing.T) {
	f := newHelperFixture(t, true)
	f.sess.state.Running = true
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("daemon 运行中应返回 error")
	}
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("old-official-binary")) {
		t.Fatalf("daemon 运行中不应替换 target，got %q", string(got))
	}
	res := f.readResult(t)
	if res.Success {
		t.Fatal("应写失败 result")
	}
}

// TestHelperRunner_MoveFailsRollsBack MoveFileEx 失败 → 从 backup 回滚，target 仍是旧版本。
func TestHelperRunner_MoveFailsRollsBack(t *testing.T) {
	f := newHelperFixture(t, false)
	f.mover = errorFileMover{err: errors.New("move denied")}
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("move 失败应返回 error")
	}
	// target 仍是旧版本（回滚后恢复）。
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("old-official-binary")) {
		t.Fatalf("move 失败后 target 应仍是旧版本，got %q", string(got))
	}
	res := f.readResult(t)
	if res.Success {
		t.Fatal("应写失败 result")
	}
}

// TestHelperRunner_HashVerifyFailsRollsBack 替换后新 target hash 不匹配 → 回滚。
func TestHelperRunner_HashVerifyFailsRollsBack(t *testing.T) {
	f := newHelperFixture(t, false)
	// 篡改 plan 的 NewSHA256，使替换后校验失败。
	f.plan.NewSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := writeHelperPlan(f.paths.Plan, f.plan); err != nil {
		t.Fatalf("rewrite plan: %v", err)
	}
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("hash 校验失败应返回 error")
	}
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("old-official-binary")) {
		t.Fatalf("hash 校验失败后 target 应回滚为旧版本，got %q", string(got))
	}
	res := f.readResult(t)
	if res.Success {
		t.Fatal("应写失败 result")
	}
}

// TestHelperRunner_StartFailsRollsBackAndRestartsOld 启动新 daemon 失败：
// 回滚到旧版本 + 用旧 target 重启 daemon，写失败 result。
func TestHelperRunner_StartFailsRollsBackAndRestartsOld(t *testing.T) {
	f := newHelperFixture(t, true)
	f.sess.startErr = errors.New("spawn denied")
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("启动失败应返回 error")
	}
	// target 回滚为旧版本。
	got, _ := os.ReadFile(f.paths.Target)
	if !bytes.Equal(got, []byte("old-official-binary")) {
		t.Fatalf("启动失败后 target 应回滚为旧版本，got %q", string(got))
	}
	// 应尝试启动两次：先新（失败），回滚后再旧（重启）。
	if f.sess.startCalls != 2 {
		t.Errorf("启动失败 + 回滚重启应 Start 两次，startCalls=%d", f.sess.startCalls)
	}
	// 最后一次启动用 target（回滚后 = 旧版本路径）。
	if f.sess.lastStartBinPath != f.paths.Target {
		t.Errorf("回滚重启应用 target，实际 %q", f.sess.lastStartBinPath)
	}
	res := f.readResult(t)
	if res.Success {
		t.Fatal("应写失败 result")
	}
}

// TestHelperRunner_InvalidPlanNoResult 计划校验失败：不写 result，返回 error。
func TestHelperRunner_InvalidPlanNoResult(t *testing.T) {
	f := newHelperFixture(t, true)
	// 用一个不存在的 planPath 触发校验失败。
	r := f.runner(t)

	err := r.Run(context.Background(), f.paths.Helper, filepath.Join(f.dir, "nonexistent-plan"))
	if err == nil {
		t.Fatal("计划校验失败应返回 error")
	}
	// 校验失败前无法确定 result 路径，不应写任何 result。
	if len(f.result.written) != 0 {
		t.Errorf("计划校验失败不应写 result，written=%v", f.result.written)
	}
}

// TestNewHelperRunner_NilDeps 依赖为空 → 装配错误。
func TestNewHelperRunner_NilDeps(t *testing.T) {
	if _, err := NewHelperRunner(nil, realFileMover{}, newFakeResultWriter(), &fakeControlManager{},
		(&recordingConfigLoader{cfg: &config.Config{}}).load); err == nil {
		t.Error("nil parentWaiter 应返回错误")
	}
}

// TestHelperRunner_WaiterReceivesPlanParent helper 把 plan.Parent 喂给 waiter（而非运行时自发现父 PID）。
// 证明等待身份来自校验过的计划，杜绝 helper 运行时自发现父进程身份。
func TestHelperRunner_WaiterReceivesPlanParent(t *testing.T) {
	f := newHelperFixture(t, false)
	r := f.runner(t)

	if err := r.Run(context.Background(), f.paths.Helper, f.paths.Plan); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if f.parent.calls != 1 {
		t.Fatalf("应调用 WaitParentExit 一次，calls=%d", f.parent.calls)
	}
	// 收到的身份必须精确等于 plan.Parent（非零、非运行时自发现的父 PID）。
	if f.parent.lastIdentity != f.plan.Parent {
		t.Errorf("waiter 收到的身份 %+v 不等于 plan.Parent %+v", f.parent.lastIdentity, f.plan.Parent)
	}
	if !f.parent.lastIdentity.Valid() {
		t.Errorf("waiter 收到的身份应为合法非零值: %+v", f.parent.lastIdentity)
	}
	// plan.Parent 的 PID 是固定 4242，证明身份取自计划而非运行时发现的任意值。
	if f.parent.lastIdentity.PID != 4242 {
		t.Errorf("身份 PID 应为 plan 里的 4242，got %d（应为计划值而非运行时发现）", f.parent.lastIdentity.PID)
	}
}
