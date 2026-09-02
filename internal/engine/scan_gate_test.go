package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === startup 跳过门（file_scan_log）相关测试 ===
//
// 入口负向锚点：非 catch-up 入口的请求不得携带 ScanExistingJSONL（该标志是 daemon
// startup catch-up 现存 JSONL 全扫的显式合同，跳过门只作用于该路径）。

// scanGateCapturingCollector 捕获每次 Collect 收到的完整请求。
type scanGateCapturingCollector struct {
	name     string
	result   collector.CollectResult
	mu       sync.Mutex
	requests []collector.CollectRequest
}

func (c *scanGateCapturingCollector) Name() string { return c.name }
func (c *scanGateCapturingCollector) SyncSources() []string {
	return nil
}
func (c *scanGateCapturingCollector) Collect(_ context.Context, req collector.CollectRequest, _ *slog.Logger) (collector.CollectResult, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return c.result, nil
}

// TestRunCollect_NonCatchUpRequestsNeverCarryScanExistingJSONL：
// CLI collect（Dates 多日）、collect all（Dates nil 全扫）、collect retry（Dates 单日）
// 三种入口形态经 RunCollect 透传给 collector 的请求必须不带 ScanExistingJSONL——
// 这是跳过门「仅 catch-up 路径」作用域的入口侧守卫（门侧守卫见门逻辑测试）。
func TestRunCollect_NonCatchUpRequestsNeverCarryScanExistingJSONL(t *testing.T) {
	forms := []struct {
		name string
		req  collector.CollectRequest
	}{
		{"cli collect dates", collector.CollectRequest{Dates: []string{"2026-09-01", "2026-09-02"}}},
		{"collect all", collector.CollectRequest{Dates: nil}},
		{"collect retry", collector.CollectRequest{Dates: []string{"2026-09-01"}}},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			usageDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer usageDB.Close()
			c := &scanGateCapturingCollector{name: "claude", result: collector.CollectResult{}}
			result := RunCollect(context.Background(), testDeps(true, c), usageDB,
				collectTestLogger(), io.Discard, "claude", form.req, false, false)
			if !result.Complete() {
				t.Fatalf("result = %+v", result)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.requests) != 1 {
				t.Fatalf("collector calls = %d, want 1", len(c.requests))
			}
			if c.requests[0].ScanExistingJSONL {
				t.Errorf("collector received ScanExistingJSONL=true; non catch-up entry must never carry the flag")
			}
		})
	}
}

// === 门逻辑（仅 ScanExistingJSONL 路径）端到端 ===

// claudeGateFixture 构造真实 ClaudeCollector（指向 tmpdir projects_dir）+ 内存 usage DB。
const gateFixtureJSONL = `{"type":"user","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/project","message":{"id":"u-1","role":"user"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:01:00+08:00","cwd":"/tmp/project","message":{"id":"msg-1","role":"assistant","model":"model-a","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":20}}}
`

func newClaudeGateDeps(t *testing.T) (*Deps, *db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": dir}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewClaudeCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usageDB.Close() })
	return deps, usageDB, dir
}

