package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

func TestFindStateDBs_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()

	files := []string{"state_5.sqlite", "state_6.sqlite", "state_7.sqlite"}
	for _, f := range files {
		os.WriteFile(filepath.Join(tmpDir, f), []byte("fake"), 0644)
	}
	os.WriteFile(filepath.Join(tmpDir, "logs_2.sqlite"), []byte("fake"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "goals_1.sqlite"), []byte("fake"), 0644)

	dbs, err := findStateDBs(tmpDir)
	if err != nil {
		t.Fatalf("findStateDBs failed: %v", err)
	}

	if len(dbs) != 3 {
		t.Errorf("expected 3 state DBs, got %d: %v", len(dbs), dbs)
	}
}

func TestFindStateDBs_NoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	dbs, err := findStateDBs(tmpDir)
	if err != nil {
		t.Fatalf("findStateDBs failed: %v", err)
	}

	if len(dbs) != 0 {
		t.Errorf("expected 0 state DBs, got %d", len(dbs))
	}
}

func TestFindStateDBs_NonexistentDir(t *testing.T) {
	_, err := findStateDBs("/nonexistent/path")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestFindStateDBs_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "state_5.sqlite"), []byte("fake"), 0644)

	dbs, err := findStateDBs(tmpDir)
	if err != nil {
		t.Fatalf("findStateDBs failed: %v", err)
	}
	if len(dbs) != 1 {
		t.Errorf("expected 1 state DB, got %d", len(dbs))
	}
}

func TestInferClient(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		originator     string
		threadSource   string
		expectedClient string
	}{
		{
			name:           "CLI by source",
			source:         "cli",
			originator:     "",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "App by source",
			source:         "vscode",
			originator:     "",
			threadSource:   "",
			expectedClient: model.RawClientCodexApp,
		},
		{
			name:           "CLI by originator",
			source:         "",
			originator:     "codex cli",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "App by originator desktop",
			source:         "",
			originator:     "codex desktop",
			threadSource:   "",
			expectedClient: model.RawClientCodexApp,
		},
		{
			name:           "Originator overrides source",
			source:         "vscode",
			originator:     "codex cli",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "Subagent with desktop originator",
			source:         "vscode",
			originator:     "codex desktop",
			threadSource:   "subagent",
			expectedClient: model.RawClientCodexApp,
		},
		{
			name:           "Subagent without originator uses source",
			source:         "vscode",
			originator:     "",
			threadSource:   "subagent",
			expectedClient: model.RawClientCodexApp,
		},
		{
			name:           "CLI subagent",
			source:         "cli",
			originator:     "",
			threadSource:   "subagent",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "codex-tui originator",
			source:         "",
			originator:     "codex-tui",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "Unknown defaults to CLI",
			source:         "unknown",
			originator:     "",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
		{
			name:           "Empty defaults to CLI",
			source:         "",
			originator:     "",
			threadSource:   "",
			expectedClient: model.RawClientCodexCLI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := inferClient(tt.source, tt.originator, tt.threadSource)
			if result != tt.expectedClient {
				t.Errorf("inferClient(%q, %q, %q) = %q, want %q",
					tt.source, tt.originator, tt.threadSource, result, tt.expectedClient)
			}
		})
	}
}

func TestParseRolloutJSONL_SessionMeta(t *testing.T) {
	path := "../../testdata/codex/rollout-001.jsonl"
	entries, err := parseRolloutJSONL(path)
	if err != nil {
		t.Fatalf("parseRolloutJSONL failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Type != "session_meta" {
		t.Errorf("entry[0].Type = %q, want 'session_meta'", entries[0].Type)
	}

	meta, err := extractSessionMeta(entries[0])
	if err != nil {
		t.Fatalf("extractSessionMeta failed: %v", err)
	}
	if meta.Originator != "codex-tui" {
		t.Errorf("Originator = %q, want 'codex-tui'", meta.Originator)
	}
	if meta.sourceString() != "cli" {
		t.Errorf("sourceString() = %q, want 'cli'", meta.sourceString())
	}
	if meta.Cwd != "/Users/test/project" {
		t.Errorf("Cwd = %q, want '/Users/test/project'", meta.Cwd)
	}
}

func TestParseRolloutJSONL_NonexistentFile(t *testing.T) {
	_, err := parseRolloutJSONL("/nonexistent/rollout.jsonl")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestParseRolloutJSONL_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.jsonl")
	os.WriteFile(emptyFile, []byte(""), 0644)

	entries, err := parseRolloutJSONL(emptyFile)
	if err != nil {
		t.Fatalf("parseRolloutJSONL failed: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty file, got %d", len(entries))
	}
}

func TestParseRolloutJSONL_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	badFile := filepath.Join(tmpDir, "bad.jsonl")
	os.WriteFile(badFile, []byte("not json\n{\"type\":\"session_meta\",\"payload\":{\"originator\":\"codex-tui\"}}\n"), 0644)

	entries, err := parseRolloutJSONL(badFile)
	if err != nil {
		t.Fatalf("parseRolloutJSONL failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 valid entry, got %d", len(entries))
	}
}

func TestParseRolloutJSONL_OnlySessionMeta(t *testing.T) {
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "meta_only.jsonl")
	os.WriteFile(file, []byte(`{"timestamp":"2026-01-20T10:00:00Z","type":"session_meta","payload":{"originator":"codex-tui","source":"cli"}}`), 0644)

	entries, err := parseRolloutJSONL(file)
	if err != nil {
		t.Fatalf("parseRolloutJSONL failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Type != "session_meta" {
		t.Errorf("type = %q, want 'session_meta'", entries[0].Type)
	}
}

