package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// newExportOutputCmdWithDeps 构造带输出捕获的 export 命令(供 deps 注入路径):
// 返回命令与 stdout/stderr 两个缓冲,分离验证「stdout 纯数据、stderr 警告」契约。
func newExportOutputCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := newExportCmdWithDeps(load, open)
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// openExportFixture 打开内存库并写入两条不同 client 的消息:Claude Code
// fresh=1500/total=2500,Codex fresh=500/total=700——数值跨过 1000 阈值,
// 可区分「原始整数」与 formatTokens 的 K 缩写形态。
func openExportFixture(t *testing.T) func(string) (*db.DB, error) {
	t.Helper()
	return func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		msgs := []model.Message{
			{ID: "exp-a", SessionID: "sess-a", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: 1,
				FreshInputTokens: 1500, OutputTokens: 10, TotalTokens: 2500},
			{ID: "exp-b", SessionID: "sess-b", Client: model.ClientCodexApp, Date: "2026-07-09", TS: 2,
				FreshInputTokens: 500, OutputTokens: 5, TotalTokens: 700},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
}

// TestExportCSVClientView CSV client 视图:表头行固定、数据行值为原始整数
// (不做 K/M 缩写)、不含总计行。
func TestExportCSVClientView(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), openExportFixture(t))
	cmd.SetArgs([]string{"client", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export client: %v", err)
	}
	got := out.String()
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", got)
	}
	wantHeader := []string{"client", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if len(records) != 3 {
		t.Fatalf("应恰为表头 + 2 数据行(无总计行),实际 %d 行:\n%s", len(records), got)
	}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("表头 = %v, want %v", records[0], wantHeader)
	}
	// 按 total 降序:Claude Code(2500) 在前,Codex(700) 在后。
	if records[1][0] != string(model.ClientClaudeCode) || records[1][7] != "2500" {
		t.Errorf("首数据行 = %v, want client=%q total=%q", records[1], model.ClientClaudeCode, "2500")
	}
	if records[2][0] != string(model.ClientCodexApp) || records[2][7] != "700" {
		t.Errorf("次数据行 = %v, want client=%q total=%q", records[2], model.ClientCodexApp, "700")
	}
	if records[1][2] != "1500" {
		t.Errorf("input 应为原始整数 1500,实际 %q", records[1][2])
	}
	// K/M 缩写形态不得出现在任何值中。
	if strings.Contains(got, " K") || strings.Contains(got, " M") {
		t.Errorf("导出值不得做 K/M 缩写:\n%s", got)
	}
}

// TestExportCSVSessionQuoting CSV session 视图引号转义:title 与 project 含
// 逗号与双引号,csv.Reader 读回后字段必须完整无损。
func TestExportCSVSessionQuoting(t *testing.T) {
	const wantProject = `proj, "with" comma`
	const wantTitle = `fix "login", retry`
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		ctx := context.Background()
		if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
			ID: "sess-quote", Client: model.ClientClaudeCode, Directory: "/w", Project: wantProject, Title: wantTitle,
		}}); err != nil {
			return nil, err
		}
		if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{{
			ID: "msg-quote", SessionID: "sess-quote", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1, TotalTokens: 42,
		}}); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"session", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export session: %v", err)
	}
	got := out.String()
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", got)
	}
	wantHeader := []string{"client", "project", "title", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if len(records) != 2 {
		t.Fatalf("应恰为表头 + 1 数据行,实际 %d 行:\n%s", len(records), got)
	}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("表头 = %v, want %v", records[0], wantHeader)
	}
	row := records[1]
	if row[0] != string(model.ClientClaudeCode) {
		t.Errorf("client = %q, want %q", row[0], model.ClientClaudeCode)
	}
	if row[1] != wantProject {
		t.Errorf("project 转义后应完整 = %q, want %q", row[1], wantProject)
	}
	if row[2] != wantTitle {
		t.Errorf("title 转义后应完整 = %q, want %q", row[2], wantTitle)
	}
	if row[9] != "42" {
		t.Errorf("total 应为原始整数 42,实际 %q", row[9])
	}
}

