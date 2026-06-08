package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

// writeJSONL 写入临时 JSONL 文件，返回路径
func writeJSONL(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	return path
}

func TestParseWorkBuddyJSONL_AssistantWithUsage(t *testing.T) {
	content := `{"id":"msg-001","timestamp":1749312000000,"type":"message","role":"user","content":[],"sessionId":"sess-001","cwd":"/path"}
{"id":"msg-002","timestamp":1749312060000,"type":"message","role":"assistant","content":[],"providerData":{"model":"claude-sonnet-4-20250514","usage":{"inputTokens":1500,"outputTokens":800,"inputTokensDetails":[{"cached_tokens":1200}],"outputTokensDetails":[{"reasoning_tokens":10}]}},"sessionId":"sess-001","cwd":"/path"}
{"id":"msg-003","timestamp":1749312120000,"type":"message","role":"assistant","content":[],"providerData":{"model":"deepseek-v4-pro","usage":{"inputTokens":2000,"outputTokens":1200,"inputTokensDetails":[{"cached_tokens":0}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"sess-001","cwd":"/path"}
`
	path := writeJSONL(t, content)

	messages, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
	}

	// 只应解析出 2 条带 usage 的 assistant 消息（user 行、无 usage 行都被跳过）
	if len(messages) != 2 {
		t.Fatalf("expected 2 assistant-with-usage messages, got %d", len(messages))
	}

	m0 := messages[0]
	if m0.Model != "claude-sonnet-4-20250514" {
		t.Errorf("messages[0].Model = %q, want claude-sonnet-4-20250514", m0.Model)
	}
	if m0.InputTokens != 1500 || m0.OutputTokens != 800 {
		t.Errorf("messages[0] tokens = in(%d)/out(%d), want 1500/800", m0.InputTokens, m0.OutputTokens)
	}
	if m0.CacheReadTokens != 1200 {
		t.Errorf("messages[0].CacheReadTokens = %d, want 1200", m0.CacheReadTokens)
	}

	if messages[1].Model != "deepseek-v4-pro" {
		t.Errorf("messages[1].Model = %q, want deepseek-v4-pro", messages[1].Model)
	}
}

func TestParseWorkBuddyJSONL_SkipsAssistantWithoutUsage(t *testing.T) {
	// assistant 但无 usage（skipRun/error 等）应被跳过
	content := `{"id":"msg-001","timestamp":1749312000000,"role":"assistant","content":[],"providerData":{"model":"m","skipRun":true},"sessionId":"s","cwd":"/"}
{"id":"msg-002","timestamp":1749312060000,"role":"assistant","content":[],"providerData":{"model":"m","error":"timeout"},"sessionId":"s","cwd":"/"}
`
	path := writeJSONL(t, content)

	messages, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected 0 messages (no usage), got %d", len(messages))
	}
}

func TestParseWorkBuddyJSONL_ModelFallback(t *testing.T) {
	// 主路径用 providerData.model；model 为空时回退 requestModelName
	tests := []struct {
		name, jsonl, wantModel string
	}{
		{
			name:      "short id model preferred",
			jsonl:     `{"id":"m","timestamp":1749312000000,"role":"assistant","providerData":{"model":"deepseek-v4-flash","requestModelName":"DeepSeek-V4 Flash","usage":{"inputTokens":1,"outputTokens":1,"inputTokensDetails":[{"cached_tokens":0}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"s","cwd":"/"}` + "\n",
			wantModel: "deepseek-v4-flash",
		},
		{
			name:      "fallback to requestModelName when model empty",
			jsonl:     `{"id":"m","timestamp":1749312000000,"role":"assistant","providerData":{"requestModelName":"DeepSeek-V4 Flash","usage":{"inputTokens":1,"outputTokens":1,"inputTokensDetails":[{"cached_tokens":0}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"s","cwd":"/"}` + "\n",
			wantModel: "DeepSeek-V4 Flash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeJSONL(t, tt.jsonl)
			messages, err := parseWorkBuddyJSONL(path, slog.Default())
			if err != nil {
				t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("expected 1 message, got %d", len(messages))
			}
			if messages[0].Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", messages[0].Model, tt.wantModel)
			}
		})
	}
}

