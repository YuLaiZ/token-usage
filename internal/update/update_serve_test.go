package update

// update_serve_test.go 校验自更新的 dashboard 运行态保持编排：更新前探测
// （缺失/损坏/陈旧一律未运行）→ 停止（探活判停，失败即中止更新）→ 替换 →
// daemon 恢复 → dashboard 按原监听地址、用新二进制恢复；任一步失败按事务
// 语义回滚到旧二进制与更新前 daemon/dashboard 运行态。全部用
// fakeServeLifecycle/fakeControlManager/fakeInstaller 驱动，不触碰真实
// serve/daemon/文件锁；internal/serve 包的状态探测与停止/启动编排另由其
// 包内测试与 cli 包命令级测试覆盖。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ---- dashboard 生命周期 fake ----

type recordedServeStart struct {
	dataDir string
	binPath string
	addr    string
}

// fakeServeLifecycle 记录全部调用；各步骤的返回值可按用例注入。
type fakeServeLifecycle struct {
	mu            sync.Mutex
	detectCalls   int
	detectRunning bool
	detectAddr    string
	detectErr     error
	stopCalls     int
	stopStopped   bool
	stopErr       error
	stopHook      func() // Stop 调用时触发（时序断言用，在计数之后执行）
	startCalls    []recordedServeStart
	startErrs     []error // 按调用次序消费；耗尽后成功
}

func (f *fakeServeLifecycle) DetectRunning(dataDir string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detectCalls++
	return f.detectRunning, f.detectAddr, f.detectErr
}

func (f *fakeServeLifecycle) Stop(dataDir string) (bool, error) {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	if f.stopHook != nil {
		f.stopHook()
	}
	return f.stopStopped, f.stopErr
}

func (f *fakeServeLifecycle) Start(dataDir, binPath, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, recordedServeStart{dataDir: dataDir, binPath: binPath, addr: addr})
	if index := len(f.startCalls) - 1; index < len(f.startErrs) {
		return f.startErrs[index]
	}
	return nil
}

func (f *fakeServeLifecycle) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.startCalls)
}

func (f *fakeServeLifecycle) lastStart() recordedServeStart {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.startCalls) == 0 {
		return recordedServeStart{}
	}
	return f.startCalls[len(f.startCalls)-1]
}

// makeServeInstallService 在 makeInstallService 之上注入 dashboard 生命周期依赖。
func makeServeInstallService(t *testing.T, daemonRunning bool, serve *fakeServeLifecycle) (*Service, *fakeControlSession, *fakeInstaller, *recordingConfigLoader) {
	t.Helper()
	svc, sess, _, installer, cfgLoader := makeInstallService(t, daemonRunning)
	svc.ServeLifecycle = serve
	return svc, sess, installer, cfgLoader
}

// ---- 四种原始运行态组合 ----

func TestApply_ServeOrchestration_BothRunning(t *testing.T) {
	svc, sess, installer, _ := makeServeInstallService(t, true, &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true})

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Installed || got.Deferred {
		t.Fatalf("应同步安装完成, got %+v", got)
	}
	if !got.DaemonWasRunning || !got.ServeWasRunning || got.ServeAddr != "127.0.0.1:8619" {
		t.Errorf("结果应记录 daemon 与 dashboard 的原运行态, got %+v", got)
	}
	if installer.calls[0].serveWasRunning != true || installer.calls[0].serveAddr != "127.0.0.1:8619" {
		t.Errorf("Install 应收到 serve 快照, got %+v", installer.calls[0])
	}
	if sess.stopCalls != 1 {
		t.Errorf("只应停止 daemon 一次, got %d", sess.stopCalls)
	}
	if sess.lastStartBinPath == "" {
		t.Error("daemon 应回到运行")
	}
}

func TestApply_ServeOrchestration_DaemonOnly(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: false}
	svc, sess, _, _ := makeServeInstallService(t, true, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.DaemonWasRunning || got.ServeWasRunning {
		t.Errorf("结果应记录 daemon 运行、dashboard 未运行, got %+v", got)
	}
	if serveFake.stopCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("dashboard 未运行时不得停止/恢复, stop=%d start=%d", serveFake.stopCalls, serveFake.startCount())
	}
	if sess.stopCalls != 1 {
		t.Errorf("应停止 daemon, got %d", sess.stopCalls)
	}
}

