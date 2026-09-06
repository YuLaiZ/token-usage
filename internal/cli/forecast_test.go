package cli

import (
	"bytes"
	"context"
	"fmt"
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
	var buf bytes.Buffer
	if err := renderForecast(&buf, forecastTestStats()); err != nil {
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
	var buf bytes.Buffer
	empty := forecastWindowStats{}
	if err := renderForecast(&buf, empty); err != nil {
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
	// 数值断言(注入数据必须真实进入图表,防止测试打不开注入库的假隔离):
	// hover 提示含两日的 tokens 缩写,副标题含区间总量与请求数。
	if !strings.Contains(string(svg), "1.20 K tokens") || !strings.Contains(string(svg), "300 tokens") {
		t.Errorf("SVG 应含两日的 tokens 悬停值 1.20 K/300:\n%s", svg)
	}
	if !strings.Contains(string(svg), "Total 1.50 K tokens / 2 requests") {
		t.Errorf("SVG 副标题应含区间汇总 1.50 K / 2:\n%s", svg)
	}
	if !strings.Contains(buf.String(), outPath) {
		t.Errorf("stdout 应回执写入路径:\n%s", buf.String())
	}
}

// chart --pie 合法路径端到端:--by model 的占比切片与图例进入 SVG,
// 零值维度被跳过(不产生扇区)。
func TestChartCmd_PieEndToEnd(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)
	msgs := []model.Message{
		{ID: "p-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), Model: "model-x", TotalTokens: 3000},
		{ID: "p-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), Model: "model-y", TotalTokens: 1000},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "pie.svg")
	cmd := newChartCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// 显式传日期:缺省日期取真实时钟,测试不应随系统日期跨天而失效。
	cmd.SetArgs([]string{"20260906", "--pie", "--by", "model", "--out", outPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	svg, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(svg), "model-x (75.0%)") || !strings.Contains(string(svg), "model-y (25.0%)") {
		t.Errorf("图例应含两模型的占比:\n%s", svg)
	}
	// 标题由 chartTitleFor 生成,恰含一个 " by model" 后缀(不双拼)。
	if !strings.Contains(string(svg), "<title>token-usage 2026-09-06 by model</title>") {
		t.Errorf("SVG 主标题应为 token-usage 2026-09-06 by model:\n%s", svg)
	}
	if strings.Contains(string(svg), "by model by model") {
		t.Errorf("SVG 标题不应重复拼接 by 后缀:\n%s", svg)
	}

	// --pie --by month(时间维度)同 day 一样被拒绝。
	cmd2 := newChartCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	cmd2.SetArgs([]string{"20260906", "--pie", "--by", "month"})
	cmd2.SetOut(&buf)
	cmd2.SetErr(&buf)
	if err := cmd2.Execute(); err == nil {
		t.Fatal("--pie --by month 应被拒绝")
	}
}

// watch 单帧渲染:输出实时监视头(刷新时间)、summary 与按模型分组;
// --once 模式渲染一帧即返回,重定向与管道友好。
func TestWatchCmd_OnceFrame(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "w-a", SessionID: "s", Client: model.ClientClaudeCode,
		Date: today.Format("2006-01-02"), TS: today.UnixMilli(),
		Model: "model-x", TotalTokens: 800,
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: t.TempDir()}
	fixedNow := func() time.Time { return today }
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		fixedNow,
		func(time.Duration) { t.Fatal("非 once 模式不应 sleep") },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once"})

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Live watch - refreshed at 09:30:00") {
		t.Errorf("应含实时监视头与刷新时间:\n%s", out)
	}
	if !strings.Contains(out, "Total requests / 请求总数: 1") {
		t.Errorf("帧内应含 summary:\n%s", out)
	}
	if !strings.Contains(out, "model-x") {
		t.Errorf("帧内应含按模型分组:\n%s", out)
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Errorf("once 模式不应输出清屏序列:\n%s", out)
	}
}

// 间隔下限校验:小于 1s 的 --interval 在开库前拒绝。
func TestWatchCmd_IntervalTooSmall(t *testing.T) {
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) {
			return &config.Config{DataDir: t.TempDir()}, nil
		},
		func(string) (*db.DB, error) { return nil, fmt.Errorf("must not open") },
		func() time.Time { return time.Unix(0, 0) },
		func(time.Duration) {},
	)
	cmd.SetArgs([]string{"--interval", "500ms", "--once"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "1 秒") {
		t.Errorf("过小间隔应报错且含 1 秒,实际: %v", err)
	}
}

// watch 循环模式:每帧清屏归位,sleep 以注入间隔推进;用 sentinel panic 在
// 第 2 次 sleep 处终止循环,断言恰渲染 2 帧。
func TestWatchCmd_LoopFrames(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 8, 0, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "w-l", SessionID: "s", Client: model.ClientClaudeCode,
		Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 50,
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: t.TempDir()}
	tick := 0
	sleep := func(d time.Duration) {
		tick++
		if tick >= 2 {
			panic(watchLoopSentinel)
		}
	}
	defer func() {
		if r := recover(); r != watchLoopSentinel {
			t.Fatalf("循环应以 sentinel 终止,实际: %v", r)
		}
	}()
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		func() time.Time { return today.Add(time.Duration(tick) * time.Second) },
		sleep,
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260906"})
	cmd.Execute()
	if n := strings.Count(buf.String(), "\x1b[2J\x1b[H"); n != 2 {
		t.Errorf("两帧应各含一次清屏归位序列,实际 %d:\n%s", n, buf.String())
	}
}
