package db

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

func dshTestMessage(id, client, date string, ts int64) model.Message {
	return model.Message{
		ID:          id,
		SessionID:   "session-x",
		Client:      client,
		Date:        date,
		TS:          ts,
		InputTokens: 1,
		TotalTokens: 1,
	}
}

func TestDeleteMessagesByClientAndIDsEmptyDates(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	messages := []model.Message{
		dshTestMessage("a", "DeepSeek Harness", "2026-10-19", 1),
		dshTestMessage("b", "DeepSeek Harness", "2026-10-20", 2),
		dshTestMessage("c", "Claude Code", "2026-10-19", 3),
	}
	if _, err := UpsertMessages(ctx, d, messages); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deleted, err := DeleteMessagesByClientAndIDs(ctx, d, "DeepSeek Harness", nil, []string{"a"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	remaining, err := FindMessageIDsByClient(ctx, d, "DeepSeek Harness", []string{"a", "b"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if _, ok := remaining["a"]; ok {
		t.Fatalf("a still present: %v", remaining)
	}
	if remaining["b"] != "2026-10-20" {
		t.Fatalf("b = %q, want 2026-10-20", remaining["b"])
	}
}

func TestDeleteMessagesByClientAndIDsDateMismatchLeavesResidual(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	if _, err := UpsertMessages(ctx, d, []model.Message{
		dshTestMessage("a", "DeepSeek Harness", "2026-10-20", 1),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deleted, err := DeleteMessagesByClientAndIDs(ctx, d, "DeepSeek Harness", []string{"2026-10-19"}, []string{"a"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (date mismatch)", deleted)
	}
	remaining, err := FindMessageIDsByClient(ctx, d, "DeepSeek Harness", []string{"a"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if remaining["a"] != "2026-10-20" {
		t.Fatalf("residual = %v, want a on 2026-10-20", remaining)
	}
}

func TestDeleteMessagesByClientAndIDsEmptyIDsIsNoop(t *testing.T) {
	d := setupTestDB(t)
	deleted, err := DeleteMessagesByClientAndIDs(context.Background(), d, "DeepSeek Harness", nil, nil)
	if err != nil || deleted != 0 {
		t.Fatalf("noop = %d / %v", deleted, err)
	}
}

func TestDeleteMessagesByClientAndIDsChunks(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	ids := make([]string, 0, 600)
	messages := make([]model.Message, 0, 600)
	for i := 0; i < 600; i++ {
		id := "id-" + itoaTest(i)
		ids = append(ids, id)
		messages = append(messages, dshTestMessage(id, "DeepSeek Harness", "2026-10-19", int64(i)))
	}
	if _, err := UpsertMessages(ctx, d, messages); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deleted, err := DeleteMessagesByClientAndIDs(ctx, d, "DeepSeek Harness", nil, ids)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 600 {
		t.Fatalf("deleted = %d, want 600", deleted)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
