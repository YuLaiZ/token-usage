package cli

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// watchGroupConfig 构造带配置视图的测试配置:组合查询 group 展开为
// client/provider/model/mpc 四张表,子查询 mpc 为 model+provider+client 多维表,
// 与 query.default 指向 group 的用户形态一致。
func watchGroupConfig(dataDir string) *config.Config {
	return &config.Config{
		DataDir: dataDir,
		RawQuery: map[string]any{
			"default":    "group",
			"groups":     map[string]any{"group": "client,provider,model,mpc"},
			"subqueries": map[string]any{"mpc": "model,provider,client"},
		},
	}
}

// seedWatchDB 写入两个客户端/模型/项目的当日消息,返回内存库。
func seedWatchDB(t *testing.T, today time.Time) *db.DB {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usageDB.Close() })
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "ws-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Project: "proj-alpha", Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 300},
		{ID: "ws-b", SessionID: "s", Client: model.ClientCodexCLI, Model: "model-b",
			Project: "proj-beta", Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 500},
	}); err != nil {
		t.Fatal(err)
	}
	return usageDB
}

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

// 不带 --by:帧体执行 query.default 指向的组合查询,展开为四张表,
// 且不再包含旧实现自造的 Summary 表;统计信息区与 query 输出一致。
func TestWatchCmd_DefaultFollowsQueryDefault(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, nil, watchGroupConfig(t.TempDir()), usageDB, today)
	for _, want := range []string{
		"Usage statistics",
		"Group by client / 按客户端分组",
		"Group by provider / 按供应商分组",
		"Group by model / 按模型分组",
		"Custom view mpc / 自定义视图 mpc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺省帧应执行 query.default=group 并含 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "总览摘要") {
		t.Errorf("缺省帧不应再包含自造 Summary 表:\n%s", out)
	}
	if !strings.Contains(out, model.ClientClaudeCode) || !strings.Contains(out, model.ClientCodexCLI) {
		t.Errorf("组合查询的 client 表应含两个客户端分组行:\n%s", out)
	}
}

// 无 query 配置:缺省帧回退内置 client 视图(与裸 query 的内置回退一致),
// 不再是旧实现的固定 model。
func TestWatchCmd_DefaultFallbackClient(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, nil, &config.Config{DataDir: t.TempDir()}, usageDB, today)
	if !strings.Contains(out, "Group by client / 按客户端分组") {
		t.Errorf("无 query.default 时缺省帧应回退 client 视图:\n%s", out)
	}
	if strings.Contains(out, "Group by model / 按模型分组") {
		t.Errorf("无 query.default 时缺省帧不应渲染 model 视图:\n%s", out)
	}
}

// --by 配置名(自定义子查询):帧内渲染多维表,标题与 query custom 一致。
func TestWatchCmd_ByConfiguredSubquery(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, []string{"--by", "mpc"}, watchGroupConfig(t.TempDir()), usageDB, today)
	if !strings.Contains(out, "Custom view mpc / 自定义视图 mpc") {
		t.Errorf("--by mpc 应渲染自定义多维视图:\n%s", out)
	}
	for _, want := range []string{model.ClientClaudeCode, model.ClientCodexCLI, "model-a", "model-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("多维视图应含键 %q:\n%s", want, out)
		}
	}
}

// --by 配置名(组合查询):帧内按声明顺序展开成员表。
func TestWatchCmd_ByConfiguredGroup(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, []string{"--by", "group"}, watchGroupConfig(t.TempDir()), usageDB, today)
	for _, want := range []string{
		"Group by client / 按客户端分组",
		"Group by provider / 按供应商分组",
		"Group by model / 按模型分组",
		"Custom view mpc / 自定义视图 mpc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--by group 应按声明顺序展开成员表 %q:\n%s", want, out)
		}
	}
	order := []string{"Group by client", "Group by provider", "Group by model", "Custom view mpc"}
	last := -1
	for _, title := range order {
		idx := strings.Index(out, title)
		if idx < 0 {
			t.Fatalf("--by group 应含成员表 %q:\n%s", title, out)
		}
		if idx <= last {
			t.Errorf("--by group 的成员表应按声明顺序 %v 渲染:\n%s", order, out)
		}
		last = idx
	}
}

// --by 内置视图:时间维度 day 同样合法(与 query day 等静态子命令一致)。
func TestWatchCmd_ByBuiltinDayFrame(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, []string{"--by", "day"}, &config.Config{DataDir: t.TempDir()}, usageDB, today)
	if !strings.Contains(out, "Usage by day / 按天用量") {
		t.Errorf("--by day 应渲染按天用量表:\n%s", out)
	}
}

