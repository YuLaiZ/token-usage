package web

// dashboard_contract_test.go 锁定 2026-09-24 确认的响应整理合同:
//   - 四维度前 9 独立项 + 至多一行"其他"(余项为零无尾行,尾行固定最后,
//     即使 total 大于最后独立项也不插队;同值按名称稳定排序);
//   - 自定义视图前 19 独立组合 + 至多一行"其他组合";
//   - 会话榜前 20 行、不生成"其他会话"、标题原文;
//   - 独立行+尾行七项与区间 totals 逐项守恒(缓存命中率由整数重算);
//   - 趋势周桶/峰值/空态;热力 future 空白与真实零格的语义区分。
// 全部走真实 HTTP 路由 + 内存库夹具,同一读事务快照可复验守恒。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// sumDimensionRows 汇总七项整数(尾行参与)。
func sumDimensionRows(rows []dimensionRowJSON) dimensionRowJSON {
	var s dimensionRowJSON
	for _, r := range rows {
		s.Requests += r.Requests
		s.FreshInput += r.FreshInput
		s.Output += r.Output
		s.CacheRead += r.CacheRead
		s.CacheCreate += r.CacheCreate
		s.Reasoning += r.Reasoning
		s.Total += r.Total
	}
	return s
}

// assertSevenConserved 断言七项逐项与期望相等。
func assertSevenConserved(t *testing.T, what string, got dimensionRowJSON, want totalsJSON) {
	t.Helper()
	if got.Requests != want.Requests || got.FreshInput != want.FreshInput ||
		got.Output != want.Output || got.CacheRead != want.CacheRead ||
		got.CacheCreate != want.CacheCreate || got.Reasoning != want.Reasoning ||
		got.Total != want.Total {
		t.Errorf("%s 七项守恒失败:\n得到 %+v\n期望 requests=%d fresh_input=%d output=%d cache_read=%d cache_create=%d reasoning=%d total=%d",
			what, got, want.Requests, want.FreshInput, want.Output, want.CacheRead, want.CacheCreate, want.Reasoning, want.Total)
	}
}

