package tui

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestMainMenu_DaemonSummaryShowsAutoStart(t *testing.T) {
	edit := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{PollInterval: 30, AutoStart: true}}
	a := newAppForTest(edit, edit, nil)
	m := newMainMenu(a)
	view := m.View()
	if !strings.Contains(view, "自启") {
		t.Errorf("守护进程摘要应含「自启」,实际:\n%s", view)
	}
	if !strings.Contains(view, "开") {
		t.Errorf("AutoStart=true 应显示「开」,实际:\n%s", view)
	}
}

func TestMainMenu_DaemonSummaryAutoStartOff(t *testing.T) {
	edit := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{PollInterval: 30, AutoStart: false}}
	a := newAppForTest(edit, edit, nil)
	m := newMainMenu(a)
	view := m.View()
	if !strings.Contains(view, "关") {
		t.Errorf("AutoStart=false 应显示「关」,实际:\n%s", view)
	}
}

func TestRouterNamesSorted(t *testing.T) {
	cfg := &config.Config{Routers: map[string]config.RouterConfig{
		"z_router": {},
		"a_router": {},
	}}
	if got := routerNames(cfg); got != "a_router, z_router" {
		t.Errorf("router 摘要应稳定排序，got %q", got)
	}
}

// 主菜单 v 进入 Query 父页(而非直接进入 Views);enter 选中 Query 项同样进入。
func TestMainMenu_VOpensQueryParent(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := a.stack[0].(*mainMenu)
	m.Update(queryTestKeyMsg("v"))
	if len(a.stack) != 2 {
		t.Fatalf("v 应进入 Query 父页,栈深 %d", len(a.stack))
	}
	if _, ok := a.stack[1].(*queryParentPage); !ok {
		t.Fatalf("栈顶应为 queryParentPage,实际 %T", a.stack[1])
	}
	// enter 光标选中 Query 项(索引 4)同样进入。
	a2 := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m2 := a2.stack[0].(*mainMenu)
	for i := 0; i < 4; i++ {
		m2.Update(queryTestKeyMsg("down"))
	}
	m2.Update(queryTestKeyMsg("enter"))
	if _, ok := a2.stack[len(a2.stack)-1].(*queryParentPage); !ok {
		t.Fatalf("enter 应进入 Query 父页,实际 %T", a2.stack[len(a2.stack)-1])
	}
}

// 主菜单 Query 摘要行:未配置显示 client(默认)+alias 数,配置后显示默认视图名。
func TestMainMenu_QuerySummaryLine(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	view := a.stack[0].(*mainMenu).View()
	if !strings.Contains(view, "Query") || !strings.Contains(view, "查询") {
		t.Errorf("主菜单应含 Query 项:\n%s", view)
	}
	if !strings.Contains(view, "client") {
		t.Errorf("未配置时摘要应显示默认 client:\n%s", view)
	}

	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,mpc"},
	}}
	a2 := newAppForTest(draft, draft, nil)
	view2 := a2.stack[0].(*mainMenu).View()
	if !strings.Contains(view2, "group_q") {
		t.Errorf("配置后摘要应显示默认视图名 group_q:\n%s", view2)
	}
}
