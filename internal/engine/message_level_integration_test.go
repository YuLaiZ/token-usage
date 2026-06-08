package engine

// message_level_integration_test.go 消息级采集端到端集成测试（）。
//
// 所有用例使用真实 collector（NewClaudeCollector / NewCodexCollector / NewOpenCodeCollector /
// NewZCodeCollector）+ 真实 DB（db.Open），不使用 mock。
// 收集到的 collector 内部测试 helper（createCCSwitchTestDB 等）在 collector 包内未导出，
// 本文件位于 engine 包无法复用，故按生产 schema 最小重建所需 SQLite 测试库。

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
	_ "modernc.org/sqlite"
)

// silentLogger 返回一个丢弃所有输出的 logger，保持测试输出干净。
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// =========================================================================
//  通用测试库构造 helper（按生产 schema 最小重建）
// =========================================================================

// createCCSwitchDB 在 path 建一个 cc-switch 测试库（providers + proxy_request_logs）。
// 与 collector.createCCSwitchTestDB 同构，但导出给 engine 包使用。
func createCCSwitchDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cc-switch 父目录失败: %v", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 cc-switch DB 失败: %v", err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TABLE providers (id TEXT PRIMARY KEY, app_type TEXT NOT NULL, name TEXT NOT NULL)`,
		`CREATE TABLE proxy_request_logs (
			request_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, app_type TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', request_model TEXT NOT NULL DEFAULT '',
			provider_id TEXT NOT NULL DEFAULT '', provider_type TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			total_cost_usd REAL NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0,
			first_token_ms INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0, error_message TEXT,
			is_streaming INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("建 cc-switch 表失败: %v", err)
		}
	}
}

// insertCCSwitchProvider 插入一个 provider 行。
func insertCCSwitchProvider(t *testing.T, path, id, appType, name string) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open cc-switch: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO providers (id, app_type, name) VALUES (?,?,?)`, id, appType, name); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
}

// ccSwitchLogRow 携带一行 proxy_request_logs 的关键字段。
type ccSwitchLogRow struct {
	requestID   string
	sessionID   string
	appType     string
	model       string
	providerID  string
	inputTokens int64
	outputToken int64
	cacheRead   int64
	cacheCreate int64
	createdAt   int64
}

func insertCCSwitchLog(t *testing.T, path string, r ccSwitchLogRow) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open cc-switch: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO proxy_request_logs
		(request_id, session_id, app_type, model, provider_id,
		 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		 total_cost_usd, latency_ms, status_code, is_streaming, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,0,0,200,1,?)`,
		r.requestID, r.sessionID, r.appType, r.model, r.providerID,
		r.inputTokens, r.outputToken, r.cacheRead, r.cacheCreate, r.createdAt); err != nil {
		t.Fatalf("insert proxy log: %v", err)
	}
}

// createZCodeDB 在 path 建与生产 ~/.zcode/cli/db/db.sqlite 同构的测试库。
func createZCodeDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir zcode 父目录失败: %v", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 zcode DB 失败: %v", err)
	}
	defer conn.Close()
	schema := `
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		parent_id TEXT NOT NULL DEFAULT '',
		directory TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE model_usage (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		completed_at INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		reasoning_tokens INTEGER,
		cache_creation_input_tokens INTEGER,
		cache_read_input_tokens INTEGER,
		provider_total_tokens INTEGER,
		computed_total_tokens INTEGER
	);`
	if _, err := conn.Exec(schema); err != nil {
		t.Fatalf("创建 zcode 表失败: %v", err)
	}
}

// zcodeUsageInsert 一行 model_usage 的插入参数。
type zcodeUsageInsert struct {
	id            string
	sessionID     string
	model         string
	provider      string
	status        string
	startedAt     int64
	completedAt   int64
	input         int64
	output        int64
	reasoning     int64
	cacheCreate   int64
	cacheRead     int64
	providerTotal int64
	computedTotal int64
}

func insertZCodeUsage(t *testing.T, path string, u zcodeUsageInsert) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open zcode: %v", err)
	}
	defer conn.Close()
	ni := func(v int64) any {
		if v == 0 {
			return nil
		}
		return v
	}
	if _, err := conn.Exec(`INSERT INTO model_usage
		(id, session_id, model_id, provider_id, status, started_at, completed_at,
		 input_tokens, output_tokens, reasoning_tokens,
		 cache_creation_input_tokens, cache_read_input_tokens,
		 provider_total_tokens, computed_total_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.id, u.sessionID, u.model, u.provider, u.status, u.startedAt, u.completedAt,
		ni(u.input), ni(u.output), ni(u.reasoning), ni(u.cacheCreate), ni(u.cacheRead),
		ni(u.providerTotal), ni(u.computedTotal)); err != nil {
		t.Fatalf("insert zcode usage: %v", err)
	}
}

func insertZCodeSession(t *testing.T, path, id, directory string) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open zcode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO session (id, parent_id, directory, title, time_created, time_updated) VALUES (?,?,?,?,0,0)`,
		id, "", directory, ""); err != nil {
		t.Fatalf("insert zcode session: %v", err)
	}
}

