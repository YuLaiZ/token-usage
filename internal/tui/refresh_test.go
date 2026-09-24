package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// ---- 查询刷新页 ----

// newRefreshAppForTest 构造带用户层草稿与有效层显示配置的测试 App。
func newRefreshAppForTest(draft, display *config.Config) *App {
	return newAppForTest(draft, display, nil)
}

// key sends a real KeyMsg to the page (the actual keyboard path users take).
func key(m *refreshPage, k string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
}

func keyName(m *refreshPage, name string) {
	var t tea.KeyType
	switch name {
	case "esc":
		t = tea.KeyEsc
	case "up":
		t = tea.KeyUp
	case "down":
		t = tea.KeyDown
	case "enter":
		t = tea.KeyEnter
	case " ":
		t = tea.KeySpace
	default:
		panic("unsupported key " + name)
	}
	m.Update(tea.KeyMsg{Type: t})
}

// TestRefreshPage_MenuEntryOpensPage 主菜单第 6 项(查询刷新)enter 进入 refreshPage。
func TestRefreshPage_MenuEntryOpensPage(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{})
	m := newMainMenu(a)
	m.cursor = 5 // 查询刷新
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.stack) != 2 {
		t.Fatalf("enter 查询刷新应 push 页面, 栈长=%d", len(a.stack))
	}
	if _, ok := a.stack[1].(*refreshPage); !ok {
		t.Fatalf("栈顶应为 *refreshPage, got %T", a.stack[1])
	}
}

// TestRefreshPage_RawValuesShown 未配置(0)落在「30 (默认)」预设;显式配置落位;
// 非预设值落「其他…」并回填输入框。
func TestRefreshPage_RawValuesShown(t *testing.T) {
	a := newRefreshAppForTest(
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 90}},
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 90}},
	)
	p := newRefreshPage(a)
	if p.dashSel != 1 || selLabel(p.dashSel) != "20" {
		t.Errorf("20 应落在 20 预设,实际 sel=%d label=%q", p.dashSel, selLabel(p.dashSel))
	}
	if p.watchSel != refreshSelCount-1 || p.watchCustom.Value() != "90" {
		t.Errorf("90 应落「其他…」并回填 90,实际 sel=%d custom=%q", p.watchSel, p.watchCustom.Value())
	}
	// 未配置:0 → 有效值 30 → 「30 (默认)」预设,不触碰。
	a2 := newRefreshAppForTest(&config.Config{}, &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}})
	p2 := newRefreshPage(a2)
	if p2.dashSel != 2 || !strings.Contains(selLabel(p2.dashSel), "30") || !strings.Contains(selLabel(p2.dashSel), "默认") {
		t.Errorf("0 应落「30 (默认)」预设,实际 sel=%d label=%q", p2.dashSel, selLabel(p2.dashSel))
	}
	if !strings.Contains(p.dashCustom.Placeholder, "3600") {
		t.Errorf("自定义输入占位应提示 1~3600, got %q", p.dashCustom.Placeholder)
	}
}

