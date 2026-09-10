package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/YuLaiZ/token-usage/internal/fmtx"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// rangeDaysLimit 是 from/to 区间的天数上限(含两端):与 cli 侧日期参数的
// 366 天上限同口径(恰好容纳一个闰年)。
const rangeDaysLimit = 366

// dashboardSessionLimit 是仪表板 Top sessions 的截断行数。
const dashboardSessionLimit = 10

// dashboardDimensions 是仪表板固定聚合的 8 个维度:顺序即聚合次序,JSON
// 键名与维度名一致(键序无关,前端按名取用)。
var dashboardDimensions = []string{"day", "hour", "weekday", "month", "client", "model", "provider", "project"}

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
type dimensionRowJSON struct {
	Key         string `json:"key"`
	Requests    int64  `json:"requests"`
	FreshInput  int64  `json:"fresh_input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheCreate int64  `json:"cache_create"`
	Reasoning   int64  `json:"reasoning"`
	Total       int64  `json:"total"`
}

// sessionRowJSON 是一条会话排行行;FirstTS/LastTS 为毫秒时间戳,
// DurationMS 为首末消息跨度,格式化交给前端。
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

// compareRowJSON 是环比对比表的预计算行:显示串与着色 class 由服务端按
// cli 侧共享格式化助手(fmtx)统一生成,前端零逻辑直绘。
type compareRowJSON struct {
	Label       string `json:"label"`
	Current     string `json:"current"`
	Base        string `json:"base"`
	Change      string `json:"change"`
	ChangeClass string `json:"change_class"`
	ChangePct   string `json:"change_pct"`
}

// compareDayJSON 是环比基线窗口的逐日行:与 dimensions.day 同构(缺口
// 填充至基线窗口长度),供前端绘制「当前区间 vs 基线窗口」逐日对比曲线。
type compareDayJSON struct {
	Key      string `json:"key"`
	Requests int64  `json:"requests"`
	Total    int64  `json:"total"`
}

// compareJSON 是环比对比区块:基线窗口、基线总量、8 行预计算对比行与基线
// 窗口逐日行(缺口填充,键即日期)。基线窗口无数据时各行为零值行照常返回
// (compare 恒填充,不序列化为 null)。
type compareJSON struct {
	BaseStart string           `json:"base_start"`
	BaseEnd   string           `json:"base_end"`
	Totals    totalsJSON       `json:"totals"`
	Rows      []compareRowJSON `json:"rows"`
	Daily     []compareDayJSON `json:"daily"`
}

// forecastRowJSON 是预估表的一行:显示串由服务端按 cli forecast 口径预
// 计算,前端零逻辑直绘;窗口无数据时数值四项均为 "—"。
type forecastRowJSON struct {
	Label    string `json:"label"`    // "Last 7 days / 最近 7 天"、"Last 30 days / 最近 30 天"
	Total    string `json:"total"`    // FormatTokens;无数据 "—"
	AvgDay   string `json:"avg_day"`  // FormatTokens(avg);无数据 "—"
	Active   string `json:"active"`   // "5/7"(ActiveDays/days);无数据 "—"
	Estimate string `json:"estimate"` // FormatTokens(avg*days);无数据 "—"
}

// forecastJSON 是预估区块:今日至今总量与恒 2 行(最近 7/30 天)回看窗口
// 行。窗口固定回看、不含今天,与仪表板 from/to 选区无关;与 compare 同理
// 恒填充,不序列化为 null。
type forecastJSON struct {
	TodaySoFar string            `json:"today_so_far"` // FormatTokens(今日至今总量)
	Rows       []forecastRowJSON `json:"rows"`         // 恒 2 行(7/30)
}

// heatmapJSON 是活动热力矩阵:Values 形状恒 7×24,行=星期(Weekdays 为双语
// 显示名,ISO 周序周一在首)、列=小时(Hours 为 "00:00".."23:00");
// Values[wi][hi] 为该交点的总 token 数,无数据交点为 0。数值矩阵交给前端
// 自绘,格式化与配色全部在展示层。
type heatmapJSON struct {
	Weekdays []string  `json:"weekdays"`
	Hours    []string  `json:"hours"`
	Values   [][]int64 `json:"values"`
}

// dashboardResponse 是 GET /api/dashboard 的载荷。dimensions 固定 8 键
// (day/hour/weekday/month/client/model/provider/project);compare、forecast、
// heatmap 恒存在(基线/回看窗口/热力矩阵零值也照常返回);columns 是 query
// 输出列布局的指标 ID 序列,供前端把指标条对齐 query 的可见列;图表不再
// 内嵌 SVG,前端消费 dimensions/heatmap 的数值行自绘,与 totals/sessions
// 同一读事务快照(encoding/json 对 map 按键排序输出,载荷确定)。
type dashboardResponse struct {
	Range      rangeJSON                     `json:"range"`
	Totals     totalsJSON                    `json:"totals"`
	Compare    compareJSON                   `json:"compare"`
	Forecast   forecastJSON                  `json:"forecast"`
	Columns    []string                      `json:"columns"`
	Dimensions map[string][]dimensionRowJSON `json:"dimensions"`
	Heatmap    heatmapJSON                   `json:"heatmap"`
	Sessions   []sessionRowJSON              `json:"sessions"`
}

// handleMeta 输出服务版本与全库数据边界:最小/最大日期、数据截至
// (全库最大消息时间)与最近一次成功采集时间。同一读事务内取齐,保证
// 边界三项互相一致。
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

// handleDashboard 输出统计区间内的汇总、8 个维度行、会话排行与活动热力
// 矩阵。全部查询在同一读事务内完成(同一 WAL 快照),并发采集写入下 totals、
// 各维度行、sessions 与热力矩阵互相一致;localhost 串行请求不要求并行取数。
// 图表由前端按本载荷的数值行自绘,刷新只需 dashboard+meta 两个请求。
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// 同一请求统一取一次时钟:区间缺省与预估窗口共用,防跨午夜瞬间口径错位。
	now := time.Now()
	dates, from, to, fromT, toT, perr := parseRangeQuery(r, now)
	if perr != nil {
		writeError(w, *perr)
		return
	}
	ctx := r.Context()
	resp := dashboardResponse{
		Range:      rangeJSON{From: from, To: to},
		Dimensions: make(map[string][]dimensionRowJSON, len(dashboardDimensions)),
		Sessions:   []sessionRowJSON{},
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
		// 输出列布局是 querier 状态,与数据无关,但仍在同一事务回调内读取。
		resp.Columns = tq.OutputColumnIDs()
		// 环比对比与 totals 同快照取基线,两窗口数据互相一致。
		compare, err := buildCompareJSON(ctx, tq, fromT, toT, stats)
		if err != nil {
			return err
		}
		resp.Compare = compare
		// 预估区块与 cli forecast 同窗口同口径:固定回看窗口、与 from/to
		// 选区无关;同一读事务内取数,与 totals/compare 同快照一致。
		fc, err := buildForecastJSON(ctx, tq, now)
		if err != nil {
			return err
		}
		resp.Forecast = fc
		// 维度循环只产出 JSON 行;数值行交给前端自绘图表,区间汇总由
		// AggregateDimensionView 随行返回,这里不再单独消费。
		for _, dim := range dashboardDimensions {
			rows, _, err := tq.AggregateDimensionView(ctx, dates, querier.DimensionView{
				Dimensions: []string{dim},
				Aliases:    aliasesFor(dim, s.providerAliases),
				TitleEn:    "dashboard", TitleZh: "dashboard",
			})
			if err != nil {
				return err
			}
			resp.Dimensions[dim] = toDimensionRows(rows)
		}
		sessions, err := tq.SessionRows(ctx, dates)
		if err != nil {
			return err
		}
		// 会话排行按总量排序后截前 10 行。
		resp.Sessions = toSessionRows(querier.TruncateTopRows(querier.SortTopRows(sessions), dashboardSessionLimit))
		// 活动热力矩阵在同一事务内取数,与 totals/dimensions/sessions 同快照。
		hm, err := tq.HeatmapMatrix(ctx, dates)
		if err != nil {
			return err
		}
		resp.Heatmap = heatmapJSON{Weekdays: hm.Weekdays, Hours: hm.Hours, Values: hm.Values}
		return nil
	})
	if err != nil {
		writeInternal(w, "dashboard query failed", "仪表板查询失败", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseRangeQuery 解析 from/to 查询参数为逐日闭区间(YYYY-MM-DD 列表),
// 并带回解析后的 fromT/toT(供环比对比推导基线窗口)。缺省 to=今天、
// from=to-29 天(共 30 天);校验:格式严格 YYYY-MM-DD、from<=to、跨度
// (含两端)不超过 rangeDaysLimit。违规返回 400 双语错误。
// /api/dashboard 与 /api/chart 共用本函数,保证两个接口的日期口径一致。
// now 由调用方传入:同一请求的区间缺省与预估窗口必须取同一时钟,防止本地
// 午夜瞬间两次 time.Now() 各落一天,导致 forecast 的「今天」与 range 缺省
// 口径错位。
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

// buildCompareJSON 推导环比基线窗口、取基线总量并预构造 8 行对比行。基线
// 窗口经 querier.CompareBaseWindow 区间模式推导(等长窗口结束于开始日前
// 一天;from==to 的单日区间自然退化为前一天,与单日粒度结果一致)。
// 显示串与着色 class 走 fmtx 共享助手(计数行千分位、token 行 K/M/B 缩写、
// 基线为 0 时变化% 为 "--"),前端零逻辑直绘。
func buildCompareJSON(ctx context.Context, tq *querier.Querier, fromT, toT time.Time, cur querier.RangeStats) (compareJSON, error) {
	baseStartT, baseEndT := querier.CompareBaseWindow(fromT, toT, 0)
	baseStats, err := tq.StatsBetween(ctx, baseStartT.Format("2006-01-02"), baseEndT.Format("2006-01-02"))
	if err != nil {
		return compareJSON{}, err
	}
	// 基线窗口逐日行(单维时间视图缺口填充至窗口长度),供前端绘制两期
	// 逐日对比曲线;与基线总量同一读事务快照。
	baseDates := make([]string, 0, 8)
	for d := baseStartT; !d.After(baseEndT); d = d.AddDate(0, 0, 1) {
		baseDates = append(baseDates, d.Format("2006-01-02"))
	}
	baseDailyRows, _, err := tq.AggregateDimensionView(ctx, baseDates, querier.DimensionView{
		Dimensions: []string{"day"},
		TitleEn:    "dashboard", TitleZh: "dashboard",
	})
	if err != nil {
		return compareJSON{}, err
	}
	baseDaily := make([]compareDayJSON, 0, len(baseDailyRows))
	for _, row := range baseDailyRows {
		key := ""
		if len(row.Keys) > 0 {
			key = row.Keys[0]
		}
		baseDaily = append(baseDaily, compareDayJSON{Key: key, Requests: row.Agg.Requests, Total: row.Agg.TotalTokens})
	}
	c := compareJSON{
		BaseStart: baseStartT.Format("2006-01-02"),
		BaseEnd:   baseEndT.Format("2006-01-02"),
		Totals: totalsJSON{
			Requests:    baseStats.Total.Requests,
			FreshInput:  baseStats.Total.FreshInput,
			Output:      baseStats.Total.OutputTokens,
			CacheRead:   baseStats.Total.CacheRead,
			CacheCreate: baseStats.Total.CacheCreate,
			Reasoning:   baseStats.Total.Reasoning,
			Total:       baseStats.Total.TotalTokens,
			ActiveDays:  baseStats.ActiveDays,
		},
		Rows:  make([]compareRowJSON, 0, 8),
		Daily: baseDaily,
	}
	countRow := func(label string, curV, baseV int64) {
		c.Rows = append(c.Rows, compareRowJSON{
			Label:       label,
			Current:     fmtx.Thousands(curV),
			Base:        fmtx.Thousands(baseV),
			Change:      fmtx.CountChange(curV - baseV),
			ChangeClass: fmtx.ChangeClass(curV - baseV),
			ChangePct:   fmtx.ChangePercent(curV, baseV),
		})
	}
	tokenRow := func(label string, curV, baseV int64) {
		c.Rows = append(c.Rows, compareRowJSON{
			Label:       label,
			Current:     querier.FormatTokens(curV),
			Base:        querier.FormatTokens(baseV),
			Change:      fmtx.SignedTokens(curV - baseV),
			ChangeClass: fmtx.ChangeClass(curV - baseV),
			ChangePct:   fmtx.ChangePercent(curV, baseV),
		})
	}
	countRow(ui.Bi("Active days", "活跃天"), cur.ActiveDays, baseStats.ActiveDays)
	countRow(ui.ColRequests, cur.Total.Requests, baseStats.Total.Requests)
	tokenRow(ui.ColInput, cur.Total.FreshInput, baseStats.Total.FreshInput)
	tokenRow(ui.ColOutput, cur.Total.OutputTokens, baseStats.Total.OutputTokens)
	tokenRow(ui.ColCacheRead, cur.Total.CacheRead, baseStats.Total.CacheRead)
	tokenRow(ui.ColCacheCreate, cur.Total.CacheCreate, baseStats.Total.CacheCreate)
	tokenRow(ui.ColReasoning, cur.Total.Reasoning, baseStats.Total.Reasoning)
	tokenRow(ui.ColTotal, cur.Total.TotalTokens, baseStats.Total.TotalTokens)
	return c, nil
}

// buildForecastJSON 构造预测面板数据:today so far 取今天区间(now 由
// handler 在请求入口统一取);回看窗口
// last7=[今天-7, 今天-1]、last30=[今天-30, 今天-1] 均不含今天(今天尚未
// 结束,计入会低估日均)。avg = TotalTokens/ActiveDays 整数除法,预估 =
// avg×未来天数(假设未来保持同等活跃强度)。窗口固定回看,与仪表板
// from/to 选区无关。
func buildForecastJSON(ctx context.Context, tq *querier.Querier, now time.Time) (forecastJSON, error) {
	day := func(offset int) string { return now.AddDate(0, 0, offset).Format("2006-01-02") }
	todayStats, err := tq.StatsBetween(ctx, day(0), day(0))
	if err != nil {
		return forecastJSON{}, err
	}
	last7, err := tq.StatsBetween(ctx, day(-7), day(-1))
	if err != nil {
		return forecastJSON{}, err
	}
	last30, err := tq.StatsBetween(ctx, day(-30), day(-1))
	if err != nil {
		return forecastJSON{}, err
	}
	f := forecastJSON{
		TodaySoFar: querier.FormatTokens(todayStats.Total.TotalTokens),
		Rows:       make([]forecastRowJSON, 0, 2),
	}
	f.Rows = append(f.Rows,
		forecastRow(ui.Bi("Last 7 days", "最近 7 天"), 7, last7),
		forecastRow(ui.Bi("Last 30 days", "最近 30 天"), 30, last30))
	return f, nil
}

// forecastRow 按一个回看窗口预构造预估行:总量、日均(整数除法)、活跃天
// 占比与预估,与 cli 侧 writeWindowLine/writeProjectionLine 同口径;无数据
// 判定同为 ActiveDays==0。CLI 在窗口无数据时省略对应预测行,表格形态恒
// 2 行,以 "—" 表达同一「无预测依据」语义。
func forecastRow(label string, days int, s querier.RangeStats) forecastRowJSON {
	if s.ActiveDays == 0 {
		return forecastRowJSON{Label: label, Total: "—", AvgDay: "—", Active: "—", Estimate: "—"}
	}
	avg := s.Total.TotalTokens / s.ActiveDays
	return forecastRowJSON{
		Label:    label,
		Total:    querier.FormatTokens(s.Total.TotalTokens),
		AvgDay:   querier.FormatTokens(avg),
		Active:   fmt.Sprintf("%d/%d", s.ActiveDays, days),
		Estimate: querier.FormatTokens(avg * int64(days)),
	}
}
