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
	// outputColumns 是已校验的输出指标 ID 序列(不可变:构造/设置时拷贝入参,
	// 渲染期间不被修改)。nil 时按默认七列渲染。
	outputColumns []string
}

func New(d *db.DB) *Querier {
	return &Querier{db: d, outputColumns: ui.DefaultOutputColumns()}
}

// SetOutputColumns 设置输出指标列布局并返回防御性校验错误:
// 序列必须非空、每个 ID 已知且不重复(大小写敏感)。入参被独立拷贝,
// 调用方后续修改切片不影响本 Querier。
func (q *Querier) SetOutputColumns(columns []string) error {
	if len(columns) == 0 {
		return errors.New(ui.Bi(
			"output column layout requires at least one metric ID",
			"输出列布局至少需要一个指标 ID",
		))
	}
	seen := make(map[string]bool, len(columns))
	for _, id := range columns {
		if _, known := ui.OutputMetricHeader(id); !known {
			return fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("unknown output metric %q (allowed: %s)", id, ui.OutputColumnIDList()),
				fmt.Sprintf("未知输出指标 %q(允许: %s)", id, ui.OutputColumnIDList()),
			))
		}
		if seen[id] {
			return fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("duplicate output metric %q in layout", id),
				fmt.Sprintf("输出列布局存在重复指标 %q", id),
			))
		}
		seen[id] = true
	}
	layout := make([]string, len(columns))
	copy(layout, columns)
	q.outputColumns = layout
	return nil
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

// 全部 query 表格视图统一的前缀无关尾部指标区:布局驱动后按 Querier 当前
// 指标序列生成,表头来自 ui 单一来源,单元格由描述符从完整聚合值计算
// (cache_hit 始终读取含 cache_create 的完整聚合,展示开关不改统计值)。
// metricColumn 是一个输出指标列的渲染描述符:双语表头 + 从聚合值生成单元格。
type metricColumn struct {
	header string
	cell   func(agg GroupAggregate) string
}

// metricColumnByID 是指标 ID → 描述符的唯一来源,与 ui 层 ID 集合同步。
var metricColumnByID = map[string]metricColumn{
	ui.MetricRequests: {ui.HRequests, func(a GroupAggregate) string { return fmt.Sprintf("%d", a.Requests) }},
	ui.MetricInput:    {ui.HInput, func(a GroupAggregate) string { return formatTokens(a.FreshInput) }},
	ui.MetricOutput:   {ui.HOutput, func(a GroupAggregate) string { return formatTokens(a.OutputTokens) }},
	ui.MetricCacheRead: {ui.HCacheRead, func(a GroupAggregate) string {
		return formatTokens(a.CacheRead)
	}},
	ui.MetricCacheCreate: {ui.HCacheCreate, func(a GroupAggregate) string {
		return formatTokens(a.CacheCreate)
	}},
	ui.MetricReasoning: {ui.HReasoning, func(a GroupAggregate) string { return formatTokens(a.Reasoning) }},
	ui.MetricTotal:     {ui.HTotal, func(a GroupAggregate) string { return formatTokens(a.TotalTokens) }},
	ui.MetricCacheHit: {ui.HCacheHit, func(a GroupAggregate) string {
		return formatCacheHit(a.FreshInput, a.CacheRead, a.CacheCreate)
	}},
}

// metricColumns 返回当前布局的描述符序列。布局仅经 New/SetOutputColumns
// 写入且已校验,此处直接查找;未知 ID 属内部不变式破坏,panic 早暴露。
func (q *Querier) metricColumns() []metricColumn {
	columns := q.outputColumns
	if len(columns) == 0 {
		columns = ui.DefaultOutputColumns()
	}
	metrics := make([]metricColumn, len(columns))
	for i, id := range columns {
		m, ok := metricColumnByID[id]
		if !ok {
			panic(fmt.Sprintf("querier: unvalidated output metric %q", id))
		}
		metrics[i] = m
	}
	return metrics
}

