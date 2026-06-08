package config

import (
	"path/filepath"
)

type Config struct {
	DataDir         string                  `mapstructure:"data_dir" toml:"data_dir"`
	Clients         map[string]Client       `mapstructure:"clients" toml:"clients,omitempty"`
	Routers         map[string]RouterConfig `mapstructure:"routers" toml:"routers,omitempty"`
	Daemon          DaemonConfig            `mapstructure:"daemon" toml:"daemon,omitempty"`
	Log             LogConfig               `mapstructure:"log" toml:"log,omitempty"`
	ProviderAliases map[string]string       `mapstructure:"provider_aliases" toml:"provider_aliases,omitempty"`
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
