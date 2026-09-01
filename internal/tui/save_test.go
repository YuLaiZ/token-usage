package tui

import (
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// fakeApply 是可注入的 ApplyFunc fake。按调用顺序消费预设结果队列。
type fakeApply struct {
	mu      sync.Mutex
	calls   []fakeApplyCall
	results []fakeApplyResult
}

type fakeApplyCall struct {
	expectedRevision []byte
	currentUser      *config.Config
}

type fakeApplyResult struct {
	result configapp.ApplyConfigResult
	err    error
	// block 非 nil 时,本次调用阻塞直到该通道关闭(精确控制 ApplyConfig 完成时序)。
	block <-chan struct{}
}

func (f *fakeApply) apply(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeApplyCall{expectedRevision: expectedRevision, currentUser: currentUser})
	idx := len(f.calls) - 1
	f.mu.Unlock()
	if idx < len(f.results) {
		r := f.results[idx]
		if r.block != nil {
			<-r.block
		}
		return r.result, r.err
	}
	return configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-default")}, nil
}

func (f *fakeApply) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeApply) callAt(i int) fakeApplyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// newApplyAppForTest 用 fake ApplyFunc 构造 App,初始化 draft/diskBaseline/diskRevision。
func newApplyAppForTest(draft *config.Config, diskRevision []byte, f *fakeApply) *App {
	a := &App{
		draft:        draft,
		diskBaseline: cloneConfig(draft),
		diskRevision: diskRevision,
		apply:        f.apply,
	}
	a.stack = []page{newMainMenu(a)}
	return a
}

// ---- Test 1: ApplyConfig 接入 + 成功后用 NewRevision 推进磁盘基线 ----
func TestApp_Save_AppliesConfigAndAdvancesRevision(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{{
			result: configapp.ApplyConfigResult{
				ConfigApplied:  true,
				Saved:          true,
				NewRevision:    []byte("rev-after-save"),
				SuccessMessage: "已更新配置",
			},
		}},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-1"), f)
	a.draft.DataDir = "/changed"
	if !a.dirty() {
		t.Fatal("应 dirty")
	}
	cmd := a.save()
	sm := cmd().(saveMsg)
	if !sm.result.ConfigApplied {
		t.Error("saveMsg 应携带 ConfigApplied=true 的结果")
	}
	a.handleSaveMsg(sm)
	if string(a.diskRevision) != "rev-after-save" {
		t.Errorf("diskRevision = %q, want rev-after-save", a.diskRevision)
	}
	if a.dirty() {
		t.Error("成功保存后 dirty 应清零")
	}
	c := f.callAt(0)
	if string(c.expectedRevision) != "rev-1" {
		t.Errorf("expectedRevision = %q, want rev-1", c.expectedRevision)
	}
	if c.currentUser.DataDir != "/changed" {
		t.Errorf("currentUser 应为冻结 snapshot,DataDir=%q", c.currentUser.DataDir)
	}
}

