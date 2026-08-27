package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// ---- ? 帮助层 ----

// TestMainMenu_HelpOpensOnQuestion 主菜单按 ? 进入帮助 overlay(showHelp=true)。
func TestMainMenu_HelpOpensOnQuestion(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	updated, _ := m.Update(keyMsg("?"))
	m = updated.(*mainMenu)
	if !m.showHelp {
		t.Fatal("按 ? 应打开帮助层(showHelp=true)")
	}
}

// TestMainMenu_HelpOverlayContainsKeyBindings 帮助层 View 含各按键说明。
func TestMainMenu_HelpOverlayContainsKeyBindings(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	m.showHelp = true
	view := m.View()
	for _, want := range []string{"保存", "退出", "草稿", "data_dir"} {
		if !strings.Contains(view, want) {
			t.Errorf("帮助层应含 %q, got:\n%s", want, view)
		}
	}
}

// TestMainMenu_HelpClosesOnEscOrQuestion 帮助层开时 esc / ? 关闭,不穿透到 exit 分流。
func TestMainMenu_HelpClosesOnEscOrQuestion(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	m.showHelp = true
	// esc 关闭
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*mainMenu)
	if m.showHelp {
		t.Error("帮助层开时 esc 应关闭帮助层,不穿透退出分流")
	}
	// 再次 ? 切换关闭
	m.showHelp = true
	updated, _ = m.Update(keyMsg("?"))
	m = updated.(*mainMenu)
	if m.showHelp {
		t.Error("帮助层开时再按 ? 应关闭(toggle)")
	}
}

// TestMainMenu_HelpOpenSwallowsNavigation 帮助层开时 j/k/enter 不导航 cursor。
func TestMainMenu_HelpOpenSwallowsNavigation(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	m.showHelp = true
	before := m.cursor
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.cursor != before {
		t.Errorf("帮助层开时导航应被吞, cursor %d→%d", before, m.cursor)
	}
}

// ---- data_dir 说明页 ----

// TestMainMenu_DataDirEntersReadOnlyPage 主菜单 cursor=5(数据目录) enter 进入 dataDirPage。
func TestMainMenu_DataDirEntersReadOnlyPage(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	m.cursor = 6 // 数据目录(只读)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.stack) != 2 {
		t.Fatalf("enter 数据目录应 push 说明页, 栈长=%d", len(a.stack))
	}
	if _, ok := a.stack[1].(*dataDirPage); !ok {
		t.Fatalf("栈顶应为 *dataDirPage, got %T", a.stack[1])
	}
}

// TestDataDirPage_ViewExplainsPathAndMigration 说明页 View 解释固定 config 路径、迁移风险、config set 命令。
func TestDataDirPage_ViewExplainsPathAndMigration(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	p := newDataDirPage(a)
	view := p.View()
	for _, want := range []string{
		"config.toml",         // 固定 config 路径
		"迁移",                  // 迁移风险
		"config set data_dir", // 命令前缀
		"--confirm-migrate",   // 确认标志
		"usage.db",            // 迁移对象
	} {
		if !strings.Contains(view, want) {
			t.Errorf("data_dir 说明页应含 %q, got:\n%s", want, view)
		}
	}
}

// TestDataDirPage_EscReturns 说明页 esc 返回主菜单(只读,无 commit)。
func TestDataDirPage_EscReturns(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	p := newDataDirPage(a)
	a.push(p)
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 1 {
		t.Errorf("data_dir 说明页 esc 应 pop 回主菜单, 栈长=%d", len(a.stack))
	}
}

// ---- 子页 esc 文案统一为「应用到草稿」 ----

func TestRouterPage_FooterSaysApplyToDraft(t *testing.T) {
	a := newAppForTest(&config.Config{}, &config.Config{}, nil)
	p := newRouterPage(a)
	view := p.View()
	if !strings.Contains(view, "应用到草稿") {
		t.Errorf("router 页 footer 应含「应用到草稿」, got:\n%s", view)
	}
	if strings.Contains(view, "自动保存") {
		t.Errorf("router 页 footer 不得含「自动保存」, got:\n%s", view)
	}
}

func TestDaemonPage_FooterSaysApplyToDraft(t *testing.T) {
	a := newAppForTest(&config.Config{}, &config.Config{}, nil)
	p := newDaemonPage(a)
	view := p.View()
	if !strings.Contains(view, "应用到草稿") {
		t.Errorf("daemon 页 footer 应含「应用到草稿」, got:\n%s", view)
	}
	if strings.Contains(view, "自动保存") {
		t.Errorf("daemon 页 footer 不得含「自动保存」, got:\n%s", view)
	}
}

func TestLogPage_FooterSaysApplyToDraft(t *testing.T) {
	a := newAppForTest(&config.Config{}, &config.Config{}, nil)
	p := newLogPage(a)
	view := p.View()
	if !strings.Contains(view, "应用到草稿") {
		t.Errorf("log 页 footer 应含「应用到草稿」, got:\n%s", view)
	}
	if strings.Contains(view, "自动保存") {
		t.Errorf("log 页 footer 不得含「自动保存」, got:\n%s", view)
	}
}

func TestClientDetailPage_FooterSaysApplyToDraft(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "codex")
	view := p.View()
	if !strings.Contains(view, "应用到草稿") {
		t.Errorf("client 详情页 footer 应含「应用到草稿」, got:\n%s", view)
	}
}

// ---- daemon 页 collect all 新语法 ----

// TestDaemonPage_ViewUsesCollectAllSyntax daemon 页提示使用 `collect all`(非旧 `collect --all`)。
func TestDaemonPage_ViewUsesCollectAllSyntax(t *testing.T) {
	a := newAppForTest(&config.Config{}, &config.Config{}, nil)
	p := newDaemonPage(a)
	view := p.View()
	if !strings.Contains(view, "collect all") {
		t.Errorf("daemon 页应使用 `collect all` 新语法, got:\n%s", view)
	}
	if strings.Contains(view, "collect --all") {
		t.Errorf("daemon 页不得再使用旧语法 `collect --all`, got:\n%s", view)
	}
}

// TestDaemonPage_AutoStartHint daemon 页明确自启只影响下次登录/开机。
func TestDaemonPage_AutoStartHint(t *testing.T) {
	a := newAppForTest(&config.Config{}, &config.Config{}, nil)
	p := newDaemonPage(a)
	view := p.View()
	if !strings.Contains(view, "下次") {
		t.Errorf("daemon 页应说明自启只影响下次登录/开机, got:\n%s", view)
	}
}

// ---- footer 只展示有效按键 ----

// TestMainMenu_FooterListsQuestionHelp 主菜单 footer 列出 ? 帮助(已实现)。
func TestMainMenu_FooterListsQuestionHelp(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	view := m.View()
	if !strings.Contains(view, "?") {
		t.Errorf("主菜单 footer 应列出 ? 帮助(已实现), got:\n%s", view)
	}
}

// keyMsg 构造单字符 KeyMsg(测试 helper)。
func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// 帮助层包含 v Query views 键说明;q 退出语义保持。
func TestMainMenu_HelpContainsQueryViewsKey(t *testing.T) {
	overlay := helpOverlay()
	if !strings.Contains(overlay, "v") || !strings.Contains(overlay, "Query views") || !strings.Contains(overlay, "查询视图") {
		t.Errorf("帮助层应说明 v Query views:\n%s", overlay)
	}
}
