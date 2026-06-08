package db

import (
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 1

func ensureSchema(db *sql.DB) error {
	version := getUserVersion(db)

	if version < 1 {
		if err := migrateV1(db); err != nil {
			return fmt.Errorf("迁移到 v1 失败: %w", err)
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