// metricTailCols 把描述符序列转为表格列定义(数值列右对齐、不限宽)。
func metricTailCols(metrics []metricColumn) []tableCol {
	cols := make([]tableCol, len(metrics))
	for i, m := range metrics {
		cols[i] = tableCol{header: m.header, align: ui.AlignRight, limit: 0}
	}
	return cols
}

// appendMetricCells 按描述符顺序把聚合值的单元格追加到 cells。
func appendMetricCells(cells []string, metrics []metricColumn, agg GroupAggregate) []string {
	for _, m := range metrics {
		cells = append(cells, m.cell(agg))
	}
	return cells
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

// dimensionOrder 是内置聚合维度的有序单一来源：声明顺序即错误文案中的
// 允许集合展示顺序。day/month 时间维度取 date 列（YYYY-MM-DD / substr 前缀
// YYYY-MM），该列恒非空，均无空值处理。
var dimensionOrder = []dimension{
	{name: "client", selectExpr: "client", header: ui.HClient},
	{name: "model", selectExpr: "model", header: ui.HModel},
	{
		name:       "provider",
		selectExpr: effectiveProviderExpr,
		header:     ui.HProvider,
		empty:      func() string { return ui.Bi("(unattributed)", "(未归因)") },
	},
	{
		name:       "project",
		selectExpr: "project",
		header:     ui.HProject,
		empty:      func() string { return ui.Bi("(uncategorized)", "(未分类)") },
	},
	{name: "day", selectExpr: "date", header: ui.HDate},
	// month 取 date 列前 7 位:messages.date 恒为 YYYY-MM-DD,substr 1..7
	// 即 YYYY-MM;ASCII 定长截取,无 UTF-8 多字节截断问题。
	{name: "month", selectExpr: "substr(date, 1, 7)", header: ui.HMonth},
	// hour 取消息时间戳按本机时区折算的小时:ts 为毫秒 Unix 时间,除以 1000
	// 得秒供 unixepoch 解释,'localtime' 与 date 列(采集时按本机时区归日)
	// 同一时区语义;strftime '%H' 恒为两位 ASCII 数字,显示键补 ":00" 后缀
	// 表示该小时起点,字典序仍即时间序。
	{
		name:       "hour",
		selectExpr: `strftime('%H', ts/1000, 'unixepoch', 'localtime')`,
		header:     ui.HHour,
	},
}

// dimensionWhitelist 由 dimensionOrder 派生的按名查找表：有序切片是唯一
// 名单来源，map 仅提供 O(1) 查找语义。
var dimensionWhitelist = func() map[string]dimension {
	m := make(map[string]dimension, len(dimensionOrder))
	for _, d := range dimensionOrder {
		m[d.name] = d
	}
	return m
}()

// dimensionNameList 返回逗号分隔的内置维度名单（错误信息中的允许集合），
// 顺序与 dimensionOrder 一致。
func dimensionNameList() string {
	names := make([]string, len(dimensionOrder))
	for i, d := range dimensionOrder {
		names[i] = d.name
	}
	return strings.Join(names, ", ")
}

// isTemporalDimension 报告维度名是否为时间维度:day/month/hour 的显示值均为
// 字典序即时间序(YYYY-MM-DD / YYYY-MM / "00:00".."23:00"),共用「时间升序优先
// 排序」与「纯单维视图缺口填充」两条时间轴语义;hour 的刻度集固定为每日 24
// 小时,与请求日期范围无关,缺口填充走固定刻度分支。
func isTemporalDimension(name string) bool {
	return name == "day" || name == "month" || name == "hour"
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

// GroupAggregate 是一个复合分组键下的 token 聚合。字段导出供导出命令等
// 机器消费方按原始整数读取;渲染侧仅经描述符消费,不直接拼数字文本。
type GroupAggregate struct {
	Requests, FreshInput, OutputTokens, CacheRead, CacheCreate, Reasoning, TotalTokens int64
}

func (a *GroupAggregate) add(o GroupAggregate) {
	a.Requests += o.Requests
	a.FreshInput += o.FreshInput
	a.OutputTokens += o.OutputTokens
	a.CacheRead += o.CacheRead
	a.CacheCreate += o.CacheCreate
	a.Reasoning += o.Reasoning
	a.TotalTokens += o.TotalTokens
}

// displayKey 把 SQL 返回的原始键值映射为显示键:provider 应用 alias 与未归因,
// project 应用未分类,hour 补 ":00" 后缀表示小时起点,client/model 保持源字段
// 空值。
func (d dimension) displayKey(raw string, aliases map[string]string) string {
	if d.name == "hour" {
		// strftime '%H' 恒为两位数字,后缀仅作显示;防御非两位形态原样返回。
		if len(raw) == 2 {
			return raw + ":00"
		}
		return raw
	}
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

// trendBarWidth 是趋势条的最大块数:含 day 维度视图中趋势列的长度上限。
const trendBarWidth = 20

// hourTicks 是 hour 维度纯单维视图缺口填充的固定刻度(显示形态,即每日 24 个
// 小时起点):hour 的轴刻度与请求日期范围无关,任一请求区间的时间轴都覆盖
// 整日 24 小时,缺数据的小时补零值行。
var hourTicks = func() []string {
	ticks := make([]string, 24)
	for i := range ticks {
		ticks[i] = fmt.Sprintf("%02d:00", i)
	}
	return ticks
}()

// trendBar 按行 totalTokens 相对结果集最大行 totalTokens 的比例生成趋势条:
// 长度 = 比例 × trendBarWidth(整数除法向下取整),totalTokens>0 但算出 0 块时
// 取 1 块;maxTotal<=0 或该行 totalTokens==0 时为空串。
// 量级前提:total ≤ maxTotal 且 maxTotal ≤ MaxInt64/20 时,total×20 的整数乘法不溢出。
func trendBar(total, maxTotal int64) string {
	if maxTotal <= 0 || total <= 0 {
		return ""
	}
	n := int(total * int64(trendBarWidth) / maxTotal)
	if n == 0 {
		n = 1
	}
	return strings.Repeat("█", n)
}

// DimensionRow 是一张维度视图聚合结果中的一行:显示键序列与该键下的聚合值。
type DimensionRow struct {
	Keys []string
	Agg  GroupAggregate
}

// AggregateDimensionView 执行维度视图的数据聚合与排序(不含渲染):
// 维度白名单校验 → raw 聚合 → alias 后复合键聚合 → 缺口填充(纯单维时间视图) →
// 稳定排序(含时间维度时时间升序优先,再 total 降序、显示键元组升序)。
// 返回数据行(不含总计)与同一日期范围的总计聚合;dates 为空时返回空行与零值总计。
// RunDimensionView 与 export 命令共用本方法,保证两边行集合与排序一致。
func (q *Querier) AggregateDimensionView(ctx context.Context, dates []string, view DimensionView) ([]DimensionRow, GroupAggregate, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return nil, GroupAggregate{}, err
	}
	if len(view.Dimensions) == 0 {
		return nil, GroupAggregate{}, errors.New(ui.Bi("dimension view requires at least one dimension", "维度视图至少需要一个维度"))
	}
	dims := make([]dimension, 0, len(view.Dimensions))
	seen := map[string]bool{}
	for _, name := range view.Dimensions {
		d, ok := dimensionWhitelist[name]
		if !ok {
			return nil, GroupAggregate{}, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("unknown query dimension %q (allowed: %s)", name, dimensionNameList()),
				fmt.Sprintf("未知查询维度 %q(允许: %s)", name, dimensionNameList()),
			))
		}
		if seen[name] {
			return nil, GroupAggregate{}, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("duplicate query dimension %q", name),
				fmt.Sprintf("重复查询维度 %q", name),
			))
		}
		seen[name] = true
		dims = append(dims, d)
	}
	if len(dates) == 0 {
		return nil, GroupAggregate{}, nil
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
		return nil, GroupAggregate{}, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	rowOrder := make([]DimensionRow, 0, 8)
	rowIndex := map[string]int{}
	for rows.Next() {
		rawKeys := make([]string, len(dims))
		scanArgs := make([]any, len(dims))
		for i := range rawKeys {
			scanArgs[i] = &rawKeys[i]
		}
		var row DimensionRow
		scanArgs = append(scanArgs, &row.Agg.Requests, &row.Agg.FreshInput, &row.Agg.OutputTokens,
			&row.Agg.CacheRead, &row.Agg.CacheCreate, &row.Agg.Reasoning, &row.Agg.TotalTokens)
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, GroupAggregate{}, fmt.Errorf("%s: %w", ui.Bi("scan aggregate rows failed", "扫描聚合结果失败"), err)
		}
		row.Keys = make([]string, len(dims))
		for i, d := range dims {
			row.Keys[i] = d.displayKey(rawKeys[i], view.Aliases)
		}
		key := strings.Join(row.Keys, "\x00")
		if idx, ok := rowIndex[key]; ok {
			rowOrder[idx].Agg.add(row.Agg)
			continue
		}
		rowIndex[key] = len(rowOrder)
		rowOrder = append(rowOrder, row)
	}
	if err := rows.Err(); err != nil {
		return nil, GroupAggregate{}, fmt.Errorf("%s: %w", ui.Bi("iterate aggregate rows failed", "遍历聚合结果失败"), err)
	}

	// 时间维度(day/month)在本次维度列表中的下标(-1 表示不含时间维度);
	// 重复维度已在参数校验拒绝,同一维度至多出现一次,取首个命中下标。
	temporalIdx := -1
	for i, d := range dims {
		if isTemporalDimension(d.name) {
			temporalIdx = i
			break
		}
	}

	// 纯单维时间视图做缺口填充:请求时间轴上没有数据行的位置插入零值行,
	// 保证时间轴连续;多维时间视图(如 day,model / month,model)不做。
	// day 视图按日期补零(dates 为连续逐日列表);month 视图按月前缀补零:
	// 从请求 dates 推导去重的有序 YYYY-MM 前缀序列(dates 连续逐日,按遍历
	// 顺序去重即保序),对没有数据行的月份插入零值行(Keys=[月前缀]);
	// hour 视图按固定 24 小时刻度补零,与请求日期范围无关。
	if len(dims) == 1 && temporalIdx == 0 {
		if dims[0].name == "hour" {
			seenHours := make(map[string]bool, len(rowOrder))
			for _, r := range rowOrder {
				seenHours[r.Keys[0]] = true
			}
			for _, tick := range hourTicks {
				if seenHours[tick] {
					continue
				}
				seenHours[tick] = true
				rowOrder = append(rowOrder, DimensionRow{Keys: []string{tick}})
			}
		} else {
			// period 把请求日期归一为当前时间维度的轴刻度:day 即日期本身,
			// month 取 YYYY-MM 前缀(数据行键已是该形态,截取为幂等)。
			period := func(key string) string { return key }
			if dims[0].name == "month" {
				period = func(key string) string { return key[:7] }
			}
			seenPeriods := make(map[string]bool, len(rowOrder))
			for _, r := range rowOrder {
				seenPeriods[period(r.Keys[0])] = true
			}
			for _, date := range dates {
				p := period(date)
				if seenPeriods[p] {
					continue
				}
				seenPeriods[p] = true
				rowOrder = append(rowOrder, DimensionRow{Keys: []string{p}})
			}
		}
	}

	// 稳定排序:含时间维度时该维度显示值升序优先(YYYY-MM-DD / YYYY-MM
	// 字典序即时间序),再按 total 降序、完整显示键元组升序(同一有效配置与
	// 语言下确定)。
	sort.SliceStable(rowOrder, func(i, j int) bool {
		if temporalIdx >= 0 && rowOrder[i].Keys[temporalIdx] != rowOrder[j].Keys[temporalIdx] {
			return rowOrder[i].Keys[temporalIdx] < rowOrder[j].Keys[temporalIdx]
		}
		if rowOrder[i].Agg.TotalTokens != rowOrder[j].Agg.TotalTokens {
			return rowOrder[i].Agg.TotalTokens > rowOrder[j].Agg.TotalTokens
		}
		for k := range dims {
			if rowOrder[i].Keys[k] != rowOrder[j].Keys[k] {
				return rowOrder[i].Keys[k] < rowOrder[j].Keys[k]
			}
		}
		return false
	})

	// 总计:同一日期范围的独立全量聚合。
	totals, err := q.rangeTotals(ctx, dates)
	if err != nil {
		return nil, GroupAggregate{}, err
	}
	return rowOrder, totals, nil
}

