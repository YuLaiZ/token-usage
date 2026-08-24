package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newErrorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "errors [YYYYMMDD]",
		Short: "View collection errors / 查看采集异常",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于 DB 打开，非法日期在打开库之前即报错。
			date, err := parseErrorDateArg(args)
			if err != nil {
				return err
			}

			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}

			dbPath := filepath.Join(cfg.DataDir, "usage.db")
			usageDB, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			sourceFlag, _ := cmd.Flags().GetString("source")
			unresolvedFlag, _ := cmd.Flags().GetBool("unresolved")

			filter := buildErrorsFilter(date, sourceFlag, unresolvedFlag)

			return runErrorsContext(cmdContext(cmd), usageDB, cmd.OutOrStdout(), filter)
		},
	}

	cmd.Flags().String("source", "", ui.Bi("Filter by client (claude/opencode/codex/workbuddy/zcode/autoclaw)", "指定数据源 (claude/opencode/codex/workbuddy/zcode/autoclaw)"))
	cmd.Flags().Bool("unresolved", false, ui.Bi("Show unresolved errors only", "只看未解决的异常"))

	return cmd
}

// buildErrorsFilter 构造 errors 命令的 ErrorFilter。
//
// 默认语义（保留）：无日期且无 source 时只看未解决（Unresolved=true）；
// 一旦给出日期或 source，则默认看全部状态。
// 显式 --unresolved 始终置 Unresolved=true。
func buildErrorsFilter(date, source string, unresolved bool) db.ErrorFilter {
	f := db.ErrorFilter{
		Dates:      nil,
		Source:     source,
		Unresolved: unresolved || (date == "" && source == ""),
	}
	if date != "" {
		f.Dates = []string{date}
	}
	return f
}

// runErrors 可测试的 errors 命令核心逻辑
func runErrors(usageDB *db.DB, out io.Writer, filter db.ErrorFilter) error {
	return runErrorsContext(context.Background(), usageDB, out, filter)
}

func runErrorsContext(ctx context.Context, usageDB *db.DB, out io.Writer, filter db.ErrorFilter) error {
	errs, err := db.GetErrorsContext(ctx, usageDB, filter)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to query errors", "查询异常失败"), err)
	}

	if len(errs) == 0 {
		fmt.Fprintln(out, ui.Bi("No error records", "暂无异常记录"))
		return nil
	}

	fmt.Fprintf(out, "⚠️  %s：\n\n", ui.Bi(
		fmt.Sprintf("collection errors (%d)", len(errs)),
		fmt.Sprintf("采集异常（%d 条）", len(errs)),
	))

	widths := []int{errColID, errColDate, errColClient, errColMessage, errColRetries, errColStatus}
	header := []string{
		"ID",
		ui.ColDate,
		ui.ColClient,
		ui.Bi("Message", "错误信息"),
		ui.Bi("Retries", "重试次数"),
		ui.Bi("Status", "状态"),
	}

	fmt.Fprintln(out, errorsTableBorder(widths, "┌", "┬", "┐"))
	fmt.Fprintln(out, errorsTableRow(header, widths))
	fmt.Fprintln(out, errorsTableBorder(widths, "├", "┼", "┤"))

	for _, e := range errs {
		status := ui.Bi("unresolved", "未解决")
		if e.Resolved {
			status = ui.Bi("resolved", "已解决")
		}
		row := []string{
			strconv.Itoa(e.ID),
			e.Date,
			e.Source,
			e.Message,
			strconv.Itoa(e.RetryCount),
			status,
		}
		fmt.Fprintln(out, errorsTableRow(row, widths))
	}

	fmt.Fprintln(out, errorsTableBorder(widths, "└", "┴", "┘"))
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi(
		"Run `token-usage collect retry` to retry all, `token-usage collect retry --client <name>` for one client.",
		"运行 `token-usage collect retry` 重试，`token-usage collect retry --client <name>` 重试指定数据源。",
	))

	return nil
}

// errors 表格列宽（显示宽度，含双语表头与状态值），补齐与截断统一走 runewidth。
// 中文按显示宽度占 2 列，宽度按双语表头/状态值实测重估，非 rune 计数。
const (
	errColID      = 3  // ID
	errColDate    = 11 // Date / 日期
	errColClient  = 15 // Client / 客户端
	errColMessage = 50
	errColRetries = 18 // Retries / 重试次数
	errColStatus  = 19 // unresolved / 未解决
)

// padDisplay 按显示宽度右补空格；超宽先截断（尾部 ...），中文与双语文本不穿透框线。
func padDisplay(s string, w int) string {
	if runewidth.StringWidth(s) > w {
		s = runewidth.Truncate(s, w, "...")
	}
	if pad := w - runewidth.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// errorsTableBorder 生成 errors 框线表的横线行。
func errorsTableBorder(widths []int, left, mid, right string) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		parts[i] = strings.Repeat("─", w+2)
	}
	return left + strings.Join(parts, mid) + right
}

// errorsTableRow 生成 errors 框线表的数据行，单元格按显示宽度补齐。
func errorsTableRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = " " + padDisplay(c, widths[i]) + " "
	}
	return "│" + strings.Join(parts, "│") + "│"
}

// truncateRunes 按显示宽度截断：超宽截到 max 显示宽度内，尾部补 ...（极窄列放不下省略号时硬截）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if max <= 3 {
		return runewidth.Truncate(s, max, "")
	}
	return runewidth.Truncate(s, max, "...")
}
