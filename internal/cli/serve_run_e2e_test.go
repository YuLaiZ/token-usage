//go:build unix

package cli

// serve_run_e2e_test.go 在类 Unix 平台对 Hidden 内部命令 _serve-run 做进程内
// 生命周期验证（参照 serve_hyperlink_e2e_test.go 的后台 Execute + 自进程信号
// 手法，不做真实 detached spawn）：断言 serve.json 的写出内容（PID 为本进程、
// addr 为实际监听地址）、/api/meta 可达、SIGTERM 优雅退出后状态文件自清理。

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestServeRunCmd_BackgroundLifecycle(t *testing.T) {
	// guard 先于 serve 注册,保证测试进程不被信号的默认终止行为杀掉;
	// 正常路径 serve 自行退出,失败路径的悬挂 goroutine 不阻碍测试结束。
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, os.Interrupt, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(guard) })

	dir := t.TempDir()
	origLoad := configLoaderForServeRun
	configLoaderForServeRun = func() (*config.Config, error) {
		return &config.Config{DataDir: dir}, nil
	}
	t.Cleanup(func() { configLoaderForServeRun = origLoad })

	cmd := newServeRunCmd("test-version")
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0"})
	out := &lockedBuffer{}
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Execute() }()

	// 轮询等待 serve.json 出现:子命令 Listen 成功才写。
	deadline := time.Now().Add(10 * time.Second)
	var st *ServeState
	var err error
	for {
		st, err = readServeState(dir)
		if err == nil && st != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("10s 内 serve.json 未写出,输出: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 状态文件内容:PID 为本进程,addr 为实际监听地址,started_at 为 RFC3339。
	if st.PID != os.Getpid() {
		t.Errorf("pid 应为本进程 %d,实际 %d", os.Getpid(), st.PID)
	}
	if st.Addr == "" || st.Addr == "127.0.0.1:0" {
		t.Errorf("addr 应为实际监听地址,实际 %q", st.Addr)
	}
	if _, err := time.Parse(time.RFC3339, st.StartedAt); err != nil {
		t.Errorf("started_at 应为 RFC3339,实际 %q: %v", st.StartedAt, err)
	}

	// /api/meta 可达。
	if !serveMetaAlive("http://"+st.Addr, 2*time.Second) {
		t.Errorf("后台服务 /api/meta 应可达: %s", st.Addr)
	}

	// 启动行落入输出(非 TTY buffer → 纯文本,无 OSC 转义)。
	startup := out.String()
	if !strings.Contains(startup, "dashboard served at http://"+st.Addr) {
		t.Errorf("输出应含启动行与实际 URL,实际 %q", startup)
	}
	if strings.Contains(startup, "\x1b]") {
		t.Errorf("非 TTY 输出不应含 OSC 转义序列,实际 %q", startup)
	}

	// SIGTERM 优雅退出,状态文件自清理。
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("_serve-run 应正常退出,实际错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("_serve-run 未在超时内退出")
	}
	if _, err := os.Stat(serveStatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("退出后 serve.json 应被自清理,stat err = %v", err)
	}
	if !strings.Contains(out.String(), "dashboard stopped") {
		t.Errorf("退出前应输出停止行,实际 %q", out.String())
	}
}
