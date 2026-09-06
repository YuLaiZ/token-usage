package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattn/go-runewidth"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// TestSortTopRows_TieBreakDeterminism 相同 TotalTokens 时按 Client 升序、再
// Title 升序决序;前三键全并列时按 FirstTS 升序决序,排名对输入顺序不敏感
// (确定性全序)。
func TestSortTopRows_TieBreakDeterminism(t *testing.T) {
	rows := []querier.SessionRow{
		{Client: "Claude Code", Title: "b-task", Agg: querier.GroupAggregate{TotalTokens: 100}},
		{Client: "Codex App", Title: "a-task", Agg: querier.GroupAggregate{TotalTokens: 100}},
		{Client: "Claude Code", Title: "a-task", Agg: querier.GroupAggregate{TotalTokens: 100}},
		{Client: "Anthropic", Title: "z-task", Agg: querier.GroupAggregate{TotalTokens: 200}},
	}
	// 两种输入排列必须得到同一行序:总量降序,并列内 Client/Title 升序。
	shuffled := []querier.SessionRow{rows[2], rows[0], rows[3], rows[1]}
	want := []string{
		"Anthropic/z-task",
		"Claude Code/a-task",
		"Claude Code/b-task",
		"Codex App/a-task",
	}
	for name, in := range map[string][]querier.SessionRow{"orig": rows, "shuffled": shuffled} {
		got := sortTopRows(in)
		if len(got) != len(want) {
			t.Fatalf("%s: 行数应保持 %d,实际 %d", name, len(want), len(got))
		}
		for i, key := range want {
			if k := got[i].Client + "/" + got[i].Title; k != key {
				t.Errorf("%s: 第 %d 行应为 %q,实际 %q", name, i+1, key, k)
			}
		}
	}

	// TotalTokens/Client/Title 三键完全并列、FirstTS 不同的两行:按 FirstTS
	// 升序决序,两种输入排列输出同一行序。
	paired := []querier.SessionRow{
		{Client: "Claude Code", Title: "dup-task", FirstTS: 2000, Agg: querier.GroupAggregate{TotalTokens: 100}},
		{Client: "Claude Code", Title: "dup-task", FirstTS: 1000, Agg: querier.GroupAggregate{TotalTokens: 100}},
	}
	reversed := []querier.SessionRow{paired[1], paired[0]}
	for name, in := range map[string][]querier.SessionRow{"pair-orig": paired, "pair-reversed": reversed} {
		got := sortTopRows(in)
		if len(got) != 2 || got[0].FirstTS != 1000 || got[1].FirstTS != 2000 {
			t.Errorf("%s: 全并列两行应按 FirstTS 升序 [1000 2000],实际 [%d %d]",
				name, got[0].FirstTS, got[1].FirstTS)
		}
	}
}

// TestTruncateTopRows_TwelveSeedLimitTenAndOne 截断语义:种子 12 条,limit 10
// 恰取排序后最大的 10 个;limit 1 只取最大者;limit 超过行数时返回全部。
func TestTruncateTopRows_TwelveSeedLimitTenAndOne(t *testing.T) {
	rows := make([]querier.SessionRow, 12)
	for i := range rows {
		rows[i] = querier.SessionRow{
			Client: "Claude Code", Title: fmt.Sprintf("s%02d", i),
			Agg: querier.GroupAggregate{TotalTokens: int64(i + 1)}, // 1..12
		}
	}
	sorted := sortTopRows(rows)

	top10 := truncateTopRows(sorted, 10)
	if len(top10) != 10 {
		t.Fatalf("limit 10 应恰 10 行,实际 %d", len(top10))
	}
	for i, row := range top10 {
		// 最大的 10 个:12,11,..3。
		if want := int64(12 - i); row.Agg.TotalTokens != want {
			t.Errorf("第 %d 行 TotalTokens 应为 %d,实际 %d", i+1, want, row.Agg.TotalTokens)
		}
	}

	top1 := truncateTopRows(sorted, 1)
	if len(top1) != 1 || top1[0].Agg.TotalTokens != 12 {
		t.Errorf("limit 1 应只含最大总量 12,实际 %+v", top1)
	}

	all := truncateTopRows(sorted, 99)
	if len(all) != 12 {
		t.Errorf("limit 超过行数应返回全部 12 行,实际 %d", len(all))
	}

	exact := truncateTopRows(sorted, 12)
	if len(exact) != 12 {
		t.Errorf("limit 恰等行数应返回全部 12 行,实际 %d", len(exact))
	}
}