func updateZCodeStatus(t *testing.T, path, id, status string, completedAt int64) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open zcode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`UPDATE model_usage SET status=?, completed_at=? WHERE id=?`, status, completedAt, id); err != nil {
		t.Fatalf("update zcode status: %v", err)
	}
}

// createOpenCodeDB 在 path 建与生产 opencode.db 同构的测试库。
func createOpenCodeDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir opencode 父目录失败: %v", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 opencode DB 失败: %v", err)
	}
	defer conn.Close()
	schema := `
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
	if _, err := conn.Exec(schema); err != nil {
		t.Fatalf("创建 opencode 表失败: %v", err)
	}
}

// ocInfo 是 OpenCode message/event data 中的 info 结构（与 collector.openCodeInfo 同构）。
type ocInfo struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func ocInsertSession(t *testing.T, path, id, directory string) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open opencode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO session (id, parent_id, directory, title, model, time_created, time_updated) VALUES (?,?,?,?,?,0,0)`,
		id, "", directory, "", "{}"); err != nil {
		t.Fatalf("insert oc session: %v", err)
	}
}

func ocInsertMessage(t *testing.T, path string, msgID, sessionID string, timeUpdated int64, info ocInfo) {
	t.Helper()
	data, _ := json.Marshal(info)
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open opencode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
		msgID, sessionID, timeUpdated, timeUpdated, string(data)); err != nil {
		t.Fatalf("insert oc message: %v", err)
	}
}