// subagent 会话的 session_meta payload.source 是对象（如 {"subagent":{...}}），
// 完整解析必须成功且 sourceString 归一为空串。
func TestExtractSessionMeta_SubagentObjectSource(t *testing.T) {
	entry := rolloutEntry{
		Type:    "session_meta",
		Payload: json.RawMessage(`{"id":"019f22b4","originator":"Codex Desktop","source":{"subagent":{"thread_spawn":{"parent_thread_id":"019f22b0","depth":1,"agent_role":"explorer"}}},"thread_source":"subagent","cwd":"/tmp/project"}`),
	}
	meta, err := extractSessionMeta(entry)
	if err != nil {
		t.Fatalf("extractSessionMeta failed: %v", err)
	}
	if meta.ID != "019f22b4" {
		t.Errorf("ID = %q, want '019f22b4'", meta.ID)
	}
	if meta.sourceString() != "" {
		t.Errorf("sourceString() = %q, want empty for object source", meta.sourceString())
	}
	if meta.Originator != "Codex Desktop" {
		t.Errorf("Originator = %q, want 'Codex Desktop'", meta.Originator)
	}
}

// 字段类型漂移（originator 变对象）时降级为仅提取 ID，不再整条丢弃。
func TestExtractSessionMeta_FieldDriftDegradedToID(t *testing.T) {
	entry := rolloutEntry{
		Type:    "session_meta",
		Payload: json.RawMessage(`{"id":"thread-drift","originator":{"nested":true},"cwd":"/tmp"}`),
	}
	meta, err := extractSessionMeta(entry)
	if err != nil {
		t.Fatalf("expected degraded extraction to succeed, got error: %v", err)
	}
	if meta.ID != "thread-drift" {
		t.Errorf("ID = %q, want 'thread-drift'", meta.ID)
	}
	if meta.Cwd != "" || meta.Originator != "" {
		t.Errorf("degraded meta should only carry ID, got cwd=%q originator=%q", meta.Cwd, meta.Originator)
	}
}

// 连 ID 都提取不到时仍返回错误。
func TestExtractSessionMeta_DriftWithoutIDFails(t *testing.T) {
	entry := rolloutEntry{
		Type:    "session_meta",
		Payload: json.RawMessage(`{"originator":{"nested":true}}`),
	}
	if _, err := extractSessionMeta(entry); err == nil {
		t.Error("expected error when neither full parse nor ID extraction succeeds")
	}
}

// sourceString 的三形态：字符串、对象、缺失。
func TestSourceString(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantLen int
	}{
		{name: "字符串形态", raw: `"cli"`, want: "cli"},
		{name: "对象形态", raw: `{"subagent":{"other":"guardian"}}`, want: ""},
		{name: "缺失", raw: ``, want: ""},
	}
	for _, tt := range tests {
		p := sessionMetaPayload{Source: json.RawMessage(tt.raw)}
		if got := p.sourceString(); got != tt.want {
			t.Errorf("%s: sourceString() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// 回归：subagent 对象 source 的 rollout 在 fallback 为空（扫描路径）下
// 不再报「缺少 session ID」，会话可建立且 token 正常采集。
func TestParseCodexRollout_SubagentObjectSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagent-rollout.jsonl")
	content := `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","originator":"Codex Desktop","source":{"subagent":{"thread_spawn":{"parent_thread_id":"019f4fda","depth":1,"agent_nickname":"Noether","agent_role":"worker"}}},"thread_source":"subagent","cwd":"/tmp/project"}}
{"timestamp":"2026-07-11T14:27:00Z","type":"turn_context","payload":{"model":"gpt-5"}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-sub-1"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout failed: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d, want 1", len(result.Sessions))
	}
	sess := result.Sessions[0]
	if sess.ID != "019f4fdb" {
		t.Errorf("Session ID = %q, want '019f4fdb' (from session_meta payload.id)", sess.ID)
	}
	if sess.Client != model.ClientCodexApp {
		t.Errorf("Session Client = %q, want %q (originator 'Codex Desktop')", sess.Client, model.ClientCodexApp)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("Messages = %d, want 1 (token_count event)", len(result.Messages))
	}
	if result.Messages[0].SessionID != "019f4fdb" {
		t.Errorf("Message SessionID = %q, want '019f4fdb'", result.Messages[0].SessionID)
	}
	if result.Messages[0].TotalTokens != 180 {
		t.Errorf("TotalTokens = %d, want 180", result.Messages[0].TotalTokens)
	}
}

// 字段漂移降级的端到端：仅 ID 可提取时，fallback 空的扫描路径仍能建立会话。
func TestParseCodexRollout_FieldDriftStillBuildsSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift-rollout.jsonl")
	content := `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"thread-drift","cwd":{"unexpected":"object"}}}
{"timestamp":"2026-07-11T14:27:00Z","type":"turn_context","payload":{"model":"gpt-5"}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-drift"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"total_tokens":15}}}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout failed: %v", err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].ID != "thread-drift" {
		t.Fatalf("Sessions = %+v, want one session with ID 'thread-drift'", result.Sessions)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("Messages = %d, want 1", len(result.Messages))
	}
}

func TestThreadEffectiveTS(t *testing.T) {
	tests := []struct {
		name     string
		sec, ms  int64
		expected int64
	}{
		{
			name:     "有毫秒字段",
			sec:      1000,
			ms:       1000500,
			expected: 1000500,
		},
		{
			name:     "无毫秒字段，fallback 到秒",
			sec:      1779439612,
			ms:       0,
			expected: 1779439612000,
		},
		{
			name:     "都为零",
			sec:      0,
			ms:       0,
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := threadEffectiveTS(tt.sec, tt.ms)
			if result != tt.expected {
				t.Errorf("threadEffectiveTS = %d, want %d", result, tt.expected)
			}
		})
	}
}

// ===== Codex 消息级采集测试 () =====

// messageLevelFixture 解析 testdata/codex/message-level.jsonl 产出 Messages。
// 共享前置。
func messageLevelFixture(t *testing.T) CollectResult {
	t.Helper()
	fallback := codexThread{}
	dates := map[string]struct{}{}
	result, err := parseCodexRollout("../../testdata/codex/message-level.jsonl", fallback, dates)
	if err != nil {
		t.Fatalf("parseCodexRollout failed: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected non-zero messages from message-level.jsonl")
	}
	return result
}

