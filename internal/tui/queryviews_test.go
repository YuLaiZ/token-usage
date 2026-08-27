package tui

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/querydef"
)

// defsAdapter 测试用生产语义适配器:config raw → querydef(与 CLI 注入同一语义)。
type defsAdapter struct{}

func (defsAdapter) Definitions(cfg *config.Config) (*querydef.QueryDefinitions, error) {
	issues := make(map[string]querydef.TopLevelIssue, len(cfg.RawQueryTopLevelIssues))
	for name, issue := range cfg.RawQueryTopLevelIssues {
		issues[name] = querydef.TopLevelIssue{Name: issue.Name, Kind: string(issue.Kind)}
	}
	return querydef.Parse(querydef.Input{RawQuery: cfg.RawQuery, RawQueryTopLevelIssues: issues})
}

func (a defsAdapter) Validate(cfg *config.Config) error {
	_, err := a.Definitions(cfg)
	return err
}

func newQueryApp(t *testing.T, draft *config.Config) *App {
	t.Helper()
	return newAppForTest(draft, draft, nil, defsAdapter{})
}

func keyString(s string) string { return s }

// 候选集合严格受限:custom 只选四个 builtin;group 只选 builtin+已定义 custom;
// default 只选 builtin+custom+group。
func TestQueryViews_CandidateSetsRestricted(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}}
	a := newQueryApp(t, draft)

	sub := newSubqueryEditPage(a, "s2", nil)
	if len(sub.sel.candidates) != 4 {
		t.Errorf("custom 候选应恰为 4 个内置维度: %v", sub.sel.candidates)
	}
	for _, c := range sub.sel.candidates {
		if c != "client" && c != "model" && c != "provider" && c != "project" {
			t.Errorf("custom 候选含非法项 %q", c)
		}
	}

	grp := newGroupEditPage(a, "g2", nil)
	want := []string{"client", "model", "provider", "project", "mpc"}
	if strings.Join(grp.sel.candidates, ",") != strings.Join(want, ",") {
		t.Errorf("group 候选 = %v, want %v(group 不得含 group 自身)", grp.sel.candidates, want)
	}

	def := newDefaultSelectPage(a)
	if len(def.items) != 7 { // 4 builtin + mpc + g + 使用默认 client
		t.Errorf("default 候选数 = %d: %v", len(def.items), def.items)
	}
	found := map[string]bool{}
	for _, it := range def.items {
		found[it] = true
	}
	for _, want := range []string{"client", "mpc", "g", useDefaultClientSentinel} {
		if !found[want] {
			t.Errorf("default 候选缺 %q: %v", want, def.items)
		}
	}
}

// 常规编辑页数据源为注入适配器的 querydef 解析结果;磁盘已有定义被正确显示。
func TestQueryViews_ListsShowDiskDefinitions(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "g",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}}
	a := newQueryApp(t, draft)
	p := newSubqueryListPage(a)
	view := p.View()
	if !strings.Contains(view, "mpc") || !strings.Contains(view, "model,provider") {
		t.Errorf("子查询列表应显示磁盘定义:\n%s", view)
	}
	g := newGroupListPage(a)
	if !strings.Contains(g.View(), "g = client,mpc") {
		t.Errorf("组合列表应显示磁盘定义:\n%s", g.View())
	}
	def := newDefaultSelectPage(a)
	defView := def.View()
	if !strings.Contains(defView, "g") {
		t.Errorf("默认页应显示 group 候选:\n%s", defView)
	}
}

