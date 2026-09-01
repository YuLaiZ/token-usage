package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// outputColumnsPage 是全局输出指标布局编辑页(复用有序多选交互):
//   - 编辑态:候选为「默认七列后接 cache_create」;space 显示/隐藏、[ ] 调序、
//     enter 提交(至少一项)、esc 放弃未提交编辑;界面显示双语标签,写盘用稳定 ID;
//     d 立即删除 query.output、丢弃未提交选择并返回上层(恢复默认布局)。
//   - 恢复态:顶层 raw 问题(与 Views 共同前置)或 query.output 自身错误
//     (非表/未知子键/非数组/非法元素);恢复动作统一删除整张 query.output
//     表,执行后按局部解析重算——无关的坏视图定义不锁死本页。
type outputColumnsPage struct {
	app      *App
	recovery *recoveryState
	sel      *orderedSelect
	errMsg   string
}

func newOutputColumnsPage(app *App) *outputColumnsPage {
	p := &outputColumnsPage{app: app}
	p.evaluate()
	return p
}

// evaluate 重新执行局部解析,决定编辑态/恢复态:
// 顶层问题态给共同恢复项;否则 ParseOutputLayout 的诊断逐条列恢复项;
// 合法时构造以当前有效布局为初始选择的有序多选。
func (p *outputColumnsPage) evaluate() {
	if p.app.query == nil {
		p.recovery = nil
		p.newEditor(ui.DefaultOutputColumns())
		return
	}
	if items := topLevelRecoveryItems(p.app.draft); len(items) > 0 {
		p.recovery = &recoveryState{items: items}
		return
	}
	cols, err := p.app.query.OutputLayout(p.app.draft)
	if err != nil {
		var items []*recoveryItem
		for _, d := range diagnosticsOf(err) {
			items = append(items, &recoveryItem{
				desc: d.Message,
				action: ui.Bi(
					"delete the whole query.output table and set it up again via this page",
					"删除整张 query.output 表后按本页引导重新设置"),
				apply: func(app *App) {
					deleteQueryOutput(app.draft)
				},
			})
		}
		if len(items) == 0 {
			items = append(items, &recoveryItem{
				desc:   err.Error(),
				action: ui.Bi("delete the whole query.output table and set it up again via this page", "删除整张 query.output 表后按本页引导重新设置"),
				apply:  func(app *App) { deleteQueryOutput(app.draft) },
			})
		}
		p.recovery = &recoveryState{items: items}
		if p.recovery.cursor >= len(items) {
			p.recovery.cursor = len(items) - 1
		}
		return
	}
	p.recovery = nil
	p.newEditor(cols)
}

// newEditor 以给定初始选择构造有序多选(候选固定,双语标签渲染)。
func (p *outputColumnsPage) newEditor(initial []string) {
	p.sel = newOrderedSelect(ui.OutputMetricIDs(), initial, ui.Bi("Output columns", "输出列"))
	p.sel.SetLabelResolver(ui.OutputMetricLabel)
	p.errMsg = ""
}

func (p *outputColumnsPage) title() string { return ui.Bi("Output columns", "输出列") }
func (p *outputColumnsPage) Init() tea.Cmd { return nil }

func (p *outputColumnsPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	if p.recovery != nil {
		return p.updateRecovery(k)
	}
	// 页面级 reset 在转交 orderedSelect 前处理:立即删除 output 表、
	// 丢弃本次尚未提交选择并返回上层(不写入显式默认数组)。
	if k.String() == "d" {
		deleteQueryOutput(p.app.draft)
		p.app.pop()
		return p, nil
	}
	p.sel.handleKeys(k)
	switch p.sel.Done() {
	case selectCancelled:
		p.app.pop()
	case selectSubmitted:
		cols := p.sel.Selection()
		if len(cols) == 0 {
			p.errMsg = ui.Bi(
				"keep at least one output column",
				"至少保留一个输出列",
			)
			p.sel.done = selectPending
			return p, nil
		}
		setQueryOutputColumns(p.app.draft, cols)
		p.app.pop()
	}
	return p, nil
}

// updateRecovery 恢复态键盘合同(与 Views 一致):↑/k ↓/j 移动、Enter 执行
// 当前项修复、Esc 返回 Query 父页并保留 draft。
func (p *outputColumnsPage) updateRecovery(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.recovery.cursor > 0 {
			p.recovery.cursor--
		}
	case "down", "j":
		if p.recovery.cursor < len(p.recovery.items)-1 {
			p.recovery.cursor++
		}
	case "enter":
		item := p.recovery.items[p.recovery.cursor]
		if item == nil || item.apply == nil {
			return p, nil
		}
		item.apply(p.app)
		p.evaluate()
	}
	return p, nil
}

func (p *outputColumnsPage) View() string {
	if p.recovery != nil {
		var b strings.Builder
		b.WriteString(ui.Bi("Output columns - recovery", "输出列 - 恢复") + "\n\n")
		b.WriteString(ui.Bi(
			"The query.output config has errors; fix each item below (enter), then the guided editor unlocks. Unrelated view definition errors do not block this page:",
			"query.output 配置存在错误;逐项修复(enter)后进入引导编辑页。无关的视图定义错误不阻断本页:") + "\n\n")
		for i, item := range p.recovery.items {
			cursor := "  "
			if i == p.recovery.cursor {
				cursor = "▸ "
			}
			b.WriteString(cursor + item.desc + "\n      " + ui.Bi("enter:", "enter:") + " " + item.action + "\n")
		}
		b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " +
			ui.Bi("enter Fix selected item", "enter 修复选中项") + "   " + ui.Bi("esc Back to Query (draft kept)", "esc 返回 Query(保留草稿)") + "\n")
		return b.String()
	}
	s := p.sel.View()
	if p.errMsg != "" {
		s += "\n  " + p.errMsg + "\n"
	}
	s += "\n  " + ui.Bi("d Reset to default (delete query.output)", "d 恢复默认(删除 query.output)") + "\n"
	return s
}
