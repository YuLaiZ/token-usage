package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// serveFixture 是内存库夹具的期望值集合:由构造函数一并算好,测试逐项断言。
type serveFixture struct {
	minDate  string
	maxDate  string
	dataThru string // data_through 的 time.DateTime 本地形态
	totals   totalsJSON
	todayRow dimensionRowJSON // client 维度中 claude-code 的行
	topTotal int64            // 会话排行首行 total
}

// newTestServer 构造内存库 + 夹具数据 + NewServer(版本串 "test-version")。
// 数据形态:今天 12 个会话各 1 条消息(会话 i 总量 i*100),其中 s12 追加一条
// 一小时后 +200 的消息(覆盖时长与首末时间戳);5 天前 1 条 999 的消息
// (跨日的 min_date 与两日热力行)。全部时间戳按 time.Local 构造,与
// 聚合核的 hour 本机时分桶同口径。
func newTestServer(t *testing.T, opts ...ServerOption) (http.Handler, serveFixture) {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")
	old := today.AddDate(0, 0, -5)
	oldDate := old.Format("2006-01-02")

	var msgs []model.Message
	var sessions []model.Session
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("s%02d", i)
		ts := today.Add(time.Duration(i) * time.Minute)
		msgs = append(msgs, model.Message{
			ID: id + "-m1", SessionID: id, Client: model.ClientClaudeCode,
			Date: todayDate, TS: ts.UnixMilli(), Model: "model-x", Project: "proj-" + id,
			TotalTokens: int64(i) * 100,
		})
		if i == 12 {
			// s12 的第二条消息:一小时后 +200,令该会话具备非零时长与首末时间戳。
			msgs = append(msgs, model.Message{
				ID: id + "-m2", SessionID: id, Client: model.ClientClaudeCode,
				Date: todayDate, TS: ts.Add(time.Hour).UnixMilli(), Model: "model-x", Project: "proj-" + id,
				TotalTokens: 200,
			})
		}
		sessions = append(sessions, model.Session{
			ID: id, Client: model.ClientClaudeCode, Project: "proj-" + id,
			Title: "session-" + id, FirstTS: ts.UnixMilli(),
			LastTS: ts.UnixMilli(),
		})
	}
	// s12 的会话元数据补最末时间戳(首条 10:12,末条 11:12)。
	lastTS := today.Add(12*time.Minute + time.Hour).UnixMilli()
	sessions[11].LastTS = lastTS
	msgs = append(msgs, model.Message{
		ID: "s-old-m1", SessionID: "s-old", Client: model.ClientClaudeCode,
		Date: oldDate, TS: old.UnixMilli(), Model: "model-x", Project: "proj-old",
		TotalTokens: 999,
	})
	sessions = append(sessions, model.Session{
		ID: "s-old", Client: model.ClientClaudeCode, Project: "proj-old",
		Title: "session-old", FirstTS: old.UnixMilli(), LastTS: old.UnixMilli(),
	})

	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertSessionMeta(ctx, usageDB, sessions); err != nil {
		t.Fatal(err)
	}

	// 期望值与夹具同源推导。
	var reqTotal, tokTotal int64
	for _, m := range msgs {
		reqTotal++
		tokTotal += m.TotalTokens
	}
	fx := serveFixture{
		minDate:  oldDate,
		maxDate:  todayDate,
		dataThru: time.UnixMilli(lastTS).Local().Format(time.DateTime),
		totals: totalsJSON{
			Requests: reqTotal, Total: tokTotal, ActiveDays: 2,
		},
		todayRow: dimensionRowJSON{Key: model.ClientClaudeCode, Requests: reqTotal, Total: tokTotal},
		topTotal: 1400, // s12:1200+200
	}
	return NewServer(querier.New(usageDB), "test-version", opts...), fx
}

// testConfig 返回注入 server 的最小配置:subqueries 定义 mpc 视图
// (model,provider 组合)与一条 provider 别名,供 custom views 与别名合并测试。
func testConfig() *config.Config {
	return &config.Config{
		ProviderAliases: map[string]string{"vendor-a": "merged-vendor"},
		RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
		},
	}
}

// doGet 执行一次 GET 并返回记录器。
func doGet(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// decodeJSON 把响应体反序列化为 map(键集合断言用)。
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应应为合法 JSON: %v:\n%s", err, rec.Body.String())
	}
	return m
}

