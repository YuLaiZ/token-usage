package db

import (
	"database/sql"
	"fmt"
)

// currentSchemaVersion 当前 schema 版本。v2 重建 file_scan_log 为 startup 跳过门
// 状态表（v1 布局是死表，无生产数据）；v3 为 raw_router_logs 加 data_source 列
// （区分 proxy 直录与 codex_session 同步行，codex 归因只消费前者）；v4 把存量
// mimocode 落库名 "Xiaomi MiMo / MiMo Code" 统一改名并折叠为 "MiMo Code"，
// 并创建持久化 trigger 兼容旧版二进制回滚后继续写旧名（见 migrateV4）。
const currentSchemaVersion = 4

// ParserVersion 是 JSONL 解析/映射逻辑的版本号（file_scan_log.parser_version）。
// 任何影响 JSONL 采集产出语义的解析/映射修复都必须递增此值：跳过门按版本整表
// 失效，升级后全部文件重读一次（幂等 upsert 安全）。
const ParserVersion = 1

func ensureSchema(db *sql.DB) error {
	version := getUserVersion(db)

	if version < 1 {
		if err := migrateV1(db); err != nil {
			return fmt.Errorf("迁移到 v1 失败: %w", err)
		}
	}
	version = getUserVersion(db)
	if version < 2 {
		if err := migrateV2(db); err != nil {
			return fmt.Errorf("迁移到 v2 失败: %w", err)
		}
	}
	version = getUserVersion(db)
	if version < 3 {
		if err := migrateV3(db); err != nil {
			return fmt.Errorf("迁移到 v3 失败: %w", err)
		}
	}
	version = getUserVersion(db)
	if version < 4 {
		if err := migrateV4(db); err != nil {
			return fmt.Errorf("迁移到 v4 失败: %w", err)
		}
	}

	return nil
}

func getUserVersion(db *sql.DB) int {
	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	return version
}

