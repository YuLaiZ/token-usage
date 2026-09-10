// internal/cli/daemon_test.go
package cli

// daemon_test.go 覆盖 `token-usage daemon` 命令组的四个生命周期动作
// （start/status/stop/restart）的结果合同：已运行/未运行/失败的 stdout
// 语义与退出码、前置拦截、autostart 解耦展示与启动阶段展示。

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/service"
)

var errStartBoom = errors.New("start boom")

// injectStartConfig 注入 startConfigLoader 替身，返回还原函数。
// start 前置谓词读取配置，测试须固定配置内容避免依赖开发机 ~/.token-usage。
func injectStartConfig(cfg *config.Config, cfgErr error) func() {
	orig := startConfigLoader
	startConfigLoader = func() (*config.Config, error) { return cfg, cfgErr }
	return func() { startConfigLoader = orig }
}

// startCfgEnabled 返回带一个 enabled JSONL 客户端的最小配置（谓词放行）。
func startCfgEnabled() *config.Config {
	return &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/tmp/claude"}},
		},
	}
}

// stubControlStartStop 实现 controlStartStopper，供 start/stop/status/restart 结果合同测试注入。
type stubControlStartStop struct {
	startRes   control.StartResult
	startErr   error
	stopRes    control.StopResult
	stopErr    error
	restartRes control.RestartResult
	restartErr error
	inspectSt  control.RuntimeState
	inspectErr error
}

func (s *stubControlStartStop) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	return s.startRes, s.startErr
}
func (s *stubControlStartStop) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return s.stopRes, s.stopErr
}
func (s *stubControlStartStop) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return s.restartRes, s.restartErr
}
func (s *stubControlStartStop) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return s.inspectSt, s.inspectErr
}

// ---- daemon 命令组形态 ----

// TestDaemonCmd_BareShowsHelpUnknownSubFails daemon 命令组：裸执行显示帮助
// 且无错误；带未知子命令按 unknown command 失败（不得静默降级为帮助）。
// 裸执行不触碰 control.Manager/进程的副作用边界由命令面测试锁定。
func TestDaemonCmd_BareShowsHelpUnknownSubFails(t *testing.T) {
	// 裸执行：显示帮助，无错误。
	cmd := newDaemonCmd()
	cmd.SetArgs(nil)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("裸 daemon 应成功显示帮助，实际: %v", err)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("帮助应列出动作 %q:\n%s", want, out.String())
		}
	}

	// 未知子命令：按 unknown command 失败（非静默帮助、非任意错误）。
	cmd2 := newDaemonCmd()
	cmd2.SetArgs([]string{"frobnicate"})
	var out2, errOut2 bytes.Buffer
	cmd2.SetOut(&out2)
	cmd2.SetErr(&errOut2)
	err := cmd2.Execute()
	if err == nil {
		t.Fatal("daemon 未知子命令应报错")
	}
	if combined := out2.String() + errOut2.String(); !strings.Contains(combined, "unknown command") {
		t.Errorf("daemon 未知子命令应报 unknown command，实际:\n%s", combined)
	}
}

// TestDaemonCmd_MountsFourLifecycleActions daemon 命令组必须恰好注册
// start/status/stop/restart 四个可见动作，无多余子命令。
func TestDaemonCmd_MountsFourLifecycleActions(t *testing.T) {
	cmd := newDaemonCmd()
	want := map[string]bool{"start": true, "status": true, "stop": true, "restart": true}
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		if sub.Hidden {
			continue
		}
		got[sub.Name()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("daemon 缺少子命令 %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("daemon 出现意外子命令 %q", name)
		}
	}
}

// TestServeCmd_MountsFourLifecycleActions serve 命令组必须恰好注册
// start/status/stop/restart 四个可见动作，无多余子命令（与 daemon 侧对称）。
func TestServeCmd_MountsFourLifecycleActions(t *testing.T) {
	cmd := newServeCmd(buildinfo.Info{})
	want := map[string]bool{"start": true, "status": true, "stop": true, "restart": true}
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		if sub.Hidden {
			continue
		}
		got[sub.Name()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("serve 缺少子命令 %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("serve 出现意外子命令 %q", name)
		}
	}
}

