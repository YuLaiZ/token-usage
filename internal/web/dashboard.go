package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// dashboard.go 输出 GET /api/dashboard 的载荷(v4):区间汇总、四个内置
// 维度的最终行(前 9 独立项+至多一行"其他")、与范围形态匹配的趋势桶与
// 热力展示桶、按 query.subqueries 配置生成的自定义视图最终行(前 19 组合
// +至多一行"其他组合")与前 20 会话。compare/forecast 区块已随仪表板重做
// 移除(定稿页面无此两区);图表全部由前端消费数值行自绘,服务端不产 SVG。
// 全部查询在同一读事务内完成(同一 WAL 快照),totals、各维度行、自定义
// 视图、sessions、趋势与热力互相一致;截断、尾行汇总与桶整理都在响应
// 前完成,前端只做呈现,不再消费完整明细或二次聚合。

// rangeDaysLimit 是 from/to 区间的天数上限(含两端):与 cli 侧日期参数的
// 366 天上限同口径(恰好容纳一个闰年)。
const rangeDaysLimit = 366

// dashboardSessionLimit 是仪表板 Top sessions 的返回行数(2026-09-24 起
// 从 10 提到 20;会话榜不生成"其他"尾行)。
const dashboardSessionLimit = 20

// 维度/自定义视图的返回上限:四维度各至多 9 个独立项+至多一行"其他",
// 自定义视图各至多 19 个独立组合+至多一行"其他组合"。余项为零时不生成
// 尾行;尾行固定最后,即使其 total 大于最后一个独立项也不插队。
const (
	dimensionTopN  = 9
	customViewTopN = 19
)

// 趋势与热力的桶形态边界(天):
//   - 趋势:单日=24 个小时桶;2~62 天=逐日桶;更长=自区间起点每 7 天一周桶;
//   - 热力:1~7 天=日期×小时;8~31 天=日期×4 小时时段(6 段);32 天以上=逐日日历。
const (
	trendDailyMaxDays   = 62
	heatmapShortMaxDays = 7
	heatmapBlockMaxDays = 31
)

// dashboardDimensions 是仪表板固定聚合的四个业务维度:JSON 键名与维度名
// 一致(键序无关,前端按名取用)。
var dashboardDimensions = []string{"client", "model", "provider", "project"}

// metaResponse 是 GET /api/meta 的载荷;四个日期/时间字段在无数据时为 null。
type metaResponse struct {
	Version        string  `json:"version"`
	MinDate        *string `json:"min_date"`
	MaxDate        *string `json:"max_date"`
	DataThrough    *string `json:"data_through"`
	LastCollection *string `json:"last_collection"`
}

// rangeJSON 是仪表板的统计区间(闭区间,YYYY-MM-DD)。
type rangeJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// totalsJSON 是区间全量汇总;全部整数,格式化交给前端。
type totalsJSON struct {
	Requests    int64 `json:"requests"`
	FreshInput  int64 `json:"fresh_input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Reasoning   int64 `json:"reasoning"`
	Total       int64 `json:"total"`
	ActiveDays  int64 `json:"active_days"`
}

// dimensionRowJSON 是一个维度分组的聚合行;全部整数,格式化交给前端。
// 尾行(其他)以 IsOther=true 标识并携带 OtherCount(被合并的独立项数):
// 前端据机器字段本地化显示,不解析展示文案。独立行两字段不出现(omitempty)。
type dimensionRowJSON struct {
	Key         string `json:"key"`
	Requests    int64  `json:"requests"`
	FreshInput  int64  `json:"fresh_input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheCreate int64  `json:"cache_create"`
	Reasoning   int64  `json:"reasoning"`
	Total       int64  `json:"total"`
	IsOther     bool   `json:"is_other,omitempty"`
	OtherCount  int    `json:"other_count,omitempty"`
}

// trendBucketJSON 是一个趋势桶:Key 为桶边界标识(小时桶 "00".."23"、日桶
// 为日期、周桶为该桶最后一天的日期),前端按粒度本地化显示;Total 为整数。
type trendBucketJSON struct {
	Key   string `json:"key"`
	Total int64  `json:"total"`
}

