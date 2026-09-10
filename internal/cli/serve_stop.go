// internal/cli/serve_stop.go
package cli

// serve_stop.go 实现 `token-usage serve stop` 命令层。停止编排（serve-state
// 锁内「读状态 → 探活判定 → 陈旧/损坏条件删除」→ 平台信号 → 探活判停 →
// 条件删除；接管重路由）由 internal/serve.StopRun 提供，serve stop 命令与
// update 的「更新前停止 dashboard」共用同一编排：命令层传 out 获得与既有
// 行为逐字节一致的文案；update 传 nil（静默）并消费结构化结果。

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newServeStopCmd 构造 `serve stop`。--addr/--open 继承自 serve 的
// persistent flags（stop 自身不使用，仅保持命令族 flag 面一致）。
func newServeStopCmd(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: ui.Bi("Stop the dashboard server running in the background", "停止后台运行的仪表板服务"),
		Long: ui.Bi(
			"Stop the dashboard server running in the background. The command reads serve.json under the data directory (~/.token-usage/serve.json by default), sends the stop signal to the recorded PID (SIGTERM on Unix with a 3s graceful window, then SIGKILL as a fallback; taskkill /F on Windows — Windows console processes have no cross-process graceful-stop channel, which is acceptable for a strictly read-only service), and confirms the stop by probing /api/meta until it stops responding. Success is judged solely by the probe — the recorded PID may have been reused by an unrelated process, so the probe wins over the signal result, and a failed signal delivery does not short-circuit the probe wait. The state file is removed only after the probe confirms shutdown; stale state (no response before signalling) and a corrupt state file are cleaned up too. If the server still responds after the SIGKILL fallback (whether or not the kill itself reported an error), the command exits non-zero with the recorded URL and PID and keeps serve.json in place for manual inspection. Stopping an already-stopped server is a no-op that still exits 0.\n\nExamples:\n  token-usage serve stop",
			"停止后台运行的仪表板服务。命令读取数据目录下的 serve.json（默认 ~/.token-usage/serve.json），向记录的 PID 发送停止信号（Unix 为 SIGTERM 并给 3s 优雅窗口，超时以 SIGKILL 兜底；Windows 为 taskkill /F——Windows 控制台进程没有跨进程的优雅停止通道，对严格只读的服务可接受），并以探活 /api/meta 直至不再响应来确认停止。是否停止成功仅以探活为准：记录的 PID 可能已被无关进程复用，探活的结论优先于信号发送结果，信号投递失败也不会短路探活等待。只有探活确认下线后才删除状态文件；陈旧状态（信号前即无响应）与损坏的状态文件同样会被清理。若强杀兜底后服务仍在响应（无论强杀本身是否报错），命令以非零退出码报错并列出记录的 URL 与 PID，保留 serve.json 供人工检查。对已停止的服务重复执行是幂等空操作，退出码仍为 0。\n\n示例：\n  token-usage serve stop",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			return serveStopRun(cfg, cmd.OutOrStdout())
		},
	}
}

// serveStopRun 执行一次完整的 stop 编排（internal/serve.StopRun 的命令层薄壳，
// 文案由编排原样透出）。由 `serve stop` 与 `serve restart` 共用。
func serveStopRun(cfg *config.Config, out io.Writer) error {
	_, err := serve.StopRun(cfg.DataDir, out)
	return err
}
