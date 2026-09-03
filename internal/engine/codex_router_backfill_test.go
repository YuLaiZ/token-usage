package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === codex router 归因合同（session + 时间窗路径，三入口统一双侧全量） ===
//
// 合同：codex 配置 router 后，三处回填入口（消息采集 / router 增量轮 / 全量回填）
// 统一执行「session 集合 → 双侧全量查询（QueryCodexRouterLogsBySessions +
// QueryCodexMessagesBySessions）→ MatchCodexRouterAttributions → BackfillRouterFields」；
// codex 轮不做 claude 日志的 message_id 回填（跨 client 防御）；codex_session
// 同步行不进归因候选。

// TestUniqueCodexSessionIDs_NonEmptyAfterNormalize：空 session 与归一化后为空
// 的畸形形态（"codex_"）都不得进集合；剥前缀、去重照常。
func TestUniqueCodexSessionIDs_NonEmptyAfterNormalize(t *testing.T) {
	msgs := []model.Message{
		{SessionID: ""},
		{SessionID: "codex_"},
		{SessionID: "codex_uuid-a"},
		{SessionID: "uuid-b"},
		{SessionID: "codex_uuid-a"}, // 重复
	}
	got := uniqueCodexSessionIDs(msgs)
	want := []string{"uuid-a", "uuid-b"}
	if len(got) != len(want) {
		t.Fatalf("uniqueCodexSessionIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueCodexSessionIDs = %v, want %v", got, want)
		}
	}
}

// codexProxyLog 构造已入库的 codex proxy 路由行 fixture（cc-switch 源形态）。
func codexProxyLog(reqID, session string, createdAt int64) model.RouterLog {
	return model.RouterLog{
		RequestID: reqID, RouterName: "cc_switch", SessionID: session,
		AppType: "codex", Model: "gpt-5.6-terra", ProviderName: "Zhipu GLM",
		DataSource: "proxy", CreatedAt: createdAt,
	}
}

// codexMsg 构造带 session 与毫秒 ts 的 codex message fixture。
func codexMsg(id, session string, tsMs int64, client string) model.Message {
	return model.Message{ID: id, SessionID: session, Client: client, Date: "2026-06-10", TS: tsMs}
}

// codexRouterDeps 构造配了 cc_switch router 的 codex 依赖。
func codexRouterDeps(c collector.Collector, router collector.RouterAdapter) *Deps {
	return &Deps{
		cfg: &config.Config{
			Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
		},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
}

// TestRunCollect_CodexMessageRoundBackfillsViaSession：codex 配置 router 后，
// 消息采集入口（daemon watcher 与 startup catch-up 两形态、CLI Dates 形态）
// 经 session+时间窗路径回填 messages.router_* 三列。
func TestRunCollect_CodexMessageRoundBackfillsViaSession(t *testing.T) {
	const baseSec = int64(1781092800) // 2026-06-10 12:00:00 UTC
	cases := []struct {
		name string
		req  collector.CollectRequest
	}{
		{name: "watcher changed file", req: collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}},
		{name: "startup catch-up client", req: collector.CollectRequest{Source: collector.CollectSourceClient}},
		{name: "cli dates", req: collector.CollectRequest{Dates: []string{"2026-06-10"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageDB, _ := db.Open(":memory:")
			defer usageDB.Close()
			if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
				[]model.RouterLog{codexProxyLog("session:codex:p1:resp_1", "codex_sess-1", baseSec)}); err != nil {
				t.Fatal(err)
			}
			c := fixedResultCollector("codex", collector.CollectResult{
				Messages: []model.Message{codexMsg("msg_a#1", "sess-1", (baseSec+3)*1000, model.ClientCodexCLI)},
			})
			result := RunCollect(context.Background(), codexRouterDeps(c, newFakeRouter()), usageDB,
				collectTestLogger(), io.Discard, "codex", tc.req, false, false)
			if !result.Complete() {
				t.Fatalf("result = %+v", result)
			}
			var provider, m, router string
			if err := usageDB.QueryRow(
				`SELECT router_provider, router_model, router_name FROM messages WHERE id='msg_a#1' AND client=?`,
				model.ClientCodexCLI).Scan(&provider, &m, &router); err != nil {
				t.Fatal(err)
			}
			if provider != "Zhipu GLM" || m != "gpt-5.6-terra" || router != "cc_switch" {
				t.Fatalf("codex 归因未回填: provider=%q model=%q router=%q", provider, m, router)
			}
		})
	}
}

