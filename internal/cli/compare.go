package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newCompareCmd() *cobra.Command {
	return newCompareCmdWithDeps(loadConfig, db.Open)
}

// newCompareCmdWithDeps 构造 compare 命令;load/open 可注入供包内测试走真实
// 调用链(生产路径传入 loadConfig 与 db.Open)。只读命令,不触碰 daemon。
func newCompareCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare <range>",
		Short: "Compare usage between two periods / 对比两个时间段的用量",
		Long: ui.Bi(
			"Compare token usage between two periods. RANGE accepts a day (YYYYMMDD), a month (YYYYMM), a year (YYYY; single arg only), or a day/month range like 20260701-20260710 whose endpoints may mix days and months; dashed ISO forms like 2026-08-01 are rejected. Without --base the baseline window is derived from RANGE's granularity: a day compares with the previous day, a month with the previous calendar month, a year with the previous calendar year, and a range with an equal-length window ending the day before it starts, e.g. token-usage compare 20260701-20260710 compares 2026-06-21..2026-06-30. Pass --base with the same forms to pick the baseline explicitly (it may overlap the current window and is parsed independently of RANGE's granularity), e.g. token-usage compare 202609 --base 202608.",
			"对比两个时间段的 token 用量。RANGE 接受日（YYYYMMDD）、月（YYYYMM）、年（YYYY，仅单独使用）或日/月区间（如 20260701-20260710，端点可日/月混用）；拒绝 2026-08-01 这类 ISO 破折号形态。缺省 --base 时按 RANGE 粒度自动推导基线窗口：单日对比前一天，单月对比上一个日历月，单年对比上一个日历年，区间对比结束于开始日前一天的等长窗口，如 token-usage compare 20260701-20260710 对比 2026-06-21..2026-06-30。可用 --base 以相同形态显式指定基线（允许与当前窗口重叠，且不与 RANGE 粒度耦合），如 token-usage compare 202609 --base 202608。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("compare requires exactly 1 positional arg (a date or date range), got %d. Accepts %s, e.g. token-usage compare 20260701", len(args), dateFormatsHintEN),
					fmt.Sprintf("compare 需要恰好 1 个位置参数（日期或日期区间），当前 %d 个。%s，例如 token-usage compare 20260701", len(args), dateFormatsHintZH),
				))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于配置与数据库打开，非法区间即时报错、不占运行时资源。
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs(args[0], cmd.Flag("base").Value.String())
			if err != nil {
				return err
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

			q := querier.New(usageDB)
			ctx := cmdContext(cmd)
			cur, err := q.StatsBetween(ctx, curStart.Format("2006-01-02"), curEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			base, err := q.StatsBetween(ctx, baseStart.Format("2006-01-02"), baseEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			return renderCompare(cmd.OutOrStdout(), compareRenderInput{
				curStart:  curStart.Format("2006-01-02"),
				curEnd:    curEnd.Format("2006-01-02"),
				baseStart: baseStart.Format("2006-01-02"),
				baseEnd:   baseEnd.Format("2006-01-02"),
				cur:       cur,
				base:      base,
			})
		},
	}
	cmd.Flags().String("base", "", ui.Bi(
		"base period range (same forms as RANGE); overrides the auto-derived previous equal-length window",
		"基线时间段（与 RANGE 同形态）；缺省时自动取前置等长窗口",
	))
	return cmd
}

// parseCompareArgs 解析 compare 的当前窗口与基线窗口：base 为空时按当前
// 窗口粒度推导缺省基线（defaultCompareBase），非空时用同一解析器独立解析。
// 返回两个窗口的归一化起止时间。
func parseCompareArgs(raw, base string) (curStart, curEnd, baseStart, baseEnd time.Time, err error) {
	curStart, curEnd, singleLen, err := parseCompareRangeArg(raw)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, time.Time{}, err
	}
	if base == "" {
		baseStart, baseEnd = defaultCompareBase(curStart, curEnd, singleLen)
		return curStart, curEnd, baseStart, baseEnd, nil
	}
	baseStart, baseEnd, _, err = parseCompareRangeArg(base)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, time.Time{}, err
	}
	return curStart, curEnd, baseStart, baseEnd, nil
}

