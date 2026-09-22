package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === mimocode Desktop 拆分 reconciliation 测试（schema v5 pending 驱动） ===
//
// fake mimo collector 提供可控的源库视图（assignments + 当前采集结果）；
// usage 库为真实 db.Open 库（v5，pending 就绪）。覆盖评审要求的：无损重归属
//（源库已删历史消息仍保留 / 源库已消失 session 原样）、批事务原子性
//（各步骤注入失败整批回滚）、自动触发与失败重试、分批与幂等。

// fakeMimoCollector 是可控的 mimocode collector 假实现。
type fakeMimoCollector struct {
	assignments  []collector.MimoSessionAssignment
	messages     []model.Message
	sessions     []model.Session
	assignErr    error
	collectErr   error
	assignCalls  int
	collectCalls int
}

func (f *fakeMimoCollector) Name() string { return "mimocode" }
func (f *fakeMimoCollector) SyncSources() []string {
	return []string{collector.SyncSourceMimoCodeMessage}
}
func (f *fakeMimoCollector) Assignments(context.Context) ([]collector.MimoSessionAssignment, error) {
	f.assignCalls++
	return f.assignments, f.assignErr
}
func (f *fakeMimoCollector) Collect(context.Context, collector.CollectRequest, *slog.Logger) (collector.CollectResult, error) {
	f.collectCalls++
	if f.collectErr != nil {
		return collector.CollectResult{}, f.collectErr
	}
	return collector.CollectResult{Messages: f.messages, Sessions: f.sessions}, nil
}

func clearInitialMimoPending(t *testing.T, usageDB *db.DB) {
	t.Helper()
	pending, generation, _, err := db.MimoReconcilePending(context.Background(), usageDB)
	if err != nil || !pending {
		t.Fatalf("初始 pending 应存在: pending=%v generation=%d err=%v", pending, generation, err)
	}
	cleared, err := db.MimoReconcileClearPendingCAS(context.Background(), usageDB, generation)
	if err != nil || !cleared {
		t.Fatalf("清除初始 pending 失败: cleared=%v err=%v", cleared, err)
	}
}

func mimoReconcileTestEnv(t *testing.T, fc *fakeMimoCollector, enabled bool) (*Deps, *db.DB) {
	t.Helper()
	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { usageDB.Close() })
	cfg := &config.Config{Clients: map[string]config.Client{
		"mimocode": {Enabled: enabled},
	}}
	deps := &Deps{cfg: cfg, collectors: []collector.Collector{fc}}
	return deps, usageDB
}