func TestApply_ServeOrchestration_ServeOnly(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:9000", stopStopped: true}
	svc, sess, _, _ := makeServeInstallService(t, false, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.DaemonWasRunning {
		t.Errorf("daemon 原本未运行, got %+v", got)
	}
	if !got.ServeWasRunning || got.ServeAddr != "127.0.0.1:9000" {
		t.Errorf("结果应记录 dashboard 原运行态, got %+v", got)
	}
	if sess.stopCalls != 0 {
		t.Errorf("daemon 未运行时不得停止 daemon, got %d", sess.stopCalls)
	}
	if sess.startCalls != 0 {
		t.Errorf("daemon 未运行时不得启动 daemon, got %d", sess.startCalls)
	}
	// 恢复调用必须以替换后的目标二进制与原监听地址进行。
	last := serveFake.lastStart()
	if last.binPath == "" {
		t.Errorf("dashboard 应以替换后二进制恢复, got %+v", last)
	}
	if last.addr != "127.0.0.1:9000" {
		t.Errorf("dashboard 应按原监听地址恢复, got %+v", last)
	}
}

func TestApply_ServeOrchestration_NoneRunning(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: false}
	svc, sess, _, _ := makeServeInstallService(t, false, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.DaemonWasRunning || got.ServeWasRunning {
		t.Errorf("两者原本都未运行, got %+v", got)
	}
	if sess.stopCalls != 0 || sess.startCalls != 0 {
		t.Errorf("不应触碰 daemon, stop=%d start=%d", sess.stopCalls, sess.startCalls)
	}
	if serveFake.stopCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("不应触碰 dashboard, stop=%d start=%d", serveFake.stopCalls, serveFake.startCount())
	}
}

// ---- 探测语义：陈旧/缺失/损坏不得误判为运行 ----

func TestApply_ServeDetectStaleStateTreatedAsNotRunning(t *testing.T) {
	// fake 模拟 serve.json 存在但 /api/meta 无响应（陈旧）：DetectRunning 返回 false。
	serveFake := &fakeServeLifecycle{detectRunning: false}
	svc, _, _, _ := makeServeInstallService(t, true, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.ServeWasRunning || got.ServeAddr != "" {
		t.Errorf("陈旧状态不得误判为运行, got %+v", got)
	}
	if serveFake.stopCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("陈旧状态不得触发停止/恢复, stop=%d start=%d", serveFake.stopCalls, serveFake.startCount())
	}
	if serveFake.detectCalls != 1 {
		t.Errorf("锁内应恰好探测一次, got %d", serveFake.detectCalls)
	}
}

// ---- serve 地址保留 ----

func TestApply_ServeAddressPreservedAcrossRestart(t *testing.T) {
	const originalAddr = "127.0.0.1:8642"
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: originalAddr, stopStopped: true}
	svc, _, _, _ := makeServeInstallService(t, true, serveFake)

	if _, err := svc.Apply(context.Background(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	for i, call := range serveFake.startCalls {
		if call.addr != originalAddr {
			t.Errorf("第 %d 次恢复的监听地址必须与更新前一致: want %s got %s", i+1, originalAddr, call.addr)
		}
		if call.dataDir != "/data" {
			t.Errorf("恢复应使用配置的数据目录, got %q", call.dataDir)
		}
	}
	if len(serveFake.startCalls) != 1 {
		t.Fatalf("应恰好恢复一次, got %d", len(serveFake.startCalls))
	}
}

// ---- serve 停止失败：中止更新，不替换二进制，不改变 daemon 运行态 ----

func TestApply_ServeStopFailureAbortsUpdate(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopErr: errors.New("server still responding")}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("serve 停止失败必须中止更新并返回错误")
	}
	if !strings.Contains(err.Error(), "dashboard") && !strings.Contains(err.Error(), "Dashboard") {
		t.Errorf("错误应指向 dashboard, got %v", err)
	}
	if installer.callCount() != 0 {
		t.Errorf("serve 停止失败时不得替换二进制, install calls=%d", installer.callCount())
	}
	if sess.stopCalls != 0 {
		t.Errorf("serve 停止失败时 daemon 运行态不得被改变, stop calls=%d", sess.stopCalls)
	}
	if got.Installed || got.Deferred {
		t.Errorf("更新未完成, got %+v", got)
	}
}

