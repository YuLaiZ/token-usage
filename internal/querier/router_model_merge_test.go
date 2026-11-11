package querier

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// cc-switch Stack 归因合并语义:model 维度取数切换为 router_model 优先后,
// 同一 client×provider 下 ccs 前缀请求 id(经 router_model 归因为裸名)与裸名
// model 行在 GROUP BY 级合并为一行、请求数与 token 求和;router_model 本身带
// 前缀的异常行(上游失败请求兜底链形态)独立成组、原样显示,不与裸名合并。
// SQL 聚合路径与共享范围扫描路径双路断言一致。
func TestModelDimension_RouterModelMerge(t *testing.T) {
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()

	date := "2026-11-06"
	msgs := []model.Message{
		// ccs 请求 id,归因命中:router_model 为剥前缀裸名。
		{ID: "m-ccs-attributed", SessionID: "s1", Client: model.ClientCodexApp,
			Date: date, TS: 1000, Model: "ccs-deepseek/deepseek-flash",
			RouterModel: "deepseek-flash", RouterProvider: "DeepSeek", TotalTokens: 100},
		// 裸名 model,未归因(router_model 空):应与上行合并为一行。
		{ID: "m-bare", SessionID: "s1", Client: model.ClientCodexApp,
			Date: date, TS: 2000, Model: "deepseek-flash",
			Provider: "DeepSeek", TotalTokens: 50},
		// ccs 请求 id,归因行本身带前缀(上游失败请求兜底链):独立成组原样显示。
		{ID: "m-ccs-prefixed", SessionID: "s1", Client: model.ClientCodexApp,
			Date: date, TS: 3000, Model: "ccs-deepseek/deepseek-flash",
			RouterModel: "ccs-deepseek/deepseek-flash", RouterProvider: "DeepSeek", TotalTokens: 30},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}
	q := New(testDB)

	dims := []string{"model", "provider", "client"}
	rows, totals, err := q.AggregateDimensionView(ctx, []string{date}, DimensionView{
		Dimensions: dims, TitleEn: "router model merge", TitleZh: "路由模型合并",
	})
	if err != nil {
		t.Fatalf("AggregateDimensionView failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (merged bare + prefixed)\nrows: %+v", len(rows), rows)
	}
	// 排序第②层 total 降序:合并行 150 在前。
	merged, prefixed := rows[0], rows[1]
	if merged.Keys[0] != "DEEPSEEK-FLASH" {
		t.Errorf("merged row model key = %q, want DEEPSEEK-FLASH\nrows: %+v", merged.Keys[0], rows)
	}
	if merged.Agg.Requests != 2 || merged.Agg.TotalTokens != 150 {
		t.Errorf("merged row agg = {requests: %d, total: %d}, want {2, 150}", merged.Agg.Requests, merged.Agg.TotalTokens)
	}
	if merged.Keys[1] != "DeepSeek" || merged.Keys[2] != model.ClientCodexApp {
		t.Errorf("merged row provider/client = %q/%q, want DeepSeek/%s", merged.Keys[1], merged.Keys[2], model.ClientCodexApp)
	}
	if prefixed.Keys[0] != "CCS-DEEPSEEK/DEEPSEEK-FLASH" {
		t.Errorf("prefixed row model key = %q, want CCS-DEEPSEEK/DEEPSEEK-FLASH\nrows: %+v", prefixed.Keys[0], rows)
	}
	if prefixed.Agg.Requests != 1 || prefixed.Agg.TotalTokens != 30 {
		t.Errorf("prefixed row agg = {requests: %d, total: %d}, want {1, 30}", prefixed.Agg.Requests, prefixed.Agg.TotalTokens)
	}
	if totals.TotalTokens != 180 || totals.Requests != 3 {
		t.Errorf("totals = {requests: %d, total: %d}, want {3, 180}", totals.Requests, totals.TotalTokens)
	}

	// 共享范围扫描路径与 SQL 聚合路径结果一致。
	snap, err := q.ScanRange(ctx, []string{date}, []AggSpec{
		{Name: "combo", Kind: AggDimension, Dimensions: dims},
	})
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}
	got, ok := snap.Dimension("combo")
	if !ok {
		t.Fatal("dimension \"combo\" not found")
	}
	if !dimensionRowsEqual(got.Rows, rows) {
		t.Errorf("range scan rows mismatch\n got  %+v\nwant  %+v", got.Rows, rows)
	}
	if got.Totals != totals {
		t.Errorf("range scan totals = %+v, want %+v", got.Totals, totals)
	}
}
