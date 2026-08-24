package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// copyFixtureToTempDir 把 testdata/claude 下的单个 fixture 复制到一个隔离的临时 projectsDir，
// 避免同目录其他 fixture 干扰按文件粒度的断言。返回 (projectsDir, filePath)。
func copyFixtureToTempDir(t *testing.T, name string) (string, string) {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "claude", name)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		t.Skipf("fixture %s 不存在，跳过测试", name)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, dst
}

// newClaudeCollectorCfg 构造指向 projectsDir 的最小 claude 配置 collector。
func newClaudeCollectorCfg(t *testing.T, projectsDir string) *ClaudeCollector {
	t.Helper()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	return NewClaudeCollector(cfg)
}

// writeClaudeBlindSpotFixture 写入超过 50 行的 JSONL，把唯一一条带 usage 的 assistant
// 消息放在第 26 行（索引 25），落在旧 head(20)+tail(30) 实现的盲区内。
func writeClaudeBlindSpotFixture(t *testing.T, path string) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 60; i++ {
		if i == 25 {
			b.WriteString(`{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"msg-middle","role":"assistant","model":"model-a","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}` + "\n")
			continue
		}
		b.WriteString(fmt.Sprintf(`{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:%02d:00+08:00","cwd":"/tmp/project","message":{"id":"u-%d","role":"user"}}`, i%60, i) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 行解析失败按文件聚合为一条汇总（count + 首行号 + 首个错误），不再逐行打印。
func TestClaudeParseLineFailuresPerFileSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj.jsonl")
	// 第 1 行合法、第 2/4 行坏、第 3 行合法。
	content := `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"u-1","role":"user"}}
not-json-line-2
{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:01:00+08:00","cwd":"/tmp/project","message":{"id":"u-2","role":"user"}}
{broken line 4
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	handler := &testLogHandler{}
	if _, err := parseClaudeMessageFile(path, nil, slog.New(handler)); err != nil {
		t.Fatalf("parseClaudeMessageFile 失败: %v", err)
	}

	var summaries []slog.Record
	for _, r := range handler.Records() {
		if strings.Contains(r.Message, "行解析失败") {
			summaries = append(summaries, r)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("期望恰好 1 条行解析失败汇总，实际 %d 条: %v", len(summaries), handler.Messages())
	}
	attrs := map[string]string{}
	summaries[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	if attrs["count"] != "2" {
		t.Errorf("count = %q, want 2", attrs["count"])
	}
	if attrs["first_line"] != "2" {
		t.Errorf("first_line = %q, want 2", attrs["first_line"])
	}
	if attrs["error"] == "" {
		t.Error("汇总缺少 error 定位线索")
	}
}

// 全部行合法时不输出行解析失败汇总。
func TestClaudeParseNoFailuresNoSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.jsonl")
	content := `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"u-1","role":"user"}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	handler := &testLogHandler{}
	if _, err := parseClaudeMessageFile(path, nil, slog.New(handler)); err != nil {
		t.Fatalf("parseClaudeMessageFile 失败: %v", err)
	}
	if handler.HasMessage("行解析失败") {
		t.Errorf("全部合法时不应有行解析失败汇总: %v", handler.Messages())
	}
}

// 超过 50 行时位于盲区（第 26 行）的 assistant usage 必须被采集。
func TestClaudeCollector_FullScanIncludesMiddleMessage(t *testing.T) {
	projectsDir := t.TempDir()
	jsonlPath := filepath.Join(projectsDir, "blind.jsonl")
	writeClaudeBlindSpotFixture(t, jsonlPath)

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-middle" {
		t.Fatalf("messages = %+v", result.Messages)
	}
	msg := result.Messages[0]
	if msg.FreshInputTokens != 100 || msg.TotalTokens != 160 {
		t.Fatalf("token formula mismatch: %+v", msg)
	}
}

