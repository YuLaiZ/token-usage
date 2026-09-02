package db

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

func setupTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecordError_And_GetUnresolved(t *testing.T) {
	db := setupTestDB(t)

	RecordError(context.Background(), db, "2026-06-09", "claude", "test error", "detail text")

	errors, err := GetUnresolvedErrors(db)
	if err != nil {
		t.Fatalf("GetUnresolvedErrors failed: %v", err)
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].Message != "test error" {
		t.Errorf("error message = %q, want %q", errors[0].Message, "test error")
	}
}

func TestRecordWarning_And_GetUnresolved(t *testing.T) {
	db := setupTestDB(t)

	RecordWarning(context.Background(), db, "2026-06-09", "claude", "test warning", "detail text")

	errors, err := GetUnresolvedErrors(db)
	if err != nil {
		t.Fatalf("GetUnresolvedErrors failed: %v", err)
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].ErrorType != "warning" {
		t.Errorf("error type = %q, want %q", errors[0].ErrorType, "warning")
	}
}

func TestResolveError(t *testing.T) {
	db := setupTestDB(t)

	RecordError(context.Background(), db, "2026-06-09", "claude", "test error", "")

	errors, _ := GetUnresolvedErrors(db)
	ResolveError(context.Background(), db, errors[0].ID)

	errors, _ = GetUnresolvedErrors(db)
	if len(errors) != 0 {
		t.Errorf("expected 0 unresolved errors, got %d", len(errors))
	}
}

func TestMarkCollected(t *testing.T) {
	db := setupTestDB(t)

	err := MarkCollected(context.Background(), db, "2026-06-09", "claude", 5)
	if err != nil {
		t.Fatalf("MarkCollected failed: %v", err)
	}
}

