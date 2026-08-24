package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// createTestZCodeDB 构造与真实 ~/.zcode/cli/db/db.sqlite 同构的测试库：
// session(id, parent_id, directory, title, time_created, time_updated) +
// model_usage(id, session_id, model_id, provider_id, status, started_at, completed_at, *_tokens)。
// 逐行 schema 与真实库一致：一行 model_usage = 一次模型 API 请求。
func createTestZCodeDB(t *testing.T, dbPath string) {
	t.Helper()
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir db 父目录失败: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	defer db.Close()

	// schema 与生产库一致：parent_id/title/directory 等列带 NOT NULL DEFAULT，
	// 与任务描述中给出的建表语句保持同构。
	schema := `
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		parent_id TEXT NOT NULL DEFAULT '',
		directory TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE model_usage (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		completed_at INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		reasoning_tokens INTEGER,
		cache_creation_input_tokens INTEGER,
		cache_read_input_tokens INTEGER,
		provider_total_tokens INTEGER,
		computed_total_tokens INTEGER
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("创建表失败: %v", err)
	}
}

// insertZCodeUsage 插入一行 completed model_usage（id 唯一）。返回该行（便于后续断言）。
func insertZCodeUsage(t *testing.T, dbPath string, u zcodeUsageRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO model_usage
		(id, session_id, model_id, provider_id, status, started_at, completed_at,
		 input_tokens, output_tokens, reasoning_tokens,
		 cache_creation_input_tokens, cache_read_input_tokens,
		 provider_total_tokens, computed_total_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.id, u.sessionID, u.model, u.provider, u.status, u.startedAt, u.completedAt,
		nilIf(u.input, u.inputSet), nilIf(u.output, u.outputSet),
		nilIf(u.reasoning, u.reasoningSet),
		nilIf(u.cacheCreate, u.cacheCreateSet), nilIf(u.cacheRead, u.cacheReadSet),
		nilIf64(u.providerTotal, u.providerTotalSet),
		nilIf64(u.computedTotal, u.computedTotalSet),
	); err != nil {
		t.Fatalf("插入 model_usage %s 失败: %v", u.id, err)
	}
}

// insertZCodeSession 插入一行 session。
func insertZCodeSession(t *testing.T, dbPath string, s zcodeSessionRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
		VALUES (?,?,?,?,?,?)`,
		s.id, s.parentID, s.directory, s.title, s.timeCreated, s.timeUpdated); err != nil {
		t.Fatalf("插入 session %s 失败: %v", s.id, err)
	}
}

