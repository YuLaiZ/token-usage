package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
)

// isQuitCmd 判断 tea.Cmd 是否会触发程序退出(返回 tea.QuitMsg)。
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// makeDirtyApp 构造一个 dirty 状态的 App(带 fakeApply),返回 App 与 fake。
func makeDirtyApp(t *testing.T, f *fakeApply) *App {
	t.Helper()
	draft := &config.Config{DataDir: "/x"}
	a := newApplyAppForTest(draft, []byte("rev-0"), f)
	a.display = cloneConfig(draft) // display 非空,避免 View 空 deref
	a.draft.DataDir = "/changed"
	if !a.dirty() {
		t.Fatal("setup: 应 dirty")
	}
	return a
}

// makeCleanApp 构造一个 clean 状态的 App(无 dirty,带 fakeApply)。
func makeCleanApp(t *testing.T, f *fakeApply) *App {
	t.Helper()
	draft := &config.Config{DataDir: "/x"}
	a := newApplyAppForTest(draft, []byte("rev-0"), f)
	a.display = cloneConfig(draft)
	if a.dirty() {
		t.Fatal("setup: 应 clean")
	}
	return a
}

// ---- 三个确认分支 ----

// 确认层选「放弃并退出」(d):直接 tea.Quit。
func TestQuitConfirm_DiscardQuits(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	// dirty 状态 q 进入确认层
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !a.confirmQuit {
		t.Fatal("dirty 按 q 应进入 confirmQuit")
	}
	if isQuitCmd(cmd) {
		t.Error("进入确认层不应立刻退出")
	}
	// 确认层选「放弃并退出」
	_, cmd = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !isQuitCmd(cmd) {
		t.Error("放弃并退出应返回 tea.Quit")
	}
	if f.callCount() != 0 {
		t.Error("放弃退出不应触发保存")
	}
}

// 确认层选「保存并退出」(s):设 quitAfterSave=true 并启动保存,不立刻 Quit。
func TestQuitConfirm_SaveAndExitStartsSaveNoImmediateQuit(t *testing.T) {
	f := &fakeApply{results: []fakeApplyResult{{
		result: configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-1")},
	}}}
	a := makeDirtyApp(t, f)
	// ctrl+c dirty 进入确认层(验证 ctrl+c 不绕过)
	_, _ = a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !a.confirmQuit {
		t.Fatal("dirty 按 ctrl+c 应进入 confirmQuit(不绕过)")
	}
	// 选「保存并退出」
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if isQuitCmd(cmd) {
		t.Error("保存并退出不应立刻 Quit")
	}
	if !a.quitAfterSave {
		t.Error("保存并退出应设 quitAfterSave=true")
	}
	if !a.saving {
		t.Error("保存并退出应启动保存(saving=true)")
	}
	if f.callCount() != 1 {
		t.Errorf("保存并退出应调 ApplyConfig,实际 %d 次", f.callCount())
	}
	if a.confirmQuit {
		t.Error("启动保存后应退出确认层(confirmQuit=false)")
	}
}

// 确认层选「返回编辑」(esc/其他):清 confirmQuit,留编辑器,不退出不保存。
func TestQuitConfirm_BackToEditClearsConfirm(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !a.confirmQuit {
		t.Fatal("setup: 应已进入 confirmQuit")
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if isQuitCmd(cmd) {
		t.Error("返回编辑不应退出")
	}
	if a.confirmQuit {
		t.Error("返回编辑应清 confirmQuit")
	}
	if a.quitAfterSave {
		t.Error("返回编辑不应设 quitAfterSave")
	}
	if f.callCount() != 0 {
		t.Error("返回编辑不应触发保存")
	}
}

// 其他任意键(非 d/s/esc)也返回编辑。
func TestQuitConfirm_AnyOtherKeyBackToEdit(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	a.confirmQuit = true
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if isQuitCmd(cmd) {
		t.Error("任意其他键应返回编辑不退出")
	}
	if a.confirmQuit {
		t.Error("任意其他键应清 confirmQuit")
	}
}

// ---- dirty 状态下 q/esc/ctrl+c 都进同一确认层 ----

// dirty 按 q 进入确认层。
func TestQuitConfirm_DirtyQEntersConfirm(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !a.confirmQuit {
		t.Error("dirty 按 q 应进入 confirmQuit")
	}
	if isQuitCmd(cmd) {
		t.Error("dirty 按 q 不应直接退出")
	}
}

// dirty 按 esc 进入确认层。
func TestQuitConfirm_DirtyEscEntersConfirm(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !a.confirmQuit {
		t.Error("dirty 按 esc 应进入 confirmQuit")
	}
	if isQuitCmd(cmd) {
		t.Error("dirty 按 esc 不应直接退出")
	}
}

// dirty 按 ctrl+c 进入确认层(不绕过)。
func TestQuitConfirm_DirtyCtrlCEntersConfirm(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !a.confirmQuit {
		t.Error("dirty 按 ctrl+c 应进入 confirmQuit")
	}
	if isQuitCmd(cmd) {
		t.Error("dirty 按 ctrl+c 不应直接退出(不绕过)")
	}
}

// ---- clean 状态正常退出 ----

// clean 状态 q/esc/ctrl+c 正常退出。
func TestQuitConfirm_CleanQuitImmediately(t *testing.T) {
	f := &fakeApply{}
	cases := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	}
	for i, km := range cases {
		a := makeCleanApp(t, f)
		_, cmd := a.Update(km)
		if !isQuitCmd(cmd) {
			t.Errorf("case %d: clean 状态应直接退出", i)
		}
		if a.confirmQuit {
			t.Errorf("case %d: clean 退出不应进入 confirmQuit", i)
		}
	}
}

// ---- save 成功后才退出 ----