func TestUpsertRawClientSessions_Insert(t *testing.T) {
	db := setupTestDB(t)

	sessions := []model.RawClientSession{
		{
			SessionID:         "test-001",
			Client:            model.RawClientClaudeCode,
			Directory:         "/Users/test/IdeaProjects/my-project",
			Model:             "claude-sonnet-4-20250514",
			Title:             "fix-login-bug",
			CreatedAt:         1749380400000,
			LastActiveAt:      1749380660000,
			InputTokens:       2000,
			OutputTokens:      800,
			CacheReadTokens:   300,
			CacheCreateTokens: 100,
			TotalTokens:       3200,
			RawData:           `{"entrypoint":"cli"}`,
			SourceFile:        "/test/session-1.jsonl",
			FileMtime:         1749380660000,
			FileSize:          1024,
		},
	}

	count, err := UpsertRawClientSessions(context.Background(), db, sessions)
	if err != nil {
		t.Fatalf("UpsertRawClientSessions failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
}

func TestUpsertRawClientSessions_Update(t *testing.T) {
	db := setupTestDB(t)

	sessions := []model.RawClientSession{
		{
			SessionID:    "test-001",
			Client:       model.RawClientClaudeCode,
			Directory:    "/Users/test/IdeaProjects/my-project",
			Model:        "claude-sonnet-4-20250514",
			InputTokens:  2000,
			OutputTokens: 800,
			TotalTokens:  2800,
			SourceFile:   "/test/session-1.jsonl",
			FileMtime:    1749380660000,
			FileSize:     1024,
		},
	}

	UpsertRawClientSessions(context.Background(), db, sessions)

	sessions[0].InputTokens = 5000
	sessions[0].TotalTokens = 5800
	sessions[0].FileMtime = 1749380700000

	count, err := UpsertRawClientSessions(context.Background(), db, sessions)
	if err != nil {
		t.Fatalf("UpsertRawClientSessions failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
}

func TestGetCollectedDates(t *testing.T) {
	db := setupTestDB(t)

	MarkCollected(context.Background(), db, "2026-06-08", "claude", 5)
	MarkCollected(context.Background(), db, "2026-06-09", "claude", 3)
	MarkCollected(context.Background(), db, "2026-06-08", "opencode", 2)

	dates, err := GetCollectedDates(db, "claude")
	if err != nil {
		t.Fatalf("GetCollectedDates failed: %v", err)
	}

	if len(dates) != 2 {
		t.Errorf("expected 2 dates, got %d: %v", len(dates), dates)
	}
}

func TestRecordErrorsByDate_StoresOneRowPerDateAtomically(t *testing.T) {
	d := setupTestDB(t)
	dates := []string{"2026-06-22", "2026-06-23"}
	if err := RecordErrorsByDate(context.Background(), d, dates, "claude", "boom", ""); err != nil {
		t.Fatal(err)
	}
	got, err := GetErrors(d, ErrorFilter{Dates: dates, Source: "claude"})
	if err != nil || len(got) != 2 {
		t.Fatalf("errors = %+v, err = %v", got, err)
	}
}

func TestRecordErrorsByDate_RollsBackWholeBatch(t *testing.T) {
	d := setupTestDB(t)
	if _, err := d.Exec(`CREATE TRIGGER fail_second_error BEFORE INSERT ON collection_errors
		WHEN NEW.date = '2026-06-23'
		BEGIN SELECT RAISE(ABORT, 'forced error insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-06-22", "2026-06-23"}, "claude", "boom", ""); err == nil {
		t.Fatal("expected batch insert failure")
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM collection_errors`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial error batch persisted: %d", count)
	}
}

func TestRecordErrorsByDate_DeduplicatesOnlyUnresolvedSameError(t *testing.T) {
	d := setupTestDB(t)
	for i := 0; i < 2; i++ {
		if err := RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "boom", "detail"); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := GetErrors(d, ErrorFilter{Dates: []string{"2026-06-23"}, Source: "claude"})
	if len(all) != 1 {
		t.Fatalf("duplicate unresolved rows: %+v", all)
	}
	if _, err := ResolveErrorsByDateSource(context.Background(), d, "2026-06-23", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "boom", "recurred"); err != nil {
		t.Fatal(err)
	}
	all, _ = GetErrors(d, ErrorFilter{Dates: []string{"2026-06-23"}, Source: "claude"})
	if len(all) != 2 || all[0].Resolved || !all[1].Resolved {
		t.Fatalf("recurrence must create new unresolved history row: %+v", all)
	}
}

func TestIncrementRetryCountByDateSource_UpdatesWholeGroup(t *testing.T) {
	d := setupTestDB(t)
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "first", "")
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "second", "")
	RecordWarning(context.Background(), d, "2026-06-23", "claude", "degraded", "")
	updated, err := IncrementRetryCountByDateSource(context.Background(), d, "2026-06-23", "claude")
	if err != nil || updated != 2 {
		t.Fatalf("updated = %d, err = %v", updated, err)
	}
	got, _ := GetErrors(d, ErrorFilter{Dates: []string{"2026-06-23"}, Source: "claude", Unresolved: true})
	for _, e := range got {
		if e.ErrorType == "error" && e.RetryCount != 1 {
			t.Fatalf("retry count not updated atomically: %+v", got)
		}
		if e.ErrorType == "warning" && e.RetryCount != 0 {
			t.Fatalf("warning must not be retried: %+v", got)
		}
	}
}

func TestResolveErrorsByDateSource_ResolvesWholeGroupOnly(t *testing.T) {
	d := setupTestDB(t)
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "first", "")
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "claude", "second", "")
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-23"}, "codex", "other", "")
	resolved, err := ResolveErrorsByDateSource(context.Background(), d, "2026-06-23", "claude")
	if err != nil || resolved != 2 {
		t.Fatalf("resolved = %d, err = %v", resolved, err)
	}
	remaining, _ := GetErrors(d, ErrorFilter{Unresolved: true})
	if len(remaining) != 1 || remaining[0].Source != "codex" {
		t.Fatalf("unexpected unresolved rows: %+v", remaining)
	}
}

func TestGetErrors_DatesFilter(t *testing.T) {
	d := setupTestDB(t)
	RecordErrorsByDate(context.Background(), d, []string{"2026-06-21", "2026-06-22", "2026-06-23"}, "claude", "boom", "")
	got, err := GetErrors(d, ErrorFilter{Dates: []string{"2026-06-21", "2026-06-23"}})
	if err != nil || len(got) != 2 {
		t.Fatalf("errors = %+v, err = %v", got, err)
	}
}

func TestGetErrors_TypeFilter(t *testing.T) {
	d := setupTestDB(t)
	RecordError(context.Background(), d, "2026-06-23", "claude", "error", "")
	RecordWarning(context.Background(), d, "2026-06-23", "claude", "warning", "")
	got, err := GetErrors(d, ErrorFilter{Type: "error", Unresolved: true})
	if err != nil || len(got) != 1 || got[0].ErrorType != "error" {
		t.Fatalf("errors = %+v, err = %v", got, err)
	}
}

