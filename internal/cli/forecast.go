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
		Short: "Estimate future usage from recent daily averages / 按近期日均估算未来用量",
		Long: ui.Bi(
			`Estimates upcoming usage from recent daily averages. Windows exclude today (an unfinished day would understate the average); today is shown separately as its running total. Last 7 days covers today-7..today-1 and last 30 days covers today-30..today-1; the daily average divides the window total by active days (integer division), and each projection multiplies that average by the coming natural days, assuming activity continues at the same intensity. A window with no data shows no data and its projection is omitted. --format selects the output format: table (default, the text panel) or json (machine-readable: raw integers, two-space indentation); invalid values are rejected before the database opens. The command is read-only.`,
			`按近期日均估算即将到来的用量。窗口不含今天（未结束的一天会拉低日均）；今天单独以「至今」累计量呈现。最近 7 天覆盖今天-7..今天-1，最近 30 天覆盖今天-30..今天-1；日均按窗口总量除以活跃天（整数除法），各预测以该日均乘以未来自然天数，假设未来保持同等活跃强度。窗口无数据时显示无数据并省略对应预测。--format 选择输出格式：table（默认，文本面板）或 json（机器可读、原始整数、两空格缩进）；非法值在打开数据库之前即被拒绝。本命令只读。`,
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --format 白名单先于配置加载与数据库打开校验（句式与 compare 一致）。
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format != "table" && format != "json" {
				return forecastFormatError(format)
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

			stats := forecastWindowStats{today: todayStats, last7: last7, last30: last30}
			if format == "json" {
				return renderForecastJSON(cmd.OutOrStdout(), stats)
			}
			return renderForecast(cmd.OutOrStdout(), stats)
		},
	}
	// --format 是本地 flag,缺省 table;取值白名单在配置加载与开库之前校验。
	cmd.Flags().String("format", "table", ui.Bi(
		"output format: table or json",
		"输出格式：table 或 json",
	))
	return cmd
}

// forecastWindowStats 是 forecast 渲染的输入:三个窗口的全量统计。
type forecastWindowStats struct {
	today  querier.RangeStats
	last7  querier.RangeStats
	last30 querier.RangeStats
}

