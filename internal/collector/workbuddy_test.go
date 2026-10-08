package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

	messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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

	messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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
			messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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

	messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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
	messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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
	messages, _, _, err := parseWorkBuddyJSONL(path, slog.Default())
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
	messages, _, _, err := parseWorkBuddyJSONL(writeJSONL(t, content), slog.Default())
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
	_, _, _, err := parseWorkBuddyJSONL("/nonexistent/file.jsonl", slog.Default())
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

func TestLoadWorkBuddyModelsMapping_CaseInsensitiveFallbackKey(t *testing.T) {
	content := `[
		{"id":"GLM-5.3-Flash","vendor":"GLM Coding Plan"},
		{"id":"gLM-5.3-FLASH","vendor":"Other Vendor"},
		{"id":"deepseek-v4-flash","vendor":"DeepSeek"},
		{"id":"glm-x","vendor":"A"},
		{"id":"GLM-X","vendor":"B"},
		{"id":"GLM-Y","vendor":"Old"},
		{"id":"GLM-Y","vendor":"New"},
		{"id":"GLM-Z","vendor":"First"},
		{"id":"glm-z","vendor":"LowerTakesOver"},
		{"id":"GLM-Z","vendor":"MustNotWin"}
	]`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(content), 0644)

	mapping, err := loadWorkBuddyModelsMapping(dir)
	if err != nil {
		t.Fatalf("loadWorkBuddyModelsMapping failed: %v", err)
	}
	// 精确键保留原始大小写条目
	if mapping["GLM-5.3-Flash"] != "GLM Coding Plan" {
		t.Errorf("mapping[GLM-5.3-Flash] = %q, want GLM Coding Plan", mapping["GLM-5.3-Flash"])
	}
	// 混合大小写条目补小写兜底键；小写键已被先到条目占用时不覆盖
	if mapping["glm-5.3-flash"] != "GLM Coding Plan" {
		t.Errorf("mapping[glm-5.3-flash] = %q, want GLM Coding Plan", mapping["glm-5.3-flash"])
	}
	// 先到的全小写精确键优先于后到混合条目的小写兜底键
	if mapping["glm-x"] != "A" {
		t.Errorf("mapping[glm-x] = %q, want A", mapping["glm-x"])
	}
	if mapping["GLM-X"] != "B" {
		t.Errorf("mapping[GLM-X] = %q, want B", mapping["GLM-X"])
	}
	// 同一原始 id 的重复条目：小写兜底键跟随精确键一起更新（last-wins 一致）
	if mapping["GLM-Y"] != "New" {
		t.Errorf("mapping[GLM-Y] = %q, want New", mapping["GLM-Y"])
	}
	if mapping["glm-y"] != "New" {
		t.Errorf("mapping[glm-y] = %q, want New (同一原始 id 的兜底键跟随更新)", mapping["glm-y"])
	}
	// 反向交叉：全小写条目接管兜底键后，原混合 id 的重复条目不得反覆盖该精确值
	if mapping["glm-z"] != "LowerTakesOver" {
		t.Errorf("mapping[glm-z] = %q, want LowerTakesOver (全小写精确键接管后不受原 id 重复反覆盖)", mapping["glm-z"])
	}
	if mapping["GLM-Z"] != "MustNotWin" {
		t.Errorf("mapping[GLM-Z] = %q, want MustNotWin", mapping["GLM-Z"])
	}
	// 全小写条目不重复补键：8 个精确键（GLM-Y/GLM-Z 各两条重复合并）+ 2 个小写兜底键
	if len(mapping) != 10 {
		t.Errorf("expected 10 mappings, got %d", len(mapping))
	}
}

// workbuddyMetaRow 是测试用源库会话元组（指针字段按 SQL NULL 处理）。
type workbuddyMetaRow struct {
	id          string
	cwd         string
	title       *string
	customTitle *string
	sourceMode  *string
	mode        *string
	playground  *int
	expertID    *string
	deleted     bool
}

func wbPtr[T any](v T) *T { return &v }

// writeWorkBuddyMetaDB 在 root 下创建 workbuddy.db（含新合同必要列）并写入
// 元组行，返回 db 路径。
func writeWorkBuddyMetaDB(t *testing.T, root string, rows ...workbuddyMetaRow) string {
	t.Helper()
	dbPath := filepath.Join(root, "workbuddy.db")
	dbh, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	if _, err := dbh.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, cwd TEXT, user_id TEXT NOT NULL,
		title TEXT, custom_title TEXT, status TEXT NOT NULL DEFAULT 'Pending',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		deleted_at INTEGER, is_playground INTEGER,
		source_mode TEXT, mode TEXT, expert_id TEXT
	)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	for _, r := range rows {
		var deleted any
		if r.deleted {
			deleted = 1749312000
		}
		if _, err := dbh.Exec(`INSERT INTO sessions
			(id, cwd, user_id, title, custom_title, status, created_at, updated_at,
			 deleted_at, is_playground, source_mode, mode, expert_id)
			VALUES (?,?,?,?,?,'completed',1749312000,1749312000,?,?,?,?,?)`,
			r.id, r.cwd, "u", r.title, r.customTitle, deleted, r.playground, r.sourceMode, r.mode, r.expertID); err != nil {
			t.Fatalf("插入元组 %s 失败: %v", r.id, err)
		}
	}
	if err := dbh.Close(); err != nil {
		t.Fatalf("关闭测试 DB 失败: %v", err)
	}
	return dbPath
}

