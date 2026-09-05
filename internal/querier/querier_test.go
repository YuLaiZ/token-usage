package querier

import (
	"context"

	"errors"
	"github.com/mattn/go-runewidth"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// setupMessageFixture 构造消息账本 fixture：
//   - 同一个 session(sess-alpha) 放两条不同 model/date 的 Message
//   - 每条 input=1000,fresh=700,cache_read=300,reasoning=50,output=50,total=1100
//     （total 不等于任何明细自行相加，确保查询必须读源字段）
//   - 另一个 session(sess-beta) 只写元数据、无任何消息，用于验证 Sessions JOIN 不展示空会话
//
// 两条消息的 token 明细一致，聚合两行时：fresh=1400、reasoning=100、total=2200。
func setupMessageFixture(t *testing.T) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })

	ctx := context.Background()
	if _, err := db.UpsertSessionMeta(ctx, testDB, []model.Session{
		{ID: "sess-alpha", Client: model.ClientClaudeCode, Directory: "/work", Project: "proj-A", Title: "fix-login", FirstTS: 1000, LastTS: 2000},
		{ID: "sess-beta", Client: model.ClientClaudeCode, Directory: "/other", Project: "proj-B", Title: "no-messages", FirstTS: 1000, LastTS: 2000},
	}); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}

	msgs := []model.Message{
		{
			ID: "msg-one", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1000, Model: "claude-sonnet-4", Provider: "Anthropic", Directory: "/work", Project: "proj-A",
			InputTokens: 1000, FreshInputTokens: 700, OutputTokens: 50,
			CacheReadTokens: 300, ReasoningTokens: 50, TotalTokens: 1100,
		},
		{
			ID: "msg-two", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
			Date: "2026-07-10", TS: 2000, Model: "gpt-5.5", Provider: "OpenAI", Directory: "/work", Project: "proj-A",
			InputTokens: 1000, FreshInputTokens: 700, OutputTokens: 50,
			CacheReadTokens: 300, ReasoningTokens: 50, TotalTokens: 1100,
		},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	return New(testDB)
}

var bothDates = []string{"2026-07-09", "2026-07-10"}

// ByClient 聚合 fresh_input 与源 total，不按 client 猜口径。
func TestByClient_UsesFreshAndSourceTotal(t *testing.T) {
	q := setupMessageFixture(t)

	result, err := q.ByClient(context.Background(), bothDates)
	if err != nil {
		t.Fatalf("ByClient failed: %v", err)
	}

	if !strings.Contains(result, "Claude Code") {
		t.Error("result should contain 'Claude Code'")
	}
	// fresh_input 聚合 700+700=1400 → "1.40 K"
	if !strings.Contains(result, "1.40 K") {
		t.Errorf("result should aggregate fresh_input_tokens to 1.40 K\ngot:\n%s", result)
	}
	// 源 total 聚合 1100+1100=2200 → "2.20 K"
	if !strings.Contains(result, "2.20 K") {
		t.Errorf("result should aggregate source total_tokens to 2.20 K\ngot:\n%s", result)
	}
}

// 同 session 两 model 必须分两组，不能塌缩。
func TestByModel_DoesNotCollapse(t *testing.T) {
	q := setupMessageFixture(t)

	result, err := q.ByModel(context.Background(), bothDates)
	if err != nil {
		t.Fatalf("ByModel failed: %v", err)
	}

	if !strings.Contains(result, "claude-sonnet-4") {
		t.Errorf("result should contain model claude-sonnet-4\ngot:\n%s", result)
	}
	if !strings.Contains(result, "gpt-5.5") {
		t.Errorf("result should contain model gpt-5.5\ngot:\n%s", result)
	}
}

// ByProvider 将所有历史空 provider 保留为未归因，不按客户端推断供应商。
func TestByProvider_SeparatesProvidersAndUnattributed(t *testing.T) {
	q := setupMessageFixture(t)
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{{
		ID: "msg-unattributed", SessionID: "sess-alpha", Client: model.ClientZhipuAutoClaw,
		Date: "2026-07-10", TS: 3000, Model: "unknown", TotalTokens: 99,
	}}); err != nil {
		t.Fatal(err)
	}

	result, err := q.ByProvider(context.Background(), bothDates, nil)
	if err != nil {
		t.Fatalf("ByProvider failed: %v", err)
	}
	for _, want := range []string{"Anthropic", "OpenAI", "(unattributed)", "(未归因)", "供应商"} {
		if !strings.Contains(result, want) {
			t.Errorf("result should contain %q\ngot:\n%s", want, result)
		}
	}
}

// provider_aliases 只在查询期改展示并合并聚合，不得修改 messages 原始字段。
func TestByProvider_AliasesMergeAtQueryTimeWithoutMutatingMessages(t *testing.T) {
	q := setupMessageFixture(t)
	msgs := []model.Message{
		{ID: "msg-alias-source", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 3000, Provider: "source-a", TotalTokens: 100},
		{ID: "msg-alias-router", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 4000, Provider: "source-b", RouterProvider: "router-b", TotalTokens: 200},
		{ID: "msg-codex-empty", SessionID: "sess-alpha", Client: model.ClientCodexApp, Date: "2026-07-11", TS: 5000, TotalTokens: 300},
		{ID: "msg-claude-empty", SessionID: "sess-alpha", Client: model.ClientClaudeDesktop, Date: "2026-07-11", TS: 6000, TotalTokens: 400},
		{ID: "msg-workbuddy-empty", SessionID: "sess-alpha", Client: model.ClientWorkBuddy, Date: "2026-07-11", TS: 7000, TotalTokens: 500},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}

	out, err := q.ByProvider(context.Background(), []string{"2026-07-11"}, map[string]string{
		"source-a": "Merged provider",
		"router-b": "Merged provider",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Merged provider") != 1 || strings.Contains(out, "source-a") || strings.Contains(out, "router-b") {
		t.Errorf("alias should merge only in output:\n%s", out)
	}
	if strings.Count(out, "(unattributed)") != 1 || !strings.Contains(out, "(未归因)") {
		t.Errorf("empty historical providers should remain unattributed:\n%s", out)
	}
	var provider, routerProvider string
	if err := q.db.QueryRow(`SELECT provider, router_provider FROM messages WHERE id='msg-alias-router' AND client=?`, model.ClientClaudeCode).Scan(&provider, &routerProvider); err != nil {
		t.Fatal(err)
	}
	if provider != "source-b" || routerProvider != "router-b" {
		t.Fatalf("query aliases modified stored attribution: provider=%q router_provider=%q", provider, routerProvider)
	}
}

// ByProject 表头与 COUNT 均为请求数，不是会话数。
func TestByProject_CountsRequests(t *testing.T) {
	q := setupMessageFixture(t)

	result, err := q.ByProject(context.Background(), bothDates)
	if err != nil {
		t.Fatalf("ByProject failed: %v", err)
	}

	if strings.Contains(result, "会话数") {
		t.Errorf("result must not contain '会话数' header\ngot:\n%s", result)
	}
	if !strings.Contains(result, "请求数") {
		t.Errorf("result header should be '请求数'\ngot:\n%s", result)
	}
	if !strings.Contains(result, "proj-A") {
		t.Errorf("result should contain project proj-A\ngot:\n%s", result)
	}
}

// Sessions 只展示范围内有消息的 session（INNER JOIN，空会话不显示）。
func TestSessions_OnlyShowsSessionsWithMessages(t *testing.T) {
	q := setupMessageFixture(t)

	// 只查 07-09：sess-alpha 有 msg-one 命中，sess-beta 无任何消息不应出现
	result, err := q.Sessions(context.Background(), []string{"2026-07-09"})
	if err != nil {
		t.Fatalf("Sessions failed: %v", err)
	}

	if !strings.Contains(result, "fix-login") {
		t.Errorf("result should contain the alpha session title fix-login\ngot:\n%s", result)
	}
	if strings.Contains(result, "no-messages") {
		t.Errorf("result must NOT contain the empty session (no messages)\ngot:\n%s", result)
	}
}

// 总览显示请求总数、fresh input、reasoning(明细)、source total。
func TestSummary_MessageLevel(t *testing.T) {
	q := setupMessageFixture(t)

	result, err := q.Summary(context.Background(), bothDates)
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}

	if !strings.Contains(result, "请求总数: 2") {
		t.Errorf("result should show request total 2\ngot:\n%s", result)
	}
	if strings.Contains(result, "会话总数") {
		t.Errorf("result must not use '会话总数' label\ngot:\n%s", result)
	}
	// fresh_input 聚合 1400 → 1.40 K
	if !strings.Contains(result, "1.40 K") {
		t.Errorf("result should aggregate fresh_input to 1.40 K\ngot:\n%s", result)
	}
	// reasoning 明细 50+50=100
	if !strings.Contains(result, "100") {
		t.Errorf("result should show reasoning detail 100\ngot:\n%s", result)
	}
	// source total 聚合 2200 → 2.20 K
	if !strings.Contains(result, "2.20 K") {
		t.Errorf("result should aggregate source total to 2.20 K\ngot:\n%s", result)
	}
}

