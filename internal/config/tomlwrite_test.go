package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// round-trip 语义比对(不比文本)
func TestWriteUserConfigAtomic_RoundTrip(t *testing.T) {
	src := &Config{
		DataDir: "~/.token-usage",
		Clients: map[string]Client{
			"codex":  {Enabled: true, Paths: map[string]string{"db": "~/.codex/x"}},
			"claude": {Enabled: false},
		},
		Daemon: DaemonConfig{PollInterval: 15},
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteUserConfigAtomic(path, src); err != nil {
		t.Fatalf("WriteUserConfigAtomic: %v", err)
	}
	got, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.DataDir != src.DataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, src.DataDir)
	}
	if got.Daemon.PollInterval != 15 {
		t.Errorf("PollInterval = %d, want 15", got.Daemon.PollInterval)
	}
	if !got.Clients["codex"].Enabled {
		t.Error("codex.Enabled 应为 true")
	}
	if got.Clients["codex"].Paths["db"] != "~/.codex/x" {
		t.Errorf("codex Paths.db = %q", got.Clients["codex"].Paths["db"])
	}
	// claude enabled=false:Client.Enabled 无 omitempty → 段保留
	c, ok := got.Clients["claude"]
	if !ok {
		t.Fatal("claude 应存在(Enabled 无 omitempty 保留禁用 client)")
	}
	if c.Enabled {
		t.Error("claude.Enabled 应为 false")
	}
}

// 覆盖写 + tmp 清理
func TestWriteUserConfigAtomic_OverwriteAndCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteUserConfigAtomic(path, &Config{DataDir: "/orig"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := WriteUserConfigAtomic(path, &Config{DataDir: "/over", Daemon: DaemonConfig{PollInterval: 99}}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ := LoadUserConfig(path)
	if got.DataDir != "/over" || got.Daemon.PollInterval != 99 {
		t.Errorf("覆盖后 = %+v", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".config.toml-*"))
	if len(matches) != 0 {
		t.Errorf("tmp 应清理,残留: %v", matches)
	}
}

// ~ 字面保留
func TestWriteUserConfigAtomic_TildePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteUserConfigAtomic(path, &Config{DataDir: "~/.token-usage"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "~/.token-usage") {
		t.Errorf("文件应含 ~ 字面: %s", string(raw))
	}
}

func TestMarshalConfig_ProviderAliasKeysAlwaysDoubleQuoted(t *testing.T) {
	src := &Config{ProviderAliases: map[string]string{
		"BigModel - Coding Plan": "Zhipu GLM",
		"OpenCode-Completions":   "OpenCode",
		`Provider "Special"`:     `Display "Name"`,
	}}
	data, err := MarshalConfig(src)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`"BigModel - Coding Plan" = "Zhipu GLM"`,
		`"OpenCode-Completions" = "OpenCode"`,
		`"Provider \"Special\"" = "Display \"Name\""`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("provider alias 应统一使用双引号，缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "OpenCode-Completions =") {
		t.Errorf("provider alias key 不应输出为 bare key:\n%s", text)
	}

	var got Config
	if err := toml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ProviderAliases, src.ProviderAliases) {
		t.Errorf("provider aliases round trip = %#v, want %#v", got.ProviderAliases, src.ProviderAliases)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteUserConfigAtomic(path, src); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUserConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.ProviderAliases, src.ProviderAliases) {
		t.Errorf("loaded provider aliases = %#v, want %#v", loaded.ProviderAliases, src.ProviderAliases)
	}
}

// sampleConfig 构造一个含多 client/router/alias map 的非空 *Config,供 MarshalConfig 系列测试复用。
func sampleConfig() *Config {
	return &Config{
		DataDir: "~/.token-usage",
		Clients: map[string]Client{
			"codex":  {Enabled: true, Router: "cc_switch", Paths: map[string]string{"db": "~/.codex/x"}},
			"claude": {Enabled: false},
		},
		Routers: map[string]RouterConfig{
			"cc_switch": {DBPath: "~/.token-usage/router.db"},
		},
		Daemon:          DaemonConfig{PollInterval: 30, AutoStart: true},
		Log:             LogConfig{Level: "info", Dir: "~/.token-usage/log", MaxDays: 7},
		ProviderAliases: map[string]string{"codex": "codex", "claude": "claude"},
	}
}

// 1) MarshalConfig(nil) 返回错误且信息含「配置不能为 nil」。
func TestMarshalConfig_NilError(t *testing.T) {
	out, err := MarshalConfig(nil)
	if err == nil {
		t.Fatal("MarshalConfig(nil) 应返回错误")
	}
	if !strings.Contains(err.Error(), "配置不能为 nil") {
		t.Errorf("错误信息应含「配置不能为 nil」, got: %v", err)
	}
	if out != nil {
		t.Errorf("MarshalConfig(nil) 输出应为 nil, got: %v", out)
	}
}

// 2) round-trip:MarshalConfig 输出可被 toml.Unmarshal 还原为等价 Config。
func TestMarshalConfig_RoundTrip(t *testing.T) {
	src := sampleConfig()
	out, err := MarshalConfig(src)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	var got Config
	if err := toml.Unmarshal(out, &got); err != nil {
		t.Fatalf("toml.Unmarshal: %v", err)
	}
	if got.DataDir != src.DataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, src.DataDir)
	}
	if got.Daemon.PollInterval != src.Daemon.PollInterval {
		t.Errorf("Daemon.PollInterval = %d, want %d", got.Daemon.PollInterval, src.Daemon.PollInterval)
	}
	if got.Log.Level != src.Log.Level {
		t.Errorf("Log.Level = %q, want %q", got.Log.Level, src.Log.Level)
	}
	if got.Log.MaxDays != src.Log.MaxDays {
		t.Errorf("Log.MaxDays = %d, want %d", got.Log.MaxDays, src.Log.MaxDays)
	}
	if len(got.Clients) != len(src.Clients) {
		t.Fatalf("Clients 数量 = %d, want %d", len(got.Clients), len(src.Clients))
	}
	codex, ok := got.Clients["codex"]
	if !ok {
		t.Fatal("Clients.codex 应存在")
	}
	if !codex.Enabled || codex.Router != "cc_switch" || codex.Paths["db"] != "~/.codex/x" {
		t.Errorf("Clients.codex = %+v", codex)
	}
	if got.Routers["cc_switch"].DBPath != src.Routers["cc_switch"].DBPath {
		t.Errorf("Routers.cc_switch.DBPath = %q", got.Routers["cc_switch"].DBPath)
	}
	if got.ProviderAliases["claude"] != src.ProviderAliases["claude"] {
		t.Errorf("ProviderAliases.claude = %q", got.ProviderAliases["claude"])
	}
}

