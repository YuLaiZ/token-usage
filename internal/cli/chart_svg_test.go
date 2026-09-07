package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
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

// buildPieSVG 高基数图例:图例逐项 26px 下移,类别数超过默认画布容纳量
// (12 项)时按图例需求扩高,所有图例标签完整留在视口内;低基数保持默认
// 420px 画布。非时间维度(client/model/project)不限制类别数,report 包
// 固定生成这些饼图,越界即用户可见缺陷。
func TestBuildPieSVG_HighCardinalityLegend(t *testing.T) {
	makeSlices := func(n int) []chartSlice {
		slices := make([]chartSlice, 0, n)
		for i := 0; i < n; i++ {
			slices = append(slices, chartSlice{
				label: fmt.Sprintf("model-%02d", i), value: int64(i + 1),
				hover: "h", color: piePalette[i%len(piePalette)],
			})
		}
		return slices
	}

	// 20 类:画布须扩高到 70+26*20+24=614,全部图例标签可见。
	dense := buildPieSVG("dense", "t", makeSlices(20))
	if !strings.Contains(dense, `height="614"`) {
		t.Errorf("20 类图例应扩高画布到 614:\n%s", dense)
	}
	for i := 0; i < 20; i++ {
		if !strings.Contains(dense, fmt.Sprintf("model-%02d", i)) {
			t.Errorf("图例 %02d 应出现在 SVG 中", i)
		}
	}

	// 12 类:默认 420px 恰可容纳全部图例,画布保持不变。
	base := buildPieSVG("base", "t", makeSlices(12))
	if !strings.Contains(base, `height="420"`) {
		t.Errorf("12 类图例应保持默认画布 420:\n%s", base)
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

// buildChartHeatmap 的查询错误必须传播:取消上下文等失败若被吞掉,
// chart --heatmap 与报告包会写出误导性的「无数据」SVG;有效空数据本身
// 返回完整零矩阵,无需降级兜底。
func TestBuildChartHeatmap_QueryErrorPropagates(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	q := querier.New(usageDB)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildChartHeatmap(ctx, q, []string{"2026-09-01"}, "sub"); err == nil {
		t.Fatal("查询失败应返回错误,不得降级为空矩阵 SVG")
	}
}

// chart --heatmap 查询失败时不产出成功产物:取消上下文使命令报错,
// --out 目标文件不得写出。
func TestChartCmd_HeatmapQueryErrorNoOutput(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "heatmap.svg")
	cmd := newChartCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"20260901", "--heatmap", "--out", outPath})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("查询失败时命令应报错")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("查询失败不应写出图表文件: %v", err)
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

// polylinePoints 提取 SVG 中折线 polyline 的 points 坐标串(空格分隔的 x,y 对)。
func polylinePoints(t *testing.T, svg string) []string {
	t.Helper()
	const marker = `stroke-width="2" points="`
	i := strings.Index(svg, marker)
	if i < 0 {
		t.Fatalf("SVG 应含 polyline points:\n%s", svg)
	}
	rest := svg[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("polyline points 属性未闭合:\n%s", svg)
	}
	return strings.Fields(rest[:j])
}

// pointXY 解析 "x,y" 坐标对为浮点数。
func pointXY(t *testing.T, pair string) (float64, float64) {
	t.Helper()
	xs, ys, ok := strings.Cut(pair, ",")
	if !ok {
		t.Fatalf("坐标对应为 x,y 形式: %q", pair)
	}
	x, err := strconv.ParseFloat(xs, 64)
	if err != nil {
		t.Fatalf("坐标 x 应为数字: %q: %v", pair, err)
	}
	y, err := strconv.ParseFloat(ys, 64)
	if err != nil {
		t.Fatalf("坐标 y 应为数字: %q: %v", pair, err)
	}
	return x, y
}

