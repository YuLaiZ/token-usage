package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// stagedLog 是构造已入库 router 日志的便捷 helper（模拟 router 增量轮先落库的交错）。
func stagedLog(messageID string) model.RouterLog {
	return model.RouterLog{
		RequestID: "r-" + messageID, MessageID: messageID, RouterName: "cc_switch",
		AppType: "claude", ProviderName: "Zhipu GLM", Model: "glm-5.3",
	}
}

// claudeRouterDeps 构造配了 cc_switch router 的 claude 依赖（fixture 组合便捷 helper）。
func claudeRouterDeps(c collector.Collector, router collector.RouterAdapter) *Deps {
	return &Deps{
		cfg: &config.Config{
			Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		},
		collectors: []collector.Collector{c},
		routers:    map[string]collector.RouterAdapter{"cc_switch": router},
	}
}

// TestRunCollect_DaemonClientRoundsBackfillFromStagedRouterLogs：
// daemon 的 client 入库轮（jsonl watcher 与 startup catch-up 两形态，Dates 恒空）
// 在 messages 入库后按 message_id 查已入库的 raw_router_logs 表回填归因，
// 覆盖「router 日志先入库、message 后入库」的交错——该交错下 router 增量轮的
// UPDATE 因 message 尚未入库而落空，且 cursor 已推过不再重读。
func TestRunCollect_DaemonClientRoundsBackfillFromStagedRouterLogs(t *testing.T) {
	cases := []struct {
		name string
		req  collector.CollectRequest
	}{
		{name: "watcher changed file", req: collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}},
		{name: "startup catch-up client", req: collector.CollectRequest{Source: collector.CollectSourceClient}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageDB, _ := db.Open(":memory:")
			defer usageDB.Close()
			if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
				[]model.RouterLog{stagedLog("m1")}); err != nil {
				t.Fatal(err)
			}
			c := fixedResultCollector("claude", collector.CollectResult{
				Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-09-01")},
			})
			result := RunCollect(context.Background(), claudeRouterDeps(c, newFakeRouter()), usageDB,
				collectTestLogger(), io.Discard, "claude", tc.req, false, false)
			if !result.Complete() {
				t.Fatalf("result = %+v", result)
			}
			var routerProvider, routerModel string
			if err := usageDB.QueryRow(
				`SELECT router_provider, router_model FROM messages WHERE id='m1' AND client=?`,
				model.ClientClaudeCode).Scan(&routerProvider, &routerModel); err != nil {
				t.Fatal(err)
			}
			if routerProvider != "Zhipu GLM" || routerModel != "glm-5.3" {
				t.Fatalf("router attribution not backfilled from staged logs: provider=%q model=%q",
					routerProvider, routerModel)
			}
		})
	}
}

