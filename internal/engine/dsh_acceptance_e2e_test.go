package engine

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// F36：删除只影响目标 client，同 ID 的其他 client 行保留。
func TestF36OtherClientRowsUntouched(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "a", SessionID: "s", Client: model.ClientDSH, Date: "2026-10-19", TS: 1, InputTokens: 1, TotalTokens: 1},
		{ID: "a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-10-19", TS: 1, InputTokens: 2, TotalTokens: 2},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deleted, err := db.DeleteMessagesByClientAndIDs(ctx, usageDB, model.ClientDSH, nil, []string{"a"})
	if err != nil || deleted != 1 {
		t.Fatalf("delete = %d / %v", deleted, err)
	}
	remaining, err := db.FindMessageIDsByClient(ctx, usageDB, model.ClientClaudeCode, []string{"a"})
	if err != nil || remaining["a"] != "2026-10-19" {
		t.Fatalf("other client row = %v / %v", remaining, err)
	}
}

// F34：collector 缓存结果在事务失败后仍可用于下一轮成功落库。
func TestF34CollectorCacheAfterPersistFailure(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions", "--tmp-e2e--", "session-e2e")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"type":"session","version":4,"id":"session-e2e","createdAt":1,"cwd":"/tmp/e2e","isSeeded":false,"delegationDepth":0}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":1700000000000,"data":{"turn":1,"step":1,"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150},"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessions, "session.v4.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"dsh": {Paths: map[string]string{"sessions_dir": filepath.Join(home, "sessions")}},
	}}
	c := collector.NewDSHCollector(cfg)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer usageDB.Close()
	ctx := context.Background()

	first, err := c.Collect(ctx, collector.CollectRequest{}, slog.Default())
	if err != nil || len(first.Messages) != 1 {
		t.Fatalf("collect: %v / %d", err, len(first.Messages))
	}
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_e2e BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'forced insert failure'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if err := persistClientBatch(ctx, usageDB, "dsh", collector.CollectRequest{}, first,
		false, collector.RouterCollectResult{}, nil, slog.Default()); err == nil {
		t.Fatalf("expected persist failure")
	}
	if _, err := usageDB.Exec(`DROP TRIGGER fail_e2e`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	second, err := c.Collect(ctx, collector.CollectRequest{}, slog.Default())
	if err != nil || len(second.Messages) != 1 {
		t.Fatalf("cached collect: %v / %d", err, len(second.Messages))
	}
	if err := persistClientBatch(ctx, usageDB, "dsh", collector.CollectRequest{}, second,
		false, collector.RouterCollectResult{}, nil, slog.Default()); err != nil {
		t.Fatalf("retry persist: %v", err)
	}
	rows, err := db.FindMessageIDsByClient(ctx, usageDB, model.ClientDSH, []string{"session-e2e:1:1:0"})
	if err != nil || rows["session-e2e:1:1:0"] == "" {
		t.Fatalf("retry row = %v / %v", rows, err)
	}
}
