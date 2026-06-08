package tui

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// routerPage 路由中间件页:列出 app.draft.Routers,每行一个 db_path textinput,
// commit 把各 textinput 值写回 draft.Routers[name].DBPath。
type routerPage struct {
	app    *App
	names  []string
	inputs []textinput.Model
	cursor int
}

func newRouterPage(app *App) *routerPage {
	names := []string{}
	for n := range app.draft.Routers {
		names = append(names, n)
	}
	sort.Strings(names)
	p := &routerPage{app: app, names: names}
	for _, n := range names {
		ti := textinput.New()
		ti.SetValue(app.draft.Routers[n].DBPath)
		ph := app.display.Routers[n].DBPath
		if ph == "" {
			ph = "(运行时默认)"
		}
		ti.Placeholder = ph
		p.inputs = append(p.inputs, ti)
	}
	p.syncFocus()
	return p
}

func (p *routerPage) title() string { return "路由中间件" }
func (p *routerPage) Init() tea.Cmd { return nil }

// setDBPath 设置第 idx 行 db_path textinput 的值(测试用)。
func (p *routerPage) setDBPath(idx int, v string) {
	if idx >= 0 && idx < len(p.inputs) {
		p.inputs[idx].SetValue(v)
	}
}

func (p *routerPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p.delegateInput(msg), nil
	}
	switch k.String() {
	case "esc":
		// 校验失败时不 pop(本页无 numeric 校验,commit 恒 nil;保留守卫与统一契约一致)。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down":
		if p.cursor < len(p.names)-1 {
			p.cursor++
		}
	}
	p.syncFocus()
	return p.delegateInput(msg), nil
}

func (p *routerPage) syncFocus() {
	for i := range p.inputs {
		if i == p.cursor {
			_ = p.inputs[i].Focus()
		} else {
			p.inputs[i].Blur()
		}
	}
}

// delegateInput 把消息交给当前聚焦的 textinput。
func (p *routerPage) delegateInput(msg tea.Msg) *routerPage {
	if p.cursor >= 0 && p.cursor < len(p.inputs) {
		m, cmd := p.inputs[p.cursor].Update(msg)
		p.inputs[p.cursor] = m
		_ = cmd
	}
	return p
}

// commit 把各 db_path textinput 写回 draft.Routers。
// 打开页时 textinput 已用 draft 原值初始化,未改动则写回相同值,不 dirty。
// Routers 是 map[string]RouterConfig(值类型,无嵌套 map),cloneConfig 浅拷贝对齐。
// 本页字段为路径文本(非 numeric),无校验失败路径,恒返回 nil。
func (p *routerPage) commit() error {
	for i, name := range p.names {
		c := p.app.draft.Routers[name]
		c.DBPath = p.inputs[i].Value()
		p.app.draft.Routers[name] = c
	}
	return nil
}

func (p *routerPage) View() string {
	s := "路由中间件\n\n"
	for i, name := range p.names {
		cur := "  "
		if i == p.cursor {
			cur = "▸ "
		}
		s += fmt.Sprintf("%s%s%s\n", cur, pad(name, 14), p.inputs[i].View())
	}
	if len(p.names) == 0 {
		s += "  (无已配置路由中间件)\n"
	}
	s += "\n  编辑 db_path   esc 应用到草稿并返回(主菜单 s 保存写盘)\n"
	return s
}