func TestParseWorkBuddyJSONL_CacheReadFromDetails(t *testing.T) {
	// cached_tokens 缺失时 CacheReadTokens 应为 0，不 panic
	content := `{"id":"m","timestamp":1749312000000,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/"}` + "\n"
	path := writeJSONL(t, content)

	messages, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d, want 0 when details missing", messages[0].CacheReadTokens)
	}
}

func TestParseWorkBuddyJSONL_EmptyFile(t *testing.T) {
	path := writeJSONL(t, "")
	messages, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected 0 messages for empty file, got %d", len(messages))
	}
}

func TestParseWorkBuddyJSONL_MalformedLineSkipped(t *testing.T) {
	content := `{"id":"m1","timestamp":1749312000000,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"inputTokensDetails":[{"cached_tokens":0}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"s","cwd":"/"}
this is not json
{"id":"m2","timestamp":1749312120000,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":300,"outputTokens":40,"inputTokensDetails":[{"cached_tokens":0}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"s","cwd":"/"}
`
	path := writeJSONL(t, content)
	messages, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL failed: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages (malformed skipped), got %d", len(messages))
	}
}

func TestParseWorkBuddyJSONL_RejectsInvalidIdentityAndDeduplicatesByID(t *testing.T) {
	content := `{"id":"dup","timestamp":1749312000000,"role":"assistant","providerData":{"model":"first","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/project"}
{"id":"dup","timestamp":1749312060000,"role":"assistant","providerData":{"model":"second","usage":{"inputTokens":999,"outputTokens":999}},"sessionId":"s","cwd":"/project"}
{"id":"","timestamp":1749312120000,"role":"assistant","providerData":{"model":"empty-id","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/project"}
{"id":"zero-ts","timestamp":0,"role":"assistant","providerData":{"model":"zero-ts","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/project"}
{"id":"valid","timestamp":1749312180000,"role":"assistant","providerData":{"model":"valid","usage":{"inputTokens":200,"outputTokens":80}},"sessionId":"s","cwd":"/project"}
`
	messages, err := parseWorkBuddyJSONL(writeJSONL(t, content), slog.Default())
	if err != nil {
		t.Fatalf("parseWorkBuddyJSONL: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("应只保留首条 dup 与 valid，实际 %+v", messages)
	}
	if messages[0].ID != "dup" || messages[0].Model != "first" || messages[0].InputTokens != 100 {
		t.Fatalf("重复 ID 应稳定保留首条有效记录，实际 %+v", messages[0])
	}
	if messages[1].ID != "valid" {
		t.Fatalf("第二条有效消息 = %+v, want valid", messages[1])
	}
}

func TestParseWorkBuddyJSONL_NonexistentFile(t *testing.T) {
	_, err := parseWorkBuddyJSONL("/nonexistent/file.jsonl", slog.Default())
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

// 以下时间戳用 time.Date(..., time.UTC) 正午生成，确保任何时区都落在同一日期
// （opencode/codex 测试同款约定）。East-8 与 UTC 机器上都得 "2025-06-08"。
func wbTS(year, month, day int) int64 {
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC).UnixMilli()
}

func TestWorkbuddyInferProject(t *testing.T) {
	tests := []struct {
		directory, want string
	}{
		{"/Users/test/WorkBuddy/2026-06-04-15-45-35", "2026-06-04-15-45-35"},
		{"/Users/test/WorkBuddy/app/", "app"}, // 尾斜杠
		{"", ""},
		{"/", "/"},
	}
	for _, tt := range tests {
		if got := workbuddyInferProject(tt.directory); got != tt.want {
			t.Errorf("workbuddyInferProject(%q) = %q, want %q", tt.directory, got, tt.want)
		}
	}
}

func TestLoadWorkBuddyModelsMapping(t *testing.T) {
	content := `[
		{"id":"deepseek-v4-pro","name":"DeepSeek-V4 Pro","vendor":"DeepSeek","apiKey":"secret"},
		{"id":"mimo-v2.5","name":"mimo-v2.5","vendor":"Custom","apiKey":"secret"}
	]`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(content), 0644)

	mapping, err := loadWorkBuddyModelsMapping(dir)
	if err != nil {
		t.Fatalf("loadWorkBuddyModelsMapping failed: %v", err)
	}
	if len(mapping) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mapping))
	}
	if mapping["deepseek-v4-pro"] != "DeepSeek" {
		t.Errorf("mapping[deepseek-v4-pro] = %q, want DeepSeek", mapping["deepseek-v4-pro"])
	}
	if mapping["mimo-v2.5"] != "Custom" {
		t.Errorf("mapping[mimo-v2.5] = %q, want Custom", mapping["mimo-v2.5"])
	}
}

