package runtimecfg

import (
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// fakeProvider 用于注入测试：记录调用参数 + 可控的默认路径行为。
type fakeProvider struct {
	appliedHome string
	appliedGoos string
	calls       int
}

func (f *fakeProvider) ApplyDefaults(cfg *config.Config, home, goos string) error {
	f.calls++
	f.appliedHome = home
	f.appliedGoos = goos
	// fake 不补任何默认路径，只验证 provider 被调用且 home/goos 透传正确。
	return nil
}

func envForTest(provider DefaultPathProvider) ResolveEnv {
	return ResolveEnv{Home: "/tmp/fake-home", GOOS: "linux", DefaultPaths: provider}
}

// ---- ValidateUserConfig ----

func TestValidateUserConfig_RegisteredClients(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("注册 client 不应报错: %v", err)
	}
}

func TestValidateUserConfig_UnknownClient(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"foobar": {Enabled: true}}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("未注册 client 应报错")
	}
}

func TestValidateUserConfig_UnknownRouter(t *testing.T) {
	cfg := &config.Config{Routers: map[string]config.RouterConfig{"bogus_router": {}}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("未注册 router 应报错")
	}
}

func TestValidateUserConfig_ClientDeclaresUnknownRouter(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "bogus_router"}}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("client 声明未注册 router 应报错")
	}
}

func TestValidateUserConfig_UnknownClientPathKey(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Paths: map[string]string{"bogus_key": "/x"}}}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("未注册 client path key 应报错")
	}
}

func TestValidateUserConfig_UnknownRouterPathKey(t *testing.T) {
	cfg := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/x"}}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("cc_switch.db_path 是注册 key，不应报错: %v", err)
	}
	// 注入非法字段：用 raw map 绕过结构体只能设 DBPath 的限制，构造一个含未知 key 的场景
	// 通过添加未注册 router 配合非法 key 验证（结构体 RouterConfig 只有 DBPath，未知 key 无法直接构造，
	// 因此未注册 router path key 的覆盖改由 router 名校验承担；此处保留 db_path 正例）。
}

func TestValidateUserConfig_BadLogLevel(t *testing.T) {
	cfg := &config.Config{Log: config.LogConfig{Level: "verbose"}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("未注册 log level 应报错")
	}
}

func TestValidateUserConfig_EmptyLogLevelOK(t *testing.T) {
	cfg := &config.Config{Log: config.LogConfig{Level: ""}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("空 log level 合法（运行时补 info），不应报错: %v", err)
	}
}

// ---- ValidateUserConfig 数值校验 ----

func TestValidateUserConfig_NegativePollIntervalRejected(t *testing.T) {
	cfg := &config.Config{Daemon: config.DaemonConfig{PollInterval: -5}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("负数 poll_interval 应报错")
	}
}

func TestValidateUserConfig_ZeroPollIntervalOK(t *testing.T) {
	cfg := &config.Config{Daemon: config.DaemonConfig{PollInterval: 0}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("0 poll_interval 表示使用默认值，不应报错: %v", err)
	}
}

func TestValidateUserConfig_PositivePollIntervalOK(t *testing.T) {
	cfg := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("正数 poll_interval 不应报错: %v", err)
	}
}

func TestValidateUserConfig_NegativeMaxDaysRejected(t *testing.T) {
	cfg := &config.Config{Log: config.LogConfig{MaxDays: -3}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("负数 log.max_days 应报错")
	}
}

func TestValidateUserConfig_ZeroMaxDaysOK(t *testing.T) {
	cfg := &config.Config{Log: config.LogConfig{MaxDays: 0}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("0 log.max_days 表示使用默认值，不应报错: %v", err)
	}
}

// ---- ValidateUserConfig alias 校验 ----

func TestValidateUserConfig_AliasEmptyKeyRejected(t *testing.T) {
	cfg := &config.Config{ProviderAliases: map[string]string{"": "Anthropic"}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("alias 空 key 应报错")
	}
}