// TestRunCollect_BackfillLogDeferredUntilCommit：回填 Debug 轨迹只能在事务提交
// 成功后记录——后续 MarkCollected、cursor 写入或 Commit 失败会连同归因一起回滚，
// 提前记录会留下回填成功的假轨迹。
func TestRunCollect_BackfillLogDeferredUntilCommit(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
		[]model.RouterLog{stagedLog("m1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_mark BEFORE INSERT ON collection_log
		BEGIN SELECT RAISE(ABORT, 'forced mark failure'); END`); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-09-01")},
	})
	handler := &levelCaptureHandler{}
	result := RunCollect(context.Background(), claudeRouterDeps(c, newFakeRouter()), usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-09-01"}}, false, false)
	if result.Complete() {
		t.Fatalf("mark failure must fail collect: %+v", result)
	}
	if handler.hasRecordAt(slog.LevelDebug, "router attribution backfilled") {
		t.Fatal("rolled-back backfill must not leave a success trace")
	}
}

// TestRunCollect_BackfillSuccessLoggedAtDebug（守卫）：提交成功且命中 >0 条时，
// 回填轨迹以 Debug 记录（预期行为，与采集完成心跳同级；0 条不打）。
func TestRunCollect_BackfillSuccessLoggedAtDebug(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
		[]model.RouterLog{stagedLog("m1")}); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-09-01")},
	})
	handler := &levelCaptureHandler{}
	result := RunCollect(context.Background(), claudeRouterDeps(c, newFakeRouter()), usageDB,
		slog.New(handler), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if !handler.hasRecordAt(slog.LevelDebug, "router attribution backfilled") {
		t.Fatal("backfilled round must leave a Debug trace")
	}
}

// TestRunCollect_BackfillDoesNotInventAttribution（守卫）：raw_router_logs 无对应
// 日志时查表不命中，回填不得无中生有——router_provider 保持空串。
func TestRunCollect_BackfillDoesNotInventAttribution(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
		[]model.RouterLog{stagedLog("other-message")}); err != nil {
		t.Fatal(err)
	}
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-09-01")},
	})
	result := RunCollect(context.Background(), claudeRouterDeps(c, newFakeRouter()), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var routerProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
		model.ClientClaudeCode).Scan(&routerProvider); err != nil {
		t.Fatal(err)
	}
	if routerProvider != "" {
		t.Fatalf("backfill must not invent attribution: %q", routerProvider)
	}
}

// TestRunCollect_NoRouterConfiguredDaemonRoundPersists（守卫）：未配置 router 的
// client 入库轮不受回填改动影响，正常入库无错误。
func TestRunCollect_NoRouterConfiguredDaemonRoundPersists(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{msg("m1", model.ClientClaudeCode, "2026-09-01")},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("messages not persisted: count=%d err=%v", count, err)
	}
}

// TestRunCollect_LegacyRouterClientRoundSkipsAttributionBackfill：
// 存量非 Claude router 配置（读取链容忍、装配正常）的 client 入库批不得回填
// 归因——codex 自己的 m1 与既有 Claude router 日志 message_id 碰撞时，若放行
// 回填会经 app_type 映射把 Claude Code 的 m1 归因改写。表驱动覆盖 CLI Dates
// 形态（本轮还拉取 router 日志、routerFetched=true）与 daemon watcher 形态。
func TestRunCollect_LegacyRouterClientRoundSkipsAttributionBackfill(t *testing.T) {
	cases := []struct {
		name string
		req  collector.CollectRequest
	}{
		{name: "cli dates", req: collector.CollectRequest{Dates: []string{"2026-09-01"}}},
		{name: "daemon watcher", req: collector.CollectRequest{ChangedFile: "/tmp/x.jsonl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageDB, _ := db.Open(":memory:")
			defer usageDB.Close()
			if _, err := db.UpsertRawRouterLogs(context.Background(), usageDB,
				[]model.RouterLog{stagedLog("m1")}); err != nil {
				t.Fatal(err)
			}
			// 预置 Claude Code 的 m1 已有归因 X（与日志值 Y 不同，碰撞可观测）。
			if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
				{ID: "m1", Client: model.ClientClaudeCode, Date: "2026-09-01", RouterProvider: "X"},
			}); err != nil {
				t.Fatal(err)
			}
			c := fixedResultCollector("codex", collector.CollectResult{
				Messages: []model.Message{msg("m1", model.ClientCodexCLI, "2026-09-01")},
			})
			router := newFakeRouter() // CLI Dates 形态拉取成功（返回空日志），routerFetched=true
			deps := &Deps{
				cfg: &config.Config{
					Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
				},
				collectors: []collector.Collector{c},
				routers:    map[string]collector.RouterAdapter{"cc_switch": router},
			}
			result := RunCollect(context.Background(), deps, usageDB,
				collectTestLogger(), io.Discard, "codex", tc.req, false, false)
			if !result.Complete() {
				t.Fatalf("result = %+v", result)
			}
			var claudeProvider, codexProvider string
			if err := usageDB.QueryRow(
				`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
				model.ClientClaudeCode).Scan(&claudeProvider); err != nil {
				t.Fatal(err)
			}
			if claudeProvider != "X" {
				t.Fatalf("legacy client round must not rewrite Claude attribution: %q", claudeProvider)
			}
			if err := usageDB.QueryRow(
				`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
				model.ClientCodexCLI).Scan(&codexProvider); err != nil {
				t.Fatal(err)
			}
			if codexProvider != "" {
				t.Fatalf("codex message must persist without attribution: %q", codexProvider)
			}
		})
	}
}

// TestRunCollect_LegacyRouterOnlyRoundWritesRawButNotAttribution：
// 存量非 Claude 配置触发的 daemon router 增量轮（Source=router）：raw 日志照写、
// cursor 照推，但不得回填归因——cc-switch 日志按 app_type 混存同一 db，
// 该轮读到的 Claude 类型日志自带非空 message_id，若照日志自身 ID 回填会直接
// 更新 Claude messages（跨 client 写入，无需 ID 碰撞前提）。
func TestRunCollect_LegacyRouterOnlyRoundWritesRawButNotAttribution(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "m1", Client: model.ClientClaudeCode, Date: "2026-09-01"},
	}); err != nil {
		t.Fatal(err)
	}
	router := newFakeRouter()
	router.result = collector.RouterCollectResult{
		Logs:       []model.RouterLog{stagedLog("m1")},
		NextCursor: model.SyncCursor{Value: 1788247004, ID: "r-m1"},
	}
	deps := &Deps{
		cfg: &config.Config{
			Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
		},
		routers: map[string]collector.RouterAdapter{"cc_switch": router},
	}
	result := RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "codex",
		collector.CollectRequest{Source: collector.CollectSourceRouter}, false, false)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	var logCount int
	if err := usageDB.QueryRow(
		`SELECT COUNT(*) FROM raw_router_logs WHERE request_id='r-m1'`).Scan(&logCount); err != nil || logCount != 1 {
		t.Fatalf("raw router logs must persist: count=%d err=%v", logCount, err)
	}
	cursors, err := db.GetSyncCursors(context.Background(), usageDB, "codex",
		[]string{router.SyncSource()})
	if err != nil {
		t.Fatal(err)
	}
	if got := cursors[router.SyncSource()]; got.Value != 1788247004 || got.ID != "r-m1" {
		t.Fatalf("router cursor must advance: %+v", got)
	}
	var claudeProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
		model.ClientClaudeCode).Scan(&claudeProvider); err != nil {
		t.Fatal(err)
	}
	if claudeProvider != "" {
		t.Fatalf("legacy router round must not backfill Claude messages: %q", claudeProvider)
	}
}

// TestRunRouterBackfill_LegacyClientWritesRawSkipsAttribution：
// 全量回填对存量非 Claude 配置只保留 raw 日志入库，跳过归因回填；
// 控制台 0 条回填的输出如实反映。raw 入库与回填门控必须分别生效：
// 门控连带跳过读取/Upsert、或完全不门控，本测试都能捕捉。
func TestRunRouterBackfill_LegacyClientWritesRawSkipsAttribution(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{"codex": {Enabled: true, Router: "cc_switch"}},
	}
	router := &fakeBackfillRouter{
		name: "cc_switch",
		logs: []model.RouterLog{{
			RequestID: "r9", MessageID: "m1", RouterName: "cc_switch",
			AppType: "claude", ProviderName: "Y",
		}},
	}
	usageDB, deps := setupBackfillTestDB(t, cfg, router)
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "m1", Client: model.ClientClaudeCode, Date: "2026-09-01", RouterProvider: "X"},
		{ID: "m1", Client: model.ClientCodexCLI, Date: "2026-09-01"},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunRouterBackfill(ctx, deps, usageDB, slog.Default(), &out, "codex"); err != nil {
		t.Fatalf("RunRouterBackfill failed: %v", err)
	}
	var logCount int
	if err := usageDB.QueryRow(
		`SELECT COUNT(*) FROM raw_router_logs WHERE request_id='r9'`).Scan(&logCount); err != nil || logCount != 1 {
		t.Fatalf("raw router logs must persist: count=%d err=%v", logCount, err)
	}
	var claudeProvider, codexProvider string
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
		model.ClientClaudeCode).Scan(&claudeProvider); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m1' AND client=?`,
		model.ClientCodexCLI).Scan(&codexProvider); err != nil {
		t.Fatal(err)
	}
	if claudeProvider != "X" {
		t.Fatalf("legacy backfill must not rewrite Claude attribution: %q", claudeProvider)
	}
	if codexProvider != "" {
		t.Fatalf("codex message must keep empty attribution: %q", codexProvider)
	}
	if !strings.Contains(out.String(), "router backfilled 0 attributions") {
		t.Fatalf("output must report zero attributions: %q", out.String())
	}
}
