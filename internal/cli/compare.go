package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
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
			"Compare token usage between two periods. RANGE accepts a day (YYYYMMDD), a month (YYYYMM), a year (YYYY; single arg only), or a day/month range like 20260701-20260710 whose endpoints may mix days and months; dashed ISO forms like 2026-08-01 are rejected. Without --base the baseline window is derived from RANGE's granularity: a day compares with the previous day, a month with the previous calendar month, a year with the previous calendar year, and a range with an equal-length window ending the day before it starts, e.g. token-usage compare 20260701-20260710 compares 2026-06-21..2026-06-30. Pass --base with the same forms to pick the baseline explicitly (it may overlap the current window and is parsed independently of RANGE's granularity), e.g. token-usage compare 202609 --base 202608. Pass --by with a non-temporal dimension (client/model/provider/project) to compare per member of that dimension across the two windows instead of whole-period totals. --format selects the output format: table (default, the framed comparison table) or json (machine-readable: raw integers, two-space indentation); invalid values are rejected before the database opens.",
			"对比两个时间段的 token 用量。RANGE 接受日（YYYYMMDD）、月（YYYYMM）、年（YYYY，仅单独使用）或日/月区间（如 20260701-20260710，端点可日/月混用）；拒绝 2026-08-01 这类 ISO 破折号形态。缺省 --base 时按 RANGE 粒度自动推导基线窗口：单日对比前一天，单月对比上一个日历月，单年对比上一个日历年，区间对比结束于开始日前一天的等长窗口，如 token-usage compare 20260701-20260710 对比 2026-06-21..2026-06-30。可用 --base 以相同形态显式指定基线（允许与当前窗口重叠，且不与 RANGE 粒度耦合），如 token-usage compare 202609 --base 202608。可用 --by 指定非时间维度（client/model/provider/project），按该维度成员对比两期用量而非两期总量。--format 选择输出格式：table（默认，框线对比表）或 json（机器可读、原始整数、两空格缩进）；非法值在打开数据库之前即被拒绝。",
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
			// 参数解析与 --by 校验先于配置与数据库打开，非法输入即时报错、不占运行时资源。
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs(args[0], cmd.Flag("base").Value.String())
			if err != nil {
				return err
			}
			by, _ := cmd.Flags().GetString("by")
			if err := validateCompareBy(by); err != nil {
				return err
			}
			// --format 白名单同样先于配置加载与数据库打开校验（句式与 export 一致）。
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format != "table" && format != "json" {
				return compareFormatError(format)
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
			curDate, baseDate := curStart.Format("2006-01-02"), baseStart.Format("2006-01-02")
			if by != "" {
				// 分维度模式：两期总量（总计行真相源）与两期成员聚合分别查询。
				cur, err := q.StatsBetween(ctx, curDate, curEnd.Format("2006-01-02"))
				if err != nil {
					return err
				}
				base, err := q.StatsBetween(ctx, baseDate, baseEnd.Format("2006-01-02"))
				if err != nil {
					return err
				}
				aliases := compareDimensionAliases(by, cfg.ProviderAliases)
				curMembers, err := aggregateDimensionChunked(ctx, q, curStart, curEnd, by, aliases)
				if err != nil {
					return err
				}
				baseMembers, err := aggregateDimensionChunked(ctx, q, baseStart, baseEnd, by, aliases)
				if err != nil {
					return err
				}
				in := compareByRenderInput{
					curStart:  curDate,
					curEnd:    curEnd.Format("2006-01-02"),
					baseStart: baseDate,
					baseEnd:   baseEnd.Format("2006-01-02"),
					by:        by,
					cur:       cur,
					base:      base,
					members:   mergeCompareByMembers(curMembers, baseMembers),
				}
				if format == "json" {
					return renderCompareByJSON(cmd.OutOrStdout(), in)
				}
				return renderCompareBy(cmd.OutOrStdout(), in)
			}
			cur, err := q.StatsBetween(ctx, curDate, curEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			base, err := q.StatsBetween(ctx, baseDate, baseEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			in := compareRenderInput{
				curStart:  curDate,
				curEnd:    curEnd.Format("2006-01-02"),
				baseStart: baseDate,
				baseEnd:   baseEnd.Format("2006-01-02"),
				cur:       cur,
				base:      base,
			}
			if format == "json" {
				return renderCompareJSON(cmd.OutOrStdout(), in)
			}
			return renderCompare(cmd.OutOrStdout(), in)
		},
	}
	cmd.Flags().String("base", "", ui.Bi(
		"base period range (same forms as RANGE); overrides the auto-derived previous equal-length window",
		"基线时间段（与 RANGE 同形态）；缺省时自动取前置等长窗口",
	))
	cmd.Flags().String("by", "", ui.Bi(
		"Compare per member of a dimension: client/model/provider/project",
		"按维度成员对比：client/model/provider/project",
	))
	// --format 是本地 flag,缺省 table;取值白名单在配置加载与开库之前校验。
	cmd.Flags().String("format", "table", ui.Bi(
		"output format: table or json",
		"输出格式：table 或 json",
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

// changePercentValue 计算两期变化的百分比数值：四舍五入到 1 位小数。表格
// formatChangePercent 与 JSON 的 change_percent 共用该函数，保证两个表面
// 同一舍入口径。base == 0 时百分比无定义，返回 ok=false（与表格 "--"、
// JSON null 同语义）。
func changePercentValue(cur, base int64) (float64, bool) {
	if base == 0 {
		return 0, false
	}
	pct := float64(cur-base) / float64(base) * 100
	return math.Round(pct*10) / 10, true
}

// formatChangePercent 渲染变化百分比：基线为 0 时百分比无定义，显示 "--"；
// 数值部分经 changePercentValue 与 JSON 同口径舍入。符号按原始差值判断
// （base > 0 时与未舍入百分比同号）：极小正百分比舍入后为 0.0 仍保留
// "+"（+0.0%），负数由 %.1f 自带 "-"，恰好持平为 "0.0%"。
func formatChangePercent(cur, base int64) string {
	pct, ok := changePercentValue(cur, base)
	if !ok {
		return "--"
	}
	switch {
	case cur > base:
		return fmt.Sprintf("+%.1f%%", pct)
	case cur < base:
		return fmt.Sprintf("%.1f%%", pct)
	default:
		return "0.0%"
	}
}

// compareByDimensions 是 compare --by 的允许集：仅非时间维度，与 querier
// 聚合核 dimensionOrder 中的非时间成员一致（顺序即错误文案的允许集展示顺序）。
var compareByDimensions = []string{"client", "model", "provider", "project"}

// validateCompareBy 校验 compare --by 的取值：空串表示总量对比，放行。时间
// 维度（day/month/hour/weekday）与未知值统一拒绝并指路——趋势图用
// token-usage chart --line，两期总量对比用不带 --by 的 compare。
func validateCompareBy(by string) error {
	if by == "" {
		return nil
	}
	for _, d := range compareByDimensions {
		if by == d {
			return nil
		}
	}
	var reasonEn, reasonZh string
	if by == "day" || by == "month" || by == "hour" || by == "weekday" {
		reasonEn = fmt.Sprintf("--by does not accept temporal dimensions, got %q", by)
		reasonZh = fmt.Sprintf("--by 不接受时间维度，当前 %q", by)
	} else {
		reasonEn = fmt.Sprintf("unknown --by dimension %q", by)
		reasonZh = fmt.Sprintf("未知 --by 维度 %q", by)
	}
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("%s (allowed: %s); trends are charted by token-usage chart --line, and whole-period totals are compared by compare without --by",
			reasonEn, strings.Join(compareByDimensions, ", ")),
		fmt.Sprintf("%s（允许：%s）；趋势图请用 token-usage chart --line，两期总量对比用不带 --by 的 compare",
			reasonZh, strings.Join(compareByDimensions, ", ")),
	))
}

// expandRangeDays 把闭区间 [start, end] 逐日展开为 YYYY-MM-DD 字符串切片；
// end 早于 start 时返回空切片。
func expandRangeDays(start, end time.Time) []string {
	days := make([]string, 0, 8)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}

// chunkDays 把闭区间 [start, end] 按 size 天一块切分为连续子区间并逐块展开
// 为 YYYY-MM-DD 切片：块内日期连续、跨块无缝不重；size<=0 视为整段一块。
// compare 总量路径无 366 天上限（StatsBetween 直接 BETWEEN），但分维度路径
// 消费 AggregateDimensionView 的逐日 IN 占位符，超长区间会触碰 SQLite 变量
// 上限，因此按块查询后跨块累加——求和可结合，与整段聚合等价。
func chunkDays(start, end time.Time, size int) [][]string {
	if size <= 0 {
		if days := expandRangeDays(start, end); len(days) > 0 {
			return [][]string{days}
		}
		return nil
	}
	var chunks [][]string
	for cur := start; !cur.After(end); {
		chunkEnd := cur.AddDate(0, 0, size-1)
		if chunkEnd.After(end) {
			chunkEnd = end
		}
		chunks = append(chunks, expandRangeDays(cur, chunkEnd))
		cur = chunkEnd.AddDate(0, 0, 1)
	}
	return chunks
}

// compareChunkSize 是 compare --by 分块查询的块大小（天）：与 query/collect
// 的 366 天上限一致，单块占位符数恒在 SQLite 默认变量上限（999）之内。
const compareChunkSize = 366

// compareDimensionAliases 返回分维度聚合应应用的 provider 显示别名：仅
// provider 维度消费别名（聚合核 displayKey 的语义），与 query/export 同源取
// cfg.ProviderAliases；其余维度传 nil，保持各自维度语义不变。
func compareDimensionAliases(by string, aliases map[string]string) map[string]string {
	if by == "provider" {
		return aliases
	}
	return nil
}

// aggregateDimensionChunked 聚合一个窗口内按维度成员的用量：区间经 chunkDays
// 分块，每块调用一次 AggregateDimensionView（内部完成 alias 合并与显示键
// 映射），跨块按显示键 Keys[0] 累加 GroupAggregate——求和可结合，与整段
// 聚合等价；非时间维度无缺口填充，块切分不会引入伪成员。
func aggregateDimensionChunked(ctx context.Context, q *querier.Querier, start, end time.Time, by string, aliases map[string]string) (map[string]querier.GroupAggregate, error) {
	merged := make(map[string]querier.GroupAggregate)
	for _, days := range chunkDays(start, end, compareChunkSize) {
		rows, _, err := q.AggregateDimensionView(ctx, days, querier.DimensionView{
			Dimensions: []string{by},
			Aliases:    aliases,
			TitleEn:    "compare", TitleZh: "compare",
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if len(row.Keys) == 0 {
				continue
			}
			key := row.Keys[0]
			agg := merged[key]
			addGroupAggregate(&agg, row.Agg)
			merged[key] = agg
		}
	}
	return merged, nil
}

// addGroupAggregate 逐字段累加两个聚合。querier.GroupAggregate 的 add 为包内
// 私有，compare 的跨块合并在消费侧等价实现。
func addGroupAggregate(a *querier.GroupAggregate, o querier.GroupAggregate) {
	a.Requests += o.Requests
	a.FreshInput += o.FreshInput
	a.OutputTokens += o.OutputTokens
	a.CacheRead += o.CacheRead
	a.CacheCreate += o.CacheCreate
	a.Reasoning += o.Reasoning
	a.TotalTokens += o.TotalTokens
}

// compareByMember 是分维度对比中的一个成员行：显示键与两期聚合（只在一期
// 出现的成员缺失侧为零值）。
type compareByMember struct {
	key  string
	cur  querier.GroupAggregate
	base querier.GroupAggregate
}

// mergeCompareByMembers 合并两期成员集：任一期出现的成员都保留，同一显示键
// 归并为一行；返回顺序不定，排序由渲染侧统一决定。
func mergeCompareByMembers(cur, base map[string]querier.GroupAggregate) []compareByMember {
	members := make([]compareByMember, 0, len(cur)+len(base))
	index := make(map[string]int, len(cur)+len(base))
	for k, agg := range cur {
		index[k] = len(members)
		members = append(members, compareByMember{key: k, cur: agg})
	}
	for k, agg := range base {
		if i, ok := index[k]; ok {
			members[i].base = agg
			continue
		}
		members = append(members, compareByMember{key: k, base: agg})
	}
	return members
}

// compareByRenderInput 是 renderCompareBy 的渲染输入：两个窗口起止日期
// （YYYY-MM-DD 字符串）、维度名、两期 StatsBetween 总量（总计行真相源，不
// 从成员行累加）与合并后的成员行。
type compareByRenderInput struct {
	curStart, curEnd   string
	baseStart, baseEnd string
	by                 string
	cur, base          querier.RangeStats
	members            []compareByMember
}

// compareByHeader 返回维度名对应的表格首列两行表头；调用前 by 已通过
// validateCompareBy 校验，default 分支仅为未知值兜底。
func compareByHeader(by string) string {
	switch by {
	case "client":
		return ui.HClient
	case "model":
		return ui.HModel
	case "provider":
		return ui.HProvider
	case "project":
		return ui.HProject
	default:
		return ui.HeaderLines("Member", "成员")
	}
}

// renderCompareBy 输出两期窗口内按维度成员的用量对比：窗口头两行与总量
// 对比一致（Current/Base 各一行）；双窗口成员集为空且两期总计均为 0 时只
// 输出无数据行，否则渲染 5 列框线表（成员/当前/基线/变化/变化%）。成员按
// 两期 TotalTokens 之和降序、同值按显示键升序；缺失侧按 0 参与对比，基线
// 为 0 时变化% 显示 "--"。末行总计取两期 StatsBetween 总量（成员行经 alias
// 合并应与总计一致，但以 StatsBetween 为真相源），列头与对齐口径同总量表。
func renderCompareBy(w io.Writer, in compareByRenderInput) error {
	fmt.Fprintln(w, ui.Bi("Compare", "用量对比"))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s: %s .. %s\n", ui.Bi("Current", "当前"), in.curStart, in.curEnd)
	fmt.Fprintf(w, "%s: %s .. %s\n", ui.Bi("Base", "基线"), in.baseStart, in.baseEnd)
	fmt.Fprintln(w)

	if len(in.members) == 0 && in.cur.Total.TotalTokens == 0 && in.base.Total.TotalTokens == 0 {
		fmt.Fprintln(w, ui.Bi("no data", "无数据"))
		return nil
	}

	t := ui.NewTable([]string{
		compareByHeader(in.by),
		ui.HeaderLines("Current", "当前"),
		ui.HeaderLines("Base", "基线"),
		ui.HeaderLines("Change", "变化"),
		ui.HeaderLines("Change %", "变化%"),
	}, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight)

	members := sortCompareByMembers(in.members)
	for _, m := range members {
		t.Row(m.key,
			querier.FormatTokens(m.cur.TotalTokens), querier.FormatTokens(m.base.TotalTokens),
			formatSignedTokens(m.cur.TotalTokens-m.base.TotalTokens),
			formatChangePercent(m.cur.TotalTokens, m.base.TotalTokens))
	}
	t.Row(ui.ColTotal,
		querier.FormatTokens(in.cur.Total.TotalTokens), querier.FormatTokens(in.base.Total.TotalTokens),
		formatSignedTokens(in.cur.Total.TotalTokens-in.base.Total.TotalTokens),
		formatChangePercent(in.cur.Total.TotalTokens, in.base.Total.TotalTokens))

	fmt.Fprintln(w, t.String())
	return nil
}

// sortCompareByMembers 返回按表格口径排序的成员行独立副本：两期 TotalTokens
// 之和降序、同值按显示键升序。表格与 JSON 输出共用，保证两表面行序一致。
func sortCompareByMembers(members []compareByMember) []compareByMember {
	sorted := make([]compareByMember, len(members))
	copy(sorted, members)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti := sorted[i].cur.TotalTokens + sorted[i].base.TotalTokens
		tj := sorted[j].cur.TotalTokens + sorted[j].base.TotalTokens
		if ti != tj {
			return ti > tj
		}
		return sorted[i].key < sorted[j].key
	})
	return sorted
}

