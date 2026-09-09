// internal/cli/serve_restart.go
package cli

// serve_restart.go 实现 `token-usage serve restart`：stop 编排（幂等、探活
// 判据、接管重路由，见 serve_stop.go）+ start 编排（serve-start.lock 串行化、
// 已运行预检、spawn + 探活确认，见 serve_start.go）的顺序组合。两段各自完整
// 复用既有实现，不引入新的锁序——stop 段只持/释 state 锁，start 段取
// serve-start.lock 并在 spawn 前释放 state 锁，段间无重叠持有。
//
// 语义要点：restart 的最终形态永远是后台实例——前台 Ctrl+C 会话同样会被
// stop 段以 SIGTERM 优雅停掉，再由 start 段拉起 detached 子进程接管；未运行
// 时等价于 serve start。stop 段失败（探活判定的实例管不住）时以非零错误中止，
// 绝不带着仍占用端口的旧实例进入 start 段。

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newServeRestartCmd 构造 `serve restart`。--addr/--open 继承自 serve 的
// persistent flags，语义与 `serve start` 一致。
func newServeRestartCmd(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: ui.Bi("Restart the dashboard server in the background", "后台重启仪表板服务"),
		Long: ui.Bi(
			"Restart the dashboard server in the background: first stop any running instance with exactly the same orchestration as `serve stop` (probe-verified — a foreground Ctrl+C session is stopped gracefully as well), then start a fresh background instance exactly like `serve start`. With no instance running it simply starts one. The --addr and --open flags apply to the freshly started background instance. If the running instance cannot be stopped (it still answers after the SIGKILL fallback), restart aborts with a non-zero error and the old instance keeps serving; use `token-usage serve stop` to inspect that case. Stopping and starting are the same orchestrations as the standalone commands, so all their guarantees — probe-verified stop, stale/corrupt state cleanup, start serialization — apply unchanged.\n\nExamples:\n  token-usage serve restart\n  token-usage serve restart --addr 127.0.0.1:9000\n  token-usage serve restart --open",
			"以后台方式重启仪表板服务：先以与 `serve stop` 完全相同的编排停止运行中的实例（以探活为判据——前台 Ctrl+C 会话同样会被优雅停止），再以与 `serve start` 完全相同的方式拉起全新的后台实例；当前没有实例在运行时等价于直接启动。--addr 与 --open 作用于新启动的后台实例。若运行中的实例无法停止（SIGKILL 兜底后仍在响应），重启以非零错误中止，旧实例继续服务；此类场景请用 `token-usage serve stop` 排查。停止与启动两段就是独立命令的原编排，全部保证——探活判停、陈旧/损坏状态清理、启动串行化——原样生效。\n\n示例：\n  token-usage serve restart\n  token-usage serve restart --addr 127.0.0.1:9000\n  token-usage serve restart --open",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			autoOpen, _ := cmd.Flags().GetBool("open")
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}

			// stop 段：幂等（未运行照常成功）、探活判停、接管重路由——
			// 全部复用 serveStopRun 的原编排。失败即中止重启：旧实例仍占着
			// 端口，继续 start 只会得到一次注定失败的监听与误导性报错。
			if err := serveStopRun(cfg, out); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("restart aborted: failed to stop the running dashboard", "重启中止：停止运行中的仪表板失败"), err)
			}

			// start 段：与 serve start 逐字节相同的编排与输出。
			return serveStartRun(cfg, addr, autoOpen, out, errOut)
		},
	}
}