// TestTopCmd_LimitValidation --limit 取值小于 1 时双语报错,且校验先于配置
// 加载与数据库打开(注入的 load/open 一旦被调用即 t.Fatal)。
func TestTopCmd_LimitValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
	}{
		{"zero", "--limit=0"},
		{"negative", "--limit=-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTopCmdWithDeps(
				func() (*config.Config, error) { t.Fatal("--limit 校验失败不应加载配置"); return nil, nil },
				func(string) (*db.DB, error) { t.Fatal("--limit 校验失败不应打开数据库"); return nil, nil },
			)
			cmd.SetArgs([]string{"20260709", tc.flag})
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("%s 应返回 error", tc.flag)
			}
			for _, want := range []string{
				"invalid --limit ",
				"(must be at least 1)",
				"无效的 --limit ",
				"（至少为 1）",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误文案缺少 %q,实际: %v", want, err)
				}
			}
		})
	}
}

// topTestSeed 写入三个会话:rank1 单条 5000(空 project)、rank2 两条共 1100
// (跨度 61 分钟)、rank3 单条 300(35 字符长标题验证截断)。全部落在 2026-07-09。
func topTestSeed(t *testing.T, usageDB *db.DB) {
	t.Helper()
	ts := func(h, m int) int64 { return time.Date(2026, 7, 9, h, m, 0, 0, time.Local).UnixMilli() }
	sessions := []model.Session{
		{ID: "sess-h1", Client: model.ClientCodexApp, Directory: "/h", Project: "", Title: "heavy-batch"},
		{ID: "sess-f2", Client: model.ClientClaudeCode, Directory: "/f", Project: "proj-alpha", Title: "fix-login"},
		{ID: "sess-t3", Client: model.ClientClaudeCode, Directory: "/t", Project: "proj-beta", Title: strings.Repeat("t", 35)},
	}
	if _, err := db.UpsertSessionMeta(context.Background(), usageDB, sessions); err != nil {
		t.Fatal(err)
	}
	msgs := []model.Message{
		{ID: "m-h1", SessionID: "sess-h1", Client: model.ClientCodexApp, Date: "2026-07-09", TS: ts(9, 0), TotalTokens: 5000},
		{ID: "m-f2a", SessionID: "sess-f2", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: ts(10, 0), TotalTokens: 500},
		{ID: "m-f2b", SessionID: "sess-f2", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: ts(11, 1), TotalTokens: 600},
		{ID: "m-t3", SessionID: "sess-t3", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: ts(12, 30), TotalTokens: 300},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}
}

// newTopTestCmd 构造注入内存库的 top 命令(真实 RunE 链,配置注入临时目录)。
func newTopTestCmd(t *testing.T, usageDB *db.DB) *cobra.Command {
	t.Helper()
	cmd := newTopCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	return cmd
}

// topTableRows 提取输出中第一张框线表的数据行(├ 与 └ 之间、按 │ 切分并
// TrimSpace 的单元格序列),供确切单元格断言。
func topTableRows(t *testing.T, out string) [][]string {
	t.Helper()
	var rows [][]string
	inBody := false
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "├") {
			inBody = true
			continue
		}
		if strings.Contains(ln, "└") {
			inBody = false
			continue
		}
		if !inBody || !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		row := make([]string, 0, len(cells))
		for _, c := range cells {
			row = append(row, strings.TrimSpace(c))
		}
		rows = append(rows, row)
	}
	return rows
}

