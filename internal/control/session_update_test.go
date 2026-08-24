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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
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
		stopErr = s.Stop(context.Background(), newConfigWith(t.TempDir()))
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
		stopErr = s.Stop(context.Background(), newConfigWith(t.TempDir()))
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

	err := s.Stop(context.Background(), newConfigWith(t.TempDir()))
	if !errors.Is(err, errSessionReleased) {
		t.Fatalf("已释放会话应返回 errSessionReleased，实际: %v", err)
	}
}

// TestSession_Stop_NilSession nil Session → 返回错误（不 panic）。
func TestSession_Stop_NilSession(t *testing.T) {
	var s *Session
	err := s.Stop(context.Background(), newConfigWith(t.TempDir()))
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
		stopErr = s.Stop(context.Background(), newConfigWith(t.TempDir()))
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
			return s.Stop(context.Background(), newConfigWith(t.TempDir()))
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
		startErr = s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), explicitBin)
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
		startErr = s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), "/opt/token-usage/bin/token-usage")
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
		startErr = s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), "")
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
		startErr = s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), "relative/token-usage")
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

	err := s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), "/opt/token-usage/bin/token-usage")
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
			return s.StartWithExecutable(context.Background(), newConfigWith(t.TempDir()), "/opt/token-usage/bin/token-usage")
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
		startErr = s.StartWithExecutable(ctx, newConfigWith(t.TempDir()), "/opt/token-usage/bin/token-usage")
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

// 自定义且尚不存在的 log.dir：start 流程在 spawn 前 MkdirAll 兜底日志目录，
// start 成功且目录被创建（子进程内 logger.Init 的 MkdirAll 在时序上来不及）。
func TestStart_CreatesFallbackLogDirWhenMissing(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 7071, 0, 1, 0, "pending")
	m := &Manager{home: t.TempDir(), deps: f.asManagerDeps()}
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "custom", "nested-logs")
	cfg := &config.Config{DataDir: dataDir, Log: config.LogConfig{Dir: logDir}}
	loader := &tracedConfigLoader{trace: f.trace, cfg: cfg}

	if _, err := m.Start(context.Background(), loader.load); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	if info, err := os.Stat(logDir); err != nil || !info.IsDir() {
		t.Errorf("start 后兜底日志目录应被创建: %v", err)
	}
}

// Log.Dir 为空时按 DataDir/logs 推导（与 runtimecfg 默认一致）。
func TestFallbackLogFilePath_EmptyLogDirFallsBackToDataDirLogs(t *testing.T) {
	cfg := newConfigWith("/base")
	want := filepath.Join("/base", "logs", "daemon-fallback.log")
	if got := fallbackLogFilePath(cfg); got != want {
		t.Errorf("fallbackLogFilePath = %q, want %q", got, want)
	}
}

// TestBuildSpawnOptionsForBin_UsesExplicitPath 构造层直接使用显式 binPath，不做 os.Executable 探测。
func TestBuildSpawnOptionsForBin_UsesExplicitPath(t *testing.T) {
	const bin = "/custom/path/token-usage"
	cfg := newConfigWith(t.TempDir())
	opts, err := buildSpawnOptionsForBin(cfg, bin)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if opts.BinPath != bin {
		t.Errorf("BinPath=%q want %q", opts.BinPath, bin)
	}
	if len(opts.Args) != 1 || opts.Args[0] != "_run" {
		t.Errorf("Args=%v want [_run]", opts.Args)
	}
	// 兜底输出指向 logs/ 下固定 fallback 文件（与结构化日志同目录，
	// 不带日期避免跨天歧义）；Log.Dir 为空时按 DataDir/logs 推导。
	want := filepath.Join(cfg.DataDir, "logs", "daemon-fallback.log")
	if opts.StdoutPath != want {
		t.Errorf("StdoutPath=%q want %q", opts.StdoutPath, want)
	}
	if opts.StderrPath != want {
		t.Errorf("StderrPath=%q want %q", opts.StderrPath, want)
	}
}

