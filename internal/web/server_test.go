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

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// serveFixture 是内存库夹具的期望值集合:由构造函数一并算好,测试逐项断言。
type serveFixture struct {
	minDate    string
	maxDate    string
	dataThru   string // data_through 的 time.DateTime 本地形态
	totals     totalsJSON
	todayRow   dimensionRowJSON // day 维度中今天的行(升序第二行)
	oldRow     dimensionRowJSON // day 维度中 5 天前的行(升序第一行)
	topTotal   int64            // 会话排行首行 total
	dateFormat string           // 日期键的参考形态
}

// newTestServer 构造内存库 + 夹具数据 + NewServer(版本串 "test-version")。
// 数据形态:今天 12 个会话各 1 条消息(会话 i 总量 i*100),其中 s12 追加一条
// 一小时后 +200 的消息(覆盖时长与首末时间戳);5 天前 1 条 999 的消息
// (跨日的 min_date 与 day 维度两行)。全部时间戳按 time.Local 构造,与
// 聚合核的 hour/weekday 本机时分桶同口径。
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
		todayRow: dimensionRowJSON{Key: todayDate, Requests: 13, Total: tokTotal - 999},
		oldRow:   dimensionRowJSON{Key: oldDate, Requests: 1, Total: 999},
		topTotal: 1400, // s12:1200+200
	}
	return NewServer(querier.New(usageDB), "test-version", opts...), fx
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
// 无采集记录时 last_collection 为 null。
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
// 各嵌套对象的字段名精确匹配;day 维度键升序;sessions 按总量降序且截前 10;
// totals 数值精确。map 反序列化与结构体反序列化双检。
func TestServeDashboard_ShapeAndOrder(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}

	// 第一检:map 键集合精确匹配(多字段/少字段/拼错均失败)。
	m := decodeJSON(t, rec)
	assertKeys(t, "dashboard 顶层", m, "range", "totals", "compare", "forecast", "columns", "dimensions", "heatmap", "sessions")
	assertKeys(t, "range", m["range"].(map[string]any), "from", "to")
	assertKeys(t, "totals", m["totals"].(map[string]any),
		"requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total", "active_days")
	assertKeys(t, "compare", m["compare"].(map[string]any), "base_start", "base_end", "totals", "rows", "daily")
	assertKeys(t, "compare.rows[0]", m["compare"].(map[string]any)["rows"].([]any)[0].(map[string]any),
		"label", "current", "base", "change", "change_class", "change_pct")
	assertKeys(t, "forecast", m["forecast"].(map[string]any), "today_so_far", "rows")
	assertKeys(t, "forecast.rows[0]", m["forecast"].(map[string]any)["rows"].([]any)[0].(map[string]any),
		"label", "total", "avg_day", "active", "estimate")
	assertKeys(t, "sessions[0]", m["sessions"].([]any)[0].(map[string]any),
		"client", "project", "title", "first_ts", "last_ts", "duration_ms", "requests", "total")
	dims := m["dimensions"].(map[string]any)
	if !reflect.DeepEqual(keySet(dims), keySet(map[string]any{
		"day": nil, "hour": nil, "weekday": nil, "month": nil,
		"client": nil, "model": nil, "provider": nil, "project": nil,
	})) {
		t.Errorf("dimensions 应恰 8 个固定键,实际 %v", dims)
	}
	assertKeys(t, "dimensions.day[0]", dims["day"].([]any)[0].(map[string]any),
		"key", "requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total")

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
	// day 维度:纯单维时间视图对默认 30 天区间做缺口填充,行数恰 30、
	// 键整体严格升序;其中有数据的恰两行(5 天前与今天)且数值精确。
	dayRows := resp.Dimensions["day"]
	if len(dayRows) != 30 {
		t.Fatalf("day 维度应按默认区间补零至 30 行,实际 %d", len(dayRows))
	}
	for i := 1; i < len(dayRows); i++ {
		if dayRows[i-1].Key >= dayRows[i].Key {
			t.Fatalf("day 维度键应严格升序,位置 %d 出现 %s >= %s", i, dayRows[i-1].Key, dayRows[i].Key)
		}
	}
	var nonZero []dimensionRowJSON
	for _, row := range dayRows {
		if row.Total > 0 {
			nonZero = append(nonZero, row)
		}
	}
	if len(nonZero) != 2 || nonZero[0].Key != fx.minDate || nonZero[1].Key != fx.maxDate {
		t.Fatalf("有数据的 day 行应恰为 %s、%s 两行,实际 %+v", fx.minDate, fx.maxDate, nonZero)
	}
	if nonZero[0].Total != fx.oldRow.Total || nonZero[0].Requests != fx.oldRow.Requests {
		t.Errorf("旧行应 %+v,实际 %+v", fx.oldRow, nonZero[0])
	}
	if nonZero[1].Total != fx.todayRow.Total || nonZero[1].Requests != fx.todayRow.Requests {
		t.Errorf("今日行应 %+v,实际 %+v", fx.todayRow, nonZero[1])
	}
	// columns 恒为 querier 布局的指标 ID 序列:夹具走 New 的默认布局,
	// 与 ui.DefaultOutputColumns 同序同值(七列,不含 cache_create)。
	if want := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"}; !reflect.DeepEqual(resp.Columns, want) {
		t.Errorf("columns 应为默认七列 %v,实际 %v", want, resp.Columns)
	}

	// hour/weekday 为固定刻度缺口填充:行数恰 24/7。
	if len(resp.Dimensions["hour"]) != 24 {
		t.Errorf("hour 维度应固定 24 行,实际 %d", len(resp.Dimensions["hour"]))
	}
	if len(resp.Dimensions["weekday"]) != 7 {
		t.Errorf("weekday 维度应固定 7 行,实际 %d", len(resp.Dimensions["weekday"]))
	}
	// compare.daily 为基线窗口的逐日行(缺口填充):默认区间 to-29..to 的
	// 基线为 to-59..to-30,行数恰 30、键严格升序;夹具数据只在今天与
	// to-5,均落在当前区间内,基线窗口逐日全为 0。
	if len(resp.Compare.Daily) != 30 {
		t.Fatalf("compare.daily 应按基线窗口补零至 30 行,实际 %d", len(resp.Compare.Daily))
	}
	var dailySum int64
	for i, row := range resp.Compare.Daily {
		if row.Total != 0 {
			t.Errorf("compare.daily[%d] 应为 0,实际 %d", i, row.Total)
		}
		dailySum += row.Total
		if i > 0 && resp.Compare.Daily[i-1].Key >= row.Key {
			t.Errorf("compare.daily 键应严格升序,位置 %d 出现 %s >= %s", i, resp.Compare.Daily[i-1].Key, row.Key)
		}
	}
	if dailySum != resp.Compare.Totals.Total {
		t.Errorf("compare.daily 总和应等于基线窗口总量 %d,实际 %d", resp.Compare.Totals.Total, dailySum)
	}
	// forecast 恒 2 行且标签顺序固定;夹具在两个回看窗口内恰有 5 天前的
	// 999(1 活跃天):日均 999、预估 999×天数,与 forecast 命令整数除法
	// 同口径;today so far 与今天窗口总量(8000)同源同格式。
	fc := resp.Forecast
	if len(fc.Rows) != 2 {
		t.Fatalf("forecast.rows 应恒 2 行,实际 %d", len(fc.Rows))
	}
	for i, want := range []string{"Last 7 days / 最近 7 天", "Last 30 days / 最近 30 天"} {
		if fc.Rows[i].Label != want {
			t.Errorf("forecast.rows[%d].label 应为 %q,实际 %q", i, want, fc.Rows[i].Label)
		}
	}
	if want := querier.FormatTokens(fx.todayRow.Total); fc.TodaySoFar != want {
		t.Errorf("forecast.today_so_far 应为 %q,实际 %q", want, fc.TodaySoFar)
	}
	for i, days := range []int64{7, 30} {
		row := fc.Rows[i]
		wantEstimate := querier.FormatTokens(999 * days)
		if row.Total != "999" || row.AvgDay != "999" || row.Active != fmt.Sprintf("1/%d", days) ||
			row.Estimate != wantEstimate {
			t.Errorf("forecast.rows[%d] 应为 total=999,avg_day=999,active=1/%d,estimate=%s,实际 %+v",
				i, days, wantEstimate, row)
		}
	}
	// sessions:总量降序、截前 10、首行即最重会话、时长为毫秒差。
	sessions := resp.Sessions
	if len(sessions) != 10 {
		t.Fatalf("sessions 应截前 10 行,实际 %d", len(sessions))
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
}

