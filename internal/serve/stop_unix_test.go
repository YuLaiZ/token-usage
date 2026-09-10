//go:build unix

package serve

// serve_stop_unix_test.go 在类 Unix 平台对信号 seam 的真实现做行为覆盖：
// 对真实子进程发 SIGTERM/SIGKILL，断言进程确被终止（编译 + 行为双覆盖；
// Windows 实现由 GOOS=windows go build 验证编译）。

import (
	"os/exec"
	"testing"
	"time"
)

// startSleepChild 启动一个 sleep 子进程供信号测试;测试进程对子进程的生命
// 周期负责(Wait 或 Kill 兜底)。
func startSleepChild(t *testing.T) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("无 sleep 命令可用于信号测试: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sleep 子进程失败: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

func TestServeStopSignalPlatform_TerminatesProcess(t *testing.T) {
	cmd := startSleepChild(t)
	if err := signalProcPlatform(cmd.Process.Pid); err != nil {
		t.Fatalf("SIGTERM 发送失败: %v", err)
	}
	// sleep 被 SIGTERM 终止后 Wait 返回(带信号退出错误);限定时间内未返回
	// 说明信号未生效。
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// 进程已终止,符合预期(退出码类型不区分信号种类)。
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM 后子进程 5s 内未退出")
	}
}

func TestServeStopKillPlatform_KillsProcess(t *testing.T) {
	cmd := startSleepChild(t)
	if err := killProcPlatform(cmd.Process.Pid); err != nil {
		t.Fatalf("SIGKILL 发送失败: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL 后子进程 5s 内未退出")
	}
}
