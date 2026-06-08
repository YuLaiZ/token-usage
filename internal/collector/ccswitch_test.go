// internal/collector/ccswitch_test.go
package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

// createCCSwitchTestDB 在 t.TempDir() 下建 cc-switch 测试库（providers + proxy_request_logs）
// seed=true 时插入覆盖四种 app_type 的 fixture（claude、claude-desktop、opencode、codex）；
// 返回 dbPath。fixture created_at = 1781092800（2026-06-10 12:00 UTC，UTC 正午在任意 Local 时区都落在 2026-06-10）
func createCCSwitchTestDB(t *testing.T, seed bool) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	defer db.Close()

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
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}

	if _, err := db.Exec(`INSERT INTO providers (id, app_type, name) VALUES
		('provider-1','claude','Zhipu GLM 宇来'),
		('provider-3','claude-desktop','DeepSeek'),
		('provider-4','opencode','DeepSeek'),
		('provider-5','codex','OpenAI')`); err != nil {
		t.Fatalf("插入 providers 失败: %v", err)
	}

	if seed {
		const day = int64(1781092800) // 2026-06-10 12:00:00 UTC
		inserts := []string{
			fmt.Sprintf(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_cost_usd,latency_ms,status_code,is_streaming,created_at) VALUES ('session:msg-code','req-1','claude','glm-5.2','provider-1',1234,567,100,20,0.005,1200,200,1,%d)`, day),
			fmt.Sprintf(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_cost_usd,latency_ms,status_code,is_streaming,created_at) VALUES ('session:msg-desktop','req-2','claude-desktop','glm-5.2','provider-3',2345,678,200,30,0.008,1500,200,1,%d)`, day+1),
			fmt.Sprintf(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_cost_usd,latency_ms,status_code,is_streaming,created_at) VALUES ('opencode_session:ses-a:msg-open','ses-a','opencode','mimo-v2.5-pro','provider-4',12521,104,1024,0,0.002,800,200,1,%d)`, day+2),
			fmt.Sprintf(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_cost_usd,latency_ms,status_code,is_streaming,created_at) VALUES ('codex_session:ses-b:12','ses-b','codex','gpt-5','provider-5',500,100,0,0,0.001,600,200,1,%d)`, day+3),
		}
		for _, ins := range inserts {
			if _, err := db.Exec(ins); err != nil {
				t.Fatalf("插入 proxy_request_logs 失败: %v", err)
			}
		}
	}
	return dbPath
}

func TestNewCCSwitchAdapter_FromConfig(t *testing.T) {
	cfg := &config.Config{}
	rc := config.RouterConfig{DBPath: "/tmp/cc.db"}
	a := NewCCSwitchAdapter("cc_switch", rc, cfg)
	if a.name != "cc_switch" {
		t.Errorf("name = %q, want cc_switch", a.name)
	}
	if a.dbPath != "/tmp/cc.db" {
		t.Errorf("dbPath = %q, want /tmp/cc.db", a.dbPath)
	}
	// db_path 留空时构造仍成功（dbPath 为空，CollectLogs 时报错——与旧版一致）
	emptyA := NewCCSwitchAdapter("cc_switch", config.RouterConfig{}, cfg)
	if emptyA.dbPath != "" {
		t.Error("未配置 db_path 时 dbPath 应为空")
	}
}

func TestCCSwitchAdapter_Capabilities(t *testing.T) {
	a := &CCSwitchAdapter{}
	caps := a.Capabilities()
	if !caps.Provider || !caps.Model || !caps.InputTokens || !caps.OutputTokens || !caps.CacheTokens {
		t.Errorf("Capabilities 不完整: %+v", caps)
	}
}

// TestCCSwitch_ClaudeRequestID 行为：Claude app_type 的 session:<id> 提取为 messages.id。
// 与 行为 一起覆盖关联范围：只有 claude/claude-desktop 的 session: 前缀生成 MessageID。
func TestCCSwitch_ClaudeRequestID(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	logs := result.Logs
	byMsg := map[string]model.RouterLog{}
	for _, l := range logs {
		byMsg[l.MessageID] = l
	}
	// claude session: 前缀提取
	if _, ok := byMsg["msg-code"]; !ok {
		t.Error("缺 claude session:msg-code 提取")
	}
	// claude-desktop session: 前缀提取
	if _, ok := byMsg["msg-desktop"]; !ok {
		t.Error("缺 claude-desktop session:msg-desktop 提取")
	}
}