func TestQueryWorkBuddyMetadata(t *testing.T) {
	root := t.TempDir()
	dbPath := writeWorkBuddyMetaDB(t, root,
		workbuddyMetaRow{id: "sess-001", cwd: "/Users/test/WorkBuddy/a", title: wbPtr("AI标题")},
		workbuddyMetaRow{id: "sess-002", cwd: "/Users/test/WorkBuddy/b", title: wbPtr("AI标题2"), customTitle: wbPtr("自定义标题")},
		workbuddyMetaRow{id: "sess-del", cwd: "/x", title: wbPtr("已删除"), deleted: true},
		workbuddyMetaRow{id: "sess-null-title", cwd: "/x"},
	)
	dbh, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dbh.Close()

	metas, err := queryWorkBuddyMetadata(context.Background(), dbh)
	if err != nil {
		t.Fatalf("queryWorkBuddyMetadata failed: %v", err)
	}
	if _, exists := metas["sess-del"]; exists {
		t.Error("deleted session should be excluded")
	}
	if metas["sess-002"].customTitle != "自定义标题" || metas["sess-002"].title != "AI标题2" {
		t.Errorf("sess-002 = %+v, want custom_title 与 title 各自独立读取", metas["sess-002"])
	}
	// NULL title 按空串读取、行保留在 map 中（空标题仍是有效元数据）
	if metas["sess-null-title"].title != "" {
		t.Errorf("NULL title 应按空串读取, got %q", metas["sess-null-title"].title)
	}
	if metas["sess-001"].playgroundOK {
		t.Errorf("NULL is_playground 应读取为无效（playgroundOK=false），got %+v", metas["sess-001"])
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 metas, got %d", len(metas))
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
// 用以验证「ID/title 关联基于文件名，与内容 sessionId 解耦」
func usageLine(ts int64, model string, in, out, cache int64) string {
	return fmt.Sprintf(`{"id":"m%d","timestamp":%d,"role":"assistant","providerData":{"model":"%s","usage":{"inputTokens":%d,"outputTokens":%d,"inputTokensDetails":[{"cached_tokens":%d}],"outputTokensDetails":[{"reasoning_tokens":0}]}},"sessionId":"content-sid","cwd":"/Users/test/WorkBuddy/app"}`,
		ts, ts, model, in, out, cache)
}

// wbTestEnv 是一个完整的 WorkBuddy 测试环境：projects JSONL + 元数据库。
type wbTestEnv struct {
	root        string
	projectsDir string
	dbPath      string
}

func newWBTestEnv(t *testing.T, metas []workbuddyMetaRow, files map[string]string) *wbTestEnv {
	t.Helper()
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	// 约定：files 的 key 形如 "dir/sessionID"（目录名/文件名，文件名为去
	// .jsonl 的 sessionID）
	for key, content := range files {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			t.Fatalf("files key 必须形如 dir/sessionID: %q", key)
		}
		dir := filepath.Join(projectsDir, parts[0])
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, parts[1]+".jsonl"), []byte(content), 0644)
	}
	dbPath := writeWorkBuddyMetaDB(t, root, metas...)
	return &wbTestEnv{root: root, projectsDir: projectsDir, dbPath: dbPath}
}

func (e *wbTestEnv) cfg() *config.Config {
	return &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": e.projectsDir,
			"db":           e.dbPath,
		}},
	}}
}

// defaultWBMetas 为给定 session id 生成普通有效元组（非 playground）。
func defaultWBMetas(ids ...string) []workbuddyMetaRow {
	rows := make([]workbuddyMetaRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, workbuddyMetaRow{
			id: id, cwd: "/Users/test/WorkBuddy/app", title: wbPtr("标题-" + id), playground: wbPtr(0),
		})
	}
	return rows
}

func TestWorkBuddyCollector_Collect_BasicFlow(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "deepseek-v4-pro", 1000, 500, 800) + "\n" +
		usageLine(wbTS(2025, 6, 8)+60000, "deepseek-v4-pro", 2000, 800, 1500) + "\n"
	env := newWBTestEnv(t, defaultWBMetas("sess-001"),
		map[string]string{"Users-test-WorkBuddy-app/sess-001": jsonl})
	os.WriteFile(filepath.Join(env.root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	collector := NewWorkBuddyCollector(env.cfg())
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
		if m.Project != "app" {
			t.Errorf("Project = %q, want app（元数据 cwd basename）", m.Project)
		}
	}
	// 计划覆盖触达会话
	if len(result.WorkBuddyPlans) != 1 || result.WorkBuddyPlans[0].SessionID != "sess-001" {
		t.Fatalf("plans = %+v, want 单个 sess-001 计划", result.WorkBuddyPlans)
	}
}

