package config

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// ---- raw query 序列化 ----

// goldenNoRaw 是 sampleConfig 在引入 raw query 序列化之前的 MarshalConfig 输出快照:
// 无 raw 状态时新实现必须与既有输出字节完全一致(回归保护)。
const goldenNoRaw = `data_dir = '~/.token-usage'

[clients]
[clients.claude]
enabled = false

[clients.codex]
enabled = true
router = 'cc_switch'

[clients.codex.paths]
db = '~/.codex/x'

[routers]
[routers.cc_switch]
db_path = '~/.token-usage/router.db'

[daemon]
poll_interval = 30
autostart = true

[log]
level = 'info'
dir = '~/.token-usage/log'
max_days = 7
[provider_aliases]
"claude" = "claude"
"codex" = "codex"
`

// 无 raw query 状态时 MarshalConfig 输出与既有字节完全一致;
// raw 字段为 nil(而非空 map)才视为未配置。
func TestMarshalConfig_NoRawQueryBytesUnchanged(t *testing.T) {
	got, err := MarshalConfig(sampleConfig())
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	if string(got) != goldenNoRaw {
		t.Errorf("无 raw query 时输出应与既有字节一致\ngot:\n%s\nwant:\n%s", got, goldenNoRaw)
	}
}

// 合法 raw query 的字符串值、键名与表头路径段统一基本双引号输出。
func TestMarshalConfig_RawQueryUsesDoubleQuotedStrings(t *testing.T) {
	cfg := sampleConfig()
	cfg.RawQuery = map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider,client"},
	}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		`["query"]`,
		`"default" = "group_q"`,
		`["query"."subqueries"]`,
		`"mpc" = "model,provider,client"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "'group_q'") || strings.Contains(text, "'model,provider,client'") {
		t.Errorf("query 字符串不得使用单引号:\n%s", text)
	}
}

// 特殊键名(空格、内嵌引号、非 bare 字符)的键与表头路径可解析往返。
func TestMarshalConfig_RawQuerySpecialKeyRoundTrip(t *testing.T) {
	cfg := sampleConfig()
	cfg.RawQuery = map[string]any{
		"a key": map[string]any{
			`inner "q"`: "v",
			"键.名":       "w",
		},
	}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		`["query"."a key"]`,
		"\"inner \\\"q\\\"\" = \"v\"",
		"\"键.名\" = \"w\"",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q:\n%s", want, text)
		}
	}
	var top map[string]any
	if err := toml.Unmarshal(out, &top); err != nil {
		t.Fatalf("特殊键名输出必须仍是合法 TOML: %v\n%s", err, text)
	}
	inner := top["query"].(map[string]any)["a key"].(map[string]any)
	if inner[`inner "q"`] != "v" || inner["键.名"] != "w" {
		t.Errorf("特殊键名 round trip 失败: %#v", inner)
	}
}

// go-toml 解码到 any 的十种类型逐一无损往返;1.0 保持 float、nan/±inf 合法。
func TestMarshalConfig_RawQueryTenValueTypesRoundTrip(t *testing.T) {
	when := time.Date(1979, 5, 27, 7, 32, 0, 0, time.UTC)
	cfg := sampleConfig()
	cfg.RawQuery = map[string]any{
		"str":   "text",
		"int":   int64(-42),
		"flt":   float64(1.0),
		"boolt": true,
		"boolf": false,
		"when":  when,
		"ld":    toml.LocalDate{Year: 1979, Month: 5, Day: 27},
		"ldt":   toml.LocalDateTime{LocalDate: toml.LocalDate{Year: 1979, Month: 5, Day: 27}, LocalTime: toml.LocalTime{Hour: 7, Minute: 32, Second: 0, Nanosecond: 0}},
		"lt":    toml.LocalTime{Hour: 7, Minute: 32, Second: 0, Nanosecond: 0},
		"nan":   math.NaN(),
		"pinf":  math.Inf(1),
		"ninf":  math.Inf(-1),
	}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"flt" = 1.0`) {
		t.Errorf("1.0 必须保持浮点标记:\n%s", text)
	}
	for _, lit := range []string{"nan", "+inf", "-inf"} {
		if !strings.Contains(text, lit) {
			t.Errorf("输出缺少 %q:\n%s", lit, text)
		}
	}
	var top map[string]any
	if err := toml.Unmarshal(out, &top); err != nil {
		t.Fatalf("十类型输出必须合法: %v\n%s", err, text)
	}
	q := top["query"].(map[string]any)
	if q["int"] != int64(-42) {
		t.Errorf("int = %#v", q["int"])
	}
	if q["flt"] != float64(1.0) {
		t.Errorf("flt = %#v", q["flt"])
	}
	if q["str"] != "text" || q["boolt"] != true || q["boolf"] != false {
		t.Errorf("str/bool round trip: %#v", q)
	}
	if got, ok := q["when"].(time.Time); !ok || !got.Equal(when) {
		t.Errorf("when = %#v, want %v", q["when"], when)
	}
	if _, ok := q["ld"].(toml.LocalDate); !ok {
		t.Errorf("ld 类型丢失: %T", q["ld"])
	}
	if _, ok := q["ldt"].(toml.LocalDateTime); !ok {
		t.Errorf("ldt 类型丢失: %T", q["ldt"])
	}
	if _, ok := q["lt"].(toml.LocalTime); !ok {
		t.Errorf("lt 类型丢失: %T", q["lt"])
	}
	if got := q["nan"].(float64); !math.IsNaN(got) {
		t.Errorf("nan = %v", got)
	}
	if got := q["pinf"].(float64); !math.IsInf(got, 1) {
		t.Errorf("pinf = %v", got)
	}
	if got := q["ninf"].(float64); !math.IsInf(got, -1) {
		t.Errorf("ninf = %v", got)
	}
}

