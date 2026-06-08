// internal/cli/restart.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
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
		Short: "重启当前守护进程",
		Long: "重启当前守护进程（在单次进程控制锁内停旧起新）。\n\n" +
			"仅重启当前运行的守护进程，不修改 config、plist 或注册表等自启定义：\n" +
			"下次登录/重启是否自启仍由 config 的 daemon.autostart 决定。\n\n" +
			"macOS 取舍：若旧进程由 launchd 启动，stop 会 bootout 当前 job，随后以\n" +
			"detached 方式 start；plist 定义保留但本次会话失去 KeepAlive。\n" +
			"如需恢复 KeepAlive 托管，请使用 config 命令重新保存自启定义。",
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
		// 未运行：ErrRestartNotRunning 的文案已含「请使用 token-usage start」，
		// 写 stderr 并返回错误（非零退出），提示用户先 start。
		if errors.Is(err, control.ErrRestartNotRunning) {
			fmt.Fprintf(cmd.ErrOrStderr(), "%v\n", err)
			return err
		}
		// 其它真实失败：写 stderr、退出非 0。
		fmt.Fprintf(cmd.ErrOrStderr(), "重启守护进程失败: %v\n", err)
		return err
	}

	fmt.Fprintf(out, "✓ 守护进程已重启（PID %d → %d）\n", res.OldPID, res.NewPID)
	return nil
}