// TestCCSwitch_NonClaudeHasNoAssociation 行为：OpenCode/Codex/未知 app_type 的 MessageID
// 为空但 raw log 保留。与 行为 同一 fixture，验证四条 raw log 都返回，
// 但只有前两条 claude/claude-desktop 的 MessageID 非空。
func TestCCSwitch_NonClaudeHasNoAssociation(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	logs := result.Logs
	// 四条 raw log 都返回
	if len(logs) != 4 {
		t.Fatalf("expected 4 raw logs (all app_types preserved), got %d", len(logs))
	}
	// 按 request_id 索引便于断言
	byReq := map[string]model.RouterLog{}
	for _, l := range logs {
		byReq[l.RequestID] = l
	}
	// claude / claude-desktop 的 MessageID 非空
	if byReq["session:msg-code"].MessageID != "msg-code" {
		t.Errorf("claude MessageID = %q, want msg-code", byReq["session:msg-code"].MessageID)
	}
	if byReq["session:msg-desktop"].MessageID != "msg-desktop" {
		t.Errorf("claude-desktop MessageID = %q, want msg-desktop", byReq["session:msg-desktop"].MessageID)
	}
	// opencode / codex 的 MessageID 为空但 raw log 保留
	if byReq["opencode_session:ses-a:msg-open"].MessageID != "" {
		t.Errorf("opencode MessageID = %q, want empty（非 claude app_type 不参与消息关联）",
			byReq["opencode_session:ses-a:msg-open"].MessageID)
	}
	if byReq["codex_session:ses-b:12"].MessageID != "" {
		t.Errorf("codex MessageID = %q, want empty（非 claude app_type 不参与消息关联）",
			byReq["codex_session:ses-b:12"].MessageID)
	}
	// 确认 raw log 真的保留了（RequestID 存在即证明未跳过）
	if _, ok := byReq["opencode_session:ses-a:msg-open"]; !ok {
		t.Error("opencode raw log 被跳过，应保留")
	}
	if _, ok := byReq["codex_session:ses-b:12"]; !ok {
		t.Error("codex raw log 被跳过，应保留")
	}
}

func TestCCSwitchAdapter_CollectLogs_ProviderName(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	logs := result.Logs
	byMsg := map[string]model.RouterLog{}
	for _, l := range logs {
		byMsg[l.MessageID] = l
	}
	// provider name 从 providers 表查（原始名；别名在 merge 层应用）
	if byMsg["msg-code"].ProviderName != "Zhipu GLM 宇来" {
		t.Errorf("ProviderName = %q, want Zhipu GLM 宇来", byMsg["msg-code"].ProviderName)
	}
}

func TestCCSwitchAdapter_CollectLogs_TokenFieldsAllFormats(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	logs := result.Logs
	byReq := map[string]model.RouterLog{}
	for _, l := range logs {
		byReq[l.RequestID] = l
	}
	// 四种 app_type 各自的 token 都应正确提取（避免某格式下字段丢失）
	cases := []struct {
		requestID                       string
		in, out, cacheRead, cacheCreate int64
	}{
		{"session:msg-code", 1234, 567, 100, 20},                 // claude
		{"session:msg-desktop", 2345, 678, 200, 30},              // claude-desktop
		{"opencode_session:ses-a:msg-open", 12521, 104, 1024, 0}, // opencode
		{"codex_session:ses-b:12", 500, 100, 0, 0},               // codex
	}
	for _, c := range cases {
		l, ok := byReq[c.requestID]
		if !ok {
			t.Errorf("缺 %q", c.requestID)
			continue
		}
		if l.InputTokens != c.in || l.OutputTokens != c.out || l.CacheReadTokens != c.cacheRead || l.CacheCreateTokens != c.cacheCreate {
			t.Errorf("%q token = in(%d)/out(%d)/cr(%d)/cc(%d), want %d/%d/%d/%d",
				c.requestID, l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheCreateTokens, c.in, c.out, c.cacheRead, c.cacheCreate)
		}
		if l.RouterName != "cc_switch" {
			t.Errorf("%q RouterName = %q, want cc_switch", c.requestID, l.RouterName)
		}
	}
}

