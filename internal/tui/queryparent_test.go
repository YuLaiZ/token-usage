package tui

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/querydef"
)

// 主菜单以 Query 替代 Query views 与 Provider aliases 两个平级项;
// v 与 Enter 均进入 Query 父页而非 Views。
func TestMainMenu_QueryParentReplacesFlatEntries(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	a := newQueryApp(t, draft)
	m := a.stack[0].(*mainMenu)

	view := m.View()
	if !strings.Contains(view, "Query / 查询") {
		t.Errorf("主菜单应含 Query 项:\n%s", view)
	}
	// 菜单项区(带 menuColWidth 对齐的行)不再有平级 Provider aliases 与 Query views。
	for _, ln := range strings.Split(view, "\n") {
		if len(ln) > 0 && ln[0] != ' ' && !strings.HasPrefix(ln, "====") {
			// 菜单项行以光标(▸/空格)开头,此处取所有非缩进行检查。
			if strings.Contains(ln, "Provider aliases") || strings.Contains(ln, "供应商别名") {
				t.Errorf("主菜单项不得再有平级 Provider aliases:\n%s", view)
			}
			if strings.Contains(ln, "Query views") || strings.Contains(ln, "查询视图") {
				t.Errorf("主菜单项不得再有 Query views:\n%s", view)
			}
		}
	}

	// v 进入 Query 父页。
	m.Update(queryTestKeyMsg("v"))
	if len(a.stack) != 2 {
		t.Fatalf("v 应进入 Query 父页,栈深 %d", len(a.stack))
	}
	if _, ok := a.stack[1].(*queryParentPage); !ok {
		t.Fatalf("栈顶应为 queryParentPage,实际 %T", a.stack[1])
	}

	// Enter 光标选中 Query 项(索引 4)同样进入父页。
	a2 := newQueryApp(t, draft)
	m2 := a2.stack[0].(*mainMenu)
	for i := 0; i < 4; i++ {
		m2.Update(queryTestKeyMsg("down"))
	}
	m2.Update(queryTestKeyMsg("enter"))
	if _, ok := a2.stack[len(a2.stack)-1].(*queryParentPage); !ok {
		t.Fatalf("enter 应进入 Query 父页,实际 %T", a2.stack[len(a2.stack)-1])
	}
}

// 主菜单 Query 摘要 = default target + alias 数。
func TestMainMenu_QuerySummaryShowsDefaultAndAliases(t *testing.T) {
	draft := &config.Config{DataDir: "/x", ProviderAliases: map[string]string{"a": "A", "b": "B"}}
	a := newQueryApp(t, draft)
	view := a.stack[0].(*mainMenu).View()
	if !strings.Contains(view, "client") {
		t.Errorf("摘要应含默认 target client:\n%s", view)
	}
	if !strings.Contains(view, "2") || !strings.Contains(view, "mappings") {
		t.Errorf("摘要应含 alias 数 2:\n%s", view)
	}

	draft2 := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,mpc"},
	}}
	a2 := newQueryApp(t, draft2)
	view2 := a2.stack[0].(*mainMenu).View()
	if !strings.Contains(view2, "group_q") {
		t.Errorf("配置后摘要应显示 group_q:\n%s", view2)
	}
}

