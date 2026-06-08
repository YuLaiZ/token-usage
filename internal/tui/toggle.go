package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Toggle 自研布尔切换组件(bubbles 无 dedicated toggle)。
type Toggle struct {
	value   bool
	label   string
	focused bool
}

func NewToggle(label string, value bool) Toggle {
	return Toggle{label: label, value: value}
}

func (t Toggle) Value() bool            { return t.value }
func (t Toggle) Focused() bool          { return t.focused }
func (t Toggle) SetFocus(f bool) Toggle { t.focused = f; return t }

func (t Toggle) Update(msg tea.Msg) Toggle {
	k, ok := msg.(tea.KeyMsg)
	if !ok || !t.focused {
		return t
	}
	switch k.String() {
	case " ", "enter":
		t.value = !t.value
	}
	return t
}

func (t Toggle) View() string {
	mark := "○"
	if t.value {
		mark = "●"
	}
	s := mark + " " + t.label
	if t.focused {
		s = "[" + s + "]"
	}
	return s
}
