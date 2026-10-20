package engine

import (
	"context"
	"log/slog"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// 真实入口：engine 传配置 key "dsh"，撤销必须命中落库名 "DeepSeek Harness"。
func TestPersistClientBatchDSHObsoleteUsesDisplayClient(t *testing.T) {
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

	collected := collector.CollectResult{
		DSHObsolete: []collector.DSHObsoleteMessage{{ID: "a", SessionID: "s", TS: 1, Date: "2026-10-19"}},
	}
	if err := persistClientBatch(ctx, usageDB, "dsh", collector.CollectRequest{}, collected,
		false, collector.RouterCollectResult{}, nil, slog.Default()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	remaining, err := db.FindMessageIDsByClient(ctx, usageDB, model.ClientDSH, []string{"a"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if _, ok := remaining["a"]; ok {
		t.Fatalf("old row still present: %v", remaining)
	}
}
