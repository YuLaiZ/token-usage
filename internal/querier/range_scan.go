package querier

// range_scan.go 定义并实现区间聚合快照。
//
// 一次声明多个聚合规格，在同一个读事务内只扫描一遍 messages，同时产出
// 区间总计、维度组合、day×hour 矩阵与会话聚合；完整行返回给调用方，
// Top N 截断与“其他”尾行由消费侧负责。
//
// 契约：Snapshot 不持有 tx/rows，取回后只读；需要排序/截断的调用方
// 必须自行复制，不得原地修改。

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// AggKind 是聚合规格的种类。
type AggKind int

const (
	// AggDimension 是任意维度组合聚合（含单维）。
	AggDimension AggKind = iota
	// AggDayHour 是 day × hour 矩阵（Web 热力图/趋势数据源）。
	AggDayHour
	// AggSession 是会话聚合（会话明细/排行数据源）。
	AggSession
)

// AggSpec 声明一个聚合规格。
//
// Name 仅用于 AggDimension 的结果取回，只需在维度结果集合内唯一；
// AggDayHour/AggSession 通过 Snapshot.DayHour/Sessions 按类型取回，Name 忽略。
type AggSpec struct {
	Name       string
	Kind       AggKind
	Dimensions []string
	Aliases    map[string]string
}

// RangeTotals 是区间总计：GroupAggregate + 活跃天数。
type RangeTotals struct {
	GroupAggregate
	ActiveDays int64
}

// DimensionResult 是单个维度视图的结果：完整行 + 该视图的总计行。
// 行不做 Top N 截断、不生成“其他”尾行，截断由消费侧负责。
type DimensionResult struct {
	Rows   []DimensionRow
	Totals GroupAggregate
}

// Snapshot 是一次 ScanRange 的结果，取回后只读；需要排序/截断的调用方
// 必须自行复制，不得原地修改。
type Snapshot struct {
	totals      RangeTotals
	dims        map[string]DimensionResult
	dayHour     *DayHourMatrix
	hasDayHour  bool
	sessions    []SessionRow
	hasSessions bool
}

// Totals 返回区间总计。
func (s *Snapshot) Totals() RangeTotals { return s.totals }

// Dimension 按名取回维度结果；未声明的名称返回 false。
func (s *Snapshot) Dimension(name string) (DimensionResult, bool) {
	if s == nil {
		return DimensionResult{}, false
	}
	r, ok := s.dims[name]
	return r, ok
}

// DayHour 取回 day × hour 矩阵；未声明时返回 false。
func (s *Snapshot) DayHour() (*DayHourMatrix, bool) {
	if s == nil || !s.hasDayHour {
		return nil, false
	}
	return s.dayHour, true
}

// Sessions 取回会话行；未声明时返回 false。
func (s *Snapshot) Sessions() ([]SessionRow, bool) {
	if s == nil || !s.hasSessions {
		return nil, false
	}
	return s.sessions, true
}

// scanRow 是单遍扫描从 messages 读出的一行（只含聚合所需列）。
type scanRow struct {
	date, client, model, provider, routerProvider, project, sessionID string
	ts                                                                int64
	freshInput, output, cacheRead, cacheCreate, reasoning, total      int64
	durationMS                                                        int64
}

// aggregateOf 把一行明细变成可加聚合：duration 仅 duration_ms>0 的行计入
// 三分量，与 groupSelectColumns 的 CASE WHEN duration_ms>0 一致。
func (r *scanRow) aggregateOf() GroupAggregate {
	agg := GroupAggregate{
		Requests: 1, FreshInput: r.freshInput, OutputTokens: r.output,
		CacheRead: r.cacheRead, CacheCreate: r.cacheCreate,
		Reasoning: r.reasoning, TotalTokens: r.total,
	}
	if r.durationMS > 0 {
		agg.DurationSumMS = r.durationMS
		agg.DurationCount = 1
		agg.DurationOutputSum = r.output
	}
	return agg
}