// TestRefreshPage_KeyboardCycleCommit 真实按键路径:空格循环选择 → esc 提交草稿。
func TestRefreshPage_KeyboardCycleCommit(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}})
	p := newRefreshPage(a)
	// 仪表盘项从「30 (默认)」出发:空格 → 60 → 空格 → 其他… → 空格 → 10。
	keyName(p, " ")
	if p.dashSel != 3 {
		t.Fatalf("30 后一个空格应到 60,实际 sel=%d label=%q", p.dashSel, selLabel(p.dashSel))
	}
	keyName(p, " ")
	if p.dashSel != refreshSelCount-1 {
		t.Fatalf("60 后一个空格应到「其他…」,实际 sel=%d", p.dashSel)
	}
	if p.dashCustom.Value() != "" {
		t.Errorf("原值 0(默认)切到「其他…」应留空由占位提示,实际 %q", p.dashCustom.Value())
	}
	keyName(p, " ")
	if p.dashSel != 0 || selLabel(p.dashSel) != "10" {
		t.Fatalf("「其他…」后一个空格应回到 10,实际 sel=%d label=%q", p.dashSel, selLabel(p.dashSel))
	}
	// watch 项:down 移动后空格一次 → 60。
	keyName(p, "down")
	keyName(p, " ")
	if p.watchSel != 3 {
		t.Fatalf("watch 空格一次应为 60,实际 sel=%d", p.watchSel)
	}
	// esc 提交:显式选择的项写显式值,未触碰项写原值(0 保持 0)。
	keyName(p, "esc")
	if len(a.stack) != 1 {
		t.Fatalf("esc 应 pop, 栈长=%d", len(a.stack))
	}
	if a.draft.Refresh.DashboardInterval != 10 {
		t.Errorf("循环到 10 后 esc 应写显式 10,实际 %d", a.draft.Refresh.DashboardInterval)
	}
	if a.draft.Refresh.WatchInterval != 60 {
		t.Errorf("循环到 60 后 esc 应写显式 60,实际 %d", a.draft.Refresh.WatchInterval)
	}
}

// TestRefreshPage_KeyboardCustomInput 「其他…」态的真实数字输入路径。
func TestRefreshPage_KeyboardCustomInput(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}})
	p := newRefreshPage(a)
	// 循环到「其他…」(30 → 60 → 其他),直接键入数字(textinput 聚焦)。
	keyName(p, " ")
	keyName(p, " ")
	if p.dashSel != refreshSelCount-1 || !p.customFocused() {
		t.Fatalf("应处于「其他…」态且输入框聚焦,sel=%d", p.dashSel)
	}
	key(p, "4")
	key(p, "5")
	if p.dashCustom.Value() != "45" {
		t.Fatalf("键入 4/5 后输入框应 45,实际 %q", p.dashCustom.Value())
	}
	keyName(p, "esc")
	if a.draft.Refresh.DashboardInterval != 45 {
		t.Errorf("「其他…」输入 45 后 esc 应写 45,实际 %d", a.draft.Refresh.DashboardInterval)
	}
}

// TestRefreshPage_KeyboardCustomValidation 自定义输入非法时 esc 阻止返回并整体回滚。
func TestRefreshPage_KeyboardCustomValidation(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "99999"} {
		a := newRefreshAppForTest(
			&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}},
			&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}},
		)
		a.stack = append(a.stack, newRefreshPage(a))
		p := a.stack[1].(*refreshPage)
		keyName(p, " ") // 30 → 60
		keyName(p, " ") // 60 → 其他…
		// 清掉回填的原值再输入非法值(模拟用户全选重输)。
		p.dashCustom.SetValue("")
		key(p, bad)
		keyName(p, "esc")
		if len(a.stack) != 2 {
			t.Fatalf("非法输入 %q 时 esc 不得 pop", bad)
		}
		if p.feedback == "" {
			t.Errorf("非法输入 %q 应展示原因", bad)
		}
		if a.draft.Refresh.DashboardInterval != 30 || a.draft.Refresh.WatchInterval != 30 {
			t.Errorf("校验失败应整体回滚(保持 30/30),实际 %+v", a.draft.Refresh)
		}
	}
	// 0 = 使用默认值,合法。
	a := newRefreshAppForTest(&config.Config{}, &config.Config{})
	p := newRefreshPage(a)
	keyName(p, " ")
	keyName(p, " ")
	p.dashCustom.SetValue("0")
	keyName(p, "esc")
	if a.draft.Refresh.DashboardInterval != 0 {
		t.Errorf("「其他…」输入 0 应写 0(默认语义),实际 %d", a.draft.Refresh.DashboardInterval)
	}
}

