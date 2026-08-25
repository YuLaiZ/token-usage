package querier

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

type Querier struct {
	db *db.DB
}

func New(d *db.DB) *Querier {
	return &Querier{db: d}
}

func (q *Querier) readyContext(ctx context.Context) (context.Context, error) {
	if q == nil || q.db == nil {
		return nil, errors.New(ui.Bi("query database must not be empty", "查询数据库不能为空"))
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

// newGroupTable 构造分组聚合表（client/model/project 共用）：key 列左对齐，
// 数字列右对齐；列名统一引用 internal/ui 常量（A4b 术语统一）。分组键
// （模型名/项目名）携带区分信息（如模型日期戳），列宽自适应不截断。
// tableCol 是一列的完整定义：表头（两行形态）、对齐与数据宽度上限。
// 表头、对齐、上限三要素收敛在同一结构，由 buildTable 一次性生成表格
// 三组参数——避免增删列时按位置索引更新上限数组的漂移（历史上两次列
// 删除均发生索引错位）。
type tableCol struct {
	header string
	align  ui.Align
	limit  int
}

// groupTailDefs 是全部 query 表格视图统一的前缀无关尾部列（数值区），
// 保证各视图最后的列集合与顺序一致。Cache Create 不设列（常用客户端恒 0，
// OpenCode 的缓存创建量并入命中率分母，需要绝对量时看 summary）。
var groupTailDefs = []tableCol{
	{ui.HRequests, ui.AlignRight, 0},
	{ui.HInput, ui.AlignRight, 0},
	{ui.HOutput, ui.AlignRight, 0},
	{ui.HCacheRead, ui.AlignRight, 0},
	{ui.HReasoning, ui.AlignRight, 0},
	{ui.HTotal, ui.AlignRight, 0},
	{ui.HCacheHit, ui.AlignRight, 0},
}

func buildTable(defs []tableCol) *ui.Table {
	headers := make([]string, len(defs))
	aligns := make([]ui.Align, len(defs))
	limits := make([]int, len(defs))
	for i, d := range defs {
		headers[i] = d.header
		aligns[i] = d.align
		limits[i] = d.limit
	}
	return ui.NewTable(headers, aligns...).Limits(limits...)
}

func newGroupTable(keyHeader string) *ui.Table {
	defs := append([]tableCol{{keyHeader, ui.AlignLeft, 0}}, groupTailDefs...)
	return buildTable(defs)
}

// formatCacheHit 返回缓存命中率：cache_read / (fresh input + cache read +
// cache create)，两位小数百分比；无任何输入时为 0.00%。
func formatCacheHit(freshInput, cacheRead, cacheCreate int64) string {
	denom := freshInput + cacheRead + cacheCreate
	if denom <= 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", float64(cacheRead)*100/float64(denom))
}

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

func addGroupRow(t *ui.Table, key string, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64) {
	t.Row(key, fmt.Sprintf("%d", requestCount),
		formatTokens(freshInput), formatTokens(outputTokens),
		formatTokens(cacheRead), formatTokens(reasoning), formatTokens(totalTokens),
		formatCacheHit(freshInput, cacheRead, cacheCreate))
}

func (q *Querier) ByClient(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Group by client - no data", "按客户端分组 - 无数据"), nil
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
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(ui.Bi("Group by client", "按客户端分组") + "\n")

	t := newGroupTable(ui.HClient)
	for rows.Next() {
		client, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan client aggregate rows failed", "扫描客户端聚合结果失败"), err)
		}
		addGroupRow(t, client, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate client aggregate rows failed", "遍历客户端聚合结果失败"), err)
	}

	sb.WriteString(t.String())
	return sb.String(), nil
}

func (q *Querier) ByModel(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Group by model - no data", "按模型分组 - 无数据"), nil
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
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(ui.Bi("Group by model", "按模型分组") + "\n")

	t := newGroupTable(ui.HModel)
	for rows.Next() {
		model, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan model aggregate rows failed", "扫描模型聚合结果失败"), err)
		}
		addGroupRow(t, model, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate model aggregate rows failed", "遍历模型聚合结果失败"), err)
	}

	sb.WriteString(t.String())
	return sb.String(), nil
}

// ByProvider 以路由归因优先、采集归因其次的顺序分组。
// 历史空值保持未归因，不根据客户端名称补写或推断供应商。
// aliases 仅用于本次查询的显示和合并，绝不写回 messages。
func (q *Querier) ByProvider(ctx context.Context, dates []string, aliases map[string]string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Group by provider - no data", "按供应商分组 - 无数据"), nil
	}

	placeholders, args := buildPlaceholders(dates)
	const effectiveProvider = `CASE
		WHEN router_provider != '' THEN router_provider
		WHEN provider != '' THEN provider
		ELSE ''
	END`
	query := fmt.Sprintf(`
		SELECT %s, %s
		FROM messages
		WHERE date IN (%s)
		GROUP BY %s
		ORDER BY SUM(total_tokens) DESC
	`, effectiveProvider, groupSelectColumns, placeholders, effectiveProvider)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(ui.Bi("Group by provider", "按供应商分组") + "\n")

	type aggregate struct {
		requests, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64
	}
	aggregates := map[string]aggregate{}
	for rows.Next() {
		provider, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan provider aggregate rows failed", "扫描供应商聚合结果失败"), err)
		}
		if alias := strings.TrimSpace(aliases[provider]); alias != "" {
			provider = alias
		}
		if provider == "" {
			provider = ui.Bi("(unattributed)", "(未归因)")
		}
		a := aggregates[provider]
		a.requests += requestCount
		a.freshInput += freshInput
		a.outputTokens += outputTokens
		a.cacheRead += cacheRead
		a.cacheCreate += cacheCreate
		a.reasoning += reasoning
		a.totalTokens += totalTokens
		aggregates[provider] = a
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate provider aggregate rows failed", "遍历供应商聚合结果失败"), err)
	}

	providers := make([]string, 0, len(aggregates))
	for provider := range aggregates {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		left, right := aggregates[providers[i]], aggregates[providers[j]]
		if left.totalTokens != right.totalTokens {
			return left.totalTokens > right.totalTokens
		}
		return providers[i] < providers[j]
	})
	t := newGroupTable(ui.HProvider)
	for _, provider := range providers {
		a := aggregates[provider]
		addGroupRow(t, provider, a.requests, a.freshInput, a.outputTokens, a.cacheRead, a.cacheCreate, a.reasoning, a.totalTokens)
	}
	sb.WriteString(t.String())
	return sb.String(), nil
}

