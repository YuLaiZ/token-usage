package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
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

// report 的 compare.txt 缺省基线按原始日期参数粒度推导(与 compare 命令
// 合同一致):单月对上一个日历月、闰年二月的上月同样完整取 1 日..月末、
// 闰单年对上一个日历年。逐日展开丢失粒度时基线会退化为等日数窗口
// (202609 错成 2026-08-02..31),负向断言钉住该退化形态。
func TestReportCmd_MonthYearBaseline(t *testing.T) {
	// 每个子测试独立内存库:命令 RunE 会关闭注入的 DB,共享实例会被首个
	// 用例关闭。
	cases := []struct {
		name    string
		dateArg string
		base    string
		stale   string
	}{
		{"单月对上一日历月", "202609", "2026-08-01 .. 2026-08-31", "2026-08-02"},
		{"闰年二月的上月完整", "202402", "2024-01-01 .. 2024-01-31", "2024-01-03"},
		{"闰单年对上一日历年", "2024", "2023-01-01 .. 2023-12-31", "2022-12-31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(t.TempDir(), "report")
			cmd := newReportCmdWithDeps(
				func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
				func(string) (*db.DB, error) { return usageDB, nil },
			)
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{tc.dateArg, "--out", outDir})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(outDir, "compare.txt"))
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			if !strings.Contains(content, tc.base) {
				t.Errorf("compare.txt 基线窗口应为 %q:\n%s", tc.base, content)
			}
			if strings.Contains(content, tc.stale) {
				t.Errorf("compare.txt 不应出现等日数退化基线 %q:\n%s", tc.stale, content)
			}
		})
	}
}

// report 查询失败时不产出报告包:取消上下文使命令在首个查询即报错,
// --out 目录不得创建(半成品报告比无报告更误导)。
func TestReportCmd_QueryErrorNoBundle(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "report")
	cmd := newReportCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return usageDB, nil },
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"20260901", "--out", outDir})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("查询失败时命令应报错")
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Errorf("查询失败不应创建报告目录: %v", err)
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
		name      string
		dates     []string
		singleLen int
		base      string
	}{
		{"跨月", []string{"2026-09-01", "2026-09-02", "2026-09-03"}, 0, "2026-08-29 .. 2026-08-31"},
		{"跨年", []string{"2026-01-01", "2026-01-02"}, 0, "2025-12-30 .. 2025-12-31"},
		{"单日", []string{"2026-09-02"}, 8, "2026-09-01 .. 2026-09-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label := tc.dates[0] + " ~ " + tc.dates[len(tc.dates)-1]
			files, err := reportFiles(context.Background(), q, tc.dates, label, tc.singleLen, nil)
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

// reportTotalOf 从渲染产物中提取区间总量口径:SVG 副标题 "Total X tokens"
// 与 summary 的 Total 行 "Total / 总计: X" 同用 FormatTokens 缩写。
func reportTotalOf(t *testing.T, name, content string) string {
	t.Helper()
	if strings.HasSuffix(name, ".svg") {
		m := regexp.MustCompile(`Total (\S+) tokens`).FindStringSubmatch(content)
		if m == nil {
			t.Fatalf("%s 应含副标题总量:\n%s", name, content)
		}
		return m[1]
	}
	m := regexp.MustCompile(`Total / 总计: (\S+)`).FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("%s 应含 Total 行:\n%s", name, content)
	}
	return m[1]
}

// 报告包整体数据一致性:并发写入(翻转 UPDATE)下,summary 总量与全部图表
// 副标题必须来自同一读快照——各查询独立快照时副标题会在 100/200 间漂移。
// 文件库才启用 WAL 并发读。
func TestReportFiles_SingleSnapshotUnderConcurrency(t *testing.T) {
	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if _, err := db.UpsertMessages(context.Background(), usageDB, []model.Message{{
		ID: "snap-a", SessionID: "s", Client: model.ClientClaudeCode,
		Date: "2026-09-01", TS: time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local).UnixMilli(),
		Model: "model-x", TotalTokens: 100,
	}}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	defer func() { close(stop); <-done }()
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = usageDB.Exec("UPDATE messages SET total_tokens=300-total_tokens")
		}
	}()

	q := querier.New(usageDB)
	for i := 0; i < 50; i++ {
		files, err := reportFiles(context.Background(), q, []string{"2026-09-01"}, "2026-09-01", 8, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		for _, f := range files {
			content, err := f.render()
			if err != nil {
				t.Fatal(err)
			}
			if f.name == "compare.txt" {
				continue // 基线窗口(前一天)无数据恒零,不参与口径比对
			}
			got := reportTotalOf(t, f.name, content)
			if want == "" {
				want = got
				continue
			}
			if got != want {
				t.Fatalf("报告包数据跨快照漂移: %s=%s 其他=%s (iteration=%d)", f.name, got, want, i)
			}
		}
	}
}
