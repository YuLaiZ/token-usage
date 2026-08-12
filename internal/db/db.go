package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB 封装 sql.DB，使用 SQLite WAL 模式支持并发读
// WAL 模式下多个读操作可以并发执行；写串行化靠 busy_timeout=5000（写锁等待最多 5s）
// 叠加采集层 collectMu（同一时刻只有一个采集在跑），足够覆盖守护进程并发场景
type DB struct {
	db *sql.DB
}

func Open(path string) (*DB, error) {
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, fmt.Errorf("构造数据库 DSN 失败: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// :memory: 对每条 SQLite 连接都是独立数据库。限制为单连接，避免连接池切换后
	// schema/数据消失；文件数据库仍保留多连接以支持 WAL 并发读。
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	// journal_mode 是数据库文件级持久设置，初始化一次即可。其余连接级 PRAGMA
	// 已编码进 DSN，由 modernc SQLite 在每条新连接创建时执行。
	pragmas := []string{
		"PRAGMA journal_mode=WAL", // 启用 WAL 模式，支持并发读
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %q 失败: %w", pragma, err)
		}
	}

	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 schema 失败: %w", err)
	}

	return &DB{db: db}, nil
}

func sqliteDSN(path string) (string, error) {
	values := url.Values{}
	values.Add("_pragma", "busy_timeout(5000)")
	values.Add("_pragma", "foreign_keys(ON)")
	values.Add("_pragma", "synchronous(NORMAL)")

	if path == ":memory:" {
		return path + "?" + values.Encode(), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "file",
		Path:     fileURIPath(filepath.ToSlash(absolute)),
		RawQuery: values.Encode(),
	}
	return u.String(), nil
}

// fileURIPath 把 file URI 的 path 段规范化为 SQLite 可解析的形式。
// Windows 盘符路径（C:/Users/...）不以 / 开头，直接放进 url.URL 会生成
// "file:C:/..."——SQLite 会把 C: 当作 URI authority 报 invalid uri authority。
// 补前导斜杠生成 "file:///C:/..."（空 host + 盘符路径）是 SQLite 的合法形式；
// POSIX 路径本已以 / 开头，保持原样。
func fileURIPath(slashed string) string {
	if !strings.HasPrefix(slashed, "/") {
		return "/" + slashed
	}
	return slashed
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) Exec(query string, args ...interface{}) (sql.Result, error) {
	return d.db.Exec(query, args...)
}

func (d *DB) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return d.db.Query(query, args...)
}

func (d *DB) QueryRow(query string, args ...interface{}) *sql.Row {
	return d.db.QueryRow(query, args...)
}

func (d *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

func (d *DB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

// BeginTx 支持事务内 ctx 取消（守护进程关闭时可中断写事务）。
func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return d.db.BeginTx(ctx, opts)
}
