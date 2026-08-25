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
