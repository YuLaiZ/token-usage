package querier

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// TestScanRangeUsesSharedSnapshot 用文件库 WAL 验证 sessions 元数据与
// messages 聚合来自同一读快照：写事务原子地同时更新会话标题与消息总量，
// 读取侧看到的标题版本必须与总量一致；若 ScanRange 拆到不同事务读取，
// 会在并发提交窗口内观察到撕裂组合。
func TestScanRangeUsesSharedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	testDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })

	ctx := context.Background()
	if _, err := db.UpsertSessionMeta(ctx, testDB, []model.Session{
		{ID: "s1", Client: "c1", Title: "v0", Project: "p"},
	}); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}
	if _, err := db.UpsertMessages(ctx, testDB, []model.Message{
		{ID: "m1", SessionID: "s1", Client: "c1", Date: "2026-07-01", TS: 1000, TotalTokens: 0},
	}); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	const rounds = 50
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= rounds; i++ {
			// 放慢提交节奏，让读者能在多个版本之间观察，避免读者次数
			// 依赖调度运气。
			time.Sleep(time.Millisecond)
			tx, err := testDB.BeginTx(ctx, nil)
			if err != nil {
				done <- err
				return
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET title = ? WHERE id = 's1'", fmt.Sprintf("v%d", i)); err != nil {
				tx.Rollback()
				done <- err
				return
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE messages SET total_tokens = ? WHERE id = 'm1'", i*100); err != nil {
				tx.Rollback()
				done <- err
				return
			}
			if err := tx.Commit(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	q := New(testDB)
	checks := 0
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("writer failed: %v", err)
			}
			if checks < rounds/2 {
				t.Fatalf("only %d consistent snapshots observed, want at least %d", checks, rounds/2)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("reader timed out after %d checks", checks)
		}
		snap, err := q.ScanRange(ctx, []string{"2026-07-01"}, []AggSpec{{Kind: AggSession}})
		if err != nil {
			t.Fatalf("ScanRange failed: %v", err)
		}
		sessions, ok := snap.Sessions()
		if !ok || len(sessions) == 0 {
			t.Fatalf("sessions missing: ok=%v len=%d", ok, len(sessions))
		}
		var ver int
		if _, err := fmt.Sscanf(sessions[0].Title, "v%d", &ver); err != nil {
			t.Fatalf("unexpected title %q: %v", sessions[0].Title, err)
		}
		if got, want := sessions[0].Agg.TotalTokens, int64(ver)*100; got != want {
			t.Fatalf("torn snapshot: title=%s total=%d (want %d)", sessions[0].Title, got, want)
		}
		checks++
	}
}

// TestScanRangeReusesOuterReadTx 验证外层已有读事务时 ScanRange 复用同一事务
// （SQLite 不支持嵌套 BEGIN），且后续查询仍可继续执行。
func TestScanRangeReusesOuterReadTx(t *testing.T) {
	q := setupMessageFixture(t)
	ctx := context.Background()
	dates := []string{"2026-07-09", "2026-07-10"}

	err := q.ReadTx(ctx, func(tq *Querier) error {
		snap, err := tq.ScanRange(ctx, dates, []AggSpec{
			{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
			{Kind: AggSession},
		})
		if err != nil {
			return err
		}
		if _, ok := snap.Dimension("client"); !ok {
			t.Fatal("client dimension missing")
		}
		// 复用外层事务后仍可继续查询，证明没有嵌套开启新事务。
		if _, err := tq.StatsBetween(ctx, dates[0], dates[len(dates)-1]); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadTx with ScanRange failed: %v", err)
	}
}

// TestScanRangeCanceledNoPartialResult 验证开始前取消即返回错误且不产生
// 部分快照；调用方无法拿半成品结果。
func TestScanRangeCanceledNoPartialResult(t *testing.T) {
	q := setupMessageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	snap, err := q.ScanRange(ctx, []string{"2026-07-09"}, []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
	})
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
	if snap != nil {
		t.Fatalf("snapshot = %+v, want nil on error", snap)
	}
}

// TestScanRangeCanceledDuringScan 验证扫描进行中取消会及时中止并返回错误，
// 且不返回部分快照。数据量足以让取消落在扫描循环内。
func TestScanRangeCanceledDuringScan(t *testing.T) {
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()

	const rows = 100000
	msgs := make([]model.Message, 0, rows)
	for i := 0; i < rows; i++ {
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("m%06d", i), SessionID: "s1", Client: "c1",
			Date: "2026-07-01", TS: int64(1000 + i), TotalTokens: 1,
		})
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	q := New(testDB)
	scanCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(time.Millisecond)
		cancel()
	}()

	snap, err := q.ScanRange(scanCtx, []string{"2026-07-01"}, []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
		{Kind: AggSession},
	})
	if err == nil {
		t.Fatal("expected cancellation error during scan")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if snap != nil {
		t.Fatalf("snapshot = %+v, want nil on error", snap)
	}
}