// RunDimensionView 按维度列表输出一张分组聚合表:聚合与排序委托给
// AggregateDimensionView(维度校验、raw 聚合、alias 合并、缺口填充与稳定排序
// 均在其中,空 dates 于维度校验之后短路),本方法只负责 readiness 检查与渲染:
// 标题 → 表头 → 趋势列(含时间维度时) → 数据行 → 总计行。
// 「标题 - 无数据」早退在委托调用之后判定:「空 dates + 非法维度」报维度校验
// 错误(与旧实现一致),「空 dates + 合法维度」渲染无数据文本。
// 总计来自同一日期范围的独立全量聚合,不由渲染后的行文本反推;无数据日期渲染表头 + 零值总计
// (纯单维时间视图改为对缺口日期/月份插入零值行,保证时间轴连续)。
func (q *Querier) RunDimensionView(ctx context.Context, dates []string, view DimensionView) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	title := ui.Bi(view.TitleEn, view.TitleZh)

	rows, totals, err := q.AggregateDimensionView(ctx, dates, view)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi(view.TitleEn+" - no data", view.TitleZh+" - 无数据"), nil
	}

	// 维度已在聚合核内完成白名单校验,此处按声明顺序取渲染表头与时间维度下标。
	// temporalIdx 取首个命中下标即 break,与聚合核 AggregateDimensionView 的
	// 取向一致(渲染侧仅作布尔消费,但两侧取向不一致是潜伏陷阱)。
	dimHeaders := make([]string, len(view.Dimensions))
	temporalIdx := -1
	for i, name := range view.Dimensions {
		dimHeaders[i] = dimensionWhitelist[name].header
		if isTemporalDimension(name) {
			temporalIdx = i
			break
		}
	}

	metrics := q.metricColumns()
	var sb strings.Builder
	sb.WriteString(title + "\n")
	defs := make([]tableCol, 0, len(dimHeaders)+len(metrics)+1)
	for _, h := range dimHeaders {
		defs = append(defs, tableCol{header: h, align: ui.AlignLeft, limit: 0})
	}
	// 含时间维度时在全部维度键列之后、指标列之前插入趋势条形列。
	if temporalIdx >= 0 {
		defs = append(defs, tableCol{header: ui.HTrend, align: ui.AlignLeft, limit: 0})
	}
	defs = append(defs, metricTailCols(metrics)...)
	t := buildTable(defs)
	// 趋势条以结果集内最大行 totalTokens 为基准(不含总计行)。
	maxTotal := int64(0)
	for _, row := range rows {
		if row.Agg.TotalTokens > maxTotal {
			maxTotal = row.Agg.TotalTokens
		}
	}
	for _, row := range rows {
		cells := make([]string, 0, len(dimHeaders)+len(metrics)+1)
		cells = append(cells, row.Keys...)
		if temporalIdx >= 0 {
			cells = append(cells, trendBar(row.Agg.TotalTokens, maxTotal))
		}
		cells = appendMetricCells(cells, metrics, row.Agg)
		t.Row(cells...)
	}
	// 总计行:第一个维度列写 Total / 总计,其余维度列留空;趋势单元格为空串。
	totalCells := make([]string, 0, len(dimHeaders)+len(metrics)+1)
	totalCells = append(totalCells, ui.Bi("Total", "总计"))
	for i := 1; i < len(dimHeaders); i++ {
		totalCells = append(totalCells, "")
	}
	if temporalIdx >= 0 {
		totalCells = append(totalCells, "")
	}
	totalCells = appendMetricCells(totalCells, metrics, totals)
	t.Row(totalCells...)
	sb.WriteString(t.String())
	return sb.String(), nil
}

