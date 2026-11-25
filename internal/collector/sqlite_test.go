package collector

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// 用 # 而不用 ? 做实际建库：两者在 URI 里都必须转义，但 ? 是 Windows 文件名
// 的保留字符，建库会失败。问号的编码由 TestSQLiteURI_PrefixesWindowsDrivePathWithSlash
// 在纯 URI 层面覆盖。
func TestOpenSQLiteReadOnly_EscapesSpecialPathAndRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source#.db")
	seed, err := sql.Open("sqlite", sqliteTestURI(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE item(id INTEGER); INSERT INTO item(id) VALUES(1)`); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	readonly, err := openSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	var count int
	if err := readonly.QueryRow(`SELECT COUNT(*) FROM item`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("特殊字符路径读取失败: count=%d err=%v", count, err)
	}
	if _, err := readonly.Exec(`INSERT INTO item(id) VALUES(2)`); err == nil {
		t.Fatal("只读连接不得允许写入")
	}
}

// 源库被所属客户端写入时只读连接靠锁等待上限渡过瞬时锁；上限必须随 DSN
// 作用于池中每条新连接，逐条取出核对。
func TestOpenSQLiteReadOnly_AppliesBusyTimeoutToEveryConn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	createSQLiteTestSource(t, path, "DELETE")

	readonly, err := openSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()

	ctx := context.Background()
	conn1, err := readonly.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	conn2, err := readonly.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	want := int(externalBusyTimeout / time.Millisecond)
	for i, conn := range []*sql.Conn{conn1, conn2} {
		var got int
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&got); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("conn %d busy_timeout = %d, want %d", i+1, got, want)
		}
	}
}

func TestOpenSQLiteReadOnly_WaitsForWriterLockInsteadOfFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	// rollback journal 模式下写者的 EXCLUSIVE 锁会拒绝只读读者；WAL 模式下读
	// 不被写阻塞，复现不了。
	createSQLiteTestSource(t, path, "DELETE")
	release := holdSQLiteExclusiveLock(t, path)
	defer release()

	// 对照：零等待连接必须立即被锁拒绝。锁没生效时，后面的等待断言没有意义。
	impatient, err := openSQLiteReadOnlyWithBusyTimeout(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()
	start := time.Now()
	err = impatient.QueryRow(`SELECT COUNT(*) FROM item`).Err()
	elapsed := time.Since(start)
	if !isSQLiteBusy(err) {
		t.Fatalf("零等待连接应被锁拒绝，实际 err=%v", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("零等待连接不应等待，实际耗时 %v", elapsed)
	}

	reader, err := openSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	type outcome struct {
		count int
		err   error
	}
	started := make(chan struct{})
	done := make(chan outcome, 1)
	go func() {
		// 读者启动后主 goroutine 才开始持锁计时，调度延迟不会算进持锁时间。
		close(started)
		var count int
		err := reader.QueryRow(`SELECT COUNT(*) FROM item`).Scan(&count)
		done <- outcome{count: count, err: err}
	}()
	<-started

	// 持锁期间查询不得返回：对照已证明锁生效，未返回即等待。不比较两边的
	// 计时差值，避免读者被调度推后时误报。
	const hold = 300 * time.Millisecond
	select {
	case <-done:
		t.Fatal("查询在锁释放前返回，没有等待")
	case <-time.After(hold):
	}
	release()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("默认入口应等到锁释放后读成功，实际 err=%v", got.err)
		}
		if got.count != 1 {
			t.Fatalf("读取行数错误: got %d want 1", got.count)
		}
	case <-time.After(externalBusyTimeout + 5*time.Second):
		t.Fatal("查询未在等待上限内返回")
	}
}

// 盘符路径必须拼成 file:///C:/...；file://C:/... 会被 SQLite 当成 authority 为
// C: 的 URI 而报错。同时覆盖路径中 ? 与 # 的转义。只断言 URI 形态，任意平台
// 都能验证。
func TestSQLiteURI_PrefixesWindowsDrivePathWithSlash(t *testing.T) {
	query := url.Values{}
	query.Set("mode", "ro")
	for _, tc := range []struct{ name, absolute, want string }{
		{"Windows 盘符路径", "C:/Temp/source?.db", "file:///C:/Temp/source%3F.db?mode=ro"},
		{"POSIX 绝对路径", "/tmp/source?.db", "file:///tmp/source%3F.db?mode=ro"},
		{"路径中的 #", "/tmp/source#.db", "file:///tmp/source%23.db?mode=ro"},
	} {
		if got := sqliteURI(tc.absolute, query); got != tc.want {
			t.Fatalf("%s: sqliteURI = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// sqliteTestURI 是测试里建库与持锁用的可写 URI，与生产入口共用同一套路径
// 规范化，含 Windows 盘符处理。
func sqliteTestURI(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return sqliteURI(absolute, url.Values{})
}

// createSQLiteTestSource 建一个只有 item 表（1 行）的源库，并把 journal_mode
// 固定为 journalMode；固定失败即报错，调用方依赖该模式复现锁行为。
func createSQLiteTestSource(t *testing.T, path, journalMode string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteTestURI(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE item(id INTEGER); INSERT INTO item(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`PRAGMA journal_mode=` + journalMode).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, journalMode) {
		t.Fatalf("固定 journal_mode 失败: got %q want %q", got, journalMode)
	}
}

// holdSQLiteExclusiveLock 在源库上以独立连接持有 EXCLUSIVE 锁，返回释放函数。
func holdSQLiteExclusiveLock(t *testing.T, path string) func() {
	t.Helper()
	raw, err := sql.Open("sqlite", sqliteTestURI(t, path))
	if err != nil {
		t.Fatal(err)
	}
	// 单连接：BEGIN 与 ROLLBACK 必须落在同一连接上。
	raw.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
		_ = raw.Close()
	}
}

// isSQLiteBusy 判断错误是否为 SQLITE_BUSY（结果码 5，含 SQLITE_BUSY_RECOVERY
// 等扩展码）；驱动包未导出该常量，按数值判断。
func isSQLiteBusy(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code()&0xff == 5
}
