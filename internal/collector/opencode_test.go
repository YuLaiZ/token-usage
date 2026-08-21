package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// ===== 基础单元测试（不依赖 DB） =====

func TestParseModelJSON_Normal(t *testing.T) {
	modelJSON := `{"id":"claude-sonnet-4-20250514","providerID":"anthropic"}`
	modelID, providerID := parseModelJSON(modelJSON)
	if modelID != "claude-sonnet-4-20250514" {
		t.Errorf("modelID = %q, want %q", modelID, "claude-sonnet-4-20250514")
	}
	if providerID != "anthropic" {
		t.Errorf("providerID = %q, want %q", providerID, "anthropic")
	}
}

func TestParseModelJSON_Empty(t *testing.T) {
	modelID, providerID := parseModelJSON("")
	if modelID != "" {
		t.Errorf("modelID = %q, want empty", modelID)
	}
	if providerID != "" {
		t.Errorf("providerID = %q, want empty", providerID)
	}
}

func TestParseModelJSON_InvalidJSON(t *testing.T) {
	modelID, providerID := parseModelJSON("not-json")
	if modelID != "" {
		t.Errorf("modelID = %q, want empty", modelID)
	}
	if providerID != "" {
		t.Errorf("providerID = %q, want empty", providerID)
	}
}

func TestParseModelJSON_OnlyID(t *testing.T) {
	modelJSON := `{"id":"gpt-4o"}`
	modelID, providerID := parseModelJSON(modelJSON)
	if modelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", modelID, "gpt-4o")
	}
	if providerID != "" {
		t.Errorf("providerID = %q, want empty", providerID)
	}
}

func TestLoadProviderMapping_Normal(t *testing.T) {
	mapping := loadProviderMapping("../../testdata", nil)

	tests := []struct {
		input    string
		expected string
	}{
		{"anthropic", "Anthropic"},
		{"openai", "OpenAI"},
		{"zhipu", "Zhipu GLM"},
		{"deepseek", "DeepSeek"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := mapping[tt.input]
			if !ok {
				t.Errorf("mapping[%q] not found", tt.input)
			}
			if got != tt.expected {
				t.Errorf("mapping[%q] = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// 新版 OpenCode 的 models.json 是 provider 注册表：顶层 provider ID →
// {id, name, models: {...}} 对象；应取 name 作显示名。
// 对象缺 name、value 为其他不可识别形态时跳过该条（消息侧回退原始值）。
func TestLoadProviderMapping_RegistrySchema(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"zhipuai": {"id": "zhipuai", "name": "Zhipu AI", "models": {"glm-5": {"id": "glm-5"}}},
		"anthropic": {"id": "anthropic", "models": {"claude": {"id": "claude"}}},
		"broken": 42
	}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mapping := loadProviderMapping(dir, nil)

	if got := mapping["zhipuai"]; got != "Zhipu AI" {
		t.Errorf(`mapping["zhipuai"] = %q, want "Zhipu AI"`, got)
	}
	if got, ok := mapping["anthropic"]; ok || got != "" {
		t.Errorf(`mapping["anthropic"] = %q (present=%v), want absent (name missing)`, got, ok)
	}
	if got, ok := mapping["broken"]; ok || got != "" {
		t.Errorf(`mapping["broken"] = %q (present=%v), want absent (unrecognized shape)`, got, ok)
	}
}

// 新旧结构混排（扁平字符串与注册表对象共存）时逐条按形态分派。
func TestLoadProviderMapping_MixedSchema(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"zhipu": "Zhipu GLM",
		"zhipuai": {"id": "zhipuai", "name": "Zhipu AI", "models": {}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mapping := loadProviderMapping(dir, nil)

	if got := mapping["zhipu"]; got != "Zhipu GLM" {
		t.Errorf(`mapping["zhipu"] = %q, want "Zhipu GLM" (flat schema)`, got)
	}
	if got := mapping["zhipuai"]; got != "Zhipu AI" {
		t.Errorf(`mapping["zhipuai"] = %q, want "Zhipu AI" (registry schema)`, got)
	}
}

// 文件损坏（非法 JSON）降级为空映射，不返回错误、不阻断采集。
func TestLoadProviderMapping_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mapping := loadProviderMapping(dir, nil)
	if len(mapping) != 0 {
		t.Errorf("expected empty mapping for corrupted file, got %d entries", len(mapping))
	}
}

func TestLoadProviderMapping_MissingFile(t *testing.T) {
	mapping := loadProviderMapping("/nonexistent/path", nil)
	if len(mapping) != 0 {
		t.Errorf("expected empty mapping, got %d entries", len(mapping))
	}
}

func TestLoadProviderMapping_UnknownProvider(t *testing.T) {
	mapping := loadProviderMapping("../../testdata", nil)
	got := mapping["unknown_provider"]
	if got != "" {
		t.Errorf("unknown provider should return empty, got %q", got)
	}
}

func TestOpenCodeCollector_Name(t *testing.T) {
	cfg := &config.Config{}
	c := NewOpenCodeCollector(cfg)
	if c.Name() != "opencode" {
		t.Errorf("Name() = %q, want %q", c.Name(), "opencode")
	}
}

func TestNewOpenCodeCollector_FromConfig(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {
				Enabled: true,
				Paths: map[string]string{
					"db": "/custom/path/opencode.db",
				},
			},
		},
	}

	c := NewOpenCodeCollector(cfg)
	if c.dbPath != "/custom/path/opencode.db" {
		t.Errorf("dbPath = %q, want %q", c.dbPath, "/custom/path/opencode.db")
	}
}

func TestNewOpenCodeCollector_DefaultCacheDir(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {
				Enabled: true,
				Paths:   map[string]string{},
			},
		},
	}

	c := NewOpenCodeCollector(cfg)
	home, _ := os.UserHomeDir()
	expectedCacheDir := filepath.Join(home, ".cache", "opencode")
	if c.cacheDir != expectedCacheDir {
		t.Errorf("cacheDir = %q, want %q", c.cacheDir, expectedCacheDir)
	}
}

