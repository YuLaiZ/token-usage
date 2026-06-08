package collector

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenSQLiteReadOnly_EscapesSpecialPathAndRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source?.db")
	seedURI := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	seed, err := sql.Open("sqlite", seedURI)
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
