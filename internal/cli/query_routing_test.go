package cli

import "testing"

// TestRunQuery_RangeScanRouting 守护“仅多表进入单遍扫描”的路由契约：
// 默认单维、单个自定义视图、单成员 group 都不得触发 ScanRange；多表 group
// 必须触发一次。删除单表分支会让本测试失败，从而防止“输出一致但性能回退”
// 的回归只靠文本断言漏过。
func TestRunQuery_RangeScanRouting(t *testing.T) {
	open := memOpen(t)
	var calls int
	observer := func() { calls++ }
	rangeScanObserver.Store(&observer)
	t.Cleanup(func() { rangeScanObserver.Store(nil) })

	cases := []struct {
		name string
		raw  map[string]any
		want int
	}{
		{
			name: "default single dimension",
			raw:  map[string]any{"default": "provider"},
			want: 0,
		},
		{
			name: "single custom view",
			raw: map[string]any{
				"default":    "mpc",
				"subqueries": map[string]any{"mpc": "model,provider,client"},
			},
			want: 0,
		},
		// querydef 要求 group 至少两个成员，单成员 group 在配置层即被拒绝，
		// 不存在“只展开一张表的 group”；单表路径由前两个用例覆盖。
		{
			name: "multi-member group",
			raw: map[string]any{
				"default": "two",
				"groups":  map[string]any{"two": "client,model"},
			},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			cmd, _ := newQueryOutputCmd()
			if err := runQueryWithDeps(cmd, []string{"20260709"}, viewDefault, loadWithRaw(tc.raw, nil), open); err != nil {
				t.Fatalf("run default: %v", err)
			}
			if calls != tc.want {
				t.Fatalf("range scan calls = %d, want %d", calls, tc.want)
			}
		})
	}
}