// TestBuildSpawnOptionsForBin_EmptyPath 构造层拒绝空 binPath。
func TestBuildSpawnOptionsForBin_EmptyPath(t *testing.T) {
	_, err := buildSpawnOptionsForBin(newConfigWith(t.TempDir()), "")
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
	opts, err := buildSpawnOptions(newConfigWith(t.TempDir()))
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

// fallbackLogDir 展开 ~（用户层 cfg 未展开时不得被当作相对路径在 CWD 误建目录），
// 空 log.dir 回退展开后的 data_dir/logs——与 service.EffectiveLogDir 同一推导。
func TestFallbackLogDir_ExpandsTilde(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cfg := &config.Config{DataDir: "~/.token-usage", Log: config.LogConfig{Dir: "~/custom-logs"}}
	if got := fallbackLogDir(cfg); got != "/home/tester/custom-logs" {
		t.Errorf("fallbackLogDir = %q, want /home/tester/custom-logs", got)
	}
	emptyLog := &config.Config{DataDir: "~/.token-usage"}
	want := filepath.Join("/home/tester/.token-usage", "logs")
	if got := fallbackLogDir(emptyLog); got != want {
		t.Errorf("空 log.dir 回退 = %q, want %q", got, want)
	}
}

// fallback 容量治理：spawn 前按大小轮转（活跃写入的 mtime 持续更新，仅靠
// logger.cleanup 的 mtime 清理无法约束增长）。超限 → rename 为 .old（覆盖旧
// .old）；未超限 → 原样保留。
func TestEnsureFallbackLogFile_RotatesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: t.TempDir(), Log: config.LogConfig{Dir: dir}}
	fb := filepath.Join(dir, daemonFallbackLogName)
	big := bytes.Repeat([]byte("x"), fallbackRotateThreshold+1)
	if err := os.WriteFile(fb, big, 0o644); err != nil {
		t.Fatalf("write big fallback: %v", err)
	}
	// 预置旧 .old，轮转应覆盖它。
	if err := os.WriteFile(fb+".old", []byte("prev-old"), 0o644); err != nil {
		t.Fatalf("write prev old: %v", err)
	}
	if err := ensureFallbackLogFile(cfg); err != nil {
		t.Fatalf("ensureFallbackLogFile: %v", err)
	}
	oldContent, err := os.ReadFile(fb + ".old")
	if err != nil {
		t.Fatalf("轮转后应存在 .old: %v", err)
	}
	if len(oldContent) != len(big) {
		t.Errorf(".old 应为原超限文件内容，len=%d want %d", len(oldContent), len(big))
	}
	if _, err := os.Stat(fb); !os.IsNotExist(err) {
		t.Errorf("超限文件应已被 rename 走（新文件由子进程 spawn 时创建），stat err=%v", err)
	}
}

func TestEnsureFallbackLogFile_KeepsSmallFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: t.TempDir(), Log: config.LogConfig{Dir: dir}}
	fb := filepath.Join(dir, daemonFallbackLogName)
	if err := os.WriteFile(fb, []byte("small"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ensureFallbackLogFile(cfg); err != nil {
		t.Fatalf("ensureFallbackLogFile: %v", err)
	}
	got, err := os.ReadFile(fb)
	if err != nil || string(got) != "small" {
		t.Errorf("未超限文件应原样保留，got=%q err=%v", got, err)
	}
	if _, err := os.Stat(fb + ".old"); !os.IsNotExist(err) {
		t.Errorf("未超限不应产生 .old")
	}
}

// 轮转失败路径：rename 受阻（.old 为目录）时返回错误，且原超限文件与旧档
// 都保持原状——不出现预删旧档后轮转失败的丢失窗口。
func TestEnsureFallbackLogFile_RenameFailureKeepsBothFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: t.TempDir(), Log: config.LogConfig{Dir: dir}}
	fb := filepath.Join(dir, daemonFallbackLogName)
	big := bytes.Repeat([]byte("x"), fallbackRotateThreshold+1)
	if err := os.WriteFile(fb, big, 0o644); err != nil {
		t.Fatalf("write big fallback: %v", err)
	}
	// .old 做成目录使 rename(file → dir) 失败。
	if err := os.Mkdir(fb+".old", 0o755); err != nil {
		t.Fatalf("mkdir old-as-dir: %v", err)
	}
	if err := ensureFallbackLogFile(cfg); err == nil {
		t.Fatal("rename 受阻时应返回错误，不得静默成功")
	}
	got, rerr := os.ReadFile(fb)
	if rerr != nil || len(got) != len(big) {
		t.Errorf("失败路径原超限文件应保持原状，len=%d err=%v", len(got), rerr)
	}
	if info, serr := os.Stat(fb + ".old"); serr != nil || !info.IsDir() {
		t.Errorf("失败路径旧档应保持原状（仍为目录），err=%v", serr)
	}
}