// 同一文件跨两日，消息按各自 date 归类（不再按文件 LastTS 整体归属）。
func TestClaudeCollector_CrossDayPerMessageFilter(t *testing.T) {
	dir, _ := copyFixtureToTempDir(t, "message-level.jsonl")
	c := newClaudeCollectorCfg(t, dir)

	// 查 2026-06-22 应只命中 msg-day1。
	r1, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-06-22"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Messages) != 1 || r1.Messages[0].ID != "msg-day1" || r1.Messages[0].Date != "2026-06-22" {
		t.Fatalf("2026-06-22 messages = %+v", r1.Messages)
	}
	// 查 2026-07-08 应只命中 msg-day2。
	r2, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Messages) != 1 || r2.Messages[0].ID != "msg-day2" || r2.Messages[0].Date != "2026-07-08" {
		t.Fatalf("2026-07-08 messages = %+v", r2.Messages)
	}
}

// 每条 Message 保留自己的 model，不塌缩到文件级单一 model。
func TestClaudeCollector_MultipleModelsRemainSeparate(t *testing.T) {
	dir, _ := copyFixtureToTempDir(t, "message-level.jsonl")
	c := newClaudeCollectorCfg(t, dir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-06-22", "2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(result.Messages), result.Messages)
	}
	models := map[string]bool{}
	for _, m := range result.Messages {
		models[m.Model] = true
	}
	if !models["model-a"] || !models["model-b"] {
		t.Fatalf("expected model-a and model-b, got %v", models)
	}
}

// 同一 message.id 的多个片段（thinking/text/tool）只产出一条 Message。
func TestClaudeCollector_DeduplicatesRepeatedMessageFragments(t *testing.T) {
	dir, _ := copyFixtureToTempDir(t, "message-level.jsonl")
	c := newClaudeCollectorCfg(t, dir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-06-22"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	// message-level.jsonl 中 2026-06-22 有两行 message.id=msg-day1（text + thinking），
	// 应只产出 1 条 Message 且 usage 取首条非零记录。
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 deduped message for msg-day1, got %d: %+v", len(result.Messages), result.Messages)
	}
	if result.Messages[0].ID != "msg-day1" {
		t.Fatalf("message id = %q, want msg-day1", result.Messages[0].ID)
	}
	if result.Messages[0].InputTokens != 100 {
		t.Fatalf("input tokens = %d, want 100 (first fragment usage)", result.Messages[0].InputTokens)
	}
}

// ChangedFile 模式只读取指定文件。
func TestClaudeCollector_ChangedFileOnly(t *testing.T) {
	projectsDir := t.TempDir()
	writeClaudeBlindSpotFixture(t, filepath.Join(projectsDir, "blind.jsonl"))
	// 制造一个干扰文件，不应被采集。
	os.WriteFile(filepath.Join(projectsDir, "other.jsonl"),
		[]byte(`{"type":"assistant","sessionId":"s2","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"other-msg","role":"assistant","model":"model-a","usage":{"input_tokens":999,"output_tokens":1}}}`+"\n"), 0o600)

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{
		Dates:       []string{"2026-07-08"},
		ChangedFile: filepath.Join(projectsDir, "blind.jsonl"),
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-middle" {
		t.Fatalf("expected only msg-middle from ChangedFile, got %+v", result.Messages)
	}
}

// entrypoint=claude-desktop-3p 映射 Claude Desktop，其余映射 Claude Code。
func TestClaudeCollector_ClientMapping(t *testing.T) {
	projectsDir := t.TempDir()
	content := `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"cli","cwd":"/tmp/p","message":{"id":"u1","role":"user"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:01:00+08:00","entrypoint":"cli","cwd":"/tmp/p","message":{"id":"code-msg","role":"assistant","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}
`
	os.WriteFile(filepath.Join(projectsDir, "code.jsonl"), []byte(content), 0o600)
	desktop := `{"type":"assistant","sessionId":"s2","timestamp":"2026-07-08T10:01:00+08:00","entrypoint":"claude-desktop-3p","cwd":"/tmp/p","message":{"id":"desktop-msg","role":"assistant","model":"m","usage":{"input_tokens":2,"output_tokens":1}}}
`
	os.WriteFile(filepath.Join(projectsDir, "desktop.jsonl"), []byte(desktop), 0o600)

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]string{}
	for _, m := range result.Messages {
		clients[m.ID] = m.Client
	}
	if clients["code-msg"] != model.ClientClaudeCode {
		t.Errorf("code-msg client = %q, want %q", clients["code-msg"], model.ClientClaudeCode)
	}
	if clients["desktop-msg"] != model.ClientClaudeDesktop {
		t.Errorf("desktop-msg client = %q, want %q", clients["desktop-msg"], model.ClientClaudeDesktop)
	}
}