// ---- Test 2: 保存期间继续编辑,draft 不被 in-flight snapshot 影响 ----
func TestApp_Save_EditsDuringSaveDoNotAffectSnapshot(t *testing.T) {
	block := make(chan struct{})
	f := &fakeApply{
		results: []fakeApplyResult{{
			result: configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-2")},
			block:  block,
		}},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-1"), f)
	a.draft.DataDir = "/v1"
	cmd := a.save()
	// ApplyConfig 阻塞中:主 goroutine 模拟用户继续编辑 draft
	a.draft.DataDir = "/v2-during-save"
	close(block)
	sm := cmd().(saveMsg)
	a.handleSaveMsg(sm)
	if f.callAt(0).currentUser.DataDir != "/v1" {
		t.Errorf("snapshot 应冻结 /v1, got %q", f.callAt(0).currentUser.DataDir)
	}
	if a.draft.DataDir != "/v2-during-save" {
		t.Errorf("draft 应保留继续编辑的值,got %q", a.draft.DataDir)
	}
	if !a.dirty() {
		t.Error("保存期间又编辑,dirty 应保留(draft≠snapshot)")
	}
}

// ---- Test 3: 过期 generation 的结果不得覆盖较新基线 ----
// bubbletea Update 单线程串行,两代保存不会并发启动(saveSkippedMsg 保护)。
// 「乱序到达」的真实语义:gen1 解锁后 gen2 启动,gen2 先处理、gen1 的延迟结果后到。
// 本测试直接构造两个 saveMsg(不同 generation)按「旧 generation 后到」投递,
// 验证 handleSaveMsg 的 generation 守卫。
func TestApp_Save_StaleGenerationDoesNotClobberBaseline(t *testing.T) {
	f := &fakeApply{}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	// gen2(较新)先到达并处理:基线推进到 rev-fresh
	a.saveGeneration = 2
	smFresh := saveMsg{
		generation: 2,
		saved:      true,
		snapshot:   &config.Config{DataDir: "/x"},
		result:     configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-fresh")},
	}
	a.handleSaveMsg(smFresh)
	if string(a.diskRevision) != "rev-fresh" {
		t.Fatalf("gen2 后 diskRevision = %q, want rev-fresh", a.diskRevision)
	}
	// gen1(较旧, stale)后到达:不得覆盖 rev-fresh
	smStale := saveMsg{
		generation: 1,
		saved:      true,
		snapshot:   &config.Config{DataDir: "/x"},
		result:     configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-stale")},
	}
	a.handleSaveMsg(smStale)
	if string(a.diskRevision) != "rev-fresh" {
		t.Errorf("stale generation 不得覆盖较新基线:diskRevision = %q, want rev-fresh", a.diskRevision)
	}
	a.saving = true // 模拟较新 generation 仍在保存
	a.handleSaveMsg(smStale)
	if !a.saving {
		t.Error("过期 generation 不得清除较新保存的 saving 状态")
	}
}

func TestApp_Save_PlatformUnsupportedDoesNotBecomeSyncPending(t *testing.T) {
	f := &fakeApply{}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	a.saveGeneration = 1
	a.saving = true

	a.handleSaveMsg(saveMsg{
		generation: 1,
		snapshot:   &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{AutoStart: true}},
		result: configapp.ApplyConfigResult{
			ConfigApplied: true,
			NewRevision:   []byte("rev-1"),
			AutoStart: configapp.AutoStartOutcome{
				Requested: true,
				Err:       service.ErrPlatformUnsupported,
			},
			ExplanatoryNotes: []string{"当前平台不支持开机自启定义"},
		},
	})

	if a.syncPending {
		t.Error("平台不支持是非致命说明，不应进入可重试 syncPending")
	}
}

// ---- Test 4: 第一次 sync 失败(syncPending),无 dirty 重试成功清除 syncPending ----
func TestApp_Save_SyncPendingRetryClearsAfterSuccess(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{
			{
				result: configapp.ApplyConfigResult{
					ConfigApplied: true,
					Saved:         true,
					NewRevision:   []byte("rev-1"),
					AutoStart:     configapp.AutoStartOutcome{Err: errors.New("launchctl failed")},
					PartialErrors: []error{errors.New("自启同步失败")},
				},
				err: errors.Join(errors.New("自启同步失败")),
			},
			{
				result: configapp.ApplyConfigResult{
					ConfigApplied: true,
					Saved:         false, // no-write definition retry
					NewRevision:   []byte("rev-1"),
				},
			},
		},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x", Daemon: config.DaemonConfig{AutoStart: true}}, []byte("rev-0"), f)
	a.draft.Daemon.AutoStart = false
	a.handleSaveMsg(a.save()().(saveMsg))
	if !a.syncPending {
		t.Fatal("AutoStart.Err!=nil 应置 syncPending=true")
	}
	if a.dirty() {
		t.Error("config 已保存应清 dirty")
	}
	// 无 dirty 再次按 s:应调 ApplyConfig(no-write retry)
	a.handleSaveMsg(a.save()().(saveMsg))
	if a.syncPending {
		t.Error("retry 成功后 syncPending 应清除")
	}
	if f.callCount() != 2 {
		t.Errorf("应调用 ApplyConfig 两次,实际 %d 次", f.callCount())
	}
}

// ---- Test 5: 无 dirty 且无 syncPending 按 s 显示「没有待保存的更改」且不调 ApplyConfig ----
func TestApp_Save_NoDirtyNoSyncPendingIsNoOp(t *testing.T) {
	f := &fakeApply{}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-1"), f)
	if a.dirty() {
		t.Fatal("初始不应 dirty")
	}
	a.statusMsg = ""
	a.saveNoOpHint()
	if f.callCount() != 0 {
		t.Error("no-op 路径不应调 ApplyConfig")
	}
	if a.statusMsg != noChangesMsg {
		t.Errorf("statusMsg = %q, want %q", a.statusMsg, noChangesMsg)
	}
}

