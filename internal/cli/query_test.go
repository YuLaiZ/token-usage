package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// TestNewQueryCmd_NoOldFlags 断言 删除的旧 flag 已全部不存在，
// 且这些 flag 现在是 unknown flag（cobra 在解析时报错，而非被静默接受）。
func TestNewQueryCmd_NoOldFlags(t *testing.T) {
	cmd := newQueryCmd()
	removed := []string{"by-model", "by-project", "sessions", "summary", "format", "date"}
	for _, name := range removed {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("旧 flag %q 应已删除（子命令化）", name)
		}
		if cmd.PersistentFlags().Lookup(name) != nil {
			t.Errorf("旧 persistent flag %q 应已删除", name)
		}
	}
}

// TestNewQueryCmd_SubcommandTree 断言 query 命令树包含且仅包含六个子命令，
// 且每个子命令的 Short/Use 与公开 CLI 文档一致。
func TestNewQueryCmd_SubcommandTree(t *testing.T) {
	cmd := newQueryCmd()

	wantShort := map[string]string{
		"client":   "Group by client (default) / 按客户端分组（默认）",
		"model":    "Group by model / 按模型分组",
		"provider": "Group by provider / 按供应商分组",
		"project":  "Group by project / 按项目分组",
		"session":  "View session details / 查看会话明细",
		"summary":  "View summary / 查看总览摘要",
		"custom":   "Run a configured custom or group query / 执行已配置的自定义或组合查询",
		"list":     "List configured query views / 列出已配置查询视图",
	}

	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
		want, ok := wantShort[sub.Name()]
		if !ok {
			t.Errorf("意外子命令: %q", sub.Name())
			continue
		}
		if sub.Short != want {
			t.Errorf("子命令 %q Short = %q, want %q", sub.Name(), sub.Short, want)
		}
	}
	for name := range wantShort {
		if !got[name] {
			t.Errorf("缺少子命令 %q", name)
		}
	}
}

// TestNewQueryCmd_SubcommandMaxOneArg 断言每个子命令最多接受一个日期/范围参数，
// 超出则在 args 校验阶段报错（不是 silently 接受）。
func TestNewQueryCmd_SubcommandMaxOneArg(t *testing.T) {
	cmd := newQueryCmd()
	for _, name := range []string{"client", "model", "provider", "project", "session", "summary"} {
		sub, _, err := cmd.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%q) err: %v", name, err)
		}
		if sub.Name() != name {
			t.Fatalf("Find(%q) returned %q", name, sub.Name())
		}
		buf := &bytes.Buffer{}
		sub.SetOut(buf)
		sub.SetErr(buf)
		if err := sub.Args(sub, []string{"20260101", "20260102"}); err == nil {
			t.Errorf("子命令 %q 接受了 2 个位置参数（应最多 1 个）", name)
		}
	}
}

// TestNewQueryCmd_RootArgsContract 断言根命令新 Args 合同:零、一、二个位置参数
// 均通过(默认视图日期 / 视图名 / 视图名加日期),三项及以上在加载配置与打开 DB
// 之前返回专用的双语四形态用法错误。
func TestNewQueryCmd_RootArgsContract(t *testing.T) {
	cmd := newQueryCmd()
	for _, n := range []int{0, 1, 2} {
		args := make([]string, n)
		for i := range args {
			args[i] = "arg"
		}
		if err := cmd.Args(cmd, args); err != nil {
			t.Errorf("根命令 %d 个参数应通过: %v", n, err)
		}
	}
	for _, args := range [][]string{{"a", "b", "c"}, {"a", "b", "c", "d"}} {
		err := cmd.Args(cmd, args)
		if err == nil {
			t.Fatalf("%d 个参数应报错", len(args))
		}
		msg := err.Error()
		for _, want := range []string{"no args", "date", "view name", "/"} {
			if !strings.Contains(msg, want) {
				t.Errorf("超参用法错误应为双语并说明四种形态(缺 %q): %q", want, msg)
			}
		}
	}
}

// TestNewQueryCmd_RootHelpPresentsBothForms 断言根命令帮助静态展示
// 「名称加可选日期」与「纯日期」两种调用形态及 custom 等价写法;
// 构造命令树不加载配置(无配置环境下同样可断言)。
func TestNewQueryCmd_RootHelpPresentsBothForms(t *testing.T) {
	cmd := newQueryCmd()
	if !strings.Contains(cmd.Use, "<name>") || !strings.Contains(cmd.Use, "YYYYMMDD") {
		t.Errorf("Use 应静态展示视图名与日期两种形态: %q", cmd.Use)
	}
	for _, want := range []string{"token-usage query <name>", "query custom <name>"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("Long 应含示例 %q: %q", want, cmd.Long)
		}
	}
}

// TestExecuteQuery_ViewDispatch 断言每个 view 调用对应的 querier 方法。
// 通过内存空库观察各方法独特的"无数据"标题文案来确认分发正确。
func TestExecuteQuery_ViewDispatch(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	cases := []struct {
		view         queryView
		wantHeader   string
		wantNoHeader string
	}{
		{viewClient, "按客户端分组", "按模型分组"},
		{viewModel, "按模型分组", "按项目分组"},
		{viewProvider, "按供应商分组", "按项目分组"},
		{viewProject, "按项目分组", "按客户端分组"},
		{viewSessions, "会话明细", "总览摘要"},
		{viewSummary, "总览摘要", "会话明细"},
	}
	for _, c := range cases {
		buf := &bytes.Buffer{}
		if err := executeQuery(buf, usageDB, nil, c.view); err != nil {
			t.Fatalf("view %v executeQuery err: %v", c.view, err)
		}
		got := buf.String()
		if !strings.Contains(got, c.wantHeader) {
			t.Errorf("view %v 输出应含 %q, got: %s", c.view, c.wantHeader, got)
		}
		if strings.Contains(got, c.wantNoHeader) {
			t.Errorf("view %v 输出不应含 %q, got: %s", c.view, c.wantNoHeader, got)
		}
	}
}

// 配置的供应商别名只传给 provider 查询，不需要也不会触发写库。
func TestExecuteQueryDatesWithAliases_AppliesProviderAlias(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "provider-alias", SessionID: "session", Client: model.ClientZCode,
		Date: "2026-08-25", TS: 1, Provider: "raw-provider", TotalTokens: 10,
	}}); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	err = executeQueryDatesWithAliases(context.Background(), buf, usageDB, []string{"2026-08-25"}, viewProvider, map[string]string{
		"raw-provider": "Display provider",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Display provider") || strings.Contains(buf.String(), "raw-provider") {
		t.Errorf("provider alias should apply only to output, got:\n%s", buf.String())
	}
}

// TestExecuteQuery_BareAndClientShareView 断言裸 query 与 query client 走同一 view。
func TestExecuteQuery_BareAndClientShareView(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	bare := &bytes.Buffer{}
	if err := executeQuery(bare, usageDB, nil, viewClient); err != nil {
		t.Fatal(err)
	}
	client := &bytes.Buffer{}
	if err := executeQuery(client, usageDB, nil, viewClient); err != nil {
		t.Fatal(err)
	}
	if bare.String() != client.String() {
		t.Errorf("裸 query 与 client 输出应相同\nbare=%q\nclient=%q", bare.String(), client.String())
	}
}

// TestExecuteQuery_DateParsedAndForwarded 断言 executeQuery 把合法日期转发给 querier。
// 通过写入一条记录到指定日期，观察该日期出现在输出中（ByClient 按日期查询命中）。
func TestExecuteQuery_DateParsedAndForwarded(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	// 在 2026-06-09 插入一条 claude 记录
	if err := insertOneMessage(usageDB, "2026-06-09", "claude"); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := executeQuery(buf, usageDB, []string{"20260609"}, viewClient); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "claude") {
		t.Errorf("20260609 解析为 2026-06-09 应命中 claude 记录, got: %s", got)
	}
}

// TestExecuteQuery_InvalidDateReturnsError 断言非法日期在 executeQuery 阶段报错。
func TestExecuteQuery_InvalidDateReturnsError(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := executeQuery(&bytes.Buffer{}, usageDB, []string{"not-a-date"}, viewClient); err == nil {
		t.Error("非法日期应报错")
	}
}

