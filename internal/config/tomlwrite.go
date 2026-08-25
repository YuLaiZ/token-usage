package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	// go-toml 会在 bare key 合法时省略引号。provider_aliases 的 key 是用户输入且
	// 匹配大小写敏感的 provider 名，统一用双引号输出，避免同一表出现混合格式。
	copyCfg := *cfg
	aliases := copyCfg.ProviderAliases
	copyCfg.ProviderAliases = nil
	data, err := toml.Marshal(&copyCfg)
	if err != nil || len(aliases) == 0 {
		return data, err
	}

	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("[provider_aliases]\n")
	keys := make([]string, 0, len(aliases))
	for key := range aliases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		quotedKey, err := marshalTOMLBasicString(key)
		if err != nil {
			return nil, err
		}
		quotedValue, err := marshalTOMLBasicString(aliases[key])
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%s = %s\n", quotedKey, quotedValue)
	}
	return []byte(b.String()), nil
}

// marshalTOMLBasicString 输出 TOML 基本字符串。JSON 字符串转义是 TOML 基本字符串的有效子集，
// 因此既能强制双引号格式，也能正确保留输入中的双引号和控制字符。
func marshalTOMLBasicString(value string) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
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
