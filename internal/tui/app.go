package tui

import (
	"errors"
	"fmt"
	"reflect"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
)

// ApplyFunc 是 TUI 保存的唯一依赖:把 (expectedRevision, currentUser 草稿) 应用到磁盘。
// 生产包装 configapp.Application.ApplyConfig(固定 confirmDataDirMigration=false,
// data_dir 在 TUI 只读);测试注入 fake 锁定 generation/revision/syncPending 行为。
// 不携带 ctx:bubbletea 的 tea.Cmd 在后台 goroutine 求值,无请求级 context;
// adapter 在构造时绑定程序级 context(见 config_tui.go)。
type ApplyFunc func(
	expectedRevision []byte,
	currentUser *config.Config,
) (configapp.ApplyConfigResult, error)

// App 是 TUI 顶层 model:持 draft/diskBaseline 双模型 + 页面栈 + ApplyFunc 保存回调。
type App struct {
	draft          *config.Config // 当前编辑草稿(用户层, marshal 对象)
	diskBaseline   *config.Config // 磁盘基线(最近一次成功保存的 snapshot), dirty 检测基准
	diskRevision   []byte         // 磁盘 revision(expectedRevision 来源)
	display        *config.Config // 运行时层(显示参考, 只读)
	apply          ApplyFunc      // 保存回调: 应用草稿到磁盘(configapp.ApplyConfig)
	statusMsg      string         // 保存成功/失败/提示(View 显示)
	saving         bool           // 串行化保存标志: 保存进行中拒绝新 save
	saveGeneration uint64         // 保存代次: 过期 generation 结果不得覆盖较新基线
	syncPending    bool           // 自启同步待重试(上次保存 AutoStart.Err!=nil)
	quitAfterSave  bool           // 保存并退出:保存完成后若干净成功则 tea.Quit
	confirmQuit    bool           // dirty 退出确认层:拦截 q/esc/ctrl+c,提供放弃/保存/返回

	stack         []page
	width, height int
}

type page interface {
	tea.Model
	title() string
}

// committer 是可「应用到草稿」的页面契约:esc 试图离开子页前调用 commit。
// 返回 error(校验失败)时,调用方不得 pop 页面,保留输入与原值,展示校验原因。
// 不实现 committer 的页面(只读页/列表页)esc 直接 pop,无校验拦截。
type committer interface {
	commit() error
}

// noChangesMsg 无 dirty 且无 syncPending 时按 s 的提示。
const noChangesMsg = "没有待保存的更改"

// saveMsg 保存完成消息(由 save() 返回的 tea.Cmd 产生, App.Update 处理)。
type saveMsg struct {
	saved      bool                        // ConfigApplied(配置已对应磁盘)
	snapshot   *config.Config              // 冻结的保存时 snapshot(成功后设为磁盘基线)
	generation uint64                      // 保存代次(过期结果被 handleSaveMsg 丢弃)
	err        error                       // ApplyConfig 返回的 error(含 ErrConfigChangedExternally/PartialErrors)
	result     configapp.ApplyConfigResult // ApplyConfig 结构化结果
}

// saveSkippedMsg 串行化保护:保存进行中再触发 save 时产生,不执行实际保存。
type saveSkippedMsg struct{}