// 保存并退出:ApplyConfig 成功(ConfigApplied=true, 无 PartialErrors)且 quitAfterSave=true → Quit。
func TestQuitConfirm_SaveSuccessThenQuit(t *testing.T) {
	f := &fakeApply{results: []fakeApplyResult{{
		result: configapp.ApplyConfigResult{
			ConfigApplied: true, Saved: true, NewRevision: []byte("rev-1"),
		},
	}}}
	a := makeDirtyApp(t, f)
	a.quitAfterSave = true
	cmd := a.save()
	sm := cmd().(saveMsg)
	_, quitCmd := a.Update(sm)
	if !isQuitCmd(quitCmd) {
		t.Error("保存成功且 quitAfterSave=true 应返回 tea.Quit")
	}
	if a.quitAfterSave {
		t.Error("成功退出后应清 quitAfterSave")
	}
}

// ---- save 失败不退出 ----

// 保存并退出:ApplyConfig 失败(ConfigApplied=false)且 quitAfterSave=true → 不 Quit,清 quitAfterSave,留编辑器。
func TestQuitConfirm_SaveFailureNoQuit(t *testing.T) {
	f := &fakeApply{results: []fakeApplyResult{{
		result: configapp.ApplyConfigResult{ConfigApplied: false},
		err:    errors.New("validation failed"),
	}}}
	a := makeDirtyApp(t, f)
	a.quitAfterSave = true
	cmd := a.save()
	sm := cmd().(saveMsg)
	_, quitCmd := a.Update(sm)
	if isQuitCmd(quitCmd) {
		t.Error("保存失败不应退出")
	}
	if a.quitAfterSave {
		t.Error("保存失败应清 quitAfterSave")
	}
	if !a.dirty() {
		t.Error("保存失败应留 dirty")
	}
	if !contains(a.statusMsg, "保存失败") {
		t.Errorf("保存失败应显示失败提示,实际 %q", a.statusMsg)
	}
}

// 保存并退出:ConfigApplied=true 但有 PartialErrors → 不 Quit(非干净成功),清 quitAfterSave,留编辑器。
func TestQuitConfirm_SavePartialErrorsNoQuit(t *testing.T) {
	f := &fakeApply{results: []fakeApplyResult{{
		result: configapp.ApplyConfigResult{
			ConfigApplied: true, Saved: true, NewRevision: []byte("rev-1"),
			PartialErrors: []error{errors.New("stale metadata cleanup failed")},
		},
	}}}
	a := makeDirtyApp(t, f)
	a.quitAfterSave = true
	cmd := a.save()
	sm := cmd().(saveMsg)
	_, quitCmd := a.Update(sm)
	if isQuitCmd(quitCmd) {
		t.Error("PartialErrors 不应退出(非干净成功)")
	}
	if a.quitAfterSave {
		t.Error("PartialErrors 应清 quitAfterSave")
	}
}

// ---- 保存中退出被阻止 ----

// saving=true 时退出键(q/esc/ctrl+c)给「保存进行中」,不退出不进确认层。
func TestQuitConfirm_SavingBlocksQuit(t *testing.T) {
	f := &fakeApply{}
	cases := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	}
	for i, km := range cases {
		a := makeDirtyApp(t, f)
		a.saving = true
		_, cmd := a.Update(km)
		if isQuitCmd(cmd) {
			t.Errorf("case %d: saving 中不应退出", i)
		}
		if a.confirmQuit {
			t.Errorf("case %d: saving 中不应进 confirmQuit", i)
		}
		if !contains(a.statusMsg, "保存进行中") {
			t.Errorf("case %d: saving 中应提示保存进行中,实际 %q", i, a.statusMsg)
		}
	}
}

// 确认层 saving 保护:已进确认层后 saving=true 时选「放弃并退出」(d) 仍不得退出。
func TestQuitConfirm_SavingBlocksDiscardInConfirm(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	a.confirmQuit = true
	a.saving = true
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if isQuitCmd(cmd) {
		t.Error("saving 中确认层放弃也不应退出")
	}
	if !contains(a.statusMsg, "保存进行中") {
		t.Errorf("saving 中应提示保存进行中,实际 %q", a.statusMsg)
	}
}

// ---- View 渲染 ----

// confirmQuit=true 时 View 显示三个选项。
func TestQuitConfirm_ViewShowsOptions(t *testing.T) {
	f := &fakeApply{}
	a := makeDirtyApp(t, f)
	a.confirmQuit = true
	view := a.View()
	if !contains(view, "放弃并退出") {
		t.Errorf("确认层应含「放弃并退出」,实际:\n%s", view)
	}
	if !contains(view, "保存并退出") {
		t.Errorf("确认层应含「保存并退出」,实际:\n%s", view)
	}
	if !contains(view, "返回编辑") {
		t.Errorf("确认层应含「返回编辑」,实际:\n%s", view)
	}
}

// v 进入 Query 父页后,主菜单 q 仍走 dirty 退出确认(键位不冲突回归)。
func TestQuitConfirm_VOpensQueryParent_QStillConfirmsWhenDirty(t *testing.T) {
	draft := &config.Config{DataDir: "/x"}
	a := newAppForTest(draft, draft, nil)
	m := a.stack[0].(*mainMenu)
	m.Update(queryTestKeyMsg("v"))
	if _, ok := a.stack[1].(*queryParentPage); !ok {
		t.Fatalf("v 应进入 Query 父页")
	}
	// 返回主菜单并制造 dirty 后按 q → 确认层。
	a.pop()
	draft.Daemon.PollInterval = 99
	m.Update(queryTestKeyMsg("q"))
	if !a.confirmQuit {
		t.Fatal("dirty 时 q 应进入退出确认层")
	}
}