// assertKeys 断言 map 的键集合与期望完全一致(字段名精确匹配)。
func assertKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	wantSet := map[string]bool{}
	for _, k := range want {
		wantSet[k] = true
	}
	if !reflect.DeepEqual(keySet(m), wantSet) {
		t.Errorf("%s 字段名应恰为 %v,实际 %v", what, want, got)
	}
}

func keySet(m map[string]any) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// TestServeMeta:/api/meta 透出版本串与全库数据边界(最小/最大日期、数据截至),
// 无采集记录时 last_collection 为 null。版本串按调用方传入值原样返回,
// 不做归一化(devdashboard 传 buildinfo.Info.Version 语义)。
func TestServeMeta(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/meta")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type 应为 JSON,实际 %q", ct)
	}
	m := decodeJSON(t, rec)
	assertKeys(t, "meta", m, "version", "min_date", "max_date", "data_through", "last_collection")
	if m["version"] != "test-version" {
		t.Errorf("version 应为 test-version,实际 %v", m["version"])
	}
	if m["min_date"] != fx.minDate || m["max_date"] != fx.maxDate {
		t.Errorf("数据边界应为 %s..%s,实际 %v..%v", fx.minDate, fx.maxDate, m["min_date"], m["max_date"])
	}
	if m["data_through"] != fx.dataThru {
		t.Errorf("data_through 应为 %q,实际 %v", fx.dataThru, m["data_through"])
	}
	if m["last_collection"] != nil {
		t.Errorf("无采集记录时 last_collection 应为 null,实际 %v", m["last_collection"])
	}
}

