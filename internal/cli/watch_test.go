package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// runWatchOnce 装配注入依赖的 watch 命令,以 once 模式执行并返回帧输出。
func runWatchOnce(t *testing.T, args []string, cfg *config.Config, usageDB *db.DB, today time.Time) string {
	t.Helper()
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		func() time.Time { return today },
		func(time.Duration) { t.Fatal("非 once 模式不应 sleep") },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--once"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// --by project:帧内分组表切换为按项目分组,两个项目的键都进帧。
func TestWatchCmd_ByProjectFrame(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "wp-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(),
			Project: "proj-alpha", TotalTokens: 300},
		{ID: "wp-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(),
			Project: "proj-beta", TotalTokens: 500},
	}); err != nil {
		t.Fatal(err)
	}

	out := runWatchOnce(t, []string{"--by", "project"}, &config.Config{DataDir: t.TempDir()}, usageDB, today)
	if !strings.Contains(out, "Group by project / 按项目分组") {
		t.Errorf("帧内应含按项目分组表头:\n%s", out)
	}
	if !strings.Contains(out, "proj-alpha") || !strings.Contains(out, "proj-beta") {
		t.Errorf("帧内应含两个项目分组行:\n%s", out)
	}
}

// --by client:帧内分组表切换为按客户端分组。
func TestWatchCmd_ByClientFrame(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "wc-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 300},
		{ID: "wc-b", SessionID: "s", Client: model.ClientCodexCLI,
			Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 500},
	}); err != nil {
		t.Fatal(err)
	}

	out := runWatchOnce(t, []string{"--by", "client"}, &config.Config{DataDir: t.TempDir()}, usageDB, today)
	if !strings.Contains(out, "Group by client / 按客户端分组") {
		t.Errorf("帧内应含按客户端分组表头:\n%s", out)
	}
	if !strings.Contains(out, model.ClientClaudeCode) || !strings.Contains(out, model.ClientCodexCLI) {
		t.Errorf("帧内应含两个客户端分组行:\n%s", out)
	}
}

// --by 非法或时间维度:在配置加载与开库之前双语报错,不得触碰任何 I/O。
func TestWatchCmd_ByInvalidDimension(t *testing.T) {
	for _, by := range []string{"day", "unknown"} {
		cmd := newWatchCmdWithDeps(
			func() (*config.Config, error) {
				t.Fatal("非法 --by 不应加载配置")
				return nil, nil
			},
			func(string) (*db.DB, error) {
				t.Fatal("非法 --by 不应打开数据库")
				return nil, fmt.Errorf("must not open")
			},
			func() time.Time { return time.Date(2026, 9, 6, 9, 30, 0, 0, time.Local) },
			func(time.Duration) {},
		)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--once", "--by", by})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("--by %s 应被拒绝", by)
		}
		for _, want := range []string{
			fmt.Sprintf("unknown --by dimension %q (allowed: client, model, provider, project); for temporal trends use `token-usage chart --line`", by),
			fmt.Sprintf("未知 --by 维度 %q（允许：client, model, provider, project）；时间趋势请用 `token-usage chart --line`", by),
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--by %s 报错应含 %q,实际: %v", by, want, err)
			}
		}
	}
}

// runWatchLoop 装配循环模式 watch:注入时钟从 start 起,sleep 每次推进 2 秒,
// 第 frames 帧渲染完后的 sleep 抛哨兵终止,返回逐帧输出(清屏序列分帧)。
func runWatchLoop(t *testing.T, args []string, usageDB *db.DB, start time.Time, frames int) []string {
	t.Helper()
	now := start
	n := 0
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
		func() time.Time { return now },
		func(time.Duration) {
			n++
			now = now.Add(2 * time.Second)
			if n == frames {
				panic(watchLoopSentinel)
			}
		},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	func() {
		defer func() {
			if r := recover(); r != watchLoopSentinel {
				t.Fatalf("unexpected panic: %v", r)
			}
		}()
		_ = cmd.Execute()
	}()
	return strings.Split(buf.String(), "\x1b[2J\x1b[H")
}

// 缺省日期跨午夜逐帧重算:循环模式的「今天」不能在启动时固定,跨日后帧应
// 查询新的一天;显式指定的日期区间保持固定(监视历史区间是合法用法)。
// 两个子用例各建独立内存库:命令 RunE 会关闭注入的 DB,共享实例会被首个
// 用例关闭。
func TestWatchCmd_MidnightRolloverDefaultDate(t *testing.T) {
	start := time.Date(2026, 9, 1, 23, 59, 59, 0, time.Local)
	mkDB := func() *db.DB {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
			{ID: "wm-a", SessionID: "s", Client: model.ClientClaudeCode,
				Date: "2026-09-01", TS: start.UnixMilli(), Model: "yesterday-model", TotalTokens: 100},
			{ID: "wm-b", SessionID: "s", Client: model.ClientClaudeCode,
				Date: "2026-09-02", TS: start.Add(2 * time.Second).UnixMilli(), Model: "today-model", TotalTokens: 200},
		}); err != nil {
			t.Fatal(err)
		}
		return usageDB
	}

	// 缺省参数:第 2 帧在跨午夜后渲染,应查询 09-02。
	frames := runWatchLoop(t, nil, mkDB(), start, 2)
	last := frames[len(frames)-1]
	if !strings.Contains(last, "today-model") {
		t.Errorf("跨午夜后缺省日期应逐帧重算,帧应含新一天的分组:\n%s", last)
	}

	// 显式日期:跨午夜后仍查询固定窗口 09-01。
	frames = runWatchLoop(t, []string{"20260901"}, mkDB(), start, 2)
	last = frames[len(frames)-1]
	if !strings.Contains(last, "yesterday-model") || strings.Contains(last, "today-model") {
		t.Errorf("显式日期跨午夜后应保持固定窗口:\n%s", last)
	}
}
