package querier

import (
	"context"

	"errors"
	"github.com/mattn/go-runewidth"
	"strings"
	"testing"

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

// 尾部 7 列统一约定收口：分组表与 sessions 的表头英文行最后 7 列必须
// 依次为 Requests / Input / Output / Cache Read / Reasoning / Total / Cache Hit。
func TestQueryViewsTailColumnsLocked(t *testing.T) {
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