func ocInsertEvent(t *testing.T, path, eventID, aggregateID string, seq int64, eventType string, info ocInfo) {
	t.Helper()
	envelope := struct {
		Info ocInfo `json:"info"`
	}{Info: info}
	data, _ := json.Marshal(envelope)
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open opencode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO event (id, aggregate_id, seq, type, data) VALUES (?,?,?,?,?)`,
		eventID, aggregateID, seq, eventType, string(data)); err != nil {
		t.Fatalf("insert oc event: %v", err)
	}
}

func ocExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open opencode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatalf("exec oc: %v", err)
	}
}

// createCodexStateDB 在 stateDir 下建单个 state_5.sqlite 并插入 threads。
func createCodexStateDB(t *testing.T, stateDir string, threads []codexStateThread) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	dbPath := filepath.Join(stateDir, "state_5.sqlite")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open state DB: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT '',
		model_provider TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		tokens_used INTEGER NOT NULL DEFAULT 0,
		archived INTEGER NOT NULL DEFAULT 0,
		first_user_message TEXT NOT NULL DEFAULT '',
		model TEXT,
		thread_source TEXT,
		agent_role TEXT,
		created_at_ms INTEGER,
		updated_at_ms INTEGER
	)`); err != nil {
		t.Fatalf("create threads table: %v", err)
	}
	for _, th := range threads {
		if _, err := conn.Exec(`INSERT OR REPLACE INTO threads
			(id, rollout_path, created_at, updated_at, source, cwd, title,
			 model, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			th.id, th.rolloutPath, th.createdAt, th.updatedAt, th.source, th.cwd, th.title,
			th.model, th.createdAtMS, th.updatedAtMS); err != nil {
			t.Fatalf("insert thread: %v", err)
		}
	}
}

type codexStateThread struct {
	id          string
	rolloutPath string
	createdAt   int64
	updatedAt   int64
	source      string
	cwd         string
	title       string
	model       string
	createdAtMS int64
	updatedAtMS int64
}

// =========================================================================
//  CLI 跨日 Claude + CC Switch 端到端
// =========================================================================

// TestIT01_ClaudeCrossDayWithCCSwitch 验证 JSONL + router DB → collector →
// 事务写入 → querier 的完整链路。
//
// fixture: testdata/claude/message-level.jsonl（跨 2026-06-22 / 2026-07-08 两日，
// 含 msg-day1 / msg-day2）。router DB 插入 session:msg-day1 / session:msg-day2
// 两条 claude app_type 日志。两日 RunCollect 后断言：
//   - messages 每条 date/model/token 正确
//   - router_provider/router_model/router_name 精确回填（不覆盖 token）
//   - querier 两个日期均有输出
func TestIT01_ClaudeCrossDayWithCCSwitch(t *testing.T) {
	tmp := t.TempDir()

	// 1. projects_dir 放 message-level.jsonl
	projectsDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureSrc := filepath.Join("..", "..", "testdata", "claude", "message-level.jsonl")
	data, err := os.ReadFile(fixtureSrc)
	if err != nil {
		t.Fatalf("读 fixture 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "message-level.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. CC Switch DB：插入匹配 session:msg-day1 / session:msg-day2 的两条 claude 日志
	ccPath := filepath.Join(tmp, "cc-switch.db")
	createCCSwitchDB(t, ccPath)
	insertCCSwitchProvider(t, ccPath, "provider-1", "claude", "Zhipu GLM 宇来")
	// created_at 用 Local 当日正午（秒），与 dateToUnix 口径一致，确保落在对应 date。
	day1Noon := localNoonUnix(t, "2026-06-22")
	day2Noon := localNoonUnix(t, "2026-07-08")
	insertCCSwitchLog(t, ccPath, ccSwitchLogRow{
		requestID: "session:msg-day1", sessionID: "cross-day", appType: "claude",
		model: "glm-5.2", providerID: "provider-1",
		inputTokens: 999, outputToken: 99, createdAt: day1Noon,
	})
	insertCCSwitchLog(t, ccPath, ccSwitchLogRow{
		requestID: "session:msg-day2", sessionID: "cross-day", appType: "claude",
		model: "glm-5.2", providerID: "provider-1",
		inputTokens: 888, outputToken: 88, createdAt: day2Noon,
	})

	// 3. usage DB + 配置 + deps
	usageDB, err := db.Open(filepath.Join(tmp, "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch", Paths: map[string]string{"projects_dir": projectsDir}},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: ccPath},
		},
	}
	deps := NewDeps(cfg)

	ctx := context.Background()
	// 第一次 RunCollect：跨两日
	res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-22", "2026-07-08"}}, true, false)
	if err := ValidateResult("claude", res); err != nil {
		t.Fatalf("首次 RunCollect 失败: %v (result=%+v)", err, res)
	}

	// 4. 断言 messages
	type row struct {
		id, date, model                              string
		input, output, cacheRead, cacheCreate, total int64
		routerProvider, routerModel, routerName      string
	}
	wantRows := map[string]row{
		"msg-day1": {
			id: "msg-day1", date: "2026-06-22", model: "model-a",
			input: 100, output: 10, cacheRead: 20, cacheCreate: 5, total: 135,
			routerProvider: "Zhipu GLM 宇来", routerModel: "glm-5.2", routerName: "cc_switch",
		},
		"msg-day2": {
			id: "msg-day2", date: "2026-07-08", model: "model-b",
			input: 200, output: 30, cacheRead: 40, cacheCreate: 15, total: 285,
			routerProvider: "Zhipu GLM 宇来", routerModel: "glm-5.2", routerName: "cc_switch",
		},
	}
	gotRows := map[string]row{}
	rows, err := usageDB.Query(`SELECT id, date, model, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens, router_provider, router_model, router_name FROM messages WHERE client=? ORDER BY id`, model.ClientClaudeCode)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.date, &r.model, &r.input, &r.output, &r.cacheRead, &r.cacheCreate, &r.total, &r.routerProvider, &r.routerModel, &r.routerName); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		gotRows[r.id] = r
	}
	rows.Close()
	if len(gotRows) != 2 {
		t.Fatalf("期望 2 条 message，实际 %d: %+v", len(gotRows), gotRows)
	}
	for id, want := range wantRows {
		got, ok := gotRows[id]
		if !ok {
			t.Errorf("缺少 message %q", id)
			continue
		}
		if got != want {
			t.Errorf("message %q 不匹配:\n got  = %+v\n want = %+v", id, got, want)
		}
	}

	// 5. querier 验证两个日期均有输出
	q := querier.New(usageDB)
	for _, date := range []string{"2026-06-22", "2026-07-08"} {
		out, err := q.ByClient(ctx, []string{date})
		if err != nil {
			t.Fatalf("querier.ByClient(%s): %v", date, err)
		}
		if !strings.Contains(out, "Claude Code") {
			t.Errorf("querier %s 输出缺少 Claude Code: %q", date, out)
		}
		if !strings.Contains(out, "请求数") {
			t.Errorf("querier %s 输出缺少请求数列: %q", date, out)
		}
	}

	// 6. 第二次 RunCollect（--force 重采）幂等：messages 数不变、token 不被 router 覆盖
	res2 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-06-22", "2026-07-08"}}, true, false)
	if err := ValidateResult("claude", res2); err != nil {
		t.Fatalf("第二次 RunCollect 失败: %v", err)
	}
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientClaudeCode).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("重采后 messages 数 = %d, want 2（主键 upsert 幂等）", count)
	}
	// token 字段不应被 router 覆盖（router token 只入 raw_router_logs，不覆盖 messages token）
	var day1Input int64
	if err := usageDB.QueryRow(`SELECT input_tokens FROM messages WHERE client=? AND id=?`, model.ClientClaudeCode, "msg-day1").Scan(&day1Input); err != nil {
		t.Fatalf("query day1 input: %v", err)
	}
	if day1Input != 100 {
		t.Errorf("msg-day1 input_tokens 重采后 = %d, want 100（router 不覆盖 token）", day1Input)
	}
}

// localNoonUnix 返回 Local 时区指定日期 12:00:00 的 unix 秒。
func localNoonUnix(t *testing.T, date string) int64 {
	t.Helper()
	tt, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		t.Fatalf("parse date %q: %v", date, err)
	}
	return tt.Add(12 * time.Hour).Unix()
}

// =========================================================================
//  Claude branch 去重端到端
// =========================================================================

// TestIT02_ClaudeBranchDedup 采集 parent/child fixture 后查询：
//
//	SELECT id,COUNT(*) FROM messages GROUP BY client,id HAVING COUNT(*)>1;
//
// 期望 0 行（主键 upsert 去重）。再断言共享 ID branch-shared 取较早 ts（parent）
// 的 date/session/directory；parent-only 与 child-only 必须存在。
func TestIT02_ClaudeBranchDedup(t *testing.T) {
	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"branch-parent.jsonl", "branch-child.jsonl"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude", name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(projectsDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	usageDB, err := db.Open(filepath.Join(tmp, "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
		},
	}
	deps := NewDeps(cfg)
	ctx := context.Background()
	res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-07-08"}}, true, false)
	if err := ValidateResult("claude", res); err != nil {
		t.Fatalf("RunCollect 失败: %v", err)
	}

	// 1. 无重复 (client, id)
	dupRows, err := usageDB.Query(`SELECT id, COUNT(*) FROM messages WHERE client=? GROUP BY client, id HAVING COUNT(*) > 1`, model.ClientClaudeCode)
	if err != nil {
		t.Fatalf("query dup: %v", err)
	}
	var dups []string
	for dupRows.Next() {
		var id string
		var c int
		dupRows.Scan(&id, &c)
		dups = append(dups, id)
	}
	dupRows.Close()
	if len(dups) != 0 {
		t.Errorf("发现重复 (client,id): %v（期望 0 行）", dups)
	}

	// 2. 共享 ID branch-shared 取较早 ts（parent 10:01:00 < child 10:01:00 相同？）
	//    parent/child 的 branch-shared 都是 2026-07-08T10:01:00+08:00，ts 相同。
	//    UPSERT ON CONFLICT 在 ts 相同时保留 messages 原值（ELSE 分支），即首次写入的值。
	//    断言 session_id/date/directory 仍为合法值（parent 或 child 之一）。
	var sharedSession, sharedDate, sharedDir string
	if err := usageDB.QueryRow(`SELECT session_id, date, directory FROM messages WHERE client=? AND id=?`,
		model.ClientClaudeCode, "branch-shared").Scan(&sharedSession, &sharedDate, &sharedDir); err != nil {
		t.Fatalf("query branch-shared: %v", err)
	}
	if sharedDate != "2026-07-08" {
		t.Errorf("branch-shared date = %q, want 2026-07-08", sharedDate)
	}
	// session_id 来自文件名（branch-parent / branch-child）。parent/child 的 branch-shared
	// ts 相同（2026-07-08T10:01:00+08:00），UPSERT 在 ts 相等时保留首次写入的 session_id
	// （ELSE 分支），故取先写入文件的 session_id。断言为两个合法文件名之一即可。
	if sharedSession != "branch-parent" && sharedSession != "branch-child" {
		t.Errorf("branch-shared session_id = %q, want branch-parent 或 branch-child", sharedSession)
	}

	// 3. parent-only / child-only 必须存在
	for _, id := range []string{"branch-parent-only", "branch-child-only"} {
		var c int
		if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`, model.ClientClaudeCode, id).Scan(&c); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if c != 1 {
			t.Errorf("message %q count = %d, want 1", id, c)
		}
	}

	// 4. 总 messages 数：branch-shared(1) + parent-only(1) + child-only(1) = 3
	var total int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientClaudeCode).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if total != 3 {
		t.Errorf("total messages = %d, want 3（shared 去重 + parent-only + child-only）", total)
	}
}