// ===== 测试 DB schema 与 fixture =====

// createTestOpenCodeDB 构造与生产库同构的测试 DB：
// session(id,parent_id,directory,title,model,time_created,time_updated) +
// message(id,session_id,time_created,time_updated,data) +
// event(id,aggregate_id,seq,type,data)。
// 不建 project 表（生产查询不再 JOIN project）。
func createTestOpenCodeDB(t *testing.T, dbPath string) {
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

	schema := `
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		parent_id TEXT NOT NULL DEFAULT '',
		directory TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '{}',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		data TEXT NOT NULL
	);
	CREATE TABLE event (
		id TEXT NOT NULL,
		aggregate_id TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("创建表失败: %v", err)
	}
}

// ocSessionRow 测试插入 session 行。
type ocSessionRow struct {
	id          string
	parentID    string
	directory   string
	title       string
	model       string
	timeCreated int64
	timeUpdated int64
}

func insertOCSession(t *testing.T, dbPath string, s ocSessionRow) {
	t.Helper()
	if s.model == "" {
		s.model = "{}"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session (id,parent_id,directory,title,model,time_created,time_updated) VALUES (?,?,?,?,?,?,?)`,
		s.id, s.parentID, s.directory, s.title, s.model, s.timeCreated, s.timeUpdated); err != nil {
		t.Fatalf("插入 session %s 失败: %v", s.id, err)
	}
}

// ocMessageRow 测试插入 message 行。
type ocMessageRow struct {
	id          string
	sessionID   string
	timeCreated int64
	timeUpdated int64
	info        openCodeInfo
}