// TestExecuteQuery_WarningUsesCollectRetrySyntax 断言异常提示使用新的
// `collect retry` 子命令语法，且不再包含旧的 `collect --retry`。
func TestExecuteQuery_WarningUsesCollectRetrySyntax(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude", "boom", "")

	buf := &bytes.Buffer{}
	if err := executeQuery(buf, usageDB, []string{"20260609"}, viewClient); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "collect --retry") {
		t.Errorf("异常提示不应再含旧的 `collect --retry`: %s", got)
	}
	if !strings.Contains(got, "collect retry") {
		t.Errorf("异常提示应含新的 `collect retry` 子命令语法: %s", got)
	}
}

// insertOneMessage 向内存库插入一条最小消息记录，供分发测试观察命中。
func insertOneMessage(usageDB *db.DB, date, client string) error {
	_, err := usageDB.ExecContext(context.Background(), `
INSERT INTO messages (id, session_id, client, date, ts, model, total_tokens)
VALUES (?, ?, ?, ?, 0, ?, 0)`,
		client+"-"+date, "sess-"+date, client, date, "test-model")
	return err
}

// 以下 showErrorWarnings 单元测试保留（起已存在，当前实现 未改其语义）。

func TestShowErrorWarnings_OnlyIncludesQueriedDates(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude", "database locked", "")
	db.RecordError(context.Background(), usageDB, "2026-06-08", "codex", "unrelated error", "")

	var buf bytes.Buffer
	if err := showErrorWarnings(&buf, usageDB, []string{"2026-06-09"}); err != nil {
		t.Fatal(err)
	}

	output := buf.String()
	if !strings.Contains(output, "采集异常") {
		t.Errorf("expected '采集异常', got: %s", output)
	}
	if !strings.Contains(output, "database locked") {
		t.Errorf("expected 'database locked', got: %s", output)
	}
	if strings.Contains(output, "unrelated error") {
		t.Errorf("warning leaked outside query dates: %s", output)
	}
}

func TestShowErrorWarnings_NoErrors(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	var buf bytes.Buffer
	if err := showErrorWarnings(&buf, usageDB, []string{"2026-06-09"}); err != nil {
		t.Fatal(err)
	}

	output := buf.String()
	if output != "" {
		t.Errorf("expected empty output, got: %s", output)
	}
}

func TestShowErrorWarnings_QueryFailureIsReturned(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	usageDB.Close()
	if err := showErrorWarnings(io.Discard, usageDB, []string{"2026-06-09"}); err == nil {
		t.Fatal("closed DB warning query must return error")
	}
}

// ---- 可配置默认视图与 custom 命令 ----

// newQueryOutputCmd 构造带输出捕获的 query 命令(供 deps 注入路径)。
func newQueryOutputCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := newQueryCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// memOpen 打开内存库并插入两条可观测消息。
func memOpen(t *testing.T) func(string) (*db.DB, error) {
	t.Helper()
	return func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if err := insertOneMessage(usageDB, "2026-07-09", "claude"); err != nil {
			t.Fatal(err)
		}
		if err := insertOneMessage(usageDB, "2026-07-09", "codex"); err != nil {
			t.Fatal(err)
		}
		return usageDB, nil
	}
}

func loadWithRaw(raw map[string]any, issues map[string]config.RawQueryTopLevelIssue) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		return &config.Config{DataDir: "/mem", RawQuery: raw, RawQueryTopLevelIssues: issues}, nil
	}
}

// 裸 query 未配置时执行 client;默认设为 custom 输出一张多维表;默认设为 group 按声明顺序输出多张表。
func TestRunQuery_DefaultTargets(t *testing.T) {
	open := memOpen(t)

	cmd, buf := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd, nil, viewDefault, loadWithRaw(nil, nil), open); err != nil {
		t.Fatalf("未配置裸 query: %v", err)
	}
	if !strings.Contains(buf.String(), "按客户端分组") {
		t.Errorf("未配置裸 query 应等价 client:\n%s", buf)
	}

	cmd2, buf2 := newQueryOutputCmd()
	rawCustom := map[string]any{
		"default":    "mpc",
		"subqueries": map[string]any{"mpc": "model,provider,client"},
	}
	if err := runQueryWithDeps(cmd2, nil, viewDefault, loadWithRaw(rawCustom, nil), open); err != nil {
		t.Fatalf("默认 custom: %v", err)
	}
	out2 := buf2.String()
	if !strings.Contains(out2, "自定义视图 mpc") {
		t.Errorf("默认 custom 应输出自定义视图标题:\n%s", out2)
	}
	if strings.Contains(out2, "按客户端分组") || strings.Contains(out2, "按供应商分组") {
		t.Errorf("默认 custom 只输出一张多维表:\n%s", out2)
	}

	cmd3, buf3 := newQueryOutputCmd()
	rawGroup := map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider,client"},
		"groups":     map[string]any{"group_q": "client,model,provider,mpc"},
	}
	if err := runQueryWithDeps(cmd3, nil, viewDefault, loadWithRaw(rawGroup, nil), open); err != nil {
		t.Fatalf("默认 group: %v", err)
	}
	out3 := buf3.String()
	for _, want := range []string{"按客户端分组", "按模型分组", "按供应商分组", "自定义视图 mpc"} {
		if !strings.Contains(out3, want) {
			t.Errorf("group 应按声明顺序输出各表,缺 %q:\n%s", want, out3)
		}
	}
	// 声明顺序:client 表在 model 表前,mpc 表最后。
	if !(strings.Index(out3, "按客户端分组") < strings.Index(out3, "按模型分组") &&
		strings.Index(out3, "按模型分组") < strings.Index(out3, "按供应商分组") &&
		strings.Index(out3, "按供应商分组") < strings.Index(out3, "自定义视图 mpc")) {
		t.Errorf("group 输出顺序与声明不一致:\n%s", out3)
	}
	// query custom group_q <date> 与默认执行同一对象。
	cmd4, buf4 := newQueryOutputCmd()
	if err := runQueryCustomWithDeps(cmd4, "group_q", []string{"20260709"}, loadWithRaw(rawGroup, nil), open); err != nil {
		t.Fatalf("custom group_q: %v", err)
	}
	if !strings.Contains(buf4.String(), "自定义视图 mpc") || strings.Count(buf4.String(), "Total / 总计") != 4 {
		t.Errorf("custom group_q 应与默认同一对象(四张表各含总计):\n%s", buf4.String())
	}
}

// custom 参数合同:RangeArgs(1,2)、双语缺参/超参错误;日期错误优先于名称/定义错误。
func TestRunQueryCustom_ArgsAndPrecedence(t *testing.T) {
	cmd, _ := newQueryOutputCmd()
	custom, _, err := cmd.Find([]string{"custom"})
	if err != nil {
		t.Fatal(err)
	}
	if err := custom.Args(custom, nil); err == nil {
		t.Error("0 参数应报错")
	} else if !strings.Contains(err.Error(), "/") {
		t.Errorf("缺参错误应为双语: %v", err)
	}
	if err := custom.Args(custom, []string{"mpc"}); err != nil {
		t.Errorf("1 参数应通过: %v", err)
	}
	if err := custom.Args(custom, []string{"mpc", "20260709"}); err != nil {
		t.Errorf("2 参数应通过: %v", err)
	}
	if err := custom.Args(custom, []string{"mpc", "20260709", "x"}); err == nil {
		t.Error("3 参数应报错")
	} else if !strings.Contains(err.Error(), "/") {
		t.Errorf("超参错误应为双语: %v", err)
	}

	open := memOpen(t)
	badDate := []string{"notadate"}
	// 非法名称 + 非法日期:日期错误优先。
	cmd2, _ := newQueryOutputCmd()
	err = runQueryCustomWithDeps(cmd2, "Bad Name", badDate, loadWithRaw(nil, nil), open)
	if err == nil || !strings.Contains(err.Error(), "notadate") {
		t.Errorf("非法名称+非法日期应报日期错误: %v", err)
	}
	// 非法名称 + 合法日期:名称/定义错误,且不打开 DB。
	openCalls := 0
	openCount := func(p string) (*db.DB, error) {
		openCalls++
		return db.Open(":memory:")
	}
	cmd3, _ := newQueryOutputCmd()
	err = runQueryCustomWithDeps(cmd3, "Bad Name", nil, loadWithRaw(nil, nil), openCount)
	if err == nil {
		t.Fatal("非法名称+合法日期应报名称错误")
	}
	if openCalls != 0 {
		t.Errorf("名称错误不得打开 DB,实际打开 %d 次", openCalls)
	}
	// 未知名称(定义合法):定义错误,不打开 DB。
	cmd4, _ := newQueryOutputCmd()
	err = runQueryCustomWithDeps(cmd4, "nope", nil, loadWithRaw(map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
	}, nil), openCount)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("未知名称应报错并含名称: %v", err)
	}
	if openCalls != 0 {
		t.Errorf("未知名称不得打开 DB")
	}
}

