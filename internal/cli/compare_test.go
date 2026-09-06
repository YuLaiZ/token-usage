package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// TestParseCompareArgs_DefaultBase 表驱动覆盖各粒度形态的 [start,end] 精确
// 断言与缺省基线推导（含闰月 202403 → 02-29、区间等长前置窗口）。
func TestParseCompareArgs_DefaultBase(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCur  [2]string
		wantBase [2]string
	}{
		{"single day", "20260907",
			[2]string{"2026-09-07", "2026-09-07"}, [2]string{"2026-09-06", "2026-09-06"}},
		{"single month", "202609",
			[2]string{"2026-09-01", "2026-09-30"}, [2]string{"2026-08-01", "2026-08-31"}},
		{"month after leap february", "202403",
			[2]string{"2024-03-01", "2024-03-31"}, [2]string{"2024-02-01", "2024-02-29"}},
		{"single year", "2026",
			[2]string{"2026-01-01", "2026-12-31"}, [2]string{"2025-01-01", "2025-12-31"}},
		{"day range equal-length base", "20260701-20260710",
			[2]string{"2026-07-01", "2026-07-10"}, [2]string{"2026-06-21", "2026-06-30"}},
		{"mixed month-day endpoints", "202607-20260710",
			[2]string{"2026-07-01", "2026-07-10"}, [2]string{"2026-06-21", "2026-06-30"}},
		{"multi-year range without 366 cap", "202401-202512",
			[2]string{"2024-01-01", "2025-12-31"}, [2]string{"2021-12-31", "2023-12-31"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs(tc.raw, "")
			if err != nil {
				t.Fatalf("parseCompareArgs(%q) 出错: %v", tc.raw, err)
			}
			assertCompareWindow(t, "current", curStart, curEnd, tc.wantCur)
			assertCompareWindow(t, "base", baseStart, baseEnd, tc.wantBase)
		})
	}
}

// TestParseCompareArgs_ExplicitBase 显式 --base 用同一解析器且不做粒度耦合：
// 单日当前窗口可配单月基线，区间可配与其重叠的区间基线。
func TestParseCompareArgs_ExplicitBase(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		base     string
		wantCur  [2]string
		wantBase [2]string
	}{
		{"day current with month base", "20260907", "202609",
			[2]string{"2026-09-07", "2026-09-07"}, [2]string{"2026-09-01", "2026-09-30"}},
		{"overlapping equal windows", "20260901-20260907", "20260901-20260907",
			[2]string{"2026-09-01", "2026-09-07"}, [2]string{"2026-09-01", "2026-09-07"}},
		{"range with single-day base", "20260901-20260907", "20260831",
			[2]string{"2026-09-01", "2026-09-07"}, [2]string{"2026-08-31", "2026-08-31"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs(tc.raw, tc.base)
			if err != nil {
				t.Fatalf("parseCompareArgs(%q, %q) 出错: %v", tc.raw, tc.base, err)
			}
			assertCompareWindow(t, "current", curStart, curEnd, tc.wantCur)
			assertCompareWindow(t, "base", baseStart, baseEnd, tc.wantBase)
		})
	}
}

