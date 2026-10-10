package web

// 时长三分量在 dashboard 载荷的合同（B 包 T13）：
//   - 维度行与自定义视图行携带三分量与由分量计算的均值/速度；
//   - 无有效样本的组 avg_duration_ms/speed_tok_s 为 null（键存在）；
//   - 「其他」尾行先合并分量再计算（均值与速度不可加）；
//   - 前端 COLS 新列与单位格式化见 appjs_behavior_test 的 Node vm 行为测试。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// seedDurationDashboardFixture 构造带估算时长的载荷夹具：
//   - client-a：两行有效（200tok/1s 与 600tok/3s）+ 一行无计时 10000tok；
//   - client-b：一行无计时（无有效样本组）。
//
// 超过维度 Top-9 阈值不需要（两个 client 即可验证行语义）。
func seedDurationDashboardFixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local)
	todayDate := today.Format("2006-01-02")
	msgs := []model.Message{
		{ID: "d1", SessionID: "s1", Client: "client-a", Model: "m1", Provider: "p", Project: "pr",
			Date: todayDate, TS: today.UnixMilli(), OutputTokens: 200, TotalTokens: 200, DurationMS: 1000},
		{ID: "d2", SessionID: "s1", Client: "client-a", Model: "m1", Provider: "p", Project: "pr",
			Date: todayDate, TS: today.Add(time.Second).UnixMilli(), OutputTokens: 600, TotalTokens: 600, DurationMS: 3000},
		{ID: "d3", SessionID: "s1", Client: "client-a", Model: "m1", Provider: "p", Project: "pr",
			Date: todayDate, TS: today.Add(2 * time.Second).UnixMilli(), OutputTokens: 10000, TotalTokens: 10000},
		{ID: "d4", SessionID: "s2", Client: "client-b", Model: "m1", Provider: "p", Project: "pr",
			Date: todayDate, TS: today.Add(3 * time.Second).UnixMilli(), OutputTokens: 400, TotalTokens: 400},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version", WithConfigProvider(func() *config.Config {
		return &config.Config{RawQuery: map[string]any{
			"subqueries": map[string]any{"cm": "client,model"},
		}}
	}))
	return h, todayDate
}