func TestValidateUserConfig_AliasEmptyValueRejected(t *testing.T) {
	cfg := &config.Config{ProviderAliases: map[string]string{"anthropic": ""}}
	if err := ValidateUserConfig(cfg); err == nil {
		t.Fatal("alias 空 value 应报错")
	}
}

func TestValidateUserConfig_AliasValidOK(t *testing.T) {
	cfg := &config.Config{ProviderAliases: map[string]string{"anthropic": "Anthropic"}}
	if err := ValidateUserConfig(cfg); err != nil {
		t.Errorf("非空 key/value 的 alias 不应报错: %v", err)
	}
}

// ---- ResolveEffectiveConfig 在唯一解析边界校验负数 ----

// TestResolveEffectiveConfig_RejectsNegativePoll 直连调用方也不能绕过用户层校验，
// 负数 poll_interval 必须在解析有效配置时返回错误，不能静默改成默认值。
func TestResolveEffectiveConfig_RejectsNegativePoll(t *testing.T) {
	user := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: -5}}
	_, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err == nil {
		t.Fatal("ResolveEffectiveConfig 应拒绝负数 poll_interval")
	}
}

// TestResolveEffectiveConfig_ZeroPollClampsToDefault 0 仍按默认值 30 处理
// （0 表示「使用默认」，不属于被拒绝的负数）。
func TestResolveEffectiveConfig_ZeroPollClampsToDefault(t *testing.T) {
	user := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: 0}}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.Daemon.PollInterval != 30 {
		t.Errorf("PollInterval=0 应解析为默认值 30, got %d", eff.Daemon.PollInterval)
	}
}

// TestLoadEffectiveConfig_NegativePollRejectedBeforeResolve pipeline 中 ValidateUserConfig 先于
// ResolveEffectiveConfig 执行，负数 poll_interval 在解析前被拒绝，不会进入解析。
func TestLoadEffectiveConfig_NegativePollRejectedBeforeResolve(t *testing.T) {
	content := []byte("data_dir = \"/data\"\ndaemon.poll_interval = -5\n")
	p := writeConfigBytes(t, content)
	_, err := LoadEffectiveConfig(p, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err == nil {
		t.Fatal("负数 poll_interval 应被 pipeline 在解析前拒绝")
	}
}

// ---- ResolveEffectiveConfig ----

// TestResolveEffectiveConfig_DeepCopyDoesNotMutateUser resolver 深拷贝用户层，
// 用户层对象不被修改（~ 不被展开、默认路径不被回填）。
func TestResolveEffectiveConfig_DeepCopyDoesNotMutateUser(t *testing.T) {
	user := &config.Config{
		DataDir: "~/.token-usage",
		Clients: map[string]config.Client{"codex": {Enabled: true}},
	}
	orig := *user
	codexOrig := user.Clients["codex"]

	fp := &fakeProvider{}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: fp})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	// 用户层不变
	if user.DataDir != orig.DataDir {
		t.Errorf("用户层 DataDir 被修改: %q（原 %q）", user.DataDir, orig.DataDir)
	}
	if user.Clients["codex"].Enabled != codexOrig.Enabled {
		t.Errorf("用户层 client 被修改")
	}
	// 有效层展开了 ~
	if eff.DataDir != "/h/.token-usage" {
		t.Errorf("有效层 DataDir 未展开 ~: %q", eff.DataDir)
	}
	// provider 被传入正确 home/goos
	if fp.appliedHome != "/h" || fp.appliedGoos != "linux" {
		t.Errorf("provider 收到 home=%q goos=%q，want /h, linux", fp.appliedHome, fp.appliedGoos)
	}
	if fp.calls != 1 {
		t.Errorf("provider 应被调用 1 次，实际 %d", fp.calls)
	}
}