// ---- daemon start ----

// TestDaemonStartCmd_NoArgs start 命令应声明 cobra.NoArgs（拒绝多余参数）。
func TestDaemonStartCmd_NoArgs(t *testing.T) {
	cmd := newDaemonStartCmd()
	if cmd.Args == nil {
		t.Fatal("start 命令应声明 Args（cobra.NoArgs）")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("NoArgs 应拒绝多余参数")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("NoArgs 对空参数应返回 nil，实际: %v", err)
	}
}

// TestDaemonStartCmd_LongSeparatesProcessAndAutostart Long 文案应明确「当前进程与自启定义分离」。
func TestDaemonStartCmd_LongSeparatesProcessAndAutostart(t *testing.T) {
	cmd := newDaemonStartCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("start Long 应提及自启定义以区分当前进程")
	}
}

// TestDaemonStartCmd_LoadConfigFails 配置缺失时 load 在 control lock 内失败 → 返回 error（非零退出）。
func TestDaemonStartCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newDaemonStartCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
}

// TestRunStart_AlreadyRunningContract start 已运行 → stdout 显示当前 PID，退出码 0（无 error）。
// 已在运行的实例不做前置拦截（谓词 false 也不提示）。
func TestRunStart_AlreadyRunningContract(t *testing.T) {
	restoreCfg := injectStartConfig(&config.Config{}, nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			inspectSt: control.RuntimeState{Running: true},
			startRes:  control.StartResult{PID: 4242, AlreadyRunning: true},
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("已运行应退出 0（无 error），实际: %v", e)
	}
	if !strings.Contains(out.String(), "4242") {
		t.Errorf("stdout 应显示当前 PID 4242，实际: %q", out.String())
	}
	if strings.Contains(out.String(), "No enabled clients") {
		t.Errorf("已在运行时不应前置拦截: %q", out.String())
	}
}

// TestRunStart_StartedContract start 未运行 → spawn 成功 → stdout 显示新 PID，退出 0。
func TestRunStart_StartedContract(t *testing.T) {
	restoreCfg := injectStartConfig(startCfgEnabled(), nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			startRes: control.StartResult{PID: 5555, AlreadyRunning: false},
		}, nil
	}

	var out bytes.Buffer
	cmd := newDaemonStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("启动成功应退出 0，实际: %v", e)
	}
	if !strings.Contains(out.String(), "5555") {
		t.Errorf("stdout 应显示新 PID 5555，实际: %q", out.String())
	}
}

// TestRunStart_RealFailureReturnsContextError 真实失败 → 返回带上下文的 error
// （cobra 统一输出），命令自身不再手写 stderr（防 cause 双打）。
func TestRunStart_RealFailureReturnsContextError(t *testing.T) {
	restoreCfg := injectStartConfig(startCfgEnabled(), nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{startErr: errStartBoom}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("真实失败应退出非 0")
	}
	if !strings.Contains(err.Error(), "启动守护进程失败") || !strings.Contains(err.Error(), "start boom") {
		t.Errorf("返回 error 应含上下文与 cause: %v", err)
	}
	if errOut.String() != "" {
		t.Errorf("命令不得手写 stderr（由 cobra 统一输出）: %q", errOut.String())
	}
}