func insertOCMessage(t *testing.T, dbPath string, m ocMessageRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	data, err := json.Marshal(m.info)
	if err != nil {
		t.Fatalf("序列化 message info 失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES (?,?,?,?,?)`,
		m.id, m.sessionID, m.timeCreated, m.timeUpdated, string(data)); err != nil {
		t.Fatalf("插入 message %s 失败: %v", m.id, err)
	}
}

// ocEventRow 测试插入 event 行。
type ocEventRow struct {
	id          string
	aggregateID string
	seq         int64
	eventType   string
	info        openCodeInfo
}

func insertOCEvent(t *testing.T, dbPath string, e ocEventRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	envelope := openCodeEventEnvelope{Info: e.info}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("序列化 event envelope 失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO event (id,aggregate_id,seq,type,data) VALUES (?,?,?,?,?)`,
		e.id, e.aggregateID, e.seq, e.eventType, string(data)); err != nil {
		t.Fatalf("插入 event 失败: %v", err)
	}
}

func newTestOpenCodeCollector(t *testing.T, dbPath string, cacheDir string) *OpenCodeCollector {
	t.Helper()
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {
				Enabled: true,
				Paths: map[string]string{
					"db": dbPath,
				},
			},
		},
	}
	c := NewOpenCodeCollector(cfg)
	if cacheDir != "" {
		c.cacheDir = cacheDir
	}
	return c
}

// ocCompletedInfo 构造一个 completed assistant info（便捷 helper）。
func ocCompletedInfo(id, sessionID, modelID, providerID string, completed int64, total, input, output int64) openCodeInfo {
	var info openCodeInfo
	info.ID = id
	info.SessionID = sessionID
	info.Role = "assistant"
	info.ModelID = modelID
	info.ProviderID = providerID
	info.Time.Created = completed
	info.Time.Completed = completed
	info.Tokens.Total = total
	info.Tokens.Input = input
	info.Tokens.Output = output
	return info
}

// ocMsgsByID 把 Messages 转成 id→Message 的 map（便于按 id 断言）。
func ocMsgsByID(msgs []model.Message) map[string]model.Message {
	m := make(map[string]model.Message, len(msgs))
	for _, msg := range msgs {
		m[msg.ID] = msg
	}
	return m
}

// ocCollect 调用 collector.Collect 并返回结果。
func ocCollect(t *testing.T, c *OpenCodeCollector, req CollectRequest) CollectResult {
	t.Helper()
	res, err := c.Collect(context.Background(), req, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	return res
}

// ocDateMS 返回本地时区某日期指定时分秒的毫秒时间戳。
func ocDateMS(year, month, day, hour, minute int) int64 {
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.Local).UnixMilli()
}

// ocSortedTS 返回按 (TS,ID) 排序后的 message IDs（便于跨实现断言顺序稳定）。
func ocSortedTS(msgs []model.Message) []string {
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return out[i].ID < out[j].ID
	})
	ids := make([]string, len(out))
	for i, m := range out {
		ids[i] = m.ID
	}
	return ids
}

