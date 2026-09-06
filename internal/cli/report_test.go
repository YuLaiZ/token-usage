package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// report 命令端到端:--out 目录生成全部报告文件(summary/compare 文本 + 8 张
// SVG),缺 --out 在开库前拒绝。
func TestReportCmd_EndToEnd(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	today := time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "r-a", SessionID: "s", Client: model.ClientClaudeCode,
		Date: today.Format("2006-01-02"), TS: today.UnixMilli(),
		Model: "model-x", TotalTokens: 900,
	}}); err != nil {
		t.Fatal(err)
	}

	// 嵌套不存在目录:覆盖 MkdirAll 自动创建路径。
	outDir := filepath.Join(t.TempDir(), "nested", "report")
	cmd := newReportCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--out", outDir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "11 个文件") {
		t.Errorf("应回执 11 个文件:\n%s", buf.String())
	}
	// 全部文件非空:summary 文本含标题,SVG 文件含结束标签。
	for _, f := range []string{
		"summary.txt", "compare.txt", "heatmap.svg", "daily.svg", "hourly.svg", "weekday.svg",
		"monthly.svg", "by-client.svg", "by-model.svg", "by-provider.svg", "by-project.svg",
	} {
		data, err := os.ReadFile(filepath.Join(outDir, f))
		if err != nil {
			t.Fatalf("报告文件缺失 %s: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("报告文件 %s 为空", f)
		}
	}
	// 内容断言:summary.txt 自证统计范围;SVG 文件为完整文档。
	summaryData, err := os.ReadFile(filepath.Join(outDir, "summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summaryData), "Query range / 统计范围") {
		t.Errorf("summary.txt 应含统计范围头:\n%s", summaryData)
	}
	// 内容断言:compare.txt 为两期对比文本(E2E 走缺省「今天」窗口,确切
	// 窗口日期在 TestReportFiles_CompareTxt 中覆盖)。
	compareData, err := os.ReadFile(filepath.Join(outDir, "compare.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compareData), "Compare / 用量对比") {
		t.Errorf("compare.txt 应含对比标题:\n%s", compareData)
	}
	dailySVG, err := os.ReadFile(filepath.Join(outDir, "daily.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dailySVG), "</svg>") {
		t.Errorf("daily.svg 应为完整 SVG 文档")
	}
}

// 缺 --out:在打开数据库之前拒绝。
func TestReportCmd_MissingOut(t *testing.T) {
	openCalls := 0
	cmd := newReportCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) {
			openCalls++
			return db.Open(":memory:")
		},
	)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("缺 --out 应报错")
	}
	if openCalls != 0 {
		t.Errorf("校验应在开库前完成,实际 open %d 次", openCalls)
	}
}

// report 包 compare.txt:直接验证窗口推导与渲染内容。基线为结束于当前窗口
// 开始日前一天的等长窗口(缺省规则与 compare 命令一致),覆盖跨月、跨年与
// 单日边界;当前窗口行、基线窗口行与 Total 行逐项断言。
func TestReportFiles_CompareTxt(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	day := time.Date(2026, 9, 2, 12, 0, 0, 0, time.Local)
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "r-c", SessionID: "s", Client: model.ClientClaudeCode,
		Date: day.Format("2006-01-02"), TS: day.UnixMilli(),
		Model: "model-x", TotalTokens: 300,
	}}); err != nil {
		t.Fatal(err)
	}

	q := querier.New(usageDB)
	cases := []struct {
		name  string
		dates []string
		base  string
	}{
		{"跨月", []string{"2026-09-01", "2026-09-02", "2026-09-03"}, "2026-08-29 .. 2026-08-31"},
		{"跨年", []string{"2026-01-01", "2026-01-02"}, "2025-12-30 .. 2025-12-31"},
		{"单日", []string{"2026-09-02"}, "2026-09-01 .. 2026-09-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label := tc.dates[0] + " ~ " + tc.dates[len(tc.dates)-1]
			files, err := reportFiles(context.Background(), q, tc.dates, label)
			if err != nil {
				t.Fatal(err)
			}
			// compare.txt 紧随 summary.txt:第二个文本文件的位置。
			if files[0].name != "summary.txt" || files[1].name != "compare.txt" {
				t.Fatalf("compare.txt 应紧跟 summary.txt,实际前两项为 %s、%s", files[0].name, files[1].name)
			}
			content, err := files[1].render()
			if err != nil {
				t.Fatal(err)
			}
			curWindow := tc.dates[0] + " .. " + tc.dates[len(tc.dates)-1]
			for _, want := range []string{
				"Compare / 用量对比",
				curWindow,
				tc.base,
				ui.ColTotal,
			} {
				if !strings.Contains(content, want) {
					t.Errorf("compare.txt 应含 %q:\n%s", want, content)
				}
			}
		})
	}
}