// reasoning 只展示明细，total 保持源值（不加 reasoning）。
func TestQueries_DoNotAddReasoning(t *testing.T) {
	q := setupMessageFixture(t)

	result, err := q.ByClient(context.Background(), bothDates)
	if err != nil {
		t.Fatalf("ByClient failed: %v", err)
	}

	// 源 total 聚合 = 2200 → "2.20 K"。
	// 若错误地把 reasoning 加进 total：fresh(1400)+reasoning(100)=1500 → "1.50 K"，
	// 或 input(2000)+reasoning(100)=2100 → "2.10 K"。这些都不得出现。
	if !strings.Contains(result, "2.20 K") {
		t.Errorf("total should remain source value 2.20 K\ngot:\n%s", result)
	}
	if strings.Contains(result, "1.50 K") {
		t.Errorf("total must not be fresh+reasoning (1.50 K)\ngot:\n%s", result)
	}
	if strings.Contains(result, "2.10 K") {
		t.Errorf("total must not be input+reasoning (2.10 K)\ngot:\n%s", result)
	}
	// reasoning 明细 100 必须作为独立列展示
	if !strings.Contains(result, "100") {
		t.Errorf("reasoning detail 100 should be shown\ngot:\n%s", result)
	}
}

// EXPLAIN QUERY PLAN 命中 messages 索引。
func TestSessions_ExplainQueryPlan(t *testing.T) {
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	q := New(testDB)

	// 与 Sessions 等价的 JOIN 形态做 EXPLAIN
	query := `EXPLAIN QUERY PLAN
SELECT s.id, COUNT(m.id)
FROM sessions s
JOIN messages m ON m.session_id=s.id AND m.client=s.client
               AND m.date IN ('2026-07-09')
GROUP BY s.id, s.client`

	rows, err := q.db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v", err)
	}
	defer rows.Close()

	var detail strings.Builder
	for rows.Next() {
		var id int
		var parent, child, text string
		if err := rows.Scan(&id, &parent, &child, &text); err != nil {
			t.Fatalf("scan EXPLAIN row failed: %v", err)
		}
		detail.WriteString(text)
		detail.WriteString(" | ")
	}
	joined := detail.String()
	if !strings.Contains(joined, "idx_messages_date") && !strings.Contains(joined, "idx_messages_session_client") {
		t.Errorf("EXPLAIN should use a messages index\ngot:\n%s", joined)
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		tokens   int64
		expected string
	}{
		{0, "0"},
		{500, "500"},
		{1500, "1.50 K"},
		{1500000, "1.50 M"},
		{999999999, "1000.00 M"},
		{1000000000, "1.00 B"},
		{4322780000, "4.32 B"},
	}

	for _, tt := range tests {
		result := formatTokens(tt.tokens)
		if result != tt.expected {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.tokens, result, tt.expected)
		}
	}
}

func TestQueries_CheckDependenciesAndCancellationBeforeEmptyDateShortcut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	q := &Querier{}
	calls := []struct {
		name string
		run  func() (string, error)
	}{
		{name: "ByClient", run: func() (string, error) { return q.ByClient(ctx, nil) }},
		{name: "ByModel", run: func() (string, error) { return q.ByModel(ctx, nil) }},
		{name: "ByProvider", run: func() (string, error) { return q.ByProvider(ctx, nil, nil) }},
		{name: "ByProject", run: func() (string, error) { return q.ByProject(ctx, nil) }},
		{name: "Sessions", run: func() (string, error) { return q.Sessions(ctx, nil) }},
		{name: "Summary", run: func() (string, error) { return q.Summary(ctx, nil) }},
	}
	for _, call := range calls {
		t.Run(call.name+"_missing_database", func(t *testing.T) {
			if _, err := call.run(); err == nil {
				t.Fatal("数据库缺失时不应由空日期快捷路径掩盖错误")
			}
		})
	}

	q = setupMessageFixture(t)
	for _, call := range calls {
		t.Run(call.name+"_canceled", func(t *testing.T) {
			if _, err := call.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// formatCacheHit 口径回归：cache_read / (fresh + read + create)，零分母 0.00%。
func TestFormatCacheHit(t *testing.T) {
	cases := []struct {
		fresh, read, create int64
		want                string
	}{
		{0, 0, 0, "0.00%"},
		{100, 0, 0, "0.00%"},
		{100, 300, 100, "60.00%"},
		{0, 9662, 353, "96.48%"}, // OpenCode 形态：分母含 cache create
		{3, 97, 0, "97.00%"},
	}
	for _, tc := range cases {
		if got := formatCacheHit(tc.fresh, tc.read, tc.create); got != tc.want {
			t.Errorf("formatCacheHit(%d,%d,%d) = %s, want %s", tc.fresh, tc.read, tc.create, got, tc.want)
		}
	}
}

// 默认布局锁定(升级不变合同):未设置输出列布局时,分组表与 sessions 的
// 表头英文行最后 7 列依次为 Requests / Input / Output / Cache Read /
// Reasoning / Total / Cache Hit,与历史版本逐字一致;cache_create 默认不显示。
func TestQueryViewsDefaultLayoutLocked(t *testing.T) {
	wantTail := []string{"Requests", "Input", "Output", "Cache Read", "Reasoning", "Total", "Cache Hit"}

	headerCells := func(t *testing.T, out string) []string {
		t.Helper()
		var header string
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "│") {
				header = ln // 首条含 │ 的行即表头英文行（顶边框用 ┌┬┐）
				break
			}
		}
		if header == "" {
			t.Fatalf("输出缺少表头行:\n%s", out)
		}
		cells := strings.Split(strings.Trim(header, "│"), "│")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return cells
	}

	q := setupMessageFixture(t)
	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"client", func() (string, error) { return q.ByClient(context.Background(), bothDates) }},
		{"model", func() (string, error) { return q.ByModel(context.Background(), bothDates) }},
		{"provider", func() (string, error) { return q.ByProvider(context.Background(), bothDates, nil) }},
		{"project", func() (string, error) { return q.ByProject(context.Background(), bothDates) }},
		{"session", func() (string, error) { return q.Sessions(context.Background(), bothDates) }},
	} {
		out, err := tc.run()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		cells := headerCells(t, out)
		if len(cells) < 7 {
			t.Fatalf("%s 列数 %d < 7:\n%s", tc.name, len(cells), out)
		}
		tail := cells[len(cells)-7:]
		for i := range wantTail {
			if tail[i] != wantTail[i] {
				t.Errorf("%s 尾部第 %d 列 = %q, want %q（完整表头: %v）", tc.name, i, tail[i], wantTail[i], cells)
			}
		}
	}
}

