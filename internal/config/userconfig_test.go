package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUserConfigTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

// 用户配置层不回填:最小配置的 Daemon.PollInterval 保持 0、Log.Level 空。
// effective 层（默认值 clamp 30）由 runtimecfg.ResolveEffectiveConfig 负责，测试见 runtimecfg 包。
func TestLoadUserConfig_NoDefaultsApplied(t *testing.T) {
	path := writeUserConfigTemp(t, `data_dir = "/tmp/x"
[clients.codex]
enabled = true
`)
	cfg, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if cfg.Daemon.PollInterval != 0 {
		t.Errorf("PollInterval 应保持 0(不回填),实际 %d", cfg.Daemon.PollInterval)
	}
	if cfg.Log.Level != "" {
		t.Errorf("Log.Level 应保持空(不回填),实际 %q", cfg.Log.Level)
	}
}

// nil map 初始化为空 map(TUI 新增/set 不 panic)
func TestLoadUserConfig_NilMapsInitialized(t *testing.T) {
	cfg, err := LoadUserConfig(writeUserConfigTemp(t, `data_dir = "/tmp/x"`))
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	cfg.Clients["newclient"] = Client{Enabled: true}
	cfg.Routers["cc_switch"] = RouterConfig{DBPath: "/x"}
	cfg.ProviderAliases["X"] = "Y"
	if !cfg.Clients["newclient"].Enabled {
		t.Error("Clients 赋值失败")
	}
}

// provider_aliases 的 key 是 query provider 的精确匹配键，大小写不得被解析器改写。
func TestLoadUserConfig_PreservesProviderAliasKeyCase(t *testing.T) {
	cfg, err := LoadUserConfig(writeUserConfigTemp(t, `[provider_aliases]
"OpenCode-Completions" = "OpenCode-Display"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProviderAliases["OpenCode-Completions"]; got != "OpenCode-Display" {
		t.Fatalf("mixed-case alias value = %q, want OpenCode-Display", got)
	}
	if _, ok := cfg.ProviderAliases["opencode-completions"]; ok {
		t.Fatalf("alias key was lowercased: %#v", cfg.ProviderAliases)
	}
}

// ~ 不展开(保持字面值)
func TestLoadUserConfig_TildeNotExpanded(t *testing.T) {
	cfg, err := LoadUserConfig(writeUserConfigTemp(t, `data_dir = "~/.token-usage"
[clients.codex]
enabled = true
[clients.codex.paths]
db = "~/.codex/x"
`))
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if cfg.DataDir != "~/.token-usage" {
		t.Errorf("DataDir 应保持 ~,实际 %q", cfg.DataDir)
	}
	if cfg.Clients["codex"].Paths["db"] != "~/.codex/x" {
		t.Errorf("Paths.db 应保持 ~,实际 %q", cfg.Clients["codex"].Paths["db"])
	}
}

func TestLoadUserConfig_RejectsEmptyAndUnknownFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty", content: " \n\t", want: "为空"},
		{
			name: "unknown nested field",
			content: `
[clients.claude]
enabled = true
unexpected = "value"
`,
			want: "unexpected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadUserConfig(writeUserConfigTemp(t, tt.content))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("err=%v, want 包含 %q", err, tt.want)
			}
		})
	}
}

// LoadUserConfigAuto 从默认路径加载
func TestLoadUserConfigAuto_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfgDir := filepath.Join(dir, ".token-usage")
	os.MkdirAll(cfgDir, 0755)
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(`data_dir = "/x"`), 0644)

	cfg, err := LoadUserConfigAuto()
	if err != nil {
		t.Fatalf("LoadUserConfigAuto: %v", err)
	}
	if cfg.DataDir != "/x" {
		t.Errorf("DataDir = %q, want /x", cfg.DataDir)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	p, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	want := filepath.Join(dir, ".token-usage", "config.toml")
	if p != want {
		t.Errorf("DefaultConfigPath = %q, want %q", p, want)
	}
}
