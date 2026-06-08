package cli

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// TestParseDates_* 已迁移为 parseDateArgs 测试，见 date_test.go。

func TestCheckDaemonConflict_DaemonRunning(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "daemon.lock")

	// 模拟守护进程运行
	f, ok := daemon.AcquireLock(lockPath)
	if !ok {
		t.Fatal("failed to acquire lock")
	}
	defer daemon.ReleaseLock(f)

	// 测试 collect 命令应该被拒绝
	err := checkDaemonConflict(lockPath)
	if err == nil {
		t.Error("expected error when daemon is running")
	}

	expected := "守护进程正在运行"
	if err != nil && !strings.Contains(err.Error(), expected) {
		t.Errorf("expected error to contain %q, got %q", expected, err.Error())
	}
}

func TestCheckDaemonConflict_DaemonNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "daemon.lock")

	// 测试 collect 命令应该正常
	err := checkDaemonConflict(lockPath)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

// TestCollectPreflight_DaemonConflictBeforeForce 守护 collect 前置检查的顺序不变式：
// 即使 --force 也必须在守护进程运行时被冲突拒绝，而非放行。
func TestCollectPreflight_DaemonConflictBeforeForce(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "token-usage.lock")

	f, ok := daemon.AcquireLock(lockPath)
	if !ok {
		t.Fatal("failed to pre-acquire lock")
	}
	defer daemon.ReleaseLock(f)

	cfg := &config.Config{DataDir: tmpDir}

	// force=true 但守护进程在运行：应被冲突拒绝（handled=true, err 非空）
	handled, err := collectPreflight(cfg, false, true /*force*/)
	if !handled || err == nil {
		t.Fatalf("expected daemon-conflict to short-circuit before force; handled=%v err=%v", handled, err)
	}
	if !strings.Contains(err.Error(), "守护进程正在运行") {
		t.Errorf("expected daemon-conflict error, got: %v", err)
	}
}

// TestCollectPreflight_ForcePassesWhenNoDaemon 守护进程未运行 + force：
// --force 表示全量重采覆盖，collectPreflight 对 force 放行（handled=false, err=nil）。
func TestCollectPreflight_ForcePassesWhenNoDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{DataDir: tmpDir}

	handled, err := collectPreflight(cfg, false, true /*force*/)
	if handled || err != nil {
		t.Fatalf("--force without daemon must pass through; handled=%v err=%v", handled, err)
	}
}

func TestCollectPreflight_RetryContinues(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	handled, err := collectPreflight(cfg, true, false)
	if handled || err != nil {
		t.Fatalf("retry must continue to runRetry: handled=%v err=%v", handled, err)
	}
}

// TestCollectPreflight_ForceWithRetryPasses --retry --force 组合放行。
func TestCollectPreflight_ForceWithRetryPasses(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	handled, err := collectPreflight(cfg, true /*retry*/, true /*force*/)
	if handled || err != nil {
		t.Fatalf("--retry --force must pass through; handled=%v err=%v", handled, err)
	}
}

// TestNewCollectCmd_FlagsStructure --client 是 PersistentFlag，--force 是 LocalFlag，
// --all/--retry 已移除（改为子命令分发）。
func TestNewCollectCmd_FlagsStructure(t *testing.T) {
	cmd := newCollectCmd()
	if cmd.PersistentFlags().Lookup("client") == nil {
		t.Error("期望 --client 注册为 PersistentFlag")
	}
	if cmd.Flags().Lookup("force") == nil {
		t.Error("期望 --force 注册为 LocalFlag")
	}
	for _, obsolete := range []string{"all", "retry", "date"} {
		if cmd.Flags().Lookup(obsolete) != nil || cmd.PersistentFlags().Lookup(obsolete) != nil {
			t.Errorf("期望 --%s flag 已移除", obsolete)
		}
	}
}

// fakeFullCollectCollector 用于 runOneFullCollect 测试。
type fakeFullCollectCollector struct {
	name    string
	callCnt int
}

func (f *fakeFullCollectCollector) Name() string          { return f.name }
func (f *fakeFullCollectCollector) SyncSources() []string { return nil }
func (f *fakeFullCollectCollector) Collect(ctx context.Context, req collector.CollectRequest, log *slog.Logger) (collector.CollectResult, error) {
	f.callCnt++
	return collector.CollectResult{}, nil
}

// TestRunOneFullCollect_SingleClient 全采单 client 调用 RunCollect 一次，Dates=nil。
func TestRunOneFullCollect_SingleClient(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true}},
	}
	fc := &fakeFullCollectCollector{name: "claude"}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fc}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer usageDB.Close()

	err = runOneFullCollect(context.Background(), deps, usageDB, slog.Default(), nil, "claude")
	if err != nil {
		t.Fatalf("runOneFullCollect failed: %v", err)
	}
	if fc.callCnt != 1 {
		t.Errorf("期望 collector.Collect 调用 1 次，实际 %d", fc.callCnt)
	}
}

// errorCollector Collect 始终返回 error（供其他测试复用）。
type errorCollector struct{ name string }

func (e *errorCollector) Name() string          { return e.name }
func (e *errorCollector) SyncSources() []string { return nil }
func (e *errorCollector) Collect(ctx context.Context, req collector.CollectRequest, log *slog.Logger) (collector.CollectResult, error) {
	return collector.CollectResult{}, fmt.Errorf("simulated failure")
}
