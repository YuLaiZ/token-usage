package querier

import (
	"context"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// ===== 估算时长与速度的聚合三分量（B 包 T10/T12/T14）=====

// setupDurationFixture 构造估算时长 fixture：
//   - 模型 fast：有效行 200 tok/1s + 无计时行 10000 tok（duration=0）；
//   - 模型 slow：有效行 300 tok/3s；
//   - 模型 none：全部无计时（400 tok, duration=0）。
//
// 有效样本集合口径：fast 速度 = 1000*200/1000 = 200 tok/s（分子绝不含
// 无计时行的 10000 tok）；fast 平均时长 1s；none 组无有效样本显示 —。
func setupDurationFixture(t *testing.T) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()
	msgs := []model.Message{
		{
			ID: "m-fast-ok", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-08", TS: 1000, Model: "fast", Provider: "Anthropic",
			OutputTokens: 200, TotalTokens: 200, DurationMS: 1000,
		},
		{
			ID: "m-fast-notimed", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-08", TS: 2000, Model: "fast", Provider: "Anthropic",
			OutputTokens: 10000, TotalTokens: 10000,
		},
		{
			ID: "m-slow-ok", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-08", TS: 3000, Model: "slow", Provider: "Anthropic",
			OutputTokens: 300, TotalTokens: 300, DurationMS: 3000,
		},
		{
			ID: "m-none", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-08", TS: 4000, Model: "none", Provider: "Anthropic",
			OutputTokens: 400, TotalTokens: 400,
		},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}
	return New(testDB)
}

// renderDurationView 渲染模型维度视图文本（配置指定指标列）。
func renderDurationView(t *testing.T, q *Querier, columns []string) string {
	t.Helper()
	if err := q.SetOutputColumns(columns); err != nil {
		t.Fatalf("SetOutputColumns: %v", err)
	}
	out, err := q.ByModel(context.Background(), []string{"2026-10-08"})
	if err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	return out
}

// T10/T14：默认布局不含新列；配置后有效集合公式成立（负向：分子不含无计时行）。
func TestDurationColumns_EffectiveSampleSet(t *testing.T) {
	q := setupDurationFixture(t)

	// T14：默认七列不含新列。
	out, err := q.ByModel(context.Background(), []string{"2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Avg dur") || strings.Contains(out, "Speed") {
		t.Fatalf("默认布局不应含时长/速度列:\n%s", out)
	}
	for _, id := range ui.DefaultOutputColumns() {
		if id == ui.MetricAvgDur || id == ui.MetricSpeed {
			t.Fatalf("DefaultOutputColumns 不应含新列 ID: %s", id)
		}
	}

	// 配置后渲染（avg_dur 在 speed 前）。逐行断言：每行含模型键与两格指标。
	out = renderDurationView(t, q, []string{ui.MetricAvgDur, ui.MetricSpeed})
	for _, row := range []struct{ key, avg, speed string }{
		{"fast", "1.0s", "200 tok/s"}, // 分子不含 10000 无计时 tok
		{"slow", "3.0s", "100 tok/s"},
		{"none", "—", "—"}, // 无有效样本
	} {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, row.key) && !strings.Contains(line, "Total") {
				if !strings.Contains(line, row.avg) || !strings.Contains(line, row.speed) {
					t.Fatalf("%s 行 = %q, want 含 %q 与 %q", row.key, line, row.avg, row.speed)
				}
			}
		}
	}
	// 总计行：三分量合并后计算（500 tok / 4s / 2 行 → 2.0s 与 125 tok/s）。
	var totalLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Total") {
			totalLine = line
		}
	}
	if !strings.Contains(totalLine, "2.0s") || !strings.Contains(totalLine, "125 tok/s") {
		t.Fatalf("总计行 = %q, want 含 2.0s 与 125 tok/s", totalLine)
	}
}

// T12：聚合分量经 foldKey 合并（大小写折叠同组）后先合并分量再计算。
func TestDurationAggregate_FoldMergeComponents(t *testing.T) {
	q := setupDurationFixture(t)
	// 同模型名大小写变体（foldKey 折叠同组）：Fast 行各自带有效时长。
	ctx := context.Background()
	testDB := q.db
	if _, err := db.UpsertMessages(ctx, testDB, []model.Message{
		{
			ID: "m-fast-upper", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-08", TS: 5000, Model: "Fast", Provider: "Anthropic",
			OutputTokens: 600, TotalTokens: 600, DurationMS: 3000,
		},
	}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := q.AggregateDimensionView(ctx, []string{"2026-10-08"}, DimensionView{Dimensions: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	var fast GroupAggregate
	for _, r := range rows {
		// fast+Fast 折叠同组（Requests=3）；代表拼写由维度分组大小写归一规则决定。
		if strings.EqualFold(r.Keys[0], "fast") {
			fast = r.Agg
		}
	}
	if fast.Requests != 3 {
		t.Fatalf("未找到 fast 折叠行: %+v", rows)
	}
	// fast+Fast 合并：时长和 4s、有效行 2、有效输出和 800（无计时 10000 不计入）。
	if fast.DurationSumMS != 4000 || fast.DurationCount != 2 || fast.DurationOutputSum != 800 {
		t.Fatalf("fold 合并分量 = sum=%d count=%d output=%d, want 4000/2/800",
			fast.DurationSumMS, fast.DurationCount, fast.DurationOutputSum)
	}
	// 速度 = 1000*800/4000 = 200 tok/s（先合并分量再计算）。
	if got := fast.SpeedTokPerSec(); got != 200 {
		t.Fatalf("合并后速度 = %v, want 200", got)
	}
	// 平均时长 = 4000/2 = 2000ms。
	if got := fast.AvgDurationMS(); got != 2000 {
		t.Fatalf("合并后平均时长 = %v, want 2000", got)
	}
}

// AggregateDimensionView 三分量在 SQL 聚合（GROUP BY）与 Go 累加（裸列）两条
// 路径下等价。
func TestDurationAggregate_SQLAndGoPathsEqual(t *testing.T) {
	q := setupDurationFixture(t)
	ctx := context.Background()
	byModel, _, err := q.AggregateDimensionView(ctx, []string{"2026-10-08"}, DimensionView{Dimensions: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	// hour 维度触发裸列 Go 累加路径。
	byHour, _, err := q.AggregateDimensionView(ctx, []string{"2026-10-08"}, DimensionView{Dimensions: []string{"hour"}})
	if err != nil {
		t.Fatal(err)
	}
	var modelSum, hourSum GroupAggregate
	for _, r := range byModel {
		modelSum.add(r.Agg)
	}
	for _, r := range byHour {
		hourSum.add(r.Agg)
	}
	if modelSum.DurationSumMS != hourSum.DurationSumMS ||
		modelSum.DurationCount != hourSum.DurationCount ||
		modelSum.DurationOutputSum != hourSum.DurationOutputSum {
		t.Fatalf("两路径三分量不等: SQL=%+v Go=%+v", modelSum, hourSum)
	}
}
