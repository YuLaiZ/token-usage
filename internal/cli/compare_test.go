package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
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
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs([]string{tc.raw}, "")
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
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs([]string{tc.raw}, tc.base)
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
			_, _, _, _, err := parseCompareArgs([]string{tc.raw}, tc.base)
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

// TestParseCompareArgs_TwoPositional 双位置参数排序制：两窗口按（起始日，
// 起始日相同再按结束日）升序，早者为基线、晚者为当前，与输入顺序无关；
// 年/月/日/区间形态与等值、同起异终窗口逐一断言四窗口精确值，并以关系
// 断言钉住与单参数 --base 写法的等价式。
func TestParseCompareArgs_TwoPositional(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCur  [2]string
		wantBase [2]string
	}{
		{"month pair", []string{"202607", "202608"},
			[2]string{"2026-08-01", "2026-08-31"}, [2]string{"2026-07-01", "2026-07-31"}},
		{"reversed order normalizes", []string{"202608", "202607"},
			[2]string{"2026-08-01", "2026-08-31"}, [2]string{"2026-07-01", "2026-07-31"}},
		{"year pair", []string{"2025", "2026"},
			[2]string{"2026-01-01", "2026-12-31"}, [2]string{"2025-01-01", "2025-12-31"}},
		{"day with month", []string{"20260815", "202609"},
			[2]string{"2026-09-01", "2026-09-30"}, [2]string{"2026-08-15", "2026-08-15"}},
		{"range pair", []string{"20260701-20260710", "20260801-20260810"},
			[2]string{"2026-08-01", "2026-08-10"}, [2]string{"2026-07-01", "2026-07-10"}},
		{"equal windows", []string{"202607", "202607"},
			[2]string{"2026-07-01", "2026-07-31"}, [2]string{"2026-07-01", "2026-07-31"}},
		{"same start sorts by end", []string{"20260701-20260831", "202607"},
			[2]string{"2026-07-01", "2026-08-31"}, [2]string{"2026-07-01", "2026-07-31"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curStart, curEnd, baseStart, baseEnd, err := parseCompareArgs(tc.args, "")
			if err != nil {
				t.Fatalf("parseCompareArgs(%v) 出错: %v", tc.args, err)
			}
			assertCompareWindow(t, "current", curStart, curEnd, tc.wantCur)
			assertCompareWindow(t, "base", baseStart, baseEnd, tc.wantBase)
		})
	}
	// 等价式（parse 层关系断言）：双参数写法与单参数 --base 写法的四个
	// 返回窗口逐值相等——按时间先后书写时 compare A B 等价于 compare B --base A。
	pairCurStart, pairCurEnd, pairBaseStart, pairBaseEnd, err := parseCompareArgs([]string{"202607", "202608"}, "")
	if err != nil {
		t.Fatal(err)
	}
	baseCurStart, baseCurEnd, baseBaseStart, baseBaseEnd, err := parseCompareArgs([]string{"202608"}, "202607")
	if err != nil {
		t.Fatal(err)
	}
	if pairCurStart != baseCurStart || pairCurEnd != baseCurEnd ||
		pairBaseStart != baseBaseStart || pairBaseEnd != baseBaseEnd {
		t.Errorf("等价式两侧窗口应逐值相等: 双参数 = %v..%v / %v..%v, --base = %v..%v / %v..%v",
			pairCurStart, pairCurEnd, pairBaseStart, pairBaseEnd,
			baseCurStart, baseCurEnd, baseBaseStart, baseBaseEnd)
	}
}

