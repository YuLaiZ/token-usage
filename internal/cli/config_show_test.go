package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// runShow 在隔离的临时 HOME 下构造一个独立的 show 命令实例，
// 绑定独立 buffer，返回 stdout 文本与执行 error。统一走 loadConfig。
func runShow(t *testing.T) (string, error) {
	t.Helper()
	cmd := newConfigShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.RunE(cmd, []string{})
	return out.String(), err
}

// runShowExecute 通过 cobra Execute 执行(覆盖 Args 校验路径)。
func runShowExecute(t *testing.T, args []string) (string, error) {
	t.Helper()
	cmd := newConfigShowCmd()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

// --- 结构测试 ---

func TestNewConfigShowCmd_Structure(t *testing.T) {
	cmd := newConfigShowCmd()
	if cmd.Use != "show" {
		t.Errorf("Use = %q, want \"show\"", cmd.Use)
	}
	if cmd.Hidden {
		t.Error("show 不应为隐藏命令")
	}
	// Args = cobra.NoArgs:无位置参数通过,有位置参数报错。
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("NoArgs 应接受空参数,实际: %v", err)
	}
	if err := cmd.Args(cmd, []string{"foo"}); err == nil {
		t.Error("NoArgs 应拒绝多余位置参数")
	}
	if cmd.RunE == nil {
		t.Error("show 必须有 RunE")
	}
}

func TestNewConfigShowCmd_LongExplainsEffective(t *testing.T) {
	cmd := newConfigShowCmd()
	long := cmd.Long
	// Long 必须明确输出 effective 配置、展开 ~、补默认值、只读、纯 TOML、
	// 路径规则(派生相对路径保持相对)、不是建议覆盖回用户配置文件的模板。
	for _, want := range []string{"effective", "~", "默认", "只读", "TOML", "派生", "相对"} {
		if !strings.Contains(long, want) {
			t.Errorf("config show Long 应含 %q,实际: %q", want, long)
		}
	}
}

// --- 行为测试 ---

// 1. 最小合法配置:codex enabled,输出补全默认值。
func TestConfigShow_MinimalCodex(t *testing.T) {
	home := setupHomeConfig(t, "[clients.codex]\nenabled = true\n")
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	// 解析为 typed Config 做强断言。
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("show 输出非合法 TOML: %v\n输出:\n%s", err, out)
	}
	// data_dir 补默认 <home>/.token-usage。
	wantDataDir := filepath.Join(home, ".token-usage")
	if cfg.DataDir != wantDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantDataDir)
	}
	// codex 默认 state_dir = <home>/.codex。
	codex, ok := cfg.Clients["codex"]
	if !ok {
		t.Fatalf("输出应含 clients.codex,实际: %#v", cfg.Clients)
	}
	wantStateDir := filepath.Join(home, ".codex")
	if codex.Paths["state_dir"] != wantStateDir {
		t.Errorf("codex state_dir = %q, want %q", codex.Paths["state_dir"], wantStateDir)
	}
	// sessions_dir 派生自 state_dir。
	wantSessions := filepath.Join(wantStateDir, "sessions")
	if codex.Paths["sessions_dir"] != wantSessions {
		t.Errorf("codex sessions_dir = %q, want %q", codex.Paths["sessions_dir"], wantSessions)
	}
	// daemon 默认 poll_interval=30,autostart=false。
	if cfg.Daemon.PollInterval != 30 {
		t.Errorf("Daemon.PollInterval = %d, want 30", cfg.Daemon.PollInterval)
	}
	if cfg.Daemon.AutoStart != false {
		t.Errorf("Daemon.AutoStart = %v, want false", cfg.Daemon.AutoStart)
	}
	// log level=info,max_days=7,dir 派生。
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
	if cfg.Log.MaxDays != 7 {
		t.Errorf("Log.MaxDays = %d, want 7", cfg.Log.MaxDays)
	}
	wantLogDir := filepath.Join(wantDataDir, "logs")
	if cfg.Log.Dir != wantLogDir {
		t.Errorf("Log.Dir = %q, want %q", cfg.Log.Dir, wantLogDir)
	}
}