func gateMessages(t *testing.T, usageDB *db.DB) []model.Message {
	t.Helper()
	rows, err := usageDB.Query(`SELECT id FROM messages WHERE client='Claude Code' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func runGateCollect(t *testing.T, deps *Deps, usageDB *db.DB, handler *levelCaptureHandler) Result {
	t.Helper()
	return RunCollect(context.Background(), deps, usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
		false, false)
}

// requireGateIdentityEnv 在当前环境不提供文件实体标识时跳过测试。本文件的门
// 行为测试断言门在真实文件系统上推进 / 命中 / 因特定原因不推进，前提是
// fsident 能给出有效 identity（项目支持平台 darwin / windows 的可靠文件系统）；
// 其余平台与不可靠文件系统上 fsident 按设计恒不给 identity（门禁用、每次全读），
// 这些断言会被击穿或退化为恒真，一律 Skip 注明，不在无区分度的环境产生假信号。
// 支持平台上「真实文件快照 identity 必须有效」的产品级守卫由 fsident 包的测试
// 承担（那里在支持平台保持 Fatal），本探测只做测试前提声明。
func requireGateIdentityEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.jsonl")
	if err := os.WriteFile(probe, []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fsident.Valid(fsident.SnapshotOfFile(probe)) {
		t.Skipf("当前平台/文件系统不提供文件实体标识（跳过门禁用设计），门行为断言前提不成立: GOOS=%s", runtime.GOOS)
	}
}

// TestScanGate_FirstRoundWritesGate：首轮 catch-up 全读并写门（记录三元组与文件实际快照一致）。
func TestScanGate_FirstRoundWritesGate(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	file := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(file, []byte(gateFixtureJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &levelCaptureHandler{}
	result := runGateCollect(t, deps, usageDB, handler)
	if !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	logs, err := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := logs[file]
	if !ok {
		t.Fatalf("首轮应写门记录: %+v", logs)
	}
	snap := fsident.SnapshotOfFile(file)
	if rec.FileIdentity != snap.Identity || rec.MtimeNS != snap.MtimeNS || rec.FileSize != snap.Size {
		t.Fatalf("门记录三元组与文件实际不符: rec=%+v snap=%+v", rec, snap)
	}
	if rec.ParserVersion != int64(db.ParserVersion) {
		t.Fatalf("ParserVersion = %d, want %d", rec.ParserVersion, db.ParserVersion)
	}
}

// TestScanGate_SecondRoundSkipsUnchangedFile：未变化文件二次 catch-up 门命中跳过
// （Debug 日志 skipped 计数 + DB 消息不变）。
func TestScanGate_SecondRoundSkipsUnchangedFile(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	file := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(file, []byte(gateFixtureJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	firstMsgs := gateMessages(t, usageDB)

	handler := &levelCaptureHandler{}
	if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
		t.Fatalf("second round failed: %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
		t.Error("二次 catch-up 应有门命中跳过日志")
	}
	if got := gateMessages(t, usageDB); len(got) != len(firstMsgs) {
		t.Fatalf("门命中跳过后 DB 消息应不变: %v vs %v", got, firstMsgs)
	}
	// 不比对 updated_at：datetime('now') 秒级精度下两轮同秒执行时无区分度，
	// 主断言（跳过日志 + 消息不变）已足够。
}

// TestScanGate_AppendMissesGateAndReReads（可检测类）：追加内容后 size 变 → 门失效
// 全读 → 新消息入库且门推进到新三元组。
func TestScanGate_AppendMissesGateAndReReads(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	file := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(file, []byte(gateFixtureJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	// 追加一条新 assistant 消息（size/mtime 均变）。
	appendLine := `{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T11:00:00+08:00","cwd":"/tmp/project","message":{"id":"msg-2","role":"assistant","model":"model-a","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appendLine); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("second round failed: %+v", result)
	}
	msgs := gateMessages(t, usageDB)
	if len(msgs) != 2 {
		t.Fatalf("追加后应全读出 2 条消息, got %v", msgs)
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	snap := fsident.SnapshotOfFile(file)
	if logs[file].FileSize != snap.Size || logs[file].MtimeNS != snap.MtimeNS {
		t.Fatalf("门应推进到新三元组: rec=%+v snap=%+v", logs[file], snap)
	}
}

// TestScanGate_BadLinesFileNeverGated：含坏行文件不写门（每次 catch-up 全读）。
func TestScanGate_BadLinesFileNeverGated(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	bad := "not-json\n" + gateFixtureJSONL
	if err := os.WriteFile(filepath.Join(dir, "bad.jsonl"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.jsonl"), []byte(gateFixtureJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	logs, err := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := logs[filepath.Join(dir, "bad.jsonl")]; ok {
		t.Error("含坏行文件不得写门记录")
	}
	if _, ok := logs[filepath.Join(dir, "good.jsonl")]; !ok {
		t.Error("同批好文件应正常写门（坏文件不拖累好文件）")
	}
}

// TestScanGate_NoTrailingNewlineNotGated：尾行无 \n 终结的文件不写门。
func TestScanGate_NoTrailingNewlineNotGated(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	if err := os.WriteFile(filepath.Join(dir, "tail.jsonl"), []byte(gateFixtureJSONL[:len(gateFixtureJSONL)-1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if len(logs) != 0 {
		t.Fatalf("尾行未终结文件不得写门: %+v", logs)
	}
}

// TestScanGate_NonCatchUpEntriesNeverTouchGate（入口闭合守卫）：
// CLI Dates / collect all（Dates nil） / ChangedFile 三形态下不读门不写门
// （多次采集后 file_scan_log 恒空、且无门命中跳过日志）。
func TestScanGate_NonCatchUpEntriesNeverTouchGate(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(gateFixtureJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	// 五入口（Dates / --force / retry / collect all / ChangedFile）的行为级断言：
	// --force 与 CLI Dates 仅 skipCollected 不同（请求形态相同）、retry 为 Dates 单日形态。
	forms := []struct {
		name string
		req  collector.CollectRequest
	}{
		{"cli dates", collector.CollectRequest{Dates: []string{"2026-07-08"}}},
		{"collect --force (same request shape as dates)", collector.CollectRequest{Dates: []string{"2026-07-08"}}},
		{"collect retry (single date)", collector.CollectRequest{Dates: []string{"2026-07-08"}}},
		{"collect all", collector.CollectRequest{Dates: nil}},
		{"changed file", collector.CollectRequest{ChangedFile: filepath.Join(dir, "s1.jsonl")}},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			handler := &levelCaptureHandler{}
			result := RunCollect(context.Background(), deps, usageDB,
				slog.New(handler), io.Discard, "claude", form.req, false, false)
			if !result.Complete() {
				t.Fatalf("%s failed: %+v", form.name, result)
			}
			if handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
				t.Errorf("%s 入口不得命中门", form.name)
			}
			logs, err := db.GetFileScanLogs(context.Background(), usageDB, "claude")
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 0 {
				t.Errorf("%s 入口不得写门: %+v", form.name, logs)
			}
		})
	}
}

// TestScanGateRowsFor_OnlyConsistentFullyParsed：写门行生成的纯函数判定——
// 快照不一致（读期间追加/替换）、identity 无效、含坏行、文件级错误、Skipped
// 的文件一律不产生门记录（门推进条件的文件级实现）。
func TestScanGateRowsFor_OnlyConsistentFullyParsed(t *testing.T) {
	base := collector.FileScanStatus{
		Path:            "/p/a.jsonl",
		Before:          model.FileSnapshot{Identity: "1:2", MtimeNS: 100, Size: 10},
		After:           model.FileSnapshot{Identity: "1:2", MtimeNS: 100, Size: 10},
		TrailingNewline: true,
	}
	cases := []struct {
		name string
		mut  func(*collector.FileScanStatus)
		want bool
	}{
		{"normal", func(*collector.FileScanStatus) {}, true},
		{"snapshot mtime changed", func(s *collector.FileScanStatus) { s.After.MtimeNS = 200 }, false},
		{"snapshot size changed", func(s *collector.FileScanStatus) { s.After.Size = 20 }, false},
		{"snapshot identity changed", func(s *collector.FileScanStatus) { s.After.Identity = "9:9" }, false},
		{"identity unavailable", func(s *collector.FileScanStatus) {
			s.Before.Identity = ""
			s.After.Identity = ""
		}, false},
		{"bad lines", func(s *collector.FileScanStatus) { s.BadLines = 1 }, false},
		{"no trailing newline", func(s *collector.FileScanStatus) { s.TrailingNewline = false }, false},
		{"file error", func(s *collector.FileScanStatus) { s.Err = io.EOF }, false},
		{"gate skipped", func(s *collector.FileScanStatus) { s.Skipped = true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := base
			tc.mut(&st)
			rows := scanGateRowsFor("claude", []collector.FileScanStatus{st})
			if tc.want && len(rows) != 1 {
				t.Fatalf("应产生门记录, got %d", len(rows))
			}
			if !tc.want && len(rows) != 0 {
				t.Fatalf("不得产生门记录, got %+v", rows)
			}
		})
	}
}

// === 门跳过正确性与数据一致性 ===

const wbGateFixture = `{"id":"wb-1","timestamp":1749312060000,"type":"message","role":"assistant","content":[],"providerData":{"model":"m","usage":{"inputTokens":1500,"outputTokens":800,"inputTokensDetails":[{"cached_tokens":1200}],"outputTokensDetails":[]}},"sessionId":"sess-001","cwd":"/path"}
`

func acGateLine(id string) string {
	return `{"type":"session","cwd":"/tmp/project"}` + "\n" +
		`{"type":"message","id":"` + id + `","timestamp":"2026-07-29T12:00:00.000Z","message":{"role":"assistant","provider":"zai","model":"zai_auto","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"reasoningTokens":0,"total":15},"stopReason":"endTurn","timestamp":1753788000000,"responseId":"resp-x"}}` + "\n"
}

const codexGateFixture = `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","originator":"Codex CLI","source":"cli","cwd":"/tmp/project"}}
{"timestamp":"2026-07-11T14:27:00Z","type":"turn_context","payload":{"model":"gpt-5"}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-c1"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}
`

// dumpRows 把查询结果拼成可比较的行集字符串（内容一致性断言用）。
func dumpRows(t *testing.T, usageDB *db.DB, query string) string {
	t.Helper()
	rows, err := usageDB.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if raw, ok := v.([]byte); ok {
				b.Write(raw)
			} else {
				fmt.Fprintf(&b, "%v", v)
			}
			b.WriteByte('|')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestScanGate_ContentConsistencyFourFormats：四格式 fixture 在
// 「有门（第二轮命中跳过）」与「无门（每轮清门表恒全读）」两库下
// messages/sessions 行集完全一致。其中 claude/codex 验证门路径一致性；
// workbuddy/autoclaw 不参与门（provider 依赖 models.json 映射，映射变化不改变
// 文件证据，故双侧均无门），该两格式退化为全读幂等基线（两次全读 vs 清表再全读）。
func TestScanGate_ContentConsistencyFourFormats(t *testing.T) {
	requireGateIdentityEnv(t)
	type formatCase struct {
		name    string
		client  string
		setup   func(t *testing.T, root string) // 在 root 下布置 fixture
		paths   map[string]string
		display string // messages.client 显示名
	}
	formats := []formatCase{
		{"claude", "claude", func(t *testing.T, root string) {
			writeFileT(t, filepath.Join(root, "PROJECTS", "s1.jsonl"), gateFixtureJSONL)
		}, map[string]string{"projects_dir": "PROJECTS"}, "Claude Code"},
		{"workbuddy", "workbuddy", func(t *testing.T, root string) {
			// workbuddy projects_dir 结构为 {projectName}/*.jsonl 两层。
			writeFileT(t, filepath.Join(root, "PROJECTS", "proj1", "sess-001.jsonl"), wbGateFixture)
		}, map[string]string{"projects_dir": "PROJECTS"}, "WorkBuddy"},
		{"autoclaw", "autoclaw", func(t *testing.T, root string) {
			writeFileT(t, filepath.Join(root, "sessions", "main", "sessions", "aaa.jsonl"), acGateLine("aaa-m1"))
		}, map[string]string{"sessions_dir": "sessions"}, "Zhipu-AutoClaw"},
		{"codex", "codex", func(t *testing.T, root string) {
			writeFileT(t, filepath.Join(root, "rollouts", "r1.jsonl"), codexGateFixture)
		}, map[string]string{"sessions_dir": "rollouts"}, "Codex CLI"},
	}
	for _, fc := range formats {
		t.Run(fc.name, func(t *testing.T) {
			root := t.TempDir()
			fc.setup(t, root)
			paths := map[string]string{}
			for k, v := range fc.paths {
				paths[k] = filepath.Join(root, v)
			}
			newDeps := func() *Deps {
				cfg := &config.Config{Clients: map[string]config.Client{
					fc.client: {Enabled: true, Paths: paths},
				}}
				var c collector.Collector
				switch fc.client {
				case "claude":
					c = collector.NewClaudeCollector(cfg)
				case "workbuddy":
					c = collector.NewWorkBuddyCollector(cfg)
				case "autoclaw":
					c = collector.NewAutoClawCollector(cfg)
				case "codex":
					c = collector.NewCodexCollector(cfg)
				}
				return NewDepsWithCollectors(cfg, []collector.Collector{c}, nil)
			}
			runCatchUp := func(deps *Deps, usageDB *db.DB) {
				result := RunCollect(context.Background(), deps, usageDB,
					slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, fc.client,
					collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
					false, false)
				if !result.Complete() {
					t.Fatalf("catch-up failed: %+v", result)
				}
			}
			// 有门库：首轮全读写门 → 二轮命中跳过。
			gatedDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer gatedDB.Close()
			gatedDeps := newDeps()
			runCatchUp(gatedDeps, gatedDB)
			runCatchUp(gatedDeps, gatedDB)
			// 无门库：每轮清门表（恒全读；模拟无门构建）。
			ungatedDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer ungatedDB.Close()
			ungatedDeps := newDeps()
			runCatchUp(ungatedDeps, ungatedDB)
			if _, err := ungatedDB.Exec("DELETE FROM file_scan_log"); err != nil {
				t.Fatal(err)
			}
			runCatchUp(ungatedDeps, ungatedDB)

			msgQuery := `SELECT id, session_id, client, date, ts, model, provider, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens FROM messages ORDER BY id`
			sessQuery := `SELECT id, client, directory, project, first_ts, last_ts FROM sessions ORDER BY id`
			if got, want := dumpRows(t, gatedDB, msgQuery), dumpRows(t, ungatedDB, msgQuery); got != want {
				t.Errorf("messages 行集不一致（有门 vs 无门）:\ngated:\n%s\nungated:\n%s", got, want)
			}
			if got, want := dumpRows(t, gatedDB, sessQuery), dumpRows(t, ungatedDB, sessQuery); got != want {
				t.Errorf("sessions 行集不一致（有门 vs 无门）:\ngated:\n%s\nungated:\n%s", got, want)
			}
			var n int
			if err := gatedDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatalf("%s: 采到 0 条消息，fixture 无效", fc.name)
			}
		})
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScanGate_DetectableReplacementsInvalidateGate（可检测类）：
// 原子替换类变化（mv 顶替、删除重建、截断、touch 回拨 mtime）即使恢复 size 与
// mtime 也必失效重读（file_identity 变化；mv/删除重建换 inode），且结果与全量一致。
func TestScanGate_DetectableReplacementsInvalidateGate(t *testing.T) {
	requireGateIdentityEnv(t)
	// wantGateUpdated：变化后重读且文件仍 fullyParsed 时，门记录应更新为新证据。
	// 截断类变化产生坏尾行（不 fullyParsed），门记录保持旧值是预期安全行为。
	mkCase := func(name string, wantGateUpdated bool, mutate func(t *testing.T, file string)) func(t *testing.T) {
		return func(t *testing.T) {
			deps, usageDB, dir := newClaudeGateDeps(t)
			file := filepath.Join(dir, "s1.jsonl")
			writeFileT(t, file, gateFixtureJSONL)
			if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
				t.Fatalf("first round failed: %+v", result)
			}
			before := fsident.SnapshotOfFile(file)
			beforeMsgs := gateMessages(t, usageDB)

			mutate(t, file)

			handler := &levelCaptureHandler{}
			if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
				t.Fatalf("second round failed: %+v", result)
			}
			if handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
				t.Errorf("%s: 门必须失效（不得跳过）", name)
			}
			if got := gateMessages(t, usageDB); len(got) != len(beforeMsgs) {
				t.Errorf("%s: 重读后消息应与全量一致: %v vs %v", name, got, beforeMsgs)
			}
			if wantGateUpdated {
				logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
				if logs[file].FileIdentity == before.Identity {
					t.Errorf("%s: 门记录应更新为新文件证据", name)
				}
			}
		}
	}
	// mv 顶替：新 inode，恢复原 mtime（size 相同内容）。
	t.Run("mv replace restores mtime", mkCase("mv 顶替", true, func(t *testing.T, file string) {
		st := fsident.SnapshotOfFile(file)
		tmp := file + ".new"
		writeFileT(t, tmp, gateFixtureJSONL)
		if err := os.Rename(tmp, file); err != nil {
			t.Fatal(err)
		}
		restoreMtime(t, file, st)
	}))
	// 删除重建：新 inode，恢复原 mtime。
	t.Run("remove and recreate restores mtime", mkCase("删除重建", true, func(t *testing.T, file string) {
		st := fsident.SnapshotOfFile(file)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		writeFileT(t, file, gateFixtureJSONL)
		restoreMtime(t, file, st)
	}))
	// 截断：size 变（截断后内容变少）。
	t.Run("truncate changes size", mkCase("截断", false, func(t *testing.T, file string) {
		if err := os.Truncate(file, int64(len(gateFixtureJSONL)/2)); err != nil {
			t.Fatal(err)
		}
	}))
	// touch 回拨 mtime：identity/size 不变，mtime 变。
	t.Run("touch rewinds mtime", mkCase("mtime 回拨", false, func(t *testing.T, file string) {
		st := fsident.SnapshotOfFile(file)
		old := time.Unix(0, st.MtimeNS-3600*1e9)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}))
}

func restoreMtime(t *testing.T, file string, snap model.FileSnapshot) {
	t.Helper()
	when := time.Unix(0, snap.MtimeNS)
	if err := os.Chtimes(file, when, when); err != nil {
		t.Fatal(err)
	}
}

// TestScanGate_InPlaceOverwriteBehaviorLock（不可检测类，行为锁定）：
// 原地截断覆盖（cp 覆盖/truncate+重写，inode 不变）+ 同长度 + 恢复 mtime 时
// file_identity 无法检测——门命中跳过是**已知漏采边界（原地截断覆盖
// 不可检测的登记残余风险，留评审裁决）**，本测试只锁定该行为，严禁改写为「必失效」断言。
//   - 同内容覆盖 → 命中跳过（无损）；
//   - 同长度不同内容 + 恢复 mtime → 仍命中跳过 = 漏采差异（rsync -a/tar 回滚类）。
func TestScanGate_InPlaceOverwriteBehaviorLock(t *testing.T) {
	requireGateIdentityEnv(t)
	t.Run("same content in-place overwrite hits gate (lossless)", func(t *testing.T) {
		deps, usageDB, dir := newClaudeGateDeps(t)
		file := filepath.Join(dir, "s1.jsonl")
		writeFileT(t, file, gateFixtureJSONL)
		if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
			t.Fatalf("first round failed: %+v", result)
		}
		st := fsident.SnapshotOfFile(file)
		// cp 覆盖：截断重写同内容（inode 不变），恢复 mtime。
		writeFileT(t, file, gateFixtureJSONL)
		restoreMtime(t, file, st)
		after := fsident.SnapshotOfFile(file)
		if after.Identity != st.Identity {
			t.Skipf("本文件系统 cp 覆盖改变了 identity（%q→%q），行为锁定前提不成立", st.Identity, after.Identity)
		}

		handler := &levelCaptureHandler{}
		if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
			t.Fatalf("second round failed: %+v", result)
		}
		if !handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
			t.Error("同内容原地覆盖（三元组一致）应命中门跳过（无损行为锁定）")
		}
	})

	t.Run("same length different content with restored mtime hits gate (KNOWN MISS BOUNDARY)", func(t *testing.T) {
		deps, usageDB, dir := newClaudeGateDeps(t)
		file := filepath.Join(dir, "s1.jsonl")
		writeFileT(t, file, gateFixtureJSONL)
		if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
			t.Fatalf("first round failed: %+v", result)
		}
		st := fsident.SnapshotOfFile(file)
		// 同长度不同内容：把 msg-1 改成 msg-9（字节数相同），cp 类原地截断重写 + 恢复 mtime。
		altered := strings.Replace(gateFixtureJSONL, "msg-1", "msg-9", 1)
		if len(altered) != len(gateFixtureJSONL) {
			t.Fatalf("fixture 改写必须保持等长: %d vs %d", len(altered), len(gateFixtureJSONL))
		}
		writeFileT(t, file, altered)
		restoreMtime(t, file, st)
		if fsident.SnapshotOfFile(file).Identity != st.Identity {
			t.Skip("本文件系统原地覆盖改变了 identity，行为锁定前提不成立")
		}

		handler := &levelCaptureHandler{}
		if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
			t.Fatalf("second round failed: %+v", result)
		}
		// 已知漏采边界：门命中跳过 → msg-9 不会被采集（旧消息保留）。
		// 这是登记的残余风险（原地截断覆盖不可检测），留评审裁决；
		// 本断言锁定现状行为，防止被误改为「必失效」的假验收标准。
		if !handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
			t.Error("等长原地覆盖 + 恢复 mtime 按设计命中门跳过（已知漏采边界）")
		}
		if msgs := gateMessages(t, usageDB); len(msgs) != 1 || msgs[0].ID != "msg-1" {
			t.Errorf("漏采边界行为漂移: %v（应保留旧 msg-1）", msgs)
		}
	})
}