// compareFormatError 非法 --format 取值:在加载配置与开库之前拒绝(句式与
// export 的同名错误一致)。
func compareFormatError(value string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --format %q (allowed: table, json)", value),
		fmt.Sprintf("无效的 --format %q（允许：table、json）", value),
	))
}

// compareJSONWindow 是 JSON 输出中的一个窗口起止（YYYY-MM-DD，与表格
// Current/Base 行同源）。
type compareJSONWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// compareJSONWindows 是对比的两个窗口。
type compareJSONWindows struct {
	Current compareJSONWindow `json:"current"`
	Base    compareJSONWindow `json:"base"`
}

// compareJSONMetric 是总量对比 JSON 中的一行指标：metric 为 ui 输出指标
// 稳定 ID（active_days 为 compare 特有）；值为原始整数（不做 K/M 缩写）；
// change_percent 经 changePercentValue 与表格同口径（四舍五入到 1 位小数，
// base==0 时为 null）。struct 序列化保证字段顺序稳定。
type compareJSONMetric struct {
	Metric        string   `json:"metric"`
	Current       int64    `json:"current"`
	Base          int64    `json:"base"`
	Change        int64    `json:"change"`
	ChangePercent *float64 `json:"change_percent"`
}

// compareJSONPayload 是 compare 总量模式的 JSON 契约：字段顺序即 struct
// 声明顺序。
type compareJSONPayload struct {
	Windows compareJSONWindows  `json:"windows"`
	Metrics []compareJSONMetric `json:"metrics"`
}