// TestTopCmd_EndToEnd 真实 RunE + 内存库:断言名次顺序与
// Title/Client/Project/Duration/Requests/Total 单元格确切值,空 project 映射
// 未分类,超宽 Title 按 30 显示宽截断加省略号。
func TestTopCmd_EndToEnd(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	topTestSeed(t, usageDB)

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "Top sessions / 会话排行\n") {
		t.Errorf("输出应以标题行开始,实际:\n%s", out)
	}

	rows := topTableRows(t, out)
	if len(rows) != 3 {
		t.Fatalf("应恰 3 行数据,实际 %d:\n%s", len(rows), out)
	}
	want := [][]string{
		{"1", "heavy-batch", "Codex App", "(uncategorized) / (未分类)", "<1s", "1", "5.00 K"},
		{"2", "fix-login", "Claude Code", "proj-alpha", "1h 1m", "2", "1.10 K"},
		// 35 个 t 截断为 27 个 t 加 "...":显示宽恰 30。
		{"3", strings.Repeat("t", 27) + "...", "Claude Code", "proj-beta", "<1s", "1", "300"},
	}
	for i, row := range want {
		if !equalCells(rows[i], row) {
			t.Errorf("第 %d 行应为 %v,实际 %v", i+1, row, rows[i])
		}
	}
}

// TestTopCmd_CJKTitleTruncation E2E 中文标题截断:显示宽 36 的 CJK 标题按 30
// 显示宽截断加省略号(13 个汉字 + "..."),且表格各行渲染宽度与边框一致,
// CJK 宽字符不破坏框线对齐。
func TestTopCmd_CJKTitleTruncation(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	ts := time.Date(2026, 7, 9, 8, 0, 0, 0, time.Local).UnixMilli()
	sessions := []model.Session{
		{ID: "sess-cjk", Client: model.ClientClaudeCode, Directory: "/c", Project: "proj-cjk", Title: strings.Repeat("数", 18)},
	}
	if _, err := db.UpsertSessionMeta(context.Background(), usageDB, sessions); err != nil {
		t.Fatal(err)
	}
	msgs := []model.Message{
		{ID: "m-cjk", SessionID: "sess-cjk", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: ts, TotalTokens: 800},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	rows := topTableRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("应恰 1 行数据,实际 %d:\n%s", len(rows), out)
	}
	// 13 个汉字显示宽 26,加省略号 "..." 恰在 30 显示宽上限内截断。
	wantTitle := strings.Repeat("数", 13) + "..."
	if rows[0][1] != wantTitle {
		t.Errorf("CJK 标题应截断为 %q(显示宽 %d),实际 %q", wantTitle, runewidth.StringWidth(wantTitle), rows[0][1])
	}

	// 表格所有行(含边框)渲染宽度一致,框线未因 CJK 宽字符错位。
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("应含标题行与完整表格,实际:\n%s", out)
	}
	borderWidth := runewidth.StringWidth(lines[1]) // 表格顶边框
	for i, ln := range lines[1:] {
		if w := runewidth.StringWidth(ln); w != borderWidth {
			t.Errorf("表格第 %d 行显示宽 %d != 边框 %d:\n%s", i+1, w, borderWidth, out)
		}
	}
}

// TestTopCmd_LimitFlagTruncates E2E --limit 截断:--limit=1 只输出名次 1 的行。
func TestTopCmd_LimitFlagTruncates(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	topTestSeed(t, usageDB)

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709", "--limit=1"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	rows := topTableRows(t, buf.String())
	if len(rows) != 1 {
		t.Fatalf("--limit=1 应恰 1 行,实际 %d:\n%s", len(rows), buf.String())
	}
	if !equalCells(rows[0], []string{"1", "heavy-batch", "Codex App", "(uncategorized) / (未分类)", "<1s", "1", "5.00 K"}) {
		t.Errorf("保留行应为名次 1,实际 %v", rows[0])
	}
}

// TestTopCmd_NoDataEmptyState 空库:标题行 + 无数据一行,不渲染空表。
func TestTopCmd_NoDataEmptyState(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "Top sessions / 会话排行\nno data / 无数据\n"
	if buf.String() != want {
		t.Errorf("空态输出应为 %q,实际 %q", want, buf.String())
	}
}

// equalCells 逐单元格比较两个表格行。
func equalCells(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
