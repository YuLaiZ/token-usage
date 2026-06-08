package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestDaemonPage_CommitValid(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("42")
	p.commit()
	if a.draft.Daemon.PollInterval != 42 {
		t.Errorf("PollInterval = %d, want 42", a.draft.Daemon.PollInterval)
	}
}

func TestDaemonPage_CommitInvalidKeepsOld(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("not-int")
	p.commit()
	if a.draft.Daemon.PollInterval != 15 {
		t.Errorf("非法输入应保留旧值 15,实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_OpenCommitNotDirty 验证打开守护进程页直接 commit(未改输入)不 dirty。
func TestDaemonPage_OpenCommitNotDirty(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.commit()
	if a.draft.Daemon.PollInterval != 30 {
		t.Errorf("未改动 commit 应保持 30, got %d", a.draft.Daemon.PollInterval)
	}
	if a.dirty() {
		t.Errorf("打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.Daemon, a.diskBaseline.Daemon)
	}
}

// toggle 初始化取 edit.Daemon.AutoStart
func TestDaemonPage_ToggleInitFromAutoStart(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30, AutoStart: true}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	if !p.toggle.Value() {
		t.Error("toggle 初始值应取 edit.Daemon.AutoStart=true")
	}
}

// commit 写 AutoStart
func TestDaemonPage_CommitWritesAutoStart(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30, AutoStart: false}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	// 模拟翻转 toggle
	p.toggle = NewToggle("开机自启", true)
	p.commit()
	if !a.draft.Daemon.AutoStart {
		t.Error("commit 应把 toggle.Value() 写回 edit.Daemon.AutoStart")
	}
}

// cursor 切换:down 从 toggle(-1) → input(0),up 从 input(0) → toggle(-1)
func TestDaemonPage_CursorToggle(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	if p.cursor != -1 {
		t.Errorf("初始 cursor 应为 -1(toggle 聚焦),实际 %d", p.cursor)
	}
	// 按 down → cursor=0(input)
	updated, _ := p.Update(tea.KeyMsg{Type: tea.KeyDown})
	p = updated.(*daemonPage)
	if p.cursor != 0 {
		t.Errorf("down 后 cursor 应为 0,实际 %d", p.cursor)
	}
	// 按 up → cursor=-1(toggle)
	updated, _ = p.Update(tea.KeyMsg{Type: tea.KeyUp})
	p = updated.(*daemonPage)
	if p.cursor != -1 {
		t.Errorf("up 后 cursor 应为 -1,实际 %d", p.cursor)
	}
}

// space 翻转 toggle(cursor=-1 时)
func TestDaemonPage_SpaceTogglesWhenCursorOnToggle(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30, AutoStart: false}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	// cursor=-1(toggle 聚焦),按 space
	updated, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	p = updated.(*daemonPage)
	if !p.toggle.Value() {
		t.Error("space 应翻转 toggle")
	}
}

// TestDaemonPage_ViewContainsHint View() 输出含首次采集提示文案(collect all 新语法)。
func TestDaemonPage_ViewContainsHint(t *testing.T) {
	edit := &config.Config{}
	app := newAppForTest(edit, edit, nil) // app.go:73 的 helper，与 TestDaemonPage_CommitValid 同款装配
	p := newDaemonPage(app)
	view := p.View()
	if !strings.Contains(view, "collect all") {
		t.Errorf("期望 View() 输出含 'collect all' 提示，实际:\n%s", view)
	}
}