// ===== completed assistant message 每个 id 一行 =====
func TestOpenCodeCollector_MessageRowsNotAggregated(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	c1 := ocDateMS(2026, 7, 1, 10, 0)
	c2 := ocDateMS(2026, 7, 1, 11, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: c1,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", c1, 100, 60, 40)})
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-2", sessionID: "sess-1", timeUpdated: c2,
		info: ocCompletedInfo("msg-2", "sess-1", "m1", "anthropic", c2, 200, 120, 80)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 2 {
		t.Fatalf("期望 2 条 message（逐行非聚合），实际 %d", len(res.Messages))
	}
	byID := ocMsgsByID(res.Messages)
	m1 := byID["msg-1"]
	if m1.TotalTokens != 100 {
		t.Errorf("msg-1 TotalTokens = %d, want 100（源 total 原样）", m1.TotalTokens)
	}
	if m1.FreshInputTokens != 60 {
		t.Errorf("msg-1 FreshInputTokens = %d, want 60（fresh=input）", m1.FreshInputTokens)
	}
	if m1.InputTokens != 60 || m1.OutputTokens != 40 {
		t.Errorf("msg-1 tokens = input %d output %d, want 60/40", m1.InputTokens, m1.OutputTokens)
	}
	m2 := byID["msg-2"]
	if m2.TotalTokens != 200 {
		t.Errorf("msg-2 TotalTokens = %d, want 200", m2.TotalTokens)
	}
	// 不应包含 Session 聚合（旧 SUM/GROUP BY 行为）
	if len(res.Sessions) != 1 {
		t.Errorf("期望 1 个 Session 元数据，实际 %d", len(res.Sessions))
	}
}

// ===== fresh=input, total 原样, 不统一加 reasoning/cache =====
func TestOpenCodeCollector_FreshInputAndTotalPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	var info openCodeInfo
	info.ID = "msg-1"
	info.SessionID = "sess-1"
	info.Role = "assistant"
	info.ModelID = "m1"
	info.ProviderID = "anthropic"
	info.Time.Created = ts
	info.Time.Completed = ts
	info.Tokens.Total = 999
	info.Tokens.Input = 500
	info.Tokens.Output = 300
	info.Tokens.Reasoning = 50
	info.Tokens.Cache.Read = 100
	info.Tokens.Cache.Write = 49
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: ts, info: info})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 message，实际 %d", len(res.Messages))
	}
	m := res.Messages[0]
	if m.TotalTokens != 999 {
		t.Errorf("TotalTokens = %d, want 999（源 total 原样，不重算）", m.TotalTokens)
	}
	if m.FreshInputTokens != 500 {
		t.Errorf("FreshInputTokens = %d, want 500（fresh=input，不扣 cache）", m.FreshInputTokens)
	}
	if m.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50", m.ReasoningTokens)
	}
	if m.CacheReadTokens != 100 || m.CacheCreateTokens != 49 {
		t.Errorf("cache tokens = read %d create %d, want 100/49", m.CacheReadTokens, m.CacheCreateTokens)
	}
}

// ===== message.modelID 为空时解析 session.model =====
func TestOpenCodeCollector_SessionModelFallback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"session-model","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// info 不带 modelID/providerID
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: ts,
		info: ocCompletedInfo("msg-1", "sess-1", "", "", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 message，实际 %d", len(res.Messages))
	}
	m := res.Messages[0]
	if m.Model != "session-model" {
		t.Errorf("Model = %q, want session-model（session.model 回退）", m.Model)
	}
	// providerID 空 → 回退 session providerID → 经 mapping 映射
	if m.Provider != "Anthropic" {
		t.Errorf("Provider = %q, want Anthropic", m.Provider)
	}
}

// ===== 同 message 多 event 取最大 rowid completed 终态 =====
func TestOpenCodeCollector_EventLatestCompletedSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// 当前 message：total=120
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: ts,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 120, 70, 50)})
	// 同 ID 两个 message.updated.1 event，后一个 total=999
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 110, 60, 50)})
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-2", aggregateID: "sess-1", seq: 2, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 999, 600, 399)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 message（同 ID 合并），实际 %d", len(res.Messages))
	}
	// message 主源优先，total=120
	if res.Messages[0].TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120（message 主源优先）", res.Messages[0].TotalTokens)
	}
}

// ===== message/event 同 ID 值冲突时使用当前 message =====
func TestOpenCodeCollector_CurrentMessageWinsConflict(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// 当前 message：total=120
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: ts,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 120, 70, 50)})
	// event：total=999
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 999, 600, 399)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 message，实际 %d", len(res.Messages))
	}
	if res.Messages[0].TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120（当前 message 优先于 event 999）", res.Messages[0].TotalTokens)
	}
}

// ===== message 不存在时 event-only 终态补回 =====
func TestOpenCodeCollector_EventFillsDeletedMessage(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// 不插入 message 行，只有 event
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-deleted", "sess-1", "m1", "anthropic", ts, 333, 200, 133)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 event-only message 被补回，实际 %d", len(res.Messages))
	}
	m := res.Messages[0]
	if m.ID != "msg-deleted" {
		t.Errorf("ID = %q, want msg-deleted", m.ID)
	}
	if m.TotalTokens != 333 {
		t.Errorf("TotalTokens = %d, want 333（event 终态补回）", m.TotalTokens)
	}
}

