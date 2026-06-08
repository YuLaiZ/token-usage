// internal/control/lock_test.go
package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLocker 是 controlLocker 的测试替身：tryLock 按 script 顺序返回结果（最后一个重复），
// 记录 try/unlock 次数。不触碰文件系统，保证测试确定性。
type fakeLocker struct {
	mu          sync.Mutex
	script      []bool
	idx         int
	tryCount    int
	unlockCount int
	unlockErr   error
}

func newFakeLocker(script ...bool) *fakeLocker {
	if len(script) == 0 {
		script = []bool{true}
	}
	return &fakeLocker{script: script}
}

func (f *fakeLocker) tryLock() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tryCount++
	r := f.script[f.idx]
	if f.idx < len(f.script)-1 {
		f.idx++
	}
	return r, nil
}

func (f *fakeLocker) unlock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlockCount++
	return f.unlockErr
}

// fakeClock 提供 injectable 的 now/sleep：sleep 仅推进虚拟时间，绝不调用 time.Sleep，
// 从而让 WithLock 的有界等待循环在测试中瞬间完成（满足“禁止真实 sleep”约束）。
type fakeClock struct {
	mu        sync.Mutex
	current   time.Time
	sleepCall int
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{current: start}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
	c.sleepCall++
}

// newTestManager 用 fake locker/clock 装配 Manager，绕开 NewManager 的文件系统行为。
func newTestManager(t *testing.T, home string, fl *fakeLocker, clk *fakeClock) *Manager {
	t.Helper()
	return &Manager{
		home: home,
		deps: managerDependencies{
			now:       clk.now,
			sleep:     clk.sleep,
			newLocker: func() controlLocker { return fl },
		},
	}
}

// ---- ControlLockPath ----

func TestControlLockPath_OnlyDependsOnHome(t *testing.T) {
	home := "/home/alice"
	got := ControlLockPath(home)
	want := filepath.Join(home, ".token-usage", "token-usage.control.lock")
	if got != want {
		t.Fatalf("ControlLockPath = %q, want %q", got, want)
	}

	// 不同 home 产出不同路径，且差异仅在前缀；data_dir 概念不存在于此函数。
	other := ControlLockPath("/home/bob")
	if other == got {
		t.Fatal("different homes should yield different lock paths")
	}
	if !strings.HasPrefix(other, "/home/bob/") {
		t.Fatalf("path should be under the given home: %q", other)
	}
}

func TestControlLockPath_FixedSuffix(t *testing.T) {
	cases := []string{"/root", "/Users/x", "C:\\Users\\x"}
	for _, home := range cases {
		p := ControlLockPath(home)
		if !strings.HasSuffix(p, filepath.Join(".token-usage", "token-usage.control.lock")) {
			t.Errorf("path %q must end with .token-usage/token-usage.control.lock", p)
		}
	}
}

// ---- NewManager 校验 ----

func TestNewManager_RejectsEmptyHome(t *testing.T) {
	_, err := NewManager("")
	if err == nil {
		t.Fatal("NewManager(\"\") should error")
	}
}

func TestNewManager_RejectsRelativeHome(t *testing.T) {
	_, err := NewManager("relative/path")
	if err == nil {
		t.Fatal("NewManager(relative) should error")
	}
	if !errors.Is(err, errNonAbsoluteHome) {
		t.Errorf("error should wrap errNonAbsoluteHome; got %v", err)
	}
}

func TestNewManager_CreatesConfigDir(t *testing.T) {
	home := t.TempDir()
	m, err := NewManager(home)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	dir := filepath.Join(home, ".token-usage")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("config dir %q should exist after NewManager: %v", dir, err)
	}
	if m.ConfigHome() != dir {
		t.Fatalf("ConfigHome = %q, want %q", m.ConfigHome(), dir)
	}
}