func countByClient(t *testing.T, usageDB *db.DB, table, client string) int {
	t.Helper()
	var n int
	if err := usageDB.QueryRow(
		"SELECT COUNT(*) FROM "+table+" WHERE client=?", client).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMimoReconcileEndToEndLossless：无损重归属端到端。
// 前置库（v4 后形态）：Desktop 会话 ses-d1 与 CLI 会话 ses-c1 的全部历史行
// 挂在 MiMo Code（经生产 DAO 写入，含一条「源库已删」的历史消息
// msg-d-old）；ses-gone 的历史行存在但会话已从源库消失（不在 assignments）。
// 期望：ses-d1 全部行（含 msg-d-old）→ MiMo Desktop；ses-c1 保持 MiMo Code；
// ses-gone 原样保留（不猜不删）；token 总量守恒；pending 清除；重复执行幂等。
func TestMimoReconcileEndToEndLossless(t *testing.T) {
	fc := &fakeMimoCollector{
		assignments: []collector.MimoSessionAssignment{
			{ID: "ses-c1", Version: "0.1.14"},
			{ID: "ses-d1", Version: "desktop-abc123"},
		},
		// 当前源库可见消息：不含 msg-d-old（源库已删）。
		messages: []model.Message{
			{ID: "msg-c1", SessionID: "ses-c1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 100, TotalTokens: 10},
			{ID: "msg-d1", SessionID: "ses-d1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 200, TotalTokens: 20},
			{ID: "msg-d-old", SessionID: "ses-d1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 50, TotalTokens: 5},
		},
		sessions: []model.Session{
			{ID: "ses-c1", Client: model.ClientMiMoCode, FirstTS: 100, LastTS: 100},
			{ID: "ses-d1", Client: model.ClientMiMoDesktop, FirstTS: 50, LastTS: 200},
		},
	}
	// 当前源库已删 msg-d-old：从 fake 的消息清单中移除。
	fc.messages = fc.messages[:2]

	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	ctx := context.Background()

	// 预置历史行（v4 后形态：全部挂 MiMo Code；经生产 DAO——库中尚无
	// Desktop 身份，split trigger 不改写）。
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "msg-c1", SessionID: "ses-c1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 100, TotalTokens: 10},
		{ID: "msg-d1", SessionID: "ses-d1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 200, TotalTokens: 20},
		{ID: "msg-d-old", SessionID: "ses-d1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 50, TotalTokens: 5},
		{ID: "msg-gone", SessionID: "ses-gone", Client: model.ClientMiMoCode, Date: "2026-09-21", TS: 30, TotalTokens: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{
		{ID: "ses-c1", Client: model.ClientMiMoCode, FirstTS: 100, LastTS: 100},
		{ID: "ses-d1", Client: model.ClientMiMoCode, FirstTS: 50, LastTS: 200},
		{ID: "ses-gone", Client: model.ClientMiMoCode, FirstTS: 30, LastTS: 30},
	}); err != nil {
		t.Fatal(err)
	}
	totalBefore := countByClient(t, usageDB, "messages", model.ClientMiMoCode) // 4

	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatalf("reconciliation 失败: %v", err)
	}

	// ses-d1 全部行（含源库已删的 msg-d-old）无损迁到 Desktop。
	var c string
	for _, id := range []string{"msg-d1", "msg-d-old"} {
		if err := usageDB.QueryRow("SELECT client FROM messages WHERE id=?", id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != model.ClientMiMoDesktop {
			t.Errorf("消息 %s 应迁至 MiMo Desktop（含源库已删的历史消息）,实际 %q", id, c)
		}
	}
	if err := usageDB.QueryRow("SELECT client FROM messages WHERE id='msg-c1'").Scan(&c); err != nil || c != model.ClientMiMoCode {
		t.Errorf("CLI 会话消息应保持 MiMo Code: %q %v", c, err)
	}
	if err := usageDB.QueryRow("SELECT client FROM messages WHERE id='msg-gone'").Scan(&c); err != nil || c != model.ClientMiMoCode {
		t.Errorf("源库已消失会话的行应原样保留: %q %v", c, err)
	}
	if err := usageDB.QueryRow("SELECT client FROM sessions WHERE id='ses-d1'").Scan(&c); err != nil || c != model.ClientMiMoDesktop {
		t.Errorf("Desktop 会话元数据应迁至 MiMo Desktop: %q %v", c, err)
	}
	if err := usageDB.QueryRow("SELECT client FROM sessions WHERE id='ses-gone'").Scan(&c); err != nil || c != model.ClientMiMoCode {
		t.Errorf("源库已消失会话元数据应原样保留: %q %v", c, err)
	}
	// token 守恒：Desktop 25 + Code 20（c1 10 + gone 10？gone=7）——按行数与总量断言。
	var sumCode, sumDesktop int64
	if err := usageDB.QueryRow("SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE client=?", model.ClientMiMoCode).Scan(&sumCode); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE client=?", model.ClientMiMoDesktop).Scan(&sumDesktop); err != nil {
		t.Fatal(err)
	}
	if sumCode != 17 || sumDesktop != 25 { // Code: c1(10)+gone(7)；Desktop: d1(20)+d-old(5)
		t.Errorf("token 应守恒分列: code=%d desktop=%d, want 17/25", sumCode, sumDesktop)
	}
	var totalRows int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id IN ('ses-c1','ses-d1','ses-gone')").Scan(&totalRows); err != nil {
		t.Fatal(err)
	}
	if totalRows != 4 {
		t.Errorf("总行数 = %d, want 4（不丢行不翻倍）", totalRows)
	}
	if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
		t.Error("reconciliation 成功后 pending 应清除")
	}
	_ = totalBefore

	// 幂等：重复执行行数不变、pending 不再触发。
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&totalRows); err != nil {
		t.Fatal(err)
	}
	if totalRows != 4 {
		t.Errorf("重复执行应幂等,总行数 %d", totalRows)
	}
}

// TestMimoReconcileBatchAtomicity：批内各步骤注入失败整批回滚——不留双侧
// 并存（同会话不得在两 client 下都有消息行）、游标不越位（pending cursor_id
// 保持注入前值）、pending 保留；清注入重试可幂等完成。
func TestMimoReconcileBatchAtomicity(t *testing.T) {
	for _, step := range []string{"bypass", "rekey", "messages", "sessions", "delete", "cursor"} {
		t.Run(step, func(t *testing.T) {
			fc := &fakeMimoCollector{
				assignments: []collector.MimoSessionAssignment{{ID: "ses-d1", Version: "desktop-abc123"}},
				messages: []model.Message{
					{ID: "msg-d1", SessionID: "ses-d1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 200, TotalTokens: 20},
				},
				sessions: []model.Session{{ID: "ses-d1", Client: model.ClientMiMoDesktop, FirstTS: 200, LastTS: 200}},
			}
			deps, usageDB := mimoReconcileTestEnv(t, fc, true)
			ctx := context.Background()
			if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
				{ID: "msg-d1", SessionID: "ses-d1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 200, TotalTokens: 20},
			}); err != nil {
				t.Fatal(err)
			}
			mimoReconcileStepHook = func(s string) error {
				if s == step {
					return errors.New("injected step failure: " + s)
				}
				return nil
			}
			t.Cleanup(func() { mimoReconcileStepHook = nil })
			if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err == nil {
				t.Fatal("应因注入失败返回错误")
			}
			// 不留双侧并存：注入批次回滚后 ses-d1 仍只有 MiMo Code 行。
			var n int
			if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id='ses-d1'").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("整批回滚后该会话应恰 1 行,实际 %d", n)
			}
			if got := countByClient(t, usageDB, "messages", model.ClientMiMoDesktop); got != 0 {
				t.Fatalf("整批回滚后不应有 Desktop 行,实际 %d", got)
			}
			pending, _, cursor, err := db.MimoReconcilePending(ctx, usageDB)
			if err != nil || !pending {
				t.Fatalf("失败后 pending 应保留: %v %v", pending, err)
			}
			if cursor != "" {
				t.Fatalf("失败后游标不应推进,实际 %q", cursor)
			}
			var bypassRows int
			if err := usageDB.QueryRow(
				"SELECT COUNT(*) FROM sync_state WHERE client='mimocode' AND source=?",
				db.V5ReconcileBypassSource,
			).Scan(&bypassRows); err != nil {
				t.Fatal(err)
			}
			if bypassRows != 0 {
				t.Fatalf("失败回滚后 bypass 标志不应残留,实际 %d 行", bypassRows)
			}
			// 清注入重试幂等完成。
			mimoReconcileStepHook = nil
			if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
				t.Fatalf("重试失败: %v", err)
			}
			if got := countByClient(t, usageDB, "messages", model.ClientMiMoDesktop); got != 1 {
				t.Fatalf("重试后 Desktop 行 = %d, want 1", got)
			}
			if got := countByClient(t, usageDB, "messages", model.ClientMiMoCode); got != 0 {
				t.Fatalf("重试后 MiMo Code 行 = %d, want 0", got)
			}
			if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
				t.Fatal("重试成功后 pending 应清除")
			}
		})
	}
}