// seedDashboardFixture 在内存库写入 count 条互异 client/model/provider/project
// 组合消息,total 递减可控。第 i 条消息(total=i*10+7)携带全部四个维度值
// client-i/model-i/provider-i/project-i,以及 fresh/cache 三项按固定比例,
// 使七项守恒断言有意义。
func seedDashboardFixture(t *testing.T, count int, extraSessions bool) (http.Handler, totalsJSON, string) {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")

	var msgs []model.Message
	var sessions []model.Session
	var want totalsJSON
	for i := 1; i <= count; i++ {
		total := int64(i)*10 + 7 // 逐条互异,避免 total 相同干扰降序断言
		msg := model.Message{
			ID: fmt.Sprintf("m%03d", i), SessionID: fmt.Sprintf("s%03d", i),
			Client: fmt.Sprintf("client-%02d", i), Model: fmt.Sprintf("model-%02d", i),
			Provider: fmt.Sprintf("provider-%02d", i), Project: fmt.Sprintf("project-%02d", i),
			Date: todayDate, TS: today.Add(time.Duration(i) * time.Second).UnixMilli(),
			FreshInputTokens: total / 4, OutputTokens: total / 4, CacheReadTokens: total / 4,
			CacheCreateTokens: total - total/4*3, ReasoningTokens: 0, TotalTokens: total,
		}
		msgs = append(msgs, msg)
		want.Requests++
		want.FreshInput += msg.FreshInputTokens
		want.Output += msg.OutputTokens
		want.CacheRead += msg.CacheReadTokens
		want.CacheCreate += msg.CacheCreateTokens
		want.Reasoning += msg.ReasoningTokens
		want.Total += msg.TotalTokens
		if extraSessions {
			sessions = append(sessions, model.Session{
				ID: msg.SessionID, Client: msg.Client, Project: msg.Project,
				Title: fmt.Sprintf("原始标题-%03d 不改写", i), FirstTS: msg.TS, LastTS: msg.TS,
			})
		}
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	if len(sessions) > 0 {
		if _, err := db.UpsertSessionMeta(ctx, usageDB, sessions); err != nil {
			t.Fatal(err)
		}
	}
	h := NewServer(querier.New(usageDB), "test-version", WithConfigProvider(func() *config.Config {
		return &config.Config{RawQuery: map[string]any{
			"subqueries": map[string]any{"mp": "model,provider"},
		}}
	}))
	return h, want, todayDate
}

// TestDashboardContract_DimensionTail:12 个独立 client → 前 9 + 尾行"其他"
// (other_count=3);尾行 total(三条最小项之和)小于第 9 项,另用倒序夹具
// 单独验证"尾行大于最后独立项仍固定末尾";独立行+尾行七项与 totals 守恒;
// 尾行携带 is_other/other_count,独立行不带。
func TestDashboardContract_DimensionTail(t *testing.T) {
	h, want, day := seedDashboardFixture(t, 12, false)
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	rows := resp.Dimensions["client"]
	if len(rows) != 10 {
		t.Fatalf("client 维度应 9 独立项+1 尾行,实际 %d 行", len(rows))
	}
	// 前 9 行:total 严格降序(夹具 total 递增 → 取前 9 为 19..11 号即 197..117)。
	for i, r := range rows[:9] {
		if r.IsOther || r.OtherCount != 0 {
			t.Errorf("独立行 %d 不得携带 is_other/other_count(键缺失语义)", i)
		}
		if i > 0 && rows[i-1].Total <= r.Total {
			t.Errorf("独立项应 total 降序,位置 %d:%d <= %d", i, rows[i-1].Total, r.Total)
		}
	}
	// 尾行固定最后:is_other=true、other_count=3、数值=最小三项(1/2/3 号)之和。
	tail := rows[9]
	if !tail.IsOther || tail.OtherCount != 3 || tail.Key != "" {
		t.Errorf("尾行应为 is_other+other_count=3+空 key,实际 %+v", tail)
	}
	// total 17+27+37=81(消息号 1..3;夹具 total 随序号递增,前 9 名是 4..12 号)。
	if tail.Total != 81 || tail.Requests != 3 {
		t.Errorf("尾行应为 3 项合计 total=81 requests=3,实际 total=%d requests=%d", tail.Total, tail.Requests)
	}
	assertSevenConserved(t, "client 维度(9+其他)", sumDimensionRows(rows), want)
	// 其余三个维度同合同。
	for _, dim := range []string{"model", "provider", "project"} {
		if got := len(resp.Dimensions[dim]); got != 10 {
			t.Errorf("%s 维度应 10 行,实际 %d", dim, got)
		}
		if !resp.Dimensions[dim][9].IsOther {
			t.Errorf("%s 维度尾行缺失", dim)
		}
	}
}

// TestDashboardContract_TailBelowLastStaysLast:构造"其他"total 大于最后
// 一个独立项的分布(前 9 项 total=1,余 3 项 total=1000),验证尾行仍固定
// 最后、不插队——旧版"其他必须小于最后独立项"规则废止。
func TestDashboardContract_TailBelowLastStaysLast(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local).Format("2006-01-02")
	var msgs []model.Message
	for i := 1; i <= 12; i++ {
		total := int64(1)
		if i > 9 {
			total = 1000 // 10..12 号合计 3000,尾行 total 远大于第 9 独立项
		}
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("t%02d", i), SessionID: "s" + fmt.Sprintf("%02d", i),
			Client: fmt.Sprintf("client-%02d", i), Date: day,
			TS: now.UnixMilli(), TotalTokens: total,
		})
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")
	var resp dashboardResponse
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	rows := resp.Dimensions["client"]
	if len(rows) != 10 {
		t.Fatalf("应 10 行,实际 %d", len(rows))
	}
	// 排序后:3 个 1000 的独立项在前(位置 0..2),随后 6 个 1(位置 3..8),
	// 尾行=剩余 3 个 1 之和(3)——尾行 total 大于最后独立项(1)仍固定最后。
	for i := 0; i < 3; i++ {
		if rows[i].Total != 1000 || rows[i].IsOther {
			t.Errorf("独立项 %d 应 total=1000,实际 %+v", i, rows[i])
		}
	}
	for i := 3; i < 9; i++ {
		if rows[i].Total != 1 || rows[i].IsOther {
			t.Errorf("独立项 %d 应 total=1,实际 %+v", i, rows[i])
		}
	}
	tail := rows[9]
	if !tail.IsOther || tail.OtherCount != 3 || tail.Total != 3 {
		t.Errorf("尾行应 is_other/other_count=3/total=3(>第 9 独立项 1 仍固定末尾),实际 %+v", tail)
	}
}

// TestDashboardContract_TieStableByName:全部 total 相等时按名称字节序稳定
// 排序——12 个同 total 客户端取前 9 应为 client-01..client-09。
func TestDashboardContract_TieStableByName(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local).Format("2006-01-02")
	var msgs []model.Message
	for i := 1; i <= 12; i++ {
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("e%02d", i), SessionID: "s" + fmt.Sprintf("%02d", i),
			Client: fmt.Sprintf("client-%02d", i), Date: day,
			TS: now.UnixMilli(), TotalTokens: 500,
		})
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")
	var resp dashboardResponse
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	rows := resp.Dimensions["client"]
	if len(rows) != 10 {
		t.Fatalf("应 10 行,实际 %d", len(rows))
	}
	for i, r := range rows[:9] {
		if want := fmt.Sprintf("client-%02d", i+1); r.Key != want {
			t.Errorf("同值独立项应按名称字节序,位置 %d 应 %s 实际 %s", i, want, r.Key)
		}
	}
	if rows[9].OtherCount != 3 {
		t.Errorf("尾行 other_count 应 3,实际 %d", rows[9].OtherCount)
	}
}

