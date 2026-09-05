package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newChartCmd() *cobra.Command {
	return newChartCmdWithDeps(loadConfig, db.Open)
}

func newChartCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chart [DATE|DATE-DATE]",
		Short: "Render daily usage as an SVG bar chart / 将按日用量渲染为 SVG 柱状图",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于 DB 打开,非法日期在打开库之前即报错;默认今天。
			dates, err := parseDateArgs(args, true, "chart")
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

			// 复用维度聚合核的 day 视图:升序逐日 total,缺口日自动补零,
			// 与 query day/export day 的行集合完全一致。
			q := querier.New(usageDB)
			rows, totals, err := q.AggregateDimensionView(cmdContext(cmd), dates, querier.DimensionView{
				Dimensions: []string{"day"},
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
			svg := buildBarSVG(title, subtitle, bars)

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
	return cmd
}