// trendJSON 是与区间形态匹配的趋势序列:Granularity 为 hour/day/week;
// PeakIndex 是首个最大值桶的下标(平手取先出现者),Empty=true 表示区间
// 无桶或全零——前端显示空状态,不渲染"峰值 0"。
type trendJSON struct {
	Granularity string            `json:"granularity"`
	Buckets     []trendBucketJSON `json:"buckets"`
	PeakIndex   int               `json:"peak_index"`
	PeakTotal   int64             `json:"peak_total"`
	Empty       bool              `json:"empty"`
}

// heatmapDayJSON 是热力数据的一行。Total 恒为该日全天合计;Future 标记
// 该日尚未发生(晚于请求时刻的今天),前端渲染为空白格——已发生但无用量的
// 零值日是真实零格,两者语义不同,不得互相伪装。Hours(24 格,模式
// day_hour)与 Blocks(6 格 4 小时时段,模式 day_block)按模式出现其一,
// calendar 模式两者都省略。
type heatmapDayJSON struct {
	Date   string  `json:"date"`
	Total  int64   `json:"total"`
	Future bool    `json:"future,omitempty"`
	Hours  []int64 `json:"hours,omitempty"`
	Blocks []int64 `json:"blocks,omitempty"`
}

// heatmapJSON 是与查询范围形态匹配的热力展示桶:Mode 决定前端渲染器
// (day_hour=1~7 天逐小时、day_block=8~31 天 4 小时时段、calendar=32 天以上
// 逐日日历);days 与范围逐日对齐(含零值日,升序),展示桶的合并已在后端
// 完成,前端不再做跨日/跨时段业务聚合。
type heatmapJSON struct {
	From string           `json:"from"`
	To   string           `json:"to"`
	Mode string           `json:"mode"`
	Days []heatmapDayJSON `json:"days"`
}

// customViewRowJSON 是自定义视图的一行:Keys 按视图维度声明顺序取显示键
// (provider 维度已合并别名),七项指标与维度行同构。尾行(其他组合)以
// IsOther=true 标识并携带 OtherCount;独立行两字段不出现(omitempty)。
type customViewRowJSON struct {
	Keys        []string `json:"keys"`
	Requests    int64    `json:"requests"`
	FreshInput  int64    `json:"fresh_input"`
	Output      int64    `json:"output"`
	CacheRead   int64    `json:"cache_read"`
	CacheCreate int64    `json:"cache_create"`
	Reasoning   int64    `json:"reasoning"`
	Total       int64    `json:"total"`
	IsOther     bool     `json:"is_other,omitempty"`
	OtherCount  int      `json:"other_count,omitempty"`
}

// customViewJSON 是一个自定义视图(query.subqueries 中的一项)的聚合结果:
// Dimensions 为配置声明顺序的维度名。行间七项合计与区间 totals 逐项守恒
// (同一读事务、同一选区聚合)。
type customViewJSON struct {
	Name       string              `json:"name"`
	Dimensions []string            `json:"dimensions"`
	Rows       []customViewRowJSON `json:"rows"`
}

// sessionRowJSON 是一条会话排行行;FirstTS/LastTS 为毫秒时间戳,
// DurationMS 为首末消息跨度,格式化交给前端。Title 为数据源原文,
// 不做清洗或重写。
type sessionRowJSON struct {
	Client     string `json:"client"`
	Project    string `json:"project"`
	Title      string `json:"title"`
	FirstTS    int64  `json:"first_ts"`
	LastTS     int64  `json:"last_ts"`
	DurationMS int64  `json:"duration_ms"`
	Requests   int64  `json:"requests"`
	Total      int64  `json:"total"`
}

// dashboardResponse 是 GET /api/dashboard 的载荷。dimensions 固定四键
// (client/model/provider/project),各键至多 9 个独立行+至多一行"其他";
// trend 按区间形态给出小时/日/周桶与峰值、空态;heatmap 按区间形态给出
// 对应展示桶;custom_views 随 query.subqueries 配置生成,无配置时为空数组;
// columns 是 query 输出列布局的指标 ID 序列,只影响明细表,顶部七项概览
// 独立于此。
type dashboardResponse struct {
	Range       rangeJSON                     `json:"range"`
	Totals      totalsJSON                    `json:"totals"`
	Columns     []string                      `json:"columns"`
	Dimensions  map[string][]dimensionRowJSON `json:"dimensions"`
	Trend       trendJSON                     `json:"trend"`
	Heatmap     heatmapJSON                   `json:"heatmap"`
	CustomViews []customViewJSON              `json:"custom_views"`
	Sessions    []sessionRowJSON              `json:"sessions"`
}

