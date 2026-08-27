package runtimecfg

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// DefaultPathProvider 把 client/router 的默认路径规则抽象为可注入接口。
// 生产用 standardDefaultPaths（registry 中实现）；测试可注入 fake provider，
// 使解析过程不依赖开发机 home/GOOS。
//
// ApplyDefaults 在 ResolveEffectiveConfig 内、深拷贝后的有效层上调用：
//   - 仅当对应路径键为空时填默认；
//   - 用户显式配置（含 dotted key 写入的）优先保留；
//   - codex sessions_dir 派生自 resolved state_dir（应用默认后的值）。
type DefaultPathProvider interface {
	ApplyDefaults(cfg *config.Config, home, goos string) error
}

// ResolveEnv 是解析有效配置所需的显式环境。
// 不读取真实 os.UserHomeDir / runtime.GOOS，由调用方注入，
// 使 daemon / collect / analyzer / ApplyConfig 影响分析在固定 env 下结果确定。
type ResolveEnv struct {
	Home         string
	GOOS         string
	DefaultPaths DefaultPathProvider
}

// ResolveEffectiveConfig 把用户层 Config 解析为有效层 Config。
//
// 不修改入参 user（深拷贝后在副本上完成）：
//  1. 校验用户层配置；
//  2. 深拷贝 user；
//  3. 初始化 nil map（避免后续写入 panic）；
//  4. 展开 ~（DataDir / Log.Dir / client paths / router db_path）；
//  5. 应用核心默认值（poll==0→30、log level→info、log dir→data_dir/logs、max_days→7）；
//  6. 调用 env.DefaultPaths.ApplyDefaults 补 client/router 默认路径。
//
// 校验属于这一唯一解析边界的一部分，避免直接调用者绕过 registry/数值校验，
// 把非法配置带入 daemon、collector 或 service。
func ResolveEffectiveConfig(user *config.Config, env ResolveEnv) (*config.Config, error) {
	if env.DefaultPaths == nil {
		return nil, fmt.Errorf("%s", ui.Bi("ResolveEnv.DefaultPaths must not be nil (resolving effective config requires a default path provider)", "ResolveEnv.DefaultPaths 不能为 nil（解析有效配置需要默认路径 provider）"))
	}
	if env.Home == "" || !filepath.IsAbs(env.Home) {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("ResolveEnv.Home must be a non-empty absolute path, got %q", env.Home),
			fmt.Sprintf("ResolveEnv.Home 必须是非空绝对路径，当前 %q", env.Home),
		))
	}
	if env.GOOS == "" {
		return nil, fmt.Errorf("%s", ui.Bi("ResolveEnv.GOOS must not be empty", "ResolveEnv.GOOS 不能为空"))
	}
	if err := ValidateUserConfig(user); err != nil {
		return nil, err
	}

	eff := deepCopy(user)
	initMaps(eff)

	expandTildeAll(eff, env.Home)
	if eff.DataDir == "" {
		eff.DataDir = filepath.Join(env.Home, ".token-usage")
	}

	applyCoreDefaults(eff)

	if err := env.DefaultPaths.ApplyDefaults(eff, env.Home, env.GOOS); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to apply default paths", "应用默认路径失败"), err)
	}
	return eff, nil
}

// LoadEffectiveConfig 一步加载有效配置：固定执行
// LoadUserConfigSnapshot → ValidateUserConfig → ResolveEffectiveConfig。
//
// 不跳过 registry 校验（未注册 client/router/path key/log level 被拒绝），
// 也不静默 clamp 负值：负数 poll_interval/max_days 由 ValidateUserConfig 在解析前拒绝，
// ResolveEffectiveConfig 自身也保留同一校验，确保所有直接调用都遵守该边界。
// daemon / collect / analyzer / config effect 分析都通过此入口，保证 effective config 单一来源。
func LoadEffectiveConfig(path string, env ResolveEnv) (*config.Config, error) {
	snap, err := LoadUserConfigSnapshot(path)
	if err != nil {
		return nil, err
	}
	if !snap.Exists {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("config file %s not found; run `token-usage config init` first", path),
			fmt.Sprintf("配置文件 %s 不存在，请先执行 `token-usage config init`", path),
		))
	}
	if err := ValidateUserConfig(snap.Config); err != nil {
		return nil, err
	}
	return ResolveEffectiveConfig(snap.Config, env)
}

// ValidateUserConfig 校验用户层 Config 中的注册项与数值/alias 合法性。
//
// 拒绝：
//   - 未注册 client / router；
//   - client 声明未注册 router；
//   - 未注册 client path key / router path key；
//   - 未注册 log level；
//   - 负数 poll_interval / max_days（0 明确保留为「使用默认值」）；
//   - provider alias 空 key / 空 value。
//
// 本函数只做静态合法性校验，不读取 FS/DB、不要求目标路径此刻存在。
// 保持纯校验使 LoadEffectiveConfig 在 ResolveEffectiveConfig 前先拒绝非法输入，
// ResolveEffectiveConfig 因此不再需要静默 clamp 负值。
func ValidateUserConfig(user *config.Config) error {
	return validateUserConfig(user, false)
}

// ValidateUserConfigForWrite 在 ValidateUserConfig 基础上追加写入口径校验：
// 非空 router 只允许配在支持归因回填的客户端上（CC Switch 仅识别 Claude 家族）。
// 读取链（show/collect/daemon 等 LoadEffectiveConfig 路径）仍用 ValidateUserConfig
// 容忍存量配置中的非法 router（行为同旧版：只写原始日志、不回填归因），
// 避免历史配置让整个程序不可用；写入链（config set / TUI 保存）用本函数拒绝新值。
func ValidateUserConfigForWrite(user *config.Config) error {
	return validateUserConfig(user, true)
}

