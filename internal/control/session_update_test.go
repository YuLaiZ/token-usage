// internal/control/session_update_test.go
// 校验锁内公开 API：Session.Stop 与 Session.StartWithExecutable。
//
// 设计要点：
//   - 两者都在 WithLock 回调内调用（已持 control lock），复用 stopLocked/startLockedCore，
//     不再二次获取 control lock（否则死锁）；
//   - StartWithExecutable 使用显式 binPath（区别于 startLocked 自动探测 os.Executable），
//     供更新替换后显式启动新二进制；
//   - 校验拒绝：已释放会话、nil 配置、空路径、相对路径。
//
// 全部基于现有 fakeDeps 装配，无真实进程/文件 IO，确定性驱动。
package control

import (
	"context"
	"errors"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"
)

// ---- Session.Stop ----

// TestSession_Stop_DelegatesToStopLocked 运行中 → Stop 复用 stopLocked：对准确 PID 发 SIGTERM + 等 lock 释放。
func TestSession_Stop_DelegatesToStopLocked(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX SIGTERM 路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 5555
	f.service.statusResult = false // 未托管 → 直接 SIGTERM
	// isRunning: check#1 (inspect)=true → SIGTERM；check#2 (waitDaemonRelease 第1轮)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)

	var stopErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		stopErr = s.Stop(context.Background(), newConfigWith("/data"))
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if stopErr != nil {
		t.Fatalf("Stop err=%v", stopErr)
	}
	if len(f.kill.sigterm) != 1 || f.kill.sigterm[0] != 5555 {
		t.Errorf("应对准确 PID 5555 SIGTERM，sigterm=%v", f.kill.sigterm)
	}
}

// TestSession_Stop_NotRunningIdempotent 未运行 → Stop 幂等成功，不触发任何停止路径。
func TestSession_Stop_NotRunningIdempotent(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = false
	m := newTestProcessManager(t, t.TempDir(), f)

	var stopErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		stopErr = s.Stop(context.Background(), newConfigWith("/data"))
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if stopErr != nil {
		t.Fatalf("未运行应幂等成功，Stop err=%v", stopErr)
	}
	if f.service.stopCalls != 0 || len(f.kill.sigterm) != 0 {
		t.Errorf("未运行不应触发停止路径，stopCalls=%d sigterm=%v", f.service.stopCalls, f.kill.sigterm)
	}
}

// TestSession_Stop_NilCfg nil cfg → 返回错误，不调 stopLocked。
func TestSession_Stop_NilCfg(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	var stopErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		stopErr = s.Stop(context.Background(), nil)
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if stopErr == nil {
		t.Fatal("nil cfg 应返回错误")
	}
	if f.service.stopCalls != 0 {
		t.Errorf("nil cfg 不应触发 stop，stopCalls=%d", f.service.stopCalls)
	}
}

// TestSession_Stop_ReleasedSession 已释放会话 → 返回 errSessionReleased。
func TestSession_Stop_ReleasedSession(t *testing.T) {
	m := &Manager{home: t.TempDir()}
	s := &Session{manager: m, released: true}

	err := s.Stop(context.Background(), newConfigWith("/data"))
	if !errors.Is(err, errSessionReleased) {
		t.Fatalf("已释放会话应返回 errSessionReleased，实际: %v", err)
	}
}

// TestSession_Stop_NilSession nil Session → 返回错误（不 panic）。
func TestSession_Stop_NilSession(t *testing.T) {
	var s *Session
	err := s.Stop(context.Background(), newConfigWith("/data"))
	if err == nil {
		t.Fatal("nil Session 应返回错误")
	}
}

// TestSession_Stop_PropagatesStopError stopLocked 失败（SIGTERM 失败）→ Stop 透传错误。
func TestSession_Stop_PropagatesStopError(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX SIGTERM 路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 5555
	f.service.statusResult = false
	f.kill.err = errors.New("sigterm boom")
	m := newTestProcessManager(t, t.TempDir(), f)

	var stopErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		stopErr = s.Stop(context.Background(), newConfigWith("/data"))
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if stopErr == nil || !containsStr(stopErr.Error(), "sigterm boom") {
		t.Fatalf("应透传 stop 错误，实际: %v", stopErr)
	}
}