// TestServeDashboard_ShapeAndOrder:/api/dashboard 默认范围含今天;顶层与
// 各嵌套对象的字段名精确匹配(载荷 v4:无 compare/forecast,新增 custom_views
// 与 trend,dimensions 固定四维度,各维度返回最终行——夹具独立项不足上限,
// 不生成尾行);client 维度按总量降序;sessions 按总量降序且上限 20(夹具
// 13 个会话全部返回);totals 数值精确。map 反序列化与结构体反序列化双检。
func TestServeDashboard_ShapeAndOrder(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}

	// 第一检:map 键集合精确匹配(多字段/少字段/拼错均失败)。
	m := decodeJSON(t, rec)
	assertKeys(t, "dashboard 顶层", m, "range", "totals", "columns", "dimensions", "trend", "heatmap", "custom_views", "sessions")
	assertKeys(t, "range", m["range"].(map[string]any), "from", "to")
	assertKeys(t, "totals", m["totals"].(map[string]any),
		"requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total", "active_days")
	assertKeys(t, "trend", m["trend"].(map[string]any), "granularity", "buckets", "peak_index", "peak_total", "empty")
	assertKeys(t, "sessions[0]", m["sessions"].([]any)[0].(map[string]any),
		"client", "project", "title", "first_ts", "last_ts", "duration_ms", "requests", "total")
	dims := m["dimensions"].(map[string]any)
	if !reflect.DeepEqual(keySet(dims), keySet(map[string]any{
		"client": nil, "model": nil, "provider": nil, "project": nil,
	})) {
		t.Errorf("dimensions 应恰 4 个固定键,实际 %v", dims)
	}
	// 独立行不得携带 is_other/other_count(omitempty 键缺失,前端据机器字段识别尾行)。
	assertKeys(t, "dimensions.client[0]", dims["client"].([]any)[0].(map[string]any),
		"key", "requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total",
		"duration_ms_sum", "duration_count", "duration_output_sum", "avg_duration_ms", "speed_tok_s")
	// 无配置注入时 custom_views 为空数组(不是 null)。
	if cv, ok := m["custom_views"].([]any); !ok || len(cv) != 0 {
		t.Errorf("无配置时 custom_views 应为空数组,实际 %v", m["custom_views"])
	}

	// 第二检:结构体反序列化校验数值与排序。
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	if resp.Range.To != fx.maxDate {
		t.Errorf("默认范围终点应为今天 %s,实际 %s", fx.maxDate, resp.Range.To)
	}
	// 默认范围起点 = to-29 天(共 30 天)。
	toT, err := time.Parse("2006-01-02", resp.Range.To)
	if err != nil {
		t.Fatalf("range.to 应为 YYYY-MM-DD: %v", err)
	}
	if wantFrom := toT.AddDate(0, 0, -29).Format("2006-01-02"); resp.Range.From != wantFrom {
		t.Errorf("默认范围起点应为 to-29 天(%s),实际 %s", wantFrom, resp.Range.From)
	}
	if resp.Totals != fx.totals {
		t.Errorf("totals 应精确匹配:\n期望 %+v\n实际 %+v", fx.totals, resp.Totals)
	}
	// client 维度:单行(claude-code),数值为全区间汇总。
	clientRows := resp.Dimensions["client"]
	if len(clientRows) != 1 {
		t.Fatalf("client 维度应 1 行,实际 %d", len(clientRows))
	}
	if clientRows[0] != fx.todayRow {
		t.Errorf("client 行应 %+v,实际 %+v", fx.todayRow, clientRows[0])
	}
	// columns 恒为 querier 布局的指标 ID 序列:夹具走 New 的默认布局,
	// 与 ui.DefaultOutputColumns 同序同值(七列,不含 cache_create)。
	if want := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"}; !reflect.DeepEqual(resp.Columns, want) {
		t.Errorf("columns 应为默认七列 %v,实际 %v", want, resp.Columns)
	}
	// sessions:总量降序、上限 20(夹具 13 个会话全部返回)、首行即最重会话。
	sessions := resp.Sessions
	if len(sessions) != 13 {
		t.Fatalf("sessions 应返回全部 13 个会话(上限 20),实际 %d", len(sessions))
	}
	if sessions[0].Total != fx.topTotal {
		t.Errorf("排行首行 total 应 %d,实际 %d", fx.topTotal, sessions[0].Total)
	}
	for i := 1; i < len(sessions); i++ {
		if sessions[i-1].Total < sessions[i].Total {
			t.Errorf("sessions 应按 total 降序,位置 %d 出现 %d < %d", i, sessions[i-1].Total, sessions[i].Total)
		}
	}
	// s12:两条消息相距 1 小时,duration_ms 恰 3600000。
	if sessions[0].DurationMS != 3600000 {
		t.Errorf("首行会话时长应 3600000ms,实际 %d", sessions[0].DurationMS)
	}
	if sessions[0].Requests != 2 {
		t.Errorf("首行会话请求数应 2,实际 %d", sessions[0].Requests)
	}
	// 会话标题为数据源原文,不得清洗或重写。
	if sessions[0].Title != "session-s12" {
		t.Errorf("会话标题应保留原文,实际 %q", sessions[0].Title)
	}
	// 趋势:默认 30 天为逐日桶;峰值=今日(7800+200=8000 那天),空态 false。
	if resp.Trend.Granularity != "day" {
		t.Errorf("默认区间趋势粒度应为 day,实际 %q", resp.Trend.Granularity)
	}
	if len(resp.Trend.Buckets) != 30 {
		t.Fatalf("默认区间趋势应 30 个日桶,实际 %d", len(resp.Trend.Buckets))
	}
	if resp.Trend.Empty {
		t.Errorf("有数据区间 trend.empty 应为 false")
	}
	if resp.Trend.PeakTotal != fx.totals.Total-999 {
		t.Errorf("趋势峰值应为今日总量 %d,实际 %d", fx.totals.Total-999, resp.Trend.PeakTotal)
	}
	if resp.Trend.Buckets[resp.Trend.PeakIndex].Key != fx.maxDate {
		t.Errorf("峰值桶应为今日 %s,实际 %+v", fx.maxDate, resp.Trend.Buckets[resp.Trend.PeakIndex])
	}
}

