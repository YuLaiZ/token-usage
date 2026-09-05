package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newForecastCmd() *cobra.Command {
	return newForecastCmdWithDeps(loadConfig, db.Open, time.Now)
}

func newForecastCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error), now func() time.Time) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forecast",
		Short: "Extrapolate usage from recent daily averages / 按近期日均外推用量",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}

			usageDB, err := open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			// 窗口不含今天(今天尚未结束,计入会低估日均);今天单独呈现至今量。
			today := now()
			day := func(offset int) string { return today.AddDate(0, 0, offset).Format("2006-01-02") }
			q := querier.New(usageDB)
			ctx := cmdContext(cmd)

			todayStats, err := q.StatsBetween(ctx, day(0), day(0))
			if err != nil {
				return err
			}
			last7, err := q.StatsBetween(ctx, day(-7), day(-1))
			if err != nil {
				return err
			}
			last30, err := q.StatsBetween(ctx, day(-30), day(-1))
			if err != nil {
				return err
			}

			return renderForecast(cmd.OutOrStdout(),
				forecastWindowStats{today: todayStats, last7: last7, last30: last30})
		},
	}
	return cmd
}

// forecastWindowStats 是 forecast 渲染的输入:三个窗口的全量统计。
type forecastWindowStats struct {
	today  querier.RangeStats
	last7  querier.RangeStats
	last30 querier.RangeStats
}

// renderForecast 输出用量外推:今日至今、最近 7/30 天的总量与日均(按活跃天
// 平均,与 query summary 的 Daily average 同一口径),再按各自日均线性外推
// 未来 7/30 天(假设未来保持同等活跃强度)。窗口内无数据时该行显示无数据、
// 对应外推行省略。双语只出现在标签位,数值行保持符号化形态避免中英文词序
// 互相割裂。
func renderForecast(w io.Writer, s forecastWindowStats) error {
	fmt.Fprintln(w, ui.Bi("Forecast", "用量外推"))
	fmt.Fprintln(w)

	fmt.Fprintf(w, "%s: %s\n",
		ui.Bi("Today so far", "今日至今"),
		querier.FormatTokens(s.today.Total.TotalTokens))

	writeWindowLine(w, ui.Bi("Last 7 days", "最近 7 天"), 7, s.last7)
	writeWindowLine(w, ui.Bi("Last 30 days", "最近 30 天"), 30, s.last30)

	fmt.Fprintln(w)
	writeProjectionLine(w, ui.Bi("Next 7 days", "未来 7 天"), 7, s.last7)
	writeProjectionLine(w, ui.Bi("Next 30 days", "未来 30 天"), 30, s.last30)
	return nil
}

// writeWindowLine 输出一个历史窗口行:总总量、日均(总量除以活跃天数,整数
// 除法向下取整)与活跃天占比;区间内无数据时显示无数据。
func writeWindowLine(w io.Writer, label string, days int, s querier.RangeStats) {
	if s.ActiveDays == 0 {
		fmt.Fprintf(w, "%s: %s\n", label, ui.Bi("no data", "无数据"))
		return
	}
	avg := s.Total.TotalTokens / s.ActiveDays
	fmt.Fprintf(w, "%s: %s, %s/day (%d/%d %s)\n",
		label, querier.FormatTokens(s.Total.TotalTokens),
		querier.FormatTokens(avg), s.ActiveDays, days,
		ui.Bi("days active", "天有数据"))
}

// writeProjectionLine 输出外推行:窗口日均乘以未来自然天数;窗口无数据时
// 省略该行(外推没有依据,不编造数字)。
func writeProjectionLine(w io.Writer, label string, days int, s querier.RangeStats) {
	if s.ActiveDays == 0 {
		return
	}
	avg := s.Total.TotalTokens / s.ActiveDays
	fmt.Fprintf(w, "%s: %s (%s/day × %d)\n",
		label, querier.FormatTokens(avg*int64(days)), querier.FormatTokens(avg), days)
}
