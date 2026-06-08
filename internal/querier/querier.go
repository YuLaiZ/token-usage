package querier

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/YuLaiZ/token-usage/internal/db"
)

type Querier struct {
	db *db.DB
}

func New(d *db.DB) *Querier {
	return &Querier{db: d}
}

func (q *Querier) readyContext(ctx context.Context) (context.Context, error) {
	if q == nil || q.db == nil {
		return nil, fmt.Errorf("查询数据库不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ctx, nil
}

// 消息账本聚合列：COUNT(*) 是消息/请求数，所有 token 直接 SUM 源字段。
// fresh_input_tokens 与 total_tokens 取源值，不按 client 推断，不叠加 reasoning。
const groupSelectColumns = `COUNT(*),
       COALESCE(SUM(fresh_input_tokens),0),
       COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_tokens),0),
       COALESCE(SUM(cache_create_tokens),0),
       COALESCE(SUM(reasoning_tokens),0),
       COALESCE(SUM(total_tokens),0)`

const groupHeader = "请求数\t输入Token\t输出Token\t缓存读取\t缓存创建\tReasoning（明细）\t总计Token"
const groupHeaderSep = "------\t------\t----------\t----------\t----------\t----------\t----------"

func buildPlaceholders(dates []string) (string, []interface{}) {
	placeholders := make([]string, len(dates))
	args := make([]interface{}, len(dates))
	for i, d := range dates {
		placeholders[i] = "?"
		args[i] = d
	}
	return strings.Join(placeholders, ","), args
}

// scanGroupRow 扫描 groupBy 查询的一行（key + 7 列聚合）。
func scanGroupRow(rows interface {
	Next() bool
	Scan(...interface{}) error
}) (key string, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64, err error) {
	err = rows.Scan(&key, &requestCount, &freshInput, &outputTokens, &cacheRead, &cacheCreate, &reasoning, &totalTokens)
	return
}

func writeGroupRow(w *tabwriter.Writer, key string, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64) {
	fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
		key, requestCount,
		formatTokens(freshInput), formatTokens(outputTokens),
		formatTokens(cacheRead), formatTokens(cacheCreate),
		formatTokens(reasoning), formatTokens(totalTokens))
}

func (q *Querier) ByClient(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "按客户端分组 - 无数据", nil
	}

	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(`
		SELECT client, %s
		FROM messages
		WHERE date IN (%s)
		GROUP BY client
		ORDER BY SUM(total_tokens) DESC
	`, groupSelectColumns, placeholders)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("按客户端分组\n")

	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "客户端\t"+groupHeader)
	fmt.Fprintln(w, "------\t"+groupHeaderSep)

	for rows.Next() {
		client, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("扫描客户端聚合结果失败: %w", err)
		}
		writeGroupRow(w, client, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历客户端聚合结果失败: %w", err)
	}

	w.Flush()
	return sb.String(), nil
}

func (q *Querier) ByModel(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "按模型分组 - 无数据", nil
	}

	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(`
		SELECT model, %s
		FROM messages
		WHERE date IN (%s)
		GROUP BY model
		ORDER BY SUM(total_tokens) DESC
	`, groupSelectColumns, placeholders)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("按模型分组\n")

	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "模型\t"+groupHeader)
	fmt.Fprintln(w, "------\t"+groupHeaderSep)

	for rows.Next() {
		model, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("扫描模型聚合结果失败: %w", err)
		}
		writeGroupRow(w, model, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历模型聚合结果失败: %w", err)
	}

	w.Flush()
	return sb.String(), nil
}

func (q *Querier) ByProject(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "按项目分组 - 无数据", nil
	}

	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(`
		SELECT project, %s
		FROM messages
		WHERE date IN (%s)
		GROUP BY project
		ORDER BY SUM(total_tokens) DESC
	`, groupSelectColumns, placeholders)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("按项目分组\n")

	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "项目\t"+groupHeader)
	fmt.Fprintln(w, "------\t"+groupHeaderSep)

	for rows.Next() {
		project, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("扫描项目聚合结果失败: %w", err)
		}
		if project == "" {
			project = "(未分类)"
		}
		writeGroupRow(w, project, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历项目聚合结果失败: %w", err)
	}

	w.Flush()
	return sb.String(), nil
}

