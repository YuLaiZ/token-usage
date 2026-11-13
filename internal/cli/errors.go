package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newErrorsCmd() *cobra.Command {
	return newErrorsCmdWithDeps(loadConfig, db.Open)
}

// newErrorsCmdWithDeps 构造 errors 命令；load/open 可注入供包内测试走真实
// 调用链或在开库前断言拒绝。
func newErrorsCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "errors [DATE|DATE-DATE]",
		Short: "View collection errors / 查看采集异常",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于 DB 打开，非法日期在打开库之前即报错。
			// 日期形态与 collect/query 一致：单日 YYYYMMDD、月 YYYYMM、年 YYYY
			// 或区间 DATE-DATE，展开上限与错误文案统一。
			dates, err := parseDateArgs(args, false, "errors")
			if err != nil {
				return err
			}

			// --format 白名单先于配置加载与数据库打开校验。
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format != "table" && format != "json" {
				return errorsFormatError(format)
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

			sourceFlag, _ := cmd.Flags().GetString("source")
			unresolvedFlag, _ := cmd.Flags().GetBool("unresolved")

			filter := buildErrorsFilter(dates, sourceFlag, unresolvedFlag)

			return runErrorsContext(cmdContext(cmd), usageDB, cmd.OutOrStdout(), filter, format)
		},
	}

	cmd.Flags().String("source", "", ui.Bi("Filter by client (claude/opencode/codex/mimocode/workbuddy/zcode/autoclaw/dsh)", "指定数据源 (claude/opencode/codex/mimocode/workbuddy/zcode/autoclaw/dsh)"))
	cmd.Flags().Bool("unresolved", false, ui.Bi("Show unresolved errors only", "只看未解决的异常"))
	cmd.Flags().String("format", "table", ui.Bi("output format: table or json", "输出格式：table 或 json"))

	return cmd
}

// buildErrorsFilter 构造 errors 命令的 ErrorFilter。
//
// 默认语义（保留）：无日期且无 source 时只看未解决（Unresolved=true）；
// 一旦给出日期（单日、月、年或区间）或 source，则默认看全部状态。
// 显式 --unresolved 始终置 Unresolved=true。
func buildErrorsFilter(dates []string, source string, unresolved bool) db.ErrorFilter {
	return db.ErrorFilter{
		Dates:      dates,
		Source:     source,
		Unresolved: unresolved || (len(dates) == 0 && source == ""),
	}
}

// runErrors 可测试的 errors 命令核心逻辑（table 输出）
func runErrors(usageDB *db.DB, out io.Writer, filter db.ErrorFilter) error {
	return runErrorsContext(context.Background(), usageDB, out, filter, "table")
}

func runErrorsContext(ctx context.Context, usageDB *db.DB, out io.Writer, filter db.ErrorFilter, format string) error {
	errs, err := db.GetErrorsContext(ctx, usageDB, filter)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to query errors", "查询异常失败"), err)
	}

	if format == "json" {
		return renderErrorsJSON(errs, out)
	}

	if len(errs) == 0 {
		fmt.Fprintln(out, ui.Bi("No error records", "暂无异常记录"))
		return nil
	}

	fmt.Fprintf(out, "⚠️  %s：\n\n", ui.Bi(
		fmt.Sprintf("collection errors (%d)", len(errs)),
		fmt.Sprintf("采集异常（%d 条）", len(errs)),
	))

	widths := []int{errColID, errColDate, errColClient, errColType, errColMessage, errColTime, errColRetries, errColStatus}
	header := []string{
		"ID",
		ui.ColDate,
		ui.ColClient,
		ui.Bi("Type", "类型"),
		ui.Bi("Message", "错误信息"),
		ui.Bi("Occurred", "发生时刻"),
		ui.Bi("Retries", "重试次数"),
		ui.Bi("Status", "状态"),
	}

	fmt.Fprintln(out, errorsTableBorder(widths, "┌", "┬", "┐"))
	fmt.Fprintln(out, errorsTableRow(header, widths))
	fmt.Fprintln(out, errorsTableBorder(widths, "├", "┼", "┤"))

	hasMonitor, hasCollectionError := false, false
	for _, e := range errs {
		switch e.ErrorType {
		case db.MonitorErrorType:
			hasMonitor = true
		case "error":
			hasCollectionError = true
		}
		status := ui.Bi("unresolved", "未解决")
		if e.Resolved {
			status = ui.Bi("resolved", "已解决")
		}
		row := []string{
			strconv.Itoa(e.ID),
			e.Date,
			e.Source,
			errorTypeLabel(e.ErrorType),
			e.Message,
			errorOccurredAtLabel(e),
			strconv.Itoa(e.RetryCount),
			status,
		}
		fmt.Fprintln(out, errorsTableRow(row, widths))
	}

	fmt.Fprintln(out, errorsTableBorder(widths, "└", "┴", "┘"))
	fmt.Fprintln(out)
	if hasCollectionError {
		fmt.Fprintln(out, ui.Bi(
			"Run `token-usage collect retry` to retry all, `token-usage collect retry --client <name>` for one client.",
			"运行 `token-usage collect retry` 重试，`token-usage collect retry --client <name>` 重试指定数据源。",
		))
	}
	if hasMonitor {
		// 监控中断是守护进程级事件，重试单条记录无法补齐：引导到 daemon status 与日志。
		fmt.Fprintln(out, ui.Bi(
			"Monitor interruptions are recorded for visibility only; check `token-usage daemon status` and the daemon log instead of retrying them.",
			"监控中断仅作记录展示，请通过 `token-usage daemon status` 与守护进程日志定位，不要重试该记录。",
		))
	}

	return nil
}