func migrateV1(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS raw_client_sessions (
			session_id          TEXT NOT NULL,
			client              TEXT NOT NULL,
			directory           TEXT NOT NULL DEFAULT '',
			model               TEXT NOT NULL DEFAULT '',
			title               TEXT NOT NULL DEFAULT '',
			created_at          INTEGER NOT NULL DEFAULT 0,
			last_active_at      INTEGER NOT NULL DEFAULT 0,
			input_tokens        INTEGER NOT NULL DEFAULT 0,
			output_tokens       INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_create_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens        INTEGER NOT NULL DEFAULT 0,
			raw_data            TEXT NOT NULL DEFAULT '{}',
			source_file         TEXT NOT NULL DEFAULT '',
			file_mtime          INTEGER NOT NULL DEFAULT 0,
			file_size           INTEGER NOT NULL DEFAULT 0,
			collected_at        TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (session_id, client)
		)`,

		`CREATE TABLE IF NOT EXISTS raw_router_logs (
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

		`CREATE TABLE IF NOT EXISTS sessions (
			id                  TEXT NOT NULL,
			client              TEXT NOT NULL,
			directory           TEXT NOT NULL DEFAULT '',
			project             TEXT NOT NULL DEFAULT '',
			title               TEXT NOT NULL DEFAULT '',
			parent_id           TEXT NOT NULL DEFAULT '',
			first_ts            INTEGER NOT NULL DEFAULT 0,
			last_ts             INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (id, client)
		)`,

		`CREATE TABLE IF NOT EXISTS messages (
			id                  TEXT NOT NULL,
			session_id          TEXT NOT NULL,
			client              TEXT NOT NULL,
			date                TEXT NOT NULL,
			ts                  INTEGER NOT NULL,
			model               TEXT NOT NULL DEFAULT '',
			provider            TEXT NOT NULL DEFAULT '',
			router_provider     TEXT NOT NULL DEFAULT '',
			router_model        TEXT NOT NULL DEFAULT '',
			router_name         TEXT NOT NULL DEFAULT '',
			directory           TEXT NOT NULL DEFAULT '',
			project             TEXT NOT NULL DEFAULT '',
			input_tokens        INTEGER NOT NULL DEFAULT 0,
			fresh_input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens       INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_create_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
			total_tokens        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (client, id)
		)`,

		`CREATE TABLE IF NOT EXISTS sync_state (
			client       TEXT NOT NULL,
			source       TEXT NOT NULL,
			cursor_value INTEGER NOT NULL DEFAULT 0,
			cursor_id    TEXT NOT NULL DEFAULT '',
			updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (client, source)
		)`,

		`CREATE TABLE IF NOT EXISTS collection_log (
			date            TEXT NOT NULL,
			source          TEXT NOT NULL,
			session_count   INTEGER NOT NULL DEFAULT 0, -- 实际为 message count
			collected_at    TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (date, source)
		)`,

		`CREATE TABLE IF NOT EXISTS file_scan_log (
			file_path       TEXT PRIMARY KEY,
			session_id      TEXT NOT NULL DEFAULT '',
			client          TEXT NOT NULL,
			source_type     TEXT NOT NULL DEFAULT 'jsonl',
			last_modified   INTEGER NOT NULL,
			file_size       INTEGER NOT NULL,
			last_line_offset INTEGER NOT NULL DEFAULT 0,
			scanned_at      TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS collection_errors (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			date            TEXT NOT NULL,
			source          TEXT NOT NULL,
			error_type      TEXT NOT NULL,
			message         TEXT NOT NULL,
			detail          TEXT NOT NULL DEFAULT '',
			retry_count     INTEGER NOT NULL DEFAULT 0,
			resolved        INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		// 索引
		`CREATE INDEX IF NOT EXISTS idx_sessions_client ON sessions(client)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_client_client ON raw_client_sessions(client)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_client_collected ON raw_client_sessions(collected_at)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_router_message ON raw_router_logs(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_router_session ON raw_router_logs(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_router_app_type ON raw_router_logs(app_type)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_router_created ON raw_router_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_errors_date ON collection_errors(date)`,
		`CREATE INDEX IF NOT EXISTS idx_errors_source ON collection_errors(source)`,
		`CREATE INDEX IF NOT EXISTS idx_errors_resolved ON collection_errors(resolved)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_client ON messages(session_id, client)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_client_date ON messages(client, date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project)`,

		`PRAGMA user_version = 1`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, stmt)
		}
	}

	return nil
}

// migrateV2PostDropHook 仅供测试注入：在 DROP 成功后、CREATE/PRAGMA 之前执行，
// 返回错误时整个迁移事务回滚（验证「DROP 之后的迁移语句失败必须恢复 v1」的
// 中段失败原子性——DROP 若不在事务内，注入失败后旧表已被删，测试可检出）。
// 生产恒为 nil。
var migrateV2PostDropHook func() error

// migrateV2 重建 file_scan_log 为 startup 跳过门状态表（v1 布局为死表、无生产
// 数据，直接 DROP）。全部语句在单个事务内提交：任一步失败则库保持 v1，下一次
// 打开时重试；重试幂等（门表数据可丢弃）。
func migrateV2(db *sql.DB) error {
	const dropStmt = `DROP TABLE IF EXISTS file_scan_log`
	stmts := []string{
		`CREATE TABLE file_scan_log (
			client         TEXT NOT NULL,
			file_path      TEXT NOT NULL,
			file_identity  TEXT NOT NULL,
			mtime_ns       INTEGER NOT NULL,
			file_size      INTEGER NOT NULL,
			parser_version INTEGER NOT NULL,
			updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (client, file_path)
		)`,
		`PRAGMA user_version = 2`,
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(dropStmt); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, dropStmt)
	}
	if migrateV2PostDropHook != nil {
		if err := migrateV2PostDropHook(); err != nil {
			return fmt.Errorf("迁移中段注入失败: %w", err)
		}
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, stmt)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// migrateV3PostAlterHook 仅供测试注入：在 ALTER 成功后、UPDATE 回填/PRAGMA
// 之前执行，返回错误时整个迁移事务回滚（验证中段失败原子性——ALTER 若不在
// 事务内，注入失败后列已存在，重试会报 duplicate column）。生产恒为 nil。
var migrateV3PostAlterHook func() error

// migrateV3 为 raw_router_logs 加 data_source 列并按 request_id 前缀一次性回填
// 存量 codex_session 行。全部语句在单个事务内提交：任一步失败则库保持 v2，
// 下一次打开时重试；重试幂等（user_version 门控保证 ALTER 不会重复到达）。
// 存量回填用 GLOB 而非 LIKE：LIKE 模式中的 `_` 是单字符通配符，
// codexXsession: 之类近似前缀会被误分类；GLOB 为字面前缀匹配
// （通配符仅 *?[]，模式前段不含）。
func migrateV3(db *sql.DB) error {
	const alterStmt = `ALTER TABLE raw_router_logs ADD COLUMN data_source TEXT NOT NULL DEFAULT 'proxy'`
	const backfillStmt = `UPDATE raw_router_logs SET data_source='codex_session' WHERE request_id GLOB 'codex_session:*'`

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(alterStmt); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, alterStmt)
	}
	if migrateV3PostAlterHook != nil {
		if err := migrateV3PostAlterHook(); err != nil {
			return fmt.Errorf("迁移中段注入失败: %w", err)
		}
	}
	if _, err := tx.Exec(backfillStmt); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, backfillStmt)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, `PRAGMA user_version = 3`)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// migrateV4PostTriggerHook 仅供测试注入：在两张表的数据折叠改名与 trigger
// 创建全部完成之后、PRAGMA user_version 之前执行，返回错误时整个迁移事务
// 回滚（验证中段失败原子性——数据、trigger、版本号三者要么全到位要么全无）。
// 生产恒为 nil。
var migrateV4PostTriggerHook func() error

// v4MigrationStatements 是 schema v4 迁移的完整冻结语句序列，按执行顺序：
// ① messages/sessions 各一条折叠改名语句（INSERT ... SELECT ... ON CONFLICT）
// 与一条 legacy 行删除；② 两张表的旧名回写兼容 trigger；PRAGMA user_version=4
// 由 migrateV4 在全部语句成功后单独执行。
//
// 合同冻结说明：列清单与 ON CONFLICT 合并语义逐字冻结自 v0.1.10 发布时的
// DAO upsert（messages 19 列 / sessions 8 列，早 ts 归因、token 新值覆盖、
// router_* 非空才覆盖、title 非空保留、first/last_ts 区间收窄）——trigger
// 的兼容目标正是 v0.1.10 旧二进制的写入合同。历史迁移必须不可变：不得改成
// 运行时解析当前 DAO 常量来生成——未来 DAO 增列或调整语义时，从 v3 或全新
// 库重放的 v4 会随之漂移，甚至因新列尚未由后续迁移创建而失败（no such
// column），与已升级库中持久化的旧 trigger 形成同一 schema version 两种
// 定义。DAO 后续演进不回写本数组；行为正确性由 schema_v4_test 的迁移/冲突/
// 回滚兼容测试锁定。
var v4MigrationStatements = []string{
	// ① messages 折叠：legacy 行按 v0.1.10 upsert 语义并入 MiMo Code 行
	//（同 id 并存时确定性合并，不撞 (client,id) 主键），随后删除旧行。
	`INSERT INTO messages (id, session_id, client, date, ts, model, provider, router_provider, router_model, router_name, directory, project, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, reasoning_tokens, total_tokens) SELECT id, session_id, 'MiMo Code', date, ts, model, provider, router_provider, router_model, router_name, directory, project, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, reasoning_tokens, total_tokens FROM messages WHERE client = 'Xiaomi MiMo / MiMo Code' ON CONFLICT(client, id) DO UPDATE SET
		ts = CASE WHEN excluded.ts < messages.ts THEN excluded.ts ELSE messages.ts END,
		date = CASE WHEN excluded.ts < messages.ts THEN excluded.date ELSE messages.date END,
		session_id = CASE WHEN excluded.ts < messages.ts THEN excluded.session_id ELSE messages.session_id END,
		directory = CASE WHEN excluded.ts < messages.ts THEN excluded.directory ELSE messages.directory END,
		project = CASE WHEN excluded.ts < messages.ts THEN excluded.project ELSE messages.project END,
		model = excluded.model,
		provider = excluded.provider,
		router_provider = CASE WHEN excluded.router_provider != '' THEN excluded.router_provider ELSE messages.router_provider END,
		router_model = CASE WHEN excluded.router_model != '' THEN excluded.router_model ELSE messages.router_model END,
		router_name = CASE WHEN excluded.router_name != '' THEN excluded.router_name ELSE messages.router_name END,
		input_tokens = excluded.input_tokens,
		fresh_input_tokens = excluded.fresh_input_tokens,
		output_tokens = excluded.output_tokens,
		cache_read_tokens = excluded.cache_read_tokens,
		cache_create_tokens = excluded.cache_create_tokens,
		reasoning_tokens = excluded.reasoning_tokens,
		total_tokens = excluded.total_tokens`,
	`DELETE FROM messages WHERE client = 'Xiaomi MiMo / MiMo Code'`,
	// ② sessions 折叠：同上，ON CONFLICT 语义冻结自 v0.1.10 的 session upsert。
	`INSERT INTO sessions (id, client, directory, project, title, parent_id, first_ts, last_ts) SELECT id, 'MiMo Code', directory, project, title, parent_id, first_ts, last_ts FROM sessions WHERE client = 'Xiaomi MiMo / MiMo Code' ON CONFLICT(id,client) DO UPDATE SET
	 directory=excluded.directory,
	 project=excluded.project,
	 title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE sessions.title END,
	 parent_id=excluded.parent_id,
	 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
	 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END`,
	`DELETE FROM sessions WHERE client = 'Xiaomi MiMo / MiMo Code'`,
	// ③ messages 兼容 trigger：旧版二进制（自更新回滚后）仍以 legacy client
	// INSERT/UPSERT 时，BEFORE INSERT 先以 MiMo Code 重放同一 upsert（列与
	// 冲突语义冻结自 v0.1.10 DAO），再 RAISE(IGNORE) 放弃原语句——旧名行永不
	// 入库，canonical 行的冲突更新行为与 v0.1.10 DAO 相同。trigger 持久驻库，
	// 二进制回滚后依旧生效；正常写入不满足 WHEN，零额外行为。recursive_
	// triggers 默认 OFF 且重放行 client 为新名，无递归。
	`CREATE TRIGGER IF NOT EXISTS messages_legacy_mimo_client_rewrite
BEFORE INSERT ON messages
WHEN NEW.client = 'Xiaomi MiMo / MiMo Code'
BEGIN
    INSERT INTO messages (id, session_id, client, date, ts, model, provider, router_provider, router_model, router_name, directory, project, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, reasoning_tokens, total_tokens) VALUES (NEW.id, NEW.session_id, 'MiMo Code', NEW.date, NEW.ts, NEW.model, NEW.provider, NEW.router_provider, NEW.router_model, NEW.router_name, NEW.directory, NEW.project, NEW.input_tokens, NEW.fresh_input_tokens, NEW.output_tokens, NEW.cache_read_tokens, NEW.cache_create_tokens, NEW.reasoning_tokens, NEW.total_tokens) ON CONFLICT(client, id) DO UPDATE SET
		ts = CASE WHEN excluded.ts < messages.ts THEN excluded.ts ELSE messages.ts END,
		date = CASE WHEN excluded.ts < messages.ts THEN excluded.date ELSE messages.date END,
		session_id = CASE WHEN excluded.ts < messages.ts THEN excluded.session_id ELSE messages.session_id END,
		directory = CASE WHEN excluded.ts < messages.ts THEN excluded.directory ELSE messages.directory END,
		project = CASE WHEN excluded.ts < messages.ts THEN excluded.project ELSE messages.project END,
		model = excluded.model,
		provider = excluded.provider,
		router_provider = CASE WHEN excluded.router_provider != '' THEN excluded.router_provider ELSE messages.router_provider END,
		router_model = CASE WHEN excluded.router_model != '' THEN excluded.router_model ELSE messages.router_model END,
		router_name = CASE WHEN excluded.router_name != '' THEN excluded.router_name ELSE messages.router_name END,
		input_tokens = excluded.input_tokens,
		fresh_input_tokens = excluded.fresh_input_tokens,
		output_tokens = excluded.output_tokens,
		cache_read_tokens = excluded.cache_read_tokens,
		cache_create_tokens = excluded.cache_create_tokens,
		reasoning_tokens = excluded.reasoning_tokens,
		total_tokens = excluded.total_tokens;
    SELECT RAISE(IGNORE);
END`,
	// ④ sessions 兼容 trigger：同③。
	`CREATE TRIGGER IF NOT EXISTS sessions_legacy_mimo_client_rewrite
BEFORE INSERT ON sessions
WHEN NEW.client = 'Xiaomi MiMo / MiMo Code'
BEGIN
    INSERT INTO sessions (id, client, directory, project, title, parent_id, first_ts, last_ts) VALUES (NEW.id, 'MiMo Code', NEW.directory, NEW.project, NEW.title, NEW.parent_id, NEW.first_ts, NEW.last_ts) ON CONFLICT(id,client) DO UPDATE SET
	 directory=excluded.directory,
	 project=excluded.project,
	 title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE sessions.title END,
	 parent_id=excluded.parent_id,
	 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
	 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END;
    SELECT RAISE(IGNORE);
END`,
}

// migrateV4 把存量 mimocode 落库名 legacy "Xiaomi MiMo / MiMo Code"（v0.1.10
// 及之前写入，即 model.LegacyClientXiaomiMiMoCode）统一改名并折叠为
// model.ClientMiMoCode，同时创建旧名回写兼容 trigger（语句见冻结的
// v4MigrationStatements）。messages.client / sessions.client 是主键成分：
// (client,id) 与 (id,client)——改名不能是裸 UPDATE（同 id 已存在新名行时会
// 主键冲突），折叠语义见冻结说明。兼容 trigger 解决自更新二进制回滚：迁移
// 提交后若回滚到旧版，旧版继续写旧名会被 trigger 改写为新名 upsert，不会
// 重新出现旧名行或因 client 不同重复入库（否则 collect all 全量重放将使
// 同一消息形成两行、token 翻倍）。全部语句在单个事务内：任一步失败则库
// 保持 v3（数据、trigger、user_version 三者整体回滚），下一次打开时重试；
// 重试幂等（user_version 门控 + CREATE TRIGGER IF NOT EXISTS 双保险）。
func migrateV4(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range v4MigrationStatements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, stmt)
		}
	}
	if migrateV4PostTriggerHook != nil {
		if err := migrateV4PostTriggerHook(); err != nil {
			return fmt.Errorf("迁移中段注入失败: %w", err)
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 4`); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, `PRAGMA user_version = 4`)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}