func TestGetFileScanLogs(t *testing.T) {
	db := setupTestDB(t)

	UpsertFileScanLog(context.Background(), db, []model.FileScanLog{
		{
			Client:        "claude",
			FilePath:      "/test/session-1.jsonl",
			FileIdentity:  "100:200",
			MtimeNS:       1749380660000000000,
			FileSize:      1024,
			ParserVersion: ParserVersion,
		},
	})

	logs, err := GetFileScanLogs(context.Background(), db, "claude")
	if err != nil {
		t.Fatalf("GetFileScanLogs failed: %v", err)
	}

	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}

	log, ok := logs["/test/session-1.jsonl"]
	if !ok {
		t.Fatal("expected log for /test/session-1.jsonl")
	}
	if log.FileIdentity != "100:200" {
		t.Errorf("FileIdentity = %q, want %q", log.FileIdentity, "100:200")
	}
	if log.ParserVersion != ParserVersion {
		t.Errorf("ParserVersion = %d, want %d", log.ParserVersion, ParserVersion)
	}
}

func scanMessage(t *testing.T, db *DB, client, id string) model.Message {
	t.Helper()
	var m model.Message
	m.Client = client
	m.ID = id
	err := db.QueryRow(`SELECT session_id, date, ts, model, provider, router_provider,
		router_model, router_name, directory, project,
		input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens,
		cache_create_tokens, reasoning_tokens, total_tokens
		FROM messages WHERE client=? AND id=?`, client, id).Scan(
		&m.SessionID, &m.Date, &m.TS, &m.Model, &m.Provider, &m.RouterProvider,
		&m.RouterModel, &m.RouterName, &m.Directory, &m.Project,
		&m.InputTokens, &m.FreshInputTokens, &m.OutputTokens, &m.CacheReadTokens,
		&m.CacheCreateTokens, &m.ReasoningTokens, &m.TotalTokens,
	)
	if err != nil {
		t.Fatalf("scanMessage %s/%s: %v", client, id, err)
	}
	return m
}