func TestNewManager_DirCreationFails(t *testing.T) {
	// home 指向一个普通文件：在其下 MkdirAll(.token-usage) 必然失败。
	parent := t.TempDir()
	fileHome := filepath.Join(parent, "notadir")
	if err := os.WriteFile(fileHome, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	_, err := NewManager(fileHome)
	if err == nil {
		t.Fatal("NewManager should fail when config dir cannot be created")
	}
}

// ---- WithLock 成功 ----

func TestManager_WithLock_Success(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	m := newTestManager(t, t.TempDir(), fl, clk)

	called := false
	err := m.WithLock(context.Background(), func(s *Session) error {
		called = true
		if s == nil || s.manager != m {
			t.Error("session should reference the manager")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock returned error: %v", err)
	}
	if !called {
		t.Error("fn should have been invoked")
	}
	if fl.tryCount != 1 {
		t.Errorf("tryLock calls = %d, want 1", fl.tryCount)
	}
	if fl.unlockCount != 1 {
		t.Errorf("unlock calls = %d, want 1 (release exactly once)", fl.unlockCount)
	}
	// 成功路径不应推进虚拟时钟。
	if clk.sleepCall != 0 {
		t.Errorf("sleep calls = %d, want 0 on immediate acquire", clk.sleepCall)
	}
}

// ---- WithLock 竞争后获取 ----

func TestManager_WithLock_ContentionThenAcquire(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	// 前两次抢锁失败（被别人持有），第三次成功。
	fl := newFakeLocker(false, false, true)
	m := newTestManager(t, t.TempDir(), fl, clk)

	called := false
	err := m.WithLock(context.Background(), func(*Session) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock returned error: %v", err)
	}
	if !called {
		t.Error("fn should have been invoked after contention")
	}
	if fl.tryCount != 3 {
		t.Errorf("tryLock calls = %d, want 3", fl.tryCount)
	}
	if fl.unlockCount != 1 {
		t.Errorf("unlock calls = %d, want 1", fl.unlockCount)
	}
	// 两次失败的尝试各 sleep 一次。
	if clk.sleepCall != 2 {
		t.Errorf("sleep calls = %d, want 2", clk.sleepCall)
	}
}

// ---- WithLock 超时 ----

func TestManager_WithLock_Timeout(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clk := newFakeClock(start)
	// 始终抢锁失败。
	fl := newFakeLocker(false)
	m := newTestManager(t, t.TempDir(), fl, clk)

	called := false
	err := m.WithLock(context.Background(), func(*Session) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrControlLockTimeout) {
		t.Fatalf("WithLock error = %v, want ErrControlLockTimeout", err)
	}
	if called {
		t.Error("fn must not be invoked on timeout")
	}
	if fl.unlockCount != 0 {
		t.Errorf("unlock calls = %d, want 0 (never acquired)", fl.unlockCount)
	}
	// 虚拟时钟应已推进到至少 deadline。
	if got := clk.now(); got.Before(start.Add(controlLockTimeout)) {
		t.Errorf("clock = %v, want >= %v", got, start.Add(controlLockTimeout))
	}
	// 100ms 间隔 × 150 = 15s。
	if clk.sleepCall != int(controlLockTimeout/controlPollInterval) {
		t.Errorf("sleep calls = %d, want %d", clk.sleepCall, controlLockTimeout/controlPollInterval)
	}
}

// ---- WithLock context 取消 ----

func TestManager_WithLock_ContextCancelledBeforehand(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false)
	m := newTestManager(t, t.TempDir(), fl, clk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 提前取消，确定性触发 ctx.Err() 分支

	called := false
	err := m.WithLock(ctx, func(*Session) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithLock error = %v, want context.Canceled", err)
	}
	if called {
		t.Error("fn must not be invoked when context cancelled")
	}
	if fl.tryCount != 0 {
		t.Errorf("tryLock calls = %d, want 0 (ctx checked first)", fl.tryCount)
	}
}

func TestManager_WithLock_ContextCancelledHasPriorityOverTimeout(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false)
	m := newTestManager(t, t.TempDir(), fl, clk)

	// 已超过 deadline 且 context 已取消：应优先返回 ctx.Err()。
	clk.current = clk.current.Add(controlLockTimeout + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.WithLock(ctx, func(*Session) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, context error must beat timeout", err)
	}
}

// TestManager_WithLock_ContextDeadlineMapsToControlLockTimeout 回归 Critical 1：
// 调用方传入带 deadline 的 context（如 _run 用 2s 获取 control lock），当 context
// 的 deadline 先于自身 controlLockTimeout 到期时，必须返回 ErrControlLockTimeout
// 而非 context.DeadlineExceeded——否则调用方的 errors.Is(err, ErrControlLockTimeout)
// 降级/防护分支永不触发。主动取消（Canceled）仍必须返回 ctx.Err()。
func TestManager_WithLock_ContextDeadlineMapsToControlLockTimeout(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false) // 始终抢锁失败，模拟 start 持有 control lock
	m := newTestManager(t, t.TempDir(), fl, clk)

	// 构造一个 deadline 已真实到期（DeadlineExceeded，非 Canceled）的 context。
	// fakeClock 只驱动 Manager 内部时间，不驱动 context 内部计时，故用一个真实过期 deadline。
	past, pastCancel := context.WithDeadline(context.Background(), time.Unix(1_699_999_999, 0))
	defer pastCancel()
	<-past.Done()
	if err := past.Err(); err != context.DeadlineExceeded {
		t.Fatalf("setup: past context should be DeadlineExceeded, got %v", err)
	}

	err := m.WithLock(past, func(*Session) error { return nil })
	if !errors.Is(err, ErrControlLockTimeout) {
		t.Fatalf("WithLock error = %v, want ErrControlLockTimeout (context deadline must map to control timeout)", err)
	}
	// 底层 ctx 错误仍可通过 errors.Is(err, context.DeadlineExceeded) 取回（包装保留原因）。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithLock error = %v, should wrap underlying context.DeadlineExceeded", err)
	}
}

// TestManager_AcquireLock_ContextDeadlineMapsToControlLockTimeout 同样的回归，
// 针对 AcquireLock（_run 实际调用的 API）。
func TestManager_AcquireLock_ContextDeadlineMapsToControlLockTimeout(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false) // 始终抢锁失败
	m := newTestManager(t, t.TempDir(), fl, clk)

	past, cancel := context.WithDeadline(context.Background(), time.Unix(1_699_999_999, 0))
	defer cancel()
	<-past.Done()

	_, err := m.AcquireLock(past)
	if !errors.Is(err, ErrControlLockTimeout) {
		t.Fatalf("AcquireLock error = %v, want ErrControlLockTimeout (context deadline must map to control timeout)", err)
	}
}

