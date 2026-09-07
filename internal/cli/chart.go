package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newChartCmd() *cobra.Command {
	return newChartCmdWithDeps(loadConfig, db.Open)
}

func newChartCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chart [DATE|DATE-DATE]",
		Short: "Render usage as an SVG chart / 将用量渲染为 SVG 图表",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数与选项校验先于 DB 打开,非法输入在打开库之前即报错。
			dates, err := parseDateArgs(args, true, "chart")
			if err != nil {
				return err
			}
			by, _ := cmd.Flags().GetString("by")
			pie, _ := cmd.Flags().GetBool("pie")
			heatmap, _ := cmd.Flags().GetBool("heatmap")
			line, _ := cmd.Flags().GetBool("line")
			if pie && piePaletteBlockedDimensions[by] {
				return fmt.Errorf("%s", ui.Bi(
					"--pie requires --by with a non-temporal dimension (client/model/provider/project); temporal splits produce unreadable pie charts",
					"--pie 需要 --by 指定非时间维度（client/model/provider/project）；时间维度切分的饼图不可读",
				))
			}
			if pie && heatmap {
				return fmt.Errorf("%s", ui.Bi(
					"--pie and --heatmap are mutually exclusive",
					"--pie 与 --heatmap 互斥",
				))
			}
			if !querydef.IsBuiltinDimension(by) {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("unknown --by dimension %q (allowed: client, model, provider, project, day, month, hour, weekday)", by),
					fmt.Sprintf("未知 --by 维度 %q（允许：client, model, provider, project, day, month, hour, weekday）", by),
				))
			}
			if line && (pie || heatmap) {
				return fmt.Errorf("%s", ui.Bi(
					"--line, --pie and --heatmap are mutually exclusive",
					"--line、--pie 与 --heatmap 互斥",
				))
			}
			if line && !lineTemporalDimensions[by] {
				return fmt.Errorf("%s", ui.Bi(
					"--line requires --by with a temporal dimension (day/month/hour/weekday); connecting unrelated categories implies a misleading trend",
					"--line 需要 --by 指定时间维度（day/month/hour/weekday）；把无关类别用线段连接会产生误导性趋势",
				))
			}

			outFlag, _ := cmd.Flags().GetString("out")

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

			rangeLabel := dates[0]
			if len(dates) > 1 {
				rangeLabel = dates[0] + " ~ " + dates[len(dates)-1]
			}
			rangeTotals, err := q.StatsBetween(cmdContext(cmd), dates[0], dates[len(dates)-1])
			if err != nil {
				return err
			}
			totals := rangeTotals.Total
			title := chartTitleFor(rangeLabel, by)
			subtitle := fmt.Sprintf("Total %s tokens / %d requests",
				querier.FormatTokens(totals.TotalTokens), totals.Requests)

			// heatmap 分支直接消费热力矩阵(--by/--pie/--line 与其无关,不浪费聚合),
			// 汇总行沿用同一聚合核的范围统计。
			if heatmap {
				svg, err := buildChartHeatmap(cmdContext(cmd), q, dates, subtitle)
				if err != nil {
					return err
				}
				return writeChartOutput(cmd, outFlag, svg)
			}

			// 柱状/折线/饼图:复用维度聚合核,缺口日自动补零(day)或 total 降序
			// (非时间维度的既有排序规则)。
			rows, _, err := q.AggregateDimensionView(cmdContext(cmd), dates, querier.DimensionView{
				Dimensions: []string{by},
				TitleEn:    "chart", TitleZh: "chart",
			})
			if err != nil {
				return err
			}
			bars := make([]chartBar, 0, len(rows))
			for _, row := range rows {
				if len(row.Keys) != 1 {
					continue
				}
				bars = append(bars, chartBar{
					label: row.Keys[0],
					value: row.Agg.TotalTokens,
					hover: fmt.Sprintf("%s: %s tokens, %d requests", row.Keys[0],
						querier.FormatTokens(row.Agg.TotalTokens), row.Agg.Requests),
				})
			}
			var svg string
			if line {
				// 折线是时间轴趋势,标题与柱状图同形态(chartTitleFor 已含 by)。
				svg = buildLineSVG(title, subtitle, bars)
			} else if pie {
				slices := make([]chartSlice, 0, len(bars))
				colorIdx := 0
				for _, bar := range bars {
					if bar.value <= 0 {
						continue
					}
					slices = append(slices, chartSlice{
						label: bar.label, value: bar.value, hover: bar.hover,
						color: piePalette[colorIdx%len(piePalette)],
					})
					colorIdx++
				}
				svg = buildPieSVG(title, subtitle, slices)
			} else {
				svg = buildBarSVG(title, subtitle, bars)
			}

			return writeChartOutput(cmd, outFlag, svg)
		},
	}

	cmd.Flags().String("out", "", ui.Bi("Write SVG to a file instead of stdout", "将 SVG 写入文件而非标准输出"))
	cmd.Flags().String("by", "day", ui.Bi("Aggregate by dimension: client/model/provider/project/day/month/hour/weekday", "按维度聚合：client/model/provider/project/day/month/hour/weekday"))
	cmd.Flags().Bool("pie", false, ui.Bi("Render a pie chart instead of a bar chart (requires a non-temporal --by: client/model/provider/project)", "渲染饼图而非柱状图（需 --by 且为非时间维度：client/model/provider/project）"))
	cmd.Flags().Bool("line", false, ui.Bi("Render a line chart instead of a bar chart (requires a temporal --by: day/month/hour/weekday)", "渲染折线图而非柱状图（--by 须为时间维度：day/month/hour/weekday）"))
	cmd.Flags().Bool("heatmap", false, ui.Bi("Render a weekday-by-hour heat matrix instead of a bar chart", "渲染星期×小时热力矩阵而非柱状图"))
	return cmd
}