// TestScanGate_BadLineRecovery：修复坏行（文件变化）后重读恢复，
// 该文件重新获得门资格。
func TestScanGate_BadLineRecovery(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	bad := filepath.Join(dir, "bad.jsonl")
	writeFileT(t, bad, "not-json\n"+gateFixtureJSONL)
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	// 修复坏行（文件重写，mtime/size 变）。
	writeFileT(t, bad, gateFixtureJSONL)
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("second round failed: %+v", result)
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if _, ok := logs[bad]; !ok {
		t.Fatal("坏行修复后应重读并写门")
	}
	// 第三轮：命中跳过（门资格恢复）。
	handler := &levelCaptureHandler{}
	if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
		t.Fatalf("third round failed: %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
		t.Error("修复后的文件应重新命中门")
	}
}

// TestScanGate_UpsertFailureRollsBackGate（事务原子性红向）：
// 门写入失败（触发器注入）时整个批次事务回滚——消息与门记录都不入库。
func TestScanGate_UpsertFailureRollsBackGate(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	writeFileT(t, filepath.Join(dir, "s1.jsonl"), gateFixtureJSONL)
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_gate_insert BEFORE INSERT ON file_scan_log BEGIN SELECT RAISE(ABORT, 'injected gate failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{})
	if result.Complete() {
		t.Fatal("门写入注入失败时批次必须失败")
	}
	if msgs := gateMessages(t, usageDB); len(msgs) != 0 {
		t.Fatalf("事务回滚后消息不得残留: %v", msgs)
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if len(logs) != 0 {
		t.Fatalf("事务回滚后门记录不得残留: %+v", logs)
	}
}

// TestScanGate_IdempotentAcrossRounds：同一目录连续 N 轮 catch-up
// 消息总数不变（门命中跳过与幂等 upsert 共同保证）。
func TestScanGate_IdempotentAcrossRounds(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	writeFileT(t, filepath.Join(dir, "s1.jsonl"), gateFixtureJSONL)
	writeFileT(t, filepath.Join(dir, "s2.jsonl"), strings.Replace(gateFixtureJSONL, "msg-1", "msg-b", 1))
	want := -1
	for i := 0; i < 3; i++ {
		if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
			t.Fatalf("round %d failed: %+v", i, result)
		}
		n := len(gateMessages(t, usageDB))
		if want == -1 {
			want = n
		} else if n != want {
			t.Fatalf("round %d: messages = %d, want %d", i, n, want)
		}
	}
}

// TestScanGate_ParserVersionInvalidatesTable：门记录 parser_version
// 与当前常量不一致时整表失效（永不命中，全部重读），重读后版本回到当前值。
func TestScanGate_ParserVersionInvalidatesTable(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	file := filepath.Join(dir, "s1.jsonl")
	writeFileT(t, file, gateFixtureJSONL)
	if result := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !result.Complete() {
		t.Fatalf("first round failed: %+v", result)
	}
	// 模拟解析器升级：门记录版本与当前常量不一致。
	if _, err := usageDB.Exec(`UPDATE file_scan_log SET parser_version = parser_version + 100`); err != nil {
		t.Fatal(err)
	}
	handler := &levelCaptureHandler{}
	if result := runGateCollect(t, deps, usageDB, handler); !result.Complete() {
		t.Fatalf("second round failed: %+v", result)
	}
	if handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
		t.Error("版本不匹配的门记录必须失效（不得跳过）")
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if logs[file].ParserVersion != int64(db.ParserVersion) {
		t.Errorf("重读后门版本应回到当前值 %d, got %d", db.ParserVersion, logs[file].ParserVersion)
	}
}

// TestScanGate_IdentityUnavailableDisablesGate：identity 获取失败/
// 无效（注入模拟网络盘/FAT 形态）→ 该文件不推进门、每轮全读；消息照常入库。
func TestScanGate_IdentityUnavailableDisablesGate(t *testing.T) {
	orig := fsident.SnapshotOfFile
	fsident.SnapshotOfFile = func(path string) model.FileSnapshot {
		fi, err := os.Stat(path)
		if err != nil {
			return model.FileSnapshot{}
		}
		// identity 恒为空：模拟 identity 不可用的文件系统形态。
		return model.FileSnapshot{MtimeNS: fi.ModTime().UnixNano(), Size: fi.Size()}
	}
	t.Cleanup(func() { fsident.SnapshotOfFile = orig })

	deps, usageDB, dir := newClaudeGateDeps(t)
	writeFileT(t, filepath.Join(dir, "s1.jsonl"), gateFixtureJSONL)
	for i := 0; i < 2; i++ {
		handler := &levelCaptureHandler{}
		result := runGateCollect(t, deps, usageDB, handler)
		if !result.Complete() {
			t.Fatalf("round %d failed: %+v", i, result)
		}
		if handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
			t.Errorf("round %d: identity 不可用文件不得命中门", i)
		}
	}
	logs, _ := db.GetFileScanLogs(context.Background(), usageDB, "claude")
	if len(logs) != 0 {
		t.Errorf("identity 不可用文件不得写门: %+v", logs)
	}
	if n := len(gateMessages(t, usageDB)); n != 1 {
		t.Errorf("全读消息应正常入库: %d", n)
	}
}

