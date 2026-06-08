// internal/cli/start_test.go
package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

var errStartBoom = errors.New("start boom")

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

// TestStartCmd_NoArgs start 命令应声明 cobra.NoArgs（拒绝多余参数）。
func TestStartCmd_NoArgs(t *testing.T) {
	cmd := newStartCmd()
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

// TestStartCmd_LongSeparatesProcessAndAutostart Long 文案应明确「当前进程与自启定义分离」。
func TestStartCmd_LongSeparatesProcessAndAutostart(t *testing.T) {
	cmd := newStartCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("start Long 应提及自启定义以区分当前进程")
	}
}

// TestStartCmd_LoadConfigFails 配置缺失时 load 在 control lock 内失败 → 返回 error（非零退出）。
func TestStartCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newStartCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
}

// TestRunStart_AlreadyRunningContract start 已运行 → stdout 显示当前 PID，退出码 0（无 error）。
func TestRunStart_AlreadyRunningContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			startRes: control.StartResult{PID: 4242, AlreadyRunning: true},
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("已运行应退出 0（无 error），实际: %v", e)
	}
	if !strings.Contains(out.String(), "4242") {
		t.Errorf("stdout 应显示当前 PID 4242，实际: %q", out.String())
	}
}

// TestRunStart_StartedContract start 未运行 → spawn 成功 → stdout 显示新 PID，退出 0。
func TestRunStart_StartedContract(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			startRes: control.StartResult{PID: 5555, AlreadyRunning: false},
		}, nil
	}

	var out bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("启动成功应退出 0，实际: %v", e)
	}
	if !strings.Contains(out.String(), "5555") {
		t.Errorf("stdout 应显示新 PID 5555，实际: %q", out.String())
	}
}

// TestRunStart_RealFailureToStderr 真实失败 → stderr，退出非 0。
func TestRunStart_RealFailureToStderr(t *testing.T) {
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{startErr: errStartBoom}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("真实失败应退出非 0")
	}
	if !strings.Contains(errOut.String(), "启动守护进程失败") {
		t.Errorf("失败应写 stderr，实际 stderr: %q", errOut.String())
	}
}