// compareJSONMember 是分维度对比 JSON 中的一个成员行：值为两期 TotalTokens
// 的原始整数，change_percent 语义与总量模式一致。
type compareJSONMember struct {
	Key           string   `json:"key"`
	Current       int64    `json:"current"`
	Base          int64    `json:"base"`
	Change        int64    `json:"change"`
	ChangePercent *float64 `json:"change_percent"`
}

// compareJSONTotalsSide 是分维度对比 JSON 中一个窗口的总量：字段名与 ui
// 输出指标稳定 ID 一致，取该窗口 StatsBetween 全量聚合（真相源，不由成员
// 行累加）。
type compareJSONTotalsSide struct {
	Requests    int64 `json:"requests"`
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Reasoning   int64 `json:"reasoning"`
	Total       int64 `json:"total"`
}

// compareJSONTotals 是分维度对比 JSON 的两期总量。
type compareJSONTotals struct {
	Current compareJSONTotalsSide `json:"current"`
	Base    compareJSONTotalsSide `json:"base"`
}

// compareJSONByPayload 是 compare --by 分维度模式的 JSON 契约。
type compareJSONByPayload struct {
	Windows   compareJSONWindows  `json:"windows"`
	Dimension string              `json:"dimension"`
	Members   []compareJSONMember `json:"members"`
	Totals    compareJSONTotals   `json:"totals"`
}