// 坏 query 配置的边界:顶层问题态与视图定义错误只挡完整 query 路径
// (裸 query/custom);五个静态表格命令不被阻断——顶层问题态静默使用
// 默认布局,无关视图错误不阻止 query.output 布局生效。
func TestRunQuery_BadQueryConfigBoundaryForStaticViews(t *testing.T) {
	open := memOpen(t)
	issues := map[string]config.RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}

	cmd, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd, nil, viewModel, loadWithRaw(nil, issues), open); err != nil {
		t.Fatalf("内置 model 不受坏 query 配置影响: %v", err)
	}
	cmd2, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd2, nil, viewDefault, loadWithRaw(nil, issues), open); err == nil {
		t.Fatal("issues 非空时裸 query 必须拒绝")
	}
	cmd3, _ := newQueryOutputCmd()
	if err := runQueryCustomWithDeps(cmd3, "mpc", nil, loadWithRaw(nil, issues), open); err == nil {
		t.Fatal("issues 非空时 custom 必须拒绝")
	}

	// 根值非表(root_not_table)同样只拒绝裸 query/custom。
	rootNotTable := map[string]config.RawQueryTopLevelIssue{
		"query": {Name: "query", Value: "x", Kind: config.RawQueryIssueRootNotTable},
	}
	cmdB, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(cmdB, nil, viewDefault, loadWithRaw(nil, rootNotTable), open); err == nil {
		t.Fatal("根值非表时裸 query 必须拒绝")
	}

	// CSV 错误与断开引用。
	csvBad := map[string]any{"subqueries": map[string]any{"mpc": "model,"}}
	cmd4, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd4, nil, viewDefault, loadWithRaw(csvBad, nil), open); err == nil {
		t.Fatal("CSV 错误应拒绝裸 query")
	}
	brokenRef := map[string]any{"groups": map[string]any{"g": "client,nope"}}
	cmd5, _ := newQueryOutputCmd()
	if err := runQueryCustomWithDeps(cmd5, "g", nil, loadWithRaw(brokenRef, nil), open); err == nil {
		t.Fatal("断开引用应拒绝 custom")
	}
	// session/summary 不能被 default 引用。
	sessionDefault := map[string]any{"default": "session"}
	cmd6, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd6, nil, viewDefault, loadWithRaw(sessionDefault, nil), open); err == nil {
		t.Fatal("default 引用 session 应拒绝")
	}
}

// group 输出多个表后只出现一次采集异常警告。
func TestRunQuery_GroupSingleWarning(t *testing.T) {
	open := func(p string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		if err := insertOneMessage(usageDB, "2026-07-09", "claude"); err != nil {
			return nil, err
		}
		db.RecordError(context.Background(), usageDB, "2026-07-09", "claude", "boom", "")
		return usageDB, nil
	}
	rawGroup := map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,model,provider,mpc"},
	}
	cmd, buf := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd, []string{"20260709"}, viewDefault, loadWithRaw(rawGroup, nil), open); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "采集异常"); n != 1 {
		t.Errorf("四张表后应只输出一次异常警告,实际 %d:\n%s", n, out)
	}
	if n := strings.Count(out, "boom"); n != 1 {
		t.Errorf("异常详情应只出现一次,实际 %d:\n%s", n, out)
	}
	// 警告在所有表之后。
	if strings.Index(out, "自定义视图 mpc") > strings.Index(out, "采集异常") {
		t.Errorf("警告应在全部表完成后输出:\n%s", out)
	}
}

// default/custom 的 provider 别名与分组视图一致生效。
func TestRunQuery_DefaultAliasesApplied(t *testing.T) {
	open := func(p string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		if _, err := usageDB.ExecContext(context.Background(), `
INSERT INTO messages (id, session_id, client, date, ts, model, provider, router_provider, total_tokens)
VALUES ('m1', 's', 'claude', '2026-07-09', 0, 'mod', 'source-a', '', 100),
       ('m2', 's', 'claude', '2026-07-09', 0, 'mod', 'x', 'router-b', 200)`); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	rawCustom := map[string]any{
		"default":    "mpc",
		"subqueries": map[string]any{"mpc": "model,provider"},
	}
	cmd, buf := newQueryOutputCmd()
	load := func() (*config.Config, error) {
		return &config.Config{
			DataDir:         "/mem",
			RawQuery:        rawCustom,
			ProviderAliases: map[string]string{"source-a": "Merged provider", "router-b": "Merged provider"},
		}, nil
	}
	if err := runQueryWithDeps(cmd, []string{"20260709"}, viewDefault, load, open); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "Merged provider") != 1 {
		t.Errorf("default 自定义视图的 provider 别名应合并:\n%s", out)
	}
	if strings.Contains(out, "source-a") || strings.Contains(out, "router-b") {
		t.Errorf("别名合并后不得残留原始标签:\n%s", out)
	}
}

// ---- 统一统计信息区（范围 / 数据截至 / 最近成功采集） ----

// TestQueryStatisticsHeader_Rendering 锁定信息区文案与三类边界形态:
// 标题不带日期、单日只显示该日、闭区间显示 a ~ b、两项时间缺数据时显示 em dash。
func TestQueryStatisticsHeader_Rendering(t *testing.T) {
	ts := time.Date(2026, 8, 9, 12, 34, 56, 0, time.UTC)
	collected := time.Date(2026, 8, 20, 9, 30, 45, 0, time.UTC)

	cases := []struct {
		name          string
		first, last   string
		fresh         querier.Freshness
		wantContains  []string
		wantNoContain []string
	}{
		{
			name:  "single day",
			first: "2026-08-09", last: "2026-08-09",
			fresh: querier.Freshness{MaxMessageTS: ts.UnixMilli(), LastCollection: collected.Local()},
			wantContains: []string{
				"Usage statistics / 使用统计\n",
				"Query range / 统计范围: 2026-08-09\n",
				"Data through / 数据截至: " + ts.Local().Format(time.DateTime) + "\n",
				"Last successful collection / 最近成功采集: " + collected.Local().Format(time.DateTime) + "\n",
			},
			wantNoContain: []string{"~"},
		},
		{
			name:  "closed range shows both endpoints once",
			first: "2026-08-01", last: "2026-08-07",
			fresh:         querier.Freshness{},
			wantContains:  []string{"Query range / 统计范围: 2026-08-01 ~ 2026-08-07\n", "—"},
			wantNoContain: []string{"0001-01-01"},
		},
		{
			name:  "no data and no collection log show em dash",
			first: "2026-08-09", last: "2026-08-09",
			fresh: querier.Freshness{},
			wantContains: []string{
				"Data through / 数据截至: —\n",
				"Last successful collection / 最近成功采集: —\n",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := queryStatisticsHeader(c.first, c.last, c.fresh)
			for _, want := range c.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("header should contain %q:\ngot: %q", want, got)
				}
			}
			for _, no := range c.wantNoContain {
				if strings.Contains(got, no) {
					t.Errorf("header should not contain %q:\ngot: %q", no, got)
				}
			}
		})
	}

	// 信息区以标题开头,恰好八行内容(标题 + Units 标签 + 三行换算说明 + 三个信息行) + 一个分隔空行。
	got := queryStatisticsHeader("2026-08-01", "2026-08-07", querier.Freshness{})
	if !strings.HasPrefix(got, "Usage statistics / 使用统计\n") {
		t.Errorf("header must start with the fixed title line, got %q", got)
	}
	if lines := strings.Split(strings.TrimRight(got, "\n"), "\n"); len(lines) != 8 {
		t.Errorf("header body must be exactly 8 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("header must end with a blank separator line before the tables, got %q", got)
	}
	// Units 换算说明是面向用户的固定说明文案,整个四行块精确锁定,避免部分改动漏行或漂移;
	// 且必须位于标题之后、Query range 之前。
	unitsBlock := "Units / 单位:\n" +
		"  1 K = 1,000 (thousand / 一千)\n" +
		"  1 M = 1,000 K = 1,000,000 (million / 一百万)\n" +
		"  1 B = 1,000 M = 1,000,000,000 (billion / 十亿)\n"
	unitsIdx := strings.Index(got, unitsBlock)
	rangeIdx := strings.Index(got, "Query range / 统计范围:")
	if unitsIdx < 0 {
		t.Errorf("header must contain the exact units block, got %q", got)
	}
	if unitsIdx < 0 || rangeIdx < 0 || unitsIdx > rangeIdx {
		t.Errorf("units block must sit between the title and the query range line, got %q", got)
	}
}

