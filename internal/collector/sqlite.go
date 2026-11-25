package collector

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// externalBusyTimeout 是只读打开外部客户端数据库时，每次锁竞争的等待预算。
//
// 外部库由所属客户端持续写入，rollback journal 模式下写事务提交期间读者会被
// 直接拒绝；不等待时瞬时锁即报 SQLITE_BUSY 并登记采集错误。等待期间驱动不
// 响应 context 取消，采集又全局串行，预算须有界：2s 足以覆盖常见的瞬时锁。
// 预算按每次锁竞争各自计算，不在整轮采集内共享：一次采集内多次遇到锁会
// 累加，外部库被长时间锁住时每次都等满。不共享是为了让同一轮里后续的
// 数据源遇到短锁时仍有机会等到。
const externalBusyTimeout = 2000 * time.Millisecond

// openSQLiteReadOnly 用 URI DSN 以只读方式打开外部客户端数据库，每次遇到锁
// 最多等待 externalBusyTimeout。
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	return openSQLiteReadOnlyWithBusyTimeout(path, externalBusyTimeout)
}

// openSQLiteReadOnlyWithBusyTimeout 与 openSQLiteReadOnly 相同，但显式指定
// 连接级 busy 等待上限；busy 为 0 时遇锁立即失败。
func openSQLiteReadOnlyWithBusyTimeout(path string, busy time.Duration) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	return sql.Open("sqlite", sqliteURI(absolute, query))
}

// sqliteURI 把已绝对化的本地路径与查询参数拼成 file URI。直接拼接 "?mode=ro"
// 会把文件名中的 ?/# 误当查询参数，这里统一转义。
// Windows 盘符路径须补前导斜杠（/C:/... → file:///C:/...），否则 SQLite 把
// C: 当 URI authority 报 invalid uri authority（与 internal/db 的 fileURIPath 同理）。
func sqliteURI(absolute string, query url.Values) string {
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{
		Scheme:   "file",
		Path:     slashed,
		RawQuery: query.Encode(),
	}
	return u.String()
}
