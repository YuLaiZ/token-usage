package cli

import (
	"strconv"
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
// "2026" 是合法年参数（单独使用），不在非法清单。
func TestParseDateArgs_InvalidFormat(t *testing.T) {
	for _, in := range []string{"invalid", "2026060", "2026060X"} {
		if _, err := parseDateArgs([]string{in}, true, "collect"); err == nil {
			t.Errorf("非法日期 %q 应返回 error", in)
		}
	}
}

// TestParseDateArgs_RejectsDashFormat YYYY-MM-DD 被拒绝（破坏性收窄）。
func TestParseDateArgs_RejectsDashFormat(t *testing.T) {
	_, err := parseDateArgs([]string{"2026-06-09"}, true, "collect")
	if err == nil {
		t.Fatal("YYYY-MM-DD 应被拒绝（统一为紧凑数字口径 YYYYMMDD/YYYYMM/YYYY）")
	}
}

// TestParseDateArgs_TooManyArgs 多余位置参数被拒绝；错误携带前缀、实际
// 数量、三粒度格式说明与命令示例（沿用现状句式，无 %q 原文引用）。
func TestParseDateArgs_TooManyArgs(t *testing.T) {
	_, err := parseDateArgs([]string{"20260601", "20260602"}, true, "collect")
	if err == nil {
		t.Fatal("多余位置参数应返回 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid date args") {
		t.Errorf("错误应含 invalid date args 前缀，实际 %q", msg)
	}
	if !strings.Contains(msg, "got 2") || !strings.Contains(msg, "得到 2 个") {
		t.Errorf("错误应含实际参数数量（双语），实际 %q", msg)
	}
	assertDateGranularityHints(t, msg)
	if !strings.Contains(msg, "token-usage collect 20260701") {
		t.Errorf("错误应含命令示例 token-usage collect 20260701，实际 %q", msg)
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

// assertDateGranularityHints 断言错误文案携带三粒度格式说明（带粒度标注的
// 独立短语，防止 YYYYMMDD/YYYYMM/YYYY 互为前缀导致断言无区分度）。
func assertDateGranularityHints(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{
		"YYYYMMDD (day)", "YYYYMM (month)", "YYYY (year)",
		"YYYYMMDD（日）", "YYYYMM（月）", "YYYY（年）",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q，实际 %q", want, msg)
		}
	}
}

// assertDateArgErrorContract 断言 parseDateArgs 拒绝分支错误文案的完整合同：
// invalid date args 前缀、%q 原文引用、三粒度格式说明与命令示例。
func assertDateArgErrorContract(t *testing.T, msg, raw, cmdName string) {
	t.Helper()
	if !strings.Contains(msg, "invalid date args") {
		t.Errorf("错误应含 invalid date args 前缀，实际 %q", msg)
	}
	if !strings.Contains(msg, strconv.Quote(raw)) {
		t.Errorf("错误应含 %q 原文引用，实际 %q", raw, msg)
	}
	assertDateGranularityHints(t, msg)
	if !strings.Contains(msg, "token-usage "+cmdName+" 20260701") {
		t.Errorf("错误应含命令示例 token-usage %s 20260701，实际 %q", cmdName, msg)
	}
}

// assertExpandedDates 断言展开结果的天数与首末日。
func assertExpandedDates(t *testing.T, dates []string, wantDays int, wantFirst, wantLast string) {
	t.Helper()
	if len(dates) != wantDays {
		t.Fatalf("期望 %d 天，实际 %d: %v", wantDays, len(dates), dates)
	}
	if dates[0] != wantFirst {
		t.Errorf("首日 = %q, want %q", dates[0], wantFirst)
	}
	if dates[len(dates)-1] != wantLast {
		t.Errorf("末日 = %q, want %q", dates[len(dates)-1], wantLast)
	}
}

// TestParseDateArgs_SingleMonth 单月 YYYYMM 展开为该自然月逐日。
func TestParseDateArgs_SingleMonth(t *testing.T) {
	dates, err := parseDateArgs([]string{"202608"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 31, "2026-08-01", "2026-08-31")
}

// TestParseDateArgs_MonthLengths 闰月 29 天、平月 28 天（月末由 AddDate 归一化推导）。
func TestParseDateArgs_MonthLengths(t *testing.T) {
	cases := []struct {
		in       string
		wantDays int
		wantLast string
	}{
		{"202402", 29, "2024-02-29"},
		{"202602", 28, "2026-02-28"},
	}
	for _, c := range cases {
		dates, err := parseDateArgs([]string{c.in}, true, "collect")
		if err != nil {
			t.Fatalf("parseDateArgs(%q) 出错: %v", c.in, err)
		}
		assertExpandedDates(t, dates, c.wantDays, c.in[0:4]+"-"+c.in[4:6]+"-01", c.wantLast)
	}
}

// TestParseDateArgs_SingleYear 单年 YYYY 展开为该自然年逐日；
// 闰年 366 天压线自洽（上限 366 不拒绝闰年单年）。
func TestParseDateArgs_SingleYear(t *testing.T) {
	cases := []struct {
		in       string
		wantDays int
		wantLast string
	}{
		{"2026", 365, "2026-12-31"},
		{"2024", 366, "2024-12-31"},
	}
	for _, c := range cases {
		dates, err := parseDateArgs([]string{c.in}, true, "collect")
		if err != nil {
			t.Fatalf("parseDateArgs(%q) 出错: %v", c.in, err)
		}
		assertExpandedDates(t, dates, c.wantDays, c.in[0:4]+"-01-01", c.wantLast)
	}
}

// TestParseDateArgs_YearBoundaries 年份边界：0000（闰年 366 天）与 9999（平年 365 天），
// 年末由 AddDate 归一化推导（与现有 8 位格式一致，年份不设上下限）。
func TestParseDateArgs_YearBoundaries(t *testing.T) {
	cases := []struct {
		in       string
		wantDays int
	}{
		{"0000", 366},
		{"9999", 365},
	}
	for _, c := range cases {
		dates, err := parseDateArgs([]string{c.in}, true, "collect")
		if err != nil {
			t.Fatalf("parseDateArgs(%q) 出错: %v", c.in, err)
		}
		assertExpandedDates(t, dates, c.wantDays, c.in[0:4]+"-01-01", c.in[0:4]+"-12-31")
	}
}

// TestParseDateArgs_MonthRange 月区间展开（同年两个月）。
func TestParseDateArgs_MonthRange(t *testing.T) {
	dates, err := parseDateArgs([]string{"202607-202608"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 62, "2026-07-01", "2026-08-31")
}

// TestParseDateArgs_CrossYearMonthRange 跨年月区间（30+31+31+28=120 天）。
func TestParseDateArgs_CrossYearMonthRange(t *testing.T) {
	dates, err := parseDateArgs([]string{"202511-202602"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 120, "2025-11-01", "2026-02-28")
}

// TestParseDateArgs_CrossYearDayRange 跨年日区间 365 天（上限内）。
func TestParseDateArgs_CrossYearDayRange(t *testing.T) {
	dates, err := parseDateArgs([]string{"20250601-20260531"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 365, "2025-06-01", "2026-05-31")
}

// TestParseDateArgs_LeapYearDayRangeGuard 366 天压线日区间回归守护
// （现状已合法的形态，实现前后均应通过，防收窄回归）。
func TestParseDateArgs_LeapYearDayRangeGuard(t *testing.T) {
	dates, err := parseDateArgs([]string{"20240101-20241231"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 366, "2024-01-01", "2024-12-31")
}

// TestParseDateArgs_LimitMonthRange 压线月区间 366 天（月端点写法）。
func TestParseDateArgs_LimitMonthRange(t *testing.T) {
	dates, err := parseDateArgs([]string{"202401-202412"}, true, "collect")
	if err != nil {
		t.Fatalf("parseDateArgs 出错: %v", err)
	}
	assertExpandedDates(t, dates, 366, "2024-01-01", "2024-12-31")
}

// TestParseDateArgs_YearAsRangeEndpointRejected 年做区间端点拒绝（含混合粒度写法），
// 文案说明年只接受单参数，并携带格式说明与命令示例。
func TestParseDateArgs_YearAsRangeEndpointRejected(t *testing.T) {
	for _, in := range []string{"2025-2026", "2026-202608", "20260801-2026"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("年端点区间 %q 应返回 error", in)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "only as a single arg") {
			t.Errorf("%q 错误应说明年只接受单参数（英文）: %q", in, msg)
		}
		if !strings.Contains(msg, "仅接受单独使用") {
			t.Errorf("%q 错误应说明年只接受单参数（中文）: %q", in, msg)
		}
		assertDateArgErrorContract(t, msg, in, "collect")
	}
}

// TestParseDateArgs_CalendarInvalidSingle 单参数日历非法（非法月/日）报
// not a valid date 句式。
func TestParseDateArgs_CalendarInvalidSingle(t *testing.T) {
	for _, in := range []string{"202613", "202600", "20260230"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("日历非法 %q 应返回 error", in)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "date is not a valid date") {
			t.Errorf("%q 错误应为 not a valid date 句式（英文）: %q", in, msg)
		}
		if !strings.Contains(msg, "日期不合法") {
			t.Errorf("%q 错误应为日期不合法句式（中文）: %q", in, msg)
		}
		assertDateArgErrorContract(t, msg, in, "collect")
	}
}

// TestParseDateArgs_CalendarInvalidRangeEndpoint 区间端点日历非法按
// start→end 顺序校验，首个非法端点即报错。
func TestParseDateArgs_CalendarInvalidRangeEndpoint(t *testing.T) {
	// end 侧非法。
	_, err := parseDateArgs([]string{"202608-202613"}, true, "collect")
	if err == nil {
		t.Fatal("非法月端点应返回 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "end date is not a valid date") {
		t.Errorf("应报 end 端点日历非法（英文）: %q", msg)
	}
	if !strings.Contains(msg, "结束日期不合法") {
		t.Errorf("应报 end 端点日历非法（中文）: %q", msg)
	}
	assertDateArgErrorContract(t, msg, "202608-202613", "collect")

	// start 侧非法：start→end 顺序，首个非法端点即报错。
	_, err = parseDateArgs([]string{"202613-202608"}, true, "collect")
	if err == nil {
		t.Fatal("非法月 start 端点应返回 error")
	}
	msg = err.Error()
	if !strings.Contains(msg, "start date is not a valid date") {
		t.Errorf("应报 start 端点日历非法（英文）: %q", msg)
	}
	if !strings.Contains(msg, "开始日期不合法") {
		t.Errorf("应报 start 端点日历非法（中文）: %q", msg)
	}
	assertDateArgErrorContract(t, msg, "202613-202608", "collect")
}

// TestParseDateArgs_LengthInvalid 长度非法（7 位/3 位/非数字）被拒绝。
func TestParseDateArgs_LengthInvalid(t *testing.T) {
	for _, in := range []string{"2026123", "202", "abc"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("长度非法 %q 应返回 error", in)
			continue
		}
		assertDateArgErrorContract(t, err.Error(), in, "collect")
	}
}

// TestParseDateArgs_RangeEndpointLengthInvalid 区间端点长度非法（非 6/8 位）
// 被拒绝：start 侧与 end 侧各一，start→end 顺序首个非法端点即报错。
func TestParseDateArgs_RangeEndpointLengthInvalid(t *testing.T) {
	for _, in := range []string{"20260-202608", "202608-20260"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("区间端点长度非法 %q 应返回 error", in)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "range endpoints must be YYYYMMDD (8 digits) or YYYYMM (6 digits)") {
			t.Errorf("%q 错误应为区间端点长度句式（英文）: %q", in, msg)
		}
		if !strings.Contains(msg, "区间端点应为 YYYYMMDD（8 位）或 YYYYMM（6 位）") {
			t.Errorf("%q 错误应为区间端点长度句式（中文）: %q", in, msg)
		}
		assertDateArgErrorContract(t, msg, in, "collect")
	}
}

// TestParseDateArgs_ISOFormStillRejected ISO 形态 2026-08-01 仍拒绝：
// 经 - 拆分后按 start→end 顺序先命中 start 端点 2026 的「年只接受单参数」错误。
func TestParseDateArgs_ISOFormStillRejected(t *testing.T) {
	_, err := parseDateArgs([]string{"2026-08-01"}, true, "collect")
	if err == nil {
		t.Fatal("ISO 形态应被拒绝")
	}
	msg := err.Error()
	if !strings.Contains(msg, "only as a single arg") {
		t.Errorf("ISO 形态应报 start 年端点错误（英文）: %q", msg)
	}
	if !strings.Contains(msg, "仅接受单独使用") {
		t.Errorf("ISO 形态应报 start 年端点错误（中文）: %q", msg)
	}
	assertDateArgErrorContract(t, msg, "2026-08-01", "collect")
}

// TestParseDateArgs_ReversedNormalizedRange 归一化后 end<start 被拒绝
// （月写法与日写法各一）。
func TestParseDateArgs_ReversedNormalizedRange(t *testing.T) {
	for _, in := range []string{"202608-202607", "20260901-20260831"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("反向区间 %q 应返回 error", in)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "must not be earlier than start date") {
			t.Errorf("%q 错误应为结束早于开始句式（英文）: %q", in, msg)
		}
		if !strings.Contains(msg, "不能早于开始日期") {
			t.Errorf("%q 错误应为结束早于开始句式（中文）: %q", in, msg)
		}
		assertDateArgErrorContract(t, msg, in, "collect")
	}
}

// TestParseDateArgs_ExpansionOverLimit 展开超 366 天被拒绝；文案给出实际
// 展开天数、上限、双语拆分指引、格式说明与命令示例。
func TestParseDateArgs_ExpansionOverLimit(t *testing.T) {
	for _, in := range []string{"20240101-20251231", "202401-202512"} {
		_, err := parseDateArgs([]string{in}, true, "collect")
		if err == nil {
			t.Errorf("超限区间 %q 应返回 error", in)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "expands to 731 days") {
			t.Errorf("%q 错误应含实际展开天数 731（英文）: %q", in, msg)
		}
		if !strings.Contains(msg, "展开为 731 天") {
			t.Errorf("%q 错误应含实际展开天数 731（中文）: %q", in, msg)
		}
		if !strings.Contains(msg, "366") {
			t.Errorf("%q 错误应含上限 366: %q", in, msg)
		}
		if !strings.Contains(msg, "split it into smaller ranges") {
			t.Errorf("%q 错误应含英文拆分指引: %q", in, msg)
		}
		if !strings.Contains(msg, "拆分") {
			t.Errorf("%q 错误应含中文拆分指引: %q", in, msg)
		}
		assertDateArgErrorContract(t, msg, in, "collect")
	}
}

// TestParseDateArgs_ExtremeRangeExactCount 极值区间拒绝：N 为精确 3652425 天
// （0000-01-01~9999-12-31，防 time.Duration 饱和的换算实现——饱和会报约
// 106752 天），且 err 路径不生成切片（上限判断先于切片生成）。
// N 在文案中按 %d 无千分位输出，断言子串不带逗号。
func TestParseDateArgs_ExtremeRangeExactCount(t *testing.T) {
	dates, err := parseDateArgs([]string{"000001-999912"}, true, "collect")
	if err == nil {
		t.Fatal("极值区间应返回 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "expands to 3652425 days") {
		t.Errorf("错误应含精确天数 3652425（英文）: %q", msg)
	}
	if !strings.Contains(msg, "展开为 3652425 天") {
		t.Errorf("错误应含精确天数 3652425（中文）: %q", msg)
	}
	if dates != nil {
		t.Errorf("err 路径不应生成切片，实际 %d 天", len(dates))
	}
	assertDateArgErrorContract(t, msg, "000001-999912", "collect")
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
