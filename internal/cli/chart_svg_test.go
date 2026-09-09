package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

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

// chart 与 report 的维度聚合须应用 [provider_aliases]:两个供应商别名合并
// 后,饼图/报告包只应出现合并显示键,分组与占比与 query/export 入口一致。
func TestChartCmd_ProviderAliasesApplied(t *testing.T) {
	cfg := &config.Config{
		DataDir:         t.TempDir(),
		ProviderAliases: map[string]string{"vendor-a": "merged-vendor", "vendor-b": "merged-vendor"},
	}
	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	// chart 与 report 各用独立内存库:命令 RunE 会关闭注入的 DB,共享实例
	// 会被首个命令关闭。
	mkDB := func() *db.DB {
		d, err := db.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		msgs := []model.Message{
			{ID: "pa-a", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-09-01", TS: stamp.UnixMilli(), Provider: "vendor-a", TotalTokens: 100},
			{ID: "pa-b", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-09-01", TS: stamp.UnixMilli(), Provider: "vendor-b", TotalTokens: 200},
		}
		if _, err := db.UpsertMessages(context.Background(), d, msgs); err != nil {
			t.Fatal(err)
		}
		return d
	}
	usageDB := mkDB()
	load := func() (*config.Config, error) { return cfg, nil }
	open := func(string) (*db.DB, error) { return usageDB, nil }

	// chart --pie --by provider:扇区图例只有合并显示键。
	var out bytes.Buffer
	chartCmd := newChartCmdWithDeps(load, open)
	chartCmd.SetOut(&out)
	chartCmd.SetErr(&out)
	chartCmd.SetArgs([]string{"20260901", "--by", "provider", "--pie"})
	if err := chartCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	svg := out.String()
	if !strings.Contains(svg, "merged-vendor") || strings.Contains(svg, "vendor-a") || strings.Contains(svg, "vendor-b") {
		t.Errorf("chart 饼图应合并供应商别名:\n%s", svg)
	}

	// report 包 by-provider.svg:同口径。
	usageDB = mkDB()
	outDir := filepath.Join(t.TempDir(), "report")
	reportCmd := newReportCmdWithDeps(load, open)
	reportCmd.SetOut(&out)
	reportCmd.SetErr(&out)
	reportCmd.SetArgs([]string{"20260901", "--out", outDir})
	if err := reportCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "by-provider.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "merged-vendor") || strings.Contains(string(data), "vendor-a") {
		t.Errorf("报告包饼图应合并供应商别名:\n%s", data)
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
