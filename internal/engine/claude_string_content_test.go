package engine

// claude_string_content_test.go Claude 字符串 content 行兼容识别的旧库回归测试。
//
// 字符串 content 的 user 行在识别前整体 Unmarshal 失败、从未参与元数据推断或
// 消息产出。兼容识别后这些行必须保持同等不可见：一旦其携带的 entrypoint/cwd
// 参与推断，client/directory/project 归类会翻转，在 messages (client,id) 主键
// 与 sessions (id,client) 主键下对既有库形成重复行、聚合 token 翻倍。本测试
// 预置「识别前解析器」产出的既有库行，升级后再次采集断言全字段、行数与聚合
// token 均不变。

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

// stringContentSeed 返回识别前解析器对 fixture 的产出（字符串 user 行不可见：
// 元数据仅来自 assistant 行，client=claude_code、目录/项目为空）。
// 返回 (messages, session)；date 由时间戳按本地时区推导，与采集器口径一致。
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

// TestRunCollect_StringContentRowsInertOnExistingDB：预置识别前的既有库后
// 再次采集，messages 与 sessions 的行数、全字段与聚合 token 必须不变。
// 若字符串行参与元数据推断，client 翻转会在 (client,id) 主键下插入新行
// （行数 2→4）、sessions 分行且 first_ts 前移，测试即失败。
func TestRunCollect_StringContentRowsInertOnExistingDB(t *testing.T) {
	projectsDir := copyStringContentFixture(t)

	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	tsA, tsB, date, seedMsgs, seedSession := stringContentSeed(t)
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
	res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{date}}, false, false)
	if !res.Complete() {
		t.Fatalf("RunCollect 失败: %+v", res)
	}

	// messages：行数与全字段逐一比对（按 id 顺序）。
	rows, err := usageDB.Query(`SELECT id, session_id, client, date, ts, model, directory, project,
		input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens
		FROM messages ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 messages 失败: %v", err)
	}
	defer rows.Close()
	var got []model.Message
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Client, &m.Date, &m.TS, &m.Model,
			&m.Directory, &m.Project, &m.InputTokens, &m.FreshInputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheCreateTokens, &m.TotalTokens); err != nil {
			t.Fatalf("扫描 messages 失败: %v", err)
		}
		got = append(got, m)
	}
	rows.Close()
	if len(got) != len(seedMsgs) {
		t.Fatalf("messages 行数 %d, want %d（字符串行参与推断会在 (client,id) 主键下重复入库）: %+v",
			len(got), len(seedMsgs), got)
	}
	for i := range seedMsgs {
		if got[i] != seedMsgs[i] {
			t.Errorf("messages[%d] 字段漂移:\n got  %+v\n want %+v", i, got[i], seedMsgs[i])
		}
	}

	// sessions：行数与字段不变（client 不翻转到另一主键行、first_ts 不前移）。
	var sessionCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("统计 sessions 行数失败: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("sessions 行数 %d, want 1（client 翻转会在 (id,client) 主键下分裂出新行）", sessionCount)
	}
	var sID, sClient, sDir, sProject, sTitle string
	var sFirst, sLast int64
	err = usageDB.QueryRow(`SELECT id, client, directory, project, title, first_ts, last_ts
		FROM sessions`).Scan(&sID, &sClient, &sDir, &sProject, &sTitle, &sFirst, &sLast)
	if err != nil {
		t.Fatalf("查询 sessions 失败: %v", err)
	}
	if sID != seedSession.ID || sClient != model.ClientClaudeCode || sDir != "" || sProject != "" ||
		sFirst != tsA || sLast != tsB {
		t.Errorf("session 字段漂移: id=%q client=%q dir=%q project=%q first=%d/%d last=%d/%d",
			sID, sClient, sDir, sProject, sFirst, tsA, sLast, tsB)
	}

	// 聚合 token 不变。
	var sum int64
	if err := usageDB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM messages`).Scan(&sum); err != nil {
		t.Fatalf("聚合 token 失败: %v", err)
	}
	if want := int64(200 + 360); sum != want {
		t.Errorf("SUM(total_tokens) = %d, want %d", sum, want)
	}
}