// compareLabels 是环比对比 8 行的期望标签与顺序:与 cli compare 命令的
// renderCompare 及静态报告页完全一致(ui.Bi 双语组合串)。
var compareLabels = []string{
	"Active days / 活跃天",
	"Requests / 请求数",
	"Input / 输入",
	"Output / 输出",
	"Cache Read / 缓存读取",
	"Cache Create / 缓存创建",
	"Reasoning / 推理",
	"Total / 总计",
}

// TestServeDashboard_Compare_DefaultWindow:缺省 30 天区间的环比区块。
// 基线窗口为等长前移(from-30..from-1,无数据),与 compare 命令区间模式
// 一致;rows 恒 8 行且顺序与 renderCompare 一致;基线全 0 时各行变化% 为
// "--"、计数行变化为 "+N"、零变化行为 "0" 且无着色。
func TestServeDashboard_Compare_DefaultWindow(t *testing.T) {
	h, _ := newTestServer(t)
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	c := resp.Compare
	// 基线窗口推导:from = to-29(共 30 天),基线为等长前移窗口
	// from-30..from-1。
	toT, err := time.Parse("2006-01-02", resp.Range.To)
	if err != nil {
		t.Fatalf("range.to 应为 YYYY-MM-DD: %v", err)
	}
	fromT := toT.AddDate(0, 0, -29)
	wantBaseStart := fromT.AddDate(0, 0, -30).Format("2006-01-02")
	wantBaseEnd := fromT.AddDate(0, 0, -1).Format("2006-01-02")
	if c.BaseStart != wantBaseStart || c.BaseEnd != wantBaseEnd {
		t.Errorf("基线窗口应为 %s..%s,实际 %s..%s", wantBaseStart, wantBaseEnd, c.BaseStart, c.BaseEnd)
	}
	// rows 恒 8 行,标签与顺序精确。
	if len(c.Rows) != len(compareLabels) {
		t.Fatalf("rows 应恒 %d 行,实际 %d", len(compareLabels), len(c.Rows))
	}
	for i, want := range compareLabels {
		if c.Rows[i].Label != want {
			t.Errorf("rows[%d].label 应为 %q,实际 %q", i, want, c.Rows[i].Label)
		}
	}
	// 夹具基线窗口(today-59..today-30)无数据:基线总量全 0,变化% 恒 "--"。
	if c.Totals != (totalsJSON{}) {
		t.Errorf("基线 totals 应全 0,实际 %+v", c.Totals)
	}
	for i, row := range c.Rows {
		if row.Base != "0" {
			t.Errorf("rows[%d].base 应为 \"0\",实际 %q", i, row.Base)
		}
		if row.ChangePct != "--" {
			t.Errorf("rows[%d].change_pct 基线为 0 应为 \"--\",实际 %q", i, row.ChangePct)
		}
	}
	// 正差值行:Requests 14 vs 0 → 千分位显示 +"+14"、pos 着色。
	reqRow := c.Rows[1]
	if reqRow.Current != "14" || reqRow.Base != "0" || reqRow.Change != "+14" ||
		reqRow.ChangeClass != "num pos" {
		t.Errorf("Requests 行应为 current=14,base=0,change=+14,class=num pos,实际 %+v", reqRow)
	}
	// token 行走 K/M 缩写:Total 8999 vs 0 → "+9.00 K"。
	totalRow := c.Rows[7]
	if totalRow.Current != "9.00 K" || totalRow.Change != "+9.00 K" || totalRow.ChangeClass != "num pos" {
		t.Errorf("Total 行应为 current=+9.00 K 口径,实际 %+v", totalRow)
	}
	// 零变化行:Reasoning 0 vs 0 → change "0"、class "num"(无着色)。
	reasoningRow := c.Rows[6]
	if reasoningRow.Change != "0" || reasoningRow.ChangeClass != "num" {
		t.Errorf("Reasoning 行应为 change=0,class=num,实际 %+v", reasoningRow)
	}
}

