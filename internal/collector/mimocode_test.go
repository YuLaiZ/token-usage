package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// ===== fixture：与生产库同构（session 无 model 列；message 含 agent_id） =====

// createTestMimoCodeDB 构造与生产库同构的测试 DB：
// session(id,parent_id,directory,title,time_created,time_updated) +
// message(id,session_id,agent_id,time_created,time_updated,data)。
// session 刻意不建 model 列（MiMo 库与 OpenCode 的差异），防查询误引用。
func createTestMimoCodeDB(t *testing.T, dbPath string) {
	t.Helper()
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir db 父目录失败: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		parent_id TEXT,
		directory TEXT,
		title TEXT,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT 'main',
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		data TEXT NOT NULL
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("创建表失败: %v", err)
	}
}

type mcSessionRow struct {
	id          string
	parentID    string
	directory   string
	title       string
	timeCreated int64
	timeUpdated int64
}

func insertMCSession(t *testing.T, dbPath string, s mcSessionRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session (id,parent_id,directory,title,time_created,time_updated) VALUES (?,?,?,?,?,?)`,
		s.id, s.parentID, s.directory, s.title, s.timeCreated, s.timeUpdated); err != nil {
		t.Fatalf("插入 session %s 失败: %v", s.id, err)
	}
}

type mcMessageRow struct {
	id          string
	sessionID   string
	agentID     string
	timeCreated int64
	timeUpdated int64
	data        string // 原始 JSON；空则失败 fixture 不可用，调用方必须给值
}

func insertMCMessage(t *testing.T, dbPath string, m mcMessageRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	agentID := m.agentID
	if agentID == "" {
		agentID = "main"
	}
	if _, err := db.Exec(`INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES (?,?,?,?,?,?)`,
		m.id, m.sessionID, agentID, m.timeCreated, m.timeUpdated, m.data); err != nil {
		t.Fatalf("插入 message %s 失败: %v", m.id, err)
	}
}

// mcData 构造 assistant 消息 data JSON（键与 OpenCode envelope 同构）。
func mcData(t *testing.T, id, sessionID, modelID, providerID string, completed int64, total, input, output, reasoning, cacheRead, cacheWrite int64) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id":         id,
		"sessionID":  sessionID,
		"role":       "assistant",
		"modelID":    modelID,
		"providerID": providerID,
		"time":       map[string]any{"created": completed - 1000, "completed": completed},
		"tokens": map[string]any{
			"total":     total,
			"input":     input,
			"output":    output,
			"reasoning": reasoning,
			"cache":     map[string]any{"read": cacheRead, "write": cacheWrite},
		},
	})
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(b)
}

func newTestMimoCodeCollector(t *testing.T, dbPath string) *MimoCodeCollector {
	t.Helper()
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"mimocode": {
				Enabled: true,
				Paths:   map[string]string{"db": dbPath},
			},
		},
	}
	return NewMimoCodeCollector(cfg)
}

// ===== 基础单元测试 =====

func TestMimoCodeCollector_Name(t *testing.T) {
	c := NewMimoCodeCollector(&config.Config{})
	if c.Name() != "mimocode" {
		t.Errorf("Name() = %q, want %q", c.Name(), "mimocode")
	}
}

func TestNewMimoCodeCollector_FromConfig(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"mimocode": {
				Enabled: true,
				Paths:   map[string]string{"db": "/custom/path/mimocode.db"},
			},
		},
	}
	c := NewMimoCodeCollector(cfg)
	if c.dbPath != "/custom/path/mimocode.db" {
		t.Errorf("dbPath = %q, want %q", c.dbPath, "/custom/path/mimocode.db")
	}
}

// ===== 全量采集与字段映射 =====

// TestMimoCodeCollect_FullScan_FieldMapping 全量模式字段映射全量断言。
// 样本取自真实库形态：total = input + output + cache_read（input 为纯新输入，
// 不含 cache_read），mimo-pro 行逐字段核对。
func TestMimoCodeCollect_FullScan_FieldMapping(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{
		id: "ses_1", directory: "/home/u/proj", title: "调试会话",
		timeCreated: 1789031000000, timeUpdated: 1789031200000,
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_1", sessionID: "ses_1", timeCreated: 1789031116578, timeUpdated: 1789031164233,
		data: mcData(t, "msg_1", "ses_1", "mimo-pro", "xiaomi", 1789031164233,
			83375, 57160, 487, 0, 25728, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(result.Messages))
	}
	m := result.Messages[0]
	if m.ID != "msg_1" || m.SessionID != "ses_1" {
		t.Errorf("ID/SessionID = %q/%q, want msg_1/ses_1", m.ID, m.SessionID)
	}
	if m.Client != model.ClientXiaomiMiMoCode {
		t.Errorf("Client = %q, want %q", m.Client, model.ClientXiaomiMiMoCode)
	}
	if m.TS != 1789031164233 || m.Date != time.UnixMilli(1789031164233).Format("2006-01-02") {
		t.Errorf("TS/Date = %d/%q", m.TS, m.Date)
	}
	if m.Model != "mimo-pro" {
		t.Errorf("Model = %q, want mimo-pro", m.Model)
	}
	if m.Provider != "Xiaomi" {
		t.Errorf("Provider = %q, want Xiaomi（xiaomi 静态映射）", m.Provider)
	}
	if m.InputTokens != 57160 || m.FreshInputTokens != 57160 {
		t.Errorf("Input/FreshInput = %d/%d, want 57160/57160（input 直赋不扣 cache）", m.InputTokens, m.FreshInputTokens)
	}
	if m.OutputTokens != 487 || m.ReasoningTokens != 0 {
		t.Errorf("Output/Reasoning = %d/%d, want 487/0", m.OutputTokens, m.ReasoningTokens)
	}
	if m.CacheReadTokens != 25728 || m.CacheCreateTokens != 0 {
		t.Errorf("CacheRead/CacheCreate = %d/%d, want 25728/0", m.CacheReadTokens, m.CacheCreateTokens)
	}
	if m.TotalTokens != 83375 {
		t.Errorf("TotalTokens = %d, want 83375（源值保留）", m.TotalTokens)
	}
	if m.Directory != "/home/u/proj" || m.Project != "proj" {
		t.Errorf("Directory/Project = %q/%q, want /home/u/proj/proj", m.Directory, m.Project)
	}
	// 全量模式不返回 NextCursors。
	if result.NextCursors != nil {
		t.Errorf("NextCursors = %v, want nil（全量模式）", result.NextCursors)
	}
}

// TestMimoCodeCollect_FreshInputNotSubtracted 口径防回归：
// MiMo 的 input 为纯新输入（恒等式 total==input+output+cache_read），
// FreshInput 必须等于 input，不做 SubtractCache。
func TestMimoCodeCollect_FreshInputNotSubtracted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	// total = input + output + cache_read 的恒等式样本。
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_1", sessionID: "ses_1", timeUpdated: 100,
		data: mcData(t, "msg_1", "ses_1", "mimo-x-pro-preview", "xiaomi", 100, 88994, 5916, 198, 0, 82880, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(result.Messages))
	}
	m := result.Messages[0]
	if m.FreshInputTokens != m.InputTokens {
		t.Errorf("FreshInput = %d, want == Input %d（直赋，不扣 cache_read %d）",
			m.FreshInputTokens, m.InputTokens, m.CacheReadTokens)
	}
}

// TestMimoCodeCollect_FiltersInvalidRows 空 assistant 行过滤：
// total 缺失（SQL 侧 NULL）/ completed 缺失的 assistant 行与 user 行均不产出。
// total 与 completed 两个谓词必须都写（实测数据中 total 非空行其 completed
// 亦非空，不能只写其一）。
func TestMimoCodeCollect_FiltersInvalidRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})

	valid := mcData(t, "msg_ok", "ses_1", "mimo-pro", "xiaomi", 1000, 10, 5, 5, 0, 0, 0)
	// total 为 NULL：手工构造去掉 tokens.total 键（Go struct Total=0 也无法区分 NULL，
	// 用原始 JSON 表达 NULL 语义——键缺失即 SQL 侧 NULL）。
	noTotal := `{"id":"msg_no_total","sessionID":"ses_1","role":"assistant","modelID":"mimo-pro","providerID":"xiaomi","time":{"created":1,"completed":2000},"tokens":{"input":1,"output":1}}`
	noCompleted := `{"id":"msg_no_completed","sessionID":"ses_1","role":"assistant","modelID":"mimo-pro","providerID":"xiaomi","time":{"created":1},"tokens":{"total":9,"input":4,"output":5}}`
	userRow := `{"id":"msg_user","sessionID":"ses_1","role":"user","time":{"created":1,"completed":3000}}`

	for i, row := range []struct {
		id string
		d  string
	}{
		{"msg_ok", valid},
		{"msg_no_total", noTotal},
		{"msg_no_completed", noCompleted},
		{"msg_user", userRow},
	} {
		insertMCMessage(t, dbPath, mcMessageRow{id: row.id, sessionID: "ses_1", timeUpdated: int64(i), data: row.d})
	}

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg_ok" {
		t.Fatalf("messages = %+v, want 仅 [msg_ok]", result.Messages)
	}
}

// TestMimoCodeCollect_AgentIDNotFiltered 防回归：agent_id 非 main 的有效行
// 照常产出（真实库中已出现 general-1 等后台 agent，过滤不得依赖 agent_id）。
func TestMimoCodeCollect_AgentIDNotFiltered(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_main", sessionID: "ses_1", agentID: "main", timeUpdated: 1,
		data: mcData(t, "msg_main", "ses_1", "mimo-pro", "xiaomi", 1000, 10, 5, 5, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_general", sessionID: "ses_1", agentID: "general-1", timeUpdated: 2,
		data: mcData(t, "msg_general", "ses_1", "mimo-pro", "xiaomi", 2000, 20, 8, 12, 0, 0, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("messages = %d, want 2（不得按 agent_id 过滤）", len(result.Messages))
	}
	for _, m := range result.Messages {
		if m.ID != "msg_main" && m.ID != "msg_general" {
			t.Errorf("unexpected message %q", m.ID)
		}
	}
}

// TestMimoCodeCollect_DatesFilter 按 completed 本机时区日期过滤。
// 时间戳用 time.Date(..., time.Local) 构造正午，规避跨时区日期偏移。
func TestMimoCodeCollect_DatesFilter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	day1 := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local).UnixMilli()
	day2 := time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local).UnixMilli()
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_d1", sessionID: "ses_1", timeUpdated: day1,
		data: mcData(t, "msg_d1", "ses_1", "mimo-pro", "xiaomi", day1, 10, 5, 5, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_d2", sessionID: "ses_1", timeUpdated: day2,
		data: mcData(t, "msg_d2", "ses_1", "mimo-pro", "xiaomi", day2, 20, 8, 12, 0, 0, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(),
		CollectRequest{Dates: []string{"2026-09-10"}}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg_d2" {
		t.Fatalf("messages = %+v, want 仅 [msg_d2]", result.Messages)
	}
}

// ===== 增量游标 =====

// TestMimoCodeCollect_IncrementalCursor 增量三轮：
// 首轮全量推进 cursor；后续轮重放输入 cursor 所在毫秒桶并取得更新行；
// 无新行时仍重放最后一个毫秒桶且保持 cursor。
func TestMimoCodeCollect_IncrementalCursor(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_1", sessionID: "ses_1", timeUpdated: 100,
		data: mcData(t, "msg_1", "ses_1", "mimo-pro", "xiaomi", 1000, 10, 5, 5, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_2", sessionID: "ses_1", timeUpdated: 200,
		data: mcData(t, "msg_2", "ses_1", "mimo-pro", "xiaomi", 2000, 20, 8, 12, 0, 0, 0),
	})

	c := newTestMimoCodeCollector(t, dbPath)
	ctx := context.Background()

	// 首轮：全量，cursor 推进到 (200, msg_2)。
	r1, err := c.Collect(ctx, CollectRequest{Incremental: true}, nil)
	if err != nil {
		t.Fatalf("round1: %v", err)
	}
	if len(r1.Messages) != 2 {
		t.Fatalf("round1 messages = %d, want 2", len(r1.Messages))
	}
	cur := r1.NextCursors[SyncSourceMimoCodeMessage]
	if cur.Value != 200 || cur.ID != "msg_2" {
		t.Fatalf("round1 cursor = (%d,%q), want (200,msg_2)", cur.Value, cur.ID)
	}

	// 次轮：新增 msg_3，重放 cursor 所在的 msg_2 并取得新行，cursor 推进；同时带上与所有行 completed 日期
	// 都不同的 Dates 验证增量模式忽略日期过滤（若误按日期过滤，新行将被
	// 2020-06-01 排除导致测试失败）。
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_3", sessionID: "ses_1", timeUpdated: 300,
		data: mcData(t, "msg_3", "ses_1", "mimo-pro", "xiaomi", 3000, 30, 10, 20, 0, 0, 0),
	})
	r2, err := c.Collect(ctx, CollectRequest{
		Incremental: true,
		Dates:       []string{"2020-06-01"},
		Cursors: map[string]model.SyncCursor{
			SyncSourceMimoCodeMessage: cur,
		},
	}, nil)
	if err != nil {
		t.Fatalf("round2: %v", err)
	}
	if len(r2.Messages) != 2 || r2.Messages[0].ID != "msg_2" || r2.Messages[1].ID != "msg_3" {
		t.Fatalf("round2 messages = %+v, want 重放 msg_2 并采集 msg_3", r2.Messages)
	}
	cur2 := r2.NextCursors[SyncSourceMimoCodeMessage]
	if cur2.Value != 300 || cur2.ID != "msg_3" {
		t.Fatalf("round2 cursor = (%d,%q), want (300,msg_3)", cur2.Value, cur2.ID)
	}

	// 第三轮：无新行，仍重放 cursor 所在毫秒桶（依赖上层 UPSERT 幂等），
	// 但 cursor 保持输入值。
	r3, err := c.Collect(ctx, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceMimoCodeMessage: cur2,
	}}, nil)
	if err != nil {
		t.Fatalf("round3: %v", err)
	}
	if len(r3.Messages) != 1 || r3.Messages[0].ID != "msg_3" {
		t.Fatalf("round3 messages = %+v, want 重放 [msg_3]", r3.Messages)
	}
	if cur3 := r3.NextCursors[SyncSourceMimoCodeMessage]; cur3 != cur2 {
		t.Fatalf("round3 cursor = %v, want 保持输入 %v", cur3, cur2)
	}
}

// TestMimoCodeCollect_CursorAdvancesPastSkippedRows 游标推进不依赖该行可转换：
// SQL 命中的行被 Go 侧跳过（零 token，或 JSON 字段类型不匹配导致 Unmarshal
// 失败）时不产出，但游标照常跨过（防增量轮询永久重扫尾行）。
// 类型不匹配行（如 tokens.total 为字符串）能通过 SQL 谓词——json_extract 对
// 合法 JSON 返回 TEXT 仍满足 IS NOT NULL——正是 Unmarshal 失败分支的可达路径。
func TestMimoCodeCollect_CursorAdvancesPastSkippedRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_zero", sessionID: "ses_1", timeUpdated: 100,
		data: mcData(t, "msg_zero", "ses_1", "mimo-pro", "xiaomi", 1000, 0, 0, 0, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_ok", sessionID: "ses_1", timeUpdated: 200,
		data: mcData(t, "msg_ok", "ses_1", "mimo-pro", "xiaomi", 2000, 10, 5, 5, 0, 0, 0),
	})
	// 合法 JSON 但 tokens.total 为字符串：SQL 谓词命中（TEXT 非 NULL），
	// Go 侧 Unmarshal 到 int64 失败，该行不产出。刻意放在最大 time_updated
	// 作尾行——游标必须停在它身上，才能证明"跳过行也推进游标"（若推进只
	// 发生在转换成功之后，游标会停在 msg_ok 导致该行被永久重扫）。
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_badtype", sessionID: "ses_1", timeUpdated: 300,
		data: `{"id":"msg_badtype","sessionID":"ses_1","role":"assistant","modelID":"mimo-pro","providerID":"xiaomi","time":{"created":1,"completed":3000},"tokens":{"total":"15","input":"5","output":"10"}}`,
	})

	c := newTestMimoCodeCollector(t, dbPath)
	r1, err := c.Collect(context.Background(), CollectRequest{Incremental: true}, nil)
	if err != nil {
		t.Fatalf("round1: %v", err)
	}
	if len(r1.Messages) != 1 || r1.Messages[0].ID != "msg_ok" {
		t.Fatalf("messages = %+v, want 仅 [msg_ok]（零 token 与类型不匹配行不产出）", r1.Messages)
	}
	cur := r1.NextCursors[SyncSourceMimoCodeMessage]
	if cur.Value != 300 || cur.ID != "msg_badtype" {
		t.Fatalf("cursor = (%d,%q), want (300,msg_badtype)（尾行被跳过也必须推进游标）", cur.Value, cur.ID)
	}
	// 次轮重放高水位桶；尾行仍因类型不匹配而不产出，也不阻碍 cursor 保持。
	r2, err := c.Collect(context.Background(), CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceMimoCodeMessage: cur,
	}}, nil)
	if err != nil {
		t.Fatalf("round2: %v", err)
	}
	if len(r2.Messages) != 0 {
		t.Fatalf("round2 messages = %+v, want 0（重放的尾行仍不可转换）", r2.Messages)
	}
}

// TestMimoCodeCollect_ReplaysHighWaterForLateSameTimestamp 防止同毫秒后到更新漏采：
// 首轮已把 msg-z 的 (time_updated,id) 写成 cursor；随后写入同一 time_updated、
// 但字典序更小的 msg-a。若仍按 (time_updated,id)>cursor 查询，msg-a 会被永久跳过。
// 现在必须重放完整高水位桶，交由上层 UPSERT 去重已有 msg-z。
func TestMimoCodeCollect_ReplaysHighWaterForLateSameTimestamp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg-z", sessionID: "ses_1", timeUpdated: 5000,
		data: mcData(t, "msg-z", "ses_1", "mimo-pro", "xiaomi", 5000, 10, 5, 5, 0, 0, 0),
	})

	c := newTestMimoCodeCollector(t, dbPath)
	first, err := c.Collect(context.Background(), CollectRequest{Incremental: true}, nil)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	cursor := first.NextCursors[SyncSourceMimoCodeMessage]
	if cursor.Value != 5000 || cursor.ID != "msg-z" {
		t.Fatalf("first cursor = (%d,%q), want (5000,msg-z)", cursor.Value, cursor.ID)
	}

	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg-a", sessionID: "ses_1", timeUpdated: 5000,
		data: mcData(t, "msg-a", "ses_1", "mimo-pro", "xiaomi", 6000, 20, 8, 12, 0, 0, 0),
	})
	res, err := c.Collect(context.Background(), CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceMimoCodeMessage: cursor,
	}}, nil)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	byID := make(map[string]bool, len(res.Messages))
	for _, m := range res.Messages {
		byID[m.ID] = true
	}
	if !byID["msg-a"] || !byID["msg-z"] || len(byID) != 2 {
		t.Errorf("高水位桶应重放 msg-a/msg-z，实际 %v", byID)
	}
	next := res.NextCursors[SyncSourceMimoCodeMessage]
	if next != cursor {
		t.Errorf("second cursor = %v, want 保持高水位 %v", next, cursor)
	}
}

// TestMimoCodeCollect_DataJSONMissingIDKeys data JSON 不含 id/sessionID 键时
// 回填列值（真实库消息 envelope 即不带这两个键，回填是常态路径）。
func TestMimoCodeCollect_DataJSONMissingIDKeys(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_noid", sessionID: "ses_1", timeUpdated: 100,
		data: `{"role":"assistant","modelID":"mimo-pro","providerID":"xiaomi","time":{"created":1,"completed":1000},"tokens":{"total":10,"input":5,"output":5}}`,
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(result.Messages))
	}
	m := result.Messages[0]
	if m.ID != "msg_noid" || m.SessionID != "ses_1" {
		t.Errorf("ID/SessionID = %q/%q, want 回填列值 msg_noid/ses_1", m.ID, m.SessionID)
	}
}

// ===== provider 映射 =====

func TestMimoCodeProviderMapping(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{id: "ses_1"})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_known", sessionID: "ses_1", timeUpdated: 1,
		data: mcData(t, "msg_known", "ses_1", "mimo-pro", "xiaomi", 1000, 10, 5, 5, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_unknown", sessionID: "ses_1", timeUpdated: 2,
		data: mcData(t, "msg_unknown", "ses_1", "mimo-pro", "xiaomi-token-plan-cn", 2000, 20, 8, 12, 0, 0, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	byID := make(map[string]model.Message, len(result.Messages))
	for _, m := range result.Messages {
		byID[m.ID] = m
	}
	if got := byID["msg_known"].Provider; got != "Xiaomi" {
		t.Errorf("known provider = %q, want Xiaomi", got)
	}
	if got := byID["msg_unknown"].Provider; got != "xiaomi-token-plan-cn" {
		t.Errorf("unknown provider = %q, want 原值回退 xiaomi-token-plan-cn", got)
	}
}

// ===== Session 元数据 =====

func TestMimoCodeCollect_SessionMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	insertMCSession(t, dbPath, mcSessionRow{
		id: "ses_parent", directory: "/home/u/parent", timeCreated: 1, timeUpdated: 2,
	})
	// 子代理会话：parent_id 非空，title 含中文。
	insertMCSession(t, dbPath, mcSessionRow{
		id: "ses_child", parentID: "ses_parent", directory: "/home/u/child", title: "子任务",
		timeCreated: 100, timeUpdated: 200,
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_p", sessionID: "ses_parent", timeUpdated: 10,
		data: mcData(t, "msg_p", "ses_parent", "mimo-pro", "xiaomi", 1000, 10, 5, 5, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_c1", sessionID: "ses_child", timeUpdated: 20,
		data: mcData(t, "msg_c1", "ses_child", "mimo-pro", "xiaomi", 2000, 20, 8, 12, 0, 0, 0),
	})
	insertMCMessage(t, dbPath, mcMessageRow{
		id: "msg_c2", sessionID: "ses_child", timeUpdated: 30,
		data: mcData(t, "msg_c2", "ses_child", "mimo-pro", "xiaomi", 3000, 30, 10, 20, 0, 0, 0),
	})

	result, err := newTestMimoCodeCollector(t, dbPath).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2（按 session 去重）", len(result.Sessions))
	}
	byID := make(map[string]model.Session, len(result.Sessions))
	for _, s := range result.Sessions {
		byID[s.ID] = s
	}
	p := byID["ses_parent"]
	if p.Client != model.ClientXiaomiMiMoCode || p.Directory != "/home/u/parent" || p.Project != "parent" ||
		p.ParentID != "" || p.FirstTS != 1 || p.LastTS != 2 {
		t.Errorf("parent session = %+v", p)
	}
	c := byID["ses_child"]
	if c.Title != "子任务" || c.ParentID != "ses_parent" || c.FirstTS != 100 || c.LastTS != 200 {
		t.Errorf("child session = %+v", c)
	}
}

// ===== 错误路径 =====

func TestMimoCodeCollect_Errors(t *testing.T) {
	ctx := context.Background()

	// 未配置 db。
	c := NewMimoCodeCollector(&config.Config{Clients: map[string]config.Client{"mimocode": {Enabled: true}}})
	if _, err := c.Collect(ctx, CollectRequest{}, nil); err == nil {
		t.Error("未配置 db 应报错")
	}

	// 文件不存在。
	c2 := newTestMimoCodeCollector(t, filepath.Join(t.TempDir(), "missing.db"))
	if _, err := c2.Collect(ctx, CollectRequest{}, nil); err == nil {
		t.Error("db 文件不存在应报错")
	}

	// 非普通文件（目录）。
	dir := t.TempDir()
	c3 := newTestMimoCodeCollector(t, dir)
	if _, err := c3.Collect(ctx, CollectRequest{}, nil); err == nil {
		t.Error("db 路径为目录应报错")
	}
}

// TestMimoCodeCollect_ContextCancelled ctx 取消返回 context error。
func TestMimoCodeCollect_ContextCancelled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	createTestMimoCodeDB(t, dbPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestMimoCodeCollector(t, dbPath).Collect(ctx, CollectRequest{}, nil); err == nil {
		t.Error("ctx 取消应返回 error")
	}
}
