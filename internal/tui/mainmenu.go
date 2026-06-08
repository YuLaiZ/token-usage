package tui

import (
	"fmt"
	"sort"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

type mainMenu struct {
	app      *App
	items    []string
	cursor   int
	showHelp bool // ? 打开帮助 overlay(开时吞掉导航/enter,esc/? 切换关闭)
}

func newMainMenu(app *App) *mainMenu {
	return &mainMenu{
		app:   app,
		items: []string{"客户端", "路由中间件", "守护进程", "日志", "Provider 别名", "数据目录(只读)"},
	}
}

func (m *mainMenu) title() string { return "主菜单" }
func (m *mainMenu) Init() tea.Cmd { return nil }

func (m *mainMenu) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	// 帮助层开时:仅响应 esc / ? 切换关闭,吞掉其余按键(不导航/不进子页/不退出)。
	if m.showHelp {
		switch k.String() {
		case "esc", "?":
			m.showHelp = false
		}
		return m, nil
	}
	switch k.String() {
	case "?":
		m.showHelp = true
		return m, nil
	case "q", "esc":
		// dirty 退出经确认层(saving 保护/dirty 确认/clean 退出 由 handleExitKey 统一分流)。
		return m, m.app.handleExitKey(k)
	case "s":
		// no-op 分流:无 dirty 且无 syncPending 时不调 ApplyConfig,只显示提示。
		if m.app.saveNoOpHint() {
			return m, nil
		}
		return m, m.app.save()
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "enter":
		switch m.cursor {
		case 0:
			m.app.push(newClientsPage(m.app))
		case 1:
			m.app.push(newRouterPage(m.app))
		case 2:
			m.app.push(newDaemonPage(m.app))
		case 3:
			m.app.push(newLogPage(m.app))
		case 4:
			m.app.push(newAliasesPage(m.app))
		case 5:
			// 数据目录(只读):进入说明页(固定 config 路径 + 迁移风险 + config set 命令)。
			m.app.push(newDataDirPage(m.app))
		}
	}
	return m, nil
}

func (m *mainMenu) View() string {
	s := ""
	enabled, disabled := 0, 0
	for _, c := range m.app.draft.Clients {
		if c.Enabled {
			enabled++
		} else {
			disabled++
		}
	}
	// 主菜单摘要:优先取 draft(用户改动),零值再 fallback display(运行时层);
	// save 也不会刷新 display,故 draft 优先才能避免摘要 stale。defTag 基于 draft 是否零值。
	pi := m.app.draft.Daemon.PollInterval
	if pi == 0 {
		pi = m.app.display.Daemon.PollInterval
	}
	lv := m.app.draft.Log.Level
	if lv == "" {
		lv = m.app.display.Log.Level
	}
	if lv == "" {
		lv = "info"
	}
	summaries := []string{
		fmt.Sprintf("%d 启用 / %d 禁用", enabled, disabled),
		routerNames(m.app.draft),
		"轮询 " + strconv.Itoa(pi) + "s" + defTag(m.app.draft.Daemon.PollInterval == 0) +
			" · 自启 " + autoStartText(m.app.draft.Daemon.AutoStart),
		"level=" + lv + defTag(m.app.draft.Log.Level == ""),
		strconv.Itoa(len(m.app.draft.ProviderAliases)) + " 条映射",
		m.app.display.DataDir + " [只读]",
	}
	for i, item := range m.items {
		cur := "  "
		if i == m.cursor {
			cur = "▸ "
		}
		s += cur + pad(item, 16) + summaries[i] + "\n"
	}
	s += "\n  s 保存   q 退出   ? 帮助\n"
	if m.showHelp {
		s += "\n" + helpOverlay()
	}
	return s
}

// helpOverlay 返回帮助层文案:解释各按键、草稿/保存语义、data_dir 只读等。
func helpOverlay() string {
	return `==== 帮助 ====
  j/k 或 ↑/↓   上下移动选择
  enter        进入子页 / 数据目录说明页
  s            保存草稿到磁盘(config.toml)
  ?            打开/关闭本帮助层
  q / esc      退出(有未保存改动时进入确认层)

草稿与保存:
  子页 esc = 校验并应用到内存草稿后返回(不写盘)
  只有主菜单 s 才真正写入 config.toml
  标题栏 ⚠ 未保存改动 表示草稿与磁盘不一致

data_dir:
  数据目录在 TUI 只读,固定 config 路径见「数据目录(只读)」说明页
  修改需在命令行执行:
  token-usage config set data_dir <path> --confirm-migrate

按 esc 或 ? 关闭帮助层`
}

// routerNames 返回已声明路由名(逗号分隔),无则「无」
func routerNames(c *config.Config) string {
	if len(c.Routers) == 0 {
		return "无"
	}
	names := make([]string, 0, len(c.Routers))
	for n := range c.Routers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := names[0]
	for _, name := range names[1:] {
		out += ", " + name
	}
	return out
}

// ---- helpers ----
func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
func defTag(isDefault bool) string {
	if isDefault {
		return " (默认)"
	}
	return ""
}

// autoStartText 返回自启状态的中文显示。
func autoStartText(on bool) string {
	if on {
		return "开"
	}
	return "关"
}
