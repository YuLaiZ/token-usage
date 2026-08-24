// internal/cli/status_test.go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/service"
)

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

	cmd := newStatusCmd()
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
	cmd := newStatusCmd()
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

// stubControlStartStopper 在 cli 包内提供的 controlStartStopper 最小桩，
// 用于 status 阶段展示测试：返回预设的 RuntimeState。
type stubControlStartStopper struct {
	state control.RuntimeState
	err   error
}

func (s stubControlStartStopper) Start(context.Context, control.ConfigLoader) (control.StartResult, error) {
	return control.StartResult{}, nil
}
func (s stubControlStartStopper) Stop(context.Context, control.ConfigLoader) (control.StopResult, error) {
	return control.StopResult{}, nil
}
func (s stubControlStartStopper) Restart(context.Context, control.ConfigLoader) (control.RestartResult, error) {
	return control.RestartResult{}, nil
}
func (s stubControlStartStopper) Inspect(context.Context, *config.Config) (control.RuntimeState, error) {
	return s.state, s.err
}

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
