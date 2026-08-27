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

// query 剥离与重编码路径不得放宽既有非 query 严格性:未知键、类型错误、
// 非 ASCII 顶层键、TOML 语法错误仍按原类别失败。
func TestParseUserConfig_NonQueryStrictnessPreserved(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "unknown top-level key",
			content: `unexpected = true`,
			want:    "unexpected",
		},
		{
			name: "unknown nested field",
			content: `
[clients.claude]
enabled = true
unexpected = "value"
`,
			want: "unexpected",
		},
		{
			name: "type error keeps key path",
			content: `
[daemon]
poll_interval = "notanumber"
`,
			want: "poll_interval",
		},
		{
			name:    "non-ascii top-level key is unknown key",
			content: `["QUÉRY"]` + "\ndefault = \"a\"\n",
			want:    "解析配置文件失败",
		},
		{
			// mapstructure 把 "-" tag 当字面字段名;字面 "-" 非空表键必须仍按解析错误拒绝,
			// 不得静默进入 raw query 载体而放宽严格性(空表形态新旧版本均静默,属既有行为)。
			name:    "literal dash key still rejected",
			content: "\"-\" = {a = 1}\n",
			want:    "解析配置文件失败",
		},
		{
			name:    "toml syntax error fails at read stage",
			content: `data_dir = `,
			want:    "读取配置文件失败",
		},
		{
			name: "unknown key still rejected alongside valid query",
			content: `
unexpected = true
[query.subqueries]
mpc = "model,provider"
`,
			want: "unexpected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseUserConfig([]byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want 包含 %q", err, tt.want)
			}
		})
	}
}

// provider_aliases 的原始大小写恢复与 query raw 状态互不影响:
// 合法 query 与顶层冲突 query 同时存在时,别名语义保持原样。
func TestParseUserConfig_ProviderAliasesCoexistWithQueryStates(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "valid query",
			content: `[provider_aliases]
"OpenCode-Completions" = "OpenCode-Display"
[query.subqueries]
mpc = "model,provider"
`,
		},
		{
			name: "conflicting query variants",
			content: `[provider_aliases]
"OpenCode-Completions" = "OpenCode-Display"
[query]
default = "a"
[Query]
default = "b"
`,
		},
		{
			name: "non-table query root",
			content: `[provider_aliases]
"OpenCode-Completions" = "OpenCode-Display"
query = "x"
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseUserConfig([]byte(tt.content))
			if err != nil {
				t.Fatalf("ParseUserConfig: %v", err)
			}
			if got := cfg.ProviderAliases["OpenCode-Completions"]; got != "OpenCode-Display" {
				t.Errorf("alias key 大小写被改写: %#v", cfg.ProviderAliases)
			}
			if _, ok := cfg.ProviderAliases["opencode-completions"]; ok {
				t.Errorf("alias key was lowercased: %#v", cfg.ProviderAliases)
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