// TestExportJSONDayViewGapFill JSON day 视图:非连续日期夹具 + 缺口补零,
// 按日期升序,缺口日期整数字段为 JSON number 0,键名集合与 CSV 列名一致。
func TestExportJSONDayViewGapFill(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		msgs := []model.Message{
			{ID: "day-max", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 1,
				FreshInputTokens: 1000, TotalTokens: 1000},
			{ID: "day-small", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-05", TS: 2,
				TotalTokens: 5},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"day", "20260701-20260705", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export day json: %v", err)
	}
	got := out.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("JSON 输出应以尾随换行结束:\n%s", got)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("输出应为合法 JSON:\n%s", got)
	}
	wantKeys := map[string]bool{
		"date": true, "requests": true, "input": true, "output": true,
		"cache_read": true, "cache_create": true, "reasoning": true, "total": true,
	}
	wantDates := []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04", "2026-07-05"}
	if len(rows) != len(wantDates) {
		t.Fatalf("应恰 %d 行(缺口补零),实际 %d:\n%s", len(wantDates), len(rows), got)
	}
	for i, row := range rows {
		if len(row) != len(wantKeys) {
			t.Errorf("第 %d 行键数 = %d, want %d: %v", i, len(row), len(wantKeys), row)
		}
		for key := range row {
			if !wantKeys[key] {
				t.Errorf("第 %d 行出现意外键 %q: %v", i, key, row)
			}
		}
		date, _ := row["date"].(string)
		if date != wantDates[i] {
			t.Errorf("第 %d 行 date = %q, want %q(按日期升序)", i, date, wantDates[i])
		}
		// JSON number 断言:整数字段必须是数值类型而非字符串。
		total, ok := row["total"].(float64)
		if !ok {
			t.Fatalf("第 %d 行 total 应为 JSON number: %v", i, row)
		}
		if date == "2026-07-02" || date == "2026-07-03" || date == "2026-07-04" {
			if total != 0 {
				t.Errorf("缺口日期 %s 的 total 应为 0,实际 %v", date, total)
			}
			if requests, ok := row["requests"].(float64); !ok || requests != 0 {
				t.Errorf("缺口日期 %s 的 requests 应为 number 0: %v", date, row["requests"])
			}
		}
	}
	// 有数据日期的数值锚定:07-01 total=1000。
	if rows[0]["total"].(float64) != 1000 {
		t.Errorf("2026-07-01 total 应为 1000: %v", rows[0])
	}
}

// TestExportInvalidFormatRejectedBeforeOpen --format 非法值:在打开数据库之前
// 报双语错误(open 计数为 0)。
func TestExportInvalidFormatRejectedBeforeOpen(t *testing.T) {
	openCalls := 0
	open := func(string) (*db.DB, error) {
		openCalls++
		return nil, errors.New("must not open database")
	}
	cmd, _, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"client", "--format", "xml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("非法 --format 应报错")
	}
	msg := err.Error()
	for _, want := range []string{"csv", "json", "/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q(双语允许集合): %q", want, msg)
		}
	}
	if openCalls != 0 {
		t.Errorf("格式拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestExportUnknownViewRejectedBeforeOpen 未知视图:错误含允许集合且在开库前拒绝。
func TestExportUnknownViewRejectedBeforeOpen(t *testing.T) {
	openCalls := 0
	open := func(string) (*db.DB, error) {
		openCalls++
		return nil, errors.New("must not open database")
	}
	cmd, _, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("未知视图应报错")
	}
	msg := err.Error()
	for _, want := range []string{"client, model, provider, project, day, month, hour, session", "/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含允许集合 %q: %q", want, msg)
		}
	}
	if openCalls != 0 {
		t.Errorf("未知视图拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestExportDigitArgAsDateEqualsClientView 数字开头的单参数作为日期:
// `export 20260901` 与 `export client 20260901` 输出逐字一致(该日有数据,
// 对比两条含数据行的输出才有区分度)。
func TestExportDigitArgAsDateEqualsClientView(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
			ID: "sep-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-01", TS: 1, FreshInputTokens: 20, TotalTokens: 30,
		}}); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	run := func(args ...string) string {
		t.Helper()
		cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
		cmd.SetArgs(append([]string{}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("export %v: %v", args, err)
		}
		return out.String()
	}
	bare := run("20260901")
	named := run("client", "20260901")
	if bare == "" {
		t.Fatal("export 20260901 形态应产生输出")
	}
	if bare != named {
		t.Errorf("数字单参数应等价 client 视图该日导出:\nbare:\n%s\nnamed:\n%s", bare, named)
	}
	if !strings.Contains(bare, string(model.ClientClaudeCode)) {
		t.Errorf("对比输出应含数据行:\n%s", bare)
	}
}