// Query 父页三个平级子项与各自摘要;进入三个子页。
func TestQueryParentPage_SubItemsAndSummaries(t *testing.T) {
	draft := &config.Config{DataDir: "/x", ProviderAliases: map[string]string{"a": "A"}}
	a := newQueryApp(t, draft)
	p := newQueryParentPage(a)

	view := p.View()
	for _, want := range []string{"Views / 查询视图", "Output columns / 输出列", "Provider aliases / 供应商别名"} {
		if !strings.Contains(view, want) {
			t.Errorf("父页应含子项 %q:\n%s", want, view)
		}
	}
	// Views 摘要 = default target;Output columns 摘要 = 列数(默认布局标识);
	// aliases 摘要 = 映射数。
	if !strings.Contains(view, "client") {
		t.Errorf("Views 子项摘要应含 client:\n%s", view)
	}
	if !strings.Contains(view, "7") || !strings.Contains(view, "default") && !strings.Contains(view, "默认") {
		t.Errorf("Output columns 摘要应显示 7 列与默认标记:\n%s", view)
	}
	if !strings.Contains(view, "1") {
		t.Errorf("aliases 子项摘要应显示 1 条映射:\n%s", view)
	}

	// enter 分别进入三个子页。
	p.Update(queryTestKeyMsg("enter"))
	if _, ok := a.stack[len(a.stack)-1].(*queryViewsPage); !ok {
		t.Fatalf("enter 应进入 Views,实际 %T", a.stack[len(a.stack)-1])
	}
	a.pop()
	p2 := newQueryParentPage(a)
	p2.cursor = 1
	p2.Update(queryTestKeyMsg("enter"))
	if _, ok := a.stack[len(a.stack)-1].(*outputColumnsPage); !ok {
		t.Fatalf("enter 应进入 Output columns,实际 %T", a.stack[len(a.stack)-1])
	}
	a.pop()
	p3 := newQueryParentPage(a)
	p3.cursor = 2
	p3.Update(queryTestKeyMsg("enter"))
	if _, ok := a.stack[len(a.stack)-1].(*aliasesPage); !ok {
		t.Fatalf("enter 应进入 Provider aliases,实际 %T", a.stack[len(a.stack)-1])
	}
	// esc 返回上层。
	a.pop()
	p4 := newQueryParentPage(a)
	p4.Update(queryTestKeyMsg("esc"))
	if len(a.stack) != 1 {
		t.Errorf("esc 应返回主菜单,栈深 %d", len(a.stack))
	}
}

// Output columns 摘要:自定义布局显示列数且无默认标记;坏布局/顶层问题态
// 显示 recovery 标记而不显示列数。
func TestQueryParentPage_OutputColumnsSummaryStates(t *testing.T) {
	custom := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total", "requests"}},
	}}
	view := newQueryParentPage(newQueryApp(t, custom)).View()
	if !strings.Contains(view, "2") {
		t.Errorf("自定义布局摘要应显示 2 列:\n%s", view)
	}
	// 只检查 Output columns 子项行:自定义布局不得显示默认标记
	// (Views 子项行的 "(default)" 与本断言无关)。
	for _, ln := range strings.Split(view, "\n") {
		if strings.Contains(ln, "Output columns") && strings.Contains(ln, "输出列") {
			if strings.Contains(ln, "default") || strings.Contains(ln, "默认") {
				t.Errorf("自定义布局行不得显示默认标记: %q", ln)
			}
		}
	}

	bad := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"foo": 1},
	}}
	badView := newQueryParentPage(newQueryApp(t, bad)).View()
	if !strings.Contains(badView, "recovery") && !strings.Contains(badView, "恢复") {
		t.Errorf("坏布局应显示 recovery 标记:\n%s", badView)
	}

	topLevel := &config.Config{DataDir: "/x", RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}}
	topView := newQueryParentPage(newQueryApp(t, topLevel)).View()
	if !strings.Contains(topView, "recovery") && !strings.Contains(topView, "恢复") {
		t.Errorf("顶层问题态应显示 recovery 标记:\n%s", topView)
	}
}

// Views 子页保留原有三项与键盘位置;Output columns 以平级入口提供。
func TestQueryParentPage_ViewsKeepsThreeItems(t *testing.T) {
	a := newQueryApp(t, &config.Config{DataDir: "/x"})
	p := newQueryViewsPage(a)
	view := p.View()
	for i, want := range []string{"Custom subqueries", "Groups", "Default view"} {
		if !strings.Contains(view, want) {
			t.Errorf("Views 子页应保留第 %d 项 %q:\n%s", i, want, view)
		}
	}
	if p.recovery != nil {
		t.Errorf("干净配置 Views 不应进恢复态")
	}
}

