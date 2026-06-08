package runtimecfg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// TestConfigPath_DerivesFromHome 固定 home 时 ConfigPath 结果唯一确定，
// 不依赖 os.UserHomeDir，测试机无关。
func TestConfigPath_DerivesFromHome(t *testing.T) {
	tests := []struct {
		name string
		home string
		want string
	}{
		{"absolute", "/tmp/fake-home", "/tmp/fake-home/.token-usage/config.toml"},
		{"nested", "/Users/alice", "/Users/alice/.token-usage/config.toml"},
		{"relative", "relative/home", "relative/home/.token-usage/config.toml"},
		{"empty", "", ".token-usage/config.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigPath(tt.home)
			if got != tt.want {
				t.Errorf("ConfigPath(%q) = %q, want %q", tt.home, got, tt.want)
			}
		})
	}
}

// writeConfigBytes 写入给定字节并返回路径（不做任何改写，便于断言 Raw 逐字节一致）。
func writeConfigBytes(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestLoadUserConfigSnapshot_RawMatchesDisk Raw 必须与磁盘字节逐字节相同，
// 且 Config 确由该次 Raw 解析（非另一次 read）。
func TestLoadUserConfigSnapshot_RawMatchesDisk(t *testing.T) {
	content := []byte("data_dir = \"/tmp/x\"\n[clients.codex]\nenabled = true\n")
	p := writeConfigBytes(t, content)

	snap, err := LoadUserConfigSnapshot(p)
	if err != nil {
		t.Fatalf("LoadUserConfigSnapshot: %v", err)
	}
	if !snap.Exists {
		t.Fatal("Exists=true 期望（文件存在）")
	}
	if !bytes.Equal(content, snap.Raw) {
		t.Fatalf("Raw 与磁盘字节不一致\nwant=%q\n got=%q", string(content), string(snap.Raw))
	}
	if snap.Config == nil {
		t.Fatal("Config 不应为 nil")
	}
	if snap.Config.DataDir != "/tmp/x" {
		t.Errorf("Config.DataDir = %q, want /tmp/x", snap.Config.DataDir)
	}
	if !snap.Config.Clients["codex"].Enabled {
		t.Error("Config 应解析出 codex.enabled=true")
	}
}

// TestLoadUserConfigSnapshot_FileMissingExistsFalse 文件缺失：Exists=false、Config=nil、Raw=nil。
// 不得与空文件分支（Exists=true + 解析错误）混淆。
func TestLoadUserConfigSnapshot_FileMissingExistsFalse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.toml")
	snap, err := LoadUserConfigSnapshot(p)
	if err != nil {
		t.Fatalf("文件缺失应返回 nil err + Exists=false，实际 err=%v", err)
	}
	if snap.Exists {
		t.Error("Exists 应为 false（文件缺失）")
	}
	if snap.Config != nil {
		t.Error("Config 应为 nil（文件缺失）")
	}
	if snap.Raw != nil {
		t.Error("Raw 应为 nil（文件缺失）")
	}
}

// TestLoadUserConfigSnapshot_EmptyFileIsParseError 空文件：Exists=true 且按解析错误处理，
// 与文件缺失分支严格区分（避免 sentinel/空文件被当作「未配置」隐式创建半份配置）。
func TestLoadUserConfigSnapshot_EmptyFileIsParseError(t *testing.T) {
	p := writeConfigBytes(t, []byte{})
	snap, err := LoadUserConfigSnapshot(p)
	if err == nil {
		t.Fatalf("空文件应返回解析错误，实际 err=nil snap=%+v", snap)
	}
	if !snap.Exists {
		t.Error("Exists 应为 true（空文件存在）")
	}
	// Config/Raw 在错误分支不保证内容；只校验 Exists 与 err。
}

// TestLoadUserConfigSnapshot_DoesNotApplyDefaults 用户层 snapshot 不回填默认值、不展开 ~。
// 保证 marshal 写回保持用户原值简洁、~ 可移植。
func TestLoadUserConfigSnapshot_DoesNotApplyDefaults(t *testing.T) {
	p := writeConfigBytes(t, []byte("data_dir = \"~/.token-usage\"\n[clients.codex]\nenabled = true\n"))
	snap, err := LoadUserConfigSnapshot(p)
	if err != nil {
		t.Fatalf("LoadUserConfigSnapshot: %v", err)
	}
	if snap.Config.Daemon.PollInterval != 0 {
		t.Errorf("PollInterval 应保持 0（用户层不回填），实际 %d", snap.Config.Daemon.PollInterval)
	}
	if snap.Config.Log.Level != "" {
		t.Errorf("Log.Level 应保持空（用户层不回填），实际 %q", snap.Config.Log.Level)
	}
	if snap.Config.DataDir != "~/.token-usage" {
		t.Errorf("DataDir 应保持 ~ 字面值，实际 %q", snap.Config.DataDir)
	}
}

// TestLoadUserConfigSnapshot_NilMapsInitialized nil map 初始化为空 map（避免 TUI/set 写入 panic）。
func TestLoadUserConfigSnapshot_NilMapsInitialized(t *testing.T) {
	p := writeConfigBytes(t, []byte("data_dir = \"/x\"\n"))
	snap, err := LoadUserConfigSnapshot(p)
	if err != nil {
		t.Fatalf("LoadUserConfigSnapshot: %v", err)
	}
	snap.Config.Clients["new"] = config.Client{Enabled: true}
	snap.Config.Routers["cc_switch"] = config.RouterConfig{DBPath: "/x"}
	snap.Config.ProviderAliases["A"] = "B"
}

func TestLoadUserConfigSnapshot_RejectsUnknownNestedFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "unknown client field",
			content: `
[clients.claude]
enabled = true
unexpected = "value"
`,
			want: "unexpected",
		},
		{
			name: "unknown router path key",
			content: `
[routers.cc_switch]
db_path = "/tmp/router.db"
unexpected_path = "/tmp/other.db"
`,
			want: "unexpected_path",
		},
		{
			name:    "unknown top-level field",
			content: `unexpected = true`,
			want:    "unexpected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigBytes(t, []byte(tt.content))
			_, err := LoadUserConfigSnapshot(path)
			if err == nil {
				t.Fatal("未知配置字段应被拒绝")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("错误 %q 未包含未知字段 %q", err, tt.want)
			}
		})
	}
}
