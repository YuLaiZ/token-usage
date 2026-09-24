package web

// dashboard_shape.go 承载仪表板载荷的响应前整理:维度/自定义视图的前 N
// 截取与尾行逐指标汇总、趋势桶(小时/日/周)与热力展示桶(逐小时/4 小时
// 时段/逐日日历)。全部为纯函数——输入是聚合核的结构化输出,输出是 JSON
// 行;截断与合并在此完成后,前端只消费最终行,不再持有完整明细。

import (
	"sort"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/querier"
)

// metricSum 把 src 的七项整数累加到 dst 上(命中率为派生值不参与合并)。
func metricSum(dst *dimensionRowJSON, src dimensionRowJSON) {
	dst.Requests += src.Requests
	dst.FreshInput += src.FreshInput
	dst.Output += src.Output
	dst.CacheRead += src.CacheRead
	dst.CacheCreate += src.CacheCreate
	dst.Reasoning += src.Reasoning
	dst.Total += src.Total
}

// sortDimensionRows 按「整数 total 降序、同值名称字节序升序」稳定排序——
// 与聚合核的排序合同一致,这里复述以保证截断与排序在同一层自洽。
func sortDimensionRows(rows []dimensionRowJSON) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].Key < rows[j].Key
	})
}

// topDimensionRows 截取前 topN 个独立项;余项逐指标合为至多一行"其他"
// (IsOther=true、OtherCount=余项数、Key 为空)并固定最后。余项为零时不
// 生成尾行;尾行的 total 允许大于最后一个独立项,不参与独立项排序。
// 独立项+尾行的七项之和与全量行逐项相等(七项守恒),缓存命中率由前端
// 按合并后的整数重算,不对百分比取平均。
func topDimensionRows(rows []dimensionRowJSON, topN int) []dimensionRowJSON {
	ordered := make([]dimensionRowJSON, len(rows))
	copy(ordered, rows)
	sortDimensionRows(ordered)
	if len(ordered) <= topN {
		return ordered
	}
	other := dimensionRowJSON{IsOther: true, OtherCount: len(ordered) - topN}
	for _, row := range ordered[topN:] {
		metricSum(&other, row)
	}
	return append(ordered[:topN:topN], other)
}

// sortCustomViewRows 按「整数 total 降序、同值组合键逐元字节序升序」稳定
// 排序——组合键的确定性排序轴与聚合核一致。
func sortCustomViewRows(rows []customViewRowJSON) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return strings.Join(rows[i].Keys, "\x00") < strings.Join(rows[j].Keys, "\x00")
	})
}

// topCustomViewRows 与 topDimensionRows 同合同:前 topN 个独立组合,余项
// 合为至多一行"其他组合"固定最后。
func topCustomViewRows(rows []customViewRowJSON, topN int) []customViewRowJSON {
	ordered := make([]customViewRowJSON, len(rows))
	copy(ordered, rows)
	sortCustomViewRows(ordered)
	if len(ordered) <= topN {
		return ordered
	}
	other := customViewRowJSON{
		Keys:       make([]string, len(ordered[0].Keys)),
		IsOther:    true,
		OtherCount: len(ordered) - topN,
	}
	for _, row := range ordered[topN:] {
		other.Requests += row.Requests
		other.FreshInput += row.FreshInput
		other.Output += row.Output
		other.CacheRead += row.CacheRead
		other.CacheCreate += row.CacheCreate
		other.Reasoning += row.Reasoning
		other.Total += row.Total
	}
	return append(ordered[:topN:topN], other)
}

// buildTrend 由日期×小时矩阵整理趋势序列:
//   - 单日:24 个小时桶(键 "00".."23"),granularity=hour;
//   - 2~62 天:逐日桶(键为日期),granularity=day;
//   - 63 天及以上:自区间起点每 7 天一个周桶,键取该桶最后一天的日期,
//     granularity=week(末桶允许不满一周)。
//
// PeakIndex 取首个最大值(平手不换行);Empty=true 表示无桶或全零,此时
// PeakIndex=-1,前端显示空状态而非"峰值 0"。
func buildTrend(hm *querier.DayHourMatrix) trendJSON {
	t := trendJSON{Granularity: "day", Buckets: []trendBucketJSON{}, PeakIndex: -1}
	if hm == nil || len(hm.Days) == 0 {
		t.Empty = true
		return t
	}
	if len(hm.Days) == 1 {
		t.Granularity = "hour"
		for h, v := range hm.Values[0] {
			t.Buckets = append(t.Buckets, trendBucketJSON{Key: hourKey(h), Total: v})
		}
	} else if len(hm.Days) <= trendDailyMaxDays {
		for i, date := range hm.Days {
			t.Buckets = append(t.Buckets, trendBucketJSON{Key: date, Total: hm.Totals[i]})
		}
	} else {
		t.Granularity = "week"
		for start := 0; start < len(hm.Days); start += 7 {
			end := start + 7
			if end > len(hm.Days) {
				end = len(hm.Days)
			}
			var sum int64
			for i := start; i < end; i++ {
				sum += hm.Totals[i]
			}
			t.Buckets = append(t.Buckets, trendBucketJSON{Key: hm.Days[end-1], Total: sum})
		}
	}
	for i, b := range t.Buckets {
		if b.Total > t.PeakTotal {
			t.PeakTotal = b.Total
			t.PeakIndex = i
		}
	}
	t.Empty = t.PeakIndex < 0
	return t
}

// hourKey 把小时下标格式化为两位键("00".."23"),与热力小时桶键同形态。
func hourKey(h int) string {
	if h < 0 {
		h = 0
	}
	if h > 23 {
		h = 23
	}
	return string(rune('0'+h/10)) + string(rune('0'+h%10))
}

// buildHeatmapDays 按范围形态产出热力展示桶与模式:
//   - 1~7 天(day_hour):逐日 24 个小时格;
//   - 8~31 天(day_block):逐日 6 个 4 小时时段格(时段由小时格求和,
//     边界 00/04/08/12/16/20 与前端图例一致);
//   - 32 天以上(calendar):仅逐日合计,小时/时段数组整体省略。
//
// 每日附带 Future 标记(晚于 now 的本机今天):true 表示尚未发生的未来日,
// 前端渲染空白;false 表示已发生,零值是真实零格。
func buildHeatmapDays(hm *querier.DayHourMatrix, now time.Time) ([]heatmapDayJSON, string) {
	days := make([]heatmapDayJSON, 0, len(hm.Days))
	today := now.Format("2006-01-02")
	mode := heatmapMode(len(hm.Days))
	for i, date := range hm.Days {
		day := heatmapDayJSON{Date: date, Total: hm.Totals[i], Future: date > today}
		switch mode {
		case "day_hour":
			day.Hours = append([]int64(nil), hm.Values[i]...)
		case "day_block":
			day.Blocks = make([]int64, 6)
			for h, v := range hm.Values[i] {
				day.Blocks[h/4] += v
			}
		}
		days = append(days, day)
	}
	return days, mode
}

// heatmapMode 按范围天数返回热力展示模式(与 buildHeatmapDays 的分支一致)。
func heatmapMode(days int) string {
	switch {
	case days <= heatmapShortMaxDays:
		return "day_hour"
	case days <= heatmapBlockMaxDays:
		return "day_block"
	default:
		return "calendar"
	}
}
