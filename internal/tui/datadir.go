package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// dataDirPage 数据目录只读说明页:解释固定 config 路径、迁移风险、config set 命令。
// data_dir 在 TUI 只读,本页不提供编辑入口(防止未停 daemon 即迁移、usage.db/logs 丢失)。
type dataDirPage struct {
	app *App
}

func newDataDirPage(app *App) *dataDirPage {
	return &dataDirPage{app: app}
}

func (p *dataDirPage) title() string { return ui.Bi("Data dir (read-only)", "数据目录(只读)") }
func (p *dataDirPage) Init() tea.Cmd { return nil }

// Update:只读页仅响应 esc/q 返回(无 commit、无编辑)。
func (p *dataDirPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	switch k.String() {
	case "esc", "q":
		p.app.pop()
	}
	return p, nil
}

// View 解释:当前 data_dir、固定 config 路径、迁移对象/风险、config set 命令、只读原因。
func (p *dataDirPage) View() string {
	s := ui.Bi("Data dir (read-only)", "数据目录(只读)") + "\n\n"
	s += "  " + ui.Bi("current data_dir", "当前 data_dir") + ": " + p.app.display.DataDir + "\n\n"
	s += ui.Bi("Config file path (fixed)", "配置文件路径(固定)") + ":\n"
	s += "  ~/.token-usage/config.toml\n"
	s += "  (" + ui.Bi("computed by config.ConfigPath(home), shared by TUI/CLI/configapp",
		"由 config.ConfigPath(home) 统一计算,TUI/CLI/configapp 共用") + ")\n\n"
	s += ui.Bi("data_dir is read-only in the TUI and cannot be changed on this page. Reasons",
		"data_dir 在 TUI 只读,无法在此页直接修改。原因") + ":\n"
	s += "  - " + ui.Bi("changing data_dir requires migrating usage.db and the logs/ directory",
		"data_dir 变更需要迁移 usage.db 与 logs/ 目录") + "\n"
	s += "  - " + ui.Bi("the daemon must be stopped before migration to avoid write conflicts and data loss",
		"迁移前必须先停止守护进程,避免写入冲突与数据丢失") + "\n"
	s += "  - " + ui.Bi("a failed migration may make historical usage data unavailable",
		"迁移失败可能导致历史用量数据不可用") + "\n\n"
	s += ui.Bi("To change data_dir, run on the command line (migration requires the --confirm-migrate confirmation)",
		"如需修改 data_dir,请在命令行执行(迁移需 --confirm-migrate 二次确认)") + ":\n"
	s += "  token-usage config set data_dir <path> --confirm-migrate\n\n"
	s += "  " + ui.Bi("esc Back", "esc 返回") + "\n"
	return s
}
