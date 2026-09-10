package serve

// guard_test.go 直打后台服务主体的单实例守卫
// LifecycleGuard 的四个分支（存活幂等拒绝、损坏清理放行、生命周期锁
// 被占报错、陈旧清理放行），httptest + 临时 DataDir，无需起真实服务；全链路
// 由 cli 包的 serve_hyperlink_e2e_test.go 与 serve_run_e2e_test.go 的 unix E2E 兜底
// （无状态场景 missing → 放行）。

import (
	"bytes"
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

// TestServeDashboard_SingleInstanceGuard_StaleProbeAlive 覆盖单实例守卫的
// 存活幂等拒绝分支：状态文件指向存活的 /api/meta（模拟运行中的实例），
// 守卫必须幂等拒绝且不触碰现状。
func TestServeDashboard_SingleInstanceGuard_StaleProbeAlive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	aliveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := WriteState(dir, &ServeState{PID: 4242, Addr: aliveAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}
	// 记录种子内容，断言守卫不覆盖状态文件（现存实例的定位信息必须保留）。
	seeded, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("读种子状态文件: %v", err)
	}

	var out bytes.Buffer
	lock, proceed, err := LifecycleGuard(dir, &out, StaleProbeTimeout)
	if err != nil {
		t.Fatalf("存活实例下守卫应幂等拒绝而非报错, err=%v", err)
	}
	if proceed {
		t.Fatal("已有存活实例时守卫不应放行")
	}
	if lock != nil {
		t.Error("幂等拒绝路径不应交出生命周期锁")
	}

	msg := out.String()
	for _, want := range []string{"already running", "4242", "http://" + aliveAddr, "serve stop", "已在运行"} {
		if !strings.Contains(msg, want) {
			t.Errorf("输出应含 %q,实际: %q", want, msg)
		}
	}

	got, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("拒绝后状态文件应保留: %v", err)
	}
	if string(got) != string(seeded) {
		t.Errorf("拒绝时状态文件不应被覆盖,种子 %q 实际 %q", seeded, got)
	}

	// serve.lock 未被守卫持有：可再次获取（拒绝路径不碰生命周期锁）。
	l, ok := daemon.AcquireLock(filepath.Join(dir, LifecycleLockFile))
	if !ok {
		t.Fatal("守卫拒绝后 serve.lock 应可获取（守卫不得持有残留）")
	}
	if err := daemon.ReleaseLock(l); err != nil {
		t.Fatalf("释放测试锁: %v", err)
	}
}

// TestServeDashboard_SingleInstanceGuard_CorruptState 覆盖损坏状态分支：
// 无法辨识的 serve.json 是残留，守卫删除后放行并交出生命周期锁。
func TestServeDashboard_SingleInstanceGuard_CorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not-json"), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	var out bytes.Buffer
	lock, proceed, err := LifecycleGuard(dir, &out, StaleProbeTimeout)
	if err != nil {
		t.Fatalf("损坏状态应清理后放行, err=%v", err)
	}
	if !proceed {
		t.Fatal("损坏状态清理后应放行")
	}
	if lock == nil {
		t.Fatal("放行路径应交出生命周期锁供调用方持有")
	}
	defer func() { _ = daemon.ReleaseLock(lock) }()

	// 损坏文件已被删除。
	if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除, stat err = %v", err)
	}
	// 生命周期锁已被守卫持有（放行的组成部分）：第二次获取应失败。
	if l2, ok := daemon.AcquireLock(filepath.Join(dir, LifecycleLockFile)); ok {
		_ = daemon.ReleaseLock(l2)
		t.Error("放行路径应持有 serve.lock,第二次获取不应成功")
	}
}

// TestServeDashboard_SingleInstanceGuard_LockHeld 覆盖生命周期锁竞态分支：
// serve.lock 被占（另一实例正在启动、尚未写出 serve.json）时守卫报双语错误。
func TestServeDashboard_SingleInstanceGuard_LockHeld(t *testing.T) {
	dir := t.TempDir()
	held, ok := daemon.AcquireLock(filepath.Join(dir, LifecycleLockFile))
	if !ok {
		t.Fatal("测试前置:占用 serve.lock 失败")
	}
	defer func() { _ = daemon.ReleaseLock(held) }()

	var out bytes.Buffer
	lock, proceed, err := LifecycleGuard(dir, &out, StaleProbeTimeout)
	if err == nil {
		t.Fatal("生命周期锁被占用时守卫应报错")
	}
	if proceed {
		t.Error("锁被占用时不应放行")
	}
	if lock != nil {
		t.Error("报错路径不应交出锁")
	}
	msg := err.Error()
	for _, want := range []string{"another serve instance is starting", "另一个 serve 实例正在启动"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
}

// TestServeDashboard_SingleInstanceGuard_StaleState 覆盖陈旧状态分支：
// 状态文件指向已无监听的端口（SIGKILL/崩溃遗留），守卫删除后放行。
func TestServeDashboard_SingleInstanceGuard_StaleState(t *testing.T) {
	// 预留后关闭一个端口构成无响应地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	if err := WriteState(dir, &ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写陈旧状态文件: %v", err)
	}

	var out bytes.Buffer
	lock, proceed, err := LifecycleGuard(dir, &out, StaleProbeTimeout)
	if err != nil {
		t.Fatalf("陈旧状态应清理后放行, err=%v", err)
	}
	if !proceed {
		t.Fatal("陈旧状态清理后应放行")
	}
	if lock == nil {
		t.Error("放行路径应交出生命周期锁")
	}
	if err := daemon.ReleaseLock(lock); err != nil {
		t.Fatalf("释放守卫交出的锁: %v", err)
	}
	// 陈旧文件已被删除。
	if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("陈旧状态文件应被删除, stat err = %v", err)
	}
}
