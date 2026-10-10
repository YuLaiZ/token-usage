package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// ===== V2 测试 fixture =====

// mkdirAllForTest / execTestDB 是测试脚手架（DDL 执行与目录创建）。
func mkdirAllForTest(dir string) error { return os.MkdirAll(dir, 0o755) }

func execTestDB(t *testing.T, dbPath, stmt string) error {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(stmt)
	return err
}

const ocV2TablesDDL = `
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

// createTestOpenCodeV2DB 构造混合布局测试 DB：V1 三表 + V2 两表（空）。
// V2 列结构按迁移语义合成（真实 2.x 完整列清单待实测闭合）。
func createTestOpenCodeV2DB(t *testing.T, dbPath string) {
	t.Helper()
	createTestOpenCodeDB(t, dbPath)
	if err := execTestDB(t, dbPath, ocV2TablesDDL); err != nil {
		t.Fatalf("创建 V2 表失败: %v", err)
	}
}

// ocSessionV2Row 测试插入 session_v2 行。
type ocSessionV2Row struct {
	id          string
	title       string
	directory   string
	timeCreated int64
	timeUpdated int64
}

func insertOCSessionV2(t *testing.T, dbPath string, s ocSessionV2Row) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session_v2 (id,title,directory,time_created,time_updated) VALUES (?,?,?,?,?)`,
		s.id, s.title, s.directory, s.timeCreated, s.timeUpdated); err != nil {
		t.Fatalf("插入 session_v2 %s 失败: %v", s.id, err)
	}
}

// ocV2MessageRow 测试插入 session_message 行（data 为手写 JSON，逼真控制字段形态）。
type ocV2MessageRow struct {
	id          string
	sessionID   string
	timeCreated int64
	timeUpdated int64
	msgType     string
	seq         int64
	data        string
}

func insertOCV2Message(t *testing.T, dbPath string, m ocV2MessageRow) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?,?,?)`,
		m.id, m.sessionID, m.data, m.timeCreated, m.timeUpdated, m.msgType, m.seq); err != nil {
		t.Fatalf("插入 session_message %s 失败: %v", m.id, err)
	}
}

// ocV2AssistantData 构造 V2 assistant 行的 data JSON（无 role 字段；modelJSON 空
// 则省略 model 字段；时间与 token 全量可控）。
func ocV2AssistantData(id, sessionID string, created, completed int64,
	total, input, output, reasoning, cacheRead, cacheWrite int64, modelJSON string) string {
	type tok struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	}
	var tk tok
	tk.Total, tk.Input, tk.Output, tk.Reasoning = total, input, output, reasoning
	tk.Cache.Read, tk.Cache.Write = cacheRead, cacheWrite
	data := map[string]interface{}{
		"id":        id,
		"sessionID": sessionID,
		"time":      map[string]int64{"created": created, "completed": completed},
		"tokens":    tk,
	}
	if modelJSON != "" {
		data["model"] = json.RawMessage(modelJSON)
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// ocV2CompactionData 构造 V2 compaction 行的 data JSON（status 终态、无 role、
// 时间只有 created）。
func ocV2CompactionData(id, sessionID string, created int64, status string,
	total, input, output, reasoning, cacheRead, cacheWrite int64) string {
	type tok struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	}
	var tk tok
	tk.Total, tk.Input, tk.Output, tk.Reasoning = total, input, output, reasoning
	tk.Cache.Read, tk.Cache.Write = cacheRead, cacheWrite
	data := map[string]interface{}{
		"id":        id,
		"sessionID": sessionID,
		"time":      map[string]int64{"created": created},
		"status":    status,
		"tokens":    tk,
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// ===== V2 布局判别 =====

// layoutOfOpenCodeDB 打开源库并探测布局（测试 helper）。
func layoutOfOpenCodeDB(t *testing.T, dbPath string) ocTablePresence {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db 失败: %v", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin tx 失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	p, err := detectOCTables(context.Background(), tx)
	if err != nil {
		t.Fatalf("detectOCTables 失败: %v", err)
	}
	return p
}

// 判别：无任何消息表（全新库）判 V1。
func TestDetectOCLayout_NoTables(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	dir := filepath.Dir(dbPath)
	if err := mkdirAllForTest(dir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE unrelated (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if got := layoutOfOpenCodeDB(t, dbPath).layout(); got != ocLayoutV1 {
		t.Fatalf("layout = %v, want ocLayoutV1", got)
	}
}

// 判别：仅 V1 表（message+session）判 V1。
func TestDetectOCLayout_V1TablesOnly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	if got := layoutOfOpenCodeDB(t, dbPath).layout(); got != ocLayoutV1 {
		t.Fatalf("layout = %v, want ocLayoutV1", got)
	}
}

// 判别：V1 表 + 仅预建空 session_message（1.18 形态，无 session_v2）不判 V2。
func TestDetectOCLayout_PrebuiltSessionMessageOnly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	if err := execTestDB(t, dbPath, `CREATE TABLE session_message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		data TEXT NOT NULL,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if got := layoutOfOpenCodeDB(t, dbPath).layout(); got != ocLayoutV1 {
		t.Fatalf("layout = %v, want ocLayoutV1（单 session_message 不构成 V2）", got)
	}
}

