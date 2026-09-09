package cli

import (
	"fmt"
	"io"
	"path/filepath"
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
			"Show the heaviest sessions by total tokens. Accepts one optional positional arg with the same forms as query: DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months; defaults to today. Sessions are ranked by total tokens descending, with ties broken by client then title ascending (full ties by first message timestamp ascending), and only the top --limit rows are printed (--limit defaults to 10; values below 1 are rejected before the database opens). --format selects the output format: table (default, the framed ranking table) or json (machine-readable: raw integers, two-space indentation); invalid values are rejected before the database opens. The command is read-only.",
			"显示总用量最重的会话排行。可附加一个位置参数，形态与 query 一致：DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月；缺省时默认今天。会话按总用量降序排列，同值按客户端、再按标题升序决定先后（全并列时按首条消息时间戳升序），只显示前 --limit 行（--limit 默认 10，小于 1 的取值在打开数据库之前即被拒绝）。--format 选择输出格式：table（默认，框线排行表）或 json（机器可读、原始整数、两空格缩进）；非法值在打开数据库之前即被拒绝。本命令只读。",
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

			// --format 白名单同样先于配置加载与数据库打开校验（句式与 compare 一致）。
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format != "table" && format != "json" {
				return topFormatError(format)
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
			if format == "json" {
				return renderTopJSON(cmd.OutOrStdout(), rows, limit)
			}
			return renderTop(cmd.OutOrStdout(), rows, limit)
		},
	}
	cmd.Flags().Int("limit", defaultTopLimit, ui.Bi(
		"Number of sessions to show",
		"显示的会话数量",
	))
	// --format 是本地 flag,缺省 table;取值白名单在配置加载与开库之前校验。
	cmd.Flags().String("format", "table", ui.Bi(
		"output format: table or json",
		"输出格式：table 或 json",
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

// topFormatError 非法 --format 取值:在加载配置与开库之前拒绝(句式与 compare
// 的同名错误一致)。
func topFormatError(value string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --format %q (allowed: table, json)", value),
		fmt.Sprintf("无效的 --format %q（允许：table、json）", value),
	))
}

// sortTopRows 返回按 top 排行口径排序的行独立副本,排序实现收敛在 querier
// 包(TotalTokens 降序,同值按 Client 升序、再 Title 升序,全并列按 FirstTS
// 升序),cli 的 top/report 与 web 仪表板共用同一排序口径。
func sortTopRows(rows []querier.SessionRow) []querier.SessionRow {
	return querier.SortTopRows(rows)
}

// truncateTopRows 取排序后前 limit 行,实现收敛在 querier 包(同上)。
func truncateTopRows(rows []querier.SessionRow, limit int) []querier.SessionRow {
	return querier.TruncateTopRows(rows, limit)
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

// topJSONRecord 是 top --format json 的单条记录投影:rank 从 1 起,与表格名次
// 列同源;client/project/title 为源字段原值(空 project 保持 "",「未分类」是
// 表格渲染方 concern,与 SessionRows 的「显示形态由渲染方决定」约定一致);
// first_ts/last_ts 为 Unix 毫秒时间戳,duration_ms 是二者之差;指标为 7 字段
// 全量原始整数(表格只展示 requests/total 两列,JSON 为超集,不做 K/M 缩写)。
// 定义顺序即 JSON 键顺序。
type topJSONRecord struct {
	Rank        int    `json:"rank"`
	Client      string `json:"client"`
	Project     string `json:"project"`
	Title       string `json:"title"`
	FirstTS     int64  `json:"first_ts"`
	LastTS      int64  `json:"last_ts"`
	DurationMS  int64  `json:"duration_ms"`
	Requests    int64  `json:"requests"`
	FreshInput  int64  `json:"fresh_input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheCreate int64  `json:"cache_create"`
	Reasoning   int64  `json:"reasoning"`
	Total       int64  `json:"total"`
}

// renderTopJSON 输出会话排行的机器可读 JSON:根为数组,排序与截断经
// sortTopRows/truncateTopRows 与表格行完全一致;stdout 纯数据,无标题行,
// 空结果输出 [] 而非 null(JSON 不做表格的 no data 早退,与 errors/compare
// 的机器输出约定一致);两空格缩进与尾随换行复用 marshalExportJSON。
func renderTopJSON(w io.Writer, rows []querier.SessionRow, limit int) error {
	records := make([]topJSONRecord, 0, len(rows))
	for i, row := range truncateTopRows(sortTopRows(rows), limit) {
		records = append(records, topJSONRecord{
			Rank:        i + 1,
			Client:      row.Client,
			Project:     row.Project,
			Title:       row.Title,
			FirstTS:     row.FirstTS,
			LastTS:      row.LastTS,
			DurationMS:  row.LastTS - row.FirstTS,
			Requests:    row.Agg.Requests,
			FreshInput:  row.Agg.FreshInput,
			Output:      row.Agg.OutputTokens,
			CacheRead:   row.Agg.CacheRead,
			CacheCreate: row.Agg.CacheCreate,
			Reasoning:   row.Agg.Reasoning,
			Total:       row.Agg.TotalTokens,
		})
	}
	s, err := marshalExportJSON(records)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to encode top sessions as JSON", "会话排行 JSON 编码失败"), err)
	}
	_, err = io.WriteString(w, s)
	return err
}
