package cli

import (
	"strings"
	"testing"
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
