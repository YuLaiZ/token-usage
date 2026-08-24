package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

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

	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read config file", "读取配置文件失败"), err)
	}
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to parse config file", "解析配置文件失败"), err)
	}
	initMaps(&cfg)
	return &cfg, nil
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
