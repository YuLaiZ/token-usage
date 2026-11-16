package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// === Claude agent transcript 的启动跳过门绕过与回填（engine 层） ===
//
// agent transcript 的标题依赖同目录 meta.json（不在门指纹内），且每次启动
// catch-up 都须重读：门判定对 agent 文件恒不命中，scanGateRowsFor 也不为其写
// 门记录——file_scan_log 继续只记录主 transcript。

const (
	gateAgentParent = "11111111-2222-3333-4444-555555555555"
	gateAgentID     = "aaaaaaaaaaaaaaaaa"
)

func writeGateMainJSONL(t *testing.T, path, sessionID, msgID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"cli","cwd":"/tmp/proj","message":{"id":%q,"role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", sessionID, msgID)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGateAgentJSONL(t *testing.T, path, rowSessionID, msgID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"claude-desktop-3p","cwd":"/tmp/proj","message":{"id":%q,"role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", rowSessionID, msgID)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGateAgentMeta(t *testing.T, jsonlPath, description string) string {
	t.Helper()
	metaPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".meta.json"
	if err := os.WriteFile(metaPath, []byte(fmt.Sprintf(`{"description":%q}`, description)), 0o600); err != nil {
		t.Fatal(err)
	}
	return metaPath
}

// claudeGateLogs 返回 claude 的 file_scan_log 路径集合（path → parser_version）。
func claudeGateLogs(t *testing.T, usageDB *db.DB) map[string]int64 {
	t.Helper()
	rows, err := usageDB.Query(`SELECT file_path, parser_version FROM file_scan_log WHERE client='claude'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var path string
		var version int64
		if err := rows.Scan(&path, &version); err != nil {
			t.Fatal(err)
		}
		out[path] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("file_scan_log 查询失败: %v", err)
	}
	return out
}

// sessionTitleAndParent 返回指定会话的 title 与 agent_parent_id。
func sessionTitleAndParent(t *testing.T, usageDB *db.DB, sessionID, client string) (string, string) {
	t.Helper()
	rows, err := usageDB.Query(`SELECT title, agent_parent_id FROM sessions WHERE id=? AND client=?`, sessionID, client)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("sessions 查询失败: %v", err)
		}
		t.Fatalf("sessions 缺少 %s/%s", client, sessionID)
	}
	var title, parent string
	if err := rows.Scan(&title, &parent); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sessions 查询失败: %v", err)
	}
	return title, parent
}

// 存量空值回填：升级前已存在的 agent 会话行（title/agent_parent_id 为空）
// 经 catch-up 重读后被非空值覆盖（升级补采的 DB 层直接断言）。
func TestScanGate_ClaudeAgentEmptyHistoricalRowBackfilled(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	agentJSONL := filepath.Join(dir, "-proj", gateAgentParent, "subagents", "agent-"+gateAgentID+".jsonl")
	writeGateAgentJSONL(t, agentJSONL, gateAgentParent, "agent-msg")
	writeGateAgentMeta(t, agentJSONL, "存量回填标题")

	// 模拟旧版本留下的存量行：仅主键，标题与父归属为空。
	if _, err := usageDB.Exec(`INSERT INTO sessions (id, client) VALUES (?, ?)`, "agent-"+gateAgentID, "Claude Desktop"); err != nil {
		t.Fatal(err)
	}
	if title, parent := sessionTitleAndParent(t, usageDB, "agent-"+gateAgentID, "Claude Desktop"); title != "" || parent != "" {
		t.Fatalf("前置存量行 = %q/%q, want 空", title, parent)
	}

	if res := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !res.Complete() {
		t.Fatalf("采集未完成: %+v", res)
	}
	if title, parent := sessionTitleAndParent(t, usageDB, "agent-"+gateAgentID, "Claude Desktop"); title != "存量回填标题" || parent != gateAgentParent {
		t.Fatalf("存量行回填 = %q/%q, want 存量回填标题/%q", title, parent, gateAgentParent)
	}
}

// 门记录：agent 文件绕过启动跳过门——首轮无 meta 采集后不写门记录；只写 meta
// （JSONL 不变）后下一轮 catch-up 重读并补齐标题；删除 meta 后再采集，已有非空
// 标题不被空值覆盖。
func TestScanGate_ClaudeAgentBypassesGateAndBackfillsTitle(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	mainJSONL := filepath.Join(dir, "-proj", gateAgentParent+".jsonl")
	writeGateMainJSONL(t, mainJSONL, gateAgentParent, "main-msg")
	agentJSONL := filepath.Join(dir, "-proj", gateAgentParent, "subagents", "agent-"+gateAgentID+".jsonl")
	writeGateAgentJSONL(t, agentJSONL, gateAgentParent, "agent-msg")

	agentSessionID := "agent-" + gateAgentID

	// 第一轮：无 meta，agent 文件被采集但标题为空。
	if res := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !res.Complete() {
		t.Fatalf("第一轮采集未完成: %+v", res)
	}
	if title, parent := sessionTitleAndParent(t, usageDB, agentSessionID, "Claude Desktop"); title != "" || parent != gateAgentParent {
		t.Fatalf("第一轮 agent 会话 title=%q parent=%q, want 空/%q", title, parent, gateAgentParent)
	}
	logs := claudeGateLogs(t, usageDB)
	if _, ok := logs[agentJSONL]; ok {
		t.Fatalf("agent transcript 不得写 file_scan_log 记录: %v", logs)
	}
	if logs[mainJSONL] != int64(db.ParserVersion) {
		t.Fatalf("主 transcript 门记录版本 = %d, want %d", logs[mainJSONL], db.ParserVersion)
	}

	// 第二轮：JSONL 不变，只写 meta——agent 绕过门重读，标题入库；主 transcript 命中门。
	writeGateAgentMeta(t, agentJSONL, "晚到标题")
	handler := &levelCaptureHandler{}
	if res := runGateCollect(t, deps, usageDB, handler); !res.Complete() {
		t.Fatalf("第二轮采集未完成: %+v", res)
	}
	if n := handler.gateSkippedCount(); n != 1 {
		t.Fatalf("第二轮门命中跳过数 = %d, want 1（主 transcript 命中、agent 绕过）", n)
	}
	if title, parent := sessionTitleAndParent(t, usageDB, agentSessionID, "Claude Desktop"); title != "晚到标题" || parent != gateAgentParent {
		t.Fatalf("第二轮 agent 会话 title=%q parent=%q, want 晚到标题/%q", title, parent, gateAgentParent)
	}
	if _, ok := claudeGateLogs(t, usageDB)[agentJSONL]; ok {
		t.Fatalf("第二轮后 agent transcript 仍不得写门记录")
	}

	// 第三轮：删除 meta 再 catch-up——agent 重读产出空标题，库中已有非空标题保留。
	metaPath := strings.TrimSuffix(agentJSONL, ".jsonl") + ".meta.json"
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if res := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !res.Complete() {
		t.Fatalf("第三轮采集未完成: %+v", res)
	}
	if title, _ := sessionTitleAndParent(t, usageDB, agentSessionID, "Claude Desktop"); title != "晚到标题" {
		t.Fatalf("空标题不得覆盖已有非空标题, got %q", title)
	}
}

// dupMessageStats 返回指定 message id 的行数与 token 总和；读完即关闭 rows
// （:memory: 库单连接，残留 rows 会让后续查询死等连接）。
func dupMessageStats(t *testing.T, usageDB *db.DB, messageID string) (count, total int64) {
	t.Helper()
	rows, err := usageDB.Query(`SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM messages WHERE id=?`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("messages 查询失败: %v", err)
		}
		t.Fatal("messages 查询无结果")
	}
	if err := rows.Scan(&count, &total); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("messages 查询失败: %v", err)
	}
	return count, total
}

// 跨父目录同内容副本按 message.id 幂等 upsert 只计一次用量，父归属取行内
// sessionId（不是复制副本所在目录），标题由带 meta 的副本补齐，与 ChangedFile
// 采集顺序无关。
func TestScanGate_ClaudeAgentDuplicateCopiesCountOnce(t *testing.T) {
	msgLine := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"claude-desktop-3p","cwd":"/tmp/proj","message":{"id":"dup-msg","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", gateAgentParent)
	for _, order := range [][]string{{"A", "B"}, {"B", "A"}} {
		t.Run("先"+strings.Join(order, "后"), func(t *testing.T) {
			deps, usageDB, dir := newClaudeGateDeps(t)
			copyA := filepath.Join(dir, "-proj", gateAgentParent, "subagents", "agent-"+gateAgentID+".jsonl")
			copyB := filepath.Join(dir, "-proj", "66666666-7777-8888-9999-aaaaaaaaaaaa", "subagents", "agent-"+gateAgentID+".jsonl")
			paths := map[string]string{"A": copyA, "B": copyB}
			for _, p := range []string{copyA, copyB} {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(msgLine), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeGateAgentMeta(t, copyA, "重复副本代理")

			for _, key := range order {
				res := RunCollect(context.Background(), deps, usageDB, collectTestLogger(), io.Discard,
					"claude", collector.CollectRequest{ChangedFile: paths[key]}, false, false)
				if !res.Complete() {
					t.Fatalf("ChangedFile %s 采集未完成: %+v", key, res)
				}
			}

			count, total := dupMessageStats(t, usageDB, "dup-msg")
			if count != 1 || total != 15 {
				t.Fatalf("dup-msg count=%d total=%d, want 1/15（副本只计一次用量）", count, total)
			}
			title, parent := sessionTitleAndParent(t, usageDB, "agent-"+gateAgentID, "Claude Desktop")
			if parent != gateAgentParent {
				t.Fatalf("父归属 = %q, want 行内 %q（与副本路径无关）", parent, gateAgentParent)
			}
			if title != "重复副本代理" {
				t.Fatalf("标题 = %q, want 带 meta 副本的 description", title)
			}
		})
	}
}

// messageAgentParent 返回指定消息的 agent_parent_id。
func messageAgentParent(t *testing.T, usageDB *db.DB, messageID string) string {
	t.Helper()
	rows, err := usageDB.Query(`SELECT agent_parent_id FROM messages WHERE id=? AND client='Claude Desktop'`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("messages 查询失败: %v", err)
		}
		t.Fatalf("messages 缺少 %s", messageID)
	}
	var parent string
	if err := rows.Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("messages 查询失败: %v", err)
	}
	return parent
}

// 存量 messages 行的 agent_parent_id 为空：catch-up 重读同一消息后由非空值回填
// （消息级 upsert 的「非空覆盖、空保留」语义）。
func TestScanGate_ClaudeAgentHistoricalMessageParentBackfilled(t *testing.T) {
	requireGateIdentityEnv(t)
	deps, usageDB, dir := newClaudeGateDeps(t)
	agentJSONL := filepath.Join(dir, "-proj", gateAgentParent, "subagents", "agent-"+gateAgentID+".jsonl")
	writeGateAgentJSONL(t, agentJSONL, gateAgentParent, "agent-msg")

	// 模拟旧版采集留下的消息行：agent_parent_id 为空。
	if _, err := usageDB.Exec(
		`INSERT INTO messages (id, session_id, client, date, ts) VALUES ('agent-msg', ?, 'Claude Desktop', '2026-07-08', 1783552800000)`,
		"agent-"+gateAgentID,
	); err != nil {
		t.Fatal(err)
	}
	if parent := messageAgentParent(t, usageDB, "agent-msg"); parent != "" {
		t.Fatalf("前置消息父归属 = %q, want 空", parent)
	}

	if res := runGateCollect(t, deps, usageDB, &levelCaptureHandler{}); !res.Complete() {
		t.Fatalf("采集未完成: %+v", res)
	}
	if parent := messageAgentParent(t, usageDB, "agent-msg"); parent != gateAgentParent {
		t.Fatalf("存量消息父归属 = %q, want %q", parent, gateAgentParent)
	}
}