// TestResolveEffectiveConfig_TildeExpansion ~ 在 DataDir / Log.Dir / client paths / router db_path 展开。
func TestResolveEffectiveConfig_TildeExpansion(t *testing.T) {
	user := &config.Config{
		DataDir: "~/.token-usage",
		Log:     config.LogConfig{Dir: "~/logs"},
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "~/projects"}},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: "~/cc.db"},
		},
	}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.DataDir != "/h/.token-usage" {
		t.Errorf("DataDir = %q", eff.DataDir)
	}
	if eff.Log.Dir != "/h/logs" {
		t.Errorf("Log.Dir = %q", eff.Log.Dir)
	}
	if eff.Clients["claude"].Paths["projects_dir"] != "/h/projects" {
		t.Errorf("claude.projects_dir = %q", eff.Clients["claude"].Paths["projects_dir"])
	}
	if eff.Routers["cc_switch"].DBPath != "/h/cc.db" {
		t.Errorf("cc_switch.db_path = %q", eff.Routers["cc_switch"].DBPath)
	}
}

// TestResolveEffectiveConfig_RelativePathUnchanged 相对路径（非 ~）保持原样。
func TestResolveEffectiveConfig_RelativePathUnchanged(t *testing.T) {
	user := &config.Config{
		DataDir: "relative/data",
	}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.DataDir != "relative/data" {
		t.Errorf("相对路径应保持不变: %q", eff.DataDir)
	}
}

// TestResolveEffectiveConfig_CoreDefaultsApplied 核心默认值（poll<1→30、log level→info、log dir、max_days）。
func TestResolveEffectiveConfig_CoreDefaultsApplied(t *testing.T) {
	user := &config.Config{DataDir: "/data"}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.Daemon.PollInterval != 30 {
		t.Errorf("PollInterval = %d, want 30", eff.Daemon.PollInterval)
	}
	if eff.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", eff.Log.Level)
	}
	if eff.Log.Dir != "/data/logs" {
		t.Errorf("Log.Dir = %q, want /data/logs（派生自 DataDir）", eff.Log.Dir)
	}
	if eff.Log.MaxDays != 7 {
		t.Errorf("Log.MaxDays = %d, want 7", eff.Log.MaxDays)
	}
}

// TestResolveEffectiveConfig_NilDefaultPathsReturnsError nil DefaultPaths 返回稳定错误，
// 而非 panic 或静默跳过。
func TestResolveEffectiveConfig_NilDefaultPathsReturnsError(t *testing.T) {
	user := &config.Config{DataDir: "/x"}
	_, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: nil})
	if err == nil {
		t.Fatal("nil DefaultPaths 应返回错误")
	}
}

// TestResolveEffectiveConfig_EmptyHomeRejected 空 home 无法确定默认路径与 ~ 展开基准，
// 必须在解析边界返回稳定错误。
func TestResolveEffectiveConfig_EmptyHomeRejected(t *testing.T) {
	user := &config.Config{DataDir: "~/x"}
	_, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err == nil {
		t.Fatal("ResolveEffectiveConfig 应拒绝空 Home")
	}
}

// TestResolveEffectiveConfig_FillsDefaultsAndCodexSessions 端到端：标准 provider 补默认 +
// codex sessions_dir 派生 + client 声明 router 补 entry。
func TestResolveEffectiveConfig_FillsDefaultsAndCodexSessions(t *testing.T) {
	const home = "/tmp/fake-home"
	user := &config.Config{
		DataDir: "/data",
		Clients: map[string]config.Client{
			"codex":  {Enabled: true},
			"claude": {Enabled: true, Router: "cc_switch"},
		},
	}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: home, GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if got := eff.Clients["codex"].Paths["state_dir"]; got != filepath.Join(home, ".codex") {
		t.Errorf("codex state_dir = %q", got)
	}
	if got := eff.Clients["codex"].Paths["sessions_dir"]; got != filepath.Join(home, ".codex", "sessions") {
		t.Errorf("codex sessions_dir = %q", got)
	}
	if got := eff.Routers["cc_switch"].DBPath; got != filepath.Join(home, ".cc-switch", "cc-switch.db") {
		t.Errorf("cc_switch db_path = %q", got)
	}
}