// 不读取 threads.tokens_used 作为 token 来源。
// message-level.jsonl 不含 state DB，token 值必须来自 rollout 的 token_count 事件。
func TestCodexCollector_UsesRolloutTokenUsage(t *testing.T) {
	result := messageLevelFixture(t)

	// 第一条 msg-a#0：input=100, output=20, total=120（来自 token_count，而非 state DB tokens_used）
	var m model.Message
	for _, msg := range result.Messages {
		if msg.ID == "msg-a#0" {
			m = msg
			break
		}
	}
	if m.ID != "msg-a#0" {
		t.Fatalf("msg-a#0 not found among %d messages", len(result.Messages))
	}
	if m.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100 (from token_count event)", m.InputTokens)
	}
	if m.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20 (from token_count event)", m.OutputTokens)
	}
	if m.TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120 (from token_count event)", m.TotalTokens)
	}
	if m.Provider != "OpenAI" {
		t.Errorf("Provider = %q, want OpenAI (official Codex channel)", m.Provider)
	}
	if m.SessionID != "thread-1" {
		t.Errorf("SessionID = %q, want 'thread-1' (from session_meta)", m.SessionID)
	}
}

// token_count 关联最近的 assistant response_item.id。
func TestCodexCollector_AssociatesNearestAssistantMessage(t *testing.T) {
	result := messageLevelFixture(t)

	// msg-a 的 token_count 关联到 response_item id="msg-a"。
	// msg-b 的 token_count 关联到 response_item id="msg-b"。
	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if _, ok := byID["msg-a#0"]; !ok {
		t.Errorf("expected msg-a#0 (associated to response_item msg-a), got IDs: %v", codexMsgIDs(result.Messages))
	}
	if _, ok := byID["msg-b#0"]; !ok {
		t.Errorf("expected msg-b#0 (associated to response_item msg-b), got IDs: %v", codexMsgIDs(result.Messages))
	}
}

// 同一 msg 多个非零 token_count 派生 #0/#1/#2 序号。
func TestCodexCollector_MultipleCallsPerMessage(t *testing.T) {
	result := messageLevelFixture(t)

	// msg-a 有两个非零 token_count（03:03 和 03:04），应派生 msg-a#0 和 msg-a#1。
	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	m0, ok0 := byID["msg-a#0"]
	if !ok0 {
		t.Fatalf("msg-a#0 missing; IDs: %v", codexMsgIDs(result.Messages))
	}
	m1, ok1 := byID["msg-a#1"]
	if !ok1 {
		t.Fatalf("msg-a#1 missing; IDs: %v", codexMsgIDs(result.Messages))
	}
	// 第一条 usage: input=100, output=20；第二条 usage: input=200, output=40。
	if m0.InputTokens != 100 || m0.OutputTokens != 20 {
		t.Errorf("msg-a#0 usage = input %d/output %d, want 100/20", m0.InputTokens, m0.OutputTokens)
	}
	if m1.InputTokens != 200 || m1.OutputTokens != 40 {
		t.Errorf("msg-a#1 usage = input %d/output %d, want 200/40", m1.InputTokens, m1.OutputTokens)
	}
}

// 全零 compaction 事件不入库且不占序号。
func TestCodexCollector_FiltersZeroReset(t *testing.T) {
	result := messageLevelFixture(t)

	// msg-a 后的全零 token_count（03:05）不应产出消息，且不占用 msg-a 的序号。
	// 验证：不存在 msg-a#2（若全零占序号，#1 后的下一条非零应是 #2）。
	for _, m := range result.Messages {
		if strings.HasPrefix(m.ID, "msg-a#") && m.ID != "msg-a#0" && m.ID != "msg-a#1" {
			t.Errorf("unexpected msg-a derived ID %q: zero reset should not produce a message or occupy a sequence", m.ID)
		}
	}
}

// 只有五个 token 字段全零才是 compaction reset；辅助字段含用量时必须保留，
// 避免未来或异常源数据中的缓存/Reasoning 用量被静默丢弃。
func TestCodexCollector_KeepsUsageWhenSecondaryTokenFieldsAreNonZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secondary-token-usage.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-secondary","source":"cli","originator":"codex-tui","cwd":"/tmp/project"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"turn_context","payload":{"model":"gpt-test"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-secondary"}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0,"cached_input_tokens":7,"output_tokens":0,"reasoning_output_tokens":3,"total_tokens":10}}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("辅助 token 字段非零的事件应保留，实际 messages=%d", len(result.Messages))
	}
	got := result.Messages[0]
	if got.CacheReadTokens != 7 || got.ReasoningTokens != 3 || got.TotalTokens != 10 {
		t.Fatalf("token fields = cache:%d reasoning:%d total:%d, want 7/3/10",
			got.CacheReadTokens, got.ReasoningTokens, got.TotalTokens)
	}
}

// 每条 token_count 使用当时 turn_context.model。
func TestCodexCollector_UsesTurnContextModel(t *testing.T) {
	result := messageLevelFixture(t)

	// 03:01 turn_context model=gpt-5.4 → msg-a 的 token_count 都用 gpt-5.4。
	// 03:06 turn_context model=gpt-5.5 → msg-b 的 token_count 用 gpt-5.5。
	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if m, ok := byID["msg-a#0"]; ok {
		if m.Model != "gpt-5.4" {
			t.Errorf("msg-a#0 Model = %q, want 'gpt-5.4' (turn_context at 03:01)", m.Model)
		}
	} else {
		t.Errorf("msg-a#0 not found")
	}
	if m, ok := byID["msg-b#0"]; ok {
		if m.Model != "gpt-5.5" {
			t.Errorf("msg-b#0 Model = %q, want 'gpt-5.5' (turn_context at 03:06)", m.Model)
		}
	} else {
		t.Errorf("msg-b#0 not found")
	}
}

// fresh=input-cached_input；total 使用源值；reasoning 保留但不重复相加。
func TestCodexCollector_FreshInputAndReasoning(t *testing.T) {
	result := messageLevelFixture(t)

	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	m, ok := byID["msg-a#1"]
	if !ok {
		t.Fatalf("msg-a#1 not found")
	}
	// usage: input=200, cached=80, output=40, reasoning=10, total=250
	// fresh = input - cached = 200 - 80 = 120
	if m.FreshInputTokens != 120 {
		t.Errorf("FreshInputTokens = %d, want 120 (input 200 - cached 80)", m.FreshInputTokens)
	}
	if m.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80", m.CacheReadTokens)
	}
	if m.ReasoningTokens != 10 {
		t.Errorf("ReasoningTokens = %d, want 10", m.ReasoningTokens)
	}
	// total 使用源值 250，不重新相加（input+output=240 ≠ 250，
	// 若错误重算会得到 240；reasoning 已在 total 内，不会重复加）。
	if m.TotalTokens != 250 {
		t.Errorf("TotalTokens = %d, want 250 (source total_tokens)", m.TotalTokens)
	}
}