// dimensionAccumulator 为单个维度规格维护分组聚合：raw 取值 → 显示键 →
// 折叠键 → 组内累加，并保留各维显示拼写变体供 finalize 回填代表拼写。
type dimensionAccumulator struct {
	dims    []dimension
	aliases map[string]string
	rows    []DimensionRow
	index   map[string]int
	rawKeys []string
	keys    []string
	foldBuf []string
}

func newDimensionAccumulator(dims []dimension, aliases map[string]string) *dimensionAccumulator {
	return &dimensionAccumulator{
		dims:    dims,
		aliases: aliases,
		index:   make(map[string]int),
		rawKeys: make([]string, len(dims)),
		keys:    make([]string, len(dims)),
		foldBuf: make([]string, len(dims)),
	}
}

func (a *dimensionAccumulator) absorb(row *scanRow, agg GroupAggregate) {
	for i, d := range a.dims {
		raw := dimensionRawValue(d, row)
		a.rawKeys[i] = raw
		a.keys[i] = d.displayKey(raw, a.aliases)
	}
	for i, d := range a.dims {
		a.foldBuf[i] = dimensionFoldKey(d, a.rawKeys[i], a.keys[i])
	}
	key := strings.Join(a.foldBuf, "\x00")
	if idx, ok := a.index[key]; ok {
		dst := &a.rows[idx]
		before := dst.Agg.Requests
		dst.Agg.add(agg)
		for i := range a.dims {
			dst.absorbVariantIfNeeded(i, a.keys[i], a.rawKeys[i], before, agg.Requests)
		}
		return
	}
	a.index[key] = len(a.rows)
	stored := DimensionRow{
		Keys:    append([]string(nil), a.keys...),
		rawKeys: append([]string(nil), a.rawKeys...),
	}
	stored.Agg = agg
	a.rows = append(a.rows, stored)
}

// dimensionRawValue 返回维度在 Go 侧与原 SQL selectExpr 等价的原始键：
// provider 沿用 effectiveProviderExpr 的取舍，month 取 date 前 7 位，
// hour/weekday 与裸列路径同样走 bucketKeyOf（本机时区）。
func dimensionRawValue(d dimension, row *scanRow) string {
	switch d.name {
	case "client":
		return row.client
	case "model":
		return row.model
	case "provider":
		if row.routerProvider != "" {
			return row.routerProvider
		}
		return row.provider
	case "project":
		return row.project
	case "day":
		return row.date
	case "month":
		if len(row.date) >= 7 {
			return row.date[:7]
		}
		return row.date
	case "hour":
		return bucketKeyOf("hour", row.ts)
	case "weekday":
		return bucketKeyOf("weekday", row.ts)
	}
	return ""
}

// sessionKey 是会话聚合的匹配键：现有 SessionRows 的 JOIN 条件即
// (session_id, client) 双列。
type sessionKey struct {
	id, client string
}

// sessionMeta 是 sessions 表中参与会话结果展示的列。
type sessionMeta struct {
	title   string
	project string
}

// sessionAccumulator 按 (session_id, client) 累加选区内的会话聚合。
// 元数据不预加载：扫描结束后只按命中的 key 批量查询 sessions，避免为短选区
// 加载全部历史会话。
type sessionAccumulator struct {
	accs      map[sessionKey]*sessionAcc
	order     []sessionKey
	firstDate map[sessionKey]string
}

type sessionAcc struct {
	client  string
	firstTS int64
	lastTS  int64
	agg     GroupAggregate
}

func newSessionAccumulator() *sessionAccumulator {
	return &sessionAccumulator{
		accs:      make(map[sessionKey]*sessionAcc),
		firstDate: make(map[sessionKey]string),
	}
}

