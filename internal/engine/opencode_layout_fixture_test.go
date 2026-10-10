package engine

// OpenCode 布局三态测试的源库 fixture（engine 包内自带，Go 跨包测试无法引用
// collector 包 _test.go 的 helper）。表结构与数据形态对照 collector 包的合成
// fixture：V1 三表 + V2 两表（真实 2.x 未知项未闭合，不据此登记 V2 兼容完成）。

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const ocLayoutV1DDL = `
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		parent_id TEXT NOT NULL DEFAULT '',
		directory TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '{}',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		data TEXT NOT NULL
	);
	CREATE TABLE event (
		id TEXT NOT NULL,
		aggregate_id TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	);`

const ocLayoutV2DDL = `
	CREATE TABLE session_v2 (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		directory TEXT NOT NULL DEFAULT '',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE session_message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		data TEXT NOT NULL,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0
	);`

func ocFixtureExec(t *testing.T, dbPath, stmt string, args ...interface{}) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec 失败: %v\nstmt: %s", err, stmt)
	}
}

// ocFixtureCreateV1 建源库 V1 三表。
func ocFixtureCreateV1(t *testing.T, dbPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	ocFixtureExec(t, dbPath, ocLayoutV1DDL)
}

// ocFixtureCreateV2 / DropV2 切换 V2 两表（混合布局与往返段）。
func ocFixtureCreateV2(t *testing.T, dbPath string) { ocFixtureExec(t, dbPath, ocLayoutV2DDL) }
func ocFixtureDropV2(t *testing.T, dbPath string) {
	ocFixtureExec(t, dbPath, `DROP TABLE session_message; DROP TABLE session_v2;`)
}

func ocFixtureInsertSession(t *testing.T, dbPath, id, directory, title string) {
	t.Helper()
	ocFixtureExec(t, dbPath,
		`INSERT INTO session (id,parent_id,directory,title,model,time_created,time_updated) VALUES (?,?,?,?, '{}', 0, 0)`,
		id, "", directory, title)
}

func ocFixtureInsertSessionV2(t *testing.T, dbPath, id, directory, title string) {
	t.Helper()
	ocFixtureExec(t, dbPath,
		`INSERT INTO session_v2 (id,title,directory,time_created,time_updated) VALUES (?,?,?,?,0)`,
		id, title, directory, 0)
}

// ocFixtureV1Message 构造 V1 completed assistant 行的 data JSON。
func ocFixtureV1Message(t *testing.T, dbPath, id, sessionID string, ts int64, total, input, output int64) {
	t.Helper()
	data := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":"assistant","modelID":"m1","providerID":"anthropic",
"time":{"created":%d,"completed":%d},
"tokens":{"total":%d,"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		id, sessionID, ts, ts, total, input, output)
	ocFixtureExec(t, dbPath,
		`INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES (?,?,?,?,?)`,
		id, sessionID, ts, ts, data)
}

// ocFixtureV2Message 构造 V2 assistant 行（data 无 role、无 modelID）。
func ocFixtureV2Message(t *testing.T, dbPath, id, sessionID string, ts int64, total int64) {
	t.Helper()
	data := fmt.Sprintf(`{"id":%q,"sessionID":%q,
"time":{"created":%d,"completed":%d},
"tokens":{"total":%d,"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		id, sessionID, ts, ts, total, total)
	ocFixtureExec(t, dbPath,
		`INSERT INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?,'assistant',0)`,
		id, sessionID, data, ts, ts)
}

// ocLayoutMSAt 返回本地时区某日期时刻的毫秒时间戳。
func ocLayoutMSAt(year, month, day, hour, minute int) int64 {
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.Local).UnixMilli()
}

// ocFixtureEvent 插入 event 表的 message.updated.1 终态行（V1 布局 DDL 内置
// event 表；engine 级 fixture 用）。
func ocFixtureEvent(t *testing.T, dbPath, msgID, sessionID string, ts int64, total, input, output int64) {
	t.Helper()
	data := fmt.Sprintf(`{"info":{"id":%q,"sessionID":%q,"role":"assistant","modelID":"m1","providerID":"anthropic",
"time":{"created":%d,"completed":%d},
"tokens":{"total":%d,"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
		msgID, sessionID, ts, ts, total, input, output)
	ocFixtureExec(t, dbPath,
		`INSERT INTO event (id,aggregate_id,seq,type,data) VALUES (?, ?, 0, 'message.updated.1', ?)`,
		"ev-"+msgID, sessionID, data)
}