// TestSession_Stop_DoesNotReacquireLock Stop 在 WithLock 内不再二次获取 control lock。
// 用 mutexFakeLocker（真正互斥）：若 Stop 二次加锁会死锁；成功返回证明只持一次锁。
func TestSession_Stop_DoesNotReacquireLock(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = false // 未运行，避免触发平台停止路径
	m := &Manager{home: t.TempDir(), deps: f.asManagerDeps()}
	shared := &mutexFakeLocker{}
	m.deps.newLocker = func() controlLocker { return shared }

	done := make(chan error, 1)
	go func() {
		done <- m.WithLock(context.Background(), func(s *Session) error {
			return s.Stop(context.Background(), newConfigWith("/data"))
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop 在 WithLock 内不应死锁或出错，err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 疑似二次获取 control lock 导致死锁")
	}
}

// ---- Session.StartWithExecutable ----

// TestSession_StartWithExecutable_UsesExplicitBinPath 未运行 → 用显式 binPath spawn 新 daemon。
func TestSession_StartWithExecutable_UsesExplicitBinPath(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 4242, 0, 1, 0, "pending")
	m := newTestProcessManager(t, t.TempDir(), f)
	const explicitBin = "/opt/token-usage/bin/token-usage"

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(context.Background(), newConfigWith("/data"), explicitBin)
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if startErr != nil {
		t.Fatalf("StartWithExecutable err=%v", startErr)
	}
	if len(f.spawn.calls) != 1 {
		t.Fatalf("应 spawn 一次，实际 %d", len(f.spawn.calls))
	}
	if f.spawn.calls[0].BinPath != explicitBin {
		t.Errorf("spawn BinPath=%q want %q（必须用显式路径，不探测 os.Executable）",
			f.spawn.calls[0].BinPath, explicitBin)
	}
}

// TestSession_StartWithExecutable_AlreadyRunning 已运行 → 幂等返回，不 spawn。
func TestSession_StartWithExecutable_AlreadyRunning(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 7777
	m := newTestProcessManager(t, t.TempDir(), f)

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(context.Background(), newConfigWith("/data"), "/opt/token-usage/bin/token-usage")
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if startErr != nil {
		t.Fatalf("已运行应幂等成功，err=%v", startErr)
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("已运行不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestSession_StartWithExecutable_EmptyPath 空路径 → 返回错误，不 spawn。
func TestSession_StartWithExecutable_EmptyPath(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(context.Background(), newConfigWith("/data"), "")
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if startErr == nil {
		t.Fatal("空 binPath 应返回错误")
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("空 binPath 不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestSession_StartWithExecutable_RelativePath 相对路径 → 返回错误，不 spawn。
func TestSession_StartWithExecutable_RelativePath(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(context.Background(), newConfigWith("/data"), "relative/token-usage")
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if startErr == nil {
		t.Fatal("相对 binPath 应返回错误")
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("相对 binPath 不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestSession_StartWithExecutable_NilCfg nil cfg → 返回错误。
func TestSession_StartWithExecutable_NilCfg(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(context.Background(), nil, "/opt/token-usage/bin/token-usage")
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if startErr == nil {
		t.Fatal("nil cfg 应返回错误")
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("nil cfg 不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestSession_StartWithExecutable_ReleasedSession 已释放会话 → 返回 errSessionReleased。
func TestSession_StartWithExecutable_ReleasedSession(t *testing.T) {
	m := &Manager{home: t.TempDir()}
	s := &Session{manager: m, released: true}

	err := s.StartWithExecutable(context.Background(), newConfigWith("/data"), "/opt/token-usage/bin/token-usage")
	if !errors.Is(err, errSessionReleased) {
		t.Fatalf("已释放会话应返回 errSessionReleased，实际: %v", err)
	}
}

// TestSession_StartWithExecutable_DoesNotReacquireLock 不二次获取 control lock（用 mutexFakeLocker 防死锁）。
func TestSession_StartWithExecutable_DoesNotReacquireLock(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 6464, 0, 1, 0, "pending")
	m := &Manager{home: t.TempDir(), deps: f.asManagerDeps()}
	shared := &mutexFakeLocker{}
	m.deps.newLocker = func() controlLocker { return shared }

	done := make(chan error, 1)
	go func() {
		done <- m.WithLock(context.Background(), func(s *Session) error {
			return s.StartWithExecutable(context.Background(), newConfigWith("/data"), "/opt/token-usage/bin/token-usage")
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartWithExecutable 在 WithLock 内不应死锁或出错，err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartWithExecutable 疑似二次获取 control lock 导致死锁")
	}
}

// TestSession_StartWithExecutable_CancelledContext context 已取消 → 立即返回 ctx.Err()。
func TestSession_StartWithExecutable_CancelledContext(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var startErr error
	err := m.WithLock(context.Background(), func(s *Session) error {
		startErr = s.StartWithExecutable(ctx, newConfigWith("/data"), "/opt/token-usage/bin/token-usage")
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock err=%v", err)
	}
	if !errors.Is(startErr, context.Canceled) {
		t.Fatalf("已取消 context 应返回 Canceled，实际: %v", startErr)
	}
}

// ---- buildSpawnOptions 分层 ----

// TestBuildSpawnOptionsForBin_UsesExplicitPath 构造层直接使用显式 binPath，不做 os.Executable 探测。
func TestBuildSpawnOptionsForBin_UsesExplicitPath(t *testing.T) {
	const bin = "/custom/path/token-usage"
	opts, err := buildSpawnOptionsForBin(newConfigWith("/data"), bin)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if opts.BinPath != bin {
		t.Errorf("BinPath=%q want %q", opts.BinPath, bin)
	}
	if len(opts.Args) != 1 || opts.Args[0] != "_run" {
		t.Errorf("Args=%v want [_run]", opts.Args)
	}
	if opts.StdoutPath != filepath.Join("/data", "daemon.out.log") {
		t.Errorf("StdoutPath=%q", opts.StdoutPath)
	}
	if opts.StderrPath != filepath.Join("/data", "daemon.err.log") {
		t.Errorf("StderrPath=%q", opts.StderrPath)
	}
}

// TestBuildSpawnOptionsForBin_EmptyPath 构造层拒绝空 binPath。
func TestBuildSpawnOptionsForBin_EmptyPath(t *testing.T) {
	_, err := buildSpawnOptionsForBin(newConfigWith("/data"), "")
	if err == nil {
		t.Fatal("空 binPath 应返回错误")
	}
}

// TestBuildSpawnOptionsForBin_NilCfg 构造层拒绝 nil cfg。
func TestBuildSpawnOptionsForBin_NilCfg(t *testing.T) {
	_, err := buildSpawnOptionsForBin(nil, "/x")
	if err == nil {
		t.Fatal("nil cfg 应返回错误")
	}
}

// TestBuildSpawnOptions_WrapperResolvesExecutable 包装层经 os.Executable 解析（保持原行为）。
func TestBuildSpawnOptions_WrapperResolvesExecutable(t *testing.T) {
	opts, err := buildSpawnOptions(newConfigWith("/data"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if opts.BinPath == "" {
		t.Error("包装层应解析出非空 BinPath（os.Executable）")
	}
	// BinPath 应等于 go test 进程自身路径（os.Executable 解析结果）。
	// 不强断言具体值（跨平台/环境差异），仅校验非空且为绝对路径。
	if !filepath.IsAbs(opts.BinPath) {
		t.Errorf("包装层 BinPath 应为绝对路径，实际 %q", opts.BinPath)
	}
	if len(opts.Args) != 1 || opts.Args[0] != "_run" {
		t.Errorf("Args=%v want [_run]", opts.Args)
	}
}

// containsStr 报告 s 是否包含 substr（测试辅助，避免引入 strings 仅一处）。
func containsStr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
