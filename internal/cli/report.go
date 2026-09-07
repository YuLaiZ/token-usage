package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// reportFile 是报告包中的一个文件:相对路径与内容生成器。
type reportFile struct {
	name    string
	render  func() (string, error)
	summary bool // true=纯文本, false=SVG
}

func newReportCmd() *cobra.Command {
	return newReportCmdWithDeps(loadConfig, db.Open)
}

func newReportCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report [DATE|DATE-DATE] --out <dir>",
		Short: "Generate a full usage report bundle / 生成完整用量报告包",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于 DB 打开与目录创建。
			dates, err := parseDateArgs(args, true, "report")
			if err != nil {
				return err
			}
			outDir, _ := cmd.Flags().GetString("out")
			if outDir == "" {
				return fmt.Errorf("%s", ui.Bi(
					"--out <dir> is required (the report is a bundle of files)",
					"必须指定 --out <目录>（报告为多文件包）",
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

			q := querier.New(usageDB)
			ctx := cmdContext(cmd)

			rangeLabel := dates[0]
			if len(dates) > 1 {
				rangeLabel = dates[0] + " ~ " + dates[len(dates)-1]
			}

			// compare.txt 的缺省基线按原始日期参数粒度推导,与 compare 命令
			// 合同一致;粒度须在逐日展开前保留(见 reportDateGranularity)。
			singleLen, err := reportDateGranularity(args)
			if err != nil {
				return err
			}
			files, err := reportFiles(ctx, q, dates, rangeLabel, singleLen)
			if err != nil {
				return err
			}

			// 目标目录不存在时自动创建(报告包是输出产物,用户不应预建目录)。
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to create report directory", "创建报告目录失败"), err)
			}

			// 逐文件渲染并原子写入;单文件失败即中止,已写文件保留(各自原子,
			// 不产生半成品文件)。
			written := 0
			for _, f := range files {
				data, err := f.render()
				if err != nil {
					return fmt.Errorf("%s: %w", ui.Bi(
						fmt.Sprintf("failed to render %s", f.name),
						fmt.Sprintf("渲染 %s 失败", f.name)), err)
				}
				target := filepath.Join(outDir, f.name)
				if err := fileutil.ReplaceCompleteFile(target, []byte(data), 0o644); err != nil {
					return fmt.Errorf("%s: %w", ui.Bi("failed to write report file", "写入报告文件失败"), err)
				}
				written++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ui.Bi(
				fmt.Sprintf("report written to %s (%d files)", outDir, written),
				fmt.Sprintf("报告已写入 %s（%d 个文件）", outDir, written),
			))
			return nil
		},
	}

	cmd.Flags().String("out", "", ui.Bi("Target directory for the report bundle (required)", "报告包的目标目录（必填）"))
	return cmd
}

// reportDateGranularity 返回原始日期参数的粒度,供 compare.txt 的缺省基线
// 按粒度推导(与 compare 命令合同一致):dates 已逐日展开,整月区间与单月
// 参数展开结果相同,粒度必须在展开前的命令边界保留。无参数时缺省今天,
// 即单日粒度。parseDateArgs 已按同构规则校验过参数,这里的解析错误仅作
// 防御性透传。
func reportDateGranularity(args []string) (int, error) {
	if len(args) != 1 {
		return 8, nil
	}
	_, _, singleLen, err := parseCompareRangeArg(args[0])
	if err != nil {
		return 0, err
	}
	return singleLen, nil
}