// errorTypeLabel 把 error_type 映射为表格可见的类型名（未知类型按采集失败显示）。
// 值与既有状态列一致使用双语短词，宽度按最长值（error / 采集失败 = 16）收敛。
func errorTypeLabel(errorType string) string {
	switch errorType {
	case db.MonitorErrorType:
		return ui.Bi("monitor", "监控")
	case "warning":
		return ui.Bi("warning", "警告")
	default:
		return ui.Bi("error", "采集失败")
	}
}

// errorOccurredAtLabel 返回可读发生时刻：监控中断优先展示实际观测的
// occurred_at，避免把事后入库时间当成故障发生时间；其余用入库时刻（UTC→本地）。
func errorOccurredAtLabel(e model.CollectionError) string {
	if e.ErrorType == db.MonitorErrorType {
		if detail, err := db.ParseMonitorErrorDetail(e.Detail); err == nil && detail.OccurredAt != "" {
			return formatStatusTime(detail.OccurredAt)
		}
	}
	return formatSQLTime(e.CreatedAt)
}

// formatSQLTime 把 SQLite datetime('now')（UTC，无时区标记）转成本地可读时刻。
func formatSQLTime(value string) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return value
	}
	return t.Local().Format("2006-01-02 15:04")
}

// errorsJSONRecord 是 errors --format json 的单条记录投影。既有字段（ID/日期/
// 客户端/错误信息/重试次数/状态）保持兼容，error_type 与 created_at 追加在
// 既有键之后；detail/updated_at 不在导出语义内。
type errorsJSONRecord struct {
	ID         int    `json:"id"`
	Date       string `json:"date"`
	Source     string `json:"source"`
	Message    string `json:"message"`
	RetryCount int    `json:"retry_count"`
	Resolved   bool   `json:"resolved"`
	ErrorType  string `json:"error_type"`
	CreatedAt  string `json:"created_at"`
}

// marshalExportJSON 以两空格缩进序列化并追加尾随换行。map 键由 encoding/json
// 按字母序输出：JSON 对象本无序，键集合与输出列名一致且同一数据输出确定。
func marshalExportJSON(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// renderErrorsJSON 把错误记录渲染为机器可读 JSON：stdout 纯数据，无统计头与
// 重试提示行；空记录初始化为空切片以输出 [] 而非 null；两空格缩进与尾随换行
// 复用 marshalExportJSON 的机器输出约定；记录的过滤与排序和 table 模式同源
// （同一 GetErrorsContext 结果）。
func renderErrorsJSON(errs []model.CollectionError, out io.Writer) error {
	records := make([]errorsJSONRecord, 0, len(errs))
	for _, e := range errs {
		records = append(records, errorsJSONRecord{
			ID:         e.ID,
			Date:       e.Date,
			Source:     e.Source,
			Message:    e.Message,
			RetryCount: e.RetryCount,
			Resolved:   e.Resolved,
			ErrorType:  e.ErrorType,
			CreatedAt:  e.CreatedAt,
		})
	}
	s, err := marshalExportJSON(records)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to encode errors as JSON", "错误记录 JSON 编码失败"), err)
	}
	_, err = io.WriteString(out, s)
	return err
}

// errorsFormatError 非法 --format 取值：在加载配置与开库之前拒绝。
func errorsFormatError(value string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --format %q (allowed: table, json)", value),
		fmt.Sprintf("无效的 --format %q（允许：table、json）", value),
	))
}

// errors 表格列宽（显示宽度，含双语表头与状态值），补齐与截断统一走 runewidth。
// 中文按显示宽度占 2 列，宽度按双语表头/状态值实测重估，非 rune 计数。
// message 保持 50：既有失败原文（如 "claude read / 读取数据源失败: ..."）在
// 50 内完整可见，收窄会截断断言依赖的错误原因。
const (
	errColID      = 3  // ID
	errColDate    = 11 // Date / 日期
	errColClient  = 15 // Client / 客户端
	errColType    = 16 // Type / 类型（error / 采集失败 最长 16）
	errColMessage = 50 // Message / 错误信息
	errColTime    = 19 // Occurred / 发生时刻（2006-01-02 15:04）
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