// renderForecast 输出用量预测:今日至今、最近 7/30 天的总量与日均(按活跃天
// 平均,与 query summary 的 Daily average 同一口径),再按各自日均乘以未来
// 天数估算未来 7/30 天(假设未来保持同等活跃强度)。窗口内无数据时该行显示
// 无数据、对应预测行省略。双语只出现在标签位,数值行保持符号化形态避免
// 中英文词序互相割裂。
func renderForecast(w io.Writer, s forecastWindowStats) error {
	fmt.Fprintln(w, ui.Bi("Forecast", "用量预测"))
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

// writeProjectionLine 输出预测行:窗口日均乘以未来自然天数;窗口无数据时
// 省略该行(预测没有依据,不编造数字)。
func writeProjectionLine(w io.Writer, label string, days int, s querier.RangeStats) {
	if s.ActiveDays == 0 {
		return
	}
	avg := s.Total.TotalTokens / s.ActiveDays
	fmt.Fprintf(w, "%s: %s (%s/day × %d)\n",
		label, querier.FormatTokens(avg*int64(days)), querier.FormatTokens(avg), days)
}

// forecastJSONToday 是 forecast --format json 中今日至今的投影:与表格
// Today so far 行同源,只含请求数与总用量两个字段(原始整数)。
type forecastJSONToday struct {
	Requests    int64 `json:"requests"`
	TotalTokens int64 `json:"total_tokens"`
}

// forecastJSONWindow 是 JSON 中一个历史窗口的投影:窗口名、自然天数、活跃天、
// 请求与总量原始整数,以及按活跃天整数除法的日均(与表格窗口行同口径)。无
// 数据窗口 active_days=0 照实输出,avg_per_active_day 为 0(不做除法)。
// 定义顺序即 JSON 键顺序。
type forecastJSONWindow struct {
	Window          string `json:"window"`
	Days            int    `json:"days"`
	ActiveDays      int64  `json:"active_days"`
	Requests        int64  `json:"requests"`
	TotalTokens     int64  `json:"total_tokens"`
	AvgPerActiveDay int64  `json:"avg_per_active_day"`
}

// forecastJSONProjection 是 JSON 中一条未来预测:based_on 指明依据的历史窗口,
// estimate_tokens 为该窗口日均乘以未来自然天数(与表格预测行同口径)。无数据
// 窗口不产生对应条目(镜像表格省略预测行,不编造数字)。
type forecastJSONProjection struct {
	Window          string `json:"window"`
	Days            int    `json:"days"`
	BasedOn         string `json:"based_on"`
	AvgPerActiveDay int64  `json:"avg_per_active_day"`
	EstimateTokens  int64  `json:"estimate_tokens"`
}

// forecastJSONPayload 是 forecast 的 JSON 契约:字段顺序即 struct 声明顺序。
type forecastJSONPayload struct {
	TodaySoFar  forecastJSONToday        `json:"today_so_far"`
	Windows     []forecastJSONWindow     `json:"windows"`
	Projections []forecastJSONProjection `json:"projections"`
}

// renderForecastJSON 输出用量预测的机器可读 JSON:窗口语义与表格逐点一致
// (last_7_days=[今天-7,今天-1]、last_30_days=[今天-30,今天-1],不含今天;
// 日均为总量除以活跃天的整数除法;估算为日均乘以未来自然天数)。windows 恒
// 2 条,无数据窗口 active_days=0 照实输出;projections 省略无数据窗口对应
// 条目,两窗口均无数据时为空数组(输出 [] 而非 null)。两空格缩进与尾随换行
// 复用 marshalExportJSON,stdout 纯数据、无标题行。
func renderForecastJSON(w io.Writer, s forecastWindowStats) error {
	payload := forecastJSONPayload{
		TodaySoFar: forecastJSONToday{
			Requests:    s.today.Total.Requests,
			TotalTokens: s.today.Total.TotalTokens,
		},
		Windows:     make([]forecastJSONWindow, 0, 2),
		Projections: make([]forecastJSONProjection, 0, 2),
	}
	appendWindow := func(name string, days int, stats querier.RangeStats) {
		var avg int64
		if stats.ActiveDays > 0 {
			avg = stats.Total.TotalTokens / stats.ActiveDays
		}
		payload.Windows = append(payload.Windows, forecastJSONWindow{
			Window:          name,
			Days:            days,
			ActiveDays:      stats.ActiveDays,
			Requests:        stats.Total.Requests,
			TotalTokens:     stats.Total.TotalTokens,
			AvgPerActiveDay: avg,
		})
	}
	appendProjection := func(name string, days int, basedOn string, stats querier.RangeStats) {
		if stats.ActiveDays == 0 {
			return
		}
		avg := stats.Total.TotalTokens / stats.ActiveDays
		payload.Projections = append(payload.Projections, forecastJSONProjection{
			Window:          name,
			Days:            days,
			BasedOn:         basedOn,
			AvgPerActiveDay: avg,
			EstimateTokens:  avg * int64(days),
		})
	}
	appendWindow("last_7_days", 7, s.last7)
	appendWindow("last_30_days", 30, s.last30)
	appendProjection("next_7_days", 7, "last_7_days", s.last7)
	appendProjection("next_30_days", 30, "last_30_days", s.last30)

	encoded, err := marshalExportJSON(payload)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to encode forecast as JSON", "用量预测 JSON 编码失败"), err)
	}
	_, err = io.WriteString(w, encoded)
	return err
}

// forecastFormatError 非法 --format 取值:在加载配置与开库之前拒绝(句式与
// compare 的同名错误一致)。
func forecastFormatError(value string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --format %q (allowed: table, json)", value),
		fmt.Sprintf("无效的 --format %q（允许：table、json）", value),
	))
}
