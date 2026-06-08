package collector

import (
	"database/sql"
	"net/url"
	"path/filepath"
)

// openSQLiteReadOnly 用 URI DSN 打开外部客户端数据库。直接拼接 "?mode=ro"
// 会把文件名中的 ?/# 误当查询参数；统一转成转义后的绝对 file URI。
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(absolute),
	}
	query := url.Values{}
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return sql.Open("sqlite", u.String())
}