func TestCCSwitchAdapter_CollectLogs_ProviderFallback(t *testing.T) {
	// 场景：provider-fb 只注册在 claude-desktop，proxy_request_logs 错标为 claude（issue #3985）
	// 精确键 (provider-fb|claude) miss，应回退到 providerID-only 返回 name
	dbPath := createCCSwitchTestDB(t, false) // 空请求表
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开 DB 失败: %v", err)
	}
	db.Exec(`INSERT INTO providers (id, app_type, name) VALUES ('provider-fb','claude-desktop','Fallback Provider')`)
	db.Exec(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,created_at) VALUES ('session:fb-1','r','claude','glm-5.2','provider-fb',10,5,0,0,1781092800)`)
	db.Close()

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	logs := result.Logs
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}
	if logs[0].ProviderName != "Fallback Provider" {
		t.Errorf("ProviderName = %q, want Fallback Provider（providerID-only fallback 命中）", logs[0].ProviderName)
	}
}

func TestCCSwitchAdapter_CollectLogs_EmptyDB(t *testing.T) {
	// 建表无数据：返回空切片，不报错（旧版 :memory: 会触发 no such table）
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, false)}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	logs := result.Logs
	if err != nil {
		t.Fatalf("空库不应报错: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("空库期望 0 条，got %d", len(logs))
	}
}

func TestCCSwitchAdapter_CollectLogs_DateFilter(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2025-01-01"}}, slog.Default())
	logs := result.Logs
	if len(logs) != 0 {
		t.Errorf("不匹配日期期望 0 条，got %d", len(logs))
	}
}

func TestCCSwitchAdapter_CollectLogs_NoDBPath(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: ""}
	if _, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default()); err == nil {
		t.Error("dbPath 为空应返回错误")
	}
}

// TestCCSwitch_UnknownRequestID_LogsDebug 验证无法关联消息的 raw log 仍保留，
// 但写 Debug 日志说明"保留 raw 但不参与消息关联"。
func TestCCSwitch_UnknownRequestID_LogsDebug(t *testing.T) {
	// 复用现有 helper，确保 fixture schema 与 CollectLogs 真实查询一致：
	// model / cache_creation_tokens，不得写成 model_name / cache_create_tokens。
	dbPath := createCCSwitchTestDB(t, false)
	testDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open cc-switch fixture: %v", err)
	}
	// 使用与查询日期一致的本地时间戳，禁止手写 epoch 常量。
	createdAt := time.Date(2026, 6, 23, 12, 0, 0, 0, time.Local).Unix()
	_, err = testDB.Exec(`INSERT INTO proxy_request_logs (
		request_id, session_id, app_type, model, provider_id,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		total_cost_usd, latency_ms, status_code, error_message, is_streaming, created_at
	) VALUES (?, 's1', 'claude', 'm1', 'provider-1', 100, 50, 0, 0, 0.01, 100, 200, '', 0, ?)`,
		"unknown_format:123", createdAt)
	if err != nil {
		t.Fatalf("insert proxy_request_logs: %v", err)
	}
	testDB.Close()

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	handler := &testLogHandler{}
	logger := slog.New(handler)

	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-23"}}, logger)
	if err != nil {
		t.Fatalf("CollectLogs failed before request_id validation: %v", err)
	}

	// raw log 保留（不再跳过）
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 raw log preserved, got %d", len(result.Logs))
	}
	if result.Logs[0].MessageID != "" {
		t.Errorf("MessageID = %q, want empty", result.Logs[0].MessageID)
	}
	if !handler.HasMessage("保留 raw 但不参与消息关联") {
		t.Errorf("expected debug record about preserving raw, got messages: %v", handler.Messages())
	}
}

// TestCCSwitchAdapter_CollectLogs_RouterNameUsesConfiguredName 防回归：
// 现有测试都传 "cc_switch"，无法发现"CollectLogs 落库仍写死 ccSwitchRouterName"的漏改。
// 用非标准名 "my_router" 构造 adapter，断言每条 RouterLog.RouterName 使用配置名而非写死常量。
func TestCCSwitchAdapter_CollectLogs_RouterNameUsesConfiguredName(t *testing.T) {
	rc := config.RouterConfig{DBPath: createCCSwitchTestDB(t, true)}
	adapter := NewCCSwitchAdapter("my_router", rc, &config.Config{}) // 非标准名
	result, err := adapter.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	logs := result.Logs
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected at least one log")
	}
	for _, l := range logs {
		if l.RouterName != "my_router" {
			t.Errorf("RouterName = %q, want my_router（应使用配置名，非写死 cc_switch）", l.RouterName)
		}
	}
}

// TestCCSwitch_DateFilter 行为：CLI Dates 使用本地日左闭右开过滤。
// fixture 全在 2026-06-10（UTC 正午，任意 Local 时区都落在当日）。
func TestCCSwitch_DateFilter(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	// 匹配日：返回全部 4 条
	result, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if len(result.Logs) != 4 {
		t.Errorf("匹配日期望 4 条，got %d", len(result.Logs))
	}
	// 非匹配日：返回 0 条
	result2, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2025-01-01"}}, slog.Default())
	if len(result2.Logs) != 0 {
		t.Errorf("不匹配日期望 0 条，got %d", len(result2.Logs))
	}
}

// TestCCSwitch_IncrementalSameSecond 行为：(created_at,request_id) 同秒多行不漏。
// 同一 created_at 下 request-a、request-b，cursor 在 a 时增量只返回 b。
func TestCCSwitch_IncrementalSameSecond(t *testing.T) {
	dbPath := createCCSwitchTestDB(t, false)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	ts := int64(1781092800)
	for _, rid := range []string{"request-a", "request-b"} {
		if _, err := db.Exec(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,created_at) VALUES (?,'s','claude','m','provider-1',10,5,?)`, rid, ts); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	// cursor 在 request-a：增量只返回 request-b
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{
		Incremental: true,
		Cursor:      model.SyncCursor{Value: ts, ID: "request-a"},
	}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 log (request-b only), got %d", len(result.Logs))
	}
	if result.Logs[0].RequestID != "request-b" {
		t.Errorf("RequestID = %q, want request-b", result.Logs[0].RequestID)
	}
}

