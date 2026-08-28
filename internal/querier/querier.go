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

// dimension 是一个受控聚合维度:SQL 选择表达式、显示表头与空值显示。
// dims 只能来自下方白名单常量,不拼接任何用户输入。
type dimension struct {
	name       string
	selectExpr string
	header     string
	// empty 返回空值显示(provider 未归因 / project 未分类);nil 表示保持源字段空值。
	empty func() string
}

// effectiveProviderExpr 供应商有效值:router_provider 非空优先,其次 provider,空为未归因。
const effectiveProviderExpr = `CASE
	WHEN router_provider != '' THEN router_provider
	WHEN provider != '' THEN provider
	ELSE ''
END`

var dimensionWhitelist = map[string]dimension{
	"client": {name: "client", selectExpr: "client", header: ui.HClient},
	"model":  {name: "model", selectExpr: "model", header: ui.HModel},
	"provider": {
		name:       "provider",
		selectExpr: effectiveProviderExpr,
		header:     ui.HProvider,
		empty:      func() string { return ui.Bi("(unattributed)", "(未归因)") },
	},
	"project": {
		name:       "project",
		selectExpr: "project",
		header:     ui.HProject,
		empty:      func() string { return ui.Bi("(uncategorized)", "(未分类)") },
	},
}

// DimensionView 描述一张分组聚合表的渲染输入(内置单维与自定义多维共用)。
type DimensionView struct {
	// Dimensions 是白名单维度名,声明顺序即维度列顺序;不允许重复。
	Dimensions []string
	// Aliases 是 provider 显示别名,仅在查询期合并展示,不写回 messages。
	Aliases map[string]string
	TitleEn string
	TitleZh string
}

// groupAggregate 是一个复合分组键下的 token 聚合。
type groupAggregate struct {
	requests, freshInput, outputTokens, cacheRead, cacheCreate, reasoning, totalTokens int64
}

func (a *groupAggregate) add(o groupAggregate) {
	a.requests += o.requests
	a.freshInput += o.freshInput
	a.outputTokens += o.outputTokens
	a.cacheRead += o.cacheRead
	a.cacheCreate += o.cacheCreate
	a.reasoning += o.reasoning
	a.totalTokens += o.totalTokens
}

// displayKey 把 SQL 返回的原始键值映射为显示键:provider 应用 alias 与未归因,
// project 应用未分类,client/model 保持源字段空值。
func (d dimension) displayKey(raw string, aliases map[string]string) string {
	if d.name == "provider" {
		if alias := strings.TrimSpace(aliases[raw]); alias != "" {
			return alias
		}
		if raw == "" && d.empty != nil {
			return d.empty()
		}
		return raw
	}
	if raw == "" && d.empty != nil {
		return d.empty()
	}
	return raw
}

// RunDimensionView 按维度列表输出一张分组聚合表:
// raw 聚合 → alias 后复合键聚合 → 稳定排序(total 降序、完整显示键元组升序)→ 表格 + 总计行。
// 总计来自同一日期范围的独立全量聚合,不由渲染后的行文本反推;无数据日期渲染表头 + 零值总计。
func (q *Querier) RunDimensionView(ctx context.Context, dates []string, view DimensionView) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(view.Dimensions) == 0 {
		return "", errors.New(ui.Bi("dimension view requires at least one dimension", "维度视图至少需要一个维度"))
	}
	dims := make([]dimension, 0, len(view.Dimensions))
	seen := map[string]bool{}
	for _, name := range view.Dimensions {
		d, ok := dimensionWhitelist[name]
		if !ok {
			return "", fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("unknown query dimension %q (allowed: client, model, provider, project)", name),
				fmt.Sprintf("未知查询维度 %q(允许: client, model, provider, project)", name),
			))
		}
		if seen[name] {
			return "", fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("duplicate query dimension %q", name),
				fmt.Sprintf("重复查询维度 %q", name),
			))
		}
		seen[name] = true
		dims = append(dims, d)
	}
	title := ui.Bi(view.TitleEn, view.TitleZh)
	if len(dates) == 0 {
		return ui.Bi(view.TitleEn+" - no data", view.TitleZh+" - 无数据"), nil
	}

	// raw 聚合:GROUP BY 各维度原始表达式(SQL 无序,排序统一在 Go 侧保证稳定)。
	selectExprs := make([]string, len(dims))
	groupExprs := make([]string, len(dims))
	for i, d := range dims {
		selectExprs[i] = d.selectExpr
		groupExprs[i] = d.selectExpr
	}
	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(
		"SELECT %s, %s FROM messages WHERE date IN (%s) GROUP BY %s",
		strings.Join(selectExprs, ", "), groupSelectColumns, placeholders, strings.Join(groupExprs, ", "),
	)
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	type compositeRow struct {
		parts [4]string
		agg   groupAggregate
	}
	rowOrder := make([]compositeRow, 0, 8)
	rowIndex := map[string]int{}
	for rows.Next() {
		rawKeys := make([]string, len(dims))
		scanArgs := make([]any, len(dims))
		for i := range rawKeys {
			scanArgs[i] = &rawKeys[i]
		}
		var agg groupAggregate
		scanArgs = append(scanArgs, &agg.requests, &agg.freshInput, &agg.outputTokens,
			&agg.cacheRead, &agg.cacheCreate, &agg.reasoning, &agg.totalTokens)
		if err := rows.Scan(scanArgs...); err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi("scan aggregate rows failed", "扫描聚合结果失败"), err)
		}
		var parts [4]string
		for i, d := range dims {
			parts[i] = d.displayKey(rawKeys[i], view.Aliases)
		}
		key := strings.Join(parts[:len(dims)], "\x00")
		if idx, ok := rowIndex[key]; ok {
			rowOrder[idx].agg.add(agg)
			continue
		}
		rowIndex[key] = len(rowOrder)
		rowOrder = append(rowOrder, compositeRow{parts: parts, agg: agg})
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("iterate aggregate rows failed", "遍历聚合结果失败"), err)
	}

	// 稳定排序:total 降序,再按完整显示键元组升序(同一有效配置与语言下确定)。
	sort.SliceStable(rowOrder, func(i, j int) bool {
		if rowOrder[i].agg.totalTokens != rowOrder[j].agg.totalTokens {
			return rowOrder[i].agg.totalTokens > rowOrder[j].agg.totalTokens
		}
		for k := range dims {
			if rowOrder[i].parts[k] != rowOrder[j].parts[k] {
				return rowOrder[i].parts[k] < rowOrder[j].parts[k]
			}
		}
		return false
	})

	// 总计:同一日期范围的独立全量聚合。
	totals, err := q.rangeTotals(ctx, dates)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(title + "\n")
	defs := make([]tableCol, 0, len(dims)+len(groupTailDefs))
	for _, d := range dims {
		defs = append(defs, tableCol{header: d.header, align: ui.AlignLeft, limit: 0})
	}
	defs = append(defs, groupTailDefs...)
	t := buildTable(defs)
	for _, row := range rowOrder {
		addDimensionRow(t, row.parts[:len(dims)], row.agg)
	}
	addDimensionTotalRow(t, len(dims), totals)
	sb.WriteString(t.String())
	return sb.String(), nil
}