// TestParseCompareArgs_Errors 负向形态：ISO 破折号、年做区间端点、end<start、
// 非法长度、非法日历值；--base 非法同样拒绝。断言错误双语关键句式，保证
// 各分支命中预期的错误辅助函数而非笼统报错。
func TestParseCompareArgs_Errors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		base string
		// wantEn/wantZh 为该错误句式的双语区分片段。
		wantEn, wantZh string
	}{
		{"ISO dashed single", "2026-08-01", "",
			"only as a single arg", "仅接受单独使用"},
		{"year as range endpoint", "2026-202607", "",
			"only as a single arg", "仅接受单独使用"},
		{"end before start", "20260710-20260701", "",
			"must not be earlier than start date", "不能早于开始日期"},
		{"illegal single length", "20260", "",
			"accepts YYYYMMDD (day)", "接受 YYYYMMDD（日）"},
		{"illegal range endpoint length", "202607011-20260710", "",
			"range endpoints must be YYYYMMDD (8 digits) or YYYYMM (6 digits)", "区间端点应为 YYYYMMDD（8 位）或 YYYYMM（6 位）"},
		{"illegal single calendar", "202613", "",
			"date is not a valid date", "日期不合法"},
		{"illegal range start calendar", "20261301-20260710", "",
			"start date is not a valid date", "开始日期不合法"},
		{"illegal range end calendar", "20260701-20261301", "",
			"end date is not a valid date", "结束日期不合法"},
		{"illegal explicit base", "20260901", "202613",
			"date is not a valid date", "日期不合法"},
		{"ISO dashed explicit base", "20260901", "2026-08-01",
			"only as a single arg", "仅接受单独使用"},
		{"explicit base end before start", "20260901-20260907", "20260810-20260801",
			"must not be earlier than start date", "不能早于开始日期"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := parseCompareArgs(tc.raw, tc.base)
			if err == nil {
				t.Fatalf("parseCompareArgs(%q, %q) 应返回 error", tc.raw, tc.base)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantEn) {
				t.Errorf("错误应含英文句式 %q，实际 %q", tc.wantEn, msg)
			}
			if !strings.Contains(msg, tc.wantZh) {
				t.Errorf("错误应含中文句式 %q，实际 %q", tc.wantZh, msg)
			}
			// 命令示例统一为 compare（cmdName 贯穿）。
			if !strings.Contains(msg, "token-usage compare 20260701") {
				t.Errorf("错误应含命令示例 token-usage compare 20260701，实际 %q", msg)
			}
		})
	}
}

// assertCompareWindow 断言一个窗口的起止日期（YYYY-MM-DD）。
func assertCompareWindow(t *testing.T, which string, start, end time.Time, want [2]string) {
	t.Helper()
	if got := start.Format("2006-01-02"); got != want[0] {
		t.Errorf("%s start = %q, want %q", which, got, want[0])
	}
	if got := end.Format("2006-01-02"); got != want[1] {
		t.Errorf("%s end = %q, want %q", which, got, want[1])
	}
}

// TestRenderCompare_FullData 全量数据逐行断言 Current/Base/Change/Change%：
// 覆盖下降（负百分比）、持平（0.0%）、base==0&&cur>0（--）、双零（--）、
// 计数行 %+d 与 token 行带符号 K 缩写。
func TestRenderCompare_FullData(t *testing.T) {
	in := compareRenderInput{
		curStart: "2026-09-01", curEnd: "2026-09-07",
		baseStart: "2026-08-25", baseEnd: "2026-08-31",
		cur: querier.RangeStats{
			ActiveDays: 7,
			Total: querier.GroupAggregate{
				Requests: 70, FreshInput: 7000, OutputTokens: 4000,
				CacheRead: 3000, CacheCreate: 0, Reasoning: 2500, TotalTokens: 7800,
			},
		},
		base: querier.RangeStats{
			ActiveDays: 5,
			Total: querier.GroupAggregate{
				Requests: 70, FreshInput: 3000, OutputTokens: 5000,
				CacheRead: 0, CacheCreate: 0, Reasoning: 5000, TotalTokens: 8000,
			},
		},
	}
	var buf bytes.Buffer
	if err := renderCompare(&buf, in); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 标题与两个窗口起止行。
	for _, want := range []string{
		"Compare / 用量对比",
		"Current / 当前: 2026-09-01 .. 2026-09-07",
		"Base / 基线: 2026-08-25 .. 2026-08-31",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q:\n%s", want, out)
		}
	}
	// 表头为双语两行，不含 Δ 等 ambiguous-width 字符，保证 CJK 框线对齐。
	if strings.ContainsAny(out, "Δ≈×") {
		t.Errorf("表头不得使用 ambiguous-width 字符:\n%s", out)
	}
	for _, want := range []string{"Metric", "指标", "Current", "当前", "Base", "基线", "Change %", "变化%"} {
		if !strings.Contains(out, want) {
			t.Errorf("表头缺少 %q:\n%s", want, out)
		}
	}

	// 逐行精确断言（按 │ 切分并去除边框空段与单元格填充空格）。
	rows := map[string][]string{
		"Active days / 活跃天":   {"Active days / 活跃天", "7", "5", "+2", "+40.0%"},
		"Requests / 请求数":      {"Requests / 请求数", "70", "70", "0", "0.0%"},
		"Input / 输入":          {"Input / 输入", "7.00 K", "3.00 K", "+4.00 K", "+133.3%"},
		"Output / 输出":         {"Output / 输出", "4.00 K", "5.00 K", "-1.00 K", "-20.0%"},
		"Cache Read / 缓存读取":   {"Cache Read / 缓存读取", "3.00 K", "0", "+3.00 K", "--"},
		"Cache Create / 缓存创建": {"Cache Create / 缓存创建", "0", "0", "0", "--"},
		"Reasoning / 推理":      {"Reasoning / 推理", "2.50 K", "5.00 K", "-2.50 K", "-50.0%"},
		"Total / 总计":          {"Total / 总计", "7.80 K", "8.00 K", "-200", "-2.5%"},
	}
	for label, want := range rows {
		got := tableRowCells(t, out, label)
		if len(got) != len(want) {
			t.Fatalf("行 %q 应有 %d 列，实际 %d: %v", label, len(want), len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("行 %q 第 %d 列 = %q, want %q", label, i, got[i], want[i])
			}
		}
	}
}

