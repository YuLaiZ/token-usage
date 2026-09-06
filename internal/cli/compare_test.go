package cli

import (
	"bytes"
	"context"
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
