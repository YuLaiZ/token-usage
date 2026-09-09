package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
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
	wantHeader := []string{"client", "project", "title", "duration_ms", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
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
	if row[3] != "0" {
		t.Errorf("duration_ms 应为原始整数 0(夹具单条消息,首末时间戳相同),实际 %q", row[3])
	}
	if row[10] != "42" {
		t.Errorf("total 应为原始整数 42,实际 %q", row[10])
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
	for _, want := range []string{"client, model, provider, project, day, month, hour, weekday, session", "/"} {
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
// 对比两条含数据行的输出才有区分度;夹具无 query 配置,缺省视图回退 client,
// 因此与显式 client 视图一致)。
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
		"client": true, "project": true, "title": true, "duration_ms": true,
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
	// duration_ms 是首末消息毫秒差,夹具单条消息恒为 0 且为 JSON number。
	if got, ok := row["duration_ms"].(float64); !ok || got != 0 {
		t.Errorf("duration_ms 应为 JSON number 0: %v", row["duration_ms"])
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

// TestParseExportInvocation 位置参数分派合同:零参数与数字单参数 → 缺省视图
// (view 为空串,执行 query.default,内置回退 client 由执行层兑现);
// 非数字单参数 → 该视图名 + 今天;两参数数字开头固定报「此位置须为视图名」
// 的双语用法错误,且不检查第二参数。
func TestParseExportInvocation(t *testing.T) {
	today := time.Now().Format("2006-01-02")

	// case 0:零参数 → 缺省视图 + today。
	inv, err := parseExportInvocation(nil)
	if err != nil {
		t.Fatalf("零参数不应报错: %v", err)
	}
	if inv.view != "" || strings.Join(inv.dates, ",") != today {
		t.Errorf("零参数 = (%q,%v), want (\"\",[%s])", inv.view, inv.dates, today)
	}

	// case 1 显式空串:named 哨兵置位,视图名保留空串(执行层按未知名拒绝)。
	inv, err = parseExportInvocation([]string{""})
	if err != nil {
		t.Fatalf("显式空串分派不应报错: %v", err)
	}
	if !inv.named || inv.view != "" {
		t.Errorf("显式空串 = (named=%v,view=%q), want (true,\"\")", inv.named, inv.view)
	}

	// case 1 非数字:视图名 + today。
	inv, err = parseExportInvocation([]string{"day"})
	if err != nil {
		t.Fatalf("非数字单参数不应报错: %v", err)
	}
	if inv.view != "day" || strings.Join(inv.dates, ",") != today {
		t.Errorf("非数字单参数 = (%q,%v), want (day,[%s])", inv.view, inv.dates, today)
	}

	// case 1 数字开头:缺省视图 + 该日期区间。
	inv, err = parseExportInvocation([]string{"20260701"})
	if err != nil {
		t.Fatalf("数字单参数不应报错: %v", err)
	}
	if inv.view != "" || strings.Join(inv.dates, ",") != "2026-07-01" {
		t.Errorf("数字单参数 = (%q,%v), want (\"\",[2026-07-01])", inv.view, inv.dates)
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
				TS:               time.Date(2026, 7, 1, 9, 30, 0, 0, time.Local).UnixMilli(),
				FreshInputTokens: 1000, TotalTokens: 1000},
			{ID: "hour-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-01",
				TS:          time.Date(2026, 7, 1, 15, 0, 0, 0, time.Local).UnixMilli(),
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

// TestExportJSONWeekdayView 导出 weekday 视图:固定 ISO 周序 7 天刻度全量补零
// (与请求日期范围无关),键列为 weekday(显示形态为双语星期名),数值为
// JSON number。
func TestExportJSONWeekdayView(t *testing.T) {
	open := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		// 2026-07-06 周一 / 07-08 周三,消息分别折入 Monday/Wednesday 行。
		msgs := []model.Message{
			{ID: "wd-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-06",
				TS:               time.Date(2026, 7, 6, 9, 0, 0, 0, time.Local).UnixMilli(),
				FreshInputTokens: 1000, TotalTokens: 1000},
			{ID: "wd-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-07-08",
				TS:          time.Date(2026, 7, 8, 15, 0, 0, 0, time.Local).UnixMilli(),
				TotalTokens: 5},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{"weekday", "20260706-20260708", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export weekday json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil {
		t.Fatalf("输出应为合法 JSON:\n%s", out.String())
	}
	if len(rows) != 7 {
		t.Fatalf("应恰 7 行(整周固定刻度补零),实际 %d:\n%s", len(rows), out.String())
	}
	wantKeys := []string{"Monday / 周一", "Tuesday / 周二", "Wednesday / 周三", "Thursday / 周四", "Friday / 周五", "Saturday / 周六", "Sunday / 周日"}
	for i, row := range rows {
		got, _ := row["weekday"].(string)
		if got != wantKeys[i] {
			t.Fatalf("第 %d 行 weekday = %q, want %q(ISO 周序): %v", i, got, wantKeys[i], row)
		}
		total, ok := row["total"].(float64)
		if !ok {
			t.Fatalf("第 %d 行 total 应为 JSON number: %v", i, row)
		}
		switch got {
		case "Monday / 周一":
			if total != 1000 {
				t.Errorf("Monday total 应为 1000,实际 %v", total)
			}
		case "Wednesday / 周三":
			if total != 5 {
				t.Errorf("Wednesday total 应为 5,实际 %v", total)
			}
		default:
			if total != 0 {
				t.Errorf("无数据星期 %s 的 total 应为 0,实际 %v", got, total)
			}
		}
	}
}

// exportMPCFixture 写入带 model/provider 的两条消息,供多维子查询与组合查询
// 导出断言维度键。
func exportMPCFixture(t *testing.T) func(string) (*db.DB, error) {
	t.Helper()
	return func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		msgs := []model.Message{
			{ID: "emp-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
				Provider: "prov-a", Date: "2026-07-09", TS: 1, TotalTokens: 100},
			{ID: "emp-b", SessionID: "s", Client: model.ClientCodexApp, Model: "model-b",
				Provider: "prov-b", Date: "2026-07-09", TS: 2, TotalTokens: 200},
		}
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
}

// TestExportCustomSubqueryMultiKey 自定义子查询按维度组合逐行导出:
// CSV 表头为声明顺序的维度键列(mpc=model,provider,client)加固定指标列;
// JSON 行对象含全部维度键。
func TestExportCustomSubqueryMultiKey(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"mpc", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export mpc: %v", err)
	}
	records, err := csv.NewReader(out).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", out.String())
	}
	wantHeader := []string{"model", "provider", "client", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("多维表头应按声明顺序 = %v,实际 %v", wantHeader, records[0])
	}
	if len(records) != 3 {
		t.Fatalf("两条消息应产出表头 + 2 数据行,实际 %d 行:\n%s", len(records), out.String())
	}

	cmdJSON, outJSON, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmdJSON.SetArgs([]string{"mpc", "20260709", "--format", "json"})
	if err := cmdJSON.Execute(); err != nil {
		t.Fatalf("export mpc json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(outJSON.Bytes(), &rows); err != nil {
		t.Fatalf("JSON 应为对象数组: %v\n%s", err, outJSON.String())
	}
	first := rows[0]
	for _, key := range []string{"model", "provider", "client", "total"} {
		if _, ok := first[key]; !ok {
			t.Errorf("JSON 行对象应含键 %q:\n%s", key, outJSON.String())
		}
	}
}

// TestExportGroupCSVSections 组合查询 CSV 按声明顺序导出成员段,段间空行
// 分隔,各段表头键列随成员视图类型(单维一列、多维多维列)。
func TestExportGroupCSVSections(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"group", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export group: %v", err)
	}
	text := out.String()
	sections := strings.Split(text, "\n\n")
	if len(sections) != 4 {
		t.Fatalf("组合查询应导出 4 个成员段,实际 %d:\n%s", len(sections), text)
	}
	wantHeaders := []string{
		"client,requests,input,output,cache_read,cache_create,reasoning,total",
		"provider,requests,input,output,cache_read,cache_create,reasoning,total",
		"model,requests,input,output,cache_read,cache_create,reasoning,total",
		"model,provider,client,requests,input,output,cache_read,cache_create,reasoning,total",
	}
	for i, want := range wantHeaders {
		header := strings.SplitN(sections[i], "\n", 2)[0]
		if header != want {
			t.Errorf("第 %d 段表头应为 %q,实际 %q", i+1, want, header)
		}
	}
}

// TestExportGroupJSONMemberMap 组合查询 JSON:顶层为成员名到行数组的对象映射,
// 各成员行对象键集合与该成员视图的列集合一致。
func TestExportGroupJSONMemberMap(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"group", "20260709", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export group json: %v", err)
	}
	var members map[string][]map[string]any
	if err := json.Unmarshal(out.Bytes(), &members); err != nil {
		t.Fatalf("JSON 顶层应为成员名到行数组的对象映射: %v\n%s", err, out.String())
	}
	for _, name := range []string{"client", "provider", "model", "mpc"} {
		if _, ok := members[name]; !ok {
			t.Errorf("JSON 应含成员 %q:\n%s", name, out.String())
		}
	}
	if got := len(members["mpc"][0]); got != 10 {
		t.Errorf("mpc 成员行应含 3 键列 + 7 指标列 = 10 键,实际 %d:\n%s", got, out.String())
	}
	// JSON map 的键按字母序序列化(文档承诺):对原始文本断言成员键的
	// 首现顺序,实现改为有序序列化时此断言会暴露形态变化。
	text := out.String()
	positions := []int{}
	for _, name := range []string{"\"client\":", "\"model\":", "\"mpc\":", "\"provider\":"} {
		idx := strings.Index(text, name)
		if idx < 0 {
			t.Fatalf("JSON 文本应含成员键 %s:\n%s", name, text)
		}
		positions = append(positions, idx)
	}
	if !sort.IntsAreSorted(positions) {
		t.Errorf("成员键应按字母序出现在 JSON 文本中:\n%s", text)
	}
}

// TestExportDefaultFollowsQueryDefault 缺省视图执行 query.default:
// 数字单参数形态(缺省视图 + 指定日期)与组合查询导出一致。
func TestExportDefaultFollowsQueryDefault(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export 20260709: %v", err)
	}
	if sections := strings.Split(out.String(), "\n\n"); len(sections) != 4 {
		t.Errorf("缺省视图(default=group)应导出 4 个成员段:\n%s", out.String())
	}
}

// TestExportUnknownViewAllowedIncludesConfigured 未知视图:允许集合动态包含
// 已配置视图名,并在打开数据库之前拒绝。
func TestExportUnknownViewAllowedIncludesConfigured(t *testing.T) {
	openCalls := 0
	open := func(string) (*db.DB, error) {
		openCalls++
		return nil, errors.New("must not open database")
	}
	cmd, _, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), open)
	cmd.SetArgs([]string{"bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("未知视图应报错")
	}
	msg := err.Error()
	for _, want := range []string{"session, mpc, group", "/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含允许集合片段 %q: %q", want, msg)
		}
	}
	if openCalls != 0 {
		t.Errorf("未知视图拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestExportExplicitBuiltinIsolatesBrokenDefs 显式内置视图与 query 静态子命令
// 同一隔离语义:视图定义坏档不阻断导出。
func TestExportExplicitBuiltinIsolatesBrokenDefs(t *testing.T) {
	broken := map[string]any{"groups": map[string]any{"bad": "client,nosuch"}}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(broken, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"client", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("显式内置视图不应被无关视图定义错误阻断: %v", err)
	}
	if !strings.Contains(out.String(), "client,requests") {
		t.Errorf("client 视图导出应正常产出:\n%s", out.String())
	}
}

// TestExportDefaultRejectsBrokenDefs 缺省视图消费完整解析:视图定义坏档时
// 与裸 query 一致拒绝,且不打开数据库。
func TestExportDefaultRejectsBrokenDefs(t *testing.T) {
	openCalls := 0
	open := func(string) (*db.DB, error) {
		openCalls++
		return nil, errors.New("must not open database")
	}
	broken := map[string]any{"groups": map[string]any{"bad": "client,nosuch"}}
	cmd, _, _ := newExportOutputCmdWithDeps(loadWithRaw(broken, nil), open)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("坏视图定义应使缺省导出拒绝")
	}
	if !strings.Contains(err.Error(), "invalid item") {
		t.Errorf("错误应含 querydef 诊断,实际: %v", err)
	}
	if openCalls != 0 {
		t.Errorf("坏定义拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestExportDefaultCustomSubquery 缺省视图指向自定义子查询:
// 数字单参数形态(缺省视图 + 指定日期)按多维 schema 导出。
func TestExportDefaultCustomSubquery(t *testing.T) {
	raw := map[string]any{
		"default":    "mpc",
		"subqueries": map[string]any{"mpc": "model,provider"},
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(raw, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export 20260709 (default=mpc): %v", err)
	}
	records, err := csv.NewReader(out).ReadAll()
	if err != nil {
		t.Fatalf("输出应为合法 CSV:\n%s", out.String())
	}
	wantHeader := []string{"model", "provider", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Errorf("多维表头应为 %v,实际 %v", wantHeader, records[0])
	}
}

// TestExportSubqueryDayKeyColumnMapping 子查询含 day 维度:键列名沿用内置
// 视图的 day → date 映射。
func TestExportSubqueryDayKeyColumnMapping(t *testing.T) {
	raw := map[string]any{
		"subqueries": map[string]any{"trend": "client,day"},
	}
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(raw, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"trend", "20260709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export trend: %v", err)
	}
	header := strings.SplitN(out.String(), "\n", 2)[0]
	want := "client,date,requests,input,output,cache_read,cache_create,reasoning,total"
	if header != want {
		t.Errorf("含 day 维度的键列应映射为 date:\nwant %q\ngot  %q", want, header)
	}
}

// TestExportEmptyViewNameRejected 显式空串视图名按未知名拒绝,
// 不得静默回退缺省视图,且在打开数据库之前拒绝。
func TestExportEmptyViewNameRejected(t *testing.T) {
	openCalls := 0
	open := func(string) (*db.DB, error) {
		openCalls++
		return nil, errors.New("must not open database")
	}
	cmd, _, _ := newExportOutputCmdWithDeps(loadWithRaw(nil, nil), open)
	cmd.SetArgs([]string{""})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("显式空串视图名应被拒绝")
	}
	if !strings.Contains(err.Error(), `unknown export view ""`) {
		t.Errorf("空串应按未知名拒绝,实际: %v", err)
	}
	if openCalls != 0 {
		t.Errorf("空串拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestExportGroupEmptyMemberSectionHeaderOnly 组合查询在区间无数据时,
// 非时间维度成员段仅含表头行(空数据段),段间仍以空行分隔。
func TestExportGroupEmptyMemberSectionHeaderOnly(t *testing.T) {
	cmd, out, _ := newExportOutputCmdWithDeps(loadWithRaw(watchGroupConfig("/mem").RawQuery, nil), exportMPCFixture(t))
	cmd.SetArgs([]string{"group", "20260101"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export group (empty range): %v", err)
	}
	sections := strings.Split(out.String(), "\n\n")
	if len(sections) != 4 {
		t.Fatalf("组合查询应导出 4 个成员段,实际 %d:\n%s", len(sections), out.String())
	}
	for i, section := range sections {
		lines := strings.Split(strings.TrimRight(section, "\n"), "\n")
		if len(lines) != 1 {
			t.Errorf("空区间下第 %d 段应仅含表头行,实际 %d 行:\n%q", i+1, len(lines), section)
		}
	}
}