// tableRowCells 在表格输出中定位含 label 的数据行，按 │ 切分并去除边框空段
// 与单元格填充空格后返回单元格序列。
func tableRowCells(t *testing.T, out, label string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, label) {
			continue
		}
		parts := strings.Split(line, "│")
		if len(parts) < 3 {
			t.Fatalf("行 %q 不是框线表格行: %q", label, line)
		}
		cells := make([]string, 0, len(parts)-2)
		for _, p := range parts[1 : len(parts)-1] {
			cells = append(cells, strings.TrimSpace(p))
		}
		return cells
	}
	t.Fatalf("输出缺少包含 %q 的表格行:\n%s", label, out)
	return nil
}

// TestFormatSignedTokens 0、正、负与跨 K/M 阈值（阈值与 formatTokens 完全一致）。
func TestFormatSignedTokens(t *testing.T) {
	cases := []struct {
		diff int64
		want string
	}{
		{0, "0"},
		{999, "+999"},
		{1000, "+1.00 K"},
		{1500, "+1.50 K"},
		{-4000, "-4.00 K"},
		{1200000, "+1.20 M"},
		{-2500000, "-2.50 M"},
		{-7, "-7"},
	}
	for _, tc := range cases {
		if got := formatSignedTokens(tc.diff); got != tc.want {
			t.Errorf("formatSignedTokens(%d) = %q, want %q", tc.diff, got, tc.want)
		}
	}
}

// TestFormatChangePercent_NearZeroKeepsSign 钉住极小正百分比的边界行为：
// 四舍五入到一位小数后为 0.0 但并非恰好持平，仍保留 "+" 符号（+0.0%）。
func TestFormatChangePercent_NearZeroKeepsSign(t *testing.T) {
	if got := formatChangePercent(1000001, 1000000); got != "+0.0%" {
		t.Errorf("formatChangePercent(1000001, 1000000) = %q, want %q", got, "+0.0%")
	}
}

// TestCompareCmd_EndToEnd 真实调用链：内存库注入两窗口数据，缺省基线按区间
// 等长前置推导；--base 显式单日基线空数据时变化% 显示 --。
func TestCompareCmd_EndToEnd(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmp-cur-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmp-cur-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(), TotalTokens: 400},
		{ID: "cmp-base", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-30", TS: day("2026-08-30", 0).UnixMilli(), TotalTokens: 750},
	}
	// seededOpen 每次调用开一个全新内存库并注入同一批消息：命令 RunE 会
	// defer Close 注入的库，多次 Execute 必须各自持有独立实例。
	seededOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	load := func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil }

	// 缺省基线：20260901-20260907 → 前置等长 2026-08-25..2026-08-31。
	cmd := newCompareCmdWithDeps(load, seededOpen)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260901-20260907"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Current / 当前: 2026-09-01 .. 2026-09-07") ||
		!strings.Contains(out, "Base / 基线: 2026-08-25 .. 2026-08-31") {
		t.Errorf("窗口行不符合缺省等长前置推导:\n%s", out)
	}
	// 当前 1000、基线 750：变化 +250、+33.3%（注入数据必须真实进入查询）。
	totalCells := tableRowCells(t, out, "Total / 总计")
	wantTotal := []string{"Total / 总计", "1.00 K", "750", "+250", "+33.3%"}
	for i := range wantTotal {
		if totalCells[i] != wantTotal[i] {
			t.Errorf("Total 行第 %d 列 = %q, want %q\n%s", i, totalCells[i], wantTotal[i], out)
		}
	}
	// 活跃天：当前 2、基线 1。
	activeCells := tableRowCells(t, out, "Active days / 活跃天")
	if activeCells[1] != "2" || activeCells[2] != "1" || activeCells[3] != "+1" {
		t.Errorf("Active days 行应为 2/1/+1，实际 %v\n%s", activeCells, out)
	}

	// 显式 --base 单日空数据：变化% 显示 --，变化为 +1.00 K。
	cmd2 := newCompareCmdWithDeps(load, seededOpen)
	var buf2 bytes.Buffer
	cmd2.SetOut(&buf2)
	cmd2.SetErr(&buf2)
	cmd2.SetArgs([]string{"20260901-20260907", "--base", "20260831"})
	if err := cmd2.Execute(); err != nil {
		t.Fatal(err)
	}
	out2 := buf2.String()
	if !strings.Contains(out2, "Base / 基线: 2026-08-31 .. 2026-08-31") {
		t.Errorf("--base 应为单日窗口:\n%s", out2)
	}
	totalCells2 := tableRowCells(t, out2, "Total / 总计")
	wantTotal2 := []string{"Total / 总计", "1.00 K", "0", "+1.00 K", "--"}
	for i := range wantTotal2 {
		if totalCells2[i] != wantTotal2[i] {
			t.Errorf("--base Total 行第 %d 列 = %q, want %q\n%s", i, totalCells2[i], wantTotal2[i], out2)
		}
	}
}

