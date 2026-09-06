package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// watchLoopSentinel 是循环模式测试的终止哨兵:sleep 注入在计数达标后抛出,
// 外层 recover 断言帧数,避免测试真实死循环。
var watchLoopSentinel = fmt.Errorf("watch loop sentinel")

func newWatchCmd() *cobra.Command {
	return newWatchCmdWithDeps(loadConfig, db.Open, time.Now, time.Sleep)
}

func newWatchCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error),
	now func() time.Time, sleep func(time.Duration)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch [DATE|DATE-DATE]",
		Short: "Refresh a live summary at a fixed interval / 以固定间隔刷新实时摘要",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 缺省日期与刷新时间戳同源,都取注入时钟:watch 的 now 是完整
			// 语义注入,缺省"今天"若绕开它读真实时钟,测试注入即失效。
			var dates []string
			var err error
			if len(args) == 0 {
				dates = []string{now().Format("2006-01-02")}
			} else {
				dates, err = parseDateArgs(args, false, "watch")
				if err != nil {
					return err
				}
			}
			interval, err := cmd.Flags().GetDuration("interval")
			if err != nil {
				return err
			}
			if interval < time.Second {
				return fmt.Errorf("%s", ui.Bi(
					"--interval must be at least 1s",
					"--interval 至少为 1 秒",
				))
			}
			once, _ := cmd.Flags().GetBool("once")

			// --by 校验先于配置加载与开库:非法维度不必触碰任何 I/O。
			by, err := cmd.Flags().GetString("by")
			if err != nil {
				return err
			}
			switch by {
			case "client", "model", "provider", "project":
			default:
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("unknown --by dimension %q (allowed: client, model, provider, project); for temporal trends use `token-usage chart --line`", by),
					fmt.Sprintf("未知 --by 维度 %q（允许：client, model, provider, project）；时间趋势请用 `token-usage chart --line`", by),
				))
			}

			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			usageDB, err := open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			// 交互式循环前启用 ANSI 转义(Windows conhost 需显式开启)。
			if !once {
				enableVT(os.Stdout)
			}

			q := querier.New(usageDB)
			ctx := cmdContext(cmd)
			out := cmd.OutOrStdout()
			// 单帧渲染:错误即返回(数据库损坏等)而非静默闪烁。
			render := func() error {
				summary, err := q.Summary(ctx, dates)
				if err != nil {
					return err
				}
				// 帧内分组表按 --by 维度切换;provider 应用配置别名合并显示。
				var dayView string
				switch by {
				case "client":
					dayView, err = q.ByClient(ctx, dates)
				case "provider":
					dayView, err = q.ByProvider(ctx, dates, cfg.ProviderAliases)
				case "project":
					dayView, err = q.ByProject(ctx, dates)
				default: // model
					dayView, err = q.ByModel(ctx, dates)
				}
				if err != nil {
					return err
				}
				stamp := now().Format("15:04:05")
				var frame bytes.Buffer
				frame.WriteString(ui.Bi(
					fmt.Sprintf("Live watch - refreshed at %s (Ctrl+C to exit)", stamp),
					fmt.Sprintf("实时监视 - 刷新于 %s(Ctrl+C 退出)", stamp)))
				frame.WriteString("\n\n")
				frame.WriteString(summary)
				frame.WriteString("\n")
				frame.WriteString(dayView)
				if _, err := fmt.Fprintln(out, strings.TrimRight(frame.String(), "\n")); err != nil {
					return err
				}
				return nil
			}

			if once {
				return render()
			}

			for {
				// 清屏并归位光标:每帧从左上角重绘,避免残影。
				fmt.Fprint(out, "\x1b[2J\x1b[H")
				if err := render(); err != nil {
					return err
				}
				sleep(interval)
			}
		},
	}

	cmd.Flags().Duration("interval", 5*time.Second, ui.Bi("Refresh interval (minimum 1s)", "刷新间隔(至少 1 秒)"))
	cmd.Flags().Bool("once", false, ui.Bi("Render a single frame and exit", "只渲染一帧后退出"))
	cmd.Flags().String("by", "model", ui.Bi("Grouping dimension for the frame table: client/model/provider/project", "帧内分组表的维度：client/model/provider/project"))
	return cmd
}
