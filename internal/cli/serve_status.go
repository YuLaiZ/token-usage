// internal/cli/serve_status.go
package cli

// serve_status.go 实现 `token-usage serve status`：读 serve.json 并以
// /api/meta 探活分诊——无状态文件（或损坏）报未运行；有且响应报运行中；
// 有但无响应判定为陈旧状态；损坏文件与陈旧状态同样在报告前删除（与文档
// 承诺一致）。退出码恒 0。
//
// 「读状态 → 探活判定 → 陈旧/损坏删除」整段在 serve-state 状态迁移锁内进行，
// 删除一律走 removeServeStateIfSame 条件删除：锁内重读发现文件已被新实例改写
// 时不删，改用当前状态重新探活——响应则按运行中的新实例报告（不删任何东西），
// 无响应才删除并报未运行。这消除了「判定陈旧后、删除前新实例恰好写出新
// serve.json」的 TOCTOU 误删窗口。

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// serveStatusProbeTimeout 是 status 对已记录地址的单次探活超时。
var serveStatusProbeTimeout = 2 * time.Second

// newServeStatusCmd 构造 `serve status`。--addr/--open 继承自 serve 的
// persistent flags（status 自身不使用，仅保持命令族 flag 面一致）。
func newServeStatusCmd(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: ui.Bi("Show whether the dashboard server is running in the background", "查看仪表板后台服务运行状态"),
		Long: ui.Bi(
			"Show whether the dashboard server is running in the background. The check reads serve.json under the data directory (~/.token-usage/serve.json by default) and probes the recorded address on /api/meta: a response reports running with URL, PID and start time; no response means the state is stale (left by a crash or SIGKILL) and the file is removed before reporting not running, and a corrupt state file is removed the same way. Every state outcome exits 0; only unexpected I/O failures exit non-zero.\n\nExamples:\n  token-usage serve status",
			"查看仪表板服务是否在后台运行。检查方式为读取数据目录下的 serve.json（默认 ~/.token-usage/serve.json）并对记录地址的 /api/meta 探活：有响应则报告运行中（含 URL、PID 与启动时间）；无响应说明是崩溃或 SIGKILL 遗留的陈旧状态，会在报告未运行前删除该文件；损坏的状态文件同样删除后再报告。所有状态结论均以退出码 0 返回；只有意外的 I/O 失败才非零。\n\n示例：\n  token-usage serve status",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			// 状态迁移锁：「读-判-删」整段持锁（status 无信号等待段，全程毫秒
			// 级至探活超时的秒级），保证判定所据内容与被删内容一致。
			stateLock, err := acquireServeStateLock(cfg.DataDir)
			if err != nil {
				return err
			}
			defer releaseServeStateLock(stateLock)

			st, err := readServeState(cfg.DataDir)
			if err != nil {
				if errors.Is(err, errServeStateCorrupt) {
					// 损坏的状态文件无法定位实例：持锁期间内容稳定，直接删除
					// 残留后按未运行如实报告。
					if rmErr := removeServeState(cfg.DataDir); rmErr != nil {
						return fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
					}
					fmt.Fprintln(out, ui.Bi("serve is not running (corrupt state removed)", "仪表板未在后台运行（已清理损坏的状态文件）"))
					return nil
				}
				return fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
			}
			if st == nil {
				fmt.Fprintln(out, ui.Bi("serve is not running", "仪表板未在后台运行"))
				return nil
			}
			url := "http://" + st.Addr
			if serveMetaAlive(url, serveStatusProbeTimeout) {
				fmt.Fprintf(out, "%s\n", ui.Bi(
					fmt.Sprintf("serve is running: %s (pid %d, since %s)", url, st.PID, serveLocalTime(st.StartedAt)),
					fmt.Sprintf("仪表板正在后台运行：%s（PID %d，自 %s 起）", url, st.PID, serveLocalTime(st.StartedAt)),
				))
				return nil
			}
			// 状态文件在但探活无响应：疑似陈旧状态。条件删除——锁内重读与判定
			// 所据一致才删；不一致说明新实例已接管，不删。
			removed, current, err := removeServeStateIfSame(cfg.DataDir, st)
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), err)
			}
			if removed {
				fmt.Fprintln(out, ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
				return nil
			}
			// 新实例已写出自己的 serve.json：用新状态重新探活——响应则按运行中
			// 报告（不删任何东西）；无响应则当前状态才是真的陈旧，删除后报未运行。
			if current != nil && serveMetaAlive("http://"+current.Addr, serveStatusProbeTimeout) {
				newURL := "http://" + current.Addr
				fmt.Fprintf(out, "%s\n", ui.Bi(
					fmt.Sprintf("serve is running: %s (pid %d, since %s)", newURL, current.PID, serveLocalTime(current.StartedAt)),
					fmt.Sprintf("仪表板正在后台运行：%s（PID %d，自 %s 起）", newURL, current.PID, serveLocalTime(current.StartedAt)),
				))
				return nil
			}
			if rmErr := removeServeState(cfg.DataDir); rmErr != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
			}
			fmt.Fprintln(out, ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
			return nil
		},
	}
}

// serveLocalTime 把 RFC3339 的 started_at 渲染为本地可读形态；解析失败时
// 原样返回，不因状态文件内容异常阻塞 status。
func serveLocalTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