func TestLoadWorkBuddyModelsMapping_FileNotExist(t *testing.T) {
	mapping, err := loadWorkBuddyModelsMapping(t.TempDir())
	if err != nil {
		t.Fatalf("should not fail for missing file: %v", err)
	}
	if len(mapping) != 0 {
		t.Errorf("expected empty mapping, got %d", len(mapping))
	}
}

func TestLoadWorkBuddyModelsMapping_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "models.json"), []byte("not json"), 0644)
	if _, err := loadWorkBuddyModelsMapping(dir); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// createWorkBuddyTestDB 在临时路径创建 workbuddy.db 测试库并插入 sessions
func createWorkBuddyTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	if dbPath == "" {
		dbPath = ":memory:" // 项目惯例：内存库用 :memory: 而非空串
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, cwd TEXT NOT NULL, user_id TEXT NOT NULL,
		title TEXT, custom_title TEXT, status TEXT NOT NULL DEFAULT 'Pending',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		deleted_at INTEGER, is_playground INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 三条 session：有 title、有 custom_title、已删除
	stmts := []string{
		`INSERT INTO sessions (id, cwd, user_id, title, custom_title, status, created_at, updated_at) VALUES ('sess-001','/Users/test/WorkBuddy/a','u','AI标题',NULL,'completed',1749312000,1749312000)`,
		`INSERT INTO sessions (id, cwd, user_id, title, custom_title, status, created_at, updated_at) VALUES ('sess-002','/Users/test/WorkBuddy/b','u','AI标题2','自定义标题','completed',1749398400,1749398400)`,
		`INSERT INTO sessions (id, cwd, user_id, title, custom_title, status, created_at, updated_at, deleted_at) VALUES ('sess-del','/Users/test/WorkBuddy/c','u','已删除',NULL,'completed',1749312000,1749312000,1749312000)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestQueryWorkBuddyTitles_BySessionID(t *testing.T) {
	db := createWorkBuddyTestDB(t, "")

	titles, err := queryWorkBuddyTitles(context.Background(), db)
	if err != nil {
		t.Fatalf("queryWorkBuddyTitles failed: %v", err)
	}
	// 已删除的 sess-del 不应出现
	if _, exists := titles["sess-del"]; exists {
		t.Error("deleted session should be excluded")
	}
	// custom_title 优先于 title
	if titles["sess-002"] != "自定义标题" {
		t.Errorf("titles[sess-002] = %q, want 自定义标题 (custom_title wins)", titles["sess-002"])
	}
	if titles["sess-001"] != "AI标题" {
		t.Errorf("titles[sess-001] = %q, want AI标题", titles["sess-001"])
	}
	if len(titles) != 2 {
		t.Errorf("expected 2 titles, got %d", len(titles))
	}
}

// buildWorkBuddyDir 在 tmpDir 下构造三层目录结构并写入 JSONL
// 返回 (workbuddyRoot, projectsDir)
func buildWorkBuddyDir(t *testing.T, sessionDir, sessionID, jsonl string) (string, string) {
	t.Helper()
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	dir := filepath.Join(projectsDir, sessionDir)
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(jsonl), 0644)
	return root, projectsDir
}

// usageLine 生成一条带 usage 的 assistant JSONL 行
// 注意：内容里的 sessionId 故意写死为 "content-sid"，与文件名 sessionID 不同——
// 用以验证「ID/title 关联基于文件名，与内容 sessionId 解耦」（决策 3）
func usageLine(ts int64, model string, in, out, cache int64) string {
	return fmt.Sprintf(`{"id":"m%d","timestamp":%d,"role":"assistant","providerData":{"model":"%s","usage":{"inputTokens":%d,"outputTokens":%d,"inputTokensDetails":[{"cached_tokens":%d}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"content-sid","cwd":"/Users/test/WorkBuddy/app"}`,
		ts, ts, model, in, out, cache)
}

func TestWorkBuddyCollector_Collect_BasicFlow(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "deepseek-v4-pro", 1000, 500, 800) + "\n" +
		usageLine(wbTS(2025, 6, 8)+60000, "deepseek-v4-pro", 2000, 800, 1500) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "Users-test-WorkBuddy-app", "sess-001", jsonl)
	os.WriteFile(filepath.Join(root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
		},
	}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	// 消息级：两条同日同模型 usage 各一行，不再聚合
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages (one per usage), got %d", len(result.Messages))
	}
	for _, m := range result.Messages {
		if m.Client != model.ClientWorkBuddy {
			t.Errorf("Client = %q, want %q", m.Client, model.ClientWorkBuddy)
		}
		if m.Model != "deepseek-v4-pro" {
			t.Errorf("Model = %q, want deepseek-v4-pro", m.Model)
		}
		if m.Provider != "DeepSeek" {
			t.Errorf("Provider = %q, want DeepSeek", m.Provider)
		}
		if m.SessionID != "sess-001" {
			t.Errorf("SessionID = %q, want sess-001（基于文件名）", m.SessionID)
		}
	}
}

func TestWorkBuddyCollector_Collect_ThreeLevelPath(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 10, 5, 0) + "\n"
	_, projectsDir := buildWorkBuddyDir(t, "Users-test-WorkBuddy-x", "uuid-001", jsonl)

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
		},
	}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("three-level scan must find 1 message, got %d (一层 glob 会得 0)", len(result.Messages))
	}
}

func TestWorkBuddyCollector_Collect_DateFilter(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n" +
		usageLine(wbTS(2025, 6, 9), "m", 200, 100, 0) + "\n"
	_, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message (date filtered), got %d", len(result.Messages))
	}
	if result.Messages[0].InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100 (only 2025-06-08)", result.Messages[0].InputTokens)
	}
}

func TestWorkBuddyCollector_Collect_MultiSessionsSameDaySameModel(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	dir := filepath.Join(projectsDir, "Users-test-WorkBuddy-app")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "sess-001.jsonl"),
		[]byte(usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0)+"\n"), 0644)
	os.WriteFile(filepath.Join(dir, "sess-002.jsonl"),
		[]byte(usageLine(wbTS(2025, 6, 8)+60000, "m", 200, 100, 0)+"\n"), 0644)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages (two different session files), got %d", len(result.Messages))
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("expected 2 sessions (one per physical file), got %d", len(result.Sessions))
	}
	idSet := map[string]bool{}
	for _, s := range result.Sessions {
		idSet[s.ID] = true
	}
	if len(idSet) != 2 {
		t.Errorf("expected 2 distinct Session IDs, got %d: %v — ID 必须基于文件名以保证唯一", len(idSet), idSet)
	}
}

func TestWorkBuddyCollector_Collect_WithDBTitles(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)

	dbPath := filepath.Join(root, "workbuddy.db")
	createWorkBuddyTestDB(t, dbPath)
	dbh, _ := sql.Open("sqlite", dbPath)
	dbh.Exec(`DELETE FROM sessions WHERE id = 'sess-001'`)
	dbh.Exec(`INSERT INTO sessions (id, cwd, user_id, title, status, created_at, updated_at) VALUES ('sess-001','/Users/test/WorkBuddy/app','u','从DB查到的标题','completed',1749312000,1749312000)`)
	dbh.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": projectsDir,
			"db":           dbPath,
		}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(result.Sessions))
	}
	if result.Sessions[0].Title != "从DB查到的标题" {
		t.Errorf("Title = %q, want 从DB查到的标题（按文件名 sess-001 关联，而非内容 content-sid）", result.Sessions[0].Title)
	}
}

func TestWorkBuddyCollector_Collect_EmptyDir(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	os.MkdirAll(projectsDir, 0755)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect should not fail for empty dir: %v", err)
	}
	if len(result.Messages) != 0 || len(result.Sessions) != 0 {
		t.Errorf("expected 0 messages/sessions, got %d/%d", len(result.Messages), len(result.Sessions))
	}
}

func TestWorkBuddyCollector_Collect_Disabled(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: false, Paths: map[string]string{"projects_dir": "/tmp"}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("disabled client should return 0 messages, got %d", len(result.Messages))
	}
}

func TestWorkBuddyCollector_UpsertIntegration(t *testing.T) {
	// 构造 Collector 输出，验证经 UpsertSessionMeta 能正确写入 sessions 表
	jsonl := usageLine(wbTS(2025, 6, 8), "deepseek-v4-pro", 1000, 500, 800) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)
	os.WriteFile(filepath.Join(root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	collector := NewWorkBuddyCollector(cfg)
	result, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("Collect 前置失败: err=%v sessions=%d", err, len(result.Sessions))
	}
	sessions := result.Sessions

	// 落库
	dbObj, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存 DB 失败: %v", err)
	}
	defer dbObj.Close()

	count, err := db.UpsertSessionMeta(context.Background(), dbObj, sessions)
	if err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}
	if count != 1 {
		t.Errorf("UpsertSessionMeta count = %d, want 1", count)
	}

	// 幂等：重复落库不产生重复
	count2, _ := db.UpsertSessionMeta(context.Background(), dbObj, sessions)
	if count2 != 1 {
		t.Errorf("重复落库 count = %d, want 1 (ON CONFLICT 幂等)", count2)
	}
	var total int
	dbObj.QueryRow(`SELECT COUNT(*) FROM sessions WHERE client = ?`, model.ClientWorkBuddy).Scan(&total)
	if total != 1 {
		t.Errorf("sessions 条数 = %d, want 1 (幂等)", total)
	}
}

// testLogHandler 用于捕获日志输出的 slog.Handler
type testLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *testLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *testLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// slog.Record 可能复用内部存储；跨 Handle 生命周期保存时必须 Clone。
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(name string) slog.Handler       { return h }

func (h *testLogHandler) Messages() []string {
	records := h.Records()
	msgs := make([]string, 0, len(records))
	for _, r := range records {
		msgs = append(msgs, r.Message)
	}
	return msgs
}

func (h *testLogHandler) Records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	records := make([]slog.Record, 0, len(h.records))
	for _, r := range h.records {
		records = append(records, r.Clone())
	}
	return records
}

func (h *testLogHandler) HasMessage(substr string) bool {
	for _, msg := range h.Messages() {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

func (h *testLogHandler) HasRecord(level slog.Level, message string) bool {
	for _, record := range h.Records() {
		if record.Level == level && record.Message == message {
			return true
		}
	}
	return false
}

func TestWorkBuddy_TitleQueryFailure_LogsDebug(t *testing.T) {
	// 准备测试数据：创建临时 workbuddy 目录和 projects 子目录
	tmpDir := t.TempDir()
	projectsDir := filepath.Join(tmpDir, "projects")
	sessionDir := filepath.Join(projectsDir, "session-001")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 创建 JSONL 文件在子目录下（匹配 projects/*/*.jsonl）
	jsonlPath := filepath.Join(sessionDir, "test.jsonl")
	ts := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local).UnixMilli()
	content := fmt.Sprintf(`{"id":"msg-001","timestamp":%d,"role":"assistant","content":[],"providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/"}
`, ts)
	if err := os.WriteFile(jsonlPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// 配置：db 路径指向不存在的文件，触发 title 查询失败
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": projectsDir,
			"db":           filepath.Join(tmpDir, "nonexistent.db"),
		}},
	}}
	c := NewWorkBuddyCollector(cfg)
	handler := &testLogHandler{}
	logger := slog.New(handler)

	// 执行采集
	if _, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-06-15"}}, logger); err != nil {
		t.Fatal(err)
	}

	if !handler.HasRecord(slog.LevelDebug, "WorkBuddy title 查询失败，降级为空 title") {
		t.Errorf("expected exact debug record, got: %v", handler.Messages())
	}
}

func TestWorkBuddy_BadLineLogsDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := "not-json\n" + `{"id":"m1","role":"assistant","timestamp":1781539200000,"sessionId":"s",` +
		`"providerData":{"model":"m","usage":{"inputTokens":1,"outputTokens":2}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	handler := &testLogHandler{}
	messages, err := parseWorkBuddyJSONL(path, slog.New(handler))
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if !handler.HasRecord(slog.LevelDebug, "WorkBuddy JSONL 行解析失败，跳过") {
		t.Fatalf("missing bad-line debug record: %v", handler.Messages())
	}
}

func TestWorkBuddy_FileParseFailureLogsWarn(t *testing.T) {
	_, projectsDir := buildWorkBuddyDir(t, "dir", "session",
		strings.Repeat("x", maxJSONLLineSize+1)+"\n")
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	handler := &testLogHandler{}
	result, err := NewWorkBuddyCollector(cfg).Collect(context.Background(),
		CollectRequest{Dates: []string{"2025-06-08"}}, slog.New(handler))
	if err != nil || len(result.Messages) != 0 {
		t.Fatalf("messages=%+v err=%v", result.Messages, err)
	}
	if !handler.HasRecord(slog.LevelWarn, "WorkBuddy JSONL 文件解析失败，跳过") {
		t.Fatalf("missing file-failure warn record: %v", handler.Messages())
	}
}

// 不再按 date/model 聚合，每个顶层 message.id 一行 Message。
// 同日同 model 的两条 assistant usage 必须产出两条独立 Message，而非一个聚合 Session。
func TestWorkBuddyCollector_OneRowPerUsage(t *testing.T) {
	jsonl := `{"id":"wb-m1","timestamp":1750001000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"inputTokensDetails":[{"cached_tokens":300}]}}}
{"id":"wb-m2","timestamp":1750002000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":2000,"outputTokens":200,"totalTokens":2200,"inputTokensDetails":[{"cached_tokens":500}]}}}
`
	_, projectsDir := buildWorkBuddyDir(t, "Users-test-WorkBuddy-app", "wb-sess-001", jsonl)
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}

	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 Messages (one per usage), got %d — 旧聚合实现会塌缩成 1 个 Session", len(result.Messages))
	}

	byID := map[string]model.Message{}
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if _, ok := byID["wb-m1"]; !ok {
		t.Errorf("missing message wb-m1; got ids %v", msgIDs(result.Messages))
	}
	if _, ok := byID["wb-m2"]; !ok {
		t.Errorf("missing message wb-m2; got ids %v", msgIDs(result.Messages))
	}
}

// usage.totalTokens 存在时原样保留；缺失时回退 input+output。
func TestWorkBuddyCollector_UsesSourceTotal(t *testing.T) {
	// m1 带 totalTokens=1100（原样保留），m2 缺 totalTokens（回退 2000+200=2200）
	jsonl := `{"id":"wb-t1","timestamp":1750001000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/p","providerData":{"model":"glm-a","usage":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"inputTokensDetails":[{"cached_tokens":300}]}}}
{"id":"wb-t2","timestamp":1750002000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/p","providerData":{"model":"glm-a","usage":{"inputTokens":2000,"outputTokens":200,"inputTokensDetails":[{"cached_tokens":500}]}}}
`
	_, projectsDir := buildWorkBuddyDir(t, "dir", "wb-sess-001", jsonl)
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}

	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 Messages, got %d", len(result.Messages))
	}
	byID := map[string]model.Message{}
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if got := byID["wb-t1"].TotalTokens; got != 1100 {
		t.Errorf("wb-t1 TotalTokens = %d, want 1100 (原样保留 source total)", got)
	}
	if got := byID["wb-t2"].TotalTokens; got != 2200 {
		t.Errorf("wb-t2 TotalTokens = %d, want 2200 (缺失时回退 input+output)", got)
	}
}

// FreshInput = max(0, input - cache_read)。
func TestWorkBuddyCollector_FreshInputSubtractsCache(t *testing.T) {
	jsonl := `{"id":"wb-m1","timestamp":1750001000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"inputTokensDetails":[{"cached_tokens":300}]}}}
{"id":"wb-m2","timestamp":1750002000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":2000,"outputTokens":200,"totalTokens":2200,"inputTokensDetails":[{"cached_tokens":500}]}}}
`
	_, projectsDir := buildWorkBuddyDir(t, "dir", "wb-sess-001", jsonl)
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}

	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 Messages, got %d", len(result.Messages))
	}
	byID := map[string]model.Message{}
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	// fresh = max(0, input - cache_read)
	if got := byID["wb-m1"].FreshInputTokens; got != 700 {
		t.Errorf("wb-m1 FreshInputTokens = %d, want 700 (1000-300)", got)
	}
	if got := byID["wb-m2"].FreshInputTokens; got != 1500 {
		t.Errorf("wb-m2 FreshInputTokens = %d, want 1500 (2000-500)", got)
	}
}

