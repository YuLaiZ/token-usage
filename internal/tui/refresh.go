package tui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// refreshPage 查询刷新页:仪表盘自动刷新与 CLI watch 两个独立间隔。
// 两者都是查询侧配置,与守护进程页的采集轮询(poll_interval)互不影响。
// 交互与网页配置页的下拉合同一致(交接 2026-09-24):每项提供
// 10/20/30/60 秒及「其他…」五个选项,空格循环切换(logPage level 五态
// toggle 同模式);选「其他…」时以同行输入框填写 1~3600 整数秒(0 表示
// 使用默认值)。
//
// 草稿语义与网页端一致:页面加载把用户层原值(0=未配置)映射到有效值
// 落位显示(0→「30 (默认)」);未被用户触碰的项在 esc 提交时写回原值,
// 只有显式操作(循环切换/填写自定义)才写所选显式数字——旧配置无关保存
// 不会把隐式默认固化成显式 [refresh] 段。
type refreshPage struct {
	app *App
	// dashSel/watchSel 是当前选项下标:0=10,1=20,2=30,3=60,4=其他。
	dashSel, watchSel int
	// dashCustom/watchCustom 是「其他…」态的自定义秒数输入。
	dashCustom, watchCustom textinput.Model
	// dashOriginal/watchOriginal 记录进入页面时的用户层原值:
	// 未触碰的项提交时写回该值(0 保持 0,不产生任何写盘差异)。
	dashOriginal, watchOriginal int
	// dashTouched/watchTouched 标记用户是否显式操作过该项。
	dashTouched, watchTouched bool
	cursor                    int // 0 = 仪表盘间隔,1 = watch 间隔
	feedback                  string
}

// refreshPresets 是预设秒数(下标即 sel 0..3);refreshSelCount 含「其他…」。
var (
	refreshPresets  = [4]int{10, 20, 30, 60}
	refreshSelCount = 5
)

// selIndexFor 把有效间隔映射到选项下标:命中预设取对应下标,否则「其他…」。
func selIndexFor(effective int) int {
	for i, p := range refreshPresets {
		if p == effective {
			return i
		}
	}
	return refreshSelCount - 1
}

// selLabel 返回选项显示文案;「30」是产品默认,标注 (默认)。
func selLabel(idx int) string {
	if idx < len(refreshPresets) {
		v := refreshPresets[idx]
		if v == config.DefaultRefreshInterval {
			return strconv.Itoa(v) + " (" + ui.Bi("default", "默认") + ")"
		}
		return strconv.Itoa(v)
	}
	return ui.Bi("other…", "其他…")
}

func newRefreshInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "1-3600"
	return ti
}

func newRefreshPage(app *App) *refreshPage {
	dashRaw := app.draft.Refresh.DashboardInterval
	watchRaw := app.draft.Refresh.WatchInterval
	dashCustom := newRefreshInput()
	watchCustom := newRefreshInput()
	p := &refreshPage{
		app:           app,
		dashOriginal:  dashRaw,
		watchOriginal: watchRaw,
		dashCustom:    dashCustom,
		watchCustom:   watchCustom,
		cursor:        0,
	}
	// 原值按有效值落位:0(未配置)与 30 都落在「30 (默认)」预设;
	// 非预设值(含非法范围的手工值)落「其他…」并回填输入框。
	dashEff := config.RefreshConfig{}.EffectiveInterval(dashRaw)
	watchEff := config.RefreshConfig{}.EffectiveInterval(watchRaw)
	p.dashSel = selIndexFor(dashEff)
	p.watchSel = selIndexFor(watchEff)
	if p.dashSel == refreshSelCount-1 {
		p.dashCustom.SetValue(strconv.Itoa(dashRaw))
	}
	if p.watchSel == refreshSelCount-1 {
		p.watchCustom.SetValue(strconv.Itoa(watchRaw))
	}
	p.syncFocus()
	return p
}

func (p *refreshPage) title() string { return ui.Bi("Refresh", "查询刷新") }
func (p *refreshPage) Init() tea.Cmd { return nil }

// setDashSel / setWatchSel 设置选项下标(测试用)。
func (p *refreshPage) setDashSel(i int) { p.dashSel = i; p.dashTouched = true }
func (p *refreshPage) setWatchSel(i int) {
	p.watchSel = i
	p.watchTouched = true
}