func TestWorkBuddyCollector_Collect_ThreeLevelPath(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 10, 5, 0) + "\n"
	env := newWBTestEnv(t, defaultWBMetas("uuid-001"),
		map[string]string{"Users-test-WorkBuddy-x/uuid-001": jsonl})

	collector := NewWorkBuddyCollector(env.cfg())
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
	env := newWBTestEnv(t, defaultWBMetas("sess-001"), map[string]string{"dir/sess-001": jsonl})

	collector := NewWorkBuddyCollector(env.cfg())
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
	dbPath := writeWorkBuddyMetaDB(t, root, defaultWBMetas("sess-001", "sess-002")...)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir, "db": dbPath}},
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
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "sess-001", cwd: "/Users/test/WorkBuddy/app", title: wbPtr("从DB查到的标题"), playground: wbPtr(0)}},
		map[string]string{"dir/sess-001": jsonl})

	collector := NewWorkBuddyCollector(env.cfg())
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
	dbPath := writeWorkBuddyMetaDB(t, root)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir, "db": dbPath}},
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
		"workbuddy": {Enabled: false, Paths: map[string]string{"projects_dir": "/tmp", "db": "/tmp/x.db"}},
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
	jsonl := usageLine(wbTS(2025, 6, 8), "deepseek-v4-pro", 1000, 500, 800) + "\n"
	env := newWBTestEnv(t, defaultWBMetas("sess-001"), map[string]string{"dir/sess-001": jsonl})
	os.WriteFile(filepath.Join(env.root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	collector := NewWorkBuddyCollector(env.cfg())
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

// 元数据强依赖合同：db 打开失败（文件不存在）时整轮失败，不再降级采集 token。
func TestWorkBuddy_MetadataDBUnreachableFailsWholeRound(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	root, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{
			"projects_dir": projectsDir,
			"db":           filepath.Join(root, "nonexistent.db"),
		}},
	}}
	c := NewWorkBuddyCollector(cfg)
	handler := &testLogHandler{}

	result, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.New(handler))
	if err == nil {
		t.Fatalf("Collect 应整轮失败（元数据强依赖），got result=%+v", result)
	}
	if !strings.Contains(err.Error(), WorkBuddyMetadataErrPrefix) {
		t.Errorf("错误应含固定片段 %q, got %v", WorkBuddyMetadataErrPrefix, err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("整轮失败不应产出消息, got %d", len(result.Messages))
	}
}

// db 路径未配置：配置不完整，必须报告失败；不能在检查 db 前因文件列表空而
// 静默成功返回（projects_dir 同时为空也不行）。
func TestWorkBuddy_EmptyDBPathFailsConfigIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name        string
		projectsDir string
	}{
		{"projects_dir configured", "/some/dir"},
		{"both paths empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Clients: map[string]config.Client{
				"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": tc.projectsDir}},
			}}
			c := NewWorkBuddyCollector(cfg)
			result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
			if err == nil {
				t.Fatalf("db 未配置应报配置不完整失败, got result=%+v", result)
			}
			if !strings.Contains(err.Error(), WorkBuddyMetadataErrPrefix) {
				t.Errorf("错误应含固定片段 %q, got %v", WorkBuddyMetadataErrPrefix, err)
			}
			if !strings.Contains(err.Error(), "db 路径未配置") {
				t.Errorf("错误应说明 db 路径未配置, got %v", err)
			}
		})
	}
}

// db 有效、projects_dir 为空：合法的仅历史刷新形态——不报配置失败、不因无
// JSONL 提前返回；无日期全量请求的计划仍覆盖全部有效元数据会话。
func TestWorkBuddy_NoProjectsDirHistoryOnlyPlan(t *testing.T) {
	root := t.TempDir()
	dbPath := writeWorkBuddyMetaDB(t, root, defaultWBMetas("hist-001", "hist-002")...)
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": dbPath}},
	}}
	c := NewWorkBuddyCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("仅历史刷新形态不应报配置失败: %v", err)
	}
	if len(result.Messages) != 0 || len(result.Sessions) != 0 {
		t.Errorf("无 JSONL 不产出消息/会话, got %d/%d", len(result.Messages), len(result.Sessions))
	}
	if len(result.WorkBuddyPlans) != 2 {
		t.Fatalf("无日期全量请求计划应覆盖全部有效元数据会话, got %+v", result.WorkBuddyPlans)
	}
	if result.WorkBuddyPlans[0].SessionID != "hist-001" || result.WorkBuddyPlans[1].SessionID != "hist-002" {
		t.Errorf("计划顺序应按 sessionID 升序, got %+v", result.WorkBuddyPlans)
	}
	// 日期请求（非全量）在无 JSONL 时不产出任何计划
	result2, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("日期请求: %v", err)
	}
	if len(result2.WorkBuddyPlans) != 0 {
		t.Errorf("日期请求不应产出仅历史计划, got %+v", result2.WorkBuddyPlans)
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
	messages, _, _, err := parseWorkBuddyJSONL(path, slog.New(handler))
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if !handler.HasRecord(slog.LevelDebug, "WorkBuddy JSONL line parse failed, skipped") {
		t.Fatalf("missing bad-line debug record: %v", handler.Messages())
	}
}

// 超限行（> maxJSONLLineSize）不再令整文件读取失败：计入坏行（Debug 心跳）
// 后继续。文件级 Warn 失败路径仅剩真实 IO 错误可触发（无法稳定构造，不再代理）。
func TestWorkBuddy_OversizedLineSkippedLogsBadLine(t *testing.T) {
	env := newWBTestEnv(t, defaultWBMetas("session"),
		map[string]string{"dir/session": strings.Repeat("x", maxJSONLLineSize+1) + "\n"})
	handler := &testLogHandler{}
	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(),
		CollectRequest{Dates: []string{"2025-06-08"}}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect err = %v, want nil（超限行不构成文件级失败）", err)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("messages=%+v, want 0 条", result.Messages)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr = %v, want nil", result.PartialErr)
	}
	if !handler.HasRecord(slog.LevelDebug, "WorkBuddy JSONL line parse failed, skipped") {
		t.Fatalf("missing bad-line debug record: %v", handler.Messages())
	}
}

