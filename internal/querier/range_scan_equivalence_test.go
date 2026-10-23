package querier

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// setupRandomFixture 构造覆盖大小写变体、provider 别名、空归因、别名与
// 会话 client 不匹配的随机数据，用于单遍扫描与逐次查询的双路对照。
func setupRandomFixture(t *testing.T, seed int64) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open db failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()

	rng := rand.New(rand.NewSource(seed))
	dates := []string{"2026-07-01", "2026-07-02", "2026-07-03"}
	clients := []string{"Claude Code", "claude code", "Codex", "OpenAI"}
	models := []string{"gpt-5", "GPT-5", "claude-sonnet-4", "Claude-Sonnet-4"}
	providers := []string{"OpenAI", "openai", "Anthropic", ""}
	projects := []string{"proj-A", "proj-B", ""}

	makeSession := func(i int) model.Session {
		return model.Session{
			ID:        fmt.Sprintf("s%02d", i),
			Client:    clients[rng.Intn(len(clients))],
			Directory: "/d",
			Project:   projects[rng.Intn(len(projects))],
			Title:     fmt.Sprintf("title-%02d", i),
			FirstTS:   1,
			LastTS:    2,
		}
	}
	sessions := make([]model.Session, 0, 12)
	for i := 0; i < 12; i++ {
		sessions = append(sessions, makeSession(i))
	}
	if _, err := db.UpsertSessionMeta(ctx, testDB, sessions); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}

	msgs := make([]model.Message, 0, 300)
	for i := 0; i < 300; i++ {
		date := dates[rng.Intn(len(dates))]
		dayStart, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatalf("parse date: %v", err)
		}
		ts := dayStart.Add(time.Duration(rng.Intn(24*60*60)) * time.Second).UnixMilli()
		dur := int64(0)
		if rng.Intn(3) == 0 {
			dur = int64(rng.Intn(5000) + 1)
		}
		msgs = append(msgs, model.Message{
			ID:                fmt.Sprintf("m%04d", i),
			SessionID:         fmt.Sprintf("s%02d", rng.Intn(12)),
			Client:            clients[rng.Intn(len(clients))],
			Date:              date,
			TS:                ts,
			Model:             models[rng.Intn(len(models))],
			Provider:          providers[rng.Intn(len(providers))],
			RouterProvider:    "",
			Directory:         "/d",
			Project:           projects[rng.Intn(len(projects))],
			FreshInputTokens:  int64(rng.Intn(1000)),
			OutputTokens:      int64(rng.Intn(500)),
			CacheReadTokens:   int64(rng.Intn(300)),
			CacheCreateTokens: int64(rng.Intn(100)),
			ReasoningTokens:   int64(rng.Intn(200)),
			TotalTokens:       int64(rng.Intn(2000)),
			DurationMS:        dur,
		})
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}
	return New(testDB)
}

func TestScanRangeRandomizedEquivalence(t *testing.T) {
	q := setupRandomFixture(t, 42)
	ctx := context.Background()
	dates := []string{"2026-07-01", "2026-07-02", "2026-07-03"}
	aliases := map[string]string{"openai": "OpenAI"}

	specs := []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
		{Name: "model", Kind: AggDimension, Dimensions: []string{"model"}},
		{Name: "provider", Kind: AggDimension, Dimensions: []string{"provider"}, Aliases: aliases},
		{Name: "project", Kind: AggDimension, Dimensions: []string{"project"}},
		{Name: "day", Kind: AggDimension, Dimensions: []string{"day"}},
		{Name: "month", Kind: AggDimension, Dimensions: []string{"month"}},
		{Name: "hour", Kind: AggDimension, Dimensions: []string{"hour"}},
		{Name: "weekday", Kind: AggDimension, Dimensions: []string{"weekday"}},
		{Name: "combo", Kind: AggDimension, Dimensions: []string{"model", "provider", "client"}, Aliases: aliases},
		{Kind: AggDayHour},
		{Kind: AggSession},
	}
	snap, err := q.ScanRange(ctx, dates, specs)
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}

	stats, err := q.StatsBetween(ctx, dates[0], dates[len(dates)-1])
	if err != nil {
		t.Fatalf("StatsBetween failed: %v", err)
	}
	if got, want := snap.Totals().ActiveDays, stats.ActiveDays; got != want {
		t.Errorf("active days = %d, want %d", got, want)
	}
	if got, want := snap.Totals().GroupAggregate, stats.Total; got != want {
		t.Errorf("totals = %+v, want %+v", got, want)
	}

	for _, spec := range specs {
		if spec.Kind != AggDimension {
			continue
		}
		rows, totals, err := q.AggregateDimensionView(ctx, dates, DimensionView{
			Dimensions: spec.Dimensions, Aliases: spec.Aliases,
			TitleEn: spec.Name, TitleZh: spec.Name,
		})
		if err != nil {
			t.Fatalf("AggregateDimensionView(%s) failed: %v", spec.Name, err)
		}
		got, ok := snap.Dimension(spec.Name)
		if !ok {
			t.Fatalf("dimension %q not found", spec.Name)
		}
		if !dimensionRowsEqual(got.Rows, rows) {
			t.Errorf("dimension %q rows mismatch\ngot  %+v\nwant %+v", spec.Name, got.Rows, rows)
		}
		if got.Totals != totals {
			t.Errorf("dimension %q totals = %+v, want %+v", spec.Name, got.Totals, totals)
		}
	}

	wantMatrix, err := q.DayHourMatrix(ctx, dates)
	if err != nil {
		t.Fatalf("DayHourMatrix failed: %v", err)
	}
	gotMatrix, ok := snap.DayHour()
	if !ok || !reflect.DeepEqual(gotMatrix, wantMatrix) {
		t.Errorf("day-hour matrix mismatch\ngot  %+v\nwant %+v", gotMatrix, wantMatrix)
	}

	wantSessions, err := q.SessionRows(ctx, dates)
	if err != nil {
		t.Fatalf("SessionRows failed: %v", err)
	}
	gotSessions, ok := snap.Sessions()
	if !ok || !reflect.DeepEqual(gotSessions, wantSessions) {
		t.Errorf("sessions mismatch\ngot  %+v\nwant %+v", gotSessions, wantSessions)
	}
}

// dimensionRowsEqual 比较两条路径的维度结果：Keys 与 Agg 是渲染契约。
// rawKeys 是折叠组内的代表原始键，取决于行序——现有 SQL 路径的
// finalizeVariantKeys 注释同样承认“首遇不可依赖”——不参与渲染，故不作为
// 等价条件；行顺序已由逐行位置比较覆盖。
func dimensionRowsEqual(got, want []DimensionRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !reflect.DeepEqual(got[i].Keys, want[i].Keys) || got[i].Agg != want[i].Agg {
			return false
		}
	}
	return true
}
