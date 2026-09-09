// Package fmtx 提供 compare 口径的数字与百分比格式化助手,供 cli(compare/
// report 命令与静态报告页)与 web(serve 仪表板 compare 区块)共用,保证
// 各入口的显示串与着色 class 单一来源。包名 fmtx 取「format extensions」:
// 依赖 querier.FormatTokens 的缩写口径,故不并入 ui(ui 不得反向依赖 querier)。
package fmtx

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/querier"
)

// Thousands 把整数渲染为千分位分组文本(含负数),用于需要精确值的场景
// (KPI 精确副值、计数列、数据明细单元格);负数按 uint64 取绝对值,避免
// MinInt64 取负溢出。
func Thousands(n int64) string {
	sign := ""
	u := uint64(n)
	if n < 0 {
		sign = "-"
		u = uint64(-(n + 1)) + 1
	}
	digits := strconv.FormatUint(u, 10)
	var b strings.Builder
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return sign + b.String()
}

// SignedTokens 渲染带符号的 token 差值：正数前缀 "+"、负数前缀 "-"、
// 零显示 "0"；幅值经 uint64 取绝对值（避免 MinInt64 取负溢出）后复用
// querier.FormatTokensUnsigned，缩写阈值与 query 表格完全一致。
func SignedTokens(diff int64) string {
	if diff == 0 {
		return "0"
	}
	mag := uint64(diff)
	if diff < 0 {
		mag = uint64(-(diff + 1)) + 1
	}
	magnitude := querier.FormatTokensUnsigned(mag)
	if diff > 0 {
		return "+" + magnitude
	}
	return "-" + magnitude
}

// CountChange 渲染计数差值：0 显示 "0"，非 0 用 %+d 自带符号。
func CountChange(diff int64) string {
	if diff == 0 {
		return "0"
	}
	return fmt.Sprintf("%+d", diff)
}

// ChangePercentValue 计算两期变化的百分比数值：四舍五入到 1 位小数。表格
// ChangePercent 与 compare JSON 的 change_percent 共用该函数，保证两个表面
// 同一舍入口径。base == 0 时百分比无定义，返回 ok=false（与表格 "--"、
// JSON null 同语义）。
func ChangePercentValue(cur, base int64) (float64, bool) {
	if base == 0 {
		return 0, false
	}
	pct := float64(cur-base) / float64(base) * 100
	return math.Round(pct*10) / 10, true
}

// ChangePercent 渲染变化百分比：基线为 0 时百分比无定义，显示 "--"；
// 数值部分经 ChangePercentValue 与 JSON 同口径舍入。符号按原始差值判断
// （base > 0 时与未舍入百分比同号）：极小正百分比舍入后为 0.0 仍保留
// "+"（+0.0%），负数由 %.1f 自带 "-"，恰好持平为 "0.0%"。
func ChangePercent(cur, base int64) string {
	pct, ok := ChangePercentValue(cur, base)
	if !ok {
		return "--"
	}
	switch {
	case cur > base:
		return fmt.Sprintf("+%.1f%%", pct)
	case cur < base:
		return fmt.Sprintf("%.1f%%", pct)
	default:
		return "0.0%"
	}
}

// ChangeClass 返回差值符号对应的表格单元格 class:正 pos、负 neg、持平无色,
// 一并携带 num 右对齐(数值列统一右对齐,着色仅叠加颜色)。
func ChangeClass(diff int64) string {
	switch {
	case diff > 0:
		return "num pos"
	case diff < 0:
		return "num neg"
	default:
		return "num"
	}
}