func TestUpsertMessages(t *testing.T) {
	tests := []struct {
		name    string
		steps   []model.Message
		check   func(t *testing.T, db *DB)
		wantErr bool
	}{
		{
			// 19 个消息字段原样写入
			name: "行为 19 fields round-trip",
			steps: []model.Message{
				{
					ID: "m1", SessionID: "s1", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/p", Project: "proj",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100, FreshInputTokens: 80,
					OutputTokens: 20, CacheReadTokens: 20, CacheCreateTokens: 5,
					ReasoningTokens: 5, TotalTokens: 120,
					RouterProvider: "Provider A", RouterModel: "route-a", RouterName: "cc_switch",
				},
			},
			check: func(t *testing.T, db *DB) {
				m := scanMessage(t, db, model.ClientCodexCLI, "m1")
				if m.SessionID != "s1" || m.Date != "2026-07-10" || m.TS != 2000 ||
					m.Model != "gpt-5.5" || m.Provider != "Zhipu" || m.Directory != "/p" ||
					m.Project != "proj" || m.InputTokens != 100 || m.FreshInputTokens != 80 ||
					m.OutputTokens != 20 || m.CacheReadTokens != 20 || m.CacheCreateTokens != 5 ||
					m.ReasoningTokens != 5 || m.TotalTokens != 120 ||
					m.RouterProvider != "Provider A" || m.RouterModel != "route-a" || m.RouterName != "cc_switch" {
					t.Errorf("fields not round-tripped: %+v", m)
				}
			},
		},
		{
			// 更早 ts 的同 ID 来时，date/session/directory/project 替换为旧值
			name: "行为 earlier ts replaces attribution",
			steps: []model.Message{
				{
					ID: "m1", SessionID: "fork", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/fork", Project: "fork",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100,
					FreshInputTokens: 80, OutputTokens: 20, CacheReadTokens: 20,
					ReasoningTokens: 5, TotalTokens: 120,
					RouterProvider: "Provider A", RouterModel: "route-a", RouterName: "cc_switch",
				},
				{
					ID: "m1", SessionID: "parent", Client: model.ClientCodexCLI,
					Date: "2026-07-09", TS: 1000, Directory: "/parent", Project: "parent",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 200,
					FreshInputTokens: 180, OutputTokens: 30, CacheReadTokens: 10,
					ReasoningTokens: 5, TotalTokens: 230,
					RouterProvider: "", RouterModel: "", RouterName: "",
				},
			},
			check: func(t *testing.T, db *DB) {
				m := scanMessage(t, db, model.ClientCodexCLI, "m1")
				// 归因来自 parent（更早 ts）
				if m.SessionID != "parent" || m.Date != "2026-07-09" ||
					m.Directory != "/parent" || m.Project != "parent" {
					t.Errorf("attribution not replaced by earlier ts: %+v", m)
				}
				// token 来自 parent（excluded 总是覆盖）
				if m.InputTokens != 200 || m.TotalTokens != 230 {
					t.Errorf("tokens not updated: %+v", m)
				}
				// router 仍保留 Provider A（空不清除）
				if m.RouterProvider != "Provider A" || m.RouterModel != "route-a" ||
					m.RouterName != "cc_switch" {
					t.Errorf("router not retained: %+v", m)
				}
			},
		},
		{
			// 相同 ts 不同 directory，已有归因不变
			name: "行为 same ts keeps existing attribution",
			steps: []model.Message{
				{
					ID: "m1", SessionID: "fork", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/fork", Project: "fork",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100,
					FreshInputTokens: 80, OutputTokens: 20, CacheReadTokens: 20,
					ReasoningTokens: 5, TotalTokens: 120,
					RouterProvider: "Provider A", RouterModel: "route-a", RouterName: "cc_switch",
				},
				{
					ID: "m1", SessionID: "other", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/other", Project: "other",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100,
					FreshInputTokens: 80, OutputTokens: 20, CacheReadTokens: 20,
					ReasoningTokens: 5, TotalTokens: 120,
					RouterProvider: "", RouterModel: "", RouterName: "",
				},
			},
			check: func(t *testing.T, db *DB) {
				m := scanMessage(t, db, model.ClientCodexCLI, "m1")
				// 相同 ts：excluded.ts < messages.ts 为 false，保留旧值
				if m.SessionID != "fork" || m.Date != "2026-07-10" ||
					m.Directory != "/fork" || m.Project != "fork" {
					t.Errorf("attribution should be unchanged on equal ts: %+v", m)
				}
				if m.RouterProvider != "Provider A" {
					t.Errorf("router should be retained: %+v", m)
				}
			},
		},
		{
			// 空 router 不清除已有值
			name: "行为 empty router does not clear",
			steps: []model.Message{
				{
					ID: "m1", SessionID: "s1", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/p", Project: "proj",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100, TotalTokens: 120,
					RouterProvider: "Provider A", RouterModel: "route-a", RouterName: "cc_switch",
				},
				{
					ID: "m1", SessionID: "s1", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/p", Project: "proj",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100, TotalTokens: 120,
				},
			},
			check: func(t *testing.T, db *DB) {
				m := scanMessage(t, db, model.ClientCodexCLI, "m1")
				if m.RouterProvider != "Provider A" || m.RouterModel != "route-a" || m.RouterName != "cc_switch" {
					t.Errorf("empty router should not clear existing: %+v", m)
				}
			},
		},
		{
			// 冲突时 token/model/provider 更新
			name: "行为 conflict updates tokens model provider",
			steps: []model.Message{
				{
					ID: "m1", SessionID: "s1", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/p", Project: "proj",
					Model: "gpt-5.5", Provider: "Zhipu", InputTokens: 100, OutputTokens: 20,
					TotalTokens: 120,
				},
				{
					ID: "m1", SessionID: "s1", Client: model.ClientCodexCLI,
					Date: "2026-07-10", TS: 2000, Directory: "/p", Project: "proj",
					Model: "glm-5.5", Provider: "OpenAI", InputTokens: 999, OutputTokens: 1,
					TotalTokens: 1000,
				},
			},
			check: func(t *testing.T, db *DB) {
				m := scanMessage(t, db, model.ClientCodexCLI, "m1")
				if m.Model != "glm-5.5" || m.Provider != "OpenAI" ||
					m.InputTokens != 999 || m.OutputTokens != 1 || m.TotalTokens != 1000 {
					t.Errorf("tokens/model/provider not updated on conflict: %+v", m)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			for _, msg := range tc.steps {
				count, err := UpsertMessages(context.Background(), db, []model.Message{msg})
				if err != nil {
					t.Fatalf("UpsertMessages failed: %v", err)
				}
				if count != 1 {
					t.Errorf("count = %d, want 1", count)
				}
			}
			if tc.check != nil {
				tc.check(t, db)
			}
		})
	}
}

// UpsertSessionMeta 只写元数据
func TestUpsertSessionMeta(t *testing.T) {
	db := setupTestDB(t)
	_, err := UpsertSessionMeta(context.Background(), db, []model.Session{
		{
			ID: "s1", Client: model.ClientCodexCLI, Directory: "/p", Project: "proj",
			Title: "hello", ParentID: "root", FirstTS: 100, LastTS: 200,
		},
	})
	if err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}
	var directory, project, title, parentID string
	var firstTS, lastTS int64
	if err := db.QueryRow(`SELECT directory, project, title, parent_id, first_ts, last_ts
		FROM sessions WHERE id=? AND client=?`, "s1", model.ClientCodexCLI).Scan(
		&directory, &project, &title, &parentID, &firstTS, &lastTS); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if directory != "/p" || project != "proj" || title != "hello" || parentID != "root" ||
		firstTS != 100 || lastTS != 200 {
		t.Errorf("meta not written: dir=%q proj=%q title=%q parent=%q first=%d last=%d",
			directory, project, title, parentID, firstTS, lastTS)
	}

	// 二次写入：first_ts 取较小、last_ts 取较大
	_, err = UpsertSessionMeta(context.Background(), db, []model.Session{
		{
			ID: "s1", Client: model.ClientCodexCLI, Directory: "/p2", Project: "proj2",
			Title: "hello2", ParentID: "root2", FirstTS: 50, LastTS: 300,
		},
	})
	if err != nil {
		t.Fatalf("UpsertSessionMeta update failed: %v", err)
	}
	if err := db.QueryRow(`SELECT directory, project, title, parent_id, first_ts, last_ts
		FROM sessions WHERE id=? AND client=?`, "s1", model.ClientCodexCLI).Scan(
		&directory, &project, &title, &parentID, &firstTS, &lastTS); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if directory != "/p2" || project != "proj2" || title != "hello2" || parentID != "root2" {
		t.Errorf("meta not updated on conflict: %+v", directory)
	}
	if firstTS != 50 {
		t.Errorf("first_ts should be 50 (min), got %d", firstTS)
	}
	if lastTS != 300 {
		t.Errorf("last_ts should be 300 (max), got %d", lastTS)
	}
}