// msgIDs 提取 Message.ID 列表，便于错误信息可读
func msgIDs(msgs []model.Message) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// daemon 增量模式（ChangedFile）只采集该文件，忽略同目录其他文件。
func TestWorkBuddyCollector_ChangedFileOnly(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	dir := filepath.Join(projectsDir, "Users-test-WorkBuddy-app")
	os.MkdirAll(dir, 0755)

	target := filepath.Join(dir, "target.jsonl")
	os.WriteFile(target, []byte(usageLine(wbTS(2025, 6, 8), "glm-a", 100, 50, 0)+"\n"), 0644)
	// 干扰文件：不应被 ChangedFile 模式采集
	os.WriteFile(filepath.Join(dir, "other.jsonl"),
		[]byte(`{"id":"wb-other","timestamp":1750001000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"model":"glm-a","usage":{"inputTokens":999,"outputTokens":1,"totalTokens":1000,"inputTokensDetails":[{"cached_tokens":0}]}}}`+"\n"), 0644)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{
		Dates:       []string{"2025-06-08"},
		ChangedFile: target,
	}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	// usageLine 生成的 id 形如 "m<ts>"；干扰文件 id 为 wb-other，不应出现
	wantID := fmt.Sprintf("m%d", wbTS(2025, 6, 8))
	if len(result.Messages) != 1 || result.Messages[0].ID != wantID {
		t.Fatalf("expected only target file message %q, got %+v", wantID, msgIDs(result.Messages))
	}
}

