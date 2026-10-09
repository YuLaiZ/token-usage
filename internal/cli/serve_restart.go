// internal/cli/serve_restart.go
package cli

// serve_restart.go 实现 `token-usage serve restart`：stop 编排（幂等、探活
// 判据、接管重路由，见 serve_stop.go）+ start 编排（serve-start.lock 串行化、
// 已运行预检、spawn + 探活确认，见 serve_start.go）的顺序组合。两段各自完整
// 复用既有实现，不引入新的锁序——stop 段只持/释 state 锁，start 段取
// serve-start.lock 并在 spawn 前释放 state 锁，段间无重叠持有。
//
// 语义要点：只有运行中的实例才能重启——stop 段返回 Stopped=false（本就无
// 运行实例：无状态文件、损坏或陈旧残留已被清理）时以非零错误退出并指引
// `serve start`，与 daemon restart 对齐：除显式 start 外任何命令不启动服务。
// stop 段失败（探活判定的实例管不住）时同样以非零错误中止，绝不带着仍占用
// 端口的旧实例进入 start 段。

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// errServeRestartNotRunning 在 restart 时没有运行中的仪表板实例（无状态
// 文件，或陈旧/损坏残留已由 stop 段清理）时返回，文案自带改用 serve start
// 的指引。与 control.ErrRestartNotRunning 的模式对齐；定义在 cli 层是因为
// restart 的组合编排在命令层（internal/serve 只有 stop/start 编排）。
var errServeRestartNotRunning = errors.New(ui.Bi("serve is not running, run token-usage serve start", "仪表板未在后台运行，请使用 token-usage serve start"))

// newServeRestartCmd 构造 `serve restart`。--addr/--open 继承自 serve 的
// persistent flags，语义与 `serve start` 一致。
func newServeRestartCmd(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: ui.Bi("Restart the dashboard server in the background", "后台重启仪表板服务"),
		Long: ui.Bi(
			"Restart the dashboard server in the background: first stop any running instance with exactly the same orchestration as `serve stop` (probe-verified — any recorded running instance is stopped gracefully, regardless of how it was started), then start a fresh background instance exactly like `serve start`. Only a running instance can be restarted: with no instance running the command exits non-zero with a hint to run `token-usage serve start` — matching `daemon restart` (a stale or corrupt state file is cleaned up first and then reported the same way). The --addr and --open flags apply to the freshly started background instance and have no effect when restart errors out. If the running instance cannot be stopped (it still answers after the SIGKILL fallback), restart aborts with a non-zero error and the old instance keeps serving; use `token-usage serve stop` to inspect that case. Stopping and starting are the same orchestrations as the standalone commands, so all their guarantees — probe-verified stop, stale/corrupt state cleanup, start serialization — apply unchanged.\n\nExamples:\n  token-usage serve restart\n  token-usage serve restart --addr 127.0.0.1:9000\n  token-usage serve restart --open",
			"以后台方式重启仪表板服务：先以与 `serve stop` 完全相同的编排停止运行中的实例（以探活为判据——任何运行中的记录实例都会被优雅停止），再以与 `serve start` 完全相同的方式拉起全新的后台实例。只有运行中的实例才能重启：当前没有实例在运行时命令以非零错误退出并提示使用 `token-usage serve start`——与 `daemon restart` 对齐（陈旧或损坏的状态文件会先被清理，随后同样报错）。--addr 与 --open 作用于新启动的后台实例，报错路径上无效果。若运行中的实例无法停止（SIGKILL 兜底后仍在响应），重启以非零错误中止，旧实例继续服务；此类场景请用 `token-usage serve stop` 排查。停止与启动两段就是独立命令的原编排，全部保证——探活判停、陈旧/损坏状态清理、启动串行化——原样生效。\n\n示例：\n  token-usage serve restart\n  token-usage serve restart --addr 127.0.0.1:9000\n  token-usage serve restart --open",
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
			res, err := serveStopRun(cfg, out)
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("restart aborted: failed to stop the running dashboard", "重启中止：停止运行中的仪表板失败"), err)
			}
			// 未运行（含陈旧/损坏残留清理后）：restart 的对象不存在，报错
			// 指引 serve start——与 daemon restart 对齐，不隐式拉起。
			if !res.Stopped {
				return errServeRestartNotRunning
			}

			// start 段：与 serve start 逐字节相同的编排与输出。
			return serveStartRun(cfg, addr, autoOpen, out, errOut)
		},
	}
}
