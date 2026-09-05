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

// frameSeparator 是非 TTY 单帧输出的帧尾分隔:循环模式下每帧之间用清屏
// 序列切帧,重定向时无法清屏,单帧模式以分隔行显式定界。
const frameSeparator = "────────────────────────────────────────"

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
			dates, err := parseDateArgs(args, true, "watch")
			if err != nil {
				return err
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
				dayView, err := q.ByModel(ctx, dates)
				if err != nil {
					return err
				}
				var frame bytes.Buffer
				frame.WriteString(ui.Bi(
					fmt.Sprintf("Live watch - refreshed at %s (Ctrl+C to exit)",
						now().Format("15:04:05")),
					fmt.Sprintf("实时监视 - 刷新于 %s(Ctrl+C 退出)",
						now().Format("15:04:05"))))
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
	return cmd
}