func (q *Querier) Sessions(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "会话明细 - 无数据", nil
	}

	placeholders, args := buildPlaceholders(dates)
	// 主模型子查询与主查询的日期 IN 各需一份参数，按 SQL 出现顺序拼接。
	// 子查询先出现（内层），主查询 JOIN 的 date IN 后出现（外层）。
	query := fmt.Sprintf(`
		SELECT s.id, s.client, s.title, s.directory, s.project,
		       COUNT(m.id),
		       COALESCE(SUM(m.fresh_input_tokens),0),
		       COALESCE(SUM(m.output_tokens),0),
		       COALESCE(SUM(m.cache_read_tokens),0),
		       COALESCE(SUM(m.cache_create_tokens),0),
		       COALESCE(SUM(m.reasoning_tokens),0),
		       COALESCE(SUM(m.total_tokens),0),
		       (SELECT m2.model FROM messages m2
		        WHERE m2.session_id=s.id AND m2.client=s.client
		          AND m2.date IN (%s)
		        GROUP BY m2.model
		        ORDER BY SUM(m2.total_tokens) DESC, m2.model
		        LIMIT 1),
		       MIN(m.date)
		FROM sessions s
		JOIN messages m ON m.session_id=s.id AND m.client=s.client
		                 AND m.date IN (%s)
		GROUP BY s.id, s.client, s.title, s.directory, s.project
		ORDER BY MIN(m.date), s.client, SUM(m.total_tokens) DESC
	`, placeholders, placeholders)

	// 日期参数按 SQL 出现顺序复制两份
	doubleArgs := append(append([]interface{}{}, args...), args...)
	rows, err := q.db.QueryContext(ctx, query, doubleArgs...)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("会话明细\n")

	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t客户端\t日期\t模型\t项目\t标题\t请求数\t输入Token\t输出Token\tReasoning\t总计Token")
	fmt.Fprintln(w, "------\t------\t------\t------\t------\t------\t------\t----------\t----------\t----------\t----------")

	for rows.Next() {
		var id, client, title, directory, project string
		var model string
		var minDate string
		var requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64
		if err := rows.Scan(
			&id, &client, &title, &directory, &project,
			&requestCount, &freshInput, &outputTokens, &cacheRead, &cacheCreate, &reasoning, &totalTokens,
			&model, &minDate,
		); err != nil {
			return "", fmt.Errorf("扫描会话明细结果失败: %w", err)
		}
		if project == "" {
			project = "(未分类)"
		}
		id = truncateRunes(id, 12)
		title = truncateRunes(title, 20)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			id, client, minDate, model, project, title, requestCount,
			formatTokens(freshInput), formatTokens(outputTokens),
			formatTokens(reasoning), formatTokens(totalTokens))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历会话明细结果失败: %w", err)
	}

	w.Flush()
	return sb.String(), nil
}

func (q *Querier) Summary(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "总览摘要 - 无数据", nil
	}

	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(`
		SELECT COUNT(DISTINCT client), COUNT(*),
		       COALESCE(SUM(fresh_input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_create_tokens),0),
		       COALESCE(SUM(reasoning_tokens),0),
		       COALESCE(SUM(total_tokens),0)
		FROM messages
		WHERE date IN (%s)
	`, placeholders)

	var clientCount, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64
	err = q.db.QueryRowContext(ctx, query, args...).Scan(
		&clientCount, &requestCount, &freshInput, &outputTokens, &cacheRead, &cacheCreate, &reasoning, &totalTokens)
	if err != nil {
		return "", fmt.Errorf("查询失败: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("总览摘要\n\n")
	firstDate, lastDate := dateBounds(dates)
	fmt.Fprintf(&sb, "日期范围: %s ~ %s\n", firstDate, lastDate)
	fmt.Fprintf(&sb, "客户端数: %d\n", clientCount)
	fmt.Fprintf(&sb, "请求总数: %d\n", requestCount)
	fmt.Fprintf(&sb, "输入Token: %s\n", formatTokens(freshInput))
	fmt.Fprintf(&sb, "输出Token: %s\n", formatTokens(outputTokens))
	fmt.Fprintf(&sb, "缓存读取: %s\n", formatTokens(cacheRead))
	fmt.Fprintf(&sb, "缓存创建: %s\n", formatTokens(cacheCreate))
	fmt.Fprintf(&sb, "Reasoning（明细）: %s\n", formatTokens(reasoning))
	fmt.Fprintf(&sb, "总计Token: %s\n", formatTokens(totalTokens))

	return sb.String(), nil
}

func dateBounds(dates []string) (string, string) {
	first, last := dates[0], dates[0]
	for _, date := range dates[1:] {
		if date < first {
			first = date
		}
		if date > last {
			last = date
		}
	}
	return first, last
}

func truncateRunes(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

func formatTokens(tokens int64) string {
	if tokens >= 1000000 {
		return fmt.Sprintf("%.2f M", float64(tokens)/1000000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.2f K", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}