func (a *sessionAccumulator) absorb(row *scanRow, agg GroupAggregate) {
	k := sessionKey{id: row.sessionID, client: row.client}
	acc, ok := a.accs[k]
	if !ok {
		acc = &sessionAcc{client: row.client, firstTS: row.ts, lastTS: row.ts}
		a.accs[k] = acc
		a.firstDate[k] = row.date
		a.order = append(a.order, k)
	} else {
		if row.ts < acc.firstTS {
			acc.firstTS = row.ts
		}
		if row.ts > acc.lastTS {
			acc.lastTS = row.ts
		}
		if row.date < a.firstDate[k] {
			a.firstDate[k] = row.date
		}
	}
	// 现有 SessionRows 不填充逐请求 duration 三分量，本次保持同一合同：
	// 会话明细的 duration 列继续由首末时间戳差渲染。
	agg.DurationSumMS, agg.DurationCount, agg.DurationOutputSum = 0, 0, 0
	acc.agg.add(agg)
}

// keys 返回选区命中的会话 key（按首次出现顺序）。
func (a *sessionAccumulator) keys() []sessionKey { return a.order }

// sessionSortEntry 把排序键与行绑定，避免排序过程中用切片下标回查
// 原始插入顺序（排序会重排切片，下标不再对应）。
type sessionSortEntry struct {
	row       SessionRow
	firstDate string
	rawClient string
}

// rows 返回按“选区首日升序、原始 client 升序、total 降序”整理的会话明细，
// Client 已映射为显示名（与现有 SessionRows 一致）。meta 只包含命中 key 的
// 元数据，未命中的会话被跳过（等价原 SQL 内连接）。
func (a *sessionAccumulator) rows(meta map[sessionKey]sessionMeta) []SessionRow {
	entries := make([]sessionSortEntry, 0, len(a.order))
	for _, k := range a.order {
		m, ok := meta[k]
		if !ok {
			continue // 等价原 SQL 内连接：sessions 中无匹配的会话不进入结果
		}
		acc := a.accs[k]
		entries = append(entries, sessionSortEntry{
			row: SessionRow{
				Client:  model.ClientDisplayName(acc.client),
				Project: m.project,
				Title:   m.title,
				FirstTS: acc.firstTS,
				LastTS:  acc.lastTS,
				Agg:     acc.agg,
			},
			firstDate: a.firstDate[k],
			rawClient: acc.client,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].firstDate != entries[j].firstDate {
			return entries[i].firstDate < entries[j].firstDate
		}
		if entries[i].rawClient != entries[j].rawClient {
			return entries[i].rawClient < entries[j].rawClient
		}
		return entries[i].row.Agg.TotalTokens > entries[j].row.Agg.TotalTokens
	})
	out := make([]SessionRow, len(entries))
	for i := range entries {
		out[i] = entries[i].row
	}
	return out
}

