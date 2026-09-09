package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// TestRenderForecastJSON_PreciseValues 已知量精确验证:windows 与 projections
// 的日均走整数除法、估算走日均乘法,数值逐项锚定。last7 取整除样本
// (9990/5=1998),last30 取非整除样本(10000/3=3333,整数除法向下取整可见)。
func TestRenderForecastJSON_PreciseValues(t *testing.T) {
	stats := forecastWindowStats{
		today:  querier.RangeStats{ActiveDays: 1, Total: querier.GroupAggregate{Requests: 3, TotalTokens: 500}},
		last7:  querier.RangeStats{ActiveDays: 5, Total: querier.GroupAggregate{Requests: 42, TotalTokens: 9990}},
		last30: querier.RangeStats{ActiveDays: 3, Total: querier.GroupAggregate{Requests: 90, TotalTokens: 10000}},
	}
	var buf bytes.Buffer
	if err := renderForecastJSON(&buf, stats); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}

	// today 独立窗口:今日量不出现在 last_7_days/last_30_days。
	today := jsonMap(t, doc["today_so_far"])
	jsonInt(t, today["requests"], 3)
	jsonInt(t, today["total_tokens"], 500)

	// windows 恒 2 条,窗口名与自然天数固定。
	windows := jsonArray(t, doc["windows"])
	if len(windows) != 2 {
		t.Fatalf("windows 恒 2 条,实际 %d:\n%s", len(windows), out)
	}
	w7 := jsonMap(t, windows[0])
	if w7["window"] != "last_7_days" {
		t.Errorf("windows[0].window = %v, want last_7_days", w7["window"])
	}
	jsonInt(t, w7["days"], 7)
	jsonInt(t, w7["active_days"], 5)
	jsonInt(t, w7["requests"], 42)
	jsonInt(t, w7["total_tokens"], 9990)
	jsonInt(t, w7["avg_per_active_day"], 1998) // 9990/5,整除。
	w30 := jsonMap(t, windows[1])
	if w30["window"] != "last_30_days" {
		t.Errorf("windows[1].window = %v, want last_30_days", w30["window"])
	}
	jsonInt(t, w30["days"], 30)
	jsonInt(t, w30["active_days"], 3)
	jsonInt(t, w30["total_tokens"], 10000)
	jsonInt(t, w30["avg_per_active_day"], 3333) // 10000/3=3333.3..,向下取整。

	// projections:估算 = 日均 × 未来自然天数。
	projections := jsonArray(t, doc["projections"])
	if len(projections) != 2 {
		t.Fatalf("两窗口均有数据,应有 2 条预测,实际 %d:\n%s", len(projections), out)
	}
	p7 := jsonMap(t, projections[0])
	if p7["window"] != "next_7_days" || p7["based_on"] != "last_7_days" {
		t.Errorf("projections[0] = %v/%v, want next_7_days/last_7_days", p7["window"], p7["based_on"])
	}
	jsonInt(t, p7["days"], 7)
	jsonInt(t, p7["avg_per_active_day"], 1998)
	jsonInt(t, p7["estimate_tokens"], 13986) // 1998×7。
	p30 := jsonMap(t, projections[1])
	if p30["window"] != "next_30_days" || p30["based_on"] != "last_30_days" {
		t.Errorf("projections[1] = %v/%v, want next_30_days/last_30_days", p30["window"], p30["based_on"])
	}
	jsonInt(t, p30["days"], 30)
	jsonInt(t, p30["avg_per_active_day"], 3333)
	jsonInt(t, p30["estimate_tokens"], 99990) // 3333×30,乘法在整除取整之后。
}

// TestRenderForecastJSON_NoDataWindowProjectionOmitted 一个窗口无数据:该窗口
// 的 projection 条目被省略,而 windows 恒 2 条且无数据窗口 active_days=0 照实
// 输出(avg_per_active_day 为 0,不做除法)。
func TestRenderForecastJSON_NoDataWindowProjectionOmitted(t *testing.T) {
	stats := forecastWindowStats{
		last7: querier.RangeStats{ActiveDays: 2, Total: querier.GroupAggregate{Requests: 4, TotalTokens: 800}},
		// today 与 last30 为零值。
	}
	var buf bytes.Buffer
	if err := renderForecastJSON(&buf, stats); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}

	windows := jsonArray(t, doc["windows"])
	if len(windows) != 2 {
		t.Fatalf("windows 恒 2 条,实际 %d:\n%s", len(windows), out)
	}
	w30 := jsonMap(t, windows[1])
	if w30["window"] != "last_30_days" {
		t.Errorf("windows[1].window = %v, want last_30_days", w30["window"])
	}
	jsonInt(t, w30["active_days"], 0)
	jsonInt(t, w30["avg_per_active_day"], 0)

	projections := jsonArray(t, doc["projections"])
	if len(projections) != 1 {
		t.Fatalf("仅 last_7_days 有数据,应恰 1 条预测,实际 %d:\n%s", len(projections), out)
	}
	if got := jsonMap(t, projections[0])["window"]; got != "next_7_days" {
		t.Errorf("唯一预测条应为 next_7_days,实际 %v", got)
	}

	// today 为零值时照实输出 0(与表格「0」口径一致)。
	today := jsonMap(t, doc["today_so_far"])
	jsonInt(t, today["requests"], 0)
	jsonInt(t, today["total_tokens"], 0)
}

