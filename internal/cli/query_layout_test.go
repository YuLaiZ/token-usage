package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// layoutHeaderCells 返回 CLI 输出中首条含 │ 的表头英文行拆出的单元格。
func layoutHeaderCells(t *testing.T, out string) []string {
	t.Helper()
	var header string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "│") {
			header = ln
			break
		}
	}
	if header == "" {
		t.Fatalf("输出缺少表头行:\n%s", out)
	}
	cells := strings.Split(strings.Trim(header, "│"), "│")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// countingOpenNoFail 计数开库次数且总返回可用的内存库。
func countingOpen(t *testing.T, counter *int) func(string) (*db.DB, error) {
	return func(p string) (*db.DB, error) {
		*counter++
		return memOpen(t)(p)
	}
}

// 五个静态表格命令应用有效布局,summary 不读取布局且输出保持 Task 0 确认的 golden。
func TestStaticTableCommands_ApplyOutputLayout(t *testing.T) {
	raw := map[string]any{
		"subqueries": map[string]any{"mpc": "model,"}, // 坏视图定义:不得影响静态布局
		"output":     map[string]any{"columns": []any{"total", "requests"}},
	}
	open := memOpen(t)

	for _, view := range []queryView{viewClient, viewModel, viewProvider, viewProject, viewSessions} {
		cmd, buf := newQueryOutputCmd()
		if err := runQueryWithDeps(cmd, []string{"20260709"}, view, loadWithRaw(raw, nil), open); err != nil {
			t.Fatalf("静态视图 %d 应不受坏视图定义阻断: %v", view, err)
		}
		header := layoutHeaderCells(t, buf.String())
		dimCount := 1
		if view == viewSessions {
			dimCount = 3 // Client / Project / Title 固定在前
		}
		if got := strings.Join(header[dimCount:], "|"); got != "Total|Requests" {
			t.Errorf("静态视图 %d 布局 = %q, want Total|Requests:\n%s", view, got, buf.String())
		}
	}

	// summary 不读取布局:即使布局只留两列,摘要仍完整输出含 Cache Create。
	cmd, buf := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd, []string{"20260709"}, viewSummary, loadWithRaw(raw, nil), open); err != nil {
		t.Fatalf("summary 应正常执行: %v", err)
	}
	wantSummary := "Summary / 总览摘要\n\nClients / 客户端数: 2\nTotal requests / 请求总数: 2\nInput / 输入: 0\nOutput / 输出: 0\nCache Read / 缓存读取: 0\nCache Create / 缓存创建: 0\nReasoning / 推理: 0\nTotal / 总计: 0\n\n"
	if idx := strings.Index(buf.String(), "Summary / 总览摘要"); idx < 0 || buf.String()[idx:] != wantSummary {
		t.Errorf("summary golden 不变(含 Cache Create):\ngot:\n%q\nwant:\n%q", buf.String()[idx:], wantSummary)
	}
}

// 裸 query、直接具名、custom 与 group 的每张表共享完整解析后的同一布局。
func TestFullQueryPaths_ShareLayout(t *testing.T) {
	raw := map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,model,mpc"},
		"output":     map[string]any{"columns": []any{"cache_create", "total"}},
	}
	open := memOpen(t)

	assertTail := func(t *testing.T, out, want string) {
		t.Helper()
		header := layoutHeaderCells(t, out)
		if got := strings.Join(header[len(header)-len(strings.Split(want, "|")):], "|"); got != want {
			t.Errorf("布局尾列 = %q, want %q:\n%s", got, want, out)
		}
	}

	t.Run("bare query renders every table with layout", func(t *testing.T) {
		cmd, buf := newQueryOutputCmd()
		if err := runQueryWithDeps(cmd, []string{"20260709"}, viewDefault, loadWithRaw(raw, nil), open); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, title := range []string{"按客户端分组", "按模型分组", "自定义视图 mpc"} {
			if !strings.Contains(out, title) {
				t.Errorf("缺表 %q:\n%s", title, out)
			}
		}
		assertTail(t, out, "Cache Create|Total")
	})

	t.Run("named direct and custom share layout", func(t *testing.T) {
		cmd, buf := newQueryOutputCmd()
		if err := runQueryNamedWithDeps(cmd, "mpc", []string{"2026-07-09"}, loadWithRaw(raw, nil), open); err != nil {
			t.Fatal(err)
		}
		assertTail(t, buf.String(), "Cache Create|Total")
		cmd2, buf2 := newQueryOutputCmd()
		if err := runQueryCustomWithDeps(cmd2, "mpc", []string{"20260709"}, loadWithRaw(raw, nil), open); err != nil {
			t.Fatal(err)
		}
		assertTail(t, buf2.String(), "Cache Create|Total")
		cmd3, buf3 := newQueryOutputCmd()
		if err := runQueryCustomWithDeps(cmd3, "group_q", []string{"20260709"}, loadWithRaw(raw, nil), open); err != nil {
			t.Fatal(err)
		}
		assertTail(t, buf3.String(), "Cache Create|Total")
	})
}