// ScanRange 在同一个读事务内只扫描一遍 messages，按 specs 产出全部聚合。
// dates 为闭区间逐日列表；空列表返回零值快照（与现有查询一致，不视为错误）。
// 规格非法时返回错误，且不产生部分结果。
func (q *Querier) ScanRange(ctx context.Context, dates []string, specs []AggSpec) (*Snapshot, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateAggSpecs(specs); err != nil {
		return nil, err
	}

	snap := &Snapshot{dims: make(map[string]DimensionResult), sessions: []SessionRow{}}
	if len(dates) == 0 {
		for _, spec := range specs {
			switch spec.Kind {
			case AggDimension:
				snap.dims[spec.Name] = DimensionResult{Rows: []DimensionRow{}}
			case AggDayHour:
				snap.dayHour = emptyDayHourMatrix(nil)
				snap.hasDayHour = true
			case AggSession:
				snap.hasSessions = true
			}
		}
		return snap, nil
	}

	err = q.ReadTx(ctx, func(tq *Querier) error {
		// 按声明构造累加器：未声明的类型不分配、不查询。
		dimAccs := make(map[string]*dimensionAccumulator, len(specs))
		dimOrder := make([]string, 0, len(specs))
		var dayHourCells map[dayHourKey]int64
		sessAcc := (*sessionAccumulator)(nil)
		for _, spec := range specs {
			switch spec.Kind {
			case AggDimension:
				dims := make([]dimension, 0, len(spec.Dimensions))
				for _, name := range spec.Dimensions {
					dims = append(dims, dimensionWhitelist[name])
				}
				dimAccs[spec.Name] = newDimensionAccumulator(dims, spec.Aliases)
				dimOrder = append(dimOrder, spec.Name)
			case AggDayHour:
				dayHourCells = make(map[dayHourKey]int64)
			case AggSession:
				sessAcc = newSessionAccumulator()
			}
		}

		rows, err := tq.queryContext(ctx, fmt.Sprintf(`
			SELECT date, ts, client, model, provider, router_provider, project, session_id,
			       fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
			       reasoning_tokens, total_tokens, duration_ms
			FROM messages WHERE date IN (%s)`, placeholdersOnly(len(dates))), dateArgs(dates)...)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
		}
		defer rows.Close()

		seenDates := make(map[string]struct{}, len(dates))
		var totals GroupAggregate
		var row scanRow
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := rows.Scan(
				&row.date, &row.ts, &row.client, &row.model, &row.provider,
				&row.routerProvider, &row.project, &row.sessionID,
				&row.freshInput, &row.output, &row.cacheRead, &row.cacheCreate,
				&row.reasoning, &row.total, &row.durationMS,
			); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("scan aggregate rows failed", "扫描聚合结果失败"), err)
			}
			agg := row.aggregateOf()
			totals.add(agg)
			seenDates[row.date] = struct{}{}
			for _, name := range dimOrder {
				dimAccs[name].absorb(&row, agg)
			}
			if dayHourCells != nil {
				dayHourCells[dayHourKey{day: row.date, hour: bucketKeyOf("hour", row.ts)}] += agg.TotalTokens
			}
			if sessAcc != nil {
				sessAcc.absorb(&row, agg)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("iterate aggregate rows failed", "遍历聚合结果失败"), err)
		}

		snap.totals = RangeTotals{GroupAggregate: totals, ActiveDays: int64(len(seenDates))}
		for _, name := range dimOrder {
			acc := dimAccs[name]
			acc.rows = finalizeDimensionRows(acc.rows, acc.dims, dates)
			if acc.rows == nil {
				acc.rows = []DimensionRow{}
			}
			snap.dims[name] = DimensionResult{Rows: acc.rows, Totals: totals}
		}
		if dayHourCells != nil {
			snap.dayHour = buildDayHourMatrix(dates, dayHourCells)
			snap.hasDayHour = true
		}
		if sessAcc != nil {
			meta, err := tq.loadSessionMetaForKeys(ctx, sessAcc.keys())
			if err != nil {
				return err
			}
			snap.sessions = sessAcc.rows(meta)
			snap.hasSessions = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// loadSessionMetaForKeys 按选区命中的 (session_id, client) 分批查询展示列。
// 短选区只读取少量会话，避免为构造会话结果而映射全部历史 sessions；批大小
// 控制单条 SQL 的绑定变量数量。返回的 map 只含命中 key 的元数据。
func (q *Querier) loadSessionMetaForKeys(ctx context.Context, keys []sessionKey) (map[sessionKey]sessionMeta, error) {
	out := make(map[sessionKey]sessionMeta, len(keys))
	const batch = 300
	for start := 0; start < len(keys); start += batch {
		end := start + batch
		if end > len(keys) {
			end = len(keys)
		}
		placeholders := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*2)
		for _, k := range keys[start:end] {
			placeholders = append(placeholders, "(?,?)")
			args = append(args, k.id, k.client)
		}
		query := fmt.Sprintf(
			"SELECT id, client, title, project FROM sessions WHERE (id, client) IN (%s)",
			strings.Join(placeholders, ","))
		rows, err := q.queryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
		}
		for rows.Next() {
			var k sessionKey
			var meta sessionMeta
			if err := rows.Scan(&k.id, &k.client, &meta.title, &meta.project); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: %w", ui.Bi("scan session rows failed", "扫描会话结果失败"), err)
			}
			out[k] = meta
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: %w", ui.Bi("iterate session rows failed", "遍历会话结果失败"), err)
		}
		rows.Close()
	}
	return out, nil
}

