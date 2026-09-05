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
)

// report 命令端到端:--out 目录生成全部报告文件(summary 文本 + 8 张 SVG),
// 缺 --out 在开库前拒绝。
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
	if !strings.Contains(buf.String(), "10 个文件") {
		t.Errorf("应回执 10 个文件:\n%s", buf.String())
	}
	// 全部文件非空:summary 文本含标题,SVG 文件含结束标签。
	for _, f := range []string{
		"summary.txt", "heatmap.svg", "daily.svg", "hourly.svg", "weekday.svg",
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
