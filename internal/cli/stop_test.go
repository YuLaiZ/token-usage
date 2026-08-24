// internal/cli/stop_test.go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/control"
)

// TestStopCmd_NoArgs stop 命令应声明 cobra.NoArgs。
func TestStopCmd_NoArgs(t *testing.T) {
	cmd := newStopCmd()
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

// TestStopCmd_LongSeparatesProcessAndAutostart stop Long 应明确「下次登录仍按自启配置决定」。
func TestStopCmd_LongSeparatesProcessAndAutostart(t *testing.T) {
	cmd := newStopCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("stop Long 应提及自启以区分当前进程与自启定义")
	}
}

// TestStopCmd_LoadConfigFails 配置缺失时返回 error（非零退出）。
func TestStopCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newStopCmd()
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
	cmd := newStopCmd()
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
	cmd := newStopCmd()
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
	cmd := newStopCmd()
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
