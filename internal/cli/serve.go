package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// defaultServeAddr 是 serve 命令 --addr 的缺省监听地址:仅绑定回环接口,
// 统计数据不暴露给局域网。
const defaultServeAddr = "127.0.0.1:8619"

// serveShutdownTimeout 是 Ctrl+C 后等待在途请求完成的宽限期。
const serveShutdownTimeout = 3 * time.Second

func newServeCmd(info buildinfo.Info) *cobra.Command {
	return newServeCmdWithDeps(loadConfig, db.Open, info.Version)
}

// newServeCmdWithDeps 构造 serve 命令;load/open 可注入供包内测试走真实
// 调用链(生产路径传入 loadConfig 与 db.Open)。version 是构建信息快照中的
// 版本串,由 /api/meta 透出。数据面只读:不写数据库与配置;本地写入仅限
// 服务生命周期状态文件 serve.json 与后台日志 serve.log(见 serve_dashboard)。
//
// serve 是 nginx 风格的命令族:无子命令时前台运行,Ctrl+C 停止;
// start/status/stop 管理同一仪表板的后台实例(serve start 拉起 Hidden 的
// _serve-run 子进程)。--addr/--open 定义在 persistent flags 上供子命令继承。
func newServeCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error), version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local dashboard server / 启动本地仪表板服务",
		Long: ui.Bi(
			"Start a read-only local HTTP server that serves the built-in dashboard: the embedded HTML page at / with JSON endpoints (/api/meta, /api/dashboard) and SVG charts (/api/chart/{kind}.svg). The server binds to 127.0.0.1 only by default; binding a public address such as 0.0.0.0 exposes your usage data to the local network. The HTTP data surface is strictly read-only — no CORS headers (same-origin use), no database or configuration writes; serve.json is the shared lifecycle state, serve.log is used for background runs, and serve.lock, serve-state.lock, and serve-start.lock coordinate the lifecycle. At most one dashboard instance (foreground or background) runs at a time: a second serve — in either form, regardless of the address — is rejected by a single-instance guard before listening, printing the running instance's URL and PID and exiting 0 idempotently (stop it first with `token-usage serve stop`, or use `token-usage serve restart`); the serving process holds a serve.lock lifecycle lock in the data directory for its whole lifetime. Press Ctrl+C to stop. For unattended use, `serve start` runs the same dashboard in the background, `serve status` / `serve stop` inspect and stop it, and `serve restart` stops any running instance and starts a fresh background one.\n\nExamples:\n  token-usage serve\n  token-usage serve --open\n  token-usage serve --addr 127.0.0.1:9000\n  token-usage serve start\n  token-usage serve status\n  token-usage serve stop\n  token-usage serve restart",
			"启动只读的本地 HTTP 服务，提供内嵌仪表板页面（/）及其 JSON 接口（/api/meta、/api/dashboard）与 SVG 图表（/api/chart/{kind}.svg）。默认仅绑定 127.0.0.1；绑定 0.0.0.0 等公网地址会把统计数据暴露给局域网。HTTP 数据面严格只读：不设 CORS 头（按同源使用）、不写数据库与配置；serve.json 是前后台共用的生命周期状态，serve.log 用于后台日志，serve.lock、serve-state.lock 与 serve-start.lock 负责生命周期协调。任意时刻至多一个仪表板实例（前台或后台）在运行：第二个 serve——无论前台后台、无论请求哪个地址——都会在监听之前被单实例守卫拒绝，打印运行中实例的 URL 与 PID 并以退出码 0 幂等返回（要重启请先用 `token-usage serve stop` 停止，或用 `token-usage serve restart` 重启）；服务主体在其整个生命周期持有数据目录下的 serve.lock 生命周期锁。按 Ctrl+C 停止。无人值守场景可用 `serve start` 把同一仪表板转入后台运行，`serve status` / `serve stop` 查看与停止，`serve restart` 停掉运行中的实例并以全新后台实例接管。\n\n示例：\n  token-usage serve\n  token-usage serve --open\n  token-usage serve --addr 127.0.0.1:9000\n  token-usage serve start\n  token-usage serve status\n  token-usage serve stop\n  token-usage serve restart",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			autoOpen, _ := cmd.Flags().GetBool("open")

			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			usageDB, err := open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			return serveDashboard(cfg, usageDB, version, addr, cmd.OutOrStdout(), cmd.ErrOrStderr(), autoOpen)
		},
	}

	// --addr/--open 放 persistent flags：foreground 与 start/status/stop 子命令
	// 共用同一 flag 集（行为与抽取前的前台一致）。
	cmd.PersistentFlags().String("addr", defaultServeAddr, ui.Bi(
		"Listen address (default loopback only; binding 0.0.0.0 exposes usage data to the network)",
		"监听地址（默认仅回环；绑定 0.0.0.0 会把用量数据暴露给局域网）",
	))
	cmd.PersistentFlags().Bool("open", false, ui.Bi(
		"Open the dashboard in the default browser after startup",
		"启动后用默认浏览器打开仪表板",
	))

	cmd.AddCommand(
		newServeStartCmd(load, version),
		newServeStatusCmd(load),
		newServeStopCmd(load),
		newServeRestartCmd(load),
	)
	return cmd
}
