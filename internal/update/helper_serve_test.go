package update

// helper_serve_test.go 校验 Windows 后台 helper 对 dashboard 运行态的处理：
// plan 记录 dashboard 原在运行时（serve 已由父进程停止以释放旧 .exe），helper
// 在 MoveFileEx 与 daemon 重启成功后以新 target、按 plan 记录的原监听地址后台
// 恢复 dashboard；恢复失败执行整体回滚（旧版本、daemon 停新启旧、dashboard
// 旧 target 重试）并写失败 result；无 dashboard 恢复能力的 helper（serve 依赖
// 未装配）不得接管会丢失 dashboard 运行态的事务。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// newServeHelperFixture 在标准 fixture 之上注入 plan 的 serve 快照。
func newServeHelperFixture(t *testing.T, wasRunning, serveWasRunning bool, serveAddr string) *helperFixture {
	t.Helper()
	f := newHelperFixture(t, wasRunning)
	f.plan.ServeWasRunning = serveWasRunning
	f.plan.ServeAddr = serveAddr
	if err := writeHelperPlan(f.paths.Plan, f.plan); err != nil {
		t.Fatalf("rewrite plan: %v", err)
	}
	return f
}

// TestHelperRunner_ServeRunning_RestoredWithNewTarget dashboard 原在运行：
// helper 在替换与 daemon 重启后用新 target 按原地址恢复 dashboard，写成功 result。
func TestHelperRunner_ServeRunning_RestoredWithNewTarget(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake

	if err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if got := serveFake.lastStart(); got.binPath != f.paths.Target || got.addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应以新 target + 原地址恢复, got %+v", got)
	}
	if serveFake.startCount() != 1 {
		t.Errorf("应恰好恢复一次, got %d", serveFake.startCount())
	}
	res := f.readResult(t)
	if !res.Success {
		t.Errorf("应写成功 result, got %+v", res)
	}
}

// TestHelperRunner_ServeRestoreFailure_RollsBackWholeUpdate dashboard 恢复失败：
// 回滚旧版本 → 停止新 daemon → 旧 target 重启 daemon → 旧 target 按原地址重试恢复
// dashboard → 写失败 result（主失败与各回滚结果记录在 rollback 字段）。
func TestHelperRunner_ServeRestoreFailure_RollsBackWholeUpdate(t *testing.T) {
	// 第一次 Start（新 target）失败；第二次（旧 target 回滚重试）成功。
	serveFake := &fakeServeLifecycle{startErrs: []error{errors.New("port occupied")}}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("dashboard 恢复失败必须返回错误")
	}
	if serveFake.startCount() != 2 {
		t.Fatalf("dashboard 应尝试恢复两次（新 + 回滚旧）, got %d", serveFake.startCount())
	}
	calls := serveFake.startCalls
	if calls[0].binPath != f.paths.Target || calls[0].addr != "127.0.0.1:8619" {
		t.Errorf("首次恢复应用新 target + 原地址, got %+v", calls[0])
	}
	if calls[1].binPath != f.paths.Target || calls[1].addr != "127.0.0.1:8619" {
		t.Errorf("回滚重试应用旧版本内容（同一路径）+ 原地址, got %+v", calls[1])
	}
	// 回滚后 target 内容必须是旧版本。
	content, err := os.ReadFile(f.paths.Target)
	if err != nil || string(content) != "old-official-binary" {
		t.Errorf("回滚后 target 应回到旧版本, got (%q, %v)", content, err)
	}
	// daemon 曾用新 target 启动：回滚时停新启旧（2 次启动）。
	if f.sess.startCalls != 2 {
		t.Errorf("daemon 康新启 2 次（新 + 回滚旧）, got %d", f.sess.startCalls)
	}
	if f.sess.stopCalls != 1 {
		t.Errorf("回滚应停止新 daemon 一次, got %d", f.sess.stopCalls)
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
	if !strings.Contains(res.Error, "dashboard") {
		t.Errorf("失败 result 应保留 dashboard 主失败, got %+v", res)
	}
}

// TestHelperRunner_ServeMissingDeps_RejectsRunningPlan plan 记录 dashboard 原在
// 运行而 helper 未装配 serve 依赖：拒绝执行并写失败 result，绝不完成替换。
func TestHelperRunner_ServeMissingDeps_RejectsRunningPlan(t *testing.T) {
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	// f.serve 保持 nil。

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("未装配 serve 依赖时不得接管 dashboard 运行态事务")
	}
	if !strings.Contains(err.Error(), "dashboard") {
		t.Errorf("错误应指向 dashboard 生命周期支持缺失, got %v", err)
	}
	// 短路发生在等待父进程与替换之前：target 仍是旧版本。
	content, err := os.ReadFile(f.paths.Target)
	if err != nil || string(content) != "old-official-binary" {
		t.Errorf("target 不得被替换, got (%q, %v)", content, err)
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
}

