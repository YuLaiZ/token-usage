package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_AppliesDefaultPaths 验证 loadConfig 回填默认 paths，并为 client 声明的
// router 创建默认 entry。最小配置：codex 只 enabled（测 paths 回填）、claude enabled+router="cc_switch"
// （测 client 声明 router 时 routers 表缺失 → 创建默认 entry）。
func TestLoadConfig_AppliesDefaultPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 上 os.UserHomeDir 读 USERPROFILE
	cfgDir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(`
data_dir = "`+cfgDir+`"
[clients.codex]
enabled = true
[clients.claude]
enabled = true
router = "cc_switch"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := cfg.Clients["codex"].Paths["state_dir"]; got != filepath.Join(home, ".codex") {
		t.Errorf("loadConfig 未回填 codex state_dir: %q", got)
	}
	if got := cfg.Routers["cc_switch"].DBPath; got != filepath.Join(home, ".cc-switch", "cc-switch.db") {
		t.Errorf("loadConfig 未为 claude 声明的 cc_switch 创建默认 db_path: %q", got)
	}
}

// TestLoadRuntime_AppliesDefaultPaths 端到端验证 loadRuntime（经 loadConfig）回填默认 paths，
// 并为 client 声明的 router 创建默认 entry。若 runtime.go 漏调 loadConfig/ApplyDefaultPaths，测试失败。
func TestLoadRuntime_AppliesDefaultPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 上 os.UserHomeDir 读 USERPROFILE

	cfgDir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	// data_dir/log 段让 loadRuntime 能初始化 logger 与 db；codex 测 paths、claude 声明 cc_switch 测 router
	configContent := `
data_dir = "` + cfgDir + `"
[clients.codex]
enabled = true
[clients.claude]
enabled = true
router = "cc_switch"
[daemon]
poll_interval = 30
[log]
level = "info"
dir = "` + cfgDir + `/logs"
max_days = 7
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	rt, cleanup, err := loadRuntime()
	if err != nil {
		t.Fatalf("loadRuntime: %v", err)
	}
	defer cleanup()

	wantState := filepath.Join(home, ".codex")
	if got := rt.cfg.Clients["codex"].Paths["state_dir"]; got != wantState {
		t.Errorf("codex state_dir = %q, want %q（loadRuntime 应经 loadConfig 回填默认）", got, wantState)
	}
	if got := rt.cfg.Clients["codex"].Paths["sessions_dir"]; got != filepath.Join(wantState, "sessions") {
		t.Errorf("codex sessions_dir = %q, want %q", got, filepath.Join(wantState, "sessions"))
	}
	wantDB := filepath.Join(home, ".cc-switch", "cc-switch.db")
	if got := rt.cfg.Routers["cc_switch"].DBPath; got != wantDB {
		t.Errorf("cc_switch db_path = %q, want %q（claude 声明 router，应创建默认 entry）", got, wantDB)
	}
}