// 编辑 custom 提交后 CSV 声明顺序逐项保持,不被排序或规范化;写入唯一 raw 内存形态。
func TestQueryViews_SubqueryEditWritesOrderedCSV(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	a := newQueryApp(t, draft)
	p := newSubqueryEditPage(a, "mpc", nil)
	// 按声明顺序选中 model、provider、client(初始光标在 client);经页面 Update 驱动。
	for _, key := range []string{"down", " ", "down", " ", "up", "up", " ", "enter"} {
		p.Update(queryTestKeyMsg(key))
	}

	if got := draft.RawQuery["subqueries"].(map[string]any)["mpc"]; got != "model,provider,client" {
		t.Errorf("CSV 声明顺序失真: %v", got)
	}
	// 唯一内存形态:标量 string、子表 map[string]any。
	if _, ok := draft.RawQuery["subqueries"].(map[string]any); !ok {
		t.Errorf("subqueries 应为 map[string]any: %T", draft.RawQuery["subqueries"])
	}
	if _, ok := draft.RawQuery["subqueries"].(map[string]any)["mpc"].(string); !ok {
		t.Errorf("子查询值应为 string: %T", draft.RawQuery["subqueries"].(map[string]any)["mpc"])
	}
	// 少于两个维度提交被拒。
	p2 := newSubqueryEditPage(a, "solo", nil)
	p2.Update(queryTestKeyMsg(" "))
	p2.Update(queryTestKeyMsg("enter"))
	if p2.sel.Done() != selectPending || p2.errMsg == "" {
		t.Errorf("单维提交应被拒: done=%v err=%q", p2.sel.Done(), p2.errMsg)
	}
	if _, exists := queryRawTable(draft, "subqueries")["solo"]; exists {
		t.Error("被拒的提交不得写 draft")
	}
}

// group 编辑提交后声明顺序保持;default 单选页 Space/Enter/Esc 合同。
func TestQueryViews_GroupEditAndDefaultSelect(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,provider"},
	}}
	a := newQueryApp(t, draft)

	// group:选 client、mpc(候选序 client model provider project mpc)。
	g := newGroupEditPage(a, "g", nil)
	for _, key := range []string{" ", "down", "down", "down", "down", " ", "enter"} {
		g.Update(queryTestKeyMsg(key))
	}
	if got := draft.RawQuery["groups"].(map[string]any)["g"]; got != "client,mpc" {
		t.Errorf("group CSV = %v", got)
	}

	// default 单选:Space 设定唯一选择,Enter 提交。
	def := newDefaultSelectPage(a)
	// 光标移到 mpc(索引 4)。
	for i := 0; i < 4; i++ {
		def.Update(queryTestKeyMsg("down"))
	}
	def.Update(queryTestKeyMsg(" "))
	def.Update(queryTestKeyMsg("enter"))
	if got := draft.RawQuery["default"]; got != "mpc" {
		t.Errorf("default = %v, want mpc", got)
	}

	// Esc 放弃未提交选择:不改 draft。
	def2 := newDefaultSelectPage(a)
	draft.RawQuery["default"] = "mpc"
	for i := 0; i < 3; i++ {
		def2.Update(queryTestKeyMsg("down"))
	}
	def2.Update(queryTestKeyMsg(" "))
	def2.Update(queryTestKeyMsg("enter"))
	if got := draft.RawQuery["default"]; got != "project" {
		t.Errorf("Esc 前的提交已生效 default = %v", got)
	}
	def3 := newDefaultSelectPage(a)
	for i := 0; i < 2; i++ {
		def3.Update(queryTestKeyMsg("down"))
	}
	def3.Update(queryTestKeyMsg(" ")) // 设定 provider 待提交
	def3.Update(queryTestKeyMsg("esc"))
	if got := draft.RawQuery["default"]; got != "project" {
		t.Errorf("Esc 放弃后 default 应保持不变: %v", got)
	}

	// 「使用默认 client」= 删除显式 default。
	def4 := newDefaultSelectPage(a)
	for i := 0; i < len(def4.items)-1; i++ {
		def4.Update(queryTestKeyMsg("down"))
	}
	def4.Update(queryTestKeyMsg(" "))
	def4.Update(queryTestKeyMsg("enter"))
	if _, exists := draft.RawQuery["default"]; exists {
		t.Errorf("使用默认 client 应删除显式 default: %v", draft.RawQuery["default"])
	}
}