// model 缺失时回退 requestModelName；models.json vendor 映射保持。
func TestWorkBuddyCollector_ModelFallbackAndVendorMapping(t *testing.T) {
	// m1: providerData.model 缺失 -> 回退 requestModelName "DeepSeek-V4 Pro"
	// m2: providerData.model="deepseek-v4-pro" -> models.json 映射 vendor "DeepSeek"
	jsonl := `{"id":"wb-fb","timestamp":1750001000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"requestModelName":"DeepSeek-V4 Pro","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}}}
{"id":"wb-map","timestamp":1750002000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"model":"deepseek-v4-pro","usage":{"inputTokens":200,"outputTokens":100,"totalTokens":300,"inputTokensDetails":[{"cached_tokens":0}]}}}
`
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)
	os.WriteFile(filepath.Join(root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}
	byID := map[string]model.Message{}
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	// model 缺失时回退 requestModelName
	if got := byID["wb-fb"].Model; got != "DeepSeek-V4 Pro" {
		t.Errorf("wb-fb Model = %q, want DeepSeek-V4 Pro (requestModelName 回退)", got)
	}
	// models.json 中无 requestModelName 映射 -> provider 为空
	if got := byID["wb-fb"].Provider; got != "" {
		t.Errorf("wb-fb Provider = %q, want 空 (models.json 无此 model 映射)", got)
	}
	// model 短 id 经 models.json 映射 vendor
	if got := byID["wb-map"].Model; got != "deepseek-v4-pro" {
		t.Errorf("wb-map Model = %q, want deepseek-v4-pro", got)
	}
	if got := byID["wb-map"].Provider; got != "DeepSeek" {
		t.Errorf("wb-map Provider = %q, want DeepSeek (models.json vendor 映射)", got)
	}
}