// updateZCodeUsageStatus 更新某 usage 行的 status 与 completed_at（模拟 running→completed）。
func updateZCodeUsageStatus(t *testing.T, dbPath, id, status string, completedAt int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE model_usage SET status=?, completed_at=? WHERE id=?`,
		status, completedAt, id); err != nil {
		t.Fatalf("更新 model_usage %s 失败: %v", id, err)
	}
}

// 测试数据行结构（带 set 标志区分 NULL 与 0）。
type zcodeUsageRow struct {
	id               string
	sessionID        string
	model            string
	provider         string
	status           string
	startedAt        int64
	completedAt      int64
	input            int64
	inputSet         bool
	output           int64
	outputSet        bool
	reasoning        int64
	reasoningSet     bool
	cacheRead        int64
	cacheReadSet     bool
	cacheCreate      int64
	cacheCreateSet   bool
	providerTotal    int64
	providerTotalSet bool
	computedTotal    int64
	computedTotalSet bool
}

type zcodeSessionRow struct {
	id          string
	parentID    string
	directory   string
	title       string
	timeCreated int64
	timeUpdated int64
}

func nilIf(v int64, set bool) any {
	if !set {
		return nil
	}
	return v
}

func nilIf64(v int64, set bool) any {
	return nilIf(v, set)
}

func newTestZCodeCollector(t *testing.T, dbPath string) *ZCodeCollector {
	t.Helper()
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode": {
				Enabled: true,
				Paths:   map[string]string{"db": dbPath},
			},
		},
	}
	return NewZCodeCollector(cfg)
}

// day1MS 返回本地时区 2026-07-01 hour:00 的毫秒时间戳。
func day1MS(hour int) int64 {
	return time.Date(2026, 7, 1, hour, 0, 0, 0, time.Local).UnixMilli()
}

// msgByID 把 Messages 转成 id→Message 的 map（便于按 id 断言）。
func msgByID(msgs []model.Message) map[string]model.Message {
	m := make(map[string]model.Message, len(msgs))
	for _, msg := range msgs {
		m[msg.ID] = msg
	}
	return m
}

// ===== completed model_usage 每个 id 一行，不 GROUP BY =====
// 插入两个同 session/model/date 的 completed 行，断言返回两个不同 Message.ID。
func TestZCodeCollector_OneRowPerCompletedUsage(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	base := day1MS(10)
	for i, id := range []string{"usage-1", "usage-2"} {
		insertZCodeUsage(t, dbPath, zcodeUsageRow{
			id: id, sessionID: "sess-1", model: "GLM-5.2",
			provider: "builtin:bigmodel-coding-plan", status: "completed",
			startedAt: base + int64(i)*1000, completedAt: base + int64(i)*1000,
			input: 1000, inputSet: true, output: 200, outputSet: true,
			computedTotal: 1200, computedTotalSet: true,
		})
	}

	c := newTestZCodeCollector(t, dbPath)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// 行为：两行 completed 同 session/model/date 必须产出两条 Message（不聚合）。
	if len(result.Messages) != 2 {
		t.Fatalf("期望 2 条 Message（逐请求不聚合），实际 %d: %+v", len(result.Messages), result.Messages)
	}
	byID := msgByID(result.Messages)
	if _, ok := byID["usage-1"]; !ok {
		t.Errorf("缺少 usage-1；得到 %v", msgIDs(result.Messages))
	}
	if _, ok := byID["usage-2"]; !ok {
		t.Errorf("缺少 usage-2；得到 %v", msgIDs(result.Messages))
	}
}

// ===== 日期和增量锚点用 completed_at =====
// 两行 started_at 在 07-01，但 completed_at 跨天：一在 07-01，一在 07-02。
// 按 ["2026-07-01"] 过滤只能命中 completed_at 在 07-01 的行。
func TestZCodeCollector_UsesCompletedAtForDate(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	startedAt := time.Date(2026, 7, 1, 23, 0, 0, 0, time.Local).UnixMilli()
	completedD1 := time.Date(2026, 7, 1, 23, 50, 0, 0, time.Local).UnixMilli()
	completedD2 := time.Date(2026, 7, 2, 0, 10, 0, 0, time.Local).UnixMilli()
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "d1-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: startedAt, completedAt: completedD1,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "d2-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: startedAt, completedAt: completedD2,
		input: 200, inputSet: true, output: 20, outputSet: true,
		computedTotal: 220, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	// 只取 completed_at 落在 2026-07-01 的行
	result, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-07-01"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("期望 1 条 Message（completed_at 落 07-01），实际 %d", len(result.Messages))
	}
	if result.Messages[0].ID != "d1-row" {
		t.Errorf("命中行 ID = %q, want d1-row（completed_at 锚点）", result.Messages[0].ID)
	}
}

// ===== provider_total 非 NULL 优先，NULL 回退 computed_total =====
func TestZCodeCollector_TotalPriority(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	base := day1MS(10)
	// 行 A：provider_total=999（非 NULL）→ TotalTokens=999（优先）
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "provider-total-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: base, completedAt: base,
		input: 100, inputSet: true, output: 10, outputSet: true,
		providerTotal: 999, providerTotalSet: true,
		computedTotal: 110, computedTotalSet: true,
	})
	// 行 B：provider_total NULL → TotalTokens=110（回退 computed_total）
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "computed-total-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: base + 1000, completedAt: base + 1000,
		input: 100, inputSet: true, output: 10, outputSet: true,
		// providerTotal 未 set → NULL
		computedTotal: 110, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	byID := msgByID(result.Messages)
	if got := byID["provider-total-row"].TotalTokens; got != 999 {
		t.Errorf("provider_total 非 NULL 行 TotalTokens = %d, want 999（provider_total 优先）", got)
	}
	if got := byID["computed-total-row"].TotalTokens; got != 110 {
		t.Errorf("provider_total NULL 行 TotalTokens = %d, want 110（回退 computed_total）", got)
	}
}

// ===== raw input/cache/reasoning 保留，fresh 非负保护 =====
func TestZCodeCollector_TokenFieldsAndReasoning(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	base := day1MS(10)
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "tok-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: base, completedAt: base,
		input: 1000, inputSet: true, output: 200, outputSet: true,
		cacheRead: 300, cacheReadSet: true, cacheCreate: 0, cacheCreateSet: true,
		reasoning: 50, reasoningSet: true,
		computedTotal: 1200, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("期望 1 条 Message，实际 %d", len(result.Messages))
	}
	m := result.Messages[0]
	if m.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000（raw 保留）", m.InputTokens)
	}
	if m.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200（raw 保留）", m.OutputTokens)
	}
	if m.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300（raw 保留）", m.CacheReadTokens)
	}
	if m.CacheCreateTokens != 0 {
		t.Errorf("CacheCreateTokens = %d, want 0（raw 保留）", m.CacheCreateTokens)
	}
	if m.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50（raw 保留）", m.ReasoningTokens)
	}
	// fresh = max(0, input - cache_read - cache_create) = 1000-300-0 = 700
	if m.FreshInputTokens != 700 {
		t.Errorf("FreshInputTokens = %d, want 700（1000-300-0）", m.FreshInputTokens)
	}
}

// ===== running→completed 被增量捕获（rowid 不变但 completed_at 更新） =====
func TestZCodeCollector_IncrementalCatchesRunningToCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	// 第一次：status=running，无 completed_at → 不产出 completed 行
	runningStartedAt := day1MS(10)
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "trans-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "running",
		startedAt: runningStartedAt,
		// completedAt 未设置（running 行无 completed_at）
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	// 第一次全量采集：running 行应被过滤
	r1, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("第一次 Collect 失败: %v", err)
	}
	if len(r1.Messages) != 0 {
		t.Fatalf("第一次（running）期望 0 条 Message，实际 %d", len(r1.Messages))
	}

	// 第二次：UPDATE 同一 id 为 completed，completed_at 推进
	completedAt := day1MS(11)
	updateZCodeUsageStatus(t, dbPath, "trans-row", "completed", completedAt)

	// 增量采集：cursor 从全量结果继承（r1.NextCursors 为空因为非 incremental）
	// 这里用 incremental=true + 旧 cursor（Value=0 表示从 0 起）验证能捕获新 completed 行
	r2, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceZCodeModelUsage: {Value: 0, ID: ""}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("第二次 Collect 失败: %v", err)
	}
	byID := msgByID(r2.Messages)
	if _, ok := byID["trans-row"]; !ok {
		t.Errorf("增量未捕获 running→completed 的 trans-row；得到 %v", msgIDs(r2.Messages))
	}
}

// ===== 同 completed_at 的更大 id 不漏（复合游标） =====
func TestZCodeCollector_CompositeCursorSameCompletedAt(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	// 两行 completed_at 完全相同，id 字典序 a < b
	sameTS := day1MS(10)
	for _, id := range []string{"usage-a", "usage-b"} {
		insertZCodeUsage(t, dbPath, zcodeUsageRow{
			id: id, sessionID: "sess-1", model: "GLM-5.2",
			provider: "p1", status: "completed",
			startedAt: sameTS, completedAt: sameTS,
			input: 100, inputSet: true, output: 10, outputSet: true,
			computedTotal: 110, computedTotalSet: true,
		})
	}

	c := newTestZCodeCollector(t, dbPath)
	// 增量采集，cursor = (sameTS, "usage-a")：必须捕获 usage-b（同 completed_at、更大 id）
	r, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceZCodeModelUsage: {Value: sameTS, ID: "usage-a"}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	byID := msgByID(r.Messages)
	if _, ok := byID["usage-b"]; !ok {
		t.Errorf("复合游标未捕获同 completed_at 的更大 id usage-b；得到 %v", msgIDs(r.Messages))
	}
	if _, ok := byID["usage-a"]; ok {
		t.Errorf("复合游标不应回捕 usage-a（== cursor 已处理）")
	}
}

// ===== 构造非零 cache_create 验证公式和 invariant warning =====
// 注意：本测试只验证代码的减法保护与非负 clamp，不代表真实 ZCode 非零 cache_create 语义已实测。
// 当前真实样本 cache_create 全为 0，减法路径是暂定口径。
func TestZCodeCollector_CacheCreateExceedsInputInvariantWarning(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	base := day1MS(10)
	// input=100, cache_read=80, cache_create=30 → fresh=max(0,100-80-30)=0
	// cache_read+cache_create=110 > input=100 → 触发 invariant warning
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "cache-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: base, completedAt: base,
		input: 100, inputSet: true, output: 10, outputSet: true,
		cacheRead: 80, cacheReadSet: true, cacheCreate: 30, cacheCreateSet: true,
		computedTotal: 110, computedTotalSet: true,
	})

	handler := &testLogHandler{}
	logger := slog.New(handler)
	c := newTestZCodeCollector(t, dbPath)
	result, err := c.Collect(context.Background(), CollectRequest{}, logger)
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	byID := msgByID(result.Messages)
	m, ok := byID["cache-row"]
	if !ok {
		t.Fatalf("缺少 cache-row")
	}
	// fresh 非负保护：100-80-30 = -10 → clamp 到 0
	if m.FreshInputTokens != 0 {
		t.Errorf("FreshInputTokens = %d, want 0（cache 超过 input 时 clamp 非负）", m.FreshInputTokens)
	}
	// raw 字段不 clamp，原样保留
	if m.InputTokens != 100 || m.CacheReadTokens != 80 || m.CacheCreateTokens != 30 {
		t.Errorf("raw token 未原样保留: in=%d cr=%d cc=%d", m.InputTokens, m.CacheReadTokens, m.CacheCreateTokens)
	}
	// 必须有 invariant warning 日志
	if !handler.HasMessage("ZCode cache token 超过 input") {
		t.Errorf("期望 invariant warning 日志，实际日志: %v", handler.Messages())
	}
}

// ===== session.parent_id 及 provider 显示映射/fallback =====
func TestZCodeCollector_SessionParentIDAndProviderMapping(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".zcode", "cli", "db", "db.sqlite")
	cachePath := filepath.Join(root, ".zcode", "v2", "bots-model-cache.v2.json")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{
		id: "sess-1", parentID: "parent-xyz",
		directory: "/Users/test/proj-a", title: "My Title",
		timeCreated: day1MS(9), timeUpdated: day1MS(12),
	})
	base := day1MS(10)
	// provider 在 cache 中有映射
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "mapped-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "builtin:bigmodel-coding-plan", status: "completed",
		startedAt: base, completedAt: base,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})
	// provider 在 cache 中无映射 → fallback provider_id 原值
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "fallback-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "unknown-provider-id", status: "completed",
		startedAt: base + 1000, completedAt: base + 1000,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})
	writeZCodeCacheJSON(t, cachePath, []map[string]any{
		{"id": "builtin:bigmodel-coding-plan", "name": "Bigmodel - Coding Plan"},
	})

	c := newTestZCodeCollector(t, dbPath)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	byID := msgByID(result.Messages)
	if m := byID["mapped-row"]; m.Provider != "Bigmodel - Coding Plan" {
		t.Errorf("mapped-row Provider = %q, want Bigmodel - Coding Plan", m.Provider)
	}
	if m := byID["fallback-row"]; m.Provider != "unknown-provider-id" {
		t.Errorf("fallback-row Provider = %q, want unknown-provider-id（回退原值）", m.Provider)
	}
	// Session 元数据：parent_id、title、directory 必须从 session 表带入
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条 Session（同 session 去重），实际 %d", len(result.Sessions))
	}
	s := result.Sessions[0]
	if s.ParentID != "parent-xyz" {
		t.Errorf("Session.ParentID = %q, want parent-xyz", s.ParentID)
	}
	if s.Title != "My Title" {
		t.Errorf("Session.Title = %q, want My Title", s.Title)
	}
	if s.Directory != "/Users/test/proj-a" {
		t.Errorf("Session.Directory = %q, want /Users/test/proj-a", s.Directory)
	}
}

// ===== result.NextCursors 为本批最后 (completed_at,id) =====
func TestZCodeCollector_NextCursorIsLastBatchMax(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	base := day1MS(10)
	// 三行 completed_at 递增
	rows := []struct {
		id string
		ts int64
	}{{"u1", base}, {"u2", base + 1000}, {"u3", base + 2000}}
	for _, r := range rows {
		insertZCodeUsage(t, dbPath, zcodeUsageRow{
			id: r.id, sessionID: "sess-1", model: "GLM-5.2",
			provider: "p1", status: "completed",
			startedAt: r.ts, completedAt: r.ts,
			input: 100, inputSet: true, output: 10, outputSet: true,
			computedTotal: 110, computedTotalSet: true,
		})
	}

	c := newTestZCodeCollector(t, dbPath)
	// 非增量：NextCursors 不应设置
	r1, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(r1.NextCursors) != 0 {
		t.Errorf("非增量模式不应设置 NextCursors，得到 %v", r1.NextCursors)
	}

	// 增量：NextCursors 必须是本批最后 (completed_at, id) = (base+2000, "u3")
	r2, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceZCodeModelUsage: {Value: 0, ID: ""}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	cur, ok := r2.NextCursors[SyncSourceZCodeModelUsage]
	if !ok {
		t.Fatalf("增量模式应设置 NextCursors[%s]", SyncSourceZCodeModelUsage)
	}
	if cur.Value != base+2000 {
		t.Errorf("NextCursor.Value = %d, want %d（本批最大 completed_at）", cur.Value, base+2000)
	}
	if cur.ID != "u3" {
		t.Errorf("NextCursor.ID = %q, want u3（同最大 completed_at 的最大 id）", cur.ID)
	}
}

// 行为 补充：增量模式下没有新行时 NextCursor 必须保持输入 cursor，避免回退。
func TestZCodeCollector_NextCursorHoldsOnNoNewRows(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	// 只插一行 completed_at=base，cursor 设为 base 之后 → 无新行
	base := day1MS(10)
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "u1", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: base, completedAt: base,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	inCursor := model.SyncCursor{Value: base + 99999, ID: "zzz-future"}
	r, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceZCodeModelUsage: inCursor},
	}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(r.Messages) != 0 {
		t.Fatalf("cursor 在未来，期望 0 条新行，实际 %d", len(r.Messages))
	}
	cur, ok := r.NextCursors[SyncSourceZCodeModelUsage]
	if !ok {
		t.Fatalf("增量模式应设置 NextCursors")
	}
	if cur.Value != inCursor.Value || cur.ID != inCursor.ID {
		t.Errorf("无新行时 NextCursor 回退: got {%d,%q}, want 保持 {%d,%q}",
			cur.Value, cur.ID, inCursor.Value, inCursor.ID)
	}
}

// ===== Dates 为空全量，非空按消息完成日期过滤 =====
func TestZCodeCollector_DatesEmptyFullNonEmptyFiltered(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/Users/test/proj-a"})
	d1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local).UnixMilli()
	d2 := time.Date(2026, 7, 2, 10, 0, 0, 0, time.Local).UnixMilli()
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "day1-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: d1, completedAt: d1,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "day2-row", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed",
		startedAt: d2, completedAt: d2,
		input: 100, inputSet: true, output: 10, outputSet: true,
		computedTotal: 110, computedTotalSet: true,
	})

	c := newTestZCodeCollector(t, dbPath)
	// Dates 空 → 全量，两行都命中
	r1, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(r1.Messages) != 2 {
		t.Errorf("Dates 空（全量）期望 2 条，实际 %d", len(r1.Messages))
	}
	// Dates=["2026-07-01"] → 只命中 completed_at 落 07-01 的行
	r2, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-07-01"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(r2.Messages) != 1 || r2.Messages[0].ID != "day1-row" {
		t.Errorf("Dates 非空期望只命中 day1-row，实际 %v", msgIDs(r2.Messages))
	}
}

// ===== 基础测试：Name、dbPath 配置、DB 不存在 =====

func TestZCodeCollector_Name(t *testing.T) {
	c := NewZCodeCollector(&config.Config{})
	if c.Name() != "zcode" {
		t.Errorf("Name() = %q, want %q", c.Name(), "zcode")
	}
}

func TestZCodeCollector_DBPathNotConfigured(t *testing.T) {
	c := NewZCodeCollector(&config.Config{Clients: map[string]config.Client{}})
	_, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err == nil {
		t.Fatal("dbPath 未配置应返回 error")
	}
}

func TestZCodeCollector_DBNotFound(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode": {Enabled: true, Paths: map[string]string{"db": "/nonexistent/zcode.db"}},
		},
	}
	c := NewZCodeCollector(cfg)
	_, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err == nil {
		t.Fatal("DB 不存在应返回 error")
	}
}

func TestNewZCodeCollector_FromConfig(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode": {Enabled: true, Paths: map[string]string{"db": "/custom/zcode/db.sqlite"}},
		},
	}
	c := NewZCodeCollector(cfg)
	if c.dbPath != "/custom/zcode/db.sqlite" {
		t.Errorf("dbPath = %q, want /custom/zcode/db.sqlite", c.dbPath)
	}
}

func TestZcodeTsMsToDate(t *testing.T) {
	local := time.Local
	ts := time.Date(2026, 6, 15, 10, 0, 0, 0, local).UnixMilli()
	if got := zcodeTsMsToDate(ts); got != "2026-06-15" {
		t.Errorf("zcodeTsMsToDate(%d) = %q, want 2026-06-15", ts, got)
	}
	if got := zcodeTsMsToDate(0); got != "" {
		t.Errorf("zcodeTsMsToDate(0) = %q, want 空串", got)
	}
	if got := zcodeTsMsToDate(-1); got != "" {
		t.Errorf("zcodeTsMsToDate(-1) = %q, want 空串", got)
	}
}

func TestZcodeDateToMillisecondRange(t *testing.T) {
	start, end, err := zcodeDateToMillisecondRange("2026-06-15")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	wantStart := time.Date(2026, 6, 15, 0, 0, 0, 0, time.Local).UnixMilli()
	wantEnd := time.Date(2026, 6, 16, 0, 0, 0, 0, time.Local).UnixMilli()
	if start != wantStart {
		t.Errorf("start = %d, want %d", start, wantStart)
	}
	if end != wantEnd {
		t.Errorf("end = %d, want %d（左闭右开：次日 00:00）", end, wantEnd)
	}
}

func TestZcodeDateToMillisecondRange_Invalid(t *testing.T) {
	if _, _, err := zcodeDateToMillisecondRange("not-a-date"); err == nil {
		t.Fatal("期望错误，实际 nil")
	}
}

// ===== Provider map 辅助函数测试（loadZCodeProviderMap 等保持不变）=====

func writeZCodeCacheJSON(t *testing.T, path string, providers []map[string]any) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cache 父目录失败: %v", err)
	}
	doc := map[string]any{
		"version":                "v2",
		"updatedAt":              "2026-07-01T00:00:00Z",
		"providers":              providers,
		"workspaceConfigOptions": map[string]any{},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal cache json 失败: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write cache json 失败: %v", err)
	}
}

func TestLoadZCodeProviderMap_ParsesIDName(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bots-model-cache.v2.json")
	writeZCodeCacheJSON(t, tmp, []map[string]any{
		{"id": "builtin:bigmodel-coding-plan", "name": "Bigmodel - Coding Plan"},
		{"id": "9848d583-72b9-457d-b6f2-a54c487c5cc7", "name": "DeepSeek"},
	})
	m := loadZCodeProviderMap(tmp)
	if len(m) != 2 {
		t.Fatalf("期望 2 项，实际 %d: %v", len(m), m)
	}
	if m["builtin:bigmodel-coding-plan"] != "Bigmodel - Coding Plan" {
		t.Errorf("bigmodel name = %q", m["builtin:bigmodel-coding-plan"])
	}
	if m["9848d583-72b9-457d-b6f2-a54c487c5cc7"] != "DeepSeek" {
		t.Errorf("deepseek name = %q", m["9848d583-72b9-457d-b6f2-a54c487c5cc7"])
	}
}

func TestLoadZCodeProviderMap_FileNotFound_EmptyMap(t *testing.T) {
	m := loadZCodeProviderMap(filepath.Join(t.TempDir(), "no-such.json"))
	if m == nil || len(m) != 0 {
		t.Fatalf("期望非 nil 空 map，实际 %v", m)
	}
}

func TestLoadZCodeProviderMap_InvalidJSON_EmptyMap(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(tmp, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := loadZCodeProviderMap(tmp)
	if m == nil || len(m) != 0 {
		t.Fatalf("坏 json 期望非 nil 空 map，实际 %v", m)
	}
}

func TestLoadZCodeProviderMap_NoAPIKeyLeak(t *testing.T) {
	const secret = "sk-SECRET-DO-NOT-LEAK-1234567"
	tmp := filepath.Join(t.TempDir(), "bots-model-cache.v2.json")
	writeZCodeCacheJSON(t, tmp, []map[string]any{
		{"id": "p1", "name": "ProviderOne", "apiKey": secret, "options": map[string]any{"apiKey": secret}},
	})
	m := loadZCodeProviderMap(tmp)
	if m["p1"] != "ProviderOne" {
		t.Fatalf("p1 name = %q, want ProviderOne", m["p1"])
	}
	for k, v := range m {
		if strings.Contains(k, secret) || strings.Contains(v, secret) {
			t.Fatalf("apiKey 泄漏到 map: key=%q val=%q", k, v)
		}
	}
}

// version 2 schema：顶层无 providers，显示名在 workspaceConfigOptions 内。
// 结构按本机真实 bots-model-cache.v2.json 构造。
func writeZCodeCacheV2JSON(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cache 父目录失败: %v", err)
	}
	doc := map[string]any{
		"version":   2,
		"updatedAt": 1784696250197,
		"workspaceConfigOptions": map[string]any{
			"/ws/one::glm": map[string]any{
				"workspacePath": "/ws/one",
				"provider":      "glm",
				"configOptions": []map[string]any{
					{
						"id":   "model",
						"name": "Model",
						"type": "select",
						"options": []map[string]any{
							{
								"value":             "builtin:bigmodel-coding-plan/GLM-5.2",
								"name":              "GLM-5.2",
								"modelProviderId":   "builtin:bigmodel-coding-plan",
								"modelProviderName": "Bigmodel - Coding Plan",
							},
							{
								"value":             "9848d583-72b9-457d-b6f2-a54c487c5cc7/deepseek-v4-flash",
								"name":              "deepseek-v4-flash",
								"modelProviderId":   "9848d583-72b9-457d-b6f2-a54c487c5cc7",
								"modelProviderName": "DeepSeek",
							},
						},
					},
					{
						"id":   "mode",
						"name": "Mode",
						"type": "select",
						"options": []map[string]any{
							// 同 provider 重复出现，映射应保持一致（后写同值幂等）。
							{
								"value":             "builtin:bigmodel-coding-plan/GLM-5-Turbo",
								"modelProviderId":   "builtin:bigmodel-coding-plan",
								"modelProviderName": "Bigmodel - Coding Plan",
							},
						},
					},
				},
			},
			"/ws/two::custom": map[string]any{
				"workspacePath": "/ws/two",
				"provider":      "custom",
				"configOptions": []map[string]any{
					{
						"id": "model",
						"options": []map[string]any{
							{
								"value":             "da4546b5-af0c-49ed-8114-afa42d53af65/glm-5.2",
								"modelProviderId":   "da4546b5-af0c-49ed-8114-afa42d53af65",
								"modelProviderName": "Zhipu GLM 小狼",
							},
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal cache json 失败: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write cache json 失败: %v", err)
	}
}

func TestLoadZCodeProviderMap_Version2WorkspaceOptions(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bots-model-cache.v2.json")
	writeZCodeCacheV2JSON(t, tmp)

	m := loadZCodeProviderMap(tmp)
	if len(m) != 3 {
		t.Fatalf("期望 3 项（重复 provider 幂等），实际 %d: %v", len(m), m)
	}
	if m["builtin:bigmodel-coding-plan"] != "Bigmodel - Coding Plan" {
		t.Errorf("bigmodel name = %q, want 'Bigmodel - Coding Plan'", m["builtin:bigmodel-coding-plan"])
	}
	if m["9848d583-72b9-457d-b6f2-a54c487c5cc7"] != "DeepSeek" {
		t.Errorf("deepseek name = %q, want 'DeepSeek'", m["9848d583-72b9-457d-b6f2-a54c487c5cc7"])
	}
	if m["da4546b5-af0c-49ed-8114-afa42d53af65"] != "Zhipu GLM 小狼" {
		t.Errorf("zhipu name = %q, want 'Zhipu GLM 小狼'", m["da4546b5-af0c-49ed-8114-afa42d53af65"])
	}
}

// v1 providers 与 v2 workspaceConfigOptions 并存时两路合并、互不覆盖冲突。
func TestLoadZCodeProviderMap_MixedSchemasMerge(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bots-model-cache.v2.json")
	writeZCodeCacheV2JSON(t, tmp)

	// 追加 v1 providers 字段（与 v2 不同 id）。
	raw, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["providers"] = []map[string]any{
		{"id": "legacy-provider", "name": "Legacy Provider"},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := loadZCodeProviderMap(tmp)
	if len(m) != 4 {
		t.Fatalf("期望 4 项（3 v2 + 1 v1），实际 %d: %v", len(m), m)
	}
	if m["legacy-provider"] != "Legacy Provider" {
		t.Errorf("legacy name = %q, want 'Legacy Provider'", m["legacy-provider"])
	}
	if m["builtin:bigmodel-coding-plan"] != "Bigmodel - Coding Plan" {
		t.Errorf("v2 bigmodel name = %q", m["builtin:bigmodel-coding-plan"])
	}
}

// provider 映射缺失在多日期全量采集下也只输出一条汇总（聚合发生在 Collect 层，
// 而非每个日期范围的 scanRows），且汇总只含未命中的 provider；命中行正常显示映射名。
func TestZCodeCollector_ProviderMissSummarySingleLog(t *testing.T) {
	tmpDir := t.TempDir()
	// db 放三层深（同 ~/.zcode/cli/db/db.sqlite 布局），使 zcodeCachePathFromDB
	// 推导出的 cache 路径落在 tmpDir 内，fixture cache 文件才会被读取。
	dbPath := filepath.Join(tmpDir, ".zcode", "cli", "db", "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/p"})
	// 两天 × 两种 provider：p-mapped 可在 cache 命中，p-missing 必然回退。
	for _, day := range []int{1, 2} {
		ts := time.Date(2026, 7, day, 10, 0, 0, 0, time.Local).UnixMilli()
		for i, prov := range []string{"p-mapped", "p-missing"} {
			insertZCodeUsage(t, dbPath, zcodeUsageRow{
				id: fmt.Sprintf("u-%d-%d", day, i), sessionID: "sess-1", model: "M",
				provider: prov, status: "completed",
				startedAt: ts + int64(i)*1000, completedAt: ts + int64(i)*1000,
				input: 10, inputSet: true, output: 5, outputSet: true,
				computedTotal: 15, computedTotalSet: true,
			})
		}
	}
	writeZCodeCacheJSON(t, filepath.Join(tmpDir, ".zcode", "v2", "bots-model-cache.v2.json"),
		[]map[string]any{{"id": "p-mapped", "name": "Mapped Provider"}})

	c := newTestZCodeCollector(t, dbPath)
	handler := &testLogHandler{}
	result, err := c.Collect(context.Background(),
		CollectRequest{Dates: []string{"2026-07-01", "2026-07-02"}}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}

	// 命中映射的行显示名称，缺失行回退原值。
	byID := msgByID(result.Messages)
	if byID["u-1-0"].Provider != "Mapped Provider" || byID["u-2-0"].Provider != "Mapped Provider" {
		t.Errorf("p-mapped 行应显示映射名，实际 u-1-0=%q u-2-0=%q",
			byID["u-1-0"].Provider, byID["u-2-0"].Provider)
	}
	if byID["u-1-1"].Provider != "p-missing" || byID["u-2-1"].Provider != "p-missing" {
		t.Errorf("p-missing 行应回退原值，实际 u-1-1=%q u-2-1=%q",
			byID["u-1-1"].Provider, byID["u-2-1"].Provider)
	}

	// 恰好一条汇总，count=2（两天各一行），provider_ids 只含 p-missing。
	var summaries []slog.Record
	for _, r := range handler.Records() {
		if strings.Contains(r.Message, "映射缺失") {
			summaries = append(summaries, r)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("期望恰好 1 条映射缺失汇总，实际 %d 条: %v", len(summaries), handler.Messages())
	}
	attrs := map[string]string{}
	summaries[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	if attrs["count"] != "2" {
		t.Errorf("汇总 count = %q, want 2", attrs["count"])
	}
	if attrs["provider_ids"] != "p-missing" {
		t.Errorf("汇总 provider_ids = %q, want p-missing", attrs["provider_ids"])
	}
}

// 全部 provider 命中映射时不输出映射缺失汇总。
func TestZCodeCollector_ProviderAllMappedNoSummary(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, ".zcode", "cli", "db", "db.sqlite")
	createTestZCodeDB(t, dbPath)
	insertZCodeSession(t, dbPath, zcodeSessionRow{id: "sess-1", directory: "/p"})
	insertZCodeUsage(t, dbPath, zcodeUsageRow{
		id: "u-1", sessionID: "sess-1", model: "M", provider: "p-ok", status: "completed",
		startedAt: day1MS(10), completedAt: day1MS(10),
		input: 10, inputSet: true, computedTotal: 10, computedTotalSet: true,
	})
	writeZCodeCacheJSON(t, filepath.Join(tmpDir, ".zcode", "v2", "bots-model-cache.v2.json"),
		[]map[string]any{{"id": "p-ok", "name": "OK Provider"}})

	c := newTestZCodeCollector(t, dbPath)
	handler := &testLogHandler{}
	if _, err := c.Collect(context.Background(), CollectRequest{}, slog.New(handler)); err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if handler.HasMessage("映射缺失") {
		t.Errorf("全部命中时不应有映射缺失汇总: %v", handler.Messages())
	}
}

func TestZcodeCachePathFromDB(t *testing.T) {
	got := zcodeCachePathFromDB("/Users/me/.zcode/cli/db/db.sqlite")
	want := "/Users/me/.zcode/v2/bots-model-cache.v2.json"
	if got != want {
		t.Errorf("cache path = %q, want %q", got, want)
	}
}
