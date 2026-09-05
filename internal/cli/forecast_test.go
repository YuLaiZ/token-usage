package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

func forecastTestStats() forecastWindowStats {
	return forecastWindowStats{
		today:  querier.RangeStats{ActiveDays: 1, Total: querier.GroupAggregate{Requests: 5, TotalTokens: 1000}},
		last7:  querier.RangeStats{ActiveDays: 7, Total: querier.GroupAggregate{Requests: 70, TotalTokens: 7000}},
		last30: querier.RangeStats{ActiveDays: 28, Total: querier.GroupAggregate{Requests: 280, TotalTokens: 28000}},
	}
}

// 渲染合同:今日至今、窗口行(总量/自然天/活跃天/日均)、外推行(日均×自然天)。
func TestRenderForecast_WithFullData(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.Local)
	var buf bytes.Buffer
	if err := renderForecast(&buf, now, forecastTestStats()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Forecast / 用量外推",
		"Today so far / 今日至今: 1.00 K",
		"Last 7 days / 最近 7 天: 7.00 K, 1.00 K/day (7/7 days active / 天有数据)",
		"Last 30 days / 最近 30 天: 28.00 K, 1.00 K/day (28/30 days active / 天有数据)",
		"Next 7 days / 未来 7 天: 7.00 K (1.00 K/day × 7)",
		"Next 30 days / 未来 30 天: 30.00 K (1.00 K/day × 30)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q:\n%s", want, out)
		}
	}
	// 日均口径:30 天窗口 28 天活跃,日均 = 28000/28 = 1000;若错误地按自然天
	// 平均(28000/30≈933)则 1.00 K/day 不会变,但外推基数变化——用整数整除
	// 敏感的数字再验一次:28000/28=1000 vs 28000/30=933(非整除可见)。
	if !strings.Contains(out, "1.00 K/day") {
		t.Errorf("日均应按活跃天平均:\n%s", out)
	}
}

// 窗口无数据:窗口行显示无数据,对应外推行省略,不编造数字。
func TestRenderForecast_NoData(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.Local)
	var buf bytes.Buffer
	empty := forecastWindowStats{}
	if err := renderForecast(&buf, now, empty); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Today so far / 今日至今: 0") {
		t.Errorf("无数据时今日行应显示 0:\n%s", out)
	}
	if strings.Count(out, "no data / 无数据") != 2 {
		t.Errorf("两个窗口行都应显示无数据:\n%s", out)
	}
	if strings.Contains(out, "Next 7 days") || strings.Contains(out, "Next 30 days") {
		t.Errorf("无数据时不应输出外推行:\n%s", out)
	}
}

// 端到端:本地时区构造消息,forecast 命令的三窗口查询与渲染走通。
func TestForecastCmd_EndToEnd(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1)
	msgs := []model.Message{
		{ID: "f-today", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 500},
		{ID: "f-yest", SessionID: "s", Client: model.ClientClaudeCode,
			Date: yesterday.Format("2006-01-02"), TS: yesterday.UnixMilli(), TotalTokens: 700},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	tmpHome := t.TempDir()
	cfg := &config.Config{DataDir: tmpHome}
	fixedNow := func() time.Time { return today }
	cmd := newForecastCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		fixedNow,
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// 今日 500 已进 Today 行;昨日 700 落在最近 7 天窗口(总量 700、1 活跃天)。
	if !strings.Contains(out, "Today so far / 今日至今: 500") {
		t.Errorf("Today 行应为 500:\n%s", out)
	}
	if !strings.Contains(out, "Last 7 days / 最近 7 天: 700, 700/day (1/7 days active / 天有数据)") {
		t.Errorf("最近 7 天行应只统计窗口内数据:\n%s", out)
	}
	// 外推:7 天日均 700×7=4900;30 天窗口同为昨日一天 → 700×30=21000。
	if !strings.Contains(out, "Next 7 days / 未来 7 天: 4.90 K (700/day × 7)") {
		t.Errorf("7 天外推应为 4.90 K:\n%s", out)
	}
	if !strings.Contains(out, "Next 30 days / 未来 30 天: 21.00 K (700/day × 30)") {
		t.Errorf("30 天外推应为 21.00 K:\n%s", out)
	}
}

// chart 命令端到端:区间数据渲染为 SVG(有数据日成柱、缺口日零柱),
// --out 原子写入文件并回执提示。
func TestChartCmd_EndToEndWithOut(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)
	d1Date := today.AddDate(0, 0, -1)
	d2Date := today.AddDate(0, 0, -2)
	d1, d2 := d1Date.Format("2006-01-02"), d2Date.Format("2006-01-02")
	// 命令参数用 compact 形态。
	d1c, d2c := d1Date.Format("20060102"), d2Date.Format("20060102")
	msgs := []model.Message{
		{ID: "c-a", SessionID: "s", Client: model.ClientClaudeCode, Date: d1, TS: today.AddDate(0, 0, -1).UnixMilli(), TotalTokens: 1200},
		{ID: "c-b", SessionID: "s", Client: model.ClientClaudeCode, Date: d2, TS: today.AddDate(0, 0, -2).UnixMilli(), TotalTokens: 300},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "usage.svg")
	cfg := &config.Config{DataDir: t.TempDir()}
	cmd := newChartCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{d2c + "-" + d1c, "--out", outPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	svg, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("--out 未写入文件: %v", err)
	}
	if !strings.Contains(string(svg), d1) {
		t.Errorf("SVG 应含日期标签 %s:\n%s", d1, svg)
	}
	if !strings.Contains(buf.String(), outPath) {
		t.Errorf("stdout 应回执写入路径:\n%s", buf.String())
	}
}