// 文件级失败（打开/读取终止性错误）路径：chmod 000 稳定触发 EACCES，Warn +
// PartialErr（超限行已降级为坏行，不再触发本路径）。
func TestWorkBuddy_FileParseFailureLogsWarn(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("windows/root 下 chmod 000 不产生打开失败，无法触发文件级失败路径")
	}
	env := newWBTestEnv(t, defaultWBMetas("session"), map[string]string{"dir/session": "{}\n"})
	bad := filepath.Join(env.projectsDir, "dir", "session.jsonl")
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	handler := &testLogHandler{}
	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(),
		CollectRequest{Dates: []string{"2025-06-08"}}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect err = %v, want nil（单文件失败不拖垮整体）", err)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("messages=%+v, want 0 条", result.Messages)
	}
	if result.PartialErr == nil {
		t.Errorf("坏文件失败应报告 PartialErr")
	}
	if !handler.HasRecord(slog.LevelWarn, "WorkBuddy JSONL file parse failed, skipped") {
		t.Fatalf("missing file-failure warn record: %v", handler.Messages())
	}
}

// 不再按 date/model 聚合，每个顶层 message.id 一行 Message。
// 同日同 model 的两条 assistant usage 必须产出两条独立 Message，而非一个聚合 Session。
func TestWorkBuddyCollector_OneRowPerUsage(t *testing.T) {
	jsonl := `{"id":"wb-m1","timestamp":1750001000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"inputTokensDetails":[{"cached_tokens":300}]}}}
{"id":"wb-m2","timestamp":1750002000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/project-a","providerData":{"model":"glm-a","usage":{"inputTokens":2000,"outputTokens":200,"totalTokens":2200,"inputTokensDetails":[{"cached_tokens":500}]}}}
`
	env := newWBTestEnv(t, defaultWBMetas("wb-sess-001"),
		map[string]string{"Users-test-WorkBuddy-app/wb-sess-001": jsonl})

	c := NewWorkBuddyCollector(env.cfg())
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
	jsonl := `{"id":"wb-t1","timestamp":1750001000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/p","providerData":{"model":"glm-a","usage":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"inputTokensDetails":[{"cached_tokens":300}]}}}
{"id":"wb-t2","timestamp":1750002000000,"role":"assistant","sessionId":"content-s1","cwd":"/tmp/p","providerData":{"model":"glm-a","usage":{"inputTokens":2000,"outputTokens":200,"inputTokensDetails":[{"cached_tokens":500}]}}}
`
	env := newWBTestEnv(t, defaultWBMetas("wb-sess-001"), map[string]string{"dir/wb-sess-001": jsonl})

	c := NewWorkBuddyCollector(env.cfg())
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
	env := newWBTestEnv(t, defaultWBMetas("wb-sess-001"), map[string]string{"dir/wb-sess-001": jsonl})

	c := NewWorkBuddyCollector(env.cfg())
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
	dbPath := writeWorkBuddyMetaDB(t, root, defaultWBMetas("target", "other")...)

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir, "db": dbPath}},
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
	// ChangedFile 请求的计划只覆盖触达会话 target，不包含 other
	if len(result.WorkBuddyPlans) != 1 || result.WorkBuddyPlans[0].SessionID != "target" {
		t.Fatalf("ChangedFile 计划应只含 target, got %+v", result.WorkBuddyPlans)
	}
}

// model 缺失时回退 requestModelName；models.json vendor 映射保持。
func TestWorkBuddyCollector_ModelFallbackAndVendorMapping(t *testing.T) {
	jsonl := `{"id":"wb-fb","timestamp":1750001000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"requestModelName":"DeepSeek-V4 Pro","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}}}
{"id":"wb-map","timestamp":1750002000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"model":"deepseek-v4-pro","usage":{"inputTokens":200,"outputTokens":100,"totalTokens":300,"inputTokensDetails":[{"cached_tokens":0}]}}}
`
	env := newWBTestEnv(t, defaultWBMetas("sess-001"), map[string]string{"dir/sess-001": jsonl})
	os.WriteFile(filepath.Join(env.root, "models.json"), []byte(`[{"id":"deepseek-v4-pro","vendor":"DeepSeek"}]`), 0644)

	c := NewWorkBuddyCollector(env.cfg())
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
	// models.json 中无 requestModelName 映射时，仍按 WorkBuddy 官方来源归因。
	if got := byID["wb-fb"].Provider; got != "WorkBuddy" {
		t.Errorf("wb-fb Provider = %q, want WorkBuddy (models.json 无此 model 映射)", got)
	}
	// model 短 id 经 models.json 映射 vendor
	if got := byID["wb-map"].Model; got != "deepseek-v4-pro" {
		t.Errorf("wb-map Model = %q, want deepseek-v4-pro", got)
	}
	if got := byID["wb-map"].Provider; got != "DeepSeek" {
		t.Errorf("wb-map Provider = %q, want DeepSeek (models.json vendor 映射)", got)
	}
}