// Run 启动 TUI。apply 由 cli 层用同一个 configapp.Application.ApplyConfig 构造
// (data_dir 在 TUI 只读, 固定 confirmDataDirMigration=false)。draft/display 为
// 双模型(用户层 + 运行时层);diskRevision 为进入时的磁盘 revision。
// 进入 TUI 时 draft/display/diskRevision 必须来自同一次 snapshot 读取。
func Run(draft, display *config.Config, diskRevision []byte, apply ApplyFunc) error {
	if draft == nil {
		return errors.New("TUI 用户配置不能为空")
	}
	if display == nil {
		return errors.New("TUI 有效配置不能为空")
	}
	if apply == nil {
		return errors.New("TUI 保存回调不能为空")
	}
	a := &App{
		draft:        draft,
		diskBaseline: cloneConfig(draft),
		diskRevision: cloneBytes(diskRevision),
		display:      display,
		apply:        apply,
	}
	a.stack = []page{newMainMenu(a)}
	p := tea.NewProgram(a, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// newAppForTest 测试用构造(不启动真实 TUI)。apply 可为 nil(测试按需注入)。
func newAppForTest(draft, display *config.Config, apply ApplyFunc) *App {
	a := &App{
		draft:        draft,
		diskBaseline: cloneConfig(draft),
		diskRevision: []byte("rev-test"),
		display:      display,
		apply:        apply,
	}
	a.stack = []page{newMainMenu(a)}
	return a
}

func (a *App) Init() tea.Cmd { return nil }

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		return a, nil
	case saveMsg:
		return a, a.handleSaveMsg(m)
	case saveSkippedMsg:
		a.statusMsg = "正在保存,请稍候"
		return a, nil
	case tea.KeyMsg:
		// 确认层优先:拦截所有按键(d=放弃退出 / s=保存并退出 / esc或其他=返回编辑)。
		if a.confirmQuit {
			return a, a.handleConfirmQuitKey(m)
		}
		// ctrl+c 全局处理(任何页面),dirty 进确认层不绕过。
		if m.String() == "ctrl+c" {
			return a, a.handleExitKey(m)
		}
	}
	if len(a.stack) == 0 {
		return a, nil
	}
	top := a.stack[len(a.stack)-1]
	updated, cmd := top.Update(msg)
	if len(a.stack) == 0 || a.stack[len(a.stack)-1] != top {
		return a, cmd
	}
	a.stack[len(a.stack)-1] = updated.(page)
	return a, cmd
}

// handleExitKey 处理退出键(q / esc / ctrl+c)的 dirty 确认分流。
// saving=true → 提示保存进行中,不退出。dirty → 进确认层。clean → tea.Quit。
func (a *App) handleExitKey(m tea.KeyMsg) tea.Cmd {
	if a.saving {
		a.statusMsg = "保存进行中,请稍候"
		return nil
	}
	if a.dirty() {
		a.confirmQuit = true
		return nil
	}
	return tea.Quit
}

// handleConfirmQuitKey 处理确认层按键:
//   - d: 放弃并退出 → tea.Quit(不保存)。saving=true 时阻止(保存进行中不得退出)。
//   - s: 保存并退出 → 设 quitAfterSave=true 并启动 save();不立刻 Quit。
//   - esc / 其他: 返回编辑 → 清 confirmQuit,留编辑器。
func (a *App) handleConfirmQuitKey(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "d":
		// saving 保护:保存进行中即便确认层「放弃」也不得退出。
		if a.saving {
			a.statusMsg = "保存进行中,请稍候"
			return nil
		}
		// 放弃并退出:直接退出,不保存。
		a.confirmQuit = false
		a.quitAfterSave = false
		return tea.Quit
	case "s":
		// 保存并退出:设 quitAfterSave, 退出确认层, 启动保存(完成时由 handleSaveMsg 决定是否 Quit)。
		// saving=true 时 save() 返回 saveSkippedMsg(串行化),quitAfterSave 已置位待当前保存完成。
		a.confirmQuit = false
		a.quitAfterSave = true
		return a.save()
	default:
		// esc / 其他:返回编辑。
		a.confirmQuit = false
		return nil
	}
}