// cursor round-trip 多 source
func TestSyncCursors(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	cursors := map[string]model.SyncCursor{
		"opencode_message": {Value: 100, ID: "m1"},
		"opencode_event":   {Value: 20, ID: "evt20"},
	}
	if err := SetSyncCursors(ctx, db, "opencode", cursors); err != nil {
		t.Fatalf("SetSyncCursors failed: %v", err)
	}

	// 重开查询
	got, err := GetSyncCursors(ctx, db, "opencode", []string{"opencode_message", "opencode_event"})
	if err != nil {
		t.Fatalf("GetSyncCursors failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 cursors, got %d", len(got))
	}
	if got["opencode_message"].Value != 100 || got["opencode_message"].ID != "m1" {
		t.Errorf("opencode_message cursor = %+v, want {100 m1}", got["opencode_message"])
	}
	if got["opencode_event"].Value != 20 || got["opencode_event"].ID != "evt20" {
		t.Errorf("opencode_event cursor = %+v, want {20 evt20}", got["opencode_event"])
	}

	// 缺失 source 返回零值
	got2, err := GetSyncCursors(ctx, db, "opencode", []string{"missing_source"})
	if err != nil {
		t.Fatalf("GetSyncCursors missing failed: %v", err)
	}
	if c, ok := got2["missing_source"]; !ok || c.Value != 0 || c.ID != "" {
		t.Errorf("missing source should be zero cursor, got %+v ok=%v", c, ok)
	}
}

