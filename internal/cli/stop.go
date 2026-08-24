// internal/cli/stop.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newStopCmd 停止守护进程。
// 经 control.Manager.Stop：control lock 内 load config → inspect daemon lock
// → 未运行幂等返回 → 运行中按平台停止（macOS bootout→查 lock→必要时 SIGTERM；Windows taskkill 准确 PID）
// → 以 daemon lock 释放为成功条件；超时返回错误，不删 PID 伪装成功。
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon / 停止守护进程",
		Long: ui.Bi("Stop the currently running daemon.\n\n"+
			"Only stops the current process and does not modify the autostart definition: whether it autostarts on next login/reboot is still decided by\n"+
			"daemon.autostart in config. To disable autostart, use the config command to set daemon.autostart=false.",
			"停止当前运行的守护进程。\n\n"+
				"仅停止当前进程，不修改开机自启定义：下次登录/重启是否自启仍由 config 的\n"+
				"daemon.autostart 决定。如需关闭自启，请使用 config 命令设置 daemon.autostart=false。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(cmd)
		},
	}
}

// runStop 抽出便于测试。
func runStop(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	res, err := mgr.Stop(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …），命令只补充上下文返回，不再手写 stderr。
		return fmt.Errorf("%s: %w", ui.Bi("failed to stop daemon", "停止守护进程失败"), err)
	}

	if !res.WasRunning {
		// 未运行：stdout 显示未运行，退出码 0
		fmt.Fprintln(out, ui.Bi("daemon not running", "守护进程未运行"))
		return nil
	}

	fmt.Fprintf(out, "✓ %s（PID %d）\n", ui.Bi("daemon stopped", "守护进程已停止"), res.PID)
	return nil
}