// TestManager_AcquireLock_SelfDeadlineIsErrControlLockTimeout 自身 controlLockTimeout
// 到期（传入 Background 无 deadline）返回 ErrControlLockTimeout。
func TestManager_AcquireLock_SelfDeadlineIsErrControlLockTimeout(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false)
	m := newTestManager(t, t.TempDir(), fl, clk)

	_, err := m.AcquireLock(context.Background())
	if !errors.Is(err, ErrControlLockTimeout) {
		t.Fatalf("AcquireLock error = %v, want ErrControlLockTimeout", err)
	}
}

// TestManager_AcquireLock_CanceledReturnsCtxErr context 主动取消返回 ctx.Err()，
// 不被映射成 ErrControlLockTimeout（保证调用方可区分取消与超时）。
func TestManager_AcquireLock_CanceledReturnsCtxErr(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(false)
	m := newTestManager(t, t.TempDir(), fl, clk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.AcquireLock(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireLock error = %v, want context.Canceled (active cancel must not map to control timeout)", err)
	}
	if errors.Is(err, ErrControlLockTimeout) {
		t.Errorf("AcquireLock error = %v, active cancel must NOT be ErrControlLockTimeout", err)
	}
}

// ---- WithLock fn 错误透传 + 仍释放锁 ----

func TestManager_WithLock_PropagatesFnErrorAndReleases(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	m := newTestManager(t, t.TempDir(), fl, clk)

	sentinel := errors.New("boom")
	err := m.WithLock(context.Background(), func(*Session) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock error = %v, want sentinel", err)
	}
	if fl.unlockCount != 1 {
		t.Errorf("unlock calls = %d, want 1 (release even on fn error)", fl.unlockCount)
	}
}

func TestManager_WithLock_JoinsUnlockError(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	unlockErr := errors.New("unlock failed")
	fl.unlockErr = unlockErr
	m := newTestManager(t, t.TempDir(), fl, clk)

	fnErr := errors.New("operation failed")
	err := m.WithLock(context.Background(), func(*Session) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("WithLock error = %v, want operation error", err)
	}
	if !errors.Is(err, unlockErr) {
		t.Fatalf("WithLock error = %v, want unlock error", err)
	}
	if fl.unlockCount != 1 {
		t.Fatalf("unlock calls = %d, want 1", fl.unlockCount)
	}
}

func TestSession_Close_ReturnsUnlockError(t *testing.T) {
	fl := newFakeLocker(true)
	unlockErr := errors.New("unlock failed")
	fl.unlockErr = unlockErr
	sess := &Session{locker: fl}

	if err := sess.Close(); !errors.Is(err, unlockErr) {
		t.Fatalf("Close error = %v, want unlock error", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close should be idempotent, got %v", err)
	}
}

// ---- 重复 release 幂等 ----

func TestSession_Release_Idempotent(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	m := newTestManager(t, t.TempDir(), fl, clk)

	sess := &Session{manager: m, locker: fl}
	sess.release()
	sess.release()
	sess.release()
	if fl.unlockCount != 1 {
		t.Errorf("unlock calls = %d, want 1 (release must be idempotent)", fl.unlockCount)
	}
}

// ---- 连续两次 WithLock 都成功（证明前次已释放）----

func TestManager_WithLock_ReleasesForReacquire(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	m := newTestManager(t, t.TempDir(), fl, clk)

	for i := 0; i < 3; i++ {
		if err := m.WithLock(context.Background(), func(*Session) error { return nil }); err != nil {
			t.Fatalf("iter %d WithLock failed: %v", i, err)
		}
	}
	if fl.unlockCount != 3 {
		t.Errorf("unlock calls = %d, want 3", fl.unlockCount)
	}
}

// ---- typed error 存在性 ----

func TestTypedErrorsExist(t *testing.T) {
	if errors.Is(ErrControlLockTimeout, ErrControlLockTimeout) == false {
		t.Fatal("ErrControlLockTimeout must be a comparable typed error")
	}
	if !strings.Contains(ErrRestartNotRunning.Error(), "start") {
		t.Fatalf("ErrRestartNotRunning message unexpected: %v", ErrRestartNotRunning)
	}
}