// TestServeDashboard_HeatmapDayHour:默认 30 天范围走 day_block 模式——
// 后端按「日期 × 4 小时时段」返回 6 个展示格(由逐小时数据在后端求和,
// 前端不再做跨时段合并);days 行数恰为范围天数(含零值日,升序);
// 过去/今日的日期不带 future 键(omitempty,已发生的零值是真实零格);
// 数值与 totals 同一读事务快照:今天 12 条消息(10:01..10:12)与 s12 第二条
// (11:12)都落在 08–12 时段共 8000,5 天前消息(10:00)落在 08–12 时段 999;
// 全部行总和恰等于 totals.Total。
func TestServeDashboard_HeatmapDayHour(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}

	// 第一检:map 键集合精确匹配。30 天范围内全部已发生,无 future 键。
	hm := decodeJSON(t, rec)["heatmap"].(map[string]any)
	assertKeys(t, "heatmap", hm, "from", "to", "mode", "days")
	if hm["mode"] != "day_block" {
		t.Errorf("30 天范围热力模式应为 day_block,实际 %v", hm["mode"])
	}
	assertKeys(t, "heatmap.days[0]", hm["days"].([]any)[0].(map[string]any), "date", "total", "blocks")

	// 第二检:结构体反序列化校验形状与数值。
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	hj := resp.Heatmap
	// 默认范围 30 天:days 恰 30 行、日期升序、from/to 与 range 一致。
	if len(hj.Days) != 30 {
		t.Fatalf("热力 days 应按默认区间补零至 30 行,实际 %d", len(hj.Days))
	}
	if hj.From != resp.Range.From || hj.To != resp.Range.To {
		t.Errorf("heatmap.from/to 应与 range 一致(%s..%s),实际 %s..%s",
			resp.Range.From, resp.Range.To, hj.From, hj.To)
	}
	for i := 1; i < len(hj.Days); i++ {
		if hj.Days[i-1].Date >= hj.Days[i].Date {
			t.Fatalf("热力 days 日期应严格升序,位置 %d 出现 %s >= %s", i, hj.Days[i-1].Date, hj.Days[i].Date)
		}
	}
	if hj.Days[0].Date != resp.Range.From || hj.Days[29].Date != resp.Range.To {
		t.Errorf("热力 days 首末行应为范围端点,实际 %s..%s", hj.Days[0].Date, hj.Days[29].Date)
	}
	for i, day := range hj.Days {
		if len(day.Blocks) != 6 {
			t.Fatalf("热力第 %d 行应 6 个 4 小时时段格,实际 %d", i, len(day.Blocks))
		}
		if day.Future {
			t.Errorf("已发生日期 %s 不得标记 future", day.Date)
		}
		if len(day.Hours) != 0 {
			t.Errorf("day_block 模式不得携带 hours 键,日期 %s 实际 %d 格", day.Date, len(day.Hours))
		}
	}
	// 桶值:今天 08–12 时段 = 7800+200,5 天前 08–12 时段 = 999。
	todayRow := hj.Days[29]
	if todayRow.Date != fx.maxDate || todayRow.Total != fx.totals.Total-999 {
		t.Errorf("今日行应为 date=%s total=%d,实际 %+v", fx.maxDate, fx.totals.Total-999, todayRow)
	}
	if todayRow.Blocks[2] != 8000 {
		t.Errorf("今日 08–12 时段应 8000,实际 %v", todayRow.Blocks)
	}
	oldRow := hj.Days[24] // 5 天前
	if oldRow.Date != fx.minDate || oldRow.Total != 999 || oldRow.Blocks[2] != 999 {
		t.Errorf("5 天前行应为 date=%s total=999(08–12 时段),实际 %+v", fx.minDate, oldRow)
	}
	// 其余行全零。
	var sum int64
	for _, day := range hj.Days {
		for _, v := range day.Blocks {
			sum += v
		}
		if day.Date != fx.maxDate && day.Date != fx.minDate && day.Total != 0 {
			t.Errorf("无数据日 %s 的 total 应为 0,实际 %d", day.Date, day.Total)
		}
	}
	// 同源一致性:全矩阵总和恰等于区间 totals.Total(同一读事务快照)。
	if sum != fx.totals.Total {
		t.Errorf("热力矩阵总和应等于 totals.Total %d,实际 %d", fx.totals.Total, sum)
	}
	// 趋势与热力同源:趋势日桶合计同样等于 totals.Total。
	var trendSum int64
	for _, b := range resp.Trend.Buckets {
		trendSum += b.Total
	}
	if trendSum != fx.totals.Total {
		t.Errorf("趋势桶合计应等于 totals.Total %d,实际 %d", fx.totals.Total, trendSum)
	}
}