// TestRunQuery_StatisticsHeaderOnBuiltinView 断言内置视图输出以统一信息区开始且只出现一次。
func TestRunQuery_StatisticsHeaderOnBuiltinView(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := insertOneMessage(usageDB, "2026-06-09", "claude"); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := executeQueryDates(context.Background(), buf, usageDB, []string{"2026-06-09"}, viewClient); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	const title = "Usage statistics / 使用统计"
	if !strings.HasPrefix(out, title+"\n") {
		t.Errorf("output must start with the statistics header:\n%s", out)
	}
	if n := strings.Count(out, title); n != 1 {
		t.Errorf("statistics header must appear exactly once, got %d:\n%s", n, out)
	}
	for _, label := range []string{"Query range / 统计范围:", "Data through / 数据截至:", "Last successful collection / 最近成功采集:"} {
		if n := strings.Count(out, label); n != 1 {
			t.Errorf("label %q must appear exactly once, got %d:\n%s", label, n, out)
		}
	}
	// 空库场景:既无消息也无采集记录,两项时间均为 em dash。
	if !strings.Contains(out, "Data through / 数据截至: —") ||
		!strings.Contains(out, "Last successful collection / 最近成功采集: —") {
		t.Errorf("empty database should show em dashes for both time fields:\n%s", out)
	}
	// 范围行为查询的实际单日参数。
	if !strings.Contains(out, "Query range / 统计范围: 2026-06-09\n") {
		t.Errorf("range line should echo the queried single date:\n%s", out)
	}
}

// TestRunQuery_StatisticsHeaderOnceForGroup 断言组合多表输出中信息区只在最开头出现一次。
func TestRunQuery_StatisticsHeaderOnceForGroup(t *testing.T) {
	rawGroup := map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,model,provider,mpc"},
	}
	cmd, buf := newQueryOutputCmd()
	if err := runQueryCustomWithDeps(cmd, "group_q", []string{"20260709"}, loadWithRaw(rawGroup, nil), memOpen(t)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	const title = "Usage statistics / 使用统计"
	if n := strings.Count(out, title); n != 1 {
		t.Errorf("group output must print the header exactly once (not per table), got %d:\n%s", n, out)
	}
	if idx := strings.Index(out, title); idx > strings.Index(out, "按客户端分组") {
		t.Errorf("header must precede the first table:\n%s", out)
	}
	if !strings.HasPrefix(out, title+"\n") {
		t.Errorf("group output must start with the statistics header:\n%s", out)
	}
}

// TestRunQuery_DataThroughFromRealTimestamps 断言数据截至取范围内最大非零毫秒 ts 并按本机时区显示到秒。
func TestRunQuery_DataThroughFromRealTimestamps(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	maxTS := time.Date(2026, 7, 9, 23, 59, 59, 0, time.UTC)
	msgs := []model.Message{
		{ID: "m-low", SessionID: "s", Client: "claude", Date: "2026-07-09", TS: time.Date(2026, 7, 9, 8, 0, 0, 0, time.UTC).UnixMilli()},
		{ID: "m-max", SessionID: "s", Client: "claude", Date: "2026-07-09", TS: maxTS.UnixMilli()},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := executeQueryDates(context.Background(), buf, usageDB, []string{"2026-07-09"}, viewClient); err != nil {
		t.Fatal(err)
	}
	wantThrough := "Data through / 数据截至: " + maxTS.Local().Format(time.DateTime) + "\n"
	if !strings.Contains(buf.String(), wantThrough) {
		t.Errorf("data-through should show the max in-range ts in local seconds:\nwant %q\ngot %s", wantThrough, buf.String())
	}
}

// TestRunQuery_LastCollectionShownInLocalTime 断言最近成功采集按本机时区展示 UTC 的 collected_at。
func TestRunQuery_LastCollectionShownInLocalTime(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := insertOneMessage(usageDB, "2026-07-09", "claude"); err != nil {
		t.Fatal(err)
	}
	insertCollectionLogAt(t, usageDB, "2026-07-09", "claude", "2026-07-10 02:00:00")
	wantUTC := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)

	buf := &bytes.Buffer{}
	if err := executeQueryDates(context.Background(), buf, usageDB, []string{"2026-07-09"}, viewClient); err != nil {
		t.Fatal(err)
	}
	wantColl := "Last successful collection / 最近成功采集: " + wantUTC.Local().Format(time.DateTime) + "\n"
	if !strings.Contains(buf.String(), wantColl) {
		t.Errorf("last collection should convert UTC text to local timezone:\nwant %q\ngot %s", wantColl, buf.String())
	}
}

// insertCollectionLogAt 以固定 UTC 文本写入 collection_log(collected_at 由 SQLite 默认生成,需手工指定才能锁定转换)。
func insertCollectionLogAt(t *testing.T, usageDB *db.DB, date, source, collectedAtUTC string) {
	t.Helper()
	if _, err := usageDB.ExecContext(context.Background(),
		`INSERT INTO collection_log (date, source, session_count, collected_at) VALUES (?, ?, 1, ?)`,
		date, source, collectedAtUTC); err != nil {
		t.Fatal(err)
	}
}

// TestRunQuery_SummarySingleRangeLine 断言 summary 的统计范围只由统一信息区承载:
// 不再保留旧 Date range 行,单日也不出现 a ~ a 重复形态。
func TestRunQuery_SummarySingleRangeLine(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	if err := insertOneMessage(usageDB, "2026-07-09", "claude"); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	if err := executeQueryDates(context.Background(), buf, usageDB, []string{"2026-07-09"}, viewSummary); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "Query range / 统计范围:"); n != 1 {
		t.Errorf("summary must show the range exactly once via the header, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Query range / 统计范围: 2026-07-09\n") {
		t.Errorf("single-day summary range should show the day alone:\n%s", out)
	}
	if strings.Contains(out, "Date range / 日期范围") {
		t.Errorf("summary must not keep the legacy Date range line:\n%s", out)
	}
	if strings.Contains(out, "2026-07-09 ~ 2026-07-09") {
		t.Errorf("single-day summary must not render a same-day a ~ a range:\n%s", out)
	}

	buf2 := &bytes.Buffer{}
	if err := executeQueryDates(context.Background(), buf2, usageDB, []string{"2026-07-08", "2026-07-09"}, viewSummary); err != nil {
		t.Fatal(err)
	}
	out2 := buf2.String()
	if strings.Count(out2, "2026-07-08 ~ 2026-07-09") != 1 {
		t.Errorf("range summary should carry both endpoints exactly once in the header:\n%s", out2)
	}
}