// parseCompareRangeArg 解析 compare 的一个时间段参数（当前窗口或 --base），
// 与 query/collect 的日期口径一致：8 位日、6 位月、4 位年（年仅单独使用）
// 或日/月端点的闭区间（可混用）；明确拒绝 ISO 破折号形态。返回归一化
// [first, last] 与单参数长度（4/6/8；A-B 区间返回 0，供缺省基线按粒度推导）。
// 错误复用 date.go 的既有辅助函数，cmdName 固定 "compare"；区间不设 366 天
// 上限（StatsBetween 直接 BETWEEN，不展开逐日）。
func parseCompareRangeArg(raw string) (first, last time.Time, singleLen int, err error) {
	if strings.Contains(raw, "-") {
		parts := strings.SplitN(raw, "-", 2)
		startStr, endStr := parts[0], parts[1]
		// 形态校验（长度）按 start→end 顺序，首个不合法端点即报错；4 位年在
		// 此报「年只接受单参数」（含 ISO 形态拆分后 start 为 4 位的场景）。
		for _, ep := range []string{startStr, endStr} {
			if len(ep) == 4 {
				return time.Time{}, time.Time{}, 0, yearEndpointError(raw, "compare")
			}
			if len(ep) != 6 && len(ep) != 8 {
				return time.Time{}, time.Time{}, 0, rangeEndpointLengthError(raw, "compare")
			}
		}
		startFirst, _, err := parseDateEndpoint(startStr)
		if err != nil {
			return time.Time{}, time.Time{}, 0, rangeEndpointCalendarError("start", raw, "compare")
		}
		_, endLast, err := parseDateEndpoint(endStr)
		if err != nil {
			return time.Time{}, time.Time{}, 0, rangeEndpointCalendarError("end", raw, "compare")
		}
		if endLast.Before(startFirst) {
			return time.Time{}, time.Time{}, 0, rangeEndBeforeStartError(raw, "compare")
		}
		return startFirst, endLast, 0, nil
	}
	if l := len(raw); l != 4 && l != 6 && l != 8 {
		return time.Time{}, time.Time{}, 0, singleArgLengthError(raw, "compare")
	}
	first, last, err = parseDateEndpoint(raw)
	if err != nil {
		return time.Time{}, time.Time{}, 0, singleArgCalendarError(raw, "compare")
	}
	return first, last, len(raw), nil
}

// defaultCompareBase 按 <range> 粒度推导缺省基线窗口：单日取前一天，单月取
// 上一个日历月（AddDate 归一化自动处理闰月），单年取上一个日历年；区间取
// 结束于开始日前一天的等长窗口。天数用纯 AddDate 循环计数，不用
// end.Sub(start) 换算（time.Duration 约容 292 年，超长区间会饱和折损天数）。
func defaultCompareBase(start, end time.Time, singleLen int) (time.Time, time.Time) {
	switch singleLen {
	case 8: // 单日：前一天
		prev := start.AddDate(0, 0, -1)
		return prev, prev
	case 6: // 单月：上一个日历月，月末由 AddDate 归一化推导
		first := start.AddDate(0, -1, 0)
		return first, first.AddDate(0, 1, -1)
	case 4: // 单年：上一个日历年
		return start.AddDate(-1, 0, 0), end.AddDate(-1, 0, 0)
	default: // 区间：结束于开始日前一天的等长窗口
		days := 0
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			days++
		}
		baseEnd := start.AddDate(0, 0, -1)
		return baseEnd.AddDate(0, 0, -(days - 1)), baseEnd
	}
}

// compareRenderInput 是 renderCompare 的渲染输入：两个窗口的起止日期
// （YYYY-MM-DD 字符串）与各自的全量统计。
type compareRenderInput struct {
	curStart, curEnd   string
	baseStart, baseEnd string
	cur                querier.RangeStats
	base               querier.RangeStats
}