// === 重播去重测试 ===

// 同一份 (total,last) 完整快照在不同 rate_limits.limit_id 下重播时只计一次。
// 复现本地真实场景：codex 切换限流桶（codex↔premium）时重播同一累计快照。
func TestParseRollout_ReplayedSnapshotUnderDifferentLimitIDDeduped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay-cross-limit.jsonl")
	// 两条 token_count 的 (total,last) 完全相同，仅 limit_id 不同 → 第二条为重播。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-replay","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-replay"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050}},"rate_limits":{"limit_id":"premium"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("重播快照应去重为 1 条 Message，实际 %d 条（IDs: %v）；未去重会让 input 翻倍",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// 单条 input 应为原始 1000，而非累计 2000。
	if got := result.Messages[0].InputTokens; got != 1000 {
		t.Errorf("去重后 InputTokens = %d, want 1000（重播未去重会变 2000）", got)
	}
}

// 两个 limit_id 通道各自累计、交错出现，且每条 last 不同 → 每条都应保留，不误杀。
// 这条同时守护「同 msg 不同签名多轮调用」语义（现有 message-level.jsonl 的场景）。
func TestParseRollout_InterleavedCountersUseExactLastUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interleaved-lanes.jsonl")
	// limit_id=codex 通道 last=(100,20)，limit_id=premium 通道 last=(300,60)，交错出现且 last 各异。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-interleave","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-a"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":300,"cached_input_tokens":0,"output_tokens":60,"reasoning_output_tokens":0,"total_tokens":360},"total_token_usage":{"input_tokens":300,"cached_input_tokens":0,"output_tokens":60,"reasoning_output_tokens":0,"total_tokens":360}},"rate_limits":{"limit_id":"premium"}}}
{"timestamp":"2026-07-08T03:04:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":25,"reasoning_output_tokens":0,"total_tokens":175},"total_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":25,"reasoning_output_tokens":0,"total_tokens":175}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("交错的不同 last 用量应全部保留为 3 条，实际 %d 条（IDs: %v）；被误杀会丢失合法多轮",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// input 总和 = 100+300+150 = 550；若误去重任一条会偏小。
	var sum int64
	for _, m := range result.Messages {
		sum += m.InputTokens
	}
	if sum != 550 {
		t.Errorf("三条 Message 的 input 总和 = %d, want 550（误去重会偏小）", sum)
	}
}

