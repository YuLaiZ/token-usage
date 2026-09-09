package querier

import "time"

// CompareBaseWindow 按 <range> 粒度推导对比基线窗口：单日取前一天，单月取
// 上一个日历月（AddDate 归一化自动处理闰月），单年取上一个日历年；区间取
// 结束于开始日前一天的等长窗口。singleLen 为原始日期参数长度（8/6/4=单日/
// 单月/单年，0=区间）。compare/report 命令与 serve 仪表板共用本函数，保证
// 基线窗口口径单一来源。天数用纯 AddDate 循环计数，不用 end.Sub(start)
// 换算（time.Duration 约容 292 年，超长区间会饱和折损天数）。
func CompareBaseWindow(start, end time.Time, singleLen int) (time.Time, time.Time) {
	switch singleLen {
	case 8: // 单日：前一天
		prev := start.AddDate(0, 0, -1)
		return prev, prev
	case 6: // 单月：上一个日历月，月末由 AddDate 归一化推导
		first := start.AddDate(0, -1, 0)
		return first, first.AddDate(0, 1, -1)
	case 4: // 单年：上一个日历年
		return start.AddDate(-1, 0, 0), end.AddDate(-1, 0, 0)
	default: // 区间：结束于开始日前一天的等长窗口
		days := 0
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			days++
		}
		baseEnd := start.AddDate(0, 0, -1)
		return baseEnd.AddDate(0, 0, -(days - 1)), baseEnd
	}
}