// 判别：session_v2 + session_message 双表（含数据）判 V2。
func TestDetectOCLayout_V2BothTablesWithData(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2", directory: "/p", title: "T"})
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "m2", sessionID: "s2", msgType: "assistant",
		data: ocV2AssistantData("m2", "s2", 1000, 2000, 10, 20, 5, 1, 2, 0, ""),
	})
	p := layoutOfOpenCodeDB(t, dbPath)
	if got := p.layout(); got != ocLayoutV2 {
		t.Fatalf("layout = %v, want ocLayoutV2", got)
	}
	if !p.message || !p.session || !p.event {
		t.Fatalf("V1 表存在性探测不完整: %+v", p)
	}
}

// 判别：V2 布局下 V1 表可能被上游清理（缺表形态仍判 V2）。
func TestDetectOCLayout_V2WithoutV1Tables(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	if err := execTestDB(t, dbPath, `
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
	);`); err != nil {
		t.Fatal(err)
	}
	p := layoutOfOpenCodeDB(t, dbPath)
	if got := p.layout(); got != ocLayoutV2 {
		t.Fatalf("layout = %v, want ocLayoutV2", got)
	}
	if p.message || p.session {
		t.Fatalf("V1 表不应误报存在: %+v", p)
	}
}

// ===== model/provider 双形态解析 =====

// V1 形态：modelID/providerID 字符串字段优先。
func TestResolveOCModelRef_V1FieldsWin(t *testing.T) {
	m, p := resolveOCModelRef("m1", "prov1", json.RawMessage(`{"id":"m2","providerID":"prov2"}`))
	if m != "m1" || p != "prov1" {
		t.Fatalf("got (%q,%q), want (m1,prov1)", m, p)
	}
}

// V2 对象形态：{id,providerID,variant}。
func TestResolveOCModelRef_ModelObject(t *testing.T) {
	m, p := resolveOCModelRef("", "fallback", json.RawMessage(`{"id":"m2","providerID":"prov2","variant":"x"}`))
	if m != "m2" || p != "prov2" {
		t.Fatalf("got (%q,%q), want (m2,prov2)", m, p)
	}
}

// V2 对象形态 providerID 缺失时保留 V1 providerID 字段值。
func TestResolveOCModelRef_ModelObjectKeepsProviderID(t *testing.T) {
	m, p := resolveOCModelRef("", "prov1", json.RawMessage(`{"id":"m2"}`))
	if m != "m2" || p != "prov1" {
		t.Fatalf("got (%q,%q), want (m2,prov1)", m, p)
	}
}

// V2 纯字符串形态：只提供模型 id。
func TestResolveOCModelRef_ModelString(t *testing.T) {
	m, p := resolveOCModelRef("", "prov1", json.RawMessage(`"m-str"`))
	if m != "m-str" || p != "prov1" {
		t.Fatalf("got (%q,%q), want (m-str,prov1)", m, p)
	}
}

// 缺失（V1 行无 model 字段）与非法 JSON 都回退原值。
func TestResolveOCModelRef_MissingOrInvalid(t *testing.T) {
	m, p := resolveOCModelRef("", "prov1", nil)
	if m != "" || p != "prov1" {
		t.Fatalf("nil model: got (%q,%q)", m, p)
	}
	m, p = resolveOCModelRef("", "", json.RawMessage(`{invalid`))
	if m != "" || p != "" {
		t.Fatalf("invalid model: got (%q,%q)", m, p)
	}
}

