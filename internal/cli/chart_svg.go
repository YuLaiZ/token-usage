package cli

import (
	"fmt"
	"math"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// chartCanvas 是 SVG 画布与绘图区几何:总宽高、边距,柱状条绘制在绘图区内。
type chartCanvas struct {
	width, height int
	left, right   int
	top, bottom   int
}

func (c chartCanvas) plotWidth() int   { return c.width - c.left - c.right }
func (c chartCanvas) plotHeight() int  { return c.height - c.top - c.bottom }
func (c chartCanvas) plotBottomY() int { return c.height - c.bottom }

// defaultChartCanvas 是 chart 命令的默认画布:900x420,左侧留 Y 轴刻度,
// 顶部留标题与汇总两行,底部留 X 轴日期标签。
func defaultChartCanvas() chartCanvas {
	return chartCanvas{width: 900, height: 420, left: 70, right: 24, top: 64, bottom: 40}
}

// chartBar 是一根柱:标签(X 轴)、值(Y)与悬停提示(SVG 原生 <title>)。
type chartBar struct {
	label string
	value int64
	hover string
}

// buildBarSVG 生成按日柱状图的独立 SVG 文档(零外部依赖,浏览器/图片查看器
// 直接打开):白色背景、标题与总计两行、Y 轴三条等分网格刻度、每根柱一个
// 矩形(自带 <title> 悬停提示)与抽样 X 轴标签。bar 值可为 0(高度 0 不绘制
// 矩形);bars 为空时绘制空坐标轴。
func buildBarSVG(title, subtitle string, bars []chartBar) string {
	c := defaultChartCanvas()
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#ffffff\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"16\" fill=\"#222\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"12\" fill=\"#555\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	plotW, plotH := c.plotWidth(), c.plotHeight()
	baseY := c.plotBottomY()
	// 坐标轴。
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#999\"/>\n",
		c.left, baseY-plotH, c.left, baseY)
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#999\"/>\n",
		c.left, baseY, c.left+plotW, baseY)

	// Y 轴最大值与三条等分网格:取 max 向上取整到「1/3 最大值」的整数倍,
	// 避免最高柱顶到绘图区上缘。
	var maxVal int64
	for _, bar := range bars {
		if bar.value > maxVal {
			maxVal = bar.value
		}
	}
	yMax := yScaleMax(maxVal)
	// 全零数据时网格会与 X 轴重合且刻度退化为 "0"/"1",跳过网格仅留坐标轴。
	for i := 1; maxVal > 0 && i <= 3; i++ {
		v := yMax / 3 * int64(i)
		y := baseY - int(float64(plotH)*float64(v)/float64(yMax))
		fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#eee\"/>\n",
			c.left, y, c.left+plotW, y)
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"end\" font-family=\"monospace\" font-size=\"11\" fill=\"#666\">%s</text>\n",
			c.left-6, y+4, querier.FormatTokens(v))
	}

	if len(bars) > 0 {
		slot := float64(plotW) / float64(len(bars))
		barW := slot - 1
		if barW < 1 {
			barW = 1
		}
		for i, bar := range bars {
			if bar.value <= 0 || yMax == 0 {
				continue
			}
			h := int(float64(plotH) * float64(bar.value) / float64(yMax))
			x := c.left + int(float64(i)*slot)
			fmt.Fprintf(&b, "  <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"#4a90d9\"><title>%s</title></rect>\n",
				x, baseY-h, int(barW)+1, h, svgEscape(bar.hover))
		}
		// X 轴标签抽样:目标约 8 个,均匀取下标(含首尾)。
		labels := xAxisLabels(bars, 8)
		for _, l := range labels {
			x := c.left + int(float64(l.index)*slot+slot/2)
			fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"10\" fill=\"#666\">%s</text>\n",
				x, baseY+14, svgEscape(l.text))
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// chartXLabel 是一个抽样 X 轴标签:柱下标与展示文本。
type chartXLabel struct {
	index int
	text  string
}

// xAxisLabels 从 bars 均匀抽样至多 max 个标签(恒含首尾),下标去重。
func xAxisLabels(bars []chartBar, max int) []chartXLabel {
	if len(bars) == 0 {
		return nil
	}
	if max < 2 {
		return nil // 均匀抽样至少需要两个端点,防御除零
	}
	if len(bars) <= max {
		out := make([]chartXLabel, len(bars))
		for i, bar := range bars {
			out[i] = chartXLabel{i, bar.label}
		}
		return out
	}
	seen := map[int]bool{}
	var out []chartXLabel
	for i := 0; i < max; i++ {
		idx := i * (len(bars) - 1) / (max - 1)
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, chartXLabel{idx, bars[idx].label})
	}
	return out
}

