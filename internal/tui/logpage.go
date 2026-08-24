package tui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// logPage 日志页:level 五态 toggle(空/info/debug/warn/error)+ dir textinput + max_days textinput(数字)。
// cursor: -1=level, 0=dir, 1=max_days。
// feedback: commit 校验失败时展示的原因(View 渲染),空表示无错误。
type logPage struct {
	app          *App
	level        string
	dirInput     textinput.Model
	maxDaysInput textinput.Model
	cursor       int
	feedback     string
}

func newLogPage(app *App) *logPage {
	dir := textinput.New()
	dir.SetValue(app.draft.Log.Dir)
	dir.Placeholder = "<data_dir>/logs (" + ui.Bi("default", "默认") + ")"
	max := textinput.New()
	max.SetValue(strconv.Itoa(app.draft.Log.MaxDays))
	max.Placeholder = "7 (" + ui.Bi("default", "默认") + ")"
	return &logPage{
		app:          app,
		level:        app.draft.Log.Level,
		dirInput:     dir,
		maxDaysInput: max,
		cursor:       -1,
	}
}

func (p *logPage) title() string { return ui.Bi("Logs", "日志") }
func (p *logPage) Init() tea.Cmd { return nil }

// setDir / setMaxDays 设置对应 textinput 值(测试用)。
func (p *logPage) setDir(v string)     { p.dirInput.SetValue(v) }
func (p *logPage) setMaxDays(v string) { p.maxDaysInput.SetValue(v) }

// toggleLevel 五态循环: "" → info → debug → warn → error → ""(空格切换)。
// "" 代表用户层空值(运行时回落到 info);覆盖 registry 全部注册 level(含 warn/error)。
func (p *logPage) toggleLevel() {
	switch p.level {
	case "":
		p.level = "info"
	case "info":
		p.level = "debug"
	case "debug":
		p.level = "warn"
	case "warn":
		p.level = "error"
	default: // error(或任何非预期值)→ 回空
		p.level = ""
	}
}

func (p *logPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p.delegateInput(msg), nil
	}
	switch k.String() {
	case "esc":
		// 校验失败时不 pop 页面,保留输入与原值,展示原因。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
	case "up":
		if p.cursor > -1 {
			p.cursor--
		}
	case "down":
		if p.cursor < 1 {
			p.cursor++
		}
	case "k":
		if p.cursor >= 0 {
			return p.delegateInput(msg), nil
		}
	case "j":
		if p.cursor >= 0 {
			return p.delegateInput(msg), nil
		}
		if p.cursor < 1 {
			p.cursor++
		}
	case " ":
		if p.cursor == -1 {
			p.toggleLevel()
		}
	}
	p.syncFocus()
	return p.delegateInput(msg), nil
}

func (p *logPage) syncFocus() {
	if p.cursor == 0 {
		_ = p.dirInput.Focus()
	} else {
		p.dirInput.Blur()
	}
	if p.cursor == 1 {
		_ = p.maxDaysInput.Focus()
	} else {
		p.maxDaysInput.Blur()
	}
}

// delegateInput 把消息交给当前聚焦的 textinput(cursor -1=level 不委托)。
func (p *logPage) delegateInput(msg tea.Msg) *logPage {
	switch p.cursor {
	case 0:
		m, _ := p.dirInput.Update(msg)
		p.dirInput = m
	case 1:
		m, _ := p.maxDaysInput.Update(msg)
		p.maxDaysInput = m
	}
	return p
}

// commit 把 level/dir/max_days 写回 draft.Log。
//
// 校验规则:
//   - max_days 必须为 0(默认值)或正整数;非数字/负数/溢出拒绝;
//   - level 必须为注册 level(runtimecfg.RegisteredLogLevels,空值合法代表默认)。
//
// 校验失败时返回 error、整体回滚(不写任何字段,保留旧值),
// 并设置 feedback 展示原因,保留输入(用户可修正)。成功时清空 feedback。
// Log 是值类型(LogConfig),cloneConfig 浅拷贝对齐,标量 commit 无 nil/empty 问题。
func (p *logPage) commit() error {
	n, err := strconv.Atoi(p.maxDaysInput.Value())
	if err != nil {
		p.feedback = ui.Bi(
			"max_days must be an integer (0 means the default)",
			"max_days 必须是整数(0 表示使用默认值)",
		)
		return fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("log.max_days %q invalid", p.maxDaysInput.Value()),
			fmt.Sprintf("log.max_days %q 非法", p.maxDaysInput.Value()),
		), err)
	}
	if n < 0 {
		p.feedback = ui.Bi(
			"max_days must not be negative (0 means the default)",
			"max_days 不能为负数(0 表示使用默认值)",
		)
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("log.max_days must not be negative (got %d)", n),
			fmt.Sprintf("log.max_days 不能为负数(当前 %d)", n),
		))
	}
	if !isRegisteredLogLevel(p.level) {
		p.feedback = ui.Bi(
			"Unknown log level (press space to cycle: default/info/debug/warn/error)",
			"未知的日志级别(空格切换可选: 默认/info/debug/warn/error)",
		)
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("log.level %q is not registered (supported: %v)", p.level, runtimecfg.RegisteredLogLevels()),
			fmt.Sprintf("log.level %q 未注册(受支持: %v)", p.level, runtimecfg.RegisteredLogLevels()),
		))
	}
	// 校验通过:整体写入,清空错误反馈。
	p.app.draft.Log.Level = p.level
	p.app.draft.Log.Dir = p.dirInput.Value()
	p.app.draft.Log.MaxDays = n
	p.feedback = ""
	return nil
}

func (p *logPage) View() string {
	levelView := p.level
	if levelView == "" {
		levelView = "(" + ui.Bi("default info", "默认 info") + ")"
	}
	s := ui.Bi("Logs", "日志") + "\n\n"
	lc := "  "
	if p.cursor == -1 {
		lc = "▸ "
	}
	s += lc + "level: " + levelView + "   (" + ui.Bi("space toggles", "空格切换") + ")\n"
	dc := "  "
	if p.cursor == 0 {
		dc = "▸ "
	}
	s += dc + pad("dir", 10) + p.dirInput.View() + "\n"
	mc := "  "
	if p.cursor == 1 {
		mc = "▸ "
	}
	s += mc + pad("max_days", 10) + p.maxDaysInput.View() + "\n"
	if p.feedback != "" {
		s += "\n  ⚠ " + p.feedback + "\n"
	}
	s += "\n  " + ui.Bi("esc Apply to draft and return (main-menu s saves to disk)",
		"esc 应用到草稿并返回(主菜单 s 保存写盘)") + "\n"
	return s
}

// isRegisteredLogLevel 报告 lv 是否为注册 level(空值合法)。
// 委托 runtimecfg 共享 registry(单一来源),避免 TUI 自维护副本漂移。
func isRegisteredLogLevel(lv string) bool {
	for _, registered := range runtimecfg.RegisteredLogLevels() {
		if lv == registered {
			return true
		}
	}
	return lv == ""
}
