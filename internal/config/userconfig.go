package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/viper"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// DefaultConfigPath 返回默认配置路径 ~/.token-usage/config.toml。
// 委托纯函数 ConfigPath：control/configapp/CLI/runtimecfg 对同一 bootstrap home 使用同一结果。
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	return ConfigPath(home), nil
}

// LoadUserConfig 加载「用户配置层」:ReadInConfig + Unmarshal + nil→空 map 初始化,
// 不调 applyDefaults(不 clamp Daemon/Log、不补路径默认)、不调 expandPaths(不展开 ~)。
// TUI 与 config set/get 操作此对象,marshal 写回保持用户原值简洁与 ~ 可移植。
func LoadUserConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read config file", "读取配置文件失败"), err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("config file %s is empty (no valid config)", path),
			fmt.Sprintf("配置文件 %s 为空（无有效配置）", path),
		))
	}

	return ParseUserConfig(raw)
}

// ParseUserConfig 从 TOML 字节解析用户配置层。
// query 顶层项先被剥离并保存为 raw 状态(结构/类型/语义校验延迟到 query 子系统与 TUI 保存),
// 其余非 query 配置仍走 Viper 的严格字段校验;Viper 会把 map key 归一为小写,
// 因此 provider_aliases 额外从原始 TOML 恢复大小写,以满足 query provider 的精确匹配。
func ParseUserConfig(raw []byte) (*Config, error) {
	stripped, rawQuery, issues, err := splitQueryTopLevel(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read config file", "读取配置文件失败"), err)
	}
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(stripped)); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read config file", "读取配置文件失败"), err)
	}
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to parse config file", "解析配置文件失败"), err)
	}
	// mapstructure 没有 "-" 忽略约定:字面 "-" 输入键可能被解码进 raw 载体字段
	// (非空形态实测会先在 UnmarshalExact 报类型错误,此处兜住解码成功的残余形态),
	// 确保 raw 载体只反映剥离后的 query 状态,不被输入键伪造。
	if cfg.RawQuery != nil || cfg.RawQueryTopLevelIssues != nil {
		return nil, fmt.Errorf("%s: %s",
			ui.Bi("failed to parse config file", "解析配置文件失败"),
			ui.Bi(`unknown config key "-"`, `未知配置键 "-"`))
	}
	cfg.RawQuery = rawQuery
	cfg.RawQueryTopLevelIssues = issues
	if err := restoreProviderAliasKeyCase(raw, &cfg); err != nil {
		return nil, err
	}
	initMaps(&cfg)
	return &cfg, nil
}

// splitQueryTopLevel 用一次裸 map 解码识别全部 ASCII 小写归一后等于 "query" 的顶层项,
// 按四态规则生成 raw 状态,并把这些顶层项从供 Viper 使用的输入中剥离后重新编码。
// 剥离后无剩余顶层项时返回 nil 字节(空 TOML),由 Viper 按空配置解析。
func splitQueryTopLevel(raw []byte) (stripped []byte, rawQuery map[string]any, issues map[string]RawQueryTopLevelIssue, err error) {
	var top map[string]any
	if err := toml.Unmarshal(raw, &top); err != nil {
		return nil, nil, nil, err
	}
	entries := make(map[string]any)
	for k, v := range top {
		if asciiLower(k) == "query" {
			entries[k] = v
		}
	}
	rawQuery, issues = classifyQueryEntries(entries)
	for k := range entries {
		delete(top, k)
	}
	if len(top) == 0 {
		return nil, rawQuery, issues, nil
	}
	stripped, err = toml.Marshal(top)
	if err != nil {
		return nil, nil, nil, err
	}
	return stripped, rawQuery, issues, nil
}

