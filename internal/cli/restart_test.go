// internal/cli/restart_test.go
package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/control"
)

// TestRestartCmd_NoArgs restart 命令应声明 cobra.NoArgs（拒绝多余参数）。
func TestRestartCmd_NoArgs(t *testing.T) {
	cmd := newRestartCmd()
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

// TestRestartCmd_Short restart Short 应为双语并列。
func TestRestartCmd_Short(t *testing.T) {
	cmd := newRestartCmd()
	if cmd.Short != "Restart the daemon / 重启当前守护进程" {
		t.Errorf("restart Short=%q want %q", cmd.Short, "重启当前守护进程")
	}
}

// TestRestartCmd_LongMentionsNoAutostartTouch Long 应明确不触碰 config/plist/注册表，
// 并说明 macOS launchd 启动的旧进程在 restart 后失去 KeepAlive 的取舍。
func TestRestartCmd_LongMentionsNoAutostartTouch(t *testing.T) {
	cmd := newRestartCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("restart Long 应提及自启定义以明确不触碰")
	}
}

// TestRestartCmd_LoadConfigFails 配置缺失时返回 error（非零退出）。
func TestRestartCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newRestartCmd()
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
	cmd := newRestartCmd()
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

// TestRunRestart_NotRunningContract restart 未运行 → 提示 token-usage start，退出非 0。
func TestRunRestart_NotRunningContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			restartErr: control.ErrRestartNotRunning,
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newRestartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("未运行应退出非 0")
	}
	// 未运行指引随 error 由 cobra 统一输出；关键含 start 提示。
	if !strings.Contains(err.Error(), "start") {
		t.Errorf("未运行 error 应含 token-usage start 指引，实际: %v", err)
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
	cmd := newRestartCmd()
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