// TestServeDashboard_HeatmapSingleDay:单日范围(from==to)走 day_hour 模式,
// days 恰 1 行、24 个小时格;今天(本机今天)不带 future 键。
func TestServeDashboard_HeatmapSingleDay(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/dashboard?from="+fx.maxDate+"&to="+fx.maxDate)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	if resp.Heatmap.Mode != "day_hour" {
		t.Errorf("单日范围热力模式应为 day_hour,实际 %q", resp.Heatmap.Mode)
	}
	if len(resp.Heatmap.Days) != 1 {
		t.Fatalf("单日范围应恰 1 行,实际 %d", len(resp.Heatmap.Days))
	}
	day := resp.Heatmap.Days[0]
	if day.Date != fx.maxDate {
		t.Errorf("单日行日期应为 %s,实际 %s", fx.maxDate, day.Date)
	}
	if len(day.Hours) != 24 {
		t.Fatalf("单日行应 24 个小时格,实际 %d", len(day.Hours))
	}
	if day.Future {
		t.Errorf("本机今天不得标记 future(已发生,零格语义)")
	}
	var sum int64
	for _, v := range day.Hours {
		sum += v
	}
	// 当日小时总和恰为当日总量(8999 中 999 属于 5 天前,不在单日范围内)。
	if want := fx.totals.Total - 999; sum != want {
		t.Errorf("单日小时总和应等于当日总量 %d,实际 %d", want, sum)
	}
	// 趋势同区间为小时桶:24 桶、粒度 hour、峰值=10 点桶(7800)。
	if resp.Trend.Granularity != "hour" || len(resp.Trend.Buckets) != 24 {
		t.Fatalf("单日趋势应为 24 个小时桶,实际粒度 %q、%d 桶", resp.Trend.Granularity, len(resp.Trend.Buckets))
	}
	if resp.Trend.PeakTotal != 7800 || resp.Trend.Buckets[resp.Trend.PeakIndex].Key != "10" {
		t.Errorf("单日趋势峰值应为 10 点桶 7800,实际 index=%d total=%d",
			resp.Trend.PeakIndex, resp.Trend.PeakTotal)
	}
}

// TestServeDashboard_CustomViews:配置提供者注入 subqueries 后,/api/dashboard
// 返回按配置维度顺序聚合的自定义视图行;行数值与区间汇总逐项守恒(七项合计
// 等于 totals);provider 维度消费别名合并。
func TestServeDashboard_CustomViews(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	msgs := []model.Message{
		{ID: "cv-a", SessionID: "s1", Client: model.ClientClaudeCode, Date: todayDate,
			TS: today.UnixMilli(), Model: "model-x", Provider: "vendor-a", TotalTokens: 100},
		{ID: "cv-b", SessionID: "s2", Client: model.ClientClaudeCode, Date: todayDate,
			TS: today.UnixMilli(), Model: "model-y", Provider: "vendor-a", TotalTokens: 200},
		{ID: "cv-c", SessionID: "s3", Client: model.ClientClaudeCode, Date: todayDate,
			TS: today.UnixMilli(), Model: "model-x", Provider: "vendor-b", TotalTokens: 400},
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ProviderAliases: map[string]string{"vendor-a": "merged-vendor", "vendor-b": "merged-vendor"},
		RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
		},
	}
	h := NewServer(querier.New(usageDB), "test-version", WithConfigProvider(func() *config.Config { return cfg }))

	rec := doGet(h, "/api/dashboard?from="+todayDate+"&to="+todayDate)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	// 第一检:custom_views 行键集合精确。
	cv := decodeJSON(t, rec)["custom_views"].([]any)
	if len(cv) != 1 {
		t.Fatalf("应恰 1 个自定义视图,实际 %d", len(cv))
	}
	view := cv[0].(map[string]any)
	assertKeys(t, "custom_views[0]", view, "name", "dimensions", "rows")
	assertKeys(t, "custom_views[0].rows[0]", view["rows"].([]any)[0].(map[string]any),
		"keys", "requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total",
		"duration_ms_sum", "duration_count", "duration_output_sum", "avg_duration_ms", "speed_tok_s")

	// 第二检:结构体反序列化校验语义。
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	if len(resp.CustomViews) != 1 {
		t.Fatalf("应恰 1 个自定义视图,实际 %d", len(resp.CustomViews))
	}
	v := resp.CustomViews[0]
	if v.Name != "mpc" {
		t.Errorf("视图名应为 mpc,实际 %q", v.Name)
	}
	if want := []string{"model", "provider"}; !reflect.DeepEqual(v.Dimensions, want) {
		t.Errorf("视图维度应按配置顺序 %v,实际 %v", want, v.Dimensions)
	}
	// vendor-a/vendor-b 合并后恰 2 组合:model-x+merged-vendor(500)、
	// model-y+merged-vendor(200)。
	if len(v.Rows) != 2 {
		t.Fatalf("合并别名后应 2 行,实际 %d:\n%+v", len(v.Rows), v.Rows)
	}
	byModel := map[string]customViewRowJSON{}
	for _, row := range v.Rows {
		if len(row.Keys) != 2 {
			t.Fatalf("组合行 keys 应 2 元,实际 %v", row.Keys)
		}
		byModel[row.Keys[0]] = row
		if row.Keys[1] != "merged-vendor" {
			t.Errorf("provider 显示键应合并别名,实际 %q", row.Keys[1])
		}
	}
	if r := byModel["MODEL-X"]; r.Total != 500 || r.Requests != 2 {
		t.Errorf("model-x 行应为 total=500/requests=2,实际 %+v", r)
	}
	if r := byModel["MODEL-Y"]; r.Total != 200 || r.Requests != 1 {
		t.Errorf("model-y 行应为 total=200/requests=1,实际 %+v", r)
	}
	// 守恒:七项逐项合计等于区间 totals(同一读事务、同一选区聚合)。
	var sums dimensionRowJSON
	for _, row := range v.Rows {
		sums.Requests += row.Requests
		sums.FreshInput += row.FreshInput
		sums.Output += row.Output
		sums.CacheRead += row.CacheRead
		sums.CacheCreate += row.CacheCreate
		sums.Reasoning += row.Reasoning
		sums.Total += row.Total
	}
	if sums.Requests != resp.Totals.Requests || sums.Total != resp.Totals.Total {
		t.Errorf("自定义视图行合计应守恒:requests %d vs %d,total %d vs %d",
			sums.Requests, resp.Totals.Requests, sums.Total, resp.Totals.Total)
	}
}