// TestServeDashboard_Compare_ExplicitRange:显式 3 天区间(today-2..today),
// 基线为前移 3 天(today-5..today-3),恰好覆盖夹具 5 天前的 999(1 请求)
// ——基线非零时变化% 正常计算且与 compare 命令同口径。
func TestServeDashboard_Compare_ExplicitRange(t *testing.T) {
	h, _ := newTestServer(t)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	from := today.AddDate(0, 0, -2).Format("2006-01-02")
	to := today.Format("2006-01-02")
	rec := doGet(h, "/api/dashboard?from="+from+"&to="+to)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	c := resp.Compare
	// 基线窗口:today-5..today-3(等长前移,覆盖 old 那天)。
	if c.BaseStart != today.AddDate(0, 0, -5).Format("2006-01-02") ||
		c.BaseEnd != today.AddDate(0, 0, -3).Format("2006-01-02") {
		t.Errorf("基线窗口应为 %s..%s,实际 %s..%s",
			today.AddDate(0, 0, -5).Format("2006-01-02"), today.AddDate(0, 0, -3).Format("2006-01-02"),
			c.BaseStart, c.BaseEnd)
	}
	// 当前窗口:today 13 请求 / 8000 token / 1 活跃天;
	// 基线窗口:1 请求 / 999 token / 1 活跃天。基线 totals 字段精确值同款断言。
	if want := (totalsJSON{Requests: 1, Total: 999, ActiveDays: 1}); c.Totals != want {
		t.Errorf("基线 totals 应 %+v,实际 %+v", want, c.Totals)
	}
	// compare.daily:基线窗口逐日行缺口填充至 3 行,键升序且恰为基线三天;
	// 999 落在首行(today-5),其余两天为 0,总和等于基线总量。
	if len(c.Daily) != 3 {
		t.Fatalf("compare.daily 应按基线窗口补零至 3 行,实际 %d", len(c.Daily))
	}
	wantKeys := []string{
		today.AddDate(0, 0, -5).Format("2006-01-02"),
		today.AddDate(0, 0, -4).Format("2006-01-02"),
		today.AddDate(0, 0, -3).Format("2006-01-02"),
	}
	var dailySum int64
	for i, row := range c.Daily {
		if row.Key != wantKeys[i] {
			t.Errorf("compare.daily[%d].key 应为 %s,实际 %s", i, wantKeys[i], row.Key)
		}
		wantTotal := int64(0)
		if i == 0 {
			wantTotal = 999
		}
		if row.Total != wantTotal {
			t.Errorf("compare.daily[%d].total 应为 %d,实际 %d", i, wantTotal, row.Total)
		}
		dailySum += row.Total
	}
	if dailySum != c.Totals.Total {
		t.Errorf("compare.daily 总和应等于基线总量 %d,实际 %d", c.Totals.Total, dailySum)
	}
	// Requests 13 vs 1 → "+12"、pos、"+1200.0%"。
	reqRow := c.Rows[1]
	if reqRow.Current != "13" || reqRow.Base != "1" || reqRow.Change != "+12" ||
		reqRow.ChangeClass != "num pos" || reqRow.ChangePct != "+1200.0%" {
		t.Errorf("Requests 行应为 13/1/+12/num pos/+1200.0%%,实际 %+v", reqRow)
	}
	// Total 8000 vs 999 → "+7.00 K"、pos、"+700.8%"。
	totalRow := c.Rows[7]
	if totalRow.Current != "8.00 K" || totalRow.Base != "999" || totalRow.Change != "+7.00 K" ||
		totalRow.ChangeClass != "num pos" || totalRow.ChangePct != "+700.8%" {
		t.Errorf("Total 行应为 8.00 K/999/+7.00 K/num pos/+700.8%%,实际 %+v", totalRow)
	}
	// Active days 1 vs 1 → 持平:"0"、num、0.0%。
	daysRow := c.Rows[0]
	if daysRow.Change != "0" || daysRow.ChangeClass != "num" || daysRow.ChangePct != "0.0%" {
		t.Errorf("Active days 行应为 0/num/0.0%%,实际 %+v", daysRow)
	}
	// Input 0 vs 0 → 基线为 0,变化% 无定义 "--"(变化值仍显示 "0")。
	inputRow := c.Rows[2]
	if inputRow.Change != "0" || inputRow.ChangeClass != "num" || inputRow.ChangePct != "--" {
		t.Errorf("Input 行应为 0/num/--,实际 %+v", inputRow)
	}
}

