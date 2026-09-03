package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// expandDaysLimit 是日期参数归一化区间展开为逐日列表的天数上限（含两端）。
// 366 = 一个闰年的长度，恰好容纳单闰年（如 2024）的全部合法形态；
// 更长的范围必须拆分多次执行。
const expandDaysLimit = 366

// dateFormatsHintEN/ZH 是所有日期参数错误共用的格式说明正文。
// 英文正文不含句首 "Accepts "/"accepts "（由各句式按大小写拼接）；
// 中文正文自带「接受」引导词。
const (
	dateFormatsHintEN = "YYYYMMDD (day), YYYYMM (month), or YYYY (year), or a day/month range like 20260701-20260831 or 202607-202608"
	dateFormatsHintZH = "接受 YYYYMMDD（日）、YYYYMM（月）或 YYYY（年），或日/月区间，如 20260701-20260831、202607-202608"
)

// parseDateArgs 解析 collect/query 的位置日期参数，返回 YYYY-MM-DD 格式的日期切片。
//
// 参数：
//   - args: 位置参数，允许 0 或 1 个；1 个时可为单日 YYYYMMDD、单月 YYYYMM、
//     单年 YYYY（年仅单独使用）或日/月闭区间（端点为 8 位日或 6 位月）。
//   - defaultToday: 无参数时的行为。true（collect/query）返回今天；false 返回空切片。
//   - cmdName: 调用方命令名（"collect"/"query"/"query custom"），用于生成正确的命令示例文案。
//
// 各形态先归一化为 [首日, 末日] 区间再统一逐日展开（含两端，展开上限 366 天，
// 更长范围须拆分多次执行）。明确拒绝 YYYY-MM-DD 形式（统一为紧凑数字口径）。
// 错误文案包含接受格式与命令示例。
func parseDateArgs(args []string, defaultToday bool, cmdName string) ([]string, error) {
	if len(args) == 0 {
		if !defaultToday {
			return nil, nil
		}
		return []string{time.Now().Format("2006-01-02")}, nil
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args: expected 0 or 1 positional arg, got %d. Accepts %s, e.g. token-usage %s 20260701", len(args), dateFormatsHintEN, cmdName),
			fmt.Sprintf("无效的日期参数：仅接受 0 或 1 个位置参数，得到 %d 个。%s，例如 token-usage %s 20260701", len(args), dateFormatsHintZH, cmdName),
		))
	}

	raw := args[0]
	var start, end time.Time
	if strings.Contains(raw, "-") {
		parts := strings.SplitN(raw, "-", 2)
		startStr, endStr := parts[0], parts[1]
		// 形态校验（长度）按 start→end 顺序，首个不合法端点即报错；4 位年
		// 在此报「年只接受单参数」（含 ISO 形态如 2026-08-01 拆分后
		// start=2026 的场景）。
		for _, ep := range []string{startStr, endStr} {
			if len(ep) == 4 {
				return nil, yearEndpointError(raw, cmdName)
			}
			if len(ep) != 6 && len(ep) != 8 {
				return nil, rangeEndpointLengthError(raw, cmdName)
			}
		}
		startFirst, _, err := parseDateEndpoint(startStr)
		if err != nil {
			return nil, rangeEndpointCalendarError("start", raw, cmdName)
		}
		_, endLast, err := parseDateEndpoint(endStr)
		if err != nil {
			return nil, rangeEndpointCalendarError("end", raw, cmdName)
		}
		start, end = startFirst, endLast
		if end.Before(start) {
			return nil, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("invalid date args %q: end date must not be earlier than start date. Accepts %s, e.g. token-usage %s 20260701", raw, dateFormatsHintEN, cmdName),
				fmt.Sprintf("无效的日期参数 %q：结束日期不能早于开始日期。%s，例如 token-usage %s 20260701", raw, dateFormatsHintZH, cmdName),
			))
		}
	} else {
		// 单参数允许 4/6/8 位（年/月/日粒度）；年只在此路径合法。
		if l := len(raw); l != 4 && l != 6 && l != 8 {
			return nil, singleArgLengthError(raw, cmdName)
		}
		first, last, err := parseDateEndpoint(raw)
		if err != nil {
			return nil, singleArgCalendarError(raw, cmdName)
		}
		start, end = first, last
	}

	// 上限判断先于切片生成：用纯 AddDate 计数循环数出精确天数（不用
	// end.Sub(start) 换算——time.Duration 约容 292 年，万年区间会饱和折损
	// 实际天数），超限直接报错、不分配切片。
	days := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days++
	}
	if days > expandDaysLimit {
		return nil, expansionOverLimitError(raw, days, cmdName)
	}
	var dates []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	return dates, nil
}

// parseDateEndpoint 把单个日期端点解析为归一化区间 [first, last]：
// 8 位日为 [当日, 当日]；6 位月为 [当月 1 日, 当月末日]；4 位年为
// [当年 1 月 1 日, 当年 12 月 31 日]。周期末日由 AddDate 归一化推导
// （闰年由 time 包处理）。调用方已按上下文完成长度校验；日历非法值
// （如 202613、20260230）由 time.Parse 拒绝。
func parseDateEndpoint(s string) (time.Time, time.Time, error) {
	var first, last time.Time
	var err error
	switch len(s) {
	case 8:
		first, err = time.Parse("20060102", s)
		last = first
	case 6:
		first, err = time.Parse("200601", s)
		if err == nil {
			last = first.AddDate(0, 1, -1)
		}
	case 4:
		first, err = time.Parse("2006", s)
		if err == nil {
			last = first.AddDate(1, 0, -1)
		}
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported date endpoint length: %d", len(s))
	}
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return first, last, nil
}