// workbuddy.db 不可读（查询失败）时 token 仍采集，title 为空。
func TestWorkBuddyCollector_DBQueryFailureStillCollects(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "glm-a", 100, 50, 0) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": projectsDir,
			"db":           filepath.Join(root, "nonexistent.db"), // 不存在 -> 查询失败降级
		}},
	}}
	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	// token 仍采集
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message even when DB unreadable, got %d", len(result.Messages))
	}
	if result.Messages[0].InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", result.Messages[0].InputTokens)
	}
	// title 为空（DB 查询失败降级）
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(result.Sessions))
	}
	if result.Sessions[0].Title != "" {
		t.Errorf("Title = %q, want 空 (DB 查询失败降级)", result.Sessions[0].Title)
	}
}

// Session 元数据 title/directory/project/first-last ts 正确。
func TestWorkBuddyCollector_SessionMetadataFields(t *testing.T) {
	jsonl := usageLine(1000, "glm-a", 100, 50, 0) + "\n" +
		usageLine(3000, "glm-a", 200, 100, 0) + "\n" +
		usageLine(2000, "glm-a", 300, 150, 0) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)
	os.WriteFile(filepath.Join(root, "models.json"), []byte(`[{"id":"glm-a","vendor":"Zhipu"}]`), 0644)

	// 准备 workbuddy.db title
	dbPath := filepath.Join(root, "workbuddy.db")
	createWorkBuddyTestDB(t, dbPath)
	dbh, _ := sql.Open("sqlite", dbPath)
	dbh.Exec(`DELETE FROM sessions WHERE id = 'sess-001'`)
	dbh.Exec(`INSERT INTO sessions (id, cwd, user_id, title, status, created_at, updated_at) VALUES ('sess-001','/Users/test/WorkBuddy/app','u','我的会话','completed',1000,1000)`)
	dbh.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": projectsDir,
			"db":           dbPath,
		}},
	}}
	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(result.Sessions))
	}
	s := result.Sessions[0]
	if s.ID != "sess-001" {
		t.Errorf("Session ID = %q, want sess-001（文件名去 .jsonl）", s.ID)
	}
	if s.Title != "我的会话" {
		t.Errorf("Title = %q, want 我的会话", s.Title)
	}
	if s.Directory != "/Users/test/WorkBuddy/app" {
		t.Errorf("Directory = %q, want /Users/test/WorkBuddy/app", s.Directory)
	}
	if s.Project != "app" {
		t.Errorf("Project = %q, want app", s.Project)
	}
	// first/last ts 来自该文件全部有效消息（乱序输入 1000/3000/2000）
	if s.FirstTS != 1000 {
		t.Errorf("FirstTS = %d, want 1000", s.FirstTS)
	}
	if s.LastTS != 3000 {
		t.Errorf("LastTS = %d, want 3000", s.LastTS)
	}
	// 三条消息各自一行
	if len(result.Messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(result.Messages))
	}
}