// compareChangePercentPtr 把 changePercentValue 的可计算性映射为 JSON 的
// 可空 change_percent：base == 0 时返回 nil（编码为 null）。
func compareChangePercentPtr(cur, base int64) *float64 {
	v, ok := changePercentValue(cur, base)
	if !ok {
		return nil
	}
	return &v
}

// compareJSONTotalsSideOf 把一个窗口的全量聚合映射为 JSON 总量（字段名与
// ui 输出指标稳定 ID 对应，FreshInput 对应 input）。
func compareJSONTotalsSideOf(a querier.GroupAggregate) compareJSONTotalsSide {
	return compareJSONTotalsSide{
		Requests:    a.Requests,
		Input:       a.FreshInput,
		Output:      a.OutputTokens,
		CacheRead:   a.CacheRead,
		CacheCreate: a.CacheCreate,
		Reasoning:   a.Reasoning,
		Total:       a.TotalTokens,
	}
}

// writeCompareJSON 序列化 compare 的 JSON 载荷并写入 w：复用 export 的
// marshalExportJSON（两空格缩进、尾随换行），序列化与写出错误以统一双语
// 前缀包装。
func writeCompareJSON(w io.Writer, payload any) error {
	s, err := marshalExportJSON(payload)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("compare failed", "对比失败"), err)
	}
	if _, err := fmt.Fprint(w, s); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("compare failed", "对比失败"), err)
	}
	return nil
}