// TestMimoReconcileBatchSplittingAndResume：assignments 超过批大小上限时
// 分批推进（每批游标前进），中断后可从游标续跑，全部完成后清 pending。
func TestMimoReconcileBatchSplittingAndResume(t *testing.T) {
	const total = mimoReconcileBatchSize + 5
	var assignments []collector.MimoSessionAssignment
	for i := 0; i < total; i++ {
		v := "0.1.14"
		if i%2 == 0 {
			v = "desktop-0abc00" + string(rune('a'+i%26))
		}
		assignments = append(assignments, collector.MimoSessionAssignment{
			ID: fmt.Sprintf("ses-%04d", i), Version: v,
		})
	}
	fc := &fakeMimoCollector{assignments: assignments}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	ctx := context.Background()

	// 第一批完成后中断（第二批首批步骤注入失败）。
	var batches int
	mimoReconcileStepHook = func(step string) error {
		if step == "cursor" {
			batches++
			if batches == 2 {
				return errors.New("injected second batch failure")
			}
		}
		return nil
	}
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err == nil {
		t.Fatal("第二批应注入失败")
	}
	pending, _, cursor, _ := db.MimoReconcilePending(ctx, usageDB)
	if !pending || cursor != fmt.Sprintf("ses-%04d", mimoReconcileBatchSize-1) {
		t.Fatalf("中断后 pending=%v cursor=%q, want 第一批末游标", pending, cursor)
	}
	mimoReconcileStepHook = nil
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
		t.Fatal("续跑完成后 pending 应清除")
	}
}

// TestMimoReconcileDisabledSuspended / FailureKeepsPending：
// 未启用时 pending 悬置静默跳过；源库读取失败保留 pending 可重试。
func TestMimoReconcileDisabledSuspended(t *testing.T) {
	fc := &fakeMimoCollector{}
	deps, usageDB := mimoReconcileTestEnv(t, fc, false)
	if err := runMimoReconciliationIfPending(context.Background(), deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatalf("未启用应静默跳过: %v", err)
	}
	if pending, _, _, _ := db.MimoReconcilePending(context.Background(), usageDB); !pending {
		t.Fatal("未启用时 pending 应悬置保留")
	}
}

