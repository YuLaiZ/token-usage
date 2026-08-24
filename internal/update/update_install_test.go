package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// update_install_test.go 校验锁内安装编排（installUnderLock）。
//
// 编排契约（在 ControlManager.WithLock 单次回调内，禁止二次加锁）：
//   - ConfigLoader 加载有效配置 → ControlSession.Inspect 判活；
//   - 运行中 → Stop（等 daemon lock 释放）；
//   - Installer.Install 实际替换（未注入时占位）；
//   - 替换前运行 → StartWithExecutable(newBinPath)；
//   - 任一步失败 → 回滚至替换前运行状态（用旧二进制重启）。
//
// 全部用 fakeControlManager/fakeControlSession 驱动，不触碰真实 control.Manager / daemon / 文件锁。

// ---- 测试 fakes ----

// fakeInstaller 实现 Installer：记录调用与返回值，不触碰 FS。
type fakeInstaller struct {
	mu             sync.Mutex
	calls          []recordedInstall
	stageContents  [][]byte // 每次 Install 时捕获的 stage 文件内容（stagePath 非空且存在时）
	newBinPath     string   // 成功时返回的新二进制路径（默认 = 入参 targetBinPath）
	err            error
	installAfter   string // 记录调用时刻的 orchestration 阶段标记（由 trace 对照）
	useTargetAsNew bool   // true 时 newBinPath 取 targetBinPath（默认行为）
	deferred       bool   // true 时返回 (targetBinPath, ErrDeferredToHelper) 模拟 Windows staged replacement
}

type recordedInstall struct {
	stagePath, oldBinPath, targetBinPath string
	wasRunning                           bool
}

func newFakeInstaller() *fakeInstaller {
	return &fakeInstaller{useTargetAsNew: true}
}

func (f *fakeInstaller) Install(ctx context.Context, stagePath, oldBinPath, targetBinPath string, wasRunning bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedInstall{stagePath: stagePath, oldBinPath: oldBinPath, targetBinPath: targetBinPath, wasRunning: wasRunning})
	// 捕获 stage 内容（Apply 成功后会删外部 stage，须在 Install 内捕获而非事后读取）。
	if stagePath != "" {
		if content, rerr := os.ReadFile(stagePath); rerr == nil {
			f.stageContents = append(f.stageContents, content)
		} else {
			f.stageContents = append(f.stageContents, nil)
		}
	} else {
		f.stageContents = append(f.stageContents, nil)
	}
	if f.deferred {
		// 模拟 Windows Installer：已 spawn 后台 helper，返回 sentinel。
		return targetBinPath, ErrDeferredToHelper
	}
	if f.err != nil {
		return "", f.err
	}
	if !f.useTargetAsNew {
		return f.newBinPath, nil
	}
	return targetBinPath, nil
}

func (f *fakeInstaller) Platform() string { return "posix-fake" }

// fakeTransactionInstaller 为编排失败路径提供可注入的 Commit/Rollback 结果。
// 它嵌入 fakeInstaller，因此复用其安装调用记录与返回行为。
type fakeTransactionInstaller struct {
	*fakeInstaller
	commitErr     error
	rollbackErr   error
	commitCalls   int
	rollbackCalls int
}

func (f *fakeTransactionInstaller) Commit() error {
	f.commitCalls++
	return f.commitErr
}

func (f *fakeTransactionInstaller) Rollback() error {
	f.rollbackCalls++
	return f.rollbackErr
}

// recordingConfigLoader 返回固定 cfg，并记录加载顺序（供 trace 对照）。
type recordingConfigLoader struct {
	cfg *config.Config
	err error
}

func (l *recordingConfigLoader) load() (*config.Config, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.cfg, nil
}

// makeInstallService 构造「可信来源 + 已注入 control 装配」的 Service，daemon 初始运行状态由 running 决定。
// 返回 Service、注入的 fakeControlSession、fakeInstaller 与 configLoader，便于断言。
func makeInstallService(t *testing.T, running bool) (*Service, *fakeControlSession, *fakeControlManager, *fakeInstaller, *recordingConfigLoader) {
	t.Helper()
	svc := makeService(t) // 默认可信、目标 v0.2.0
	sess := &fakeControlSession{}
	sess.state.Running = running
	mgr := &fakeControlManager{session: sess}
	installer := newFakeInstaller()
	cfgLoader := &recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}
	svc.ControlManager = mgr
	svc.Installer = installer
	svc.ConfigLoader = cfgLoader.load
	return svc, sess, mgr, installer, cfgLoader
}

