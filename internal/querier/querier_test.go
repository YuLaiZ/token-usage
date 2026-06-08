package querier

import (
	"context"
	"errors"
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
			Date: "2026-07-09", TS: 1000, Model: "claude-sonnet-4", Directory: "/work", Project: "proj-A",
			InputTokens: 1000, FreshInputTokens: 700, OutputTokens: 50,
			CacheReadTokens: 300, ReasoningTokens: 50, TotalTokens: 1100,
		},
		{
			ID: "msg-two", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
			Date: "2026-07-10", TS: 2000, Model: "gpt-5.5", Directory: "/work", Project: "proj-A",
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

	if !strings.Contains(result, "sess-alpha") {
		t.Errorf("result should contain sess-alpha\ngot:\n%s", result)
	}
	if strings.Contains(result, "sess-beta") {
		t.Errorf("result must NOT contain sess-beta (no messages)\ngot:\n%s", result)
	}
}

// 主模型只按查询日期范围内 total 最大值选择。
func TestSessions_MainModelByDateRange(t *testing.T) {
	q := setupMessageFixture(t)

	// 07-09 范围内只有 msg-one(claude-sonnet-4, total 1100)
	r1, err := q.Sessions(context.Background(), []string{"2026-07-09"})
	if err != nil {
		t.Fatalf("Sessions 07-09 failed: %v", err)
	}
	if !strings.Contains(r1, "claude-sonnet-4") {
		t.Errorf("07-09 main model should be claude-sonnet-4\ngot:\n%s", r1)
	}
	if strings.Contains(r1, "gpt-5.5") {
		t.Errorf("07-09 result must not contain gpt-5.5\ngot:\n%s", r1)
	}

	// 07-10 范围内只有 msg-two(gpt-5.5, total 1100)
	r2, err := q.Sessions(context.Background(), []string{"2026-07-10"})
	if err != nil {
		t.Fatalf("Sessions 07-10 failed: %v", err)
	}
	if !strings.Contains(r2, "gpt-5.5") {
		t.Errorf("07-10 main model should be gpt-5.5\ngot:\n%s", r2)
	}
	if strings.Contains(r2, "claude-sonnet-4") {
		t.Errorf("07-10 result must not contain claude-sonnet-4\ngot:\n%s", r2)
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