// 坏 subqueries 不阻断 Output columns;坏 output 不阻断 Views;
// 修复一侧后另一侧保留自己的恢复状态。
func TestQueryPages_BadViewsAndBadOutputDoNotLockEachOther(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,"},
		"output":     map[string]any{"foo": 1},
	}}
	a := newQueryApp(t, draft)

	viewsPage := newQueryViewsPage(a)
	if viewsPage.recovery == nil {
		t.Fatal("坏 subqueries 应使 Views 进恢复态")
	}
	if !strings.Contains(viewsPage.View(), "mpc") {
		t.Errorf("Views 恢复态应定位 mpc:\n%s", viewsPage.View())
	}
	if strings.Contains(viewsPage.View(), "output") {
		t.Errorf("Views 恢复态不得包含 output 错误(隔离):\n%s", viewsPage.View())
	}

	outputPage := newOutputColumnsPage(a)
	if outputPage.recovery == nil {
		t.Fatal("坏 output 应使 Output columns 进恢复态")
	}
	if !strings.Contains(outputPage.View(), "query.output") && !strings.Contains(outputPage.View(), "output") {
		t.Errorf("Output columns 恢复态应定位 output:\n%s", outputPage.View())
	}
	if strings.Contains(outputPage.View(), "mpc") {
		t.Errorf("Output columns 恢复态不得包含视图错误(隔离):\n%s", outputPage.View())
	}

	// 修复 output 一侧(删除整表)后:Output columns 回编辑态,Views 保留恢复态。
	outputPage.recovery.cursor = 0
	outputPage.Update(queryTestKeyMsg("enter"))
	if _, exists := draft.RawQuery["output"]; exists {
		t.Fatal("恢复动作应删除整张 query.output 表")
	}
	if outputPage.recovery != nil {
		t.Fatalf("删除 output 后应回到编辑态: %v", outputPage.recovery.items)
	}
	viewsStill := newQueryViewsPage(a)
	if viewsStill.recovery == nil {
		t.Fatal("修复 output 后 Views 的坏 subqueries 恢复态应保留")
	}
}

// 顶层 raw 问题使 Views 与 Output columns 都先显示共同恢复项;
// 转正前两页均不写 RawQuery;两个 raw 载体互斥(mutation)。
func TestQueryPages_TopLevelIssuesAreSharedPrecondition(t *testing.T) {
	// [query](值携带合法视图定义)与 [Query] 变体并存:删除变体后唯一
	// 精确小写表转正为 RawQuery。
	draft := &config.Config{DataDir: "/x", RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
		"query": {Name: "query", Value: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
		}, Kind: config.RawQueryIssueNameConflict},
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}}
	a := newQueryApp(t, draft)

	viewsPage := newQueryViewsPage(a)
	if viewsPage.recovery == nil {
		t.Fatal("顶层问题态 Views 应进恢复态")
	}
	if !strings.Contains(viewsPage.View(), "Query") {
		t.Errorf("Views 应显示顶层恢复项:\n%s", viewsPage.View())
	}

	outputPage := newOutputColumnsPage(a)
	if outputPage.recovery == nil {
		t.Fatal("顶层问题态 Output columns 应进恢复态")
	}
	if !strings.Contains(outputPage.View(), "Query") {
		t.Errorf("Output columns 应显示同一组顶层恢复项:\n%s", outputPage.View())
	}

	// 在 Output columns 页删除 "Query" 变体:唯一精确小写表转正,
	// 两载体保持互斥且 Views 重算回常规态。
	for i, item := range outputPage.recovery.items {
		if strings.Contains(item.desc, `"Query"`) {
			outputPage.recovery.cursor = i
			break
		}
	}
	outputPage.Update(queryTestKeyMsg("enter"))
	if draft.RawQuery != nil && draft.RawQueryTopLevelIssues != nil {
		t.Fatal("两个 raw 载体不得同时非空")
	}
	viewsAfter := newQueryViewsPage(a)
	if viewsAfter.recovery != nil {
		t.Fatalf("顶层修复后 Views 应回常规态: %v", viewsAfter.recovery.items)
	}
}

// 保存失败提示精确指向 Query 的 Views/Output columns 与 config set 替代路径。
func TestSave_FailureGuidanceMentionsQueryChildren(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	a := newQueryApp(t, draft)
	draft.RawQuery = map[string]any{"foo": "bar"} // 构造基线后制造 dirty
	a.apply = func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		t.Fatal("坏 query 草稿保存必须被拒绝")
		return configapp.ApplyConfigResult{}, nil
	}
	cmd := a.save()
	if cmd != nil {
		t.Fatal("保存应被拒绝")
	}
	for _, want := range []string{"Views", "Output columns", "config set", "草稿已保留"} {
		if !strings.Contains(a.statusMsg, want) {
			t.Errorf("保存失败提示应含 %q: %q", want, a.statusMsg)
		}
	}
}

