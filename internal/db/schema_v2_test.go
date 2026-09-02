package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v2 升级合同（file_scan_log 重建为 startup 跳过门状态表） ===
//
// 合同：currentSchemaVersion=2；migrateV2 单事务 DROP 旧 file_scan_log
// （死表无生产数据）+ 重建新结构（主键 (client, file_path)，列含 file_identity /
// mtime_ns / file_size / parser_version / updated_at）；失败则库保持 v1 可重试；
// v1 库升级后门为空表（首轮 catch-up 必全量）。

// openFreshDB 打开全新库（经 Open 内的 ensureSchema 直达当前 schema 版本）。
func openFreshDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fileScanLogColumns 返回 file_scan_log 的列布局（比对升级前后结构用）。
func fileScanLogColumns(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.Query("SELECT name FROM pragma_table_info('file_scan_log') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	return cols
}

func userVersion(t *testing.T, d *DB) int {
	t.Helper()
	var v int
	if err := d.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSchemaCurrentVersionIsTwo：当前 schema 版本常量为 2（file_scan_log 门表合同）。
func TestSchemaCurrentVersionIsTwo(t *testing.T) {
	if currentSchemaVersion != 2 {
		t.Fatalf("currentSchemaVersion = %d, want 2", currentSchemaVersion)
	}
}

// TestFreshDBReachesV2：全新库 Open 后 user_version=2 且 file_scan_log 为新结构
// （主键 (client, file_path) + file_identity/mtime_ns/file_size/parser_version 列）。
func TestFreshDBReachesV2(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 2 {
		t.Fatalf("fresh DB user_version = %d, want 2", got)
	}
	cols := fileScanLogColumns(t, d)
	want := []string{"client", "file_path", "file_identity", "mtime_ns", "file_size", "parser_version", "updated_at"}
	if len(cols) != len(want) {
		t.Fatalf("file_scan_log columns = %v, want %v", cols, want)
	}
	for i, c := range want {
		if cols[i] != c {
			t.Fatalf("file_scan_log columns = %v, want %v", cols, want)
		}
	}
	// 复合主键 (client, file_path)。
	var pkCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM pragma_table_info('file_scan_log') WHERE pk>0").Scan(&pkCount); err != nil {
		t.Fatal(err)
	}
	if pkCount != 2 {
		t.Fatalf("primary key column count = %d, want 2 (client, file_path)", pkCount)
	}
	rows, err := d.Query("SELECT name FROM pragma_table_info('file_scan_log') WHERE pk=1 OR pk=2 ORDER BY pk")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var first, second string
	for rows.Next() {
		if first == "" {
			rows.Scan(&first)
		} else {
			rows.Scan(&second)
		}
	}
	if first != "client" || second != "file_path" {
		t.Fatalf("primary key = (%s, %s), want (client, file_path)", first, second)
	}
}

// TestV1UpgradeRebuildsFileScanLog：v1 库（含旧结构 file_scan_log 与一行数据）升级后
// 表结构为新布局、user_version=2、门记录为空（首轮 catch-up 必全量）。
func TestV1UpgradeRebuildsFileScanLog(t *testing.T) {
	// 构造真实 v1 形态库文件（v1 列布局 + 一行旧数据 + user_version=1），
	// 经 Open 触发 migrateV2 升级后校验结构与数据清理。
	path := filepath.Join(t.TempDir(), "downgrade.db")
	if err := sqlDumpV1(t, path); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	defer upgraded.Close()
	if got := userVersion(t, upgraded); got != 2 {
		t.Fatalf("upgraded user_version = %d, want 2", got)
	}
	cols := fileScanLogColumns(t, upgraded)
	want := []string{"client", "file_path", "file_identity", "mtime_ns", "file_size", "parser_version", "updated_at"}
	if len(cols) != len(want) {
		t.Fatalf("upgraded columns = %v, want %v", cols, want)
	}
	// 旧 v1 行不得残留（死表数据无保留价值）。
	var n int
	if err := upgraded.QueryRow("SELECT COUNT(*) FROM file_scan_log").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("file_scan_log rows = %d, want 0 (v1 死表数据被 DROP)", n)
	}
}

// TestMigrateV2FailureKeepsV1：migrateV2 在 DROP 成功之后、后续语句之前注入
// 确定性失败（migrateV2PostDropHook），整个迁移事务回滚——库保持 v1：
// user_version=1、旧表结构（v1 列布局）与旧数据原样恢复；清除注入后重试升至 v2。
// 区分度：若 DROP 不在事务内（错误实现），注入失败时旧表已被删，
// 「旧表结构与旧行仍在」的断言会失败。
func TestMigrateV2FailureKeepsV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.db")
	if err := sqlDumpV1(t, path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	migrateV2PostDropHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV2PostDropHook = nil })

	if err := migrateV2(db); err == nil {
		t.Fatal("migrateV2 应因中段注入失败")
	}

	// 库保持 v1：版本号、旧表结构（v1 列布局含 session_id 等）与旧数据原样。
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("失败后 user_version = %d, want 1 (事务回滚保持 v1)", v)
	}
	cols := fileScanLogColumns(t, &DB{db: db})
	v1Columns := []string{"file_path", "session_id", "client", "source_type", "last_modified", "file_size", "last_line_offset", "scanned_at"}
	if len(cols) != len(v1Columns) {
		t.Fatalf("失败后 file_scan_log 列布局应为 v1: %v", cols)
	}
	for i, c := range v1Columns {
		if cols[i] != c {
			t.Fatalf("失败后 file_scan_log 列布局应为 v1（第 %d 列 %q != %q）: %v", i, cols[i], c, cols)
		}
	}
	var oldRow string
	if err := db.QueryRow("SELECT file_path FROM file_scan_log WHERE client='claude'").Scan(&oldRow); err != nil {
		t.Fatalf("v1 旧数据应随事务回滚恢复: %v", err)
	}
	if oldRow != "/old/a.jsonl" {
		t.Fatalf("v1 旧数据被改动: %q", oldRow)
	}

	// 清除注入后重试成功。
	migrateV2PostDropHook = nil
	if err := migrateV2(db); err != nil {
		t.Fatalf("重试 migrateV2 失败: %v", err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("重试后 user_version = %d, want 2", v)
	}
}

