package cli

import (
	"context"
	"fmt"
	"io"
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
			"Compare token usage between two periods. RANGE accepts a day (YYYYMMDD), a month (YYYYMM), a year (YYYY; single arg only), or a day/month range like 20260701-20260710 whose endpoints may mix days and months; dashed ISO forms like 2026-08-01 are rejected. Without --base the baseline window is derived from RANGE's granularity: a day compares with the previous day, a month with the previous calendar month, a year with the previous calendar year, and a range with an equal-length window ending the day before it starts, e.g. token-usage compare 20260701-20260710 compares 2026-06-21..2026-06-30. Pass --base with the same forms to pick the baseline explicitly (it may overlap the current window and is parsed independently of RANGE's granularity), e.g. token-usage compare 202609 --base 202608. Pass --by with a non-temporal dimension (client/model/provider/project) to compare per member of that dimension across the two windows instead of whole-period totals.",
			"对比两个时间段的 token 用量。RANGE 接受日（YYYYMMDD）、月（YYYYMM）、年（YYYY，仅单独使用）或日/月区间（如 20260701-20260710，端点可日/月混用）；拒绝 2026-08-01 这类 ISO 破折号形态。缺省 --base 时按 RANGE 粒度自动推导基线窗口：单日对比前一天，单月对比上一个日历月，单年对比上一个日历年，区间对比结束于开始日前一天的等长窗口，如 token-usage compare 20260701-20260710 对比 2026-06-21..2026-06-30。可用 --base 以相同形态显式指定基线（允许与当前窗口重叠，且不与 RANGE 粒度耦合），如 token-usage compare 202609 --base 202608。可用 --by 指定非时间维度（client/model/provider/project），按该维度成员对比两期用量而非两期总量。",
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
				return renderCompareBy(cmd.OutOrStdout(), compareByRenderInput{
					curStart:  curDate,
					curEnd:    curEnd.Format("2006-01-02"),
					baseStart: baseDate,
					baseEnd:   baseEnd.Format("2006-01-02"),
					by:        by,
					cur:       cur,
					base:      base,
					members:   mergeCompareByMembers(curMembers, baseMembers),
				})
			}
			cur, err := q.StatsBetween(ctx, curDate, curEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			base, err := q.StatsBetween(ctx, baseDate, baseEnd.Format("2006-01-02"))
			if err != nil {
				return err
			}
			return renderCompare(cmd.OutOrStdout(), compareRenderInput{
				curStart:  curDate,
				curEnd:    curEnd.Format("2006-01-02"),
				baseStart: baseDate,
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
	cmd.Flags().String("by", "", ui.Bi(
		"Compare per member of a dimension: client/model/provider/project",
		"按维度成员对比：client/model/provider/project",
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

	members := make([]compareByMember, len(in.members))
	copy(members, in.members)
	sort.SliceStable(members, func(i, j int) bool {
		ti := members[i].cur.TotalTokens + members[i].base.TotalTokens
		tj := members[j].cur.TotalTokens + members[j].base.TotalTokens
		if ti != tj {
			return ti > tj
		}
		return members[i].key < members[j].key
	})
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
