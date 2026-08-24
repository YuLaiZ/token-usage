package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// parseDateArgs 解析 collect/query 的位置日期参数，返回 YYYY-MM-DD 格式的日期切片。
//
// 参数：
//   - args: 位置参数，允许 0 或 1 个；1 个时可为单日 YYYYMMDD 或闭区间 YYYYMMDD-YYYYMMDD。
//   - defaultToday: 无参数时的行为。true（collect/query）返回今天；false 返回空切片。
//   - cmdName: 调用方命令名（"collect"/"query"），用于生成正确的命令示例文案。
//
// 明确拒绝 YYYY-MM-DD 形式（破坏性收窄，统一为 YYYYMMDD 口径）。
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
			fmt.Sprintf("invalid date args: expected 0 or 1 positional arg, got %d. Accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", len(args), cmdName),
			fmt.Sprintf("无效的日期参数：仅接受 0 或 1 个位置参数，得到 %d 个。接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", len(args), cmdName),
		))
	}

	raw := args[0]
	if strings.Contains(raw, "-") {
		parts := strings.SplitN(raw, "-", 2)
		startStr, endStr := parts[0], parts[1]
		start, end, err := parseRangeEndpoints(startStr, endStr, raw, cmdName)
		if err != nil {
			return nil, err
		}
		if end.Before(start) {
			return nil, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("invalid date args %q: end date must not be earlier than start date. Accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", raw, cmdName),
				fmt.Sprintf("无效的日期参数 %q：结束日期不能早于开始日期。接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", raw, cmdName),
			))
		}
		var dates []string
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			dates = append(dates, d.Format("2006-01-02"))
		}
		return dates, nil
	}

	t, err := parseCompactDate(raw)
	if err != nil {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", raw, cmdName),
			fmt.Sprintf("无效的日期参数 %q：接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", raw, cmdName),
		))
	}
	return []string{t.Format("2006-01-02")}, nil
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

// parseRangeEndpoints 校验区间端点均为合法 YYYYMMDD。
// cmdName 用于生成正确的命令示例文案（如 collect/query）。
func parseRangeEndpoints(startStr, endStr, raw, cmdName string) (time.Time, time.Time, error) {
	if len(startStr) != 8 || len(endStr) != 8 {
		return time.Time{}, time.Time{}, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: range endpoints must be 8-digit YYYYMMDD. Accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", raw, cmdName),
			fmt.Sprintf("无效的日期参数 %q：区间端点应为 8 位 YYYYMMDD。接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", raw, cmdName),
		))
	}
	start, err := time.Parse("20060102", startStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: start date is not a valid date. Accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", raw, cmdName),
			fmt.Sprintf("无效的日期参数 %q：开始日期不合法。接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", raw, cmdName),
		))
	}
	end, err := time.Parse("20060102", endStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid date args %q: end date is not a valid date. Accepts YYYYMMDD or YYYYMMDD-YYYYMMDD, e.g. token-usage %s 20260701", raw, cmdName),
			fmt.Sprintf("无效的日期参数 %q：结束日期不合法。接受 YYYYMMDD 或 YYYYMMDD-YYYYMMDD，例如 token-usage %s 20260701", raw, cmdName),
		))
	}
	return start, end, nil
}