// classifyQueryEntries 对全部 query 顶层项应用四态分类:
// 无项→未配置;唯一精确小写且值为表→RawQuery;其余按 name_conflict / root_not_table
// 逐项标注进 issues(精确小写但值非表为 root_not_table,含变体并存时为 name_conflict)。
func classifyQueryEntries(entries map[string]any) (rawQuery map[string]any, issues map[string]RawQueryTopLevelIssue) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) == 1 {
		if m, ok := entries["query"].(map[string]any); ok {
			return m, nil
		}
	}
	issues = make(map[string]RawQueryTopLevelIssue, len(entries))
	for k, v := range entries {
		kind := RawQueryIssueNameConflict
		if k == "query" {
			if _, ok := v.(map[string]any); !ok {
				kind = RawQueryIssueRootNotTable
			}
		}
		issues[k] = RawQueryTopLevelIssue{Name: k, Value: v, Kind: kind}
	}
	return nil, issues
}

// ReclassifyRawQuery 以 draft 当前全部 query 顶层项(原始名称→原始值)重建
// RawQuery 与 RawQueryTopLevelIssues,复用解析期的四态分类规则并原子更新两个互斥载体。
// entries 为空时两载体均置 nil(等价未配置)。本函数只处理 raw 状态,不做 query 语义校验。
func ReclassifyRawQuery(cfg *Config, entries map[string]any) {
	rawQuery, issues := classifyQueryEntries(entries)
	cfg.RawQuery = rawQuery
	cfg.RawQueryTopLevelIssues = issues
}

// CloneRawQueryState 返回两个 raw query 载体的递归深拷贝(含任意嵌套 map/slice)。
// 所有拷贝点(runtimecfg、configapp、TUI 草稿)统一调用本函数,不保留等价实现分支;
// nil 载体保持 nil,不制造空 map。
func CloneRawQueryState(c *Config) (map[string]any, map[string]RawQueryTopLevelIssue) {
	if c == nil {
		return nil, nil
	}
	var raw map[string]any
	if c.RawQuery != nil {
		raw = make(map[string]any, len(c.RawQuery))
		for k, v := range c.RawQuery {
			raw[k] = cloneRawValue(v)
		}
	}
	var issues map[string]RawQueryTopLevelIssue
	if c.RawQueryTopLevelIssues != nil {
		issues = make(map[string]RawQueryTopLevelIssue, len(c.RawQueryTopLevelIssues))
		for k, issue := range c.RawQueryTopLevelIssues {
			issue.Value = cloneRawValue(issue.Value)
			issues[k] = issue
		}
	}
	return raw, issues
}

// cloneRawValue 递归复制 raw query 值;TOML 可表示的标量为值类型,直接返回。
func cloneRawValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = cloneRawValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = cloneRawValue(vv)
		}
		return out
	default:
		return v
	}
}

// asciiLower 仅把 ASCII 大写字母映射为小写,非 ASCII 字节原样保留:
// ["QUÉRY"] 之类顶层键不会被归一成 "query",仍由 Viper 按未知键严格失败。
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// restoreProviderAliasKeyCase 用 TOML 解码器保留 provider_aliases 的原始 key。
// 仅覆盖该表，其他配置仍完全沿用 Viper 的现有解析语义。
func restoreProviderAliasKeyCase(raw []byte, cfg *Config) error {
	var doc struct {
		ProviderAliases map[string]any `toml:"provider_aliases"`
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to parse config file", "解析配置文件失败"), err)
	}
	if doc.ProviderAliases == nil {
		return nil
	}
	aliases := make(map[string]string, len(doc.ProviderAliases))
	for key, value := range doc.ProviderAliases {
		text, ok := value.(string)
		if !ok {
			// 由 Viper 保留既有的类型校验/转换合同；此处只处理字符串别名。
			return nil
		}
		aliases[key] = text
	}
	cfg.ProviderAliases = aliases
	return nil
}

// LoadUserConfigAuto 从默认路径加载用户配置层。
func LoadUserConfigAuto() (*Config, error) {
	p, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadUserConfig(p)
}

// initMaps 把 nil map 初始化为空 map(仅此,不回填默认值)。
// 从 applyDefaults 抽出共用,供 LoadUserConfig 使用。
func initMaps(cfg *Config) {
	if cfg.Clients == nil {
		cfg.Clients = make(map[string]Client)
	}
	if cfg.Routers == nil {
		cfg.Routers = make(map[string]RouterConfig)
	}
	if cfg.ProviderAliases == nil {
		cfg.ProviderAliases = make(map[string]string)
	}
}
