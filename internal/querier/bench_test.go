package querier

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// benchQuerier 构造一张 days×perDay 规模的内存消息账本(约 7.3 万行对应
// 一年×每天 200 条,与真实库量级相当),作为各维度视图的性能基线。
// 时间戳不影响基准结果,统一用 UTC 折算避免对本地时区的依赖。
func benchQuerier(b *testing.B, days, perDay int) *Querier {
	b.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	b.Cleanup(func() { testDB.Close() })

	models := []string{"model-a", "model-b", "model-c"}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgs := make([]model.Message, 0, days*perDay)
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		date := day.Format("2006-01-02")
		base := day.UnixMilli()
		for i := 0; i < perDay; i++ {
			msgs = append(msgs, model.Message{
				ID:          fmt.Sprintf("bench-%d-%d", d, i),
				SessionID:   fmt.Sprintf("sess-%d", d%64),
				Client:      model.ClientClaudeCode,
				Date:        date,
				TS:          base + int64(i)*1000,
				Model:       models[i%len(models)],
				TotalTokens: int64(100 + i),
			})
		}
	}
	if _, err := db.UpsertMessages(context.Background(), testDB, msgs); err != nil {
		b.Fatalf("UpsertMessages failed: %v", err)
	}
	return New(testDB)
}

// benchDates 生成 [start, start+days) 的连续逐日请求列表。
func benchDates(days int) []string {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dates := make([]string, days)
	for i := range dates {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	return dates
}

// BenchmarkDimensionViews 是维度视图的聚合-渲染全链路基准:同一年规模账本上
// 对比各视图,耗时的量级变化(如索引失效、逐行折算回归)应在此体现。
func BenchmarkDimensionViews(b *testing.B) {
	q := benchQuerier(b, 365, 200)
	dates := benchDates(365)
	ctx := context.Background()

	benchmarks := []struct {
		name string
		run  func() error
	}{
		{"ByClient", func() error { _, err := q.ByClient(ctx, dates); return err }},
		{"ByDay", func() error { _, err := q.ByDay(ctx, dates); return err }},
		{"ByMonth", func() error { _, err := q.ByMonth(ctx, dates); return err }},
		{"ByHour", func() error { _, err := q.ByHour(ctx, dates); return err }},
		{"ByWeekday", func() error { _, err := q.ByWeekday(ctx, dates); return err }},
		{"Sessions", func() error { _, err := q.Sessions(ctx, dates); return err }},
		{"HourModel", func() error {
			_, err := q.RunDimensionView(ctx, dates, DimensionView{
				Dimensions: []string{"hour", "model"},
				TitleEn:    "bench", TitleZh: "bench",
			})
			return err
		}},
		// weekday×hour 双时间戳维度(heatmap 数据源),Go 分桶路径的双维场景。
		{"HeatmapMatrix", func() error {
			_, err := q.HeatmapMatrix(ctx, dates)
			return err
		}},
	}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := bm.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