// setCustom 设置指定项的自定义输入并标记触碰(测试用):kind 为 "dashboard" 或 "watch"。
func (p *refreshPage) setCustom(kind, v string) {
	if kind == "watch" {
		p.watchCustom.SetValue(v)
		p.watchTouched = true
		return
	}
	p.dashCustom.SetValue(v)
	p.dashTouched = true
}

// cycle 把当前 cursor 所指项循环切到下一选项(空格键):预设 → … → 其他… → 首个预设。
// 切到「其他…」时回填当前有效值便于微调;离开「其他…」清空自定义输入。
func (p *refreshPage) cycle() {
	if p.cursor == 0 {
		p.dashSel = (p.dashSel + 1) % refreshSelCount
		p.dashTouched = true
		if p.dashSel != refreshSelCount-1 {
			p.dashCustom.SetValue("")
		} else if p.dashCustom.Value() == "" {
			// 回填非零原值便于微调;0(默认)没有微调价值,留空由占位提示。
			if p.dashOriginal != 0 {
				p.dashCustom.SetValue(strconv.Itoa(p.dashOriginal))
			}
		}
		return
	}
	p.watchSel = (p.watchSel + 1) % refreshSelCount
	p.watchTouched = true
	if p.watchSel != refreshSelCount-1 {
		p.watchCustom.SetValue("")
	} else if p.watchCustom.Value() == "" && p.watchOriginal != 0 {
		p.watchCustom.SetValue(strconv.Itoa(p.watchOriginal))
	}
}

func (p *refreshPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		p.delegateInput(msg)
		return p, nil
	}
	switch k.String() {
	case "esc":
		// 校验失败时不 pop 页面,保留输入与选择,展示原因。
		if err := p.commit(); err != nil {
			return p, nil
		}
		p.app.pop()
		return p, nil
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down":
		if p.cursor < 1 {
			p.cursor++
		}
	case "k", "j":
		// 自定义输入聚焦时 k/j 属于文本,交给输入框。
		if p.customFocused() {
			p.delegateInput(msg)
			return p, nil
		}
		if k.String() == "j" && p.cursor < 1 {
			p.cursor++
		}
		if k.String() == "k" && p.cursor > 0 {
			p.cursor--
		}
	case " ", "enter":
		p.cycle()
	}
	p.syncFocus()
	p.delegateInput(msg)
	return p, nil
}

// customFocused 报告当前项是否处于「其他…」态(输入框可编辑)。
func (p *refreshPage) customFocused() bool {
	sel := p.dashSel
	if p.cursor == 1 {
		sel = p.watchSel
	}
	return sel == refreshSelCount-1
}

func (p *refreshPage) syncFocus() {
	// 只有「其他…」态的当前项有输入框;预设态无输入,不占用焦点。
	if p.cursor == 0 && p.dashSel == refreshSelCount-1 {
		_ = p.dashCustom.Focus()
	} else {
		p.dashCustom.Blur()
	}
	if p.cursor == 1 && p.watchSel == refreshSelCount-1 {
		_ = p.watchCustom.Focus()
	} else {
		p.watchCustom.Blur()
	}
}

// delegateInput 把消息交给当前项的自定义输入框(仅「其他…」态有内容)。
func (p *refreshPage) delegateInput(msg tea.Msg) {
	if p.cursor == 0 && p.dashSel == refreshSelCount-1 {
		m, _ := p.dashCustom.Update(msg)
		p.dashCustom = m
	} else if p.cursor == 1 && p.watchSel == refreshSelCount-1 {
		m, _ := p.watchCustom.Update(msg)
		p.watchCustom = m
	}
}

// parseInterval 校验自定义秒数:0(默认值)或 1~MaxRefreshInterval 的整数;
// 非数字/负数/超上限返回双语原因。与网页配置页「其他…」输入的校验口径一致
// (runtimecfg 写入链校验同域,保存被 configapp 双重拦截)。
func parseInterval(value string) (int, string) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, ui.Bi(
			"Refresh intervals must be whole seconds (0 means the default)",
			"刷新间隔必须是整数秒(0 表示使用默认值)",
		)
	}
	if n < 0 {
		return 0, ui.Bi(
			"Refresh intervals must not be negative (0 means the default)",
			"刷新间隔不能为负数(0 表示使用默认值)",
		)
	}
	if n > config.MaxRefreshInterval {
		return 0, ui.Bi(
			fmt.Sprintf("Refresh intervals must be 0 or 1-%d seconds (got %d)", config.MaxRefreshInterval, n),
			fmt.Sprintf("刷新间隔应为 0 或 1~%d 秒(当前 %d)", config.MaxRefreshInterval, n),
		)
	}
	return n, ""
}