// 删除被引用对象在常规页被拒并列出引用方。
func TestQueryViews_DeleteReferencedRejected(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "g",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}}
	a := newQueryApp(t, draft)

	sub := newSubqueryListPage(a)
	// mpc 是唯一子查询,光标 0;删除应被拒(g 引用 + default 引用)。
	sub.Update(queryTestKeyMsg("d"))
	if !strings.Contains(sub.feedback, "g") {
		t.Errorf("删除被引用子查询应列出引用方: %q", sub.feedback)
	}
	if _, exists := queryRawTable(draft, "subqueries")["mpc"]; !exists {
		t.Error("被引用子查询不得被删除")
	}

	grp := newGroupListPage(a)
	// g 是唯一组合,被 default 引用。
	grp.Update(queryTestKeyMsg("d"))
	if !strings.Contains(grp.feedback, "query.default") {
		t.Errorf("删除被引用组合应指出 default 引用: %q", grp.feedback)
	}
	if _, exists := queryRawTable(draft, "groups")["g"]; !exists {
		t.Error("被 default 引用的组合不得被删除")
	}
}

// 坏 raw query(内部数组/嵌套表/未知键/CSV 错/断开引用/顶层 issues)进入恢复态;
// 恢复态不阻止其他 TUI 页面浏览。
func TestQueryViews_RecoveryModeEntry(t *testing.T) {
	cases := []struct {
		name  string
		draft *config.Config
	}{
		{
			name: "top-level issues",
			draft: &config.Config{DataDir: "/x", RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
				"Query": {Name: "Query", Value: map[string]any{}, Kind: config.RawQueryIssueNameConflict},
			}},
		},
		{
			name:  "array value",
			draft: &config.Config{DataDir: "/x", RawQuery: map[string]any{"subqueries": map[string]any{"mpc": []any{"model"}}}},
		},
		{
			name:  "nested table",
			draft: &config.Config{DataDir: "/x", RawQuery: map[string]any{"subqueries": map[string]any{"mpc": map[string]any{"a": "b"}}}},
		},
		{
			name:  "unknown key",
			draft: &config.Config{DataDir: "/x", RawQuery: map[string]any{"foo": "bar"}},
		},
		{
			name:  "csv error",
			draft: &config.Config{DataDir: "/x", RawQuery: map[string]any{"subqueries": map[string]any{"mpc": "model,"}}},
		},
		{
			name:  "broken reference",
			draft: &config.Config{DataDir: "/x", RawQuery: map[string]any{"groups": map[string]any{"g": "client,nope"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newQueryApp(t, tc.draft)
			p := newQueryViewsPage(a)
			if p.recovery == nil {
				t.Fatalf("%s 应进入恢复态:\n%s", tc.name, p.View())
			}
			if !strings.Contains(p.View(), "enter") {
				t.Errorf("恢复态应显示修复操作:\n%s", p.View())
			}
			// 恢复态不阻止其他页面:主菜单仍可进 clients(结构上恢复态只在 queryviews 页内)。
			m := a.stack[0].(*mainMenu)
			m.Update(queryTestKeyMsg("enter"))
			if _, ok := a.stack[len(a.stack)-1].(*clientsPage); !ok {
				t.Errorf("恢复态不应阻止其他页面浏览: %T", a.stack[len(a.stack)-1])
			}
		})
	}
}

// 恢复态:逐项 Enter 修复;每次修复后重新评估;错误 default 可恢复 client;
// a/d 按键不响应(禁用);Esc 返回主菜单保留 draft。
func TestQueryViews_RecoveryFixFlow(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "ghost",
		"subqueries": map[string]any{"mpc": "model,"},
		"foo":        "bar",
	}}
	a := newQueryApp(t, draft)
	p := newQueryViewsPage(a)
	if p.recovery == nil {
		t.Fatal("应进入恢复态")
	}
	// a/d 禁用:按键后错误项不变、不产生意外修改。
	before := len(p.recovery.items)
	p.Update(queryTestKeyMsg("a"))
	p.Update(queryTestKeyMsg("d"))
	if len(p.recovery.items) != before || draft.RawQuery["foo"] != "bar" {
		t.Fatalf("恢复态 a/d 应禁用: items=%d foo=%v", len(p.recovery.items), draft.RawQuery["foo"])
	}

	// 逐项 Enter 修复直到清空。findItem 定位包含关键字的恢复项索引。
	findItem := func(substr string) int {
		for i, item := range p.recovery.items {
			if strings.Contains(item.desc, substr) {
				return i
			}
		}
		return -1
	}
	fix := func(substr string) {
		t.Helper()
		idx := findItem(substr)
		if idx < 0 {
			t.Fatalf("缺少恢复项 %q: %v", substr, p.recovery.items)
		}
		p.recovery.cursor = idx
		p.Update(queryTestKeyMsg("enter"))
	}

	fix("foo")           // 删除未知键
	fix("query.default") // 恢复 default 为 client(ghost 未知)
	fix("mpc")           // 删除错误子查询
	if p.recovery != nil {
		t.Fatalf("全部修复后应回到常规态,剩余: %v", p.recovery.items)
	}
	if len(draft.RawQuery) != 0 {
		t.Errorf("修复后 query 段应等价未配置: %#v", draft.RawQuery)
	}

	// Esc 返回主菜单保留 draft(半修复仍 dirty 可继续)。
	draft2 := &config.Config{DataDir: "/x", RawQuery: map[string]any{"foo": "bar"}}
	a2 := newQueryApp(t, draft2)
	p2 := newQueryViewsPage(a2)
	p2.Update(queryTestKeyMsg("esc"))
	if len(a2.stack) != 1 {
		t.Errorf("Esc 应返回主菜单,栈深 %d", len(a2.stack))
	}
	if draft2.RawQuery["foo"] != "bar" {
		t.Error("Esc 保留 draft 未修复项")
	}
}