// TestCompareCmd_RequiresExactlyOneArg Args 校验：数量非 1 时手写双语错误。
func TestCompareCmd_RequiresExactlyOneArg(t *testing.T) {
	for _, args := range [][]string{{}, {"20260901", "20260902"}} {
		cmd := newCompareCmdWithDeps(
			func() (*config.Config, error) { t.Fatal("参数错误不应加载配置"); return nil, nil },
			func(string) (*db.DB, error) { t.Fatal("参数错误不应打开数据库"); return nil, nil },
		)
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("args %v 应返回 error", args)
		}
		msg := err.Error()
		if !strings.Contains(msg, "exactly 1 positional arg") || !strings.Contains(msg, "恰好 1 个位置参数") {
			t.Errorf("args %v 错误应双语说明参数数量，实际 %q", args, msg)
		}
	}
}

// testDay 解析 YYYY-MM-DD 为 UTC 日期，供纯函数测试构造区间端点。
func testDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("解析测试日期 %q 失败: %v", s, err)
	}
	return d
}

// TestExpandRangeDays 闭区间逐日展开：多日、单日（start==end）、跨月边界、
// end 早于 start 返回空。
func TestExpandRangeDays(t *testing.T) {
	cases := []struct {
		name  string
		start string
		end   string
		want  []string
	}{
		{"three days", "2026-09-01", "2026-09-03",
			[]string{"2026-09-01", "2026-09-02", "2026-09-03"}},
		{"single day", "2026-09-05", "2026-09-05",
			[]string{"2026-09-05"}},
		{"month boundary", "2026-08-31", "2026-09-01",
			[]string{"2026-08-31", "2026-09-01"}},
		{"year boundary", "2026-12-31", "2027-01-01",
			[]string{"2026-12-31", "2027-01-01"}},
		{"end before start", "2026-09-03", "2026-09-01", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandRangeDays(testDay(t, tc.start), testDay(t, tc.end))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("expandRangeDays(%s, %s) = %v, want %v", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

// TestChunkDays 表驱动覆盖分块：7 天按 3 → [3,3,1]、恰好整块 → [3,3]、
// 单日（start==end）、块大于区间、size=1；size<=0 的防御分支视为整段一块
// （空区间返回 nil，不 panic 不死循环）；并断言展平后与整段展开逐元素
// 相等（块内连续、跨块无缝不重）。
func TestChunkDays(t *testing.T) {
	cases := []struct {
		name     string
		start    string
		end      string
		size     int
		wantLens []int
	}{
		{"7 days size 3", "2026-09-01", "2026-09-07", 3, []int{3, 3, 1}},
		{"exact multiple 6 days size 3", "2026-09-01", "2026-09-06", 3, []int{3, 3}},
		{"single day start==end", "2026-09-05", "2026-09-05", 3, []int{1}},
		{"chunk larger than range", "2026-09-01", "2026-09-02", 366, []int{2}},
		{"size 1 splits per day", "2026-09-01", "2026-09-03", 1, []int{1, 1, 1}},
		{"leap year exactly one chunk", "2024-01-01", "2024-12-31", 366, []int{366}},
		{"size 0 whole range one chunk", "2026-09-01", "2026-09-03", 0, []int{3}},
		{"negative size whole range one chunk", "2026-09-01", "2026-09-02", -1, []int{2}},
		{"size 0 empty range no chunks", "2026-09-03", "2026-09-01", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := testDay(t, tc.start), testDay(t, tc.end)
			chunks := chunkDays(start, end, tc.size)
			if len(chunks) != len(tc.wantLens) {
				t.Fatalf("chunkDays 应分 %d 块，实际 %d 块: %v", len(tc.wantLens), len(chunks), chunks)
			}
			flat := make([]string, 0, len(tc.wantLens))
			for i, c := range chunks {
				if len(c) != tc.wantLens[i] {
					t.Errorf("第 %d 块长度 = %d, want %d", i, len(c), tc.wantLens[i])
				}
				flat = append(flat, c...)
			}
			if want := expandRangeDays(start, end); !reflect.DeepEqual(flat, want) {
				t.Errorf("展平分块 %v 应与整段展开 %v 逐元素相等（块内连续、跨块无缝不重）", flat, want)
			}
		})
	}
}

// TestMergeCompareByMembers 两期不同成员集合并：A 只在当前期、B 只在基线期
// （缺失侧零值）、C 两期都有（各期独立保留）。
func TestMergeCompareByMembers(t *testing.T) {
	cur := map[string]querier.GroupAggregate{
		"A": {Requests: 1, TotalTokens: 100},
		"C": {Requests: 1, TotalTokens: 50},
	}
	base := map[string]querier.GroupAggregate{
		"B": {Requests: 1, TotalTokens: 80},
		"C": {Requests: 1, TotalTokens: 70},
	}
	members := mergeCompareByMembers(cur, base)
	if len(members) != 3 {
		t.Fatalf("应合并为 3 个成员（A/B/C），实际 %d: %v", len(members), members)
	}
	byKey := map[string]compareByMember{}
	for _, m := range members {
		byKey[m.key] = m
	}
	if m := byKey["A"]; m.cur.TotalTokens != 100 || m.base != (querier.GroupAggregate{}) {
		t.Errorf("A 缺失基线侧应为零值聚合，实际 %+v", m)
	}
	if m := byKey["B"]; m.base.TotalTokens != 80 || m.cur != (querier.GroupAggregate{}) {
		t.Errorf("B 缺失当前侧应为零值聚合，实际 %+v", m)
	}
	if m := byKey["C"]; m.cur.TotalTokens != 50 || m.base.TotalTokens != 70 {
		t.Errorf("C 两期聚合应各自保留，实际 %+v", m)
	}
}

// tableRowCellsByFirst 定位首列单元格（去空格后）恰为 first 的表格行并返回
// 单元格序列；专供分维度对比表按成员键取行（成员键可能与表头词撞前缀）。
func tableRowCellsByFirst(t *testing.T, out, first string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "│")
		if len(parts) < 3 {
			continue
		}
		if strings.TrimSpace(parts[1]) != first {
			continue
		}
		cells := make([]string, 0, len(parts)-2)
		for _, p := range parts[1 : len(parts)-1] {
			cells = append(cells, strings.TrimSpace(p))
		}
		return cells
	}
	t.Fatalf("输出缺少首列为 %q 的表格行:\n%s", first, out)
	return nil
}

// tableRowLineIndex 返回首列恰为 first 的表格行所在行号（找不到为 -1），
// 供行序断言。
func tableRowLineIndex(out, first string) int {
	for i, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "│")
		if len(parts) >= 3 && strings.TrimSpace(parts[1]) == first {
			return i
		}
	}
	return -1
}

