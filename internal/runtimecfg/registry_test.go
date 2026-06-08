package runtimecfg

import (
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// TestRegisteredClients_FixedContent registry 固定包含 6 个 client。
// 顺序无关，只校验集合一致。
func TestRegisteredClients_FixedContent(t *testing.T) {
	got := RegisteredClients()
	want := map[string]bool{"claude": true, "codex": true, "opencode": true, "workbuddy": true, "zcode": true, "autoclaw": true}
	if len(got) != len(want) {
		t.Fatalf("RegisteredClients 返回 %d 个，want %d 个 (%v)", len(got), len(want), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("未预期的 client %q", c)
		}
	}
}

func TestRegisteredRouters_FixedContent(t *testing.T) {
	got := RegisteredRouters()
	if len(got) != 1 || got[0] != "cc_switch" {
		t.Errorf("RegisteredRouters = %v, want [cc_switch]", got)
	}
}

func TestRegisteredLogLevels_IncludesDefaults(t *testing.T) {
	got := RegisteredLogLevels()
	// 用户层空值视为 default（不强制写 level），运行时 default→info。
	want := map[string]bool{"default": true, "info": true, "debug": true, "warn": true, "error": true}
	for _, lv := range got {
		if !want[lv] {
			t.Errorf("未预期的 log level %q", lv)
		}
	}
	for k := range want {
		found := false
		for _, lv := range got {
			if lv == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("RegisteredLogLevels 缺少 %q（实际 %v）", k, got)
		}
	}
}

// TestRegisteredClientPathKeys_FixedContent 固定 path key。
func TestRegisteredClientPathKeys_FixedContent(t *testing.T) {
	tests := []struct {
		client string
		want   []string
	}{
		{"claude", []string{"projects_dir"}},
		{"codex", []string{"state_dir", "sessions_dir"}},
		{"opencode", []string{"db"}},
		{"workbuddy", []string{"db", "projects_dir"}},
		{"zcode", []string{"db"}},
		{"autoclaw", []string{"sessions_dir"}},
	}
	for _, tt := range tests {
		t.Run(tt.client, func(t *testing.T) {
			got := RegisteredClientPathKeys(tt.client)
			if !sameSet(got, tt.want) {
				t.Errorf("RegisteredClientPathKeys(%q) = %v, want %v", tt.client, got, tt.want)
			}
		})
	}
}

func TestRegisteredRouterPathKeys_CCswitch(t *testing.T) {
	got := RegisteredRouterPathKeys("cc_switch")
	if !sameSet(got, []string{"db_path"}) {
		t.Errorf("RegisteredRouterPathKeys(cc_switch) = %v, want [db_path]", got)
	}
}

// TestRegisteredClientPathKeys_UnknownClientReturnsNil 未注册 client 返回 nil（非 panic）。
func TestRegisteredClientPathKeys_UnknownClientReturnsNil(t *testing.T) {
	if got := RegisteredClientPathKeys("unknown"); got != nil {
		t.Errorf("未注册 client 应返回 nil，实际 %v", got)
	}
}

// TestRegistry_ReturnsCopy registry 返回副本，调用方修改不污染全局。
// 通过修改返回 slice 并再次调用验证（注册表内部数据未被影响）。
func TestRegistry_ReturnsCopy(t *testing.T) {
	clients := RegisteredClients()
	if len(clients) == 0 {
		t.Fatal("预期非空 clients")
	}
	clients[0] = "tampered"
	again := RegisteredClients()
	for _, c := range again {
		if c == "tampered" {
			t.Fatal("调用方修改返回 slice 污染了全局 registry")
		}
	}

	keys := RegisteredClientPathKeys("codex")
	if len(keys) == 0 {
		t.Fatal("预期非空 codex path keys")
	}
	keys[0] = "tampered"
	againKeys := RegisteredClientPathKeys("codex")
	for _, k := range againKeys {
		if k == "tampered" {
			t.Fatal("调用方修改 path keys slice 污染了全局 registry")
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(b))
	for _, x := range b {
		m[x] = true
	}
	for _, x := range a {
		if !m[x] {
			return false
		}
	}
	return true
}

// ---- DefaultPathProvider（standardDefaultPaths）----

// runProvider 调用标准 provider 并返回已应用默认路径的 cfg（便于断言）。
func runProvider(t *testing.T, cfg *config.Config, home, goos string) *config.Config {
	t.Helper()
	// initMaps 由 provider 内部保证 Paths 非 nil，这里直接调 ApplyDefaults。
	if err := newStandardProvider().ApplyDefaults(cfg, home, goos); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	return cfg
}

// TestStandardProvider_FillsAllClientDefaults 固定 Home/GOOS 时默认路径确定，
// 不受测试机影响（provider 只用入参 home，不读 os.UserHomeDir）。
func TestStandardProvider_FillsAllClientDefaults(t *testing.T) {
	const home = "/tmp/fake-home"
	const goos = "linux"
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":    {Enabled: true},
			"codex":     {Enabled: true},
			"opencode":  {Enabled: true},
			"workbuddy": {Enabled: true},
			"zcode":     {Enabled: true},
			"autoclaw":  {Enabled: true},
		},
	}
	runProvider(t, cfg, home, goos)

	tests := []struct {
		client, key, suffix string
	}{
		{"claude", "projects_dir", "/.claude/projects"},
		{"codex", "state_dir", "/.codex"},
		{"codex", "sessions_dir", "/.codex/sessions"},
		{"opencode", "db", "/.local/share/opencode/opencode.db"},
		{"workbuddy", "projects_dir", "/.workbuddy/projects"},
		{"workbuddy", "db", "/.workbuddy/workbuddy.db"},
		{"zcode", "db", "/.zcode/cli/db/db.sqlite"},
		{"autoclaw", "sessions_dir", "/.openclaw-autoclaw/agents"},
	}
	for _, tt := range tests {
		got := cfg.Clients[tt.client].Paths[tt.key]
		want := filepath.Join(home, tt.suffix)
		if got != want {
			t.Errorf("%s.%s = %q, want %q", tt.client, tt.key, got, want)
		}
	}
}