// TestRunStart_NoTargetsPreflight 三态前置拦截：
// 全关 → 双语提示 + error（不调用 Start）；仅 router 配置无 enabled client → 同样拦截；
// 有 enabled 客户端 → 放行调用 Start。
func TestRunStart_NoTargetsPreflight(t *testing.T) {
	onlyRouterCfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: false, Router: "cc_switch"},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: "/tmp/cc-switch.db"},
		},
	}
	cases := []struct {
		name       string
		cfg        *config.Config
		wantBlock  bool
		wantStdout []string
	}{
		{"all disabled", &config.Config{}, true,
			[]string{"No enabled clients", "没有任何已启用的客户端", "token-usage config set clients.claude.enabled true"}},
		{"router configured but no enabled client", onlyRouterCfg, true,
			[]string{"No enabled clients"}},
		{"enabled client", startCfgEnabled(), false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreCfg := injectStartConfig(tc.cfg, nil)
			defer restoreCfg()
			orig := controlManagerFactory
			defer func() { controlManagerFactory = orig }()

			startCalled := false
			stub := &startCallRecorder{stub: stubControlStartStop{startRes: control.StartResult{PID: 5555}}, called: &startCalled}
			controlManagerFactory = func() (controlStartStopper, error) { return stub, nil }

			var out bytes.Buffer
			cmd := newDaemonStartCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.RunE(cmd, nil)

			if tc.wantBlock {
				if err == nil {
					t.Fatal("无监控目标应返回 error（exit 1）")
				}
				if startCalled {
					t.Error("前置拦截后不得调用 Start")
				}
				for _, want := range tc.wantStdout {
					if !strings.Contains(out.String(), want) {
						t.Errorf("stdout 缺少 %q，实际: %q", want, out.String())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("有监控目标应放行，实际: %v", err)
			}
			if !startCalled {
				t.Error("有监控目标必须调用 Start")
			}
			if strings.Contains(out.String(), "No enabled clients") {
				t.Errorf("有监控目标不应输出前置提示: %q", out.String())
			}
		})
	}
}

// startCallRecorder 记录 Start 是否被调用（前置拦截测试断言 Start 不可达）。
type startCallRecorder struct {
	stub   stubControlStartStop
	called *bool
}

func (s *startCallRecorder) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	*s.called = true
	return s.stub.Start(ctx, load)
}
func (s *startCallRecorder) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return s.stub.Stop(ctx, load)
}
func (s *startCallRecorder) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return s.stub.Restart(ctx, load)
}
func (s *startCallRecorder) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return s.stub.Inspect(ctx, cfg)
}

// ---- daemon status ----

// stubAutoStartManager 在 cli 包内提供的 service.AutoStartManager 最小桩，
// 用于 status 命令的状态分类测试。
type stubAutoStartManager struct {
	statusResult service.AutoStartStatus
	statusErr    error
}

func (s *stubAutoStartManager) Enable(opts service.Options) error  { return nil }
func (s *stubAutoStartManager) Disable(opts service.Options) error { return nil }
func (s *stubAutoStartManager) Status(opts service.Options) (service.AutoStartStatus, error) {
	return s.statusResult, s.statusErr
}
func (s *stubAutoStartManager) Platform() string { return "test" }

func cfgWithAutostart(autostart bool) *config.Config {
	return &config.Config{DataDir: "/data/status-test", Daemon: config.DaemonConfig{AutoStart: autostart}}
}

func runStatusAutostart(mgr service.AutoStartManager, cfg *config.Config) string {
	var out bytes.Buffer
	printAutoStartStatus(&out, cfg, mgr)
	return out.String()
}

// 组合 1：autostart=true + Exists=true + SpecMatches=true → 已启用（无警告）
func TestPrintAutoStartStatus_EnabledConverged(t *testing.T) {
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: true, SpecMatches: true}}
	got := runStatusAutostart(mgr, cfgWithAutostart(true))
	if !strings.Contains(got, "已启用") {
		t.Errorf("组合 1 应输出「已启用」, 实际: %q", got)
	}
	if strings.Contains(got, "⚠") {
		t.Errorf("组合 1 不应有警告, 实际: %q", got)
	}
}

// 组合 2：autostart=true + Exists=false → 定义丢失，建议重新保存配置（漂移）
// （替代旧 DefinitionExists 的「真漂移」分支：新接口直接用 Exists 判定）
func TestPrintAutoStartStatus_DefinitionMissing(t *testing.T) {
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: false}}
	got := runStatusAutostart(mgr, cfgWithAutostart(true))
	if !strings.Contains(got, "⚠") || !strings.Contains(got, "建议重新保存配置") {
		t.Errorf("组合 2 应输出漂移/建议重新保存警告, 实际: %q", got)
	}
}