// --by client:帧内分组表切换为按客户端分组。
func TestWatchCmd_ByClientFrame(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	out := runWatchOnce(t, []string{"--by", "client"}, &config.Config{DataDir: t.TempDir()}, usageDB, today)
	if !strings.Contains(out, "Group by client / 按客户端分组") {
		t.Errorf("帧内应含按客户端分组表头:\n%s", out)
	}
	if !strings.Contains(out, model.ClientClaudeCode) || !strings.Contains(out, model.ClientCodexCLI) {
		t.Errorf("帧内应含两个客户端分组行:\n%s", out)
	}
}

// 显式内置 --by 与 query 静态子命令同一隔离语义:视图定义坏档不阻断,
// 只把 query.output 布局错误作为该路径错误。
func TestWatchCmd_ExplicitBuiltinIsolatesBrokenDefs(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)
	usageDB := seedWatchDB(t, today)

	cfg := &config.Config{
		DataDir: t.TempDir(),
		RawQuery: map[string]any{
			"groups": map[string]any{"bad": "client,nosuch"},
		},
	}
	out := runWatchOnce(t, []string{"--by", "client"}, cfg, usageDB, today)
	if !strings.Contains(out, "Group by client / 按客户端分组") {
		t.Errorf("显式内置 --by 不应被无关视图定义错误阻断:\n%s", out)
	}
}

// 缺省路径与配置视图名路径消费完整解析:视图定义坏档时与裸 query 一致拒绝。
func TestWatchCmd_DefaultRejectsBrokenDefs(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)

	cfg := &config.Config{
		DataDir: t.TempDir(),
		RawQuery: map[string]any{
			"groups": map[string]any{"bad": "client,nosuch"},
		},
	}
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) {
			t.Fatal("坏视图定义应在打开数据库前拒绝")
			return nil, fmt.Errorf("must not open")
		},
		func() time.Time { return today },
		func(time.Duration) {},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("坏视图定义应使缺省 watch 拒绝启动")
	}
	for _, want := range []string{"invalid item", "无效项"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 querydef 诊断 %q,实际: %v", want, err)
		}
	}
}

// 非法 --by:既非内置视图也非配置视图名时,在打开数据库之前双语报错,
// 允许集合动态列出已配置视图名(时间维度自本版起合法,不再单独提示)。
func TestWatchCmd_ByInvalidDimension(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)

	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return watchGroupConfig(t.TempDir()), nil },
		func(string) (*db.DB, error) {
			t.Fatal("非法 --by 不应打开数据库")
			return nil, fmt.Errorf("must not open")
		},
		func() time.Time { return today },
		func(time.Duration) {},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once", "--by", "bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--by bogus 应被拒绝")
	}
	views := "client, model, provider, project, day, month, hour, weekday, heatmap, session, summary, mpc, group"
	if !strings.Contains(err.Error(), fmt.Sprintf("unknown --by view %q (allowed: %s)", "bogus", views)) {
		t.Errorf("报错应含动态允许集合,实际: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("未知 --by 视图 %q(允许：%s)", "bogus", views)) {
		t.Errorf("中文报错应含动态允许集合,实际: %v", err)
	}
}

// 帧体与 query 输出逐字节一致(去掉 Live watch 壳后,忽略尾随空行差异):
// 统计信息区、视图表、输出列布局与 provider 别名全部来自同一执行链。
// 两个命令的 RunE 各自关闭注入实例,故用文件库并让注入 open 每次重新打开。
func TestWatchCmd_FrameMatchesQueryOutput(t *testing.T) {
	today := time.Now()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{
		{ID: "wf-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Project: "proj-alpha", Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 300},
		{ID: "wf-b", SessionID: "s", Client: model.ClientCodexCLI, Model: "model-b",
			Project: "proj-beta", Date: today.Format("2006-01-02"), TS: today.UnixMilli(), TotalTokens: 500},
	}); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := watchGroupConfig(filepath.Dir(dbPath))
	reopen := func(string) (*db.DB, error) { return db.Open(dbPath) }

	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		reopen,
		func() time.Time { return today },
		func(time.Duration) { t.Fatal("非 once 模式不应 sleep") },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	frame := buf.String()
	sep := "\n\n"
	idx := strings.Index(frame, sep)
	if idx < 0 {
		t.Fatalf("帧应含 Live watch 壳与帧体分隔:\n%s", frame)
	}
	frameBody := strings.TrimRight(frame[idx+len(sep):], "\n")

	var qbuf bytes.Buffer
	qcmd := newQueryCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		reopen,
	)
	qcmd.SetOut(&qbuf)
	qcmd.SetErr(&qbuf)
	if err := qcmd.Execute(); err != nil {
		t.Fatal(err)
	}
	queryOut := strings.TrimRight(qbuf.String(), "\n")

	if frameBody != queryOut {
		t.Errorf("watch 帧体应与 query 输出一致。\n--- frame ---\n%s\n--- query ---\n%s", frameBody, queryOut)
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

	// 缺省参数:第 2 帧在跨午夜后渲染,应查询 09-02(缺省视图回退 client,
	// 窗口切换以统计信息区的 Query range 行断言)。
	frames := runWatchLoop(t, nil, mkDB(), start, 2)
	last := frames[len(frames)-1]
	if !strings.Contains(last, "Query range / 统计范围: 2026-09-02") {
		t.Errorf("跨午夜后缺省日期应逐帧重算,帧应查询新的一天:\n%s", last)
	}

	// 显式日期:跨午夜后仍查询固定窗口 09-01。
	frames = runWatchLoop(t, []string{"20260901"}, mkDB(), start, 2)
	last = frames[len(frames)-1]
	if !strings.Contains(last, "Query range / 统计范围: 2026-09-01") || strings.Contains(last, "2026-09-02") {
		t.Errorf("显式日期跨午夜后应保持固定窗口:\n%s", last)
	}
}