func validateUserConfig(user *config.Config, forWrite bool) error {
	if user == nil {
		return fmt.Errorf("%s", ui.Bi("config must not be nil", "配置不能为 nil"))
	}
	if user.DataDir != "" && strings.TrimSpace(user.DataDir) == "" {
		return fmt.Errorf("%s", ui.Bi("data_dir must not be whitespace-only", "data_dir 不能只包含空白字符"))
	}
	for name := range user.Clients {
		if !isRegisteredClient(name) {
			return errNotRegistered("client", name)
		}
		for key := range user.Clients[name].Paths {
			if !isValidClientPathKey(name, key) {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("unregistered path key %q (client %q supports: %v)", key, name, RegisteredClientPathKeys(name)),
					fmt.Sprintf("未注册的 path key %q（client %q 受支持: %v）", key, name, RegisteredClientPathKeys(name)),
				))
			}
		}
		if r := user.Clients[name].Router; r != "" {
			if !isRegisteredRouter(r) {
				return errNotRegistered("router", r)
			}
			if forWrite && !ClientSupportsRouter(name) {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("client %q does not support router attribution (currently supported: %v); remove clients.%s.router or leave it empty", name, RouterCapableClients(), name),
					fmt.Sprintf("client %q 不支持 router 归因（当前仅 %v 支持）；请移除 clients.%s.router 或留空", name, RouterCapableClients(), name),
				))
			}
		}
	}
	for name := range user.Routers {
		if !isRegisteredRouter(name) {
			return errNotRegistered("router", name)
		}
	}
	if !isRegisteredLogLevel(user.Log.Level) {
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("unregistered log level %q (supported: %v)", user.Log.Level, RegisteredLogLevels()),
			fmt.Sprintf("未注册的 log level %q（受支持: %v）", user.Log.Level, RegisteredLogLevels()),
		))
	}
	if user.Daemon.PollInterval < 0 {
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("daemon.poll_interval must not be negative (0 means use the default, got %d)", user.Daemon.PollInterval),
			fmt.Sprintf("daemon.poll_interval 不能为负数（0 表示使用默认值，当前 %d）", user.Daemon.PollInterval),
		))
	}
	if user.Log.MaxDays < 0 {
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("log.max_days must not be negative (0 means use the default, got %d)", user.Log.MaxDays),
			fmt.Sprintf("log.max_days 不能为负数（0 表示使用默认值，当前 %d）", user.Log.MaxDays),
		))
	}
	for key, val := range user.ProviderAliases {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s", ui.Bi("provider_aliases keys must not be empty", "provider_aliases 的 key 不能为空"))
		}
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("provider_aliases[%q] value must not be empty", key),
				fmt.Sprintf("provider_aliases[%q] 的 value 不能为空", key),
			))
		}
	}
	return nil
}

// ---- 内部工具 ----

// deepCopy 深拷贝 Config（含 map 字段），保证 ResolveEffectiveConfig 不修改入参。
func deepCopy(c *config.Config) *config.Config {
	if c == nil {
		return &config.Config{}
	}
	cp := *c
	if c.Clients != nil {
		cp.Clients = make(map[string]config.Client, len(c.Clients))
		for k, v := range c.Clients {
			v.Paths = copyStringMap(v.Paths)
			cp.Clients[k] = v
		}
	}
	if c.Routers != nil {
		cp.Routers = make(map[string]config.RouterConfig, len(c.Routers))
		for k, v := range c.Routers {
			cp.Routers[k] = v
		}
	}
	if c.ProviderAliases != nil {
		cp.ProviderAliases = copyStringMap(c.ProviderAliases)
	}
	// raw query 状态递归深拷贝传播(不影响运行时校验与 effect,仅保证快照独立)。
	cp.RawQuery, cp.RawQueryTopLevelIssues = config.CloneRawQueryState(c)
	return &cp
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// expandTildeAll 展开所有路径字段的 ~ 前缀。
func expandTildeAll(cfg *config.Config, home string) {
	cfg.DataDir = expandTilde(cfg.DataDir, home)
	cfg.Log.Dir = expandTilde(cfg.Log.Dir, home)

	for name, router := range cfg.Routers {
		router.DBPath = expandTilde(router.DBPath, home)
		cfg.Routers[name] = router
	}
	for name, client := range cfg.Clients {
		for key, val := range client.Paths {
			client.Paths[key] = expandTilde(val, home)
		}
		cfg.Clients[name] = client
	}
}

// expandTilde 把 ~ 或 ~/ 前缀展开为 home。
// 非 ~ 前缀（含相对路径、绝对路径）原样返回。
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// applyCoreDefaults 应用核心默认值（运行时保护，不依赖 registry）。
//
// 数值字段只处理「0 表示使用默认值」：
//   - poll_interval == 0 → 30（analyzer 用 time.NewTicker(interval)，零值需补默认）；
//   - max_days == 0 → 7。
//
// 负值不再在此静默 clamp：ResolveEffectiveConfig 入口的 ValidateUserConfig 会先拒绝，
// 这里仅负责把合法的零值解析为默认值。
func applyCoreDefaults(cfg *config.Config) {
	if cfg.Daemon.PollInterval == 0 {
		cfg.Daemon.PollInterval = 30
	}
	if cfg.Log.Level == "" || cfg.Log.Level == "default" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Dir == "" {
		cfg.Log.Dir = filepath.Join(cfg.DataDir, "logs")
	}
	if cfg.Log.MaxDays == 0 {
		cfg.Log.MaxDays = 7
	}
}
