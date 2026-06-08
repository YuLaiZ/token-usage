// internal/service/service.go
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// Label 是所有平台共用的服务标识。
const Label = "com.yulaiz.token-usage"

// ErrPlatformUnsupported 表示当前平台不支持开机自启定义。
// ApplyConfig 把它映射为非致命说明（不加入 PartialErrors、不触发 syncPending），
// 与真实 install/remove 失败（普通 error）严格区分。
var ErrPlatformUnsupported = errors.New("当前平台不支持开机自启")

// Options 描述安装一个开机自启服务所需的参数。
type Options struct {
	Label   string   // 服务标识，固定 com.yulaiz.token-usage
	BinPath string   // os.Executable() 探测到的二进制绝对路径
	DataDir string   // 配置/日志目录，用于服务文件里的日志重定向
	Args    []string // 固定 ["_run"]（指向 Hidden 内部命令）
}

// AutoStartStatus 只表达「下次登录/开机的自启定义」状态，不含当前进程信息。
//
//   - Exists：macOS=plist 文件存在；Windows=注册表 Run 值存在。
//   - SpecMatches：定义内容是否与当前 Options 完全一致（BinPath + 完整 Args + 日志路径）。
//
// Status 不再把 launchd job 是否 loaded、daemon 是否 running 混入 Exists。
type AutoStartStatus struct {
	Exists      bool
	SpecMatches bool
}

// SyncReport 描述一次 definition 收敛前后的准确状态。
type SyncReport struct {
	Before        AutoStartStatus
	After         AutoStartStatus
	Triggered     bool
	Unsupported   bool
	DriftRepaired bool
}

// AutoStartManager 只管理下次登录/开机的 definition，不暴露启停当前 daemon 的方法。
//
// macOS Exists = plist 文件存在；Windows Exists = 注册表 Run 值存在。
// Enable/Disable 都不碰当前进程；macOS Disable 删 plist 但不以当前 job/daemon 仍存在作为失败，
// Windows Enable/Disable 只收敛准确 Run 值不 spawn/taskkill。
// 当前进程停止所需的 macOS bootout 放在 RuntimeStopper（见 StopCurrent），不作为 autostart 状态判断。
type AutoStartManager interface {
	// Enable 安装或修复定义，不启动进程（下次登录/重启触发）。
	Enable(opts Options) error
	// Disable 删除定义，不停止进程。
	Disable(opts Options) error
	// Status 只报告定义是否存在及内容是否匹配。
	Status(opts Options) (AutoStartStatus, error)
	Platform() string // "launchd" / "registry" / "unsupported"
}

// RuntimeStopper 表达「停止当前运行进程」的平台能力，与自启定义彻底解耦。
//
// 停止当前 daemon 不属于纯 definition 接口（AutoStartManager）。
// control 包的 stop 需要 macOS bootout（先于 SIGTERM 尝试卸载 launchd job），
// Windows taskkill 精确 PID；这些由本接口提供，供 control 包独立装配。
//
// 不动定义文件：StopCurrent 后下次登录/重启仍按 autostart 定义启动。
type RuntimeStopper interface {
	// StopCurrent 仅停止当前进程，保留定义文件。
	// 调用点：stop 命令的「受托管」分支。
	StopCurrent(opts Options) error
}

// Manager 同时满足 AutoStartManager（定义层）与 RuntimeStopper（进程停止层）。
// 生产装配（New）返回该组合类型，便于调用方按需取其中一接口。
type Manager interface {
	AutoStartManager
	RuntimeStopper
}

// New 返回当前平台的 Manager 实现（同时实现 AutoStartManager 与 RuntimeStopper）。
func New() Manager { return newPlatformManager() }

// NewAutoStartManager 返回只暴露定义层 API 的 AutoStartManager（状态/漂移检测用）。
func NewAutoStartManager() AutoStartManager { return newPlatformManager() }

// NewRuntimeStopper 返回只暴露进程停止能力的 RuntimeStopper（control stop 用）。
func NewRuntimeStopper() RuntimeStopper { return newPlatformManager() }

// Sync 是供 TUI write 回调和 CLI set 共用的幂等同步函数。
// 只同步「自启定义」：autostart=true 时确保定义存在且 SpecMatches；
// autostart=false 时确保定义已卸载。不会自动启动或停止进程。
// 返回 (triggered, err)：triggered=true 表示本次执行了 Enable/Disable（非 noop）。
func Sync(cfg *config.Config) (bool, error) { return SyncWith(cfg, NewAutoStartManager()) }

