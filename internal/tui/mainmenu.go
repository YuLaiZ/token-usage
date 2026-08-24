package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// menuColWidth 主菜单项列宽:按双语化后最长项
// "Data dir (read-only) / 数据目录(只读)"(显示宽度 37)取整加余量。
const menuColWidth = 40

type mainMenu struct {
	app      *App
	items    []string
	cursor   int
	showHelp bool // ? 打开帮助 overlay(开时吞掉导航/enter,esc/? 切换关闭)
}

func newMainMenu(app *App) *mainMenu {
	return &mainMenu{
		app: app,
		items: []string{
			ui.Bi("Clients", "客户端"),
			ui.Bi("Routers", "路由中间件"),
			ui.Bi("Daemon", "守护进程"),
			ui.Bi("Logs", "日志"),
			ui.Bi("Provider aliases", "Provider 别名"),
			ui.Bi("Data dir (read-only)", "数据目录(只读)"),
		},
	}
}

func (m *mainMenu) title() string { return ui.Bi("Main menu", "主菜单") }
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
		ui.Bi(
			fmt.Sprintf("%d enabled / %d disabled", enabled, disabled),
			fmt.Sprintf("%d 启用 / %d 禁用", enabled, disabled),
		),
		routerNames(m.app.draft),
		ui.Bi(
			"poll "+strconv.Itoa(pi)+"s"+defTagEn(m.app.draft.Daemon.PollInterval == 0)+
				" · auto-start "+autoStartTextEn(m.app.draft.Daemon.AutoStart),
			"轮询 "+strconv.Itoa(pi)+"s"+defTagZh(m.app.draft.Daemon.PollInterval == 0)+
				" · 自启 "+autoStartTextZh(m.app.draft.Daemon.AutoStart),
		),
		ui.Bi(
			"level="+lv+defTagEn(m.app.draft.Log.Level == ""),
			"level="+lv+defTagZh(m.app.draft.Log.Level == ""),
		),
		ui.Bi(
			fmt.Sprintf("%d mappings", len(m.app.draft.ProviderAliases)),
			fmt.Sprintf("%d 条映射", len(m.app.draft.ProviderAliases)),
		),
		m.app.display.DataDir + " [" + ui.Bi("read-only", "只读") + "]",
	}
	for i, item := range m.items {
		cur := "  "
		if i == m.cursor {
			cur = "▸ "
		}
		s += cur + pad(item, menuColWidth) + summaries[i] + "\n"
	}
	s += "\n  s " + ui.Bi("Save", "保存") + "   q " + ui.Bi("Quit", "退出") + "   ? " + ui.Bi("Help", "帮助") + "\n"
	if m.showHelp {
		s += "\n" + helpOverlay()
	}
	return s
}

// helpOverlay 返回帮助层文案:解释各按键、草稿/保存语义、data_dir 只读等。
// 逐行双语:说明列统一「English / 中文」,中文原文一字不改保留在中文侧。
func helpOverlay() string {
	return `==== ` + ui.Bi("Help", "帮助") + ` ====
  j/k 或 ↑/↓   ` + ui.Bi("Move up/down", "上下移动选择") + `
  enter        ` + ui.Bi("Open subpage / data-dir page", "进入子页 / 数据目录说明页") + `
  s            ` + ui.Bi("Save draft to disk (config.toml)", "保存草稿到磁盘(config.toml)") + `
  ?            ` + ui.Bi("Toggle this help", "打开/关闭本帮助层") + `
  q / esc      ` + ui.Bi("Quit (confirm layer if unsaved changes)", "退出(有未保存改动时进入确认层)") + `

` + ui.Bi("Draft and save", "草稿与保存") + `:
  ` + ui.Bi("esc in a subpage validates and applies to the in-memory draft, then returns (no disk write)",
		"子页 esc = 校验并应用到内存草稿后返回(不写盘)") + `
  ` + ui.Bi("Only s on the main menu actually writes config.toml", "只有主菜单 s 才真正写入 config.toml") + `
  ` + ui.Bi("⚠ Unsaved changes in the title bar means the draft differs from disk",
		"标题栏 ⚠ 未保存改动 表示草稿与磁盘不一致") + `

data_dir:
  ` + ui.Bi(`The data directory is read-only in the TUI; see the "Data dir (read-only)" page for the fixed config path`,
		`数据目录在 TUI 只读,固定 config 路径见「数据目录(只读)」说明页`) + `
  ` + ui.Bi("To change it, run on the command line", "修改需在命令行执行") + `:
  token-usage config set data_dir <path> --confirm-migrate

` + ui.Bi("Press esc or ? to close this help", "按 esc 或 ? 关闭帮助层")
}

// routerNames 返回已声明路由名(逗号分隔),无则「None / 无」
func routerNames(c *config.Config) string {
	if len(c.Routers) == 0 {
		return ui.Bi("None", "无")
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

// pad 按显示宽度补空格到 n 列:runewidth.StringWidth 为显示宽度真相源
// (中文占 2 列,按字节 len 补齐必然错位)。
func pad(s string, n int) string {
	w := runewidth.StringWidth(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// defTagEn/defTagZh 返回默认值标注,供摘要行英文/中文两侧各自拼接。
func defTagEn(isDefault bool) string {
	if isDefault {
		return " (default)"
	}
	return ""
}
func defTagZh(isDefault bool) string {
	if isDefault {
		return " (默认)"
	}
	return ""
}

// autoStartTextEn/autoStartTextZh 返回自启状态的英文/中文显示。
func autoStartTextEn(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
func autoStartTextZh(on bool) string {
	if on {
		return "开"
	}
	return "关"
}