// placeholdersOnly 返回 n 个以逗号分隔的 SQL 占位符。
func placeholdersOnly(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// dateArgs 把日期列表转成查询参数。
func dateArgs(dates []string) []any {
	args := make([]any, len(dates))
	for i, d := range dates {
		args[i] = d
	}
	return args
}

// dayHourKey 是 day×hour 矩阵的单元格键。
type dayHourKey struct {
	day, hour string
}

// emptyDayHourMatrix 构造与现有 DayHourMatrix 一致的零值矩阵。
func emptyDayHourMatrix(dates []string) *DayHourMatrix {
	m := &DayHourMatrix{
		Days:   append([]string(nil), dates...),
		Hours:  append([]string(nil), hourBucketTable[:]...),
		Totals: make([]int64, len(dates)),
		Values: make([][]int64, len(dates)),
	}
	for i := range m.Values {
		m.Values[i] = make([]int64, len(hourBucketTable))
	}
	return m
}

// buildDayHourMatrix 按请求日期顺序与固定小时刻度构造矩阵。
func buildDayHourMatrix(dates []string, cells map[dayHourKey]int64) *DayHourMatrix {
	m := emptyDayHourMatrix(dates)
	for di, day := range dates {
		for hi, hour := range m.Hours {
			v := cells[dayHourKey{day: day, hour: hour}]
			m.Values[di][hi] = v
			m.Totals[di] += v
		}
	}
	return m
}

// validateAggSpecs 校验规格集合：维度白名单、重复维度、结果名非空且唯一、
// DayHour/Session 各至多一次、Kind 已知。
func validateAggSpecs(specs []AggSpec) error {
	seenNames := make(map[string]bool, len(specs))
	seenDayHour, seenSession := false, false
	for i, spec := range specs {
		switch spec.Kind {
		case AggDimension:
			if spec.Name == "" {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("aggregate spec %d: dimension result name must not be empty", i),
					fmt.Sprintf("聚合规格 %d：维度结果名不能为空", i),
				))
			}
			if seenNames[spec.Name] {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("aggregate spec %d: duplicate dimension result name %q", i, spec.Name),
					fmt.Sprintf("聚合规格 %d：维度结果名 %q 重复", i, spec.Name),
				))
			}
			seenNames[spec.Name] = true
			if len(spec.Dimensions) == 0 {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("aggregate spec %d: dimension view requires at least one dimension", i),
					fmt.Sprintf("聚合规格 %d：维度视图至少需要一个维度", i),
				))
			}
			seenDims := make(map[string]bool, len(spec.Dimensions))
			for _, name := range spec.Dimensions {
				if _, ok := dimensionWhitelist[name]; !ok {
					return fmt.Errorf("%s", ui.Bi(
						fmt.Sprintf("aggregate spec %d: unknown dimension %q (allowed: %s)", i, name, dimensionNameList()),
						fmt.Sprintf("聚合规格 %d：未知维度 %q（允许：%s）", i, name, dimensionNameList()),
					))
				}
				if seenDims[name] {
					return fmt.Errorf("%s", ui.Bi(
						fmt.Sprintf("aggregate spec %d: duplicate dimension %q", i, name),
						fmt.Sprintf("聚合规格 %d：维度 %q 重复", i, name),
					))
				}
				seenDims[name] = true
			}
		case AggDayHour:
			if seenDayHour {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("aggregate spec %d: day-hour matrix may be declared at most once", i),
					fmt.Sprintf("聚合规格 %d：day×hour 矩阵至多声明一次", i),
				))
			}
			seenDayHour = true
		case AggSession:
			if seenSession {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("aggregate spec %d: session aggregate may be declared at most once", i),
					fmt.Sprintf("聚合规格 %d：会话聚合至多声明一次", i),
				))
			}
			seenSession = true
		default:
			return fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("aggregate spec %d: unknown aggregate kind %d", i, int(spec.Kind)),
				fmt.Sprintf("聚合规格 %d：未知聚合类型 %d", i, int(spec.Kind)),
			))
		}
	}
	return nil
}