// 会话日志中的模型短 id 被客户端小写化（glm-5.3-flash），models.json 条目 id 为
// 混合大小写（GLM-5.3-Flash）时，vendor 映射经小写兜底键命中；Model 字段保留原始值。
func TestWorkBuddyCollector_ModelIdCaseInsensitiveVendorMapping(t *testing.T) {
	jsonl := `{"id":"wb-ci","timestamp":1750001000000,"role":"assistant","sessionId":"s","cwd":"/tmp/p","providerData":{"model":"glm-5.3-flash","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}}}
`
	env := newWBTestEnv(t, defaultWBMetas("sess-001"), map[string]string{"dir/sess-001": jsonl})
	os.WriteFile(filepath.Join(env.root, "models.json"), []byte(`[{"id":"GLM-5.3-Flash","vendor":"GLM Coding Plan"}]`), 0644)

	c := NewWorkBuddyCollector(env.cfg())
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	m := result.Messages[0]
	if got := m.Provider; got != "GLM Coding Plan" {
		t.Errorf("Provider = %q, want GLM Coding Plan (小写化模型 id 经小写兜底键命中)", got)
	}
	if got := m.Model; got != "glm-5.3-flash" {
		t.Errorf("Model = %q, want glm-5.3-flash (保留会话日志原始值)", got)
	}
}

// Session 元数据 title/directory/project/first-last ts 正确。
func TestWorkBuddyCollector_SessionMetadataFields(t *testing.T) {
	jsonl := usageLine(1000, "glm-a", 100, 50, 0) + "\n" +
		usageLine(3000, "glm-a", 200, 100, 0) + "\n" +
		usageLine(2000, "glm-a", 300, 150, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "sess-001", cwd: "/Users/test/WorkBuddy/app", title: wbPtr("我的会话"), playground: wbPtr(0)}},
		map[string]string{"dir/sess-001": jsonl})
	os.WriteFile(filepath.Join(env.root, "models.json"), []byte(`[{"id":"glm-a","vendor":"Zhipu"}]`), 0644)

	c := NewWorkBuddyCollector(env.cfg())
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

// ---------- 以下为 2026-10 WorkBuddy 专家维度需求的验收矩阵场景 ----------

// 助理识别：cwd 等于主目录下 WorkBuddy/Claw 且 source_mode/mode 双空 → 标题
// 固定「本地助理」；等价命中覆盖 ~ 展开与 Clean；大小写不同、相对路径、仅
// basename 相同、mode 非空、其他目录双空均不命中。
func TestWorkBuddy_AssistantRecognition(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	clawPath := filepath.Join(home, "WorkBuddy", "Claw")

	cases := []struct {
		name       string
		cwd        string
		sourceMode *string
		mode       *string
		wantHit    bool
	}{
		{"exact claw path", clawPath, nil, nil, true},
		{"tilde expand", "~/WorkBuddy/Claw", nil, nil, true},
		{"trailing slash cleaned", clawPath + string(filepath.Separator), nil, nil, true},
		{"case differs", filepath.Join(home, "workbuddy", "claw"), nil, nil, false},
		{"relative path", "WorkBuddy/Claw", nil, nil, false},
		{"other dir double-empty", filepath.Join(home, "WorkBuddy", "Other"), nil, nil, false},
		{"claw but mode set", clawPath, wbPtr("working"), nil, false},
		{"claw but source_mode set", clawPath, nil, wbPtr("craft"), false},
		{"deeper subdirectory", filepath.Join(home, "WorkBuddy", "Claw", "sub"), nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkBuddyAssistantPath(tc.cwd, home) && tc.sourceMode == nil && tc.mode == nil; got != tc.wantHit {
				t.Fatalf("isWorkBuddyAssistantPath(%q) 双空=%v, want hit=%v", tc.cwd, got, tc.wantHit)
			}
		})
	}

	// 端到端：源库 cwd 指向主目录 Claw + 双空 → 落库标题固定为「本地助理」，
	// 旧话题标题不透传；同目录 basename 但非完整路径命中的负向由 cases 覆盖。
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{
			id: "claw-sess", cwd: clawPath, title: wbPtr("卫生间下雨渗水原因排查"),
			playground: wbPtr(0), // 助理无 playground 标记，双空 mode 才是判据
		}},
		map[string]string{"clawdir/claw-sess": jsonl})
	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Title != workBuddyAssistantTitle {
		t.Errorf("Title = %q, want 固定标题 %q（旧话题不透传）", result.Sessions[0].Title, workBuddyAssistantTitle)
	}
	if result.Sessions[0].Project != "Claw" {
		t.Errorf("Project = %q, want Claw（非 playground 正常 basename）", result.Sessions[0].Project)
	}
}

