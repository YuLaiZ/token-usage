package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v5 升级合同（MiMo Desktop 拆分能力） ===
//
// 合同：currentSchemaVersion=5；migrateV5 单事务执行 v5MigrationStatements
//（冻结字面量）——DROP v4 版 legacy trigger、重建增强版（旧名写入按 v4 冻结
// 语义改写为 'MiMo Code' + 同事务重置 reconciliation pending）、创建 split
// trigger（'MiMo Code' 写入且 session 已知 Desktop 时改写为 'MiMo Desktop'）、
// 写入初始 pending 行、最后 PRAGMA user_version=5。任一步失败整体回滚保持
// v4。回滚兼容核心：v0.1.10 旧名写入经 v4→v5 两级 trigger 链直达
// 'MiMo Desktop'（session 已知时）并重置 pending（首现 session 自愈入口）。

const (
	v5TrigLegacyMessages = "messages_legacy_mimo_client_rewrite"
	v5TrigLegacySessions = "sessions_legacy_mimo_client_rewrite"
	v5TrigSplitMessages  = "messages_mimo_split_rewrite"
	v5TrigSplitSessions  = "sessions_mimo_split_rewrite"
)

// TestV5MigrationStatementsFrozenStructure 锁定 v5 migration-local 字面量的
// 结构合同：语句数/顺序、legacy trigger 的 generation re-arm、split trigger
// 的事务内 bypass，以及版本号最后提升。行为语义另由升级、回滚与 reconciliation
// 测试覆盖；本测试防止后续维护把这些并发保护从冻结 DDL 中意外删掉。
func TestV5MigrationStatementsFrozenStructure(t *testing.T) {
	if len(v5MigrationStatements) != 8 {
		t.Fatalf("v5MigrationStatements 条数 = %d, want 8", len(v5MigrationStatements))
	}
	for i, want := range []struct {
		prefix   string
		contains string
	}{
		{"DROP TRIGGER IF EXISTS messages_legacy_mimo_client_rewrite", "messages_legacy_mimo_client_rewrite"},
		{"DROP TRIGGER IF EXISTS sessions_legacy_mimo_client_rewrite", "sessions_legacy_mimo_client_rewrite"},
		{"CREATE TRIGGER IF NOT EXISTS messages_legacy_mimo_client_rewrite", "ON CONFLICT(client,source) DO UPDATE SET"},
		{"CREATE TRIGGER IF NOT EXISTS sessions_legacy_mimo_client_rewrite", "ON CONFLICT(client,source) DO UPDATE SET"},
		{"CREATE TRIGGER IF NOT EXISTS messages_mimo_split_rewrite", "source='split_reconcile_bypass'"},
		{"CREATE TRIGGER IF NOT EXISTS sessions_mimo_split_rewrite", "source='split_reconcile_bypass'"},
		{"INSERT INTO sync_state", "cursor_value = cursor_value + 1"},
		{"PRAGMA user_version = 5", "PRAGMA user_version = 5"},
	} {
		stmt := v5MigrationStatements[i]
		if !strings.HasPrefix(stmt, want.prefix) {
			t.Errorf("v5 语句[%d] 应以 %q 开头", i, want.prefix)
		}
		if !strings.Contains(stmt, want.contains) {
			t.Errorf("v5 语句[%d] 应含 %q", i, want.contains)
		}
	}
	for _, i := range []int{2, 3} {
		stmt := v5MigrationStatements[i]
		for _, frag := range []string{
			"cursor_value = cursor_value + 1",
			"cursor_id = ''",
			"SELECT RAISE(IGNORE)",
		} {
			if !strings.Contains(stmt, frag) {
				t.Errorf("legacy trigger 语句[%d] 应含 generation 合同片段 %q", i, frag)
			}
		}
	}
	for _, i := range []int{4, 5} {
		stmt := v5MigrationStatements[i]
		for _, frag := range []string{
			"AND NOT EXISTS(SELECT 1 FROM sync_state WHERE client='mimocode' AND source='split_reconcile_bypass')",
			"client = 'MiMo Desktop'",
			"SELECT RAISE(IGNORE)",
		} {
			if !strings.Contains(stmt, frag) {
				t.Errorf("split trigger 语句[%d] 应含冻结合同片段 %q", i, frag)
			}
		}
	}
}

func v5TriggerExists(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}, name string) bool {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?", name).Scan(&n); err != nil {
		t.Fatalf("query trigger %s: %v", name, err)
	}
	return n > 0
}

// TestFreshDBReachesV5：全新库 Open 后 user_version=5、四个 trigger 就绪、
// pending 行就绪（首笔起即具备拆分与自动重归属能力）。
func TestFreshDBReachesV5(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 5 {
		t.Fatalf("fresh DB user_version = %d, want 5", got)
	}
	for _, name := range []string{v5TrigLegacyMessages, v5TrigLegacySessions, v5TrigSplitMessages, v5TrigSplitSessions} {
		if !v5TriggerExists(t, d, name) {
			t.Fatalf("fresh DB 应含 trigger %s", name)
		}
	}
	pending, _, cursor, err := MimoReconcilePending(context.Background(), d)
	if err != nil || !pending {
		t.Fatalf("fresh DB 应有 reconciliation pending: pending=%v err=%v", pending, err)
	}
	if cursor != "" {
		t.Fatalf("fresh pending 游标应为空串,实际 %q", cursor)
	}
}