// TestRunCollect_CodexMessageRoundMatchesBothSidesFullData：消息入口不豁免
// 双侧全量——本轮增量数据与表内既有数据两侧都参与匹配（跨日交错的修复依赖）。
// 本轮只采 sess-1 的 m2，但表内既有同 session 的 m1 也必须被回填；
// 集合外 session（sess-2）即使时间窗内有 proxy 行也不参与本轮归因。
func TestRunCollect_CodexMessageRoundMatchesBothSidesFullData(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 表内既有：sess-1 的 m1（非本轮数据）与 sess-2 的 proxy 行。
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		codexMsg("msg_early#1", "sess-1", (baseSec+1)*1000, model.ClientCodexApp),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{
		codexProxyLog("session:codex:p1:resp_early", "codex_sess-1", baseSec),
		codexProxyLog("session:codex:p1:resp_late", "codex_sess-1", baseSec+5),
		codexProxyLog("session:codex:p1:resp_other", "codex_sess-2", baseSec+6),
	}); err != nil {
		t.Fatal(err)
	}
	// 本轮只采 sess-1 的 m2。
	c := fixedResultCollector("codex", collector.CollectResult{
		Messages: []model.Message{codexMsg("msg_late#2", "sess-1", (baseSec+5)*1000, model.ClientCodexApp)},
	})
	result := RunCollect(context.Background(), codexRouterDeps(c, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// 消息侧全量：既有 m1（非本轮）也获得归因。
	var earlyProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_early#1'`).Scan(&earlyProvider); err != nil {
		t.Fatal(err)
	}
	if earlyProvider != "Zhipu GLM" {
		t.Fatalf("表内既有 message 应参与匹配（消息侧全量）: %q", earlyProvider)
	}
	// 本轮 m2 亦回填。
	var lateProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_late#2'`).Scan(&lateProvider); err != nil {
		t.Fatal(err)
	}
	if lateProvider != "Zhipu GLM" {
		t.Fatalf("本轮 message 应回填: %q", lateProvider)
	}
}

// TestRunCollect_CodexCrossMidnightBackfill：跨午夜交错红向回归——
// D1 消息轮入库 token 事件 23:59:59 的 message，D2 router 轮才入库 00:00:01 的
// proxy 行；两次普通采集（无全量 backfill）后归因不得缺失。
func TestRunCollect_CodexCrossMidnightBackfill(t *testing.T) {
	const d1Sec = int64(1781135999) // 2026-06-10 23:59:59 UTC（token 事件时刻）
	const d2Sec = int64(1781136001) // 2026-06-11 00:00:01 UTC（proxy 行写库时刻）
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	// D1：消息轮入库（router 日志尚不存在）。
	d1Collector := fixedResultCollector("codex", collector.CollectResult{
		Messages: []model.Message{codexMsg("msg_mid#1", "sess-mid", d1Sec*1000, model.ClientCodexCLI)},
	})
	if result := RunCollect(context.Background(), codexRouterDeps(d1Collector, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/d1.jsonl"}, false, false); !result.Complete() {
		t.Fatalf("D1 消息轮失败: %+v", result)
	}
	var d1Provider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_mid#1'`).Scan(&d1Provider); err != nil {
		t.Fatal(err)
	}
	if d1Provider != "" {
		t.Fatalf("D1 时点不应有归因: %q", d1Provider)
	}

	// D2：router 增量轮入库 00:00:01 的 proxy 行（跨日数据，本轮无 message）。
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs:       []model.RouterLog{codexProxyLog("session:codex:p1:resp_mid", "codex_sess-mid", d2Sec)},
		NextCursor: model.SyncCursor{Value: d2Sec, ID: "session:codex:p1:resp_mid"},
	}
	deps := &Deps{
		cfg: &config.Config{
			Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
		},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	if result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false); !result.Complete() {
		t.Fatalf("D2 router 轮失败: %+v", result)
	}
	var provider, m string
	if err := usageDB.QueryRow(
		`SELECT router_provider, router_model FROM messages WHERE id='msg_mid#1'`).Scan(&provider, &m); err != nil {
		t.Fatal(err)
	}
	if provider != "Zhipu GLM" || m != "gpt-5.6-terra" {
		t.Fatalf("跨午夜归因缺失（D2 轮应经双侧全量补上）: provider=%q model=%q", provider, m)
	}
}