// compareJSONMetricRow 构造一行指标：change 为原始差值，change_percent 走
// 共用的可空百分比口径。
func compareJSONMetricRow(id string, cur, base int64) compareJSONMetric {
	return compareJSONMetric{
		Metric:        id,
		Current:       cur,
		Base:          base,
		Change:        cur - base,
		ChangePercent: compareChangePercentPtr(cur, base),
	}
}

// renderCompareJSON 输出总量对比的机器可读 JSON：windows 与表格 Current/Base
// 行同源；metrics 顺序与表格行一致，metric 标识复用 ui 输出指标稳定 ID
// （active_days 为 compare 特有、无 ui ID）；整数保持原始值（不做 K/M 缩写），
// change_percent 与表格同口径（1 位小数舍入、base==0 为 null）。序列化经
// marshalExportJSON：两空格缩进、尾随换行。
func renderCompareJSON(w io.Writer, in compareRenderInput) error {
	payload := compareJSONPayload{
		Windows: compareJSONWindows{
			Current: compareJSONWindow{From: in.curStart, To: in.curEnd},
			Base:    compareJSONWindow{From: in.baseStart, To: in.baseEnd},
		},
		Metrics: []compareJSONMetric{
			// active_days 是 compare 特有指标，表格首行对应。
			compareJSONMetricRow("active_days", in.cur.ActiveDays, in.base.ActiveDays),
			compareJSONMetricRow(ui.MetricRequests, in.cur.Total.Requests, in.base.Total.Requests),
			compareJSONMetricRow(ui.MetricInput, in.cur.Total.FreshInput, in.base.Total.FreshInput),
			compareJSONMetricRow(ui.MetricOutput, in.cur.Total.OutputTokens, in.base.Total.OutputTokens),
			compareJSONMetricRow(ui.MetricCacheRead, in.cur.Total.CacheRead, in.base.Total.CacheRead),
			compareJSONMetricRow(ui.MetricCacheCreate, in.cur.Total.CacheCreate, in.base.Total.CacheCreate),
			compareJSONMetricRow(ui.MetricReasoning, in.cur.Total.Reasoning, in.base.Total.Reasoning),
			compareJSONMetricRow(ui.MetricTotal, in.cur.Total.TotalTokens, in.base.Total.TotalTokens),
		},
	}
	return writeCompareJSON(w, payload)
}

