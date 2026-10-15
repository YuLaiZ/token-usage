package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

func TestDashboardFreshnessUpdatesAndRemainsGlobal(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := NewServer(querier.New(store), "test")
	// 查询空的历史区间，时间字段仍表示全库快照，不随选区变窄或归零。
	url := "/api/dashboard?from=2025-01-01&to=2025-01-01"
	check := func(through, collected any) {
		t.Helper()
		r := doGet(h, url)
		if r.Code != http.StatusOK {
			t.Fatalf("status %d: %s", r.Code, r.Body.String())
		}
		body := decodeJSON(t, r)
		if _, ok := body["data_through"]; !ok {
			t.Fatal("missing data_through field, including for an empty database")
		}
		if _, ok := body["last_collection"]; !ok {
			t.Fatal("missing last_collection field, including for an empty database")
		}
		if body["data_through"] != through || body["last_collection"] != collected {
			t.Fatalf("freshness %v/%v, want %v/%v", body["data_through"], body["last_collection"], through, collected)
		}
		if body["totals"].(map[string]any)["total"] != float64(0) {
			t.Fatal("global freshness must not change the historical range totals")
		}
	}
	check(nil, nil)
	ctx := context.Background()
	for _, hour := range []int{10, 11} {
		at := time.Date(2026, 10, 15, hour, 0, 0, 0, time.Local)
		_, err := db.UpsertMessages(ctx, store, []model.Message{{ID: at.Format("150405"), SessionID: "s", Client: model.ClientCodexApp, Date: "2026-10-15", TS: at.UnixMilli(), TotalTokens: 12}})
		if err != nil {
			t.Fatal(err)
		}
		collected := at.Add(5 * time.Second)
		_, err = store.ExecContext(ctx, `INSERT OR REPLACE INTO collection_log(date,source,session_count,collected_at) VALUES(?,?,?,?)`, "2026-10-15", "codex", 1, collected.UTC().Format(time.DateTime))
		if err != nil {
			t.Fatal(err)
		}
		check(at.Format(time.DateTime), collected.Format(time.DateTime))
		meta := decodeJSON(t, doGet(h, "/api/meta"))
		if meta["data_through"] != at.Format(time.DateTime) || meta["last_collection"] != collected.Format(time.DateTime) {
			t.Fatalf("meta/dashboard freshness differs: %v", meta)
		}
	}
}