// TestRunCollect_CodexRouterRoundSkipsClaudeBackfill：codex 的 router 轮不得
// 回填 claude message——本轮读到的 claude 类型日志带非空 message_id，codex 轮
// 跳过 message_id 提取回填（跨 client 防御），只走 codex session 路径。
func TestRunCollect_CodexRouterRoundSkipsClaudeBackfill(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		msg("m_claude", model.ClientClaudeCode, "2026-06-10"),
		codexMsg("msg_cx#1", "sess-1", baseSec*1000, model.ClientCodexCLI),
	}); err != nil {
		t.Fatal(err)
	}
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs: []model.RouterLog{
			stagedLog("m_claude"), // claude 日志混入 codex 轮
			codexProxyLog("session:codex:p1:resp_1", "codex_sess-1", baseSec),
		},
		NextCursor: model.SyncCursor{Value: baseSec + 1, ID: "session:codex:p1:resp_1"},
	}
	deps := &Deps{
		cfg: &config.Config{
			Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
		},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	if result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false); !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var claudeProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m_claude' AND client=?`,
		model.ClientClaudeCode).Scan(&claudeProvider); err != nil {
		t.Fatal(err)
	}
	if claudeProvider != "" {
		t.Fatalf("codex 轮不得回填 claude message: %q", claudeProvider)
	}
	var codexProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_cx#1' AND client=?`,
		model.ClientCodexCLI).Scan(&codexProvider); err != nil {
		t.Fatal(err)
	}
	if codexProvider != "Zhipu GLM" {
		t.Fatalf("codex session 路径应正常回填: %q", codexProvider)
	}
}

// TestRunRouterBackfill_CodexFullBackfill：全量回填入口——codex proxy 行归因成功；
// codex_session 行与 claude 行不进候选（即使同 session / 同 message id 空间）。
func TestRunRouterBackfill_CodexFullBackfill(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		codexMsg("msg_fb#1", "sess-1", baseSec*1000, model.ClientCodexApp),
		codexMsg("msg_fb#2", "sess-2", (baseSec+1)*1000, model.ClientCodexApp),
		msg("m_claude", model.ClientClaudeCode, "2026-06-10"),
	}); err != nil {
		t.Fatal(err)
	}
	router := &fakeBackfillRouter{
		name: "cc_switch",
		logs: []model.RouterLog{
			codexProxyLog("session:codex:p1:resp_fb1", "codex_sess-1", baseSec),
			// codex_session 行：session 归属 sess-2 但无路由价值，不得归因。
			{RequestID: "codex_session:thread-v1:sess-2:1", RouterName: "cc_switch",
				SessionID: "codex_sess-2", AppType: "codex", Model: "session-model",
				DataSource: "codex_session", CreatedAt: baseSec + 1},
			// claude 行：不进 codex 归因。
			stagedLog("m_claude"),
		},
	}
	cfg := &config.Config{
		Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
	}
	deps := &Deps{cfg: cfg, routers: map[string]collector.RouterAdapter{"cc_switch": router}}
	var out bytes.Buffer
	if err := RunRouterBackfill(context.Background(), deps, usageDB, collectTestLogger(), &out, "codex"); err != nil {
		t.Fatalf("RunRouterBackfill failed: %v", err)
	}
	var s1Provider, s2Provider, claudeProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_fb#1'`).Scan(&s1Provider); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='msg_fb#2'`).Scan(&s2Provider); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m_claude' AND client=?`,
		model.ClientClaudeCode).Scan(&claudeProvider); err != nil {
		t.Fatal(err)
	}
	if s1Provider != "Zhipu GLM" {
		t.Fatalf("codex proxy 行应归因: %q", s1Provider)
	}
	if s2Provider != "" {
		t.Fatalf("codex_session 行不得归因: %q", s2Provider)
	}
	if claudeProvider != "" {
		t.Fatalf("claude 行不得进入 codex 归因: %q", claudeProvider)
	}
}

