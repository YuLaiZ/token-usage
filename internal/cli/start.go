// internal/cli/start.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// controlStartStopper 是 start/stop/status/restart 命令需要的 control.Manager 子集。
// 用接口而非 *control.Manager，使 CLI 测试可注入 stub 覆盖结果合同（已运行/未运行/失败）。
type controlStartStopper interface {
	Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error)
	Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error)
	Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error)
	Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error)
}

// realControlStartStopper 适配 *control.Manager 到 controlStartStopper（直接转发，类型一致）。
type realControlStartStopper struct{ m *control.Manager }

func (r realControlStartStopper) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	return r.m.Start(ctx, load)
}
func (r realControlStartStopper) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return r.m.Stop(ctx, load)
}
func (r realControlStartStopper) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return r.m.Restart(ctx, load)
}
func (r realControlStartStopper) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return r.m.Inspect(ctx, cfg)
}

// controlManagerFactory 是 controlStartStopper 的工厂（默认走 *control.Manager）。
// 测试覆盖以注入 stub，避免触碰真实 home/文件系统。
var controlManagerFactory = func() (controlStartStopper, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}
	return realControlStartStopper{m: mgr}, nil
}

// newStartCmd 后台启动守护进程（立即返回，nginx 风格）。
// 经 control.Manager.Start：control lock 内 load config → inspect daemon lock
// → 已运行返回 PID（不 spawn）→ 未运行 spawn _run → 等 PID+daemon lock 就绪。
func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background (nginx-style) / 后台启动守护进程（立即返回，nginx 风格）",
		Long: ui.Bi("Start the daemon in the background (returns immediately, nginx-style).\n\n"+
			"What gets started is the currently running daemon (the live collection/analysis monitor),\n"+
			"separate from the autostart definition: autostart is decided by daemon.autostart in config;\n"+
			"this command only starts the current process and does not touch the autostart definition.",
			"后台启动守护进程（立即返回，nginx 风格）。\n\n"+
				"启动的是当前运行的守护进程（采集/分析的实时监控进程），\n"+
				"与开机自启定义分离：开机自启由 config 的 daemon.autostart 决定，\n"+
				"本命令只启动当前进程，不修改自启定义。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd)
		},
	}
}

// startConfigLoader 是 start 前置检查用的配置加载函数（默认 loadConfig），
// 测试注入替身以覆盖「全关/仅 router/有 enabled」三态，不依赖开发机配置。
var startConfigLoader = loadConfig

// runStart 抽出便于测试。
func runStart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	// 前置拦截：配置上无任何监控目标时，daemon 启动后必然「无存活监控」失败，
	// 等待就绪只会白耗 5 秒超时。提前提示引导先启用客户端；已在运行的实例不
	// 拦截（走原 AlreadyRunning 输出）；load/inspect 失败降级放行，由 Start
	// 内部权威路径兜底。
	if cfg, cfgErr := startConfigLoader(); cfgErr == nil {
		if st, insErr := mgr.Inspect(cmdContext(cmd), cfg); insErr == nil && !st.Running {
			if !analyzer.HasMonitorTargets(cfg) {
				fmt.Fprintln(out, "⚠ "+ui.Bi("No enabled clients; the daemon would have nothing to monitor",
					"没有任何已启用的客户端，守护进程将无事可做"))
				fmt.Fprintln(out, "  "+ui.Bi("Enable at least one client before starting",
					"请先启用客户端再启动")+":")
				fmt.Fprintln(out, "  token-usage config set clients.claude.enabled true")
				return errors.New(ui.Bi("no enabled clients, start aborted", "没有已启用的客户端，已取消启动"))
			}
		}
	}

	res, err := mgr.Start(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …），命令只补充上下文返回，不再手写 stderr。
		return fmt.Errorf("%s: %w", ui.Bi("failed to start daemon", "启动守护进程失败"), err)
	}

	if res.AlreadyRunning {
		// 已运行：stdout 显示当前 PID，退出码 0
		if res.PID > 0 {
			fmt.Fprintf(out, "%s（PID %d）\n", ui.Bi("daemon already running", "守护进程已在运行"), res.PID)
		} else {
			fmt.Fprintln(out, ui.Bi("daemon already running", "守护进程已在运行"))
		}
		return nil
	}

	fmt.Fprintf(out, "✓ %s（PID %d）\n", ui.Bi("daemon started", "守护进程已启动"), res.PID)
	return nil
}
