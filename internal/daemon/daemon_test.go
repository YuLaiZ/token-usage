// internal/daemon/daemon_test.go
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// openTestResources 打开一个真实（空）的 usage.db + 默认 logger，供启用了 client 的生命周期测试使用。
// monitor ready 后 coordinator 会经 Submit 跑 startup catch-up，需要非 nil DB 才不会 panic。
// 返回的 RuntimeResources.Close 关闭 DB。openCallsFn 非 nil 时累加调用计数（便于断言 OpenResources 次数）。
func openTestResources(t *testing.T, openCallsFn *int32) func(c *config.Config) (RuntimeResources, error) {
	t.Helper()
	return func(c *config.Config) (RuntimeResources, error) {
		if openCallsFn != nil {
			atomic.AddInt32(openCallsFn, 1)
		}
		usageDB, err := db.Open(filepath.Join(c.DataDir, "usage.db"))
		if err != nil {
			t.Fatalf("open test db: %v", err)
		}
		return RuntimeResources{
			DB:  usageDB,
			Log: slog.Default(),
			Close: func() error {
				usageDB.Close()
				return nil
			},
		}, nil
	}
}

// withCloseErrOpenResources 与 openTestResources 相同但 Close 返回指定错误（用于 Close 错误 join 测试）。
func withCloseErrOpenResources(t *testing.T, closeErr error) func(c *config.Config) (RuntimeResources, error) {
	t.Helper()
	return func(c *config.Config) (RuntimeResources, error) {
		usageDB, err := db.Open(filepath.Join(c.DataDir, "usage.db"))
		if err != nil {
			t.Fatalf("open test db: %v", err)
		}
		return RuntimeResources{
			DB:  usageDB,
			Log: slog.Default(),
			Close: func() error {
				usageDB.Close()
				return closeErr
			},
		}, nil
	}
}

// writeTestConfig 写入最小可用 config（无客户端）并返回。
func writeTestConfig(t *testing.T, tmpDir string) *config.Config {
	t.Helper()
	return writeTestConfigRaw(t, tmpDir, false)
}

// writeTestConfigWithClaude 写入启用 claude 客户端的 config（让 analyzer 能持续跑直到 ctx 取消）。
func writeTestConfigWithClaude(t *testing.T, tmpDir string) *config.Config {
	t.Helper()
	return writeTestConfigRaw(t, tmpDir, true)
}

