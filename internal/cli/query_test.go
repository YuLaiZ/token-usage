package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
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

// TestNewQueryCmd_BareQueryAcceptsDateArg 断言裸 query 仍接受 0 或 1 个位置参数。
func TestNewQueryCmd_BareQueryAcceptsDateArg(t *testing.T) {
	cmd := newQueryCmd()
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("裸 query 0 参数应通过: %v", err)
	}
	if err := cmd.Args(cmd, []string{"20260101"}); err != nil {
		t.Errorf("裸 query 1 参数应通过: %v", err)
	}
	if err := cmd.Args(cmd, []string{"20260101", "20260102"}); err == nil {
		t.Error("裸 query 2 参数应报错")
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
	})
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

// issues、CSV 错误、断开引用只挡裸 query/custom;内置显式子命令保持可用。
func TestRunQuery_BadQueryConfigOnlyBlocksDefaultAndCustom(t *testing.T) {
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
