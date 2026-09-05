package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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
			if pie && by == "day" {
				return fmt.Errorf("%s", ui.Bi(
					"--pie requires --by with a non-temporal dimension (client/model/provider/project); day splits would be unreadable",
					"--pie 需要 --by 指定非时间维度（client/model/provider/project）；按天切分饼图不可读",
				))
			}
			heatmap, _ := cmd.Flags().GetBool("heatmap")
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

			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}

			usageDB, err := open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			// 复用维度聚合核:day 视图升序逐日 total 且缺口日自动补零,
			// 与 query day/export day 的行集合完全一致;其余维度按 total
			// 降序(非时间维度的既有排序规则)。
			q := querier.New(usageDB)
			rows, totals, err := q.AggregateDimensionView(cmdContext(cmd), dates, querier.DimensionView{
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
			rangeLabel := dates[0]
			if len(dates) > 1 {
				rangeLabel = dates[0] + " ~ " + dates[len(dates)-1]
			}
			title := "token-usage " + rangeLabel
			subtitle := fmt.Sprintf("Total %s tokens / %d requests",
				querier.FormatTokens(totals.TotalTokens), totals.Requests)

			var svg string
			if heatmap, _ := cmd.Flags().GetBool("heatmap"); heatmap {
				svg = buildChartHeatmap(cmdContext(cmd), q, dates)
			} else if pie {
				slices := make([]chartSlice, 0, len(bars))
				for i, bar := range bars {
					if bar.value <= 0 {
						continue
					}
					slices = append(slices, chartSlice{
						label: bar.label, value: bar.value, hover: bar.hover,
						color: piePalette[i%len(piePalette)],
					})
				}
				svg = buildPieSVG(title+" by "+by, subtitle, slices)
			} else {
				svg = buildBarSVG(title, subtitle, bars)
			}

			outFlag, _ := cmd.Flags().GetString("out")
			if outFlag == "" {
				_, err = fmt.Fprint(cmd.OutOrStdout(), svg)
				return err
			}
			// 原子写:先写临时文件再换名,失败不破坏既有图表文件。
			if err := fileutil.ReplaceCompleteFile(outFlag, []byte(svg), 0o644); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to write chart", "写入图表失败"), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ui.Bi(
				fmt.Sprintf("chart written to %s", outFlag),
				fmt.Sprintf("图表已写入 %s", outFlag),
			))
			return nil
		},
	}

	cmd.Flags().String("out", "", ui.Bi("Write SVG to a file instead of stdout", "将 SVG 写入文件而非标准输出"))
	cmd.Flags().String("by", "day", ui.Bi("Aggregate by dimension: client/model/provider/project/day/month/hour/weekday", "按维度聚合：client/model/provider/project/day/month/hour/weekday"))
	cmd.Flags().Bool("pie", false, ui.Bi("Render a pie chart instead of a bar chart (requires --by, not day)", "渲染饼图而非柱状图（需 --by 且不为 day）"))
	cmd.Flags().Bool("heatmap", false, ui.Bi("Render a weekday-by-hour heat matrix instead of a bar chart", "渲染星期×小时热力矩阵而非柱状图"))
	return cmd
}

// buildChartHeatmap 组装星期×小时热力矩阵 SVG:复用维度聚合核的
// weekday,hour 组合(矩阵交点缺失即零值),行列标签与终端 heatmap 一致
// (ISO 周序星期、本机时区小时)。
func buildChartHeatmap(ctx context.Context, q *querier.Querier, dates []string) string {
	// 数据来自 querier.HeatmapMatrix(与终端 heatmap 同一来源,行列与数值
	// 完全一致);聚合失败时输出空矩阵(全最低级)比静默退出更可观察。
	m, err := q.HeatmapMatrix(ctx, dates)
	if err != nil || m == nil {
		m = &querier.HeatmapMatrix{}
	}
	return buildHeatmapSVG(
		"Weekday x hour heatmap / 星期×小时热力图",
		strings.Join(dates, " ~ "),
		m.Weekdays, m.Hours,
		func(wi, hi int) int64 {
			if wi < len(m.Values) && hi < len(m.Values[wi]) {
				return m.Values[wi][hi]
			}
			return 0
		},
	)
}
