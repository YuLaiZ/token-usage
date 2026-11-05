package collector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// === Codex 子代理家族标记解析合同 ===
//
// 合同：session_meta 的顶层 parent_thread_id / thread_source 决定消息的
// AgentParentID（子代理家族归因的输入）；旧版/对象形态 source 走
// {"subagent":{"thread_spawn":{"parent_thread_id"}}} 降级提取；主会话为空；
// 自环（parent==id）防御为空，避免建立自我父子关系。

func TestParseCodexRollout_SubagentParent(t *testing.T) {
	cases := []struct {
		name     string
		metaLine string
		want     string
	}{
		{
			name:     "顶层 parent_thread_id + thread_source=subagent",
			metaLine: `{"type":"session_meta","payload":{"id":"child-1","parent_thread_id":"root-1","thread_source":"subagent"}}`,
			want:     "root-1",
		},
		{
			name:     "对象形态 source 降级提取",
			metaLine: `{"type":"session_meta","payload":{"id":"child-2","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root-2","depth":1}}}}}`,
			want:     "root-2",
		},
		{
			name:     "主会话无父（字符串 source）",
			metaLine: `{"type":"session_meta","payload":{"id":"root-3","source":"cli","originator":"codex-tui"}}`,
			want:     "",
		},
		{
			name:     "自环防御",
			metaLine: `{"type":"session_meta","payload":{"id":"self-1","parent_thread_id":"self-1"}}`,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout-2026-07-08T03-00-00-x.jsonl")
			content := tc.metaLine + "\n" +
				`{"timestamp":"2026-07-08T03:01:00Z","type":"turn_context","payload":{"model":"ccs-opencode/deepseek-v4.1-flash"}}` + "\n" +
				`{"timestamp":"2026-07-08T03:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-1"}}` + "\n" +
				`{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}` + "\n"
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			res, _, err := parseCodexRollout(path, codexThread{}, nil)
			if err != nil {
				t.Fatalf("parseCodexRollout: %v", err)
			}
			if len(res.Messages) != 1 {
				t.Fatalf("messages = %d, want 1", len(res.Messages))
			}
			if got := res.Messages[0].AgentParentID; got != tc.want {
				t.Fatalf("AgentParentID = %q, want %q", got, tc.want)
			}
			if res.Messages[0].Model != "ccs-opencode/deepseek-v4.1-flash" {
				t.Fatalf("message model = %q（turn_context 应照常生效）", res.Messages[0].Model)
			}
		})
	}
}

// TestCodexCollector_KeepsSessionForMessagelessSubagent：普通 state DB 采集路径
// （`collect all --client codex` 与 daemon 全扫走这条）对「无 token 消息但携带
// 子代理父关系」的 rollout 必须保留 Session——中间层代理关系是孙代理家族链的
// 关键，丢弃会使父链断裂（sessions 是家族关系权威来源）。
func TestCodexCollector_KeepsSessionForMessagelessSubagent(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 中间层代理 rollout：只有 session_meta（parent_thread_id），无 token 消息。
	rollout := filepath.Join(sessionsDir, "rollout-2026-11-05T08-00-00-mid-1.jsonl")
	content := `{"timestamp":"2026-11-05T08:00:00Z","type":"session_meta","payload":{"id":"mid-1","parent_thread_id":"root-1","thread_source":"subagent"}}
`
	if err := os.WriteFile(rollout, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	createStateDBWithThreads(t, stateDir, []codexThread{
		{ID: "mid-1", RolloutPath: rollout, Cwd: "/tmp/a", Source: "cli", ThreadSource: "subagent", UpdatedAtMS: 1000},
	})

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	result, err := NewCodexCollector(cfg).Collect(context.Background(),
		CollectRequest{Source: CollectSourceClient}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("中间层 rollout 不应产出消息，got %d", len(result.Messages))
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("无消息会话不应携带完整 Session（避免覆盖元数据），got %d", len(result.Sessions))
	}
	if len(result.SessionRelations) != 1 {
		t.Fatalf("无消息但携带父关系的会话关系必须保留（collect all 路径），got %d", len(result.SessionRelations))
	}
	if result.SessionRelations[0].AgentParentID != "root-1" {
		t.Fatalf("SessionRelations.AgentParentID = %q, want root-1", result.SessionRelations[0].AgentParentID)
	}
}

// TestCodexCollector_DateFilteredSubagentKeepsRelationOnly：按日期采集时，消息
// 全部被日期过滤掉的 rollout 不携带完整 Session（不覆盖历史元数据），但保留
// 会话关系（SessionRelations，engine 只写 agent_parent_id）——中间代理昨日有
// 消息、孙代理今日有消息时，跨日期家族父链依赖这条关系。
func TestCodexCollector_DateFilteredSubagentKeepsRelationOnly(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 子代理 rollout：消息日期 2026-07-08，父关系存在。
	rollout := filepath.Join(sessionsDir, "rollout-2026-07-08T03-00-00-child-d1.jsonl")
	content := `{"timestamp":"2026-07-08T03:00:00Z","type":"session_meta","payload":{"id":"child-d1","parent_thread_id":"root-d1","thread_source":"subagent"}}
{"timestamp":"2026-07-08T03:01:00Z","type":"turn_context","payload":{"model":"gpt-5.6"}}
{"timestamp":"2026-07-08T03:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-d1"}}
{"timestamp":"2026-07-08T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}
`
	if err := os.WriteFile(rollout, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	createStateDBWithThreads(t, stateDir, []codexThread{
		{ID: "child-d1", RolloutPath: rollout, Cwd: "/tmp/a", Source: "cli", ThreadSource: "subagent", UpdatedAtMS: 1000},
	})
	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	// 采集 2026-07-09（rollout 的消息都在 07-08 → 全部被日期过滤）。
	result, err := NewCodexCollector(cfg).Collect(context.Background(),
		CollectRequest{Source: CollectSourceClient, Dates: []string{"2026-07-09"}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("日期外的消息不应入库，got %d", len(result.Messages))
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("日期过滤后的历史会话不应携带完整 Session（不得覆盖元数据），got %d", len(result.Sessions))
	}
	if len(result.SessionRelations) != 1 {
		t.Fatalf("日期过滤后仍须保留会话关系（跨日期家族父链），got %d", len(result.SessionRelations))
	}
	if result.SessionRelations[0].AgentParentID != "root-d1" {
		t.Fatalf("SessionRelations.AgentParentID = %q, want root-d1", result.SessionRelations[0].AgentParentID)
	}
}