// TestCCSwitch_IncrementalZeroCursorFullScan 行为：零 cursor 首次返回全量日志，
// NextCursor 指向最大 (created_at,request_id)。
func TestCCSwitch_IncrementalZeroCursorFullScan(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Incremental: true}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	// 全量返回 4 条
	if len(result.Logs) != 4 {
		t.Fatalf("expected 4 logs on first incremental scan, got %d", len(result.Logs))
	}
	// NextCursor 指向最大 (created_at,request_id)：fixture 最大为 day+3 的 codex_session:ses-b:12
	wantCursor := model.SyncCursor{Value: 1781092800 + 3, ID: "codex_session:ses-b:12"}
	if result.NextCursor != wantCursor {
		t.Errorf("NextCursor = %+v, want %+v", result.NextCursor, wantCursor)
	}
}

// TestCCSwitch_IncrementalNoNewRowsKeepsCursor 行为 扩展：增量无新行时返回输入 Cursor。
func TestCCSwitch_IncrementalNoNewRowsKeepsCursor(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	inCursor := model.SyncCursor{Value: 9999999999, ID: "zzz"}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{
		Incremental: true,
		Cursor:      inCursor,
	}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(result.Logs) != 0 {
		t.Errorf("expected 0 new logs, got %d", len(result.Logs))
	}
	// 无新行时返回输入 Cursor，不回退
	if result.NextCursor != inCursor {
		t.Errorf("NextCursor = %+v, want input cursor %+v", result.NextCursor, inCursor)
	}
}

