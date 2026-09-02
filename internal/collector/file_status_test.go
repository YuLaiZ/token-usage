package collector

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// discardingLogger 丢弃输出的最小 logger（FileStatus 测试只断言状态不断言日志）。
func discardingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// === 逐文件采集状态（FileScanStatus）：四 collector 三形态 ===
//
// 覆盖 startup 跳过门的「fullyParsed」锚点：
//   - 正常文件：无坏行且尾行以 \n 终结（FullyParsed=true）；
//   - 含坏行文件：BadLines 计数 + 首行号 + 首错误（FullyParsed=false，永不推进门）；
//   - 无尾换行文件：TrailingNewline=false（尾行可能仍在写，不推进门）；
//   - 结构性过滤（stringForm、非 assistant、无 usage、user 行、空行）不计坏行。

func writeFileStatusFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- claude ----

const claudeNormalFixture = `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"u-1","role":"user","content":"plain string form"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:01:00+08:00","cwd":"/tmp/project","message":{"id":"msg-1","role":"assistant","model":"model-a","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}
`

func TestClaudeParseFileStatus_Normal(t *testing.T) {
	path := writeFileStatusFixture(t, claudeNormalFixture)
	_, status, err := parseClaudeMessageFile(path, nil, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.Path != path {
		t.Errorf("Path = %q, want %q", status.Path, path)
	}
	// stringForm（user 字符串形态）与 user 行都是设计内跳过，不得计入坏行。
	if status.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0 (stringForm/user 行为结构性过滤)", status.BadLines)
	}
	if !status.TrailingNewline {
		t.Error("TrailingNewline = false, want true")
	}
	if !status.FullyParsed() {
		t.Errorf("FullyParsed = false, want true (status=%+v)", status)
	}
}

