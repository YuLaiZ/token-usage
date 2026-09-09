package querier

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

type Querier struct {
	db *db.DB
	// tx 非 nil 时全部查询走该事务(ReadTx 构造的事务化副本):同一读事务内
	// 的多条查询共享 WAL 读快照,供报告包等需要跨查询一致数据的调用方
	// 使用;零值 Querier 不受影响。
	tx *sql.Tx
	// outputColumns 是已校验的输出指标 ID 序列(不可变:构造/设置时拷贝入参,
	// 渲染期间不被修改)。nil 时按默认七列渲染。
	outputColumns []string
}

// queryContext/queryRowContext 按是否处于事务分派查询目标。
func (q *Querier) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if q.tx != nil {
		return q.tx.QueryContext(ctx, query, args...)
	}
	return q.db.QueryContext(ctx, query, args...)
}

func (q *Querier) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if q.tx != nil {
		return q.tx.QueryRowContext(ctx, query, args...)
	}
	return q.db.QueryRowContext(ctx, query, args...)
}

// ReadTx 在一个(deferred)读事务内执行 fn:fn 收到的 Querier 副本的全部查询
// 共享同一 WAL 读快照且不阻塞写者,事务由本方法统一提交/回滚。已在事务中
// 时直接复用当前事务(SQLite 不支持嵌套 BEGIN,聚合核等内部自建事务的调用
// 方在外层事务内自动合并为同一快照)。报告包等多查询输出经此获得跨查询
// 一致的数据快照。fn 返回后事务即提交/回滚,其收到的副本随之失效,不得
// 在 fn 外继续持有使用。
func (q *Querier) ReadTx(ctx context.Context, fn func(*Querier) error) error {
	if q.tx != nil {
		return fn(q)
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	defer tx.Rollback()
	if err := fn(&Querier{db: q.db, tx: tx, outputColumns: q.outputColumns}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	return nil
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

// rawValueColumns 是 Go 侧累加路径(含时间戳维度的聚合)拉取的 token 源列,
// 与 groupSelectColumns 的聚合列一一对应(列序即 GroupAggregate 字段序);
// messages 列恒 NOT NULL,逐行累加与 COUNT(*)/COALESCE(SUM()) 严格等价。
const rawValueColumns = `fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, reasoning_tokens, total_tokens`

// absorbMemoKey 是聚合行显示键映射的 memo 键:维度下标 + 该维度的原始键
// (时间戳维度为已折算的时间桶键)。memo 落在低基数层(桶键 hour 24 个/
// weekday 7 个,模型/客户端等文本键为 distinct 值),按行重复拼接显示键是
// 大额分配来源;桶键折算本身走预生成查表,无需 memo。
type absorbMemoKey struct {
	dim int
	raw string
}

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
	// weekday 取消息时间戳按本机时区折算的星期,与 date 列同一时区语义;
	// strftime '%w' 以周日为 0,表达式将其转为 ISO 周序(周一=0..周日=6),
	// 与 GB/T 7408 / ISO 8601 的周起首一致;原始键 "0".."6" 字典序即周序,
	// 显示键为双语星期名。
	{
		name:       "weekday",
		selectExpr: `CAST((strftime('%w', ts/1000, 'unixepoch', 'localtime') + 6) % 7 AS TEXT)`,
		header:     ui.HWeekday,
	},
}

// hourBucketTable/weekdayBucketTable 是时间桶键的预生成表(零分配查表):
// hour "00".."23",weekday ISO 周序 "0".."6"(周一=0)。
var hourBucketTable = [24]string{
	"00", "01", "02", "03", "04", "05", "06", "07", "08", "09",
	"10", "11", "12", "13", "14", "15", "16", "17", "18", "19",
	"20", "21", "22", "23",
}

var weekdayBucketTable = [7]string{"0", "1", "2", "3", "4", "5", "6"}

// bucketKeyOf 把消息时间戳(毫秒)折算为本机时区的时间桶键,与 SQL
// strftime(..., 'unixepoch', 'localtime') 逐时刻等价(含 DST:两者均按
// 各时刻生效偏移折算,时区规则同源于系统时区库/TZ 环境);ms/1000 整数
// 除法与 SQL 侧同为向零截断。
func bucketKeyOf(dimName string, ms int64) string {
	t := time.Unix(ms/1000, 0)
	if dimName == "hour" {
		return hourBucketTable[t.Hour()]
	}
	return weekdayBucketTable[(int(t.Weekday())+6)%7]
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

// isTemporalDimension 报告维度名是否为时间维度:day/month/hour/weekday 的
// 原始键均为「字典序即时间序」的形态(YYYY-MM-DD / YYYY-MM / "00".."23" /
// ISO 周序 "0".."6"),共用「时间升序优先排序」与「纯单维视图缺口填充」两条
// 时间轴语义;day/month 的刻度随请求日期范围生成,hour/weekday 的刻度集固定
// (每日 24 小时 / 每周 7 天),与请求日期范围无关。
func isTemporalDimension(name string) bool {
	return name == "day" || name == "month" || name == "hour" || name == "weekday"
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
// project 应用未分类,hour 补 ":00" 后缀表示小时起点,weekday 映射为双语星期
// 名,client/model 保持源字段空值。
func (d dimension) displayKey(raw string, aliases map[string]string) string {
	if d.name == "hour" {
		// strftime '%H' 恒为两位数字,后缀仅作显示;防御非两位形态原样返回。
		if len(raw) == 2 {
			return raw + ":00"
		}
		return raw
	}
	if d.name == "weekday" {
		return weekdayDisplayKey(raw)
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

// trendBarWidth 是趋势条的最大块数:含时间维度(day/month/hour/weekday)视图中趋势列的长度上限。
const trendBarWidth = 20

// hourTicks 是 hour 维度纯单维视图缺口填充的固定原始键刻度(SQL 形态
// "00".."23",显示形态经 displayKey 补 ":00" 后缀):hour 的轴刻度与请求日期
// 范围无关,任一请求区间的时间轴都覆盖整日 24 小时,缺数据的小时补零值行。
var hourTicks = func() []string {
	ticks := make([]string, 24)
	for i := range ticks {
		ticks[i] = fmt.Sprintf("%02d", i)
	}
	return ticks
}()

// weekdayTicks 是 weekday 维度纯单维视图缺口填充的固定原始键刻度(ISO 周序
// "0".."6"):weekday 的轴刻度与请求日期范围无关,任一请求区间的时间轴都覆盖
// 整周 7 天,缺数据的星期补零值行。
var weekdayTicks = []string{"0", "1", "2", "3", "4", "5", "6"}

// weekdayNames 是 weekday 维度显示名的单一来源,下标即 ISO 周序原始键
// ("0"=周一 .. "6"=周日),中英文名一一对应。
var (
	weekdayNamesEn = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	weekdayNamesZh = []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
)

// weekdayDisplayKey 把 weekday 的 ISO 周序原始键("0".."6")映射为双语星期名;
// 非法形态原样返回(数据异常时保持可观察而非静默吞掉)。
func weekdayDisplayKey(raw string) string {
	i, err := strconv.Atoi(raw)
	if err != nil || i < 0 || i >= len(weekdayNamesEn) {
		return raw
	}
	return ui.Bi(weekdayNamesEn[i], weekdayNamesZh[i])
}

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
// rawKeys 是与 Keys 一一对应的 SQL 原始键(未做显示映射),时间维度的排序轴
// 消费原始键(weekday 的显示名为双语星期名,字典序不是周序,原始键 "0".."6"
// 才是),缺口填充行同样填原始键形态保持排序统一;非导出仅供聚合核内部使用。
type DimensionRow struct {
	Keys    []string
	Agg     GroupAggregate
	rawKeys []string
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
	// 时间戳维度(hour/weekday)的分组键在此细化为裸 ts,桶键改由 Go 侧逐时刻
	// 折算(bucketKeyOf):SQLite 纯 Go 实现对每行求值 'localtime' 无
	// 缓存,是 hour/weekday 聚合数倍耗时与海量分配的来源。其余维度维持
	// SQL 折算不变。
	tsBucketed := false
	for _, d := range dims {
		if d.name == "hour" || d.name == "weekday" {
			tsBucketed = true
		}
	}
	placeholders, args := buildPlaceholders(dates)
	var query string
	if tsBucketed {
		// 裸列形态:非时间戳维度键 + ts + token 源列,无 GROUP BY,聚合在
		// Go 侧逐行累加(与 SQL 聚合列严格等价,见 rawValueColumns)。SQLite
		// 每行求值 'localtime' 无缓存、GROUP BY ts 的临时结构均是数倍开销
		// 来源(7.3 万行对照:GROUP BY 形态 SQL 138ms vs 裸列 48ms),时间
		// 戳→桶键折算移到 Go 侧查表(bucketKeyOf,与 'localtime' 逐时刻
		// 等价含 DST)。
		dimExprs := make([]string, 0, len(dims))
		for _, d := range dims {
			if d.name != "hour" && d.name != "weekday" {
				dimExprs = append(dimExprs, d.selectExpr)
			}
		}
		if len(dimExprs) == 0 {
			query = fmt.Sprintf(
				"SELECT ts, %s FROM messages WHERE date IN (%s)",
				rawValueColumns, placeholders)
		} else {
			query = fmt.Sprintf(
				"SELECT %s, ts, %s FROM messages WHERE date IN (%s)",
				strings.Join(dimExprs, ", "), rawValueColumns, placeholders)
		}
	} else {
		selectExprs := make([]string, len(dims))
		for i, d := range dims {
			selectExprs[i] = d.selectExpr
		}
		query = fmt.Sprintf(
			"SELECT %s, %s FROM messages WHERE date IN (%s) GROUP BY %s",
			strings.Join(selectExprs, ", "), groupSelectColumns, placeholders, strings.Join(selectExprs, ", "),
		)
	}
	// 同一读事务内完成分组聚合与总计聚合:WAL 下事务内的两次读取共享
	// 同一快照,daemon 并发写入不再造成分组合计与总计漂移;deferred
	// BEGIN 的只读事务不阻塞写者。经 ReadTx:外层已建立事务时(如报告包的
	// 整体快照)自动复用同一事务,否则独立开启并在成功后提交。
	var rowOrder []DimensionRow
	var totals GroupAggregate
	if err := q.ReadTx(ctx, func(tq *Querier) error {
		rows, err := tq.queryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
		}
		defer rows.Close()

		rowOrder = make([]DimensionRow, 0, 8)
		rowIndex := map[string]int{}
		if tsBucketed {
			// 裸列行循环:行数=消息数,Scan 参数与行缓冲循环外构造一次复用
			// (指针装箱进 any 不逃逸分配);桶键查表折算零分配,显示键 memo
			// 落在低基数层(桶键 hour 24/weekday 7 个、文本维度 distinct 键),
			// 单维视图免 join 直接以唯一键比较。
			var ts, freshIn, output, cacheRead, cacheCreate, reasoning, total int64
			keyBuf := make([]string, len(dims))
			keyCols := 0
			scanArgs := make([]any, 0, len(dims)+6)
			for _, d := range dims {
				if d.name == "hour" || d.name == "weekday" {
					continue
				}
				scanArgs = append(scanArgs, &keyBuf[keyCols])
				keyCols++
			}
			scanArgs = append(scanArgs, &ts, &freshIn, &output, &cacheRead, &cacheCreate, &reasoning, &total)
			rawKeys := make([]string, len(dims))
			keys := make([]string, 0, len(dims))
			displays := make(map[absorbMemoKey]string)
			for rows.Next() {
				if err := rows.Scan(scanArgs...); err != nil {
					return fmt.Errorf("%s: %w", ui.Bi("scan aggregate rows failed", "扫描聚合结果失败"), err)
				}
				keys = keys[:0]
				ki := 0
				for i, d := range dims {
					var raw string
					if d.name == "hour" || d.name == "weekday" {
						raw = bucketKeyOf(d.name, ts)
					} else {
						raw = keyBuf[ki]
						ki++
					}
					rawKeys[i] = raw
					mk := absorbMemoKey{dim: i, raw: raw}
					disp, ok := displays[mk]
					if !ok {
						disp = d.displayKey(raw, view.Aliases)
						displays[mk] = disp
					}
					keys = append(keys, disp)
				}
				agg := GroupAggregate{
					Requests: 1, FreshInput: freshIn, OutputTokens: output,
					CacheRead: cacheRead, CacheCreate: cacheCreate,
					Reasoning: reasoning, TotalTokens: total,
				}
				key := keys[0]
				if len(keys) > 1 {
					key = strings.Join(keys, "\x00")
				}
				if idx, ok := rowIndex[key]; ok {
					rowOrder[idx].Agg.add(agg)
					continue
				}
				rowIndex[key] = len(rowOrder)
				// rawKeys/keys 为复用缓冲,新建行时拷贝。
				stored := DimensionRow{
					Keys:    append([]string(nil), keys...),
					rawKeys: append([]string(nil), rawKeys...),
				}
				stored.Agg = agg
				rowOrder = append(rowOrder, stored)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("iterate aggregate rows failed", "遍历聚合结果失败"), err)
			}
		} else {
			// Scan 参数与行缓冲在循环外构造一次复用;组数=distinct 键组合数,
			// 行处理逐维显示键映射后按显示键元组累加或新建行。
			var row DimensionRow
			rawKeys := make([]string, len(dims))
			scanArgs := make([]any, 0, len(dims)+7)
			for i := range rawKeys {
				scanArgs = append(scanArgs, &rawKeys[i])
			}
			scanArgs = append(scanArgs, &row.Agg.Requests, &row.Agg.FreshInput, &row.Agg.OutputTokens,
				&row.Agg.CacheRead, &row.Agg.CacheCreate, &row.Agg.Reasoning, &row.Agg.TotalTokens)
			for rows.Next() {
				if err := rows.Scan(scanArgs...); err != nil {
					return fmt.Errorf("%s: %w", ui.Bi("scan aggregate rows failed", "扫描聚合结果失败"), err)
				}
				row.Keys = row.Keys[:0]
				for _, d := range dims {
					row.Keys = append(row.Keys, d.displayKey(rawKeys[len(row.Keys)], view.Aliases))
				}
				key := strings.Join(row.Keys, "\x00")
				if idx, ok := rowIndex[key]; ok {
					rowOrder[idx].Agg.add(row.Agg)
					continue
				}
				rowIndex[key] = len(rowOrder)
				// Keys/rawKeys 必须拷贝:row.Keys 与 rawKeys 是循环复用缓冲,直接引用
				// 会被后续行覆写。
				stored := DimensionRow{
					Keys:    append([]string(nil), row.Keys...),
					rawKeys: append([]string(nil), rawKeys...),
				}
				stored.Agg = row.Agg
				rowOrder = append(rowOrder, stored)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("iterate aggregate rows failed", "遍历聚合结果失败"), err)
			}
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
		// 顺序去重即保序),对没有数据行的月份插入零值行;hour/weekday 的刻度集
		// 固定(整日 24 小时 / 整周 7 天,原始键形态),与请求日期范围无关。
		// 填充行同时携带显示键与原始键(两者一致或经同一映射),排序轴统一。
		if len(dims) == 1 && temporalIdx == 0 {
			fill := func(rawKey, displayKey string) {
				rowOrder = append(rowOrder, DimensionRow{
					Keys:    []string{displayKey},
					rawKeys: []string{rawKey},
				})
			}
			switch dims[0].name {
			case "hour":
				seenHours := make(map[string]bool, len(rowOrder))
				for _, r := range rowOrder {
					seenHours[r.rawKeys[0]] = true
				}
				for _, tick := range hourTicks {
					if seenHours[tick] {
						continue
					}
					seenHours[tick] = true
					fill(tick, tick+":00")
				}
			case "weekday":
				seenWeekdays := make(map[string]bool, len(rowOrder))
				for _, r := range rowOrder {
					seenWeekdays[r.rawKeys[0]] = true
				}
				for _, tick := range weekdayTicks {
					if seenWeekdays[tick] {
						continue
					}
					seenWeekdays[tick] = true
					fill(tick, weekdayDisplayKey(tick))
				}
			default:
				// period 把请求日期归一为当前时间维度的轴刻度:day 即日期本身,
				// month 取 YYYY-MM 前缀(数据行键已是该形态,截取为幂等)。
				period := func(key string) string { return key }
				if dims[0].name == "month" {
					period = func(key string) string { return key[:7] }
				}
				seenPeriods := make(map[string]bool, len(rowOrder))
				for _, r := range rowOrder {
					seenPeriods[period(r.rawKeys[0])] = true
				}
				for _, date := range dates {
					p := period(date)
					if seenPeriods[p] {
						continue
					}
					seenPeriods[p] = true
					fill(p, p)
				}
			}
		}

		// 稳定排序:含时间维度时该维度按原始键升序优先(day/month/hour 的原始键
		// 与显示键同序;weekday 显示名为星期名,原始键 ISO 周序 "0".."6" 才是时间
		// 序),再按 total 降序、完整显示键元组升序(同一有效配置与语言下确定)。
		sort.SliceStable(rowOrder, func(i, j int) bool {
			if temporalIdx >= 0 && rowOrder[i].rawKeys[temporalIdx] != rowOrder[j].rawKeys[temporalIdx] {
				return rowOrder[i].rawKeys[temporalIdx] < rowOrder[j].rawKeys[temporalIdx]
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

		// 总计:同一日期范围的独立全量聚合,与分组查询同处同一读事务。
		totals, err = rangeTotals(ctx, tq.tx, dates)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
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

	// 维度已在聚合核内完成白名单校验,此处按声明顺序完整填充渲染表头;
	// temporalIdx 记录首个时间维度下标(趋势列插入与行排序的轴),循环
	// 不得提前退出——时间维度之后声明的维度同样需要表头。
	dimHeaders := make([]string, len(view.Dimensions))
	temporalIdx := -1
	for i, name := range view.Dimensions {
		dimHeaders[i] = dimensionWhitelist[name].header
		if isTemporalDimension(name) && temporalIdx < 0 {
			temporalIdx = i
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

// rangeTotals 返回日期范围的全量聚合(总计行数据源,独立于分组结果);
// 与分组查询同处调用方开启的读事务,保证并发写入下两者看到同一快照。
func rangeTotals(ctx context.Context, tx *sql.Tx, dates []string) (GroupAggregate, error) {
	placeholders, args := buildPlaceholders(dates)
	query := fmt.Sprintf(
		"SELECT %s FROM messages WHERE date IN (%s)",
		groupSelectColumns, placeholders,
	)
	var totals GroupAggregate
	err := tx.QueryRowContext(ctx, query, args...).Scan(
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

// ByWeekday 按本机时区的星期分布聚合,ISO 周序呈现(周一在首):纯单维
// weekday 视图对整周 7 天固定刻度补零值行,时间轴与请求日期范围无关。
func (q *Querier) ByWeekday(ctx context.Context, dates []string) (string, error) {
	return q.RunDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"weekday"},
		TitleEn:    "Usage by weekday", TitleZh: "按星期用量",
	})
}

// SessionRow 是一条会话聚合结果:会话标识字段与该会话在日期范围内的聚合值。
// FirstTS/LastTS 是范围内该会话首末消息的毫秒时间戳(机器消费方据此计算会话
// 请求跨度),渲染侧经 formatDuration 显示为人类可读时长。
type SessionRow struct {
	Client  string
	Project string
	Title   string
	FirstTS int64
	LastTS  int64
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
		       COALESCE(MIN(m.ts),0),
		       COALESCE(MAX(m.ts),0),
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

	rows, err := q.queryContext(ctx, query, args...)
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
			&row.FirstTS, &row.LastTS,
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
	// 其余列（项目/客户端等分组键）自适应不截断。Duration 列由首末消息时间戳
	// 差计算请求跨度，位于标题之后、指标列之前。
	metrics := q.metricColumns()
	defs := append([]tableCol{
		{ui.HClient, ui.AlignLeft, 0},
		{ui.HProject, ui.AlignLeft, 0},
		{ui.HTitle, ui.AlignLeft, 30},
		{ui.HDuration, ui.AlignLeft, 0},
	}, metricTailCols(metrics)...)
	t := buildTable(defs)

	for _, row := range rows {
		project := row.Project
		// 「(未分类)」空值映射留在渲染侧,SessionRows 保留源字段原值供机器消费。
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		cells := append([]string{row.Client, project, row.Title, formatDuration(row.LastTS - row.FirstTS)},
			appendMetricCells(nil, metrics, row.Agg)...)
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
	err = q.queryRowContext(ctx, query, args...).Scan(
		&clientCount, &requestCount, &freshInput, &outputTokens, &cacheRead, &cacheCreate, &reasoning, &totalTokens)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	// 活跃天数:请求范围内实际有数据的天数(日均的分母)。
	var activeDays int64
	err = q.queryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(DISTINCT date) FROM messages WHERE date IN (%s)", placeholders), args...).Scan(&activeDays)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	// 单日峰值:按日聚合 total 的最大行,同分取日期升序首个,保证确定性;
	// 范围内无数据时无行,保持零值即可。
	var peakDate string
	var peakTotal int64
	err = q.queryRowContext(ctx, fmt.Sprintf(
		"SELECT date, SUM(total_tokens) FROM messages WHERE date IN (%s) GROUP BY date ORDER BY 2 DESC, 1 ASC LIMIT 1",
		placeholders), args...).Scan(&peakDate, &peakTotal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	var sb strings.Builder
	sb.WriteString(ui.Bi("Summary", "总览摘要") + "\n\n")
	// 统计范围只由 CLI 统一信息区的 Query range 行承载(单日不渲染 a ~ a),这里不再重复。
	fmt.Fprintf(&sb, "%s: %d\n", ui.Bi("Clients", "客户端数"), clientCount)
	fmt.Fprintf(&sb, "%s: %d\n", ui.Bi("Total requests", "请求总数"), requestCount)
	fmt.Fprintf(&sb, "%s: %d\n", ui.Bi("Active days", "活跃天数"), activeDays)
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColInput, formatTokens(freshInput))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColOutput, formatTokens(outputTokens))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColCacheRead, formatTokens(cacheRead))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColCacheCreate, formatTokens(cacheCreate))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColReasoning, formatTokens(reasoning))
	fmt.Fprintf(&sb, "%s: %s\n", ui.ColTotal, formatTokens(totalTokens))
	if activeDays > 0 {
		fmt.Fprintf(&sb, "%s: %s (%s)\n", ui.Bi("Peak day", "单日峰值"), peakDate, formatTokens(peakTotal))
		fmt.Fprintf(&sb, "%s: %s\n", ui.Bi("Daily average", "日均总量"), formatTokens(totalTokens/activeDays))
	}

	return sb.String(), nil
}

// RangeStats 是一段日期区间的全量聚合与活跃天数(区间内实际有数据的天数)。
type RangeStats struct {
	ActiveDays int64
	Total      GroupAggregate
}

// StatsBetween 统计 [fromDate, toDate] 闭区间的全量聚合。日期为 YYYY-MM-DD
// 形态,SQL 侧用 BETWEEN 字典序比较(等价时间序),不展开逐日占位符,任意长
// 区间参数量恒定。区间内无数据时返回零值(ActiveDays=0),不视为错误。
func (q *Querier) StatsBetween(ctx context.Context, fromDate, toDate string) (RangeStats, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return RangeStats{}, err
	}
	const query = `
		SELECT COUNT(DISTINCT date),
		       COALESCE(COUNT(*),0),
		       COALESCE(SUM(fresh_input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_create_tokens),0),
		       COALESCE(SUM(reasoning_tokens),0),
		       COALESCE(SUM(total_tokens),0)
		FROM messages
		WHERE date BETWEEN ? AND ?
	`
	var s RangeStats
	err = q.queryRowContext(ctx, query, fromDate, toDate).Scan(
		&s.ActiveDays,
		&s.Total.Requests, &s.Total.FreshInput, &s.Total.OutputTokens,
		&s.Total.CacheRead, &s.Total.CacheCreate, &s.Total.Reasoning, &s.Total.TotalTokens)
	if err != nil {
		return RangeStats{}, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}
	return s, nil
}

func formatTokens(tokens int64) string {
	if tokens < 0 {
		return fmt.Sprintf("%d", tokens)
	}
	return formatTokensUnsigned(uint64(tokens))
}

func formatTokensUnsigned(tokens uint64) string {
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

// FormatTokens 是 formatTokens 的导出别名,供 cli 层 forecast 渲染等包外
// 消费方与 query 表格使用同一 K/M/B 缩写口径,防止两处格式化漂移。
func FormatTokens(tokens int64) string {
	return formatTokens(tokens)
}

// FormatTokensUnsigned 使用与 FormatTokens 相同的 K/M/B 缩写口径渲染非负
// token 数。供需要表示 int64 最小值绝对值的调用方使用，避免将 1<<63
// 转回 int64 时溢出为负数。
func FormatTokensUnsigned(tokens uint64) string {
	return formatTokensUnsigned(tokens)
}

// heatLevels 是热力单元格的强度字符序列(下标 0..9):0 级为空格,密度沿
// . : - = + * # % @ 递增。只用 ASCII 保证任意终端下显示宽度恒为 1,避免块
// 字符(U+2580 系)在 CJK 环境的 ambiguous width 歧义。
const heatLevels = " .:-=+*#%@"

// heatCell 按单元格 total 相对全表最大值折算强度字符。total ≤ 0(无数据)
// 或 maxTotal ≤ 0(全表无正格)为空格;正值经 HeatLevel 量化后至少取最低
// 强度 heatLevels[1]——相对占比再小也是真实活动,不得折算回无数据空格。
func heatCell(total, maxTotal int64) byte {
	if total <= 0 || maxTotal <= 0 {
		return heatLevels[0]
	}
	l := HeatLevel(total, maxTotal)
	if l < 1 {
		l = 1
	}
	return heatLevels[l]
}

// HeatLevel 把 v 相对 max 的占比折算为 0..9 强度级(终端字符下标);SVG 侧
// 取色在其上加 1 得 1..10(0 值用最低级浅灰)。单一实现防止终端与 SVG 两处
// 折算漂移。
func HeatLevel(v, max int64) int {
	if max <= 0 || v <= 0 {
		return 0
	}
	var level int64
	// 先乘后除在 max 较大时可能溢出,按阈值切换运算顺序。
	if max < int64(1<<54) {
		level = v * 9 / max
	} else {
		level = v / (max / 9)
	}
	if level > 9 {
		level = 9
	}
	return int(level)
}

// HeatmapMatrix 是星期×小时热力矩阵的结构化数据:星期与小时标签为显示形态
// (双语星期名、HH:00),Values[wi][hi] 为交点 total(缺失交点为 0)。
type HeatmapMatrix struct {
	Weekdays []string
	Hours    []string
	Values   [][]int64
}

// HeatmapMatrix 计算星期×小时交点矩阵(本机时区归属),供终端渲染(Heatmap)
// 与 SVG 导出(chart)两类消费方共用,保证两侧行列与数值完全一致。
func (q *Querier) HeatmapMatrix(ctx context.Context, dates []string) (*HeatmapMatrix, error) {
	if _, err := q.readyContext(ctx); err != nil {
		return nil, err
	}
	rows, _, err := q.AggregateDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"weekday", "hour"},
		TitleEn:    "heatmap", TitleZh: "heatmap",
	})
	if err != nil {
		return nil, err
	}
	type cellKey struct{ weekday, hour string }
	cell := make(map[cellKey]int64, len(rows))
	for _, row := range rows {
		if len(row.rawKeys) != 2 {
			continue
		}
		cell[cellKey{row.rawKeys[0], row.rawKeys[1]}] += row.Agg.TotalTokens
	}

	m := &HeatmapMatrix{
		Weekdays: make([]string, len(weekdayTicks)),
		Hours:    hourDisplayLabels(),
		Values:   make([][]int64, len(weekdayTicks)),
	}
	for wi, tickW := range weekdayTicks {
		m.Weekdays[wi] = weekdayDisplayKey(tickW)
		m.Values[wi] = make([]int64, len(hourTicks))
		for hi, tickH := range hourTicks {
			m.Values[wi][hi] = cell[cellKey{tickW, tickH}]
		}
	}
	return m, nil
}

// hourDisplayLabels 返回小时列的显示标签("00:00".."23:00")。
func hourDisplayLabels() []string {
	labels := make([]string, len(hourTicks))
	for i, tick := range hourTicks {
		labels[i] = tick + ":00"
	}
	return labels
}

// Heatmap 输出星期×小时热力透视表:行为 ISO 周序(周一在首)的 7 个星期,
// 列为 00..23 的 24 个小时,单元格为该交点 total 相对全表最大值的强度字符,
// 尾列为各星期日合计,尾行为各小时合计与全表总计。矩阵数据来自
// HeatmapMatrix,与 SVG 导出共用同一来源。
func (q *Querier) Heatmap(ctx context.Context, dates []string) (string, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return "", err
	}
	m, err := q.HeatmapMatrix(ctx, dates)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return ui.Bi("Heatmap - no data", "热力图 - 无数据"), nil
	}

	var maxCell int64
	dayTotals := make([]int64, len(m.Weekdays))
	hourTotals := make([]int64, len(m.Hours))
	for wi := range m.Weekdays {
		for hi := range m.Hours {
			v := m.Values[wi][hi]
			if v > maxCell {
				maxCell = v
			}
			dayTotals[wi] += v
			hourTotals[hi] += v
		}
	}

	defs := make([]tableCol, 0, len(m.Hours)+2)
	defs = append(defs, tableCol{header: ui.HWeekday, align: ui.AlignLeft, limit: 0})
	for _, h := range m.Hours {
		defs = append(defs, tableCol{header: strings.TrimSuffix(h, ":00"), align: ui.AlignLeft, limit: 0})
	}
	defs = append(defs, tableCol{header: ui.HDayTotal, align: ui.AlignLeft, limit: 0})
	t := buildTable(defs)

	for wi, wd := range m.Weekdays {
		rowCells := []string{wd}
		for hi := range m.Hours {
			rowCells = append(rowCells, string(heatCell(m.Values[wi][hi], maxCell)))
		}
		rowCells = append(rowCells, formatTokens(dayTotals[wi]))
		t.Row(rowCells...)
	}

	totalRow := []string{ui.Bi("Total", "总计")}
	var grand int64
	for hi := range m.Hours {
		totalRow = append(totalRow, formatTokens(hourTotals[hi]))
		grand += hourTotals[hi]
	}
	totalRow = append(totalRow, formatTokens(grand))
	t.Row(totalRow...)

	var sb strings.Builder
	sb.WriteString(ui.Bi("Heatmap (weekday x hour, local time)", "热力图(星期×小时,本机时区)") + "\n")
	sb.WriteString(t.String())
	return sb.String(), nil
}

// formatDuration 把会话请求跨度(首末消息毫秒差)渲染为紧凑人类可读时长:
// 秒级以下归 "<1s",分钟以内保留秒,小时以内保留分钟,跨天保留小时;
// 负值(数据异常)渲染为占位符保持表格列宽稳定。
func formatDuration(ms int64) string {
	if ms < 0 {
		return "-"
	}
	if ms < 1000 {
		return "<1s"
	}
	totalSeconds := ms / 1000
	days := totalSeconds / 86400
	hours := (totalSeconds % 86400) / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// FormatDuration 是 formatDuration 的导出别名,供 cli 层 top 排行渲染等包外
// 消费方与 query session 表格使用同一时长口径,防止两处格式化漂移。
func FormatDuration(ms int64) string {
	return formatDuration(ms)
}

// SortTopRows 返回按会话排行口径排序的行独立副本:TotalTokens 降序,同值按
// Client 升序、再 Title 升序;前三键全并列时按会话首条消息时间戳 FirstTS 升序
// 决序(确定性全序,不依赖 SessionRows 的 SQL 排序,消除同名同量会话的行序
// 残余)。cli 的 top/report 与 web 仪表板的会话排行共用本实现,防止多入口
// 排序口径漂移。
func SortTopRows(rows []SessionRow) []SessionRow {
	sorted := make([]SessionRow, len(rows))
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

// TruncateTopRows 取排序后前 limit 行(limit 不小于行数时返回全部)。
func TruncateTopRows(rows []SessionRow, limit int) []SessionRow {
	if limit < len(rows) {
		return rows[:limit]
	}
	return rows
}