// 组合 3：autostart=true + Exists=true + SpecMatches=false → 内容不一致，建议重新保存配置（漂移）
// （替代旧 DefinitionExists 的「良性停止」分支：新接口移除该概念，统一为「建议重新保存配置」）
func TestPrintAutoStartStatus_SpecDrift(t *testing.T) {
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: true, SpecMatches: false}}
	got := runStatusAutostart(mgr, cfgWithAutostart(true))
	if !strings.Contains(got, "⚠") || !strings.Contains(got, "建议重新保存配置") {
		t.Errorf("组合 3 应输出内容不一致/建议重新保存警告, 实际: %q", got)
	}
}

// 组合 4：autostart=false + Exists=true → 残留，建议重新保存配置
func TestPrintAutoStartStatus_StaleDefinition(t *testing.T) {
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: true}}
	got := runStatusAutostart(mgr, cfgWithAutostart(false))
	if !strings.Contains(got, "⚠") || !strings.Contains(got, "建议重新保存配置") {
		t.Errorf("组合 4 应输出残留/建议重新保存警告, 实际: %q", got)
	}
}

// 组合 5：autostart=false + Exists=false → 未启用（已收敛，无警告）
func TestPrintAutoStartStatus_DisabledClean(t *testing.T) {
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: false}}
	got := runStatusAutostart(mgr, cfgWithAutostart(false))
	if !strings.Contains(got, "未启用") {
		t.Errorf("组合 5 应输出「未启用」, 实际: %q", got)
	}
	if strings.Contains(got, "⚠") {
		t.Errorf("组合 5 不应有警告, 实际: %q", got)
	}
}

// 平台不支持（Status 返回 err）→ 打印 autostart 值 + 检测失败，不报错
func TestPrintAutoStartStatus_PlatformUnsupported(t *testing.T) {
	mgr := &stubAutoStartManager{statusErr: service.ErrPlatformUnsupported}
	got := runStatusAutostart(mgr, cfgWithAutostart(true))
	if !strings.Contains(got, "检测失败") {
		t.Errorf("平台不支持应输出「检测失败」, 实际: %q", got)
	}
	if !strings.Contains(got, "开") {
		t.Errorf("应显示 autostart 配置值「开」, 实际: %q", got)
	}
}

func TestBoolText(t *testing.T) {
	if boolText(true) != "on / 开" {
		t.Error("boolText(true) 应为 on / 开")
	}
	if boolText(false) != "off / 关" {
		t.Error("boolText(false) 应为 off / 关")
	}
}

// status 命令在配置缺失时应返回 error（loadConfig 失败），不 panic
func TestStatusCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newDaemonStatusCmd()
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
	if !strings.Contains(err.Error(), "加载配置失败") {
		t.Errorf("错误应与配置加载相关，实际: %v", err)
	}
}

// status 命令在有效配置下应输出「守护进程未运行」（隔离 HOME + TempDir 保证 flock 可创建且无残留锁）
func TestStatusCmd_RunsAndOutputsNotRunning(t *testing.T) {
	dataDir := t.TempDir()
	setupHomeConfig(t, `data_dir = "`+dataDir+`"
[daemon]
poll_interval = 30
`)
	cmd := newDaemonStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("status 不应报错: %v", err)
	}
	if !strings.Contains(out.String(), "未运行") {
		t.Errorf("无守护进程时应输出「未运行」, 实际: %q", out.String())
	}
}

// ---- 启动阶段展示 ----
//
// printStartupPhase 据 control.RuntimeState 解释启动阶段（仅 Running 时调用）：
//
//	state 缺失/非法/不匹配 → 启动阶段未知
//	monitor_ready=false     → 监听初始化中
//	catch_up pending/running → 监听已就绪，正在补采
//	catch_up succeeded       → （无额外行，仅运行中）
//	catch_up failed          → 补采部分失败（N），执行 token-usage errors
//	PID 元数据不可用         → PID 元数据不可用