// TestScanGate_PathFormChangeMisses（路径形态，macOS 大小写不敏感卷）：
// config 路径大小写变化 → Walk 产出路径形态变化 → 门记录查不到 → miss 全读（安全方向）。
func TestScanGate_PathFormChangeMisses(t *testing.T) {
	records := map[string]model.FileScanLog{
		"/data/proj/S1.jsonl": {Client: "claude", FilePath: "/data/proj/S1.jsonl", FileIdentity: "1:1", MtimeNS: 1, FileSize: 1, ParserVersion: 1},
	}
	gate := newScanGate(records)
	snap := model.FileSnapshot{Identity: "1:1", MtimeNS: 1, Size: 1}
	if !gate("/data/proj/S1.jsonl", snap) {
		t.Fatal("同形态路径应命中（前置校验）")
	}
	// 大小写变化（大小写不敏感卷上指向同一文件实体，identity 相同）：
	// 路径形态不同 → 门 miss → 全读，安全方向。
	if gate("/data/proj/s1.jsonl", snap) {
		t.Error("路径形态变化（大小写）不得命中门（应 miss 全读）")
	}
}

// TestScanGate_ContentConsistency1MB（分级一致性之 1MB 级，claude 格式代表）：
// 重复行块放大到 1MB 的 fixture 在有门/无门两库下 messages 行集零差异。
// 更大分级（12MB/50MB）在 perf 构建标签下覆盖（scan_gate_perf_test.go）。
func TestScanGate_ContentConsistency1MB(t *testing.T) {
	requireGateIdentityEnv(t)
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(gateFixtureJSONL)
	for b.Len() < 1024*1024 {
		b.WriteString(strings.Replace(gateFixtureJSONL, "msg-1", fmt.Sprintf("filler-%d", b.Len()), 1))
	}
	writeFileT(t, filepath.Join(dir, "PROJECTS", "big.jsonl"), b.String())

	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Join(dir, "PROJECTS")}},
	}}
	newDeps := func() *Deps {
		return NewDepsWithCollectors(cfg, []collector.Collector{collector.NewClaudeCollector(cfg)}, nil)
	}
	runCatchUp := func(deps *Deps, usageDB *db.DB) {
		result := RunCollect(context.Background(), deps, usageDB,
			slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
			false, false)
		if !result.Complete() {
			t.Fatalf("catch-up failed: %+v", result)
		}
	}
	gatedDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer gatedDB.Close()
	gatedDeps := newDeps()
	runCatchUp(gatedDeps, gatedDB)
	runCatchUp(gatedDeps, gatedDB)
	ungatedDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ungatedDB.Close()
	ungatedDeps := newDeps()
	runCatchUp(ungatedDeps, ungatedDB)
	if _, err := ungatedDB.Exec("DELETE FROM file_scan_log"); err != nil {
		t.Fatal(err)
	}
	runCatchUp(ungatedDeps, ungatedDB)

	msgQuery := `SELECT id, session_id, client, date, ts, model, provider, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens FROM messages ORDER BY id`
	if got, want := dumpRows(t, gatedDB, msgQuery), dumpRows(t, ungatedDB, msgQuery); got != want {
		t.Errorf("1MB fixture messages 行集不一致（有门 vs 无门）")
	}
	var n int
	if err := gatedDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 100 {
		t.Fatalf("1MB fixture 消息数 %d 异常偏少", n)
	}
}

