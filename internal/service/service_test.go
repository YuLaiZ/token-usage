// internal/service/service_test.go
package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// fakeAutoStartManager 是可注入的 AutoStartManager，记录调用并返回可控行为。
// 命名 Ensure compile-time satisfaction of both interfaces（生产实现同样满足）。
type fakeAutoStartManager struct {
	platform     string
	statusResult AutoStartStatus
	statusErr    error
	enableErr    error
	disableErr   error

	enableCalls  []Options
	disableCalls []Options
}

func (f *fakeAutoStartManager) Enable(opts Options) error {
	f.enableCalls = append(f.enableCalls, opts)
	return f.enableErr
}

func (f *fakeAutoStartManager) Disable(opts Options) error {
	f.disableCalls = append(f.disableCalls, opts)
	return f.disableErr
}

func (f *fakeAutoStartManager) Status(opts Options) (AutoStartStatus, error) {
	return f.statusResult, f.statusErr
}

func (f *fakeAutoStartManager) Platform() string { return f.platform }

// 编译期保证 fakeAutoStartManager 实现 AutoStartManager。
var _ AutoStartManager = (*fakeAutoStartManager)(nil)

func cfgWith(autostart bool) *config.Config {
	return &config.Config{DataDir: "/data", Daemon: config.DaemonConfig{AutoStart: autostart}}
}

// false→true 且定义缺失 → Enable，triggered=true
func TestSync_EnableWhenDefinitionMissing_CallsEnable(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: false}}
	triggered, err := SyncWith(cfgWith(true), f)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !triggered {
		t.Error("triggered 应为 true（执行了 Enable）")
	}
	if len(f.enableCalls) != 1 {
		t.Fatalf("应仅调一次 Enable，实际 %d 次", len(f.enableCalls))
	}
	got := f.enableCalls[0]
	if len(got.Args) != 1 || got.Args[0] != "_run" {
		t.Errorf("Enable 应收到 Args=[_run]，实际 %v", got.Args)
	}
	if len(f.disableCalls) != 0 {
		t.Errorf("不应调 Disable，实际 %v", f.disableCalls)
	}
}

func TestSyncWithReport_EnableIsTransitionNotDriftRepair(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: false}}
	report, err := SyncWithReport(cfgWith(true), f)
	if err != nil {
		t.Fatal(err)
	}
	if report.Before.Exists || !report.After.Exists || !report.After.SpecMatches {
		t.Errorf("定义前后状态不准确: %+v", report)
	}
	if !report.Triggered || report.DriftRepaired {
		t.Errorf("首次启用应触发但不是漂移修复: %+v", report)
	}
}

// true→false 且定义存在 → Disable，triggered=true
func TestSync_DisableWhenDefinitionPresent_CallsDisable(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: true, SpecMatches: true}}
	triggered, err := SyncWith(cfgWith(false), f)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !triggered {
		t.Error("triggered 应为 true（执行了 Disable）")
	}
	if len(f.disableCalls) != 1 {
		t.Errorf("应仅调一次 Disable，实际 %v", f.disableCalls)
	}
	if len(f.enableCalls) != 0 {
		t.Errorf("不应调 Enable，实际 %v", f.enableCalls)
	}
}

// 同值漂移（Exists=true, SpecMatches=false）→ Disable+Enable，triggered=true
func TestSync_DriftRepair_CallsDisableThenEnable(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: true, SpecMatches: false}}
	triggered, err := SyncWith(cfgWith(true), f)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !triggered {
		t.Error("triggered 应为 true（漂移修复）")
	}
	if len(f.disableCalls) != 1 || len(f.enableCalls) != 1 {
		t.Fatalf("应先 Disable 再 Enable，实际 disable=%d enable=%d", len(f.disableCalls), len(f.enableCalls))
	}
}

func TestSyncWithReport_DriftRepairPreservesBeforeAndAfter(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: true, SpecMatches: false}}
	report, err := SyncWithReport(cfgWith(true), f)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Before.Exists || report.Before.SpecMatches {
		t.Errorf("同步前状态错误: %+v", report.Before)
	}
	if !report.After.Exists || !report.After.SpecMatches || !report.DriftRepaired {
		t.Errorf("同步后状态或漂移标记错误: %+v", report)
	}
}

// 已收敛（autostart=true, Exists=true, SpecMatches=true）→ noop，triggered=false
func TestSync_AlreadyConverged_Noop(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: true, SpecMatches: true}}
	triggered, err := SyncWith(cfgWith(true), f)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if triggered {
		t.Error("triggered 应为 false（noop）")
	}
	if len(f.enableCalls) != 0 || len(f.disableCalls) != 0 {
		t.Errorf("不应调用 Enable/Disable，实际 enable=%d disable=%d", len(f.enableCalls), len(f.disableCalls))
	}
}

// autostart=false 且定义已不存在 → noop（已收敛）
func TestSync_DisableAlreadyGone_Noop(t *testing.T) {
	f := &fakeAutoStartManager{statusResult: AutoStartStatus{Exists: false}}
	triggered, err := SyncWith(cfgWith(false), f)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if triggered {
		t.Error("triggered 应为 false（已收敛 noop）")
	}
	if len(f.enableCalls) != 0 || len(f.disableCalls) != 0 {
		t.Errorf("不应调用 Enable/Disable，实际 enable=%d disable=%d", len(f.enableCalls), len(f.disableCalls))
	}
}

