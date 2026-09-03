// internal/tui/toggle_test.go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestToggle_SpaceTogglesValue 聚焦时按空格翻转布尔值。
func TestToggle_SpaceTogglesValue(t *testing.T) {
	tg := NewToggle("enable", false).SetFocus(true)
	tg = tg.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !tg.Value() {
		t.Fatal("聚焦时空格应把 false 翻转为 true")
	}
	tg = tg.Update(tea.KeyMsg{Type: tea.KeySpace})
	if tg.Value() {
		t.Error("再次空格应翻转回 false")
	}
}

// TestToggle_EnterTogglesValue 聚焦时按 enter 同样翻转布尔值。
func TestToggle_EnterTogglesValue(t *testing.T) {
	tg := NewToggle("enable", false).SetFocus(true)
	tg = tg.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !tg.Value() {
		t.Fatal("聚焦时 enter 应把 false 翻转为 true")
	}
}

// TestToggle_IgnoresKeysWhenUnfocused 未聚焦时空格/enter 不翻转。
func TestToggle_IgnoresKeysWhenUnfocused(t *testing.T) {
	tg := NewToggle("enable", false)
	tg = tg.Update(tea.KeyMsg{Type: tea.KeySpace})
	if tg.Value() {
		t.Error("未聚焦时空格不得翻转值")
	}
	tg = tg.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if tg.Value() {
		t.Error("未聚焦时 enter 不得翻转值")
	}
}

// TestToggle_IgnoresOtherKeys 聚焦时其他按键不翻转值。
func TestToggle_IgnoresOtherKeys(t *testing.T) {
	tg := NewToggle("enable", false).SetFocus(true)
	tg = tg.Update(keyMsg("j"))
	tg = tg.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if tg.Value() {
		t.Error("聚焦时 j/esc 等其他按键不得翻转值")
	}
}

// TestToggle_IgnoresNonKeyMessages 非键盘消息(如窗口尺寸变化)不影响值与聚焦态。
func TestToggle_IgnoresNonKeyMessages(t *testing.T) {
	tg := NewToggle("enable", true).SetFocus(true)
	tg = tg.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !tg.Value() || !tg.Focused() {
		t.Errorf("非键盘消息不得改变状态, value=%v focused=%v", tg.Value(), tg.Focused())
	}
}

// TestToggle_SetFocusPreservesValue SetFocus 只改聚焦态,不重置布尔值。
func TestToggle_SetFocusPreservesValue(t *testing.T) {
	tg := NewToggle("enable", true)
	tg = tg.SetFocus(true)
	if !tg.Focused() || !tg.Value() {
		t.Errorf("SetFocus(true) 应只改聚焦态, value=%v focused=%v", tg.Value(), tg.Focused())
	}
	tg = tg.SetFocus(false)
	if tg.Focused() {
		t.Error("SetFocus(false) 应清除聚焦态")
	}
}

// TestToggle_ViewRendersMarkAndFocus View 按值渲染 ○/●,聚焦时加方括号包裹。
func TestToggle_ViewRendersMarkAndFocus(t *testing.T) {
	tests := []struct {
		name    string
		value   bool
		focused bool
		want    string
	}{
		{"off unfocused", false, false, "○ enable"},
		{"on unfocused", true, false, "● enable"},
		{"off focused", false, true, "[○ enable]"},
		{"on focused", true, true, "[● enable]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := NewToggle("enable", tt.value).SetFocus(tt.focused).View()
			if view != tt.want {
				t.Errorf("View() = %q, want %q", view, tt.want)
			}
			if !strings.Contains(view, "enable") {
				t.Errorf("View 应含标签文本, got %q", view)
			}
		})
	}
}
