package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// newErrorsJSONTestCmd 注入内存库的 errors 命令（真实调用链：parseDateArgs →
// --format 校验 → load → open → GetErrorsContext → 渲染）。
func newErrorsJSONTestCmd(t *testing.T, usageDB *db.DB) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newErrorsCmdWithDeps(
		func() (*config.Config, error) {
			return &config.Config{DataDir: t.TempDir()}, nil
		},
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	return cmd, &buf
}

// seedErrorsJSONFixture 注入三条同日记录：claude 未解决 retry=0、codex 未解决
// retry=2、opencode 已解决 retry=0，返回库句柄。
func seedErrorsJSONFixture(t *testing.T) *db.DB {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open :memory: failed: %v", err)
	}
	t.Cleanup(func() { usageDB.Close() })

	ctx := context.Background()
	if err := db.RecordError(ctx, usageDB, "2026-07-21", "claude", "database locked", "detail"); err != nil {
		t.Fatalf("seed claude failed: %v", err)
	}
	if err := db.RecordError(ctx, usageDB, "2026-07-21", "codex", "JSONL parse failed", ""); err != nil {
		t.Fatalf("seed codex failed: %v", err)
	}
	if err := db.RecordError(ctx, usageDB, "2026-07-21", "opencode", "rate limited", ""); err != nil {
		t.Fatalf("seed opencode failed: %v", err)
	}
	// codex 重试两次 → retry_count=2。
	for i := 0; i < 2; i++ {
		if _, err := db.IncrementRetryCountByDateSource(ctx, usageDB, "2026-07-21", "codex"); err != nil {
			t.Fatalf("increment retry failed: %v", err)
		}
	}
	// 解决 claude 那条。
	errs, err := db.GetErrors(usageDB, db.ErrorFilter{Source: "claude"})
	if err != nil || len(errs) != 1 {
		t.Fatalf("lookup claude error: %v records=%d", err, len(errs))
	}
	if err := db.ResolveError(ctx, usageDB, errs[0].ID); err != nil {
		t.Fatalf("resolve claude failed: %v", err)
	}
	return usageDB
}

// TestErrorsCmd_EndToEnd_JSON 真实调用链 --format json：字段名、原始类型、
// 条数与解析值逐项断言，并与同 filter 的 table 输出比对记录顺序一致。
func TestErrorsCmd_EndToEnd_JSON(t *testing.T) {
	usageDB := seedErrorsJSONFixture(t)

	// table 参照输出先于 json Execute 生成：RunE 的 defer Close 会关闭注入的
	// 共享内存库（同 compare E2E 的约束：注入库不跨多次执行复用）。
	var tbuf bytes.Buffer
	if err := runErrors(usageDB, &tbuf, db.ErrorFilter{Dates: []string{"2026-07-21"}}); err != nil {
		t.Fatalf("table run failed: %v", err)
	}
	tableIDs := extractErrorsTableIDs(tbuf.String(), t)

	cmd, buf := newErrorsJSONTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260721", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--format json failed: %v", err)
	}
	out := buf.String()

	// stdout 纯数据：无统计头、无「暂无异常记录」、无重试提示行。
	for _, banned := range []string{"暂无异常记录", "collection errors", "collect retry", "┌"} {
		if strings.Contains(out, banned) {
			t.Errorf("json 输出不应含提示/表格内容 %q:\n%s", banned, out)
		}
	}

	// 键名与原始类型：retry_count 为无引号整数、resolved 为布尔。
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(raw) != 3 {
		t.Fatalf("expected 3 records, got %d:\n%s", len(raw), out)
	}
	wantKeys := []string{"id", "date", "source", "message", "retry_count", "resolved"}
	for i, rec := range raw {
		if len(rec) != len(wantKeys) {
			t.Errorf("record %d 键数 %d != %d: %v", i, len(rec), len(wantKeys), rec)
		}
		for _, k := range wantKeys {
			v, ok := rec[k]
			if !ok {
				t.Errorf("record %d 缺少键 %q: %v", i, k, rec)
				continue
			}
			switch k {
			case "id", "retry_count":
				f, ok := v.(float64)
				if !ok || f != float64(int(f)) {
					t.Errorf("record %d 键 %q 应为原始整数, got %v (%T)", i, k, v, v)
				}
			case "resolved":
				if _, ok := v.(bool); !ok {
					t.Errorf("record %d 键 resolved 应为布尔, got %T", i, v)
				}
			default:
				if _, ok := v.(string); !ok {
					t.Errorf("record %d 键 %q 应为字符串, got %T", i, k, v)
				}
			}
		}
	}

	// 解析值锚定：retry 次数不同与 resolved 状态按注入值落位。
	var records []errorsJSONRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("unmarshal into errorsJSONRecord failed: %v", err)
	}
	bySource := map[string]errorsJSONRecord{}
	for _, r := range records {
		bySource[r.Source] = r
	}
	if r := bySource["codex"]; r.RetryCount != 2 || r.Resolved {
		t.Errorf("codex 应为 retry_count=2 且未解决, got %+v", r)
	}
	if r := bySource["claude"]; !r.Resolved || r.RetryCount != 0 {
		t.Errorf("claude 应为已解决且 retry_count=0, got %+v", r)
	}
	if r := bySource["opencode"]; r.Resolved || r.RetryCount != 0 {
		t.Errorf("opencode 应为未解决且 retry_count=0, got %+v", r)
	}

	// 顺序与 table 模式一致（同一 filter、同一 GetErrorsContext 排序）。
	jsonIDs := make([]string, 0, len(records))
	for _, r := range records {
		jsonIDs = append(jsonIDs, strconv.Itoa(r.ID))
	}
	if strings.Join(tableIDs, ",") != strings.Join(jsonIDs, ",") {
		t.Errorf("json 顺序 %v != table 顺序 %v", jsonIDs, tableIDs)
	}
}