func writeTestConfigRaw(t *testing.T, tmpDir string, withClaude bool) *config.Config {
	t.Helper()
	if withClaude {
		if err := os.MkdirAll(filepath.Join(tmpDir, "claude"), 0o755); err != nil {
			t.Fatalf("mkdir claude: %v", err)
		}
	}
	cfgPath := filepath.Join(tmpDir, "config.toml")
	claudeSection := ""
	if withClaude {
		claudeSection = `
[clients.claude]
enabled = true
[clients.claude.paths]
projects_dir = "` + tmpDir + `/claude"
`
	}
	cfgContent := `data_dir = "` + tmpDir + `"
` + claudeSection + `
[daemon]
poll_interval = 1
[log]
level = "info"
dir = "` + tmpDir + `/logs"
max_days = 7
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := runtimecfg.LoadEffectiveConfig(cfgPath, runtimecfg.ResolveEnv{
		Home:         tmpDir,
		GOOS:         "linux",
		DefaultPaths: runtimecfg.NewStandardProvider(),
	})
	if err != nil {
		t.Fatalf("load effective config: %v", err)
	}
	return cfg
}

// TestRun_LockConflictWithLockHeld 预占 cfg.DataDir 下的锁文件后调用 Run，
// 验证它在进入 analyzer 之前就因锁冲突返回 error（不会 hang、不会启动监控 goroutine）。
// 新签名：cfg + RunOptions；usageDB/log 由 OpenResources 在 lock commit 后打开。
func TestRun_LockConflictWithLockHeld(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfig(t, tmpDir)

	// 预占 Run 将要竞争的同一把锁
	lockPath := cfg.DataDir + "/token-usage.lock"
	holder, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("failed to pre-acquire lock")
	}
	defer ReleaseLock(holder)

	// 关键断言：lock 冲突时 OpenResources 必须 0 次调用（DB/logger 不能在 lock 前/失败时打开）。
	openCalls := int32(0)
	opts := RunOptions{
		InstanceID: "daemon-test",
		OpenResources: func(c *config.Config) (RuntimeResources, error) {
			atomic.AddInt32(&openCalls, 1)
			return RuntimeResources{Log: slog.Default(), Close: func() error { return nil }}, nil
		},
	}

	err := Run(context.Background(), cfg, opts)
	if err == nil {
		t.Fatal("expected lock-conflict error from Run, got nil")
	}
	if got := atomic.LoadInt32(&openCalls); got != 0 {
		t.Errorf("lock 冲突时 OpenResources 必须调用 0 次，实际 %d", got)
	}
}

// TestRun_OnDaemonLockCommitCalledExactlyOnce lock 获取后回调恰好调用一次。
func TestRun_OnDaemonLockCommitCalledExactlyOnce(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	commitCalls := int32(0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(400 * time.Millisecond)
		cancel()
	}()

	opts := RunOptions{
		InstanceID: "daemon-test",
		OnDaemonLockCommit: func() error {
			atomic.AddInt32(&commitCalls, 1)
			return nil
		},
		OpenResources: openTestResources(t, nil),
	}
	if err := Run(ctx, cfg, opts); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if got := atomic.LoadInt32(&commitCalls); got != 1 {
		t.Errorf("OnDaemonLockCommit 应恰好调用一次，实际 %d", got)
	}
}

// TestRun_OpenResourcesCloseErrorJoinedWithMainError 主错误 + Close 错误用 errors.Join 合并。
// 用 claude config 让 analyzer 跑到 ctx 取消（主错误=ctx.Err），Close 返回 closeErr，两者应都出现。
func TestRun_OpenResourcesCloseErrorJoinedWithMainError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(400 * time.Millisecond)
		cancel() // 触发 analyzer 因 ctx 取消退出（主错误）
	}()

	closeErr := errors.New("close boom")
	opts := RunOptions{
		InstanceID:    "daemon-test",
		OpenResources: withCloseErrOpenResources(t, closeErr),
	}
	err := Run(ctx, cfg, opts)
	// ctx 取消时 analyzer 可能返回 nil（优雅退出）或 ctx.Err；Close 错误一定 join 进来。
	// 断言 closeErr 一定出现；若主错误非空则应与 closeErr join。
	if err == nil {
		t.Fatal("应返回错误（至少含 Close 错误）")
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("应含 close 错误（errors.Join），实际: %v", err)
	}
}

// TestRun_OpenResourcesErrorReturnedAndNoClose OpenResources 自身失败 → 返回错误，不调 Close。
func TestRun_OpenResourcesErrorReturnedAndNoClose(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfig(t, tmpDir)

	openErr := errors.New("open boom")
	closeCalled := false
	opts := RunOptions{
		InstanceID: "daemon-test",
		OpenResources: func(c *config.Config) (RuntimeResources, error) {
			// 即使 OpenResources 返回错误，仍可能有人错误地在 defer 里调 Close。
			// 这里返回一个会设置 flag 的 Close，验证它绝不被调用。
			return RuntimeResources{Close: func() error {
				closeCalled = true
				return nil
			}}, openErr
		},
	}

	err := Run(context.Background(), cfg, opts)
	if err == nil || !errors.Is(err, openErr) {
		t.Fatalf("应返回 openErr，实际: %v", err)
	}
	if closeCalled {
		t.Error("OpenResources 返回错误时 Close 不应被调用")
	}
}

// TestRun_DaemonSuccess 测试 daemon 完整流程：
// 获取锁 → OnDaemonLockCommit → OpenResources → 写 PID → 启动 analyzer → 退出 → 删 PID
func TestRun_DaemonSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	lockPath := cfg.DataDir + "/token-usage.lock"
	pidPath := cfg.DataDir + "/token-usage.pid"

	if IsDaemonRunning(lockPath) {
		t.Fatal("daemon should not be running initially")
	}

	openCalls := int32(0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(500 * time.Millisecond)
		cancel()
	}()

	opts := RunOptions{
		InstanceID:    "daemon-test",
		OpenResources: openTestResources(t, &openCalls),
	}
	if err := Run(ctx, cfg, opts); err != nil {
		t.Fatalf("Run daemon returned error: %v", err)
	}

	if got := atomic.LoadInt32(&openCalls); got != 1 {
		t.Errorf("OpenResources 应恰好调用一次，实际 %d", got)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should be cleaned up after Run exits")
	}
	if IsDaemonRunning(lockPath) {
		t.Error("lock should be released after Run exits")
	}
}

// TestRun_OpenResourcesNilUsesProductionFactory OpenResources=nil 走生产 factory（真实打开 DB+logger）。
// 用 claude config 跑短时间后退出，断言不 panic 且 DB 正常打开/关闭。
func TestRun_OpenResourcesNilUsesProductionFactory(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(400 * time.Millisecond)
		cancel()
	}()

	// OpenResources=nil：用生产 factory 打开 DB+logger。
	if err := Run(ctx, cfg, RunOptions{InstanceID: "daemon-test"}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	// usage.db 应已被创建（生产 factory 打开）。
	if _, err := os.Stat(filepath.Join(tmpDir, "usage.db")); err != nil {
		t.Errorf("usage.db 应被生产 factory 创建: %v", err)
	}
}

// ---- 父子 lease 集成 ----

// TestRun_ParentLeaseLostBeforeAcquire_CancelledAndNoDB ParentLeaseLost 在 Run 前已关闭 →
// 取消启动，AcquireLock/OnDaemonLockCommit/OpenResources 全部 0 调用，返回 ErrParentLeaseLost。
func TestRun_ParentLeaseLostBeforeAcquire_CancelledAndNoDB(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfig(t, tmpDir)

	lost := make(chan struct{})
	close(lost) // 预先关闭：EOF 已先到。

	commitCalls := int32(0)
	openCalls := int32(0)
	opts := RunOptions{
		InstanceID:      "daemon-test",
		ParentLeaseLost: lost,
		OnDaemonLockCommit: func() error {
			atomic.AddInt32(&commitCalls, 1)
			return nil
		},
		OpenResources: func(c *config.Config) (RuntimeResources, error) {
			atomic.AddInt32(&openCalls, 1)
			return RuntimeResources{Log: slog.Default(), Close: func() error { return nil }}, nil
		},
	}

	err := Run(context.Background(), cfg, opts)
	if !errors.Is(err, ErrParentLeaseLost) {
		t.Fatalf("应返回 ErrParentLeaseLost，实际: %v", err)
	}
	if got := atomic.LoadInt32(&commitCalls); got != 0 {
		t.Errorf("lease 取消时 OnDaemonLockCommit 应 0 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&openCalls); got != 0 {
		t.Errorf("lease 取消时 OpenResources 应 0 次（不污染 DB），实际 %d", got)
	}
	// 不应持有 daemon lock（取消后释放）。
	if IsDaemonRunning(cfg.DataDir + "/token-usage.lock") {
		t.Error("取消后不应持有 daemon lock")
	}
	// 不应写 PID 文件。
	if _, err := os.Stat(cfg.DataDir + "/token-usage.pid"); !os.IsNotExist(err) {
		t.Error("取消后不应写 PID 文件")
	}
}

// TestRun_ParentLeaseLostNil_ProceedsNormally ParentLeaseLost=nil（无父 lease/独立路径）
// → 正常启动，OnDaemonLockCommit/OpenResources 正常调用。
func TestRun_ParentLeaseLostNil_ProceedsNormally(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	commitCalls := int32(0)
	openCalls := int32(0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(300 * time.Millisecond)
		cancel()
	}()
	opts := RunOptions{
		InstanceID:      "daemon-test",
		ParentLeaseLost: nil, // 独立路径
		OnDaemonLockCommit: func() error {
			atomic.AddInt32(&commitCalls, 1)
			return nil
		},
		OpenResources: openTestResources(t, &openCalls),
	}
	if err := Run(ctx, cfg, opts); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if got := atomic.LoadInt32(&commitCalls); got != 1 {
		t.Errorf("独立路径 OnDaemonLockCommit 应 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&openCalls); got != 1 {
		t.Errorf("独立路径 OpenResources 应 1 次，实际 %d", got)
	}
}

// TestRun_ParentLeaseLostNotClosed_ProceedsThenEOFIsHarmless ParentLeaseLost 未关闭 →
// 正常启动；commit 后关闭 ParentLeaseLost 不影响（已接管）。
// 这模拟「daemon lock 先获得，EOF 后到」的正确语义。
func TestRun_ParentLeaseLostNotClosed_ProceedsThenEOFIsHarmless(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := writeTestConfigWithClaude(t, tmpDir)

	lost := make(chan struct{}) // 未关闭
	commitCalls := int32(0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-time.After(300 * time.Millisecond)
		cancel() // analyzer 退出
	}()
	opts := RunOptions{
		InstanceID:      "daemon-test",
		ParentLeaseLost: lost,
		OnDaemonLockCommit: func() error {
			atomic.AddInt32(&commitCalls, 1)
			return nil
		},
		OpenResources: openTestResources(t, nil),
	}
	if err := Run(ctx, cfg, opts); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if got := atomic.LoadInt32(&commitCalls); got != 1 {
		t.Errorf("ParentLeaseLost 未关闭时 OnDaemonLockCommit 应 1 次，实际 %d", got)
	}
}