func TestMimoReconcileSourceFailureKeepsPending(t *testing.T) {
	fc := &fakeMimoCollector{assignErr: errors.New("source db unavailable")}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	if err := runMimoReconciliationIfPending(context.Background(), deps, usageDB, slog.Default(), nil); err == nil {
		t.Fatal("源库读取失败应返回错误")
	}
	if pending, _, _, _ := db.MimoReconcilePending(context.Background(), usageDB); !pending {
		t.Fatal("失败后 pending 应保留")
	}
	// 恢复源库后重试成功。
	fc.assignErr = nil
	fc.assignments = []collector.MimoSessionAssignment{{ID: "ses-c1", Version: "0.1.14"}}
	if err := runMimoReconciliationIfPending(context.Background(), deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if pending, _, _, _ := db.MimoReconcilePending(context.Background(), usageDB); pending {
		t.Fatal("重试成功后 pending 应清除")
	}
}

// TestMimoReconcileReverseDirectionLossless（P1-1 回归）：target=MiMoCode 的
// 逆向 re-key 必须无损——历史行仅在 MiMo Desktop、assignment 为 0.1.14/2.1.x
// 时全部迁入 MiMo Code（messages+sessions），源库已删历史消息保留、两侧
// collision 确定性合并、当前源值胜出、token/行数守恒、其他 client 不受影响、
// 批外 split trigger 照常拦截。
func TestMimoReconcileReverseDirectionLossless(t *testing.T) {
	fc := &fakeMimoCollector{
		assignments: []collector.MimoSessionAssignment{
			{ID: "ses-r1", Version: "0.1.14"},  // CLI：应从 Desktop 迁回 Code
			{ID: "ses-r2", Version: "2.1.156"}, // 2.1.x 默认归 Code
		},
		// 当前源值：ses-r1 有新消息 msg-r1-new（源值 total=88 胜出）；
		// msg-r1-old 已被源库删除但历史行在库（Desktop 侧）。
		messages: []model.Message{
			{ID: "msg-r1-new", SessionID: "ses-r1", Client: model.ClientMiMoCode, Date: "2026-09-22", TS: 500, TotalTokens: 88},
		},
		sessions: []model.Session{
			{ID: "ses-r1", Client: model.ClientMiMoCode, FirstTS: 100, LastTS: 500},
			{ID: "ses-r2", Client: model.ClientMiMoCode, FirstTS: 200, LastTS: 200},
		},
	}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	ctx := context.Background()
	// 预置（异常态，模拟修复错误分类/开发版残留）：历史行全挂 MiMo Desktop。
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "msg-r1-new", SessionID: "ses-r1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 500, TotalTokens: 20},
		{ID: "msg-r1-old", SessionID: "ses-r1", Client: model.ClientMiMoDesktop, Date: "2026-09-21", TS: 100, TotalTokens: 30},
		{ID: "msg-r2-1", SessionID: "ses-r2", Client: model.ClientMiMoDesktop, Date: "2026-09-21", TS: 200, TotalTokens: 40},
		{ID: "msg-other", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-09-21", TS: 10, TotalTokens: 999},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{
		{ID: "ses-r1", Client: model.ClientMiMoDesktop, FirstTS: 100, LastTS: 500},
		{ID: "ses-r2", Client: model.ClientMiMoDesktop, FirstTS: 200, LastTS: 200},
	}); err != nil {
		t.Fatal(err)
	}

	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatalf("reconciliation 失败: %v", err)
	}

	var c string
	var total int64
	// 逆向迁移：全部历史行（含源库已删的 msg-r1-old）进 MiMo Code。
	for _, id := range []string{"msg-r1-new", "msg-r1-old", "msg-r2-1"} {
		if err := usageDB.QueryRow("SELECT client FROM messages WHERE id=?", id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != model.ClientMiMoCode {
			t.Errorf("消息 %s 应逆向迁入 MiMo Code,实际 %q", id, c)
		}
	}
	// 两侧 collision（msg-r1-new 同 id 两侧）：合并为一条，当前源值 88 胜出。
	if err := usageDB.QueryRow("SELECT total_tokens FROM messages WHERE id='msg-r1-new'").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 88 {
		t.Errorf("当前源值应胜出: total=%d, want 88", total)
	}
	var n int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id='msg-r1-new'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("collision 应恰 1 行: %d %v", n, err)
	}
	// sessions 同步逆向。
	if err := usageDB.QueryRow("SELECT client FROM sessions WHERE id='ses-r1'").Scan(&c); err != nil || c != model.ClientMiMoCode {
		t.Errorf("ses-r1 元数据应迁入 MiMo Code: %q %v", c, err)
	}
	// token 守恒（Code 侧 88+30+40=158）+ 其他 client 不受影响。
	var sumCode int64
	if err := usageDB.QueryRow("SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE client=?", model.ClientMiMoCode).Scan(&sumCode); err != nil {
		t.Fatal(err)
	}
	if sumCode != 158 {
		t.Errorf("MiMo Code token = %d, want 158", sumCode)
	}
	if got := countByClient(t, usageDB, "messages", model.ClientMiMoDesktop); got != 0 {
		t.Errorf("Desktop 残留 %d 行, want 0", got)
	}
	if got := countByClient(t, usageDB, "messages", model.ClientClaudeCode); got != 1 {
		t.Errorf("其他 client 不受影响: %d", got)
	}
	if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
		t.Error("完成后 pending 应清除")
	}

	// 批事务提交后 bypass 必须清理；随后在同一数据库造一个已知 Desktop 会话，
	// 直接写 MiMo Code 必须仍被 split trigger 改写为 Desktop。这个断言同时锁住
	// “bypass 不残留”与“批外 trigger 照常工作”，不能用已归属 Code 的会话代替。
	if err := usageDB.QueryRow(
		"SELECT COUNT(*) FROM sync_state WHERE client='mimocode' AND source=?",
		db.V5ReconcileBypassSource,
	).Scan(&n); err != nil || n != 0 {
		t.Fatalf("完成后 bypass 标志应清理: rows=%d err=%v", n, err)
	}
	if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
		ID: "ses-trigger-check", Client: model.ClientMiMoDesktop, FirstTS: 700, LastTS: 700,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{{
		ID: "msg-trigger-check", SessionID: "ses-trigger-check", Client: model.ClientMiMoCode,
		Date: "2026-09-22", TS: 700, TotalTokens: 7,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow(
		"SELECT client FROM messages WHERE id='msg-trigger-check'",
	).Scan(&c); err != nil || c != model.ClientMiMoDesktop {
		t.Fatalf("批外 MiMo Code 写入应被 split trigger 改写为 Desktop: client=%q err=%v", c, err)
	}
}

// TestMimoReconcilePendingRearmResetsAdvancedCursor（P1-2 回归）：第一批成功
// 游标非空后，旧名写入一个字典序小于游标的新 Desktop 会话——generation 递增
// 且游标复位为空；新版续跑后该会话自动进入 Desktop，最终收敛清除 pending。
func TestMimoReconcilePendingRearmResetsAdvancedCursor(t *testing.T) {
	// 构造两批（字典序）：填充 100 个 a 系会话（无消息，re-key 空操作）+
	// ses-z9 落第二批。
	var assignments []collector.MimoSessionAssignment
	for i := 0; i < mimoReconcileBatchSize; i++ {
		assignments = append(assignments, collector.MimoSessionAssignment{
			ID: fmt.Sprintf("ses-a%03d", i), Version: "0.1.14",
		})
	}
	assignments = append(assignments, collector.MimoSessionAssignment{ID: "ses-z9", Version: "desktop-abc123"})
	fc := &fakeMimoCollector{
		assignments: assignments,
		messages: []model.Message{
			{ID: "msg-z9", SessionID: "ses-z9", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 100, TotalTokens: 10},
		},
		sessions: []model.Session{{ID: "ses-z9", Client: model.ClientMiMoDesktop, FirstTS: 100, LastTS: 100}},
	}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	ctx := context.Background()

	// 第一批成功推进游标（gen=1），第二批注入失败 → pending 保留非空游标。
	var batches int
	mimoReconcileStepHook = func(step string) error {
		if step == "cursor" {
			batches++
			if batches == 2 {
				return errors.New("injected second batch failure")
			}
		}
		return nil
	}
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err == nil {
		t.Fatal("第二批应注入失败")
	}
	mimoReconcileStepHook = nil
	pending, gen, cursor, err := db.MimoReconcilePending(ctx, usageDB)
	if err != nil || !pending || gen != 1 || cursor != fmt.Sprintf("ses-a%03d", mimoReconcileBatchSize-1) {
		t.Fatalf("第一批后 pending=%v gen=%d cursor=%q err=%v", pending, gen, cursor, err)
	}

	// 旧版回滚：旧名写入字典序更小的首现 Desktop 会话（生产 DAO——经
	// legacy trigger re-arm：gen+1、游标复位空）。
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "msg-a1", SessionID: "ses-a1", Client: model.LegacyClientXiaomiMiMoCode, Date: "2026-09-22", TS: 50, TotalTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	pending, gen, cursor, err = db.MimoReconcilePending(ctx, usageDB)
	if err != nil || !pending || gen != 2 || cursor != "" {
		t.Fatalf("re-arm 后应 gen+1 且游标复位: pending=%v gen=%d cursor=%q err=%v", pending, gen, cursor, err)
	}

	// 新版续跑：源库现含 ses-a1(desktop) 与 ses-z9，全部归 Desktop。
	fc.assignments = append(fc.assignments, collector.MimoSessionAssignment{ID: "ses-a1", Version: "desktop-abc123"})
	fc.messages = append(fc.messages, model.Message{ID: "msg-a1", SessionID: "ses-a1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 50, TotalTokens: 5})
	fc.sessions = append(fc.sessions, model.Session{ID: "ses-a1", Client: model.ClientMiMoDesktop, FirstTS: 50, LastTS: 50})
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	var c string
	if err := usageDB.QueryRow("SELECT client FROM messages WHERE id='msg-a1'").Scan(&c); err != nil || c != model.ClientMiMoDesktop {
		t.Fatalf("re-arm 后小 id 会话应自动归 Desktop: %q %v", c, err)
	}
	if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
		t.Fatal("收敛后 pending 应清除")
	}
	var n int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 2 {
		t.Fatalf("收敛后总行数 = %d, want 2（不双计）", n)
	}
}

// TestMimoReconcileClearRearmRaceNotLost（P1-2 回归）：最后一批提交与最终
// 清除之间发生旧名写入（generation 改变）——CAS 清除未命中、不得宣布完成；
// 重跑按新 generation 收敛且不双计。
func TestMimoReconcileClearRearmRaceNotLost(t *testing.T) {
	fc := &fakeMimoCollector{
		assignments: []collector.MimoSessionAssignment{{ID: "ses-b1", Version: "desktop-abc123"}},
		messages: []model.Message{
			{ID: "msg-b1", SessionID: "ses-b1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 100, TotalTokens: 10},
		},
		sessions: []model.Session{{ID: "ses-b1", Client: model.ClientMiMoDesktop, FirstTS: 100, LastTS: 100}},
	}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	ctx := context.Background()

	// 手动驱动协议序列模拟竞态窗口：round 成功（gen=1 全批提交）→ 旧名写入
	// re-arm（gen=2）→ 按 gen=1 CAS 清除应未命中。
	pending, gen, cursor, err := db.MimoReconcilePending(ctx, usageDB)
	if err != nil || !pending {
		t.Fatalf("pending 应就绪: %v %v", pending, err)
	}
	proc, dsk, genChanged, rerr := runMimoReconcileRound(ctx, fc, usageDB, slog.Default(), gen, cursor)
	if rerr != nil || genChanged || proc != 1 {
		t.Fatalf("round 失败: proc=%d dsk=%d genChanged=%v err=%v", proc, dsk, genChanged, rerr)
	}
	// 竞态窗口：最后批已提交、CAS 清除尚未执行时，旧版写入 re-arm。
	if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "msg-c1", SessionID: "ses-c1", Client: model.LegacyClientXiaomiMiMoCode, Date: "2026-09-22", TS: 60, TotalTokens: 6},
	}); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.MimoReconcileClearPendingCAS(ctx, usageDB, gen)
	if err != nil || cleared {
		t.Fatalf("gen 已变时 CAS 清除应未命中: cleared=%v err=%v", cleared, err)
	}
	if pending, gen2, cursor2, _ := db.MimoReconcilePending(ctx, usageDB); !pending || gen2 != gen+1 || cursor2 != "" {
		t.Fatalf("新 pending 不得被旧轮删除: pending=%v gen=%d cursor=%q", pending, gen2, cursor2)
	}
	// 重跑收敛（源库含新会话）。
	fc.assignments = append(fc.assignments, collector.MimoSessionAssignment{ID: "ses-c1", Version: "desktop-abc123"})
	fc.messages = append(fc.messages, model.Message{ID: "msg-c1", SessionID: "ses-c1", Client: model.ClientMiMoDesktop, Date: "2026-09-22", TS: 60, TotalTokens: 6})
	fc.sessions = append(fc.sessions, model.Session{ID: "ses-c1", Client: model.ClientMiMoDesktop, FirstTS: 60, LastTS: 60})
	if err := runMimoReconciliationIfPending(ctx, deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE client=?", model.ClientMiMoDesktop).Scan(&n); err != nil || n != 2 {
		t.Fatalf("收敛后 Desktop 行 = %d, want 2（不双计）", n)
	}
	if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
		t.Fatal("收敛后 pending 应清除")
	}
}