// TestV3UpgradeChainReachesV5WithPending：真实 v3 库经 Open 一次走完
// v3→v4→v5：legacy 行折叠为 MiMo Code（v4 合同不变），pending 就绪。
func TestV3UpgradeChainReachesV5WithPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3to5.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer upgraded.Close()
	if got := userVersion(t, upgraded); got != 5 {
		t.Fatalf("user_version = %d, want 5", got)
	}
	if n := v4RowsByClient(t, upgraded, "messages", model.ClientMiMoCode); n != 3 {
		t.Fatalf("v4 折叠合同应保持: MiMo Code 行 = %d, want 3", n)
	}
	if n := v4RowsByClient(t, upgraded, "messages", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("legacy 残留 %d 行", n)
	}
	pending, _, _, err := MimoReconcilePending(context.Background(), upgraded)
	if err != nil || !pending {
		t.Fatalf("v5 后 pending 应就绪: %v %v", pending, err)
	}
}

// TestMigrateV5FailureKeepsV4：中段注入失败（全部语句后、版本号前）整体回滚
// ——库保持 v4：split trigger 不存在、legacy trigger 保持 v4 版（无 pending
// 副作用语句——通过「无 pending 行」验证其未增强生效）、版本号不前进；
// 清除注入后重试成功。
func TestMigrateV5FailureKeepsV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail5.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// 基线：直调 migrateV4 到 v4（migrateV5 的失败保持目标）。
	if err := migrateV4(raw); err != nil {
		t.Fatalf("baseline migrateV4: %v", err)
	}

	migrateV5PostStatementsHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV5PostStatementsHook = nil })
	if err := migrateV5(raw); err == nil {
		t.Fatal("migrateV5 应因中段注入失败")
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Fatalf("失败后 user_version = %d, want 4", v)
	}
	for _, name := range []string{v5TrigSplitMessages, v5TrigSplitSessions} {
		if v5TriggerExists(t, raw, name) {
			t.Fatalf("失败后 split trigger %s 不应残留", name)
		}
	}
	// v4 版 legacy trigger 随 DROP 回滚仍在（v4 合同不受 v5 失败影响）。
	if !v5TriggerExists(t, raw, v5TrigLegacyMessages) {
		t.Fatal("失败后 v4 版 legacy trigger 应仍在（DROP 随事务回滚）")
	}
	var n int
	if err := raw.QueryRow("SELECT COUNT(*) FROM sync_state WHERE source=?", V5ReconcileSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("失败后 pending 行不应残留")
	}

	migrateV5PostStatementsHook = nil
	if err := migrateV5(raw); err != nil {
		t.Fatalf("重试 migrateV5 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("重试后 user_version = %d, want 5", v)
	}
	if !v5TriggerExists(t, raw, v5TrigSplitMessages) || !v5TriggerExists(t, raw, v5TrigSplitSessions) {
		t.Fatal("重试后 split trigger 应存在")
	}
}

// v5LegacyWriteFixture 构造「已拆分」前提：一个 Desktop 会话在 MiMo Desktop
// 下已有消息与会话行（经生产 DAO 写入），供回滚改写测试使用。
func v5LegacyWriteFixture(t *testing.T) *DB {
	path := filepath.Join(t.TempDir(), "rollback5.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if got := userVersion(t, d); got != 5 {
		t.Fatalf("user_version = %d, want 5", got)
	}
	// 用生产 DAO 写入 Desktop 会话的既有身份（模拟 reconciliation 已完成）。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "ses-known-desktop", Client: model.ClientMiMoDesktop,
		Directory: "/d", Project: "proj-D", Title: "desktop-work", FirstTS: 100, LastTS: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "msg-known-desktop", SessionID: "ses-known-desktop", Client: model.ClientMiMoDesktop,
		Date: "2026-09-22", TS: 150, TotalTokens: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	// 消除初始 pending（模拟 reconciliation 已完成删除），验证旧名写入的
	// 重置行为独立于初始 pending。
	if cleared, err := MimoReconcileClearPendingCAS(context.Background(), d, 1); err != nil || !cleared {
		t.Fatalf("clear pending: cleared=%v err=%v", cleared, err)
	}
	return d
}