// ---- 根命令快捷分派(query <name> [date])与共用具名执行链 ----

// parseQueryInvocation 表驱动合同:
//   - 零参数/合法单日期或区间 → 默认目标;
//   - 非数字首字符单参数 → 具名目标 + 今天;
//   - 名称加合法日期/区间 → 具名目标 + 解析后的日期列表;
//   - 数字开头单参数非法日期 → 复用既有单参数日期错误;
//   - 两参数数字首参数(第二参数无论合法与否) → 固定优先报“此位置须为视图名称”
//     的双语用法错误,不再检查第二参数;
//   - 合法名称加非法日期 → 复用单元素 parseDateArgs 的日期形态错误,
//     绝不出现“仅接受 0 或 1 个位置参数”的参数个数错误。
func TestParseQueryInvocation(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	cases := []struct {
		name      string
		args      []string
		wantNamed bool
		wantName  string
		wantDates []string
		wantErr   []string // 非空时期望错误包含这些子串
		wantNoErr []string // 错误不得包含这些子串
	}{
		{
			name:      "zero args selects default and today",
			args:      nil,
			wantDates: []string{today},
		},
		{
			name:      "single legal date selects default",
			args:      []string{"20260709"},
			wantDates: []string{"2026-07-09"},
		},
		{
			name:      "single legal range selects default",
			args:      []string{"20260709-20260711"},
			wantDates: []string{"2026-07-09", "2026-07-10", "2026-07-11"},
		},
		{
			name:      "non-digit single arg is a view name with today",
			args:      []string{"mpc"},
			wantNamed: true,
			wantName:  "mpc",
			wantDates: []string{today},
		},
		{
			name:      "name plus date selects named target",
			args:      []string{"group_q", "20260709"},
			wantNamed: true,
			wantName:  "group_q",
			wantDates: []string{"2026-07-09"},
		},
		{
			name:      "name plus range selects named target",
			args:      []string{"mpc", "20260709-20260710"},
			wantNamed: true,
			wantName:  "mpc",
			wantDates: []string{"2026-07-09", "2026-07-10"},
		},
		{
			name:      "digit-leading invalid single date reuses existing date error",
			args:      []string{"20260132"},
			wantErr:   []string{"invalid date args", `"20260132"`, "/"},
			wantNoErr: []string{"expected 0 or 1 positional arg"},
		},
		{
			name:      "two args digit-first rejects first as needing a name (legal second)",
			args:      []string{"20260709", "20260710"},
			wantErr:   []string{"view name", "/", "token-usage query <name>", "/"},
			wantNoErr: []string{"expected 0 or 1 positional arg", "unknown query view"},
		},
		{
			name:      "two args digit-first rejects without checking second arg",
			args:      []string{"20260709", "notadate"},
			wantErr:   []string{"view name", "/"},
			wantNoErr: []string{"notadate", "expected 0 or 1 positional arg"},
		},
		{
			name:      "name plus bad date reuses single-element date form error",
			args:      []string{"mpc", "notadate"},
			wantErr:   []string{"invalid date args", `"notadate"`, "/"},
			wantNoErr: []string{"expected 0 or 1 positional arg"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := parseQueryInvocation(tc.args)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("应报错,得到 %+v", inv)
				}
				msg := err.Error()
				for _, want := range tc.wantErr {
					if !strings.Contains(msg, want) {
						t.Errorf("错误 %q 未包含 %q", msg, want)
					}
				}
				// 反向锚定优先级合同:这些子串不得出现在错误中。
				for _, no := range tc.wantNoErr {
					if strings.Contains(msg, no) {
						t.Errorf("错误不应包含 %q: %q", no, msg)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQueryInvocation(%v): %v", tc.args, err)
			}
			if inv.named != tc.wantNamed || inv.name != tc.wantName {
				t.Errorf("target = (%v,%q), want (%v,%q)", inv.named, inv.name, tc.wantNamed, tc.wantName)
			}
			if strings.Join(inv.dates, ",") != strings.Join(tc.wantDates, ",") {
				t.Errorf("dates = %v, want %v", inv.dates, tc.wantDates)
			}
		})
	}

	// 三项及以上同样由纯分派函数给出同一份用法错误(Args 校验的第一道防线)。
	_, err := parseQueryInvocation([]string{"a", "b", "c"})
	if err == nil || !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "/") {
		t.Errorf("三个参数应报双语超参用法错误: %v", err)
	}
}