// TestParseCompareArgs_TwoPositionalErrors 双位置参数负向形态：与 --base
// 同用冲突、日历非法、长度非法、ISO 破折号拆分出年端点；断言双语关键
// 句式与各自的命令示例，保证各分支命中预期的错误辅助函数。
func TestParseCompareArgs_TwoPositionalErrors(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		base        string
		wantEn      string
		wantZh      string
		wantExample string
	}{
		{"two positional with --base", []string{"202607", "202608"}, "202605",
			"either two positional periods or --base", "只能二选一",
			"token-usage compare 202607 202608"},
		{"illegal calendar", []string{"202613", "202607"}, "",
			"date is not a valid date", "日期不合法",
			"token-usage compare 20260701"},
		{"illegal length", []string{"20260", "202607"}, "",
			"accepts YYYYMMDD (day)", "接受 YYYYMMDD（日）",
			"token-usage compare 20260701"},
		{"ISO dashed splits to year endpoint", []string{"2026-07", "202608"}, "",
			"only as a single arg", "仅接受单独使用",
			"token-usage compare 20260701"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := parseCompareArgs(tc.args, tc.base)
			if err == nil {
				t.Fatalf("parseCompareArgs(%v, %q) 应返回 error", tc.args, tc.base)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantEn) {
				t.Errorf("错误应含英文句式 %q，实际 %q", tc.wantEn, msg)
			}
			if !strings.Contains(msg, tc.wantZh) {
				t.Errorf("错误应含中文句式 %q，实际 %q", tc.wantZh, msg)
			}
			if !strings.Contains(msg, tc.wantExample) {
				t.Errorf("错误应含命令示例 %s，实际 %q", tc.wantExample, msg)
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

// TestCompareCmd_RejectsArgCount Args 校验：数量非 1/2 时手写双语错误，
// 文案自带两种调用范式的可照抄示例；参数错误不加载配置、不打开数据库。
func TestCompareCmd_RejectsArgCount(t *testing.T) {
	for _, args := range [][]string{{}, {"20260901", "20260902", "20260903"}} {
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
		if !strings.Contains(msg, "1 or 2 positional args") || !strings.Contains(msg, "1 或 2 个位置参数") {
			t.Errorf("args %v 错误应双语说明参数数量，实际 %q", args, msg)
		}
		for _, example := range []string{"token-usage compare 202608", "token-usage compare 202607 202608"} {
			if !strings.Contains(msg, example) {
				t.Errorf("args %v 错误应含示例 %q，实际 %q", args, example, msg)
			}
		}
	}
}

// TestCompareCmd_HelpGuidance 帮助契约守卫：Long 双语含两种调用范式的
// 可照抄示例与区间澄清句，--base flag 描述含示例命令——cobra 参数错误
// 路径不输出 Long，帮助质量由本测试钉住。
func TestCompareCmd_HelpGuidance(t *testing.T) {
	cmd := newCompareCmd()
	long := cmd.Long
	for _, want := range []string{
		"token-usage compare 202607 202608",
		"token-usage compare 202607 --base 202608",
		"not a two-period comparison",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("Long 应含 %q:\n%s", want, long)
		}
	}
	baseUsage := cmd.Flags().Lookup("base").Usage
	if !strings.Contains(baseUsage, "token-usage compare 202609 --base 202608") {
		t.Errorf("--base flag 描述应含示例命令:\n%s", baseUsage)
	}
}

// TestCompareCmd_EndToEnd_TwoPositional 真实调用链双位置参数：排序制窗口
// 分配（早=基线）、Total 方向断言、逆序输出逐字节一致、区间对双参数与
// 单参数 --base 区间写法逐字节一致（等价式守卫）。
func TestCompareCmd_EndToEnd_TwoPositional(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmp2p-cur-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-02", TS: day("2026-08-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmp2p-cur-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-03", TS: day("2026-08-03", 0).UnixMilli(), TotalTokens: 400},
		{ID: "cmp2p-base", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-07-02", TS: day("2026-07-02", 0).UnixMilli(), TotalTokens: 750},
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
	run := func(args ...string) string {
		cmd := newCompareCmdWithDeps(load, seededOpen)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("compare %v 执行失败: %v", args, err)
		}
		return buf.String()
	}

	// 正序：早者 202607 为基线、晚者 202608 为当前。
	out := run("202607", "202608")
	if !strings.Contains(out, "Current / 当前: 2026-08-01 .. 2026-08-31") ||
		!strings.Contains(out, "Base / 基线: 2026-07-01 .. 2026-07-31") {
		t.Errorf("窗口行不符合排序制（早=基线）:\n%s", out)
	}
	totalCells := tableRowCells(t, out, "Total / 总计")
	wantTotal := []string{"Total / 总计", "1.00 K", "750", "+250", "+33.3%"}
	for i := range wantTotal {
		if totalCells[i] != wantTotal[i] {
			t.Errorf("Total 行第 %d 列 = %q, want %q\n%s", i, totalCells[i], wantTotal[i], out)
		}
	}

	// 顺序无关：逆序输出与正序逐字节一致。
	if reversed := run("202608", "202607"); reversed != out {
		t.Errorf("逆序输出应与正序逐字节一致:\n正序:\n%s\n逆序:\n%s", out, reversed)
	}

	// 等价式守卫：区间对双参数与单参数 --base 区间写法逐字节一致。
	rangePair := run("20260701-20260710", "20260801-20260810")
	withBase := run("20260801-20260810", "--base", "20260701-20260710")
	if rangePair != withBase {
		t.Errorf("等价式两侧输出应逐字节一致:\n双参数:\n%s\n--base:\n%s", rangePair, withBase)
	}
	if !strings.Contains(rangePair, "Current / 当前: 2026-08-01 .. 2026-08-10") ||
		!strings.Contains(rangePair, "Base / 基线: 2026-07-01 .. 2026-07-10") {
		t.Errorf("区间对窗口行不符:\n%s", rangePair)
	}
}

// TestCompareCmd_EndToEnd_TwoPositional_ByModel 真实调用链双位置参数 +
// --by model：窗口头按时间先后（当前 8 月、基线 7 月），成员行按两期之
// 和降序、缺失侧语义不变。
func TestCompareCmd_EndToEnd_TwoPositional_ByModel(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmp2pm-cur-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Date: "2026-08-02", TS: day("2026-08-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmp2pm-base-b", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-b",
			Date: "2026-07-02", TS: day("2026-07-02", 0).UnixMilli(), TotalTokens: 750},
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
	cmd.SetArgs([]string{"202607", "202608", "--by", "model"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Current / 当前: 2026-08-01 .. 2026-08-31") ||
		!strings.Contains(out, "Base / 基线: 2026-07-01 .. 2026-07-31") {
		t.Errorf("窗口行不符合排序制（早=基线）:\n%s", out)
	}
	// model-b 只在基线期（全部流失 -100.0%）、model-a 只在当前期（变化% 无定义 "--"）。
	rows := map[string][]string{
		"model-b": {"model-b", "0", "750", "-750", "-100.0%"},
		"model-a": {"model-a", "600", "0", "+600", "--"},
	}
	for key, want := range rows {
		if got := tableRowCellsByFirst(t, out, key); !reflect.DeepEqual(got, want) {
			t.Errorf("成员 %q 行 = %v, want %v\n%s", key, got, want, out)
		}
	}
	wantTotal := []string{"Total / 总计", "600", "750", "-150", "-20.0%"}
	if got := tableRowCellsByFirst(t, out, "Total / 总计"); !reflect.DeepEqual(got, wantTotal) {
		t.Errorf("Total 行 = %v, want %v\n%s", got, wantTotal, out)
	}
}

// TestCompareCmd_EndToEnd_TwoPositional_JSON 真实调用链双位置参数 JSON：
// windows 与排序制窗口同源；--by model --format json 组合下窗口一致且
// dimension 字段不变。
func TestCompareCmd_EndToEnd_TwoPositional_JSON(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmp2pj-cur-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Date: "2026-08-02", TS: day("2026-08-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmp2pj-base-b", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-b",
			Date: "2026-07-02", TS: day("2026-07-02", 0).UnixMilli(), TotalTokens: 750},
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
	run := func(args ...string) map[string]interface{} {
		cmd := newCompareCmdWithDeps(load, seededOpen)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("compare %v 执行失败: %v", args, err)
		}
		var doc map[string]interface{}
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("输出应为合法 JSON: %v\n%s", err, buf.String())
		}
		return doc
	}

	doc := run("202607", "202608", "--format", "json")
	windows := jsonMap(t, doc["windows"])
	current := jsonMap(t, windows["current"])
	base := jsonMap(t, windows["base"])
	if current["from"] != "2026-08-01" || current["to"] != "2026-08-31" {
		t.Errorf("windows.current = %v/%v, want 2026-08-01/2026-08-31", current["from"], current["to"])
	}
	if base["from"] != "2026-07-01" || base["to"] != "2026-07-31" {
		t.Errorf("windows.base = %v/%v, want 2026-07-01/2026-07-31", base["from"], base["to"])
	}

	byDoc := run("202607", "202608", "--by", "model", "--format", "json")
	if byDoc["dimension"] != "model" {
		t.Errorf("dimension = %v, want \"model\"", byDoc["dimension"])
	}
	byWindows := jsonMap(t, byDoc["windows"])
	byCurrent := jsonMap(t, byWindows["current"])
	byBase := jsonMap(t, byWindows["base"])
	if byCurrent["from"] != "2026-08-01" || byBase["from"] != "2026-07-01" {
		t.Errorf("--by 组合窗口与总量模式应一致: current=%v/%v base=%v/%v",
			byCurrent["from"], byCurrent["to"], byBase["from"], byBase["to"])
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

// TestChangePercentValue 表驱动钉住共享纯函数的舍入口径：正、负、零、
// base==0（ok=false，百分比无定义）、(1000001,1000000) 极小正差舍入到 0.0、
// (9999999,10000000) 极小负差舍入为负零 -0（数值等价 0 但符号位为 1，
// JSON 会原样写出 "-0"）、非半途值四舍五入到 1 位小数。wantSignbit 用
// math.Signbit 逐例钉住符号位，防止负零被改写成正零或反向回归。
func TestChangePercentValue(t *testing.T) {
	cases := []struct {
		name        string
		cur, base   int64
		want        float64
		wantOK      bool
		wantSignbit bool
	}{
		{"positive rounds to 1 decimal", 4, 3, 33.3, true, false},
		{"negative rounds to 1 decimal", 1, 3, -66.7, true, true},
		{"exact zero", 5, 5, 0, true, false},
		{"full loss", 0, 5, -100, true, true},
		{"base zero not computable", 7, 0, 0, false, false},
		{"both zero not computable", 0, 0, 0, false, false},
		{"near-zero positive rounds to 0.0", 1000001, 1000000, 0, true, false},
		{"near-zero negative rounds to -0", 9999999, 10000000, 0, true, true},
		{"repeating decimal rounds to 1 decimal", 5, 3, 66.7, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := changePercentValue(tc.cur, tc.base)
			if ok != tc.wantOK {
				t.Errorf("changePercentValue(%d, %d) ok = %v, want %v", tc.cur, tc.base, ok, tc.wantOK)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("changePercentValue(%d, %d) = %v, want %v", tc.cur, tc.base, got, tc.want)
			}
			if math.Signbit(got) != tc.wantSignbit {
				t.Errorf("changePercentValue(%d, %d) = %v 符号位 = %v, want %v", tc.cur, tc.base, got, math.Signbit(got), tc.wantSignbit)
			}
		})
	}
}

// jsonFloat 断言 JSON 解码值为 float64 并返回；jsonNil 断言值为 nil
// （JSON null）；jsonMap/jsonArray 做对应的容器类型断言。
func jsonFloat(t *testing.T, v interface{}) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("JSON 值应为 number，实际 %T: %v", v, v)
	}
	return f
}

func jsonNil(t *testing.T, v interface{}) {
	t.Helper()
	if v != nil {
		t.Fatalf("JSON 值应为 null（base==0 时百分比无定义），实际 %T: %v", v, v)
	}
}

func jsonMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("JSON 值应为 object，实际 %T: %v", v, v)
	}
	return m
}

