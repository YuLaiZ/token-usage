package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v7 升级合同（messages.duration_ms） ===
//
// 合同：currentSchemaVersion=7；migrateV7 单事务 ALTER TABLE messages 加
// duration_ms INTEGER NOT NULL DEFAULT 0 一列，最后 PRAGMA user_version=7。
// 存量行保持 0（未记录），不做数据回填（重采自然补齐是既定范围选择）。
// 任一步失败整体回滚保持 v6。v7 语句为 migration-local 冻结字面量，
// 不随运行期 DAO 演进。

// messagesDurationColumn 查询 messages 表 duration_ms 列的默认值。
func messagesDurationColumn(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) (exists bool, dflt string) {
	t.Helper()
	var d string
	err := q.QueryRow(`SELECT COALESCE(dflt_value,'') FROM pragma_table_info('messages') WHERE name='duration_ms'`).Scan(&d)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return false, ""
		}
		t.Fatalf("query table_info(messages): %v", err)
	}
	return true, d
}

// TestFreshDBReachesV7：全新库 Open 后 user_version=7、duration_ms 列就绪、
// 非空约束、默认 0；不写该列的新消息行取 0。
func TestFreshDBReachesV7(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 7 {
		t.Fatalf("fresh DB user_version = %d, want 7", got)
	}
	exists, dflt := messagesDurationColumn(t, d)
	if !exists {
		t.Fatal("fresh DB messages 应含 duration_ms 列")
	}
	if dflt != "0" {
		t.Fatalf("duration_ms 默认值应为 0,实际 %q", dflt)
	}
}

// buildV6RawDB 造一个停在 v6 的原始库（v1..v6 顺序迁移，模拟旧版二进制
// 写入的存量库）。
func buildV6RawDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v6.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []func(*sql.DB) error{migrateV1, migrateV2, migrateV3, migrateV4, migrateV5, migrateV6} {
		if err := m(raw); err != nil {
			t.Fatalf("seed migration: %v", err)
		}
	}
	return raw
}

// TestV6UpgradePreservesData：v6 库升级到 v7 后存量行零数据变化
// （duration_ms 全 0），token 列原样保留。
func TestV6UpgradePreservesData(t *testing.T) {
	raw := buildV6RawDB(t)
	defer raw.Close()
	// 存量行按 v6 布局（无 duration_ms 列）预置，模拟旧版二进制的写入。
	if _, err := raw.Exec(`INSERT INTO messages (id,session_id,client,date,ts,model,provider,
input_tokens,fresh_input_tokens,output_tokens,total_tokens)
VALUES ('m1','s1',?,'2026-10-09',1000,'m','Anthropic',10,10,20,30)`,
		model.ClientClaudeCode); err != nil {
		t.Fatal(err)
	}
	if err := migrateV7(raw); err != nil {
		t.Fatalf("migrateV7: %v", err)
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 7 {
		t.Fatalf("user_version = %d, want 7", v)
	}
	var duration, total int64
	if err := raw.QueryRow(`SELECT duration_ms, total_tokens FROM messages WHERE client=? AND id=?`,
		model.ClientClaudeCode, "m1").Scan(&duration, &total); err != nil {
		t.Fatal(err)
	}
	if duration != 0 || total != 30 {
		t.Fatalf("升级后 duration=%d total=%d, want 0/30（零数据变化）", duration, total)
	}
}

// TestMigrateV7FailureKeepsV6：中段注入失败整体回滚——库保持 v6：列不残留、
// 版本号不前进；清除注入后重试成功。
func TestMigrateV7FailureKeepsV6(t *testing.T) {
	raw := buildV6RawDB(t)
	defer raw.Close()
	migrateV7PostAlterHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV7PostAlterHook = nil })
	if err := migrateV7(raw); err == nil {
		t.Fatal("migrateV7 应因中段注入失败")
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 6 {
		t.Fatalf("失败后 user_version = %d, want 6", v)
	}
	if exists, _ := messagesDurationColumn(t, raw); exists {
		t.Fatal("失败后 duration_ms 列不应残留（ALTER 随事务回滚）")
	}
	migrateV7PostAlterHook = nil
	if err := migrateV7(raw); err != nil {
		t.Fatalf("重试 migrateV7 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 7 {
		t.Fatalf("重试后 user_version = %d, want 7", v)
	}
}

// TestV7PersistsDuration：UpsertMessages 落库 duration_ms 且重放覆盖。
func TestV7PersistsDuration(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	m := model.Message{
		ID: "m1", SessionID: "s1", Client: model.ClientClaudeCode,
		Date: "2026-10-09", TS: 1000, TotalTokens: 5, OutputTokens: 5,
		DurationMS: 4200,
	}
	if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := d.QueryRow(`SELECT duration_ms FROM messages WHERE client=? AND id=?`,
		model.ClientClaudeCode, "m1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 4200 {
		t.Fatalf("duration_ms = %d, want 4200", got)
	}
	// 重放（UPSERT）覆盖为新值：时长补齐语义。
	m.DurationMS = 5000
	if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT duration_ms FROM messages WHERE client=? AND id=?`,
		model.ClientClaudeCode, "m1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 5000 {
		t.Fatalf("重放后 duration_ms = %d, want 5000（UPSERT 覆盖）", got)
	}
}