// Views 恢复态的返回文案改为 Back to Query(新增层级)。
func TestQueryViews_RecoveryEscSaysBackToQuery(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{"foo": "bar"}}
	p := newQueryViewsPage(newQueryApp(t, draft))
	view := p.View()
	if !strings.Contains(view, "Back to Query") {
		t.Errorf("恢复态 esc 文案应为 Back to Query:\n%s", view)
	}
	if strings.Contains(view, "Back to main menu") {
		t.Errorf("恢复态不得再写 Back to main menu:\n%s", view)
	}
}

// 恢复项仅由 Diagnostic Path/Kind 构建:跨表重名删 group 保留同名子查询;
// 定义值类型只删该条目;定义项目不误删整表。
func TestQueryViews_RecoveryUsesDiagnosticsNotErrorText(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"dup": "model,provider", "mpc": "model,provider"},
		"groups":     map[string]any{"dup": "client,model"},
	}}
	a := newQueryApp(t, draft)
	p := newQueryViewsPage(a)
	if p.recovery == nil {
		t.Fatal("跨表重名应进恢复态")
	}
	// 恢复动作删除 group dup,保留子查询 dup。
	found := false
	for _, item := range p.recovery.items {
		if item.desc == "" || !strings.Contains(item.desc, "dup") {
			continue
		}
		found = true
		p.app = a
		item.apply(a)
		break
	}
	if !found {
		t.Fatalf("应含跨表重名恢复项: %v", p.recovery.items)
	}
	if _, exists := queryRawTable(draft, "groups")["dup"]; exists {
		t.Fatal("跨表重名恢复应删除 group dup")
	}
	if _, exists := queryRawTable(draft, "subqueries")["dup"]; !exists {
		t.Fatal("跨表重名恢复不得删除同名子查询")
	}

	// 定义值类型错误只删该条目而非整表。
	draft2 := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"bad": []any{"model"}, "good": "model,provider"},
	}}
	a2 := newQueryApp(t, draft2)
	p2 := newQueryViewsPage(a2)
	if p2.recovery == nil {
		t.Fatal("值类型错误应进恢复态")
	}
	for _, item := range p2.recovery.items {
		item.apply(a2)
		break
	}
	if _, exists := queryRawTable(draft2, "subqueries")["good"]; !exists {
		t.Fatal("值类型恢复不得误删整表/其他条目")
	}
	if _, exists := queryRawTable(draft2, "subqueries")["bad"]; exists {
		t.Fatal("值类型恢复应删除出错条目")
	}
}

// 删除 errorMentions 后同一组错误仍产生预期恢复动作
// (证明恢复项不再依赖错误文本子串)。
func TestQueryViews_NoErrorMentionsHelperRemains(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "ghost",
		"foo":        "bar",
		"subqueries": map[string]any{"mpc": "model,"},
	}}
	p := newQueryViewsPage(newQueryApp(t, draft))
	if p.recovery == nil {
		t.Fatal("多错误应进恢复态")
	}
	view := p.View()
	for _, want := range []string{"foo", "mpc", "query.default"} {
		if !strings.Contains(view, want) {
			t.Errorf("恢复态应定位 %q:\n%s", want, view)
		}
	}
}

// defsAdapter 实现扩展后的 QueryAdapter(Views/OutputLayout)。
var _ QueryAdapter = defsAdapter{}

func (defsAdapter) Views(cfg *config.Config) (*querydef.ViewDefinitions, error) {
	return querydef.ParseViews(querydefTestInput(cfg))
}

func (defsAdapter) OutputLayout(cfg *config.Config) ([]string, error) {
	return querydef.ParseOutputLayout(querydefTestInput(cfg))
}

// defsAdapter 的局部入口共享顶层共同前置(与生产 tuiQueryAdapter 同合同):
// 顶层问题态下全部方法报顶层诊断,Views/OutputLayout 不返回可写结果。
func TestDefsAdapter_TopLevelIssuesSharedPrecondition(t *testing.T) {
	cfg := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
	}, RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
		"Query": {Name: "Query", Value: "x", Kind: config.RawQueryIssueNameConflict},
	}}
	a := defsAdapter{}
	if defs, err := a.Definitions(cfg); err == nil || defs != nil {
		t.Error("Definitions 应拒绝并返回 nil 定义")
	}
	views, viewsErr := a.Views(cfg)
	if viewsErr == nil || views != nil {
		t.Fatalf("Views 顶层问题态应返回共同前置诊断且不返回定义,实际 views=%v err=%v", views, viewsErr)
	}
	layout, layoutErr := a.OutputLayout(cfg)
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