// newQueryCmdWithDeps 构造可注入配置加载与 DB 打开的 query 根命令(测试接线);
// 生产入口 newQueryCmd 固定传入 loadConfig 与 dbOpener。
func newQueryOutputCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) (*cobra.Command, *bytes.Buffer) {
	cmd := newQueryCmdWithDeps(load, open)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// openWithWarning 打开内存库、写入两条消息并为 2026-07-09 记录一条采集异常,
// 使每次具名执行都覆盖统计信息区、表格与末尾异常告警三段输出。
func openWithWarning(t *testing.T) func(string) (*db.DB, error) {
	t.Helper()
	base := memOpen(t)
	return func(path string) (*db.DB, error) {
		usageDB, err := base(path)
		if err != nil {
			return nil, err
		}
		db.RecordError(context.Background(), usageDB, "2026-07-09", "claude", "boom", "")
		return usageDB, nil
	}
}

// 直接写法与 custom 写法对一个子查询和一个组合查询在无日期、单日、日期范围下
// 输出逐字相同;统计信息区恰好一次、异常告警只在末尾一次。
// custom 侧经由其入口管线 parseDateArgs("query custom"示例文案)+共用具名执行链,
// 与生产 RunE 同构且注入同一对 deps,保证输出可比、不触碰真实环境。
func TestRunQuery_DirectNameEquivalentToCustom(t *testing.T) {
	open := openWithWarning(t)
	rawGroup := map[string]any{
		"default":    "client",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,model,provider,mpc"},
	}
	load := loadWithRaw(rawGroup, nil)

	runDirect := func(args []string) string {
		t.Helper()
		cmd, buf := newQueryOutputCmdWithDeps(load, open)
		cmd.SetArgs(append([]string{}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("direct %v: %v", args, err)
		}
		return buf.String()
	}
	runCustom := func(args []string) string {
		t.Helper()
		var dateArgs []string
		if len(args) > 1 {
			dateArgs = args[1:]
		}
		cmd, buf := newQueryOutputCmd()
		if err := runQueryCustomWithDeps(cmd, args[0], dateArgs, load, open); err != nil {
			t.Fatalf("custom %v: %v", args, err)
		}
		return buf.String()
	}

	forms := [][]string{
		{"mpc"},
		{"mpc", "20260709"},
		{"mpc", "20260708-20260710"},
		{"group_q"},
		{"group_q", "20260709"},
		{"group_q", "20260708-20260710"},
	}
	for _, form := range forms {
		directOut := runDirect(form)
		customOut := runCustom(form)
		if directOut != customOut {
			t.Errorf("%v 两种写法输出不同:\ndirect:\n%s\ncustom:\n%s", form, directOut, customOut)
		}
		if n := strings.Count(directOut, "Usage statistics / 使用统计"); n != 1 {
			t.Errorf("%v 统计信息区应恰一次,实际 %d:\n%s", form, n, directOut)
		}
		// 今天形态的范围不含已记录异常的日期,告警按既有合同缺省;
		// 显式日期形态均覆盖 2026-07-09,应恰一次并出现在全部表之后。
		if len(form) == 1 {
			if strings.Count(directOut, "boom") != 0 || strings.Contains(directOut, "采集异常") {
				t.Errorf("%v 不应出现范围外告警:\n%s", form, directOut)
			}
			continue
		}
		if strings.Count(directOut, "boom") != 1 {
			t.Errorf("%v 异常告警应恰一次:\n%s", form, directOut)
		}
	}

	// 组合查询顺序与告警位置单独锚定(输出两侧逐字相等已隐含,这里显式给出诊断锚点)。
	out := runDirect([]string{"group_q", "20260709"})
	if !(strings.Index(out, "按客户端分组") < strings.Index(out, "按模型分组") &&
		strings.Index(out, "按模型分组") < strings.Index(out, "按供应商分组") &&
		strings.Index(out, "按供应商分组") < strings.Index(out, "自定义视图 mpc")) {
		t.Errorf("组合表未按声明顺序输出:\n%s", out)
	}
	if strings.Index(out, "自定义视图 mpc") > strings.Index(out, "boom") {
		t.Errorf("告警应在全部表之后:\n%s", out)
	}
}

// 注入依赖后的根命令 Execute:零参数与单日期形态仍真实执行 query.default。
func TestRunQuery_RootExecutesDefaultTarget(t *testing.T) {
	open := memOpen(t)
	rawDefault := map[string]any{
		"default":    "mpc",
		"subqueries": map[string]any{"mpc": "model,provider"},
	}
	load := loadWithRaw(rawDefault, nil)

	cmd, buf := newQueryOutputCmdWithDeps(load, open)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("零参数根命令: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "自定义视图 mpc") {
		t.Errorf("零参数应执行 default 指向的自定义视图:\n%s", out)
	}

	cmd2, buf2 := newQueryOutputCmdWithDeps(load, open)
	cmd2.SetArgs([]string{"20260709"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("单日期根命令: %v", err)
	}
	out2 := buf2.String()
	if !strings.Contains(out2, "自定义视图 mpc") {
		t.Errorf("单日期仍应执行 default 目标:\n%s", out2)
	}
	if !strings.Contains(out2, "Query range / 统计范围: 2026-07-09\n") {
		t.Errorf("日期应被解析并进入统计范围行:\n%s", out2)
	}
}

// 根命令各错误路径均在打开 DB 前失败;坏定义不阻断内置静态视图。
func TestRunQuery_RootErrorsBeforeDB(t *testing.T) {
	opens := 0
	countingOpen := func(p string) (*db.DB, error) {
		opens++
		return db.Open(":memory:")
	}
	assertNoOpen := func(t *testing.T, stage string) {
		t.Helper()
		if opens != 0 {
			t.Fatalf("%s 不应打开 DB,实际 %d 次", stage, opens)
		}
	}

	run := func(args ...string) error {
		t.Helper()
		cmd, _ := newQueryOutputCmdWithDeps(loadWithRaw(nil, nil), countingOpen)
		cmd.SetArgs(args)
		err := cmd.Execute()
		assertNoOpen(t, fmt.Sprintf("%v", args))
		return err
	}

	// 超参与数字首参数两参数形态。
	err := run("a", "b", "c")
	if err == nil || !strings.Contains(err.Error(), "/") {
		t.Errorf("超参应报双语用法错误: %v", err)
	}
	err = run("20260709", "20260710")
	if err == nil || !strings.Contains(err.Error(), "view name") {
		t.Errorf("数字首参数两参数应报须为视图名称: %v", err)
	}
	// 非法第二日期。
	err = run("mpc", "20260132")
	if err == nil || !strings.Contains(err.Error(), "20260132") {
		t.Errorf("非法第二日期应报日期形态错误: %v", err)
	}
	// 未知具名视图。
	rawSub := map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}}
	unkCmd, _ := newQueryOutputCmdWithDeps(loadWithRaw(rawSub, nil), countingOpen)
	unkCmd.SetArgs([]string{"ghost"})
	err = unkCmd.Execute()
	assertNoOpen(t, "未知名称")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("未知名称应报未知查询视图: %v", err)
	}
	// CSV 错误。
	badCSV := loadWithRaw(map[string]any{"subqueries": map[string]any{"mpc": "model,"}}, nil)
	csvCmd, _ := newQueryOutputCmdWithDeps(badCSV, countingOpen)
	csvCmd.SetArgs([]string{"mpc"})
	err = csvCmd.Execute()
	assertNoOpen(t, "CSV 错误")
	if err == nil || !strings.Contains(err.Error(), "query.subqueries.mpc") {
		t.Errorf("CSV 错误应定位键: %v", err)
	}
	// 断开引用。
	brokenRef := loadWithRaw(map[string]any{"groups": map[string]any{"g": "client,nope"}}, nil)
	refCmd, _ := newQueryOutputCmdWithDeps(brokenRef, countingOpen)
	refCmd.SetArgs([]string{"g"})
	err = refCmd.Execute()
	assertNoOpen(t, "断开引用")
	if err == nil || !strings.Contains(err.Error(), "query.groups.g") {
		t.Errorf("断开引用应定位键: %v", err)
	}
	// RawQuery 顶层问题。
	issues := map[string]config.RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}
	issueCmd, _ := newQueryOutputCmdWithDeps(loadWithRaw(nil, issues), countingOpen)
	issueCmd.SetArgs([]string{"g"})
	err = issueCmd.Execute()
	assertNoOpen(t, "顶层问题")
	if err == nil || !strings.Contains(err.Error(), "Query") {
		t.Errorf("顶层问题应在具名路径拒绝并定位: %v", err)
	}

	// 坏 query 定义不阻断六个内置静态视图(内置子命令经 runQueryWithDeps 保持既有路径)。
	for _, v := range []queryView{viewClient, viewModel, viewProvider, viewProject, viewSessions, viewSummary} {
		cmdB, _ := newQueryOutputCmd()
		if err := runQueryWithDeps(cmdB, nil, v, loadWithRaw(nil, issues), memOpen(t)); err != nil {
			t.Errorf("内置视图 %d 受坏配置阻断: %v", v, err)
		}
	}
}

// custom 的日期错误示例必须指向真实存在的命令形态:
// "token-usage query custom 20260701",不得出现不存在的裸 "token-usage custom"。
func TestRunQueryCustom_DateErrorExampleIsValidCommand(t *testing.T) {
	open := memOpen(t)
	run := func(dateArgs ...string) string {
		t.Helper()
		cmd, _ := newQueryOutputCmd()
		err := runQueryCustomWithDeps(cmd, "mpc", dateArgs, loadWithRaw(nil, nil), open)
		if err == nil {
			t.Fatalf("custom %v 应报日期错误", dateArgs)
		}
		return err.Error()
	}
	for _, dc := range [][]string{{"notadate"}, {"20260709-20260732"}} {
		msg := run(dc...)
		if !strings.Contains(msg, "token-usage query custom 20260701") {
			t.Errorf("日期错误示例应为有效命令 token-usage query custom 20260701: %q", msg)
		}
		// 有效示例是 "token-usage query custom ...",两者之间不可能出现
		// 紧邻的 "token-usage custom",该子串即代表无效旧示例。
		if strings.Contains(msg, "token-usage custom") {
			t.Errorf("不得出现不存在的裸 token-usage custom 示例: %q", msg)
		}
	}
}

// Cobra 级路由:静态子命令优先命中;非静态首参数留在根命令做名称分派;
// 用户定义名不会被动态注册进命令树。
func TestRunQuery_StaticRoutingAndTreeStable(t *testing.T) {
	root := newQueryCmd()
	if got, _, err := root.Find([]string{"custom"}); err != nil || got.Name() != "custom" {
		t.Errorf("custom 应由静态子命令命中: %v %q", err, got.Name())
	}
	if got, _, err := root.Find([]string{"list"}); err != nil || got.Name() != "list" {
		t.Errorf("list 应由静态子命令命中: %v %q", err, got.Name())
	}
	if got, _, err := root.Find([]string{"mpc"}); err != nil || got.Name() != "query" {
		t.Errorf("非静态首参数应由根命令处理: %v %q", err, got.Name())
	}
	names := map[string]bool{}
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"client", "model", "provider", "project", "session", "summary", "custom", "list"} {
		if !names[want] {
			t.Errorf("缺少静态子命令 %q", want)
		}
	}
	if len(root.Commands()) != 8 {
		t.Errorf("静态命令树应恰为 8 个子命令: %v", root.Commands())
	}

	// 执行过具名查询后命令树不变:配置中的名称不会注册为动态子命令。
	open := memOpen(t)
	rawSub := map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}}
	cmd, buf := newQueryOutputCmdWithDeps(loadWithRaw(rawSub, nil), open)
	cmd.SetArgs([]string{"mpc"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "自定义视图 mpc") {
		t.Fatalf("具名直接写法应成功执行:\n%s", buf.String())
	}
	// Execute 会为独立执行的根命令自动补挂 help/completion;
	// 树稳定性以行为合同断言:mpc 未被注册,仍路由回 query 根命令做名称分派。
	for _, sub := range cmd.Commands() {
		if sub.Name() == "mpc" {
			t.Errorf("配置名 %q 被动态注册为子命令", sub.Name())
		}
	}
	if got, _, err := cmd.Find([]string{"mpc"}); err != nil || got.Name() != "query" {
		t.Errorf("执行后非静态首参数仍应由根命令处理: %v %q", err, got.Name())
	}
}