// TestScanGate_UnsupportedClientsExcluded：WorkBuddy/AutoClaw 的 provider 依赖
// models.json 映射（映射变化不改变 JSONL 文件证据），不参与跳过门——catch-up
// 请求即使携带 ScanExistingJSONL 也不读门、不写门、不产生跳过（映射纠正依赖
// 全读重放，provider=excluded.provider 仅在重读产出消息时生效）。
func TestScanGate_UnsupportedClientsExcluded(t *testing.T) {
	wbFixture := func(t *testing.T, root string) {
		writeFileT(t, filepath.Join(root, "PROJECTS", "proj1", "sess-001.jsonl"), wbGateFixture)
	}
	acFixture := func(t *testing.T, root string) {
		writeFileT(t, filepath.Join(root, "sessions", "main", "sessions", "aaa.jsonl"), acGateLine("aaa-m1"))
	}
	cases := []struct {
		name   string
		client string
		paths  map[string]string
		setup  func(t *testing.T, root string)
	}{
		{"workbuddy", "workbuddy", map[string]string{"projects_dir": "PROJECTS"}, wbFixture},
		{"autoclaw", "autoclaw", map[string]string{"sessions_dir": "sessions"}, acFixture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			paths := map[string]string{}
			for k, v := range tc.paths {
				paths[k] = filepath.Join(root, v)
			}
			cfg := &config.Config{Clients: map[string]config.Client{
				tc.client: {Enabled: true, Paths: paths},
			}}
			var c collector.Collector
			if tc.client == "workbuddy" {
				c = collector.NewWorkBuddyCollector(cfg)
			} else {
				c = collector.NewAutoClawCollector(cfg)
			}
			deps := NewDepsWithCollectors(cfg, []collector.Collector{c}, nil)
			usageDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer usageDB.Close()

			for i := 0; i < 2; i++ {
				handler := &levelCaptureHandler{}
				result := RunCollect(context.Background(), deps, usageDB,
					slog.New(handler), io.Discard, tc.client,
					collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
					false, false)
				if !result.Complete() {
					t.Fatalf("round %d failed: %+v", i, result)
				}
				if handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
					t.Errorf("round %d: %s 不参与门，不得产生跳过", i, tc.client)
				}
			}
			logs, err := db.GetFileScanLogs(context.Background(), usageDB, tc.client)
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 0 {
				t.Errorf("%s 不参与门，不得写门记录: %+v", tc.client, logs)
			}
			var n int
			if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatalf("%s: 采到 0 条消息，fixture 无效", tc.client)
			}
		})
	}
}