// 删除大小写变体后仅剩唯一精确小写表 → 自动转正 RawQuery;
// 转正后内部仍有错时继续显示内部错误并可修复。
func TestQueryViews_RecoveryPromotesSurvivingLowercase(t *testing.T) {
	// 顶层冲突:[query](内部坏)+ [Query]。
	draft := &config.Config{DataDir: "/x", RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
		"query": {Name: "query", Value: map[string]any{"subqueries": map[string]any{"mpc": "model,"}}, Kind: config.RawQueryIssueNameConflict},
		"Query": {Name: "Query", Value: map[string]any{"default": "b"}, Kind: config.RawQueryIssueNameConflict},
	}}
	a := newQueryApp(t, draft)
	p := newQueryViewsPage(a)
	if p.recovery == nil {
		t.Fatal("冲突应进入恢复态")
	}
	// 删除 "Query" 变体:剩余唯一精确小写表转正,内部 mpc 错误继续显示。
	for i, item := range p.recovery.items {
		if strings.Contains(item.desc, `"Query"`) {
			p.recovery.cursor = i
			p.Update(queryTestKeyMsg("enter"))
			break
		}
	}
	if draft.RawQuery == nil {
		t.Fatal("删除变体后唯一小写表应转正为 RawQuery")
	}
	if draft.RawQueryTopLevelIssues != nil {
		t.Fatalf("转正后 issues 应清空: %#v", draft.RawQueryTopLevelIssues)
	}
	if p.recovery == nil {
		t.Fatal("转正后内部错误应继续显示在恢复态")
	}
	if !strings.Contains(p.View(), "mpc") {
		t.Errorf("内部错误应可定位:\n%s", p.View())
	}
	// 继续修复内部错误后回到常规态。
	for i, item := range p.recovery.items {
		if strings.Contains(item.desc, "mpc") {
			p.recovery.cursor = i
			p.Update(queryTestKeyMsg("enter"))
			break
		}
	}
	if p.recovery != nil {
		t.Fatalf("内部错误修复后应回常规态: %v", p.recovery.items)
	}
}