// renderCompareByJSON 输出 --by 分维度对比的机器可读 JSON：members 排序经
// sortCompareByMembers 与表格一致；totals 取两期 StatsBetween 总量（真相源）。
// 双窗口均无数据时 members 为空数组照常输出——JSON 不做表格的 no data 早退。
func renderCompareByJSON(w io.Writer, in compareByRenderInput) error {
	payload := compareJSONByPayload{
		Windows: compareJSONWindows{
			Current: compareJSONWindow{From: in.curStart, To: in.curEnd},
			Base:    compareJSONWindow{From: in.baseStart, To: in.baseEnd},
		},
		Dimension: in.by,
		Members:   make([]compareJSONMember, 0, len(in.members)),
		Totals: compareJSONTotals{
			Current: compareJSONTotalsSideOf(in.cur.Total),
			Base:    compareJSONTotalsSideOf(in.base.Total),
		},
	}
	for _, m := range sortCompareByMembers(in.members) {
		payload.Members = append(payload.Members, compareJSONMember{
			Key:           m.key,
			Current:       m.cur.TotalTokens,
			Base:          m.base.TotalTokens,
			Change:        m.cur.TotalTokens - m.base.TotalTokens,
			ChangePercent: compareChangePercentPtr(m.cur.TotalTokens, m.base.TotalTokens),
		})
	}
	return writeCompareJSON(w, payload)
}
