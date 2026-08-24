// internal/cli/start.go
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
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
		return nil, fmt.Errorf("获取用户主目录失败: %w", err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("创建进程控制管理器失败: %w", err)
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
		Long: "后台启动守护进程（立即返回，nginx 风格）。\n\n" +
			"启动的是当前运行的守护进程（采集/分析的实时监控进程），\n" +
			"与开机自启定义分离：开机自启由 config 的 daemon.autostart 决定，\n" +
			"本命令只启动当前进程，不修改自启定义。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd)
		},
	}
}

// runStart 抽出便于测试。
func runStart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	res, err := mgr.Start(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 真实失败写 stderr、退出非 0
		fmt.Fprintf(cmd.ErrOrStderr(), "启动守护进程失败: %v\n", err)
		return err
	}

	if res.AlreadyRunning {
		// 已运行：stdout 显示当前 PID，退出码 0
		if res.PID > 0 {
			fmt.Fprintf(out, "守护进程已在运行（PID %d）\n", res.PID)
		} else {
			fmt.Fprintln(out, "守护进程已在运行")
		}
		return nil
	}

	fmt.Fprintf(out, "✓ 守护进程已启动（PID %d）\n", res.PID)
	return nil
}
