package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// writeCodexSubagentRollout 写一个含子代理家族标记的最小 rollout。
func writeCodexSubagentRollout(t *testing.T, path, sessionID, parentID, modelName string) {
	t.Helper()
	content := `{"timestamp":"2026-11-05T08:28:37Z","type":"session_meta","payload":{"id":"` + sessionID +
		`","parent_thread_id":"` + parentID +
		`","thread_source":"subagent","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `","depth":1}}}}}
{"timestamp":"2026-11-05T08:28:40Z","type":"turn_context","payload":{"model":"` + modelName + `"}}
{"timestamp":"2026-11-05T08:28:45Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-c1"}}
{"timestamp":"2026-11-05T08:28:50Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
}

// TestScanGate_ParserVersionUpgradeReparsesCodexFamily：ParserVersion 升级后的
// 升级链路回归——已有旧版本门记录的 Codex rollout 在 startup catch-up 必须被
// 重读，历史子代理的父关系补齐到 messages 与 sessions（否则升级后历史子代理
// 永久缺归因；collect router 不解析 rollout，无法补救）。
func TestScanGate_ParserVersionUpgradeReparsesCodexFamily(t *testing.T) {
	requireGateIdentityEnv(t)

	base := t.TempDir()
	sessionsDir := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(sessionsDir, "rollout-2026-11-05T16-28-37-child-1.jsonl")
	writeCodexSubagentRollout(t, rollout, "child-1", "root-1", "ccs-opencode/deepseek-v4.1-flash")

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{"sessions_dir": sessionsDir, "state_dir": stateDir}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewCodexCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	// 预置「旧解析器版本」的门记录：文件快照与门记录一致，仅 parser_version
	// 落后一个版本——模拟升级前已扫描过的存量文件。
	snap := fsident.SnapshotOfFile(rollout)
	if !fsident.Valid(snap) {
		t.Skip("当前文件系统不提供可靠文件实体标识")
	}
	if err := db.UpsertFileScanLog(context.Background(), usageDB, []model.FileScanLog{{
		Client:        "codex",
		FilePath:      rollout,
		FileIdentity:  snap.Identity,
		MtimeNS:       snap.MtimeNS,
		FileSize:      snap.Size,
		ParserVersion: int64(db.ParserVersion) - 1,
	}}); err != nil {
		t.Fatal(err)
	}

	result := RunCollect(context.Background(), deps, usageDB, collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}

	// 重读生效：子代理消息的父关系落库。
	var msgParent string
	if err := usageDB.QueryRow(
		`SELECT agent_parent_id FROM messages WHERE session_id='child-1' LIMIT 1`).Scan(&msgParent); err != nil {
		t.Fatalf("查询 messages: %v", err)
	}
	if msgParent != "root-1" {
		t.Fatalf("messages.agent_parent_id = %q, want root-1（版本升级必须触发重读补齐）", msgParent)
	}
	// sessions 是家族关系权威来源（即使该子代理没有消息也不丢）。
	var sessParent string
	if err := usageDB.QueryRow(
		`SELECT agent_parent_id FROM sessions WHERE id='child-1' AND client IN ('Codex App','Codex CLI')`).Scan(&sessParent); err != nil {
		t.Fatalf("查询 sessions: %v", err)
	}
	if sessParent != "root-1" {
		t.Fatalf("sessions.agent_parent_id = %q, want root-1", sessParent)
	}
	// 门记录版本回到当前值（后续轮次可正常命中）。
	logs, err := db.GetFileScanLogs(context.Background(), usageDB, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if got := logs[rollout].ParserVersion; got != int64(db.ParserVersion) {
		t.Fatalf("重读后门版本 = %d, want %d", got, db.ParserVersion)
	}
}

// writeCodexFamilyRolloutAt 写一个带父关系的 rollout（时间由 base 决定，本地
// 时区日期稳定，避免跨时区 fixture 漂移）。
func writeCodexFamilyRolloutAt(t *testing.T, path, sessionID, parentID, modelName string, base time.Time) {
	t.Helper()
	ts := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }
	content := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"parent_thread_id":%q,"thread_source":"subagent"}}
{"timestamp":%q,"type":"turn_context","payload":{"model":%q}}
{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-%s"}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}
`, ts(0), sessionID, parentID, ts(1*time.Minute), modelName, ts(2*time.Minute), sessionID, ts(3*time.Minute))
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
}

// TestRunCollect_CrossDateFamilyRelation：跨日期家族回归——中间代理昨日有消息
// （今日按日期采集时其消息全部被过滤），孙代理今日有消息；数据库尚无 mid→root
// 关系。今日采集必须保留中间代理的会话关系（SessionRelations 专用写入），使
// 孙代理能沿父链归因到根会话的 proxy 行（新库/未重采场景可能断链）。
func TestRunCollect_CrossDateFamilyRelation(t *testing.T) {
	base := t.TempDir()
	sessionsDir := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 本地时区日期：mid=2026-07-08，grand=2026-07-09（跨时区稳定）。
	midBase := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
	grandBase := time.Date(2026, 7, 9, 12, 0, 0, 0, time.Local)
	midRollout := filepath.Join(sessionsDir, "rollout-mid-0708.jsonl")
	grandRollout := filepath.Join(sessionsDir, "rollout-grand-0709.jsonl")
	writeCodexFamilyRolloutAt(t, midRollout, "mid", "root", "gpt-5.6", midBase)
	writeCodexFamilyRolloutAt(t, grandRollout, "grand", "mid", "gpt-5.6", grandBase)
	createCodexStateDB(t, stateDir, []codexStateThread{
		{id: "mid", rolloutPath: midRollout, source: "cli", cwd: "/tmp/a", updatedAtMS: 1000},
		{id: "grand", rolloutPath: grandRollout, source: "cli", cwd: "/tmp/a", updatedAtMS: 2000},
	})

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	// 根会话 proxy 行（时间对齐 grand 的 token_count 事件）；数据库无 mid→root 关系。
	grandTokenSec := grandBase.Add(3 * time.Minute).Unix()
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{{
		RequestID: "session:codex:p1:r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "gpt-5.6", ProviderName: "OpenAI Official",
		DataSource: "proxy", CreatedAt: grandTokenSec,
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Router: "cc_switch", Paths: map[string]string{
			"sessions_dir": sessionsDir, "state_dir": stateDir,
		}},
	}}
	deps := &Deps{
		cfg:        cfg,
		collectors: []collector.Collector{collector.NewCodexCollector(cfg)},
		routers:    map[string]collector.RouterAdapter{"cc_switch": newFakeRouter()},
	}
	// 按今日（2026-07-09）采集：mid 的消息被日期过滤，grand 入库。
	result := RunCollect(context.Background(), deps, usageDB, collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceClient, Dates: []string{"2026-07-09"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// mid 无今日消息，但其会话关系必须落库（SessionRelations 专用写入）。
	var rel string
	if err := usageDB.QueryRow(
		`SELECT agent_parent_id FROM sessions WHERE id='mid' AND client IN ('Codex App','Codex CLI')`).Scan(&rel); err != nil {
		t.Fatalf("中间代理关系应落库（跨日期家族父链）: %v", err)
	}
	if rel != "root" {
		t.Fatalf("sessions.agent_parent_id = %q, want root", rel)
	}
	// 孙代理今日消息沿「grand→mid→root」归因到根会话 proxy 行。
	var provider, routerModel string
	if err := usageDB.QueryRow(
		`SELECT router_provider, router_model FROM messages WHERE session_id='grand' LIMIT 1`).Scan(&provider, &routerModel); err != nil {
		t.Fatalf("grand 消息应入库: %v", err)
	}
	if provider != "OpenAI Official" || routerModel != "gpt-5.6" {
		t.Fatalf("grand 归因 = %q/%q, want OpenAI Official/gpt-5.6（跨日期父链应通）", provider, routerModel)
	}
}

// TestRunCollect_UnchangedRelationsDoNotRewrite：按日期采集遍历到「无当日消息
// 的历史子代理」时，其关系（库中已存在且未变化）不得作为归因重算触发源——
// 否则单日采集会全量重算所有历史家族，旧日志降级匹配可能覆盖既有归因。
// 反向保证：目标家族（本轮有当日消息的孙代理）的跨日期父链仍能归因（见
// TestRunCollect_CrossDateFamilyRelation）。
func TestRunCollect_UnchangedRelationsDoNotRewrite(t *testing.T) {
	base := t.TempDir()
	sessionsDir := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 家族 B（历史，关系已存在）：midB 昨日有消息；家族 A（今日）：grandA 有消息。
	midBBase := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
	grandABase := time.Date(2026, 7, 9, 12, 0, 0, 0, time.Local)
	midBRollout := filepath.Join(sessionsDir, "rollout-midB-0708.jsonl")
	grandARollout := filepath.Join(sessionsDir, "rollout-grandA-0709.jsonl")
	writeCodexFamilyRolloutAt(t, midBRollout, "midB", "rootB", "gpt-5.6", midBBase)
	writeCodexFamilyRolloutAt(t, grandARollout, "grandA", "midA", "gpt-5.6", grandABase)
	createCodexStateDB(t, stateDir, []codexStateThread{
		{id: "midB", rolloutPath: midBRollout, source: "cli", cwd: "/tmp/b", updatedAtMS: 1000},
		{id: "grandA", rolloutPath: grandARollout, source: "cli", cwd: "/tmp/a", updatedAtMS: 2000},
	})

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	// 库中已存在 B 的父关系（未变化）。
	if _, err := db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{{
		ID: "midB", Client: model.ClientCodexCLI, AgentParentID: "rootB", FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	// B 的历史消息（已有归因 Original）+ B 的 proxy 行（重算会覆盖成 Rewritten）。
	bMsgTS := midBBase.Add(3 * time.Minute)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "bm1#1", SessionID: "rootB", Client: model.ClientCodexApp,
		Date: midBBase.Format("2006-01-02"), TS: bMsgTS.UnixMilli(), Model: "gpt-5.6",
		RouterProvider: "Original", RouterModel: "gpt-5.6", RouterName: "cc_switch", TotalTokens: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{{
		RequestID: "session:codex:p1:rb", RouterName: "cc_switch", SessionID: "codex_rootB",
		AppType: "codex", Model: "gpt-5.6", ProviderName: "Rewritten",
		DataSource: "proxy", CreatedAt: bMsgTS.Unix(),
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Router: "cc_switch", Paths: map[string]string{
			"sessions_dir": sessionsDir, "state_dir": stateDir,
		}},
	}}
	deps := &Deps{
		cfg:        cfg,
		collectors: []collector.Collector{collector.NewCodexCollector(cfg)},
		routers:    map[string]collector.RouterAdapter{"cc_switch": newFakeRouter()},
	}
	result := RunCollect(context.Background(), deps, usageDB, collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceClient, Dates: []string{"2026-07-09"}}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// B 的关系未变化：不得触发 B 家族重算，既有归因不得被改写。
	var provider string
	if err := usageDB.QueryRow(`SELECT router_provider FROM messages WHERE id='bm1#1'`).Scan(&provider); err != nil {
		t.Fatal(err)
	}
	if provider != "Original" {
		t.Fatalf("未变化的历史关系不得触发重算（归因被改写为 %q）", provider)
	}
	// A 的当日消息正常入库。
	var n int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id='grandA'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("grandA 当日消息应入库，got %d", n)
	}
}

// TestCatchUpCoversRolloutsMissingFromStateDB：两个采集入口的覆盖边界——
// 普通采集 / `collect all` 走 state DB threads（rollout 未登记则不处理）；
// startup catch-up（ScanExistingJSONL）按 sessions_dir 目录全扫，能补到
// state DB 未列出的 rollout（含其父关系）。文档据此区分两个入口的承诺。
func TestCatchUpCoversRolloutsMissingFromStateDB(t *testing.T) {
	base := t.TempDir()
	sessionsDir := filepath.Join(base, "sessions")
	stateDir := filepath.Join(base, "state") // 空目录：没有任何 state_*.sqlite 线程登记
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(sessionsDir, "rollout-2026-11-05T08-00-00-orphan-1.jsonl")
	writeCodexSubagentRollout(t, rollout, "orphan-1", "root-1", "gpt-5.6")

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{"sessions_dir": sessionsDir, "state_dir": stateDir}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewCodexCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	result := RunCollect(context.Background(), deps, usageDB, collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// catch-up 目录全扫覆盖 state DB 未登记的 rollout：消息与父关系都采到。
	var parent string
	if err := usageDB.QueryRow(
		`SELECT agent_parent_id FROM messages WHERE session_id='orphan-1' LIMIT 1`).Scan(&parent); err != nil {
		t.Fatalf("catch-up 应采到 state DB 未登记的 rollout: %v", err)
	}
	if parent != "root-1" {
		t.Fatalf("messages.agent_parent_id = %q, want root-1", parent)
	}
	var relation string
	if err := usageDB.QueryRow(
		`SELECT agent_parent_id FROM sessions WHERE id='orphan-1' AND client IN ('Codex App','Codex CLI')`).Scan(&relation); err != nil {
		t.Fatalf("sessions 关系应落库: %v", err)
	}
	if relation != "root-1" {
		t.Fatalf("sessions.agent_parent_id = %q, want root-1", relation)
	}
}