// handleSaveMsg 处理保存结果并返回 tea.Cmd(quitAfterSave 干净成功时返回 tea.Quit)。
// 核心不变量:
//   - 过期 generation(stale)直接丢弃, 不覆盖较新基线或提示。
//   - ConfigApplied=true: 用 snapshot + NewRevision 推进磁盘基线; draft==snapshot 清 dirty。
//   - ConfigApplied=false: 基线/revision 不变, draft 保持 dirty。
//   - AutoStart.Err!=nil(自启同步失败): syncPending=true; 否则 syncPending=false。
//   - revision conflict(ErrConfigChangedExternally): 保留 draft 和 dirty, 提示重新载入。
//   - 无论结果如何 saving=false(避免一次失败后永久无法保存)。
//   - quitAfterSave 衔接: 仅 ConfigApplied=true 且无 PartialErrors(干净成功)且 quitAfterSave=true
//     才返回 tea.Quit;否则清 quitAfterSave 留编辑器(部分失败保留 syncPending 供重试)。
func (a *App) handleSaveMsg(sm saveMsg) tea.Cmd {
	// generation 守卫: 过期结果不得覆盖较新基线或提示。
	if sm.generation != a.saveGeneration {
		return nil
	}
	a.saving = false

	// revision conflict: 配置已被其他进程修改。保留 draft 和 dirty, 提示重新载入。
	if errors.Is(sm.err, configapp.ErrConfigChangedExternally) {
		a.statusMsg = "配置已被其他进程修改，请退出后重新进入以载入最新配置（TUI 不会自动覆盖外部改动）"
		a.quitAfterSave = false
		return nil
	}

	if !sm.result.ConfigApplied {
		// 写入前/校验/revision 失败: 基线/revision 不变, draft 保持 dirty。
		msg := "保存失败"
		if sm.err != nil {
			msg = "保存失败:" + sm.err.Error()
		}
		a.statusMsg = msg
		a.quitAfterSave = false
		return nil
	}

	// ConfigApplied=true: 用 snapshot + NewRevision 推进磁盘基线。
	if sm.snapshot != nil {
		a.diskBaseline = cloneConfig(sm.snapshot)
	}
	if sm.result.NewRevision != nil {
		a.diskRevision = cloneBytes(sm.result.NewRevision)
	}

	// 自启同步失败 → syncPending=true(下次按 s 做 no-write definition retry);
	// 成功 → syncPending=false。非自启 PartialErrors 不影响 syncPending。
	a.syncPending = sm.result.AutoStart.NeedsRetry()

	// 组合 statusMsg: 优先用 ApplyConfigResult 的结构化信息(SuccessMessage/SuggestedSteps/
	// ExplanatoryNotes), 再叠加自启同步失败提示。
	a.statusMsg = a.composeSaveStatus(sm)

	// quitAfterSave 衔接: 干净成功(ConfigApplied=true 且无 PartialErrors)才退出。
	// AutoStart 失败(syncPending)或 PartialErrors 都视为非干净成功,清 quitAfterSave 留编辑器。
	if a.quitAfterSave {
		if len(sm.result.PartialErrors) == 0 {
			a.quitAfterSave = false
			return tea.Quit
		}
		a.quitAfterSave = false
	}
	return nil
}