// TestExportProviderAliasMergesRows provider 别名:cfg 别名使两行合并为一行
// (别名键),原始标签不残留。
func TestExportProviderAliasMergesRows(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		msgs := []model.Message{
			{ID: "pa-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: 1,
				Provider: "source-a", TotalTokens: 100},
			{ID: "pa-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-09", TS: 2,
				Provider: "x", RouterProvider: "router-b", TotalTokens: 200},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	load := func() (*config.Config, error) {
		return &config.Config{
			DataDir: "/mem",
			ProviderAliases: map[string]string{
				"source-a": "Merged provider",
				"router-b": "Merged provider",
			},
		}, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(load, open)
	cmd.SetArgs([]string{"provider", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export provider: %v", err)
	}
	got := out.String()
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", got)
	}
	if len(records) != 2 {
		t.Fatalf("别名合并后应恰为表头 + 1 数据行,实际 %d 行:\n%s", len(records), got)
	}
	if records[1][0] != "Merged provider" {
		t.Errorf("合并行键应为别名 %q,实际 %q", "Merged provider", records[1][0])
	}
	if records[1][7] != "300" {
		t.Errorf("合并行 total 应为 100+200=300,实际 %q", records[1][7])
	}
	if strings.Contains(got, "source-a") || strings.Contains(got, "router-b") {
		t.Errorf("别名合并后不得残留原始标签:\n%s", got)
	}
}

// TestExportWarningsGoToStderr 采集异常警告落 stderr 不落 stdout:
// stdout 是纯 CSV,stderr 含警告标记。
func TestExportWarningsGoToStderr(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
			ID: "warn-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1, TotalTokens: 10,
		}}); err != nil {
			return nil, err
		}
		if err := db.RecordError(context.Background(), usageDB, "2026-07-09", "claude", "boom", ""); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, errOut := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"client", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export: %v", err)
	}
	stdout := out.String()
	records, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("stdout 应为纯 CSV(无任何警告文本混入):\n%s", stdout)
	}
	if len(records) != 2 || records[0][0] != "client" {
		t.Errorf("stdout 应恰为表头 + 1 数据行:\n%s", stdout)
	}
	for _, absent := range []string{"采集异常", "collection errors", "boom", "⚠"} {
		if strings.Contains(stdout, absent) {
			t.Errorf("stdout 不得含警告文本 %q:\n%s", absent, stdout)
		}
	}
	if !strings.Contains(errOut.String(), "采集异常") {
		t.Errorf("stderr 应含采集异常警告:\n%s", errOut.String())
	}
}

// TestExportCSVSessionTitleNewlineQuoting session 视图 title 含换行符:
// csv.Writer 以引号包裹含换行字段,csv.Reader 读回后字段必须完整无损。
func TestExportCSVSessionTitleNewlineQuoting(t *testing.T) {
	const wantTitle = "first line\nsecond line"
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		ctx := context.Background()
		if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
			ID: "sess-nl", Client: model.ClientClaudeCode, Directory: "/w", Project: "proj-nl", Title: wantTitle,
		}}); err != nil {
			return nil, err
		}
		if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{{
			ID: "msg-nl", SessionID: "sess-nl", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1, TotalTokens: 7,
		}}); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"session", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export session: %v", err)
	}
	got := out.String()
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", got)
	}
	if len(records) != 2 {
		t.Fatalf("应恰为表头 + 1 数据行,实际 %d 行:\n%q", len(records), got)
	}
	if records[1][2] != wantTitle {
		t.Errorf("title 含换行读回应完整 = %q, want %q", records[1][2], wantTitle)
	}
}

// TestExportEmptyRangeOutputsHeaderOrEmptyArray 空数据合同:CSV 仅表头一行,
// JSON 输出恰为 "[]\n"。day 视图对合法日期区间恒做缺口补零(与 query day
// 行为一致),永远不会输出空行集,因此用不补零的 client 视图选无数据区间。
func TestExportEmptyRangeOutputsHeaderOrEmptyArray(t *testing.T) {
	emptyOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		return usageDB, nil
	}

	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), emptyOpen)
	cmd.SetArgs([]string{"client", "20990101-20990105"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export client csv: %v", err)
	}
	records, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", out.String())
	}
	if len(records) != 1 || records[0][0] != "client" {
		t.Errorf("空数据 CSV 应仅含表头行,实际 %v", records)
	}

	cmd2, out2, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), emptyOpen)
	cmd2.SetArgs([]string{"client", "20990101-20990105", "--format", "json"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("export client json: %v", err)
	}
	if got := out2.String(); got != "[]\n" {
		t.Errorf("空数据 JSON 应恰为 \"[]\\n\",实际 %q", got)
	}
}