// ===== message 不在本批增量但主表存在时仍由 message 覆盖 event =====
func TestOpenCodeCollector_DaemonLooksUpCurrentMessage(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// message：total=120，time_updated 较早（不在本批增量内）
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: 1000,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 120, 70, 50)})
	// event：total=999（在本批增量内）
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 999, 600, 399)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	// message cursor 让当前 message 不属于本批增量（time_updated=1000 <= cursor）
	// event cursor=0 从头扫，event 在本批
	res := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: {Value: 1000, ID: "msg-1"},
			SyncSourceOpenCodeEvent:   {},
		},
	})

	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条 message，实际 %d", len(res.Messages))
	}
	// daemon 回查主表后选择 message=120
	if res.Messages[0].TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120（daemon 回查主表后 message 覆盖 event）", res.Messages[0].TotalTokens)
	}
}

// ===== 只有非 token event 时 rowid cursor 仍推进（high-water） =====
func TestOpenCodeCollector_HighWaterAdvancesOnNonTokenEvents(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// 先插一条 token event（rowid=1），跑一批拿到 cursor
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res1 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: {},
			SyncSourceOpenCodeEvent:   {},
		},
	})
	if len(res1.Messages) != 1 {
		t.Fatalf("第一批期望 1 条 message，实际 %d", len(res1.Messages))
	}
	eventCursor1 := res1.NextCursors[SyncSourceOpenCodeEvent]
	if eventCursor1.Value == 0 {
		t.Fatal("第一批 event cursor 应推进到 high-water")
	}

	// 追加一条非 token event（message.part.updated.1）
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-2", aggregateID: "sess-1", seq: 2, eventType: "message.part.updated.1",
		info: ocCompletedInfo("msg-2", "sess-1", "m1", "anthropic", ts, 0, 0, 0)})
	res2 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: res1.NextCursors[SyncSourceOpenCodeMessage],
			SyncSourceOpenCodeEvent:   eventCursor1,
		},
	})
	// 本批 Messages 为空（非 token event 不产出）
	if len(res2.Messages) != 0 {
		t.Fatalf("第二批（纯非 token）期望 0 条 message，实际 %d", len(res2.Messages))
	}
	// NextCursor 等于最后一条非 token event 的 (rowid,id)
	eventCursor2 := res2.NextCursors[SyncSourceOpenCodeEvent]
	if eventCursor2.Value <= eventCursor1.Value {
		t.Fatalf("event cursor 应推进到新 high-water: %d > %d", eventCursor2.Value, eventCursor1.Value)
	}

	// 再追加一条 message.updated.1，下一批必须捕获它
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-3", aggregateID: "sess-1", seq: 3, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-3", "sess-1", "m1", "anthropic", ts, 300, 200, 100)})
	res3 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: res2.NextCursors[SyncSourceOpenCodeMessage],
			SyncSourceOpenCodeEvent:   eventCursor2,
		},
	})
	byID := ocMsgsByID(res3.Messages)
	if _, ok := byID["msg-3"]; !ok {
		t.Fatalf("第三批应捕获 msg-3，实际 messages: %v", ocSortedTS(res3.Messages))
	}
	if byID["msg-3"].TotalTokens != 300 {
		t.Errorf("msg-3 TotalTokens = %d, want 300", byID["msg-3"].TotalTokens)
	}
}

// ===== 哨兵缺失/不一致时从 rowid 0 重扫 =====
func TestOpenCodeCollector_SentinelResetOnStaleCursor(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	// 用一个不一致的 cursor（rowid=999 但 id 不匹配）→ 应 reset 到 0 重扫
	res := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: {},
			SyncSourceOpenCodeEvent:   {Value: 999, ID: "stale-id"},
		},
	})

	if len(res.Messages) != 1 {
		t.Fatalf("哨兵 reset 后应重扫到 1 条 message，实际 %d", len(res.Messages))
	}
	if res.Messages[0].ID != "msg-1" {
		t.Errorf("ID = %q, want msg-1", res.Messages[0].ID)
	}
}