// TestServeDashboard_Compare_NegativeChange:独立小夹具(昨天 500、今天 100,
// 单日区间 from=to)覆盖负差值三态:Total 下降为 neg 着色与负百分比,持平
// 指标(基线非 0)为无色 0.0%。
func TestServeDashboard_Compare_NegativeChange(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")
	yesterday := today.AddDate(0, 0, -1)
	yesterdayDate := yesterday.Format("2006-01-02")

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	msgs := []model.Message{
		{ID: "neg-y", SessionID: "s", Client: model.ClientClaudeCode, Date: yesterdayDate, TS: yesterday.UnixMilli(), TotalTokens: 500},
		{ID: "neg-t", SessionID: "s", Client: model.ClientClaudeCode, Date: todayDate, TS: today.UnixMilli(), TotalTokens: 100},
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")

	rec := doGet(h, "/api/dashboard?from="+todayDate+"&to="+todayDate)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	c := resp.Compare
	// 单日区间基线退化为前一天。
	if c.BaseStart != yesterdayDate || c.BaseEnd != yesterdayDate {
		t.Errorf("单日区间基线应为前一天 %s..%s,实际 %s..%s", yesterdayDate, yesterdayDate, c.BaseStart, c.BaseEnd)
	}
	// 基线 totals:1 请求 / 500 token / 1 活跃天。
	if want := (totalsJSON{Requests: 1, Total: 500, ActiveDays: 1}); c.Totals != want {
		t.Errorf("基线 totals 应 %+v,实际 %+v", want, c.Totals)
	}
	// Total 100 vs 500 → "-400"、neg、"-80.0%"。
	totalRow := c.Rows[7]
	if totalRow.Current != "100" || totalRow.Base != "500" || totalRow.Change != "-400" ||
		totalRow.ChangeClass != "num neg" || totalRow.ChangePct != "-80.0%" {
		t.Errorf("Total 行应为 100/500/-400/num neg/-80.0%%,实际 %+v", totalRow)
	}
	// Requests 1 vs 1 → 持平(基线非 0):"0"、num、0.0%。
	reqRow := c.Rows[1]
	if reqRow.Change != "0" || reqRow.ChangeClass != "num" || reqRow.ChangePct != "0.0%" {
		t.Errorf("Requests 行应为 0/num/0.0%%,实际 %+v", reqRow)
	}
}

// TestServeDashboard_Forecast:预估区块与 forecast 命令逐点同口径。独立
// 夹具在 yesterday-6..yesterday(恰为 7 天回看窗口)造 5 个活跃日共 9990
// token:日均 9990/5=1998,预估 1998×7=13986、1998×30=59940;精确串按
// querier.FormatTokens 阈值(>=1000 → %.2f K)推导。今天的数据只进
// today so far,不得泄漏进不含今天的回看窗口。
func TestServeDashboard_Forecast(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	amounts := []struct {
		offset int
		tokens int64
	}{
		{-6, 1000}, {-5, 2000}, {-4, 3000}, {-3, 1990}, {-2, 2000},
	}
	var msgs []model.Message
	for i, a := range amounts {
		day := today.AddDate(0, 0, a.offset)
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("fc-%d", i), SessionID: "s", Client: model.ClientClaudeCode,
			Date: day.Format("2006-01-02"), TS: day.UnixMilli(), Model: "model-x",
			TotalTokens: a.tokens,
		})
	}
	msgs = append(msgs, model.Message{
		ID: "fc-today", SessionID: "s", Client: model.ClientClaudeCode,
		Date: todayDate, TS: today.UnixMilli(), Model: "model-x", TotalTokens: 1234,
	})
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")

	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	f := resp.Forecast
	// 今日至今:1234 → "1.23 K";今天不计入回看窗口。
	if f.TodaySoFar != "1.23 K" {
		t.Errorf("today_so_far 应为 %q(FormatTokens(1234)),实际 %q", "1.23 K", f.TodaySoFar)
	}
	wantRows := []forecastRowJSON{
		{Label: "Last 7 days / 最近 7 天", Total: "9.99 K", AvgDay: "2.00 K", Active: "5/7", Estimate: "13.99 K"},
		{Label: "Last 30 days / 最近 30 天", Total: "9.99 K", AvgDay: "2.00 K", Active: "5/30", Estimate: "59.94 K"},
	}
	if !reflect.DeepEqual(f.Rows, wantRows) {
		t.Errorf("forecast.rows 应精确匹配:\n期望 %+v\n实际 %+v", wantRows, f.Rows)
	}
}

