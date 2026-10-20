package engine

import (
	"context"
	"log/slog"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// F34/F36：删除后 UPSERT 失败时整轮回滚，旧行恢复；随后重试成功删除。
func TestPersistClientBatchDSHRollbackOnFailure(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-19", TS: 1, InputTokens: 100, TotalTokens: 100},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 让真实 UPSERT 的新行插入失败：删除先执行，随后 INSERT 触发 ABORT。
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_dsh_insert BEFORE INSERT ON messages
		WHEN NEW.id = 'b' BEGIN SELECT RAISE(ABORT, 'forced upsert failure'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	collected := collector.CollectResult{
		Messages: []model.Message{
			{ID: "b", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-20", TS: 2, InputTokens: 50, TotalTokens: 50},
		},
		DSHObsolete: []collector.DSHObsoleteMessage{{ID: "a", SessionID: "s", TS: 1, Date: "2026-10-19"}},
	}
	err = persistClientBatch(ctx, usageDB, "dsh", collector.CollectRequest{}, collected,
		false, collector.RouterCollectResult{}, nil, slog.Default())
	if err == nil {
		t.Fatalf("expected upsert failure")
	}
	if _, rollbackErr := usageDB.Exec(`DROP TRIGGER fail_dsh_insert`); rollbackErr != nil {
		t.Fatalf("drop trigger: %v", rollbackErr)
	}

	remaining, err := db.FindMessageIDsByClient(ctx, usageDB, model.ClientDSH, []string{"a"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if remaining["a"] != "2026-10-19" {
		t.Fatalf("rollback did not restore row: %v", remaining)
	}
	if _, ok := remaining["b"]; ok {
		t.Fatalf("rolled-back new row exists: %v", remaining)
	}

	if err := persistClientBatch(ctx, usageDB, "dsh", collector.CollectRequest{}, collected,
		false, collector.RouterCollectResult{}, nil, slog.Default()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	remaining, err = db.FindMessageIDsByClient(ctx, usageDB, model.ClientDSH, []string{"a", "b"})
	if err != nil {
		t.Fatalf("find after retry: %v", err)
	}
	if _, ok := remaining["a"]; ok {
		t.Fatalf("retry did not delete old row: %v", remaining)
	}
	if remaining["b"] != "2026-10-20" {
		t.Fatalf("retry did not insert new row: %v", remaining)
	}
}