// TestSplitTriggerRewritesDirectMiMoCodeWrite：路径一（v4-only 中间版直写
// 'MiMo Code'）：session 已知 Desktop 时被 split trigger 改写为 Desktop——
// 同 id 单行、upsert 语义逐字段（早 ts 归因、token 覆盖、router_* 非空才
// 覆盖）、不置 pending、幂等。
func TestSplitTriggerRewritesDirectMiMoCodeWrite(t *testing.T) {
	d := v5LegacyWriteFixture(t)
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "msg-known-desktop", SessionID: "ses-known-desktop", Client: model.ClientMiMoCode,
		Date: "2026-09-22", TS: 120, TotalTokens: 99, RouterProvider: "cc_switch",
	}}); err != nil {
		t.Fatal(err)
	}
	var client string
	var ts int64
	var total int64
	var routerProvider string
	if err := d.QueryRow(`SELECT client,ts,total_tokens,router_provider FROM messages WHERE id='msg-known-desktop'`).Scan(&client, &ts, &total, &routerProvider); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoDesktop {
		t.Fatalf("直写 MiMo Code 应被 split trigger 改写为 Desktop,实际 %q", client)
	}
	if ts != 120 || total != 99 || routerProvider != "cc_switch" {
		t.Errorf("upsert 语义应保持: ts=%d total=%d router=%q", ts, total, routerProvider)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='msg-known-desktop'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同 id 应恰 1 行,实际 %d", n)
	}
	// split trigger 不置 pending（正常 CLI 写入高频）。
	if pending, _, _, _ := MimoReconcilePending(context.Background(), d); pending {
		t.Fatal("split trigger 不应置 pending")
	}
}

// TestLegacyWriteChainV4ToV5ToDesktop：路径二（真实 v0.1.10 回滚链）：生产
// DAO 写旧名 →(v4 语义)→ 'MiMo Code' →(split)→ 'MiMo Desktop'，同 id 单行、
// sessions 同构、幂等，且**重置 pending**（首现 session 自愈入口——即使初始
// pending 已删除）。全部经生产 DAO、默认 recursive_triggers=OFF。
func TestLegacyWriteChainV4ToV5ToDesktop(t *testing.T) {
	d := v5LegacyWriteFixture(t)

	// 已知 Desktop 会话的新消息（旧名写入）。
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "msg-known-desktop", SessionID: "ses-known-desktop", Client: model.LegacyClientXiaomiMiMoCode,
		Date: "2026-09-22", TS: 180, TotalTokens: 30,
	}}); err != nil {
		t.Fatal(err)
	}
	var client string
	var total int64
	if err := d.QueryRow(`SELECT client,total_tokens FROM messages WHERE id='msg-known-desktop'`).Scan(&client, &total); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoDesktop || total != 30 {
		t.Fatalf("旧名写入应经 v4→v5 链直达 Desktop 且 token 覆盖: client=%q total=%d", client, total)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='msg-known-desktop'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同 id 应恰 1 行,实际 %d", n)
	}

	// sessions 旧名写入：同链折叠进既有 Desktop 行，title 保留、区间收窄。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "ses-known-desktop", Client: model.LegacyClientXiaomiMiMoCode,
		Directory: "/d2", Project: "proj-D2", Title: "", FirstTS: 300, LastTS: 50,
	}}); err != nil {
		t.Fatal(err)
	}
	var sClient, title string
	var first, last int64
	if err := d.QueryRow(`SELECT client,title,first_ts,last_ts FROM sessions WHERE id='ses-known-desktop'`).Scan(&sClient, &title, &first, &last); err != nil {
		t.Fatal(err)
	}
	if sClient != model.ClientMiMoDesktop || title != "desktop-work" || first != 100 || last != 200 {
		t.Fatalf("sessions 链语义错误: client=%q title=%q first=%d last=%d", sClient, title, first, last)
	}

	// 旧名写入重置 pending。
	pending, _, _, err := MimoReconcilePending(context.Background(), d)
	if err != nil || !pending {
		t.Fatalf("旧名写入应重置 pending: %v %v", pending, err)
	}

	// 幂等：重复旧名写入行数不变。
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "msg-known-desktop", SessionID: "ses-known-desktop", Client: model.LegacyClientXiaomiMiMoCode,
		Date: "2026-09-22", TS: 180, TotalTokens: 30,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='msg-known-desktop'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重复写入应幂等,实际 %d 行", n)
	}
}

// TestLegacyWriteFirstSeenDesktopSessionPending：回滚期间首现的新 Desktop
// 会话（库中无 Desktop 身份）：暂落 MiMo Code（可接受边界）且旧名写入已置
// pending——新版恢复后自动从源库重归属（engine 测试覆盖自愈路径）。
func TestLegacyWriteFirstSeenDesktopSessionPending(t *testing.T) {
	d := v5LegacyWriteFixture(t)
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "msg-new-session", SessionID: "ses-brand-new", Client: model.LegacyClientXiaomiMiMoCode,
		Date: "2026-09-22", TS: 500, TotalTokens: 77,
	}}); err != nil {
		t.Fatal(err)
	}
	var client string
	if err := d.QueryRow(`SELECT client FROM messages WHERE id='msg-new-session'`).Scan(&client); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoCode {
		t.Fatalf("首现会话应暂落 MiMo Code（边界）,实际 %q", client)
	}
	pending, _, _, err := MimoReconcilePending(context.Background(), d)
	if err != nil || !pending {
		t.Fatalf("首现旧名写入应重置 pending 供新版自愈: %v %v", pending, err)
	}
}
