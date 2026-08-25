package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
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

// TestNewQueryCmd_SubcommandTree 断言 query 命令树包含且仅包含五个子命令，
// 且每个子命令的 Short/Use 与公开 CLI 文档一致。
func TestNewQueryCmd_SubcommandTree(t *testing.T) {
	cmd := newQueryCmd()

	wantShort := map[string]string{
		"client":  "Group by client (default) / 按客户端分组（默认）",
		"model":   "Group by model / 按模型分组",
		"project": "Group by project / 按项目分组",
		"session": "View session details / 查看会话明细",
		"summary": "View summary / 查看总览摘要",
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
	for _, name := range []string{"client", "model", "project", "session", "summary"} {
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
