package cli

// serve_start_test.go 驱动 serve start 的可进程内测分支：真实 spawn 交给
// 主线程人工 E2E，这里注入 serveSpawnAndWait 假实现断言编排逻辑——已运行
// 拒绝、陈旧放行（spawn 前清理）、成功输出、失败附日志尾、--open 警告。
// 探活与陈旧分支以 httptest 假 /api/meta 端点驱动，不发真信号。

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
)

// serveFamilyFixture 构造完整 serve 命令族（子命令继承 persistent flags），
// load 注入临时 DataDir。
func serveFamilyFixture(t *testing.T, dataDir string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: dataDir}, nil },
		"test-version",
	)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	return cmd, &out, &errBuf
}

// stubServeSpawnAndWait 注入假 spawn 并返回恢复函数。
func stubServeSpawnAndWait(t *testing.T, fn func(cfg *config.Config, addr, logPath string) (int, string, error)) {
	t.Helper()
	orig := serveSpawnAndWait
	serveSpawnAndWait = fn
	t.Cleanup(func() { serveSpawnAndWait = orig })
}

func TestServeStartCmd_AlreadyRunningIdempotent(t *testing.T) {
	// 这是「已有运行实例（seeded 状态 + 探活响应，等价于 serveDashboard
	// 守卫放行后写出的 serve.json）时再次 start」的回归：start 必须幂等
	// 拒绝——单实例契约对已运行实例一视同仁。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := writeServeState(dir, &ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	// 已运行时绝不 spawn。
	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		t.Error("已运行时不应 spawn")
		return 0, "", nil
	})

	cmd, outBuf, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	// 已运行为幂等返回（与 daemon start 的 AlreadyRunning 同语义）：exit 0、
	// 信息走 stdout、不倒 Usage。
	if err := cmd.Execute(); err != nil {
		t.Fatalf("已运行时 start 应幂等成功, err=%v", err)
	}
	msg := outBuf.String()
	for _, want := range []string{"already running", "4242", "http://" + addr, "serve stop", "已在后台运行"} {
		if !strings.Contains(msg, want) {
			t.Errorf("输出应含 %q,实际: %q", want, msg)
		}
	}
	// 状态文件保留（仍指向运行中的实例）。
	if _, err := os.Stat(serveStatePath(dir)); err != nil {
		t.Errorf("已运行拒绝时状态文件应保留: %v", err)
	}
}

func TestServeStartCmd_StaleStateRemovedBeforeSpawn(t *testing.T) {
	// 预留后关闭一个端口构成陈旧记录。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	// 存活的假端点作为假 spawn 的返回值。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := writeServeState(dir, &ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	stubServeSpawnAndWait(t, func(cfg *config.Config, addr, logPath string) (int, string, error) {
		// spawn 前陈旧状态必须已被清理,否则轮询会把旧文件当就绪信号。
		if st, err := readServeState(cfg.DataDir); err != nil || st != nil {
			t.Errorf("spawn 时陈旧状态应已被删除,实际 (%v, %v)", st, err)
		}
		return 4242, liveAddr, nil
	})

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("陈旧放行后 start 应成功,实际错误: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"dashboard started in background",
		"http://" + liveAddr,
		"4242",
		serveLogPath(dir),
		"token-usage serve stop",
		"仪表板已后台启动",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
}

func TestServeStartCmd_FailureIncludesLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := serveLogPath(dir)
	if err := os.WriteFile(logPath, []byte("line-1\nline-2\n监听 127.0.0.1:8619 失败\n"), 0644); err != nil {
		t.Fatalf("写日志: %v", err)
	}
	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		return 0, "", errors.New("background serve did not become ready")
	})

	cmd, _, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("spawn 失败应报错")
	}
	msg := execErr.Error()
	for _, want := range []string{
		"failed to start dashboard in background",
		"background serve did not become ready",
		"log tail:",
		"日志末尾",
		"监听 127.0.0.1:8619 失败",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
}

func TestServeStartCmd_FailureLogTailCappedAtTenLines(t *testing.T) {
	dir := t.TempDir()
	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		return 0, "", errors.New("not ready")
	})

	// 写 15 行带序号的日志,只应保留末尾 10 行。
	var logBuf strings.Builder
	for i := 1; i <= 15; i++ {
		logBuf.WriteString("unique-log-line-" + strings.Repeat("x", i) + "\n")
	}
	if err := os.WriteFile(serveLogPath(dir), []byte(logBuf.String()), 0644); err != nil {
		t.Fatalf("写日志: %v", err)
	}

	cmd, _, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("spawn 失败应报错")
	}
	msg := execErr.Error()
	if got := strings.Count(msg, "unique-log-line-"); got != 10 {
		t.Errorf("日志尾应恰好 10 行,实际 %d 行:\n%s", got, msg)
	}
	if strings.Contains(msg, "unique-log-line-x\n") {
		t.Errorf("最早的一行不应出现在日志尾:\n%s", msg)
	}
}