// ---- 编排测试 ----

// TestApply_InstallOrchestration_RunningDaemonStoppedThenStarted 替换前运行：
// Inspect(running) → Stop → Install → StartWithExecutable(newBinPath)，Installed=true。
func TestApply_InstallOrchestration_RunningDaemonStoppedThenStarted(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall {
		t.Fatal("应到达 ReadyToInstall=true")
	}
	if !got.Installed {
		t.Fatal("锁内编排成功后 Installed 应为 true")
	}
	if sess.inspectCalls != 1 {
		t.Errorf("Inspect 调用 %d 次，want 1", sess.inspectCalls)
	}
	if sess.stopCalls != 1 {
		t.Errorf("运行中应 Stop 一次，stopCalls=%d", sess.stopCalls)
	}
	if len(installer.calls) != 1 {
		t.Fatalf("应 Install 一次，calls=%d", len(installer.calls))
	}
	if sess.startCalls != 1 {
		t.Errorf("替换前运行应 StartWithExecutable 一次，startCalls=%d", sess.startCalls)
	}
	// StartWithExecutable 必须用新二进制路径（默认 = 当前二进制路径，占位安装）。
	if sess.lastStartBinPath != got.BinaryPath {
		t.Errorf("startBinPath=%q want %q", sess.lastStartBinPath, got.BinaryPath)
	}
}