// TestResolveEffectiveConfig_PreservesExplicitValues 显式配置优先于默认。
func TestResolveEffectiveConfig_PreservesExplicitValues(t *testing.T) {
	user := &config.Config{
		DataDir: "/data",
		Daemon:  config.DaemonConfig{PollInterval: 10},
		Log:     config.LogConfig{Level: "debug", MaxDays: 14},
	}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.Daemon.PollInterval != 10 {
		t.Errorf("显式 PollInterval 被覆盖: %d", eff.Daemon.PollInterval)
	}
	if eff.Log.Level != "debug" {
		t.Errorf("显式 Log.Level 被覆盖: %q", eff.Log.Level)
	}
	if eff.Log.MaxDays != 14 {
		t.Errorf("显式 MaxDays 被覆盖: %d", eff.Log.MaxDays)
	}
}

// TestResolveEffectiveConfig_RawMarshalNotBackfilled 解析后 marshal 用户层不应回填默认路径。
// （间接：user 对象保持原值；本测试断言 user.DataDir 仍为 ~ 字面值。）
func TestResolveEffectiveConfig_RawMarshalNotBackfilled(t *testing.T) {
	user := &config.Config{DataDir: "~/.token-usage", Clients: map[string]config.Client{"codex": {Enabled: true}}}
	_, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if user.DataDir != "~/.token-usage" {
		t.Errorf("用户层 DataDir 被回填默认值: %q（应保持 ~）", user.DataDir)
	}
	if len(user.Clients["codex"].Paths) != 0 {
		t.Errorf("用户层 codex paths 被回填默认: %v", user.Clients["codex"].Paths)
	}
}

// ---- LoadEffectiveConfig ----

// TestLoadEffectiveConfig_Pipeline 固定 pipeline：
// LoadUserConfigSnapshot → ValidateUserConfig → ResolveEffectiveConfig。
func TestLoadEffectiveConfig_Pipeline(t *testing.T) {
	content := []byte("data_dir = \"/data\"\n[clients.codex]\nenabled = true\n")
	p := writeConfigBytes(t, content)

	eff, err := LoadEffectiveConfig(p, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("LoadEffectiveConfig: %v", err)
	}
	if eff.DataDir != "/data" {
		t.Errorf("DataDir = %q", eff.DataDir)
	}
	if eff.Daemon.PollInterval != 30 {
		t.Errorf("PollInterval 默认值 = %d, want 30", eff.Daemon.PollInterval)
	}
	if got := eff.Clients["codex"].Paths["state_dir"]; got != "/h/.codex" {
		t.Errorf("codex state_dir = %q, want /h/.codex（标准 provider 默认）", got)
	}
}

// TestLoadEffectiveConfig_UnknownClientRejected pipeline 在应用默认值前调 ValidateUserConfig，
// 未注册 client 被拒绝。
func TestLoadEffectiveConfig_UnknownClientRejected(t *testing.T) {
	content := []byte("data_dir = \"/data\"\n[clients.foobar]\nenabled = true\n")
	p := writeConfigBytes(t, content)

	_, err := LoadEffectiveConfig(p, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err == nil {
		t.Fatal("未注册 client 应被 LoadEffectiveConfig 拒绝")
	}
}

// TestLoadEffectiveConfig_MissingFileExistsFalse 文件缺失 → Exists=false，按错误返回（不创建半份配置）。
func TestLoadEffectiveConfig_MissingFileReturnsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.toml")
	_, err := LoadEffectiveConfig(p, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err == nil {
		t.Fatal("文件缺失应返回错误（不得返回半份默认配置）")
	}
}

// TestLoadEffectiveConfig_EmptyEnvReturnsError 空 home/goos 仍能解析（home 影响展开结果，非错误条件），
// 但 nil DefaultPaths 返回错误（与 ResolveEffectiveConfig 一致）。
func TestLoadEffectiveConfig_NilDefaultPathsReturnsError(t *testing.T) {
	content := []byte("data_dir = \"/data\"\n")
	p := writeConfigBytes(t, content)
	_, err := LoadEffectiveConfig(p, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: nil})
	if err == nil {
		t.Fatal("nil DefaultPaths 应返回错误")
	}
}