// handleMeta 输出服务版本与全库数据边界:最小/最大日期、数据截至
// (全库最大消息时间)与最近一次成功采集时间。同一读事务内取齐,保证
// 边界三项互相一致。在线状态由页面成功连接本服务这一事实表达,不在
// 载荷中重复下发。
func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := metaResponse{Version: s.version}
	err := s.q.ReadTx(ctx, func(tq *querier.Querier) error {
		minDate, maxDate, found, err := tq.DateSpan(ctx)
		if err != nil {
			return err
		}
		if found {
			resp.MinDate, resp.MaxDate = &minDate, &maxDate
		}
		through, err := tq.DataThrough(ctx)
		if err != nil {
			return err
		}
		if through > 0 {
			formatted := time.UnixMilli(through).Local().Format(time.DateTime)
			resp.DataThrough = &formatted
		}
		// dates 为 nil 时 Freshness 只回最近成功采集,不做范围过滤。
		fresh, err := tq.Freshness(ctx, nil)
		if err != nil {
			return err
		}
		if !fresh.LastCollection.IsZero() {
			formatted := fresh.LastCollection.Format(time.DateTime)
			resp.LastCollection = &formatted
		}
		return nil
	})
	if err != nil {
		writeInternal(w, "failed to load metadata", "读取元信息失败", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboard 输出统计区间内的汇总、四个业务维度行、日期×小时热力
// 数据、自定义视图与 Top sessions。区间缺省 to=今天、from=to-29;前端不
// 限制日期,未来或无数据范围返回结构完整的零值(不回退到最后有数据日期)。
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// 同一请求统一取一次时钟:区间缺省口径唯一,防跨午夜瞬间错位。
	now := time.Now()
	dates, from, to, _, _, perr := parseRangeQuery(r, now)
	if perr != nil {
		writeError(w, *perr)
		return
	}
	ctx := r.Context()
	// 配置在事务外读取:provider 别名、自定义视图定义与输出列布局都是
	// 配置面,不参与数据快照;每请求重读,配置保存后的下一次请求即时生效。
	cfg := s.currentConfig()
	aliases := providerAliasesOf(cfg)
	views := customViewDefs(cfg)
	columns := outputColumnsFor(cfg)

	resp := dashboardResponse{
		Range:       rangeJSON{From: from, To: to},
		Dimensions:  make(map[string][]dimensionRowJSON, len(dashboardDimensions)),
		Trend:       trendJSON{Granularity: "day", Buckets: []trendBucketJSON{}, PeakIndex: -1},
		Heatmap:     heatmapJSON{From: from, To: to, Days: []heatmapDayJSON{}},
		CustomViews: []customViewJSON{},
		Sessions:    []sessionRowJSON{},
	}
	err := s.q.ReadTx(ctx, func(tq *querier.Querier) error {
		stats, err := tq.StatsBetween(ctx, from, to)
		if err != nil {
			return err
		}
		resp.Totals = totalsJSON{
			Requests:    stats.Total.Requests,
			FreshInput:  stats.Total.FreshInput,
			Output:      stats.Total.OutputTokens,
			CacheRead:   stats.Total.CacheRead,
			CacheCreate: stats.Total.CacheCreate,
			Reasoning:   stats.Total.Reasoning,
			Total:       stats.Total.TotalTokens,
			ActiveDays:  stats.ActiveDays,
		}
		resp.Columns = columns
		// 业务维度循环:同一选区全量聚合后,在响应前完成排序、前 9 截取与
		// 尾行逐指标汇总;独立行加尾行与区间 totals 逐项守恒。
		for _, dim := range dashboardDimensions {
			rows, _, err := tq.AggregateDimensionView(ctx, dates, querier.DimensionView{
				Dimensions: []string{dim},
				Aliases:    aliasesFor(dim, aliases),
				TitleEn:    "dashboard", TitleZh: "dashboard",
			})
			if err != nil {
				return err
			}
			resp.Dimensions[dim] = topDimensionRows(toDimensionRows(rows), dimensionTopN)
		}
		// 自定义视图:按配置维度顺序的多维组合聚合,与四维度同一选区、
		// 同一事务;前 19 截取与尾行同样在响应前完成。
		for _, view := range views {
			rows, _, err := tq.AggregateDimensionView(ctx, dates, querier.DimensionView{
				Dimensions: view.dimensions,
				Aliases:    aliases,
				TitleEn:    view.name, TitleZh: view.name,
			})
			if err != nil {
				return err
			}
			resp.CustomViews = append(resp.CustomViews, customViewJSON{
				Name:       view.name,
				Dimensions: view.dimensionNames,
				Rows:       topCustomViewRows(toCustomViewRows(rows), customViewTopN),
			})
		}
		sessions, err := tq.SessionRows(ctx, dates)
		if err != nil {
			return err
		}
		// 会话排行按总量降序返回前 20 行,不生成"其他会话"。
		resp.Sessions = toSessionRows(querier.TruncateTopRows(querier.SortTopRows(sessions), dashboardSessionLimit))
		// 日期×小时数据在同一事务内取数,与 totals/dimensions/sessions
		// 同快照;趋势桶与热力展示桶由同一份数据确定性整理。
		hm, err := tq.DayHourMatrix(ctx, dates)
		if err != nil {
			return err
		}
		resp.Trend = buildTrend(hm)
		resp.Heatmap.Days, resp.Heatmap.Mode = buildHeatmapDays(hm, now)
		return nil
	})
	if err != nil {
		writeInternal(w, "dashboard query failed", "仪表板查询失败", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// toCustomViewRows 把多维组合聚合行转换为 JSON 行:Keys 保留全部维度的
// 显示键(顺序即视图维度声明顺序)。
func toCustomViewRows(rows []querier.DimensionRow) []customViewRowJSON {
	out := make([]customViewRowJSON, 0, len(rows))
	for _, row := range rows {
		keys := append([]string(nil), row.Keys...)
		out = append(out, customViewRowJSON{
			Keys:        keys,
			Requests:    row.Agg.Requests,
			FreshInput:  row.Agg.FreshInput,
			Output:      row.Agg.OutputTokens,
			CacheRead:   row.Agg.CacheRead,
			CacheCreate: row.Agg.CacheCreate,
			Reasoning:   row.Agg.Reasoning,
			Total:       row.Agg.TotalTokens,
		})
	}
	return out
}

// outputColumnsFor 从当前配置解析输出列布局,与 cli 侧
// staticTableOutputLayout 同语义:query 顶层问题态回退默认布局,query.output
// 自身解析失败也回退默认(仪表板是只读展示,诊断由 /api/config 下发)。
// 每请求调用——列布局不再依赖服务启动时的 Querier 状态,网页保存新列后
// 下一次仪表板请求即生效。
func outputColumnsFor(cfg *config.Config) []string {
	if cfg == nil || len(cfg.RawQueryTopLevelIssues) > 0 {
		return ui.DefaultOutputColumns()
	}
	cols, err := querydef.ParseOutputLayout(querydef.Input{RawQuery: cfg.RawQuery})
	if err != nil {
		return ui.DefaultOutputColumns()
	}
	return cols
}

// providerAliasesOf 安全取配置中的供应商别名(nil 配置返回 nil)。
func providerAliasesOf(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	return cfg.ProviderAliases
}

// customViewDef 是一个待查询的自定义视图定义:querydef 维度值序列与
// 名字切片(Dimensions 的字符串形态,供 JSON 输出)。
type customViewDef struct {
	name           string
	dimensions     []string
	dimensionNames []string
}

// customViewDefs 从配置解析 query.subqueries;解析失败(配置损坏)时返回
// 空列表——仪表板是只读展示,损坏配置的诊断由 /api/config 下发,不在
// 数据接口报错。
func customViewDefs(cfg *config.Config) []customViewDef {
	if cfg == nil {
		return nil
	}
	defs, err := querydef.ParseViews(querydef.Input{RawQuery: cfg.RawQuery})
	if err != nil {
		return nil
	}
	out := make([]customViewDef, 0, len(defs.Subqueries))
	for _, sq := range defs.Subqueries {
		def := customViewDef{name: sq.Name, dimensionNames: make([]string, 0, len(sq.Dimensions))}
		for _, dim := range sq.Dimensions {
			def.dimensions = append(def.dimensions, string(dim))
			def.dimensionNames = append(def.dimensionNames, string(dim))
		}
		out = append(out, def)
	}
	return out
}

// parseRangeQuery 解析 from/to 查询参数为逐日闭区间(YYYY-MM-DD 列表),
// 并带回解析后的 fromT/toT。缺省 to=今天、from=to-29 天(共 30 天);校验:
// 格式严格 YYYY-MM-DD、from<=to、跨度(含两端)不超过 rangeDaysLimit。违规
// 返回 400 双语错误。前端不设置日期边界,未来范围原样进入本函数并返回
// 结构完整的零值载荷。
func parseRangeQuery(r *http.Request, now time.Time) (dates []string, from, to string, fromT, toT time.Time, perr *apiError) {
	q := r.URL.Query()
	from, to = q.Get("from"), q.Get("to")
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if from == "" {
		from = now.AddDate(0, 0, -29).Format("2006-01-02")
	}
	fromT, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, "", "", time.Time{}, time.Time{}, &apiError{
			status: http.StatusBadRequest,
			en:     fmt.Sprintf("invalid from %q: expected YYYY-MM-DD", from),
			zh:     fmt.Sprintf("无效的 from %q：应为 YYYY-MM-DD", from),
		}
	}
	toT, err = time.Parse("2006-01-02", to)
	if err != nil {
		return nil, "", "", time.Time{}, time.Time{}, &apiError{
			status: http.StatusBadRequest,
			en:     fmt.Sprintf("invalid to %q: expected YYYY-MM-DD", to),
			zh:     fmt.Sprintf("无效的 to %q：应为 YYYY-MM-DD", to),
		}
	}
	if toT.Before(fromT) {
		return nil, "", "", time.Time{}, time.Time{}, &apiError{
			status: http.StatusBadRequest,
			en:     fmt.Sprintf("from %s must not be after to %s", from, to),
			zh:     fmt.Sprintf("from %s 不能晚于 to %s", from, to),
		}
	}
	// 跨度按 AddDate 逐日计数(含两端),不用 time.Duration 换算
	// (约容 292 年,长区间会饱和折损实际天数)。
	days := 0
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		days++
	}
	if days > rangeDaysLimit {
		return nil, "", "", time.Time{}, time.Time{}, &apiError{
			status: http.StatusBadRequest,
			en:     fmt.Sprintf("range %s..%s spans %d days, exceeding the limit of %d days", from, to, days, rangeDaysLimit),
			zh:     fmt.Sprintf("区间 %s..%s 跨度 %d 天，超过 %d 天上限", from, to, days, rangeDaysLimit),
		}
	}
	dates = make([]string, 0, days)
	for d := fromT; !d.After(toT); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	return dates, from, to, fromT, toT, nil
}

// aliasesFor 与 cli 侧 dimensionAliases 同构:仅 provider 维度消费别名映射,
// 其余维度传 nil 保持各自语义。别名可能为 nil(未配置),聚合核对 nil map
// 读取安全,空值仍按「未归因」显示。
func aliasesFor(dim string, m map[string]string) map[string]string {
	if dim == "provider" {
		return m
	}
	return nil
}

// toDimensionRows 把聚合核的结构化行转换为 JSON 行(字段一一对应)。
func toDimensionRows(rows []querier.DimensionRow) []dimensionRowJSON {
	out := make([]dimensionRowJSON, 0, len(rows))
	for _, row := range rows {
		key := ""
		if len(row.Keys) > 0 {
			key = row.Keys[0]
		}
		out = append(out, dimensionRowJSON{
			Key:         key,
			Requests:    row.Agg.Requests,
			FreshInput:  row.Agg.FreshInput,
			Output:      row.Agg.OutputTokens,
			CacheRead:   row.Agg.CacheRead,
			CacheCreate: row.Agg.CacheCreate,
			Reasoning:   row.Agg.Reasoning,
			Total:       row.Agg.TotalTokens,
		})
	}
	return out
}

// toSessionRows 把会话行转换为 JSON 行(空切片保持 [],不序列化为 null)。
func toSessionRows(rows []querier.SessionRow) []sessionRowJSON {
	out := make([]sessionRowJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionRowJSON{
			Client:     row.Client,
			Project:    row.Project,
			Title:      row.Title,
			FirstTS:    row.FirstTS,
			LastTS:     row.LastTS,
			DurationMS: row.LastTS - row.FirstTS,
			Requests:   row.Agg.Requests,
			Total:      row.Agg.TotalTokens,
		})
	}
	return out
}
