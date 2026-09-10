package cli

import (
	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// defaultServeAddr 是 serve 命令 --addr 的缺省监听地址:仅绑定回环接口,
// 统计数据不暴露给局域网。
const defaultServeAddr = "127.0.0.1:8619"

func newServeCmd(info buildinfo.Info) *cobra.Command {
	return newServeCmdWithDeps(loadConfig, info.Version)
}

// newServeCmdWithDeps 构造 serve 命令组;load 可注入供包内测试走真实
// 调用链(生产路径传入 loadConfig)。version 是构建信息快照中的
// 版本串,由 /api/meta 透出。数据面只读:不写数据库与配置;本地写入仅限
// 服务生命周期状态文件 serve.json 与后台日志 serve.log(见 serve_dashboard)。
//
// serve 是纯命令组：RunE 只打印帮助并返回成功——不监听端口、不 spawn 进程、
// 不创建 serve.json/serve.lock 等运行态文件（显式 RunE 使 cobra 的 NoArgs
// 校验先生效，`serve <未知子命令>` 按 unknown command 失败而非静默打印帮助）。
// 仪表板只能由 start/status/stop/restart 四个后台动作管理（serve start 拉起
// Hidden 的 _serve-run 子进程）。--addr/--open 定义在 persistent flags 上供
// 子命令继承。
func newServeCmdWithDeps(load func() (*config.Config, error), version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Manage the local dashboard server (start/status/stop/restart) / 管理本地仪表板服务（start/status/stop/restart）",
		Long: ui.Bi(
			"Manage the read-only local HTTP server that serves the built-in dashboard: the embedded HTML page at / with JSON endpoints (/api/meta, /api/dashboard) and SVG charts (/api/chart/{kind}.svg). The dashboard always runs in the background: `serve start` launches it as a detached process, `serve status` / `serve stop` inspect and stop it, and `serve restart` stops any running instance and starts a fresh background one. Bare `token-usage serve` only prints this help — it listens on no port, starts no process, and creates no state files. The server binds to 127.0.0.1 only by default; binding a public address such as 0.0.0.0 exposes your usage data to the local network. The HTTP data surface is strictly read-only — no CORS headers (same-origin use), no database or configuration writes; serve.json is the shared lifecycle state, serve.log is used for background runs, and serve.lock, serve-state.lock, and serve-start.lock coordinate the lifecycle. At most one dashboard instance runs at a time: a second start — regardless of the address — is rejected by a single-instance guard before listening, printing the running instance's URL and PID and exiting 0 idempotently (stop it first with `token-usage serve stop`, or use `token-usage serve restart`). The collection daemon is managed separately by the `daemon` command group.\n\nExamples:\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve status\n  token-usage serve stop\n  token-usage serve restart",
			"管理只读的本地 HTTP 服务，提供内嵌仪表板页面（/）及其 JSON 接口（/api/meta、/api/dashboard）与 SVG 图表（/api/chart/{kind}.svg）。仪表板始终以后台方式运行：`serve start` 以 detached 子进程启动，`serve status` / `serve stop` 查看与停止，`serve restart` 停掉运行中的实例并以全新后台实例接管。裸执行 `token-usage serve` 只显示本帮助——不监听端口、不启动进程、不创建状态文件。默认仅绑定 127.0.0.1；绑定 0.0.0.0 等公网地址会把统计数据暴露给局域网。HTTP 数据面严格只读：不设 CORS 头（按同源使用）、不写数据库与配置；serve.json 是共用的生命周期状态，serve.log 用于后台日志，serve.lock、serve-state.lock 与 serve-start.lock 负责生命周期协调。任意时刻至多一个仪表板实例在运行：第二次 start——无论请求哪个地址——都会在监听之前被单实例守卫拒绝，打印运行中实例的 URL 与 PID 并以退出码 0 幂等返回（要重启请先用 `token-usage serve stop` 停止，或用 `token-usage serve restart` 重启）。采集守护进程由 `daemon` 命令组单独管理。\n\n示例：\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve status\n  token-usage serve stop\n  token-usage serve restart",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	// --addr/--open 放 persistent flags：start/restart 子命令共用同一 flag 集。
	cmd.PersistentFlags().String("addr", defaultServeAddr, ui.Bi(
		"Listen address (default loopback only; binding 0.0.0.0 exposes usage data to the network)",
		"监听地址（默认仅回环；绑定 0.0.0.0 会把用量数据暴露给局域网）",
	))
	cmd.PersistentFlags().Bool("open", false, ui.Bi(
		"Open the dashboard in the default browser after the background server is confirmed up",
		"后台服务确认启动成功后用默认浏览器打开仪表板",
	))

	cmd.AddCommand(
		newServeStartCmd(load, version),
		newServeStatusCmd(load),
		newServeStopCmd(load),
		newServeRestartCmd(load),
	)
	return cmd
}