// TestDashboardContract_CustomViewTail:mp 视图 25 个组合 → 前 19 + 尾行
// "其他组合"(other_count=6),七项守恒,尾行固定最后;同值组合按键序。
func TestDashboardContract_CustomViewTail(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local).Format("2006-01-02")
	var msgs []model.Message
	for i := 1; i <= 25; i++ {
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("c%02d", i), SessionID: "s" + fmt.Sprintf("%02d", i),
			Model: fmt.Sprintf("model-%02d", i), Provider: fmt.Sprintf("provider-%02d", i),
			Date: day, TS: now.UnixMilli(), TotalTokens: int64(i) * 100,
		})
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RawQuery: map[string]any{
		"subqueries": map[string]any{"mp": "model,provider"},
	}}
	h := NewServer(querier.New(usageDB), "test-version", WithConfigProvider(func() *config.Config { return cfg }))
	var resp dashboardResponse
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.CustomViews) != 1 {
		t.Fatalf("应 1 个自定义视图,实际 %d", len(resp.CustomViews))
	}
	rows := resp.CustomViews[0].Rows
	if len(rows) != 20 {
		t.Fatalf("mp 视图应 19 独立组合+1 尾行,实际 %d", len(rows))
	}
	for i, r := range rows[:19] {
		if r.IsOther {
			t.Errorf("独立组合 %d 不得携带 is_other", i)
		}
		if i > 0 && rows[i-1].Total <= r.Total {
			t.Errorf("独立组合应 total 降序,位置 %d", i)
		}
	}
	tail := rows[19]
	if !tail.IsOther || tail.OtherCount != 6 {
		t.Errorf("尾行应 is_other+other_count=6,实际 %+v", tail)
	}
	// 尾行数值 = 最小 6 个组合合计(100..600 号,i=1..6)。
	if tail.Total != 2100 {
		t.Errorf("尾行 total 应 100+200+...+600=2100,实际 %d", tail.Total)
	}
	// 七项守恒(此夹具只有 total 有值)。
	var sum customViewRowJSON
	for _, r := range rows {
		sum.Total += r.Total
		sum.Requests += r.Requests
	}
	if sum.Total != resp.Totals.Total || sum.Requests != resp.Totals.Requests {
		t.Errorf("自定义视图行合计应守恒:total %d vs %d,requests %d vs %d",
			sum.Total, resp.Totals.Total, sum.Requests, resp.Totals.Requests)
	}
}

// TestDashboardContract_SessionTop20:21 个会话 → 恰 20 行、不生成其他行、
// 标题原文不改写、降序稳定。
func TestDashboardContract_SessionTop20(t *testing.T) {
	h, _, day := seedDashboardFixture(t, 21, true)
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 20 {
		t.Fatalf("会话榜应恰 20 行(后端截断),实际 %d", len(resp.Sessions))
	}
	// 夹具 total 递增:前 20 名为 21..2 号,第 1 行 total=21*10+7=217。
	if resp.Sessions[0].Total != 217 {
		t.Errorf("榜首应为 21 号会话 total=217,实际 %d", resp.Sessions[0].Total)
	}
	if resp.Sessions[19].Total != 27 { // 2 号会话 2*10+7
		t.Errorf("第 20 行应为 2 号会话 total=27,实际 %d", resp.Sessions[19].Total)
	}
	for _, s := range resp.Sessions {
		// 夹具标题与 client 同源:client-XX → 标题「原始标题-0XX 不改写」。
		want := "原始标题-0" + s.Client[len(s.Client)-2:] + " 不改写"
		if s.Title != want {
			t.Errorf("会话标题应保留原文:期望 %q 实际 %q", want, s.Title)
		}
	}
}

