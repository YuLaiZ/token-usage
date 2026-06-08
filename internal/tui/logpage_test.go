package tui

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	tea "github.com/charmbracelet/bubbletea"
)

func TestLogPage_Commit(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setDir("/logs")
	p.setMaxDays("3")
	p.toggleLevel() // "" → info
	p.toggleLevel() // info → debug
	p.commit()
	if a.draft.Log.Dir != "/logs" || a.draft.Log.MaxDays != 3 || a.draft.Log.Level != "debug" {
		t.Errorf("commit 后 Log = %+v", a.draft.Log)
	}
}

// TestLogPage_InvalidMaxDaysKeepsOld 验证 max_days 非法输入 commit 保留旧值。
func TestLogPage_InvalidMaxDaysKeepsOld(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{Level: "info", Dir: "/old", MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("abc")
	p.commit()
	if a.draft.Log.MaxDays != 7 {
		t.Errorf("非法 max_days 应保留旧值 7, got %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_ToggleLevelCycle 验证 level 五态循环 "" → info → debug → warn → error → ""。
// 覆盖 registry 全部注册 level(含 warn/error,修复旧版仅三态遗漏 warn/error)。
func TestLogPage_ToggleLevelCycle(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	if p.level != "" {
		t.Fatalf("初始 level 应为空, got %q", p.level)
	}
	want := []string{"info", "debug", "warn", "error", ""}
	for i, w := range want {
		p.toggleLevel()
		if p.level != w {
			t.Errorf("第 %d 次 toggle 应 %q, got %q", i+1, w, p.level)
		}
	}
}

func TestLogPage_KeyboardInputFollowsCursorFocus(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)

	p.Update(tea.KeyMsg{Type: tea.KeyDown}) // level → dir
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/logs")})
	if got := p.dirInput.Value(); got != "/logs" {
		t.Fatalf("dir 输入框应能接收键盘输入，got %q", got)
	}

	p.Update(tea.KeyMsg{Type: tea.KeyDown}) // dir → max_days
	p.maxDaysInput.SetValue("")
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	if got := p.maxDaysInput.Value(); got != "3" {
		t.Fatalf("max_days 输入框应在切换焦点后接收输入，got %q", got)
	}
}