// 集合外运行时类型拒绝写盘并报出路径与类型。
func TestMarshalConfig_RawQueryRejectsUnsupportedTypes(t *testing.T) {
	cfg := sampleConfig()
	cfg.RawQuery = map[string]any{"bad": make(chan int)}
	_, err := MarshalConfig(cfg)
	if err == nil {
		t.Fatal("集合外类型应拒绝保存")
	}
	msg := err.Error()
	if !strings.Contains(msg, "query.bad") || !strings.Contains(msg, "chan") {
		t.Errorf("错误应含路径与类型,got %q", msg)
	}

	cfg2 := sampleConfig()
	fn := func() {}
	cfg2.RawQuery = map[string]any{"arr": []any{fn}}
	_, err = MarshalConfig(cfg2)
	if err == nil {
		t.Fatal("数组内集合外类型应拒绝保存")
	}
	if !strings.Contains(err.Error(), "query.arr[0]") {
		t.Errorf("错误应含数组元素路径,got %q", err.Error())
	}
}

// 合法 raw query 的空子表不落盘,空数组保留 key = []。
func TestMarshalConfig_RawQueryEmptyShapes(t *testing.T) {
	cfg := sampleConfig()
	cfg.RawQuery = map[string]any{"arr": []any{}}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	if !strings.Contains(string(out), `"arr" = []`) {
		t.Errorf("空数组必须保留:\n%s", out)
	}

	// 空 map(整个 RawQuery 为空)与仅含空子表:均不产生 query 段。
	for name, raw := range map[string]map[string]any{
		"empty root":  {},
		"empty child": {"subqueries": map[string]any{}},
	} {
		cfg := sampleConfig()
		cfg.RawQuery = raw
		out, err := MarshalConfig(cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), "[query") {
			t.Errorf("%s: 空段不应落盘:\n%s", name, out)
		}
	}

	// 非空父表中混合空子表:空子表跳过,其余保留。
	cfg = sampleConfig()
	cfg.RawQuery = map[string]any{
		"s": "x",
		"nested": map[string]any{
			"empty": map[string]any{},
			"keep":  "v",
		},
	}
	out, err = MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"s" = "x"`) || !strings.Contains(text, `["query"."nested"]`) || !strings.Contains(text, `"keep" = "v"`) {
		t.Errorf("非空内容应保留:\n%s", text)
	}
	if strings.Contains(text, "empty") {
		t.Errorf("空子表应跳过:\n%s", text)
	}
}

// issue 写回后重新解析仍是同一问题态,绝不变成裸 query 回退。
func TestMarshalConfig_QueryIssuesStayProblemStateAfterRoundTrip(t *testing.T) {
	cfg := sampleConfig()
	cfg.RawQueryTopLevelIssues = map[string]RawQueryTopLevelIssue{
		"query": {Name: "query", Value: "x", Kind: RawQueryIssueRootNotTable},
	}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	if !strings.Contains(string(out), `"query" = "x"`) {
		t.Errorf("根级标量 issue 应按原顶层键写出:\n%s", out)
	}
	reloaded, err := ParseUserConfig(out)
	if err != nil {
		t.Fatalf("issue 写回必须可解析: %v", err)
	}
	if reloaded.RawQuery != nil {
		t.Errorf("round-trip 后不得转正: %#v", reloaded.RawQuery)
	}
	issue := reloaded.RawQueryTopLevelIssues["query"]
	if issue.Kind != RawQueryIssueRootNotTable || issue.Value != "x" {
		t.Errorf("round-trip 后 issue 失真: %#v", issue)
	}

	// 数组根值 + map issue 混合。
	cfg = sampleConfig()
	cfg.RawQueryTopLevelIssues = map[string]RawQueryTopLevelIssue{
		"query": {Name: "query", Value: []any{"a", "b"}, Kind: RawQueryIssueRootNotTable},
		"Query": {Name: "Query", Value: map[string]any{"default": "a"}, Kind: RawQueryIssueNameConflict},
	}
	out, err = MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"query" = [`) || !strings.Contains(text, "\"a\", \"b\"") {
		t.Errorf("根级数组 issue 应按原顶层键写出:\n%s", text)
	}
	if !strings.Contains(text, "[\"Query\"]") || !strings.Contains(text, `"default" = "a"`) {
		t.Errorf("map issue 应按原始段名写表头(即使需引号):\n%s", text)
	}
	reloaded, err = ParseUserConfig(out)
	if err != nil {
		t.Fatalf("issue 写回必须可解析: %v", err)
	}
	if reloaded.RawQuery != nil || len(reloaded.RawQueryTopLevelIssues) != 2 {
		t.Errorf("round-trip 后问题态失真: %#v / %#v", reloaded.RawQuery, reloaded.RawQueryTopLevelIssues)
	}
	if reloaded.RawQueryTopLevelIssues["Query"].Kind != RawQueryIssueNameConflict {
		t.Errorf("Query issue 类别失真: %#v", reloaded.RawQueryTopLevelIssues["Query"])
	}

	// 空表 issue 无条件写表头,不被空段省略规则吞掉。
	cfg = sampleConfig()
	cfg.RawQueryTopLevelIssues = map[string]RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: map[string]any{}, Kind: RawQueryIssueNameConflict},
	}
	out, err = MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	if !strings.Contains(string(out), "[\"Query\"]") {
		t.Errorf("空表 issue 必须无条件写出表头:\n%s", out)
	}
	reloaded, err = ParseUserConfig(out)
	if err != nil {
		t.Fatalf("空表 issue 写回必须可解析: %v", err)
	}
	if _, ok := reloaded.RawQueryTopLevelIssues["Query"]; !ok {
		t.Errorf("空表 issue round-trip 后丢失: %#v", reloaded.RawQueryTopLevelIssues)
	}
}