// TestDashboardContract_TrendWeekBuckets:70 天范围 → 周桶(自 from 每 7 天,
// 末桶 0 天余量时恰 10 桶);键为各桶最后一天;峰值/空态正确。
func TestDashboardContract_TrendWeekBuckets(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	from := to.AddDate(0, 0, -69) // 恰 70 天
	// 两条消息:from 当天 700、to 当天 300;其余 68 天零值。
	day := func(d time.Time) string { return d.Format("2006-01-02") }
	msgs := []model.Message{
		{ID: "w1", SessionID: "w1", Client: "claude", Date: day(from), TS: from.UnixMilli(), TotalTokens: 700},
		{ID: "w2", SessionID: "w2", Client: "claude", Date: day(to), TS: to.UnixMilli(), TotalTokens: 300},
	}
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")
	rec := doGet(h, "/api/dashboard?from="+day(from)+"&to="+day(to))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tr := resp.Trend
	if tr.Granularity != "week" {
		t.Fatalf("70 天趋势粒度应为 week,实际 %q", tr.Granularity)
	}
	if len(tr.Buckets) != 10 {
		t.Fatalf("70 天应恰 10 个周桶,实际 %d", len(tr.Buckets))
	}
	if tr.Buckets[0].Total != 700 || tr.Buckets[0].Key != day(from.AddDate(0, 0, 6)) {
		t.Errorf("首周桶应含 from 当天 700、键为第 7 天 %s,实际 %+v", day(from.AddDate(0, 0, 6)), tr.Buckets[0])
	}
	if tr.Buckets[9].Total != 300 || tr.Buckets[9].Key != day(to) {
		t.Errorf("末桶应含 to 当天 300、键为 to,实际 %+v", tr.Buckets[9])
	}
	if tr.Empty || tr.PeakIndex != 0 || tr.PeakTotal != 700 {
		t.Errorf("峰值应为首桶 700,实际 empty=%v index=%d total=%d", tr.Empty, tr.PeakIndex, tr.PeakTotal)
	}
	// 热力 70 天走 calendar 模式:仅 total,无 hours/blocks。
	if resp.Heatmap.Mode != "calendar" {
		t.Errorf("70 天热力模式应为 calendar,实际 %q", resp.Heatmap.Mode)
	}
	if len(resp.Heatmap.Days) != 70 {
		t.Errorf("calendar 热力应 70 行,实际 %d", len(resp.Heatmap.Days))
	}
	if len(resp.Heatmap.Days[0].Hours) != 0 || len(resp.Heatmap.Days[0].Blocks) != 0 {
		t.Errorf("calendar 模式不得携带 hours/blocks 键")
	}
}

// TestDashboardContract_FutureBlankVsZero:跨今天的范围——今天及之前的零值
// 日是真实零格(future 缺省 false),今天之后的日期标记 future(空白格)。
func TestDashboardContract_FutureBlankVsZero(t *testing.T) {
	h, _, today := seedDashboardFixture(t, 2, false)
	to, err := time.Parse("2006-01-02", today)
	if err != nil {
		t.Fatal(err)
	}
	from := to.AddDate(0, 0, -5)
	toFuture := to.AddDate(0, 0, 3)
	target := fmt.Sprintf("/api/dashboard?from=%s&to=%s", from.Format("2006-01-02"), toFuture.Format("2006-01-02"))
	rec := doGet(h, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Heatmap.Days) != 9 {
		t.Fatalf("范围应 9 天,实际 %d", len(resp.Heatmap.Days))
	}
	for i, d := range resp.Heatmap.Days {
		futureExpected := i > 5
		if d.Future != futureExpected {
			t.Errorf("日期 %s future 应为 %v,实际 %v", d.Date, futureExpected, d.Future)
		}
		if i <= 5 && d.Total == 0 && d.Future {
			t.Errorf("已发生零值日 %s 不得伪装成未来空白", d.Date)
		}
	}
}

// TestDashboardContract_CalendarModeDayHourPresence:2 天范围走 day_hour,
// 每日 24 小时格、无 blocks;4 小时时段模式只在 8~31 天出现(由
// TestServeDashboard_HeatmapDayHour 覆盖 30 天)。
func TestDashboardContract_CalendarModeDayHourPresence(t *testing.T) {
	h, want, today := seedDashboardFixture(t, 3, false)
	to, err := time.Parse("2006-01-02", today)
	if err != nil {
		t.Fatal(err)
	}
	from := to.AddDate(0, 0, -1)
	rec := doGet(h, fmt.Sprintf("/api/dashboard?from=%s&to=%s", from.Format("2006-01-02"), today))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", rec.Code)
	}
	m := decodeJSON(t, rec)
	hm := m["heatmap"].(map[string]any)
	if hm["mode"] != "day_hour" {
		t.Errorf("2 天范围热力模式应为 day_hour,实际 %v", hm["mode"])
	}
	day0 := hm["days"].([]any)[0].(map[string]any)
	assertKeys(t, "day_hour days[0]", day0, "date", "total", "hours")
	var resp dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	assertSevenConserved(t, "3 天四维度行守恒",
		sumDimensionRows(resp.Dimensions["client"]), want)
	if !reflect.DeepEqual(len(resp.Trend.Buckets), 2) {
		t.Errorf("2 天趋势应 2 个日桶,实际 %d", len(resp.Trend.Buckets))
	}
}