func TestServeStartCmd_OpenWarningOnFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		return 4242, liveAddr, nil
	})
	origOpen := serveOpenBrowser
	serveOpenBrowser = func(string) error { return errors.New("no browser") }
	t.Cleanup(func() { serveOpenBrowser = origOpen })

	cmd, _, errBuf := serveFamilyFixture(t, t.TempDir())
	cmd.SetArgs([]string{"start", "--open"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--open 失败不应致命,实际错误: %v", err)
	}
	for _, want := range []string{"failed to open browser", "打开浏览器失败"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr 应含 %q,实际: %q", want, errBuf.String())
		}
	}
}

func TestServeStartCmd_MissingAddr(t *testing.T) {
	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		t.Error("空 --addr 不应 spawn")
		return 0, "", nil
	})
	cmd, _, _ := serveFamilyFixture(t, t.TempDir())
	cmd.SetArgs([]string{"start", "--addr", ""})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("空 --addr 应报错")
	}
	msg := execErr.Error()
	for _, want := range []string{"missing required --addr", "token-usage serve start --addr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
}

// TestServeStartCmd_StartLockSerializes 覆盖跨进程启动互斥：测试先占住
// serve.lock 模拟另一个并发 start 在执行 → start 必须报双语错误且不 spawn；
// 释放锁后 start 走正常分支（seeded 陈旧态被清理 + spawn + 就绪输出）。
func TestServeStartCmd_StartLockSerializes(t *testing.T) {
	// 预留后关闭一个端口构成陈旧记录（第二阶段放行用）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	// 存活的假端点作为假 spawn 的返回值。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := writeServeState(dir, &ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	spawned := false
	stubServeSpawnAndWait(t, func(cfg *config.Config, addr, logPath string) (int, string, error) {
		spawned = true
		// 锁释放后 spawn 前陈旧状态必须已被清理,否则轮询会把旧文件当就绪信号。
		if st, err := readServeState(cfg.DataDir); err != nil || st != nil {
			t.Errorf("spawn 时陈旧状态应已被删除,实际 (%v, %v)", st, err)
		}
		return 4242, liveAddr, nil
	})

	// 阶段一：占住启动锁 → start 报错且不 spawn。
	lock, ok := daemon.AcquireLock(filepath.Join(dir, serveStartLockFile))
	if !ok {
		t.Fatal("测试前置:占用 serve-start.lock 失败")
	}
	cmd, _, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("启动锁被占用时 start 应报错")
	}
	msg := execErr.Error()
	for _, want := range []string{"another serve start is in progress", "另一个 serve start 正在执行"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
	if spawned {
		t.Error("启动锁被占用时不应 spawn")
	}

	// 阶段二：释放锁后 start 走正常分支。
	if err := daemon.ReleaseLock(lock); err != nil {
		t.Fatalf("释放启动锁: %v", err)
	}
	cmd2, out, _ := serveFamilyFixture(t, dir)
	cmd2.SetArgs([]string{"start"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("锁释放后 start 应成功,实际错误: %v", err)
	}
	if !spawned {
		t.Error("锁释放后应执行 spawn")
	}
	got := out.String()
	for _, want := range []string{"dashboard started in background", "仪表板已后台启动"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
}

// TestServeStartCmd_IdempotentRejectWhenStateReplaced 锁定 TOCTOU 重评估：
// 旧状态探活无响应（判定陈旧），条件删除时发现文件已被存活的新实例 B 接管——
// start 必须按新实例幂等拒绝（exit 0、不 spawn、不触碰 B 的状态文件），
// 绝不带着一份被误删的状态进入 spawn。
func TestServeStartCmd_IdempotentRejectWhenStateReplaced(t *testing.T) {
	// 预留后关闭一个端口构成旧状态的陈旧地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	// 存活的假端点模拟新实例 B。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
		t.Error("新实例存活时 start 应幂等拒绝,不应 spawn")
		return 0, "", nil
	})

	dir := t.TempDir()
	stale := &ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}
	if err := writeServeState(dir, stale); err != nil {
		t.Fatalf("写陈旧状态: %v", err)
	}

	// 条件删除 seam：模拟磁盘状态已被改写为存活的新实例 B（真实写入），
	// 返回 (false, B) 触发重评估分支。
	next := &ServeState{PID: 5555, Addr: liveAddr, StartedAt: time.Now().Format(time.RFC3339)}
	stubRemoveServeStateIfSame(t, func(dataDir string, judged *ServeState) (bool, *ServeState, error) {
		if err := writeServeState(dataDir, next); err != nil {
			return false, nil, err
		}
		return false, next, nil
	})

	cmd, outBuf, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("新实例存活时应幂等拒绝,实际错误: %v", err)
	}
	msg := outBuf.String()
	for _, want := range []string{"already running", "5555", "http://" + liveAddr, "serve stop", "已在后台运行"} {
		if !strings.Contains(msg, want) {
			t.Errorf("输出应含 %q,实际: %q", want, msg)
		}
	}
	// 新实例 B 的状态文件原样保留（拒绝路径不删不写）。
	got, err := readServeState(dir)
	if err != nil || got == nil || got.PID != 5555 || got.Addr != liveAddr {
		t.Errorf("新实例状态应原样保留,实际 (%+v, %v)", got, err)
	}
}
