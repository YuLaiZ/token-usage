package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// queryView 标识 query 命令的互斥视图。裸 query 与 query client 共用 viewClient。
type queryView int

const (
	viewClient queryView = iota
	viewModel
	viewProject
	viewSessions
	viewSummary
)

func newQueryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query [YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: "Query token usage statistics / 查询 token 使用统计",
		Long: ui.Bi(
			"Query token usage statistics. Accepts one optional positional arg: a single date YYYYMMDD or a range YYYYMMDD-YYYYMMDD; defaults to today.",
			"查询 token 使用统计。可附加一个位置参数：单个日期 YYYYMMDD 或日期范围 YYYYMMDD-YYYYMMDD；缺省时默认今天。",
		),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQuery(cmd, args, viewClient)
		},
	}

	cmd.AddCommand(
		newQuerySubCmd("client", "Group by client (default) / 按客户端分组（默认）", viewClient),
		newQuerySubCmd("model", "Group by model / 按模型分组", viewModel),
		newQuerySubCmd("project", "Group by project / 按项目分组", viewProject),
		newQuerySubCmd("sessions", "View session details / 查看会话明细", viewSessions),
		newQuerySubCmd("summary", "View summary / 查看总览摘要", viewSummary),
	)

	return cmd
}

// newQuerySubCmd 构造 query 的一个子命令。子命令仅选定视图，
// 配置加载/DB 打开/日期解析/输出/异常提示逻辑全部复用 runQuery。
func newQuerySubCmd(name, short string, view queryView) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: short,
		Long: short + " " + ui.Bi(
			"Accepts one optional positional arg: a single date YYYYMMDD or a range YYYYMMDD-YYYYMMDD; defaults to today.",
			"。可附加一个位置参数：单个日期 YYYYMMDD 或日期范围 YYYYMMDD-YYYYMMDD；缺省时默认今天。",
		),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQuery(cmd, args, view)
		},
	}
}

// runQuery 是 query 及其五个子命令的公共执行入口。
func runQuery(cmd *cobra.Command, args []string, view queryView) error {
	return runQueryWithDeps(cmd, args, view, loadConfig, dbOpener)
}

func runQueryWithDeps(
	cmd *cobra.Command,
	args []string,
	view queryView,
	load func() (*config.Config, error),
	open func(string) (*db.DB, error),
) error {
	// 参数错误必须在配置、日志或 DB 等运行时资源打开之前返回。
	dates, err := parseDateArgs(args, true, "query")
	if err != nil {
		return err
	}

	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}

	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := open(dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	return executeQueryDates(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, view)
}

// executeQuery 执行 query 的视图分发核心逻辑（不依赖 cobra，便于单测）：
// 参数解析 → querier 分发 → 输出 → 异常提示。
// 警告统一写到与结果相同的 out；query 固定使用 table 输出，
// 所有视图固定 table 输出，不再有 JSON/CSV 分流。
func executeQuery(out io.Writer, usageDB *db.DB, args []string, view queryView) error {
	return executeQueryContext(context.Background(), out, usageDB, args, view)
}

func executeQueryContext(ctx context.Context, out io.Writer, usageDB *db.DB, args []string, view queryView) error {
	// 参数解析先于分发，非法日期即时报错。
	dates, err := parseDateArgs(args, true, "query")
	if err != nil {
		return err
	}
	return executeQueryDates(ctx, out, usageDB, dates, view)
}

func executeQueryDates(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, view queryView) error {
	q := querier.New(usageDB)

	var result string
	var err error
	switch view {
	case viewModel:
		result, err = q.ByModel(ctx, dates)
	case viewProject:
		result, err = q.ByProject(ctx, dates)
	case viewSessions:
		result, err = q.Sessions(ctx, dates)
	case viewSummary:
		result, err = q.Summary(ctx, dates)
	default:
		result, err = q.ByClient(ctx, dates)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	fmt.Fprintln(out, result)
	return showErrorWarningsContext(ctx, out, usageDB, dates)
}

func showErrorWarnings(out io.Writer, usageDB *db.DB, dates []string) error {
	return showErrorWarningsContext(context.Background(), out, usageDB, dates)
}

func showErrorWarningsContext(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string) error {
	errs, err := db.GetErrorsContext(ctx, usageDB, db.ErrorFilter{Dates: dates, Unresolved: true})
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to query collection errors", "查询采集异常失败"), err)
	}
	if len(errs) == 0 {
		return nil
	}

	fmt.Fprintf(out, "\n⚠️  %s：\n", ui.Bi(
		fmt.Sprintf("collection errors (%d)", len(errs)),
		fmt.Sprintf("采集异常（%d 条）", len(errs)),
	))
	for _, e := range errs {
		msg := truncateRunes(e.Message, 50)
		fmt.Fprintf(out, "  - %s (%s): %s", e.Source, e.Date, msg)
		if e.RetryCount > 0 {
			fmt.Fprintf(out, "，%s", ui.Bi(fmt.Sprintf("retried %d times", e.RetryCount), fmt.Sprintf("已重试 %d 次", e.RetryCount)))
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, ui.Bi("  Run `token-usage errors` for details, `token-usage collect retry` to retry", "  运行 `token-usage errors` 查看详情，`token-usage collect retry` 重试"))
	return nil
}