func runStartupPhase(st control.RuntimeState) string {
	var out bytes.Buffer
	printStartupPhase(&out, st)
	return out.String()
}

// 组合 1：state 缺失/非法/不匹配（PhaseAvailable=false，PID 可读）→ 启动阶段未知
func TestPrintStartupPhase_PhaseUnknown(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 1234, PhaseAvailable: false}
	got := runStartupPhase(st)
	if !strings.Contains(got, "启动阶段") || !strings.Contains(got, "未知") {
		t.Errorf("PhaseAvailable=false 应输出「启动阶段: 未知」, 实际: %q", got)
	}
}

// 组合 2：monitor_ready=false → 监听初始化中
func TestPrintStartupPhase_MonitorNotReady(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 1234, PhaseAvailable: true, MonitorReady: false}
	got := runStartupPhase(st)
	if !strings.Contains(got, "监听初始化中") {
		t.Errorf("monitor_ready=false 应输出「监听初始化中」, 实际: %q", got)
	}
}

// 组合 3：catch_up pending → 监听已就绪，正在补采
func TestPrintStartupPhase_CatchUpPending(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 1234, PhaseAvailable: true, MonitorReady: true, CatchUp: "pending"}
	got := runStartupPhase(st)
	if !strings.Contains(got, "监听已就绪，正在补采") {
		t.Errorf("catch_up=pending 应输出补采中, 实际: %q", got)
	}
}

// 组合 3b：catch_up running → 监听已就绪，正在补采
func TestPrintStartupPhase_CatchUpRunning(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 1234, PhaseAvailable: true, MonitorReady: true, CatchUp: "running"}
	got := runStartupPhase(st)
	if !strings.Contains(got, "监听已就绪，正在补采") {
		t.Errorf("catch_up=running 应输出补采中, 实际: %q", got)
	}
}

// 组合 4：catch_up succeeded → 无额外阶段行（仅运行中）
func TestPrintStartupPhase_CatchUpSucceeded_NoExtraLine(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 1234, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded"}
	got := runStartupPhase(st)
	if strings.TrimSpace(got) != "" {
		t.Errorf("catch_up=succeeded 不应有额外阶段行, 实际: %q", got)
	}
}

// 组合 5：catch_up failed → 补采部分失败（N），执行 token-usage errors
func TestPrintStartupPhase_CatchUpFailed(t *testing.T) {
	st := control.RuntimeState{
		Running: true, PID: 1234, PhaseAvailable: true, MonitorReady: true,
		CatchUp: "failed", CatchUpFailures: 3,
	}
	got := runStartupPhase(st)
	if !strings.Contains(got, "补采部分失败") || !strings.Contains(got, "（3）") {
		t.Errorf("catch_up=failed 应输出补采部分失败（3）, 实际: %q", got)
	}
	if !strings.Contains(got, "token-usage errors") {
		t.Errorf("catch_up=failed 应提示执行 token-usage errors, 实际: %q", got)
	}
}

// 组合 6：PID 元数据不可用（Running=true, PID=0）→ PID 元数据不可用
func TestPrintStartupPhase_PIDMetadataUnavailable(t *testing.T) {
	st := control.RuntimeState{Running: true, PID: 0, PhaseAvailable: false}
	got := runStartupPhase(st)
	if !strings.Contains(got, "PID 元数据不可用") {
		t.Errorf("PID 不可用应输出「PID 元数据不可用」, 实际: %q", got)
	}
	if strings.Contains(got, "启动阶段未知") {
		t.Errorf("PID 不可用应区别于「启动阶段未知」, 实际: %q", got)
	}
}

// 未运行时不输出阶段行（printStartupPhase 只在 Running 时被调用）。
func TestPrintStartupPhase_NotRunning_NoOutput(t *testing.T) {
	st := control.RuntimeState{Running: false}
	got := runStartupPhase(st)
	if strings.TrimSpace(got) != "" {
		t.Errorf("未运行时不应有阶段行, 实际: %q", got)
	}
}

