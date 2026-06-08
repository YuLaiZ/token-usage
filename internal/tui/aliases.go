package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// aliasesPage provider_aliases 页:list key→value,a 新增/d 删除/enter 编辑 value。
// 改动原地写 app.draft.ProviderAliases(add/delete/edit 直接改 map)。
// commit 为 no-op:改动原地生效,无需写回;保持 ProviderAliases nil/empty 原态,
// 与 cloneConfig(保留原态)对齐,避免 dirty 误报。
//
// mode: 0=浏览, 1=新增输入 key, 2=输入/编辑 value。
// feedback: 最近一次操作反馈(已覆盖/已删除等),View 渲染。
type aliasesPage struct {
	app      *App
	keys     []string
	cursor   int
	mode     int
	editKey  string
	keyInput textinput.Model
	valInput textinput.Model
	feedback string
}

func newAliasesPage(app *App) *aliasesPage {
	p := &aliasesPage{app: app}
	p.refreshKeys()
	p.keyInput = textinput.New()
	p.keyInput.Placeholder = "provider key"
	p.valInput = textinput.New()
	p.valInput.Placeholder = "display name"
	return p
}

// refreshKeys 重新读 draft.ProviderAliases 的 key 并排序,供 View 渲染。
func (p *aliasesPage) refreshKeys() {
	p.keys = nil
	for k := range p.app.draft.ProviderAliases {
		p.keys = append(p.keys, k)
	}
	sort.Strings(p.keys)
}

func (p *aliasesPage) title() string { return "Provider 别名" }
func (p *aliasesPage) Init() tea.Cmd { return nil }

// add 新增或覆盖 alias(key→value)。key/value 均 trim 后非空才写入。
// lazy init:仅 ProviderAliases 为 nil 才 init 为 non-nil。
// 成功返回 true;失败(key 或 value trim 后空)不改变 draft/dirty,反馈失败提示。
func (p *aliasesPage) add(key, val string) bool {
	k := strings.TrimSpace(key)
	v := strings.TrimSpace(val)
	if k == "" || v == "" {
		p.feedback = "key 和 value 不能为空"
		return false
	}
	overwrite := false
	if _, ok := p.app.draft.ProviderAliases[k]; ok {
		overwrite = true
	}
	if p.app.draft.ProviderAliases == nil {
		p.app.draft.ProviderAliases = map[string]string{}
	}
	p.app.draft.ProviderAliases[k] = v
	p.refreshKeys()
	if overwrite {
		p.feedback = fmt.Sprintf("已覆盖 %s", k)
	} else {
		p.feedback = fmt.Sprintf("已新增 %s", k)
	}
	return true
}

// deleteKey 删除 alias。成功返回 true 并设「已删除 <key>」反馈;
// key 不存在返回 false(无可删项)。不归 nil:删除至空时保留 empty non-nil 原态,
// 与 cloneConfig 对齐(避免 nil↔empty 转换触发 dirty 误报)。
func (p *aliasesPage) deleteKey(key string) bool {
	if _, ok := p.app.draft.ProviderAliases[key]; !ok {
		p.feedback = "无可删除的项"
		return false
	}
	delete(p.app.draft.ProviderAliases, key)
	p.refreshKeys()
	p.feedback = fmt.Sprintf("已删除 %s", key)
	return true
}

// editValue 编辑现有 key 的 value(键不存在时不写入,避免引入新键)。
// value trim 后非空才写入;空则失败(false)保留原值,不改 draft/dirty。
func (p *aliasesPage) editValue(key, val string) bool {
	v := strings.TrimSpace(val)
	if v == "" {
		p.feedback = "value 不能为空"
		return false
	}
	if _, ok := p.app.draft.ProviderAliases[key]; ok {
		p.app.draft.ProviderAliases[key] = v
		p.feedback = fmt.Sprintf("已更新 %s", key)
		return true
	}
	p.feedback = "键不存在"
	return false
}

// commit no-op:改动原地生效,无需写回。存在供 esc 调用与测试守护 nil/empty 对称。
// 本页无 numeric 校验(add/edit/delete 已在原地校验 key/value 非空),恒返回 nil。
func (p *aliasesPage) commit() error { return nil }

func (p *aliasesPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p.delegateInput(msg), nil
	}
	// 输入模式优先处理(enter 提交 / esc 退出输入模式)
	if p.mode != 0 {
		switch k.String() {
		case "esc":
			p.mode = 0
			p.syncInputFocus()
			return p, nil
		case "enter":
			if p.mode == 1 {
				p.editKey = p.keyInput.Value()
				p.mode = 2
				p.syncInputFocus()
			} else {
				// 提交新增:value 校验失败时保留输入(editKey/valInput 不清),
				// 停留 mode=2 供修正。
				if p.add(p.editKey, p.valInput.Value()) {
					p.mode = 0
					p.syncInputFocus()
				}
			}
			return p, nil
		}
		return p.delegateInput(msg), nil
	}
	// 浏览模式
	switch k.String() {
	case "esc":
		// 校验失败时不 pop(本页 commit 恒 nil,保留守卫与统一契约一致)。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.keys)-1 {
			p.cursor++
		}
	case "a":
		p.mode = 1
		p.keyInput.SetValue("")
		p.valInput.SetValue("")
		p.syncInputFocus()
	case "d":
		if p.cursor >= 0 && p.cursor < len(p.keys) {
			p.deleteKey(p.keys[p.cursor])
			if p.cursor >= len(p.keys) {
				p.cursor = len(p.keys) - 1
			}
		}
	case "enter":
		if p.cursor >= 0 && p.cursor < len(p.keys) {
			p.editKey = p.keys[p.cursor]
			p.valInput.SetValue(p.app.draft.ProviderAliases[p.editKey])
			p.mode = 2
			p.syncInputFocus()
		}
	}
	return p, nil
}

func (p *aliasesPage) syncInputFocus() {
	if p.mode == 1 {
		_ = p.keyInput.Focus()
	} else {
		p.keyInput.Blur()
	}
	if p.mode == 2 {
		_ = p.valInput.Focus()
	} else {
		p.valInput.Blur()
	}
}

// delegateInput 在输入模式把消息委托给对应 textinput。
func (p *aliasesPage) delegateInput(msg tea.Msg) *aliasesPage {
	switch p.mode {
	case 1:
		m, _ := p.keyInput.Update(msg)
		p.keyInput = m
	case 2:
		m, _ := p.valInput.Update(msg)
		p.valInput = m
	}
	return p
}

func (p *aliasesPage) View() string {
	s := "Provider 别名\n\n"
	for i, k := range p.keys {
		cur := "  "
		if i == p.cursor && p.cursor < len(p.keys) {
			cur = "▸ "
		}
		s += cur + k + " → " + p.app.draft.ProviderAliases[k] + "\n"
	}
	if len(p.keys) == 0 {
		s += "  (无别名)\n"
	}
	if p.mode == 1 {
		s += "\n  新增 key: " + p.keyInput.View() + "\n"
	} else if p.mode == 2 {
		s += "\n  " + p.editKey + " 的 value: " + p.valInput.View() + "\n"
	}
	if p.feedback != "" {
		s += "\n  " + p.feedback + "\n"
	}
	s += "\n  a 新增   d 删除   enter 编辑   esc 返回\n"
	return s
}