// TestRunCollectReconcileTriggerBoundary（P2 回归）：真实 RunCollect 触发
// 边界——显式采集其他 client 绝不触发 mimocode reconciliation（源库故障也
// 不拖累）；显式 mimocode 触发并把失败计入结果；全客户端入口触发一次。
func TestRunCollectReconcileTriggerBoundary(t *testing.T) {
	newEnv := func() (*fakeMimoCollector, *Deps, *db.DB) {
		fc := &fakeMimoCollector{assignErr: errors.New("source db unavailable")}
		deps, usageDB := mimoReconcileTestEnv(t, fc, true)
		return fc, deps, usageDB
	}

	// 显式 claude：mimocode 源库故障不影响其成功，Assignments 不被调用。
	fc, deps, usageDB := newEnv()
	cc := fixedResultCollector("claude", collector.CollectResult{
		Messages: []model.Message{{ID: "cl-1", Client: model.ClientClaudeCode, Date: "2026-06-23", TotalTokens: 5}},
	})
	deps.collectors = append(deps.collectors, cc)
	deps.cfg.Clients["claude"] = config.Client{Enabled: true}
	res := RunCollect(context.Background(), deps, usageDB, slog.Default(), io.Discard, "claude",
		collector.CollectRequest{}, true, false)
	if res.Err != nil || !res.Complete() {
		t.Fatalf("显式 claude 采集不应受 mimocode 故障拖累: %+v", res)
	}
	if fc.assignCalls != 0 {
		t.Fatalf("显式其他 client 不应触发 reconciliation: assignCalls=%d", fc.assignCalls)
	}

	// 显式 mimocode：触发并把失败计入结果。recordError=false 是 collect retry
	// 合同，不得绕过开关新增 collection_errors。
	fc2, deps2, usageDB2 := newEnv()
	res2 := RunCollect(context.Background(), deps2, usageDB2, slog.Default(), io.Discard, "mimocode",
		collector.CollectRequest{}, false, false)
	if res2.Err == nil {
		t.Fatal("显式 mimocode 应触发 reconciliation 并返回失败")
	}
	if fc2.assignCalls != 1 {
		t.Fatalf("显式 mimocode 应触发一次: assignCalls=%d", fc2.assignCalls)
	}
	// pending 保留（可重试）。
	if pending, _, _, _ := db.MimoReconcilePending(context.Background(), usageDB2); !pending {
		t.Fatal("失败后 pending 应保留")
	}
	errs2, err := db.GetErrors(usageDB2, db.ErrorFilter{Source: "mimocode", Unresolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs2) != 0 {
		t.Fatalf("recordError=false 不应新增 reconciliation 错误: %+v", errs2)
	}

	// 普通采集 recordError=true：同一失败必须进入既有 errors 模型，message
	// 用固定前缀区分；pending 仍保留供下次采集自动重试。
	fcRecord, depsRecord, usageDBRecord := newEnv()
	resRecord := RunCollect(context.Background(), depsRecord, usageDBRecord, slog.Default(), io.Discard, "mimocode",
		collector.CollectRequest{}, true, false)
	if resRecord.Err == nil || fcRecord.assignCalls != 1 {
		t.Fatalf("普通 mimocode 采集应触发一次并失败: result=%+v calls=%d", resRecord, fcRecord.assignCalls)
	}
	errsRecord, err := db.GetErrors(usageDBRecord, db.ErrorFilter{Source: "mimocode", Unresolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(errsRecord) != 1 || !strings.HasPrefix(errsRecord[0].Message, "mimocode split reconcile failed:") {
		t.Fatalf("recordError=true 应记录一条带固定前缀的错误: %+v", errsRecord)
	}

	// 全客户端入口（client==""）触发一次（源库正常时收敛）。
	fc3, deps3, usageDB3 := newEnv()
	fc3.assignErr = nil
	fc3.assignments = []collector.MimoSessionAssignment{{ID: "ses-x1", Version: "0.1.14"}}
	res3 := RunCollect(context.Background(), deps3, usageDB3, slog.Default(), io.Discard, "",
		collector.CollectRequest{}, true, false)
	if res3.Err != nil {
		t.Fatalf("全客户端入口应成功: %+v", res3)
	}
	if fc3.assignCalls != 1 {
		t.Fatalf("全客户端入口应恰触发一次: assignCalls=%d", fc3.assignCalls)
	}
	if pending, _, _, _ := db.MimoReconcilePending(context.Background(), usageDB3); pending {
		t.Fatal("收敛后 pending 应清除")
	}
}

// TestRunCollectMimoIncrementalVersionChangeRekeysAtomically（P1 回归）：pending
// 已清除后，会话 version 向任一方向变化并被普通增量采集触达时，本轮写事务必须
// 先把另一 client 的全部历史行 re-key，再 upsert 当前值并删源侧。否则
// Code→Desktop 会因 (client,id) 主键不同留下同 id 双侧行并把 token 计两次。
func TestRunCollectMimoIncrementalVersionChangeRekeysAtomically(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
	}{
		{"CodeToDesktop", model.ClientMiMoCode, model.ClientMiMoDesktop},
		{"DesktopToCode", model.ClientMiMoDesktop, model.ClientMiMoCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeMimoCollector{
				messages: []model.Message{{
					ID: "msg-current", SessionID: "ses-version-change", Client: tc.to,
					Date: "2026-09-22", TS: 200, TotalTokens: 80,
				}},
				sessions: []model.Session{{
					ID: "ses-version-change", Client: tc.to, FirstTS: 100, LastTS: 200,
				}},
			}
			deps, usageDB := mimoReconcileTestEnv(t, fc, true)
			ctx := context.Background()
			if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{
				{ID: "msg-current", SessionID: "ses-version-change", Client: tc.from, Date: "2026-09-22", TS: 200, TotalTokens: 80},
				{ID: "msg-history", SessionID: "ses-version-change", Client: tc.from, Date: "2026-09-21", TS: 100, TotalTokens: 20},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
				ID: "ses-version-change", Client: tc.from, FirstTS: 100, LastTS: 200,
			}}); err != nil {
				t.Fatal(err)
			}
			clearInitialMimoPending(t, usageDB)

			res := RunCollect(ctx, deps, usageDB, slog.Default(), io.Discard, "mimocode",
				collector.CollectRequest{Incremental: true}, true, false)
			if res.Err != nil || !res.Complete() {
				t.Fatalf("增量版本变更采集失败: %+v", res)
			}
			if fc.assignCalls != 0 {
				t.Fatalf("pending=false 的增量采集不应跑全量 assignments: %d", fc.assignCalls)
			}
			if got := countByClient(t, usageDB, "messages", tc.from); got != 0 {
				t.Fatalf("源 client %q 残留 %d 行", tc.from, got)
			}
			if got := countByClient(t, usageDB, "messages", tc.to); got != 2 {
				t.Fatalf("目标 client %q 行数=%d, want 2", tc.to, got)
			}
			var total, duplicated int64
			if err := usageDB.QueryRow(`SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE session_id='ses-version-change'`).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if err := usageDB.QueryRow(`SELECT COUNT(*) FROM (
				SELECT id FROM messages WHERE session_id='ses-version-change' GROUP BY id HAVING COUNT(*)>1
			)`).Scan(&duplicated); err != nil {
				t.Fatal(err)
			}
			if total != 100 || duplicated != 0 {
				t.Fatalf("版本变更后 token/主键应守恒: total=%d duplicated_ids=%d", total, duplicated)
			}
			if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
				t.Fatal("事务内 re-key 完成后不应凭空留下 pending")
			}
		})
	}
}