// 紧邻重播判定不受非 token 事件（turn_context/response_item）影响。
func TestParseRollout_AdjacentReplayAcrossNonTokenEventsDeduped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay-across-events.jsonl")
	// 两条相同 (total,last)，中间隔了 turn_context + 新的 response_item。
	// 注意：第二条关联到不同 assistant msg，但签名相同且 prevTokenSig 匹配 → 应去重。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-across","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-first"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":30,"reasoning_output_tokens":5,"total_tokens":530},"total_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":30,"reasoning_output_tokens":5,"total_tokens":530}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"turn_context","payload":{"model":"gpt-other"}}
{"timestamp":"2026-07-08T03:04:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-second"}}
{"timestamp":"2026-07-08T03:05:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":30,"reasoning_output_tokens":5,"total_tokens":530},"total_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":30,"reasoning_output_tokens":5,"total_tokens":530}},"rate_limits":{"limit_id":"premium"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("跨非 token 事件的相同签名仍应去重为 1 条，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
}

// 合法计数器 reset 后再现旧值，但 total 已变化 → 不应被误去重。
// 守护窄比对原则：只比同源最近 + 紧邻，不做全表去重，避免吞掉真实 reset。
func TestParseRollout_LegitimateResetNotSwallowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legit-reset.jsonl")
	// 场景：codex 通道先用量 (100,20)，premium 通道重播同一快照（应去重），
	// 然后 codex 通道经历 reset 后再现 input=100（last 与首条相同，但 total 已增长）→ 应保留。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-reset","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-r"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"premium"}}}
{"timestamp":"2026-07-08T03:04:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":2000,"cached_input_tokens":0,"output_tokens":400,"reasoning_output_tokens":0,"total_tokens":2400}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 第1条保留；第2条重播（premium 同签名 total=120）去重；第3条 last 虽与第1条相同
	// 但 total=2400 已变化（非重播）→ 保留。共 2 条。
	if len(result.Messages) != 2 {
		t.Fatalf("合法 reset（total 变化）不应被误去重，期望 2 条 Message，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// 第3条因走 total 高水位差减：delta = 2400-120=2280 input（验证 total-only 路径正确）。
	// 按 last 优先原则，第3条 last 存在且非零，应取 last=(100,20) 而非 total 差减。
	var secondInput int64
	for _, m := range result.Messages {
		if m.ID == "msg-r#1" {
			secondInput = m.InputTokens
		}
	}
	if secondInput != 100 {
		t.Errorf("第3条（合法 reset）应取 last input=100，实际 %d；若被误去重或误用 total 差减值会不同", secondInput)
	}
}

// 空 total 对象 {} 不启用去重：两条相同非零 last、不同 limit_id、total 都是 {}，
// 应都保留。若 {} 被误当作有效 total 启用去重，相同 last 的紧邻/同源判定会吞掉第二条。
func TestParseRollout_EmptyTotalObjectDoesNotEnableDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-total.jsonl")
	// 两条 last 完全相同（100/30/20），total 都是空对象 {}，limit_id 不同。
	// hasTotal 应为 false（空对象过滤为 nil），去重不启用 → 两条都保留。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-empty","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-e"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{}},"rate_limits":{"limit_id":"premium"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 两条 last 相同但 total 为空对象（不启用去重）→ 都应保留。共 2 条。
	// 若空对象错误启用去重：紧邻判定（签名相同）会吞掉第2条，剩 1 条。
	if len(result.Messages) != 2 {
		t.Fatalf("空 total 不应启用去重，期望 2 条相同 last 的 Message 都保留，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	var sum int64
	for _, m := range result.Messages {
		sum += m.InputTokens
	}
	// 100 + 100 = 200；若空对象启用去重吞掉一条，sum 变 100。
	if sum != 200 {
		t.Errorf("input 总和 = %d, want 200（空对象错误启用去重会吞掉一条变 100）", sum)
	}
}

// 带 last 的完整快照也要推进 total 高水位，否则后续 total-only 回退会重复计。
// 场景：last=100/total=100 → 空 last/total=150。正确 delta = 100 + (150-100) = 150。
// 若高水位未随第1条 total 推进，第2条会算成 100+150=250。
func TestParseRollout_LastSnapshotAdvancesTotalHighWater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "highwater.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-hw","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-hw"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"reasoning_output_tokens":0,"total_tokens":180}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("期望 2 条 Message，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// 第1条 last input=100；第2条 total-only 差减：150-100=50。
	var sum int64
	for _, m := range result.Messages {
		sum += m.InputTokens
	}
	if sum != 150 {
		t.Errorf("input 总和 = %d, want 150（100 + total 差减 50）；高水位未随第1条推进会算成 250", sum)
	}
}

// total-only 差减产生 zero delta 时不分配序号、不生成 Message。
// 场景：total=100 → total=100（相同累计，delta=0）。不应产出零 token 消息。
func TestParseRollout_ZeroDeltaSuppressesIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero-delta.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-zd","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-zd"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:04:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":240}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 第1条 total=100 → delta=100（首条）；第2条 total=100 → delta=0（zero，抑制）；
	// 第3条 total=200 → delta=100。共 2 条 Message，且 ID 连续为 #0/#1（zero 不占序号）。
	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if len(result.Messages) != 2 {
		t.Fatalf("zero delta 应抑制，期望 2 条 Message，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	if _, ok := byID["msg-zd#0"]; !ok {
		t.Errorf("期望 msg-zd#0 存在，实际 IDs: %v", codexMsgIDs(result.Messages))
	}
	// 第3条应是 #1（第2条 zero delta 不占序号），而非 #2。
	if _, ok := byID["msg-zd#1"]; !ok {
		t.Errorf("zero delta 不应占序号，期望第3条为 msg-zd#1，实际 IDs: %v", codexMsgIDs(result.Messages))
	}
	if _, ok := byID["msg-zd#2"]; ok {
		t.Errorf("不应存在 msg-zd#2（zero delta 占了序号），IDs: %v", codexMsgIDs(result.Messages))
	}
}

// 同 limit_id 下 total 相同、last 不同 → 不应误判重播（完整签名比对的价值）。
// 仅比 total 会把这类合法多轮调用误杀。
func TestParseRollout_SameSourceSameTotalDifferentLastNotDeduped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "same-total-diff-last.jsonl")
	// 两条同 limit_id=codex，total 都是 200，但 last 不同（100/30 vs 150/40）→ 都保留。
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-st","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-st"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":240}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":40,"output_tokens":25,"reasoning_output_tokens":0,"total_tokens":175},"total_token_usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":240}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 两条 last 不同，即使 total 相同也应都保留。共 2 条。
	if len(result.Messages) != 2 {
		t.Fatalf("同 total 不同 last 不应误杀，期望 2 条 Message，实际 %d 条（IDs: %v）；仅比 total 会误判重播",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// 验证两条 input 不同（100 和 150），确是不同事件。
	if result.Messages[0].InputTokens != 100 || result.Messages[1].InputTokens != 150 {
		t.Errorf("两条 input 应为 100/150，实际 %d/%d",
			result.Messages[0].InputTokens, result.Messages[1].InputTokens)
	}
}

// 显式零 last（last 字段存在但全零）不回退到 total 差减，应得到 zero delta 并被抑制。
// 场景：last=100/total=100 → last={0...}/total=150。正确：第1条 100，第2条 zero delta 抑制。
// 若把显式零 last 当缺失回退 total 差减，第2条会错误写入 input=50。
func TestParseRollout_ExplicitZeroLastDoesNotFallBackToTotal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero-last.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-zl","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-zl"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":120}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":0},"total_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"reasoning_output_tokens":0,"total_tokens":180}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 第1条 last=100 保留；第2条 last 全零 → zero delta 抑制（不回退 total 差减）。共 1 条。
	if len(result.Messages) != 1 {
		t.Fatalf("显式零 last 应得到 zero delta 抑制，期望 1 条 Message，实际 %d 条（IDs: %v）；若回退 total 差减会多出 input=50 的消息",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	if result.Messages[0].InputTokens != 100 {
		t.Errorf("唯一保留的 Message input 应为 100，实际 %d", result.Messages[0].InputTokens)
	}
}

// total-only 差减的 cache clamp：累计 (input,cached) 从 (100,0) 涨到 (100,50) 时，
// delta 得到 (0,50)，应 clamp 为 (0,0) → zero delta 抑制。不 clamp 会落 cache>input 的消息。
func TestParseRollout_TotalDeltaCacheClamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-clamp.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"thread-cc","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-cc"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":110}},"rate_limits":{"limit_id":"codex"}}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":160}},"rate_limits":{"limit_id":"codex"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	// 第1条 total-only delta=(100,0,10) 保留；第2条 delta=(0,50,0) → clamp cached=min(50,0)=0 → zero delta 抑制。
	// 共 1 条。若不 clamp，第2条会落 cache=50>input=0 的消息。
	if len(result.Messages) != 1 {
		t.Fatalf("cache clamp 后 zero delta 应抑制第2条，期望 1 条 Message，实际 %d 条（IDs: %v）",
			len(result.Messages), codexMsgIDs(result.Messages))
	}
	// 确认保留的消息没有 cache>input。
	m := result.Messages[0]
	if m.CacheReadTokens > m.InputTokens {
		t.Errorf("cache(%d) 不应大于 input(%d)；total-only 差减结果未做 clamp", m.CacheReadTokens, m.InputTokens)
	}
}

