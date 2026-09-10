package serve

// start_test.go 校验后台启动编排：serve-start.lock 串行化、已运行预检幂等
// 避让、显式 BinPath 透传（update 自动恢复的关键合同——替换后的目标二进制与
// 恢复进程可能不同，禁止运行时探测 os.Executable），以及空 BinPath 的既有
// 行为（serve start 命令运行时探测）。

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/daemon"
)

// stubSpawnAndWait 注入 SpawnAndWait seam 并在测试结束恢复，返回捕获的入参。
func stubSpawnAndWait(t *testing.T, fn func(dataDir, binPath, addr, logPath string) (int, string, error)) *struct {
	dataDir, binPath, addr, logPath string
	calls                           int
} {
	t.Helper()
	captured := &struct {
		dataDir, binPath, addr, logPath string
		calls                           int
	}{}
	orig := SpawnAndWait
	SpawnAndWait = func(dataDir, binPath, addr, logPath string) (int, string, error) {
		captured.dataDir, captured.binPath, captured.addr, captured.logPath = dataDir, binPath, addr, logPath
		captured.calls++
		return fn(dataDir, binPath, addr, logPath)
	}
	t.Cleanup(func() { SpawnAndWait = orig })
	return captured
}

// TestStartInBackground_ExplicitBinPathPassedThrough update 的恢复路径显式传入
// 目标二进制：编排必须原样透传，绝不留空（留空会触发运行时探测，恢复进程
// 探测到的是自身而非目标二进制）。
func TestStartInBackground_ExplicitBinPathPassedThrough(t *testing.T) {
	// 存活假端点作为假 spawn 的探活目标。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	captured := stubSpawnAndWait(t, func(dataDir, binPath, addr, logPath string) (int, string, error) {
		// spawn 前状态文件必须已清理（无残留预检干扰）。
		if st, err := ReadState(dataDir); err != nil || st != nil {
			t.Errorf("spawn 时状态文件应不存在,实际 (%v, %v)", st, err)
		}
		return 4242, liveAddr, nil
	})

	got, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8699", BinPath: "/opt/new/token-usage"})
	if err != nil {
		t.Fatalf("StartInBackground err=%v", err)
	}
	if got.AlreadyRunning {
		t.Error("全新启动不应标记 AlreadyRunning")
	}
	if captured.calls != 1 || captured.binPath != "/opt/new/token-usage" {
		t.Errorf("显式 BinPath 必须原样透传, calls=%d binPath=%q", captured.calls, captured.binPath)
	}
	if captured.addr != "127.0.0.1:8699" {
		t.Errorf("请求地址必须透传, got %q", captured.addr)
	}
	if captured.logPath != LogPath(dir) {
		t.Errorf("日志路径应为 DataDir 下的 serve.log, got %q", captured.logPath)
	}
	if got.PID != 4242 || got.Addr != liveAddr {
		t.Errorf("结果应来自 spawn 返回值, got %+v", got)
	}
}

// TestStartInBackground_EmptyBinPathKeepsRuntimeProbe serve start 命令的既有
// 行为：空 BinPath 保留给编排内部的运行时探测（不在此处拒绝）。
func TestStartInBackground_EmptyBinPathKeepsRuntimeProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	captured := stubSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		return 7, strings.TrimPrefix(ts.URL, "http://"), nil
	})

	if _, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8698"}); err != nil {
		t.Fatalf("StartInBackground err=%v", err)
	}
	if captured.calls != 1 || captured.binPath != "" {
		t.Errorf("空 BinPath 应保持空由 spawn 层探测, calls=%d binPath=%q", captured.calls, captured.binPath)
	}
}

// TestStartInBackground_AlreadyRunningIdempotent 已有存活实例：幂等避让——
// 不 spawn，返回现存实例并标记 AlreadyRunning（update 恢复路径据此不重复拉起）。
func TestStartInBackground_AlreadyRunningIdempotent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := WriteState(dir, &ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}
	captured := stubSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("已运行时不应 spawn")
		return 0, "", nil
	})

	got, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8697"})
	if err != nil {
		t.Fatalf("StartInBackground err=%v", err)
	}
	if !got.AlreadyRunning || got.PID != 4242 || got.Addr != addr {
		t.Errorf("应幂等避让并返回现存实例, got %+v", got)
	}
	if captured.calls != 0 {
		t.Errorf("已运行时不得 spawn, calls=%d", captured.calls)
	}
}

// TestStartInBackground_MissingAddr 空地址拒绝（命令层与恢复路径共用同一校验）。
func TestStartInBackground_MissingAddr(t *testing.T) {
	stubSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("空 --addr 不应 spawn")
		return 0, "", nil
	})
	_, err := StartInBackground(StartOptions{DataDir: t.TempDir(), Addr: "  "})
	if err == nil {
		t.Fatal("空地址应报错")
	}
	for _, want := range []string{"missing required --addr", "token-usage serve start --addr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, err.Error())
		}
	}
}

// TestStartInBackground_FailureIncludesLogTail spawn 失败时错误附带日志尾。
func TestStartInBackground_FailureIncludesLogTail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(LogPath(dir), []byte("line-1\n监听失败详情\n"), 0644); err != nil {
		t.Fatalf("写日志: %v", err)
	}
	stubSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		return 0, "", errors.New("not ready")
	})
	_, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8696"})
	if err == nil {
		t.Fatal("spawn 失败应报错")
	}
	msg := err.Error()
	for _, want := range []string{"failed to start dashboard in background", "not ready", "log tail:", "监听失败详情"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
}

// TestStartInBackground_StartLockSerializes 跨进程启动互斥：serve-start.lock
// 被占时报双语错误且不 spawn。
func TestStartInBackground_StartLockSerializes(t *testing.T) {
	dir := t.TempDir()
	lock, ok := daemon.AcquireLock(filepath.Join(dir, StartLockFile))
	if !ok {
		t.Fatal("测试前置:占用 serve-start.lock 失败")
	}
	defer func() { _ = daemon.ReleaseLock(lock) }()

	captured := stubSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("启动锁被占用时不应 spawn")
		return 0, "", nil
	})
	_, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8695"})
	if err == nil {
		t.Fatal("启动锁被占用时应报错")
	}
	if !strings.Contains(err.Error(), "another serve start is in progress") {
		t.Errorf("错误信息应指向启动互斥,实际: %q", err.Error())
	}
	if captured.calls != 0 {
		t.Error("锁占用时不应 spawn")
	}
}

// TestStartInBackground_StaleStateRemovedBeforeSpawn 陈旧状态放行：spawn 前
// 条件删除（预检段覆盖），不阻塞全新启动。
func TestStartInBackground_StaleStateRemovedBeforeSpawn(t *testing.T) {
	// 预留后关闭一个端口构成陈旧记录。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := WriteState(dir, &ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写陈旧状态: %v", err)
	}
	stubSpawnAndWait(t, func(dataDir, binPath, addr, logPath string) (int, string, error) {
		if st, rerr := ReadState(dataDir); rerr != nil || st != nil {
			t.Errorf("spawn 时陈旧状态应已被删除,实际 (%v, %v)", st, rerr)
		}
		return 4242, liveAddr, nil
	})

	got, err := StartInBackground(StartOptions{DataDir: dir, Addr: "127.0.0.1:8694"})
	if err != nil {
		t.Fatalf("陈旧放行后启动应成功, err=%v", err)
	}
	if got.AlreadyRunning || got.Addr != liveAddr {
		t.Errorf("应完成全新启动, got %+v", got)
	}
}
