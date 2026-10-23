package querier

// range_scan_session_batch_test.go 覆盖会话元数据按 key 分批查询的批边界与
// 语义边界。批边界用例只构造目标 key 的消息，确保实际参与查询的 key 数精确
// 等于 0、1、299、300、301、601；重复 client 与孤儿消息另设语义用例。

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// TestScanRangeSessionMetaBatchBoundaries 验证按 key 分批查询的批边界：
// 0/1/299/300/301/601 对 key 分别对应空集、单条、批内、整批、跨批与多批。
// 每个库只构造目标 key 的会话与消息，不掺入孤儿或重复 client，因此
// loadSessionMetaForKeys 实际收到的 key 数严格等于用例目标值。
func TestScanRangeSessionMetaBatchBoundaries(t *testing.T) {
	for _, keyCount := range []int{0, 1, 299, 300, 301, 601} {
		t.Run(fmt.Sprintf("keys=%d", keyCount), func(t *testing.T) {
			testDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			t.Cleanup(func() { testDB.Close() })
			ctx := context.Background()

			sessions := make([]model.Session, 0, keyCount)
			msgs := make([]model.Message, 0, keyCount)
			keys := make([]sessionKey, 0, keyCount)
			for i := 0; i < keyCount; i++ {
				id := fmt.Sprintf("sess-%04d", i)
				client := fmt.Sprintf("client-%d", i%3)
				sessions = append(sessions, model.Session{
					ID: id, Client: client, Title: fmt.Sprintf("title-%d", i),
					Project: fmt.Sprintf("proj-%d", i%5),
				})
				msgs = append(msgs, model.Message{
					ID: fmt.Sprintf("msg-%04d", i), SessionID: id, Client: client,
					Date: "2026-07-01", TS: int64(1000 + i), TotalTokens: int64(10 + i),
				})
				keys = append(keys, sessionKey{id: id, client: client})
			}
			if len(sessions) > 0 {
				if _, err := db.UpsertSessionMeta(ctx, testDB, sessions); err != nil {
					t.Fatalf("UpsertSessionMeta failed: %v", err)
				}
			}
			if len(msgs) > 0 {
				if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
					t.Fatalf("UpsertMessages failed: %v", err)
				}
			}

			q := New(testDB)
			meta, err := q.loadSessionMetaForKeys(ctx, keys)
			if err != nil {
				t.Fatalf("loadSessionMetaForKeys failed: %v", err)
			}
			if len(meta) != keyCount {
				t.Fatalf("meta keys = %d, want %d", len(meta), keyCount)
			}

			dates := []string{"2026-07-01"}
			want, err := q.SessionRows(ctx, dates)
			if err != nil {
				t.Fatalf("SessionRows failed: %v", err)
			}
			snap, err := q.ScanRange(ctx, dates, []AggSpec{{Kind: AggSession}})
			if err != nil {
				t.Fatalf("ScanRange failed: %v", err)
			}
			got, ok := snap.Sessions()
			if !ok {
				t.Fatal("Sessions() not available")
			}
			if len(got) != keyCount {
				t.Fatalf("session rows = %d, want %d", len(got), keyCount)
			}
			if len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
				t.Fatalf("session rows mismatch:\n got %+v\nwant %+v", got, want)
			}

			stats, err := q.StatsBetween(ctx, dates[0], dates[0])
			if err != nil {
				t.Fatalf("StatsBetween failed: %v", err)
			}
			totals := snap.Totals()
			if stats.ActiveDays != totals.ActiveDays ||
				stats.Total.Requests != totals.Requests ||
				stats.Total.TotalTokens != totals.TotalTokens {
				t.Fatalf("totals mismatch: stats={days:%d requests:%d tokens:%d} snap={days:%d requests:%d tokens:%d}",
					stats.ActiveDays, stats.Total.Requests, stats.Total.TotalTokens,
					totals.ActiveDays, totals.Requests, totals.TotalTokens)
			}
		})
	}
}

// TestScanRangeSessionMetaSemantics 验证同一 session ID 的不同 client 各自成行，
// 孤儿消息（client 不匹配、session 不存在）不计入会话行但计入区间总计。
func TestScanRangeSessionMetaSemantics(t *testing.T) {
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()

	sessions := []model.Session{
		{ID: "sess-dup", Client: "client-a", Title: "a", Project: "proj-a"},
		{ID: "sess-dup", Client: "client-b", Title: "b", Project: "proj-b"},
	}
	if _, err := db.UpsertSessionMeta(ctx, testDB, sessions); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}
	msgs := []model.Message{
		{ID: "m1", SessionID: "sess-dup", Client: "client-a", Date: "2026-07-01", TS: 1000, TotalTokens: 11},
		{ID: "m2", SessionID: "sess-dup", Client: "client-b", Date: "2026-07-01", TS: 2000, TotalTokens: 22},
		// 孤儿一：session 存在但 client 不匹配；孤儿二：session 不存在。
		{ID: "m3", SessionID: "sess-dup", Client: "ghost", Date: "2026-07-01", TS: 3000, TotalTokens: 33},
		{ID: "m4", SessionID: "sess-ghost", Client: "client-a", Date: "2026-07-01", TS: 4000, TotalTokens: 44},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	q := New(testDB)
	dates := []string{"2026-07-01"}
	want, err := q.SessionRows(ctx, dates)
	if err != nil {
		t.Fatalf("SessionRows failed: %v", err)
	}
	snap, err := q.ScanRange(ctx, dates, []AggSpec{{Kind: AggSession}})
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}
	got, ok := snap.Sessions()
	if !ok {
		t.Fatal("Sessions() not available")
	}
	if len(got) != 2 {
		t.Fatalf("session rows = %d, want 2", len(got))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session rows mismatch:\n got %+v\nwant %+v", got, want)
	}

	stats, err := q.StatsBetween(ctx, dates[0], dates[0])
	if err != nil {
		t.Fatalf("StatsBetween failed: %v", err)
	}
	totals := snap.Totals()
	if totals.Requests != 4 || totals.TotalTokens != 110 {
		t.Fatalf("totals = {requests:%d tokens:%d}, want {4 110}（孤儿必须计入区间总计）",
			totals.Requests, totals.TotalTokens)
	}
	if stats.Total.Requests != totals.Requests || stats.Total.TotalTokens != totals.TotalTokens {
		t.Fatalf("totals mismatch: stats=%+v snap=%+v", stats.Total, totals.GroupAggregate)
	}
}