// ---- query list 只读发现 ----

// writeConfigForHome 在给定 home 下写入 .token-usage/config.toml。
func writeConfigForHome(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 静态帮助合同:query -h 在无配置、有效配置与坏 query 配置三态下输出
// 完全一致;测试用 t.Setenv 同时隔离 HOME 与 USERPROFILE 到临时目录,
// 不读取开发机真实配置;帮助生成不触发配置加载语义上的差异。
func TestNewQueryCmd_StaticHelpAcrossConfigStates(t *testing.T) {
	helpSnapshot := func(home string) string {
		t.Helper()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		cmd := newQueryCmd()
		buf := &bytes.Buffer{}
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{"--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("query --help: %v", err)
		}
		return buf.String()
	}

	states := []struct {
		name    string
		content string // 空串表示不创建配置文件
	}{
		{name: "no config", content: ""},
		{name: "valid config", content: config.DefaultConfigTemplate()},
		{name: "broken config", content: "[query\nfoo="},
	}
	var baseline string
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			home := t.TempDir()
			if st.content != "" {
				writeConfigForHome(t, home, st.content)
			}
			got := helpSnapshot(home)
			if baseline == "" {
				baseline = got
				return
			}
			if got != baseline {
				t.Errorf("帮助文本随配置状态变化:\n--- first ---\n%s\n--- %s ---\n%s", baseline, st.name, got)
			}
		})
	}
}

// list 命令树与参数合同:Cobra 静态路由命中(不进入根命令名称分派——若落入
// 根分派,list 会被当作视图名而报未知视图);零参数成功;一/多个位置参数在
// 配置加载前返回双语错误(非 cobra.NoArgs 英文默认文本)。
func TestRunQueryList_TreeRoutingAndArgs(t *testing.T) {
	cmd, buf := newQueryOutputCmdWithDeps(loadWithRaw(nil, nil), memOpen(t))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("query list 应由静态子命令命中并成功: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Configured query views / 已配置查询视图") {
		t.Fatalf("应输出 list 正文:\n%s", out)
	}
	if strings.Contains(out, "unknown query view") || strings.Contains(out, "未知的查询视图") {
		t.Fatalf("list 不应进入根命令名称分派:\n%s", out)
	}

	// 多余位置参数在配置加载前失败:list 子命令与根共用同一注入 loader,
	// 哨兵 loader 一旦被调用即记数;同时禁止 cobra.NoArgs 英文默认文案。
	loadCalls := 0
	sentinelLoad := func() (*config.Config, error) {
		loadCalls++
		return nil, errors.New("sentinel-load")
	}
	argsCmd, _ := newQueryOutputCmdWithDeps(sentinelLoad, memOpen(t))
	for _, extra := range [][]string{{"extra"}, {"20260709"}, {"a", "b"}} {
		full := append([]string{"list"}, extra...)
		argsCmd.SetArgs(full)
		err := argsCmd.Execute()
		if err == nil {
			t.Fatalf("%v 应拒绝", full)
		}
		msg := err.Error()
		if !strings.Contains(msg, "/") {
			t.Errorf("list 超参错误应为双语: %q", msg)
		}
		if !strings.Contains(msg, fmt.Sprintf("%d", len(extra))) {
			t.Errorf("超参错误应含实际数量 %d: %q", len(extra), msg)
		}
		for _, forbidden := range []string{
			"positional arguments were not accepted",
			"unknown command",
			"加载配置失败",
			"failed to load config",
			"sentinel-load",
		} {
			if strings.Contains(msg, forbidden) {
				t.Errorf("错误不得为 cobra.NoArgs 默认文案或已触发配置加载(%q): %q", forbidden, msg)
			}
		}
	}
	if loadCalls != 0 {
		t.Errorf("参数错误不得加载配置,loader 被调用 %d 次", loadCalls)
	}
}

// 默认行为四类状态:缺 [query]/空段/空白 default → built-in fallback / 内置回退;
// 显式内置 default → built-in view / 内置视图;显式子查询 → custom subquery / 自定义子查询;
// 显式组合 → group / 组合查询。名称与类别逐字断言。
func TestRunQueryList_DefaultBehaviorCategories(t *testing.T) {
	withSubGroup := map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}
	cases := []struct {
		name        string
		raw         map[string]any
		wantText    string // 默认行为整行形态前缀
		wantContext string // 类别双语全文
	}{
		{
			name:        "missing query section",
			raw:         nil,
			wantText:    "token-usage query -> client (",
			wantContext: "built-in fallback / 内置回退",
		},
		{
			name:        "empty query section",
			raw:         map[string]any{},
			wantText:    "token-usage query -> client (",
			wantContext: "built-in fallback / 内置回退",
		},
		{
			name:        "blank default",
			raw:         map[string]any{"default": "   "},
			wantText:    "token-usage query -> client (",
			wantContext: "built-in fallback / 内置回退",
		},
		{
			name:        "explicit builtin default",
			raw:         map[string]any{"default": "model"},
			wantText:    "token-usage query -> model (",
			wantContext: "built-in view / 内置视图",
		},
		{
			name:        "explicit subquery default",
			raw:         map[string]any{"default": "mpc", "subqueries": map[string]any{"mpc": "model,provider"}},
			wantText:    "token-usage query -> mpc (",
			wantContext: "custom subquery / 自定义子查询",
		},
		{
			name:        "explicit group default",
			raw:         withSubGroupAndDefault(withSubGroup, "g"),
			wantText:    "token-usage query -> g (",
			wantContext: "group / 组合查询",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			if err := runQueryListWithDeps(buf, loadWithRaw(tc.raw, nil)); err != nil {
				t.Fatalf("runQueryListWithDeps: %v", err)
			}
			out := buf.String()
			idx := strings.Index(out, "Default behavior / 默认行为")
			if idx < 0 || !strings.Contains(out[idx:], "\n"+tc.wantText+tc.wantContext+")") {
				t.Errorf("默认行为行应为 %s%s):\n%s", tc.wantText, tc.wantContext, out)
			}
		})
	}
}

func withSubGroupAndDefault(raw map[string]any, def string) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	out["default"] = def
	return out
}