// composeSaveStatus 组合保存后的状态提示。融合 ApplyConfigResult 的结构化信息
// 与 syncPending/PartialErrors 提示。
func (a *App) composeSaveStatus(sm saveMsg) string {
	var parts []string
	if sm.result.SuccessMessage != "" {
		parts = append(parts, sm.result.SuccessMessage)
	} else {
		parts = append(parts, "已保存")
	}
	if a.syncPending {
		parts = append(parts, "自启定义同步失败:"+sm.result.AutoStart.Err.Error()+"（下次按 s 重试）")
	}
	// 非自启 PartialErrors 也展示(自启的已在 syncPending 分支覆盖)。
	for _, e := range sm.result.PartialErrors {
		if isAutoStartErr(e, sm.result.AutoStart.Err) {
			continue
		}
		parts = append(parts, "部分失败:"+e.Error())
	}
	for _, n := range sm.result.ExplanatoryNotes {
		parts = append(parts, n)
	}
	if len(sm.result.SuggestedSteps) > 0 {
		parts = append(parts, "建议:")
		for _, s := range sm.result.SuggestedSteps {
			parts = append(parts, "  "+s)
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

// isAutoStartErr 判断 PartialErrors 中的某项是否就是 AutoStart.Err(避免重复展示)。
func isAutoStartErr(e, autoStartErr error) bool {
	if autoStartErr == nil {
		return false
	}
	return errors.Is(e, autoStartErr)
}

func (a *App) View() string {
	if len(a.stack) == 0 {
		return ""
	}
	top := a.stack[len(a.stack)-1]
	header := lipgloss.NewStyle().Bold(true).Render("token-usage 配置")
	if a.dirty() {
		header += "   ⚠ 未保存改动"
	}
	if a.syncPending {
		header += "   ↻ 同步待重试"
	}
	body := top.View()
	if a.statusMsg != "" {
		body += "\n" + a.statusMsg + "\n"
	}
	// 确认层覆盖:dirty 退出时显示三选项(d 放弃退出 / s 保存并退出 / esc 返回)。
	if a.confirmQuit {
		body += "\n有未保存的改动,确认退出?\n  d 放弃并退出   s 保存并退出   esc/其他 返回编辑\n"
	}
	return header + "\n\n" + body
}

func (a *App) push(p page) { a.stack = append(a.stack, p) }
func (a *App) pop() {
	if len(a.stack) > 1 {
		a.stack = a.stack[:len(a.stack)-1]
	}
}

// dirty: draft 与 diskBaseline 深度不等。
func (a *App) dirty() bool {
	return !reflect.DeepEqual(a.draft, a.diskBaseline)
}

// saveNoOpHint 在主菜单按 s 时先行判断:无 dirty 且无 syncPending → 显示「没有待保存的更改」,
// 不调 ApplyConfig。返回 true 表示命中 no-op(主菜单据此返回 nil cmd)。
func (a *App) saveNoOpHint() bool {
	if a.saving {
		return false
	}
	if a.dirty() || a.syncPending {
		return false
	}
	a.statusMsg = noChangesMsg
	return true
}

// save 返回 tea.Cmd。无 dirty 且无 syncPending 时不启动保存(no-op)。
// saving=true 时返回 saveSkippedMsg(串行化)。否则:
//  1. 递增 saveGeneration, 标记 saving=true;
//  2. 深拷贝冻结 saveSnapshot(=draft 当前值)、saveRevision(=diskRevision)、saveGeneration;
//  3. 异步 ApplyConfig 只读 snapshot, 不引用可变 draft。
func (a *App) save() tea.Cmd {
	// 串行化优先于 no-op 判断：已有保存进行中时必须明确拒绝第二次保存。
	if a.saving {
		return func() tea.Msg { return saveSkippedMsg{} }
	}
	// no-op: 无 dirty 且无 syncPending。
	if !a.dirty() && !a.syncPending {
		a.statusMsg = noChangesMsg
		return nil
	}
	a.saving = true
	a.saveGeneration++
	apply := a.apply
	// 主 goroutine 侧深拷贝冻结: Cmd 在 bubbletea 后台 goroutine 执行 ApplyConfig,
	// 同时主 goroutine 可能改 draft(commit 写 map), 并发 map 读写会触发 data race。
	snapshot := cloneConfig(a.draft)
	saveRevision := cloneBytes(a.diskRevision)
	generation := a.saveGeneration
	return func() tea.Msg {
		if apply == nil {
			return saveMsg{
				snapshot:   snapshot,
				generation: generation,
				err:        fmt.Errorf("TUI 保存回调不能为空"),
			}
		}
		result, err := apply(saveRevision, snapshot)
		return saveMsg{
			saved:      result.ConfigApplied,
			snapshot:   snapshot,
			generation: generation,
			err:        err,
			result:     result,
		}
	}
}

// cloneBytes 返回 b 的副本(避免 caller 改动底层数组影响冻结值)。
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// cloneConfig 深拷贝(供 dirty 快照)。
func cloneConfig(c *config.Config) *config.Config {
	if c == nil {
		return nil
	}
	clone := *c
	if c.Clients != nil {
		clone.Clients = map[string]config.Client{}
		for k, v := range c.Clients {
			v2 := v
			if v.Paths != nil {
				v2.Paths = map[string]string{}
				for pk, pv := range v.Paths {
					v2.Paths[pk] = pv
				}
			}
			clone.Clients[k] = v2
		}
	}
	if c.Routers != nil {
		clone.Routers = map[string]config.RouterConfig{}
		for k, v := range c.Routers {
			clone.Routers[k] = v
		}
	}
	if c.ProviderAliases != nil {
		clone.ProviderAliases = map[string]string{}
		for k, v := range c.ProviderAliases {
			clone.ProviderAliases[k] = v
		}
	}
	return &clone
}

var _ tea.Model = (*App)(nil)