// TestRunCollectMimoFullRecheckRearmsWithoutCurrentMessages（P1 回归）：显式
// 全量采集必须在 pending 已清除时重新置位并从 assignments 全量复核。即使源库
// 已无可采消息、collector 当前结果为空，只要 session 仍存在且 version 已改变，
// token-usage 保存的历史行也应双向无损 re-key；这才使 collect all 成为真实复核。
func TestRunCollectMimoFullRecheckRearmsWithoutCurrentMessages(t *testing.T) {
	for _, tc := range []struct {
		name, version, from, to string
	}{
		{"CodeToDesktop", "desktop-abc123", model.ClientMiMoCode, model.ClientMiMoDesktop},
		{"DesktopToCode", "0.1.14", model.ClientMiMoDesktop, model.ClientMiMoCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeMimoCollector{assignments: []collector.MimoSessionAssignment{{
				ID: "ses-history-only", Version: tc.version,
			}}}
			deps, usageDB := mimoReconcileTestEnv(t, fc, true)
			ctx := context.Background()
			if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{{
				ID: "msg-history-only", SessionID: "ses-history-only", Client: tc.from,
				Date: "2026-09-21", TS: 100, TotalTokens: 80,
			}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
				ID: "ses-history-only", Client: tc.from, FirstTS: 100, LastTS: 100,
			}}); err != nil {
				t.Fatal(err)
			}
			clearInitialMimoPending(t, usageDB)

			res := RunCollect(ctx, deps, usageDB, slog.Default(), io.Discard, "mimocode",
				collector.CollectRequest{Dates: nil}, true, false)
			if res.Err != nil || !res.Complete() {
				t.Fatalf("显式全量复核失败: %+v", res)
			}
			if fc.assignCalls != 1 {
				t.Fatalf("显式全量复核应恰读取一次 assignments: %d", fc.assignCalls)
			}
			if fc.collectCalls != 2 {
				t.Fatalf("应分别执行 reconciliation 全量读取和常规 collect all: %d", fc.collectCalls)
			}
			if got := countByClient(t, usageDB, "messages", tc.from); got != 0 {
				t.Fatalf("源 client %q 残留 %d 行", tc.from, got)
			}
			if got := countByClient(t, usageDB, "messages", tc.to); got != 1 {
				t.Fatalf("目标 client %q 行数=%d, want 1", tc.to, got)
			}
			var total int64
			if err := usageDB.QueryRow(`SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE id='msg-history-only'`).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if total != 80 {
				t.Fatalf("历史消息 re-key 后 token=%d, want 80", total)
			}
			if pending, _, _, _ := db.MimoReconcilePending(ctx, usageDB); pending {
				t.Fatal("显式全量复核成功后 pending 应清除")
			}
		})
	}
}

