package tui

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// detailLabelColWidth 客户端详情页 label 列宽:按双语化后最长项
// "paths.sessions_dir"(18)与"Router / 绑定路由"(显示宽度 17)取整加余量。
const detailLabelColWidth = 20

// 客户端列表页
type clientsPage struct {
	app    *App
	names  []string
	cursor int
}

func newClientsPage(app *App) *clientsPage {
	names := []string{}
	for n := range app.draft.Clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return &clientsPage{app: app, names: names}
}

func (p *clientsPage) title() string { return ui.Bi("Clients", "客户端") }
func (p *clientsPage) Init() tea.Cmd { return nil }

func (p *clientsPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.names)-1 {
			p.cursor++
		}
	case " ":
		// 切换启用:空列表时 no-op(不 panic)
		p.handleSpace()
	case "enter":
		// 进详情:空列表时 no-op(不 panic)
		if p.cursor >= 0 && p.cursor < len(p.names) {
			p.app.push(newClientDetailPage(p.app, p.names[p.cursor]))
		}
	}
	return p, nil
}

// handleSpace 切换当前光标客户端的启用状态。
// map value 不可寻址,需取出副本改后再回写。空列表/越界时 no-op。
func (p *clientsPage) handleSpace() {
	if p.cursor < 0 || p.cursor >= len(p.names) {
		return
	}
	name := p.names[p.cursor]
	c := p.app.draft.Clients[name]
	c.Enabled = !c.Enabled
	p.app.draft.Clients[name] = c
}

func (p *clientsPage) View() string {
	s := ""
	for i, name := range p.names {
		cur := "  "
		if i == p.cursor {
			cur = "▸ "
		}
		c := p.app.draft.Clients[name]
		mark := "○"
		if c.Enabled {
			mark = "●"
		}
		router := ui.Bi("None", "无")
		if c.Router != "" {
			router = c.Router
		}
		s += fmt.Sprintf("%s%s %s %s %s\n", cur, mark, pad(name, 10), ui.Bi("Router:", "路由:"), router)
	}
	if len(p.names) == 0 {
		s += "  (" + ui.Bi("no configured clients", "无已配置客户端") + ")\n"
	}
	s += "\n  " + ui.Bi("enter Edit", "回车 编辑") + "   " + ui.Bi("space Toggle enabled", "空格 切换启用") +
		"   " + ui.Bi("esc Back", "esc 返回") + "\n"
	return s
}

// 客户端详情页:启用 toggle / 绑定路由 / 各路径 textinput
// cursor 语义(对齐 daemon/logpage): -1=toggle 聚焦, 0..N-1=各字段聚焦。
// 字段布局: [0]=绑定路由(只读选择,space/enter 循环), [1..]=各 path key textinput。
type clientDetailPage struct {
	app    *App
	name   string
	fields []detailField
	cursor int
	toggle Toggle
}

type detailField struct {
	label string
	key   string // paths 的 key,空表示非路径字段
	input textinput.Model
	// isRouter 标记 router 绑定字段:聚焦时 space/enter 从「无 + RegisteredRouters」
	// 循环选择,禁自由文本输入(防拼写错误)。choices 为可选集,choiceIdx 当前索引。
	isRouter  bool
	choices   []string
	choiceIdx int
}