// ===== (time_updated,id) 同毫秒不漏 =====
func TestOpenCodeCollector_CompositeCursorSameTimeUpdated(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	// 三条同 time_updated 不同 id
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-a", sessionID: "sess-1", timeUpdated: 5000,
		info: ocCompletedInfo("msg-a", "sess-1", "m1", "anthropic", ts, 100, 60, 40)})
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-b", sessionID: "sess-1", timeUpdated: 5000,
		info: ocCompletedInfo("msg-b", "sess-1", "m1", "anthropic", ts, 200, 120, 80)})
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-c", sessionID: "sess-1", timeUpdated: 5000,
		info: ocCompletedInfo("msg-c", "sess-1", "m1", "anthropic", ts, 300, 180, 120)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	// 第一批：cursor 指向 msg-a
	res1 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: {Value: 5000, ID: "msg-a"},
			SyncSourceOpenCodeEvent:   {},
		},
	})
	byID := ocMsgsByID(res1.Messages)
	if _, ok := byID["msg-b"]; !ok {
		t.Errorf("同毫秒 cursor 应捕获 msg-b，实际 %v", ocSortedTS(res1.Messages))
	}
	if _, ok := byID["msg-c"]; !ok {
		t.Errorf("同毫秒 cursor 应捕获 msg-c，实际 %v", ocSortedTS(res1.Messages))
	}
	// 不应重复捕获 msg-a
	if _, ok := byID["msg-a"]; ok {
		t.Errorf("同毫秒 cursor 不应重复捕获 msg-a")
	}
}

// ===== token 后填充更新 time_updated 后被捕获 =====
func TestOpenCodeCollector_IncrementalCatchesRunningToCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	// 第一阶段：running 行（无 completed/total），time_updated=1000
	var running openCodeInfo
	running.ID = "msg-1"
	running.SessionID = "sess-1"
	running.Role = "assistant"
	running.ModelID = "m1"
	running.ProviderID = "anthropic"
	running.Time.Created = 1000
	// 无 completed、无 total
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: 1000, info: running})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res1 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: {},
			SyncSourceOpenCodeEvent:   {},
		},
	})
	// running 行不应产出
	if len(res1.Messages) != 0 {
		t.Fatalf("第一批（running）期望 0 条 message，实际 %d", len(res1.Messages))
	}
	msgCursor1 := res1.NextCursors[SyncSourceOpenCodeMessage]

	// 第二阶段：更新为 completed 且 time_updated 增大
	ts := ocDateMS(2026, 7, 1, 10, 0)
	completed := ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 100, 60, 40)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	data, _ := json.Marshal(completed)
	if _, err := db.Exec(`UPDATE message SET data=?, time_updated=? WHERE id=?`, string(data), 2000, "msg-1"); err != nil {
		db.Close()
		t.Fatalf("更新 message 失败: %v", err)
	}
	db.Close()

	res2 := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: msgCursor1,
			SyncSourceOpenCodeEvent:   res1.NextCursors[SyncSourceOpenCodeEvent],
		},
	})
	byID := ocMsgsByID(res2.Messages)
	if _, ok := byID["msg-1"]; !ok {
		t.Fatalf("第二批应捕获 completed 的 msg-1，实际 %v", ocSortedTS(res2.Messages))
	}
}

// ===== 返回结果 (client,id) 唯一，collection count 不放大 =====
func TestOpenCodeCollector_DeduplicatedByClientID(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: ts,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 120, 70, 50)})
	// 同 ID event（total 不同）
	insertOCEvent(t, dbPath, ocEventRow{id: "evt-1", aggregateID: "sess-1", seq: 1, eventType: "message.updated.1",
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 999, 600, 399)})
	// 第二个 message
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-2", sessionID: "sess-1", timeUpdated: ts + 1000,
		info: ocCompletedInfo("msg-2", "sess-1", "m1", "anthropic", ts+1000, 200, 120, 80)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	// msg-1 只应出现一次（message + event 合并）
	idCount := make(map[string]int)
	for _, m := range res.Messages {
		idCount[m.ID]++
	}
	for id, cnt := range idCount {
		if cnt > 1 {
			t.Errorf("ID %q 出现 %d 次，期望最多 1 次（不放大）", id, cnt)
		}
	}
	if len(res.Messages) != 2 {
		t.Errorf("期望 2 条唯一 message，实际 %d", len(res.Messages))
	}
}

