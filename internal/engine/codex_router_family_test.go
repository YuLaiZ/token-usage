package engine

import (
	"context"
	"io"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// TestRunCollect_CodexFamilyBackfill 覆盖子代理家族回填端到端（异模型）：
//
//   - 主会话 sess-1 为 gpt-6.1-terra，子代理 sess-2（AgentParentID=sess-1）为
//     deepseek-v4.1-flash；子代理请求在 cc-switch 侧记在主会话 session 下。
//   - 本轮采集入口只触达子代理消息（session 集合={sess-2}）——家族展开必须把
//     集合扩到父会话，messages 侧全量含父会话 6 条 terra 消息，日志侧用根集合
//     查得 6 条 proxy 行（5 terra + 1 deepseek）。
//   - 断言：子代理消息归因到 OpenCode Go/deepseek（不被父会话抢）；父会话所有
//     归因都是 terra（不得出现 terra 消息标 deepseek 的错配）。
func TestRunCollect_CodexFamilyBackfill(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	terra := func(reqID string, delta int64) model.RouterLog {
		return model.RouterLog{RequestID: "session:codex:p1:" + reqID, RouterName: "cc_switch",
			SessionID: "codex_sess-1", AppType: "codex", Model: "gpt-6.1-terra",
			ProviderName: "OpenAI Official", DataSource: "proxy", CreatedAt: baseSec + delta}
	}
	logs := []model.RouterLog{
		terra("r1", 0), terra("r2", 5), terra("r3", 10),
		{RequestID: "session:codex:p1:rd", RouterName: "cc_switch", SessionID: "codex_sess-1",
			AppType: "codex", Model: "deepseek-v4.1-flash", ProviderName: "OpenCode Go",
			DataSource: "proxy", CreatedAt: baseSec + 15},
		terra("r4", 20), terra("r5", 25),
	}
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, logs); err != nil {
		t.Fatal(err)
	}

	// 表内既有：父会话 6 条 terra 消息（m4 与子代理请求同窗——无模型校验时会被抢）。
	parentMsgs := []model.Message{
		{ID: "m1#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 1) * 1000, TotalTokens: 1},
		{ID: "m2#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 6) * 1000, TotalTokens: 1},
		{ID: "m3#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 11) * 1000, TotalTokens: 1},
		{ID: "m4#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 16) * 1000, TotalTokens: 1},
		{ID: "m5#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 21) * 1000, TotalTokens: 1},
		{ID: "m6#1", SessionID: "sess-1", Model: "ccs-opencode/gpt-6.1-terra",
			Client: model.ClientCodexApp, Date: "2026-06-10", TS: (baseSec + 26) * 1000, TotalTokens: 1},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, parentMsgs); err != nil {
		t.Fatal(err)
	}
	// 父会话已带父关系先验（归因家族展开的输入来自 messages.agent_parent_id）。
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "seed#1", SessionID: "sess-2", AgentParentID: "sess-1",
			Model: "ccs-opencode/deepseek-v4.1-flash", Client: model.ClientCodexApp,
			Date: "2026-06-10", TS: (baseSec + 2) * 1000, TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// 本轮采集：只触达子代理消息（入口 session 集合={sess-2}）。
	c := fixedResultCollector("codex", collector.CollectResult{
		Messages: []model.Message{{
			ID: "cm1#1", SessionID: "sess-2", AgentParentID: "sess-1",
			Model: "ccs-opencode/deepseek-v4.1-flash", Client: model.ClientCodexApp,
			Date: "2026-06-10", TS: (baseSec + 15) * 1000, TotalTokens: 1,
		}},
	})
	result := RunCollect(context.Background(), codexRouterDeps(c, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}

	// 子代理消息：归因到 OpenCode Go/deepseek（家族展开 + 模型校验）。
	var childProvider, childModel string
	if err := usageDB.QueryRow(
		`SELECT router_provider, router_model FROM messages WHERE id='cm1#1'`).Scan(&childProvider, &childModel); err != nil {
		t.Fatal(err)
	}
	if childProvider != "OpenCode Go" || childModel != "deepseek-v4.1-flash" {
		t.Fatalf("子代理归因 = %q/%q, want OpenCode Go/deepseek-v4.1-flash", childProvider, childModel)
	}
	// 父会话：所有非空归因必须是 terra，且不得出现 deepseek。
	rows, err := usageDB.Query(`SELECT id, COALESCE(router_model,''), COALESCE(router_provider,'') FROM messages
		WHERE session_id='sess-1' AND router_model<>''`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, rm, rp string
		if err := rows.Scan(&id, &rm, &rp); err != nil {
			t.Fatal(err)
		}
		count++
		if rm != "gpt-6.1-terra" || rp != "OpenAI Official" {
			t.Fatalf("父会话 %s 归因 = %q/%q, want gpt-6.1-terra/OpenAI Official（异模型不得串）", id, rp, rm)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("父会话归因数 = %d, want 5（6 条消息 5 条 terra proxy 行）", count)
	}
}

// TestRunCollect_CodexLateFamilyRelationTriggersBackfill：父关系晚于消息入库的
// 到达顺序回归——孙代理消息与根会话 proxy 行先入库（中间层关系尚不存在 →
// 不归因），随后中间层代理的纯会话元数据（无消息）才被采集；本轮必须因
// Session 父关系触发家族重算并补齐孙代理归因（不能等新消息/新日志）。
func TestRunCollect_CodexLateFamilyRelationTriggersBackfill(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	// 先入库：根会话 proxy 行 + 孙代理消息（AgentParentID=mid，但 mid→root 关系不存在）。
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{{
		RequestID: "session:codex:p1:r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "deepseek-v4.1-flash", ProviderName: "OpenCode Go",
		DataSource: "proxy", CreatedAt: baseSec,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "gm1#1", SessionID: "grand", AgentParentID: "mid", Client: model.ClientCodexApp,
		Date: "2026-06-10", TS: (baseSec + 1) * 1000, TotalTokens: 1,
		Model: "ccs-opencode/deepseek-v4.1-flash",
	}}); err != nil {
		t.Fatal(err)
	}

	// 第一轮：空采集（无新消息/会话）→ 不归因（前置：父链缺 mid→root）。
	empty := fixedResultCollector("codex", collector.CollectResult{})
	if res := RunCollect(context.Background(), codexRouterDeps(empty, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false); !res.Complete() {
		t.Fatalf("round 1 result = %+v", res)
	}
	var before string
	if err := usageDB.QueryRow(`SELECT COALESCE(router_provider,'') FROM messages WHERE id='gm1#1'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != "" {
		t.Fatalf("前置校验失败：父链缺失时不应归因，got %q", before)
	}

	// 第二轮：只补中间层会话关系（无消息，只带父关系的最小元数据）。必须触发家族重算。
	late := fixedResultCollector("codex", collector.CollectResult{
		SessionRelations: []model.Session{{
			ID: "mid", Client: model.ClientCodexCLI, AgentParentID: "root",
		}},
	})
	if res := RunCollect(context.Background(), codexRouterDeps(late, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/y.jsonl"}, false, false); !res.Complete() {
		t.Fatalf("round 2 result = %+v", res)
	}
	var provider, routerModel string
	if err := usageDB.QueryRow(
		`SELECT router_provider, router_model FROM messages WHERE id='gm1#1'`).Scan(&provider, &routerModel); err != nil {
		t.Fatal(err)
	}
	if provider != "OpenCode Go" || routerModel != "deepseek-v4.1-flash" {
		t.Fatalf("父关系晚到后孙代理归因 = %q/%q, want OpenCode Go/deepseek-v4.1-flash", provider, routerModel)
	}
}
