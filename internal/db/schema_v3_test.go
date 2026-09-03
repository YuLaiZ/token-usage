package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// === schema v3 升级合同（raw_router_logs 增加 data_source 列） ===
//
// 合同：currentSchemaVersion=3；migrateV3 单事务 ALTER TABLE raw_router_logs
// 加 data_source TEXT NOT NULL DEFAULT 'proxy' + 按 request_id GLOB
// 'codex_session:*' 一次性回填存量 codex_session 行 + user_version=3；任一步
// 失败则库保持 v2 下次重试；GLOB 为字面前缀匹配（LIKE 的 `_` 是单字符通配符，
// 近似前缀 codexXsession: 不得被误分类）。

// rawRouterDataSourceColumns 返回 raw_router_logs 是否含 data_source 列。
func rawRouterHasDataSource(t *testing.T, q interface {
	Query(string, ...any) (*sql.Rows, error)
}) bool {
	t.Helper()
	rows, err := q.Query("SELECT name FROM pragma_table_info('raw_router_logs')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c == "data_source" {
			return true
		}
	}
	return false
}

// rawRouterDataSource 按 request_id 返回该行迁移后的 data_source 值。
func rawRouterDataSource(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}, requestID string) string {
	t.Helper()
	var ds string
	if err := q.QueryRow("SELECT data_source FROM raw_router_logs WHERE request_id=?", requestID).Scan(&ds); err != nil {
		t.Fatalf("query data_source for %q: %v", requestID, err)
	}
	return ds
}

// TestFreshDBReachesV3：全新库 Open 后 user_version=3 且 raw_router_logs 含
// data_source 列、新行默认 'proxy'。
func TestFreshDBReachesV3(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 3 {
		t.Fatalf("fresh DB user_version = %d, want 3", got)
	}
	if !rawRouterHasDataSource(t, d) {
		t.Fatal("fresh DB raw_router_logs 应含 data_source 列")
	}
	// 未显式指定 data_source 的新行走列默认 'proxy'。
	if _, err := d.Exec("INSERT INTO raw_router_logs (request_id, router_name, app_type) VALUES ('fresh-default-1', 'cc_switch', 'codex')"); err != nil {
		t.Fatal(err)
	}
	if got := rawRouterDataSource(t, d, "fresh-default-1"); got != "proxy" {
		t.Fatalf("新行 data_source = %q, want proxy (列默认值)", got)
	}
}

// TestV2UpgradeBackfillsDataSource：v2 存量库（含 codex_session: 前缀行、claude
// 行、普通 proxy 行、近似前缀行）升级后 data_source 列存在且按 GLOB 严格前缀回填：
// codex_session: 前缀 → 'codex_session'；近似前缀（codexXsession:）与其余 → 'proxy'。
func TestV2UpgradeBackfillsDataSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	if err := sqlDumpV2(t, path); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer upgraded.Close()
	if got := userVersion(t, upgraded); got != 3 {
		t.Fatalf("upgraded user_version = %d, want 3", got)
	}
	if !rawRouterHasDataSource(t, upgraded) {
		t.Fatal("升级后 raw_router_logs 应含 data_source 列")
	}
	cases := []struct {
		requestID string
		want      string
	}{
		{"codex_session:thread-v1:11111111-1111-1111-1111-111111111111:1", "codex_session"},
		{"codex_session:thread-v1:22222222-2222-2222-2222-222222222222:7", "codex_session"},
		// 近似前缀：LIKE 的 `_` 是单字符通配符会误匹配 codexXsession:，
		// GLOB 字面前缀不误匹配，必须保持 'proxy'。
		{"codexXsession:1", "proxy"},
		{"codessession:not-a-prefix", "proxy"},
		{"session:msg_claude_01", "proxy"},
		{"33333333-3333-3333-3333-333333333333", "proxy"},
	}
	for _, c := range cases {
		if got := rawRouterDataSource(t, upgraded, c.requestID); got != c.want {
			t.Fatalf("request_id %q data_source = %q, want %q", c.requestID, got, c.want)
		}
	}
}