// selectionValue 解析指定项当前选择的写入值:
// 预设 → 显式秒数;「其他…」→ 输入框整数(0 或 1~3600,非法返回原因)。
func (p *refreshPage) selectionValue(kind string) (int, string) {
	sel, input := p.dashSel, p.dashCustom
	if kind == "watch" {
		sel, input = p.watchSel, p.watchCustom
	}
	if sel < len(refreshPresets) {
		return refreshPresets[sel], ""
	}
	return parseInterval(input.Value())
}

// commit 把两项选择写回 draft.Refresh。校验失败时不写任何一项(整体回滚,
// 保留旧值与选择)并设置 feedback;成功时清空 feedback。
// 未被用户触碰的项写回进入页面时的原值:旧配置(无 [refresh] 段,原值 0)
// 只看不改,提交后草稿仍是 0,不会把隐式默认固化成显式配置。
func (p *refreshPage) commit() error {
	dash := p.dashOriginal
	watch := p.watchOriginal
	if p.dashTouched {
		v, why := p.selectionValue("dashboard")
		if why != "" {
			p.feedback = why
			return fmt.Errorf("%s: %s", ui.Bi("refresh.dashboard_interval invalid", "refresh.dashboard_interval 非法"), why)
		}
		dash = v
	}
	if p.watchTouched {
		v, why := p.selectionValue("watch")
		if why != "" {
			p.feedback = why
			return fmt.Errorf("%s: %s", ui.Bi("refresh.watch_interval invalid", "refresh.watch_interval 非法"), why)
		}
		watch = v
	}
	p.app.draft.Refresh.DashboardInterval = dash
	p.app.draft.Refresh.WatchInterval = watch
	p.feedback = ""
	return nil
}

func (p *refreshPage) View() string {
	dashVal := selLabel(p.dashSel)
	watchVal := selLabel(p.watchSel)
	if p.dashSel == refreshSelCount-1 {
		dashVal = ui.Bi("other: ", "其他: ") + p.dashCustom.View()
	}
	if p.watchSel == refreshSelCount-1 {
		watchVal = ui.Bi("other: ", "其他: ") + p.watchCustom.View()
	}
	dashMark, watchMark := "  ", "  "
	if p.cursor == 0 {
		dashMark = "▸ "
	} else {
		watchMark = "▸ "
	}
	s := ui.Bi("Refresh", "查询刷新") + "\n\n"
	s += dashMark + ui.Bi("Dashboard auto refresh (s): ", "仪表盘自动刷新(秒): ") + dashVal + "\n"
	s += watchMark + ui.Bi("CLI watch refresh (s): ", "CLI watch 刷新(秒): ") + watchVal + "\n"
	s += "\n  " + ui.Bi(
		"space cycles 10 / 20 / 30 / 60 / other; \"other\" takes 1-3600 seconds (0 means the default 30s)",
		"空格循环切换 10 / 20 / 30 / 60 / 其他;「其他」输入 1~3600 秒(0 表示使用默认 30 秒)") + "\n"
	if p.feedback != "" {
		s += "\n  ⚠ " + p.feedback + "\n"
	}
	s += "\n  " + ui.Bi(
		"These two query refresh intervals are independent of daemon.poll_interval (SQLite collection polling); changing them does not restart the daemon",
		"这两项查询刷新间隔与 daemon.poll_interval(SQLite 采集轮询)互相独立;修改不会重启守护进程") + "\n"
	s += "  " + ui.Bi(
		"watch --interval overrides watch_interval for a single run",
		"watch --interval 可在单次运行时覆盖 watch_interval") + "\n"
	s += "\n  " + ui.Bi("esc Apply to draft and return (main-menu s saves to disk)",
		"esc 应用到草稿并返回(主菜单 s 保存写盘)") + "\n"
	return s
}