func newClientDetailPage(app *App, name string) *clientDetailPage {
	c := app.draft.Clients[name]
	p := &clientDetailPage{
		app:    app,
		name:   name,
		cursor: -1, // 默认 toggle 聚焦
	}
	p.toggle = NewToggle(ui.Bi("Enabled", "启用"), c.Enabled).SetFocus(true)
	// 路由字段:只读选择,从「无 + RegisteredRouters」枚举。
	// choices[0]=""(无), 其余为 RegisteredRouters()。
	// 仅支持归因回填的客户端提供该字段;其余客户端(CC Switch 不识别)不展示,
	// 避免配置出永不生效的 router。例外:存量配置已带非空 router 时仍显示,
	// 让用户能把值清回「无」(保存校验会拒绝非空值)。
	if runtimecfg.ClientSupportsRouter(name) || c.Router != "" {
		routerChoices := append([]string{""}, runtimecfg.RegisteredRouters()...)
		ti := textinput.New()
		ti.SetValue(c.Router)
		field := detailField{label: ui.Bi("Router", "绑定路由"), input: ti, isRouter: true, choices: routerChoices}
		field.choiceIdx = routerChoiceIndex(c.Router, routerChoices)
		p.fields = append(p.fields, field)
	}
	// 各路径:textinput,值为 draft(用户层);空则占位 display 默认。
	// path keys 来自 runtimecfg registry(共享只读入口)+ draft 用户已写键 + display 默认键并集。
	displayC := app.display.Clients[name]
	pathKeys := pathKeysFor(name, c.Paths, displayC.Paths)
	for _, pk := range pathKeys {
		ti := textinput.New()
		ti.SetValue(c.Paths[pk])
		ph := displayC.Paths[pk] // 默认占位
		ti.Placeholder = ph
		p.fields = append(p.fields, detailField{label: "paths." + pk, key: pk, input: ti})
	}
	return p
}

// routerChoiceIndex 返回 router 在 choices 中的索引,未匹配回退到 0(无)。
func routerChoiceIndex(router string, choices []string) int {
	for i, c := range choices {
		if c == router {
			return i
		}
	}
	return 0
}

func (p *clientDetailPage) title() string {
	return ui.Bi("Edit client", "编辑客户端") + ": " + p.name
}
func (p *clientDetailPage) Init() tea.Cmd { return nil }

func (p *clientDetailPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p.delegateInput(msg), nil
	}
	switch k.String() {
	case "esc":
		// 校验失败时不 pop 页面(本页无 numeric 校验,commit 恒 nil;保留守卫与统一契约一致)。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
		return p, nil
	case "up":
		if p.cursor > -1 {
			p.cursor--
			p.syncFocus()
		}
		return p, nil
	case "down":
		if p.cursor < len(p.fields)-1 {
			p.cursor++
			p.syncFocus()
		}
		return p, nil
	case "k":
		if p.focusesEditablePath() {
			return p.delegateInput(msg), nil
		}
		if p.cursor > -1 {
			p.cursor--
			p.syncFocus()
		}
		return p, nil
	case "j":
		if p.focusesEditablePath() {
			return p.delegateInput(msg), nil
		}
		if p.cursor < len(p.fields)-1 {
			p.cursor++
			p.syncFocus()
		}
		return p, nil
	case " ", "enter":
		if p.cursor == -1 {
			// toggle 聚焦:翻转 enabled
			p.toggle = p.toggle.Update(msg)
			return p, nil
		}
		f := &p.fields[p.cursor]
		if f.isRouter {
			// router 字段:循环选择,不接受自由文本
			f.choiceIdx = (f.choiceIdx + 1) % len(f.choices)
			f.input.SetValue(f.choices[f.choiceIdx])
			return p, nil
		}
		// path textinput 字段:enter 不特别处理,委托 textinput;space 由 textinput 接收(空格字符)
	}
	return p.delegateInput(msg), nil
}

// focusesEditablePath 报告当前焦点是否为可自由输入的 path 字段。
// j/k 在 toggle/router 上作为导航键，在路径输入框中必须作为普通字符写入。
func (p *clientDetailPage) focusesEditablePath() bool {
	return p.cursor >= 0 &&
		p.cursor < len(p.fields) &&
		!p.fields[p.cursor].isRouter
}

// syncFocus 移动 cursor 时同步聚焦:toggle 与 textinput 互斥聚焦。
func (p *clientDetailPage) syncFocus() {
	if p.cursor == -1 {
		p.toggle = p.toggle.SetFocus(true)
		p.blurAllInputs()
		return
	}
	p.toggle = p.toggle.SetFocus(false)
	// 仅聚焦当前字段,其余失焦
	for i := range p.fields {
		if i == p.cursor && !p.fields[i].isRouter {
			p.fields[i].input.Focus()
		} else {
			p.fields[i].input.Blur()
		}
	}
}