// 2. ~ 在 data_dir/log/client path/router path 全部展开。
func TestConfigShow_TildeExpanded(t *testing.T) {
	home := setupHomeConfig(t, `data_dir = "~/data"
[clients.codex]
enabled = true
[clients.codex.paths]
state_dir = "~/codex-state"
[clients.opencode]
enabled = true
[clients.opencode.paths]
db = "~/oc.db"
[routers.cc_switch]
db_path = "~/cc.db"
[log]
dir = "~/logs"
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("show 输出非合法 TOML: %v\n输出:\n%s", err, out)
	}
	if cfg.DataDir != filepath.Join(home, "data") {
		t.Errorf("DataDir 未展开 ~: %q", cfg.DataDir)
	}
	if cfg.Log.Dir != filepath.Join(home, "logs") {
		t.Errorf("Log.Dir 未展开 ~: %q", cfg.Log.Dir)
	}
	codex := cfg.Clients["codex"]
	if codex.Paths["state_dir"] != filepath.Join(home, "codex-state") {
		t.Errorf("codex state_dir 未展开 ~: %q", codex.Paths["state_dir"])
	}
	oc := cfg.Clients["opencode"]
	if oc.Paths["db"] != filepath.Join(home, "oc.db") {
		t.Errorf("opencode db 未展开 ~: %q", oc.Paths["db"])
	}
	router := cfg.Routers["cc_switch"]
	if router.DBPath != filepath.Join(home, "cc.db") {
		t.Errorf("router db_path 未展开 ~: %q", router.DBPath)
	}
	if strings.Contains(out, "~") {
		t.Errorf("输出不应残留 ~,实际:\n%s", out)
	}
}

// 2b. 显式相对路径及其派生的默认路径保持相对。
//
// runtimecfg.expandTilde 只展开 ~ 前缀,不规范化相对路径;而部分默认路径
// 从已解析的基路径派生(data_dir→log.dir、state_dir→sessions_dir),
// 因此当基路径是相对路径时,派生路径也是相对路径。此测试锁定该行为,
// 防止文案或实现误把派生路径变成绝对路径。
func TestConfigShow_RelativeBasePathPropagatesToDerivedDefaults(t *testing.T) {
	setupHomeConfig(t, `data_dir = "relative-data"
[clients.codex]
enabled = true
[clients.codex.paths]
state_dir = "relative-codex"
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("show 输出非合法 TOML: %v\n输出:\n%s", err, out)
	}
	// 显式相对基路径保持相对。
	if cfg.DataDir != "relative-data" {
		t.Errorf("DataDir 应保持相对 %q, 实际: %q", "relative-data", cfg.DataDir)
	}
	codex := cfg.Clients["codex"]
	if codex.Paths["state_dir"] != "relative-codex" {
		t.Errorf("codex state_dir 应保持相对 %q, 实际: %q", "relative-codex", codex.Paths["state_dir"])
	}
	// 由相对基路径派生的默认路径也保持相对(log.dir 从 data_dir 派生、
	// sessions_dir 从 state_dir 派生)。派生走 filepath.Join,分隔符随平台,
	// 用 filepath.Join 构造预期避免在 Windows 上误判反斜杠。
	wantLogDir := filepath.Join("relative-data", "logs")
	if cfg.Log.Dir != wantLogDir {
		t.Errorf("Log.Dir 应从相对 data_dir 派生为 %q, 实际: %q", wantLogDir, cfg.Log.Dir)
	}
	wantSessionsDir := filepath.Join("relative-codex", "sessions")
	if got := codex.Paths["sessions_dir"]; got != wantSessionsDir {
		t.Errorf("codex sessions_dir 应从相对 state_dir 派生为 %q, 实际: %q", wantSessionsDir, got)
	}
}

// 3. 用户显式配置优先保留。
func TestConfigShow_UserExplicitPreserved(t *testing.T) {
	setupHomeConfig(t, `[clients.codex]
enabled = true
[daemon]
poll_interval = 99
[log]
level = "debug"
max_days = 42
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("show 输出非合法 TOML: %v", err)
	}
	if cfg.Daemon.PollInterval != 99 {
		t.Errorf("PollInterval = %d, want 99", cfg.Daemon.PollInterval)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want debug", cfg.Log.Level)
	}
	if cfg.Log.MaxDays != 42 {
		t.Errorf("Log.MaxDays = %d, want 42", cfg.Log.MaxDays)
	}
}

// 4. client 引用 router 但 routers 表缺失:补默认 router entry。
func TestConfigShow_ClientRouterMissingEntryAdded(t *testing.T) {
	setupHomeConfig(t, `[clients.opencode]
enabled = true
router = "cc_switch"
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("show 输出非合法 TOML: %v", err)
	}
	router, ok := cfg.Routers["cc_switch"]
	if !ok {
		t.Fatalf("输出应含 routers.cc_switch 段,实际: %#v", cfg.Routers)
	}
	if router.DBPath == "" {
		t.Error("cc_switch 应有默认 db_path")
	}
}

