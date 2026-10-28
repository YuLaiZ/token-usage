package analyzer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// claude watcher 根列表：强根=projects_dir、可选根=cc-switch 多账号根；home 取不到只有强根。
func TestClaudeWatchRoots(t *testing.T) {
	strong, optional := claudeWatchRoots("/data/projects", "/home/u")
	if len(strong) != 1 || strong[0] != "/data/projects" {
		t.Fatalf("strong = %v, want [/data/projects]", strong)
	}
	want := collector.CCSwitchClaudeAccountsRoot("/home/u")
	if len(optional) != 1 || optional[0] != want {
		t.Fatalf("optional = %v, want [%s]", optional, want)
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
	want := []string{
		filepath.Clean(filepath.Join(tmpDir, "claude")),
		collector.CCSwitchClaudeAccountsRoot(home),
	}
	if len(claude.dirs) != 2 || claude.dirs[0] != want[0] || claude.dirs[1] != want[1] {
		t.Fatalf("claude watcher dirs = %v, want %v", claude.dirs, want)
	}
	if len(claude.optional) != 1 {
		t.Fatalf("claude watcher optional = %v, want exactly the account root", claude.optional)
	}
	if _, ok := claude.optional[want[1]]; !ok {
		t.Fatalf("claude watcher optional = %v, want %q marked optional", claude.optional, want[1])
	}
}
