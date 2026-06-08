package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// TestNewTUIApplyFunc_ReturnsApplyFunc 验证 TUI adapter 装配:给定临时 HOME,
// newTUIApplyFunc 返回非 nil 的 ApplyFunc(与 configSetApplyFactory 同一装配)。
func TestNewTUIApplyFunc_ReturnsApplyFunc(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	home := os.Getenv("HOME")
	apply, err := newTUIApplyFunc(home)
	if err != nil {
		t.Fatalf("newTUIApplyFunc: %v", err)
	}
	if apply == nil {
		t.Fatal("ApplyFunc 不应为 nil")
	}
}

// TestNewTUIApplyFunc_SavesAndAdvancesRevision 验证 TUI ApplyFunc 端到端:
// 用 snapshot 初始化 draft + diskRevision,调 ApplyFunc 保存后磁盘配置已更新、
// NewRevision 非空、ConfigApplied=true。这是 TUI 保存路径的生产合同。
func TestNewTUIApplyFunc_SavesAndAdvancesRevision(t *testing.T) {
	setupHomeConfig(t, "data_dir = \"/x\"\n[daemon]\npoll_interval = 15\n")
	home := os.Getenv("HOME")
	path := runtimecfg.ConfigPath(home)

	apply, err := newTUIApplyFunc(home)
	if err != nil {
		t.Fatalf("newTUIApplyFunc: %v", err)
	}

	// 一次 snapshot 读取初始化 draft + diskRevision(与 runConfigTUI 同源)。
	snap, err := runtimecfg.LoadUserConfigSnapshot(path)
	if err != nil {
		t.Fatalf("LoadUserConfigSnapshot: %v", err)
	}
	draft := snap.Config
	diskRevision := configapp.Revision(snap.Raw)

	// 改 draft 制造 dirty。
	draft.Daemon.PollInterval = 42

	result, applyErr := apply(diskRevision, draft)
	if applyErr != nil {
		t.Fatalf("ApplyFunc 保存失败: %v", applyErr)
	}
	if !result.ConfigApplied {
		t.Error("ConfigApplied 应为 true")
	}
	if len(result.NewRevision) == 0 {
		t.Error("NewRevision 不应为空")
	}

	// 重新加载磁盘验证 PollInterval 已写入。
	cfg, err := config.LoadUserConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Daemon.PollInterval != 42 {
		t.Errorf("磁盘 PollInterval = %d, want 42", cfg.Daemon.PollInterval)
	}
}

// TestNewTUIApplyFunc_FixedConfirmFalse 验证 TUI ApplyFunc 固定 confirm=false:
// 即使 draft 改了 data_dir,ApplyFunc 也不会触发迁移确认流程(会按未确认处理)。
// 这锁定 data_dir 在 TUI 只读的合同。
func TestNewTUIApplyFunc_FixedConfirmFalse(t *testing.T) {
	setupHomeConfig(t, "data_dir = \"/orig\"\n")
	home := os.Getenv("HOME")
	path := runtimecfg.ConfigPath(home)

	apply, err := newTUIApplyFunc(home)
	if err != nil {
		t.Fatalf("newTUIApplyFunc: %v", err)
	}
	snap, err := runtimecfg.LoadUserConfigSnapshot(path)
	if err != nil {
		t.Fatalf("LoadUserConfigSnapshot: %v", err)
	}
	draft := snap.Config
	diskRevision := configapp.Revision(snap.Raw)
	// 尝试改 data_dir:TUI ApplyFunc 固定 confirm=false → ApplyConfig 应拒绝迁移
	// (返回 ConfigApplied=false 与 data_dir 需确认错误),不写盘。
	draft.DataDir = filepath.Join(t.TempDir(), "newdata")

	result, applyErr := apply(diskRevision, draft)
	// data_dir 变化未确认 → ApplyConfig 返回非 nil err,ConfigApplied=false。
	if applyErr == nil {
		t.Fatal("改 data_dir 但 confirm=false 应被 ApplyConfig 拒绝(err 非 nil)")
	}
	if result.ConfigApplied {
		t.Error("data_dir 未确认时 ConfigApplied 应为 false")
	}
}

func TestLoadTUIConfigState_DerivesDraftDisplayAndRevisionFromOneSnapshot(t *testing.T) {
	home := setupHomeConfig(t, "data_dir = \"~/data\"\n[daemon]\npoll_interval = 15\n")
	path := runtimecfg.ConfigPath(home)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := runtimecfg.ResolveEnv{Home: home, GOOS: goruntime.GOOS, DefaultPaths: runtimecfg.NewStandardProvider()}

	draft, display, revision, err := loadTUIConfigState(path, env)
	if err != nil {
		t.Fatal(err)
	}
	if draft.DataDir != "~/data" {
		t.Errorf("draft 必须保留用户层字面值，got %q", draft.DataDir)
	}
	if display.DataDir != filepath.Join(home, "data") || display.Daemon.PollInterval != 15 {
		t.Errorf("display 应从同一 snapshot 解析有效值，got %+v", display)
	}
	if !bytes.Equal(revision, configapp.Revision(raw)) {
		t.Error("revision 必须来自同一 snapshot 的 raw bytes")
	}
}

func TestEnsureDefaultConfig_DoesNotOverwriteExistingFile(t *testing.T) {
	home := t.TempDir()
	path := runtimecfg.ConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("data_dir = \"/custom\"\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatal(err)
	}
	created, err := ensureDefaultConfig(context.Background(), mgr, path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("已有文件不应被标记为新建")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Error("已有配置被覆盖")
	}
}