// renderCompare 输出两个时间段的用量对比：标题、两个窗口起止行与一张
// 5 列框线表（指标/当前/基线/变化/变化%）。token 行沿用 K/M/B 缩写口径，
// 变化列带 +/- 符号；计数行用 %+d；基线为 0 时变化% 显示 "--"（百分比无
// 定义）。表头为双语两行（上行英文、下行中文），不含 Δ 等 ambiguous-width
// 字符，保证 CJK 终端下的框线对齐。
func renderCompare(w io.Writer, in compareRenderInput) error {
	fmt.Fprintln(w, ui.Bi("Compare", "用量对比"))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s: %s .. %s\n", ui.Bi("Current", "当前"), in.curStart, in.curEnd)
	fmt.Fprintf(w, "%s: %s .. %s\n", ui.Bi("Base", "基线"), in.baseStart, in.baseEnd)
	fmt.Fprintln(w)

	t := ui.NewTable([]string{
		ui.HeaderLines("Metric", "指标"),
		ui.HeaderLines("Current", "当前"),
		ui.HeaderLines("Base", "基线"),
		ui.HeaderLines("Change", "变化"),
		ui.HeaderLines("Change %", "变化%"),
	}, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight)

	countRow := func(label string, curV, baseV int64) {
		t.Row(label, fmt.Sprintf("%d", curV), fmt.Sprintf("%d", baseV),
			formatCountChange(curV-baseV), formatChangePercent(curV, baseV))
	}
	tokenRow := func(label string, curV, baseV int64) {
		t.Row(label, querier.FormatTokens(curV), querier.FormatTokens(baseV),
			formatSignedTokens(curV-baseV), formatChangePercent(curV, baseV))
	}

	countRow(ui.Bi("Active days", "活跃天"), in.cur.ActiveDays, in.base.ActiveDays)
	countRow(ui.ColRequests, in.cur.Total.Requests, in.base.Total.Requests)
	tokenRow(ui.ColInput, in.cur.Total.FreshInput, in.base.Total.FreshInput)
	tokenRow(ui.ColOutput, in.cur.Total.OutputTokens, in.base.Total.OutputTokens)
	tokenRow(ui.ColCacheRead, in.cur.Total.CacheRead, in.base.Total.CacheRead)
	tokenRow(ui.ColCacheCreate, in.cur.Total.CacheCreate, in.base.Total.CacheCreate)
	tokenRow(ui.ColReasoning, in.cur.Total.Reasoning, in.base.Total.Reasoning)
	tokenRow(ui.ColTotal, in.cur.Total.TotalTokens, in.base.Total.TotalTokens)

	fmt.Fprintln(w, t.String())
	return nil
}

// formatSignedTokens 渲染带符号的 token 差值：正数前缀 "+"、负数前缀 "-"、
// 零显示 "0"；幅值经 uint64 取绝对值（避免 MinInt64 取负溢出）后复用
// querier.FormatTokens，缩写阈值与 query 表格完全一致。
func formatSignedTokens(diff int64) string {
	if diff == 0 {
		return "0"
	}
	mag := uint64(diff)
	if diff < 0 {
		mag = uint64(-(diff + 1)) + 1
	}
	magnitude := querier.FormatTokens(int64(mag))
	if diff > 0 {
		return "+" + magnitude
	}
	return "-" + magnitude
}

// formatCountChange 渲染计数差值：0 显示 "0"，非 0 用 %+d 自带符号。
func formatCountChange(diff int64) string {
	if diff == 0 {
		return "0"
	}
	return fmt.Sprintf("%+d", diff)
}

// formatChangePercent 渲染变化百分比：基线为 0 时百分比无定义，显示 "--"；
// 正数带 "+" 前缀，负数由 %.1f 自带 "-"，恰好持平为 "0.0%"。
func formatChangePercent(cur, base int64) string {
	if base == 0 {
		return "--"
	}
	pct := float64(cur-base) / float64(base) * 100
	switch {
	case pct > 0:
		return fmt.Sprintf("+%.1f%%", pct)
	case pct < 0:
		return fmt.Sprintf("%.1f%%", pct)
	default:
		return "0.0%"
	}
}