// TestHelperPlan_ServeAddrRequiredWhenRunning plan 记录 dashboard 在运行但缺少
// serve_addr：校验必须拒绝（helper 恢复只认 plan 内的原地址）。
func TestHelperPlan_ServeAddrRequiredWhenRunning(t *testing.T) {
	plan := helperPlan{
		Nonce:           "0123456789abcdef0123456789abcdef",
		TargetBasename:  "token-usage.exe",
		ServeWasRunning: true,
		Parent:          ProcessIdentity{PID: 4242, CreationTime: 0xabcdef},
	}
	if err := validateHelperPlanFields(plan); err == nil {
		t.Error("ServeWasRunning=true 且缺少 serve_addr 应被拒绝")
	}
	plan.ServeAddr = "127.0.0.1:8619"
	if err := validateHelperPlanFields(plan); err != nil {
		t.Errorf("带 serve_addr 的合法 plan 不应被拒绝, err=%v", err)
	}
	// dashboard 未运行时 addr 可空（不参与校验）。
	plan.ServeWasRunning = false
	plan.ServeAddr = ""
	if err := validateHelperPlanFields(plan); err != nil {
		t.Errorf("未运行 dashboard 的 plan 不需要 serve_addr, err=%v", err)
	}
}

// ---- 替换前置步骤失败的事务回滚（P2 修复的覆盖）----

// TestHelperRunner_InspectFailureRestoresServices Inspect 失败（替换未发生）：
// helper 按事务语义恢复 daemon（旧 target）与 dashboard（原地址），写失败 result。
func TestHelperRunner_InspectFailureRestoresServices(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake
	f.sess.inspectErr = errors.New("inspect exploded")

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("Inspect 失败必须返回错误")
	}
	// daemon 与 dashboard 都以旧 target（未替换）恢复。
	if f.sess.startCalls != 1 {
		t.Errorf("daemon 应回滚重启一次, got %d", f.sess.startCalls)
	}
	if f.sess.startPaths != nil && f.sess.startPaths[0] != f.paths.Target {
		t.Errorf("daemon 应回滚到旧 target, got %q", f.sess.startPaths[0])
	}
	if serveFake.startCount() != 1 || serveFake.lastStart().addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应按原地址恢复一次, got (%d, %+v)", serveFake.startCount(), serveFake.lastStart())
	}
	// target 未被替换。
	content, rerr := os.ReadFile(f.paths.Target)
	if rerr != nil || string(content) != "old-official-binary" {
		t.Errorf("target 不得被替换, got (%q, %v)", content, rerr)
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
}

// TestHelperRunner_BackupFailureRestoresServices 备份失败（旧 hash 校验不过）：
// 替换未发生，恢复 daemon 与 dashboard 原运行态。
func TestHelperRunner_BackupFailureRestoresServices(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake
	// 使备份前 hash 校验失败：plan 记录与磁盘内容不符的旧 hash。
	f.plan.OldSHA256 = "deadbeef"
	if err := writeHelperPlan(f.paths.Plan, f.plan); err != nil {
		t.Fatalf("rewrite plan: %v", err)
	}

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("备份失败必须返回错误")
	}
	if f.sess.startCalls != 1 || serveFake.startCount() != 1 {
		t.Errorf("daemon 与 dashboard 都应恢复, daemon=%d serve=%d", f.sess.startCalls, serveFake.startCount())
	}
	if serveFake.startCount() == 1 && serveFake.lastStart().addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应按原地址恢复, got %+v", serveFake.lastStart())
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
}

// TestHelperRunner_MoveFailureRestoresServices MoveFileEx 失败：回滚后恢复
// daemon 与 dashboard 原运行态。
func TestHelperRunner_MoveFailureRestoresServices(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake
	f.mover = errorFileMover{err: errors.New("move denied")}

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("移动失败必须返回错误")
	}
	if f.sess.startCalls != 1 {
		t.Errorf("daemon 应回滚重启一次, got %d", f.sess.startCalls)
	}
	if serveFake.startCount() != 1 || serveFake.lastStart().addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应按原地址恢复一次, got (%d, %+v)", serveFake.startCount(), serveFake.lastStart())
	}
	content, rerr := os.ReadFile(f.paths.Target)
	if rerr != nil || string(content) != "old-official-binary" {
		t.Errorf("回滚后 target 应回到旧版本, got (%q, %v)", content, rerr)
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
	if !strings.Contains(res.Error, "move denied") {
		t.Errorf("失败 result 应保留主失败, got %+v", res)
	}
	if res.Rollback != "" {
		// 回滚与两个服务的恢复全部成功：Rollback 字段应为空。
		t.Errorf("回滚成功时 Rollback 字段应为空, got %q", res.Rollback)
	}
}

// TestHelperRunner_DaemonUnexpectedlyRunningRestoresServeOnly daemon 在停止后
// 意外运行：放弃替换（不触碰他人启动的 daemon），只按事务语义恢复 dashboard。
func TestHelperRunner_DaemonUnexpectedlyRunningRestoresServeOnly(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = serveFake
	f.sess.state.Running = true

	err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan)
	if err == nil {
		t.Fatal("daemon 意外运行必须放弃替换并返回错误")
	}
	if f.sess.startCalls != 0 {
		t.Errorf("他人启动的 daemon 不得被触碰, got %d", f.sess.startCalls)
	}
	if serveFake.startCount() != 1 || serveFake.lastStart().addr != "127.0.0.1:8619" {
		t.Errorf("dashboard 应回滚恢复一次, got (%d, %+v)", serveFake.startCount(), serveFake.lastStart())
	}
	res := f.readResult(t)
	if res.Success {
		t.Error("应写失败 result")
	}
}
