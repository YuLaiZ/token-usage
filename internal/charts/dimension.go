package charts

import (
	"context"
	"fmt"

	"github.com/YuLaiZ/token-usage/internal/querier"
)

// TitleFor 统一柱状/折线/饼图标题:by=day(默认按日柱状)时仅区间,
// 其余维度追加 " by <维度>",与报告包内图表命名一致。
func TitleFor(rangeLabel, by string) string {
	if by == "day" {
		return "token-usage " + rangeLabel
	}
	return "token-usage " + rangeLabel + " by " + by
}

// DimensionBars 把单维度聚合行转为柱状/折线数据点:非单键行跳过(防御),
// 悬停文案与 report/chart 各入口同源(唯一实现)。
func DimensionBars(rows []querier.DimensionRow) []Bar {
	bars := make([]Bar, 0, len(rows))
	for _, row := range rows {
		if len(row.Keys) != 1 {
			continue
		}
		bars = append(bars, Bar{
			Label: row.Keys[0],
			Value: row.Agg.TotalTokens,
			Hover: fmt.Sprintf("%s: %s tokens, %d requests", row.Keys[0],
				querier.FormatTokens(row.Agg.TotalTokens), row.Agg.Requests),
		})
	}
	return bars
}

// DimensionSlices 把非时间维度的聚合行转为饼图扇区:非正值跳过(零值扇区
// 不可读),取色按排序位次循环;调用方保证 by 为占比类维度。
func DimensionSlices(rows []querier.DimensionRow) []Slice {
	slices := make([]Slice, 0, len(rows))
	colorIdx := 0
	for _, row := range rows {
		if len(row.Keys) != 1 || row.Agg.TotalTokens <= 0 {
			continue
		}
		label := row.Keys[0]
		slices = append(slices, Slice{
			Label: label,
			Value: row.Agg.TotalTokens,
			Hover: fmt.Sprintf("%s: %s tokens, %d requests", label,
				querier.FormatTokens(row.Agg.TotalTokens), row.Agg.Requests),
			Color: piePalette[colorIdx%len(piePalette)],
		})
		colorIdx++
	}
	return slices
}

// BuildDimensionSVG 把「单维度聚合行 + 区间汇总」组装为一张 SVG 图:
// pie=false 生成柱状图,pie=true 生成饼图;标题按 TitleFor 规则,副标题与
// report/chart 命令共用区间汇总口径。rows 的显示键须已完成别名合并
// (AggregateDimensionView 的返回值即满足)。report 包与 web 的
// /api/chart 接口共用本函数,保证两个载体的图表逐字节同构。
func BuildDimensionSVG(by, rangeLabel string, pie bool, rows []querier.DimensionRow, totals querier.GroupAggregate) string {
	title := TitleFor(rangeLabel, by)
	subtitle := fmt.Sprintf("Total %s tokens / %d requests",
		querier.FormatTokens(totals.TotalTokens), totals.Requests)
	if pie {
		return PieSVG(title, subtitle, DimensionSlices(rows))
	}
	return BarSVG(title, subtitle, DimensionBars(rows))
}

// Heatmap 组装星期×小时热力矩阵 SVG:复用维度聚合核的 weekday,hour 组合
// (矩阵交点缺失即零值),行列标签与终端 heatmap 一致(ISO 周序星期、本机
// 时区小时)。查询失败时返回错误——有效空数据本身返回完整零矩阵,吞错降级
// 为空图会把查询错误伪装成无数据。
func Heatmap(ctx context.Context, q *querier.Querier, dates []string, subtitle string) (string, error) {
	m, err := q.HeatmapMatrix(ctx, dates)
	if err != nil {
		return "", err
	}
	return HeatmapSVG(
		"Weekday x hour heatmap / 星期×小时热力图",
		subtitle,
		m.Weekdays, m.Hours,
		func(wi, hi int) int64 {
			if wi < len(m.Values) && hi < len(m.Values[wi]) {
				return m.Values[wi][hi]
			}
			return 0
		},
	), nil
}