// yScaleMax 把最大柱值向上取整到 3 的倍数刻度(三条等分网格的顶格值),
// 保证网格标注为整数;maxVal 为 0 时返回 1 避免除零。
func yScaleMax(maxVal int64) int64 {
	if maxVal <= 0 {
		return 1
	}
	third := (maxVal + 2) / 3
	if third == 0 {
		third = 1
	}
	return third * 3
}

// svgEscape 转义 SVG 文本节点与属性中的五个 XML 实体。
func svgEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// piePalette 是饼图扇区的固定取色序列(10 色循环),顺序即维度值的排序位次。
var piePalette = []string{
	"#4a90d9", "#e07a5f", "#5faa64", "#b58fd8", "#d9a648",
	"#4fb0a5", "#d97ba6", "#8a9bab", "#c96f4a", "#7a9e5f",
}

// chartSlice 是一个饼图扇区:标签、值、悬停提示与取色。
type chartSlice struct {
	label string
	value int64
	hover string
	color string
}

// buildPieSVG 生成占比饼图的独立 SVG 文档:左侧扇区(原生 path 圆弧),
// 右侧图例(色块+标签+百分比)。slices 为空或总和为 0 时输出无数据文本,
// 不绘制任何扇区。
func buildPieSVG(title, subtitle string, slices []chartSlice) string {
	c := chartCanvas{width: 760, height: 420, left: 24, right: 24, top: 64, bottom: 24}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#ffffff\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"16\" fill=\"#222\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"12\" fill=\"#555\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	cx, cy, r := 240, 240, 150
	var total int64
	for _, sl := range slices {
		total += sl.value
	}
	if total <= 0 {
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"monospace\" font-size=\"14\" fill=\"#666\">%s</text>\n",
			cx, cy, svgEscape(ui.Bi("no data", "无数据")))
		b.WriteString("</svg>\n")
		return b.String()
	}

	// 扇区累加:起点固定 12 点方向(-90°),顺时针;比例角度,累计到 360° 收口。
	const twoPi = 2 * 3.141592653589793
	angle := -twoPi / 4
	for i, sl := range slices {
		frac := float64(sl.value) / float64(total)
		sweep := frac * twoPi
		if sweep <= 0 {
			continue
		}
		// 单扇区独占全圆(100%)时 path 圆弧退化为零面积,退化为整圆。
		if sweep >= twoPi-1e-9 {
			fmt.Fprintf(&b, "  <circle cx=\"%d\" cy=\"%d\" r=\"%d\" fill=\"%s\"><title>%s</title></circle>\n",
				cx, cy, r, sl.color, svgEscape(sl.hover))
		} else {
			x1 := float64(cx) + float64(r)*cos(angle)
			y1 := float64(cy) + float64(r)*sin(angle)
			x2 := float64(cx) + float64(r)*cos(angle+sweep)
			y2 := float64(cy) + float64(r)*sin(angle+sweep)
			large := 0
			if sweep > twoPi/2 {
				large = 1
			}
			fmt.Fprintf(&b, "  <path d=\"M %d %d L %.2f %.2f A %d %d 0 %d 1 %.2f %.2f Z\" fill=\"%s\"><title>%s</title></path>\n",
				cx, cy, x1, y1, r, r, large, x2, y2, sl.color, svgEscape(sl.hover))
		}
		// 图例:色块 + 标签 + 百分比。
		lx, ly := 460, 84+i*26
		fmt.Fprintf(&b, "  <rect x=\"%d\" y=\"%d\" width=\"12\" height=\"12\" fill=\"%s\"/>\n", lx, ly, sl.color)
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" font-family=\"monospace\" font-size=\"12\" fill=\"#333\">%s (%.1f%%)</text>\n",
			lx+20, ly+10, svgEscape(sl.label), frac*100)
		angle += sweep
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// cos/sin 是 math 包的同名包装:集中引入便于一致替换或测试打桩。
func cos(x float64) float64 { return math.Cos(x) }
func sin(x float64) float64 { return math.Sin(x) }
