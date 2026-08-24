package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/pelletier/go-toml/v2"
)

// MarshalConfig 把配置序列化为 TOML(go-toml/v2),层级中性。
// config show 等读取 effective 配置的入口使用它;用户配置写盘仍用 MarshalUserConfig。
func MarshalConfig(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%s", ui.Bi("config must not be nil", "配置不能为 nil"))
	}
	return toml.Marshal(cfg)
}

// MarshalUserConfig 把用户配置层序列化为 TOML(go-toml/v2)。
// 丢注释 + map 键字典序重排(决策记录);依赖 Config 的 toml tag(T1)。
func MarshalUserConfig(cfg *Config) ([]byte, error) {
	return MarshalConfig(cfg)
}

// WriteUserConfigAtomic 使用全仓统一的完整文件替换 helper。
func WriteUserConfigAtomic(path string, cfg *Config) error {
	data, err := MarshalUserConfig(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to marshal config", "序列化配置失败"), err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create config directory", "创建配置目录失败"), err)
	}
	if err := fileutil.ReplaceCompleteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to replace config file", "完整替换配置失败"), err)
	}
	return nil
}