// ---- 安装失败：按事务语义恢复旧二进制 + daemon/dashboard 原状态 ----

func TestApply_ServeInstallFailureRollsBackBoth(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)
	installer.err = errors.New("rename failed")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("安装失败必须返回错误")
	}
	// daemon 已被停止：必须用旧二进制重启。
	if sess.stopCalls != 1 {
		t.Errorf("应停止 daemon, got %d", sess.stopCalls)
	}
	if len(sess.startPaths) == 0 || sess.startPaths[len(sess.startPaths)-1] != oldBinPath(t, svc) {
		t.Errorf("daemon 应回滚重启（旧二进制路径）, got %v", sess.startPaths)
	}
	// dashboard 必须以旧二进制按原地址恢复（安装失败时 target 已是旧版本）。
	if serveFake.startCount() != 1 {
		t.Fatalf("应恢复 dashboard 一次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.binPath != oldBinPath(t, svc) || got.addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应回滚恢复（旧二进制 + 原地址）, got %+v", got)
	}
	// dashboard 回滚成功时错误只保留主失败（安装失败）；恢复失败的信息只在
	// 回滚自身也失败时聚合出现（见 ServeStartFailure 用例）。
}

// ---- daemon 新二进制启动失败：回滚 + dashboard 以旧二进制恢复 ----

func TestApply_ServeDaemonStartFailureRollsBackServe(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)
	sess.startErrs = []error{errors.New("new binary failed to start")}

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("新二进制启动失败必须返回错误")
	}
	if installer.callCount() != 1 {
		t.Fatalf("应恰好替换一次, got %d", installer.callCount())
	}
	// dashboard 原在运行：daemon 启动失败回滚后必须以旧二进制恢复。
	if serveFake.startCount() != 1 {
		t.Fatalf("应恢复 dashboard 一次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.binPath != oldBinPath(t, svc) || got.addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应回滚恢复（旧二进制 + 原地址）, got %+v", got)
	}
}

// ---- serve 恢复失败：整体回滚（旧二进制 + daemon 停新启旧 + dashboard 旧二进制重试）----

func TestApply_ServeStartFailureRollsBackWholeUpdate(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	serveFake.startErrs = []error{errors.New("port already in use")}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)
	tx := &fakeTransactionInstaller{fakeInstaller: installer}
	svc.Installer = tx // 服务侧持有事务包装，Rollback/Commit 断言才可达

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("dashboard 恢复失败必须返回错误")
	}
	if tx.rollbackCalls != 1 {
		t.Errorf("必须回滚二进制事务, rollback calls=%d", tx.rollbackCalls)
	}
	// daemon 已用新二进制启动成功 → 回滚时必须停新启旧。
	if sess.stopCalls != 2 { // 替换前 1 次 + 回滚停新 daemon 1 次
		t.Errorf("回滚应再次停止 daemon（新二进制）, got %d", sess.stopCalls)
	}
	if len(sess.startPaths) != 2 {
		t.Fatalf("daemon 康新启 2 次（新二进制 + 回滚旧二进制）, got %v", sess.startPaths)
	}
	if sess.startPaths[1] != oldBinPath(t, svc) {
		t.Errorf("第二次启动必须是旧二进制回滚, got %q", sess.startPaths[1])
	}
	// dashboard 共两次 Start：新二进制（失败）+ 旧二进制（回滚重试）。
	if serveFake.startCount() != 2 {
		t.Fatalf("dashboard 应共尝试恢复两次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.binPath != oldBinPath(t, svc) || got.addr != "127.0.0.1:8619" {
		t.Errorf("回滚重试必须用旧二进制与原地址, got %+v", got)
	}
	if !strings.Contains(err.Error(), "dashboard") {
		t.Errorf("主失败（dashboard 恢复失败）必须保留, got %v", err)
	}
}

// ---- Inspect 失败：编排早段中止，无服务被触碰 ----