// TestRenderForecastJSON_AllEmpty 两窗口均无数据:windows 照常 2 条,
// projections 为空数组 [](非 null)。
func TestRenderForecastJSON_AllEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderForecastJSON(&buf, forecastWindowStats{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}
	if n := len(jsonArray(t, doc["windows"])); n != 2 {
		t.Errorf("windows 恒 2 条,实际 %d", n)
	}
	projections, ok := doc["projections"].([]any)
	if !ok {
		t.Fatalf("projections 应为空数组而非 null,实际 %T: %v", doc["projections"], doc["projections"])
	}
	if len(projections) != 0 {
		t.Errorf("projections 应为空数组,实际 %v", projections)
	}
}

// TestForecastCmd_EndToEnd_JSON 真实调用链 --format json:now 注入,窗口语义
// 与表格版一致(last7/last30 不含今天,今天单独进 today_so_far)。
func TestForecastCmd_EndToEnd_JSON(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1)
	msgs := []model.Message{
		{ID: "fj-today", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 500},
		{ID: "fj-yest", SessionID: "s", Client: model.ClientClaudeCode,
			Date: yesterday.Format("2006-01-02"), TS: yesterday.UnixMilli(), TotalTokens: 700},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	cmd := newForecastCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		func() time.Time { return today },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "Forecast") || strings.Contains(out, "Today so far") {
		t.Errorf("json 输出不应含表格标题/标签行:\n%s", out)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}
	// 今日 500 只进 today_so_far;昨日 700 落在两个历史窗口(各 1 活跃天)。
	jsonInt(t, jsonMap(t, doc["today_so_far"])["total_tokens"], 500)
	w7 := jsonMap(t, jsonArray(t, doc["windows"])[0])
	jsonInt(t, w7["total_tokens"], 700)
	jsonInt(t, w7["active_days"], 1)
	jsonInt(t, w7["avg_per_active_day"], 700)
	projections := jsonArray(t, doc["projections"])
	if len(projections) != 2 {
		t.Fatalf("应有 2 条预测,实际 %d:\n%s", len(projections), out)
	}
	jsonInt(t, jsonMap(t, projections[0])["estimate_tokens"], 4900)  // 700×7。
	jsonInt(t, jsonMap(t, projections[1])["estimate_tokens"], 21000) // 700×30。
}

// TestForecastCmd_RejectsFormat --format 非法值:双语报错且在加载配置与打开
// 数据库之前拒绝(open 计数为 0)。
func TestForecastCmd_RejectsFormat(t *testing.T) {
	openCalls := 0
	cmd := newForecastCmdWithDeps(
		func() (*config.Config, error) { t.Fatal("--format 校验失败不应加载配置"); return nil, nil },
		func(string) (*db.DB, error) {
			openCalls++
			return nil, fmt.Errorf("must not open database")
		},
		func() time.Time { return time.Unix(0, 0) },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "xml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--format xml 应返回 error")
	}
	msg := err.Error()
	for _, want := range []string{
		`invalid --format "xml" (allowed: table, json)`,
		`无效的 --format "xml"（允许：table、json）`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q: %q", want, msg)
		}
	}
	if openCalls != 0 {
		t.Errorf("格式拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestForecastCmd_FormatFlagDefault --format 缺省 table,usage 双语与 compare 一致。
func TestForecastCmd_FormatFlagDefault(t *testing.T) {
	flag := newForecastCmd().Flags().Lookup("format")
	if flag == nil {
		t.Fatal("expected --format flag")
	}
	if flag.DefValue != "table" {
		t.Errorf("--format 缺省应为 table, got %q", flag.DefValue)
	}
	if want := "output format: table or json / 输出格式：table 或 json"; flag.Usage != want {
		t.Errorf("--format usage = %q, want %q", flag.Usage, want)
	}
}