func TestClaudeParseFileStatus_BadLines(t *testing.T) {
	path := writeFileStatusFixture(t, `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"u-1","role":"user"}}
not-json-line-2
{broken line 3
`)
	_, status, err := parseClaudeMessageFile(path, nil, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 2 {
		t.Errorf("BadLines = %d, want 2", status.BadLines)
	}
	if status.FirstBadLine != 2 {
		t.Errorf("FirstBadLine = %d, want 2", status.FirstBadLine)
	}
	if status.FirstBadErr == nil {
		t.Error("FirstBadErr = nil, want first parse error")
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false (含坏行)")
	}
}

func TestClaudeParseFileStatus_NoTrailingNewline(t *testing.T) {
	path := writeFileStatusFixture(t, strings.TrimSuffix(claudeNormalFixture, "\n"))
	_, status, err := parseClaudeMessageFile(path, nil, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0 (尾段恰好可解析也不算坏行)", status.BadLines)
	}
	if status.TrailingNewline {
		t.Error("TrailingNewline = true, want false (尾行无 \\n 终结)")
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

func TestClaudeCollectFileStatuses_Snapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(claudeNormalFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClaudeCollectorCfg(t, dir)
	result, err := c.Collect(context.Background(), CollectRequest{}, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FileStatuses) != 1 {
		t.Fatalf("FileStatuses = %d, want 1", len(result.FileStatuses))
	}
	st := result.FileStatuses[0]
	if st.Err != nil {
		t.Errorf("Err = %v, want nil", st.Err)
	}
	if !st.FullyParsed() {
		t.Errorf("FullyParsed = false, want true (status=%+v)", st)
	}
	// 读前/读后快照一致。identity 断言按平台分派：项目支持平台（darwin / windows）
	// 的真实文件系统上 dev:ino / file index 必须可用；其余平台 fsident 按设计恒不
	// 提供 identity（跳过门禁用），反向锁定为空。
	supportsIdentity := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if supportsIdentity && (st.Before.Identity == "" || st.After.Identity == "") {
		t.Errorf("快照 identity 为空: before=%+v after=%+v", st.Before, st.After)
	}
	if !supportsIdentity && (st.Before.Identity != "" || st.After.Identity != "") {
		t.Errorf("不支持平台快照 identity 应恒为空（跳过门禁用设计）: before=%+v after=%+v", st.Before, st.After)
	}
	if st.Before != st.After {
		t.Errorf("读写窗口内快照不一致: before=%+v after=%+v", st.Before, st.After)
	}
	if st.Before.Size != int64(len(claudeNormalFixture)) {
		t.Errorf("Before.Size = %d, want %d", st.Before.Size, len(claudeNormalFixture))
	}
}

// ---- codex ----

const codexNormalFixture = `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","originator":"Codex CLI","source":"cli","cwd":"/tmp/project"}}
{"timestamp":"2026-07-11T14:27:00Z","type":"turn_context","payload":{"model":"gpt-5"}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-1"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}
`

func TestCodexParseFileStatus_Normal(t *testing.T) {
	path := writeFileStatusFixture(t, codexNormalFixture)
	_, status, err := parseCodexRolloutContext(context.Background(), path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0", status.BadLines)
	}
	if !status.TrailingNewline {
		t.Error("TrailingNewline = false, want true")
	}
	if !status.FullyParsed() {
		t.Errorf("FullyParsed = false, want true (status=%+v)", status)
	}
}

func TestCodexParseFileStatus_BadLines(t *testing.T) {
	content := `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","source":"cli","cwd":"/tmp/project"}}
not-json-line-2
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-1"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}
`
	path := writeFileStatusFixture(t, content)
	_, status, err := parseCodexRolloutContext(context.Background(), path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (行 Unmarshal 失败须计数，不再静默)", status.BadLines)
	}
	if status.FirstBadLine != 2 {
		t.Errorf("FirstBadLine = %d, want 2", status.FirstBadLine)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

func TestCodexParseFileStatus_NoTrailingNewline(t *testing.T) {
	path := writeFileStatusFixture(t, strings.TrimSuffix(codexNormalFixture, "\n"))
	_, status, err := parseCodexRolloutContext(context.Background(), path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.TrailingNewline {
		t.Error("TrailingNewline = true, want false")
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// session_meta payload 整体解析失败降级为仅提取 ID：属数据异常（字段可能丢失），
// 必须计入坏行阻止门推进（此前为静默降级）。
func TestCodexParseFileStatus_SessionMetaDegraded(t *testing.T) {
	degraded := `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","source":{"subagent":{"unexpected":123}},"cwd":42}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-1"}}
`
	path := writeFileStatusFixture(t, degraded)
	_, status, err := parseCodexRolloutContext(context.Background(), path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (session_meta 降级提取计入坏行)", status.BadLines)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// ---- workbuddy ----

const workbuddyNormalFixture = `{"id":"msg-001","timestamp":1749312000000,"type":"message","role":"user","content":[],"sessionId":"sess-001","cwd":"/path"}
{"id":"msg-002","timestamp":1749312060000,"type":"message","role":"assistant","content":[],"providerData":{"model":"m","usage":{"inputTokens":1500,"outputTokens":800,"inputTokensDetails":[{"cached_tokens":1200}],"outputTokensDetails":[]}},"sessionId":"sess-001","cwd":"/path"}
`

func TestWorkBuddyParseFileStatus_Normal(t *testing.T) {
	path := writeFileStatusFixture(t, workbuddyNormalFixture)
	_, status, err := parseWorkBuddyJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0 (user 行/无 usage 行为结构性过滤)", status.BadLines)
	}
	if !status.TrailingNewline || !status.FullyParsed() {
		t.Errorf("TrailingNewline/..= %+v, want true/true", status)
	}
}

func TestWorkBuddyParseFileStatus_BadLines(t *testing.T) {
	content := `not-json-line-1
{"id":"msg-002","timestamp":1749312060000,"type":"message","role":"assistant","content":[],"providerData":{"model":"m","usage":{"inputTokens":1500,"outputTokens":800}},"sessionId":"sess-001","cwd":"/path"}
`
	path := writeFileStatusFixture(t, content)
	_, status, err := parseWorkBuddyJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 || status.FirstBadLine != 1 {
		t.Errorf("BadLines/FirstBadLine = %d/%d, want 1/1", status.BadLines, status.FirstBadLine)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// assistant 带 usage 但 timestamp 无效：数据异常（本应产出消息被丢弃），计入坏行。
func TestWorkBuddyParseFileStatus_TimestampInvalid(t *testing.T) {
	content := `{"id":"msg-001","timestamp":0,"type":"message","role":"assistant","content":[],"providerData":{"model":"m","usage":{"inputTokens":1500,"outputTokens":800}},"sessionId":"sess-001","cwd":"/path"}
`
	path := writeFileStatusFixture(t, content)
	_, status, err := parseWorkBuddyJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (timestamp 非法计入坏行)", status.BadLines)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

func TestWorkBuddyParseFileStatus_NoTrailingNewline(t *testing.T) {
	path := writeFileStatusFixture(t, strings.TrimSuffix(workbuddyNormalFixture, "\n"))
	_, status, err := parseWorkBuddyJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.TrailingNewline {
		t.Error("TrailingNewline = true, want false")
	}
}

// ---- autoclaw ----

func acStatusLine(id string) string {
	return acUsageLine(id, acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15)
}

const autoclawSessionLine = `{"type":"session","cwd":"/tmp/project"}` + "\n"

func TestAutoClawParseFileStatus_Normal(t *testing.T) {
	content := autoclawSessionLine + acStatusLine("aaa-m1") + "\n"
	path := writeFileStatusFixture(t, content)
	_, status, err := parseAutoClawJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0 (session 行为结构性过滤)", status.BadLines)
	}
	if !status.TrailingNewline || !status.FullyParsed() {
		t.Errorf("status = %+v, want fully parsed", status)
	}
}

func TestAutoClawParseFileStatus_BadLines(t *testing.T) {
	content := "not-json\n" + acStatusLine("aaa-m1") + "\n"
	path := writeFileStatusFixture(t, content)
	_, status, err := parseAutoClawJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 || status.FirstBadLine != 1 {
		t.Errorf("BadLines/FirstBadLine = %d/%d, want 1/1", status.BadLines, status.FirstBadLine)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// assistant 带 usage 但顶层 id 空：数据异常（防空 id 写入 (client,"") 主键），计入坏行。
func TestAutoClawParseFileStatus_EmptyID(t *testing.T) {
	noID := `{"type":"message","timestamp":"2026-07-29T12:00:00.000Z","message":{"role":"assistant","usage":{"input":10,"output":5,"total":15},"timestamp":1753788000000}}`
	path := writeFileStatusFixture(t, noID+"\n")
	_, status, err := parseAutoClawJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (顶层 id 空计入坏行)", status.BadLines)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// message.timestamp 缺失且顶层 RFC3339 解析失败：数据异常，计入坏行。
func TestAutoClawParseFileStatus_TimestampInvalid(t *testing.T) {
	badTS := `{"type":"message","id":"ts-bad","timestamp":"not-a-time","message":{"role":"assistant","usage":{"input":10,"output":5,"total":15},"timestamp":0}}`
	path := writeFileStatusFixture(t, badTS+"\n")
	_, status, err := parseAutoClawJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (timestamp 非法计入坏行)", status.BadLines)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

func TestAutoClawParseFileStatus_NoTrailingNewline(t *testing.T) {
	path := writeFileStatusFixture(t, strings.TrimSuffix(acStatusLine("aaa-m1"), "\n"))
	_, status, err := parseAutoClawJSONLContext(context.Background(), path, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.TrailingNewline {
		t.Error("TrailingNewline = true, want false")
	}
}

// 孤儿 token_count（无前置 assistant response_item）计入坏行：完整 rollout 中
// token_count 前必有 response_item，缺关联属数据异常，保守阻止门推进。
func TestCodexParseFileStatus_OrphanTokenCount(t *testing.T) {
	content := `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","source":"cli","cwd":"/tmp/project"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}
`
	path := writeFileStatusFixture(t, content)
	_, status, err := parseCodexRolloutContext(context.Background(), path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1 (孤儿 token_count 计入坏行)", status.BadLines)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false")
	}
}

// TestTailHasNewlineCRLF：CRLF 行结尾文件末字节是 \n（\r\n 序列的尾字节），
// 正常推进门资格；CR-only（无 \n）文件视为尾行未终结。
func TestTailHasNewlineCRLF(t *testing.T) {
	dir := t.TempDir()
	crlf := filepath.Join(dir, "crlf.jsonl")
	if err := os.WriteFile(crlf, []byte("{\"id\":\"a\"}\r\n{\"id\":\"b\"}\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, status, err := parseWorkBuddyJSONLContext(context.Background(), crlf, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !status.TrailingNewline {
		t.Error("CRLF 完整文件的末字节应为 \\n（TrailingNewline=true）")
	}
	cronly := filepath.Join(dir, "cr.jsonl")
	if err := os.WriteFile(cronly, []byte("{\"id\":\"a\"}\r{\"id\":\"b\"}\r"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, status, err = parseWorkBuddyJSONLContext(context.Background(), cronly, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if status.TrailingNewline {
		t.Error("CR-only（无 \\n 终结）文件不得视为完整尾行")
	}
}

// TestAutoClawParseFileStatus_ReadWindowMutationCaptured（读窗口竞态集成级）：
// 通过 parseFn seam 在解析完成后、读后快照取定前修改文件，Collect 层的
// Before/After 快照必须捕获到不一致（该状态交给 engine 后门不推进、消息照常落库）。
func TestAutoClawParseFileStatus_ReadWindowMutationCaptured(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions", "main", "sessions")
	cfg := newAutoClawCfg(t, filepath.Join(root, "sessions"))
	c := NewAutoClawCollector(cfg)
	file := filepath.Join(sessionsDir, "race.jsonl")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(acStatusLine("race-m1")), 0o600); err != nil {
		t.Fatal(err)
	}
	c.parseFn = func(ctx context.Context, path string, logger *slog.Logger) ([]autoclawParsedMessage, FileScanStatus, error) {
		msgs, status, err := parseAutoClawJSONLContext(ctx, path, logger)
		if err == nil && path == file {
			// 模拟读取窗口内的并发追加：解析已读旧内容，读后快照将取到新 size。
			f, aerr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if aerr != nil {
				t.Errorf("append: %v", aerr)
			} else {
				f.WriteString(acStatusLine("race-m2"))
				f.Close()
			}
		}
		return msgs, status, err
	}
	result, err := c.Collect(context.Background(), CollectRequest{}, discardingLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FileStatuses) != 1 {
		t.Fatalf("FileStatuses = %d, want 1", len(result.FileStatuses))
	}
	st := result.FileStatuses[0]
	if st.Err != nil {
		t.Fatalf("status.Err = %v", st.Err)
	}
	if st.Before == st.After {
		t.Fatalf("读窗口内追加必须被快照捕获: before=%+v after=%+v", st.Before, st.After)
	}
	if st.After.Size <= st.Before.Size {
		t.Errorf("读后 size 应大于读前: %+v → %+v", st.Before, st.After)
	}
	// 消息照常产出（成功部分保留语义）。
	if len(result.Messages) != 1 || result.Messages[0].ID != "race-m1" {
		t.Errorf("消息应照常产出: %+v", result.Messages)
	}
	// fullyParsed 状态与快照一致性是两个维度：快照不一致由 engine 写门判定拦截
	//（Before != After 不产门记录），这里锁定 collector 提供了真实证据。
}