// rangeTotals 返回日期范围的全量聚合(总计行数据源,独立于分组结果)。
func (q *Querier) rangeTotals(ctx context.Context, dates []string) (GroupAggregate, error) {
	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(
		"SELECT %s FROM messages WHERE date IN (%s)",
		groupSelectColumns, placeholders,
	)
	var totals GroupAggregate
	err := q.db.QueryRowContext(ctx, query, args...).Scan(
		&totals.Requests, &totals.FreshInput, &totals.OutputTokens,
		&totals.CacheRead, &totals.CacheCreate, &totals.Reasoning, &totals.TotalTokens)
	if err != nil {
		return totals, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	return totals, nil
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

func (q *Querier) ByDay(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"day"},
		TitleEn:    "Usage by day", TitleZh: "按天用量",
	})
}

func (q *Querier) ByMonth(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"month"},
		TitleEn:    "Usage by month", TitleZh: "按月用量",
	})
}

// ByHour 按本机时区的小时分布聚合:纯单维 hour 视图对整日 24 小时固定刻度
// 补零值行,时间轴与请求日期范围无关(任一区间都呈现完整 24 小时)。
func (q *Querier) ByHour(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"hour"},
		TitleEn:    "Usage by hour", TitleZh: "按小时用量",
	})
}

// SessionRow 是一条会话聚合结果:会话标识字段与该会话在日期范围内的聚合值。
type SessionRow struct {
	Client  string
	Project string
	Title   string
	Agg     GroupAggregate
}