// branch/rewind 场景——collector 不删除任何 ID，DB 去重与最早归因由 DB 层处理。
func TestClaudeCollector_BranchRewindKeepsAllIDs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"branch-parent.jsonl", "branch-child.jsonl"} {
		src := filepath.Join("..", "..", "testdata", "claude", name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			t.Skipf("fixture %s 不存在，跳过测试", name)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c := newClaudeCollectorCfg(t, dir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	// parent + child 合计应有 3 个不同 ID：branch-shared(每文件去重，共2条同 ID)、branch-parent-only、branch-child-only。
	ids := map[string]bool{}
	for _, m := range result.Messages {
		ids[m.ID] = true
	}
	if !ids["branch-shared"] || !ids["branch-parent-only"] || !ids["branch-child-only"] {
		t.Fatalf("missing IDs, got %v", ids)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("expected 4 messages (shared appears once per file), got %d: %+v", len(result.Messages), result.Messages)
	}
}

// token 公式 fresh=input; total=input+cache_read+cache_create+output。
func TestClaudeCollector_TokenFormula(t *testing.T) {
	projectsDir := t.TempDir()
	content := `{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"cli","cwd":"/tmp/p","message":{"id":"formula","role":"assistant","model":"m","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}
`
	os.WriteFile(filepath.Join(projectsDir, "formula.jsonl"), []byte(content), 0o600)

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	msg := result.Messages[0]
	if msg.FreshInputTokens != 100 {
		t.Errorf("FreshInputTokens = %d, want 100", msg.FreshInputTokens)
	}
	wantTotal := int64(100 + 30 + 10 + 20)
	if msg.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", msg.TotalTokens, wantTotal)
	}
	if msg.CacheReadTokens != 30 || msg.CacheCreateTokens != 10 || msg.OutputTokens != 20 {
		t.Errorf("token breakdown mismatch: %+v", msg)
	}
}

// 文件名作为 session ID、最后非空 title、完整文件 first/last ts。
func TestClaudeCollector_SessionMetadata(t *testing.T) {
	dir, _ := copyFixtureToTempDir(t, "message-level.jsonl")
	c := newClaudeCollectorCfg(t, dir)
	result, err := c.Collect(t.Context(), CollectRequest{Dates: []string{"2026-06-22"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(result.Sessions))
	}
	s := result.Sessions[0]
	if s.ID != "message-level" {
		t.Errorf("session ID = %q, want message-level", s.ID)
	}
	if s.Title != "cross-day-title" {
		t.Errorf("session title = %q, want cross-day-title", s.Title)
	}
	// firstTS 来自文件首行 2026-06-22T10:00:00+08:00, lastTS 来自末行 2026-07-08T11:01:00+08:00。
	wantFirst := int64(1782093600000) // 2026-06-22T02:00:00Z
	wantLast := int64(1783479660000)  // 2026-07-08T03:01:00Z
	if s.FirstTS != wantFirst {
		t.Errorf("FirstTS = %d, want %d", s.FirstTS, wantFirst)
	}
	if s.LastTS != wantLast {
		t.Errorf("LastTS = %d, want %d", s.LastTS, wantLast)
	}
}

func TestFindClaudeJSONLFiles(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "session-1.jsonl"), []byte(""), 0644)
	os.WriteFile(filepath.Join(tmpDir, "session-2.jsonl"), []byte(""), 0644)
	os.WriteFile(filepath.Join(tmpDir, "readme.txt"), []byte(""), 0644)

	subDir := filepath.Join(tmpDir, "subdir")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "session-3.jsonl"), []byte(""), 0644)

	files, err := findClaudeJSONLFiles(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("findClaudeJSONLFiles failed: %v", err)
	}

	if len(files) != 3 {
		t.Errorf("expected 3 jsonl files, got %d: %v", len(files), files)
	}
}