// --by 配置名路径与缺省路径同样消费完整解析:坏视图定义在打开数据库之前拒绝。
func TestWatchCmd_ByConfiguredNameRejectsBrokenDefs(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)

	cfg := &config.Config{
		DataDir: t.TempDir(),
		RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "client,provider"},
			"groups":     map[string]any{"bad": "client,nosuch"},
		},
	}
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return cfg, nil },
		func(string) (*db.DB, error) {
			t.Fatal("坏视图定义应在打开数据库前拒绝")
			return nil, fmt.Errorf("must not open")
		},
		func() time.Time { return today },
		func(time.Duration) {},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once", "--by", "mpc"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("坏视图定义应使 --by 配置名路径拒绝启动")
	}
	if !strings.Contains(err.Error(), "invalid item") {
		t.Errorf("错误应含 querydef 诊断,实际: %v", err)
	}
}

// --by 显式空串:非内置视图名,走配置名解析后按未知名拒绝,不得回退缺省视图。
func TestWatchCmd_ByEmptyStringRejected(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)

	opened := false
	cmd := newWatchCmdWithDeps(
		func() (*config.Config, error) { return watchGroupConfig(t.TempDir()), nil },
		func(string) (*db.DB, error) {
			opened = true
			return nil, fmt.Errorf("must not open")
		},
		func() time.Time { return today },
		func(time.Duration) {},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--once", "--by", ""})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--by 空串应被拒绝")
	}
	if !strings.Contains(err.Error(), `unknown --by view ""`) {
		t.Errorf("空串应按未知名拒绝,实际: %v", err)
	}
	if opened {
		t.Error("空串拒绝不得打开数据库")
	}
}

// --by 内置特殊视图(summary/heatmap/session):帧内渲染对应视图,
// summary 不被 query.output 坏布局阻断(与 query summary 同一边界)。
// 每个 case 独立建库:RunE 会关闭注入实例。
func TestWatchCmd_ByBuiltinSpecialViews(t *testing.T) {
	today := time.Date(2026, 9, 7, 9, 30, 0, 0, time.Local)

	cases := []struct {
		by   string
		want string
	}{
		{"summary", "Summary / 总览摘要"},
		{"heatmap", "Heatmap (weekday x hour, local time) / 热力图(星期×小时,本机时区)"},
		{"session", "Session details / 会话明细"},
	}
	for _, tc := range cases {
		out := runWatchOnce(t, []string{"--by", tc.by}, &config.Config{DataDir: t.TempDir()}, seedWatchDB(t, today), today)
		if !strings.Contains(out, tc.want) {
			t.Errorf("--by %s 帧内应含 %q:\n%s", tc.by, tc.want, out)
		}
	}

	// 坏 query.output 不阻断 --by summary(query summary 同一边界)。
	brokenLayout := &config.Config{
		DataDir: t.TempDir(),
		RawQuery: map[string]any{
			"output": map[string]any{"columns": []any{"nosuch"}},
		},
	}
	out := runWatchOnce(t, []string{"--by", "summary"}, brokenLayout, seedWatchDB(t, today), today)
	if !strings.Contains(out, "Summary / 总览摘要") {
		t.Errorf("坏布局不应阻断 --by summary:\n%s", out)
	}
}
