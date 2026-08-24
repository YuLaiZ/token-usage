package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/YuLaiZ/token-usage/internal/db"
)

func TestNewErrorsCmd_HasFlags(t *testing.T) {
	cmd := newErrorsCmd()

	if cmd.Use != "errors [YYYYMMDD]" {
		t.Errorf("expected Use='errors [YYYYMMDD]', got %q", cmd.Use)
	}

	// --date flag 已移除（日期改为位置参数）。
	if cmd.Flags().Lookup("date") != nil {
		t.Error("--date flag 应已移除")
	}

	sourceFlag := cmd.Flags().Lookup("source")
	if sourceFlag == nil {
		t.Error("expected --source flag")
	}

	unresolvedFlag := cmd.Flags().Lookup("unresolved")
	if unresolvedFlag == nil {
		t.Error("expected --unresolved flag")
	}
}

func TestRunErrors_NoErrors(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	var buf bytes.Buffer
	err := runErrors(usageDB, &buf, db.ErrorFilter{Unresolved: true})
	if err != nil {
		t.Fatalf("runErrors failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "暂无异常记录") {
		t.Errorf("expected '暂无异常记录', got: %s", output)
	}
}

func TestRunErrors_WithErrors(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude", "database locked", "detail")
	db.RecordError(context.Background(), usageDB, "2026-06-08", "codex", "JSONL parse failed", "")

	var buf bytes.Buffer
	err := runErrors(usageDB, &buf, db.ErrorFilter{Unresolved: true})
	if err != nil {
		t.Fatalf("runErrors failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "2 条") {
		t.Errorf("expected '2 条', got: %s", output)
	}
	if !strings.Contains(output, "database locked") {
		t.Errorf("expected 'database locked', got: %s", output)
	}
}

// TestRunErrors_ChineseMessageTableAlignment：双语表头与中文错误内容（历史
// 库存中文 message、新入库的双语 error 值）同表渲染时必须按显示宽度对齐：
// 所有行等宽、│ 分隔列逐列对齐、超宽中文 message 截断不穿透边框。
func TestRunErrors_ChineseMessageTableAlignment(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	longZh := strings.Repeat("读取数据源失败:", 10) + " 打开数据库失败"
	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude",
		"claude read / 读取数据源失败: 打开数据库失败", "")
	db.RecordError(context.Background(), usageDB, "2026-06-08", "codex", longZh, "")

	var buf bytes.Buffer
	if err := runErrors(usageDB, &buf, db.ErrorFilter{Unresolved: true}); err != nil {
		t.Fatalf("runErrors failed: %v", err)
	}

	// 提取 ┌..└ 之间的表格行。
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var table []string
	inTable := false
	for _, ln := range lines {
		if strings.Contains(ln, "┌") {
			inTable = true
		}
		if inTable {
			table = append(table, ln)
			if strings.Contains(ln, "└") {
				break
			}
		}
	}
	if len(table) < 5 { // 顶边框+表头+分隔线+2 数据行+底边框
		t.Fatalf("表格行数不足: %q", buf.String())
	}

	wantWidth := runewidth.StringWidth(table[0])
	for i, ln := range table {
		if got := runewidth.StringWidth(ln); got != wantWidth {
			t.Errorf("第 %d 行显示宽度 %d != 边框 %d: %q", i, got, wantWidth, ln)
		}
	}

	// │ 分隔列逐列对齐（表头与数据行；边框行无 │）。
	barCols := func(ln string) []int {
		var cols []int
		w := 0
		for _, r := range ln {
			if r == '│' {
				cols = append(cols, w)
			}
			w += runewidth.RuneWidth(r)
		}
		return cols
	}
	var ref []int
	for _, ln := range table {
		if b := barCols(ln); len(b) > 0 {
			ref = b
			break
		}
	}
	if ref == nil {
		t.Fatal("表格中未找到数据行")
	}
	for i, ln := range table {
		b := barCols(ln)
		if len(b) == 0 {
			continue
		}
		if len(b) != len(ref) {
			t.Errorf("第 %d 行 │ 数量 %d != %d: %q", i, len(b), len(ref), ln)
			continue
		}
		for j := range b {
			if b[j] != ref[j] {
				t.Errorf("第 %d 行第 %d 个 │ 列位 %d != %d: %q", i, j, b[j], ref[j], ln)
				break
			}
		}
	}

	// 超宽中文 message 截断并显示省略号。
	if !strings.Contains(buf.String(), "...") {
		t.Errorf("超长中文 message 应被截断显示省略号: %q", buf.String())
	}
}

func TestRunErrors_FilterBySource(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude", "error 1", "")
	db.RecordError(context.Background(), usageDB, "2026-06-09", "codex", "error 2", "")

	var buf bytes.Buffer
	err := runErrors(usageDB, &buf, db.ErrorFilter{Source: "claude"})
	if err != nil {
		t.Fatalf("runErrors failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "1 条") {
		t.Errorf("expected '1 条', got: %s", output)
	}
	if strings.Contains(output, "error 2") {
		t.Errorf("should not contain error 2 from other source")
	}
}

func TestRunErrors_FilterByDates(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.RecordError(context.Background(), usageDB, "2026-06-09", "claude", "wanted", "")
	db.RecordError(context.Background(), usageDB, "2026-06-08", "claude", "unrelated", "")
	var buf bytes.Buffer
	if err := runErrors(usageDB, &buf, db.ErrorFilter{Dates: []string{"2026-06-09"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "wanted") || strings.Contains(buf.String(), "unrelated") {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestTruncateRunes_DoesNotSplitUTF8(t *testing.T) {
	got := truncateRunes(strings.Repeat("错", 40), 36)
	// 截断按显示宽度（中文占 2 列），UTF-8 不得被拆开且总宽不得超限。
	if !utf8.ValidString(got) || runewidth.StringWidth(got) > 36 {
		t.Fatalf("invalid truncation: %q", got)
	}
}

func TestTruncateRunes_SmallLimits(t *testing.T) {
	if got := truncateRunes("abcdef", 0); got != "" {
		t.Fatalf("truncateRunes max=0 = %q, want empty", got)
	}
	if got := truncateRunes("abcdef", 2); got != "ab" {
		t.Fatalf("truncateRunes max=2 = %q, want ab", got)
	}
}

// TestNormalizeErrorDate 旧函数已删除，行为迁移到 parseErrorDateArg（见 date_test.go）。
// 仅保留对 errors 专用解析的破坏性收窄断言：YYYY-MM-DD 与范围均被拒绝。
func TestNormalizeErrorDate_MigratedToParseErrorDateArg(t *testing.T) {
	// 合法 YYYYMMDD
	got, err := parseErrorDateArg([]string{"20260609"})
	if err != nil || got != "2026-06-09" {
		t.Fatalf("parseErrorDateArg([20260609]) = %q, %v", got, err)
	}
	// YYYY-MM-DD 被拒绝（破坏性收窄）
	if _, err := parseErrorDateArg([]string{"2026-06-09"}); err == nil {
		t.Fatal("YYYY-MM-DD 应被 errors 拒绝")
	}
	// 范围被拒绝
	if _, err := parseErrorDateArg([]string{"20260601-20260603"}); err == nil {
		t.Fatal("errors 应拒绝范围日期")
	}
	// 非法格式
	if _, err := parseErrorDateArg([]string{"2026/06/09"}); err == nil {
		t.Fatal("invalid date must fail")
	}
}
