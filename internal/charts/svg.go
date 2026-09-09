// Package charts 是柱状/折线/饼图/热力矩阵 SVG 的唯一构建实现:cli 的
// chart/report 命令与 web 仪表板的 /api/chart 接口共用同一份几何、取色与
// 悬停文案,防止多入口图表漂移。
package charts

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

// defaultChartCanvas 是柱状/折线图的默认画布:900x420,左侧留 Y 轴刻度,
// 顶部留标题与汇总两行,底部留 X 轴日期标签。
func defaultChartCanvas() chartCanvas {
	return chartCanvas{width: 900, height: 420, left: 70, right: 24, top: 64, bottom: 40}
}

// Bar 是一根柱(或折线图的一个点):标签(X 轴)、值(Y)与悬停提示(SVG 原生 <title>)。
type Bar struct {
	Label string
	Value int64
	Hover string
}

// BarSVG 生成柱状图的独立 SVG 文档(零外部依赖,浏览器/图片查看器直接打开):
// 深色背景(#131a22)、标题与总计两行、Y 轴三条等分网格刻度、每根柱一个矩形(自带
// <title> 悬停提示)与抽样 X 轴标签。bar 值可为 0(高度 0 不绘制矩形);
// bars 为空时绘制空坐标轴。
func BarSVG(title, subtitle string, bars []Bar) string {
	c := defaultChartCanvas()
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#131a22\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"16\" fill=\"#dee8f2\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"12\" fill=\"#8ca0b4\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	plotW, plotH := c.plotWidth(), c.plotHeight()
	baseY := c.plotBottomY()
	// 坐标轴。
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#31435a\"/>\n",
		c.left, baseY-plotH, c.left, baseY)
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#31435a\"/>\n",
		c.left, baseY, c.left+plotW, baseY)

	// Y 轴最大值与三条等分网格:取 max 向上取整到「1/3 最大值」的整数倍,
	// 避免最高柱顶到绘图区上缘。
	var maxVal int64
	for _, bar := range bars {
		if bar.Value > maxVal {
			maxVal = bar.Value
		}
	}
	yMax := yScaleMax(maxVal)
	// 全零数据时网格会与 X 轴重合且刻度退化为 "0"/"1",跳过网格仅留坐标轴。
	for i := 1; maxVal > 0 && i <= 3; i++ {
		v := yMax / 3 * int64(i)
		y := baseY - int(float64(plotH)*float64(v)/float64(yMax))
		fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#223041\"/>\n",
			c.left, y, c.left+plotW, y)
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"end\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"11\" fill=\"#7f92a4\">%s</text>\n",
			c.left-6, y+4, querier.FormatTokens(v))
	}

	if len(bars) > 0 {
		slot := float64(plotW) / float64(len(bars))
		barW := slot - 1
		if barW < 1 {
			barW = 1
		}
		for i, bar := range bars {
			if bar.Value <= 0 || yMax == 0 {
				continue
			}
			h := int(float64(plotH) * float64(bar.Value) / float64(yMax))
			x := c.left + int(float64(i)*slot)
			fmt.Fprintf(&b, "  <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"#5cc8ff\"><title>%s</title></rect>\n",
				x, baseY-h, int(barW)+1, h, svgEscape(bar.Hover))
		}
		// X 轴标签抽样:目标约 8 个,均匀取下标(含首尾);中心位置钳制在
		// 画布内,避免首尾标签以 middle 锚点越出画布边缘被裁剪。
		labels := xAxisLabels(bars, 8)
		for _, l := range labels {
			x := c.left + int(float64(l.index)*slot+slot/2)
			if x < c.left+34 {
				x = c.left + 34
			}
			if x > c.width-34 {
				x = c.width - 34
			}
			fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">%s</text>\n",
				x, baseY+14, svgEscape(l.text))
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// LineSVG 生成时间趋势折线图的独立 SVG 文档:画布、坐标轴、Y 轴网格刻度
// 与抽样 X 轴标签同柱状图(同一比例映射);数据点以 polyline 连续连线,零值点
// 仍参与连线保证折线连续,点数不超过 60 时逐点绘制带 <title> 悬停提示的圆点,
// 更密的序列只画折线避免杂乱。全零数据折线贴 X 轴并跳过网格;points 为空时
// 绘制空坐标轴。
func LineSVG(title, subtitle string, points []Bar) string {
	c := defaultChartCanvas()
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#131a22\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"16\" fill=\"#dee8f2\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"12\" fill=\"#8ca0b4\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	plotW, plotH := c.plotWidth(), c.plotHeight()
	baseY := c.plotBottomY()
	// 坐标轴。
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#31435a\"/>\n",
		c.left, baseY-plotH, c.left, baseY)
	fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#31435a\"/>\n",
		c.left, baseY, c.left+plotW, baseY)

	// Y 轴最大值与三条等分网格:同柱状图取 max 向上取整到「1/3 最大值」的
	// 整数倍;全零数据网格与 X 轴重合,跳过仅留坐标轴。
	var maxVal int64
	for _, p := range points {
		if p.Value > maxVal {
			maxVal = p.Value
		}
	}
	yMax := yScaleMax(maxVal)
	for i := 1; maxVal > 0 && i <= 3; i++ {
		v := yMax / 3 * int64(i)
		y := baseY - int(float64(plotH)*float64(v)/float64(yMax))
		fmt.Fprintf(&b, "  <line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"#223041\"/>\n",
			c.left, y, c.left+plotW, y)
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"end\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"11\" fill=\"#7f92a4\">%s</text>\n",
			c.left-6, y+4, querier.FormatTokens(v))
	}

	if len(points) > 0 {
		slot := float64(plotW) / float64(len(points))
		// 折线坐标:与柱状图同一比例,x 取槽位中点,y 按 yMax 归一(yMax 为 0
		// 的防御分支全部落回基线);零值点同样产出坐标,保证折线连续。
		coords := make([]string, 0, len(points))
		for i, p := range points {
			x := float64(c.left) + float64(i)*slot + slot/2
			y := float64(baseY)
			if yMax > 0 {
				y -= float64(plotH) * float64(p.Value) / float64(yMax)
			}
			coords = append(coords, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, "  <polyline fill=\"none\" stroke=\"#5cc8ff\" stroke-width=\"2\" points=\"%s\"/>\n",
			strings.Join(coords, " "))
		// 逐点悬停圆点:超过 60 点时只画折线,密集圆点会杂乱难读。
		if len(points) <= 60 {
			for i, p := range points {
				cx, cy, _ := strings.Cut(coords[i], ",")
				fmt.Fprintf(&b, "  <circle cx=\"%s\" cy=\"%s\" r=\"2.5\" fill=\"#5cc8ff\"><title>%s</title></circle>\n",
					cx, cy, svgEscape(p.Hover))
			}
		}
		// X 轴标签抽样与钳制:同柱状图,首尾标签不越出画布边缘。
		labels := xAxisLabels(points, 8)
		for _, l := range labels {
			x := c.left + int(float64(l.index)*slot+slot/2)
			if x < c.left+34 {
				x = c.left + 34
			}
			if x > c.width-34 {
				x = c.width - 34
			}
			fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">%s</text>\n",
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
func xAxisLabels(bars []Bar, max int) []chartXLabel {
	if len(bars) == 0 {
		return nil
	}
	if max < 2 {
		return nil // 均匀抽样至少需要两个端点,防御除零
	}
	if len(bars) <= max {
		out := make([]chartXLabel, len(bars))
		for i, bar := range bars {
			out[i] = chartXLabel{i, bar.Label}
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
		out = append(out, chartXLabel{idx, bars[idx].Label})
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
	"#5cc8ff", "#ffb454", "#57d9a3", "#d98ce5", "#ff8a66",
	"#7a9eff", "#ffd166", "#63d3ff", "#9ae6b4", "#f2789f",
}

// Slice 是一个饼图扇区:标签、值、悬停提示与取色。
type Slice struct {
	Label string
	Value int64
	Hover string
	Color string
}

// PieSVG 生成占比饼图的独立 SVG 文档:左侧扇区(原生 path 圆弧),
// 右侧图例(色块+标签+百分比)。slices 为空或总和为 0 时输出无数据文本,
// 不绘制任何扇区。
func PieSVG(title, subtitle string, slices []Slice) string {
	c := chartCanvas{width: 760, height: 420, left: 24, right: 24, top: 64, bottom: 24}
	// 图例逐项按 26px 下移,最后一项底缘为 70+26n px;非时间维度不限制
	// 类别数,超出默认高度容纳量(12 项)时按图例需求扩高,避免高基数图例
	// 被视口底部裁剪(与热力图的动态行高同理)。
	if h := 70 + 26*len(slices) + c.bottom; h > c.height {
		c.height = h
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#131a22\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"16\" fill=\"#dee8f2\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"12\" fill=\"#8ca0b4\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	cx, cy, r := 240, 240, 150
	var total int64
	for _, sl := range slices {
		total += sl.Value
	}
	if total <= 0 {
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"14\" fill=\"#7f92a4\">%s</text>\n",
			cx, cy, svgEscape(ui.Bi("no data", "无数据")))
		b.WriteString("</svg>\n")
		return b.String()
	}

	// 扇区累加:起点固定 12 点方向(-90°),顺时针;比例角度,累计到 360° 收口。
	const twoPi = 2 * 3.141592653589793
	angle := -twoPi / 4
	for i, sl := range slices {
		frac := float64(sl.Value) / float64(total)
		sweep := frac * twoPi
		if sweep <= 0 {
			continue
		}
		// 单扇区独占全圆(100%)时 path 圆弧退化为零面积,退化为整圆。
		if sweep >= twoPi-1e-9 {
			fmt.Fprintf(&b, "  <circle cx=\"%d\" cy=\"%d\" r=\"%d\" fill=\"%s\"><title>%s</title></circle>\n",
				cx, cy, r, sl.Color, svgEscape(sl.Hover))
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
				cx, cy, x1, y1, r, r, large, x2, y2, sl.Color, svgEscape(sl.Hover))
		}
		// 图例:色块 + 标签 + 百分比。
		lx, ly := 460, 84+i*26
		fmt.Fprintf(&b, "  <rect x=\"%d\" y=\"%d\" width=\"12\" height=\"12\" fill=\"%s\"/>\n", lx, ly, sl.Color)
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"12\" fill=\"#8ca0b4\">%s (%.1f%%)</text>\n",
			lx+20, ly+10, svgEscape(sl.Label), frac*100)
		angle += sweep
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// cos/sin 是 math 包的同名包装:集中引入便于一致替换或测试打桩。
func cos(x float64) float64 { return math.Cos(x) }
func sin(x float64) float64 { return math.Sin(x) }

// heatFills 是 SVG 热力格子的 11 级取色(下标 0..10):0 级为无数据的底色
// 深蓝灰,1..10 级为深蓝到浅青的单色渐变(值越大越亮),对应 querier 热力的
// 「0 值 + 9 级」且 SVG 侧有值至少 1 级(与终端空格语义区分)。
var heatFills = []string{
	"#141c26", "#12324a", "#1b4d6e", "#26719c", "#3898c6", "#45a9d4",
	"#5cc8ff", "#79d4ff", "#96e0ff", "#b3ebff", "#d0f5ff",
}

// HeatmapSVG 渲染星期×小时热力矩阵:行=ISO 周序 7 星期,列=24 小时,
// 格子取色按交点值相对最大值的 11 级渐变(0 级底色为无数据),每格带悬停
// 提示(星期/小时/tokens)。行列由 weekdays/hours 与 values 回调按下标对应。
func HeatmapSVG(title, subtitle string, weekdays, hours []string, values func(wi, hi int) int64) string {
	// 高度随星期行数动态收紧:头部 + 行数×格高 + 图例区 + 底边距。
	c := chartCanvas{width: 860, left: 150, right: 24, top: 64, bottom: 24}
	c.height = c.top + len(weekdays)*26 + 46 + c.bottom
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+"\n",
		c.width, c.height, c.width, c.height)
	fmt.Fprintf(&b, "  <title>%s</title>\n", svgEscape(title))
	fmt.Fprintf(&b, "  <rect width=\"%d\" height=\"%d\" fill=\"#131a22\"/>\n", c.width, c.height)
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"26\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"16\" fill=\"#dee8f2\">%s</text>\n",
		c.width/2, svgEscape(title))
	fmt.Fprintf(&b, "  <text x=\"%d\" y=\"48\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"12\" fill=\"#8ca0b4\">%s</text>\n",
		c.width/2, svgEscape(subtitle))

	if len(hours) == 0 || len(weekdays) == 0 {
		// 空矩阵(无数据):只输出标题,不绘制网格。
		fmt.Fprintf(&b, "  <text x=\"%d\" y=\"%d\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"14\" fill=\"#7f92a4\">%s</text>\n",
			c.width/2, c.height/2, svgEscape(ui.Bi("no data", "无数据")))
		b.WriteString("</svg>\n")
		return b.String()
	}

	cellW, cellH, gap := 26.0, 26.0, 2.0
	originX := float64(c.left)
	originY := float64(c.top)

	// 两遍计算:先求最大值,再渲染(取色需全局最大)。
	maxVal := int64(0)
	for wi := range weekdays {
		for hi := range hours {
			if v := values(wi, hi); v > maxVal {
				maxVal = v
			}
		}
	}

	// 列头:每 2 小时标注一次,避免拥挤。
	fmt.Fprintf(&b, "  <text x=\"%.0f\" y=\"%.0f\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">%s</text>\n",
		originX+float64(0)*cellW+cellW/2, originY-6, svgEscape(hours[0]))
	for hi := 2; hi < len(hours); hi += 2 {
		fmt.Fprintf(&b, "  <text x=\"%.0f\" y=\"%.0f\" text-anchor=\"middle\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">%s</text>\n",
			originX+float64(hi)*cellW+cellW/2, originY-6, svgEscape(hours[hi]))
	}

	// 级数折算复用 querier.HeatLevel(0..9):0 值(缺失交点)取 heatFills[0]
	// 无数据底色,有值交点取 2..11 档(与终端空格语义区分,深底上以亮色渐进);
	// 相对占比再小的正值也至少取最低正值档 heatFills[2],不得折算回
	// 无数据底色。
	fill := func(v int64) string {
		if v == 0 {
			return heatFills[0]
		}
		l := querier.HeatLevel(v, maxVal)
		if l < 1 {
			l = 1
		}
		return heatFills[l+1]
	}

	for wi, wd := range weekdays {
		y := originY + float64(wi)*cellH
		fmt.Fprintf(&b, "  <text x=\"%.0f\" y=\"%.0f\" text-anchor=\"end\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"11\" fill=\"#8ca0b4\">%s</text>\n",
			originX-8, y+cellH/2+4, svgEscape(wd))
		for hi, h := range hours {
			v := values(wi, hi)
			x := originX + float64(hi)*cellW
			fmt.Fprintf(&b, "  <rect x=\"%.0f\" y=\"%.0f\" width=\"%.0f\" height=\"%.0f\" fill=\"%s\"><title>%s %s: %s tokens</title></rect>\n",
				x+gap/2, y+gap/2, cellW-gap, cellH-gap, fill(v),
				svgEscape(wd), svgEscape(h), svgEscape(querier.FormatTokens(v)))
		}
	}

	// 图例:色阶条(11 格)与两端标注,读者无需悬停即可理解取色语义。
	legendY := originY + float64(len(weekdays))*cellH + 20
	fmt.Fprintf(&b, "  <text x=\"%.0f\" y=\"%.0f\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">0</text>\n", originX, legendY+10)
	for i := range heatFills {
		fmt.Fprintf(&b, "  <rect x=\"%.0f\" y=\"%.0f\" width=\"18\" height=\"12\" fill=\"%s\"/>\n",
			originX+14+float64(i)*20, legendY, heatFills[i])
	}
	fmt.Fprintf(&b, "  <text x=\"%.0f\" y=\"%.0f\" font-family=\"ui-monospace,'SF Mono','Cascadia Code',Menlo,Consolas,monospace\" font-size=\"10\" fill=\"#7f92a4\">%s</text>\n",
		originX+14+float64(len(heatFills))*20+6, legendY+10,
		svgEscape(querier.FormatTokens(maxVal)))
	b.WriteString("</svg>\n")
	return b.String()
}