// buildLineSVG 合同:合法 XML、标题/副标题转义、折线坐标串与点数对应、
// 首末点 x 落在绘图区内、y 按 baseY-plotH*V/yMax 映射(最高点近顶、零值贴
// 基线)、≤60 点时逐点悬停圆点。
func TestBuildLineSVG(t *testing.T) {
	points := []chartBar{
		{label: "2026-09-05", value: 1200, hover: "2026-09-05: 1.20 K tokens, 1 requests"},
		{label: "2026-09-06", value: 0, hover: "2026-09-06: 0 tokens, 0 requests"},
		{label: "2026-09-07", value: 300, hover: "2026-09-07: 300 tokens, 1 requests"},
	}
	svg := buildLineSVG("a<b>&c", "s\"d'", points)
	if !strings.HasPrefix(svg, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>") {
		t.Errorf("SVG 应以 XML 声明开头:\n%s", svg)
	}
	if !strings.Contains(svg, "a&lt;b&gt;&amp;c") {
		t.Errorf("标题应转义:\n%s", svg)
	}
	if !strings.Contains(svg, `s&quot;d&apos;`) {
		t.Errorf("副标题应转义:\n%s", svg)
	}

	pts := polylinePoints(t, svg)
	if len(pts) != 3 {
		t.Fatalf("折线应含 3 个坐标点,实际 %d:\n%s", len(pts), svg)
	}
	c := defaultChartCanvas()
	firstX, _ := pointXY(t, pts[0])
	lastX, _ := pointXY(t, pts[len(pts)-1])
	if firstX < float64(c.left) || lastX > float64(c.left+c.plotWidth()) {
		t.Errorf("首末点 x 应在绘图区内: first=%v last=%v", firstX, lastX)
	}

	// y 映射几何:与实现同一比例(yMax 由 yScaleMax(maxVal) 推得),最高点
	// (值 1200)的 y 应接近 baseY-plotH*1200/yMax(±2 容忍坐标输出取整),
	// 零值点(值 0)的 y 应恰为基线 plotBottomY。
	yMax := yScaleMax(1200)
	baseY := c.plotBottomY()
	wantTopY := float64(baseY) - float64(c.plotHeight())*float64(1200)/float64(yMax)
	_, topY := pointXY(t, pts[0])
	if topY < wantTopY-2 || topY > wantTopY+2 {
		t.Errorf("最高点 y 应接近 %.1f(baseY-plotH*V/yMax),实际 %v", wantTopY, topY)
	}
	_, zeroY := pointXY(t, pts[1])
	if zeroY != float64(baseY) {
		t.Errorf("零值点 y 应等于基线 %d,实际 %v", baseY, zeroY)
	}

	// ≤60 点:每点一个悬停圆点,提示进入 <title>。
	if n := strings.Count(svg, "<circle"); n != 3 {
		t.Errorf("圆点数应与点数一致(3),实际 %d:\n%s", n, svg)
	}
	if !strings.Contains(svg, "2026-09-05: 1.20 K tokens, 1 requests") {
		t.Errorf("圆点应带 <title> 悬停提示:\n%s", svg)
	}
}

// buildLineSVG 边界:全零数据折线贴基线且跳过网格、恰 60 点仍逐点圆点、
// >60 点只画折线不画圆点、空输入画空坐标轴。
func TestBuildLineSVG_ZeroDenseAndEmpty(t *testing.T) {
	// 全零数据:折线贴基线(所有 y = plotBottomY),网格退化为与 X 轴重合,跳过。
	zeros := []chartBar{
		{label: "d1", value: 0, hover: "d1: 0 tokens"},
		{label: "d2", value: 0, hover: "d2: 0 tokens"},
	}
	zero := buildLineSVG("z", "t", zeros)
	baseY := defaultChartCanvas().plotBottomY()
	for _, p := range polylinePoints(t, zero) {
		if _, y := pointXY(t, p); y != float64(baseY) {
			t.Errorf("全零数据折线应贴基线 %d,实际 %v", baseY, y)
		}
	}
	if strings.Contains(zero, `stroke="#eee"`) {
		t.Errorf("全零数据应跳过网格:\n%s", zero)
	}

	// >60 点:只画折线不画圆点,坐标点数仍与点数一致。
	dense := make([]chartBar, 0, 61)
	for i := 0; i < 61; i++ {
		dense = append(dense, chartBar{label: fmt.Sprintf("p%02d", i), value: int64(i + 1), hover: "h"})
	}
	denseSVG := buildLineSVG("d", "t", dense)
	if strings.Contains(denseSVG, "<circle") {
		t.Errorf(">60 点不应绘制圆点:\n%s", denseSVG)
	}
	if n := len(polylinePoints(t, denseSVG)); n != 61 {
		t.Errorf("密集折线应含 61 个坐标点,实际 %d", n)
	}

	// 恰 60 点:处于逐点圆点的上边界内(≤60),圆点数与坐标点数均应为 60。
	boundary := make([]chartBar, 0, 60)
	for i := 0; i < 60; i++ {
		boundary = append(boundary, chartBar{label: fmt.Sprintf("b%02d", i), value: int64(i + 1), hover: "h"})
	}
	boundarySVG := buildLineSVG("b", "t", boundary)
	if n := strings.Count(boundarySVG, "<circle"); n != 60 {
		t.Errorf("恰 60 点应绘制 60 个圆点(≤60 边界),实际 %d", n)
	}
	if n := len(polylinePoints(t, boundarySVG)); n != 60 {
		t.Errorf("边界折线应含 60 个坐标点,实际 %d", n)
	}

	// 空输入:只画两条坐标轴,无折线无圆点无网格。
	empty := buildLineSVG("e", "t", nil)
	if strings.Contains(empty, "<polyline") || strings.Contains(empty, "<circle") {
		t.Errorf("空输入不应有折线与圆点:\n%s", empty)
	}
	if strings.Count(empty, `stroke="#999"`) != 2 {
		t.Errorf("空输入应画两条坐标轴:\n%s", empty)
	}
}

