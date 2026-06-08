package cli

import (
	"strings"
	"testing"
)

// TestParseDateArgs_DefaultToday defaultToday=true 且无参数应返回今天的单日期。
func TestParseDateArgs_DefaultToday(t *testing.T) {
	dates, err := parseDateArgs(nil, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs(nil,true) 出错: %v", err)
	}
	if len(dates) != 1 {
		t.Fatalf("期望 1 个日期，实际 %d: %v", len(dates), dates)
	}
	if len(dates[0]) != 10 {
		t.Errorf("日期格式应为 YYYY-MM-DD，实际 %q", dates[0])
	}
}

// TestParseDateArgs_DefaultEmpty defaultToday=false 且无参数应返回空切片。
func TestParseDateArgs_DefaultEmpty(t *testing.T) {
	dates, err := parseDateArgs(nil, false, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs(nil,false) 出错: %v", err)
	}
	if len(dates) != 0 {
		t.Errorf("期望空切片，实际 %v", dates)
	}
}

// TestParseDateArgs_SingleDate 单个合法 YYYYMMDD。
func TestParseDateArgs_SingleDate(t *testing.T) {
	dates, err := parseDateArgs([]string{"20260609"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	if len(dates) != 1 || dates[0] != "2026-06-09" {
		t.Errorf("期望 [2026-06-09]，实际 %v", dates)
	}
}

// TestParseDateArgs_DateRange 合法闭区间及区间展开结果。
func TestParseDateArgs_DateRange(t *testing.T) {
	dates, err := parseDateArgs([]string{"20260601-20260603"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	want := []string{"2026-06-01", "2026-06-02", "2026-06-03"}
	if len(dates) != len(want) {
		t.Fatalf("期望 %d 个日期，实际 %d: %v", len(want), len(dates), dates)
	}
	for i, d := range dates {
		if d != want[i] {
			t.Errorf("dates[%d] = %q, want %q", i, d, want[i])
		}
	}
}

// TestParseDateArgs_ReversedRange 反向区间被拒绝。
func TestParseDateArgs_ReversedRange(t *testing.T) {
	_, err := parseDateArgs([]string{"20260603-20260601"}, true, "collect")
	if err == nil {
		t.Fatal("反向区间应返回 error")
	}
}

// TestParseDateArgs_InvalidEndpoint 非法区间端点（非日历日期）被拒绝。
func TestParseDateArgs_InvalidEndpoint(t *testing.T) {
	_, err := parseDateArgs([]string{"20261301-20260601"}, true, "collect")
	if err == nil {
		t.Fatal("非法日历日期端点应返回 error")
	}
}

// TestParseDateArgs_InvalidFormat 非法长度/格式被拒绝。
func TestParseDateArgs_InvalidFormat(t *testing.T) {
	for _, in := range []string{"invalid", "2026", "2026060", "2026060X"} {
		if _, err := parseDateArgs([]string{in}, true, "collect"); err == nil {
			t.Errorf("非法日期 %q 应返回 error", in)
		}
	}
}

// TestParseDateArgs_RejectsDashFormat YYYY-MM-DD 被拒绝（破坏性收窄）。
func TestParseDateArgs_RejectsDashFormat(t *testing.T) {
	_, err := parseDateArgs([]string{"2026-06-09"}, true, "collect")
	if err == nil {
		t.Fatal("YYYY-MM-DD 应被拒绝（仅接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD）")
	}
}

// TestParseDateArgs_TooManyArgs 多余位置参数被拒绝。
func TestParseDateArgs_TooManyArgs(t *testing.T) {
	_, err := parseDateArgs([]string{"20260601", "20260602"}, true, "collect")
	if err == nil {
		t.Fatal("多余位置参数应返回 error")
	}
}

// TestParseDateArgs_ErrorContainsFormatAndExample 错误文案包含格式与示例，
// 覆盖 collect 与 query 两种命令名路径（cmdName 决定示例文案）。
func TestParseDateArgs_ErrorContainsFormatAndExample(t *testing.T) {
	// collect 路径
	_, err := parseDateArgs([]string{"bad"}, true, "collect")
	if err == nil {
		t.Fatal("期望 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "YYYYMMDD") {
		t.Errorf("错误应含格式说明，实际 %q", msg)
	}
	if !strings.Contains(msg, "token-usage collect 20260701") {
		t.Errorf("collect 错误应含 token-usage collect 命令示例，实际 %q", msg)
	}

	// query 路径
	_, err = parseDateArgs([]string{"bad"}, true, "query")
	if err == nil {
		t.Fatal("期望 error")
	}
	msg = err.Error()
	if !strings.Contains(msg, "YYYYMMDD") {
		t.Errorf("错误应含格式说明，实际 %q", msg)
	}
	if !strings.Contains(msg, "token-usage query 20260701") {
		t.Errorf("query 错误应含 token-usage query 命令示例，实际 %q", msg)
	}
}

// TestParseErrorDateArg_Empty 无参数返回空串。
func TestParseErrorDateArg_Empty(t *testing.T) {
	got, err := parseErrorDateArg(nil)
	if err != nil {
		t.Fatalf("parseErrorDateArg(nil) 出错: %v", err)
	}
	if got != "" {
		t.Errorf("无参数应返回空串，实际 %q", got)
	}
}

// TestParseErrorDateArg_Single 合法单日。
func TestParseErrorDateArg_Single(t *testing.T) {
	got, err := parseErrorDateArg([]string{"20260701"})
	if err != nil {
		t.Fatalf("parseErrorDateArg 出错: %v", err)
	}
	if got != "2026-07-01" {
		t.Errorf("期望 2026-07-01，实际 %q", got)
	}
}

// TestParseErrorDateArg_RejectsRange 范围被拒绝。
func TestParseErrorDateArg_RejectsRange(t *testing.T) {
	if _, err := parseErrorDateArg([]string{"20260701-20260703"}); err == nil {
		t.Fatal("errors 应拒绝范围日期")
	}
}

// TestParseErrorDateArg_RejectsDashFormat YYYY-MM-DD 被拒绝。
func TestParseErrorDateArg_RejectsDashFormat(t *testing.T) {
	if _, err := parseErrorDateArg([]string{"2026-07-01"}); err == nil {
		t.Fatal("errors 应拒绝 YYYY-MM-DD")
	}
}

// TestParseErrorDateArg_RejectsInvalid 非法格式/日历日期被拒绝。
func TestParseErrorDateArg_RejectsInvalid(t *testing.T) {
	for _, in := range []string{"not-a-date", "2026", "2026070", "20261301"} {
		if _, err := parseErrorDateArg([]string{in}); err == nil {
			t.Errorf("errors 应拒绝非法日期 %q", in)
		}
	}
}

// TestParseErrorDateArg_TooManyArgs 多余参数被拒绝。
func TestParseErrorDateArg_TooManyArgs(t *testing.T) {
	if _, err := parseErrorDateArg([]string{"20260701", "20260702"}); err == nil {
		t.Fatal("errors 多余参数应返回 error")
	}
}

// TestParseErrorDateArg_ErrorContainsFormatAndExample 错误文案含格式与示例。
func TestParseErrorDateArg_ErrorContainsFormatAndExample(t *testing.T) {
	_, err := parseErrorDateArg([]string{"bad"})
	if err == nil {
		t.Fatal("期望 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "YYYYMMDD") {
		t.Errorf("错误应含格式说明，实际 %q", msg)
	}
	if !strings.Contains(msg, "token-usage errors") {
		t.Errorf("错误应含 errors 命令示例，实际 %q", msg)
	}
}