// sqlDumpV1 生成真实 v1 形态库：v1 全部表结构 + 旧 file_scan_log（v1 列布局）+
// user_version=1。用原生 SQL 直建（不依赖当前包内常量，锁定「已发布 v1 结构」事实）。
func sqlDumpV1(t *testing.T, path string) error {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer raw.Close()
	stmts := []string{
		`CREATE TABLE messages (id TEXT NOT NULL, client TEXT NOT NULL, date TEXT NOT NULL, ts INTEGER NOT NULL, PRIMARY KEY (client, id))`,
		`CREATE TABLE file_scan_log (
			file_path       TEXT PRIMARY KEY,
			session_id      TEXT NOT NULL DEFAULT '',
			client          TEXT NOT NULL,
			source_type     TEXT NOT NULL DEFAULT 'jsonl',
			last_modified   INTEGER NOT NULL,
			file_size       INTEGER NOT NULL,
			last_line_offset INTEGER NOT NULL DEFAULT 0,
			scanned_at      TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT INTO file_scan_log (file_path, session_id, client, last_modified, file_size) VALUES ('/old/a.jsonl', 's1', 'claude', 1, 2)`,
		`PRAGMA user_version = 1`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// TestFileScanLogDAORoundTrip：新 DAO dbtx 形态读写往返 + (client, file_path) 主键
// 互不覆盖（两 client 同一路径各自独立）。
func TestFileScanLogDAORoundTrip(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()

	entry := model.FileScanLog{
		Client:        "claude",
		FilePath:      "/proj/s1.jsonl",
		FileIdentity:  "100:200",
		MtimeNS:       1753800000000000000,
		FileSize:      1024,
		ParserVersion: ParserVersion,
	}
	if err := UpsertFileScanLog(context.Background(), d, []model.FileScanLog{entry}); err != nil {
		t.Fatalf("UpsertFileScanLog failed: %v", err)
	}
	// 同一路径另一 client：主键 (client, file_path) 互不覆盖。
	other := entry
	other.Client = "workbuddy"
	other.FileIdentity = "100:999"
	if err := UpsertFileScanLog(context.Background(), d, []model.FileScanLog{other}); err != nil {
		t.Fatalf("UpsertFileScanLog(other client) failed: %v", err)
	}

	logs, err := GetFileScanLogs(context.Background(), d, "claude")
	if err != nil {
		t.Fatalf("GetFileScanLogs failed: %v", err)
	}
	got, ok := logs["/proj/s1.jsonl"]
	if !ok {
		t.Fatal("expected log for /proj/s1.jsonl")
	}
	if got.FileIdentity != "100:200" || got.MtimeNS != 1753800000000000000 || got.FileSize != 1024 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.ParserVersion != ParserVersion {
		t.Fatalf("ParserVersion = %d, want %d", got.ParserVersion, ParserVersion)
	}
	// workbuddy 侧记录独立存在。
	wb, err := GetFileScanLogs(context.Background(), d, "workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	if len(wb) != 1 || wb["/proj/s1.jsonl"].FileIdentity != "100:999" {
		t.Fatalf("workbuddy logs = %+v, want independent identity", wb)
	}
	// claude 侧未被 workbuddy 写入覆盖。
	if logs["/proj/s1.jsonl"].FileIdentity != "100:200" {
		t.Fatal("claude 侧记录被另一 client 覆盖")
	}
}

// TestFileScanLogUpsertInsideTransaction：dbtx 形态可在外部事务内调用（门记录与
// 消息同事务提交的合同前提）；事务回滚时门记录不残留。
func TestFileScanLogUpsertInsideTransaction(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()

	tx, err := d.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpsertFileScanLog(context.Background(), tx, []model.FileScanLog{{
		Client: "claude", FilePath: "/proj/s2.jsonl", FileIdentity: "1:2",
		MtimeNS: 1, FileSize: 2, ParserVersion: ParserVersion,
	}}); err != nil {
		t.Fatalf("UpsertFileScanLog inside tx failed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	logs, err := GetFileScanLogs(context.Background(), d, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("rollback 后门记录应不残留, got %d", len(logs))
	}
}

// TestParserVersionConstant：ParserVersion 常量存在且为正整数（起版 1）。
func TestParserVersionConstant(t *testing.T) {
	if ParserVersion < 1 {
		t.Fatalf("ParserVersion = %d, want >= 1", ParserVersion)
	}
}