// sessions 长 Title 截断：超过 30 显示宽的标题截断加省略号且不穿透框线。
func TestSessionsLongTitleTruncated(t *testing.T) {
	q := setupMessageFixture(t)
	// 直接在 fixture 之上写一条长标题会话。
	longTitle := strings.Repeat("长标题", 30) // 60 显示宽
	if _, err := db.UpsertSessionMeta(context.Background(), q.db, []model.Session{{
		ID: "sess-long", Client: model.ClientClaudeCode, Directory: "/work", Project: "proj-A",
		Title: longTitle, FirstTS: 1000, LastTS: 2000,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{{
		ID: "msg-long", SessionID: "sess-long", Client: model.ClientClaudeCode,
		Date: "2026-07-09", TS: 1000, InputTokens: 10, TotalTokens: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := q.Sessions(context.Background(), []string{"2026-07-09"})
	if err != nil {
		t.Fatal(err)
	}
	// 锁定 30 显示宽合同：截断结果必须与 runewidth.Truncate(longTitle, 30, ...)
	// 逐字节一致（上限回退为 20 或标题缺失均不得通过）。
	wantTitle := runewidth.Truncate(longTitle, 30, "...")
	var border string
	for _, bl := range strings.Split(result, "\n") {
		if strings.Contains(bl, "┌") {
			border = bl
			break
		}
	}
	found := false
	for _, ln := range strings.Split(result, "\n") {
		if !strings.Contains(ln, "长标题") {
			continue
		}
		found = true
		if !strings.Contains(ln, wantTitle) {
			t.Errorf("标题应精确截断为 %q:\n%s", wantTitle, result)
		}
		if border != "" {
			if w, bw := runewidth.StringWidth(ln), runewidth.StringWidth(border); w != bw {
				t.Errorf("截断后行宽 %d 与边框 %d 不一致:\n%s", w, bw, result)
			}
		}
	}
	if !found {
		t.Fatalf("长标题行未出现于输出:\n%s", result)
	}
}

// ---- 通用维度聚合与总计行 ----

// 四个内置分组视图各追加唯一 Total / 总计 行;Sessions 与 Summary 不追加。
func TestGroupViews_AppendSingleTotalRow(t *testing.T) {
	q := setupMessageFixture(t)
	views := []struct {
		name string
		run  func() (string, error)
	}{
		{"ByClient", func() (string, error) { return q.ByClient(context.Background(), bothDates) }},
		{"ByModel", func() (string, error) { return q.ByModel(context.Background(), bothDates) }},
		{"ByProvider", func() (string, error) { return q.ByProvider(context.Background(), bothDates, nil) }},
		{"ByProject", func() (string, error) { return q.ByProject(context.Background(), bothDates) }},
	}
	for _, v := range views {
		out, err := v.run()
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if n := strings.Count(out, "Total / 总计"); n != 1 {
			t.Errorf("%s 应恰有一行 Total / 总计,实际 %d:\n%s", v.name, n, out)
		}
	}
	sessions, err := q.Sessions(context.Background(), bothDates)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sessions, "Total / 总计") {
		t.Errorf("Sessions 不应追加总计行:\n%s", sessions)
	}
	summary, err := q.Summary(context.Background(), bothDates)
	if err != nil {
		t.Fatal(err)
	}
	// Summary 仅保留字段标签形态的 Total / 总计(键值行),不追加表格总计行。
	if n := strings.Count(summary, "Total / 总计"); n != 1 {
		t.Errorf("Summary 应只含一个 Total 字段标签,实际 %d:\n%s", n, summary)
	}
}

// 总计行各字段与同日期 summary 对应字段一致;Cache Hit 按全量公式独立核对。
func TestGroupViews_TotalRowMatchesSummary(t *testing.T) {
	q := setupMessageFixture(t)
	out, err := q.ByClient(context.Background(), bothDates)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := q.Summary(context.Background(), bothDates)
	if err != nil {
		t.Fatal(err)
	}
	// fixture 两行聚合:requests=2, fresh=1400→1.40 K, output=100, cache_read=600,
	// reasoning=100, total=2200→2.20 K;CacheHit=600/(1400+600+0)=30.00%。
	for _, want := range []string{"2.20 K", "1.40 K"} {
		if !strings.Contains(out, want) {
			t.Errorf("ByClient 总计行应含 %q:\n%s", want, out)
		}
		if !strings.Contains(summary, want) {
			t.Errorf("Summary 应含对应字段 %q:\n%s", want, summary)
		}
	}
	// summary 没有 Cache Hit 列,总计行 Cache Hit 按全量公式独立断言:600/(1400+600+0)。
	if want := formatCacheHit(1400, 600, 0); want != "30.00%" || !strings.Contains(out, want) {
		t.Errorf("ByClient 总计行 Cache Hit 应为 30.00%%:\n%s", out)
	}
	// output=100、cache read=600、reasoning=100 在两侧均为原值显示。
	for _, want := range []string{"600", "100"} {
		if !strings.Contains(out, want) || !strings.Contains(summary, want) {
			t.Errorf("字段 %q 应同时出现在两份输出:\n%s\n%s", want, out, summary)
		}
	}
}

// mpc 三维分组的列顺序、总量与表头;零记录日期输出表头 + 零值总计。
func TestRunDimensionView_MultidimensionalAndZeroRecords(t *testing.T) {
	q := setupMessageFixture(t)
	out, err := q.RunDimensionView(context.Background(), bothDates, DimensionView{
		Dimensions: []string{"model", "provider", "client"},
		TitleEn:    "Custom view mpc", TitleZh: "自定义视图 mpc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Custom view mpc / 自定义视图 mpc") {
		t.Errorf("输出应含双语标题:\n%s", out)
	}
	if !strings.Contains(out, "claude-sonnet-4") || !strings.Contains(out, "Anthropic") {
		t.Errorf("三维表应含模型与供应商维度值:\n%s", out)
	}
	if n := strings.Count(out, "Total / 总计"); n != 1 {
		t.Errorf("应恰有一行总计,实际 %d:\n%s", n, out)
	}
	if !strings.Contains(out, "2.20 K") {
		t.Errorf("三维表总量应与全量一致(2.20 K):\n%s", out)
	}

	// 维度顺序改变只改变列顺序,不改变总量。
	out2, err := q.RunDimensionView(context.Background(), bothDates, DimensionView{
		Dimensions: []string{"client", "model", "provider"},
		TitleEn:    "Custom view cmp", TitleZh: "自定义视图 cmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "2.20 K") {
		t.Errorf("维度重排后总量不变:\n%s", out2)
	}

	// 零记录日期:表头 + 零值总计行(不是 no-data 文案)。
	zero, err := q.RunDimensionView(context.Background(), []string{"2099-01-01"}, DimensionView{
		Dimensions: []string{"model", "provider", "client"},
		TitleEn:    "Custom view mpc", TitleZh: "自定义视图 mpc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(zero, "no data") || strings.Contains(zero, "无数据") {
		t.Errorf("合法日期零记录不得显示 no-data:\n%s", zero)
	}
	if !strings.Contains(zero, "Total / 总计") || !strings.Contains(zero, "0.00%") {
		t.Errorf("零记录应为表头 + 零值总计:\n%s", zero)
	}
}

// len(dates)==0 防御分支保留既有 no-data 文案,不渲染总计行。
func TestRunDimensionView_NoDataBranchKept(t *testing.T) {
	q := setupMessageFixture(t)
	out, err := q.RunDimensionView(context.Background(), nil, DimensionView{
		Dimensions: []string{"model"},
		TitleEn:    "Group by model", TitleZh: "按模型分组",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Group by model - no data / 按模型分组 - 无数据" {
		t.Errorf("no-data 文案变化: %q", out)
	}
}

// provider 有效值优先 router_provider;alias 在组合键形成前合并且不改 messages。
func TestRunDimensionView_ProviderAliasMergesBeforeCompositeKey(t *testing.T) {
	q := setupMessageFixture(t)
	// 两条记录其余维度相同,provider 经 alias 合并;第三条空 provider 保持未归因。
	msgs := []model.Message{
		{ID: "md-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 3100, Model: "same-model", Provider: "source-a", Project: "p", TotalTokens: 100, FreshInputTokens: 10},
		{ID: "md-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 3200, Model: "same-model", Provider: "x", RouterProvider: "router-b", Project: "p", TotalTokens: 200, FreshInputTokens: 20},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-07-11"}, DimensionView{
		Dimensions: []string{"model", "provider", "client"},
		TitleEn:    "Custom view mpc", TitleZh: "自定义视图 mpc",
		Aliases: map[string]string{"source-a": "Merged provider", "router-b": "Merged provider"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "Merged provider") != 1 {
		t.Errorf("alias 应在组合键形成前合并为一行:\n%s", out)
	}
	if strings.Contains(out, "source-a") || strings.Contains(out, "router-b") {
		t.Errorf("合并后不得残留原始 provider 标签:\n%s", out)
	}
	var provider, routerProvider string
	if err := q.db.QueryRow(`SELECT provider, router_provider FROM messages WHERE id='md-b' AND client=?`, model.ClientClaudeCode).Scan(&provider, &routerProvider); err != nil {
		t.Fatal(err)
	}
	if provider != "x" || routerProvider != "router-b" {
		t.Errorf("查询不得修改 messages 归因: %q/%q", provider, routerProvider)
	}

	// 未归因:router 与 provider 均空保持独立显示。
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{
		{ID: "md-c", SessionID: "sess-alpha", Client: model.ClientCodexApp, Date: "2026-07-11", TS: 3300, Model: "same-model", TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	out2, err := q.RunDimensionView(context.Background(), []string{"2026-07-11"}, DimensionView{
		Dimensions: []string{"model", "provider", "client"},
		TitleEn:    "Custom view mpc", TitleZh: "自定义视图 mpc",
		Aliases: map[string]string{"source-a": "Merged provider", "router-b": "Merged provider"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "(unattributed)") || !strings.Contains(out2, "(未归因)") {
		t.Errorf("未归因保持独立显示:\n%s", out2)
	}
}

// project 未分类与 client/model 空值显示规则不变。
func TestRunDimensionView_EmptyDimensionValuesKeepDisplayRules(t *testing.T) {
	q := setupMessageFixture(t)
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{
		{ID: "md-empty-model", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 3400, Project: "", TotalTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-07-11"}, DimensionView{
		Dimensions: []string{"model", "project"},
		TitleEn:    "Custom view", TitleZh: "自定义视图",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(uncategorized)") || !strings.Contains(out, "(未分类)") {
		t.Errorf("空 project 应显示未分类:\n%s", out)
	}
	// 空 model 保持源字段空值显示(不补写任何占位标签)。
	if strings.Contains(out, "(unknown)") || strings.Contains(out, "(未知)") {
		t.Errorf("空 model 不应补写占位标签:\n%s", out)
	}
}

// 排序:total 降序,同 total 按完整显示键元组升序;同一配置下输出稳定。
func TestRunDimensionView_StableSort(t *testing.T) {
	q := setupMessageFixture(t)
	// 三条同 total 的消息,按 client 键升序断言行序。
	msgs := []model.Message{
		{ID: "st-c", SessionID: "sess-alpha", Client: model.ClientZhipuAutoClaw, Date: "2026-07-11", TS: 3500, Model: "m", TotalTokens: 10},
		{ID: "st-b", SessionID: "sess-alpha", Client: model.ClientCodexApp, Date: "2026-07-11", TS: 3600, Model: "m", TotalTokens: 10},
		{ID: "st-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-11", TS: 3700, Model: "m", TotalTokens: 10},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	view := DimensionView{Dimensions: []string{"client"}, TitleEn: "Group by client", TitleZh: "按客户端分组"}
	out1, err := q.RunDimensionView(context.Background(), []string{"2026-07-11"}, view)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := q.RunDimensionView(context.Background(), []string{"2026-07-11"}, view)
	if err != nil {
		t.Fatal(err)
	}
	if out1 != out2 {
		t.Errorf("同一配置下输出应稳定:\n%s\n%s", out1, out2)
	}
	// 同 total 行按显示键升序:三个 client 显示名按字节序排列。
	lines := strings.Split(out1, "\n")
	var keyOrder []string
	for _, ln := range lines {
		if strings.Contains(ln, "│") && !strings.Contains(ln, "Total / 总计") {
			cells := strings.Split(strings.Trim(ln, "│"), "│")
			if len(cells) > 1 {
				key := strings.TrimSpace(cells[0])
				if key != "" && !strings.Contains(key, "Client") && key != "客户端" {
					keyOrder = append(keyOrder, key)
				}
			}
		}
	}
	if len(keyOrder) < 3 {
		t.Fatalf("应有至少三个分组行:\n%s", out1)
	}
	for i := 1; i < len(keyOrder); i++ {
		if keyOrder[i-1] > keyOrder[i] {
			t.Errorf("同 total 行未按键升序: %v\n%s", keyOrder, out1)
			break
		}
	}
}

// SessionRows 返回原始空 project(空串,不映射「未分类」),且排序与 Sessions
// 渲染一致(首条消息日期、client、total 降序)。
func TestSessionRows_RawProjectAndSameOrderAsSessions(t *testing.T) {
	q := setupMessageFixture(t)
	// 追加一个空 project 的会话,total 高于 sess-alpha 在 07-09 的 1100,
	// 使其按 total 降序应排在前面。
	if _, err := db.UpsertSessionMeta(context.Background(), q.db, []model.Session{{
		ID: "sess-empty-proj", Client: model.ClientClaudeCode, Directory: "/work", Project: "", Title: "no-proj",
		FirstTS: 1000, LastTS: 2000,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{{
		ID: "msg-empty-proj", SessionID: "sess-empty-proj", Client: model.ClientClaudeCode,
		Date: "2026-07-09", TS: 1500, TotalTokens: 2000,
	}}); err != nil {
		t.Fatal(err)
	}

	rows, err := q.SessionRows(context.Background(), []string{"2026-07-09"})
	if err != nil {
		t.Fatalf("SessionRows failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应恰 2 行(空会话不出现),实际 %d: %+v", len(rows), rows)
	}
	// total 降序:no-proj(2000) 在前,fix-login(1100) 在后。
	if rows[0].Title != "no-proj" || rows[1].Title != "fix-login" {
		t.Errorf("排序应与 Sessions 一致(total 降序): %+v", rows)
	}
	// Project 保留源字段原值:空 project 就是空串,不做「未分类」映射。
	if rows[0].Project != "" {
		t.Errorf("空 project 应保持空串,实际 %q", rows[0].Project)
	}
	// 渲染侧才做映射,且两个标题在 Sessions 输出中的先后与 SessionRows 一致。
	out, err := q.Sessions(context.Background(), []string{"2026-07-09"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(uncategorized)") || !strings.Contains(out, "(未分类)") {
		t.Errorf("Sessions 渲染应保留未分类映射:\n%s", out)
	}
	if !(strings.Index(out, "no-proj") < strings.Index(out, "fix-login")) {
		t.Errorf("Sessions 渲染行序应与 SessionRows 一致:\n%s", out)
	}
}

// AggregateDimensionView 的行集合与排序和 RunDimensionView 渲染的数据行一致
// (同一夹具对比 client 与 day 两个视图,day 视图含缺口填充)。
func TestAggregateDimensionView_MatchesRunDimensionViewRows(t *testing.T) {
	q := setupMessageFixture(t)
	// 增加第二个 client 与一个无数据日期,覆盖排序(total 降序)与缺口填充。
	msgs := []model.Message{
		{ID: "agg-codex", SessionID: "sess-alpha", Client: model.ClientCodexApp,
			Date: "2026-07-09", TS: 3000, TotalTokens: 300},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	dates := []string{"2026-07-08", "2026-07-09", "2026-07-10"}

	// 从渲染表提取数据行首列(显示键):只取表头分隔线 ├ 与底部 └ 之间的表格行,
	// 排除表头(两行)与总计行。
	renderedKeys := func(t *testing.T, out string) []string {
		t.Helper()
		var keys []string
		inBody := false
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "├") {
				inBody = true
				continue
			}
			if strings.Contains(ln, "└") {
				inBody = false
			}
			if !inBody || !strings.Contains(ln, "│") || strings.Contains(ln, "Total / 总计") {
				continue
			}
			cells := strings.Split(strings.Trim(ln, "│"), "│")
			if len(cells) == 0 {
				continue
			}
			keys = append(keys, strings.TrimSpace(cells[0]))
		}
		return keys
	}

	for _, view := range []DimensionView{
		{Dimensions: []string{"client"}, TitleEn: "Group by client", TitleZh: "按客户端分组"},
		{Dimensions: []string{"day"}, TitleEn: "Usage by day", TitleZh: "按天用量"},
	} {
		rows, totals, err := q.AggregateDimensionView(context.Background(), dates, view)
		if err != nil {
			t.Fatalf("%v: %v", view.Dimensions, err)
		}
		out, err := q.RunDimensionView(context.Background(), dates, view)
		if err != nil {
			t.Fatalf("%v render: %v", view.Dimensions, err)
		}
		// 表头(两行)与总计行已由 renderedKeys 排除。
		var aggKeys []string
		for _, r := range rows {
			if len(r.Keys) != 1 {
				t.Fatalf("单维视图行键数应为 1: %+v", r.Keys)
			}
			aggKeys = append(aggKeys, r.Keys[0])
		}
		want := renderedKeys(t, out)
		if strings.Join(aggKeys, ",") != strings.Join(want, ",") {
			t.Errorf("%v 行集合/排序不一致:\nAggregateDimensionView=%v\nRunDimensionView=%v\n%s",
				view.Dimensions, aggKeys, want, out)
		}
		// 总计与各行聚合一致:total 求和恰等于 rangeTotals(缺口行为零值)。
		var sum int64
		for _, r := range rows {
			sum += r.Agg.TotalTokens
		}
		if sum != totals.TotalTokens {
			t.Errorf("%v 行 total 求和 %d 与总计 %d 不一致", view.Dimensions, sum, totals.TotalTokens)
		}
	}

	// day 视图缺口行为零值行;client 视图按 total 降序:Claude Code(2200) 在 Codex(300) 前。
	dayRows, _, err := q.AggregateDimensionView(context.Background(), dates, DimensionView{
		Dimensions: []string{"day"}, TitleEn: "Usage by day", TitleZh: "按天用量",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dayRows[0].Keys[0] != "2026-07-08" || dayRows[0].Agg.TotalTokens != 0 {
		t.Errorf("缺口日期应补零值行且居首(升序): %+v", dayRows[0])
	}
	if dayRows[0].Agg.Requests != 0 {
		t.Errorf("缺口行 requests 应为 0: %+v", dayRows[0].Agg)
	}
}

// 空 dates 的校验顺序锚定:「空 dates + 未知维度」报维度校验错误(校验先于
// 无数据早退,与旧实现一致);「空 dates + 合法维度」渲染「标题 - 无数据」文本。
func TestRunDimensionView_EmptyDatesValidationOrder(t *testing.T) {
	q := setupMessageFixture(t)

	_, err := q.RunDimensionView(context.Background(), nil, DimensionView{
		Dimensions: []string{"bogus"}, TitleEn: "x", TitleZh: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown query dimension") {
		t.Fatalf("空 dates + 未知维度应报维度校验错误: %v", err)
	}

	out, err := q.RunDimensionView(context.Background(), nil, DimensionView{
		Dimensions: []string{"client"}, TitleEn: "Group by client", TitleZh: "按客户端分组",
	})
	if err != nil {
		t.Fatalf("空 dates + 合法维度不应报错: %v", err)
	}
	if out != "Group by client - no data / 按客户端分组 - 无数据" {
		t.Errorf("空 dates + 合法维度应渲染无数据文本: %q", out)
	}
}

// AggregateDimensionView 空 dates 返回空行与零值总计(渲染与导出共用此语义)。
func TestAggregateDimensionView_EmptyDates(t *testing.T) {
	q := setupMessageFixture(t)
	rows, totals, err := q.AggregateDimensionView(context.Background(), nil, DimensionView{
		Dimensions: []string{"client"}, TitleEn: "Group by client", TitleZh: "按客户端分组",
	})
	if err != nil {
		t.Fatalf("空 dates 不应报错: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("空 dates 应返回空行,实际 %d 行: %+v", len(rows), rows)
	}
	if totals != (GroupAggregate{}) {
		t.Errorf("空 dates 应返回零值总计: %+v", totals)
	}
}

// 未知维度名被白名单拒绝,不得拼进 SQL。
func TestRunDimensionView_RejectsUnknownDimension(t *testing.T) {
	q := setupMessageFixture(t)
	_, err := q.RunDimensionView(context.Background(), bothDates, DimensionView{
		Dimensions: []string{"client", "hacker; DROP TABLE"},
		TitleEn:    "x", TitleZh: "x",
	})
	if err == nil {
		t.Fatal("未知维度必须被拒绝")
	}
}

// 空维度列表被拒绝(至少一个维度)。
func TestRunDimensionView_RejectsEmptyDimensions(t *testing.T) {
	q := setupMessageFixture(t)
	if _, err := q.RunDimensionView(context.Background(), bothDates, DimensionView{TitleEn: "x", TitleZh: "x"}); err == nil {
		t.Fatal("空维度列表必须被拒绝")
	}
}

// ---- day 维度:按天用量视图 ----

// 纯 day 视图:行按日期升序、缺口日期补零值行、趋势条按 totalTokens 比例分块、总计行数值正确。
func TestByDay_AscendingRowsGapFillAndTrendBars(t *testing.T) {
	q := setupMessageFixture(t)
	// 四个日期(含非连续):07-01 total=1000(最大), 07-03 total=500(半值), 07-05 total=5(小值)。
	// 请求连续 07-01..07-05,07-02 与 07-04 无数据,应补零值行(趋势为空)。
	msgs := []model.Message{
		{ID: "day-max", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 1000, TotalTokens: 1000},
		{ID: "day-half", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-03", TS: 2000, TotalTokens: 500},
		{ID: "day-small", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-05", TS: 3000, TotalTokens: 5},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	dates := []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04", "2026-07-05"}
	out, err := q.ByDay(context.Background(), dates)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage by day / 按天用量") {
		t.Errorf("输出应含双语标题:\n%s", out)
	}

	// 逐日期行断言:按日期升序,趋势条块数为 max=1000→20、半值=500→10、
	// 小值=5→1(正值但整除为 0 取 1)、缺口与零值→0。
	wantBlocks := map[string]int{
		"2026-07-01": 20,
		"2026-07-02": 0,
		"2026-07-03": 10,
		"2026-07-04": 0,
		"2026-07-05": 1,
	}
	var rowOrder []string
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) == 0 {
			continue
		}
		key := strings.TrimSpace(cells[0])
		blocks, isDayRow := wantBlocks[key]
		if !isDayRow {
			continue
		}
		rowOrder = append(rowOrder, key)
		if got := strings.Count(ln, "█"); got != blocks {
			t.Errorf("日期 %s 趋势条应 %d 块,实际 %d:\n%s", key, blocks, got, ln)
		}
	}
	wantOrder := []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04", "2026-07-05"}
	if strings.Join(rowOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("日期行应按时间升序且缺口日期补零值行: got %v\n%s", rowOrder, out)
	}

	// 总计行:三行 total 聚合恰为 1505(用 formatTokens 换算,聚合值错即不等),
	// 请求数 3,且总计行趋势单元格为空(总计行不含 █)。
	if n := strings.Count(out, "Total / 总计"); n != 1 {
		t.Errorf("应恰有一行总计,实际 %d:\n%s", n, out)
	}
	if !strings.Contains(out, formatTokens(1505)) {
		t.Errorf("总计行 total 应为聚合值 1505 的换算(%s):\n%s", formatTokens(1505), out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Total / 总计") && strings.Contains(ln, "█") {
			t.Errorf("总计行趋势单元格应为空串:\n%s", ln)
		}
	}
}

// day,model 多维视图:主序为日期升序、同日内 total 降序,无缺口填充,趋势列存在。
func TestRunDimensionView_DayModelOrdersByDateThenTotal(t *testing.T) {
	q := setupMessageFixture(t)
	msgs := []model.Message{
		{ID: "dm-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 1000, Model: "model-a", TotalTokens: 300},
		{ID: "dm-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 2000, Model: "model-b", TotalTokens: 100},
		{ID: "dm-c", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-03", TS: 3000, Model: "model-c", TotalTokens: 200},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-07-01", "2026-07-02", "2026-07-03"}, DimensionView{
		Dimensions: []string{"day", "model"},
		TitleEn:    "Usage by day and model", TitleZh: "按天与模型用量",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 趋势列以两行表头渲染(上行 Trend、下行 趋势),输出中二者仅出现在该列。
	if !strings.Contains(out, "Trend") || !strings.Contains(out, "趋势") {
		t.Errorf("含 day 维度的多维视图应含趋势列:\n%s", out)
	}
	// 数据行(首列为日期)序列:07-01 内 total 降序(a 300 在 b 100 前),随后 07-03;
	// 07-02 无数据,多维视图不补零值行。
	type dayRow struct{ day, model string }
	var rows []dayRow
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) < 2 {
			continue
		}
		day := strings.TrimSpace(cells[0])
		if !strings.Contains(day, "2026-07-") {
			continue
		}
		rows = append(rows, dayRow{day: day, model: strings.TrimSpace(cells[1])})
	}
	want := []dayRow{
		{"2026-07-01", "model-a"},
		{"2026-07-01", "model-b"},
		{"2026-07-03", "model-c"},
	}
	if len(rows) != len(want) {
		t.Fatalf("数据行数 = %d, want %d(多维不补缺口):\n%s", len(rows), len(want), out)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("第 %d 行 = %+v, want %+v(日期升序优先,同日 total 降序):\n%s", i, rows[i], want[i], out)
		}
	}
}

// 未知维度错误文案由有序名单动态拼接:含 day 与 month 两个时间维度,
// 不再是不含 month 的旧六维文本。
func TestRunDimensionView_UnknownDimensionMessageListsDayAndMonth(t *testing.T) {
	q := setupMessageFixture(t)
	_, err := q.RunDimensionView(context.Background(), bothDates, DimensionView{
		Dimensions: []string{"client", "bogus"},
		TitleEn:    "x", TitleZh: "x",
	})
	if err == nil {
		t.Fatal("未知维度必须被拒绝")
	}
	msg := err.Error()
	for _, want := range []string{
		"(allowed: client, model, provider, project, day, month, hour, weekday)",
		"(允许: client, model, provider, project, day, month, hour, weekday)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含含 weekday 的允许集合 %q:\n%s", want, msg)
		}
	}
	// 旧的不含 weekday 的八维文案不得再出现(以此保证本断言的区分度)。
	for _, legacy := range []string{
		"(allowed: client, model, provider, project, day, month, hour)",
		"(允许: client, model, provider, project, day, month, hour)",
	} {
		if strings.Contains(msg, legacy) {
			t.Errorf("错误不得再使用不含 month 的旧文案 %q:\n%s", legacy, msg)
		}
	}
}

// ---- month 维度:按月用量视图 ----

// 纯 month 视图:行按月升序、跨月缺口补零值行、趋势条按 totalTokens 比例分块、总计行数值正确。
// 夹具固定消息在 2026-07,本组数据选 2026-08 与 2026-10,请求 0801-1030
// (92 天,不与夹具日期重叠):2026-09 无数据应补零值行,共 3 行。
func TestByMonth_AscendingRowsCrossMonthGapFillAndTrendBars(t *testing.T) {
	q := setupMessageFixture(t)
	msgs := []model.Message{
		{ID: "mo-max", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-15", TS: 1000, TotalTokens: 1000},
		{ID: "mo-half", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-10-05", TS: 2000, TotalTokens: 500},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	var dates []string
	// 2026-08-01..2026-10-30 连续逐日列表(与 parseDateArgs 展开形态一致)。
	for d := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC); !d.After(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	out, err := q.ByMonth(context.Background(), dates)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage by month / 按月用量") {
		t.Errorf("输出应含双语标题:\n%s", out)
	}

	// 逐月行断言:按月升序,趋势条块数为 max=1000→20、半值=500→10、缺口→0。
	wantBlocks := map[string]int{
		"2026-08": 20,
		"2026-09": 0,
		"2026-10": 10,
	}
	var rowOrder []string
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) == 0 {
			continue
		}
		key := strings.TrimSpace(cells[0])
		blocks, isMonthRow := wantBlocks[key]
		if !isMonthRow {
			continue
		}
		rowOrder = append(rowOrder, key)
		if got := strings.Count(ln, "█"); got != blocks {
			t.Errorf("月份 %s 趋势条应 %d 块,实际 %d:\n%s", key, blocks, got, ln)
		}
	}
	wantOrder := []string{"2026-08", "2026-09", "2026-10"}
	if strings.Join(rowOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("月份行应按时间升序且缺口月份补零值行: got %v\n%s", rowOrder, out)
	}

	// 总计行:两月 total 聚合恰为 1500,请求数 2,且总计行趋势单元格为空。
	if n := strings.Count(out, "Total / 总计"); n != 1 {
		t.Errorf("应恰有一行总计,实际 %d:\n%s", n, out)
	}
	if !strings.Contains(out, formatTokens(1500)) {
		t.Errorf("总计行 total 应为聚合值 1500 的换算(%s):\n%s", formatTokens(1500), out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Total / 总计") && strings.Contains(ln, "█") {
			t.Errorf("总计行趋势单元格应为空串:\n%s", ln)
		}
	}
}

// month,model 多维视图:主序为月升序、同月内 total 降序,无缺口填充,趋势列存在。
func TestRunDimensionView_MonthModelOrdersByMonthThenTotal(t *testing.T) {
	q := setupMessageFixture(t)
	msgs := []model.Message{
		{ID: "mm-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-15", TS: 1000, Model: "model-a", TotalTokens: 300},
		{ID: "mm-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-15", TS: 2000, Model: "model-b", TotalTokens: 100},
		{ID: "mm-c", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-10-05", TS: 3000, Model: "model-c", TotalTokens: 200},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-08-15", "2026-09-15", "2026-10-05"}, DimensionView{
		Dimensions: []string{"month", "model"},
		TitleEn:    "Usage by month and model", TitleZh: "按月与模型用量",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 趋势列以两行表头渲染(上行 Trend、下行 趋势),输出中二者仅出现在该列。
	if !strings.Contains(out, "Trend") || !strings.Contains(out, "趋势") {
		t.Errorf("含 month 维度的多维视图应含趋势列:\n%s", out)
	}
	// 数据行(首列为月份)序列:2026-08 内 total 降序(a 300 在 b 100 前),随后 2026-10;
	// 2026-09 无数据,多维视图不补零值行。
	type monthRow struct{ month, model string }
	var rows []monthRow
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) < 2 {
			continue
		}
		month := strings.TrimSpace(cells[0])
		if len(month) != 7 || !strings.HasPrefix(month, "2026-") {
			continue
		}
		rows = append(rows, monthRow{month: month, model: strings.TrimSpace(cells[1])})
	}
	want := []monthRow{
		{"2026-08", "model-a"},
		{"2026-08", "model-b"},
		{"2026-10", "model-c"},
	}
	if len(rows) != len(want) {
		t.Fatalf("数据行数 = %d, want %d(多维不补缺口):\n%s", len(rows), len(want), out)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("第 %d 行 = %+v, want %+v(月升序优先,同月 total 降序):\n%s", i, rows[i], want[i], out)
		}
	}
}

// month,day 双时间维度组合视图:排序主轴取声明首维(month 升序),同月内按
// total 降序,第二时间维 day 不参与主序;temporalIdx 只作布尔消费(趋势列存在),
// 双时间维度不做缺口填充。
func TestRunDimensionView_MonthDayDualTemporalOrdersByDeclaredFirst(t *testing.T) {
	q := setupMessageFixture(t)
	// 夹具固定消息在 2026-07,本组数据放 2026-08(同月三条不同 total,日期序
	// 与 total 序相反以区分排序依据)与 2026-10,请求列表覆盖全部数据日期。
	msgs := []model.Message{
		{ID: "md-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-15", TS: 1000, Model: "model-a", TotalTokens: 300},
		{ID: "md-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-20", TS: 2000, Model: "model-b", TotalTokens: 100},
		{ID: "md-c", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-08-25", TS: 2500, Model: "model-c", TotalTokens: 200},
		{ID: "md-d", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-10-05", TS: 3000, Model: "model-d", TotalTokens: 500},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(),
		[]string{"2026-08-15", "2026-08-20", "2026-08-25", "2026-10-05"},
		DimensionView{
			Dimensions: []string{"month", "day"},
			TitleEn:    "Usage by month and day", TitleZh: "按月与日用量",
		})
	if err != nil {
		t.Fatal(err)
	}
	// 趋势列存在即证明时间维度只作布尔消费(任一时间维度命中即插趋势列)。
	if !strings.Contains(out, "Trend") || !strings.Contains(out, "趋势") {
		t.Fatalf("双时间维度视图应含趋势列:\n%s", out)
	}
	type row struct{ month, day string }
	var rows []row
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) < 2 {
			continue
		}
		month := strings.TrimSpace(cells[0])
		// 数据行首列是 7 字符月份;表头(Month/月份)与总计行不匹配该形态。
		if len(month) != 7 || !strings.HasPrefix(month, "2026-") {
			continue
		}
		rows = append(rows, row{month: month, day: strings.TrimSpace(cells[1])})
	}
	// 行序锚定:主轴 month 升序(2026-08 全部行在 2026-10 前),同月内 total
	// 降序(300→200→100,日期序 15→20→25 被打乱即为排序依据的区分度)。
	want := []row{
		{"2026-08", "2026-08-15"},
		{"2026-08", "2026-08-25"},
		{"2026-08", "2026-08-20"},
		{"2026-10", "2026-10-05"},
	}
	if len(rows) != len(want) {
		t.Fatalf("数据行数 = %d, want %d(双时间维度不补缺口):\n%s", len(rows), len(want), out)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("第 %d 行 = %+v, want %+v(主轴 month 升序,同月 total 降序):\n%s", i, rows[i], want[i], out)
		}
	}
}

// hourTS 按本机时区构造毫秒时间戳:hour 维度的 SQL 侧用 strftime 'localtime'
// 归属小时,与 Go time.Local 同为系统本地时区,期望值在任意时区机器上一致。
func hourTS(y int, mo time.Month, d, h, mi int) int64 {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local).UnixMilli()
}

// newEmptyQuerier 构造不带预置消息的空内存库:hour 视图的聚合按 ts 折算,
// setupMessageFixture 预置消息的 ts(1970 年小毫秒值)会随本机时区落入不同
// 小时行,污染小时归属断言,故 hour 系测试用空库自插消息。
func newEmptyQuerier(t *testing.T) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return New(testDB)
}

// 纯 hour 视图:ts 按本机时区折算小时归属,跨日同小时合并;固定补全
// 00:00..23:00 全部 24 行(与请求日期范围无关),按小时升序,趋势条以最繁忙
// 小时为基准,总计为独立全量聚合。
func TestByHour_Fixed24TicksAscendingGapFillAndTrendBars(t *testing.T) {
	q := newEmptyQuerier(t)
	msgs := []model.Message{
		// 07-09 与 07-10 的本地 14 时两条消息合并进 "14:00" 行,total 1500(最大)。
		{ID: "hour-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: hourTS(2026, 7, 9, 14, 30), TotalTokens: 1000},
		{ID: "hour-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-10", TS: hourTS(2026, 7, 10, 14, 45), TotalTokens: 500},
		// 本地 20 时一条小值,total 100 → 趋势条 100*20/1500 向下取整为 1 块。
		{ID: "hour-c", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-10", TS: hourTS(2026, 7, 10, 20, 0), TotalTokens: 100},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.ByHour(context.Background(), []string{"2026-07-09", "2026-07-10"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage by hour / 按小时用量") {
		t.Errorf("输出应含双语标题:\n%s", out)
	}

	// 逐小时行断言:00:00..23:00 恒为 24 行(22 个缺口小时补零值行,趋势为空)。
	wantBlocks := map[string]int{"14:00": 20, "20:00": 1}
	var rowOrder []string
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) == 0 {
			continue
		}
		key := strings.TrimSpace(cells[0])
		if len(key) != 5 || !strings.HasSuffix(key, ":00") {
			continue
		}
		rowOrder = append(rowOrder, key)
		want := wantBlocks[key]
		if got := strings.Count(ln, "█"); got != want {
			t.Errorf("小时 %s 趋势条应 %d 块,实际 %d:\n%s", key, want, got, ln)
		}
	}
	// 期望序列按显示形态构造(hourTicks 为原始键形态,显示键补 ":00" 后缀)。
	wantOrder := make([]string, len(hourTicks))
	for i, tick := range hourTicks {
		wantOrder[i] = tick + ":00"
	}
	if len(rowOrder) != len(wantOrder) {
		t.Fatalf("小时行数 = %d, want %d(固定 24 刻度):\n%s", len(rowOrder), len(wantOrder), out)
	}
	if strings.Join(rowOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("小时行应按 00:00..23:00 升序: got %v", rowOrder)
	}

	// 总计行:三行 total 聚合 1600,且总计行趋势单元格为空。
	if !strings.Contains(out, formatTokens(1600)) {
		t.Errorf("总计行 total 应为聚合值 1600 的换算(%s):\n%s", formatTokens(1600), out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Total / 总计") && strings.Contains(ln, "█") {
			t.Errorf("总计行趋势单元格应为空串:\n%s", ln)
		}
	}
}

// hour,model 多维视图:排序主轴取声明首维 hour 升序(显示键字典序即时间序),
// 同小时内按 total 降序;多维时间视图不做缺口填充,不出现补零行。
func TestRunDimensionView_HourModelOrdersByHourThenTotal(t *testing.T) {
	q := newEmptyQuerier(t)
	msgs := []model.Message{
		{ID: "hm-a", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: hourTS(2026, 7, 9, 9, 10), Model: "model-a", TotalTokens: 300},
		{ID: "hm-b", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: hourTS(2026, 7, 9, 9, 20), Model: "model-b", TotalTokens: 100},
		{ID: "hm-c", SessionID: "sess-alpha", Client: model.ClientClaudeCode, Date: "2026-07-10", TS: hourTS(2026, 7, 10, 22, 0), Model: "model-c", TotalTokens: 200},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-07-09", "2026-07-10"}, DimensionView{
		Dimensions: []string{"hour", "model"},
		TitleEn:    "Usage by hour and model", TitleZh: "按小时与模型用量",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Trend") || !strings.Contains(out, "趋势") {
		t.Errorf("含 hour 维度的多维视图应含趋势列:\n%s", out)
	}
	type hourRow struct{ hour, modelName string }
	var rows []hourRow
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) < 2 {
			continue
		}
		hour := strings.TrimSpace(cells[0])
		if len(hour) != 5 || !strings.HasSuffix(hour, ":00") {
			continue
		}
		rows = append(rows, hourRow{hour: hour, modelName: strings.TrimSpace(cells[1])})
	}
	want := []hourRow{
		{"09:00", "model-a"},
		{"09:00", "model-b"},
		{"22:00", "model-c"},
	}
	if len(rows) != len(want) {
		t.Fatalf("数据行数 = %d, want %d(多维不补缺口):\n%s", len(rows), len(want), out)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("第 %d 行 = %+v, want %+v(主轴 hour 升序,同小时 total 降序):\n%s", i, rows[i], want[i], out)
		}
	}
}

// 纯 weekday 视图:ts 按本机时区折算星期归属,固定补全 ISO 周序 Monday..Sunday
// 全部 7 行(与请求日期范围无关),按周序升序(而非显示名字典序),趋势条以最
// 繁忙星期为基准,总计为独立全量聚合。
func TestByWeekday_Fixed7TicksISOOrderGapFillAndTrendBars(t *testing.T) {
	q := newEmptyQuerier(t)
	// 2026-07-06 周一 / 07-08 周三 / 07-12 周日;周二、周四、周五、周六为缺口。
	msgs := []model.Message{
		{ID: "wd-mon", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-06", TS: hourTS(2026, 7, 6, 10, 0), TotalTokens: 500},
		{ID: "wd-wed", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-08", TS: hourTS(2026, 7, 8, 14, 30), TotalTokens: 1000},
		{ID: "wd-sun", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-12", TS: hourTS(2026, 7, 12, 20, 0), TotalTokens: 100},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.ByWeekday(context.Background(), []string{"2026-07-06", "2026-07-07", "2026-07-08", "2026-07-09", "2026-07-10", "2026-07-11", "2026-07-12"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage by weekday / 按星期用量") {
		t.Errorf("输出应含双语标题:\n%s", out)
	}

	// 逐星期行断言:ISO 周序 7 行,缺口星期补零值行(趋势为空);
	// 趋势条 Wednesday 1000→20 块、Monday 500→10 块、Sunday 100→2 块。
	wantBlocks := map[string]int{
		"Monday / 周一":    10,
		"Wednesday / 周三": 20,
		"Sunday / 周日":    2,
	}
	var rowOrder []string
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) == 0 {
			continue
		}
		key := strings.TrimSpace(cells[0])
		day, ok := weekdayRowKey(key)
		if !ok {
			continue
		}
		rowOrder = append(rowOrder, day)
		if got := strings.Count(ln, "█"); got != wantBlocks[day] {
			t.Errorf("星期 %s 趋势条应 %d 块,实际 %d:\n%s", day, wantBlocks[day], got, ln)
		}
	}
	if len(rowOrder) != len(weekdayTicks) {
		t.Fatalf("星期行数 = %d, want %d(固定 7 刻度):\n%s", len(rowOrder), len(weekdayTicks), out)
	}
	// 行序按 ISO 周序:显示名 Friday 字典序在 Monday 之前,若按显示名排序该
	// 断言即失败,以此锁定排序轴为原始键周序。
	for i, tick := range weekdayTicks {
		if rowOrder[i] != weekdayDisplayKey(tick) {
			t.Errorf("第 %d 行 = %q, want %q(ISO 周序): %v", i, rowOrder[i], weekdayDisplayKey(tick), rowOrder)
		}
	}

	// 总计行:三行 total 聚合 1600,且总计行趋势单元格为空。
	if !strings.Contains(out, formatTokens(1600)) {
		t.Errorf("总计行 total 应为聚合值 1600 的换算(%s):\n%s", formatTokens(1600), out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Total / 总计") && strings.Contains(ln, "█") {
			t.Errorf("总计行趋势单元格应为空串:\n%s", ln)
		}
	}
}

// weekdayRowKey 判定表格行首列是否为星期显示键,是则返回该键。
func weekdayRowKey(key string) (string, bool) {
	for _, tick := range weekdayTicks {
		if name := weekdayDisplayKey(tick); name == key {
			return name, true
		}
	}
	return "", false
}

// weekday,model 多维视图:排序主轴取声明首维 weekday 的原始键 ISO 周序
// (Monday 在 Friday 之前,尽管显示名字典序相反),同星期内按 total 降序;
// 多维时间视图不做缺口填充。
func TestRunDimensionView_WeekdayModelOrdersByISOWeekdayThenTotal(t *testing.T) {
	q := newEmptyQuerier(t)
	msgs := []model.Message{
		{ID: "wm-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-06", TS: hourTS(2026, 7, 6, 9, 0), Model: "model-a", TotalTokens: 300},
		{ID: "wm-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-06", TS: hourTS(2026, 7, 6, 18, 0), Model: "model-b", TotalTokens: 100},
		{ID: "wm-c", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-10", TS: hourTS(2026, 7, 10, 12, 0), Model: "model-c", TotalTokens: 200},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.RunDimensionView(context.Background(), []string{"2026-07-06", "2026-07-10"}, DimensionView{
		Dimensions: []string{"weekday", "model"},
		TitleEn:    "Usage by weekday and model", TitleZh: "按星期与模型用量",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Trend") || !strings.Contains(out, "趋势") {
		t.Errorf("含 weekday 维度的多维视图应含趋势列:\n%s", out)
	}
	type weekdayRow struct{ weekday, modelName string }
	var rows []weekdayRow
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		if len(cells) < 2 {
			continue
		}
		day, ok := weekdayRowKey(strings.TrimSpace(cells[0]))
		if !ok {
			continue
		}
		rows = append(rows, weekdayRow{weekday: day, modelName: strings.TrimSpace(cells[1])})
	}
	want := []weekdayRow{
		{"Monday / 周一", "model-a"},
		{"Monday / 周一", "model-b"},
		{"Friday / 周五", "model-c"},
	}
	if len(rows) != len(want) {
		t.Fatalf("数据行数 = %d, want %d(多维不补缺口):\n%s", len(rows), len(want), out)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("第 %d 行 = %+v, want %+v(主轴 ISO 周序,同星期 total 降序):\n%s", i, rows[i], want[i], out)
		}
	}
}
