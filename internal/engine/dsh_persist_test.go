package engine

import (
	"context"
	"log/slog"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

func TestApplyDSHPersistPlanDeletesWithoutDates(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: "DeepSeek Harness", Date: "2026-10-19", TS: 1, InputTokens: 1, TotalTokens: 1},
		{ID: "b", SessionID: "s", Client: "DeepSeek Harness", Date: "2026-10-20", TS: 2, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	plan := []collector.DSHObsoleteMessage{
		{ID: "a", SessionID: "s", TS: 1, Date: "2026-10-19"},
		{ID: "a", SessionID: "s", TS: 1, Date: "2026-10-19"},
	}
	if err := applyDSHPersistPlan(ctx, tx, plan, nil, slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	remaining, err := db.FindMessageIDsByClient(ctx, usageDB, "DeepSeek Harness", []string{"a", "b"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if _, ok := remaining["a"]; ok {
		t.Fatalf("a should be deleted: %v", remaining)
	}
	if remaining["b"] != "2026-10-20" {
		t.Fatalf("b = %q", remaining["b"])
	}
}

func TestApplyDSHPersistPlanDateMismatchKeepsRow(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: "DeepSeek Harness", Date: "2026-10-20", TS: 1, InputTokens: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	plan := []collector.DSHObsoleteMessage{{ID: "a", SessionID: "s", TS: 1, Date: "2026-10-20"}}
	if err := applyDSHPersistPlan(ctx, tx, plan, []string{"2026-10-19"}, slog.Default()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	remaining, err := db.FindMessageIDsByClient(ctx, usageDB, "DeepSeek Harness", []string{"a"})
	if err != nil || remaining["a"] != "2026-10-20" {
		t.Fatalf("residual = %v / %v", remaining, err)
	}
}

func TestApplyDSHPersistPlanEmptyIsNoop(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	tx, err := usageDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := applyDSHPersistPlan(context.Background(), tx, nil, nil, slog.Default()); err != nil {
		t.Fatalf("empty plan: %v", err)
	}
}
