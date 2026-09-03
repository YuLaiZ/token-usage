// internal/collector/ccswitch_datasource_test.go
// schema v3 采集合同：data_source / request_model / input_token_semantics 三列
// 按列粒度探测采集（旧版 cc-switch 库缺列时独立降级，不做整体豁免）。
package collector

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

// createCCSwitchColumnVariantDB 建指定可选列组合的 cc-switch fixture 库。
// 三个开关分别控制 data_source / request_model / input_token_semantics 列是否建出，
// 用于覆盖新版库、中间版本库与最老库的按列粒度分支。
func createCCSwitchColumnVariantDB(t *testing.T, withDataSource, withRequestModel, withSemantics bool) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cc-switch.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Exec(`CREATE TABLE providers (id TEXT PRIMARY KEY, app_type TEXT NOT NULL, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("建 providers 表失败: %v", err)
	}
	schema := `CREATE TABLE proxy_request_logs (
		request_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, app_type TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '', provider_id TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		total_cost_usd REAL NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0,
		status_code INTEGER NOT NULL DEFAULT 0, error_message TEXT,
		is_streaming INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL`
	if withDataSource {
		schema += `, data_source TEXT NOT NULL DEFAULT 'proxy'`
	}
	if withRequestModel {
		schema += `, request_model TEXT NOT NULL DEFAULT ''`
	}
	if withSemantics {
		schema += `, input_token_semantics INTEGER NOT NULL DEFAULT 0`
	}
	schema += `)`
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return dbPath
}

// ccSwitchFixtureRow 描述一行 fixture；dataSource/requestModel/semantics 为 nil
// 表示该列在当前 fixture 库中不存在（对应旧版 cc-switch schema）。
type ccSwitchFixtureRow struct {
	requestID, sessionID, appType, model string
	dataSource                           *string
	requestModel                         *string
	semantics                            *int
	createdAt                            int64
}

func fixtureStr(s string) *string { return &s }
func fixtureInt(i int) *int       { return &i }

// insertCCSwitchRows 向 fixture 库插入若干行（列名自适应可选列组合）。
func insertCCSwitchRows(t *testing.T, dbPath string, rows ...ccSwitchFixtureRow) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试 DB 失败: %v", err)
	}
	defer raw.Close()
	for _, r := range rows {
		cols := `request_id, session_id, app_type, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, status_code, is_streaming, created_at`
		vals := []any{r.requestID, r.sessionID, r.appType, r.model, 100, 50, 0, 0, 200, 0, r.createdAt}
		ph := `?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`
		if r.dataSource != nil {
			cols += `, data_source`
			ph += `, ?`
			vals = append(vals, *r.dataSource)
		}
		if r.requestModel != nil {
			cols += `, request_model`
			ph += `, ?`
			vals = append(vals, *r.requestModel)
		}
		if r.semantics != nil {
			cols += `, input_token_semantics`
			ph += `, ?`
			vals = append(vals, *r.semantics)
		}
		if _, err := raw.Exec(`INSERT INTO proxy_request_logs (`+cols+`) VALUES (`+ph+`)`, vals...); err != nil {
			t.Fatalf("插入 fixture 行失败: %v", err)
		}
	}
}

// TestCCSwitch_ModernDBCollectsDataSourceAndRequestColumns：新版库（三列齐）混存
// codex proxy / codex_session / claude 三类行——DataSource 落库值、raw_data JSON
// 含 request_model 与 input_token_semantics、codex proxy 行提取 message_id。
func TestCCSwitch_ModernDBCollectsDataSourceAndRequestColumns(t *testing.T) {
	const day = int64(1781092800) // 2026-06-10 12:00:00 UTC
	dbPath := createCCSwitchColumnVariantDB(t, true, true, true)
	insertCCSwitchRows(t, dbPath,
		ccSwitchFixtureRow{"session:codex:provider-9:resp_001", "codex_11111111-1111-1111-1111-111111111111",
			"codex", "gpt-5.6-terra", fixtureStr("proxy"), fixtureStr("gpt-5.6"), fixtureInt(1), day},
		ccSwitchFixtureRow{"codex_session:thread-v1:22222222-2222-2222-2222-222222222222:3",
			"22222222-2222-2222-2222-222222222222", "codex", "gpt-5.6-sol", fixtureStr("codex_session"), fixtureStr("gpt-5.6-sol"), fixtureInt(0), day + 10},
		ccSwitchFixtureRow{"session:msg_claude_77", "33333333-3333-3333-3333-333333333333",
			"claude", "glm-5.3", fixtureStr("proxy"), fixtureStr("claude-opus-4-8"), fixtureInt(2), day + 20},
	)

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	if len(result.Logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(result.Logs))
	}
	byReq := map[string]model.RouterLog{}
	for _, l := range result.Logs {
		byReq[l.RequestID] = l
	}

	proxy := byReq["session:codex:provider-9:resp_001"]
	if proxy.DataSource != "proxy" {
		t.Errorf("codex proxy 行 DataSource = %q, want proxy", proxy.DataSource)
	}
	if proxy.MessageID != "resp_001" {
		t.Errorf("codex proxy 行 MessageID = %q, want resp_001", proxy.MessageID)
	}
	if !strings.Contains(proxy.RawData, `"request_model":"gpt-5.6"`) {
		t.Errorf("codex proxy 行 raw_data 缺 request_model: %s", proxy.RawData)
	}
	if !strings.Contains(proxy.RawData, `"input_token_semantics":1`) {
		t.Errorf("codex proxy 行 raw_data 缺 input_token_semantics: %s", proxy.RawData)
	}

	cs := byReq["codex_session:thread-v1:22222222-2222-2222-2222-222222222222:3"]
	if cs.DataSource != "codex_session" {
		t.Errorf("codex_session 行 DataSource = %q, want codex_session", cs.DataSource)
	}
	if cs.MessageID != "" {
		t.Errorf("codex_session 行 MessageID = %q, want empty（不参与消息关联）", cs.MessageID)
	}

	cl := byReq["session:msg_claude_77"]
	if cl.DataSource != "proxy" {
		t.Errorf("claude 行 DataSource = %q, want proxy", cl.DataSource)
	}
	if cl.MessageID != "msg_claude_77" {
		t.Errorf("claude 行 MessageID = %q, want msg_claude_77", cl.MessageID)
	}
	if !strings.Contains(cl.RawData, `"request_model":"claude-opus-4-8"`) {
		t.Errorf("claude 行 raw_data 缺 request_model: %s", cl.RawData)
	}
	if !strings.Contains(cl.RawData, `"input_token_semantics":2`) {
		t.Errorf("claude 行 raw_data 缺 input_token_semantics: %s", cl.RawData)
	}
}

// TestCCSwitch_LegacyDBDegradesPerColumn：三列全缺的最老库——采集成功、
// Warn 一次列出全部缺失列名、request_model/semantics 用默认值、
// DataSource 按严格 codex_session: 前缀本地分类（近似前缀 codexXsession: 保持 proxy）。
func TestCCSwitch_LegacyDBDegradesPerColumn(t *testing.T) {
	const day = int64(1781092800)
	dbPath := createCCSwitchColumnVariantDB(t, false, false, false)
	insertCCSwitchRows(t, dbPath,
		ccSwitchFixtureRow{"codex_session:thread-v1:11111111-1111-1111-1111-111111111111:1",
			"11111111-1111-1111-1111-111111111111", "codex", "gpt-5.6-terra", nil, nil, nil, day},
		ccSwitchFixtureRow{"codexXsession:1", "c0debabe-c0de-c0de-c0de-c0dec0dec0de",
			"codex", "gpt-5.6-terra", nil, nil, nil, day + 1},
		ccSwitchFixtureRow{"44444444-4444-4444-4444-444444444444", "55555555-5555-5555-5555-555555555555",
			"codex", "glm-5.3", nil, nil, nil, day + 2},
	)

	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	handler := &testLogHandler{}
	result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.New(handler))
	if err != nil {
		t.Fatalf("旧版库采集不应失败: %v", err)
	}
	if len(result.Logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(result.Logs))
	}
	byReq := map[string]model.RouterLog{}
	for _, l := range result.Logs {
		byReq[l.RequestID] = l
	}
	if got := byReq["codex_session:thread-v1:11111111-1111-1111-1111-111111111111:1"].DataSource; got != "codex_session" {
		t.Errorf("缺列源 codex_session: 前缀行 DataSource = %q, want codex_session（本地严格前缀分类）", got)
	}
	if got := byReq["codexXsession:1"].DataSource; got != "proxy" {
		t.Errorf("缺列源近似前缀行 DataSource = %q, want proxy（严格前缀不得误分类）", got)
	}
	if got := byReq["44444444-4444-4444-4444-444444444444"].DataSource; got != "proxy" {
		t.Errorf("缺列源随机 UUID request_id 行 DataSource = %q, want proxy", got)
	}
	for _, l := range result.Logs {
		if !strings.Contains(l.RawData, `"request_model":""`) || !strings.Contains(l.RawData, `"input_token_semantics":0`) {
			t.Errorf("缺列源 raw_data 应含默认值键: %s", l.RawData)
		}
	}

	// Warn 一次，列出缺失列名。
	warns := 0
	for _, r := range handler.Records() {
		if strings.Contains(r.Message, "missing optional columns") {
			warns++
			var cols string
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "columns" {
					cols = attr.Value.String()
				}
				return true
			})
			for _, want := range []string{"data_source", "request_model", "input_token_semantics"} {
				if !strings.Contains(cols, want) {
					t.Errorf("Warn columns %q 缺 %q", cols, want)
				}
			}
		}
	}
	if warns != 1 {
		t.Errorf("缺列 Warn 次数 = %d, want 1", warns)
	}
}

// TestCCSwitch_PartialColumnsCollectedIndependently：中间版本库（request_model 在、
// data_source/input_token_semantics 缺）——可得列照常采集、缺失列独立降级；
// 再覆盖「仅缺 data_source 一列」的分支。按列粒度 = 无整体豁免。
func TestCCSwitch_PartialColumnsCollectedIndependently(t *testing.T) {
	const day = int64(1781092800)

	t.Run("request_model only", func(t *testing.T) {
		dbPath := createCCSwitchColumnVariantDB(t, false, true, false)
		insertCCSwitchRows(t, dbPath, ccSwitchFixtureRow{"session:codex:provider-2:resp_pm", "codex_99999999-9999-9999-9999-999999999999",
			"codex", "gpt-5.6-terra", nil, fixtureStr("gpt-5.6"), nil, day})
		a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
		handler := &testLogHandler{}
		result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.New(handler))
		if err != nil {
			t.Fatalf("CollectLogs failed: %v", err)
		}
		if len(result.Logs) != 1 {
			t.Fatalf("expected 1 log, got %d", len(result.Logs))
		}
		l := result.Logs[0]
		if !strings.Contains(l.RawData, `"request_model":"gpt-5.6"`) {
			t.Errorf("存在列应照常采集: %s", l.RawData)
		}
		if got := l.DataSource; got != "proxy" {
			t.Errorf("缺 data_source 列的 codex 无前缀行 DataSource = %q, want proxy", got)
		}
		var cols string
		for _, r := range handler.Records() {
			if strings.Contains(r.Message, "missing optional columns") {
				r.Attrs(func(attr slog.Attr) bool {
					if attr.Key == "columns" {
						cols = attr.Value.String()
					}
					return true
				})
			}
		}
		if !strings.Contains(cols, "data_source") || !strings.Contains(cols, "input_token_semantics") {
			t.Errorf("Warn columns = %q, 应含 data_source 与 input_token_semantics", cols)
		}
		if strings.Contains(cols, "request_model") {
			t.Errorf("Warn columns = %q 不应含存在的 request_model", cols)
		}
	})

	t.Run("missing data_source only", func(t *testing.T) {
		dbPath := createCCSwitchColumnVariantDB(t, false, true, true)
		insertCCSwitchRows(t, dbPath, ccSwitchFixtureRow{"session:codex:provider-3:resp_ds", "codex_88888888-8888-8888-8888-888888888888",
			"codex", "gpt-5.6-terra", nil, fixtureStr("gpt-5.6"), fixtureInt(1), day})
		a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
		result, err := a.CollectLogs(context.Background(), RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
		if err != nil {
			t.Fatalf("CollectLogs failed: %v", err)
		}
		l := result.Logs[0]
		if !strings.Contains(l.RawData, `"request_model":"gpt-5.6"`) || !strings.Contains(l.RawData, `"input_token_semantics":1`) {
			t.Errorf("存在列应照常采集: %s", l.RawData)
		}
		if l.DataSource != "proxy" {
			t.Errorf("DataSource = %q, want proxy（本地分类）", l.DataSource)
		}
	})
}

// TestCCSwitch_RediscoverDoesNotDowngradeMigrationMarker：端到端——token-usage 库
// 已由 v3 迁移正确标记 data_source='codex_session' 的行，经缺 data_source 列的
// 旧版源重采后 UpsertRawRouterLogs（INSERT OR REPLACE 整行重写）不得把标记降级
// 覆盖为 'proxy'（否则这些行混入 codex 归因匹配）。
func TestCCSwitch_RediscoverDoesNotDowngradeMigrationMarker(t *testing.T) {
	const day = int64(1781092800)
	const reqID = "codex_session:thread-v1:77777777-7777-7777-7777-777777777777:5"

	// token-usage 库：预置 v3 迁移产物的已标记行。
	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if _, err := db.UpsertRawRouterLogs(ctx, usageDB, []model.RouterLog{{
		RequestID: reqID, RouterName: "cc_switch", SessionID: "77777777-7777-7777-7777-777777777777",
		AppType: "codex", Model: "gpt-5.6-terra", DataSource: "codex_session", CreatedAt: day,
	}}); err != nil {
		t.Fatalf("预置已标记行失败: %v", err)
	}

	// 缺 data_source 列的旧版源重采同一行（本地分类产出）后落库。
	dbPath := createCCSwitchColumnVariantDB(t, false, false, false)
	insertCCSwitchRows(t, dbPath, ccSwitchFixtureRow{reqID, "77777777-7777-7777-7777-777777777777",
		"codex", "gpt-5.6-terra", nil, nil, nil, day})
	a := &CCSwitchAdapter{name: "cc_switch", dbPath: dbPath}
	result, err := a.CollectLogs(ctx, RouterCollectRequest{Dates: []string{"2026-06-10"}}, slog.Default())
	if err != nil {
		t.Fatalf("CollectLogs failed: %v", err)
	}
	if len(result.Logs) != 1 || result.Logs[0].DataSource != "codex_session" {
		t.Fatalf("采集侧本地分类产出错误: %+v", result.Logs)
	}
	if _, err := db.UpsertRawRouterLogs(ctx, usageDB, result.Logs); err != nil {
		t.Fatalf("重采落库失败: %v", err)
	}

	var ds string
	if err := usageDB.QueryRow(`SELECT data_source FROM raw_router_logs WHERE request_id=?`, reqID).Scan(&ds); err != nil {
		t.Fatal(err)
	}
	if ds != "codex_session" {
		t.Fatalf("REPLACE 后 data_source = %q, want codex_session（不得降级覆盖）", ds)
	}
}

// TestExtractMessageIDFromRequestID_CodexForm：codex 形态 message_id 提取表驱动。
// 形态：session:codex:{provider_id}:{message_id} → 末段 message_id；
// provider_id 含冒号取最后一段；codex_session: 前缀与随机 UUID 不提取；
// claude/claude-desktop 旧形态回归不变。
func TestExtractMessageIDFromRequestID_CodexForm(t *testing.T) {
	cases := []struct {
		appType, requestID, want string
	}{
		{"codex", "session:codex:provider-1:resp_abc123", "resp_abc123"},
		{"codex", "session:codex:provider-1:chatcmpl_xyz", "chatcmpl_xyz"},
		// provider id 含冒号：取最后一个冒号后的末段。
		{"codex", "session:codex:prov:with:colons:resp_tail", "resp_tail"},
		// codex_session 同步行不提取（非 session:codex: 形态）。
		{"codex", "codex_session:thread-v1:uuid:1", ""},
		// 随机 UUID（上游响应无 id）不提取。
		{"codex", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ""},
		// codex 行出现非 codex 段形态不提取（严格前缀）。
		{"codex", "session:notcodex:resp_x", ""},
		{"codex", "session:", ""},
		// claude / claude-desktop 旧形态回归。
		{"claude", "session:msg_01", "msg_01"},
		{"claude-desktop", "session:msg_02", "msg_02"},
		{"claude", "session:codex:provider-1:resp_abc", "codex:provider-1:resp_abc"},
		{"opencode", "opencode_session:ses-a:msg-open", ""},
		{"gemini", "session:codex:provider-1:resp_abc", ""},
	}
	for _, c := range cases {
		if got := extractMessageIDFromRequestID(c.appType, c.requestID); got != c.want {
			t.Errorf("extractMessageIDFromRequestID(%q, %q) = %q, want %q", c.appType, c.requestID, got, c.want)
		}
	}
}