// rangeTotals 返回日期范围的全量聚合(总计行数据源,独立于分组结果)。
func (q *Querier) rangeTotals(ctx context.Context, dates []string) (groupAggregate, error) {
	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(
		"SELECT %s FROM messages WHERE date IN (%s)",
		groupSelectColumns, placeholders,
	)
	var totals groupAggregate
	err := q.db.QueryRowContext(ctx, query, args...).Scan(
		&totals.requests, &totals.freshInput, &totals.outputTokens,
		&totals.cacheRead, &totals.cacheCreate, &totals.reasoning, &totals.totalTokens)
	if err != nil {
		return totals, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	return totals, nil
}

func addDimensionRow(t *ui.Table, keys []string, agg groupAggregate) {
	cells := make([]string, 0, len(keys)+7)
	for _, k := range keys {
		cells = append(cells, k)
	}
	cells = append(cells,
		fmt.Sprintf("%d", agg.requests),
		formatTokens(agg.freshInput), formatTokens(agg.outputTokens),
		formatTokens(agg.cacheRead), formatTokens(agg.reasoning), formatTokens(agg.totalTokens),
		formatCacheHit(agg.freshInput, agg.cacheRead, agg.cacheCreate),
	)
	t.Row(cells...)
}

// addDimensionTotalRow 渲染总计行:第一个维度列写 Total / 总计,其余维度列留空。
func addDimensionTotalRow(t *ui.Table, dimCount int, totals groupAggregate) {
	cells := make([]string, 0, dimCount+7)
	cells = append(cells, ui.Bi("Total", "总计"))
	for i := 1; i < dimCount; i++ {
		cells = append(cells, "")
	}
	cells = append(cells,
		fmt.Sprintf("%d", totals.requests),
		formatTokens(totals.freshInput), formatTokens(totals.outputTokens),
		formatTokens(totals.cacheRead), formatTokens(totals.reasoning), formatTokens(totals.totalTokens),
		formatCacheHit(totals.freshInput, totals.cacheRead, totals.cacheCreate),
	)
	t.Row(cells...)
}

func (q *Querier) ByClient(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"client"},
		TitleEn:    "Group by client", TitleZh: "按客户端分组",
	})
}

func (q *Querier) ByModel(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"model"},
		TitleEn:    "Group by model", TitleZh: "按模型分组",
	})
}

// ByProvider 以路由归因优先、采集归因其次的顺序分组。
// 历史空值保持未归因，不根据客户端名称补写或推断供应商。
// aliases 仅用于本次查询的显示和合并，绝不写回 messages。
func (q *Querier) ByProvider(ctx context.Context, dates []string, aliases map[string]string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"provider"},
		Aliases:    aliases,
		TitleEn:    "Group by provider", TitleZh: "按供应商分组",
	})
}

func (q *Querier) ByProject(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"project"},
		TitleEn:    "Group by project", TitleZh: "按项目分组",
	})
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
	// 统计范围只由 CLI 统一信息区的 Query range 行承载(单日不渲染 a ~ a),这里不再重复。
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

func formatTokens(tokens int64) string {
	if tokens >= 1000000000 {
		return fmt.Sprintf("%.2f B", float64(tokens)/1000000000)
	}
	if tokens >= 1000000 {
		return fmt.Sprintf("%.2f M", float64(tokens)/1000000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.2f K", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}