func TestInferProject(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{"normal path", "/Users/test/IdeaProjects/my-project", "my-project"},
		{"nested path", "/Users/test/IdeaProjects/group/sub-project", "sub-project"},
		{"empty cwd", "", ""},
		{"trailing slash", "/Users/test/IdeaProjects/my-project/", "my-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := inferProject(tt.cwd)
			if result != tt.expected {
				t.Errorf("inferProject(%q) = %q, want %q", tt.cwd, result, tt.expected)
			}
		})
	}
}

// TestFindClaudeJSONLFiles_RespectsCancelledContext 守护 ctx 取消可中断遍历：
// 守护进程 SIGTERM 取消 ctx 时，findClaudeJSONLFiles 必须尽快中止并返回错误，
// 而非遍历完整目录树（否则关闭路径会被长采集阻塞，参见 debounce.Stop 超时）。
func TestFindClaudeJSONLFiles_RespectsCancelledContext(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 200; i++ {
		if err := os.MkdirAll(filepath.Join(tmpDir, fmt.Sprintf("d%d", i)), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	_, err := findClaudeJSONLFiles(ctx, tmpDir)
	if err == nil {
		t.Fatal("expected error when context cancelled (Walk should abort early), got nil")
	}
}

func TestClaude_BadLineLogsDebug(t *testing.T) {
	tmpDir := t.TempDir()
	projectsDir := filepath.Join(tmpDir, "projects", "-Users-test-project")
	os.MkdirAll(projectsDir, 0755)

	content := `{"type":"summary","sessionId":"s1","timestamp":"2026-06-22T10:00:00Z","summary":{"costUSD":0.1,"duration":{"api":1000},"numTurns":1,"usage":{"input_tokens":100,"output_tokens":50}}}
invalid json line
`
	jsonlPath := filepath.Join(projectsDir, "test.jsonl")
	os.WriteFile(jsonlPath, []byte(content), 0644)

	c := newClaudeCollectorCfg(t, projectsDir)
	handler := &testLogHandler{}
	logger := slog.New(handler)

	c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-06-22"}}, logger)

	if !handler.HasRecord(slog.LevelDebug, "Claude JSONL 行解析失败，跳过") {
		t.Errorf("expected exact debug record, got messages: %v", handler.Messages())
	}
}

func TestClaude_FileParseFailureLogsWarn(t *testing.T) {
	projectsDir := t.TempDir()
	valid := `{"type":"assistant","sessionId":"s1","timestamp":"2026-06-22T10:00:00Z",` +
		`"entrypoint":"cli","cwd":"/tmp/project","message":{"id":"m1","role":"assistant",` +
		`"model":"claude-sonnet","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectsDir, "good.jsonl"), []byte(valid), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "bad.jsonl"),
		[]byte(strings.Repeat("x", maxJSONLLineSize+1)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := newClaudeCollectorCfg(t, projectsDir)
	handler := &testLogHandler{}
	result, err := c.Collect(context.Background(), CollectRequest{Dates: []string{"2026-06-22"}}, slog.New(handler))
	if err != nil || len(result.Messages) != 1 {
		t.Fatalf("messages=%+v err=%v", result.Messages, err)
	}
	if !handler.HasRecord(slog.LevelWarn, "Claude JSONL 文件解析失败，跳过") {
		t.Fatalf("missing file-failure warn record: %v", handler.Messages())
	}
}