// 5. 合法配置但 clients/routers 均为空:输出可解析,不 panic。
func TestConfigShow_EmptyClientsRouters(t *testing.T) {
	setupHomeConfig(t, `[log]
level = "info"
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	// 输出必须是合法 TOML,能解析为 Config。
	var cfg config.Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("空 clients/routers 输出应可解析,实际: %v\n输出:\n%s", err, out)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
}

// 6. 强断言:round-trip typed Config(完整语义断言)。
func TestConfigShow_RoundTripTypedConfig(t *testing.T) {
	home := setupHomeConfig(t, `data_dir = "~/data"
[clients.codex]
enabled = true
[clients.codex.paths]
state_dir = "~/cstate"
[clients.opencode]
enabled = true
router = "cc_switch"
[daemon]
poll_interval = 99
[log]
level = "warn"
max_days = 3
`)
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	// 重新解析为 typed Config,与期望值逐一断言。
	var got config.Config
	if err := toml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("round-trip 解析失败: %v\n输出:\n%s", err, out)
	}
	if got.DataDir != filepath.Join(home, "data") {
		t.Errorf("DataDir = %q, want %q", got.DataDir, filepath.Join(home, "data"))
	}
	if got.Daemon.PollInterval != 99 {
		t.Errorf("PollInterval = %d, want 99", got.Daemon.PollInterval)
	}
	if got.Log.Level != "warn" {
		t.Errorf("Log.Level = %q, want warn", got.Log.Level)
	}
	codex, ok := got.Clients["codex"]
	if !ok {
		t.Fatal("round-trip 后应含 clients.codex")
	}
	if codex.Paths["state_dir"] != filepath.Join(home, "cstate") {
		t.Errorf("codex state_dir = %q", codex.Paths["state_dir"])
	}
	if codex.Paths["sessions_dir"] != filepath.Join(filepath.Join(home, "cstate"), "sessions") {
		t.Errorf("codex sessions_dir 派生错误: %q", codex.Paths["sessions_dir"])
	}
	if _, ok := got.Routers["cc_switch"]; !ok {
		t.Error("round-trip 后应含 routers.cc_switch")
	}
}

// 7. stdout 首字符属于 TOML 内容,无前缀。
func TestConfigShow_NoPrefix(t *testing.T) {
	setupHomeConfig(t, "[clients.codex]\nenabled = true\n")
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		t.Fatal("输出不应为空")
	}
	// TOML 内容首字符应是表头 [ 或 key=value。
	if !strings.HasPrefix(out, "[") && !isTOMLKeyFirst(out) {
		t.Errorf("输出首字符应是 TOML 表头/key,实际: %q", out)
	}
	for _, bad := range []string{"当前配置", "提示", "warning", "Warning", "WARNING", "配置如下"} {
		if strings.Contains(out, bad) {
			t.Errorf("输出不应含前缀/提示语 %q,实际:\n%s", bad, out)
		}
	}
}

// isTOMLKeyFirst 粗略判断首行是否为 key = value 形式。
func isTOMLKeyFirst(s string) bool {
	firstLine := strings.SplitN(s, "\n", 2)[0]
	return strings.Contains(firstLine, "=")
}

// 8. 配置文件不存在:返回 error,stdout 为空,error 含 config init 建议。
func TestConfigShow_ConfigMissing(t *testing.T) {
	// 临时空 HOME,不写 config。同时设置 USERPROFILE 以在 Windows 上隔离。
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	out, err := runShow(t)
	if err == nil {
		t.Fatal("配置缺失应返回 error")
	}
	if out != "" {
		t.Errorf("配置缺失时 stdout 应为空,实际: %q", out)
	}
	if !strings.Contains(err.Error(), "config init") {
		t.Errorf("error 应含 config init 建议,实际: %v", err)
	}
}

// 9. 空配置文件:error。
func TestConfigShow_EmptyFile(t *testing.T) {
	setupHomeConfig(t, "")
	out, err := runShow(t)
	if err == nil {
		t.Fatal("空配置文件应返回 error")
	}
	if out != "" {
		t.Errorf("空配置文件时 stdout 应为空,实际: %q", out)
	}
}

// 10. TOML 损坏:error。
func TestConfigShow_CorruptedTOML(t *testing.T) {
	setupHomeConfig(t, "=invalid\n")
	out, err := runShow(t)
	if err == nil {
		t.Fatal("损坏 TOML 应返回 error")
	}
	if out != "" {
		t.Errorf("损坏 TOML 时 stdout 应为空,实际: %q", out)
	}
}

// 11. 未注册 client:error。
func TestConfigShow_UnregisteredClient(t *testing.T) {
	setupHomeConfig(t, "[clients.ghost]\nenabled = true\n")
	out, err := runShow(t)
	if err == nil {
		t.Fatal("未注册 client 应返回 error")
	}
	if out != "" {
		t.Errorf("未注册 client 时 stdout 应为空,实际: %q", out)
	}
}

// 12. 多余位置参数:NoArgs 拒绝。
// 直接调用 cmd.RunE 不经 cobra 参数校验(无副作用),此处通过 Execute 走 cobra 完整校验路径,
// 断言返回 error。注:SetOut 同时影响 OutOrStderr,cobra 在参数校验失败时会把 usage 写入该 writer,
// 因此本场景不断言 stdout 为空,仅断言拒绝行为(error);NoArgs 类型已在 Structure 测试中验证。
func TestConfigShow_RejectsPositionalArgs(t *testing.T) {
	setupHomeConfig(t, "[clients.codex]\nenabled = true\n")
	_, err := runShowExecute(t, []string{"foo"})
	if err == nil {
		t.Fatal("多余位置参数应报错(NoArgs 拒绝)")
	}
	// 再次确认 NoArgs 类型。
	cmd := newConfigShowCmd()
	if cmd.Args == nil {
		t.Error("Args 不应为 nil")
	}
}

// 13. 执行前后 config 原始字节相同。
func TestConfigShow_DoesNotMutateConfigFile(t *testing.T) {
	content := "[clients.codex]\nenabled = true\n"
	home := setupHomeConfig(t, content)
	cfgPath := filepath.Join(home, ".token-usage", "config.toml")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	beforeInfo, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat 配置失败: %v", err)
	}
	if _, err := runShow(t); err != nil {
		t.Fatalf("show: %v", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("配置字节被修改: before=%q after=%q", before, after)
	}
	afterInfo, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat 配置失败: %v", err)
	}
	// mtime 在可比较平台上不变。
	if beforeInfo.ModTime() != afterInfo.ModTime() {
		t.Errorf("mtime 被修改: before=%v after=%v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
}

// 14. 执行后不存在 usage.db / 日志目录 / PID / runtime-state / lock 文件。
func TestConfigShow_NoRuntimeSideEffects(t *testing.T) {
	home := setupHomeConfig(t, "[clients.codex]\nenabled = true\n")
	if _, err := runShow(t); err != nil {
		t.Fatalf("show: %v", err)
	}
	// 不应触发 loadRuntime,故无 usage.db。
	usageDB := filepath.Join(home, ".token-usage", "usage.db")
	if _, err := os.Stat(usageDB); err == nil {
		t.Error("show 不应创建 usage.db")
	}
	// 不应初始化日志,故无 logs 目录。
	logDir := filepath.Join(home, ".token-usage", "logs")
	if _, err := os.Stat(logDir); err == nil {
		t.Error("show 不应创建日志目录")
	}
	// 不应产生 PID/lock/runtime-state 文件。
	for _, p := range []string{"daemon.pid", "daemon.lock", "runtime-state", ".lock"} {
		full := filepath.Join(home, ".token-usage", p)
		if _, err := os.Stat(full); err == nil {
			t.Errorf("show 不应创建运行时元数据 %s", p)
		}
	}
}

// 确保新命令构造函数存在且返回 *cobra.Command(编译期 + 运行期断言)。
func TestNewConfigShowCmd_ReturnsCommand(t *testing.T) {
	var _ *cobra.Command = newConfigShowCmd()
}

// 合法与问题态 query 在 show(effective 输出)中原样保留,输出仍是合法 TOML。
func TestConfigShow_QueryStatesPreserved(t *testing.T) {
	setupHomeConfig(t, "[query.subqueries]\nmpc = \"model,provider\"\n")
	out, err := runShow(t)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	// query 键名与表头路径段统一双引号。
	if !strings.Contains(out, `["query"."subqueries"]`) || !strings.Contains(out, `"mpc" = "model,provider"`) {
		t.Errorf("合法 query 段应保留(双引号键形态):\n%s", out)
	}
	var top map[string]any
	if err := toml.Unmarshal([]byte(out), &top); err != nil {
		t.Fatalf("show 输出应仍是合法 TOML: %v\n%s", err, out)
	}
	if q, ok := top["query"].(map[string]any); !ok || q["subqueries"] == nil {
		t.Errorf("query 段丢失: %#v", top["query"])
	}

	setupHomeConfig(t, "data_dir = \"/x\"\nquery = \"x\"\n")
	out2, err := runShow(t)
	if err != nil {
		t.Fatalf("问题态 query 不得阻塞 config show: %v", err)
	}
	if !strings.Contains(out2, `"query" = "x"`) {
		t.Errorf("根级标量问题项应按原样保留(双引号键形态):\n%s", out2)
	}
	var top2 map[string]any
	if err := toml.Unmarshal([]byte(out2), &top2); err != nil {
		t.Fatalf("问题态 show 输出应仍是合法 TOML: %v\n%s", err, out2)
	}
	if top2["query"] != "x" {
		t.Errorf("问题项内容失真: %#v", top2["query"])
	}
}