// 输出顺序:问题态根级标量固定在既有根级键之后、首个表头之前并按键名字节序;
// 普通非 query 表后再输出合法 query 表或 issue 表;子表按完整路径字节序。
// (两个 raw 载体互斥,合法表与问题表不会同时出现。)
func TestMarshalConfig_QueryOutputOrdering(t *testing.T) {
	// 场景一:问题态(根级标量 + 问题表并存于 issues 载体)。
	cfg := sampleConfig()
	cfg.RawQueryTopLevelIssues = map[string]RawQueryTopLevelIssue{
		"QUERY": {Name: "QUERY", Value: int64(1), Kind: RawQueryIssueNameConflict},
		"Query": {Name: "Query", Value: map[string]any{"default": "a"}, Kind: RawQueryIssueNameConflict},
		"query": {Name: "query", Value: "x", Kind: RawQueryIssueRootNotTable},
	}
	out, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	lines := strings.Split(string(out), "\n")
	var idxDataDir, idxRootIssueQuery, idxRootIssueUpperMost, idxClients, idxIssueTable int
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "data_dir"):
			idxDataDir = i
		case l == `"query" = "x"`:
			idxRootIssueQuery = i
		case l == `"QUERY" = 1`:
			idxRootIssueUpperMost = i
		case l == "[clients]":
			idxClients = i
		case l == "[\"Query\"]":
			idxIssueTable = i
		}
	}
	if !(idxDataDir < idxRootIssueUpperMost && idxRootIssueUpperMost < idxRootIssueQuery && idxRootIssueQuery < idxClients) {
		t.Errorf("问题态根级键应在 data_dir 之后、首表头之前按键名字节序(QUERY < query):\n%s", out)
	}
	if idxClients > idxIssueTable {
		t.Errorf("普通非 query 表应先于问题态表:\n%s", out)
	}
	if idxIssueTable == 0 {
		t.Errorf("输出应含问题态表头 [\"Query\"]:\n%s", out)
	}

	// 场景二:合法 RawQuery 在普通表之后;段内先标量后子表,子表间按完整路径字节序。
	cfg2 := sampleConfig()
	cfg2.RawQuery = map[string]any{
		"sub": map[string]any{
			"z": int64(1),
			"b": map[string]any{"m": "v"},
			"a": map[string]any{"k": "v"},
		},
	}
	out2, err := MarshalConfig(cfg2)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	text2 := string(out2)
	idxClients2 := strings.Index(text2, "[clients]")
	idxQuerySub := strings.Index(text2, `["query"."sub"]`)
	idxSubZ := strings.Index(text2, `"z" = 1`)
	idxSubA := strings.Index(text2, `["query"."sub"."a"]`)
	idxSubB := strings.Index(text2, `["query"."sub"."b"]`)
	if !(idxClients2 < idxQuerySub) {
		t.Errorf("合法 query 表应在普通表之后:\n%s", text2)
	}
	if !(idxQuerySub < idxSubZ && idxSubZ < idxSubA) {
		t.Errorf("段内应先标量键值对后子表:\n%s", text2)
	}
	if !(idxSubA < idxSubB) {
		t.Errorf("子表应按完整路径字节序(a 先于 b):\n%s", text2)
	}
}