// TestRenderCompareBy_Members 分维度渲染确切断言：成员行按两期之和降序
// （C 120 > A 100 > B 80）、只在一期出现的成员缺失侧渲染 0 且变化% 为
// "--"、Total 行取 StatsBetween 总量而非成员累加输入顺序。
func TestRenderCompareBy_Members(t *testing.T) {
	members := mergeCompareByMembers(
		map[string]querier.GroupAggregate{
			"A": {Requests: 1, TotalTokens: 100},
			"C": {Requests: 1, TotalTokens: 50},
		},
		map[string]querier.GroupAggregate{
			"B": {Requests: 1, TotalTokens: 80},
			"C": {Requests: 1, TotalTokens: 70},
		},
	)
	in := compareByRenderInput{
		curStart: "2026-09-01", curEnd: "2026-09-07",
		baseStart: "2026-08-25", baseEnd: "2026-08-31",
		by:      "model",
		cur:     querier.RangeStats{ActiveDays: 3, Total: querier.GroupAggregate{Requests: 3, TotalTokens: 150}},
		base:    querier.RangeStats{ActiveDays: 2, Total: querier.GroupAggregate{Requests: 3, TotalTokens: 150}},
		members: members,
	}
	var buf bytes.Buffer
	if err := renderCompareBy(&buf, in); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 窗口头两行与总量对比一致；首列表头为维度两行常量，不再有 Metric 指标表。
	for _, want := range []string{
		"Compare / 用量对比",
		"Current / 当前: 2026-09-01 .. 2026-09-07",
		"Base / 基线: 2026-08-25 .. 2026-08-31",
		"Model", "模型", "Current", "当前", "Base", "基线", "Change %", "变化%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Metric") || strings.Contains(out, "指标") {
		t.Errorf("--by 模式不应输出 8 行指标表:\n%s", out)
	}

	// 行序：按两期 TotalTokens 之和降序 C(120) > A(100) > B(80)。
	idxC, idxA, idxB := tableRowLineIndex(out, "C"), tableRowLineIndex(out, "A"), tableRowLineIndex(out, "B")
	if idxC < 0 || idxA < 0 || idxB < 0 || !(idxC < idxA && idxA < idxB) {
		t.Errorf("成员行应按两期之和降序 C、A、B，实际行号 C=%d A=%d B=%d:\n%s", idxC, idxA, idxB, out)
	}
	rows := map[string][]string{
		"C": {"C", "50", "70", "-20", "-28.6%"},
		"A": {"A", "100", "0", "+100", "--"},
		"B": {"B", "0", "80", "-80", "-100.0%"},
	}
	for key, want := range rows {
		got := tableRowCellsByFirst(t, out, key)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("成员 %q 行 = %v, want %v\n%s", key, got, want, out)
		}
	}
	// Total 行真相源为 StatsBetween 两期总量（150/150 → 持平 0.0%）。
	wantTotal := []string{"Total / 总计", "150", "150", "0", "0.0%"}
	if got := tableRowCellsByFirst(t, out, "Total / 总计"); !reflect.DeepEqual(got, wantTotal) {
		t.Errorf("Total 行 = %v, want %v\n%s", got, wantTotal, out)
	}
}