func (q *Querier) ByProject(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Group by project - no data", "按项目分组 - 无数据"), nil
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
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(ui.Bi("Group by project", "按项目分组") + "\n")

	t := newGroupTable(ui.HProject)
	for rows.Next() {
		project, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens, err := scanGroupRow(rows)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan project aggregate rows failed", "扫描项目聚合结果失败"), err)
		}
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		addGroupRow(t, project, requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate project aggregate rows failed", "遍历项目聚合结果失败"), err)
	}

	sb.WriteString(t.String())
	return sb.String(), nil
}

func (q *Querier) Sessions(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Session details - no data", "会话明细 - 无数据"), nil
	}

	placeholders, args := buildPlaceholders(dates)
	// 主模型子查询与主查询的日期 IN 各需一份参数，按 SQL 出现顺序拼接。
	// 子查询先出现（内层），主查询 JOIN 的 date IN 后出现（外层）。
	query := fmt.Sprintf(`
		SELECT s.client, s.title, s.directory, s.project,
		       COUNT(m.id),
		       COALESCE(SUM(m.fresh_input_tokens),0),
		       COALESCE(SUM(m.output_tokens),0),
		       COALESCE(SUM(m.cache_read_tokens),0),
		       COALESCE(SUM(m.cache_create_tokens),0),
		       COALESCE(SUM(m.reasoning_tokens),0),
		       COALESCE(SUM(m.total_tokens),0)
		FROM sessions s
		JOIN messages m ON m.session_id=s.id AND m.client=s.client
		                 AND m.date IN (%s)
		GROUP BY s.id, s.client, s.title, s.directory, s.project
		ORDER BY MIN(m.date), s.client, SUM(m.total_tokens) DESC
	`, placeholders)

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(ui.Bi("Session details", "会话明细") + "\n")

	// Date（会话首条消息日期）与 ID 不设列：单日下日期恒等于查询日无信息、
	// 跨日需要日期归属时按日分别查询更直接；会话 ID 是内部标识，标题已足够
	// 区分常用场景。首条消息日期仍用于 SQL 排序。尾部数值列与分组表统一。
	// Title 是长自由文本（无版本区分价值），上限 30 截断保住表格总宽；
	// 其余列（项目/客户端等分组键）自适应不截断。
	defs := append([]tableCol{
		{ui.HClient, ui.AlignLeft, 0},
		{ui.HProject, ui.AlignLeft, 0},
		{ui.HTitle, ui.AlignLeft, 30},
	}, groupTailDefs...)
	t := buildTable(defs)

	for rows.Next() {
		var client, title, directory, project string
		var requestCount, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64
		if err := rows.Scan(
			&client, &title, &directory, &project,
			&requestCount, &freshInput, &outputTokens, &cacheRead, &cacheCreate, &reasoning, &totalTokens,
		); err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan session detail rows failed", "扫描会话明细结果失败"), err)
		}
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		t.Row(client, project, title, fmt.Sprintf("%d", requestCount),
			formatTokens(freshInput), formatTokens(outputTokens), formatTokens(cacheRead),
			formatTokens(reasoning), formatTokens(totalTokens),
			formatCacheHit(freshInput, cacheRead, cacheCreate))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate session detail rows failed", "遍历会话明细结果失败"), err)
	}

	sb.WriteString(t.String())
	return sb.String(), nil
}

func (q *Querier) Summary(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Summary - no data", "总览摘要 - 无数据"), nil
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
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	var sb strings.Builder
	sb.WriteString(ui.Bi("Summary", "总览摘要") + "\n\n")
	firstDate, lastDate := dateBounds(dates)
	fmt.Fprintf(&sb, "%s: %s ~ %s\n", ui.Bi("Date range", "日期范围"), firstDate, lastDate)
	fmt.Fprintf(&sb, "%s: %d\n", ui.Bi("Clients", "客户端数"), clientCount)
	fmt.Fprintf(&sb, "%s: %d\n", ui.Bi("Total requests", "请求总数"), requestCount)
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColInput, formatTokens(freshInput))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColOutput, formatTokens(outputTokens))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColCacheRead, formatTokens(cacheRead))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColCacheCreate, formatTokens(cacheCreate))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColReasoning, formatTokens(reasoning))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColTotal, formatTokens(totalTokens))

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

func formatTokens(tokens int64) string {
	if tokens >= 1000000 {
		return fmt.Sprintf("%.2f M", float64(tokens)/1000000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.2f K", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}