// Inspect 在停止任何服务之前执行（恢复意图快照需要两个运行态同时在场），
// 失败时只有探测与快照之前的只读动作发生过：中止更新即可，无需恢复。
// （「停止后失败」的事务语义由各停止/恢复分支与恢复意图快照覆盖。）
func TestApply_ServeInspectFailureAbortsBeforeAnyStop(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, sess, _, _ := makeServeInstallService(t, true, serveFake)
	sess.inspectErr = errors.New("inspect exploded")

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("Inspect 失败必须返回错误")
	}
	if serveFake.stopCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("Inspect 失败时 dashboard 不得被触碰, stop=%d start=%d", serveFake.stopCalls, serveFake.startCount())
	}
	if sess.stopCalls != 0 {
		t.Errorf("Inspect 失败时 daemon 不得被停止, got %d", sess.stopCalls)
	}
	if installerCalled(t, svc) {
		t.Error("Inspect 失败时不得替换二进制")
	}
	// 此刻没有任何停止发生，恢复意图也不应写入。
	if _, found, _ := findIntent(oldBinPath(t, svc)); found {
		t.Error("未进入停止阶段不应留下恢复意图")
	}
}

// installerCalled 报告该 Service 的 Installer 是否被调用过（需要 *fakeInstaller）。
func installerCalled(t *testing.T, svc *Service) bool {
	t.Helper()
	fi, ok := svc.Installer.(*fakeInstaller)
	if !ok {
		t.Fatal("测试装配的 Installer 不是 *fakeInstaller")
	}
	return fi.callCount() != 0
}

// ---- serve 在停止前自行退出：更正运行态，不恢复 ----

func TestApply_ServeVanishedBeforeStopSkipsRestore(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: false}
	svc, _, _, _ := makeServeInstallService(t, true, serveFake)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.ServeWasRunning || got.ServeAddr != "" {
		t.Errorf("停止前已退出的实例不得恢复, got %+v", got)
	}
	if serveFake.startCount() != 0 {
		t.Errorf("不应恢复已消失的实例, got %d", serveFake.startCount())
	}
}

// ---- 未注入 ServeLifecycle：完全跳过 serve 编排（向后兼容） ----

func TestApply_ServeLifecycleNilKeepsLegacyBehavior(t *testing.T) {
	svc, sess, installer, _ := makeServeInstallService(t, true, nil)
	svc.ServeLifecycle = nil

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Installed || got.ServeWasRunning {
		t.Errorf("未注入 serve 依赖时应保持既有行为, got %+v", got)
	}
	if installer.calls[0].serveWasRunning {
		t.Error("Install 不应收到 serve 运行快照")
	}
	if sess.startCalls != 1 { // 仅 daemon
		t.Errorf("应只恢复 daemon, got %d", sess.startCalls)
	}
}

// ---- Deferred 路径（Windows helper 接管）：快照传出供渲染与 helper plan ----

func TestApply_ServeDeferredCarriesSnapshot(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, _, installer, _ := makeServeInstallService(t, true, serveFake)
	installer.deferred = true

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Deferred || got.Installed {
		t.Fatalf("应为后台替换排队, got %+v", got)
	}
	if !got.ServeWasRunning || got.ServeAddr != "127.0.0.1:8619" {
		t.Errorf("Deferred 结果应携带 serve 快照, got %+v", got)
	}
	// helper 接管后父进程不再恢复 serve。
	if serveFake.startCount() != 0 {
		t.Errorf("Deferred 路径父进程不得恢复 serve, got %d", serveFake.startCount())
	}
}

// ---- DetectRunning 失败：中止更新 ----

func TestApply_ServeDetectErrorAbortsUpdate(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectErr: errors.New("permission denied")}
	svc, _, installer, _ := makeServeInstallService(t, true, serveFake)

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("探测失败必须中止更新")
	}
	if installer.callCount() != 0 {
		t.Errorf("探测失败时不得替换二进制, install calls=%d", installer.callCount())
	}
}

// ---- 断言辅助 ----

// oldBinPath 返回更新前二进制路径（Provenance 校验结果，Install 的 oldBinPath
// 参数；POSIX 安装失败或 Rollback 后它就是 target 的内容版本）。
func oldBinPath(t *testing.T, svc *Service) string {
	t.Helper()
	if svc.binPathForTest == "" {
		t.Fatal("测试装配缺少 binPathForTest")
	}
	return svc.binPathForTest
}