// TestCCSwitch_NonIncrementalNoCursor 行为：非 Incremental 返回零 NextCursor。
func TestCCSwitch_NonIncrementalNoCursor(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	result, _ := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if result.NextCursor != (model.SyncCursor{}) {
		t.Errorf("非 Incremental 期望零 NextCursor, got %+v", result.NextCursor)
	}
}

// TestCCSwitch_IncrementalIgnoresDates 行为：Incremental 为 true 时忽略 Dates，
// 只按复合游标过滤。
func TestCCSwitch_IncrementalIgnoresDates(t *testing.T) {
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: createCCSwitchTestDB(t, true)}
	// 即使 Dates 不匹配 fixture（2025），Incremental 零 cursor 仍返回全部
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{
		Dates:       []string{"2025-01-01"},
		Incremental: true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(result.Logs) != 4 {
		t.Errorf("Incremental 应忽略 Dates，期望 4 条, got %d", len(result.Logs))
	}
}

// TestCCSwitch_ProviderFallback 行为：provider 精确查找 miss 时 ID fallback。
func TestCCSwitch_ProviderFallback(t *testing.T) {
	// 场景：provider-fb 只注册在 claude-desktop，proxy_request_logs 错标为 claude（issue #3985）
	// 精确键 (provider-fb|claude) miss，应回退到 providerID-only 返回 name
	dbPath := createCCSwitchTestDB(t, false)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开 DB 失败: %v", err)
	}
	db.Exec(`INSERT INTO providers (id, app_type, name) VALUES ('provider-fb','claude-desktop','Fallback Provider')`)
	db.Exec(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,created_at) VALUES ('session:fb-1','r','claude','glm-5.2','provider-fb',10,5,0,0,1781092800)`)
	db.Close()

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(result.Logs))
	}
	if result.Logs[0].ProviderName != "Fallback Provider" {
		t.Errorf("ProviderName = %q, want Fallback Provider（providerID-only fallback 命中）", result.Logs[0].ProviderName)
	}
}

// TestCCSwitch_RawDataPreserved 行为：cost/status/error 的 raw_data 和配置 router name 原样保存。
func TestCCSwitch_RawDataPreserved(t *testing.T) {
	dbPath := createCCSwitchTestDB(t, false)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	ts := int64(1781092800)
	if _, err := db.Exec(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_cost_usd,latency_ms,status_code,error_message,is_streaming,created_at) VALUES ('session:raw-1','s','claude','glm-5.2','provider-1',100,50,10,5,0.012,300,200,'some error',1,?)`, ts); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	a := &CCSwitchAdapter{name: "my_router", dbPath: dbPath}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(result.Logs))
	}
	l := result.Logs[0]
	// router name 来自配置
	if l.RouterName != "my_router" {
		t.Errorf("RouterName = %q, want my_router", l.RouterName)
	}
	// raw_data 包含 cost/status/error 字段
	if !strings.Contains(l.RawData, "total_cost_usd") {
		t.Errorf("RawData 缺 total_cost_usd: %s", l.RawData)
	}
	if !strings.Contains(l.RawData, "status_code") {
		t.Errorf("RawData 缺 status_code: %s", l.RawData)
	}
	if !strings.Contains(l.RawData, "some error") {
		t.Errorf("RawData 缺 error_message: %s", l.RawData)
	}
}

// TestCCSwitch_RetrySameMessageReturnsAll 行为：同 message 重试的 raw log 都返回
// （首条选择由 DAO 在 行为 按最早日志归因，collector 层只负责全部返回）。
func TestCCSwitch_RetrySameMessageReturnsAll(t *testing.T) {
	dbPath := createCCSwitchTestDB(t, false)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	ts := int64(1781092800)
	// 同 message id 的两次请求（重试），request_id 相同前缀不同后缀
	for i, suffix := range []string{"retry-1", "retry-2"} {
		rid := "session:" + suffix
		if _, err := db.Exec(`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,created_at) VALUES (?,'s','claude','m','provider-1',?, ?, ?)`,
			rid, 100+i, 50+i, ts+int64(i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	// 两条 raw log 都返回（去重由 DAO 负责）
	if len(result.Logs) != 2 {
		t.Fatalf("expected 2 raw logs (retries preserved), got %d", len(result.Logs))
	}
}