// ---- Test 6: consecutive saves 使用返回的新 revision 作为下次 expectedRevision ----
func TestApp_Save_ConsecutiveSavesUseNewRevision(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{
			{result: configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-1")}},
			{result: configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-2")}},
		},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	a.draft.DataDir = "/a"
	a.handleSaveMsg(a.save()().(saveMsg))
	if string(a.diskRevision) != "rev-1" {
		t.Fatalf("第一次保存后 diskRevision = %q, want rev-1", a.diskRevision)
	}
	a.draft.DataDir = "/b"
	a.handleSaveMsg(a.save()().(saveMsg))
	if string(a.diskRevision) != "rev-2" {
		t.Errorf("第二次保存后 diskRevision = %q, want rev-2", a.diskRevision)
	}
	if string(f.callAt(1).expectedRevision) != "rev-1" {
		t.Errorf("第二次保存 expectedRevision = %q, want rev-1", f.callAt(1).expectedRevision)
	}
}

// ---- Test 7: revision conflict (ErrConfigChangedExternally) 保留 draft 和 dirty ----
func TestApp_Save_RevisionConflictKeepsDraftAndDirty(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{
			{result: configapp.ApplyConfigResult{}, err: configapp.ErrConfigChangedExternally},
		},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-stale"), f)
	a.draft.DataDir = "/changed"
	if !a.dirty() {
		t.Fatal("应 dirty")
	}
	a.handleSaveMsg(a.save()().(saveMsg))
	if a.draft.DataDir != "/changed" {
		t.Errorf("conflict 后 draft 应保留,got %q", a.draft.DataDir)
	}
	if !a.dirty() {
		t.Error("conflict 后 dirty 应保留")
	}
	if string(a.diskRevision) != "rev-stale" {
		t.Errorf("conflict 后 diskRevision 应不变 = %q", a.diskRevision)
	}
	if !contains(a.statusMsg, "其他进程修改") {
		t.Errorf("statusMsg 应提示已被其他进程修改,实际 %q", a.statusMsg)
	}
}

// ---- Test 8: ConfigApplied=false (写入前/校验失败) 基线/revision 不变,dirty 保持 ----
func TestApp_Save_ConfigAppliedFalseKeepsBaselineAndDirty(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{
			{result: configapp.ApplyConfigResult{ConfigApplied: false}, err: errors.New("validation failed")},
		},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	a.draft.DataDir = "/changed"
	a.handleSaveMsg(a.save()().(saveMsg))
	if string(a.diskRevision) != "rev-0" {
		t.Errorf("ConfigApplied=false 时 diskRevision 应不变 = %q", a.diskRevision)
	}
	if !a.dirty() {
		t.Error("ConfigApplied=false 时 dirty 应保持")
	}
	if a.syncPending {
		t.Error("ConfigApplied=false 时 syncPending 应保持 false(仅自启失败才 true)")
	}
}

// ---- Test 9: ConfigApplied=true 但 PartialErrors(非自启) 仍推进基线 ----
func TestApp_Save_PartialErrorsStillAdvancesBaseline(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{{
			result: configapp.ApplyConfigResult{
				ConfigApplied: true,
				Saved:         true,
				NewRevision:   []byte("rev-1"),
				PartialErrors: []error{errors.New("stale metadata cleanup failed")},
			},
			err: errors.Join(errors.New("stale metadata cleanup failed")),
		}},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	a.draft.DataDir = "/changed"
	a.handleSaveMsg(a.save()().(saveMsg))
	if string(a.diskRevision) != "rev-1" {
		t.Errorf("PartialErrors 时基线应推进 = %q, want rev-1", a.diskRevision)
	}
	if a.dirty() {
		t.Error("config 已保存应清 dirty")
	}
	if a.syncPending {
		t.Error("非自启 PartialErrors 不应置 syncPending")
	}
}

// ---- Test 10: saving 串行化:saving=true 时再次 save 返回 saveSkippedMsg ----
func TestApp_Save_SavingFlagSkipsConcurrentApply(t *testing.T) {
	f := &fakeApply{}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	a.draft.DataDir = "/changed"
	a.saving = true
	msg := a.save()()
	if _, ok := msg.(saveSkippedMsg); !ok {
		t.Fatalf("saving 中应返回 saveSkippedMsg,实际 %T", msg)
	}
	if f.callCount() != 0 {
		t.Error("saving 中不应调 ApplyConfig")
	}
}

// ---- Test 11: 主菜单 s 路径集成:dirty 触发保存,no-op 显示提示 ----
func TestApp_MainMenuS_DirtySaves_NoDirtyHints(t *testing.T) {
	f := &fakeApply{
		results: []fakeApplyResult{
			{result: configapp.ApplyConfigResult{ConfigApplied: true, Saved: true, NewRevision: []byte("rev-1")}},
		},
	}
	a := newApplyAppForTest(&config.Config{DataDir: "/x"}, []byte("rev-0"), f)
	main := a.stack[0].(*mainMenu)
	a.draft.DataDir = "/changed"
	_, cmd := main.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		t.Fatal("dirty 按 s 应返回非 nil cmd")
	}
	a.handleSaveMsg(cmd().(saveMsg))
	if f.callCount() != 1 {
		t.Errorf("dirty 按 s 应调 ApplyConfig,实际 %d 次", f.callCount())
	}
	// no-op 路径:无 dirty 无 syncPending 再次按 s
	_, cmd2 := main.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd2 != nil {
		t.Errorf("no-op 按 s 应返回 nil cmd(不启动保存),got %T", cmd2())
	}
	if a.statusMsg != noChangesMsg {
		t.Errorf("no-op 路径 statusMsg = %q, want %q", a.statusMsg, noChangesMsg)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---- 保存前 query 校验 ----

// fakeQueryAdapter 是可注入的 QueryAdapter fake:err 控制校验结果。
type fakeQueryAdapter struct {
	err error
}

func (f fakeQueryAdapter) Validate(cfg *config.Config) error { return f.err }

func (f fakeQueryAdapter) Definitions(cfg *config.Config) (*querydef.QueryDefinitions, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &querydef.QueryDefinitions{ViewDefinitions: querydef.ViewDefinitions{Default: querydef.Target{Name: "client", Kind: querydef.TargetBuiltin}}}, nil
}

func (f fakeQueryAdapter) Views(cfg *config.Config) (*querydef.ViewDefinitions, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &querydef.ViewDefinitions{Default: querydef.Target{Name: "client", Kind: querydef.TargetBuiltin}}, nil
}

func (f fakeQueryAdapter) OutputLayout(cfg *config.Config) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return ui.DefaultOutputColumns(), nil
}

// save() 在创建异步 snapshot 前调用注入的 query 校验:失败时不调用 ApplyConfig、
// 保留 draft,并提示「在 Query 的 Views/Output columns 修复」与「使用 config set 完成无关单值修改」两条出路。
func TestApp_Save_QueryValidationRejectsSave(t *testing.T) {
	draft := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{PollInterval: 42}}
	a := newAppForTest(draft, draft, func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		t.Fatal("query 校验失败时不得调用 ApplyConfig")
		return configapp.ApplyConfigResult{}, nil
	}, fakeQueryAdapter{err: errors.New("invalid query config / 无效 query 配置")})
	draft.Daemon.PollInterval = 43 // 构造基线后制造 dirty

	cmd := a.save()
	if cmd != nil {
		t.Fatalf("校验失败应返回 nil cmd(不启动保存),实际 %T", cmd)
	}
	if !a.dirty() {
		t.Error("校验失败必须保留 draft dirty")
	}
	if a.saving {
		t.Error("校验失败不得进入 saving 态")
	}
	if !strings.Contains(a.statusMsg, "invalid query config") {
		t.Errorf("提示应含校验错误: %q", a.statusMsg)
	}
	for _, want := range []string{"Views", "Output columns", "config set", "草稿已保留"} {
		if !strings.Contains(a.statusMsg, want) {
			t.Errorf("拒绝提示应含出路 %q: %q", want, a.statusMsg)
		}
	}
}

// 校验通过时保存正常进行;未注入适配器(nil)保持既有行为。
func TestApp_Save_QueryValidationPassesSavesNormally(t *testing.T) {
	draft := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{PollInterval: 42}}
	a := newAppForTest(draft, draft, nil)
	a.query = fakeQueryAdapter{}
	draft.Daemon.PollInterval = 50

	cmd := a.save()
	if cmd == nil {
		t.Fatal("校验通过应正常启动保存")
	}
	msg := cmd()
	sm, ok := msg.(saveMsg)
	if !ok {
		t.Fatalf("cmd 应产生 saveMsg,实际 %T", msg)
	}
	if sm.err == nil || !strings.Contains(sm.err.Error(), "保存回调不能为空") {
		t.Errorf("nil apply 的错误应回流,实际 %v", sm.err)
	}

	// 未注入适配器:保持既有行为(不因 nil 适配器拒绝保存)。
	a2 := newAppForTest(draft, draft, nil)
	draft.Daemon.PollInterval = 60
	if cmd := a2.save(); cmd == nil {
		t.Fatal("nil 适配器不应阻断保存")
	}
}
