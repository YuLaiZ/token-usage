package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用 HOME/USERPROFILE 指向 temp dir，写一份用户配置，跑 config get。
// 同时设置 HOME 与 USERPROFILE：os.UserHomeDir() 在 POSIX 读 HOME、
// 在 Windows 读 USERPROFILE，两者都设置才能跨平台隔离到临时目录。
func setupHomeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	cfgDir := filepath.Join(dir, ".token-usage")
	os.MkdirAll(cfgDir, 0755)
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(content), 0644)
	return dir
}

func TestConfigGetCmd(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"
[clients.codex]
enabled = true
[clients.codex.paths]
db = "/codex/db"
[daemon]
poll_interval = 15
`)
	cmd := newConfigGetCmd()
	cmd.SetArgs([]string{"clients.codex.paths.db"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.TrimSpace(out.String()) != "/codex/db" {
		t.Errorf("get = %q, want /codex/db", out.String())
	}
}

func TestConfigGetCmd_UnknownPath(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	cmd := newConfigGetCmd()
	cmd.SetArgs([]string{"unknown.path"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("未知路径应报错")
	}
}

// config get 的 Long 应明确：读取的是「用户配置层」（不展开 ~、不补默认路径）。
func TestConfigGetCmd_LongExplainsUserLayer(t *testing.T) {
	cmd := newConfigGetCmd()
	if !strings.Contains(cmd.Long, "用户配置层") {
		t.Errorf("config get Long 应说明读取用户配置层，实际: %q", cmd.Long)
	}
}

// config get 的 Long 应明确:完整 effective 配置入口为 config show,
// status/TUI 只提供人机摘要。
func TestConfigGetCmd_LongPointsToConfigShowForEffective(t *testing.T) {
	cmd := newConfigGetCmd()
	// 必须断言 Long 含 config show(完整 effective 入口)。
	if !strings.Contains(cmd.Long, "config show") {
		t.Errorf("config get Long 应指向 config show 作为 effective 入口,实际: %q", cmd.Long)
	}
	// 同时断言 Long 仍含 status 或 TUI(二者只提供人机摘要)。
	if !strings.Contains(cmd.Long, "status") && !strings.Contains(cmd.Long, "TUI") {
		t.Errorf("config get Long 应保留 status/TUI 作为人机摘要提示,实际: %q", cmd.Long)
	}
}
