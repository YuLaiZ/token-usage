//go:build unix

package cli

// serve_hyperlink_e2e_test.go 在类 Unix 平台验证 serve 启动行的超链接降级
// 合同：测试输出 buffer 不是终端，默认 writerIsTerminal 判定非 TTY，启动行
// 必须是纯文本（不含 OSC 转义序列）。serve 会阻塞等待信号，因此用自进程
// SIGINT 驱动优雅停止；guard 通道先于 serve 注册，保证测试进程不被信号的
// 默认终止行为杀掉。

import (
	"bytes"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// lockedBuffer 是并发安全的输出缓冲：Execute 在后台 goroutine 写入，
// 主测试 goroutine 轮询读取。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServeCmd_StartupLinePlainTextWhenNotTTY(t *testing.T) {
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, os.Interrupt, syscall.SIGTERM)
	// 测试结束注销 guard 即可,不发兜底信号:正常路径 serve 已自行退出;
	// 失败路径的悬挂 goroutine 不阻碍测试进程结束。若在注销 guard 前后
	// 向自进程发信号,存在交付与注销竞态,信号可能走默认行为终止整个
	// 测试进程,故严禁在此兜底发信号。
	t.Cleanup(func() { signal.Stop(guard) })

	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return db.Open(":memory:") },
		"test-version",
	)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0"})
	out := &lockedBuffer{}
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Execute() }()

	// 轮询等待启动行出现;启动行打印前 serve 已注册信号监听,此后发信号必达。
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "dashboard served at") {
		if time.Now().After(deadline) {
			t.Fatalf("10s 内未出现启动行,输出: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	startup := out.String()
	if !strings.Contains(startup, "http://127.0.0.1:") {
		t.Errorf("启动行应含 URL,实际 %q", startup)
	}
	if strings.Contains(startup, "\x1b]") {
		t.Errorf("非 TTY 输出不应含 OSC 转义序列,实际 %q", startup)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("发送 SIGINT 失败: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve 应正常退出,实际错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve 未在超时内退出")
	}
	if !strings.Contains(out.String(), "dashboard stopped") {
		t.Errorf("停止后应输出停止行,实际 %q", out.String())
	}
}
