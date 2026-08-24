// internal/cli/stop.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// newStopCmd 停止守护进程。
// 经 control.Manager.Stop：control lock 内 load config → inspect daemon lock
// → 未运行幂等返回 → 运行中按平台停止（macOS bootout→查 lock→必要时 SIGTERM；Windows taskkill 准确 PID）
// → 以 daemon lock 释放为成功条件；超时返回错误，不删 PID 伪装成功。
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon / 停止守护进程",
		Long: "停止当前运行的守护进程。\n\n" +
			"仅停止当前进程，不修改开机自启定义：下次登录/重启是否自启仍由 config 的\n" +
			"daemon.autostart 决定。如需关闭自启，请使用 config 命令设置 daemon.autostart=false。",
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
		// 真实失败写 stderr、退出非 0
		fmt.Fprintf(cmd.ErrOrStderr(), "停止守护进程失败: %v\n", err)
		return err
	}

	if !res.WasRunning {
		// 未运行：stdout 显示未运行，退出码 0
		fmt.Fprintln(out, "守护进程未运行")
		return nil
	}

	fmt.Fprintf(out, "✓ 守护进程已停止（PID %d）\n", res.PID)
	return nil
}