// TestServeDashboard_FutureRange:前端不限制日期,未来范围由后端返回结构
// 完整的零值(200):totals 全 0、趋势空态(peak=-1)、热力 days 恰为范围
// 天数且全零、全部日期标记 future(尚未发生→空白格,与真实零格区分)、
// 会话为空数组、四维度行与自定义视图行为空。
func TestServeDashboard_FutureRange(t *testing.T) {
	h, _ := newTestServer(t)
	rec := doGet(h, "/api/dashboard?from=2027-01-01&to=2027-01-31")
	if rec.Code != http.StatusOK {
		t.Fatalf("未来范围应 200(不伪装成错误),实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	assertKeys(t, "dashboard 顶层", m, "range", "totals", "columns", "dimensions", "trend", "heatmap", "custom_views", "sessions")
	if got := m["range"].(map[string]any); got["from"] != "2027-01-01" || got["to"] != "2027-01-31" {
		t.Errorf("range 应原样返回未来范围,实际 %v", got)
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	if resp.Totals != (totalsJSON{}) {
		t.Errorf("未来范围 totals 应全 0,实际 %+v", resp.Totals)
	}
	// 趋势:31 天逐日桶非空但全零 → empty=true、peak=-1,不产生"峰值 0"。
	if !resp.Trend.Empty || resp.Trend.PeakIndex != -1 || resp.Trend.PeakTotal != 0 {
		t.Errorf("未来范围趋势应为空态(empty/peak=-1),实际 %+v", resp.Trend)
	}
	if len(resp.Trend.Buckets) != 31 {
		t.Errorf("未来范围趋势仍应按日给出 31 个零值桶,实际 %d", len(resp.Trend.Buckets))
	}
	if len(resp.Heatmap.Days) != 31 {
		t.Fatalf("未来范围热力 days 应补零至 31 行,实际 %d", len(resp.Heatmap.Days))
	}
	for _, day := range resp.Heatmap.Days {
		if day.Total != 0 {
			t.Errorf("未来日 %s 应为 0,实际 %d", day.Date, day.Total)
		}
		if !day.Future {
			t.Errorf("未来日 %s 必须标记 future(尚未发生→空白格)", day.Date)
		}
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("未来范围会话应为空数组,实际 %+v", resp.Sessions)
	}
	for dim, rows := range resp.Dimensions {
		if len(rows) != 0 {
			t.Errorf("未来范围 %s 维度应为空,实际 %+v", dim, rows)
		}
	}
}

// TestServeDashboard_BadParams:/api/dashboard 的 400 场景(格式错、from>to、
// 跨度超 366 天),错误 message 非空。
func TestServeDashboard_BadParams(t *testing.T) {
	h, _ := newTestServer(t)
	cases := []struct {
		name   string
		target string
	}{
		{"格式错", "/api/dashboard?from=2026-9-1&to=2026-09-30"},
		{"from晚于to", "/api/dashboard?from=2026-09-10&to=2026-09-01"},
		{"跨度367天", "/api/dashboard?from=2025-01-01&to=2026-01-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(h, tc.target)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400,实际 %d:\n%s", rec.Code, rec.Body.String())
			}
			m := decodeJSON(t, rec)
			errObj, ok := m["error"].(map[string]any)
			if !ok {
				t.Fatalf("错误响应应为 {\"error\":{...}} 形态:\n%s", rec.Body.String())
			}
			msg, _ := errObj["message"].(string)
			if strings.TrimSpace(msg) == "" {
				t.Errorf("错误 message 应非空:\n%s", rec.Body.String())
			}
		})
	}
}