// TestExportJSONSessionView session JSON 视图:键集合为 client/project/title
// 加固定指标列,整数为 JSON number,空 project 保留空串原值。
func TestExportJSONSessionView(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		ctx := context.Background()
		if _, err := db.UpsertSessionMeta(ctx, usageDB, []model.Session{{
			ID: "sess-js", Client: model.ClientClaudeCode, Directory: "/w", Project: "", Title: "json-title",
		}}); err != nil {
			return nil, err
		}
		if _, err := db.UpsertMessages(ctx, usageDB, []model.Message{{
			ID: "msg-js", SessionID: "sess-js", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1, FreshInputTokens: 20, TotalTokens: 30,
		}}); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"session", "20260709", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export session json: %v", err)
	}
	got := out.String()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("输出应为合法 JSON:\n%s", got)
	}
	if len(rows) != 1 {
		t.Fatalf("应恰 1 行,实际 %d:\n%s", len(rows), got)
	}
	row := rows[0]
	wantKeys := map[string]bool{
		"client": true, "project": true, "title": true,
		"requests": true, "input": true, "output": true, "cache_read": true,
		"cache_create": true, "reasoning": true, "total": true,
	}
	if len(row) != len(wantKeys) {
		t.Errorf("键数 = %d, want %d: %v", len(row), len(wantKeys), row)
	}
	for key := range row {
		if !wantKeys[key] {
			t.Errorf("出现意外键 %q: %v", key, row)
		}
	}
	if row["project"] != "" {
		t.Errorf("空 project 应保留空串原值,实际 %v", row["project"])
	}
	if row["title"] != "json-title" || row["client"] != string(model.ClientClaudeCode) {
		t.Errorf("client/title 应为字符串原值: %v", row)
	}
	// 整数字段必须是 JSON number 而非字符串。
	if requests, ok := row["requests"].(float64); !ok || requests != 1 {
		t.Errorf("requests 应为 number 1,实际 %v", row["requests"])
	}
	if total, ok := row["total"].(float64); !ok || total != 30 {
		t.Errorf("total 应为 number 30,实际 %v", row["total"])
	}
	if input, ok := row["input"].(float64); !ok || input != 20 {
		t.Errorf("input 应为 number 20,实际 %v", row["input"])
	}
}

// TestParseExportInvocation 位置参数分派合同:零参数 → client + 今天;
// 非数字单参数 → 该视图名 + 今天;两参数数字开头固定报「此位置须为视图名」
// 的双语用法错误,且不检查第二参数。
func TestParseExportInvocation(t *testing.T) {
	today := time.Now().Format("2006-01-02")

	// case 0:零参数 → client + today。
	inv, err := parseExportInvocation(nil)
	if err != nil {
		t.Fatalf("零参数不应报错: %v", err)
	}
	if inv.view != "client" || strings.Join(inv.dates, ",") != today {
		t.Errorf("零参数 = (%q,%v), want (client,[%s])", inv.view, inv.dates, today)
	}

	// case 1 非数字:视图名 + today。
	inv, err = parseExportInvocation([]string{"day"})
	if err != nil {
		t.Fatalf("非数字单参数不应报错: %v", err)
	}
	if inv.view != "day" || strings.Join(inv.dates, ",") != today {
		t.Errorf("非数字单参数 = (%q,%v), want (day,[%s])", inv.view, inv.dates, today)
	}

	// case 2 数字开头:固定优先报视图名错误,合法与非法第二参数都不再检查。
	for _, second := range []string{"20260710", "notadate"} {
		_, err := parseExportInvocation([]string{"20260709", second})
		if err == nil {
			t.Fatalf("数字开头两参数(第二参数 %q)应报错", second)
		}
		msg := err.Error()
		for _, want := range []string{"export view name", "token-usage export", "/"} {
			if !strings.Contains(msg, want) {
				t.Errorf("错误应含 %q: %q", want, msg)
			}
		}
		if strings.Contains(msg, second) {
			t.Errorf("错误不应检查第二参数(不得含 %q): %q", second, msg)
		}
	}
}

// TestExportCSVMonthView month 视图 CSV:表头为 month 前缀加固定指标列,
// 跨月数据行按时间升序,缺口月份按月前缀补零值行。
func TestExportCSVMonthView(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		msgs := []model.Message{
			{ID: "exm-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-08-15", TS: 1, TotalTokens: 1000},
			{ID: "exm-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-10-05", TS: 2, TotalTokens: 500},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"month", "20260801-20261030"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export month: %v", err)
	}
	got := out.String()
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", got)
	}
	wantHeader := []string{"month", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if len(records) != 4 {
		t.Fatalf("应恰为表头 + 3 月行(含缺口月补零),实际 %d 行:\n%s", len(records), got)
	}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("表头 = %v, want %v", records[0], wantHeader)
	}
	wantMonths := []string{"2026-08", "2026-09", "2026-10"}
	for i, month := range wantMonths {
		if records[i+1][0] != month {
			t.Errorf("第 %d 数据行键 = %q, want %q(按月升序)", i, records[i+1][0], month)
		}
	}
	if records[1][7] != "1000" || records[3][7] != "500" {
		t.Errorf("数据月 total 应为原始整数: %v", records[1:4])
	}
	// 缺口月 2026-09 整行零值。
	for j, want := range []string{"2026-09", "0", "0", "0", "0", "0", "0", "0"} {
		if records[2][j] != want {
			t.Errorf("缺口月行第 %d 列 = %q, want %q", j, records[2][j], want)
		}
	}
}