// CloneRawQueryState 深拷贝两载体:嵌套 map/slice 无共享引用,nil 保持 nil。
func TestCloneRawQueryStateMutationProbe(t *testing.T) {
	src := &Config{
		RawQuery: map[string]any{
			"sub": map[string]any{"list": []any{int64(1), "two"}},
		},
		RawQueryTopLevelIssues: map[string]RawQueryTopLevelIssue{
			"Query": {Name: "Query", Value: map[string]any{"k": []any{"v"}}, Kind: RawQueryIssueNameConflict},
		},
	}
	rawCopy, issueCopy := CloneRawQueryState(src)

	src.RawQuery["sub"].(map[string]any)["list"].([]any)[0] = int64(999)
	if got := rawCopy["sub"].(map[string]any)["list"].([]any)[0]; got != int64(1) {
		t.Errorf("RawQuery 深层 slice 共享引用: got %v", got)
	}
	src.RawQueryTopLevelIssues["Query"].Value.(map[string]any)["k"].([]any)[0] = "mutated"
	if got := issueCopy["Query"].Value.(map[string]any)["k"].([]any)[0]; got != "v" {
		t.Errorf("issues 深层 slice 共享引用: got %v", got)
	}

	rawCopy["sub"].(map[string]any)["new"] = "x"
	if _, ok := src.RawQuery["sub"].(map[string]any)["new"]; ok {
		t.Error("克隆侧写入泄漏到源")
	}

	// nil 载体克隆后保持 nil(不制造空 map)。
	nilRaw, nilIssues := CloneRawQueryState(&Config{})
	if nilRaw != nil || nilIssues != nil {
		t.Errorf("nil 载体应保持 nil: %#v / %#v", nilRaw, nilIssues)
	}
}

// [query.output].columns 数组的写回-读回往返:MarshalUserConfig 产出 TOML 后
// ParseUserConfig 还原同序 []any 字符串数组,raw 状态不共享引用。
func TestMarshalUserConfig_QueryOutputColumnsRoundTrip(t *testing.T) {
	src := &Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
		"output":     map[string]any{"columns": []any{"total", "cache_create", "requests"}},
	}}
	data, err := MarshalUserConfig(src)
	if err != nil {
		t.Fatalf("MarshalUserConfig: %v", err)
	}
	parsed, err := ParseUserConfig(data)
	if err != nil {
		t.Fatalf("ParseUserConfig: %v\nTOML:\n%s", err, data)
	}
	output, ok := parsed.RawQuery["output"].(map[string]any)
	if !ok {
		t.Fatalf("query.output 应为表: %T\nTOML:\n%s", parsed.RawQuery["output"], data)
	}
	if len(output) != 1 {
		t.Errorf("output 表只应含 columns: %v\nTOML:\n%s", output, data)
	}
	got, ok := output["columns"].([]any)
	if !ok {
		t.Fatalf("columns 应为 []any: %T\nTOML:\n%s", output["columns"], data)
	}
	want := []any{"total", "cache_create", "requests"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("columns 往返 = %v, want %v\nTOML:\n%s", got, want, data)
	}
	// 解析产物不与源 raw 共享引用。
	output["columns"].([]any)[0] = "mutated"
	if src.RawQuery["output"].(map[string]any)["columns"].([]any)[0] != "total" {
		t.Error("解析产物与源 raw 共享了 []any 引用")
	}
}