// per-session is_playground 决定 project：1 → 空串；0 → basename。同目录混合
// 0/1 会话各自处理；消息 project 与 session project 同值。
func TestWorkBuddy_PlaygroundProjectRules(t *testing.T) {
	tsPlayground := wbTS(2025, 6, 8)
	tsNormal := wbTS(2025, 6, 8) + 60000
	jsonlPlayground := usageLine(tsPlayground, "m", 100, 50, 0) + "\n"
	jsonlNormal := usageLine(tsNormal, "m", 200, 100, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{
			{id: "pg-1", cwd: "/Users/test/WorkBuddy/2025-06-08-09-00-00", title: wbPtr("查询当前时间"), playground: wbPtr(1)},
			{id: "pg-0", cwd: "/Users/test/WorkBuddy/2025-06-04-15-45-35", title: wbPtr("时间戳空间"), playground: wbPtr(0)},
		},
		map[string]string{
			"Users-test-WorkBuddy-2025-06-08-09-00-00/pg-1": jsonlPlayground,
			"Users-test-WorkBuddy-2025-06-04-15-45-35/pg-0": jsonlNormal,
		})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	sessByID := map[string]model.Session{}
	for _, s := range result.Sessions {
		sessByID[s.ID] = s
	}
	if s := sessByID["pg-1"]; s.Project != "" {
		t.Errorf("playground=1 会话 Project = %q, want 空串", s.Project)
	}
	if s := sessByID["pg-0"]; s.Project != "2025-06-04-15-45-35" {
		t.Errorf("playground=0 会话 Project = %q, want 时间戳空间 basename", s.Project)
	}
	for _, m := range result.Messages {
		want := ""
		if m.SessionID == "pg-0" {
			want = "2025-06-04-15-45-35"
		}
		if m.Project != want {
			t.Errorf("消息 %s Project = %q, want %q（与会话同目标值）", m.ID, m.Project, want)
		}
	}
}

// expert 会话：非空 expert_id 编码为 WorkBuddy Expert:<hex> client；provider
// 回退仍是普通 WorkBuddy 名，不随 expert 改名。
func TestWorkBuddy_ExpertClientEncoding(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "unknown-model", 100, 50, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "exp-1", cwd: "/Users/test/WorkBuddy/pg", title: wbPtr("专家会话"), playground: wbPtr(1), expertID: wbPtr("MeituanLivingAssistant")}},
		map[string]string{"dir/exp-1": jsonl})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantClient := model.WorkBuddyExpertClientKey("MeituanLivingAssistant")
	if wantClient == "" {
		t.Fatal("编码函数应产出非空键")
	}
	if len(result.Sessions) != 1 || result.Sessions[0].Client != wantClient {
		t.Fatalf("session client = %+v, want %q", result.Sessions, wantClient)
	}
	if len(result.Messages) != 1 || result.Messages[0].Client != wantClient {
		t.Fatalf("message client 应与计划一致, got %+v", result.Messages)
	}
	// playground=1 → project 空串（expert 与 playground 两轴独立）
	if result.Sessions[0].Project != "" || result.Messages[0].Project != "" {
		t.Errorf("expert×playground 会话 project 应为空串")
	}
	// provider 回退保持普通 client 名
	if result.Messages[0].Provider != model.ClientWorkBuddy {
		t.Errorf("Provider = %q, want WorkBuddy（回退不随 expert 改名）", result.Messages[0].Provider)
	}
	if result.WorkBuddyPlans[0].Client != wantClient {
		t.Errorf("计划 client = %q, want %q", result.WorkBuddyPlans[0].Client, wantClient)
	}
}

// 单会话元数据未命中（JSONL 有文件但源库无有效行）→ 该会话整体暂缓：不产出
// 消息/会话/计划，PartialErr 报告且含固定片段；其他成功会话正常产出。
func TestWorkBuddy_MetaMissDefersSession(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	env := newWBTestEnv(t, defaultWBMetas("good-1"), map[string]string{
		"dir/good-1":   jsonl,
		"dir/deferred": jsonl, // 源库无 deferred 行
	})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 不应整体失败（单会话暂缓走 PartialErr）: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatal("未命中会话应经 PartialErr 报告")
	}
	if !strings.Contains(result.PartialErr.Error(), WorkBuddyMetadataErrPrefix) {
		t.Errorf("PartialErr 应含固定片段, got %v", result.PartialErr)
	}
	for _, m := range result.Messages {
		if m.SessionID == "deferred" {
			t.Errorf("暂缓会话不应产出消息: %+v", m)
		}
	}
	for _, s := range result.Sessions {
		if s.ID == "deferred" {
			t.Errorf("暂缓会话不应产出会话行: %+v", s)
		}
	}
	for _, p := range result.WorkBuddyPlans {
		if p.SessionID == "deferred" {
			t.Errorf("暂缓会话不应进入计划: %+v", p)
		}
	}
	if len(result.Messages) != 1 || result.Messages[0].SessionID != "good-1" {
		t.Errorf("成功会话应正常产出, got %+v", msgIDs(result.Messages))
	}
}

// is_playground 取值非法（NULL 或非 0/1）→ 会话暂缓，不猜 project。
func TestWorkBuddy_InvalidPlaygroundDefers(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	two := 2
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{
			{id: "null-pg", cwd: "/x", title: wbPtr("t"), playground: nil},
			{id: "two-pg", cwd: "/x", title: wbPtr("t"), playground: &two},
		},
		map[string]string{"dir/null-pg": jsonl, "dir/two-pg": jsonl})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatal("非法 is_playground 应经 PartialErr 报告")
	}
	if len(result.Messages) != 0 || len(result.WorkBuddyPlans) != 0 {
		t.Errorf("暂缓会话不应产出消息/计划, got %+v / %+v", result.Messages, result.WorkBuddyPlans)
	}
}