// singleArgLengthError 单参数长度非法（非 4/6/8 位）。
func singleArgLengthError(raw, cmdName string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: accepts %s, e.g. token-usage %s 20260701", raw, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：%s，例如 token-usage %s 20260701", raw, dateFormatsHintZH, cmdName),
	))
}

// singleArgCalendarError 单参数日历非法（与区间端点统一 not a valid date 句式）。
func singleArgCalendarError(raw, cmdName string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: date is not a valid date. Accepts %s, e.g. token-usage %s 20260701", raw, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：日期不合法。%s，例如 token-usage %s 20260701", raw, dateFormatsHintZH, cmdName),
	))
}

// yearEndpointError 4 位年做区间端点：年只接受单参数。只告知端点可用形态，
// 不给替代写法——同一 4 位端点可能来自可等价转换的区间或超限区间，拆分
// 指引统一由超限文案承担。
func yearEndpointError(raw, cmdName string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: a year (YYYY) is accepted only as a single arg; range endpoints must be YYYYMMDD or YYYYMM. Accepts %s, e.g. token-usage %s 20260701", raw, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：年（YYYY）仅接受单独使用；区间端点应为 YYYYMMDD 或 YYYYMM。%s，例如 token-usage %s 20260701", raw, dateFormatsHintZH, cmdName),
	))
}

// rangeEndpointLengthError 区间端点长度非法（4 位年之外的其他非法长度）。
func rangeEndpointLengthError(raw, cmdName string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: range endpoints must be YYYYMMDD (8 digits) or YYYYMM (6 digits). Accepts %s, e.g. token-usage %s 20260701", raw, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：区间端点应为 YYYYMMDD（8 位）或 YYYYMM（6 位）。%s，例如 token-usage %s 20260701", raw, dateFormatsHintZH, cmdName),
	))
}

// rangeEndpointCalendarError 区间端点日历非法，which 为 "start"/"end"
// 定位首个非法端点（按 start→end 顺序校验）。
func rangeEndpointCalendarError(which, raw, cmdName string) error {
	whichZH := "开始"
	if which == "end" {
		whichZH = "结束"
	}
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: %s date is not a valid date. Accepts %s, e.g. token-usage %s 20260701", raw, which, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：%s日期不合法。%s，例如 token-usage %s 20260701", raw, whichZH, dateFormatsHintZH, cmdName),
	))
}

// expansionOverLimitError 归一化区间展开超上限（含两端计数的精确天数）。
func expansionOverLimitError(raw string, days int, cmdName string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid date args %q: range expands to %d days, exceeding the limit of %d days; split it into smaller ranges. Accepts %s, e.g. token-usage %s 20260701", raw, days, expandDaysLimit, dateFormatsHintEN, cmdName),
		fmt.Sprintf("无效的日期参数 %q：区间展开为 %d 天，超过 %d 天上限；请拆分为较小的区间分次查询。%s，例如 token-usage %s 20260701", raw, days, expandDaysLimit, dateFormatsHintZH, cmdName),
	))
}

// parseErrorDateArg 解析 errors 命令的位置日期参数。
//
// 只接受 0 或 1 个 YYYYMMDD（8 位），返回单个 YYYY-MM-DD 字符串；无参数返回 ("", nil)。
// 明确拒绝 YYYY-MM-DD、范围、多余参数（破坏性收窄）。
func parseErrorDateArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) > 1 {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args: expected 0 or 1 positional arg, got %d. Accepts YYYYMMDD (8 digits) only, e.g. token-usage errors 20260701", len(args)),
			fmt.Sprintf("无效的日期参数：仅接受 0 或 1 个位置参数，得到 %d 个。只接受 YYYYMMDD（8 位），例如 token-usage errors 20260701", len(args)),
		))
	}
	raw := args[0]
	// 拒绝范围与含连字符的形式。
	if strings.Contains(raw, "-") {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: accepts YYYYMMDD (8 digits) only, e.g. token-usage errors 20260701", raw),
			fmt.Sprintf("无效的日期参数 %q：只接受 YYYYMMDD（8 位），例如 token-usage errors 20260701", raw),
		))
	}
	t, err := parseCompactDate(raw)
	if err != nil {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: accepts YYYYMMDD (8 digits) only, e.g. token-usage errors 20260701", raw),
			fmt.Sprintf("无效的日期参数 %q：只接受 YYYYMMDD（8 位），例如 token-usage errors 20260701", raw),
		))
	}
	return t.Format("2006-01-02"), nil
}

// parseCompactDate 解析 8 位 YYYYMMDD，要求严格匹配长度与日历。
func parseCompactDate(s string) (time.Time, error) {
	if len(s) != 8 {
		return time.Time{}, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("date must be 8-digit YYYYMMDD: %q", s),
			fmt.Sprintf("日期长度应为 8 位 YYYYMMDD: %q", s),
		))
	}
	return time.Parse("20060102", s)
}
