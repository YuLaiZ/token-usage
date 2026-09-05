package cli

import "testing"

// FuzzParseDateArgs 锁定 parseDateArgs 对任意输入的成功路径不变量:
// 成功解析时返回非空、严格升序去重的连续逐日列表,每个元素为 YYYY-MM-DD
// 形态(字典序即时间序),天数不超过展开上限;失败路径只要求返回 error
// 而不 panic。解析器被 collect/query/errors 等命令共用,此不变量是全部
// 调用方的公共合同。
func FuzzParseDateArgs(f *testing.F) {
	f.Add("20260701")
	f.Add("20260701-20260731")
	f.Add("202602")
	f.Add("2024") // 闰年
	f.Add("2026")
	f.Add("2026-07-01")
	f.Add("bad")
	f.Add("")
	f.Add("00000000")
	f.Add("99991231-00000101")
	f.Add("20260701-20260701")
	f.Add("20241231-20250101")

	f.Fuzz(func(t *testing.T, raw string) {
		dates, err := parseDateArgs([]string{raw}, false, "fuzz")
		if err != nil {
			return
		}
		if len(dates) == 0 {
			t.Fatalf("解析成功但返回空列表: %q", raw)
		}
		if len(dates) > expandDaysLimit {
			t.Fatalf("展开 %d 天超过上限 %d: %q", len(dates), expandDaysLimit, raw)
		}
		for i, d := range dates {
			if len(d) != 10 || d[4] != '-' || d[7] != '-' {
				t.Fatalf("第 %d 个元素 %q 非 YYYY-MM-DD 形态: %q", i, d, raw)
			}
		}
		for i := 1; i < len(dates); i++ {
			// 字符串比较同时覆盖「升序」与「去重」:YYYY-MM-DD 字典序即时间序。
			if dates[i] <= dates[i-1] {
				t.Fatalf("日期序列在 %d 处非严格升序: %v (输入 %q)", i, dates, raw)
			}
		}
	})
}
