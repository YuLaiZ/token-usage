package analyzer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// claude watcher 根列表：强根=projects_dir；可选根=cc-switch 多账号根 + Cowork 根。
// Cowork 根按与生产装配相同的取值方式（进程环境，含 Windows 的 APPDATA）计算，
// 测试夹具与装配点因此不会因环境取值不同而指向不同目录。
func TestClaudeWatchRoots(t *testing.T) {
	strong, optional := claudeWatchRoots("/data/projects", "/home/u")
	if len(strong) != 1 || strong[0] != "/data/projects" {
		t.Fatalf("strong = %v, want [/data/projects]", strong)
	}
	cowork := collector.ClaudeCoworkRoots("/home/u", runtime.GOOS, os.Getenv)
	want := append([]string{collector.CCSwitchClaudeAccountsRoot("/home/u")}, cowork...)
	if len(optional) != len(want) {
		t.Fatalf("optional = %v, want %v", optional, want)
	}
	for i := range want {
		if optional[i] != want[i] {
			t.Fatalf("optional[%d] = %q, want %q", i, optional[i], want[i])
		}
	}
}

func TestClaudeWatchRoots_EmptyHomeKeepsMainRootOnly(t *testing.T) {
	strong, optional := claudeWatchRoots("/data/projects", "")
	if len(strong) != 1 || strong[0] != "/data/projects" {
		t.Fatalf("strong = %v", strong)
	}
	if len(optional) != 0 {
		t.Fatalf("optional = %v, want none", optional)
	}
}

// 装配接线锁定：setupFromConfig 装配的 claude watcher 确实携带账号根
// （纯函数测试锁不了装配点，特判块被移除时本测试转红）。同
// TestSetupFromConfig_RouterPollerTriggersConfiguredClient 的独立测试模式。
func TestSetupFromConfig_ClaudeWatcherIncludesAccountRoot(t *testing.T) {
	tmpDir := t.TempDir()
	cfgContent := fmt.Sprintf(`data_dir = %q
[clients.claude]
enabled = true
[clients.claude.paths]
projects_dir = %q
[daemon]
poll_interval = 1
`, tmpDir, filepath.Join(tmpDir, "claude"))
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)
	a := NewFromConfig(cfg, noopExecute, nil, 5*time.Second)

	var claude *JSONLWatcher
	for _, w := range a.jsonlWatchers {
		if w.clientName == "claude" {
			claude = w
		}
	}
	if claude == nil {
		t.Fatal("claude watcher not assembled")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantDirs := []string{
		filepath.Clean(filepath.Join(tmpDir, "claude")),
	}
	wantOptional := append([]string{collector.CCSwitchClaudeAccountsRoot(home)},
		collector.ClaudeCoworkRoots(home, runtime.GOOS, os.Getenv)...)
	want := append(append([]string{}, wantDirs...), wantOptional...)
	if len(claude.dirs) != len(want) {
		t.Fatalf("claude watcher dirs = %v, want %v", claude.dirs, want)
	}
	for i := range want {
		if claude.dirs[i] != want[i] {
			t.Fatalf("claude watcher dirs[%d] = %q, want %q", i, claude.dirs[i], want[i])
		}
	}
	if len(claude.optional) != len(wantOptional) {
		t.Fatalf("claude watcher optional = %v, want %v", claude.optional, wantOptional)
	}
	for _, w := range wantOptional {
		if _, ok := claude.optional[w]; !ok {
			t.Fatalf("claude watcher optional = %v, want %q marked optional", claude.optional, w)
		}
	}
}
