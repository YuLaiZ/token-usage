// internal/cli/restart.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newRestartCmd 重启当前守护进程：在单次 control lock 内停旧起新。
// 经 control.Manager.Restart：Acquire control lock → load config → inspect daemon lock
// → 未运行返回 ErrRestartNotRunning → 运行中 stopLocked 等旧 daemon lock 释放
// → startLocked spawn 新 child 等 PID+daemon lock 就绪。
//
// 全流程不触碰 config、plist 或注册表：stop 是 bootout/SIGTERM（保留定义），
// start 是 detached spawn。macOS 若旧进程由 launchd 启动，stop 会 bootout 当前 job，
// 随后以 detached 方式 start；plist 定义保留，但本次会话失去 KeepAlive（已接受取舍，
// 不增加 kickstart 或隐式 bootstrap）。如需恢复 KeepAlive 托管，请用 config 重存自启定义。
func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon / 重启当前守护进程",
		Long: ui.Bi("Restart the currently running daemon (stop the old and start the new within a single process control lock).\n\n"+
			"Only restarts the currently running daemon and does not modify autostart definitions in config, plist or the registry:\n"+
			"whether it autostarts on next login/reboot is still decided by daemon.autostart in config.\n\n"+
			"macOS trade-off: if the old process was started by launchd, stop boots out the current job and then\n"+
			"starts detached; the plist definition is kept but this session loses KeepAlive.\n"+
			"To restore KeepAlive management, re-save the autostart definition via the config command.",
			"重启当前守护进程（在单次进程控制锁内停旧起新）。\n\n"+
				"仅重启当前运行的守护进程，不修改 config、plist 或注册表等自启定义：\n"+
				"下次登录/重启是否自启仍由 config 的 daemon.autostart 决定。\n\n"+
				"macOS 取舍：若旧进程由 launchd 启动，stop 会 bootout 当前 job，随后以\n"+
				"detached 方式 start；plist 定义保留但本次会话失去 KeepAlive。\n"+
				"如需恢复 KeepAlive 托管，请使用 config 命令重新保存自启定义。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestart(cmd)
		},
	}
}

// runRestart 抽出便于测试。
func runRestart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	res, err := mgr.Restart(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …）。未运行的文案已含「请使用
		// token-usage start」指引，原样返回；其它失败补充上下文。
		if errors.Is(err, control.ErrRestartNotRunning) {
			return err
		}
		return fmt.Errorf("%s: %w", ui.Bi("failed to restart daemon", "重启守护进程失败"), err)
	}

	fmt.Fprintf(out, "✓ %s（PID %d → %d）\n", ui.Bi("daemon restarted", "守护进程已重启"), res.OldPID, res.NewPID)
	return nil
}
