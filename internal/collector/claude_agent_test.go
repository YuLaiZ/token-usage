package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === Claude 子代理会话（agent transcript）采集测试 ===
//
// agent transcript 形态：<projects>/<编码cwd>/<父sessionId>/subagents/agent-<agentId>.jsonl
// （经典子代理直下，或 workflows/<runId>/ 下）。行内 sessionId 为父会话 UUID，
// 标题来自同目录 agent-<agentId>.meta.json 的 description。

const (
	agentTestParentA = "11111111-2222-3333-4444-555555555555"
	agentTestParentB = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	agentTestAgent1  = "aaaaaaaaaaaaaaaaa"
	agentTestAgent2  = "bbbbbbbbbbbbbbbbb"
)

// writeClaudeAssistantJSONL 写入一条最小可采的 assistant 消息行（带 usage），
// 用于构造主 transcript。
func writeClaudeAssistantJSONL(t *testing.T, path, sessionID, msgID, entrypoint string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":%q,"cwd":"/tmp/proj","message":{"id":%q,"role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", sessionID, entrypoint, msgID)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeAgentJSONL 写入 agent transcript：每行 sessionId 为父会话 UUID
// （rowSessionID），带 isSidechain 与 agentId，message.id 逐条产出。
func writeClaudeAgentJSONL(t *testing.T, path, rowSessionID string, msgIDs ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, id := range msgIDs {
		b.WriteString(fmt.Sprintf(`{"type":"assistant","sessionId":%q,"isSidechain":true,"agentId":%q,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"claude-desktop-3p","cwd":"/tmp/proj","message":{"id":%q,"role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`, rowSessionID, filepath.Base(path), id) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeAgentMeta 写入 agent 同目录 meta.json（description 为标题来源），
// 返回 meta 路径。
func writeClaudeAgentMeta(t *testing.T, jsonlPath, description string) string {
	t.Helper()
	metaPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".meta.json"
	data := fmt.Sprintf(`{"description":%q,"agentType":"general-purpose","toolUseId":"toolu_test"}`, description)
	if err := os.WriteFile(metaPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return metaPath
}

// claudeAgentJSONL 返回项目根下某父会话某 agent 的 transcript 路径。
func claudeAgentJSONL(projectsDir, parentID, agentID string) string {
	return filepath.Join(projectsDir, "-Users-test-proj", parentID, "subagents", "agent-"+agentID+".jsonl")
}

// claudeWorkflowAgentJSONL 返回 workflows/<runId> 下的 agent transcript 路径。
func claudeWorkflowAgentJSONL(projectsDir, parentID, runID, agentID string) string {
	return filepath.Join(projectsDir, "-Users-test-proj", parentID, "subagents", "workflows", runID, "agent-"+agentID+".jsonl")
}

// 统一判定的形态矩阵：只有采集根下 <编码cwd>/<父sessionId>/subagents/agent-*.jsonl
// 与 .../subagents/workflows/<runId>/agent-*.jsonl 是 agent transcript。判定以
// 采集根为边界：根祖先目录名与受控段同名不影响结果。
func TestIsClaudeAgentTranscriptInRoots(t *testing.T) {
	roots := []string{"/p"}
	cases := []struct {
		path string
		want bool
	}{
		{"/p/proj/parent/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", true},
		{"/p/proj/parent/subagents/workflows/run-1/agent-aaaaaaaaaaaaaaaaa.jsonl", true},
		{"/p/proj/parent/subagents/agent-x.meta.json", false},
		{"/p/proj/parent/subagents/journal.jsonl", false},
		{"/p/proj/parent/subagents/random.jsonl", false},
		{"/p/proj/parent/subagents/agent-.jsonl", false},
		{"/p/proj/parent/subagents/workflows/run-1/journal.jsonl", false},
		{"/p/proj/parent/subagents/workflows/wf_1.json", false},
		{"/p/proj/parent/subagents/nested/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		{"/p/proj/parent/subagents/workflows/run-1/deep/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		{"/p/proj/parent/subagents/workflows/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		// runId 目录名恰为 subagents 时仍是合法的 workflows/<runId> 布局
		// （父归属按相对段取父会话目录，不受目录名影响）。
		{"/p/proj/parent/subagents/workflows/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", true},
		// 相对路径（不以根开头的调用方输入）与根哨兵：不属于受控布局。
		{"subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		{"/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		// 嵌套同名目录：相对根多出的层级不匹配受控布局。
		{"/p/proj/parent/subagents/x/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		{"/p/proj/parent/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
		{"/p/proj/agent-aaaaaaaaaaaaaaaaa.jsonl", false},
	}
	for _, tc := range cases {
		if got := IsClaudeAgentTranscriptInRoots(roots, tc.path); got != tc.want {
			t.Errorf("IsClaudeAgentTranscriptInRoots(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	// 采集根本身位于名为 subagents 的目录下（projects_dir 可配置）：正常
	// agent transcript 仍判真，非 agent 文件仍判假。
	nestedRoot := "/base/subagents/projects"
	nestedRoots := []string{nestedRoot}
	if !IsClaudeAgentTranscriptInRoots(nestedRoots, nestedRoot+"/proj/parent/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl") {
		t.Error("采集根祖先含 subagents 时正常 agent transcript 应判真")
	}
	if IsClaudeAgentTranscriptInRoots(nestedRoots, nestedRoot+"/proj/parent/subagents/journal.jsonl") {
		t.Error("journal.jsonl 不应判真")
	}
}

// ChangedFile 识别两种路径形态，父归属来自行内 sessionId，标题来自 meta。
func TestClaudeAgentChangedFileRecognizesBothLayouts(t *testing.T) {
	setIsolatedHome(t)
	t.Run("subagents 直下", func(t *testing.T) {
		projectsDir := t.TempDir()
		jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
		writeClaudeAgentJSONL(t, jsonl, agentTestParentA, "agent-msg-1")
		writeClaudeAgentMeta(t, jsonl, "经典子代理描述")

		c := newClaudeCollectorCfg(t, projectsDir)
		result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(result.Messages) != 1 {
			t.Fatalf("messages = %+v", result.Messages)
		}
		msg := result.Messages[0]
		if msg.SessionID != "agent-"+agentTestAgent1 {
			t.Errorf("Message.SessionID = %q, want 文件名 agent id", msg.SessionID)
		}
		if msg.AgentParentID != agentTestParentA {
			t.Errorf("Message.AgentParentID = %q, want %q", msg.AgentParentID, agentTestParentA)
		}
		if msg.Client != model.ClientClaudeDesktop {
			t.Errorf("Message.Client = %q, want Claude Desktop（entrypoint 判定不变）", msg.Client)
		}
		if len(result.Sessions) != 1 {
			t.Fatalf("sessions = %+v", result.Sessions)
		}
		s := result.Sessions[0]
		if s.ID != "agent-"+agentTestAgent1 {
			t.Errorf("Session.ID = %q", s.ID)
		}
		if s.AgentParentID != agentTestParentA {
			t.Errorf("Session.AgentParentID = %q, want %q", s.AgentParentID, agentTestParentA)
		}
		if s.Title != "经典子代理描述" {
			t.Errorf("Session.Title = %q, want meta description", s.Title)
		}
	})

	t.Run("workflows/<runId> 下", func(t *testing.T) {
		projectsDir := t.TempDir()
		jsonl := claudeWorkflowAgentJSONL(projectsDir, agentTestParentA, "run-1", agentTestAgent2)
		writeClaudeAgentJSONL(t, jsonl, agentTestParentA, "agent-msg-2")
		writeClaudeAgentMeta(t, jsonl, "工作流代理描述")

		c := newClaudeCollectorCfg(t, projectsDir)
		result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(result.Sessions) != 1 {
			t.Fatalf("sessions = %+v", result.Sessions)
		}
		s := result.Sessions[0]
		if s.ID != "agent-"+agentTestAgent2 || s.AgentParentID != agentTestParentA || s.Title != "工作流代理描述" {
			t.Fatalf("session = %+v", s)
		}
	})
}

// meta 缺失时 title 留空、归属仍正确，且不产生坏行。
func TestClaudeAgentMissingMetaKeepsTitleEmpty(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, jsonl, agentTestParentA, "agent-msg-1")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
	s := result.Sessions[0]
	if s.Title != "" {
		t.Errorf("Title = %q, want 空（meta 缺失）", s.Title)
	}
	if s.AgentParentID != agentTestParentA {
		t.Errorf("AgentParentID = %q, want %q", s.AgentParentID, agentTestParentA)
	}
	if len(result.FileStatuses) != 1 || result.FileStatuses[0].BadLines != 0 {
		t.Errorf("file statuses = %+v, want 无坏行", result.FileStatuses)
	}
}

// 行内 sessionId 为空或等于文件名 agent id 时跳过继续找：首个有效父 UUID
// 无论出现在哪一行（含无 usage 的 user 行）都必须被取到。
func TestClaudeAgentRowSessionIDSkipsInvalidValues(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	if err := os.MkdirAll(filepath.Dir(jsonl), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`{"type":"user","sessionId":"","isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","message":{"role":"user","content":[{"type":"text","text":"a"}]}}
{"type":"user","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:01+08:00","message":{"role":"user","content":[{"type":"text","text":"b"}]}}
{"type":"assistant","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:02+08:00","message":{"id":"skip-msg","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}
`, agentTestAgent1, agentTestParentA)
	if err := os.WriteFile(jsonl, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
	if got := result.Sessions[0].AgentParentID; got != agentTestParentA {
		t.Errorf("AgentParentID = %q, want %q（空值与 agent id 行应跳过）", got, agentTestParentA)
	}
}

// meta 容错与优先级：损坏/超大/空白 description 一律留空标题且不产生坏行；
// meta 与行内 custom-title 并存时 meta description 优先。
func TestClaudeAgentMetaFaultToleranceAndPriority(t *testing.T) {
	setIsolatedHome(t)
	cases := []struct {
		name     string
		metaData string
		title    string
	}{
		{"损坏 JSON", `{broken`, ""},
		// 合法 JSON 但总长超过读取上限：完整读取会解析出标题，截断读取则
		// Unmarshal 失败留空——锁定「超限按不可解析处理」的合同。
		{"超大 meta", `{"description":"` + strings.Repeat("x", maxClaudeAgentMetaSize) + `"}`, ""},
		{"空白 description", `{"description":"   "}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectsDir := t.TempDir()
			jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
			writeClaudeAgentJSONL(t, jsonl, agentTestParentA, "agent-msg-1")
			metaPath := strings.TrimSuffix(jsonl, ".jsonl") + ".meta.json"
			if err := os.WriteFile(metaPath, []byte(tc.metaData), 0o600); err != nil {
				t.Fatal(err)
			}
			c := newClaudeCollectorCfg(t, projectsDir)
			result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(result.Sessions) != 1 || result.Sessions[0].Title != tc.title {
				t.Fatalf("session title = %+v, want %q", result.Sessions, tc.title)
			}
			if result.FileStatuses[0].BadLines != 0 {
				t.Errorf("meta 异常不得计入坏行, got %d", result.FileStatuses[0].BadLines)
			}
		})
	}

	t.Run("meta 优先 custom-title", func(t *testing.T) {
		projectsDir := t.TempDir()
		jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
		if err := os.MkdirAll(filepath.Dir(jsonl), 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf(`{"type":"custom-title","sessionId":%q,"customTitle":"文件内标题"}
{"type":"assistant","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","message":{"id":"prio-msg","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}
`, agentTestParentA, agentTestParentA)
		if err := os.WriteFile(jsonl, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		writeClaudeAgentMeta(t, jsonl, "meta 标题")
		c := newClaudeCollectorCfg(t, projectsDir)
		result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(result.Sessions) != 1 || result.Sessions[0].Title != "meta 标题" {
			t.Fatalf("title = %+v, want meta description 优先", result.Sessions)
		}
	})
}

// 全扫受控进入 subagents 子树——收集直下与 workflows/<runId> 的 agent 文件，
// journal.jsonl、meta.json 与其他子目录文件忽略。
func TestClaudeAgentFullScanCollectsControlledLayouts(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	mainJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, mainJSONL, agentTestParentA, "main-msg", "cli")

	agent1 := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, agent1, agentTestParentA, "agent-msg-1")
	writeClaudeAgentMeta(t, agent1, "直下代理")

	subagentsDir := filepath.Dir(agent1)
	if err := os.WriteFile(filepath.Join(subagentsDir, "journal.jsonl"), []byte(`{"type":"started","label":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagentsDir, "random.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	agent2 := claudeWorkflowAgentJSONL(projectsDir, agentTestParentA, "run-1", agentTestAgent2)
	writeClaudeAgentJSONL(t, agent2, agentTestParentA, "agent-msg-2")
	writeClaudeAgentMeta(t, agent2, "工作流代理")
	if err := os.WriteFile(filepath.Join(filepath.Dir(agent2), "journal.jsonl"), []byte(`{"type":"result"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 不在受控范围：subagents 下的其他子目录不进入。
	deep := filepath.Join(subagentsDir, "other", "agent-ccccccccccccccccc.jsonl")
	writeClaudeAgentJSONL(t, deep, agentTestParentA, "deep-msg")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr = %v", result.PartialErr)
	}
	ids := map[string]bool{}
	for _, m := range result.Messages {
		ids[m.ID] = true
	}
	for _, want := range []string{"main-msg", "agent-msg-1", "agent-msg-2"} {
		if !ids[want] {
			t.Errorf("missing %s, got %v", want, ids)
		}
	}
	if ids["deep-msg"] {
		t.Errorf("deep-msg 不应被采集（subagents 下其他子目录不进入）")
	}
	if len(result.Messages) != 3 {
		t.Errorf("messages = %d, want 3", len(result.Messages))
	}
	// journal.jsonl 等非 agent 文件不产 FileStatus。
	for _, st := range result.FileStatuses {
		base := filepath.Base(st.Path)
		if base == "journal.jsonl" || base == "random.jsonl" {
			t.Errorf("不应采集 %s", st.Path)
		}
	}
	seen := map[string]model.Session{}
	for _, s := range result.Sessions {
		seen[s.ID] = s
	}
	for _, id := range []string{"agent-" + agentTestAgent1, "agent-" + agentTestAgent2} {
		s, ok := seen[id]
		if !ok {
			t.Fatalf("缺少会话 %s: %+v", id, result.Sessions)
		}
		if s.AgentParentID != agentTestParentA {
			t.Errorf("session %s AgentParentID = %q", id, s.AgentParentID)
		}
		if s.Title == "" {
			t.Errorf("session %s Title 为空，应来自 meta", id)
		}
	}
}

// 同一 agent 文件经 ChangedFile 与全扫产出一致。
func TestClaudeAgentChangedFileAndFullScanAgree(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, jsonl, agentTestParentA, "agent-msg-1", "agent-msg-2")
	writeClaudeAgentMeta(t, jsonl, "一致性代理")

	c := newClaudeCollectorCfg(t, projectsDir)
	changed, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
	if err != nil {
		t.Fatalf("ChangedFile Collect: %v", err)
	}
	full, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("full Collect: %v", err)
	}

	if len(changed.Sessions) != 1 {
		t.Fatalf("changed sessions = %+v", changed.Sessions)
	}
	var fullSession *model.Session
	for i := range full.Sessions {
		if full.Sessions[i].ID == changed.Sessions[0].ID {
			fullSession = &full.Sessions[i]
			break
		}
	}
	if fullSession == nil {
		t.Fatalf("全扫缺少会话 %s", changed.Sessions[0].ID)
	}
	if !reflect.DeepEqual(changed.Sessions[0], *fullSession) {
		t.Fatalf("session 不一致:\nchanged: %+v\nfull:    %+v", changed.Sessions[0], *fullSession)
	}
	var changedMsgs, fullMsgs []model.Message
	for _, m := range changed.Messages {
		if m.SessionID == changed.Sessions[0].ID {
			changedMsgs = append(changedMsgs, m)
		}
	}
	for _, m := range full.Messages {
		if m.SessionID == changed.Sessions[0].ID {
			fullMsgs = append(fullMsgs, m)
		}
	}
	sort.Slice(changedMsgs, func(i, j int) bool { return changedMsgs[i].ID < changedMsgs[j].ID })
	sort.Slice(fullMsgs, func(i, j int) bool { return fullMsgs[i].ID < fullMsgs[j].ID })
	if !reflect.DeepEqual(changedMsgs, fullMsgs) {
		t.Fatalf("messages 不一致:\nchanged: %+v\nfull:    %+v", changedMsgs, fullMsgs)
	}
}

// 父会话与 agent 文件同时采集，message.id 零交集，用量不重复。
func TestClaudeAgentParentAndAgentMessagesDisjoint(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	parentJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, parentJSONL, agentTestParentA, "parent-msg-1", "cli")
	f, err := os.OpenFile(parentJSONL, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, `{"type":"assistant","sessionId":%q,"timestamp":"2026-07-08T10:01:00+08:00","entrypoint":"cli","cwd":"/tmp/proj","message":{"id":"parent-msg-2","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", agentTestParentA); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	agent := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, agent, agentTestParentA, "agent-msg-1", "agent-msg-2")
	writeClaudeAgentMeta(t, agent, "子代理")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	seen := map[string]int{}
	var total int64
	for _, m := range result.Messages {
		seen[m.ID]++
		total += m.TotalTokens
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %s 出现 %d 次，父子文件不应有重复 id", id, n)
		}
	}
	if len(seen) != 4 {
		t.Errorf("unique ids = %d, want 4", len(seen))
	}
	if total != 4*15 {
		t.Errorf("total tokens = %d, want %d", total, 4*15)
	}
	var parentSession, agentSession *model.Session
	for i := range result.Sessions {
		s := &result.Sessions[i]
		if s.ID == agentTestParentA {
			parentSession = s
		}
		if s.ID == "agent-"+agentTestAgent1 {
			agentSession = s
		}
	}
	if parentSession == nil || agentSession == nil {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
	if parentSession.AgentParentID != "" {
		t.Errorf("父会话 AgentParentID = %q, want 空", parentSession.AgentParentID)
	}
	if agentSession.AgentParentID != agentTestParentA {
		t.Errorf("子代理 AgentParentID = %q, want %q", agentSession.AgentParentID, agentTestParentA)
	}
}

// 旧版本（2.1.161 形态：meta 字段更少、无 api-request 行）兼容。
func TestClaudeAgentLegacyFixtureCompatibility(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	jsonl := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	if err := os.MkdirAll(filepath.Dir(jsonl), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`{"type":"user","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/old","message":{"role":"user","content":[{"type":"text","text":"任务"}]}}
{"type":"assistant","sessionId":%q,"isSidechain":true,"timestamp":"2026-07-08T10:00:05+08:00","cwd":"/tmp/old","message":{"id":"old-msg-1","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":7,"output_tokens":3}}}
`, agentTestParentA, agentTestParentA)
	if err := os.WriteFile(jsonl, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 旧版 meta 只有 description。
	if err := os.WriteFile(strings.TrimSuffix(jsonl, ".jsonl")+".meta.json", []byte(`{"description":"旧版子代理"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClaudeCollectorCfg(t, projectsDir)
	handler := &testLogHandler{}
	result, err := c.Collect(t.Context(), CollectRequest{}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr = %v", result.PartialErr)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "old-msg-1" {
		t.Fatalf("messages = %+v", result.Messages)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
	s := result.Sessions[0]
	if s.Title != "旧版子代理" || s.AgentParentID != agentTestParentA {
		t.Fatalf("session = %+v", s)
	}
	if result.FileStatuses[0].BadLines != 0 {
		t.Errorf("BadLines = %d, want 0", result.FileStatuses[0].BadLines)
	}
}

// cc-switch 账号根下的 subagents 同样生效。
func TestClaudeAgentCCSwitchAccountRoots(t *testing.T) {
	home := setIsolatedHome(t)
	acctProjects := ccSwitchAccountProjects(t, home, "a")
	parent := agentTestParentA
	jsonl := filepath.Join(acctProjects, "-Users-test-proj", parent, "subagents", "agent-"+agentTestAgent1+".jsonl")
	writeClaudeAgentJSONL(t, jsonl, parent, "acct-agent-msg")
	writeClaudeAgentMeta(t, jsonl, "账号代理")

	c := newClaudeCollectorCfg(t, t.TempDir())
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "acct-agent-msg" {
		t.Fatalf("messages = %+v", result.Messages)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].Title != "账号代理" || result.Sessions[0].AgentParentID != parent {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
}

// 跨父目录同名副本——行内 sessionId 优先于路径兜底，标题来自带 meta 的副本。
func TestClaudeAgentDuplicateCopiesRowSessionIDWins(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	copyA := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	copyB := claudeAgentJSONL(projectsDir, agentTestParentB, agentTestAgent1)
	content := `{"type":"assistant","sessionId":"` + agentTestParentA + `","isSidechain":true,"timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"claude-desktop-3p","cwd":"/tmp/proj","message":{"id":"dup-msg","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	for _, p := range []string{copyA, copyB} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// meta 只存在于真正父会话（A）目录。
	writeClaudeAgentMeta(t, copyA, "重复副本代理")

	c := newClaudeCollectorCfg(t, projectsDir)
	handler := &testLogHandler{}
	result, err := c.Collect(t.Context(), CollectRequest{}, slog.New(handler))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("sessions = %+v", result.Sessions)
	}
	var titles []string
	for _, s := range result.Sessions {
		if s.AgentParentID != agentTestParentA {
			t.Errorf("session %s AgentParentID = %q, want 行内 %q（路径兜底不得覆盖）", s.ID, s.AgentParentID, agentTestParentA)
		}
		titles = append(titles, s.Title)
	}
	sort.Strings(titles)
	if !reflect.DeepEqual(titles, []string{"", "重复副本代理"}) {
		t.Errorf("titles = %v, want [\"\" \"重复副本代理\"]（meta 只在 A 副本）", titles)
	}
	// B 副本路径兜底与其父目录名不一致（行内指向 A），必须 Warn。
	if !handler.HasRecord(slog.LevelWarn, "Claude agent transcript parent mismatch, using row sessionId") {
		t.Errorf("缺少 parent mismatch Warn: %v", handler.Messages())
	}

	// 顺序无关：先扫 B 再扫 A，归属仍为行内 A。
	changedB, err := c.Collect(t.Context(), CollectRequest{ChangedFile: copyB}, nil)
	if err != nil {
		t.Fatalf("Collect B: %v", err)
	}
	changedA, err := c.Collect(t.Context(), CollectRequest{ChangedFile: copyA}, nil)
	if err != nil {
		t.Fatalf("Collect A: %v", err)
	}
	for _, r := range []CollectResult{changedB, changedA} {
		if len(r.Sessions) != 1 || r.Sessions[0].AgentParentID != agentTestParentA {
			t.Fatalf("ChangedFile 归属 = %+v, want %q", r.Sessions, agentTestParentA)
		}
	}
	if changedB.Sessions[0].Title != "" || changedA.Sessions[0].Title != "重复副本代理" {
		t.Fatalf("title B=%q A=%q", changedB.Sessions[0].Title, changedA.Sessions[0].Title)
	}
}

// 坏 workflow 目录（不可读）隔离——同根主会话与后续健康 agent 仍入库，
// 失败记为部分失败而非整体失败；ctx 取消立即退出。
func TestClaudeAgentBadWorkflowDirIsolated(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("windows/root 下 chmod 000 不产生读取失败，无法触发子树失败路径")
	}
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	mainJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, mainJSONL, agentTestParentA, "main-msg", "cli")

	badRun := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA, "subagents", "workflows", "bad-run")
	if err := os.MkdirAll(badRun, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRun, "agent-deadbeefdeadbeef0.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badRun, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(badRun, 0o755) })

	goodAgent := claudeWorkflowAgentJSONL(projectsDir, agentTestParentA, "good-run", agentTestAgent2)
	writeClaudeAgentJSONL(t, goodAgent, agentTestParentA, "good-agent-msg")
	writeClaudeAgentMeta(t, goodAgent, "健康代理")
	// 同根下的兄弟项目目录，验证坏子树不影响根内其他目录的采集。
	healthy := filepath.Join(projectsDir, "-Users-test-other", "healthy.jsonl")
	writeClaudeAssistantJSONL(t, healthy, "other-session", "healthy-msg", "cli")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect err = %v, want nil（子树失败不构成整体失败）", err)
	}
	if result.PartialErr == nil {
		t.Fatalf("坏 workflow 目录应记为 PartialErr")
	}
	ids := map[string]bool{}
	for _, m := range result.Messages {
		ids[m.ID] = true
	}
	for _, want := range []string{"main-msg", "good-agent-msg", "healthy-msg"} {
		if !ids[want] {
			t.Errorf("missing %s（坏子树不得拖累健康兄弟）, got %v", want, ids)
		}
	}

	// ctx 取消：Collect 立即返回取消错误，不继续遍历。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, cerr := c.Collect(ctx, CollectRequest{}, nil); cerr == nil {
		t.Fatalf("已取消 ctx 应立即返回错误")
	}
}

// claudeAgentParentInRoots 的兜底矩阵：受控布局取父会话目录名；非受控布局
// 返回空串。
func TestClaudeAgentParentInRoots(t *testing.T) {
	roots := []string{"/p"}
	cases := []struct{ path, want string }{
		{"/p/slug/parent/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", "parent"},
		{"/p/slug/parent/subagents/workflows/run-1/agent-aaaaaaaaaaaaaaaaa.jsonl", "parent"},
		{"/p/slug/parent/subagents/x/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl", ""},
		{"/p/slug/parent/subagents/journal.jsonl", ""},
	}
	for _, tc := range cases {
		if got := claudeAgentParentInRoots(roots, tc.path); got != tc.want {
			t.Errorf("claudeAgentParentInRoots(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}

	// 采集根祖先含 subagents：父归属仍取相对段的父会话目录名。
	nestedRoot := "/base/subagents/projects"
	if got := claudeAgentParentInRoots([]string{nestedRoot}, nestedRoot+"/slug/parent/subagents/agent-aaaaaaaaaaaaaaaaa.jsonl"); got != "parent" {
		t.Errorf("根祖先含 subagents 时父归属 = %q, want parent", got)
	}
}

// 行内 sessionId 全部缺失时，父归属由受控路径兜底（subagents 直下与
// workflows/<runId> 两种布局）。
func TestClaudeAgentParentFallsBackToPath(t *testing.T) {
	setIsolatedHome(t)
	for _, tc := range []struct {
		name string
		path func(projectsDir string) string
	}{
		{"subagents 直下", func(d string) string { return claudeAgentJSONL(d, agentTestParentA, agentTestAgent1) }},
		{"workflows 下", func(d string) string { return claudeWorkflowAgentJSONL(d, agentTestParentA, "run-1", agentTestAgent1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectsDir := t.TempDir()
			jsonl := tc.path(projectsDir)
			writeClaudeAgentJSONL(t, jsonl, "", "fallback-msg")
			c := newClaudeCollectorCfg(t, projectsDir)
			result, err := c.Collect(t.Context(), CollectRequest{ChangedFile: jsonl}, nil)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(result.Sessions) != 1 || result.Sessions[0].AgentParentID != agentTestParentA {
				t.Fatalf("session = %+v, want 路径兜底父 %q", result.Sessions, agentTestParentA)
			}
		})
	}
}

// 非 subagents 的普通子目录不可读：记为部分失败并继续扫描健康兄弟（旧实现
// 会中止整棵根目录遍历）。
func TestClaudeAgentBadSiblingDirIsolated(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("windows/root 下 chmod 000 不产生读取失败，无法触发子树失败路径")
	}
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	badDir := filepath.Join(projectsDir, "-Users-test-broken")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "agent-deadbeefdeadbeef0.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDir, 0o755) })

	healthy := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, healthy, agentTestParentA, "healthy-msg", "cli")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect err = %v, want nil（子树失败不构成整体失败）", err)
	}
	if result.PartialErr == nil {
		t.Fatalf("坏兄弟目录应记为 PartialErr")
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "healthy-msg" {
		t.Fatalf("messages = %+v, want 健康主会话仍入库", result.Messages)
	}
}

// 全扫枚举结果与 IsClaudeAgentTranscriptInRoots 谓词对目录树的判定等价：主 transcript
// 全收，subagents 子树只收谓词判真的文件（防止遍历结构与谓词两套实现漂移）。
func TestClaudeAgentFullScanMatchesPredicate(t *testing.T) {
	setIsolatedHome(t)
	projectsDir := t.TempDir()
	mainJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, mainJSONL, agentTestParentA, "main-msg", "cli")

	agent1 := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, agent1, agentTestParentA, "a1")
	subagentsDir := filepath.Dir(agent1)
	if err := os.WriteFile(filepath.Join(subagentsDir, "journal.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent2 := claudeWorkflowAgentJSONL(projectsDir, agentTestParentA, "run-1", agentTestAgent2)
	writeClaudeAgentJSONL(t, agent2, agentTestParentA, "a2")
	if err := os.WriteFile(filepath.Join(filepath.Dir(agent2), "journal.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(subagentsDir, "other", "agent-ccccccccccccccccc.jsonl")
	writeClaudeAgentJSONL(t, deep, agentTestParentA, "deep")

	got, err := findClaudeJSONLFiles(context.Background(), projectsDir)
	if err != nil {
		t.Fatalf("findClaudeJSONLFiles: %v", err)
	}

	var want []string
	if werr := filepath.Walk(projectsDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/subagents/") && !IsClaudeAgentTranscriptInRoots([]string{projectsDir}, path) {
			return nil
		}
		want = append(want, path)
		return nil
	}); werr != nil {
		t.Fatalf("walk: %v", werr)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("全扫结果与谓词判定不一致:\ngot:  %v\nwant: %v", got, want)
	}
}

// 采集根本身位于名为 subagents 的目录下（projects_dir 可配置）：全扫与
// ChangedFile 仍正常采集主会话与 agent transcript（判定以采集根为边界）。
func TestClaudeAgentRootUnderSubagentsDir(t *testing.T) {
	setIsolatedHome(t)
	base := t.TempDir()
	projectsDir := filepath.Join(base, "subagents", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, mainJSONL, agentTestParentA, "main-msg", "cli")
	agentJSONL := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, agentJSONL, agentTestParentA, "agent-msg")
	writeClaudeAgentMeta(t, agentJSONL, "根祖先同名代理")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr = %v", result.PartialErr)
	}
	ids := map[string]bool{}
	for _, m := range result.Messages {
		ids[m.ID] = true
	}
	if !ids["main-msg"] || !ids["agent-msg"] {
		t.Fatalf("messages = %v, want 主会话与 agent 都采集", ids)
	}

	// ChangedFile 入口同样生效。
	changed, err := c.Collect(t.Context(), CollectRequest{ChangedFile: agentJSONL}, nil)
	if err != nil {
		t.Fatalf("ChangedFile Collect: %v", err)
	}
	if len(changed.Sessions) != 1 || changed.Sessions[0].AgentParentID != agentTestParentA || changed.Sessions[0].Title != "根祖先同名代理" {
		t.Fatalf("ChangedFile session = %+v", changed.Sessions)
	}
}

// ClaudeProjectsRoots 统一返回绝对 Clean 形态（相对配置也规范为绝对），与
// watcher 监控根的规范化一致。
func TestClaudeProjectsRootsAbsForm(t *testing.T) {
	setIsolatedHome(t)
	base := t.TempDir()
	t.Chdir(base)
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "relative-projects"}},
	}}
	roots := ClaudeProjectsRoots(cfg)
	if len(roots) != 1 || !filepath.IsAbs(roots[0]) {
		t.Fatalf("roots = %v, want 单个绝对根", roots)
	}
	if want := filepath.Join(base, "relative-projects"); filepath.Clean(roots[0]) != filepath.Clean(want) {
		t.Fatalf("roots[0] = %q, want %q", roots[0], want)
	}

	// 绝对配置保持绝对 Clean 形态。
	abs := filepath.Join(base, "abs-projects")
	cfg2 := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": abs}},
	}}
	roots2 := ClaudeProjectsRoots(cfg2)
	if len(roots2) != 1 || roots2[0] != filepath.Clean(abs) {
		t.Fatalf("roots2 = %v, want [%q]", roots2, filepath.Clean(abs))
	}
}

// 相对 projects_dir：全扫与 ChangedFile 都按绝对形态判定，agent 的标题与父
// 归属正确（watcher 绝对事件路径与相对采集根形态不一致曾导致运行期丢失）。
func TestClaudeAgentRelativeProjectsDir(t *testing.T) {
	setIsolatedHome(t)
	base := t.TempDir()
	t.Chdir(base)
	projectsDir := "relative-projects"
	agentJSONL := claudeAgentJSONL(projectsDir, agentTestParentA, agentTestAgent1)
	writeClaudeAgentJSONL(t, agentJSONL, agentTestParentA, "rel-agent-msg")
	writeClaudeAgentMeta(t, agentJSONL, "相对路径代理")
	mainJSONL := filepath.Join(projectsDir, "-Users-test-proj", agentTestParentA+".jsonl")
	writeClaudeAssistantJSONL(t, mainJSONL, agentTestParentA, "rel-main-msg", "cli")

	c := newClaudeCollectorCfg(t, projectsDir)
	result, err := c.Collect(t.Context(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr = %v", result.PartialErr)
	}
	var agentSession *model.Session
	for i := range result.Sessions {
		if result.Sessions[i].ID == "agent-"+agentTestAgent1 {
			agentSession = &result.Sessions[i]
		}
	}
	if agentSession == nil || agentSession.AgentParentID != agentTestParentA || agentSession.Title != "相对路径代理" {
		t.Fatalf("全扫 agent 会话 = %+v, want 父 %q、标题来自 meta", result.Sessions, agentTestParentA)
	}

	// ChangedFile 传相对路径同样正确（入口统一为绝对 Clean 形态）。
	changed, err := c.Collect(t.Context(), CollectRequest{ChangedFile: agentJSONL}, nil)
	if err != nil {
		t.Fatalf("ChangedFile Collect: %v", err)
	}
	if len(changed.Sessions) != 1 || changed.Sessions[0].AgentParentID != agentTestParentA || changed.Sessions[0].Title != "相对路径代理" {
		t.Fatalf("ChangedFile 会话 = %+v", changed.Sessions)
	}
}

// 多根全扫入口同样把相对目录规范为绝对路径（与主根和 ChangedFile 一致）。
func TestFindClaudeJSONLFilesInDirsAbsForm(t *testing.T) {
	setIsolatedHome(t)
	base := t.TempDir()
	t.Chdir(base)
	rel := "rel-root"
	p := filepath.Join(rel, "-proj", "parent", "subagents", "agent-aaaaaaaaaaaaaaaaa.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := findClaudeJSONLFilesInDirs(context.Background(), []string{rel})
	if err != nil {
		t.Fatalf("findClaudeJSONLFilesInDirs: %v", err)
	}
	if len(files) != 1 || !filepath.IsAbs(files[0]) {
		t.Fatalf("files = %v, want 单个绝对路径", files)
	}
}