// ValidateUserConfig（读取链）容忍非 Claude 家族的非空 router：存量配置
// 不应让 show/collect/daemon 等读路径失败（行为同旧版：只写原始日志不回填）。
func TestValidateUserConfig_ReadToleratesNonCapableRouter(t *testing.T) {
	user := &config.Config{Clients: map[string]config.Client{
		"opencode": {Enabled: true, Router: "cc_switch"},
	}}
	if err := ValidateUserConfig(user); err != nil {
		t.Errorf("读取链应容忍非 Claude 家族 router, got %v", err)
	}
}

// ValidateUserConfigForWrite（写入链）拒绝非 Claude 家族的非空 router；
// 空值（清除）与 Claude 家族的非空值放行。
func TestValidateUserConfigForWrite_RejectsNonCapableRouter(t *testing.T) {
	bad := &config.Config{Clients: map[string]config.Client{
		"opencode": {Enabled: true, Router: "cc_switch"},
	}}
	if err := ValidateUserConfigForWrite(bad); err == nil {
		t.Error("写入链应拒绝 opencode 的非空 router")
	}

	cleared := &config.Config{Clients: map[string]config.Client{
		"opencode": {Enabled: true, Router: ""},
	}}
	if err := ValidateUserConfigForWrite(cleared); err != nil {
		t.Errorf("写入链应放行空 router(清除), got %v", err)
	}

	ok := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Router: "cc_switch"},
	}}
	if err := ValidateUserConfigForWrite(ok); err != nil {
		t.Errorf("写入链应放行 claude 的 router, got %v", err)
	}
}

// ClientSupportsRouter 仅 Claude 家族为真（CC Switch 的 app_type 只识别 Claude）。
func TestClientSupportsRouter_OnlyClaude(t *testing.T) {
	if !ClientSupportsRouter("claude") {
		t.Error("claude 应支持 router")
	}
	for _, name := range []string{"opencode", "codex", "workbuddy", "zcode", "autoclaw"} {
		if ClientSupportsRouter(name) {
			t.Errorf("client %q 不应支持 router", name)
		}
	}
}

// raw query 状态随有效化深拷贝传播:effective 层与用户层不共享嵌套 map/slice 引用。
func TestResolveEffectiveConfig_RawQueryDeepCopyPropagation(t *testing.T) {
	user := &config.Config{
		DataDir:  "/d",
		RawQuery: map[string]any{"sub": map[string]any{"list": []any{int64(1)}}},
		RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
			"Query": {Name: "Query", Value: map[string]any{"k": []any{"v"}}, Kind: config.RawQueryIssueNameConflict},
		},
	}
	eff, err := ResolveEffectiveConfig(user, ResolveEnv{Home: "/h", GOOS: "linux", DefaultPaths: newStandardProvider()})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig: %v", err)
	}
	if eff.RawQuery == nil || eff.RawQueryTopLevelIssues == nil {
		t.Fatal("raw query 状态应传播到 effective 层")
	}
	user.RawQuery["sub"].(map[string]any)["list"].([]any)[0] = int64(9)
	if got := eff.RawQuery["sub"].(map[string]any)["list"].([]any)[0]; got != int64(1) {
		t.Errorf("RawQuery 深层 slice 共享引用: got %v", got)
	}
	user.RawQueryTopLevelIssues["Query"].Value.(map[string]any)["k"].([]any)[0] = "mutated"
	if got := eff.RawQueryTopLevelIssues["Query"].Value.(map[string]any)["k"].([]any)[0]; got != "v" {
		t.Errorf("issues 深层 slice 共享引用: got %v", got)
	}
	eff.RawQuery["sub"].(map[string]any)["new"] = "x"
	if _, ok := user.RawQuery["sub"].(map[string]any)["new"]; ok {
		t.Error("effective 侧写入泄漏到用户层")
	}
}