// TestRefreshPage_UntouchedKeepsRawValues 未触碰的项提交写回原值:旧配置(0/0)
// 只看不改,esc 后草稿仍 0/0(不固化隐式默认);App 级 dirty 不误报。
func TestRefreshPage_UntouchedKeepsRawValues(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}})
	a.stack = append(a.stack, newRefreshPage(a))
	p := a.stack[1].(*refreshPage)
	// 只移动光标,不空格、不输入。
	keyName(p, "down")
	keyName(p, "up")
	keyName(p, "esc")
	if len(a.stack) != 1 {
		t.Fatalf("esc 应 pop")
	}
	if a.draft.Refresh.DashboardInterval != 0 || a.draft.Refresh.WatchInterval != 0 {
		t.Errorf("未触碰项应写回原值 0/0,实际 %+v", a.draft.Refresh)
	}
	if a.dirty() {
		t.Errorf("只看不改不得产生 dirty")
	}
	// 显式配置的项同理:原值 20/45 未触碰 → 提交写回 20/45。
	a2 := newRefreshAppForTest(
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 45}},
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 45}},
	)
	a2.stack = append(a2.stack, newRefreshPage(a2))
	p2 := a2.stack[1].(*refreshPage)
	keyName(p2, "esc")
	if a2.draft.Refresh.DashboardInterval != 20 || a2.draft.Refresh.WatchInterval != 45 {
		t.Errorf("显式配置未触碰应写回 20/45,实际 %+v", a2.draft.Refresh)
	}
	if a2.dirty() {
		t.Errorf("只看不改不得产生 dirty")
	}
}

// TestRefreshPage_CursorMoves 光标在两项间移动并驱动空格作用对象。
func TestRefreshPage_CursorMoves(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{})
	p := newRefreshPage(a)
	if p.cursor != 0 {
		t.Fatalf("进入页面 cursor 应为 0,实际 %d", p.cursor)
	}
	keyName(p, "down")
	if p.cursor != 1 {
		t.Fatalf("down 后 cursor 应为 1")
	}
	keyName(p, "up")
	if p.cursor != 0 {
		t.Fatalf("up 后 cursor 应为 0")
	}
	// 光标在仪表盘项时空格只影响该项。
	keyName(p, " ")
	if p.dashTouched != true || p.watchTouched != false {
		t.Errorf("空格应只触碰当前项:dash=%v watch=%v", p.dashTouched, p.watchTouched)
	}
}

// TestRefreshPage_SummaryInMainMenu 主菜单摘要行展示两个刷新值(含默认标注)。
func TestRefreshPage_SummaryInMainMenu(t *testing.T) {
	a := newRefreshAppForTest(
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20}},
		&config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 30}},
	)
	m := newMainMenu(a)
	view := m.View()
	if !strings.Contains(view, "dash 20s") {
		t.Errorf("摘要应含 dash 20s, got:\n%s", view)
	}
	// watch 草稿为 0 → 取有效层 30 并带 (default) 标注。
	if !strings.Contains(view, "watch 30s (default)") {
		t.Errorf("摘要应含 watch 30s (default), got:\n%s", view)
	}
}

// TestRefreshPage_ViewShowsPresetsAndHelp View 列出全部预设选项与自定义提示。
func TestRefreshPage_ViewShowsPresetsAndHelp(t *testing.T) {
	a := newRefreshAppForTest(&config.Config{}, &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 30, WatchInterval: 30}})
	p := newRefreshPage(a)
	view := p.View()
	for _, want := range []string{"仪表盘自动刷新", "CLI watch 刷新", "10 / 20 / 30 / 60 / 其他", "poll_interval"} {
		if !strings.Contains(view, want) {
			t.Errorf("View 应含 %q, got:\n%s", want, view)
		}
	}
	// 英文侧同样列出选项集。
	if !strings.Contains(view, "10 / 20 / 30 / 60 / other") {
		t.Errorf("View 应含英文选项集, got:\n%s", view)
	}
}
