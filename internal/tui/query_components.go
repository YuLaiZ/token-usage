package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// selectOutcome 描述一个可提交/可取消组件会话的当前状态。
type selectOutcome int

const (
	selectPending selectOutcome = iota
	selectSubmitted
	selectCancelled
)

// orderedSelect 是与业务无关的有序多选组件:
// 候选集合固定、已选集合有序,键盘合同为
//
//	↑/k、↓/j 移动候选光标;space 选中/取消当前候选(选中追加到已选尾部);
//	[、] 把当前已选项在已选序列中前移/后移一位(未选中项无操作,首尾不越界);
//	enter 提交深拷贝的有序选择;esc 取消且不影响调用方数据。
//
// 组件只接收候选、初始选择与标题文案,不知道 builtin/custom/group 规则,
// 也不创建全局可变状态;页面负责把提交结果应用到草稿。
type orderedSelect struct {
	title      string
	candidates []string
	selected   []string
	cursor     int
	done       selectOutcome
	keyHandler func(key string) bool
}

// newOrderedSelect 构造有序多选;initial 中不在候选内的项被过滤,顺序保留。
func newOrderedSelect(candidates []string, initial []string, title string) *orderedSelect {
	s := &orderedSelect{
		title:      title,
		candidates: append([]string(nil), candidates...),
	}
	set := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		set[c] = true
	}
	for _, name := range initial {
		if set[name] {
			s.selected = append(s.selected, name)
		}
	}
	if s.cursor >= len(s.candidates) {
		s.cursor = len(s.candidates) - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	return s
}

// handleKeys 依次处理按键(测试与页面共用同一键盘合同)。
func (s *orderedSelect) handleKeys(msgs ...tea.KeyMsg) {
	for _, m := range msgs {
		s.HandleKey(m)
	}
}

// HandleKey 处理一个按键;已提交/取消后忽略后续输入。
func (s *orderedSelect) HandleKey(msg tea.KeyMsg) {
	if s.done != selectPending {
		return
	}
	switch msg.String() {
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
	case "down", "j":
		if s.cursor < len(s.candidates)-1 {
			s.cursor++
		}
	case " ":
		s.toggle()
	case "[":
		s.moveSelected(-1)
	case "]":
		s.moveSelected(1)
	case "enter":
		s.done = selectSubmitted
	case "esc":
		s.done = selectCancelled
	}
}

// toggle 选中当前候选(追加到已选尾部)或取消选中(从已选移除),不产生重复项。
func (s *orderedSelect) toggle() {
	if s.cursor < 0 || s.cursor >= len(s.candidates) {
		return
	}
	name := s.candidates[s.cursor]
	for i, sel := range s.selected {
		if sel == name {
			s.selected = append(s.selected[:i], s.selected[i+1:]...)
			return
		}
	}
	s.selected = append(s.selected, name)
}

// moveSelected 把当前候选(若已选中)在已选序列中移动 step 位;首尾不越界。
func (s *orderedSelect) moveSelected(step int) {
	if s.cursor < 0 || s.cursor >= len(s.candidates) {
		return
	}
	name := s.candidates[s.cursor]
	for i, sel := range s.selected {
		if sel != name {
			continue
		}
		j := i + step
		if j < 0 || j >= len(s.selected) {
			return
		}
		s.selected[i], s.selected[j] = s.selected[j], s.selected[i]
		return
	}
}

// Done 返回会话状态。
func (s *orderedSelect) Done() selectOutcome { return s.done }

// Selection 返回已选序列的深拷贝。
func (s *orderedSelect) Selection() []string {
	return append([]string(nil), s.selected...)
}

// View 渲染候选列表(光标与选中标记)与有序预览。
func (s *orderedSelect) View() string {
	var b strings.Builder
	b.WriteString(s.title + "\n\n")
	for i, name := range s.candidates {
		mark := "  "
		if isSelected(s.selected, name) {
			mark = "● "
		}
		cursor := " "
		if i == s.cursor {
			cursor = "▸"
		}
		b.WriteString("  " + cursor + " " + mark + name + "\n")
	}
	if len(s.candidates) == 0 {
		b.WriteString("  (" + ui.Bi("no candidates", "无候选") + ")\n")
	}
	b.WriteString("\n  " + ui.Bi("Selected order", "已选顺序") + ": " + previewOrder(s.selected) + "\n")
	b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " + ui.Bi("space Select", "space 选择") + "   " +
		ui.Bi("[ ] Reorder", "[ ] 调序") + "   " + ui.Bi("enter Confirm", "enter 确认") + "   " + ui.Bi("esc Cancel", "esc 取消") + "\n")
	return b.String()
}

func isSelected(selected []string, name string) bool {
	for _, s := range selected {
		if s == name {
			return true
		}
	}
	return false
}

// previewOrder 渲染有序预览:空列表为空串,其余以箭头连接。
func previewOrder(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.Join(items, " → ")
}

// namePrompt 是可复用的单行名称输入组件:
// enter 提交 trim 后的值(合法性由注入的校验函数判定,非法停留并显示反馈),
// esc 取消。组件不写任何配置;校验回调只读输入。
type namePrompt struct {
	input    textinput.Model
	validate func(string) string
	done     selectOutcome
	value    string
}

// newNamePrompt 构造名称输入;validate 返回空串表示合法,否则返回双语反馈文案。
func newNamePrompt(title string, validate func(string) string) *namePrompt {
	ti := textinput.New()
	ti.Placeholder = title
	ti.Focus()
	return &namePrompt{input: ti, validate: validate}
}

func (p *namePrompt) handleKeys(msgs ...tea.KeyMsg) {
	for _, m := range msgs {
		p.HandleKey(m)
	}
}

// HandleKey 处理按键;enter 提交(trim + 校验),esc 取消,其余委托 textinput。
func (p *namePrompt) HandleKey(msg tea.KeyMsg) {
	if p.done != selectPending {
		return
	}
	switch msg.String() {
	case "enter":
		name := strings.TrimSpace(p.input.Value())
		if p.validate != nil {
			if feedback := p.validate(name); feedback != "" {
				return
			}
		}
		p.value = name
		p.done = selectSubmitted
	case "esc":
		p.done = selectCancelled
	default:
		m, _ := p.input.Update(msg)
		p.input = m
	}
}

// Done 返回会话状态。
func (p *namePrompt) Done() selectOutcome { return p.done }

// Value 返回提交值(仅 Done 为 submitted 时有意义)。
func (p *namePrompt) Value() string { return p.value }

// Feedback 返回当前输入的校验反馈(空串表示合法或未输入)。
func (p *namePrompt) Feedback() string {
	name := strings.TrimSpace(p.input.Value())
	if p.validate == nil || name == "" {
		return ""
	}
	return p.validate(name)
}

// View 渲染输入与校验反馈。
func (p *namePrompt) View() string {
	s := p.input.View() + "\n"
	if fb := p.Feedback(); fb != "" {
		s += "\n  " + fb + "\n"
	}
	s += "\n  " + ui.Bi("enter Confirm", "enter 确认") + "   " + ui.Bi("esc Cancel", "esc 取消") + "\n"
	return s
}
