package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/db"
)

func newErrorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "errors [YYYYMMDD]",
		Short: "查看采集异常",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 参数解析先于 DB 打开，非法日期在打开库之前即报错。
			date, err := parseErrorDateArg(args)
			if err != nil {
				return err
			}

			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}

			dbPath := filepath.Join(cfg.DataDir, "usage.db")
			usageDB, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("打开数据库失败: %w", err)
			}
			defer usageDB.Close()

			sourceFlag, _ := cmd.Flags().GetString("source")
			unresolvedFlag, _ := cmd.Flags().GetBool("unresolved")

			filter := buildErrorsFilter(date, sourceFlag, unresolvedFlag)

			return runErrorsContext(cmdContext(cmd), usageDB, cmd.OutOrStdout(), filter)
		},
	}

	cmd.Flags().String("source", "", "指定数据源 (claude/opencode/codex/workbuddy/zcode/autoclaw)")
	cmd.Flags().Bool("unresolved", false, "只看未解决的异常")

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
		return fmt.Errorf("查询异常失败: %w", err)
	}

	if len(errs) == 0 {
		fmt.Fprintln(out, "暂无异常记录")
		return nil
	}

	fmt.Fprintf(out, "⚠️  采集异常（%d 条）：\n\n", len(errs))
	fmt.Fprintln(out, "┌─────┬────────────┬──────────┬──────────────────────────────────────┬──────────┬──────────┐")
	fmt.Fprintln(out, "│ ID  │ 日期       │ 数据源   │ 错误信息                             │ 重试次数 │ 状态     │")
	fmt.Fprintln(out, "├─────┼────────────┼──────────┼──────────────────────────────────────┼──────────┼──────────┤")

	for _, e := range errs {
		msg := truncateRunes(e.Message, 36)
		status := "未解决"
		if e.Resolved {
			status = "已解决"
		}
		fmt.Fprintf(out, "│ %-3d │ %-10s │ %-8s │ %-36s │ %-8d │ %-8s │\n",
			e.ID, e.Date, e.Source, msg, e.RetryCount, status)
	}

	fmt.Fprintln(out, "└─────┴────────────┴──────────┴──────────────────────────────────────┴──────────┴──────────┘")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "运行 `token-usage collect retry` 重试，`token-usage collect retry --client <name>` 重试指定数据源。")

	return nil
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}