// SyncWith 是可注入 fake AutoStartManager 的 Sync 实现（单测用）。
// 不管当前处于什么不一致状态，都往 cfg.Daemon.AutoStart 指定的目标态推进。
//
// 平台不支持的处理：autostart=true 时返回 ErrPlatformUnsupported（调用方映射为非致命说明）；
// autostart=false 时静默跳过（err=nil）。
func SyncWith(cfg *config.Config, m AutoStartManager) (bool, error) {
	report, err := SyncWithReport(cfg, m)
	return report.Triggered, err
}

// SyncWithReport 在 SyncWith 的兼容返回值之外，保留同步前后 definition 状态，
// 供 ApplyConfig 生成准确的结构化结果。
func SyncWithReport(cfg *config.Config, m AutoStartManager) (SyncReport, error) {
	var report SyncReport
	if cfg == nil {
		return report, errors.New("同步自启定义时 config 不能为 nil")
	}
	if m == nil {
		return report, errors.New("同步自启定义时 AutoStartManager 不能为 nil")
	}
	opts, err := buildOptionsChecked(cfg)
	if err != nil {
		return report, err
	}
	st, err := m.Status(opts)
	report.Before = st
	report.After = st
	if errors.Is(err, ErrPlatformUnsupported) {
		report.Unsupported = true
		if cfg.Daemon.AutoStart {
			return report, err // 试图开启自启但平台不支持 → 报错
		}
		return report, nil // 关闭自启且平台不支持 → 静默跳过
	}
	if err != nil {
		return report, fmt.Errorf("检测服务状态失败: %w", err)
	}

	switch {
	case cfg.Daemon.AutoStart && !st.Exists:
		// 目标开启但定义缺失 → 写定义文件（不启动进程：下次登录/重启才触发）
		report.Triggered = true
		if err := m.Enable(opts); err != nil {
			return report, err
		}
		report.After = AutoStartStatus{Exists: true, SpecMatches: true}
		return report, nil

	case cfg.Daemon.AutoStart && st.Exists && !st.SpecMatches:
		// 已存在但定义漂移（BinPath/Args/日志路径变了）→ 删旧定义 + 写新定义。
		// Enable/Disable 都不碰进程，旧守护进程继续跑，用户需手动 stop + start 才能用新配置。
		report.Triggered = true
		if err := m.Disable(opts); err != nil {
			return report, fmt.Errorf("更新服务定义失败（清理旧定义时）: %w", err)
		}
		report.After = AutoStartStatus{Exists: false}
		if err := m.Enable(opts); err != nil {
			return report, err
		}
		report.DriftRepaired = true
		report.After = AutoStartStatus{Exists: true, SpecMatches: true}
		return report, nil

	case !cfg.Daemon.AutoStart && st.Exists:
		// 目标关闭但定义仍存在 → 删定义文件（不停止进程：进程继续跑直到用户 stop 或 reboot/登录）
		report.Triggered = true
		if err := m.Disable(opts); err != nil {
			return report, err
		}
		report.After = AutoStartStatus{Exists: false}
		return report, nil

	default:
		// autostart=true && Exists && SpecMatches → 定义已一致，无需操作
		// autostart=false && !Exists → 已收敛，无需操作
		// 即使进程当前未运行也不自动停止——那是 stop 的职责
		return report, nil
	}
}

// buildOptions 从 config 构造 Options：探测二进制路径，固定 Args=["_run"]。
// DataDir 来自用户配置层（LoadUserConfig 不展开 ~），服务定义要求绝对路径
// （macOS plist 的 StandardOutPath、Windows 注册表路径同理），故在构造 Options 时展开 ~。
// 注意：不修改 cfg.DataDir 本身，保留用户配置原值以便 marshal 回写保持可移植性。
var executablePath = os.Executable

func buildOptionsChecked(cfg *config.Config) (Options, error) {
	if cfg == nil {
		return Options{}, errors.New("构造自启参数时 config 不能为 nil")
	}
	bin, err := executablePath()
	if err != nil {
		return Options{}, fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	if strings.TrimSpace(bin) == "" {
		return Options{}, errors.New("当前可执行文件路径为空")
	}
	return Options{
		Label:   Label,
		BinPath: bin,
		DataDir: expandTilde(cfg.DataDir),
		Args:    []string{"_run"},
	}, nil
}

func buildOptions(cfg *config.Config) Options {
	opts, _ := buildOptionsChecked(cfg)
	return opts
}

// expandTilde 展开 ~ 或 ~/... 前缀为 $HOME（与 config.expandTilde 等价，service 包内私有副本）。
// 选择复制而非导出 config 包函数，避免为单一工具函数改动 config 公共 API。
func expandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