// TestPrintStartupPhase_DoesNotAffectAutostartDrift 阶段信息不参与 autostart 漂移判断：
// 即便阶段是 failed（最坏状态），autostart 收敛分支仍按 Exists/SpecMatches 判定，不被阶段推翻。
func TestPrintStartupPhase_DoesNotAffectAutostartDrift(t *testing.T) {
	// autostart=false + 无定义 → 已收敛（无警告），与 daemon 阶段无关。
	mgr := &stubAutoStartManager{statusResult: service.AutoStartStatus{Exists: false}}
	cfg := cfgWithAutostart(false)
	var autoOut bytes.Buffer
	printAutoStartStatus(&autoOut, cfg, mgr)
	autoText := autoOut.String()
	if strings.Contains(autoText, "⚠") {
		t.Errorf("收敛分支不应有警告, 实际: %q", autoText)
	}
	// 阶段为 failed 时仍单独打印，不影响 autostart 输出。
	phaseText := runStartupPhase(control.RuntimeState{
		Running: true, PID: 1, PhaseAvailable: true, MonitorReady: true,
		CatchUp: "failed", CatchUpFailures: 9,
	})
	if !strings.Contains(phaseText, "补采部分失败") {
		t.Errorf("阶段 failed 应打印, 实际: %q", phaseText)
	}
	// 阶段文本不得污染 autostart 文本（互不引用）。
	if strings.Contains(autoText, "补采") || strings.Contains(phaseText, "开机自启") {
		t.Errorf("阶段与 autostart 展示应相互独立, auto=%q phase=%q", autoText, phaseText)
	}
}

// ---- daemon stop ----

// TestDaemonStopCmd_NoArgs stop 命令应声明 cobra.NoArgs。
func TestDaemonStopCmd_NoArgs(t *testing.T) {
	cmd := newDaemonStopCmd()
	if cmd.Args == nil {
		t.Fatal("stop 命令应声明 Args（cobra.NoArgs）")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("NoArgs 应拒绝多余参数")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("NoArgs 对空参数应返回 nil，实际: %v", err)
	}
}

// TestDaemonStopCmd_LongSeparatesProcessAndAutostart stop Long 应明确「下次登录仍按自启配置决定」。
func TestDaemonStopCmd_LongSeparatesProcessAndAutostart(t *testing.T) {
	cmd := newDaemonStopCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("stop Long 应提及自启以区分当前进程与自启定义")
	}
}

// TestDaemonStopCmd_LoadConfigFails 配置缺失时返回 error（非零退出）。
func TestDaemonStopCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newDaemonStopCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
}

// TestRunStop_NotRunningContract stop 未运行 → stdout 显示未运行，退出 0（幂等）。
func TestRunStop_NotRunningContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			stopRes: control.StopResult{WasRunning: false},
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonStopCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("未运行应退出 0（幂等），实际: %v", e)
	}
	if !strings.Contains(out.String(), "未运行") {
		t.Errorf("stdout 应显示未运行，实际: %q", out.String())
	}
}

// TestRunStop_StoppedContract stop 运行中 → 成功停止 → stdout 显示 PID，退出 0。
func TestRunStop_StoppedContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			stopRes: control.StopResult{PID: 7777, WasRunning: true},
		}, nil
	}

	var out bytes.Buffer
	cmd := newDaemonStopCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("成功停止应退出 0，实际: %v", e)
	}
	if !strings.Contains(out.String(), "7777") {
		t.Errorf("stdout 应显示已停止 PID 7777，实际: %q", out.String())
	}
}

