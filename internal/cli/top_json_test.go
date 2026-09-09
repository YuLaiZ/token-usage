package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// TestTopCmd_EndToEnd_JSON 真实调用链 --format json:根为数组,顺序与 rank
// 沿用表格排序口径(TotalTokens 降序,topTestSeed 三会话 total 各不相同);
// 字段全量 14 键且指标为原始整数;空 project 原样输出 "",不映射「未分类」;
// stdout 纯数据,无标题行与表格内容。
func TestTopCmd_EndToEnd_JSON(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	topTestSeed(t, usageDB)

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709", "--format", "json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, banned := range []string{"Top sessions", "no data", "│"} {
		if strings.Contains(out, banned) {
			t.Errorf("json 输出不应含表格内容 %q:\n%s", banned, out)
		}
	}

	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("输出应为合法 JSON 数组: %v\n%s", err, out)
	}
	if len(raw) != 3 {
		t.Fatalf("应恰 3 条记录,实际 %d:\n%s", len(raw), out)
	}

	// 顺序与 rank:TotalTokens 降序 5000/1100/300,rank 从 1 起。
	wantTitles := []string{"heavy-batch", "fix-login", strings.Repeat("t", 35)}
	for i, rec := range raw {
		if got, ok := rec["rank"].(float64); !ok || got != float64(i+1) {
			t.Errorf("第 %d 条 rank 应为 %d,实际 %v (%T)", i, i+1, rec["rank"], rec["rank"])
		}
		if got := rec["title"]; got != wantTitles[i] {
			t.Errorf("第 %d 条 title 应为 %q,实际 %v", i, wantTitles[i], got)
		}
	}

	// 字段全量:14 键,键名固定。
	wantKeys := []string{
		"rank", "client", "project", "title", "first_ts", "last_ts", "duration_ms",
		"requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total",
	}
	for i, rec := range raw {
		if len(rec) != len(wantKeys) {
			t.Errorf("第 %d 条键数 %d != %d: %v", i, len(rec), len(wantKeys), rec)
		}
		for _, k := range wantKeys {
			if _, ok := rec[k]; !ok {
				t.Errorf("第 %d 条缺少键 %q: %v", i, k, rec)
			}
		}
	}

	// 解析值锚定(与 topTestSeed 的注入值一一对应):时间戳为 Unix 毫秒,
	// duration_ms = last_ts - first_ts,指标为原始整数。
	ts := func(h, m int) int64 { return time.Date(2026, 7, 9, h, m, 0, 0, time.Local).UnixMilli() }
	var records []topJSONRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("unmarshal into topJSONRecord failed: %v", err)
	}
	want := []topJSONRecord{
		{Rank: 1, Client: "Codex App", Project: "", Title: "heavy-batch",
			FirstTS: ts(9, 0), LastTS: ts(9, 0), DurationMS: 0,
			Requests: 1, Total: 5000},
		{Rank: 2, Client: "Claude Code", Project: "proj-alpha", Title: "fix-login",
			FirstTS: ts(10, 0), LastTS: ts(11, 1), DurationMS: 61 * 60 * 1000,
			Requests: 2, Total: 1100},
		{Rank: 3, Client: "Claude Code", Project: "proj-beta", Title: strings.Repeat("t", 35),
			FirstTS: ts(12, 30), LastTS: ts(12, 30), DurationMS: 0,
			Requests: 1, Total: 300},
	}
	for i, got := range records {
		w := want[i]
		if got.Client != w.Client || got.Project != w.Project || got.Title != w.Title {
			t.Errorf("第 %d 条标识字段 = %+v, want client=%q project=%q title=%q",
				i+1, got, w.Client, w.Project, w.Title)
		}
		if got.FirstTS != w.FirstTS || got.LastTS != w.LastTS || got.DurationMS != w.DurationMS {
			t.Errorf("第 %d 条时间字段 = %d/%d/%d, want %d/%d/%d",
				i+1, got.FirstTS, got.LastTS, got.DurationMS, w.FirstTS, w.LastTS, w.DurationMS)
		}
		if got.Requests != w.Requests || got.Total != w.Total {
			t.Errorf("第 %d 条 requests/total = %d/%d, want %d/%d",
				i+1, got.Requests, got.Total, w.Requests, w.Total)
		}
		// seed 未注入的指标字段应为原始整数 0,而非缺省或缺席。
		if got.FreshInput != 0 || got.Output != 0 || got.CacheRead != 0 ||
			got.CacheCreate != 0 || got.Reasoning != 0 {
			t.Errorf("第 %d 条未注入指标应全为 0,实际 %+v", i+1, got)
		}
	}
	if records[0].Project != "" {
		t.Errorf("空 project 应原样输出 \"\",实际 %q", records[0].Project)
	}
}

// TestTopCmd_JSON_LimitTruncates --limit 截断在 JSON 模式同样生效:排序后
// 只保留前 --limit 条,rank 仍从 1 起。
func TestTopCmd_JSON_LimitTruncates(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	topTestSeed(t, usageDB)

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709", "--limit=1", "--format", "json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var records []topJSONRecord
	if err := json.Unmarshal(buf.Bytes(), &records); err != nil {
		t.Fatalf("输出应为合法 JSON 数组: %v\n%s", err, buf.String())
	}
	if len(records) != 1 {
		t.Fatalf("--limit=1 应恰 1 条,实际 %d:\n%s", len(records), buf.String())
	}
	if records[0].Rank != 1 || records[0].Title != "heavy-batch" || records[0].Total != 5000 {
		t.Errorf("保留条应为 rank 1 heavy-batch(5000),实际 %+v", records[0])
	}
}

// TestTopCmd_JSON_EmptyDatabase 空库 --format json 原文输出 [](含尾随换行),
// 不做表格的 no data 早退。
func TestTopCmd_JSON_EmptyDatabase(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	cmd := newTopTestCmd(t, usageDB)
	cmd.SetArgs([]string{"20260709", "--format", "json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--format json failed: %v", err)
	}
	if got := buf.String(); got != "[]\n" {
		t.Errorf("空库 json 输出应为 %q, got %q", "[]\n", got)
	}
}

// TestTopCmd_RejectsFormat --format 非法值:双语报错且在加载配置与打开数据库
// 之前拒绝(open 计数为 0,与 errors/compare 的既有测试手法一致)。
func TestTopCmd_RejectsFormat(t *testing.T) {
	openCalls := 0
	cmd := newTopCmdWithDeps(
		func() (*config.Config, error) { t.Fatal("--format 校验失败不应加载配置"); return nil, nil },
		func(string) (*db.DB, error) {
			openCalls++
			return nil, fmt.Errorf("must not open database")
		},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260709", "--format", "xml"})
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

// TestTopCmd_FormatFlagDefault --format 缺省 table,usage 双语与 compare 一致。
func TestTopCmd_FormatFlagDefault(t *testing.T) {
	flag := newTopCmd().Flags().Lookup("format")
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