// V2 转换：assistant 行（model 对象、TS=completed、五分项与 total 直取）。
func TestOpenCodeV2Message_Assistant(t *testing.T) {
	info := openCodeInfo{
		ID:        "m1",
		SessionID: "s1",
		Model:     json.RawMessage(`{"id":"m-obj","providerID":"prov-obj","variant":""}`),
	}
	info.Time.Created = 1000
	info.Time.Completed = 2000
	info.Tokens.Total = 38
	info.Tokens.Input = 10
	info.Tokens.Output = 20
	info.Tokens.Reasoning = 3
	info.Tokens.Cache.Read = 1
	info.Tokens.Cache.Write = 4
	sess := ocSessionData{directory: "/proj/x", title: "T"}
	m := openCodeV2Message("m1", "s1", "assistant", info, sess, nil)
	if m.TS != 2000 || m.Date != "1970-01-01" {
		t.Fatalf("TS/Date = %d/%q（assistant 应取 completed）", m.TS, m.Date)
	}
	if m.Model != "m-obj" || m.Provider != "prov-obj" {
		t.Fatalf("model/provider = %q/%q", m.Model, m.Provider)
	}
	if m.TotalTokens != 38 || m.InputTokens != 10 || m.OutputTokens != 20 ||
		m.ReasoningTokens != 3 || m.CacheReadTokens != 1 || m.CacheCreateTokens != 4 {
		t.Fatalf("tokens = %+v", m)
	}
	if m.Client != model.ClientOpenCode || m.Directory != "/proj/x" || m.Project != "x" {
		t.Fatalf("client/directory/project = %q/%q/%q", m.Client, m.Directory, m.Project)
	}
}

// V2 转换：compaction 行（TS/Date 取 created、status 终态、model 缺失回退 session.model）。
func TestOpenCodeV2Message_Compaction(t *testing.T) {
	info := openCodeInfo{
		ID:     "c1",
		Status: "completed",
	}
	info.Time.Created = 5000
	info.Tokens.Total = 7
	info.Tokens.Output = 7
	sess := ocSessionData{directory: "/p", modelJSON: `{"id":"sess-model","providerID":"sess-prov"}`}
	m := openCodeV2Message("c1", "s1", "compaction", info, sess, nil)
	if m.TS != 5000 {
		t.Fatalf("TS = %d（compaction 应取 created）", m.TS)
	}
	if m.Model != "sess-model" || m.Provider != "sess-prov" {
		t.Fatalf("model/provider = %q/%q（应回退 session.model）", m.Model, m.Provider)
	}
	if m.TotalTokens != 7 || m.OutputTokens != 7 {
		t.Fatalf("tokens = %+v", m)
	}
}

// ===== F2：仅预建空 session_message（1.18 形态）判 V1 =====

