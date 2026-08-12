package collector

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
)

// openSQLiteReadOnly 用 URI DSN 打开外部客户端数据库。直接拼接 "?mode=ro"
// 会把文件名中的 ?/# 误当查询参数；统一转成转义后的绝对 file URI。
// Windows 盘符路径须补前导斜杠（/C:/... → file:///C:/...），否则 SQLite 把
// C: 当 URI authority 报 invalid uri authority（与 internal/db 的 fileURIPath 同理）。
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{
		Scheme: "file",
		Path:   slashed,
	}
	query := url.Values{}
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return sql.Open("sqlite", u.String())
}
