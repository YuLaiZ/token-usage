package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/charts"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// chartBarKinds 是柱状图维度(时间维度);chartPieKinds 是饼图维度(占比类)。
// 时间维度不提供饼图形态(day 366 扇区、hour 24 项图例溢出,不可读)。
var (
	chartBarKinds = map[string]bool{"day": true, "hour": true, "weekday": true, "month": true}
	chartPieKinds = map[string]bool{"client": true, "model": true, "provider": true, "project": true}
)

// chartKindValid 报告 kind 是否为合法图表类别(柱状/饼图/热力矩阵)。
func chartKindValid(kind string) bool {
	return chartBarKinds[kind] || chartPieKinds[kind] || kind == "heatmap"
}

// parseChartKind 从 /api/chart/{kind}.svg 路径解析图表类别:仅接受单段
// ".svg" 结尾形态;后缀缺失、含路径分隔符或 kind 非法均返回 false(404)。
func parseChartKind(path string) (string, bool) {
	const prefix = "/api/chart/"
	rest, found := strings.CutPrefix(path, prefix)
	if !found || !strings.HasSuffix(rest, ".svg") {
		return "", false
	}
	kind := strings.TrimSuffix(rest, ".svg")
	if kind == "" || strings.Contains(kind, "/") || !chartKindValid(kind) {
		return "", false
	}
	return kind, true
}

// handleChart 输出单张 SVG 图表:kind ∈ day|hour|weekday|month(柱状)、
// client|model|provider|project(饼图)、heatmap(热力矩阵)。日期参数与
// /api/dashboard 完全一致(共用 parseRangeQuery);图表组装与 chart/report
// 命令共用 charts 包(标题、副标题、悬停文案逐字节同构)。
func (s *server) handleChart(w http.ResponseWriter, r *http.Request) {
	kind, ok := parseChartKind(r.URL.Path)
	if !ok {
		writeError(w, apiError{
			status: http.StatusNotFound,
			en:     fmt.Sprintf("unknown chart kind in %q (allowed: day, hour, weekday, month, client, model, provider, project, heatmap)", r.URL.Path),
			zh:     fmt.Sprintf("未知图表类别 %q（允许：day, hour, weekday, month, client, model, provider, project, heatmap）", r.URL.Path),
		})
		return
	}
	dates, from, to, _, _, perr := parseRangeQuery(r, time.Now())
	if perr != nil {
		writeError(w, *perr)
		return
	}
	// 图表区间标签:单日仅日期,区间用 " ~ " 连接。
	rangeLabel := from
	if from != to {
		rangeLabel = from + " ~ " + to
	}
	pie := chartPieKinds[kind]
	ctx := r.Context()
	var svg string
	err := s.q.ReadTx(ctx, func(tq *querier.Querier) error {
		if kind == "heatmap" {
			// 热力图副标题消费区间汇总口径(同 chart 命令);柱状/饼图的副标题
			// 由 BuildDimensionSVG 内部按维度总计生成,无需在此预查全量聚合。
			stats, err := tq.StatsBetween(ctx, from, to)
			if err != nil {
				return err
			}
			subtitle := fmt.Sprintf("Total %s tokens / %d requests",
				querier.FormatTokens(stats.Total.TotalTokens), stats.Total.Requests)
			svg, err = charts.Heatmap(ctx, tq, dates, subtitle)
			return err
		}
		rows, totals, err := tq.AggregateDimensionView(ctx, dates, querier.DimensionView{
			Dimensions: []string{kind},
			Aliases:    aliasesFor(kind, s.providerAliases),
			TitleEn:    "chart", TitleZh: "chart",
		})
		if err != nil {
			return err
		}
		svg = charts.BuildDimensionSVG(kind, rangeLabel, pie, rows, totals)
		return nil
	})
	if err != nil {
		writeInternal(w, "chart query failed", "图表查询失败", err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	_, _ = w.Write([]byte(svg))
}