// 无日期全量请求计划覆盖全部有效元数据会话（含无 JSONL 的仅历史会话）；
// 日期请求只覆盖触达会话。
func TestWorkBuddy_PlanScopeFullVsDated(t *testing.T) {
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{
			{id: "touched", cwd: "/a", title: wbPtr("t1"), playground: wbPtr(0)},
			{id: "history-only", cwd: "/b", title: wbPtr("t2"), playground: wbPtr(0)},
		},
		map[string]string{"dir/touched": jsonl})

	full, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("全量: %v", err)
	}
	if len(full.WorkBuddyPlans) != 2 {
		t.Fatalf("全量计划应含 touched 与 history-only, got %+v", full.WorkBuddyPlans)
	}
	// history-only 无 JSONL：FirstTS/LastTS 为 0、directory 取源库 cwd
	for _, p := range full.WorkBuddyPlans {
		if p.SessionID == "history-only" {
			if p.FirstTS != 0 || p.LastTS != 0 {
				t.Errorf("history-only 计划时间区间应为 0, got %+v", p)
			}
			if p.Directory != "/b" {
				t.Errorf("history-only Directory = %q, want /b（源库 cwd）", p.Directory)
			}
		}
	}

	dated, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{Dates: []string{"2025-06-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("日期: %v", err)
	}
	if len(dated.WorkBuddyPlans) != 1 || dated.WorkBuddyPlans[0].SessionID != "touched" {
		t.Fatalf("日期请求计划只应含触达会话, got %+v", dated.WorkBuddyPlans)
	}
}

// 源库 cwd 为空时，有效会话目录取 JSONL 首条非空 cwd；仍为空则 project 允许
// 为空（playground=0 时 project 为空串，不猜 basename）。
func TestWorkBuddy_EmptyCWDFallback(t *testing.T) {
	emptyCwdLine := func(id string, ts int64, cwd string) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":10,"outputTokens":5,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":%q}`,
			id, ts, cwd)
	}
	jsonl := emptyCwdLine("e1", wbTS(2025, 6, 8), "") + "\n" +
		emptyCwdLine("e2", wbTS(2025, 6, 8)+1000, "/fallback/dir") + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "nocwd", cwd: "", title: wbPtr("t"), playground: wbPtr(0)}},
		map[string]string{"dir/nocwd": jsonl})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	s := result.Sessions[0]
	if s.Directory != "/fallback/dir" {
		t.Errorf("Directory = %q, want /fallback/dir（JSONL 首条非空 cwd 兜底）", s.Directory)
	}
	if s.Project != "dir" {
		t.Errorf("Project = %q, want dir", s.Project)
	}
}

// 必要列缺失（旧 schema 无 expert_id/is_playground）→ 整轮元数据失败。
func TestWorkBuddy_MissingColumnsFailsWholeRound(t *testing.T) {
	root := t.TempDir()
	oldDB := filepath.Join(root, "old.db")
	dbh, err := sql.Open("sqlite", oldDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, title TEXT, deleted_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	dbh.Close()

	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	_, projectsDir := buildWorkBuddyDir(t, "dir", "sess-001", jsonl)
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir, "db": oldDB}},
	}}
	result, err := NewWorkBuddyCollector(cfg).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err == nil {
		t.Fatalf("缺列应整轮失败, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), WorkBuddyMetadataErrPrefix) {
		t.Errorf("错误应含固定片段, got %v", err)
	}
}

// ---------- GPT 评审复现场景（修复回归锚点） ----------

// 文件级解析失败的会话整会话暂缓：无日期全量计划不得把该会话当作「仅历史
// 会话」重新纳入（否则 engine 仍会刷新其历史归属，违反整会话暂缓）。
func TestWorkBuddy_FailedFileExcludedFromFullPlans(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("windows/root 下 chmod 000 不产生打开失败，无法触发文件级失败路径")
	}
	jsonl := usageLine(wbTS(2025, 6, 8), "m", 100, 50, 0) + "\n"
	env := newWBTestEnv(t, defaultWBMetas("s1", "s2"), map[string]string{
		"dir/s1": jsonl,
		"dir/s2": jsonl,
	})
	if err := os.Chmod(filepath.Join(env.projectsDir, "dir", "s1.jsonl"), 0); err != nil {
		t.Fatal(err)
	}

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatal("文件读取失败应报告 PartialErr")
	}
	for _, p := range result.WorkBuddyPlans {
		if p.SessionID == "s1" {
			t.Errorf("失败会话 s1 不得进入全量计划: %+v", p)
		}
	}
	if len(result.WorkBuddyPlans) != 1 || result.WorkBuddyPlans[0].SessionID != "s2" {
		t.Errorf("全量计划应只含成功会话 s2: %+v", result.WorkBuddyPlans)
	}
}

// 文件级首条非空 cwd 收集自全部原生 JSON 行（含 user 消息、无 usage 的
// assistant），不受「带 usage 的 assistant」结构性过滤影响。
func TestWorkBuddy_FirstCwdIncludesNonUsageLines(t *testing.T) {
	content := `{"id":"u1","timestamp":1749312000000,"role":"user","content":[],"sessionId":"s","cwd":"/first/repo"}
{"id":"a1","timestamp":1749312060000,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50}},"sessionId":"s","cwd":"/Users/test/WorkBuddy/app"}
`
	path := writeJSONL(t, content)
	messages, fileCwd, _, err := parseWorkBuddyJSONL(path, slog.Default())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("usage 消息数 = %d, want 1", len(messages))
	}
	if fileCwd != "/first/repo" {
		t.Errorf("fileCwd = %q, want /first/repo（user 行的 cwd 按物理顺序在前）", fileCwd)
	}

	// 端到端：源库 cwd 为空时，会话目录/project 兜底取文件级首条 cwd。
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "sess-001", cwd: "", title: wbPtr("t"), playground: wbPtr(0)}},
		map[string]string{"dir/sess-001": content})
	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d", len(result.Sessions))
	}
	if s := result.Sessions[0]; s.Directory != "/first/repo" || s.Project != "repo" {
		t.Errorf("Directory/Project = %q/%q, want /first/repo/repo（文件级首条 cwd 兜底）", s.Directory, s.Project)
	}
	// 消息自身 directory 保留各自原值（assistant 行的 cwd）。
	if m := result.Messages[0]; m.Directory != "/Users/test/WorkBuddy/app" {
		t.Errorf("消息 directory = %q, want 保留原值 /Users/test/WorkBuddy/app", m.Directory)
	}
}

// 同一 session ID 出现在两个项目子目录、其中一份文件终止性读取失败：该会话
// 整会话暂缓——成功文件的消息/会话/计划也不得产出（否则失败会话的历史归属
// 与标题仍会被 engine 刷新，新消息仍会入库）。
func TestWorkBuddy_DuplicateSessionFiles_FailureDefersWholeSession(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("windows/root 下 chmod 000 不产生打开失败，无法触发文件级失败路径")
	}
	expert := model.WorkBuddyExpertClientKey("A")
	ts := wbTS(2025, 6, 8)
	jsonl := usageLine(ts, "m", 100, 50, 0) + "\n"
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{{id: "s1", cwd: "/repo", title: wbPtr("new title"), playground: wbPtr(0), expertID: wbPtr("A")}},
		map[string]string{
			"bad/s1": jsonl,
			"dir/s1": jsonl,
		})
	if err := os.Chmod(filepath.Join(env.projectsDir, "bad", "s1.jsonl"), 0); err != nil {
		t.Fatal(err)
	}

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatal("失败文件应报告 PartialErr")
	}
	// 整会话暂缓：成功文件的消息、会话与计划全部不产出。
	if len(result.Messages) != 0 {
		t.Errorf("暂缓会话不应产出消息, got %d", len(result.Messages))
	}
	if len(result.Sessions) != 0 {
		t.Errorf("暂缓会话不应产出会话行, got %+v", result.Sessions)
	}
	for _, p := range result.WorkBuddyPlans {
		if p.SessionID == "s1" {
			t.Errorf("暂缓会话不得进入计划: %+v", p)
		}
	}
	_ = expert
}

// 同一 session 的多份成功文件：计划与会话行的时间区间必须跨文件汇总
// （最早/最晚），不能只剩最后一个文件的区间。
func TestWorkBuddy_DuplicateSessionFilesAllSuccess_AggregatedRange(t *testing.T) {
	line := func(id string, ts int64) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":"/repo"}`, id, ts)
	}
	env := newWBTestEnv(t, defaultWBMetas("sess-001"), map[string]string{
		"a/sess-001": line("m0", 1000) + "\n",
		"b/sess-001": line("m1", 2000) + "\n",
	})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("messages = %d, want 2（两份文件全部产出）", len(result.Messages))
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	if s := result.Sessions[0]; s.FirstTS != 1000 || s.LastTS != 2000 {
		t.Errorf("session 区间 = %d..%d, want 1000..2000（跨文件汇总）", s.FirstTS, s.LastTS)
	}
	if p := result.WorkBuddyPlans[0]; p.FirstTS != 1000 || p.LastTS != 2000 {
		t.Errorf("计划区间 = %d..%d, want 1000..2000", p.FirstTS, p.LastTS)
	}
}