func jsonArray(t *testing.T, v interface{}) []interface{} {
	t.Helper()
	a, ok := v.([]interface{})
	if !ok {
		t.Fatalf("JSON 值应为 array，实际 %T: %v", v, v)
	}
	return a
}

// jsonInt 断言 JSON 数值等于期望整数（int64 原始值经 JSON number 往返，
// 不做 K/M 缩写——缩写会变成字符串导致此处类型断言失败）。
func jsonInt(t *testing.T, v interface{}, want int64) {
	t.Helper()
	if got := jsonFloat(t, v); got != float64(want) {
		t.Errorf("JSON 数值 = %v, want %d", got, want)
	}
}

// jsonPercent 断言 change_percent：base==0 期望 nil，否则为四舍五入到
// 1 位小数的数值（与表格 formatChangePercent 同口径）。
func jsonPercent(t *testing.T, v interface{}, want *float64) {
	t.Helper()
	if want == nil {
		jsonNil(t, v)
		return
	}
	if got := jsonFloat(t, v); math.Abs(got-*want) > 1e-9 {
		t.Errorf("change_percent = %v, want %v", got, *want)
	}
}

// floatPtr 测试辅助：构造 *float64 字面量。
func floatPtr(f float64) *float64 { return &f }

// assertJSONMemberFieldOrder 截取 JSON 原文中首个成员对象（含 "key" 的扁平
// 对象）的原文片段，断言字段出现顺序恰为 key→current→base→change→
// change_percent。encoding/json 反序列化到 map 会丢失成员顺序，字段顺序
// 契约只能在原文上检查；成员对象为纯标量字段，"key" 前最近的 "{" 即对象
// 起点，其后最近的 "}" 即终点。
func assertJSONMemberFieldOrder(t *testing.T, out string) {
	t.Helper()
	keyIdx := strings.Index(out, `"key"`)
	if keyIdx < 0 {
		t.Fatalf("JSON 输出应含成员对象 key 字段:\n%s", out)
	}
	start := strings.LastIndex(out[:keyIdx], "{")
	endRel := strings.Index(out[keyIdx:], "}")
	if start < 0 || endRel < 0 {
		t.Fatalf("无法截取成员对象原文片段:\n%s", out)
	}
	frag := out[start : keyIdx+endRel+1]
	prev := -1
	for _, field := range []string{`"key"`, `"current"`, `"base"`, `"change"`, `"change_percent"`} {
		i := strings.Index(frag, field)
		if i < 0 {
			t.Fatalf("成员对象片段应含字段 %s:\n%s", field, frag)
		}
		if prev >= 0 && i <= prev {
			t.Errorf("成员对象字段应按 key→current→base→change→change_percent 排序，%s 首现下标 %d 未晚于前一字段 %d:\n%s", field, i, prev, frag)
		}
		prev = i
	}
}