// buildChartHeatmap 组装星期×小时热力矩阵 SVG:复用维度聚合核的
// weekday,hour 组合(矩阵交点缺失即零值),行列标签与终端 heatmap 一致
// (ISO 周序星期、本机时区小时)。查询失败时返回错误——有效空数据本身
// 返回完整零矩阵,吞错降级为空图会把查询错误伪装成无数据。
func buildChartHeatmap(ctx context.Context, q *querier.Querier, dates []string, subtitle string) (string, error) {
	m, err := q.HeatmapMatrix(ctx, dates)
	if err != nil {
		return "", err
	}
	return buildHeatmapSVG(
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

// writeChartOutput 输出 SVG:未指定 --out 时写 stdout,指定时原子写入文件
// (先写临时文件再换名,失败不破坏既有图表文件)并回执路径。
func writeChartOutput(cmd *cobra.Command, outFlag, svg string) error {
	if outFlag == "" {
		_, err := fmt.Fprint(cmd.OutOrStdout(), svg)
		return err
	}
	if err := fileutil.ReplaceCompleteFile(outFlag, []byte(svg), 0o644); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to write chart", "写入图表失败"), err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ui.Bi(
		fmt.Sprintf("chart written to %s", outFlag),
		fmt.Sprintf("图表已写入 %s", outFlag),
	))
	return nil
}

// piePaletteBlockedDimensions 是 --pie 拒绝的维度:时间维度切分的饼图不可读
// (day 366 扇区、hour 24 项图例溢出画布),只有占比类维度适合饼图。
var piePaletteBlockedDimensions = map[string]bool{"day": true, "month": true, "hour": true, "weekday": true}

// lineTemporalDimensions 是 --line 允许的维度:折线表达时间趋势,把无关类别
// (client/model/provider/project)用线段连接会产生误导性趋势。
var lineTemporalDimensions = map[string]bool{"day": true, "month": true, "hour": true, "weekday": true}

// chartTitleFor 统一柱状/折线/饼图标题:by=day(默认按日柱状)时仅区间,
// 其余维度追加 " by <维度>" 与 report 包内图表命名一致。
func chartTitleFor(rangeLabel, by string) string {
	if by == "day" {
		return "token-usage " + rangeLabel
	}
	return "token-usage " + rangeLabel + " by " + by
}