func TestDashboardDuration_DimensionRows(t *testing.T) {
	h, day := seedDurationDashboardFixture(t)
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Dimensions map[string][]struct {
			Key               string   `json:"key"`
			DurationSumMS     int64    `json:"duration_ms_sum"`
			DurationCount     int64    `json:"duration_count"`
			DurationOutputSum int64    `json:"duration_output_sum"`
			AvgDurationMS     *int64   `json:"avg_duration_ms"`
			SpeedTokPerSec    *float64 `json:"speed_tok_s"`
		} `json:"dimensions"`
		CustomViews []struct {
			Name string `json:"name"`
			Rows []struct {
				Keys              []string `json:"keys"`
				DurationSumMS     int64    `json:"duration_ms_sum"`
				DurationCount     int64    `json:"duration_count"`
				DurationOutputSum int64    `json:"duration_output_sum"`
				AvgDurationMS     *int64   `json:"avg_duration_ms"`
				SpeedTokPerSec    *float64 `json:"speed_tok_s"`
			} `json:"rows"`
		} `json:"custom_views"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]struct {
		Key               string   `json:"key"`
		DurationSumMS     int64    `json:"duration_ms_sum"`
		DurationCount     int64    `json:"duration_count"`
		DurationOutputSum int64    `json:"duration_output_sum"`
		AvgDurationMS     *int64   `json:"avg_duration_ms"`
		SpeedTokPerSec    *float64 `json:"speed_tok_s"`
	}{}
	for _, r := range resp.Dimensions["client"] {
		byKey[r.Key] = r
	}
	// client-a：分量 4s/2 行/800 tok；均值 2s、速度 200 tok/s（分子不含 10000）。
	a := byKey["client-a"]
	if a.DurationSumMS != 4000 || a.DurationCount != 2 || a.DurationOutputSum != 800 {
		t.Fatalf("client-a 分量 = %d/%d/%d, want 4000/2/800", a.DurationSumMS, a.DurationCount, a.DurationOutputSum)
	}
	if a.AvgDurationMS == nil || *a.AvgDurationMS != 2000 {
		t.Fatalf("client-a avg = %v, want 2000", a.AvgDurationMS)
	}
	if a.SpeedTokPerSec == nil || *a.SpeedTokPerSec != 200 {
		t.Fatalf("client-a speed = %v, want 200", a.SpeedTokPerSec)
	}
	// client-b：无有效样本 → 计算值为 null（键存在）。
	b := byKey["client-b"]
	if b.AvgDurationMS != nil || b.SpeedTokPerSec != nil {
		t.Fatalf("client-b 无有效样本应为 null: %v/%v", b.AvgDurationMS, b.SpeedTokPerSec)
	}
	// 自定义视图行同语义。
	if len(resp.CustomViews) == 0 || len(resp.CustomViews[0].Rows) < 2 {
		t.Fatalf("custom_views 行缺失: %+v", resp.CustomViews)
	}
	found := false
	for _, r := range resp.CustomViews[0].Rows {
		if len(r.Keys) > 0 && r.Keys[0] == "client-a" {
			found = true
			if r.DurationCount != 2 || r.AvgDurationMS == nil || *r.AvgDurationMS != 2000 {
				t.Fatalf("自定义视图 client-a = %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("自定义视图未找到 client-a: %+v", resp.CustomViews[0].Rows)
	}
}

// seedDurationTailFixture 构造 11 个 client（触发维度 Top-9 截断生成「其他」
// 尾行）：client-01/02 带有效时长（落入尾行），其余落独立行。
func seedDurationTailFixture(t *testing.T) (http.Handler, string) {
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
	for i := 1; i <= 11; i++ {
		m := model.Message{
			ID: fmt.Sprintf("t%02d", i), SessionID: "s", Client: fmt.Sprintf("client-%02d", i),
			Model: "m", Provider: "p", Project: "pr",
			Date: todayDate, TS: today.Add(time.Duration(i) * time.Second).UnixMilli(),
			OutputTokens: 500, TotalTokens: int64(i) * 100,
		}
		if i <= 2 {
			m.DurationMS = int64(i) * 1000 // client-01: 1s; client-02: 2s（各 500 tok）
		}
		msgs = append(msgs, m)
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(usageDB), "test-version")
	return h, todayDate
}

// 尾行先合并分量再计算：均值 1.5s（(1s+2s)/2）与速度 333 tok/s
// （1000*1000/3000），不是各行均值/速度的相加或平均。
func TestDashboardDuration_OtherTailMergesComponents(t *testing.T) {
	h, day := seedDurationTailFixture(t)
	rec := doGet(h, "/api/dashboard?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Dimensions map[string][]json.RawMessage `json:"dimensions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Key               string   `json:"key"`
		IsOther           bool     `json:"is_other"`
		DurationSumMS     int64    `json:"duration_ms_sum"`
		DurationCount     int64    `json:"duration_count"`
		DurationOutputSum int64    `json:"duration_output_sum"`
		AvgDurationMS     *int64   `json:"avg_duration_ms"`
		SpeedTokPerSec    *float64 `json:"speed_tok_s"`
	}
	var tail *row
	if err := json.Unmarshal(resp.Dimensions["client"][len(resp.Dimensions["client"])-1], &tail); err != nil {
		t.Fatal(err)
	}
	if tail == nil || !tail.IsOther {
		t.Fatalf("末行应为「其他」尾行: %+v", tail)
	}
	if tail.DurationSumMS != 3000 || tail.DurationCount != 2 || tail.DurationOutputSum != 1000 {
		t.Fatalf("尾行分量 = %d/%d/%d, want 3000/2/1000", tail.DurationSumMS, tail.DurationCount, tail.DurationOutputSum)
	}
	if tail.AvgDurationMS == nil || *tail.AvgDurationMS != 1500 {
		t.Fatalf("尾行均值 = %v, want 1500（先合并分量再计算）", tail.AvgDurationMS)
	}
	if tail.SpeedTokPerSec == nil || *tail.SpeedTokPerSec < 333.33 || *tail.SpeedTokPerSec > 333.34 {
		t.Fatalf("尾行速度 = %v, want ≈333.33", tail.SpeedTokPerSec)
	}
}