// TestStandardProvider_PreservesExplicitPaths 用户显式配置优先保留。
func TestStandardProvider_PreservesExplicitPaths(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/custom/claude"}},
			"codex":  {Enabled: true, Paths: map[string]string{"state_dir": "/custom/codex"}},
		},
	}
	runProvider(t, cfg, "/tmp/h", "linux")

	if got := cfg.Clients["claude"].Paths["projects_dir"]; got != "/custom/claude" {
		t.Errorf("显式 claude.projects_dir 被覆盖: %q", got)
	}
	if got := cfg.Clients["codex"].Paths["state_dir"]; got != "/custom/codex" {
		t.Errorf("显式 codex.state_dir 被覆盖: %q", got)
	}
	// codex sessions_dir 未显式配置，应基于显式 state_dir 派生。
	if got := cfg.Clients["codex"].Paths["sessions_dir"]; got != "/custom/codex/sessions" {
		t.Errorf("codex sessions_dir = %q, want /custom/codex/sessions（派生自显式 state_dir）", got)
	}
}

// TestStandardProvider_CodexSessionsDerivedFromResolvedState codex sessions_dir
// 必须派生自 resolved state_dir（即应用默认后的 state_dir），而非 raw 默认。
func TestStandardProvider_CodexSessionsDerivedFromResolvedState(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true}}}
	runProvider(t, cfg, "/h", "linux")
	state := cfg.Clients["codex"].Paths["state_dir"]
	if cfg.Clients["codex"].Paths["sessions_dir"] != filepath.Join(state, "sessions") {
		t.Errorf("sessions_dir 应 = state_dir/sessions，state=%q sessions=%q",
			state, cfg.Clients["codex"].Paths["sessions_dir"])
	}
}

// TestStandardProvider_RouterDefault cc_switch 默认 db_path。
func TestStandardProvider_RouterDefault(t *testing.T) {
	cfg := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {}}}
	runProvider(t, cfg, "/tmp/h", "linux")
	want := filepath.Join("/tmp/h", ".cc-switch", "cc-switch.db")
	if got := cfg.Routers["cc_switch"].DBPath; got != want {
		t.Errorf("cc_switch db_path = %q, want %q", got, want)
	}
}

// TestStandardProvider_PreservesExplicitRouterPath 显式 router db_path 保留。
func TestStandardProvider_PreservesExplicitRouterPath(t *testing.T) {
	cfg := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/custom/cc.db"}}}
	runProvider(t, cfg, "/tmp/h", "linux")
	if got := cfg.Routers["cc_switch"].DBPath; got != "/custom/cc.db" {
		t.Errorf("显式 db_path 被覆盖: %q", got)
	}
}

// TestStandardProvider_CreatesRouterForClientDeclaration client 声明 router 但 routers 表缺失时补默认 entry。
func TestStandardProvider_CreatesRouterForClientDeclaration(t *testing.T) {
	const home = "/tmp/h"
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
	}
	runProvider(t, cfg, home, "linux")
	got, ok := cfg.Routers["cc_switch"]
	if !ok {
		t.Fatal("client 声明 cc_switch 但 routers 表缺失时应创建默认 entry")
	}
	want := filepath.Join(home, ".cc-switch", "cc-switch.db")
	if got.DBPath != want {
		t.Errorf("创建的 cc_switch db_path = %q, want %q", got.DBPath, want)
	}
}

// TestStandardProvider_OpenCode_OSIndependent opencode 多 OS 地址一致（xdg-basedir，无 APPDATA 分支）。
func TestStandardProvider_OpenCode_OSIndependent(t *testing.T) {
	want := filepath.Join("/h", ".local", "share", "opencode", "opencode.db")
	for _, goos := range []string{"linux", "darwin", "windows"} {
		cfg := &config.Config{Clients: map[string]config.Client{"opencode": {Enabled: true}}}
		runProvider(t, cfg, "/h", goos)
		if got := cfg.Clients["opencode"].Paths["db"]; got != want {
			t.Errorf("opencode db(goos=%s) = %q, want %q", goos, got, want)
		}
	}
}

// TestStandardProvider_NilClientsMap_NoPanic 整个 [clients] 段缺失时不 panic。
func TestStandardProvider_NilClientsMap_NoPanic(t *testing.T) {
	cfg := &config.Config{}
	runProvider(t, cfg, "/h", "linux") // 不 panic 即通过
}

// TestStandardProvider_RejectsUnknownClient provider 对未注册 client 不填路径但也不 panic；
// registry 校验由 ValidateUserConfig 负责，provider 只补注册项。
func TestStandardProvider_SkipsUnknownClient(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"unknown": {Enabled: true, Paths: map[string]string{}}}}
	runProvider(t, cfg, "/h", "linux") // unknown 被跳过，不 panic
	if _, ok := cfg.Clients["unknown"]; !ok {
		t.Fatal("unknown client 应仍存在（provider 不删除，只跳过补默认）")
	}
}