// TestApply_InstallOrchestration_NotRunningNoStopNoStart 替换前未运行：
// Inspect(not running) →（跳过 Stop）→ Install →（跳过 Start），Installed=true。
func TestApply_InstallOrchestration_NotRunningNoStopNoStart(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, false)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Installed {
		t.Fatal("锁内编排成功后 Installed 应为 true")
	}
	if sess.stopCalls != 0 {
		t.Errorf("未运行不应 Stop，stopCalls=%d", sess.stopCalls)
	}
	if len(installer.calls) != 1 {
		t.Errorf("应 Install 一次，calls=%d", len(installer.calls))
	}
	if sess.startCalls != 0 {
		t.Errorf("未运行不应 Start，startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_NoControlManagerSkipsLock 未注入 ControlManager：
// 只到 ReadyToInstall=true，不做锁内编排（向后兼容）。
func TestApply_InstallOrchestration_NoControlManagerSkipsLock(t *testing.T) {
	svc := makeService(t) // 默认未注入 ControlManager... 但 makeService 注入了 &fakeControlManager{}
	// 显式清空，模拟未注入。
	svc.ControlManager = nil

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall {
		t.Fatal("应到达 ReadyToInstall=true")
	}
	if got.Installed {
		t.Fatal("未注入 ControlManager 时 Installed 应为 false")
	}
}

// TestApply_InstallOrchestration_ConfigLoaderNilSkipsLock 注入 ControlManager 但未注入 ConfigLoader：
// 缺配置无法与 control.Session 交互，不做锁内编排。
func TestApply_InstallOrchestration_ConfigLoaderNilSkipsLock(t *testing.T) {
	svc := makeService(t)
	svc.ConfigLoader = nil

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.Installed {
		t.Fatal("未注入 ConfigLoader 时 Installed 应为 false")
	}
	if !got.ReadyToInstall {
		t.Fatal("应仍到达 ReadyToInstall=true")
	}
}

// TestApply_InstallOrchestration_InspectFailsRollsBack Inspect 失败 → 返回错误，Installed=false，不 Stop/Start。
func TestApply_InstallOrchestration_InspectFailsRollsBack(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	sess.inspectErr = errors.New("inspect boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("Inspect 失败应返回错误")
	}
	if sess.stopCalls != 0 {
		t.Errorf("Inspect 失败不应 Stop，stopCalls=%d", sess.stopCalls)
	}
	if len(installer.calls) != 0 {
		t.Errorf("Inspect 失败不应 Install，calls=%d", len(installer.calls))
	}
	if sess.startCalls != 0 {
		t.Errorf("Inspect 失败不应 Start，startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_StopFailsPreservesError Stop 失败 → 透传错误，不 Install/Start。
func TestApply_InstallOrchestration_StopFailsPreservesError(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	sess.stopErr = errors.New("stop boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("Stop 失败应返回错误")
	}
	if len(installer.calls) != 0 {
		t.Errorf("Stop 失败不应 Install，calls=%d", len(installer.calls))
	}
	if sess.startCalls != 0 {
		t.Errorf("Stop 失败不应 Start，startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_InstallFailsRollbackRestart 替换前运行 + Install 失败：
// 用旧二进制回滚重启，返回错误，Installed=false。
func TestApply_InstallOrchestration_InstallFailsRollbackRestart(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	installer.err = errors.New("install boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("Install 失败应返回错误")
	}
	// 回滚：用旧二进制（oldBinPath = BinaryPath）重启。
	if sess.startCalls != 1 {
		t.Errorf("Install 失败应回滚重启一次，startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != svc.binPathForTest {
		t.Errorf("回滚应用旧二进制 %q，实际 %q", svc.binPathForTest, sess.lastStartBinPath)
	}
}

// TestApply_InstallOrchestration_InstallFailsNotRunningNoRollback 替换前未运行 + Install 失败：
// 不回滚重启（无需保持运行），返回错误。
func TestApply_InstallOrchestration_InstallFailsNotRunningNoRollback(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, false)
	installer.err = errors.New("install boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("Install 失败应返回错误")
	}
	if strings.Contains(err.Error(), "已用旧二进制重启") {
		t.Errorf("未运行时不应声称已重启旧二进制，err=%v", err)
	}
	if sess.startCalls != 0 {
		t.Errorf("未运行时 Install 失败不应回滚重启，startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_StartNewFailsRollbackToOld 替换前运行 + 新二进制启动失败：
// 用旧二进制回滚重启，错误透传。
func TestApply_InstallOrchestration_StartNewFailsRollbackToOld(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	// Installer 返回与 oldBinPath 不同的 newBinPath，使 Start(new) 失败后能回滚 Start(old)。
	newBin := "/new/bin/token-usage"
	installer.useTargetAsNew = false
	installer.newBinPath = newBin
	sess.startErr = errors.New("start boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("启动失败应返回错误")
	}
	// 应尝试启动两次：先 newBin（失败），再 oldBin（回滚）。
	if sess.startCalls != 2 {
		t.Fatalf("新启动失败 + 回滚应 Start 两次，startCalls=%d", sess.startCalls)
	}
	// fakeControlSession 只记录最后一次 binPath，应是回滚用的旧二进制。
	if sess.lastStartBinPath != svc.binPathForTest {
		t.Errorf("回滚应用旧二进制 %q，实际 %q", svc.binPathForTest, sess.lastStartBinPath)
	}
}

// TestApply_InstallOrchestration_LockAcquireFails control lock 获取失败 → 透传错误。
func TestApply_InstallOrchestration_LockAcquireFails(t *testing.T) {
	svc, _, mgr, _, _ := makeInstallService(t, true)
	mgr.lockErr = errors.New("control lock timeout")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("control lock 获取失败应返回错误")
	}
}

// TestApply_InstallOrchestration_ConfigLoaderFails ConfigLoader 失败 → 透传错误，不进锁。
func TestApply_InstallOrchestration_ConfigLoaderFails(t *testing.T) {
	svc, _, mgr, _, cfgLoader := makeInstallService(t, true)
	cfgLoader.err = errors.New("load boom")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("ConfigLoader 失败应返回错误")
	}
	// consume/sweep 在 install 之前用一次 WithLock（provenance 可信后）。installUnderLockOutcome
	// 的 ConfigLoader 在其 WithLock 前调用并失败，故 install 的 WithLock 不应被调用。
	if mgr.calls > 1 {
		t.Errorf("ConfigLoader 失败不应进入 install control lock，WithLock calls=%d", mgr.calls)
	}
}

// TestApply_InstallOrchestration_UsesInstallerNewBinPathForStart Installer 返回独立 newBinPath：
// StartWithExecutable 用 newBinPath（非 oldBinPath）。
func TestApply_InstallOrchestration_UsesInstallerNewBinPathForStart(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	newBin := "/opt/token-usage-new/bin/token-usage"
	installer.useTargetAsNew = false
	installer.newBinPath = newBin

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Installed {
		t.Fatal("应 Installed=true")
	}
	if sess.lastStartBinPath != newBin {
		t.Errorf("应用 Installer 返回的 newBinPath %q 启动，实际 %q", newBin, sess.lastStartBinPath)
	}
}

// TestApply_InstallOrchestration_NoInstallerStillStarts 未注入 Installer（占位安装）：
// newBinPath = oldBinPath，替换前运行 → 用 oldBinPath 启动。验证骨架在无 Installer 时仍跑通锁内编排。
func TestApply_InstallOrchestration_NoInstallerStillStarts(t *testing.T) {
	svc := makeService(t)
	sess := &fakeControlSession{}
	sess.state.Running = true
	svc.ControlManager = &fakeControlManager{session: sess}
	svc.Installer = nil
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Installed {
		t.Fatal("占位安装（无 Installer）也应 Installed=true（骨架跑通）")
	}
	if sess.stopCalls != 1 {
		t.Errorf("运行中应 Stop 一次，stopCalls=%d", sess.stopCalls)
	}
	if sess.startCalls != 1 {
		t.Errorf("占位安装后应用 oldBinPath 启动，startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_DeferredToHelperSkipsStartCommit Installer 返回
// ErrDeferredToHelper（Windows staged replacement）：installUnderLock 在 Stop 后短路，
// installed=false，跳过 Start/Commit/Rollback（由后台 helper 负责）。
// 替换前运行 → Stop 仍执行（腾出干净状态供 helper）；但不 Start。
func TestApply_InstallOrchestration_DeferredToHelperSkipsStartCommit(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	installer.deferred = true

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	// ReadyToInstall=true（来源校验已通过），但 Installed=false（helper 异步）。
	if !got.ReadyToInstall {
		t.Fatal("应到达 ReadyToInstall=true")
	}
	if got.Installed {
		t.Fatal("deferred-to-helper 时 Installed 应为 false")
	}
	if !got.Deferred {
		t.Fatal("deferred-to-helper 时 Deferred 应为 true")
	}
	if len(installer.calls) != 1 {
		t.Fatalf("应 Install 一次，calls=%d", len(installer.calls))
	}
	// 运行中仍 Stop（父进程腾出干净状态），但不 Start（helper 负责）。
	if sess.stopCalls != 1 {
		t.Errorf("运行中应 Stop 一次，stopCalls=%d", sess.stopCalls)
	}
	if sess.startCalls != 0 {
		t.Errorf("deferred-to-helper 不应 Start（由 helper 负责），startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallOrchestration_DeferredToHelperNotRunningNoStop 替换前未运行 + deferred：
// 不 Stop（本来就没运行），不 Start，installed=false。
func TestApply_InstallOrchestration_DeferredToHelperNotRunningNoStop(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, false)
	installer.deferred = true

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.Installed {
		t.Fatal("deferred 时 Installed 应为 false")
	}
	if !got.Deferred {
		t.Fatal("deferred 时 Deferred 应为 true")
	}
	if sess.stopCalls != 0 {
		t.Errorf("未运行不应 Stop，stopCalls=%d", sess.stopCalls)
	}
	if sess.startCalls != 0 {
		t.Errorf("deferred 不应 Start，startCalls=%d", sess.startCalls)
	}
}

// TestErrDeferredToHelper_IsSentinel 确认 ErrDeferredToHelper 可被 errors.Is 识别。
func TestErrDeferredToHelper_IsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("wrap: %w", ErrDeferredToHelper)
	if !errors.Is(wrapped, ErrDeferredToHelper) {
		t.Fatal("errors.Is 应识别 ErrDeferredToHelper（即便被包装）")
	}
}

// TestApply_InstallOrchestration_StartFailuresJoinAllCauses 确认新二进制启动、
// 回滚和旧二进制重启同时失败时，每个根因都可由 errors.Is 提取。
func TestApply_InstallOrchestration_StartFailuresJoinAllCauses(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	installer.useTargetAsNew = false
	installer.newBinPath = "/new/bin/token-usage"

	newStartErr := errors.New("new binary start failed")
	rollbackErr := errors.New("rollback failed")
	restartErr := errors.New("old binary restart failed")
	sess.startErrs = []error{newStartErr, restartErr}
	svc.Installer = &fakeTransactionInstaller{
		fakeInstaller: installer,
		rollbackErr:   rollbackErr,
	}

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("启动与回滚失败应返回错误")
	}
	for _, want := range []error{newStartErr, rollbackErr, restartErr} {
		if !errors.Is(err, want) {
			t.Errorf("errors.Is(err, %v) = false，err=%v", want, err)
		}
	}
	if sess.startCalls != 2 {
		t.Errorf("应尝试启动新旧二进制各一次，startCalls=%d", sess.startCalls)
	}
}
