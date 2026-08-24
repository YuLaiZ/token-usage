package runtimecfg

import (
	"fmt"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// registry 是固定注册表：哪些 client/router/log level/path key 受支持。
// 新增类型（client/router）必须先有 collector/adapter 代码实现与测试，
// registry 只是注册表；新增 client/router 不是纯配置。
//
// 所有 Registered* 函数返回 slice 副本，调用方修改不污染全局。
var registry = struct {
	clients     []string
	routers     []string
	logLevels   []string
	clientPaths map[string][]string
	routerPaths map[string][]string
}{
	clients:   []string{"claude", "codex", "opencode", "workbuddy", "zcode", "autoclaw"},
	routers:   []string{"cc_switch"},
	logLevels: []string{"default", "info", "debug", "warn", "error"},
	clientPaths: map[string][]string{
		"claude":    {"projects_dir"},
		"codex":     {"state_dir", "sessions_dir"},
		"opencode":  {"db"},
		"workbuddy": {"db", "projects_dir"},
		"zcode":     {"db"},
		"autoclaw":  {"sessions_dir"},
	},
	routerPaths: map[string][]string{
		"cc_switch": {"db_path"},
	},
}

// routerCapableClients 是支持 router 归因回填的客户端集合。
// CC Switch 的 app_type 仅识别 Claude 家族，其余客户端即使配置了 router
// 也只写原始日志、不回填归因；因此配置入口（config set / TUI / 保存校验）
// 统一拒绝，避免产生「已配置即生效」的误导。支持面扩大时更新此表。
var routerCapableClients = []string{"claude"}

func copySlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// RegisteredClients 返回受支持的 client 名集合（副本）。
func RegisteredClients() []string {
	return copySlice(registry.clients)
}

// RegisteredRouters 返回受支持的 router 名集合（副本）。
func RegisteredRouters() []string {
	return copySlice(registry.routers)
}

// ClientSupportsRouter 判断客户端是否支持 router 归因回填。
func ClientSupportsRouter(client string) bool {
	return contains(routerCapableClients, client)
}

// RouterCapableClients 返回支持 router 归因回填的客户端名集合（副本）。
func RouterCapableClients() []string {
	return copySlice(routerCapableClients)
}

// RegisteredLogLevels 返回受支持的 log level 集合（副本）。
// "default" 代表用户层空值（运行时回落到 info）。
func RegisteredLogLevels() []string {
	return copySlice(registry.logLevels)
}

// RegisteredClientPathKeys 返回某 client 受支持的 path key 集合（副本）。
// 未注册 client 返回 nil（非 panic）。
func RegisteredClientPathKeys(client string) []string {
	return copySlice(registry.clientPaths[client])
}

// RegisteredRouterPathKeys 返回某 router 受支持的 path key 集合（副本）。
// 未注册 router 返回 nil（非 panic）。
func RegisteredRouterPathKeys(router string) []string {
	return copySlice(registry.routerPaths[router])
}

// contains 报告 slice 是否包含 s。
func contains(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}

// isRegisteredClient 报告 name 是否为注册的 client。
func isRegisteredClient(name string) bool {
	return contains(registry.clients, name)
}

// isRegisteredRouter 报告 name 是否为注册的 router。
func isRegisteredRouter(name string) bool {
	return contains(registry.routers, name)
}

// isRegisteredLogLevel 报告 lv 是否为注册的 log level（"" 视为 default）。
func isRegisteredLogLevel(lv string) bool {
	if lv == "" {
		return true // 用户层空值合法，运行时由默认值补 info
	}
	return contains(registry.logLevels, lv)
}

// isValidClientPathKey 报告 client 是否支持 pathKey。
func isValidClientPathKey(client, pathKey string) bool {
	return contains(registry.clientPaths[client], pathKey)
}

// isValidRouterPathKey 报告 router 是否支持 pathKey。
func isValidRouterPathKey(router, pathKey string) bool {
	return contains(registry.routerPaths[router], pathKey)
}

// ---- DefaultPathProvider 实现 ----

// standardDefaultPaths 是生产用 DefaultPathProvider：把 collector/router 的默认路径
// 规则集中于此。不读取真实 os.UserHomeDir，仅使用入参 home；goos 当前各 client/router
// 多 OS 地址一致（opencode 用 xdg-basedir 一视同仁），goos 预留给未来「不同 OS 地址不同」的工具。
type standardDefaultPaths struct{}

// NewStandardProvider 返回生产用 provider（测试可注入 fake）。
func NewStandardProvider() DefaultPathProvider {
	return standardDefaultPaths{}
}

// newStandardProvider 保留为包内别名（测试用，与导出版本等价）。
func newStandardProvider() DefaultPathProvider {
	return NewStandardProvider()
}

// ApplyDefaults 为每个注册 client/router 补默认路径（仅当对应键为空时）。
// 用户显式配置优先保留；codex sessions_dir 派生自 resolved state_dir（应用默认后的值）。
// 为 client 声明但 routers 表缺失的 router 创建默认 entry（开箱即用）。
func (standardDefaultPaths) ApplyDefaults(cfg *config.Config, home, goos string) error {
	_ = goos // 当前各 client/router 多 OS 地址一致；保留参数供未来按 OS 分支展开。

	if cfg.Routers == nil {
		cfg.Routers = map[string]config.RouterConfig{}
	}

	// client 默认路径：仅对注册 client 补，键为空才填。
	for name, c := range cfg.Clients {
		if c.Paths == nil {
			c.Paths = map[string]string{}
		}
		switch name {
		case "claude":
			if c.Paths["projects_dir"] == "" {
				c.Paths["projects_dir"] = defaultClaudeProjectsDir(home)
			}
		case "codex":
			if c.Paths["state_dir"] == "" {
				c.Paths["state_dir"] = defaultCodexStateDir(home)
			}
			// sessions_dir 派生自 resolved state_dir（应用默认后的值）。
			if c.Paths["sessions_dir"] == "" {
				c.Paths["sessions_dir"] = defaultCodexSessionsDir(c.Paths["state_dir"])
			}
		case "opencode":
			if c.Paths["db"] == "" {
				c.Paths["db"] = defaultOpenCodeDB(home)
			}
		case "workbuddy":
			if c.Paths["projects_dir"] == "" {
				c.Paths["projects_dir"] = defaultWorkBuddyProjectsDir(home)
			}
			if c.Paths["db"] == "" {
				c.Paths["db"] = defaultWorkBuddyDB(home)
			}
		case "zcode":
			if c.Paths["db"] == "" {
				c.Paths["db"] = defaultZCodeDB(home)
			}
		case "autoclaw":
			if c.Paths["sessions_dir"] == "" {
				c.Paths["sessions_dir"] = defaultAutoClawSessionsDir(home)
			}
		}
		cfg.Clients[name] = c // map value 不可寻址，需回写
	}

	// router 默认 db_path。
	for name, r := range cfg.Routers {
		switch name {
		case "cc_switch":
			if r.DBPath == "" {
				r.DBPath = defaultCCSwitchDB(home)
			}
		}
		cfg.Routers[name] = r
	}

	// 为 client 声明但 routers 表缺失的 router 创建默认 entry（开箱即用）。
	for _, c := range cfg.Clients {
		if c.Router == "" {
			continue
		}
		if _, exists := cfg.Routers[c.Router]; exists {
			continue
		}
		switch c.Router {
		case "cc_switch":
			cfg.Routers[c.Router] = config.RouterConfig{DBPath: defaultCCSwitchDB(home)}
		default:
			// 未注册 router：不在 provider 报错（registry 校验由 ValidateUserConfig 负责），
			// 但也不会为未知 router 创建 entry。
		}
	}
	return nil
}

func defaultClaudeProjectsDir(home string) string {
	return filepath.Join(home, ".claude", "projects")
}

func defaultCodexStateDir(home string) string {
	return filepath.Join(home, ".codex")
}

func defaultCodexSessionsDir(stateDir string) string {
	return filepath.Join(stateDir, "sessions")
}

// defaultOpenCodeDB 返回 opencode 的 SQLite 数据库路径。
// opencode 用 xdg-basedir（对所有平台一视同仁，无 Windows APPDATA 分支），故多 OS 地址一致，
// 统一 ~/.local/share/opencode/opencode.db。
func defaultOpenCodeDB(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

func defaultWorkBuddyProjectsDir(home string) string {
	return filepath.Join(home, ".workbuddy", "projects")
}

func defaultWorkBuddyDB(home string) string {
	return filepath.Join(home, ".workbuddy", "workbuddy.db")
}

func defaultCCSwitchDB(home string) string {
	return filepath.Join(home, ".cc-switch", "cc-switch.db")
}

// defaultZCodeDB 返回 ZCode CLI 的 SQLite 数据库路径 ~/.zcode/cli/db/db.sqlite。
// ZCode CLI 多 OS 地址一致（实测 darwin/linux 均为该路径），按约定忽略 OS 条件。
func defaultZCodeDB(home string) string {
	return filepath.Join(home, ".zcode", "cli", "db", "db.sqlite")
}

// defaultAutoClawSessionsDir 返回 AutoClaw 的 sessions 根目录 agents/（collector 从 agents/*/sessions/ 递归发现）。
func defaultAutoClawSessionsDir(home string) string {
	return filepath.Join(home, ".openclaw-autoclaw", "agents")
}

// errNotRegistered 构造「未注册 X」错误（稳定错误信息，便于测试与 CLI 提示）。
func errNotRegistered(kind, name string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unregistered %s %q (supported: %v)", kind, name, registeredNamesForKind(kind)),
		fmt.Sprintf("未注册的 %s %q（受支持: %v）", kind, name, registeredNamesForKind(kind)),
	))
}

func registeredNamesForKind(kind string) []string {
	switch kind {
	case "client":
		return RegisteredClients()
	case "router":
		return RegisteredRouters()
	default:
		return nil
	}
}