// TestRunCollect_CodexBackfillOverwriteAndIdempotent：覆盖语义与重跑幂等——
// 300s 窗内双 proxy 行（近/远）竞争同一 message：早期子集轮次（只有较远行）
// 先写入较远归因，后续完整集合轮次以非空新值覆盖纠正为最近邻；
// 同输入重跑结果稳定（幂等）。
func TestRunCollect_CodexBackfillOverwriteAndIdempotent(t *testing.T) {
	const baseSec = int64(1781092800)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	// 第一轮（子集）：表内只有较远 proxy 行（Δt=+50s）。
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{
		codexProxyLog("session:codex:p1:resp_far", "codex_sess-1", baseSec+50),
	}); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("codex", collector.CollectResult{
		Messages: []model.Message{codexMsg("msg_ov#1", "sess-1", baseSec*1000, model.ClientCodexCLI)},
	})
	if result := RunCollect(context.Background(), codexRouterDeps(c, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/1.jsonl"}, false, false); !result.Complete() {
		t.Fatalf("第一轮失败: %+v", result)
	}
	var model1 string
	if err := usageDB.QueryRow(
		`SELECT router_model FROM messages WHERE id='msg_ov#1'`).Scan(&model1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model1, "terra") {
		t.Fatalf("第一轮应回填较远行归因: %q", model1)
	}

	// 第二轮（完整集合）：较近行（Δt=+2s）入库后重算，非空新值覆盖纠正。
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB, []model.RouterLog{
		codexProxyLog("session:codex:p1:resp_near", "codex_sess-1", baseSec+2),
	}); err != nil {
		t.Fatal(err)
	}
	// 近邻行 model 不同以区分纠偏方向。
	if _, err := usageDB.Exec(
		`UPDATE raw_router_logs SET model='gpt-5.6-sol' WHERE request_id='session:codex:p1:resp_near'`); err != nil {
		t.Fatal(err)
	}
	if result := RunCollect(context.Background(), codexRouterDeps(
		fixedResultCollector("codex", collector.CollectResult{
			Messages: []model.Message{codexMsg("msg_ov#1", "sess-1", baseSec*1000, model.ClientCodexCLI)},
		}), newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/2.jsonl"}, false, false); !result.Complete() {
		t.Fatalf("第二轮失败: %+v", result)
	}
	var model2 string
	if err := usageDB.QueryRow(
		`SELECT router_model FROM messages WHERE id='msg_ov#1'`).Scan(&model2); err != nil {
		t.Fatal(err)
	}
	if model2 != "gpt-5.6-sol" {
		t.Fatalf("完整集合轮次应纠偏为最近邻行: %q", model2)
	}

	// 重跑幂等：同输入第三次运行结果不变、不报错。
	if result := RunCollect(context.Background(), codexRouterDeps(
		fixedResultCollector("codex", collector.CollectResult{
			Messages: []model.Message{codexMsg("msg_ov#1", "sess-1", baseSec*1000, model.ClientCodexCLI)},
		}), newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/3.jsonl"}, false, false); !result.Complete() {
		t.Fatalf("第三轮失败: %+v", result)
	}
	var model3 string
	if err := usageDB.QueryRow(
		`SELECT router_model FROM messages WHERE id='msg_ov#1'`).Scan(&model3); err != nil {
		t.Fatal(err)
	}
	if model3 != "gpt-5.6-sol" {
		t.Fatalf("重跑应幂等: %q", model3)
	}
}

// TestRunCollect_CodexNoRouterConfiguredPersistsWithoutAttribution：
// 未配置 codex router 时消息入库行为不变：无归因、无错误（现状守卫）。
func TestRunCollect_CodexNoRouterConfiguredPersistsWithoutAttribution(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("codex", collector.CollectResult{
		Messages: []model.Message{codexMsg("msg_plain#1", "sess-1", 1781092800000, model.ClientCodexCLI)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var count, attributed int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("messages not persisted: count=%d err=%v", count, err)
	}
	if err := usageDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE router_provider != ''`).Scan(&attributed); err != nil || attributed != 0 {
		t.Fatalf("未配置 router 不得产生归因: attributed=%d err=%v", attributed, err)
	}
}
