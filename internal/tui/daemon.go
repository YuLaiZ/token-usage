package tui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// daemonPage 守护进程页:开机自启 toggle + poll_interval textinput,cursor 切换聚焦。
// cursor=-1 → toggle 聚焦;cursor=0 → input 聚焦。
// feedback: commit 校验失败时展示的原因(View 渲染),空表示无错误。
type daemonPage struct {
	app      *App
	input    textinput.Model
	toggle   Toggle
	cursor   int // -1 = toggle 聚焦,0 = input 聚焦
	feedback string
}

func newDaemonPage(app *App) *daemonPage {
	ti := textinput.New()
	ti.SetValue(strconv.Itoa(app.draft.Daemon.PollInterval))
	ti.Placeholder = "30 (默认)"
	p := &daemonPage{
		app:    app,
		input:  ti,
		toggle: NewToggle("开机自启", app.draft.Daemon.AutoStart).SetFocus(true),
		cursor: -1, // 默认 toggle 聚焦
	}
	return p
}

func (p *daemonPage) title() string { return "守护进程" }
func (p *daemonPage) Init() tea.Cmd { return nil }

// setValue 设置 textinput 值(测试用)。
func (p *daemonPage) setValue(v string) { p.input.SetValue(v) }

func (p *daemonPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		// 非 KeyMsg 委托当前聚焦的 textinput(cursor=0 时)
		if p.cursor == 0 {
			m, cmd := p.input.Update(msg)
			p.input = m
			return p, cmd
		}
		return p, nil
	}
	switch k.String() {
	case "esc":
		// 校验失败时不 pop 页面,保留输入与原值,展示原因。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
		return p, nil
	case "up", "k":
		if p.cursor == 0 {
			p.cursor = -1
			p.toggle = p.toggle.SetFocus(true)
			p.input.Blur()
		}
		return p, nil
	case "down", "j":
		if p.cursor == -1 {
			p.cursor = 0
			p.toggle = p.toggle.SetFocus(false)
			p.input.Focus()
		}
		return p, nil
	case " ", "enter":
		if p.cursor == -1 {
			// toggle 聚焦:翻转开关
			p.toggle = p.toggle.Update(msg)
			return p, nil
		}
	}
	// 字符键委托 textinput(cursor=0 时)
	if p.cursor == 0 {
		m, cmd := p.input.Update(msg)
		p.input = m
		return p, cmd
	}
	return p, nil
}

// commit 把 input 值 Atoi 校验后写回 draft.Daemon.PollInterval + toggle 值写 AutoStart。
//
// 校验规则:poll_interval 必须为 0(默认值)或正整数;非数字/负数/溢出拒绝。
// 校验失败时返回 error、整体回滚(不写 PollInterval 也不写 AutoStart,保留旧值),
// 并设置 feedback 展示原因,保留输入(用户可修正)。成功时清空 feedback。
func (p *daemonPage) commit() error {
	n, err := strconv.Atoi(p.input.Value())
	if err != nil {
		p.feedback = "轮询间隔必须是整数(0 表示使用默认值)"
		return fmt.Errorf("daemon.poll_interval %q 非法: %w", p.input.Value(), err)
	}
	if n < 0 {
		p.feedback = "轮询间隔不能为负数(0 表示使用默认值)"
		return fmt.Errorf("daemon.poll_interval 不能为负数(当前 %d)", n)
	}
	// 校验通过:整体写入,清空错误反馈。
	p.app.draft.Daemon.PollInterval = n
	p.app.draft.Daemon.AutoStart = p.toggle.Value()
	p.feedback = ""
	return nil
}

func (p *daemonPage) View() string {
	s := "守护进程\n\n"
	s += "  " + p.toggle.View() + "   (空格切换)\n"
	s += "    轮询间隔(秒): " + p.input.View() + "\n"
	if p.feedback != "" {
		s += "\n  ⚠ " + p.feedback + "\n"
	}
	s += "\n  开机自启仅影响下次登录/开机,不会启动或停止当前正在运行的 daemon\n"
	s += "  首次使用请先 token-usage collect all 初始化历史数据，再开启本项\n"
	s += "\n  esc 应用到草稿并返回(主菜单 s 保存写盘)\n"
	return s
}