// TestMigrateV3FailureKeepsV2：migrateV3 在 ALTER 成功之后、UPDATE 回填/PRAGMA
// 之前注入确定性失败（migrateV3PostAlterHook），整个迁移事务回滚——库保持 v2：
// user_version=2、data_source 列不存在、存量行原样；清除注入后重试成功。
// 区分度：若 ALTER 不在事务内（错误实现），注入失败后列已存在，重试 ALTER
// 报 duplicate column，测试可检出。
func TestMigrateV3FailureKeepsV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail3.db")
	if err := sqlDumpV2(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	migrateV3PostAlterHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV3PostAlterHook = nil })

	if err := migrateV3(raw); err == nil {
		t.Fatal("migrateV3 应因中段注入失败")
	}

	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("失败后 user_version = %d, want 2 (事务回滚保持 v2)", v)
	}
	if rawRouterHasDataSource(t, raw) {
		t.Fatal("失败后 data_source 列不应存在（ALTER 随事务回滚）")
	}
	var n int
	if err := raw.QueryRow("SELECT COUNT(*) FROM raw_router_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("失败后存量行数 = %d, want 6 (数据原样)", n)
	}

	// 清除注入后重试成功。
	migrateV3PostAlterHook = nil
	if err := migrateV3(raw); err != nil {
		t.Fatalf("重试 migrateV3 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Fatalf("重试后 user_version = %d, want 3", v)
	}
	if got := rawRouterDataSource(t, raw, "codex_session:thread-v1:11111111-1111-1111-1111-111111111111:1"); got != "codex_session" {
		t.Fatalf("重试后回填值 = %q, want codex_session", got)
	}
}

// sqlDumpV2 生成真实 v2 形态库：raw_router_logs 为 v2 列布局（无 data_source）
// + 六行三类形态存量 + user_version=2。用原生 SQL 直建（锁定「已发布 v2 结构」事实）。
func sqlDumpV2(t *testing.T, path string) error {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer raw.Close()
	stmts := []string{
		`CREATE TABLE raw_router_logs (
			request_id              TEXT NOT NULL,
			message_id              TEXT NOT NULL DEFAULT '',
			router_name             TEXT NOT NULL,
			session_id              TEXT NOT NULL DEFAULT '',
			app_type                TEXT NOT NULL DEFAULT '',
			model                   TEXT NOT NULL DEFAULT '',
			provider_id             TEXT NOT NULL DEFAULT '',
			provider_name           TEXT NOT NULL DEFAULT '',
			input_tokens            INTEGER NOT NULL DEFAULT 0,
			output_tokens           INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
			cache_create_tokens     INTEGER NOT NULL DEFAULT 0,
			created_at              INTEGER NOT NULL DEFAULT 0,
			raw_data                TEXT NOT NULL DEFAULT '{}',
			collected_at            TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (request_id, router_name)
		)`,
		`INSERT INTO raw_router_logs (request_id, router_name, app_type, model, created_at) VALUES
			('codex_session:thread-v1:11111111-1111-1111-1111-111111111111:1', 'cc_switch', 'codex', 'gpt-5.6-terra', 1750000000),
			('codex_session:thread-v1:22222222-2222-2222-2222-222222222222:7', 'cc_switch', 'codex', 'gpt-5.6-sol', 1750000100),
			('codexXsession:1', 'cc_switch', 'codex', 'gpt-5.6-terra', 1750000200),
			('codessession:not-a-prefix', 'cc_switch', 'codex', 'gpt-5.6-terra', 1750000300),
			('session:msg_claude_01', 'cc_switch', 'claude', 'glm-5.3', 1750000400),
			('33333333-3333-3333-3333-333333333333', 'cc_switch', 'codex', 'glm-5.3', 1750000500)`,
		`PRAGMA user_version = 2`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