// 父/子共享 msg 的有序非零 usage 与派生 ID 完全一致。
func TestCodexCollector_ForkSharedMessageStable(t *testing.T) {
	dates := map[string]struct{}{}
	parent, err := parseCodexRollout("../../testdata/codex/fork-parent.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse parent failed: %v", err)
	}
	child, err := parseCodexRollout("../../testdata/codex/fork-child.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse child failed: %v", err)
	}

	parentShared := filterSharedMessages(parent.Messages, "msg-shared")
	childShared := filterSharedMessages(child.Messages, "msg-shared")

	if len(parentShared) != len(childShared) {
		t.Fatalf("shared msg count differs: parent=%d child=%d", len(parentShared), len(childShared))
	}
	if len(parentShared) != 2 {
		t.Fatalf("expected 2 shared non-zero token_count, got %d", len(parentShared))
	}
	for i := range parentShared {
		if parentShared[i].ID != childShared[i].ID {
			t.Errorf("shared[%d] ID: parent=%q child=%q", i, parentShared[i].ID, childShared[i].ID)
		}
		if parentShared[i].ID != "msg-shared#0" && parentShared[i].ID != "msg-shared#1" {
			t.Errorf("unexpected derived ID %q, want msg-shared#0 or msg-shared#1", parentShared[i].ID)
		}
		if parentShared[i].InputTokens != childShared[i].InputTokens ||
			parentShared[i].OutputTokens != childShared[i].OutputTokens ||
			parentShared[i].CacheReadTokens != childShared[i].CacheReadTokens ||
			parentShared[i].ReasoningTokens != childShared[i].ReasoningTokens ||
			parentShared[i].TotalTokens != childShared[i].TotalTokens {
			t.Errorf("shared[%d] %q usage mismatch:\n  parent=%+v\n  child =%+v",
				i, parentShared[i].ID, parentShared[i], childShared[i])
		}
	}
	// 确保派生 ID 顺序为 #0, #1。
	if parentShared[0].ID != "msg-shared#0" {
		t.Errorf("parent shared[0].ID = %q, want msg-shared#0", parentShared[0].ID)
	}
	if parentShared[1].ID != "msg-shared#1" {
		t.Errorf("parent shared[1].ID = %q, want msg-shared#1", parentShared[1].ID)
	}
}

// fork 重写时间后 DB 仍保留父会话较早归因。
// 先写子 fork Messages（ts 较大），再写父 Messages（ts 较小）；
// 共享派生 ID 行的 ts/date/session/directory 应来自父记录（较早 ts），token 值以最后一次写入覆盖。
func TestCodexCollector_ForkUpsertRetainsEarlierAttribution(t *testing.T) {
	dates := map[string]struct{}{}
	child, err := parseCodexRollout("../../testdata/codex/fork-child.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse child failed: %v", err)
	}
	parent, err := parseCodexRollout("../../testdata/codex/fork-parent.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse parent failed: %v", err)
	}

	dbObj, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	t.Cleanup(func() { dbObj.Close() })

	ctx := context.Background()
	// 先写子（ts 较大，03:00+）。
	if _, err := db.UpsertMessages(ctx, dbObj, child.Messages); err != nil {
		t.Fatalf("upsert child failed: %v", err)
	}
	// 再写父（ts 较小，01:00+）。
	if _, err := db.UpsertMessages(ctx, dbObj, parent.Messages); err != nil {
		t.Fatalf("upsert parent failed: %v", err)
	}

	// 共享 ID msg-shared#0：父 ts(01:03) < 子 ts(02:03)，归因应来自父。
	var sessionID, directory, date string
	var ts int64
	var inputTokens, totalTokens int64
	err = dbObj.QueryRow(`SELECT session_id, directory, date, ts, input_tokens, total_tokens
		FROM messages WHERE client=? AND id=?`,
		model.ClientCodexCLI, "msg-shared#0").
		Scan(&sessionID, &directory, &date, &ts, &inputTokens, &totalTokens)
	if err != nil {
		t.Fatalf("query msg-shared#0 failed: %v", err)
	}
	// 归因来自父（sessionID=parent-thread, directory=/tmp/parent, 较早 ts）。
	if sessionID != "parent-thread" {
		t.Errorf("session_id = %q, want 'parent-thread' (earlier parent attribution)", sessionID)
	}
	if directory != "/tmp/parent" {
		t.Errorf("directory = %q, want '/tmp/parent' (earlier parent attribution)", directory)
	}
	parentTS := time.Date(2026, 7, 9, 1, 3, 0, 0, time.UTC).UnixMilli()
	if ts != parentTS {
		t.Errorf("ts = %d, want %d (parent earlier ts)", ts, parentTS)
	}
	// token 值由 excluded 总是覆盖：父 input=100, total=120。
	if inputTokens != 100 {
		t.Errorf("input_tokens = %d, want 100 (parent overwrites)", inputTokens)
	}
	if totalTokens != 120 {
		t.Errorf("total_tokens = %d, want 120 (parent overwrites)", totalTokens)
	}
}

