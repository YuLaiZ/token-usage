package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 36 {
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