// reportFiles 组装报告包的全部文件:文本摘要 + 两期用量对比文本 + 各维度
// SVG 图表 + SVG 热力矩阵。渲染器与对应的 query/chart/compare 视图共用同一
// 聚合核。singleLen 是原始日期参数的粒度(8/6/4=单日/单月/单年,0=区间)。
func reportFiles(ctx context.Context, q *querier.Querier, dates []string, rangeLabel string, singleLen int) ([]reportFile, error) {
	// summary 文本带上统计范围/数据截至/最近采集三项,与 query summary 的
	// 终端输出对齐(报告包的主文本文件可自证统计范围)。
	fresh, err := q.Freshness(ctx, dates)
	if err != nil {
		return nil, err
	}
	header := queryStatisticsHeader(dates[0], dates[len(dates)-1], fresh)
	summary, err := q.Summary(ctx, dates)
	if err != nil {
		return nil, err
	}
	// 副标题与柱状/饼图共用区间汇总口径。
	rangeStats, err := q.StatsBetween(ctx, dates[0], dates[len(dates)-1])
	if err != nil {
		return nil, err
	}
	subtitle := fmt.Sprintf("Total %s tokens / %d requests",
		querier.FormatTokens(rangeStats.Total.TotalTokens), rangeStats.Total.Requests)
	heatmapSVG, err := buildChartHeatmap(ctx, q, dates, subtitle)
	if err != nil {
		return nil, err
	}

	// compare.txt:本期与缺省基线窗口的用量对比。缺省基线规则与 compare 命令
	// 完全一致(defaultCompareBase 按原始参数粒度分派:单日取前一天、单月取
	// 上一个日历月、单年取上一个日历年、区间取结束于开始日前一天的等长窗口,
	// 天数用纯 AddDate 循环推导)。当前窗口总量复用上方的 rangeStats(同一
	// 查询,不重复聚合),基线窗口单独查一次;渲染复用 compare 的 renderCompare,
	// 用 bytes.Buffer 承接。
	curStartT, err := time.Parse("2006-01-02", dates[0])
	if err != nil {
		return nil, err
	}
	curEndT, err := time.Parse("2006-01-02", dates[len(dates)-1])
	if err != nil {
		return nil, err
	}
	baseStartT, baseEndT := defaultCompareBase(curStartT, curEndT, singleLen)
	baseStats, err := q.StatsBetween(ctx, baseStartT.Format("2006-01-02"), baseEndT.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	var compareBuf bytes.Buffer
	if err := renderCompare(&compareBuf, compareRenderInput{
		curStart:  dates[0],
		curEnd:    dates[len(dates)-1],
		baseStart: baseStartT.Format("2006-01-02"),
		baseEnd:   baseEndT.Format("2006-01-02"),
		cur:       rangeStats,
		base:      baseStats,
	}); err != nil {
		return nil, err
	}

	// compare.txt 紧随 summary.txt,作为报告包的第二个文本文件;summary
	// 标记沿「true=纯文本」语义(写盘流程不消费该字段,仅作类型标注)。
	files := []reportFile{
		{name: "summary.txt", summary: true, render: func() (string, error) {
			return header + "\n" + summary, nil
		}},
		{name: "compare.txt", summary: true, render: func() (string, error) {
			return compareBuf.String(), nil
		}},
		{name: "heatmap.svg", render: func() (string, error) { return heatmapSVG, nil }},
	}
	// 单维度图表:柱状(day/hour/weekday/month)+ 饼图(占比类维度)。
	dimensionCharts := []struct {
		file, by string
		pie      bool
	}{
		{"daily.svg", "day", false},
		{"hourly.svg", "hour", false},
		{"weekday.svg", "weekday", false},
		{"monthly.svg", "month", false},
		{"by-client.svg", "client", true},
		{"by-model.svg", "model", true},
		{"by-provider.svg", "provider", true},
		{"by-project.svg", "project", true},
	}
	for _, dc := range dimensionCharts {
		by, pie, file := dc.by, dc.pie, dc.file
		files = append(files, reportFile{
			name: file,
			render: func() (string, error) {
				rows, totals, err := q.AggregateDimensionView(ctx, dates, querier.DimensionView{
					Dimensions: []string{by},
					TitleEn:    "chart", TitleZh: "chart",
				})
				if err != nil {
					return "", err
				}
				sub := fmt.Sprintf("Total %s tokens / %d requests",
					querier.FormatTokens(totals.TotalTokens), totals.Requests)
				chartTitle := chartTitleFor(rangeLabel, by)
				if pie {
					slices := make([]chartSlice, 0, len(rows))
					colorIdx := 0
					for _, row := range rows {
						if row.Agg.TotalTokens <= 0 {
							continue
						}
						label := row.Keys[0]
						slices = append(slices, chartSlice{
							label: label, value: row.Agg.TotalTokens,
							hover: fmt.Sprintf("%s: %s tokens, %d requests", label,
								querier.FormatTokens(row.Agg.TotalTokens), row.Agg.Requests),
							color: piePalette[colorIdx%len(piePalette)],
						})
						colorIdx++
					}
					return buildPieSVG(chartTitle, sub, slices), nil
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
				return buildBarSVG(chartTitleFor(rangeLabel, by), sub, bars), nil
			},
		})
	}
	return files, nil
}
