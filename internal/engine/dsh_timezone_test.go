package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// F38②：落库日期由首次样本时间决定；后续 UPSERT 不把 date 改成新时区日期。
func TestF38OldDatePreserved(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-19", TS: 100, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 同一 TS 在新时区重采：Date 由时区现算为 10-20，但落库日期必须保留 10-19。
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-20", TS: 100, InputTokens: 2, TotalTokens: 2},
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	rows, err := db.FindMessageIDsByClient(ctx, usageDB, model.ClientDSH, []string{"a"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if rows["a"] != "2026-10-19" {
		t.Fatalf("date = %q, want the earliest 2026-10-19", rows["a"])
	}
}

// F38④：删除 0 行且无残留时不告警（幂等完成）。
func TestF38IdempotentDeleteNoWarning(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	plan := []collector.DSHObsoleteMessage{{ID: "missing", SessionID: "s", TS: 1, Date: "2026-10-19"}}
	if err := applyDSHPersistPlan(ctx, tx, plan, []string{"2026-10-19"}, logger); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if strings.Contains(buf.String(), "still present") {
		t.Fatalf("idempotent delete warned: %s", buf.String())
	}
}

// F38⑤：重建数据库后按新时区日期落库，撤销成功。
func TestF38RebuildThenObsolete(t *testing.T) {
	rebuild := func() *db.DB {
		d, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return d
	}
	ctx := context.Background()

	oldDB := rebuild()
	if _, err := db.UpsertMessages(ctx, oldDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-19", TS: 100, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("old seed: %v", err)
	}
	oldDB.Close()

	fresh := rebuild()
	defer fresh.Close()
	newDate := time.UnixMilli(100).In(time.Local).Format("2006-01-02")
	if _, err := db.UpsertMessages(ctx, fresh, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: newDate, TS: 100, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("new seed: %v", err)
	}
	tx, err := fresh.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	plan := []collector.DSHObsoleteMessage{{ID: "a", SessionID: "s", TS: 100, Date: newDate}}
	if err := applyDSHPersistPlan(ctx, tx, plan, []string{newDate}, slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := db.FindMessageIDsByClient(ctx, fresh, model.ClientDSH, []string{"a"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if _, ok := rows["a"]; ok {
		t.Fatalf("rebuild obsolete did not delete: %v", rows)
	}
}

// F38③：日期不符时告警必须包含请求日期与实际落库日期。
func TestF38DateMismatchWarns(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-20", TS: 100, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	plan := []collector.DSHObsoleteMessage{{ID: "a", SessionID: "s", TS: 100, Date: "2026-10-20"}}
	if err := applyDSHPersistPlan(ctx, tx, plan, []string{"2026-10-19"}, logger); err != nil {
		t.Fatalf("apply: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "db_date=2026-10-20") || !strings.Contains(logs, "request_dates=[2026-10-19]") {
		t.Fatalf("warn does not carry both dates: %s", logs)
	}
}