// autostart=true 且平台不支持 → 返回 ErrPlatformUnsupported
func TestSync_EnableOnUnsupported_ReturnsErrPlatformUnsupported(t *testing.T) {
	f := &fakeAutoStartManager{statusErr: ErrPlatformUnsupported}
	triggered, err := SyncWith(cfgWith(true), f)
	if !errors.Is(err, ErrPlatformUnsupported) {
		t.Fatalf("应返回 ErrPlatformUnsupported，实际 err=%v", err)
	}
	if triggered {
		t.Error("triggered 应为 false")
	}
}

// autostart=false 且平台不支持 → 静默跳过（err=nil）
func TestSync_DisableOnUnsupported_SilentSkip(t *testing.T) {
	f := &fakeAutoStartManager{statusErr: ErrPlatformUnsupported}
	triggered, err := SyncWith(cfgWith(false), f)
	if err != nil {
		t.Fatalf("关闭自启平台不支持应静默跳过，err=%v", err)
	}
	if triggered {
		t.Error("triggered 应为 false")
	}
}

// 漂移修复时 Disable 失败 → 返回 error，triggered=true（确实执行过动作），不再 Enable
func TestSync_DriftDisableFails_DoesNotEnable(t *testing.T) {
	f := &fakeAutoStartManager{
		statusResult: AutoStartStatus{Exists: true, SpecMatches: false},
		disableErr:   errors.New("disable failed"),
	}
	triggered, err := SyncWith(cfgWith(true), f)
	if err == nil {
		t.Fatal("Disable 失败应返回 error")
	}
	if !triggered {
		t.Error("triggered 应为 true（已调用 Disable，即使修复未完成）")
	}
	if len(f.disableCalls) != 1 {
		t.Errorf("应调一次 Disable，实际 %d", len(f.disableCalls))
	}
	if len(f.enableCalls) != 0 {
		t.Errorf("Disable 失败后不应 Enable，实际 %d", len(f.enableCalls))
	}
}

// SyncForInstallFailed: 真实 install 失败（非平台不支持）应与 ErrPlatformUnsupported 可区分。
func TestSync_EnableFails_DistinguishableFromPlatformUnsupported(t *testing.T) {
	f := &fakeAutoStartManager{
		statusResult: AutoStartStatus{Exists: false},
		enableErr:    errors.New("real install write failed"),
	}
	triggered, err := SyncWith(cfgWith(true), f)
	if err == nil {
		t.Fatal("Enable 失败应返回 error")
	}
	if errors.Is(err, ErrPlatformUnsupported) {
		t.Fatal("真实 install 失败不应被识别为 ErrPlatformUnsupported")
	}
	if !triggered {
		t.Fatal("已调用 Enable 时 Triggered 必须为 true")
	}
}

func TestBuildOptions(t *testing.T) {
	opts := buildOptions(&config.Config{DataDir: "/data"})
	if opts.Label != Label {
		t.Errorf("Label=%q want %q", opts.Label, Label)
	}
	if opts.BinPath == "" {
		t.Error("BinPath 不应为空（os.Executable）")
	}
	if opts.DataDir != "/data" {
		t.Errorf("DataDir=%q want /data", opts.DataDir)
	}
	if len(opts.Args) != 1 || opts.Args[0] != "_run" {
		t.Errorf("Args=%v want [_run]", opts.Args)
	}
}

func TestSyncWithReport_ExecutableLookupFailureStopsBeforeStatus(t *testing.T) {
	original := executablePath
	lookupErr := errors.New("executable lookup failed")
	executablePath = func() (string, error) { return "", lookupErr }
	t.Cleanup(func() { executablePath = original })

	f := &fakeAutoStartManager{}
	report, err := SyncWithReport(cfgWith(true), f)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("应传播可执行文件探测错误，report=%+v err=%v", report, err)
	}
	if len(f.enableCalls) != 0 || len(f.disableCalls) != 0 {
		t.Fatalf("参数构造失败后不得修改自启定义，enable=%d disable=%d",
			len(f.enableCalls), len(f.disableCalls))
	}
}

// buildOptions 应展开 DataDir 的 ~ 前缀，使服务定义（plist/注册表）的日志路径为绝对路径。
func TestBuildOptions_ExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	cfg := &config.Config{DataDir: "~/.token-usage"}
	opts := buildOptions(cfg)
	want := filepath.Join(home, ".token-usage")
	if opts.DataDir != want {
		t.Errorf("DataDir=%q want %q（~ 应被展开为 $HOME）", opts.DataDir, want)
	}
	if strings.HasPrefix(opts.DataDir, "~") {
		t.Errorf("DataDir=%q 不应保留 ~ 前缀", opts.DataDir)
	}
	if cfg.DataDir != "~/.token-usage" {
		t.Errorf("cfg.DataDir 被改为 %q，应保留原值 ~/.token-usage", cfg.DataDir)
	}
}

func TestBuildOptions_AbsolutePathUnchanged(t *testing.T) {
	cfg := &config.Config{DataDir: "/var/lib/token-usage"}
	opts := buildOptions(cfg)
	if opts.DataDir != "/var/lib/token-usage" {
		t.Errorf("DataDir=%q want /var/lib/token-usage（绝对路径不应改动）", opts.DataDir)
	}
}
