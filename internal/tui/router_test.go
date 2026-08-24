package tui

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	tea "github.com/charmbracelet/bubbletea"
)

func TestRouterPage_Commit(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/old"}}}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	p.setDBPath(0, "/new")
	p.commit()
	if a.draft.Routers["cc_switch"].DBPath != "/new" {
		t.Errorf("commit 后 DBPath = %q", a.draft.Routers["cc_switch"].DBPath)
	}
}

// TestRouterPage_EmptyDBPathCommitKeepsNotDirty 验证 db_path 为空时 commit 不引入脏写:
// 用户清空 db_path(commit 仅当非空才写),保持原值不变,不 dirty。
func TestRouterPage_EmptyDBPathCommitKeepsNotDirty(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/orig"}}}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	// 不改任何输入,直接 commit(esc 语义)
	p.commit()
	if a.draft.Routers["cc_switch"].DBPath != "/orig" {
		t.Errorf("未改动 commit 应保持原值, got %q", a.draft.Routers["cc_switch"].DBPath)
	}
	if a.dirty() {
		t.Errorf("打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.Routers, a.diskBaseline.Routers)
	}
}

// ---- 空列表安全 ----

// TestRouterPage_EmptyListKeysSafeNoPanic 验证 router 表为空时所有键操作不 panic、不越界、不改 draft。
func TestRouterPage_EmptyListKeysSafeNoPanic(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{}}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	if len(p.names) != 0 {
		t.Fatalf("空配置应无 router, got %v", p.names)
	}
	// esc 触发 commit + pop,空列表 commit 应 no-op 不 panic
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.dirty() {
		t.Error("空列表 esc 不应改 draft/dirty")
	}
	// 重新构造(esc 已 pop),测试空格/enter/up/down 安全
	p2 := newRouterPage(a)
	p2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	p2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	p2.Update(tea.KeyMsg{Type: tea.KeyDown})
	p2.Update(tea.KeyMsg{Type: tea.KeyUp})
}

// TestRouterPage_NilRoutersMapSafe 验证 Routers map 为 nil(测试直构)时页不 panic。
func TestRouterPage_NilRoutersMapSafe(t *testing.T) {
	edit := &config.Config{Routers: nil}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
}

// TestRouterPage_EmptyListShowsReason 验证空列表 View 显示原因说明(无已注册 router)。
func TestRouterPage_EmptyListShowsReason(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{}}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	view := p.View()
	if !contains(view, "无") {
		t.Errorf("空列表 View 应含「无」类说明, got:\n%s", view)
	}
}

// TestRouterPage_CursorBoundedWithinInputs 验证 cursor 不越界:
// 仅 1 个 router 时反复 down 不超出,cursor 停在 0。
func TestRouterPage_CursorBoundedWithinInputs(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/db"}}}
	a := newAppForTest(edit, edit, nil)
	p := newRouterPage(a)
	for i := 0; i < 5; i++ {
		p.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if p.cursor != 0 {
		t.Errorf("单 router 反复 down 应停在 0, got %d", p.cursor)
	}
}

// 双语化后 placeholder 为「(runtime default / 运行时默认)」,断言适配为包含中文片段:
// 校验意图不变——display 无默认时不虚构路径,显示通用运行时默认提示。
func TestRouterPage_MissingDisplayDefaultDoesNotInventPath(t *testing.T) {
	edit := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {}}}
	display := &config.Config{Routers: map[string]config.RouterConfig{"cc_switch": {}}}
	p := newRouterPage(newAppForTest(edit, display, nil))

	if got := p.inputs[0].Placeholder; !contains(got, "运行时默认") {
		t.Fatalf("placeholder = %q, want generic runtime-default hint", got)
	}
}