// ===== session.parent_id、directory、title、first/last ts 正确 =====
func TestOpenCodeCollector_SessionMetadataFields(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id:          "sess-1",
		parentID:    "parent-1",
		directory:   "/proj/a",
		title:       "实现功能",
		model:       `{"id":"m1","providerID":"anthropic"}`,
		timeCreated: 1000,
		timeUpdated: 5000,
	})
	ts := ocDateMS(2026, 7, 1, 10, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "msg-1", sessionID: "sess-1", timeUpdated: 5000,
		info: ocCompletedInfo("msg-1", "sess-1", "m1", "anthropic", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Sessions) != 1 {
		t.Fatalf("期望 1 个 session，实际 %d", len(res.Sessions))
	}
	s := res.Sessions[0]
	if s.ID != "sess-1" {
		t.Errorf("session ID = %q, want sess-1", s.ID)
	}
	if s.ParentID != "parent-1" {
		t.Errorf("session ParentID = %q, want parent-1", s.ParentID)
	}
	if s.Directory != "/proj/a" {
		t.Errorf("session Directory = %q, want /proj/a", s.Directory)
	}
	if s.Title != "实现功能" {
		t.Errorf("session Title = %q, want 实现功能", s.Title)
	}
	if s.Project != "a" {
		t.Errorf("session Project = %q, want a", s.Project)
	}
	if s.FirstTS != 1000 {
		t.Errorf("session FirstTS = %d, want 1000（取自 session 表）", s.FirstTS)
	}
	if s.LastTS != 5000 {
		t.Errorf("session LastTS = %d, want 5000（取自 session 表）", s.LastTS)
	}
	if s.Client != model.ClientOpenCode {
		t.Errorf("session Client = %q, want %q", s.Client, model.ClientOpenCode)
	}
}

// ===== 边界场景 =====

func TestOpenCodeCollector_DBNotFound(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {
				Enabled: true,
				Paths: map[string]string{
					"db": "/nonexistent/opencode.db",
				},
			},
		},
	}
	c := NewOpenCodeCollector(cfg)
	_, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-07-01"}}, slog.Default())
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
}

func TestOpenCodeCollector_DBPathNotConfigured(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{},
	}
	c := NewOpenCodeCollector(cfg)
	_, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-07-01"}}, slog.Default())
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
}

func TestOpenCodeCollector_NoData(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-07-01"}})

	if len(res.Messages) != 0 {
		t.Errorf("期望 0 条 message，实际 %d", len(res.Messages))
	}
	if len(res.Sessions) != 0 {
		t.Errorf("期望 0 个 session，实际 %d", len(res.Sessions))
	}
}

func TestOpenCodeCollector_IncrementalNextCursorHoldsOnNoNewRows(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{
		id: "sess-1", directory: "/proj/a", title: "T1",
		model: `{"id":"m1","providerID":"anthropic"}`,
	})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	inMsg := model.SyncCursor{Value: 1000, ID: "msg-x"}
	inEvt := model.SyncCursor{Value: 2000, ID: "evt-x"}
	res := ocCollect(t, c, CollectRequest{
		Incremental: true,
		Cursors: map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: inMsg,
			SyncSourceOpenCodeEvent:   inEvt,
		},
	})
	// 无新行时应保持输入 cursor，不回退
	if res.NextCursors[SyncSourceOpenCodeMessage] != inMsg {
		t.Errorf("message cursor 应保持输入值，实际 %+v", res.NextCursors[SyncSourceOpenCodeMessage])
	}
	// event：输入 cursor rowid=2000 但库空 → 哨兵校验通过（sql.ErrNoRows 视为 false → reset）
	// 空 event 表时 highWater 为零值；保持零值
}