// TestRenderCompareBy_NoData 双窗口成员集为空且两期总计均为 0 时，窗口头后
// 只输出无数据行、不画表。
func TestRenderCompareBy_NoData(t *testing.T) {
	in := compareByRenderInput{
		curStart: "2026-09-01", curEnd: "2026-09-07",
		baseStart: "2026-08-25", baseEnd: "2026-08-31",
		by: "client",
	}
	var buf bytes.Buffer
	if err := renderCompareBy(&buf, in); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "no data / 无数据") {
		t.Errorf("空数据应输出 no data / 无数据:\n%s", out)
	}
	if strings.Contains(out, "│") {
		t.Errorf("无数据不应画表:\n%s", out)
	}
}

// TestCompareCmd_RejectsByDimension --by 取时间维度或未知值即报双语错误，
// 且先于配置加载与数据库打开。
func TestCompareCmd_RejectsByDimension(t *testing.T) {
	cases := []struct {
		name   string
		by     string
		wantEn string
		wantZh string
	}{
		{"temporal day", "day", "does not accept temporal dimensions", "不接受时间维度"},
		{"temporal hour", "hour", "does not accept temporal dimensions", "不接受时间维度"},
		{"unknown", "unknown", "unknown --by dimension", "未知 --by 维度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCompareCmdWithDeps(
				func() (*config.Config, error) { t.Fatal("--by 校验失败不应加载配置"); return nil, nil },
				func(string) (*db.DB, error) { t.Fatal("--by 校验失败不应打开数据库"); return nil, nil },
			)
			cmd.SetArgs([]string{"20260901-20260907", "--by", tc.by})
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("--by %q 应返回 error", tc.by)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantEn) || !strings.Contains(msg, tc.wantZh) {
				t.Errorf("--by %q 错误应含双语句式 %q / %q，实际 %q", tc.by, tc.wantEn, tc.wantZh, msg)
			}
			if !strings.Contains(msg, "client, model, provider, project") {
				t.Errorf("错误应枚举允许集，实际 %q", msg)
			}
			if !strings.Contains(msg, "token-usage chart --line") {
				t.Errorf("错误应指路 token-usage chart --line，实际 %q", msg)
			}
		})
	}
}

