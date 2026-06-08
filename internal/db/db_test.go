package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpen_CreateDBFile(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("database file should exist after Open")
	}
}

func TestOpen_InMemory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if db.db == nil {
		t.Error("db.db should not be nil")
	}
}

func TestOpen_AppliesConnectionPragmasToEveryPooledConnection(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "pooled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.db.SetMaxOpenConns(2)

	ctx := context.Background()
	conn1, err := d.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	conn2, err := d.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	for i, conn := range []*sql.Conn{conn1, conn2} {
		var foreignKeys, busyTimeout, synchronous int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i+1, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i+1, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatalf("conn %d synchronous: %v", i+1, err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 || synchronous != 1 {
			t.Fatalf("conn %d PRAGMA = foreign_keys:%d busy_timeout:%d synchronous:%d",
				i+1, foreignKeys, busyTimeout, synchronous)
		}
	}
}

func TestOpen_PathContainingQuestionMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage?.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("特殊字符路径应由 URI DSN 正确转义: %v", err)
	}
	defer d.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据库应创建在原始路径: %v", err)
	}
}

func TestEnsureSchema_CreatesTables(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(db.db); err != nil {
		t.Fatalf("ensureSchema failed: %v", err)
	}

	tables := []string{
		"raw_client_sessions",
		"raw_router_logs",
		"sessions",
		"collection_log",
		"file_scan_log",
		"collection_errors",
	}

	for _, table := range tables {
		var count int
		err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil {
			t.Errorf("table %q does not exist: %v", table, err)
		}
	}
}

// TestEnsureSchema_MessageLedgerV1 断言消息账本 V1 最终 schema：
// sessions 精确列集合、daily_stats 不存在、user_version=1。
func TestEnsureSchema_MessageLedgerV1(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(db.db); err != nil {
		t.Fatalf("ensureSchema failed: %v", err)
	}

	// sessions 精确列集合
	wantSessionColumns := map[string]bool{
		"id": true, "client": true, "directory": true, "project": true,
		"title": true, "parent_id": true, "first_ts": true, "last_ts": true,
	}
	rows, err := db.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(sessions) failed: %v", err)
	}
	got := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info row failed: %v", err)
		}
		got[name] = true
	}
	rows.Close()
	if len(got) != len(wantSessionColumns) {
		t.Errorf("sessions 列数 = %d, want %d (got=%v want=%v)",
			len(got), len(wantSessionColumns), got, wantSessionColumns)
	}
	for col := range wantSessionColumns {
		if !got[col] {
			t.Errorf("sessions 缺少列 %q", col)
		}
	}
	for col := range got {
		if !wantSessionColumns[col] {
			t.Errorf("sessions 存在多余列 %q（应为 message ledger 精简 schema）", col)
		}
	}

	// daily_stats 表不应存在
	var dailyName string
	err = db.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='daily_stats'`).Scan(&dailyName)
	if err == nil {
		t.Errorf("daily_stats 表不应存在（message ledger 已移除会话级聚合）")
	}

	// user_version 必须为 1
	version := getUserVersion(db.db)
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
	}
}

func TestEnsureSchema_Idempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 运行两次不应该出错
	if err := ensureSchema(db.db); err != nil {
		t.Fatalf("first ensureSchema failed: %v", err)
	}
	if err := ensureSchema(db.db); err != nil {
		t.Fatalf("second ensureSchema failed: %v", err)
	}
}

func TestEnsureSchema_HasMessageLedgerTables(t *testing.T) {
	d := setupTestDB(t)
	for _, table := range []string{"messages", "sessions", "sync_state"} {
		var name string
		if err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestDB_ContextMethods(t *testing.T) {
	// 用文件库而非 :memory:：:memory: 在 database/sql 连接池下每个连接独立，
	// 不同语句可能落到不同内存库导致表不可见；文件库共享，且 WAL 才生效。
	dbPath := filepath.Join(t.TempDir(), "ctx.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	// ExecContext
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS t(c TEXT)`); err != nil {
		t.Fatalf("ExecContext: %v", err)
	}

	// BeginTx + Commit
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO t(c) VALUES(?)`, "x"); err != nil {
		t.Fatalf("tx.ExecContext: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// QueryContext
	rows, err := d.QueryContext(ctx, `SELECT c FROM t`)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("QueryContext 无结果")
	}

	// QueryRowContext
	var c string
	if err := d.QueryRowContext(ctx, `SELECT c FROM t LIMIT 1`).Scan(&c); err != nil {
		t.Fatalf("QueryRowContext: %v", err)
	}
	if c != "x" {
		t.Fatalf("QueryRowContext 结果 = %q", c)
	}
}