// archived_sessions 的绝对 rollout_path 可读取。
func TestCodexCollector_ReadsArchivedRolloutPath(t *testing.T) {
	// 创建临时 sessions 根，rollout 存放在 archived_sessions 子目录。
	tmp := t.TempDir()
	archivedDir := filepath.Join(tmp, "sessions", "archived_sessions", "2026", "07", "08")
	if err := os.MkdirAll(archivedDir, 0755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(archivedDir, "rollout-archived.jsonl")
	data, err := os.ReadFile("../../testdata/codex/message-level.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// state thread 指向 archived 绝对路径。
	stateDir := filepath.Join(tmp, "state")
	os.MkdirAll(stateDir, 0755)
	threads := []codexThread{{
		ID: "thread-1", RolloutPath: rolloutPath, Cwd: "/tmp/codex",
		Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 1,
	}}
	createStateDBWithThreads(t, stateDir, threads)

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": filepath.Join(tmp, "sessions"),
		}},
	}}
	collector := NewCodexCollector(cfg)
	result, err := collector.Collect(context.Background(),
		CollectRequest{Dates: []string{"2026-07-08"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected messages from archived rollout_path")
	}
}

// ChangedFile 模式只扫指定 rollout；无 state fallback 时用 session_meta.source/originator
// 区分 Codex CLI/App；缺 session_meta.id 报错。
func TestCodexCollector_ChangedFileMode(t *testing.T) {
	t.Run("vscode source maps to Codex App", func(t *testing.T) {
		cfg := &config.Config{Clients: map[string]config.Client{
			"codex": {Enabled: true, Paths: map[string]string{
				"state_dir": "/nonexistent", "sessions_dir": "/nonexistent",
			}},
		}}
		collector := NewCodexCollector(cfg)
		result, err := collector.Collect(context.Background(),
			CollectRequest{ChangedFile: "../../testdata/codex/vscode-app.jsonl"}, slog.Default())
		if err != nil {
			t.Fatalf("Collect failed: %v", err)
		}
		if len(result.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(result.Messages))
		}
		if result.Messages[0].Client != model.ClientCodexApp {
			t.Errorf("Client = %q, want %q (vscode source → Codex App)",
				result.Messages[0].Client, model.ClientCodexApp)
		}
		if result.Messages[0].SessionID != "vscode-thread" {
			t.Errorf("SessionID = %q, want 'vscode-thread' (from session_meta.id)",
				result.Messages[0].SessionID)
		}
	})

	t.Run("cli source maps to Codex CLI", func(t *testing.T) {
		cfg := &config.Config{Clients: map[string]config.Client{
			"codex": {Enabled: true, Paths: map[string]string{
				"state_dir": "/nonexistent", "sessions_dir": "/nonexistent",
			}},
		}}
		collector := NewCodexCollector(cfg)
		result, err := collector.Collect(context.Background(),
			CollectRequest{ChangedFile: "../../testdata/codex/message-level.jsonl"}, slog.Default())
		if err != nil {
			t.Fatalf("Collect failed: %v", err)
		}
		for _, m := range result.Messages {
			if m.Client != model.ClientCodexCLI {
				t.Errorf("Client = %q, want %q (cli/codex-tui → Codex CLI)",
					m.Client, model.ClientCodexCLI)
			}
		}
	})

	t.Run("missing session_meta.id returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		file := filepath.Join(tmpDir, "no-meta-id.jsonl")
		// session_meta 无 id 字段，response_item 与 token_count 仍在。
		content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"source":"cli","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-x"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}
`
		os.WriteFile(file, []byte(content), 0644)

		cfg := &config.Config{Clients: map[string]config.Client{
			"codex": {Enabled: true, Paths: map[string]string{
				"state_dir": "/nonexistent", "sessions_dir": "/nonexistent",
			}},
		}}
		collector := NewCodexCollector(cfg)
		_, err := collector.Collect(context.Background(),
			CollectRequest{ChangedFile: file}, slog.Default())
		if err == nil {
			t.Fatal("expected error when session_meta.id missing and no state fallback")
		}
	})
}

// state 增量 (updated_at_ms,id) 定位变更 rollout。
func TestCodexCollector_IncrementalCursor(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	// 两个 rollout 文件 + 两个 state thread。
	rollout1 := filepath.Join(sessionsDir, "rollout-1.jsonl")
	rollout2 := filepath.Join(sessionsDir, "rollout-2.jsonl")
	writeRollout(t, rollout1, "thread-1", "gpt-5.4", "msg-1")
	writeRollout(t, rollout2, "thread-2", "gpt-5.5", "msg-2")

	threads := []codexThread{
		{ID: "thread-1", RolloutPath: rollout1, Cwd: "/tmp/a", Source: "cli", UpdatedAtMS: 1000},
		{ID: "thread-2", RolloutPath: rollout2, Cwd: "/tmp/b", Source: "cli", UpdatedAtMS: 2000},
	}
	createStateDBWithThreads(t, stateDir, threads)

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	collector := NewCodexCollector(cfg)

	// 首次增量：cursor 为零值，应返回全部 thread。
	first, err := collector.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceCodexState: {}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("first incremental Collect failed: %v", err)
	}
	if len(first.Messages) != 2 {
		t.Fatalf("expected 2 messages on first incremental, got %d", len(first.Messages))
	}
	nc, ok := first.NextCursors[SyncSourceCodexState]
	if !ok {
		t.Fatal("expected NextCursor for codex_state")
	}
	if nc.Value != 2000 || nc.ID != "thread-2" {
		t.Errorf("first NextCursor = {%d,%q}, want {2000,thread-2}", nc.Value, nc.ID)
	}

	// 第二次增量：cursor={2000,thread-2}，无新 thread，应返回 0 消息。
	second, err := collector.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceCodexState: {Value: 2000, ID: "thread-2"}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("second incremental Collect failed: %v", err)
	}
	if len(second.Messages) != 0 {
		t.Errorf("expected 0 messages after cursor, got %d", len(second.Messages))
	}
	// cursor 不回退。
	nc2, ok := second.NextCursors[SyncSourceCodexState]
	if !ok || nc2.Value != 2000 || nc2.ID != "thread-2" {
		t.Errorf("second NextCursor = {%d,%q}, want {2000,thread-2} (no regression)", nc2.Value, nc2.ID)
	}

	// 新增 thread-3（updated_at_ms=3000），只返回 thread-3 的消息。
	rollout3 := filepath.Join(sessionsDir, "rollout-3.jsonl")
	writeRollout(t, rollout3, "thread-3", "gpt-5.5", "msg-3")
	threads3 := []codexThread{
		{ID: "thread-1", RolloutPath: rollout1, Cwd: "/tmp/a", Source: "cli", UpdatedAtMS: 1000},
		{ID: "thread-2", RolloutPath: rollout2, Cwd: "/tmp/b", Source: "cli", UpdatedAtMS: 2000},
		{ID: "thread-3", RolloutPath: rollout3, Cwd: "/tmp/c", Source: "cli", UpdatedAtMS: 3000},
	}
	createStateDBWithThreads(t, stateDir, threads3)
	third, err := collector.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceCodexState: {Value: 2000, ID: "thread-2"}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("third incremental Collect failed: %v", err)
	}
	if len(third.Messages) != 1 {
		t.Fatalf("expected 1 message for new thread-3, got %d", len(third.Messages))
	}
	if third.Messages[0].SessionID != "thread-3" {
		t.Errorf("message SessionID = %q, want 'thread-3'", third.Messages[0].SessionID)
	}
}

// session_meta.forked_from_id 写入 ParentID。
func TestCodexCollector_ForkParentIDFromMeta(t *testing.T) {
	dates := map[string]struct{}{}
	child, err := parseCodexRollout("../../testdata/codex/fork-child.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse child failed: %v", err)
	}
	// fork-child.jsonl 的 session_meta 包含 forked_from_id=parent-thread。
	var found bool
	for _, s := range child.Sessions {
		if s.ID == "child-thread" && s.ParentID == "parent-thread" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected Session child-thread with ParentID 'parent-thread'; sessions: %+v", child.Sessions)
	}

	// parent 没有 forked_from_id，ParentID 应为空。
	parent, err := parseCodexRollout("../../testdata/codex/fork-parent.jsonl", codexThread{}, dates)
	if err != nil {
		t.Fatalf("parse parent failed: %v", err)
	}
	for _, s := range parent.Sessions {
		if s.ParentID != "" {
			t.Errorf("parent Session %q should have empty ParentID, got %q", s.ID, s.ParentID)
		}
	}
}

// dates 过滤掉所有消息时不应返回 Session。
// parseCodexRollout 自身在无命中消息时仍构造 Session（用于上游汇总），
// 但 Collect 必须丢弃这类空 Session，避免 INSERT OR REPLACE 用空数据覆盖已有会话。
func TestCodexCollector_DropsSessionWhenAllMessagesFiltered(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	rolloutPath := filepath.Join(sessionsDir, "rollout-1.jsonl")
	writeRollout(t, rolloutPath, "thread-1", "gpt-5.4", "msg-1")
	threads := []codexThread{
		{ID: "thread-1", RolloutPath: rolloutPath, Cwd: "/tmp", Source: "cli", UpdatedAtMS: 1},
	}
	createStateDBWithThreads(t, stateDir, threads)

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	collector := NewCodexCollector(cfg)

	// Dates 指定一个不匹配任何消息的日期（消息均为 2026-07-08）。
	result, err := collector.Collect(context.Background(),
		CollectRequest{Dates: []string{"2099-01-01"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("expected 0 messages when no date matches, got %d", len(result.Messages))
	}
	if len(result.Sessions) != 0 {
		t.Errorf("expected 0 sessions when no message matches dates (avoid empty overwrite), got %d: %+v",
			len(result.Sessions), result.Sessions)
	}
}

// ===== Codex 多 state DB 降级测试 =====

func TestCodex_StateDBFailure_LogsWarn(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	// 有效 DB（含一个 thread，rollout 文件存在）。
	rolloutPath := filepath.Join(sessionsDir, "rollout-1.jsonl")
	writeRollout(t, rolloutPath, "thread-1", "gpt-5.4", "msg-1")
	goodThreads := []codexThread{
		{ID: "thread-1", RolloutPath: rolloutPath, Cwd: "/tmp", Source: "cli", UpdatedAtMS: 1},
	}
	createStateDBWithThreads(t, stateDir, goodThreads)

	// 损坏 DB（无 threads 表）。
	broken, err := sql.Open("sqlite", filepath.Join(stateDir, "state_6.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Exec(`CREATE TABLE metadata (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	broken.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	handler := &testLogHandler{}
	_, err = NewCodexCollector(cfg).Collect(context.Background(),
		CollectRequest{Dates: []string{}}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if !handler.HasRecord(slog.LevelWarn, "Codex state DB query failed, skipped") {
		t.Fatalf("missing state DB warn record: %v", handler.Messages())
	}
}

func TestCodexCollector_AllStateDBsFailReturnsError(t *testing.T) {
	stateDir := t.TempDir()
	broken, err := sql.Open("sqlite", filepath.Join(stateDir, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Exec(`CREATE TABLE metadata (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	broken.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": filepath.Join(stateDir, "sessions"),
		}},
	}}
	_, err = NewCodexCollector(cfg).Collect(context.Background(),
		CollectRequest{Dates: []string{}}, slog.Default())
	if err == nil {
		t.Fatal("all state DBs failing must return error")
	}
}

// ===== 辅助函数 =====

func codexMsgIDs(msgs []model.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// filterSharedMessages 返回 ID 以 prefix 开头（如 "msg-shared"）的消息，按原顺序。
func filterSharedMessages(msgs []model.Message, prefix string) []model.Message {
	var out []model.Message
	for _, m := range msgs {
		if strings.HasPrefix(m.ID, prefix+"#") {
			out = append(out, m)
		}
	}
	return out
}

// createStateDBWithThreads 在 stateDir 下创建单个 state_5.sqlite，插入给定 threads。
func createStateDBWithThreads(t *testing.T, stateDir string, threads []codexThread) {
	t.Helper()
	dbPath := filepath.Join(stateDir, "state_5.sqlite")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open state DB: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT '',
		model_provider TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		tokens_used INTEGER NOT NULL DEFAULT 0,
		archived INTEGER NOT NULL DEFAULT 0,
		first_user_message TEXT NOT NULL DEFAULT '',
		model TEXT,
		thread_source TEXT,
		agent_role TEXT,
		created_at_ms INTEGER,
		updated_at_ms INTEGER
	)`); err != nil {
		t.Fatalf("create threads table: %v", err)
	}
	for _, th := range threads {
		if _, err := conn.Exec(`INSERT OR REPLACE INTO threads
			(id, rollout_path, created_at, updated_at, source, cwd, title,
			 model, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			th.ID, th.RolloutPath, th.CreatedAt, th.UpdatedAt, th.Source, th.Cwd, th.Title,
			th.Model, th.CreatedAtMS, th.UpdatedAtMS); err != nil {
			t.Fatalf("insert thread: %v", err)
		}
	}
}

// writeRollout 写一个最小 rollout JSONL：session_meta + turn_context + response_item + 一个 token_count。
func writeRollout(t *testing.T, path, sessionID, model, msgID string) {
	t.Helper()
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"turn_context","payload":{"model":"` + model + `"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"` + msgID + `"}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
}

// testLogHandler 复用 workbuddy_test.go 中的定义（同一 package）。