// =========================================================================
//  Codex fork 去重端到端
// =========================================================================

// TestIT03_CodexForkDedup 用 ChangedFile 模式分别采集 fork-parent / fork-child rollout，
// UPSERT 后断言无重复、共享派生 ID 取较早归因、child-only 存在。
func TestIT03_CodexForkDedup(t *testing.T) {
	tmp := t.TempDir()
	usageDB, err := db.Open(filepath.Join(tmp, "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"codex": {Enabled: true, Paths: map[string]string{}},
		},
	}
	deps := NewDeps(cfg)
	ctx := context.Background()

	// 先采 child（ts 较大 02:xx），再采 parent（ts 较小 01:xx）
	childPath, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "codex", "fork-child.jsonl"))
	parentPath, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "codex", "fork-parent.jsonl"))

	for _, p := range []string{childPath, parentPath} {
		res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "codex",
			collector.CollectRequest{ChangedFile: p}, true, false)
		if err := ValidateResult("codex", res); err != nil {
			t.Fatalf("RunCollect %s 失败: %v", p, err)
		}
	}

	// 1. 无重复
	dupRows, err := usageDB.Query(`SELECT id, COUNT(*) FROM messages WHERE client IN (?,?) GROUP BY client, id HAVING COUNT(*) > 1`, model.ClientCodexCLI, model.ClientCodexApp)
	if err != nil {
		t.Fatalf("query dup: %v", err)
	}
	var dups []string
	for dupRows.Next() {
		var id string
		var c int
		dupRows.Scan(&id, &c)
		dups = append(dups, id)
	}
	dupRows.Close()
	if len(dups) != 0 {
		t.Errorf("发现重复 (client,id): %v（期望 0 行）", dups)
	}

	// 2. 共享派生 ID msg-shared#0 归因取较早 ts（parent 01:03 < child 02:03）
	var sessionID, date string
	var ts int64
	err = usageDB.QueryRow(`SELECT session_id, date, ts FROM messages WHERE client=? AND id=?`,
		model.ClientCodexCLI, "msg-shared#0").Scan(&sessionID, &date, &ts)
	if err != nil {
		t.Fatalf("query msg-shared#0: %v", err)
	}
	if sessionID != "parent-thread" {
		t.Errorf("msg-shared#0 session_id = %q, want parent-thread（较早 parent 归因）", sessionID)
	}
	parentTS := time.Date(2026, 7, 9, 1, 3, 0, 0, time.UTC).UnixMilli()
	if ts != parentTS {
		t.Errorf("msg-shared#0 ts = %d, want %d（parent 较早 ts）", ts, parentTS)
	}
	// 3. 派生 ID #序号 正确
	for _, derived := range []string{"msg-shared#0", "msg-shared#1"} {
		var c int
		if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`, model.ClientCodexCLI, derived).Scan(&c); err != nil {
			t.Fatalf("query %s: %v", derived, err)
		}
		if c != 1 {
			t.Errorf("派生 ID %q count = %d, want 1", derived, c)
		}
	}
	// 4. child-only msg-child 存在
	var childCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`, model.ClientCodexCLI, "msg-child#0").Scan(&childCount); err != nil {
		t.Fatalf("query msg-child#0: %v", err)
	}
	if childCount != 1 {
		t.Errorf("msg-child#0 count = %d, want 1", childCount)
	}
}