// 固定输出结构:分区顺序恒为 标题→默认行为→调用说明→内置表→自定义子查询→组合查询;
// 内置表恰六行且用途逐字等于静态元数据 Short;调用说明各出现一次且声明等价;
// 每条配置只渲染一条不含 [date] 占位符的简写完整命令([date] 全文仅出现在两行说明中);
// custom/list 的 Short 不作为内置视图行出现;空分区显示 None / 无而非空表头;
// 成功输出不含统计信息区或采集异常提示。
func TestRunQueryList_OutputContract(t *testing.T) {
	raw := map[string]any{
		"default":    "grp_z",
		"subqueries": map[string]any{"zeta": "provider,model", "alpha": "model,project"},
		"groups":     map[string]any{"grp_z": "client,zeta", "grp_a": "model,provider"},
	}
	buf := &bytes.Buffer{}
	if err := runQueryListWithDeps(buf, loadWithRaw(raw, nil)); err != nil {
		t.Fatalf("runQueryListWithDeps: %v", err)
	}
	out := buf.String()

	markers := []string{
		"Configured query views / 已配置查询视图",
		"Default behavior / 默认行为",
		"Configured view invocation / 已配置视图调用",
		ui.Bi("Direct", "简写"),
		ui.Bi("Explicit", "显式"),
		"Built-in query commands / 内置查询命令",
		"Custom subqueries / 自定义子查询",
		"Groups / 组合查询",
	}
	last := -1
	for _, m := range markers {
		idx := strings.Index(out, m)
		if idx < 0 {
			t.Fatalf("缺分区标记 %q:\n%s", m, out)
		}
		if idx <= last {
			t.Errorf("分区顺序错误:%q 出现在前一标记之前\n%s", m, out)
		}
		last = idx
	}

	// 调用说明一次且声明等价。
	for _, want := range []string{
		ui.Bi("Direct", "简写") + ": token-usage query <name> [date]",
		ui.Bi("Explicit", "显式") + ": token-usage query custom <name> [date]",
	} {
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("调用说明 %q 应恰一次,实际 %d:\n%s", want, n, out)
		}
	}
	if !strings.Contains(out, "(equivalent / 等价)") {
		t.Errorf("显式说明应标注等价:\n%s", out)
	}
	// [date] 字面占位符只属于两行调用说明。
	if n := strings.Count(out, "[date]"); n != 2 {
		t.Errorf("[date] 应只出现在两行调用说明中,实际 %d 次:\n%s", n, out)
	}

	// 内置表:六行固定命令,用途等于元数据 Short。
	for _, meta := range queryBuiltinCmds {
		cell := "token-usage query " + meta.name
		if strings.Count(out, cell) != 1 {
			t.Errorf("内置行 %q 应恰一次:\n%s", cell, out)
		}
		if !strings.Contains(out, meta.short) {
			t.Errorf("内置用途文案应复用元数据 Short %q:\n%s", meta.short, out)
		}
	}
	// custom/list 不作为内置视图行出现(其 Short 在正文任何位置都不应渲染)。
	for _, forbidden := range []string{
		"Run a configured custom or group query",
		"List configured query views",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("固定操作入口不得作为内置视图行出现(%q):\n%s", forbidden, out)
		}
	}

	// 配置行的简写完整命令唯一且不含日期占位符。
	for _, name := range []string{"zeta", "alpha", "grp_z", "grp_a"} {
		cell := "token-usage query " + name
		if strings.Count(out, cell) != 1 {
			t.Errorf("配置行 %q 应恰一次:\n%s", cell, out)
		}
	}
	if strings.Count(out, "token-usage query client") != 1 ||
		strings.Count(out, "token-usage query model") != 1 {
		t.Errorf("内置行每条命令恰一次,配置行不得重复它们:\n%s", out)
	}

	// 名称字节序稳定(alpha<zeta, grp_a<grp_z),CSV 维持声明顺序。
	if !(strings.Index(out, "token-usage query alpha") < strings.Index(out, "token-usage query zeta")) {
		t.Errorf("子查询行应按名称字节序:\n%s", out)
	}
	if !(strings.Index(out, "token-usage query grp_a") < strings.Index(out, "token-usage query grp_z")) {
		t.Errorf("组合行应按名称字节序:\n%s", out)
	}
	if !strings.Contains(out, "provider,model") || strings.Contains(out, "model,provider,") {
		t.Errorf("维度 CSV 应保持声明顺序 provider,model(已 TrimSpace):\n%s", out)
	}
	if !strings.Contains(out, "client,zeta") || strings.Contains(out, ", zeta") || strings.Contains(out, "zeta, ") {
		t.Errorf("成员 CSV 应保持声明顺序 client,zeta(不夹带空格):\n%s", out)
	}

	// 无统计区/采集提示。
	for _, absent := range []string{"Usage statistics", "采集异常", "collection errors"} {
		if strings.Contains(out, absent) {
			t.Errorf("list 是只读发现命令,不应输出 %q:\n%s", absent, out)
		}
	}
}

// 空分区显示 None / 无而非空表头;存在内容时正文不出现 None。
func TestRunQueryList_EmptySectionsShowNone(t *testing.T) {
	buf := &bytes.Buffer{}
	if err := runQueryListWithDeps(buf, loadWithRaw(nil, nil)); err != nil {
		t.Fatalf("runQueryListWithDeps: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, ui.Bi("None", "无")); n != 2 {
		t.Errorf("两个空分区应各显示 None / 无,实际 %d 次:\n%s", n, out)
	}

	buf2 := &bytes.Buffer{}
	raw := map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}}
	if err := runQueryListWithDeps(buf2, loadWithRaw(raw, nil)); err != nil {
		t.Fatalf("runQueryListWithDeps: %v", err)
	}
	// 子查询有定义、组合为空时,仅组合分区恰好一个 None。
	if strings.Contains(buf2.String(), "Groups / 组合查询") &&
		strings.Count(buf2.String(), ui.Bi("None", "无")) != 1 {
		t.Errorf("仅空组合分区应有一个 None:\n%s", buf2.String())
	}
}

// 错误可定位:配置加载失败、CSV 错误、断开引用、顶层 RawQuery 问题均按既有
// 双语定位错误返回,绝不渲染成空列表(None 只表达「确实没有定义」)。
func TestRunQueryList_ErrorsAreLocalizable(t *testing.T) {
	failLoad := func() (*config.Config, error) { return nil, errors.New("boom-file") }
	buf := &bytes.Buffer{}
	err := runQueryListWithDeps(buf, failLoad)
	if err == nil || !strings.Contains(err.Error(), "加载配置失败") || !strings.Contains(err.Error(), "boom-file") {
		t.Fatalf("配置加载失败应包装双语错误并携带原因: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("错误路径不得输出正文:\n%s", buf.String())
	}

	cases := []struct {
		name string
		raw  map[string]any
		want []string
	}{
		{"csv error", map[string]any{"subqueries": map[string]any{"mpc": "model,"}}, []string{"query.subqueries.mpc"}},
		{"broken reference", map[string]any{"groups": map[string]any{"g": "client,nope"}}, []string{"query.groups.g", `"nope"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			err := runQueryListWithDeps(buf, loadWithRaw(tc.raw, nil))
			if err == nil {
				t.Fatal("应报错")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误未包含 %q: %q", w, err.Error())
				}
			}
			if strings.Contains(buf.String(), ui.Bi("None", "无")) {
				t.Errorf("坏定义不得渲染为 None:\n%s", buf.String())
			}
		})
	}

	t.Run("top-level issues", func(t *testing.T) {
		issues := map[string]config.RawQueryTopLevelIssue{
			"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
		}
		buf := &bytes.Buffer{}
		err := runQueryListWithDeps(buf, loadWithRaw(nil, issues))
		if err == nil || !strings.Contains(err.Error(), `"Query"`) || !strings.Contains(err.Error(), "name_conflict") {
			t.Fatalf("顶层问题应列名与类别: %v", err)
		}
		if buf.String() != "" {
			t.Errorf("错误路径不得输出正文:\n%s", buf.String())
		}
	})
}

// 零 DB 副作用:全局 dbOpener 替换为计数函数后,list 在有效定义下成功且计数为零;
// 数据目录指向不存在 usage.db 的临时目录同样成功;输出无统计区与采集提示。
func TestRunQueryList_NeverOpensDB(t *testing.T) {
	orig := dbOpener
	calls := 0
	dbOpener = func(path string) (*db.DB, error) {
		calls++
		return db.Open(":memory:")
	}
	defer func() { dbOpener = orig }()

	raw := map[string]any{
		"default":    "mpc",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}
	tmpData := t.TempDir() // 目录内不存在 usage.db
	load := func() (*config.Config, error) {
		return &config.Config{DataDir: tmpData, RawQuery: raw}, nil
	}
	cmd, buf := newQueryOutputCmdWithDeps(load, memOpen(t))
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("query list: %v", err)
	}
	out := buf.String()
	if calls != 0 {
		t.Errorf("全局 dbOpener 计数应为 0,实际 %d", calls)
	}
	for _, absent := range []string{"Usage statistics", "采集异常", "collection errors"} {
		if strings.Contains(out, absent) {
			t.Errorf("list 输出不应含 %q:\n%s", absent, out)
		}
	}
}