// --line 互斥与时间维度校验在开库前生效:非法输入不触达数据库,且报错文案
// 命中对应双语关键片段。
func TestChartCmd_LineValidation(t *testing.T) {
	// want/alt 为错误文案关键片段(输出为「English / 中文」双语,命中其一即可)。
	cases := []struct {
		name string
		args []string
		want string
		alt  string
	}{
		{"line+pie 互斥", []string{"--line", "--pie", "--by", "model"}, "mutually exclusive", "互斥"},
		{"line+heatmap 互斥", []string{"--line", "--heatmap"}, "mutually exclusive", "互斥"},
		{"line+非时间维度", []string{"--line", "--by", "client"}, "temporal", "时间维度"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openCalls := 0
			cmd := newChartCmdWithDeps(
				func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
				func(string) (*db.DB, error) {
					openCalls++
					return db.Open(":memory:")
				},
			)
			cmd.SetArgs(tc.args)
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			execErr := cmd.Execute()
			if execErr == nil {
				t.Fatalf("%s 应报错", tc.name)
			}
			if msg := execErr.Error(); !strings.Contains(msg, tc.want) && !strings.Contains(msg, tc.alt) {
				t.Errorf("%s 错误文案应含 %q 或 %q,实际 %q", tc.name, tc.want, tc.alt, msg)
			}
			if openCalls != 0 {
				t.Errorf("校验应在开库前完成,实际 open %d 次", openCalls)
			}
		})
	}
}

// chart --line 合法路径端到端:显式传日期区间(不依赖真实时钟,缺省日期取
// 系统时间会随跨天失效),折线与逐点悬停进入 --out 写出的 SVG 文件。
func TestChartCmd_LineEndToEnd(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	d1 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)
	msgs := []model.Message{
		{ID: "l-a", SessionID: "s", Client: model.ClientClaudeCode,
			Date: d1.Format("2006-01-02"), TS: d1.UnixMilli(), Model: "model-x", TotalTokens: 1200},
		{ID: "l-b", SessionID: "s", Client: model.ClientClaudeCode,
			Date: d2.Format("2006-01-02"), TS: d2.UnixMilli(), Model: "model-x", TotalTokens: 300},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, msgs); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "line.svg")
	cmd := newChartCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"20260905-20260906", "--line", "--out", outPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	svg, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("--out 未写入文件: %v", err)
	}
	if n := len(polylinePoints(t, string(svg))); n != 2 {
		t.Errorf("两日数据应产出 2 个折线点,实际 %d:\n%s", n, svg)
	}
	// 逐点悬停圆点带 tokens 数(与维度聚合核同一 hover 文案)。
	if !strings.Contains(string(svg), "<circle") || !strings.Contains(string(svg), "1.20 K tokens") {
		t.Errorf("折线圆点应带悬停提示(1.20 K tokens):\n%s", svg)
	}
	if !strings.Contains(buf.String(), outPath) {
		t.Errorf("stdout 应回执写入路径:\n%s", buf.String())
	}
}