// QueryRouterLogsByMessageIDs 按 router_name 隔离 + app_type→client 映射 + 首条优先
func TestQueryRouterLogsByMessageIDs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// 插入目标 router 的两条日志（claude 与 claude-desktop 两个 app_type）
	logs := []model.RouterLog{
		{RequestID: "r1", MessageID: "msgA", RouterName: "cc_switch", AppType: "claude",
			ProviderName: "Provider A", Model: "route-a", CreatedAt: 100},
		{RequestID: "r2", MessageID: "msgA", RouterName: "cc_switch", AppType: "claude-desktop",
			ProviderName: "Provider B", Model: "route-b", CreatedAt: 50},
		{RequestID: "r3", MessageID: "msgB", RouterName: "cc_switch", AppType: "claude",
			ProviderName: "Provider C", Model: "route-c", CreatedAt: 200},
		// 另一个 router_name 的同 message_id，不应串线
		{RequestID: "r4", MessageID: "msgA", RouterName: "other_router", AppType: "claude",
			ProviderName: "Wrong Provider", Model: "wrong", CreatedAt: 10},
	}
	if _, err := UpsertRawRouterLogs(ctx, db, logs); err != nil {
		t.Fatalf("UpsertRawRouterLogs failed: %v", err)
	}

	got, err := QueryRouterLogsByMessageIDs(ctx, db, "cc_switch", []string{"msgA", "msgB"})
	if err != nil {
		t.Fatalf("QueryRouterLogsByMessageIDs failed: %v", err)
	}
	// 每个 (client, message_id) 只保留首条；msgA 跨两个 client（claude + claude-desktop）
	byKey := make(map[string]model.RouterAttribution)
	for _, a := range got {
		byKey[a.Client+"\x00"+a.MessageID] = a
	}
	if len(byKey) != 3 {
		t.Fatalf("expected 3 unique (client,message_id) attributions, got %d: %+v", len(byKey), got)
	}
	// msgA/Claude Desktop：createdAt=50 早于同 client 的其他条目（此 client 仅一条）
	a, ok := byKey[model.ClientClaudeDesktop+"\x00msgA"]
	if !ok {
		t.Fatalf("expected Claude Desktop attribution for msgA, got: %+v", byKey)
	}
	if a.Provider != "Provider B" || a.Model != "route-b" {
		t.Errorf("msgA Claude Desktop = %+v, want Provider B/route-b", a)
	}
	// msgA/Claude Code：createdAt=100
	a2, ok := byKey[model.ClientClaudeCode+"\x00msgA"]
	if !ok {
		t.Fatalf("expected Claude Code attribution for msgA, got: %+v", byKey)
	}
	if a2.Provider != "Provider A" || a2.Model != "route-a" {
		t.Errorf("msgA Claude Code = %+v, want Provider A/route-a", a2)
	}
	// msgB/Claude Code：唯一
	b, ok := byKey[model.ClientClaudeCode+"\x00msgB"]
	if !ok {
		t.Fatalf("expected Claude Code attribution for msgB, got: %+v", byKey)
	}
	if b.Provider != "Provider C" || b.Model != "route-c" {
		t.Errorf("msgB Claude Code = %+v, want Provider C/route-c", b)
	}
	// 不应出现 other_router 的数据（router_name 隔离）
	for _, a := range got {
		if a.Provider == "Wrong Provider" {
			t.Errorf("other_router data leaked: %+v", a)
		}
	}
}

// 1001 个 message ID 分块查询（实现 500 分块，验证大输入不报错）
func TestQueryRouterLogsByMessageIDs_LargeBatch(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logs := make([]model.RouterLog, 0, 1001)
	for i := 0; i < 1001; i++ {
		logs = append(logs, model.RouterLog{
			RequestID: "r" + itoa(i), MessageID: "msg" + itoa(i),
			RouterName: "cc_switch", AppType: "claude", ProviderName: "P", CreatedAt: int64(i),
		})
	}
	if _, err := UpsertRawRouterLogs(ctx, db, logs); err != nil {
		t.Fatalf("UpsertRawRouterLogs failed: %v", err)
	}

	ids := make([]string, 1001)
	for i := 0; i < 1001; i++ {
		ids[i] = "msg" + itoa(i)
	}
	got, err := QueryRouterLogsByMessageIDs(ctx, db, "cc_switch", ids)
	if err != nil {
		t.Fatalf("QueryRouterLogsByMessageIDs large batch failed: %v", err)
	}
	if len(got) != 1001 {
		t.Errorf("expected 1001 attributions, got %d", len(got))
	}
}