// TestExportJSONMonthViewKeys month 视图 JSON:键集合为 month 加固定指标列,
// 行按月升序,缺口月整数字段为 JSON number 0。
func TestExportJSONMonthViewKeys(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
			ID: "exm-j", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-15", TS: 1, TotalTokens: 1000,
		}}); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"month", "20260801-20261030", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export month json: %v", err)
	}
	got := out.String()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("输出应为合法 JSON:\n%s", got)
	}
	wantKeys := map[string]bool{
		"month": true, "requests": true, "input": true, "output": true,
		"cache_read": true, "cache_create": true, "reasoning": true, "total": true,
	}
	wantMonths := []string{"2026-08", "2026-09", "2026-10"}
	if len(rows) != len(wantMonths) {
		t.Fatalf("应恰 %d 行,实际 %d:\n%s", len(wantMonths), len(rows), got)
	}
	for i, row := range rows {
		if len(row) != len(wantKeys) {
			t.Errorf("第 %d 行键数 = %d, want %d: %v", i, len(row), len(wantKeys), row)
		}
		for key := range row {
			if !wantKeys[key] {
				t.Errorf("第 %d 行出现意外键 %q: %v", i, key, row)
			}
		}
		if month, _ := row["month"].(string); month != wantMonths[i] {
			t.Errorf("第 %d 行 month = %q, want %q(按月升序)", i, month, wantMonths[i])
		}
	}
	// 缺口月 2026-09 的整数字段为 number 0。
	if rows[1]["month"] != "2026-09" {
		t.Fatalf("第 2 行应为缺口月 2026-09: %v", rows[1])
	}
	for _, col := range []string{"requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"} {
		if v, ok := rows[1][col].(float64); !ok || v != 0 {
			t.Errorf("缺口月 %s 应为 number 0,实际 %v", col, rows[1][col])
		}
	}
	// 数据月数值锚定。
	if total, ok := rows[0]["total"].(float64); !ok || total != 1000 {
		t.Errorf("2026-08 total 应为 number 1000: %v", rows[0])
	}
}

// TestExportJSONHourView 导出 hour 视图:固定 24 小时刻度全量补零(与请求日期
// 范围无关),键列为 hour(显示形态 "HH:00"),数值为 JSON number。
func TestExportJSONHourView(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		// ts 按本机时区取 09:30 与 15:00,消息分别折入 "09:00"/"15:00" 行。
		msgs := []model.Message{
			{ID: "hour-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-01",
				TS: time.Date(2026, 7, 1, 9, 30, 0, 0, time.Local).UnixMilli(),
				FreshInputTokens: 1000, TotalTokens: 1000},
			{ID: "hour-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-01",
				TS: time.Date(2026, 7, 1, 15, 0, 0, 0, time.Local).UnixMilli(),
				TotalTokens: 5},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"hour", "20260701", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export hour json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil {
		t.Fatalf("输出应为合法 JSON:\n%s", out.String())
	}
	if len(rows) != 24 {
		t.Fatalf("应恰 24 行(整日固定刻度补零),实际 %d:\n%s", len(rows), out.String())
	}
	// 行序:00:00..23:00 升序;键列名为 hour。
	for i, row := range rows {
		wantHour := fmt.Sprintf("%02d:00", i)
		got, _ := row["hour"].(string)
		if got != wantHour {
			t.Fatalf("第 %d 行 hour = %q, want %q(按小时升序): %v", i, got, wantHour, row)
		}
		total, ok := row["total"].(float64)
		if !ok {
			t.Fatalf("第 %d 行 total 应为 JSON number: %v", i, row)
		}
		switch wantHour {
		case "09:00":
			if total != 1000 {
				t.Errorf("09:00 total 应为 1000,实际 %v", total)
			}
		case "15:00":
			if total != 5 {
				t.Errorf("15:00 total 应为 5,实际 %v", total)
			}
		default:
			if total != 0 {
				t.Errorf("无数据小时 %s 的 total 应为 0,实际 %v", wantHour, total)
			}
		}
	}
}
