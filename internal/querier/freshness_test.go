package querier

import (
	"context"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// setupFreshnessDB 打开一个空的内存库并返回 Querier,由各测试自行插入数据。
func setupFreshnessDB(t *testing.T) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return New(testDB)
}

// insertFreshnessMessages 通过 UpsertMessages 插入消息,验证真实写入链路。
func insertFreshnessMessages(t *testing.T, q *Querier, msgs []model.Message) {
	t.Helper()
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}
}

// insertCollectedAt 手工指定 collected_at 写入 collection_log,
// 固定 UTC 文本以锁定时区转换行为(MarkCollected 由 SQLite 默认值生成时间,不可控)。
func insertCollectedAt(t *testing.T, q *Querier, date, source, collectedAtUTC string) {
	t.Helper()
	_, err := q.db.ExecContext(context.Background(),
		`INSERT INTO collection_log (date, source, session_count, collected_at) VALUES (?, ?, 1, ?)`,
		date, source, collectedAtUTC)
	if err != nil {
		t.Fatalf("insert collection_log failed: %v", err)
	}
}

// TestFreshness_MaxNonZeroTSWithinRange 断言数据截至取统计日期范围内的最大非零 ts:
// 排除 ts=0 记录,也排除范围外更大的时间戳。
func TestFreshness_MaxNonZeroTSWithinRange(t *testing.T) {
	q := setupFreshnessDB(t)
	inRangeMax := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	outOfRange := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	insertFreshnessMessages(t, q, []model.Message{
		{ID: "m-zero", SessionID: "s", Client: "claude", Date: "2026-08-09", TS: 0},
		{ID: "m-in", SessionID: "s", Client: "claude", Date: "2026-08-09", TS: inRangeMax.UnixMilli()},
		{ID: "m-out", SessionID: "s", Client: "codex", Date: "2026-08-15", TS: outOfRange.UnixMilli()},
	})

	fresh, err := q.Freshness(context.Background(), []string{"2026-08-09"})
	if err != nil {
		t.Fatalf("Freshness failed: %v", err)
	}
	if fresh.MaxMessageTS != inRangeMax.UnixMilli() {
		t.Errorf("MaxMessageTS = %d, want range-scoped non-zero %d", fresh.MaxMessageTS, inRangeMax.UnixMilli())
	}
}

// TestFreshness_NoMessagesInRange 断言范围内无消息事件时 MaxMessageTS 为 0(展示为 em dash)。
func TestFreshness_NoMessagesInRange(t *testing.T) {
	q := setupFreshnessDB(t)
	inRange := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	insertFreshnessMessages(t, q, []model.Message{
		{ID: "m-only-zero", SessionID: "s", Client: "claude", Date: "2026-08-09", TS: 0},
		{ID: "m-other-date", SessionID: "s", Client: "claude", Date: "2026-08-10", TS: inRange.UnixMilli()},
	})

	fresh, err := q.Freshness(context.Background(), []string{"2026-08-09"})
	if err != nil {
		t.Fatalf("Freshness failed: %v", err)
	}
	if fresh.MaxMessageTS != 0 {
		t.Errorf("MaxMessageTS = %d, want 0 when no non-zero ts in range", fresh.MaxMessageTS)
	}
}

// TestFreshness_LastCollectionIsMaxUTCTimestampAsLocal 断言最近成功采集取全库
// collected_at 最大值(字典序即时间序),并以本机时区返回时刻。
func TestFreshness_LastCollectionIsMaxUTCTimestampAsLocal(t *testing.T) {
	q := setupFreshnessDB(t)
	insertCollectedAt(t, q, "2026-08-01", "claude", "2026-08-01 03:00:00")
	wantUTC := time.Date(2026, 8, 20, 9, 30, 45, 0, time.UTC)
	insertCollectedAt(t, q, "2026-08-20", "codex", wantUTC.Format("2006-01-02 15:04:05"))

	fresh, err := q.Freshness(context.Background(), []string{"2026-08-01"})
	if err != nil {
		t.Fatalf("Freshness failed: %v", err)
	}
	if !fresh.LastCollection.Equal(wantUTC) {
		t.Errorf("LastCollection = %v, want %v (instant equality)", fresh.LastCollection, wantUTC)
	}
	if fresh.LastCollection.Location() != time.Local {
		t.Errorf("LastCollection location = %v, want local timezone", fresh.LastCollection.Location())
	}
}

// TestFreshness_EmptyDatabaseZeroValues 断言空库下两项均为零值边界。
func TestFreshness_EmptyDatabaseZeroValues(t *testing.T) {
	q := setupFreshnessDB(t)

	fresh, err := q.Freshness(context.Background(), []string{"2026-08-09"})
	if err != nil {
		t.Fatalf("Freshness failed: %v", err)
	}
	if fresh.MaxMessageTS != 0 || !fresh.LastCollection.IsZero() {
		t.Errorf("empty database should give zero values, got %+v", fresh)
	}
}

// TestFreshness_CollectionLogIndependentOfQueryDates 断言最近成功采集是全库口径:
// 与本次统计日期无关的历史记录同样参与取最大。
func TestFreshness_CollectionLogIndependentOfQueryDates(t *testing.T) {
	q := setupFreshnessDB(t)
	inRange := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	insertFreshnessMessages(t, q, []model.Message{
		{ID: "m1", SessionID: "s", Client: "claude", Date: "2026-08-09", TS: inRange.UnixMilli()},
	})
	wantUTC := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	insertCollectedAt(t, q, "2025-01-01", "zcode", wantUTC.Format("2006-01-02 15:04:05"))

	fresh, err := q.Freshness(context.Background(), []string{"2026-08-09"})
	if err != nil {
		t.Fatalf("Freshness failed: %v", err)
	}
	if !fresh.LastCollection.Equal(wantUTC) {
		t.Errorf("LastCollection = %v, want all-history max %v", fresh.LastCollection, wantUTC)
	}
	if fresh.MaxMessageTS == 0 {
		t.Error("in-range message ts should be reported")
	}
}

// TestFreshness_RejectsNilDB 复用 readyContext 的空库防御。
func TestFreshness_RejectsNilDB(t *testing.T) {
	var q *Querier
	if _, err := q.Freshness(context.Background(), []string{"2026-08-09"}); err == nil {
		t.Fatal("nil querier must return error")
	}
}

// TestDateBounds 锁定导出的日期边界函数:最小/最大按 YYYY-MM-DD 字典序即时间序。
func TestDateBounds(t *testing.T) {
	first, last := DateBounds([]string{"2026-08-10", "2026-08-08", "2026-08-09"})
	if first != "2026-08-08" || last != "2026-08-10" {
		t.Errorf("DateBounds = (%q, %q), want (2026-08-08, 2026-08-10)", first, last)
	}
	singleFirst, singleLast := DateBounds([]string{"2026-08-09"})
	if singleFirst != "2026-08-09" || singleLast != "2026-08-09" {
		t.Errorf("single-date bounds = (%q, %q), want same day twice", singleFirst, singleLast)
	}
}
