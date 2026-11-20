package engine

// claude_string_content_test.go 字符串 content 行参与 entrypoint 判定后的存量
// 收敛回归测试。
//
// 字符串 content 的 user 行携带 entrypoint=claude-desktop-3p。该值现在参与
// 「任一行 Desktop 即 Desktop」的整份文件累积判定，整份 transcript 归
// Claude Desktop；其 cwd 等元数据仍不可见（不进 entries）。按旧判定记为
// Claude Code 的历史行，在判定修正后的全量采集里通过「写入即收敛」把归因迁移
// 到新行后删除旧行，不跨 client 双计。预置判定修正前产出的 Claude Code 旧库行，
// 全量采集后断言收敛为 Claude Desktop 的唯一 2 行 + 归因迁移 + session 唯一。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// copyStringContentFixture 把 testdata/claude/string-content.jsonl 复制到
// t.TempDir 下的 projectsDir，返回该目录。
func copyStringContentFixture(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "claude", "string-content.jsonl"))
	if err != nil {
		t.Fatalf("解析源 fixture 路径失败: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "string-content.jsonl"), data, 0o600); err != nil {
		t.Fatalf("写入 fixture 失败: %v", err)
	}
	return dir
}

// stringContentSeed 返回判定修正前解析器对 fixture 的产出（旧语义 client=Claude Code，
// 目录/项目为空），其中 msg-string-a 带 router 归因以验证收敛时的迁移。
func stringContentSeed(t *testing.T) (tsA, tsB int64, date string, msgs []model.Message, session model.Session) {
	t.Helper()
	parse := func(ts string) int64 {
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatalf("解析时间戳 %q: %v", ts, err)
		}
		return parsed.UnixMilli()
	}
	tsA = parse("2026-06-02T09:35:20.100Z")
	tsB = parse("2026-06-02T09:36:15.900Z")
	date = time.UnixMilli(tsA).Format("2006-01-02")
	msgs = []model.Message{
		{
			ID: "msg-string-a", SessionID: "string-content", Client: model.ClientClaudeCode,
			Date: date, TS: tsA, Model: "model-a",
			RouterProvider: "router-p", RouterModel: "router-m", RouterName: "cc-switch",
			InputTokens: 120, FreshInputTokens: 120, OutputTokens: 40,
			CacheReadTokens: 30, CacheCreateTokens: 10, TotalTokens: 200,
		},
		{
			ID: "msg-string-b", SessionID: "string-content", Client: model.ClientClaudeCode,
			Date: date, TS: tsB, Model: "model-a",
			InputTokens: 200, FreshInputTokens: 200, OutputTokens: 80,
			CacheReadTokens: 60, CacheCreateTokens: 20, TotalTokens: 360,
		},
	}
	session = model.Session{
		ID: "string-content", Client: model.ClientClaudeCode, FirstTS: tsA, LastTS: tsB,
	}
	return
}

// TestRunCollect_StringContentDesktopEntrypointConverges：
// 预置旧 Claude Code 库行（含 router 归因）后全量采集，字符串行的 Desktop
// entrypoint 使新行归 Claude Desktop；写入即收敛应删除旧 Code 行、迁移归因，
// 最终 messages 仅 2 行（Desktop）、sessions 仅 1 行（Desktop），token 不翻倍。
func TestRunCollect_StringContentDesktopEntrypointConverges(t *testing.T) {
	isolateClaudeDiscoveryHome(t)
	projectsDir := copyStringContentFixture(t)

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	_, _, _, seedMsgs, seedSession := stringContentSeed(t)
	ctx := context.Background()
	if _, err := db.UpsertMessages(ctx, usageDB, seedMsgs); err != nil {
		t.Fatalf("预置 messages 失败: %v", err)
	}
	if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{seedSession}); err != nil {
		t.Fatalf("预置 sessions 失败: %v", err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	deps := NewDeps(cfg)
	// Dates 为空（全量）才触发收敛。
	res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{}, false, false)
	if !res.Complete() {
		t.Fatalf("RunCollect 失败: %+v", res)
	}

	// messages：只剩 Claude Desktop 的 2 行，无跨 client 双计。
	rows, err := usageDB.Query(`SELECT id, client, router_provider, router_model FROM messages ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 messages 失败: %v", err)
	}
	var got []model.Message
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.Client, &m.RouterProvider, &m.RouterModel); err != nil {
			rows.Close()
			t.Fatalf("扫描 messages 失败: %v", err)
		}
		got = append(got, m)
	}
	rows.Close()
	if len(got) != 2 {
		t.Fatalf("收敛后 messages 行数 %d, want 2: %+v", len(got), got)
	}
	for _, m := range got {
		if m.Client != model.ClientClaudeDesktop {
			t.Errorf("message %s client = %q, want %q（旧 Code 行应被收敛删除）", m.ID, m.Client, model.ClientClaudeDesktop)
		}
	}
	var rp, rm string
	if err := usageDB.QueryRow(`SELECT router_provider, router_model FROM messages WHERE id='msg-string-a' AND client=?`,
		model.ClientClaudeDesktop).Scan(&rp, &rm); err != nil {
		t.Fatalf("查询迁移归因失败: %v", err)
	}
	if rp != "router-p" || rm != "router-m" {
		t.Errorf("归因迁移失败: provider=%q model=%q, want router-p/router-m", rp, rm)
	}

	var sessionCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("sessions 行数 %d, want 1（旧 Code 会话应被收敛删除）", sessionCount)
	}
	var sClient string
	if err := usageDB.QueryRow(`SELECT client FROM sessions`).Scan(&sClient); err != nil {
		t.Fatalf("查询 session 失败: %v", err)
	}
	if sClient != model.ClientClaudeDesktop {
		t.Errorf("session client = %q, want %q", sClient, model.ClientClaudeDesktop)
	}

	var sum int64
	if err := usageDB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM messages`).Scan(&sum); err != nil {
		t.Fatalf("聚合 token 失败: %v", err)
	}
	if want := int64(200 + 360); sum != want {
		t.Errorf("SUM(total_tokens) = %d, want %d", sum, want)
	}
}