func TestOpenCodeV2_PrebuiltSingleTableStaysV1(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	// 1.18 预建形态：仅多一张空 session_message，无 session_v2。
	if err := execTestDB(t, dbPath, `CREATE TABLE session_message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		data TEXT NOT NULL,
		time_created INTEGER NOT NULL DEFAULT 0,
		time_updated INTEGER NOT NULL DEFAULT 0,
		type TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	insertOCSession(t, dbPath, ocSessionRow{id: "s1", directory: "/p", title: "T"})
	ts := ocDateMS(2026, 10, 9, 10, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "m1", sessionID: "s1", timeUpdated: ts,
		info: ocCompletedInfo("m1", "s1", "m1", "anthropic", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true})
	if got := ocMsgsByID(res.Messages)["m1"]; got.ID != "m1" || got.TotalTokens != 100 {
		t.Fatalf("V1 行应照常采集: %+v", got)
	}
	// ""×V1：正常采集成功后写标记 V1。
	if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV1 {
		t.Fatalf("layout mark = %d, want %d（仅预建单表不判 V2）", got.Value, ocLayoutMarkV1)
	}
}

// ===== F3：纯 V2 的 assistant 与 compaction 落账 =====

func TestOpenCodeV2_PureV2FallbackSweep(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2", directory: "/proj/v2", title: "V2T"})

	created1 := ocDateMS(2026, 10, 8, 9, 0)
	completed1 := ocDateMS(2026, 10, 8, 9, 1)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "v2a", sessionID: "s2", timeCreated: created1, timeUpdated: completed1, msgType: "assistant", seq: 1,
		data: ocV2AssistantData("v2a", "s2", created1, completed1, 38, 10, 20, 3, 1, 4, `{"id":"m-obj","providerID":"prov-obj","variant":"v"}`),
	})
	compTS := ocDateMS(2026, 10, 8, 9, 5)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "v2c", sessionID: "s2", timeCreated: compTS, timeUpdated: compTS, msgType: "compaction", seq: 2,
		data: ocV2CompactionData("v2c", "s2", compTS, "completed", 9, 2, 7, 0, 0, 0),
	})
	// failed compaction 终态同样落账（终态到达且用量非全零即导入，含失败请求）。
	compFailTS := ocDateMS(2026, 10, 8, 9, 7)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "v2f", sessionID: "s2", timeCreated: compFailTS, timeUpdated: compFailTS, msgType: "compaction", seq: 3,
		data: ocV2CompactionData("v2f", "s2", compFailTS, "failed", 5, 0, 5, 0, 0, 0),
	})
	// 五分项全零行跳过；未终态行（assistant 无 completed、compaction running）跳过。
	zeroTS := ocDateMS(2026, 10, 8, 9, 9)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "v2z", sessionID: "s2", timeCreated: zeroTS, timeUpdated: zeroTS, msgType: "assistant",
		data: ocV2AssistantData("v2z", "s2", zeroTS, zeroTS, 0, 0, 0, 0, 0, 0, ""),
	})
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "v2r", sessionID: "s2", timeCreated: zeroTS, timeUpdated: zeroTS, msgType: "compaction",
		data: ocV2CompactionData("v2r", "s2", zeroTS, "running", 7, 0, 7, 0, 0, 0),
	})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true})

	byID := ocMsgsByID(res.Messages)
	a := byID["v2a"]
	if a.ID != "v2a" {
		t.Fatalf("assistant 行缺失: %+v", byID)
	}
	if a.TS != completed1 || a.Date != "2026-10-08" {
		t.Fatalf("assistant TS/Date = %d/%q, want completed=%d/2026-10-08", a.TS, a.Date, completed1)
	}
	if a.Model != "m-obj" || a.Provider != "prov-obj" {
		t.Fatalf("model/provider = %q/%q, want m-obj/prov-obj", a.Model, a.Provider)
	}
	if a.InputTokens != 10 || a.OutputTokens != 20 || a.ReasoningTokens != 3 ||
		a.CacheReadTokens != 1 || a.CacheCreateTokens != 4 || a.TotalTokens != 38 {
		t.Fatalf("assistant tokens = %+v", a)
	}
	comp := byID["v2c"]
	if comp.ID != "v2c" || comp.TS != compTS || comp.Date != "2026-10-08" {
		t.Fatalf("compaction TS/Date 应取 created: %+v", comp)
	}
	if comp.OutputTokens != 7 || comp.TotalTokens != 9 {
		t.Fatalf("compaction tokens = %+v", comp)
	}
	if byID["v2f"].ID != "v2f" || byID["v2f"].TotalTokens != 5 {
		t.Fatalf("failed compaction 应落账: %+v", byID["v2f"])
	}
	for _, skipped := range []string{"v2z", "v2r"} {
		if _, ok := byID[skipped]; ok {
			t.Fatalf("%s 不应落账（全零/未终态）", skipped)
		}
	}
	// ""×V2：全量兜底后标记 V2、游标重建为 V2 侧最大 (time_updated,id)。
	if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV2 {
		t.Fatalf("layout mark = %d, want %d", got.Value, ocLayoutMarkV2)
	}
	if got := res.NextCursors[SyncSourceOpenCodeMessage]; got.Value != zeroTS || got.ID != "v2z" {
		t.Fatalf("message 游标应重建为 V2 扫描最大行: %+v", got)
	}
	// 会话元数据来自 session_v2；model/parent 对 V2 行降级为空。
	if len(res.Sessions) != 1 || res.Sessions[0].Title != "V2T" || res.Sessions[0].Directory != "/proj/v2" {
		t.Fatalf("sessions = %+v", res.Sessions)
	}
}

// ===== F5：V2 model 双形态（Collect 级）=====

func TestOpenCodeV2_ModelDualForms(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2", directory: "/p"})
	ts := ocDateMS(2026, 10, 8, 8, 0)
	// 行 1：model 纯字符串形态。
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "ms", sessionID: "s2", timeCreated: ts, timeUpdated: ts, msgType: "assistant",
		data: ocV2AssistantData("ms", "s2", ts, ts, 10, 10, 0, 0, 0, 0, `"m-str"`)})
	// 行 2：model 对象缺 providerID（回退 session.model 的 provider）。
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s3"})
	ts2 := ocDateMS(2026, 10, 8, 8, 1)
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "mo", sessionID: "s3", timeCreated: ts2, timeUpdated: ts2, msgType: "assistant",
		data: ocV2AssistantData("mo", "s3", ts2, ts2, 10, 10, 0, 0, 0, 0, `{"id":"m-obj-only"}`)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-10-08"}})
	byID := ocMsgsByID(res.Messages)
	if got := byID["ms"]; got.Model != "m-str" {
		t.Fatalf("纯字符串 model: %q", got.Model)
	}
	if got := byID["mo"]; got.Model != "m-obj-only" {
		t.Fatalf("对象 model: %q", got.Model)
	}
}

// ===== F6：V2 布局下 event 补偿源不劣化 =====

func TestOpenCodeV2_EventCompensationStillWorks(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2", directory: "/p", title: "ET"})
	ts := ocDateMS(2026, 10, 8, 7, 0)
	// 主源行：event 也提到它 → 主源优先。
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "both", sessionID: "s2", timeCreated: ts, timeUpdated: ts, msgType: "assistant",
		data: ocV2AssistantData("both", "s2", ts, ts, 111, 111, 0, 0, 0, 0, "")})
	// event-only 行：session_message 中不存在 → event 终态补回。
	eo := ocCompletedInfo("ev-only", "s2", "m1", "anthropic", ts, 222, 100, 122)
	insertOCEvent(t, dbPath, ocEventRow{id: "ev-1", aggregateID: "s2", eventType: "message.updated.1", info: eo})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true})
	byID := ocMsgsByID(res.Messages)
	if got := byID["both"]; got.TotalTokens != 111 {
		t.Fatalf("主源应优先于 event: %+v", got)
	}
	if got := byID["ev-only"]; got.ID != "ev-only" || got.TotalTokens != 222 {
		t.Fatalf("event-only 行应由补偿源入账: %+v", got)
	}
}

// ===== 三态判定表：正常增量不写标记 =====

// V1×V1：正常增量，不写布局标记。
func TestOpenCodeV2_MarkedV1OnV1StaysIncremental(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	insertOCSession(t, dbPath, ocSessionRow{id: "s1"})
	ts := ocDateMS(2026, 10, 8, 6, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "m1", sessionID: "s1", timeUpdated: ts,
		info: ocCompletedInfo("m1", "s1", "m1", "anthropic", ts, 100, 60, 40)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceOpenCodeMessage: {Value: ts, ID: "m1"},
		SyncSourceOpenCodeLayout:  {Value: ocLayoutMarkV1},
	}})
	if len(res.Messages) != 0 {
		t.Fatalf("游标已推过应无新行: %+v", res.Messages)
	}
	if _, ok := res.NextCursors[SyncSourceOpenCodeLayout]; ok {
		t.Fatalf("V1×V1 正常增量不应重写布局标记")
	}
}

// V2×V2：正常增量（主源仅扫 session_message），不写布局标记。
func TestOpenCodeV2_MarkedV2OnV2StaysIncremental(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
	ts1 := ocDateMS(2026, 10, 8, 5, 0)
	ts2 := ocDateMS(2026, 10, 8, 5, 1)
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "old", sessionID: "s2", timeCreated: ts1, timeUpdated: ts1, msgType: "assistant",
		data: ocV2AssistantData("old", "s2", ts1, ts1, 10, 10, 0, 0, 0, 0, "")})
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "new", sessionID: "s2", timeCreated: ts2, timeUpdated: ts2, msgType: "assistant",
		data: ocV2AssistantData("new", "s2", ts2, ts2, 20, 20, 0, 0, 0, 0, "")})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceOpenCodeMessage: {Value: ts1, ID: "old"},
		SyncSourceOpenCodeLayout:  {Value: ocLayoutMarkV2},
	}})
	byID := ocMsgsByID(res.Messages)
	if _, ok := byID["old"]; ok {
		t.Fatalf("游标已推过不应重采 old")
	}
	if byID["new"].ID != "new" {
		t.Fatalf("新行应被增量捕获: %+v", byID)
	}
	if _, ok := res.NextCursors[SyncSourceOpenCodeLayout]; ok {
		t.Fatalf("V2×V2 正常增量不应重写布局标记")
	}
}

// ===== F7：V1→V2 迁移 time_updated 两变体（V1×V2 兜底）=====

func TestOpenCodeV2_MigrationTimeUpdatedVariants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		v2Updated int64 // 迁移后的 time_updated：保留（高于旧游标）或下调（低于旧游标）
	}{
		{name: "preserved", v2Updated: ocDateMS(2026, 10, 8, 4, 30)},
		{name: "downgraded", v2Updated: ocDateMS(2026, 10, 7, 23, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "opencode.db")
			createTestOpenCodeV2DB(t, dbPath)
			insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
			created := ocDateMS(2026, 10, 7, 22, 0)
			insertOCV2Message(t, dbPath, ocV2MessageRow{
				id: "mig", sessionID: "s2", timeCreated: created, timeUpdated: tc.v2Updated, msgType: "assistant",
				data: ocV2AssistantData("mig", "s2", created, tc.v2Updated, 33, 33, 0, 0, 0, 0, ""),
			})
			c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
			// 标记 V1（旧版本按 V1 采集过），旧游标=迁移前 V1 time_updated。
			res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
				SyncSourceOpenCodeMessage: {Value: ocDateMS(2026, 10, 8, 4, 0), ID: "old-v1"},
				SyncSourceOpenCodeLayout:  {Value: ocLayoutMarkV1},
			}})
			byID := ocMsgsByID(res.Messages)
			if byID["mig"].ID != "mig" || byID["mig"].TotalTokens != 33 {
				t.Fatalf("迁移行必须经兜底轮捕获（含下调变体）: %+v", byID)
			}
			if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV2 {
				t.Fatalf("layout mark = %d, want V2", got.Value)
			}
		})
	}
}

// ===== F10：旧版游标存在+无标记+V2 源库+新版首次运行 =====

func TestOpenCodeV2_UnmarkedLegacyCursorFullSweep(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
	// 两行：一行的 time_updated 低于旧 V1 时代游标、一行高于。
	low := ocDateMS(2026, 10, 6, 1, 0)
	high := ocDateMS(2026, 10, 9, 2, 0)
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "low", sessionID: "s2", timeCreated: low, timeUpdated: low, msgType: "assistant",
		data: ocV2AssistantData("low", "s2", low, low, 11, 11, 0, 0, 0, 0, "")})
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "high", sessionID: "s2", timeCreated: high, timeUpdated: high, msgType: "assistant",
		data: ocV2AssistantData("high", "s2", high, high, 22, 22, 0, 0, 0, 0, "")})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		// 旧版本写的 V1 时代游标：高于 low 行。
		SyncSourceOpenCodeMessage: {Value: ocDateMS(2026, 10, 7, 0, 0), ID: "v1-era"},
	}})
	byID := ocMsgsByID(res.Messages)
	if byID["low"].ID != "low" || byID["high"].ID != "high" {
		t.Fatalf("不得直接沿用旧游标（low 行必须被兜底捕获）: %+v", byID)
	}
	if got := res.NextCursors[SyncSourceOpenCodeMessage]; got.Value != high || got.ID != "high" {
		t.Fatalf("游标应重建为 V2 扫描最大行: %+v", got)
	}
	if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV2 {
		t.Fatalf("layout mark = %d, want V2", got.Value)
	}
}

// ===== V2×V1 降级兜底（单段；完整往返见 engine 级 F12）=====

func TestOpenCodeV2_DowngradeToV1FullSweep(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeDB(t, dbPath) // 源库已回退 V1 布局（V2 表不存在）
	insertOCSession(t, dbPath, ocSessionRow{id: "s1", directory: "/p"})
	low := ocDateMS(2026, 10, 5, 1, 0)
	high := ocDateMS(2026, 10, 9, 3, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "v1-low", sessionID: "s1", timeUpdated: low,
		info: ocCompletedInfo("v1-low", "s1", "m1", "anthropic", low, 7, 7, 0)})
	insertOCMessage(t, dbPath, ocMessageRow{id: "v1-new", sessionID: "s1", timeUpdated: high,
		info: ocCompletedInfo("v1-new", "s1", "m1", "anthropic", high, 9, 9, 0)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		// 旧 V2 时代游标：高于 v1-low 行（200 > 150 变体）。
		SyncSourceOpenCodeMessage: {Value: ocDateMS(2026, 10, 7, 0, 0), ID: "v2-era"},
		SyncSourceOpenCodeLayout:  {Value: ocLayoutMarkV2},
	}})
	byID := ocMsgsByID(res.Messages)
	if byID["v1-low"].ID != "v1-low" || byID["v1-new"].ID != "v1-new" {
		t.Fatalf("降级轮必须全量兜底（低于旧游标的行也要入账）: %+v", byID)
	}
	if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV1 {
		t.Fatalf("layout mark = %d, want V1（降级）", got.Value)
	}
	if got := res.NextCursors[SyncSourceOpenCodeMessage]; got.Value != high || got.ID != "v1-new" {
		t.Fatalf("游标应重建为 V1 扫描最大行: %+v", got)
	}
}

// ===== F4：混合布局（V2 行 + V1 未迁移行）全量路径 =====

func TestOpenCodeV2_MixedLayoutFullScanMerges(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	// 已迁移会话：session_v2 + session_message（V1 侧行仍在，NOT EXISTS 排除）。
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s-mig", directory: "/mig", title: "MIG"})
	insertOCSession(t, dbPath, ocSessionRow{id: "s-mig", directory: "/mig-old", title: "OLD"})
	migTS := ocDateMS(2026, 10, 4, 1, 0)
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "mig-msg", sessionID: "s-mig", timeCreated: migTS, timeUpdated: migTS, msgType: "assistant",
		data: ocV2AssistantData("mig-msg", "s-mig", migTS, migTS, 50, 50, 0, 0, 0, 0, "")})
	insertOCMessage(t, dbPath, ocMessageRow{id: "mig-msg", sessionID: "s-mig", timeUpdated: migTS,
		info: ocCompletedInfo("mig-msg", "s-mig", "m1", "anthropic", migTS, 99, 99, 0)})
	// 未迁移会话：只在 V1 表。
	insertOCSession(t, dbPath, ocSessionRow{id: "s-old", directory: "/old", title: "UNMIG"})
	oldTS := ocDateMS(2026, 10, 3, 1, 0)
	insertOCMessage(t, dbPath, ocMessageRow{id: "old-msg", sessionID: "s-old", timeUpdated: oldTS,
		info: ocCompletedInfo("old-msg", "s-old", "m1", "anthropic", oldTS, 40, 40, 0)})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Dates: []string{"2026-10-03", "2026-10-04"}})
	byID := ocMsgsByID(res.Messages)
	if got := byID["mig-msg"]; got.TotalTokens != 50 {
		t.Fatalf("同 id 双侧行应 V2 优先不双计: %+v", got)
	}
	if got := byID["old-msg"]; got.TotalTokens != 40 {
		t.Fatalf("V1 未迁移行应被兜底扫描入账: %+v", got)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("期望恰好 2 条（同 id 不双计）: %+v", res.Messages)
	}
	// CLI Dates 模式不产出 NextCursors（含布局标记）。
	if len(res.NextCursors) != 0 {
		t.Fatalf("CLI 模式不应产出 NextCursors: %+v", res.NextCursors)
	}
}

// ===== F9 补充：V2 布局缺表容错（无 event 表 / V1 表被清理）=====

// 纯 V2 形态（仅 session_v2 + session_message，无 V1 表、无 event 表）：
// 采集不断链，event 补偿源自然停更（游标保持输入值），V1 兜底扫描跳过。
func TestOpenCodeV2_MissingTablesTolerated(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	if err := mkdirAllForTest(filepath.Dir(dbPath)); err != nil {
		t.Fatal(err)
	}
	if err := execTestDB(t, dbPath, ocV2TablesDDL); err != nil {
		t.Fatal(err)
	}
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2", directory: "/p", title: "T"})
	ts := ocDateMS(2026, 10, 8, 3, 0)
	insertOCV2Message(t, dbPath, ocV2MessageRow{id: "only", sessionID: "s2", timeCreated: ts, timeUpdated: ts, msgType: "assistant",
		data: ocV2AssistantData("only", "s2", ts, ts, 70, 70, 0, 0, 0, 0, "")})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	eventCursor := model.SyncCursor{Value: 42, ID: "ev-old"}
	res := ocCollect(t, c, CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{
		SyncSourceOpenCodeEvent: eventCursor,
	}})
	if got := ocMsgsByID(res.Messages)["only"]; got.ID != "only" || got.TotalTokens != 70 {
		t.Fatalf("缺表形态下消息应正常落账: %+v", got)
	}
	if got := res.NextCursors[SyncSourceOpenCodeEvent]; got.Value != eventCursor.Value || got.ID != eventCursor.ID {
		t.Fatalf("event 补偿源缺表应保持输入游标（自然停更）: %+v", got)
	}
	if got := res.NextCursors[SyncSourceOpenCodeLayout]; got.Value != ocLayoutMarkV2 {
		t.Fatalf("layout mark = %d, want V2", got.Value)
	}
}

// ===== total 键缺失/null 的 V2 行：不落账 + PartialErr（未知构成保护） =====

// ocV2AssistantDataNoTotal 构造省略 tokens.total 键的 V2 assistant 行。
func ocV2AssistantDataNoTotal(id, sessionID string, created, completed int64,
	input, output int64) string {
	data := map[string]interface{}{
		"id":        id,
		"sessionID": sessionID,
		"time":      map[string]int64{"created": created, "completed": completed},
		"tokens":    map[string]interface{}{"input": input, "output": output},
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// ocV2AssistantDataNullTotal 构造 tokens.total=null 的 V2 assistant 行。
func ocV2AssistantDataNullTotal(id, sessionID string, created, completed int64,
	input, output int64) string {
	return fmt.Sprintf(`{"id":%q,"sessionID":%q,"time":{"created":%d,"completed":%d},"tokens":{"total":null,"input":%d,"output":%d}}`,
		id, sessionID, created, completed, input, output)
}

func TestOpenCodeV2_MissingTotalIsPartialFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func() string
	}{
		{"key-absent", func() string {
			return ocV2AssistantDataNoTotal("nt-1", "s2", 1000, 2000, 50, 60)
		}},
		{"null-value", func() string {
			return ocV2AssistantDataNullTotal("nt-1", "s2", 1000, 2000, 50, 60)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "opencode.db")
			createTestOpenCodeV2DB(t, dbPath)
			insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
			insertOCV2Message(t, dbPath, ocV2MessageRow{
				id: "nt-1", sessionID: "s2", timeCreated: 1000, timeUpdated: 2000, msgType: "assistant",
				data: tc.data(),
			})
			// 同批一条 total 正常行：应照常落账（成功部分入库）。
			tsOK := ocDateMS(2026, 10, 8, 6, 0)
			insertOCV2Message(t, dbPath, ocV2MessageRow{
				id: "ok-1", sessionID: "s2", timeCreated: tsOK, timeUpdated: tsOK, msgType: "assistant",
				data: ocV2AssistantData("ok-1", "s2", tsOK, tsOK, 40, 40, 0, 0, 0, 0, ""),
			})

			c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
			res := ocCollect(t, c, CollectRequest{Incremental: true})
			byID := ocMsgsByID(res.Messages)
			if _, ok := byID["nt-1"]; ok {
				t.Fatalf("total 缺失行不得落账（未知构成不得按 0 入库）")
			}
			if byID["ok-1"].ID != "ok-1" {
				t.Fatalf("成功部分应照常落账: %+v", byID)
			}
			if res.PartialErr == nil {
				t.Fatalf("total 缺失行应产生 PartialErr（游标/标记不推进、可重试）")
			}
		})
	}
}

// total 键存在但值为 0：属「源报零值」而非缺失，按方案直取落账（不触发保护）。
func TestOpenCodeV2_ZeroTotalStillLanded(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
	ts := ocDateMS(2026, 10, 8, 7, 0)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "z-1", sessionID: "s2", timeCreated: ts, timeUpdated: ts, msgType: "assistant",
		data: ocV2AssistantData("z-1", "s2", ts, ts, 0, 50, 60, 0, 0, 0, ""),
	})
	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true})
	m := ocMsgsByID(res.Messages)["z-1"]
	if m.ID != "z-1" {
		t.Fatalf("total=0（键存在）应落账: %+v", m)
	}
	if m.TotalTokens != 0 || m.InputTokens != 50 {
		t.Fatalf("tokens = %+v", m)
	}
	if res.PartialErr != nil {
		t.Fatalf("源报零值不触发保护: %v", res.PartialErr)
	}
}

// 受保护 ID 不得被 event 补偿绕过：主源行 total 缺失（暂缓落账）+ event 表
// 存在同 ID 旧终态（total=100）——event 不得代落（否则旧值覆盖既有账目）。
func TestOpenCodeV2_MissingTotalBlocksEventCompensation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	createTestOpenCodeV2DB(t, dbPath)
	insertOCSessionV2(t, dbPath, ocSessionV2Row{id: "s2"})
	ts := ocDateMS(2026, 10, 8, 9, 0)
	insertOCV2Message(t, dbPath, ocV2MessageRow{
		id: "prot-1", sessionID: "s2", timeCreated: ts, timeUpdated: ts, msgType: "assistant",
		data: ocV2AssistantDataNoTotal("prot-1", "s2", ts, ts, 50, 60),
	})
	// event 终态（旧 total=100）：若保护被绕过，它会以 event-only 形态落账。
	ev := ocCompletedInfo("prot-1", "s2", "m1", "anthropic", ts, 100, 40, 60)
	insertOCEvent(t, dbPath, ocEventRow{id: "ev-1", aggregateID: "s2", eventType: "message.updated.1", info: ev})

	c := newTestOpenCodeCollector(t, dbPath, "../../testdata")
	res := ocCollect(t, c, CollectRequest{Incremental: true})
	byID := ocMsgsByID(res.Messages)
	if _, ok := byID["prot-1"]; ok {
		t.Fatalf("受保护 ID 不得被 event 补偿以旧终态带入: %+v", byID["prot-1"])
	}
	if res.PartialErr == nil {
		t.Fatalf("total 缺失行应产生 PartialErr")
	}
}