// TestCompareCmd_EndToEnd_ByModel 真实调用链：内存库注入两期不同成员集
// （model-a 只在当前期、model-b 只在基线期、model-c 两期都有），--by model
// 输出成员行、缺失侧 "--" 与 StatsBetween 总计。
func TestCompareCmd_EndToEnd_ByModel(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmpby-cur-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmpby-cur-c", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-c",
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(), TotalTokens: 400},
		{ID: "cmpby-base-b", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-b",
			Date: "2026-08-30", TS: day("2026-08-30", 0).UnixMilli(), TotalTokens: 750},
		{ID: "cmpby-base-c", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-c",
			Date: "2026-08-29", TS: day("2026-08-29", 0).UnixMilli(), TotalTokens: 250},
	}
	seededOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	load := func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil }

	cmd := newCompareCmdWithDeps(load, seededOpen)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260901-20260907", "--by", "model"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 窗口头与总量模式一致。
	if !strings.Contains(out, "Current / 当前: 2026-09-01 .. 2026-09-07") ||
		!strings.Contains(out, "Base / 基线: 2026-08-25 .. 2026-08-31") {
		t.Errorf("窗口行不符合缺省等长前置推导:\n%s", out)
	}
	// 行序按两期之和降序：model-b(750) > model-c(650) > model-a(600)；
	// model-b 当前侧缺失为 0（变化% 为 -100.0%）；model-a 基线缺失为 0（变化% 为 --）。
	idxB := tableRowLineIndex(out, "model-b")
	idxC := tableRowLineIndex(out, "model-c")
	idxA := tableRowLineIndex(out, "model-a")
	if idxB < 0 || idxC < 0 || idxA < 0 || !(idxB < idxC && idxC < idxA) {
		t.Errorf("成员行应按两期之和降序 model-b、model-c、model-a:\n%s", out)
	}
	rows := map[string][]string{
		"model-b": {"model-b", "0", "750", "-750", "-100.0%"},
		"model-c": {"model-c", "400", "250", "+150", "+60.0%"},
		"model-a": {"model-a", "600", "0", "+600", "--"},
	}
	for key, want := range rows {
		got := tableRowCellsByFirst(t, out, key)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("成员 %q 行 = %v, want %v\n%s", key, got, want, out)
		}
	}
	// Total 行来自两期 StatsBetween：当前 1000、基线 1000。
	wantTotal := []string{"Total / 总计", "1.00 K", "1.00 K", "0", "0.0%"}
	if got := tableRowCellsByFirst(t, out, "Total / 总计"); !reflect.DeepEqual(got, wantTotal) {
		t.Errorf("Total 行 = %v, want %v\n%s", got, wantTotal, out)
	}
}