// TestMimoReconcileAssignmentSnapshotDefinesBatchClient：Assignments 与 Collect
// 是两次源库读取；若 version 恰在两次读取间变化，本轮必须以 assignment 快照
// 为唯一目标，不能把 Collect 自带的相反 client 写入后又当 source 删除。下一次
// 普通采集会通过事务内 re-key 按更新后的 version 再收敛。
func TestMimoReconcileAssignmentSnapshotDefinesBatchClient(t *testing.T) {
	fc := &fakeMimoCollector{
		assignments: []collector.MimoSessionAssignment{{ID: "ses-snapshot", Version: "0.1.14"}},
		// 故意模拟两次读取间 version 变成 Desktop；本轮仍应按 assignment 的 Code。
		messages: []model.Message{{
			ID: "msg-snapshot", SessionID: "ses-snapshot", Client: model.ClientMiMoDesktop,
			Date: "2026-09-22", TS: 100, TotalTokens: 9,
		}},
		sessions: []model.Session{{
			ID: "ses-snapshot", Client: model.ClientMiMoDesktop, FirstTS: 100, LastTS: 100,
		}},
	}
	deps, usageDB := mimoReconcileTestEnv(t, fc, true)
	if err := runMimoReconciliationIfPending(context.Background(), deps, usageDB, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	var messageClient, sessionClient string
	if err := usageDB.QueryRow(`SELECT client FROM messages WHERE id='msg-snapshot'`).Scan(&messageClient); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow(`SELECT client FROM sessions WHERE id='ses-snapshot'`).Scan(&sessionClient); err != nil {
		t.Fatal(err)
	}
	if messageClient != model.ClientMiMoCode || sessionClient != model.ClientMiMoCode {
		t.Fatalf("本轮应统一按 assignment target 落库: message=%q session=%q", messageClient, sessionClient)
	}
}