// 坏 subqueries/groups 不影响静态表格命令的有效布局;
// 相同配置下裸/具名/list 仍在开库前按完整定义失败。
func TestBadViewsDoNotBlockStaticLayout(t *testing.T) {
	raw := map[string]any{
		"subqueries": map[string]any{"mpc": "model,"},
		"output":     map[string]any{"columns": []any{"total"}},
	}
	opens := 0
	open := countingOpen(t, &opens)

	cmd, buf := newQueryOutputCmd()
	if err := runQueryWithDeps(cmd, []string{"20260709"}, viewClient, loadWithRaw(raw, nil), open); err != nil {
		t.Fatalf("坏视图定义不得阻断静态 client: %v", err)
	}
	header := layoutHeaderCells(t, buf.String())
	if got := strings.Join(header[1:], "|"); got != "Total" {
		t.Errorf("静态 client 布局 = %q, want Total:\n%s", got, buf.String())
	}

	before := opens
	bare, _ := newQueryOutputCmd()
	if err := runQueryWithDeps(bare, nil, viewDefault, loadWithRaw(raw, nil), open); err == nil {
		t.Fatal("坏视图定义应拒绝裸 query")
	}
	named, _ := newQueryOutputCmd()
	if err := runQueryNamedWithDeps(named, "mpc", []string{"2026-07-09"}, loadWithRaw(raw, nil), open); err == nil {
		t.Fatal("坏视图定义应拒绝具名查询")
	}
	if err := runQueryListWithDeps(&bytes.Buffer{}, loadWithRaw(raw, nil)); err == nil {
		t.Fatal("坏视图定义应拒绝 query list")
	}
	if opens != before {
		t.Errorf("完整路径失败不得打开数据库: 额外 %d 次", opens-before)
	}
}

// query.output.foo 与拼写错误的 query.output.colums 均在开库前阻止五个
// 受影响静态表格命令;summary 保持可执行;裸/具名/list 按完整配置路径失败。
func TestBadOutputBlocksStaticTableCommands(t *testing.T) {
	for name, output := range map[string]any{
		"unknown output key": map[string]any{"foo": []any{"total"}},
		"misspelled colums":  map[string]any{"colums": []any{"total"}},
		"columns not array":  map[string]any{"columns": "total"},
		"unknown column id":  map[string]any{"columns": []any{"totals"}},
		"empty column array": map[string]any{"columns": []any{}},
		"output not a table": "total",
	} {
		t.Run(name, func(t *testing.T) {
			raw := map[string]any{"output": output}
			opens := 0
			open := countingOpen(t, &opens)

			for _, view := range []queryView{viewClient, viewModel, viewProvider, viewProject, viewSessions} {
				cmd, _ := newQueryOutputCmd()
				err := runQueryWithDeps(cmd, []string{"20260709"}, view, loadWithRaw(raw, nil), open)
				if err == nil {
					t.Fatalf("静态视图 %d 应被坏 output 阻止", view)
				}
				if !strings.Contains(err.Error(), "query.output") {
					t.Errorf("错误应定位 query.output: %v", err)
				}
			}
			if opens != 0 {
				t.Fatalf("坏 output 应在开库前失败,实际开库 %d 次", opens)
			}

			// summary 不读取布局,保持可执行。
			cmdSum, bufSum := newQueryOutputCmd()
			if err := runQueryWithDeps(cmdSum, []string{"20260709"}, viewSummary, loadWithRaw(raw, nil), open); err != nil {
				t.Fatalf("summary 不受坏 output 影响: %v", err)
			}
			if !strings.Contains(bufSum.String(), "Cache Create / 缓存创建") {
				t.Errorf("summary 应保持完整摘要:\n%s", bufSum.String())
			}

			// 完整路径同样失败且不开库。
			bare, _ := newQueryOutputCmd()
			if err := runQueryWithDeps(bare, nil, viewDefault, loadWithRaw(raw, nil), open); err == nil {
				t.Fatal("裸 query 应按完整配置失败")
			}
			named, _ := newQueryOutputCmd()
			if err := runQueryNamedWithDeps(named, "mpc", []string{"2026-07-09"}, loadWithRaw(raw, nil), open); err == nil {
				t.Fatal("具名查询应按完整配置失败")
			}
			if err := runQueryListWithDeps(&bytes.Buffer{}, loadWithRaw(raw, nil)); err == nil {
				t.Fatal("query list 应按完整配置失败")
			}
			if opens != 1 { // 仅 summary 一次
				t.Errorf("仅 summary 可开库,实际 %d 次", opens)
			}
		})
	}
}