// =========================================================================
//  : ZCode running→completed + daemon restart
// =========================================================================

// TestIT04_IT08_ZCodeRunningCompletedAndRestart 验证：
//  1. 首次 Incremental 在 running 行存在时执行并提交 cursor；
//  2. UPDATE 同一行为 completed 后第二次执行，断言被捕获；
//  3. 关闭 usage DB 并重新 db.Open，插入下一条记录后第三次执行，只新增一条且旧行不重复计数。
func TestIT04_IT08_ZCodeRunningCompletedAndRestart(t *testing.T) {
	tmp := t.TempDir()
	zcodePath := filepath.Join(tmp, "db.sqlite")
	createZCodeDB(t, zcodePath)
	insertZCodeSession(t, zcodePath, "sess-1", "/Users/test/proj-a")

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local).UnixMilli()
	// row1: 初始 running，completed_at 为 NULL
	insertZCodeUsage(t, zcodePath, zcodeUsageInsert{
		id: "usage-1", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "running", startedAt: base,
		input: 100, output: 10, computedTotal: 110,
	})

	usageDBPath := filepath.Join(tmp, "usage.db")
	usageDB, err := db.Open(usageDBPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode": {Enabled: true, Paths: map[string]string{"db": zcodePath}},
		},
	}
	deps := NewDeps(cfg)
	ctx := context.Background()

	// 第一次 Incremental：running 行应被跳过（status != completed），messages 0，cursor 推进
	res1 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "zcode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("zcode", res1); err != nil {
		t.Fatalf("第一次 RunCollect 失败: %v", err)
	}
	var count1 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientZCode).Scan(&count1)
	if count1 != 0 {
		t.Errorf("第一次（running 行）messages = %d, want 0", count1)
	}

	// UPDATE 为 completed（completed_at = base）
	updateZCodeStatus(t, zcodePath, "usage-1", "completed", base)

	// 第二次 Incremental：completed 行被捕获，messages 1
	res2 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "zcode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("zcode", res2); err != nil {
		t.Fatalf("第二次 RunCollect 失败: %v", err)
	}
	var count2 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientZCode).Scan(&count2)
	if count2 != 1 {
		t.Errorf("第二次（completed 行）messages = %d, want 1", count2)
	}

	// 关闭 usage DB 重新打开，插入新行后第三次采集只新增一条
	if err := usageDB.Close(); err != nil {
		t.Fatalf("close usageDB: %v", err)
	}
	usageDB2, err := db.Open(usageDBPath)
	if err != nil {
		t.Fatalf("reopen usageDB: %v", err)
	}
	defer usageDB2.Close()

	// 插入第二条 completed 行
	insertZCodeUsage(t, zcodePath, zcodeUsageInsert{
		id: "usage-2", sessionID: "sess-1", model: "GLM-5.2",
		provider: "p1", status: "completed", startedAt: base + 1000, completedAt: base + 1000,
		input: 200, output: 20, computedTotal: 220,
	})

	res3 := RunCollect(ctx, deps, usageDB2, silentLogger(), io.Discard, "zcode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("zcode", res3); err != nil {
		t.Fatalf("第三次 RunCollect（重启后）失败: %v", err)
	}
	var count3 int
	usageDB2.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientZCode).Scan(&count3)
	if count3 != 2 {
		t.Errorf("重启后 messages = %d, want 2（只新增一条，旧行不重复计数）", count3)
	}
	// 确认两条都在
	for _, id := range []string{"usage-1", "usage-2"} {
		var c int
		usageDB2.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`, model.ClientZCode, id).Scan(&c)
		if c != 1 {
			t.Errorf("重启后 %q count = %d, want 1", id, c)
		}
	}
}

// =========================================================================
//  : OpenCode rewind/event reset
// =========================================================================

// TestIT05_IT06_OpenCodeRewindAndEventReset 验证：
//
//	顺序：创建 completed message/event → 第一次采集（2 条）→ DELETE message →
//	第二次 event 增量（message 主源空，event 补偿源保留已删调用）→ DROP/重建 event 表
//	并插入新 event → 第三次采集（sentinel reset 后新调用被采集）。
//
// 断言：被删调用始终一行、本地旧消息不删除、sentinel reset 后新调用被采集。
func TestIT05_IT06_OpenCodeRewindAndEventReset(t *testing.T) {
	tmp := t.TempDir()

	// OpenCodeCollector 通过 os.UserHomeDir() 推导 cacheDir（~/.cache/opencode），
	// 在构造期读取 models.json。真实 HOME 下该文件结构与 collector 期望的
	// map[string]string 不兼容，故用 t.Setenv("HOME") 指向临时 HOME，放置扁平
	// models.json，使 NewDeps→NewOpenCodeCollector 装配出可用的 collector。
	fakeHome := filepath.Join(tmp, "home")
	cacheDir := filepath.Join(fakeHome, ".cache", "opencode")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "models.json"),
		[]byte(`{"anthropic":"Anthropic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	ocPath := filepath.Join(tmp, "opencode.db")
	createOpenCodeDB(t, ocPath)
	ocInsertSession(t, ocPath, "sess-1", "/Users/test/proj-a")

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local).UnixMilli()
	mkInfo := func(id string, completed int64, total int64) ocInfo {
		var info ocInfo
		info.ID = id
		info.SessionID = "sess-1"
		info.Role = "assistant"
		info.ProviderID = "anthropic"
		info.ModelID = "claude-sonnet"
		info.Time.Created = completed
		info.Time.Completed = completed
		info.Tokens.Total = total
		info.Tokens.Input = total / 2
		info.Tokens.Output = total / 2
		return info
	}

	// 初始：一条 message（msg-a）+ 一条 event（msg-a 终态）
	ocInsertMessage(t, ocPath, "msg-a", "sess-1", base, mkInfo("msg-a", base, 100))
	ocInsertEvent(t, ocPath, "evt-a", "sess-1", 1, "message.updated.1", mkInfo("msg-a", base, 100))

	usageDBPath := filepath.Join(tmp, "usage.db")
	usageDB, err := db.Open(usageDBPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {Enabled: true, Paths: map[string]string{"db": ocPath}},
		},
	}
	deps := NewDeps(cfg)
	ctx := context.Background()

	// 第一次增量采集：message 主源有 msg-a → messages 1
	res1 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "opencode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("opencode", res1); err != nil {
		t.Fatalf("第一次 RunCollect 失败: %v", err)
	}
	var count1 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientOpenCode).Scan(&count1)
	if count1 != 1 {
		t.Errorf("第一次 messages = %d, want 1（msg-a）", count1)
	}

	// DELETE message（模拟 rewind 删除主源行）
	ocExec(t, ocPath, `DELETE FROM message WHERE id=?`, "msg-a")

	// 第二次增量：message 主源空，event 补偿源（msg-a 终态仍存在）应补回。
	// 本地旧消息不删除（messages 账本只 UPSERT，不删除），msg-a 仍 1 条。
	res2 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "opencode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("opencode", res2); err != nil {
		t.Fatalf("第二次 RunCollect 失败: %v", err)
	}
	var count2 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientOpenCode).Scan(&count2)
	if count2 != 1 {
		t.Errorf("第二次 messages = %d, want 1（msg-a 由 event 补偿源保留，本地不删除）", count2)
	}

	// DROP/重建 event 表，插入新 event msg-b（sentinel reset 场景）。
	// event 表 rowid 重置后，旧 cursor.Value 指向的 rowid 不再存在 → sentinel 校验失败 →
	// cursor reset 为 0，从 0 扫描，msg-b 被采集。
	ocExec(t, ocPath, `DROP TABLE event`)
	ocExec(t, ocPath, `CREATE TABLE event (
		id TEXT NOT NULL,
		aggregate_id TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL,
		data TEXT NOT NULL)`)
	ocInsertEvent(t, ocPath, "evt-b", "sess-1", 1, "message.updated.1", mkInfo("msg-b", base+2000, 200))

	// 第三次增量采集：sentinel reset 后 msg-b 被采集
	res3 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "opencode",
		collector.CollectRequest{Incremental: true}, true, false)
	if err := ValidateResult("opencode", res3); err != nil {
		t.Fatalf("第三次 RunCollect 失败: %v", err)
	}
	var count3 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientOpenCode).Scan(&count3)
	if count3 != 2 {
		t.Errorf("第三次 messages = %d, want 2（msg-a + msg-b）", count3)
	}
	// msg-b 必须存在
	var bCount int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`, model.ClientOpenCode, "msg-b").Scan(&bCount)
	if bCount != 1 {
		t.Errorf("msg-b count = %d, want 1（sentinel reset 后新调用被采集）", bCount)
	}
}

// =========================================================================
//  router late arrival
// =========================================================================

// TestIT07_RouterLateArrival 验证：
//  1. 先用 ChangedFile client request 写入 router 空字段（router DB 此时无日志）；
//  2. 再向 router DB 插日志并执行 Source=router；
//  3. 前后比较所有 token/ts/date 字段完全相等，只允许 router_provider/router_model/router_name 从空变为值。
func TestIT07_RouterLateArrival(t *testing.T) {
	tmp := t.TempDir()

	// 1. Claude projects_dir 放一个 fixture
	projectsDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude", "branch-parent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "branch-parent.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. CC Switch DB 初始为空（无 proxy_request_logs 行）
	ccPath := filepath.Join(tmp, "cc-switch.db")
	createCCSwitchDB(t, ccPath)
	insertCCSwitchProvider(t, ccPath, "provider-1", "claude", "Zhipu GLM 宇来")

	usageDBPath := filepath.Join(tmp, "usage.db")
	usageDB, err := db.Open(usageDBPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch", Paths: map[string]string{"projects_dir": projectsDir}},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: ccPath},
		},
	}
	deps := NewDeps(cfg)
	ctx := context.Background()

	// 第一阶段：ChangedFile client request（router DB 空，router_* 字段为空）
	changedFile := filepath.Join(projectsDir, "branch-parent.jsonl")
	res1 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: changedFile}, true, false)
	if err := ValidateResult("claude", res1); err != nil {
		t.Fatalf("第一阶段 client 采集失败: %v", err)
	}

	// 快照所有字段
	type snapshot struct {
		input, output, cacheRead, cacheCreate, total int64
		ts                                           int64
		date, sessionID, directory                   string
		routerProvider, routerModel, routerName      string
	}
	before := map[string]snapshot{}
	rows, err := usageDB.Query(`SELECT id, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens, ts, date, session_id, directory, router_provider, router_model, router_name FROM messages WHERE client=?`, model.ClientClaudeCode)
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	for rows.Next() {
		var id string
		var s snapshot
		if err := rows.Scan(&id, &s.input, &s.output, &s.cacheRead, &s.cacheCreate, &s.total, &s.ts, &s.date, &s.sessionID, &s.directory, &s.routerProvider, &s.routerModel, &s.routerName); err != nil {
			rows.Close()
			t.Fatalf("scan before: %v", err)
		}
		before[id] = s
	}
	rows.Close()
	if len(before) == 0 {
		t.Fatal("第一阶段后无 messages")
	}
	// 确认 router_* 全空
	for id, s := range before {
		if s.routerProvider != "" || s.routerModel != "" || s.routerName != "" {
			t.Errorf("第一阶段后 %q router_* 应为空: %+v", id, s)
		}
	}

	// 第二阶段：向 router DB 插日志（匹配 branch-shared / branch-parent-only）
	dayNoon := localNoonUnix(t, "2026-07-08")
	insertCCSwitchLog(t, ccPath, ccSwitchLogRow{
		requestID: "session:branch-shared", sessionID: "branch-session", appType: "claude",
		model: "glm-5.2", providerID: "provider-1", createdAt: dayNoon,
	})
	insertCCSwitchLog(t, ccPath, ccSwitchLogRow{
		requestID: "session:branch-parent-only", sessionID: "branch-session", appType: "claude",
		model: "glm-5.2", providerID: "provider-1", createdAt: dayNoon,
	})

	// 执行 Source=router
	res2 := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true}, true, false)
	if err := ValidateResult("claude", res2); err != nil {
		t.Fatalf("第二阶段 router 采集失败: %v", err)
	}

	// 断言：token/ts/date/session/directory 完全相等，router_* 从空变为值
	after := map[string]snapshot{}
	rows2, err := usageDB.Query(`SELECT id, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens, ts, date, session_id, directory, router_provider, router_model, router_name FROM messages WHERE client=?`, model.ClientClaudeCode)
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	for rows2.Next() {
		var id string
		var s snapshot
		if err := rows2.Scan(&id, &s.input, &s.output, &s.cacheRead, &s.cacheCreate, &s.total, &s.ts, &s.date, &s.sessionID, &s.directory, &s.routerProvider, &s.routerModel, &s.routerName); err != nil {
			rows2.Close()
			t.Fatalf("scan after: %v", err)
		}
		after[id] = s
	}
	rows2.Close()

	// 有日志的两个 ID 应 router_* 变非空
	expectRouterIDs := map[string]bool{"branch-shared": true, "branch-parent-only": true}
	for id, a := range after {
		b, ok := before[id]
		if !ok {
			t.Errorf("第二阶段后出现新 ID %q（router 路径不应新增 message）", id)
			continue
		}
		// token/ts/date/session/directory 必须完全相等
		if a.input != b.input || a.output != b.output || a.cacheRead != b.cacheRead || a.cacheCreate != b.cacheCreate || a.total != b.total {
			t.Errorf("%q token 字段被 router 修改:\n before=%+v\n after =%+v", id, b, a)
		}
		if a.ts != b.ts || a.date != b.date || a.sessionID != b.sessionID || a.directory != b.directory {
			t.Errorf("%q ts/date/session/directory 被 router 修改:\n before=%+v\n after =%+v", id, b, a)
		}
		// router_*：有日志的从空变值，无日志的保持空
		if expectRouterIDs[id] {
			if a.routerProvider != "Zhipu GLM 宇来" || a.routerModel != "glm-5.2" || a.routerName != "cc_switch" {
				t.Errorf("%q router_* 未回填: before=%+v after=%+v", id, b, a)
			}
		} else {
			if a.routerProvider != "" || a.routerModel != "" || a.routerName != "" {
				t.Errorf("%q（无 router 日志）router_* 不应变: %+v", id, a)
			}
		}
	}
}