// TestCompareCmd_EndToEnd_JSON 真实调用链总量模式 --format json：E2E 后
// json.Unmarshal 到 map，断言 windows 日期与表格 Current/Base 行同源、
// metrics 顺序（按下标断言稳定 ID）、数值原始性（7000 保持 JSON number，
// 不做 K/M 缩写）、base==0 → change_percent 为 null、四舍五入 1 位小数、
// 两空格缩进与尾随换行。
func TestCompareCmd_EndToEnd_JSON(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmpj-cur-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(),
			FreshInputTokens: 4000, OutputTokens: 1000, CacheReadTokens: 2000, ReasoningTokens: 300, TotalTokens: 600},
		{ID: "cmpj-cur-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(),
			FreshInputTokens: 3000, OutputTokens: 800, CacheCreateTokens: 100, ReasoningTokens: 200, TotalTokens: 400},
		{ID: "cmpj-base", SessionID: "s", Client: model.ClientClaudeCode,
			Date: "2026-08-30", TS: day("2026-08-30", 0).UnixMilli(),
			FreshInputTokens: 8000, OutputTokens: 600, ReasoningTokens: 100, TotalTokens: 750},
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
	cmd.SetArgs([]string{"20260901-20260907", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 两空格缩进 + 尾随换行（与 export 的机器可读约定一致）。
	if !strings.HasPrefix(out, "{\n  \"windows\"") {
		t.Errorf("JSON 应以两空格缩进的 windows 对象开头:\n%q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("JSON 应以尾随换行结束:\n%q", out)
	}
	// 数值原始性佐证：input 当前值 7000 以原始整数写出（无 K/M 缩写）。
	if !strings.Contains(out, "\"current\": 7000") {
		t.Errorf("JSON 应含原始整数 \"current\": 7000:\n%s", out)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}

	// windows 与表格 Current/Base 行同源（缺省等长前置基线）。
	windows := jsonMap(t, doc["windows"])
	current := jsonMap(t, windows["current"])
	base := jsonMap(t, windows["base"])
	if current["from"] != "2026-09-01" || current["to"] != "2026-09-07" {
		t.Errorf("windows.current = %v/%v, want 2026-09-01/2026-09-07", current["from"], current["to"])
	}
	if base["from"] != "2026-08-25" || base["to"] != "2026-08-31" {
		t.Errorf("windows.base = %v/%v, want 2026-08-25/2026-08-31", base["from"], base["to"])
	}

	// metrics 顺序与表格行一致：active_days + 七个 ui 稳定 ID，按下标断言。
	metrics := jsonArray(t, doc["metrics"])
	wantOrder := []string{"active_days", "requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	if len(metrics) != len(wantOrder) {
		t.Fatalf("metrics 应有 %d 行，实际 %d: %v", len(wantOrder), len(metrics), metrics)
	}
	for i, want := range wantOrder {
		row := jsonMap(t, metrics[i])
		if row["metric"] != want {
			t.Errorf("metrics[%d].metric = %v, want %q（顺序与表格行一致）", i, row["metric"], want)
		}
	}

	// 逐行数值断言（按下标，同时钉住顺序）：active_days 2/1、input 原始
	// 整数 7000/8000、cache_read base==0 → null、total 四舍五入 33.3。
	type wantRow struct {
		cur, bas, change int64
		pct              *float64
	}
	wantRows := map[int]wantRow{
		0: {2, 1, 1, floatPtr(100)},             // active_days
		1: {2, 1, 1, floatPtr(100)},             // requests
		2: {7000, 8000, -1000, floatPtr(-12.5)}, // input：原始整数，无缩写
		3: {1800, 600, 1200, floatPtr(200)},     // output
		4: {2000, 0, 2000, nil},                 // cache_read：base==0 → null
		5: {100, 0, 100, nil},                   // cache_create：base==0 → null
		6: {500, 100, 400, floatPtr(400)},       // reasoning
		7: {1000, 750, 250, floatPtr(33.3)},     // total：+33.333.. → 33.3
	}
	for i, want := range wantRows {
		row := jsonMap(t, metrics[i])
		// 字段存在性：change_percent 无论是否为 null 都必须出现在对象里，
		// 区分"字段缺失"与"字段为 null"两种不同语义。
		if _, ok := row["change_percent"]; !ok {
			t.Errorf("metrics[%d] 应存在 change_percent 字段（缺失与 null 语义不同）", i)
		}
		jsonInt(t, row["current"], want.cur)
		jsonInt(t, row["base"], want.bas)
		jsonInt(t, row["change"], want.change)
		jsonPercent(t, row["change_percent"], want.pct)
	}
}

// TestCompareCmd_EndToEnd_JSON_ByModel 真实调用链分维度模式 --format json：
// members 顺序与表格一致（两期之和降序）、缺基线侧 change_percent 为 null、
// dimension 字段、成员对象字段顺序恰为 key→current→base→change→change_percent、
// totals 字段名与 ui 稳定 ID 齐全且取 StatsBetween 真相源。
func TestCompareCmd_EndToEnd_JSON_ByModel(t *testing.T) {
	day := func(date string, offsetDays int) time.Time {
		base, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return base.AddDate(0, 0, offsetDays).Add(9 * time.Hour)
	}
	msgs := []model.Message{
		{ID: "cmpjby-cur-a", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-a",
			Date: "2026-09-02", TS: day("2026-09-02", 0).UnixMilli(), TotalTokens: 600},
		{ID: "cmpjby-cur-c", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-c",
			Date: "2026-09-03", TS: day("2026-09-03", 0).UnixMilli(), TotalTokens: 400},
		{ID: "cmpjby-base-b", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-b",
			Date: "2026-08-30", TS: day("2026-08-30", 0).UnixMilli(), TotalTokens: 750},
		{ID: "cmpjby-base-c", SessionID: "s", Client: model.ClientClaudeCode, Model: "model-c",
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
	cmd.SetArgs([]string{"20260901-20260907", "--by", "model", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out)
	}

	// 成员对象字段顺序契约：struct 字段序即序列化顺序，第一个成员对象的
	// 原文片段应按 key→current→base→change→change_percent 排列。截取首个
	// 含 "key" 的扁平对象片段，按片段内首次出现下标断言严格递增；搜索键
	// 均带闭合引号，"change" 不会误匹配 "change_percent"（map 反序列化会
	// 丢顺序，必须在原文上检查）。
	assertJSONMemberFieldOrder(t, out)

	if doc["dimension"] != "model" {
		t.Errorf("dimension = %v, want \"model\"", doc["dimension"])
	}

	// members 顺序与表格一致：两期之和降序 model-b(750) > model-c(650) > model-a(600)。
	members := jsonArray(t, doc["members"])
	if len(members) != 3 {
		t.Fatalf("members 应有 3 项，实际 %d: %v", len(members), members)
	}
	wantKeys := []string{"model-b", "model-c", "model-a"}
	wantRows := []struct {
		cur, bas, change int64
		pct              *float64
	}{
		{0, 750, -750, floatPtr(-100)}, // model-b 只在基线期：全部流失
		{400, 250, 150, floatPtr(60)},  // model-c 两期都有
		{600, 0, 600, nil},             // model-a 只在当前期：base==0 → null
	}
	for i, want := range wantRows {
		row := jsonMap(t, members[i])
		if row["key"] != wantKeys[i] {
			t.Errorf("members[%d].key = %v, want %q（顺序与表格一致）", i, row["key"], wantKeys[i])
		}
		jsonInt(t, row["current"], want.cur)
		jsonInt(t, row["base"], want.bas)
		jsonInt(t, row["change"], want.change)
		jsonPercent(t, row["change_percent"], want.pct)
	}

	// totals 字段名与 ui 稳定 ID 齐全，值为两期 StatsBetween 真相源
	// （当前 2 请求 1000 tokens、基线 2 请求 1000 tokens）。
	wantFields := []string{"requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}
	totals := jsonMap(t, doc["totals"])
	for _, side := range []string{"current", "base"} {
		m := jsonMap(t, totals[side])
		for _, f := range wantFields {
			if _, ok := m[f]; !ok {
				t.Errorf("totals.%s 缺少字段 %q", side, f)
			}
		}
	}
	curTotals := jsonMap(t, totals["current"])
	baseTotals := jsonMap(t, totals["base"])
	jsonInt(t, curTotals["requests"], 2)
	jsonInt(t, curTotals["total"], 1000)
	jsonInt(t, baseTotals["requests"], 2)
	jsonInt(t, baseTotals["total"], 1000)
}

// TestCompareCmd_EndToEnd_JSON_NoData 分维度模式双窗口无数据：JSON 不做
// 表格的 no data 早退，members 为空数组、totals 两侧字段照常输出。
func TestCompareCmd_EndToEnd_JSON_NoData(t *testing.T) {
	seededOpen := func(string) (*db.DB, error) {
		usageDB, err := db.Open(":memory:")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { usageDB.Close() })
		return usageDB, nil
	}
	load := func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil }

	cmd := newCompareCmdWithDeps(load, seededOpen)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260901-20260907", "--by", "model", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, buf.String())
	}
	if members := jsonArray(t, doc["members"]); len(members) != 0 {
		t.Errorf("无数据时 members 应为空数组，实际 %v", members)
	}
	totals := jsonMap(t, doc["totals"])
	for _, side := range []string{"current", "base"} {
		jsonInt(t, jsonMap(t, totals[side])["total"], 0)
	}
}

// TestCompareCmd_RejectsFormat --format 取值不在 table|json 白名单时双语
// 报错，且先于配置加载与数据库打开。
func TestCompareCmd_RejectsFormat(t *testing.T) {
	cmd := newCompareCmdWithDeps(
		func() (*config.Config, error) { t.Fatal("--format 校验失败不应加载配置"); return nil, nil },
		func(string) (*db.DB, error) { t.Fatal("--format 校验失败不应打开数据库"); return nil, nil },
	)
	cmd.SetArgs([]string{"20260901-20260907", "--format", "yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--format yaml 应返回 error")
	}
	msg := err.Error()
	for _, want := range []string{
		`invalid --format "yaml" (allowed: table, json)`,
		`无效的 --format "yaml"（允许：table、json）`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q，实际 %q", want, msg)
		}
	}
}
