package engine

import "testing"

// 日志摘要形态：空/单个/多个（数量+首尾）。错误表登记不受影响（完整集合
// 仍传 RecordErrorsByDate）。
func TestSummarizeDates(t *testing.T) {
	cases := []struct {
		dates []string
		want  string
	}{
		{nil, "0"},
		{[]string{}, "0"},
		{[]string{"2026-10-09"}, "2026-10-09"},
		{[]string{"2026-06-01", "2026-06-02", "2026-10-07"}, "3(2026-06-01..2026-10-07)"},
		{[]string{"2026-06-01", "2026-10-07"}, "2(2026-06-01..2026-10-07)"},
	}
	for _, tc := range cases {
		if got := summarizeDates(tc.dates); got != tc.want {
			t.Errorf("summarizeDates(%v) = %q, want %q", tc.dates, got, tc.want)
		}
	}
}