// TestRunStop_RealFailureReturnsContextError 真实失败 → 返回带上下文的 error
// （cobra 统一输出），命令自身不再手写 stderr（防 cause 双打）。
func TestRunStop_RealFailureReturnsContextError(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	stopErr := errStartBoom // 复用哨兵
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{stopErr: stopErr}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonStopCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("真实失败应退出非 0")
	}
	if !strings.Contains(err.Error(), "停止守护进程失败") || !strings.Contains(err.Error(), errStartBoom.Error()) {
		t.Errorf("返回 error 应含上下文与 cause: %v", err)
	}
	if errOut.String() != "" {
		t.Errorf("命令不得手写 stderr（由 cobra 统一输出）: %q", errOut.String())
	}
}

// ---- daemon restart ----

// TestDaemonRestartCmd_NoArgs restart 命令应声明 cobra.NoArgs（拒绝多余参数）。
func TestDaemonRestartCmd_NoArgs(t *testing.T) {
	cmd := newDaemonRestartCmd()
	if cmd.Args == nil {
		t.Fatal("restart 命令应声明 Args（cobra.NoArgs）")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("NoArgs 应拒绝多余参数")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("NoArgs 对空参数应返回 nil，实际: %v", err)
	}
}

// TestDaemonRestartCmd_Short restart Short 应为双语并列。
func TestDaemonRestartCmd_Short(t *testing.T) {
	cmd := newDaemonRestartCmd()
	if cmd.Short != "Restart the daemon / 重启当前守护进程" {
		t.Errorf("restart Short=%q want %q", cmd.Short, "Restart the daemon / 重启当前守护进程")
	}
}

// TestDaemonRestartCmd_LongMentionsNoAutostartTouch Long 应明确不触碰 config/plist/注册表，
// 并说明 macOS launchd 启动的旧进程在 restart 后失去 KeepAlive 的取舍。
func TestDaemonRestartCmd_LongMentionsNoAutostartTouch(t *testing.T) {
	cmd := newDaemonRestartCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("restart Long 应提及自启定义以明确不触碰")
	}
}

// TestDaemonRestartCmd_LoadConfigFails 配置缺失时返回 error（非零退出）。
func TestDaemonRestartCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newDaemonRestartCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
}

// TestRunRestart_RestartedContract restart 成功 → stdout 显示 PID old → new，退出 0。
func TestRunRestart_RestartedContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			restartRes: control.RestartResult{OldPID: 1234, NewPID: 5678},
		}, nil
	}

	var out bytes.Buffer
	cmd := newDaemonRestartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("成功重启应退出 0，实际: %v", e)
	}
	if !strings.Contains(out.String(), "1234") || !strings.Contains(out.String(), "5678") {
		t.Errorf("stdout 应显示 PID 1234 → 5678，实际: %q", out.String())
	}
	if !strings.Contains(out.String(), "已重启") {
		t.Errorf("stdout 应包含已重启，实际: %q", out.String())
	}
}

// TestRunRestart_NotRunningContract restart 未运行 → 提示 token-usage daemon start，退出非 0。
func TestRunRestart_NotRunningContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			restartErr: control.ErrRestartNotRunning,
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonRestartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("未运行应退出非 0")
	}
	// 未运行指引随 error 由 cobra 统一输出；关键含 daemon start 提示。
	if !strings.Contains(err.Error(), "token-usage daemon start") {
		t.Errorf("未运行 error 应含 token-usage daemon start 指引，实际: %v", err)
	}
}

// TestRunRestart_RealFailureReturnsContextError 真实失败（非 ErrRestartNotRunning）
// → 返回带上下文的 error（cobra 统一输出），命令自身不再手写 stderr（防 cause 双打）。
func TestRunRestart_RealFailureReturnsContextError(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{restartErr: errors.New("restart boom")}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newDaemonRestartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("真实失败应退出非 0")
	}
	if !strings.Contains(err.Error(), "重启守护进程失败") || !strings.Contains(err.Error(), "restart boom") {
		t.Errorf("返回 error 应含上下文与 cause: %v", err)
	}
	if errOut.String() != "" {
		t.Errorf("命令不得手写 stderr（由 cobra 统一输出）: %q", errOut.String())
	}
}