// BackfillRouterFields 非空覆盖
func TestBackfillRouterFields(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// 先写入一条无 router 归因的消息
	if _, err := UpsertMessages(ctx, db, []model.Message{
		{ID: "m1", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-07-10", TS: 2000, Model: "x", TotalTokens: 10},
	}); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	// 回填 router 归因
	infos := []model.RouterAttribution{
		{Client: model.ClientClaudeCode, MessageID: "m1", Provider: "Zhipu GLM",
			Model: "glm-5.5", RouterName: "cc_switch"},
	}
	n, err := BackfillRouterFields(ctx, db, infos)
	if err != nil {
		t.Fatalf("BackfillRouterFields failed: %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1", n)
	}

	m := scanMessage(t, db, model.ClientClaudeCode, "m1")
	if m.RouterProvider != "Zhipu GLM" || m.RouterModel != "glm-5.5" || m.RouterName != "cc_switch" {
		t.Errorf("router fields not backfilled: %+v", m)
	}
}

// itoa 简易整数转字符串（避免在测试里引入 strconv 的额外导入噪声）
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestGetMessageIDsByDisplayNames_SingleDisplayName(t *testing.T) {
	db := setupTestDB(t) // 复用 dao_test.go:10 的 setupTestDB helper
	ctx := context.Background()

	// 插入 client="OpenCode" 的两条 message
	_, err := UpsertMessages(ctx, db, []model.Message{
		{ID: "msg1", Client: model.ClientOpenCode, Date: "2026-07-01", TS: 1},
		{ID: "msg2", Client: model.ClientOpenCode, Date: "2026-07-02", TS: 2},
	})
	if err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	ids, err := GetMessageIDsByDisplayNames(ctx, db, []string{model.ClientOpenCode})
	if err != nil {
		t.Fatalf("GetMessageIDsByDisplayNames failed: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 ids, got %d: %v", len(ids), ids)
	}
}

// TestGetMessageIDsByDisplayNames_ClaudeMultiMapping C2 核心验证：
// claude 配置 key 对应 Claude Code + Claude Desktop 两个显示名，必须都能查到。
func TestGetMessageIDsByDisplayNames_ClaudeMultiMapping(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	_, err := UpsertMessages(ctx, db, []model.Message{
		{ID: "cc1", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 1},
		{ID: "cd1", Client: model.ClientClaudeDesktop, Date: "2026-07-01", TS: 2},
		{ID: "oc1", Client: model.ClientOpenCode, Date: "2026-07-01", TS: 3},
	})
	if err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	ids, err := GetMessageIDsByDisplayNames(ctx, db, []string{model.ClientClaudeCode, model.ClientClaudeDesktop})
	if err != nil {
		t.Fatalf("GetMessageIDsByDisplayNames failed: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 ids (cc1+cd1), got %d: %v", len(ids), ids)
	}
	// OpenCode 的 oc1 不应出现
	for _, id := range ids {
		if id == "oc1" {
			t.Errorf("OpenCode message should not be returned for Claude query, got id=%q", id)
		}
	}
}

// TestGetMessageIDsByDisplayNames_ConfigKeyReturnsEmpty C2 回归守护：
// 用配置 key（如 "claude"）查应返回空，证明不能误用配置 key。
func TestGetMessageIDsByDisplayNames_ConfigKeyReturnsEmpty(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	_, err := UpsertMessages(ctx, db, []model.Message{
		{ID: "cc1", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 1},
	})
	if err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	// 用配置 key "claude" 查（错误用法），应返回空
	ids, err := GetMessageIDsByDisplayNames(ctx, db, []string{"claude"})
	if err != nil {
		t.Fatalf("GetMessageIDsByDisplayNames failed: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("用配置 key 查询应返回空（messages.client 存的是显示名），got %d: %v", len(ids), ids)
	}
}

// TestGetMessageIDsByDisplayNames_EmptyList 空显示名列表不应产生非法 SQL。
func TestGetMessageIDsByDisplayNames_EmptyList(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	ids, err := GetMessageIDsByDisplayNames(ctx, db, nil)
	if err != nil {
		t.Fatalf("GetMessageIDsByDisplayNames(nil) failed: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids for nil input, got %d", len(ids))
	}
}

// TestGetMessageIDsByDisplayNames_NoMatch 无匹配返回空切片，不报错。
func TestGetMessageIDsByDisplayNames_NoMatch(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	ids, err := GetMessageIDsByDisplayNames(ctx, db, []string{"NonExistent"})
	if err != nil {
		t.Fatalf("GetMessageIDsByDisplayNames failed: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids, got %d", len(ids))
	}
}