// TestCompareCmd_EndToEnd_ByProviderAliasMergesRows 真实调用链：--by provider
// 时 cfg.ProviderAliases 注入聚合核，两个原始供应商名（provider 归因与
// router_provider 归因各一）合并为一行别名显示，原始标签不残留；未配置
// 别名的供应商保持独立行。
func TestCompareCmd_EndToEnd_ByProviderAliasMergesRows(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmpalias-cur-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(),
			Provider: "source-a", TotalTokens: 100},
		{ID: "cmpalias-cur-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(),
			Provider: "x", RouterProvider: "router-b", TotalTokens: 200},
		{ID: "cmpalias-base-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-30", TS: day("2026-08-30", 0).UnixMilli(),
			Provider: "source-a", TotalTokens: 50},
	}
	seededOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	// 别名来源经 cfg 注入，与生产 loadConfig 读到的 [provider_aliases] 同源。
	load := func() (*config.Config, error) {
		return &config.Config{
			DataDir: t.TempDir(),
			ProviderAliases: map[string]string{
				"source-a": "Merged provider",
				"router-b": "Merged provider",
			},
		}, nil
	}

	cmd := newCompareCmdWithDeps(load, seededOpen)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260901-20260907", "--by", "provider"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 两个原始名经别名合并为一行 "Merged provider"，原始标签不残留。
	if strings.Contains(out, "source-a") || strings.Contains(out, "router-b") {
		t.Errorf("别名合并后不得残留原始供应商标签:\n%s", out)
	}
	// 合并行：当前 100+200=300、基线 50；未别名供应商 "other" 不存在，不误伤。
	wantMerged := []string{"Merged provider", "300", "50", "+250", "+500.0%"}
	if got := tableRowCellsByFirst(t, out, "Merged provider"); !reflect.DeepEqual(got, wantMerged) {
		t.Errorf("别名合并行 = %v, want %v\n%s", got, wantMerged, out)
	}
	// Total 行取两期 StatsBetween：当前 300、基线 50。
	wantTotal := []string{"Total / 总计", "300", "50", "+250", "+500.0%"}
	if got := tableRowCellsByFirst(t, out, "Total / 总计"); !reflect.DeepEqual(got, wantTotal) {
		t.Errorf("Total 行 = %v, want %v\n%s", got, wantTotal, out)
	}
}

// TestCompareCmd_EndToEnd_ByModelExplicitBase 真实调用链：--by model 与显式
// --base 组合——基线窗口不再按粒度推导而是整月 202608；两期不同成员集下
// 缺失侧语义成立（只当前期变化% 为 "--"、只基线期为 -100.0%）。
func TestCompareCmd_EndToEnd_ByModelExplicitBase(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmpxb-cur-x", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-x",
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(), TotalTokens: 500},
		{ID: "cmpxb-cur-y", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-y",
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(), TotalTokens: 300},
		{ID: "cmpxb-base-y", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-y",
			Date: "2026-08-10", TS: day("2026-08-10", 0).UnixMilli(), TotalTokens: 100},
		{ID: "cmpxb-base-z", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-z",
			Date: "2026-08-20", TS: day("2026-08-20", 0).UnixMilli(), TotalTokens: 400},
	}
	seededOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
			return nil, err
		}
		return usageDB, nil
	}
	load := func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil }

	cmd := newCompareCmdWithDeps(load, seededOpen)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260901-20260907", "--by", "model", "--base", "202608"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 显式 --base 整月窗口生效，覆盖缺省等长前置推导。
	if !strings.Contains(out, "Current / 当前: 2026-09-01 .. 2026-09-07") ||
		!strings.Contains(out, "Base / 基线: 2026-08-01 .. 2026-08-31") {
		t.Errorf("窗口行不符合显式 --base 202608:\n%s", out)
	}
	// 行序按两期之和降序、同值按显示键升序：model-x(500) > model-y(400) > model-z(400)。
	idxX := tableRowLineIndex(out, "model-x")
	idxY := tableRowLineIndex(out, "model-y")
	idxZ := tableRowLineIndex(out, "model-z")
	if idxX < 0 || idxY < 0 || idxZ < 0 || !(idxX < idxY && idxY < idxZ) {
		t.Errorf("成员行应按 model-x、model-y、model-z 排序:\n%s", out)
	}
	rows := map[string][]string{
		// model-x 只在当前期：基线侧 0，变化% 无定义为 "--"。
		"model-x": {"model-x", "500", "0", "+500", "--"},
		"model-y": {"model-y", "300", "100", "+200", "+200.0%"},
		// model-z 只在基线期：当前侧 0，全部流失为 -100.0%。
		"model-z": {"model-z", "0", "400", "-400", "-100.0%"},
	}
	for key, want := range rows {
		got := tableRowCellsByFirst(t, out, key)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("成员 %q 行 = %v, want %v\n%s", key, got, want, out)
		}
	}
	// Total 行取两期 StatsBetween：当前 800、基线 500。
	wantTotal := []string{"Total / 总计", "800", "500", "+300", "+60.0%"}
	if got := tableRowCellsByFirst(t, out, "Total / 总计"); !reflect.DeepEqual(got, wantTotal) {
		t.Errorf("Total 行 = %v, want %v\n%s", got, wantTotal, out)
	}
}
