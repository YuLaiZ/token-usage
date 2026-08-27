package config

import (
	"path/filepath"
)

// RawQueryIssueKind 标记一个 query 顶层问题项的类别。
type RawQueryIssueKind string

const (
	// RawQueryIssueNameConflict:顶层键 ASCII 小写归一后等于 "query",
	// 但存在大小写变体或并非唯一精确小写 "query"。
	RawQueryIssueNameConflict RawQueryIssueKind = "name_conflict"
	// RawQueryIssueRootNotTable:精确小写 "query" 顶层键的值不是表。
	RawQueryIssueRootNotTable RawQueryIssueKind = "root_not_table"
)

// RawQueryTopLevelIssue 保存一个 query 顶层问题项的原始顶层名称、完整 raw 值和类别。
// 它不是用户 TOML 键,仅是内存态问题载体;序列化时按原始名称与原始值形态写回。
type RawQueryTopLevelIssue struct {
	Name  string
	Value any
	Kind  RawQueryIssueKind
}

type Config struct {
	DataDir         string                  `mapstructure:"data_dir" toml:"data_dir"`
	Clients         map[string]Client       `mapstructure:"clients" toml:"clients,omitempty"`
	Routers         map[string]RouterConfig `mapstructure:"routers" toml:"routers,omitempty"`
	Daemon          DaemonConfig            `mapstructure:"daemon" toml:"daemon,omitempty"`
	Log             LogConfig               `mapstructure:"log" toml:"log,omitempty"`
	ProviderAliases map[string]string       `mapstructure:"provider_aliases" toml:"provider_aliases,omitempty"`

	// RawQuery 保存唯一精确小写 [query] 段的完整子树(内部原始键大小写与值类型原样保留)。
	// RawQueryTopLevelIssues 保存顶层名称冲突或根值非表的全部问题项。两者互斥,
	// 任一配置快照只能有一个非空;均不参与 struct 编码,由序列化层手工写回,
	// 语义校验延迟到 query 子系统与 TUI 保存前执行。
	RawQuery               map[string]any                   `mapstructure:"-" toml:"-"`
	RawQueryTopLevelIssues map[string]RawQueryTopLevelIssue `mapstructure:"-" toml:"-"`
}

type Client struct {
	Enabled bool              `mapstructure:"enabled" toml:"enabled"`
	Router  string            `mapstructure:"router" toml:"router,omitempty"`
	Paths   map[string]string `mapstructure:"paths" toml:"paths,omitempty"`
}

// RouterConfig 单个路由中间件的配置。
// 路由的"实现类型"由配置表名（map key，如 "cc_switch"）决定，不再用冗余的 type 字段——
// 未来新增路由中间件时，约定其表名并在装配代码（engine.NewDeps / analyzer.setupFromConfig）
// 按表名增 case 即可。
type RouterConfig struct {
	DBPath string `mapstructure:"db_path" toml:"db_path,omitempty"`
}

type DaemonConfig struct {
	PollInterval int  `mapstructure:"poll_interval" toml:"poll_interval,omitempty"`
	AutoStart    bool `mapstructure:"autostart" toml:"autostart"` // 不加 omitempty：bool 总是写出来，用户能看到明确的「关闭」状态而非字段消失
}

type LogConfig struct {
	Level   string `mapstructure:"level" toml:"level,omitempty"`
	Dir     string `mapstructure:"dir" toml:"dir,omitempty"`
	MaxDays int    `mapstructure:"max_days" toml:"max_days,omitempty"`
}

// ConfigPath 返回给定 home 下的默认配置文件路径 home/.token-usage/config.toml。
// 纯函数：不读取真实 os.UserHomeDir。control/configapp/CLI/runtimecfg 对同一 bootstrap home
// 都用本函数得到同一结果，DefaultConfigPath 也委托它。
func ConfigPath(home string) string {
	return filepath.Join(home, ".token-usage", "config.toml")
}

func (c *Config) ClientConfig(name string) (*Client, bool) {
	if c == nil {
		return nil, false
	}
	client, ok := c.Clients[name]
	return &client, ok
}

func (c *Config) RouterConfig(name string) (*RouterConfig, bool) {
	if c == nil {
		return nil, false
	}
	router, ok := c.Routers[name]
	return &router, ok
}
