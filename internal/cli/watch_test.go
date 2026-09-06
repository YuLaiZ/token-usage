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
