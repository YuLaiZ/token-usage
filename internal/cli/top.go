package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// defaultTopLimit 是 top 命令 --limit 的缺省显示行数。
const defaultTopLimit = 10

func newTopCmd() *cobra.Command {
	return newTopCmdWithDeps(loadConfig, db.Open)
}

// newTopCmdWithDeps 构造 top 命令;load/open 可注入供包内测试走真实调用链
// (生产路径传入 loadConfig 与 db.Open)。只读命令,不触碰 daemon。
func newTopCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "top [DATE|DATE-DATE]",
		Short: "Show the heaviest sessions by total tokens / 显示总用量最重的会话排行",
		Long: ui.Bi(
			"Show the heaviest sessions by total tokens. Accepts one optional positional arg with the same forms as query: DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months; defaults to today. Sessions are ranked by total tokens descending, with ties broken by client then title ascending (full ties by first message timestamp ascending), and only the top --limit rows are printed (--limit defaults to 10; values below 1 are rejected before the database opens). The command is read-only.",
			"显示总用量最重的会话排行。可附加一个位置参数，形态与 query 一致：DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月；缺省时默认今天。会话按总用量降序排列，同值按客户端、再按标题升序决定先后（全并列时按首条消息时间戳升序），只显示前 --limit 行（--limit 默认 10，小于 1 的取值在打开数据库之前即被拒绝）。本命令只读。",
		),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --limit 校验先于配置加载与数据库打开，非法输入即时报错、不占运行时资源。
			limit, err := cmd.Flags().GetInt("limit")
			if err != nil {
				return err
			}
			if limit < 1 {
				return topLimitError(limit)
			}
			// 日期解析同样先于配置与数据库打开（与 query 一致）。
			dates, err := parseDateArgs(args, true, "top")
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

			rows, err := querier.New(usageDB).SessionRows(cmdContext(cmd), dates)
			if err != nil {
				return err
			}
			return renderTop(cmd.OutOrStdout(), rows, limit)
		},
	}
	cmd.Flags().Int("limit", defaultTopLimit, ui.Bi(
		"Number of sessions to show",
		"显示的会话数量",
	))
	return cmd
}

// topLimitError 非法 --limit 取值:在加载配置与开库之前拒绝(句式与 compare
// 的 --format 错误一致)。
func topLimitError(value int) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --limit %d (must be at least 1)", value),
		fmt.Sprintf("无效的 --limit %d（至少为 1）", value),
	))
}

// sortTopRows 返回按 top 排行口径排序的行独立副本:TotalTokens 降序,同值按
// Client 升序、再 Title 升序;前三键全并列时按会话首条消息时间戳 FirstTS 升序
// 决序(确定性全序,不依赖 SessionRows 的 SQL 排序,消除同名同量会话的行序残差)。
func sortTopRows(rows []querier.SessionRow) []querier.SessionRow {
	sorted := make([]querier.SessionRow, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Agg.TotalTokens != sorted[j].Agg.TotalTokens {
			return sorted[i].Agg.TotalTokens > sorted[j].Agg.TotalTokens
		}
		if sorted[i].Client != sorted[j].Client {
			return sorted[i].Client < sorted[j].Client
		}
		if sorted[i].Title != sorted[j].Title {
			return sorted[i].Title < sorted[j].Title
		}
		return sorted[i].FirstTS < sorted[j].FirstTS
	})
	return sorted
}

// truncateTopRows 取排序后前 limit 行(limit 不小于行数时返回全部)。
func truncateTopRows(rows []querier.SessionRow, limit int) []querier.SessionRow {
	if limit < len(rows) {
		return rows[:limit]
	}
	return rows
}

// renderTop 输出会话排行:标题行 + 一张 7 列框线表(名次/标题/客户端/项目/
// 时长/请求数/总量)。排序与截断委托给 sortTopRows/truncateTopRows,行数据来自
// SessionRows(与 query session 同一聚合核)。Title 沿用 query session 的
// 30 显示宽上限截断;空 Project 显示「(uncategorized) / (未分类)」;Duration
// 是会话首末消息跨度,经 querier.FormatDuration 与 query session 同口径;
// Total 沿用 K/M/B 缩写。无数据时只输出无数据一行,不渲染空表。
func renderTop(w io.Writer, rows []querier.SessionRow, limit int) error {
	fmt.Fprintln(w, ui.Bi("Top sessions", "会话排行"))
	if len(rows) == 0 {
		fmt.Fprintln(w, ui.Bi("no data", "无数据"))
		return nil
	}

	t := ui.NewTable([]string{
		"#",
		ui.HTitle,
		ui.HClient,
		ui.HProject,
		ui.HDuration,
		ui.HRequests,
		ui.HTotal,
	},
		ui.AlignRight, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight,
	).Limits(0, 30, 0, 0, 0, 0, 0)

	for i, row := range truncateTopRows(sortTopRows(rows), limit) {
		project := row.Project
		// 「(未分类)」空值映射与 query session 渲染一致,行数据保留源字段原值。
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		t.Row(
			strconv.Itoa(i+1),
			row.Title,
			row.Client,
			project,
			querier.FormatDuration(row.LastTS-row.FirstTS),
			strconv.FormatInt(row.Agg.Requests, 10),
			querier.FormatTokens(row.Agg.TotalTokens),
		)
	}

	fmt.Fprintln(w, t.String())
	return nil
}
