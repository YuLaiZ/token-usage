// Package runtimecfg 是 raw config 与 effective config 之间的唯一解析边界。
//
// daemon / collect / analyzer / ApplyConfig 影响分析都通过本包把「用户配置层」
// （不展开 ~、不补默认路径）解析成「有效配置层」（展开 ~、补默认值与 registry 默认路径）。
// raw config 包（internal/config）不反向依赖本包，避免循环。
package runtimecfg

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// readAll 读取 path 全部字节。
func readAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// ConfigPath 返回给定 home 下的默认配置文件路径。
// 委托 config.ConfigPath（单一来源），保证 runtimecfg 与 control/configapp/CLI 对同一
// bootstrap home 使用同一结果。保留导出便于不依赖 runtimecfg 的调用方在 runtimecfg 入口
// 一次性取得路径。
func ConfigPath(home string) string {
	return config.ConfigPath(home)
}

// UserSnapshot 是一次文件读取产生的用户层快照。
// Config 与 Raw 必须来自同一次读取，保证两者一致（不会因两次 read 之间文件被外部修改
// 产生混合版本）。Exists=false 时 Config 与 Raw 均为 nil（文件不存在）。
type UserSnapshot struct {
	Config *config.Config
	Raw    []byte
	Exists bool
}

// LoadUserConfigSnapshot 从 path 做单次读取并解析为用户层 Config。
//
// 语义（用户层，不展开 ~、不补默认路径、不 clamp Daemon/Log）：
//   - 文件不存在：返回 Exists=false, Config=nil, Raw=nil，err=nil。
//   - 空文件（含纯空白）：Exists=true，按解析错误返回（err 非 nil），与「文件缺失」分支严格区分，
//     避免 sentinel/空文件被当作「未配置」隐式创建半份配置。
//   - 正常文件：Raw 与磁盘字节逐字节相同，Config 确由该次 Raw 解析。
//
// nil map 在返回前初始化为空 map（TUI/set 写入不 panic），但不回填任何默认值。
func LoadUserConfigSnapshot(path string) (UserSnapshot, error) {
	raw, err := readAll(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UserSnapshot{Exists: false}, nil
		}
		return UserSnapshot{Exists: true}, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 空文件（仅含空白）单独判错：viper 会把空 TOML 当成合法空配置，
	// 这会让「文件缺失」与「空文件」语义混淆——config set / TUI 据此判断是否提示先 config init。
	if strings.TrimSpace(string(raw)) == "" {
		return UserSnapshot{Exists: true}, fmt.Errorf("配置文件 %s 为空（无有效配置），请先执行 `token-usage config init`", path)
	}

	cfg, perr := parseUserConfig(raw)
	if perr != nil {
		// 解析失败：Exists=true，按解析错误返回（调用方据此区分「缺失」与「损坏」）。
		return UserSnapshot{Exists: true}, perr
	}
	return UserSnapshot{Config: cfg, Raw: raw, Exists: true}, nil
}

// parseUserConfig 从 TOML 字节解析用户层 Config（initMaps 初始化 nil map，不补默认值）。
// 抽出便于 snapshot 与 raw config 的 LoadUserConfig 复用同一字节→对象语义。
func parseUserConfig(raw []byte) (*config.Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	var cfg config.Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	initMaps(&cfg)
	return &cfg, nil
}

// initMaps 把 nil map 初始化为空 map（仅此，不回填默认值）。
func initMaps(cfg *config.Config) {
	if cfg.Clients == nil {
		cfg.Clients = make(map[string]config.Client)
	}
	if cfg.Routers == nil {
		cfg.Routers = make(map[string]config.RouterConfig)
	}
	if cfg.ProviderAliases == nil {
		cfg.ProviderAliases = make(map[string]string)
	}
}
