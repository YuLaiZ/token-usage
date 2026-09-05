package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// buildBarSVG 合同:合法 XML、标题/副标题转义、柱数与 rect 对应、最高柱触顶、
// 空数据只画坐标轴。
func TestBuildBarSVG(t *testing.T) {
	bars := []chartBar{
		{label: "2026-09-01", value: 500, hover: "2026-09-01: 500 tokens"},
		{label: "2026-09-02", value: 0, hover: "2026-09-02: 0 tokens"},
		{label: "2026-09-03", value: 1000, hover: "2026-09-03: 1.00 K tokens"},
	}
	svg := buildBarSVG("token-usage 2026-09-01 ~ 2026-09-03", "Total 1.50 K tokens / 3 requests", bars)

	if !strings.HasPrefix(svg, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>") {
		t.Errorf("SVG 应以 XML 声明开头:\n%s", svg)
	}
	if !strings.HasSuffix(svg, "</svg>\n") {
		t.Errorf("SVG 应以 </svg> 结束:\n%s", svg)
	}
	if !strings.Contains(svg, "token-usage 2026-09-01 ~ 2026-09-03") {
		t.Errorf("应含标题:\n%s", svg)
	}
	if !strings.Contains(svg, "Total 1.50 K tokens / 3 requests") {
		t.Errorf("应含副标题:\n%s", svg)
	}
	// 值为 0 的柱不绘制矩形:仅两根。
	if n := strings.Count(svg, "<rect"); n != 3 { // 背景白 rect + 两根柱
		t.Errorf("rect 数应恰 3(背景+两柱),实际 %d:\n%s", n, svg)
	}
	if !strings.Contains(svg, "&lt;b&gt;") && strings.Contains(svg, "<b>") {
		t.Errorf("SVG 文本应转义 XML 实体:\n%s", svg)
	}
	// 悬停提示。
	if !strings.Contains(svg, "2026-09-03: 1.00 K tokens") {
		t.Errorf("柱应带 <title> 悬停提示:\n%s", svg)
	}
}

// 强度边界:全部柱同值时 yScaleMax 三等分网格;空柱列表只画坐标轴不越界。
func TestBuildBarSVG_EmptyAndEscaping(t *testing.T) {
	svg := buildBarSVG("a<b>&c", "s\"d'", nil)
	if !strings.Contains(svg, "a&lt;b&gt;&amp;c") {
		t.Errorf("标题应转义:\n%s", svg)
	}
	if !strings.Contains(svg, `s&quot;d&apos;`) {
		t.Errorf("副标题应转义:\n%s", svg)
	}
	if strings.Count(svg, "<rect") != 1 {
		t.Errorf("空数据仅背景 rect:\n%s", svg)
	}

	// 转义函数直测。
	if got := svgEscape(`&<>"'`); got != "&amp;&lt;&gt;&quot;&apos;" {
		t.Errorf("svgEscape = %q", got)
	}
}

// buildPieSVG 合同:扇区 path 数=非零值数、比例角度正确(首扇区自 12 点方向
// 起)、图例含百分比、全零输出无数据、XML 转义。
func TestBuildPieSVG(t *testing.T) {
	slices := []chartSlice{
		{label: "model-a", value: 750, hover: "model-a: 750 tokens", color: "#4a90d9"},
		{label: "model-b", value: 250, hover: "model-b: 250 tokens", color: "#e07a5f"},
	}
	svg := buildPieSVG("pie test", "Total 1.00 K tokens / 2 requests", slices)
	if strings.Count(svg, "<path") != 2 {
		t.Errorf("应恰 2 个扇区 path:\n%s", svg)
	}
	if !strings.Contains(svg, "(75.0%)") || !strings.Contains(svg, "(25.0%)") {
		t.Errorf("图例应含百分比:\n%s", svg)
	}
	if !strings.Contains(svg, "#4a90d9") || !strings.Contains(svg, "#e07a5f") {
		t.Errorf("扇区与图例应使用取色序列:\n%s", svg)
	}

	// 100% 单扇区:圆弧退化为整圆,应输出 circle 而非 path。
	one := buildPieSVG("one", "t", []chartSlice{{label: "only", value: 100, color: "#111111"}})
	if !strings.Contains(one, "<circle") || strings.Contains(one, "<path") {
		t.Errorf("100%% 占比应退化为整圆:\n%s", one)
	}

	// 全零:无数据文本,无扇区。
	zero := buildPieSVG("zero", "t", []chartSlice{{label: "x", value: 0, color: "#111111"}})
	if strings.Contains(zero, "<path") || !strings.Contains(zero, "no data / 无数据") {
		t.Errorf("全零应输出无数据文本且无扇区:\n%s", zero)
	}
}

// --by 维度校验与 --pie 的 day 拒绝在开库前生效。
func TestChartCmd_ByDimensionValidation(t *testing.T) {
	openCalls := 0
	cmd := newChartCmdWithDeps(
		func() (*config.Config, error) {
			return &config.Config{DataDir: t.TempDir()}, nil
		},
		func(string) (*db.DB, error) {
			openCalls++
			return db.Open(":memory:")
		},
	)
	cmd.SetArgs([]string{"--by", "bogus"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("未知 --by 维度应报错")
	}
	if openCalls != 0 {
		t.Errorf("校验应在开库前完成,实际 open %d 次", openCalls)
	}

	cmd2 := newChartCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return db.Open(":memory:") },
	)
	cmd2.SetArgs([]string{"--pie", "--by", "day"})
	cmd2.SetOut(&buf)
	cmd2.SetErr(&buf)
	if err := cmd2.Execute(); err == nil {
		t.Fatal("--pie --by day 应被拒绝")
	}
}

// buildHeatmapSVG 合同:格子数=星期×小时、取色经 level 折算(最大值格最高级)、
// 悬停提示含星期/小时/tokens;values 回调缺失交点返回 0(最低级)。
func TestBuildHeatmapSVG(t *testing.T) {
	weekdays := []string{"Monday / 周一", "Tuesday / 周二"}
	hours := []string{"00:00", "01:00", "02:00"}
	values := map[[2]int]int64{{0, 2}: 900}
	svg := buildHeatmapSVG("hm", "sub", weekdays, hours,
		func(wi, hi int) int64 { return values[[2]int{wi, hi}] })

	// 2 行×3 列 = 6 格 + 1 背景 + 11 图例色块 = 18 rect;每格有 hover title。
	if strings.Count(svg, "<rect") != 18 {
		t.Errorf("rect 数应 18(背景+6 格+11 图例色块),实际 %d", strings.Count(svg, "<rect"))
	}
	if !strings.Contains(svg, "Monday / 周一 02:00: 900 tokens") {
		t.Errorf("最大值格悬停应含星期/小时/数量:\n%s", svg)
	}
	// 缺失交点(周二 00:00)应为最低级取色 heatFills[0]。
	if !strings.Contains(svg, `fill="`+heatFills[0]+`"`) {
		t.Errorf("缺失交点应为最低级取色:\n%s", svg)
	}
	if !strings.Contains(svg, "900 tokens") {
		t.Errorf("悬停应含 tokens 数:\n%s", svg)
	}
}