// 恢复态删除被引用的错误 custom 不被引用阻止;引用方成为断开引用错误,可逐项删除收敛。
func TestQueryViews_RecoveryDeleteBreaksReferencesProgressively(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"subqueries": map[string]any{"bad": "model,"},
		"groups":     map[string]any{"g": "client,bad"},
	}}
	a := newQueryApp(t, draft)
	p := newQueryViewsPage(a)
	if p.recovery == nil {
		t.Fatal("应进入恢复态")
	}
	// 删除错误子查询 bad:不受引用阻止(恢复态豁免)。
	for i, item := range p.recovery.items {
		if strings.Contains(item.desc, "bad") && strings.Contains(item.action, "删除") {
			p.recovery.cursor = i
			p.Update(queryTestKeyMsg("enter"))
			break
		}
	}
	if _, exists := queryRawTable(draft, "subqueries")["bad"]; exists {
		t.Fatal("恢复态删除错误条目不受引用阻止")
	}
	// 引用方 g 成为断开引用错误,继续可删,直至整体可解析。
	if p.recovery == nil {
		t.Fatal("断开引用的 g 应进入错误列表")
	}
	for i, item := range p.recovery.items {
		if strings.Contains(item.desc, "g") && strings.Contains(item.desc, "group") {
			p.recovery.cursor = i
			p.Update(queryTestKeyMsg("enter"))
			break
		}
	}
	if p.recovery != nil {
		t.Fatalf("逐项删除应收敛到可解析: %v", p.recovery.items)
	}
}

// page-local Esc 取消未提交输入(名称输入);完成操作后 Esc 返回保留 draft;
// 半修复草稿 dirty、可浏览,主保存被拒。
func TestQueryViews_PageLocalEscAndDirtySaveRejected(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	a := newQueryApp(t, draft)
	draft.RawQuery = map[string]any{"foo": "bar"} // 构造基线后制造 dirty

	// 名称输入态 Esc:取消未提交输入,不新增条目。
	sub := newSubqueryListPage(a)
	sub.Update(queryTestKeyMsg("a"))
	if !sub.adding {
		t.Fatal("a 应进入新增名称输入")
	}
	sub.nameIn.input.SetValue("newsub")
	sub.Update(queryTestKeyMsg("esc"))
	if sub.adding {
		t.Error("Esc 应取消名称输入")
	}
	if _, exists := queryRawTable(draft, "subqueries")["newsub"]; exists {
		t.Error("取消的输入不得写 draft")
	}

	// 半修复(仍含 foo 未知键)dirty 为真;s 保存被拒且不调 ApplyConfig。
	if !a.dirty() {
		t.Error("半修复草稿应 dirty")
	}
	a.apply = func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		t.Fatal("半修复草稿保存必须被 query 校验拒绝")
		return configapp.ApplyConfigResult{}, nil
	}
	cmd := a.save()
	if cmd != nil {
		t.Error("保存应被拒绝(nil cmd)")
	}
	if !strings.Contains(a.statusMsg, "Query views") {
		t.Errorf("拒绝提示应指引进 Query views: %q", a.statusMsg)
	}

	// 修复后保存可通过。
	delete(draft.RawQuery, "foo")
	a.apply = func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		return configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("r2")}, nil
	}
	if cmd := a.save(); cmd == nil {
		t.Error("修复后保存应正常启动")
	}
}

