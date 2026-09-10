// internal/cli/serve_start.go
package cli

// serve_start.go 实现 `token-usage serve start`：nginx 风格的后台启动命令层。
// 预检 → spawn → 探活确认的完整编排由 internal/serve.StartInBackground 提供
// （serve start 命令与 update 的 dashboard 自动恢复共用同一编排，update 的
// 恢复显式传入新二进制路径且绝不打开浏览器）；本文件只负责命令参数解析、
// 结果渲染与 --open 的浏览器打开（打开失败不致命，后台服务已就绪）。

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// serveOpenBrowser 是 start --open 的浏览器 seam：默认转发 openBrowser；
// 测试注入以避免拉起真实浏览器。浏览器打开只属于本命令层——update 的自动
// 恢复路径（internal/serve.StartInBackground）不含任何浏览器逻辑。
var serveOpenBrowser = openBrowser

// newServeStartCmd 构造 `serve start`。--addr/--open 继承自 serve 的
// persistent flags。
func newServeStartCmd(load func() (*config.Config, error), version string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: ui.Bi("Start the dashboard server in the background", "后台启动仪表板服务"),
		Long: ui.Bi(
			"Start the read-only dashboard server in the background (nginx-style): this command spawns a detached child that keeps running after it exits, then waits until the child reports ready. State is recorded in serve.json under the data directory (~/.token-usage/serve.json by default) and logs go to serve.log in the same directory; each start truncates the log. If the recorded state still answers on /api/meta the command reports it and returns idempotently with exit code 0 (stop it first with `token-usage serve stop` to restart); stale state left by a crash or SIGKILL — and a corrupt state file — is removed and start proceeds. Concurrent starts are serialized by a serve-start.lock file lock in the data directory (start coordination only — a running instance is described by serve.json and held via the serve.lock lifecycle lock): a second start while another is still working fails with a retry hint. Use `token-usage serve status` to check and `token-usage serve stop` to stop. With --open the browser is opened by this command after the background server is confirmed up.\n\nExamples:\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve start --open",
			"以后台方式（nginx 风格）启动只读仪表板服务：本命令拉起一个 detached 子进程并在退出后继续运行，然后等待子进程报告就绪。状态记录在数据目录下的 serve.json（默认 ~/.token-usage/serve.json），日志写入同目录的 serve.log；每次启动都会截断日志。若已记录的状态在 /api/meta 上仍有响应则报告已在运行并以退出码 0 幂等返回（要重启请先用 `token-usage serve stop` 停止）；崩溃或 SIGKILL 遗留的陈旧状态与损坏的状态文件会被清理并照常启动。并发启动由数据目录下的 serve-start.lock 文件锁串行化（仅用于启动协调——运行中的实例由 serve.json 描述、以 serve.lock 生命周期锁持有）：另一个 serve start 尚在执行时报错并提示稍后重试。用 `token-usage serve status` 查看状态、`token-usage serve stop` 停止。带 --open 时由本命令在确认后台启动成功后打开浏览器。\n\n示例：\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve start --open",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			autoOpen, _ := cmd.Flags().GetBool("open")
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			return serveStartRun(cfg, addr, autoOpen, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

// serveStartRun 执行 serve start 命令层：后台启动编排（serve.StartInBackground）
// → 结果渲染 →（--open 时开浏览器）。由 `serve start` 与 `serve restart` 共用：
// restart 在调用本函数前先完成 stop 编排。返回值语义与 start 的 RunE 一致
// （已运行为幂等成功 exit 0）。
func serveStartRun(cfg *config.Config, addr string, autoOpen bool, out, errOut io.Writer) error {
	res, err := serve.StartInBackground(serve.StartOptions{
		DataDir: cfg.DataDir,
		Addr:    addr,
	})
	if err != nil {
		return err
	}
	// 已有存活实例：幂等成功，输出既有拒绝文案并以退出码 0 返回。
	if res.AlreadyRunning {
		fmt.Fprintf(out, "%s\n", ui.Bi(
			fmt.Sprintf("serve is already running (pid %d, http://%s); stop it first with token-usage serve stop", res.PID, res.Addr),
			fmt.Sprintf("仪表板已在后台运行（PID %d，http://%s）；请先用 token-usage serve stop 停止", res.PID, res.Addr)))
		return nil
	}

	url := "http://" + res.Addr
	// 交互终端下用 OSC 8 链接包裹 URL，支持单击打开（非 TTY 自动降级纯文本）。
	linked := hyperlinkURL(url, writerIsTerminal(out))
	fmt.Fprintf(out, "%s\n", ui.Bi(
		fmt.Sprintf("dashboard started in background at %s (pid %d)", linked, res.PID),
		fmt.Sprintf("仪表板已后台启动 %s（PID %d）", linked, res.PID),
	))
	fmt.Fprintf(out, "%s\n", ui.Bi(
		fmt.Sprintf("log: %s · stop: token-usage serve stop", serve.LogPath(cfg.DataDir)),
		fmt.Sprintf("日志 %s · 停止 token-usage serve stop", serve.LogPath(cfg.DataDir)),
	))

	if autoOpen {
		// 打开浏览器失败不致命：打印警告，后台服务已就绪。
		if err := serveOpenBrowser(url); err != nil {
			fmt.Fprintf(errOut, "%s\n", ui.Bi(
				fmt.Sprintf("failed to open browser: %v", err),
				fmt.Sprintf("打开浏览器失败：%v", err),
			))
		}
	}
	return nil
}
