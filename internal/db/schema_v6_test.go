package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v6 升级合同（sessions.title_source + title_index_ts） ===
//
// 合同：currentSchemaVersion=6；migrateV6 单事务 ALTER TABLE sessions 加
// title_source TEXT NOT NULL DEFAULT '' 与 title_index_ts INTEGER NOT NULL
// DEFAULT 0 两列，最后 PRAGMA user_version=6。存量行保持零值（来源未知/无
// 已应用索引记录），不做数据回填（运行期标题同步步骤负责覆盖命中行并回填
// 存量空标题子线程兜底）。任一步失败整体回滚保持 v5。

// sessionsTitleSourceColumns 查询 sessions 表是否已有 title_source 列
// （不存在列时 sqlite 报 no such column，测试借以区分迁移是否到位）。
func sessionsTitleSourceColumns(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) (exists bool, dflt string) {
	t.Helper()
	var d string
	err := q.QueryRow(`SELECT COALESCE(dflt_value,'') FROM pragma_table_info('sessions') WHERE name='title_source'`).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		t.Fatalf("query table_info(sessions): %v", err)
	}
	return true, d
}

// TestFreshDBReachesV6：全新库 Open 后 user_version=6、title_source 列就绪且
// 非空约束、默认空字符串；新插入的会话行不写该列时取空字符串。
func TestFreshDBReachesV6(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 6 {
		t.Fatalf("fresh DB user_version = %d, want 6", got)
	}
	exists, dflt := sessionsTitleSourceColumns(t, d)
	if !exists {
		t.Fatal("fresh DB sessions 应含 title_source 列")
	}
	if dflt != "''" {
		t.Fatalf("title_source 默认值应为 '' 字面量,实际 %q", dflt)
	}
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "s1", Client: model.ClientClaudeCode, Title: "t", FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := d.QueryRow(`SELECT title_source FROM sessions WHERE id='s1'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "" {
		t.Fatalf("非 Codex 行未写来源时应取列默认 '',实际 %q", src)
	}
}

// TestV5UpgradeChainReachesV6：真实 v5 库（v3 dump 经 Open 走完链）升级到 v6，
// 存量标题原样保留、title_source 为空字符串（未知，等价可覆盖地位）。
func TestV5UpgradeChainReachesV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5to6.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	// 先以直调链固定到 v5（隔离 v6 的注入影响）。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV1(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV2(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV3(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV4(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV5(raw); err != nil {
		t.Fatal(err)
	}
	// v5 库基线写入一条带标题的存量行（v5 结构无 title_source 列）。
	if _, err := raw.Exec(`INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts)
VALUES ('v5row',?,?,?, '历史标题', '', 10, 20)`,
		model.ClientCodexApp, "/d", "proj"); err != nil {
		t.Fatal(err)
	}
	var baseline int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != 5 {
		t.Fatalf("baseline user_version = %d, want 5", baseline)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	defer upgraded.Close()
	if got := userVersion(t, upgraded); got != 6 {
		t.Fatalf("升级后 user_version = %d, want 6", got)
	}
	var title, src string
	var ts int64
	if err := upgraded.QueryRow(`SELECT title,COALESCE(title_source,''),COALESCE(title_index_ts,0) FROM sessions WHERE id='v5row'`).Scan(&title, &src, &ts); err != nil {
		t.Fatal(err)
	}
	if title != "历史标题" || src != "" || ts != 0 {
		t.Fatalf("存量行应保留标题且来源/索引时间未知: title=%q source=%q ts=%d", title, src, ts)
	}
}

// TestMigrateV6FailureKeepsV5：中段注入失败（ALTER 后、版本号前）整体回滚
// ——库保持 v5：列不残留、版本号不前进；清除注入后重试成功。
func TestMigrateV6FailureKeepsV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail6.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := migrateV1(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV2(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV3(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV4(raw); err != nil {
		t.Fatal(err)
	}
	if err := migrateV5(raw); err != nil {
		t.Fatal(err)
	}

	migrateV6PostAlterHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV6PostAlterHook = nil })
	if err := migrateV6(raw); err == nil {
		t.Fatal("migrateV6 应因中段注入失败")
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("失败后 user_version = %d, want 5", v)
	}
	if exists, _ := sessionsTitleSourceColumns(t, raw); exists {
		t.Fatal("失败后 title_source 列不应残留（ALTER 随事务回滚）")
	}
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name IN ('title_source','title_index_ts')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("失败后两列都不应残留（ALTER 随事务回滚）")
	}

	migrateV6PostAlterHook = nil
	if err := migrateV6(raw); err != nil {
		t.Fatalf("重试 migrateV6 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 6 {
		t.Fatalf("重试后 user_version = %d, want 6", v)
	}
}