// 3) 核心防回归:MarshalUserConfig 与 MarshalConfig 对同一输入字节完全一致。
func TestMarshalConfig_SameBytesAsMarshalUserConfig(t *testing.T) {
	cfg := sampleConfig()
	a, err := MarshalUserConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalUserConfig: %v", err)
	}
	b, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("MarshalUserConfig 与 MarshalConfig 输出应字节一致\nUserConfig:\n%s\nConfig:\n%s", a, b)
	}
}

// 4) 同一输入重复 marshal 两次,输出字节稳定相等。
func TestMarshalConfig_StableAcrossCalls(t *testing.T) {
	cfg := sampleConfig()
	first, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("first MarshalConfig: %v", err)
	}
	second, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("second MarshalConfig: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("重复 marshal 输出不稳定\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// 5) 非零 daemon/log 字段不被 omitempty 意外省略。
func TestMarshalConfig_NonZeroFieldsRetained(t *testing.T) {
	src := sampleConfig()
	out, err := MarshalConfig(src)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	var got Config
	if err := toml.Unmarshal(out, &got); err != nil {
		t.Fatalf("toml.Unmarshal: %v", err)
	}
	if got.Daemon.PollInterval != 30 {
		t.Errorf("PollInterval 应保留为 30, got %d", got.Daemon.PollInterval)
	}
	if got.Daemon.AutoStart != true {
		t.Errorf("AutoStart 应保留为 true, got %v", got.Daemon.AutoStart)
	}
	if got.Log.MaxDays != 7 {
		t.Errorf("MaxDays 应保留为 7, got %d", got.Log.MaxDays)
	}
	if got.Log.Level != "info" {
		t.Errorf("Level 应保留为 info, got %q", got.Log.Level)
	}
}