// blurAllInputs 使所有字段 textinput 失焦。
func (p *clientDetailPage) blurAllInputs() {
	for i := range p.fields {
		p.fields[i].input.Blur()
	}
}

// delegateInput 把消息交给当前聚焦的 path textinput。
// router 字段(isRouter)不接收自由文本委托(只读选择)。
func (p *clientDetailPage) delegateInput(msg tea.Msg) *clientDetailPage {
	if p.cursor >= 0 && p.cursor < len(p.fields) {
		f := &p.fields[p.cursor]
		if f.isRouter {
			return p // router 字段只读,不委托自由文本
		}
		m, cmd := f.input.Update(msg)
		f.input = m
		_ = cmd
	}
	return p
}

// commit 把详情页输入写回 draft model。
// router 字段:仅当值在 registry 枚举(无 + RegisteredRouters)内才写入,未注册归一化为空。
// path 键采用 lazy init:仅当有非空值待写才初始化 c.Paths,
// 这样 nil Paths 保持 nil、empty Paths 保持 empty,不强制转换,
// 与 diskBaseline(cloneConfig 保留原态)DeepEqual 一致,避免 dirty 误报。
// 本页字段均非 numeric(布尔 toggle / router 选择 / path 文本),无校验失败路径,恒返回 nil。
func (p *clientDetailPage) commit() error {
	c := p.app.draft.Clients[p.name]
	c.Enabled = p.toggle.Value()
	// router 字段:按 isRouter 标志定位(非 Claude 客户端无该字段、存量清除时
	// 可能不在 index 0),值必须落在 registry 枚举内,否则归一化为空
	//(防未注册进入草稿)。
	routerVal := ""
	for _, f := range p.fields {
		if f.isRouter {
			if routerChoiceIndex(f.input.Value(), f.choices) > 0 {
				routerVal = f.input.Value()
			}
			break
		}
	}
	c.Router = routerVal
	for _, f := range p.fields {
		if f.key == "" || f.isRouter {
			continue
		}
		if v := f.input.Value(); v != "" {
			if c.Paths == nil {
				c.Paths = map[string]string{}
			}
			c.Paths[f.key] = v
		} else if c.Paths != nil {
			delete(c.Paths, f.key) // 用户清空已有值:仅当 Paths 非 nil 时删
		}
	}
	p.app.draft.Clients[p.name] = c
	return nil
}

func (p *clientDetailPage) View() string {
	s := ui.Bi("Edit client", "编辑客户端") + ": " + p.name + "\n\n"
	tc := "  "
	if p.cursor == -1 {
		tc = "▸ "
	}
	s += tc + p.toggle.View() + "   (" + ui.Bi("space toggles", "空格切换") + ")\n"
	for i, f := range p.fields {
		cur := "  "
		if i == p.cursor {
			cur = "▸ "
		}
		hint := ""
		if f.isRouter {
			hint = "   (" + ui.Bi("space/enter selects", "空格/回车选择") + ")"
		}
		s += cur + pad(f.label, detailLabelColWidth) + f.input.View() + hint + "\n"
	}
	s += "\n  " + ui.Bi("esc Apply to draft and return (main-menu s saves to disk)",
		"esc 应用到草稿并返回(主菜单 s 保存写盘)") + "\n"
	return s
}

// pathKeysFor 返回某 client 的路径键集合,用于详情页渲染各 paths.* textinput。
// 规范键来自 runtimecfg.RegisteredClientPathKeys(共享只读 registry),与 draft 用户层
// 已写键、display 运行时层默认键取并集,排序后返回;这样 TUI 能展示用户实际配置过的键。
func pathKeysFor(name string, draftPaths, displayPaths map[string]string) []string {
	seen := make(map[string]struct{})
	var keys []string
	add := func(k string) {
		if k == "" {
			return
		}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	// 规范键来自 runtimecfg registry(共享只读入口)
	for _, k := range runtimecfg.RegisteredClientPathKeys(name) {
		add(k)
	}
	for k := range displayPaths {
		add(k)
	}
	for k := range draftPaths {
		add(k)
	}
	sort.Strings(keys)
	return keys
}