// 同一 session 的多份成功文件产出相互矛盾的目标（源库 cwd 为空时 directory
// 依赖各文件 fileCwd）：整会话暂缓（消息/会话/计划全不产出 + PartialErr），
// 其他有效会话不受影响。
func TestWorkBuddy_DuplicateSessionFiles_ConflictingTargetsDefers(t *testing.T) {
	line := func(id string, ts int64, cwd string) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":%q}`, id, ts, cwd)
	}
	env := newWBTestEnv(t,
		[]workbuddyMetaRow{
			{id: "sess-001", cwd: "", title: wbPtr("t"), playground: wbPtr(0)},
			{id: "healthy", cwd: "/repo", title: wbPtr("h"), playground: wbPtr(0)},
		},
		map[string]string{
			"a/sess-001": line("m0", 1000, "/a") + "\n",
			"b/sess-001": line("m1", 2000, "/b") + "\n",
			"c/healthy":  line("h0", 1500, "/repo") + "\n",
		})

	result, err := NewWorkBuddyCollector(env.cfg()).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr == nil || !strings.Contains(result.PartialErr.Error(), "相互矛盾的目标") {
		t.Fatalf("矛盾目标应经 PartialErr 报告: %v", result.PartialErr)
	}
	for _, m := range result.Messages {
		if m.SessionID == "sess-001" {
			t.Errorf("矛盾会话不应产出消息: %+v", m)
		}
	}
	for _, s := range result.Sessions {
		if s.ID == "sess-001" {
			t.Errorf("矛盾会话不应产出会话行: %+v", s)
		}
	}
	for _, p := range result.WorkBuddyPlans {
		if p.SessionID == "sess-001" {
			t.Errorf("矛盾会话不应进入计划: %+v", p)
		}
	}
	if len(result.Messages) != 1 || result.Messages[0].SessionID != "healthy" {
		t.Errorf("healthy 会话应正常产出: %+v", msgIDs(result.Messages))
	}
}
