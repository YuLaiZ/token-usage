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