// extractErrorsTableIDs 从 errors 框线表输出中按行提取 ID 列（表头行非数字，跳过）。
func extractErrorsTableIDs(table string, t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, ln := range strings.Split(table, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		cols := strings.Split(ln, "│")
		if len(cols) < 3 {
			continue
		}
		if id := strings.TrimSpace(cols[1]); id != "ID" {
			if _, err := strconv.Atoi(id); err != nil {
				t.Fatalf("表格行 ID 列解析失败: %q in %q", id, ln)
			}
			ids = append(ids, id)
		}
	}
	return ids
}

// TestErrorsCmd_JSON_EmptyDatabase 空库 --format json 原文输出 []（含尾随换行）。
func TestErrorsCmd_JSON_EmptyDatabase(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open :memory: failed: %v", err)
	}
	defer usageDB.Close()

	cmd, buf := newErrorsJSONTestCmd(t, usageDB)
	cmd.SetArgs([]string{"--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--format json failed: %v", err)
	}
	if got := buf.String(); got != "[]\n" {
		t.Errorf("空库 json 输出应为 %q, got %q", "[]\n", got)
	}
}

// TestErrorsCmd_RejectsFormat --format 非法值：双语报错且在加载配置与打开
// 数据库之前拒绝（open 计数为 0）。
func TestErrorsCmd_RejectsFormat(t *testing.T) {
	openCalls := 0
	cmd := newErrorsCmdWithDeps(
		func() (*config.Config, error) { t.Fatal("--format 校验失败不应加载配置"); return nil, nil },
		func(string) (*db.DB, error) {
			openCalls++
			return nil, fmt.Errorf("must not open database")
		},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "xml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--format xml 应返回 error")
	}
	msg := err.Error()
	for _, want := range []string{
		`invalid --format "xml" (allowed: table, json)`,
		`无效的 --format "xml"（允许：table、json）`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q: %q", want, msg)
		}
	}
	if openCalls != 0 {
		t.Errorf("格式拒绝不得打开 DB,实际调用 open %d 次", openCalls)
	}
}

// TestErrorsCmd_FormatFlagDefault --format 缺省 table，描述双语对齐 compare。
func TestErrorsCmd_FormatFlagDefault(t *testing.T) {
	flag := newErrorsCmd().Flags().Lookup("format")
	if flag == nil {
		t.Fatal("expected --format flag")
	}
	if flag.DefValue != "table" {
		t.Errorf("--format 缺省应为 table, got %q", flag.DefValue)
	}
	if want := "output format: table or json / 输出格式：table 或 json"; flag.Usage != want {
		t.Errorf("--format usage = %q, want %q", flag.Usage, want)
	}
}