// TestServeDashboard_ColumnsFollowConfig:columns 每请求从当前配置解析,
// 不依赖服务启动时的布局——配置提供者返回新列后,下一次请求即生效;
// query 段顶层问题态回退默认七列。
func TestServeDashboard_ColumnsFollowConfig(t *testing.T) {
	h, _ := newTestServer(t)
	// 无配置源:默认七列。
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if got := m["columns"].([]any); len(got) != 7 {
		t.Errorf("无配置时应为默认七列,实际 %v", got)
	}

	// 配置源:只保留 total 一列。
	cfg := &config.Config{RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total"}},
	}}
	h2, _ := newTestServer(t, WithConfigProvider(func() *config.Config { return cfg }))
	m2 := decodeJSON(t, doGet(h2, "/api/dashboard"))
	if got := m2["columns"].([]any); len(got) != 1 || got[0] != "total" {
		t.Errorf("配置单列后 columns 应为 [total],实际 %v", got)
	}

	// 顶层问题态(RawQuery 为 nil 且存在顶层问题):回退默认七列。
	badCfg := &config.Config{RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
		"query": {Name: "query", Kind: "root_not_table"},
	}}
	h3, _ := newTestServer(t, WithConfigProvider(func() *config.Config { return badCfg }))
	m3 := decodeJSON(t, doGet(h3, "/api/dashboard"))
	if got := m3["columns"].([]any); len(got) != 7 {
		t.Errorf("顶层问题态应回退默认七列,实际 %v", got)
	}
}

// TestServeStatic:/ 与 /assets/ 返回内嵌资产(200 + 非空,不断言内容),
// 全部响应携带 Cache-Control: no-store。
func TestServeStatic(t *testing.T) {
	h, _ := newTestServer(t)
	cases := []struct {
		target         string
		wantCT         string
		wantCTContains bool // Content-Type 用包含匹配(文件服务自行推断)
	}{
		{target: "/", wantCT: "text/html; charset=utf-8"},
		{target: "/assets/app.css", wantCTContains: true, wantCT: "text/css"},
		{target: "/assets/app.js", wantCTContains: true, wantCT: "javascript"},
	}
	for _, tc := range cases {
		rec := doGet(h, tc.target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s 应 200,实际 %d", tc.target, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s 响应体应非空", tc.target)
		}
		ct := rec.Header().Get("Content-Type")
		if tc.wantCTContains {
			if !strings.Contains(ct, tc.wantCT) {
				t.Errorf("GET %s Content-Type 应含 %q,实际 %q", tc.target, tc.wantCT, ct)
			}
		} else if ct != tc.wantCT {
			t.Errorf("GET %s Content-Type 应为 %q,实际 %q", tc.target, tc.wantCT, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s 应携带 Cache-Control: no-store,实际 %q", tc.target, cc)
		}
	}

	// 目录与缺失资产:404。
	for _, target := range []string{"/assets/", "/assets/nope.css"} {
		rec := doGet(h, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 应 404,实际 %d", target, rec.Code)
		}
	}
}