// TestServeDashboard_Forecast_NoData:回看窗口内无数据(仅今天有记录)时,
// 两行的数值格均为 "—"——CLI writeProjectionLine 省略预测行的同一语义在
// 表格形态下的表达;today so far 照常显示今日量。
func TestServeDashboard_Forecast_NoData(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	msgs := []model.Message{
		{ID: "fc-empty-t", SessionID: "s", Client: model.ClientClaudeCode,
			Date: todayDate, TS: today.UnixMilli(), Model: "model-x", TotalTokens: 500},
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")

	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	f := resp.Forecast
	if f.TodaySoFar != "500" {
		t.Errorf("today_so_far 应为 \"500\",实际 %q", f.TodaySoFar)
	}
	wantRows := []forecastRowJSON{
		{Label: "Last 7 days / 最近 7 天", Total: "—", AvgDay: "—", Active: "—", Estimate: "—"},
		{Label: "Last 30 days / 最近 30 天", Total: "—", AvgDay: "—", Active: "—", Estimate: "—"},
	}
	if !reflect.DeepEqual(f.Rows, wantRows) {
		t.Errorf("无数据窗口应整行 \"—\":\n期望 %+v\n实际 %+v", wantRows, f.Rows)
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

// TestServeDashboard_Heatmap:/api/dashboard 的活动热力矩阵。矩阵恒 7×24,
// 行=星期(ISO 周序周一在首)、列=小时("00:00".."23:00");数值与 totals
// 同一读事务快照:今天 12 条消息(10:01..10:12)落在 10 点桶共 7800,
// s12 的第二条(11:12)落在 11 点桶 200,5 天前消息(10:00)落在 10 点桶
// 999,其余交点全为 0;全矩阵总和恰等于 totals.Total。
func TestServeDashboard_Heatmap(t *testing.T) {
	h, fx := newTestServer(t)
	rec := doGet(h, "/api/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}

	// 第一检:map 键集合精确匹配(多字段/少字段/拼错均失败)。
	hm := decodeJSON(t, rec)["heatmap"].(map[string]any)
	assertKeys(t, "heatmap", hm, "weekdays", "hours", "values")

	// 第二检:结构体反序列化校验形状与数值。
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	hj := resp.Heatmap
	if len(hj.Values) != 7 {
		t.Fatalf("热力矩阵应 7 行(按星期),实际 %d", len(hj.Values))
	}
	if len(hj.Weekdays) != 7 {
		t.Fatalf("星期行标签应 7 项,实际 %d", len(hj.Weekdays))
	}
	if len(hj.Hours) != 24 || hj.Hours[0] != "00:00" || hj.Hours[23] != "23:00" {
		t.Fatalf("小时列标签应为 00:00..23:00 共 24 项,实际 %v", hj.Hours)
	}
	for wi, row := range hj.Values {
		if len(row) != 24 {
			t.Fatalf("热力矩阵第 %d 行应 24 列,实际 %d", wi, len(row))
		}
	}
	// 与夹具同源推导两天的星期行(ISO 周序:周一=0)。
	today, err := time.ParseInLocation("2006-01-02", fx.maxDate, time.Local)
	if err != nil {
		t.Fatalf("maxDate 应为 YYYY-MM-DD: %v", err)
	}
	old, err := time.ParseInLocation("2006-01-02", fx.minDate, time.Local)
	if err != nil {
		t.Fatalf("minDate 应为 YYYY-MM-DD: %v", err)
	}
	todayRow, oldRow := int(today.Weekday()+6)%7, int(old.Weekday()+6)%7
	wantCells := []struct {
		wi, hi   int
		want     int64
		describe string
	}{
		{todayRow, 10, 7800, "今天 10 点桶(12 条消息)"},
		{todayRow, 11, 200, "今天 11 点桶(s12 第二条)"},
		{oldRow, 10, 999, "5 天前 10 点桶"},
	}
	for _, wc := range wantCells {
		if got := hj.Values[wc.wi][wc.hi]; got != wc.want {
			t.Errorf("%s 应为 %d,实际 %d", wc.describe, wc.want, got)
		}
	}
	// 其余交点全为 0:置零两个非零桶所在星期行后整体求和应为 0。
	var sum int64
	for wi, row := range hj.Values {
		for hi, v := range row {
			if wi == todayRow && (hi == 10 || hi == 11) {
				continue
			}
			if wi == oldRow && hi == 10 {
				continue
			}
			sum += v
		}
	}
	if sum != 0 {
		t.Errorf("其余交点应全为 0,实际和 %d", sum)
	}
	// 同源一致性:全矩阵总和恰等于区间 totals.Total(同一读事务快照)。
	for _, row := range hj.Values {
		for _, v := range row {
			sum += v
		}
	}
	if sum != fx.totals.Total {
		t.Errorf("矩阵总和应等于 totals.Total %d,实际 %d", fx.totals.Total, sum)
	}
}

// TestServeChartSVG:柱状/饼图/热力矩阵类别返回完整 SVG 与正确 Content-Type;
// 非法 kind 与缺失 .svg 后缀返回 404。
func TestServeChartSVG(t *testing.T) {
	h, _ := newTestServer(t)

	rec := doGet(h, "/api/chart/day.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("day.svg 应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml; charset=utf-8" {
		t.Errorf("Content-Type 应为 image/svg+xml; charset=utf-8,实际 %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "</svg>") {
		t.Errorf("day.svg 应为完整 SVG 文档:\n%s", body)
	}
	// 标题规则与 chart 命令同源:day 维度标题仅区间。
	if !strings.Contains(body, "<title>token-usage") {
		t.Errorf("day.svg 应含 token-usage 前缀标题:\n%s", body)
	}

	for _, kind := range []string{"hour", "weekday", "month", "client", "model", "provider", "project", "heatmap"} {
		rec := doGet(h, "/api/chart/"+kind+".svg")
		if rec.Code != http.StatusOK {
			t.Errorf("%s.svg 应 200,实际 %d:\n%s", kind, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "<svg") {
			t.Errorf("%s.svg 应含 SVG 文档:\n%s", kind, rec.Body.String())
		}
	}

	// 饼图占比:两模型 1500/500 → 75.0%/25.0%(与 chart 命令同构)。
	rec = doGet(h, "/api/chart/model.svg?from=2026-01-01&to=2026-12-31")
	if rec.Code != http.StatusOK {
		t.Fatalf("model.svg(区间)应 200,实际 %d", rec.Code)
	}
	// 400 边界在 dashboard 用例覆盖;此处区间超限同样 400。
	rec = doGet(h, "/api/chart/day.svg?from=2024-01-01&to=2026-01-02")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("图表端点超 366 天应 400,实际 %d", rec.Code)
	}

	// 非法类别与形态:404。
	for _, target := range []string{
		"/api/chart/bogus.svg",
		"/api/chart/day",     // 缺 .svg 后缀
		"/api/chart/a/b.svg", // 含路径分隔符
		"/api/chart/.svg",
	} {
		rec := doGet(h, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应 404,实际 %d", target, rec.Code)
		}
	}
}

// TestServeChart_ProviderAliases:WithProviderAliases 注入后 provider 维度
// 只出现合并显示键(与 cli 侧 dimensionAliases 同构)。
func TestServeChart_ProviderAliases(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	msgs := []model.Message{
		{ID: "pa-a", SessionID: "s", Client: model.ClientClaudeCode, Date: todayDate, TS: today.UnixMilli(), Provider: "vendor-a", TotalTokens: 100},
		{ID: "pa-b", SessionID: "s", Client: model.ClientClaudeCode, Date: todayDate, TS: today.UnixMilli(), Provider: "vendor-b", TotalTokens: 200},
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	h := NewServer(querier.New(usageDB), "test-version",
		WithProviderAliases(map[string]string{"vendor-a": "merged-vendor", "vendor-b": "merged-vendor"}))
	rec := doGet(h, "/api/chart/provider.svg?from="+todayDate+"&to="+todayDate)
	if rec.Code != http.StatusOK {
		t.Fatalf("provider.svg 应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "merged-vendor") || strings.Contains(body, "vendor-a") || strings.Contains(body, "vendor-b") {
		t.Errorf("provider.svg 应合并供应商别名:\n%s", body)
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