// 恢复态摘要可定位:顶层项含原始名称与类别;条目含名称;未知键含键名;default 含键名。
func TestQueryViews_RecoverySummariesActionable(t *testing.T) {
	// 场景一:顶层 issues 存在时,querydef 先按顶层问题拒绝(原始名称与类别可定位)。
	issueDraft := &config.Config{DataDir: "/x",
		RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
			"QUERY": {Name: "QUERY", Value: "x", Kind: config.RawQueryIssueNameConflict},
		},
	}
	p := newQueryViewsPage(newQueryApp(t, issueDraft))
	view := p.View()
	for _, want := range []string{"QUERY", "name_conflict"} {
		if !strings.Contains(view, want) {
			t.Errorf("恢复态摘要缺 %q:\n%s", want, view)
		}
	}

	// 场景二:无顶层问题时,内部错误(default/未知键/条目)逐项可定位。
	innerDraft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "ghost",
		"foo":        "bar",
		"subqueries": map[string]any{"mpc": "model,"},
	}}
	p2 := newQueryViewsPage(newQueryApp(t, innerDraft))
	view2 := p2.View()
	for _, want := range []string{"foo", "mpc", "query.default"} {
		if !strings.Contains(view2, want) {
			t.Errorf("恢复态摘要缺 %q:\n%s", want, view2)
		}
	}
}

// 恢复态只列真正的错误条目:aaa_bad 报错时,合法的 mpc/group_q/default
// 不得因错误扩散被列为可删除项(误删风险回归)。
func TestQueryViews_RecoveryDoesNotListValidEntriesBesideErrors(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"aaa_bad": "model,", "mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,mpc"},
	}}
	p := newQueryViewsPage(newQueryApp(t, draft))
	if p.recovery == nil {
		t.Fatal("应进入恢复态(aaa_bad 有错)")
	}
	view := p.View()
	if !strings.Contains(view, "aaa_bad") {
		t.Errorf("恢复态应列出 aaa_bad:\n%s", view)
	}
	for _, valid := range []string{"mpc", "group_q", "query.default"} {
		if strings.Contains(view, valid) {
			t.Errorf("合法条目 %q 不应出现在恢复列表(误删风险):\n%s", valid, view)
		}
	}
	// 只修复 aaa_bad 后整体应回到常规态,合法定义保留。
	for i, item := range p.recovery.items {
		if strings.Contains(item.desc, "aaa_bad") {
			p.recovery.cursor = i
			p.Update(queryTestKeyMsg("enter"))
			break
		}
	}
	if p.recovery != nil {
		t.Fatalf("仅剩的错误修复后应回常规态: %v", p.recovery.items)
	}
	if got := queryRawTable(draft, "subqueries")["mpc"]; got != "model,provider" {
		t.Errorf("合法子查询 mpc 应保留: %v", got)
	}
	if got := queryRawTable(draft, "groups")["group_q"]; got != "client,mpc" {
		t.Errorf("合法组合 group_q 应保留: %v", got)
	}
	if got := draft.RawQuery["default"]; got != "group_q" {
		t.Errorf("default 应保留: %v", got)
	}
}

// validateQueryName 即时校验合同:保留名(含新增 list)在新增子查询与组合查询的
// 输入现场即被拒并提示保留名不可用,不拖到保存前整体校验;普通合法名称放行。
func TestValidateQueryName_RejectsReservedImmediately(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	for _, table := range []string{"subqueries", "groups"} {
		got := validateQueryName(draft, "list", table)
		if got == "" || (!strings.Contains(got, "reserved name") && !strings.Contains(got, "保留名不可用")) {
			t.Errorf("在 %s 新增 %q 应当场报保留名,得到: %q", table, "list", got)
		}
	}
	// 既有保留名回归:同一条件链不得因收敛到单一来源而漏判。
	for _, name := range []string{"client", "session", "custom"} {
		if got := validateQueryName(draft, name, "subqueries"); !strings.Contains(got, "reserved name") && !strings.Contains(got, "保留名不可用") {
			t.Errorf("既有保留名 %q 应被即时拒绝,得到: %q", name, got)
		}
	}
	if got := validateQueryName(draft, "mpc", "subqueries"); got != "" {
		t.Errorf("普通合法名称应通过即时校验: %q", got)
	}
}