// 顶层 query 问题态:五个表格命令静默使用默认七列,summary 输出不变,
// 完整 query 路径保持既有定位错误。
func TestTopLevelIssues_StaticCommandsUseDefaultLayout(t *testing.T) {
	issues := map[string]config.RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}
	open := memOpen(t)

	for _, view := range []queryView{viewClient, viewModel, viewProvider, viewProject, viewSessions} {
		cmd, buf := newQueryOutputCmd()
		if err := runQueryWithDeps(cmd, []string{"20260709"}, view, loadWithRaw(nil, issues), open); err != nil {
			t.Fatalf("顶层问题态下静态视图 %d 应可用: %v", view, err)
		}
		header := layoutHeaderCells(t, buf.String())
		want := "Requests|Input|Output|Cache Read|Reasoning|Total|Cache Hit"
		if got := strings.Join(header[len(header)-7:], "|"); got != want {
			t.Errorf("顶层问题态应回退默认七列,静态视图 %d = %q:\n%s", view, got, buf.String())
		}
	}

	cmdSum, bufSum := newQueryOutputCmd()
	if err := runQueryWithDeps(cmdSum, []string{"20260709"}, viewSummary, loadWithRaw(nil, issues), open); err != nil {
		t.Fatalf("summary 不受顶层问题影响: %v", err)
	}
	if !strings.Contains(bufSum.String(), "Cache Create / 缓存创建") {
		t.Errorf("summary 输出应保持:\n%s", bufSum.String())
	}

	bare, _ := newQueryOutputCmd()
	err := runQueryWithDeps(bare, nil, viewDefault, loadWithRaw(nil, issues), open)
	if err == nil || !strings.Contains(err.Error(), "Query") {
		t.Errorf("完整路径应保持既有顶层定位错误: %v", err)
	}
}

// 生产 QueryAdapter 的局部入口共享顶层共同前置:顶层问题态下四个方法全部
// 报顶层诊断,Views/OutputLayout 不得返回可写结果——否则坏顶层配置会被
// 后续复用方误当成有效空视图或默认布局。
func TestTUIQueryAdapter_TopLevelIssuesSharedPrecondition(t *testing.T) {
	adapter := tuiQueryAdapter{}
	cfg := &config.Config{
		DataDir:  "/mem",
		RawQuery: map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}},
		RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
			"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
		},
	}
	if err := adapter.Validate(cfg); err == nil {
		t.Error("Validate 应报顶层诊断")
	}
	if defs, err := adapter.Definitions(cfg); err == nil || defs != nil {
		t.Error("Definitions 应拒绝并返回 nil 定义")
	}
	views, viewsErr := adapter.Views(cfg)
	if viewsErr == nil || views != nil {
		t.Fatalf("Views 顶层问题态应返回共同前置诊断且不返回定义,实际 views=%v err=%v", views, viewsErr)
	}
	layout, layoutErr := adapter.OutputLayout(cfg)
	if layoutErr == nil || layout != nil {
		t.Fatalf("OutputLayout 顶层问题态应返回共同前置诊断且不返回布局,实际 layout=%v err=%v", layout, layoutErr)
	}
	if viewsErr.Error() != layoutErr.Error() {
		t.Errorf("两个局部入口的顶层诊断应一致:\n%q\n%q", viewsErr.Error(), layoutErr.Error())
	}
	for _, want := range []string{`"Query"`, "name_conflict"} {
		if !strings.Contains(viewsErr.Error(), want) {
			t.Errorf("顶层诊断应含 %q: %q", want, viewsErr.Error())
		}
	}
}
