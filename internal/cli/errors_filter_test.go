package cli

import (
	"reflect"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
)

// TestBuildErrorsFilter 六种过滤组合的 Dates/Source/Unresolved 值。
// 覆盖 既定契约 第 6 条的六种组合，断言 ErrorFilter 的三个字段。
func TestBuildErrorsFilter(t *testing.T) {
	tests := []struct {
		name       string
		dates      []string // 传入 buildErrorsFilter 的归一化日期列表（nil 表示未指定）
		source     string
		unresolved bool // --unresolved flag
		want       db.ErrorFilter
	}{
		{
			name:   "裸 errors: 全部日期、仅未解决",
			dates:  nil,
			source: "",
			want:   db.ErrorFilter{Dates: nil, Source: "", Unresolved: true},
		},
		{
			name:   "单日: 指定日期、全部状态",
			dates:  []string{"2026-07-01"},
			source: "",
			want:   db.ErrorFilter{Dates: []string{"2026-07-01"}, Source: "", Unresolved: false},
		},
		{
			name:   "仅 source: 全部日期、全部状态",
			dates:  nil,
			source: "claude",
			want:   db.ErrorFilter{Dates: nil, Source: "claude", Unresolved: false},
		},
		{
			name:       "仅 unresolved: 全部日期、仅未解决",
			dates:      nil,
			source:     "",
			unresolved: true,
			want:       db.ErrorFilter{Dates: nil, Source: "", Unresolved: true},
		},
		{
			name:   "单日+source: 指定日期/source、全部状态",
			dates:  []string{"2026-07-01"},
			source: "claude",
			want:   db.ErrorFilter{Dates: []string{"2026-07-01"}, Source: "claude", Unresolved: false},
		},
		{
			name:       "单日+source+unresolved: 指定日期/source、仅未解决",
			dates:      []string{"2026-07-01"},
			source:     "claude",
			unresolved: true,
			want:       db.ErrorFilter{Dates: []string{"2026-07-01"}, Source: "claude", Unresolved: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildErrorsFilter(tt.dates, tt.source, tt.unresolved)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildErrorsFilter(%v,%q,%v) = %+v, want %+v",
					tt.dates, tt.source, tt.unresolved, got, tt.want)
			}
		})
	}
}