// SessionRows 返回会话明细的结构化行:SQL 与排序与 Sessions 完全一致
// (首条消息日期、client、total 降序)。Project 返回源字段原值(空串不映射
// 为「未分类」,显示形态由渲染方决定),供导出等机器消费方使用。
func (q *Querier) SessionRows(ctx context.Context, dates []string) ([]SessionRow, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return nil, err
	}
	if len(dates) == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer rows.Close()

	var result []SessionRow
	for rows.Next() {
		var row SessionRow
		// directory 列照旧 SELECT 维持 SQL 形态,机器行不消费,扫描进废弃变量。
		var directory string
		if err := rows.Scan(
			&row.Client, &row.Title, &directory, &row.Project,
			&row.Agg.Requests, &row.Agg.FreshInput, &row.Agg.OutputTokens,
			&row.Agg.CacheRead, &row.Agg.CacheCreate, &row.Agg.Reasoning, &row.Agg.TotalTokens,
		); err != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("scan session detail rows failed", "扫描会话明细结果失败"), err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("iterate session detail rows failed", "遍历会话明细结果失败"), err)
	}
	return result, nil
}

func (q *Querier) Sessions(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Session details - no data", "会话明细 - 无数据"), nil
	}

	rows, err := q.SessionRows(ctx, dates)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(ui.Bi("Session details", "会话明细") + "\n")

	// Date（会话首条消息日期）与 ID 不设列：单日下日期恒等于查询日无信息、
	// 跨日需要日期归属时按日分别查询更直接；会话 ID 是内部标识，标题已足够
	// 区分常用场景。首条消息日期仍用于 SQL 排序。尾部数值列由全局布局驱动。
	// Title 是长自由文本（无版本区分价值），上限 30 截断保住表格总宽；
	// 其余列（项目/客户端等分组键）自适应不截断。
	metrics := q.metricColumns()
	defs := append([]tableCol{
		{ui.HClient, ui.AlignLeft, 0},
		{ui.HProject, ui.AlignLeft, 0},
		{ui.HTitle, ui.AlignLeft, 30},
	}, metricTailCols(metrics)...)
	t := buildTable(defs)

	for _, row := range rows {
		project := row.Project
		// 「(未分类)」空值映射留在渲染侧,SessionRows 保留源字段原值供机器消费。
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		cells := appendMetricCells([]string{row.Client, project, row.Title}, metrics, row.Agg)
		t.Row(cells...)
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
