package tui

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	tea "github.com/charmbracelet/bubbletea"
)

func TestClientsPage_ToggleEnabled(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientsPage(a)
	p.cursor = 0
	// 模拟空格切换
	p.handleSpace()
	if a.draft.Clients["codex"].Enabled {
		t.Error("空格应切换为 false")
	}
}

func TestClientDetailPage_CommitPaths(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true, Paths: map[string]string{}}}}
	display := &config.Config{Clients: map[string]config.Client{"codex": {Paths: map[string]string{"db": "/default/db"}}}}
	a := newAppForTest(edit, display, nil)
	p := newClientDetailPage(a, "codex")
	// codex 不支持 router,字段全部是 path 输入框;按 key 定位,不依赖字段顺序
	var dbField *detailField
	for i := range p.fields {
		if p.fields[i].key == "db" {
			dbField = &p.fields[i]
			break
		}
	}
	if dbField == nil {
		t.Fatalf("codex 字段应含 db, got %d 个字段", len(p.fields))
	}
	dbField.input.SetValue("/custom/db")
	p.commit()
	if a.draft.Clients["codex"].Paths["db"] != "/custom/db" {
		t.Errorf("commit 后 Paths.db = %q", a.draft.Clients["codex"].Paths["db"])
	}
}

// TestClientDetailPage_OpenAndCommitNotDirty 验证打开详情页后直接 commit(未修改任何字段)
// 不应往 edit 写空串键,dirty 保持 false(防止展示键污染 edit 与 dirty 误报)。
func TestClientDetailPage_OpenAndCommitNotDirty(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true, Paths: map[string]string{"state_dir": "/orig/state"}}}}
	display := &config.Config{Clients: map[string]config.Client{"codex": {Paths: map[string]string{"state_dir": "/disp/state", "sessions_dir": "/disp/sess"}}}}
	a := newAppForTest(edit, display, nil)
	a.push(newClientDetailPage(a, "codex"))
	// 直接 commit(未修改任何 textinput)
	top := a.stack[len(a.stack)-1].(*clientDetailPage)
	top.commit()
	// 不应出现 display 引入的空串键
	if _, ok := a.draft.Clients["codex"].Paths["sessions_dir"]; ok {
		t.Errorf("commit 不应写入 display 占位键 sessions_dir, got Paths=%v", a.draft.Clients["codex"].Paths)
	}
	if a.dirty() {
		t.Errorf("打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.Clients["codex"], a.diskBaseline.Clients["codex"])
	}
}

// TestClientDetailPage_NilPathsOpenCommitNotDirty 验证 client 未配 paths(Paths==nil,
// 模拟 `[clients.codex] enabled=true` 无 paths 段)时,打开详情页直接 commit 不应把
// nil 转为 empty map 触发 dirty 误报(commit 须归一化空 Paths 回 nil)。
func TestClientDetailPage_NilPathsOpenCommitNotDirty(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true}}}
	display := &config.Config{Clients: map[string]config.Client{"codex": {Paths: map[string]string{"state_dir": "/disp/state", "sessions_dir": "/disp/sess"}}}}
	a := newAppForTest(edit, display, nil)
	a.push(newClientDetailPage(a, "codex"))
	top := a.stack[len(a.stack)-1].(*clientDetailPage)
	top.commit()
	if a.draft.Clients["codex"].Paths != nil {
		t.Errorf("nil Paths 场景 commit 后应归一化回 nil, got Paths=%v", a.draft.Clients["codex"].Paths)
	}
	if a.dirty() {
		t.Errorf("nil Paths 打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.Clients["codex"], a.diskBaseline.Clients["codex"])
	}
}

// TestClientDetailPage_EmptyNonNilPathsOpenCommitNotDirty 验证 client 配了空 paths 段
// (Paths==empty non-nil,模拟 `[clients.codex.paths]` 段存在但无键)时,打开详情页直接
// commit 不应把 empty 转为 nil(或反之)触发 dirty 误报(commit 须 lazy init,不预转换)。
func TestClientDetailPage_EmptyNonNilPathsOpenCommitNotDirty(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true, Paths: map[string]string{}}}}
	display := &config.Config{Clients: map[string]config.Client{"codex": {Paths: map[string]string{"state_dir": "/disp/state", "sessions_dir": "/disp/sess"}}}}
	a := newAppForTest(edit, display, nil)
	a.push(newClientDetailPage(a, "codex"))
	top := a.stack[len(a.stack)-1].(*clientDetailPage)
	top.commit()
	if a.draft.Clients["codex"].Paths == nil {
		t.Errorf("empty non-nil Paths 场景 commit 后应保持 empty non-nil, 不应转 nil, got Paths=%v", a.draft.Clients["codex"].Paths)
	}
	if a.dirty() {
		t.Errorf("empty non-nil Paths 打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.Clients["codex"], a.diskBaseline.Clients["codex"])
	}
}

// ---- cursor 覆盖 toggle 与全部字段(移动时同步 focus) ----

// TestClientDetailPage_CursorReachesToggleAndFields 验证 cursor 在 toggle(-1) 与各 path
// textinput 字段(0..N-1)间双向可达:从 toggle 向下逐字段到达最后,从最后向上逐字段回到 toggle。
// 修复 cursor=-1 无法到达(原 down 边界 cursor<len 允许越界到 len)与越界状态。
func TestClientDetailPage_CursorReachesToggleAndFields(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true, Paths: map[string]string{}}}}
	display := &config.Config{Clients: map[string]config.Client{"codex": {Paths: map[string]string{"state_dir": "/s", "sessions_dir": "/ss"}}}}
	a := newAppForTest(edit, display, nil)
	p := newClientDetailPage(a, "codex")
	// 字段: [0]=router(codex 支持归因), [1]=state_dir, [2]=sessions_dir
	wantFields := 3
	if len(p.fields) != wantFields {
		t.Fatalf("codex 应有 %d 字段(router+2 path), got %d", wantFields, len(p.fields))
	}
	if p.cursor != -1 {
		t.Fatalf("打开页默认 cursor 应在 toggle(-1), got %d", p.cursor)
	}
	if !p.toggle.Focused() {
		t.Error("toggle 应聚焦(cursor=-1)")
	}
	// down: -1 → 0 → 1 → 2,然后停在 2(不越界)
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 0 {
		t.Errorf("down 应到 router 字段(0), got %d", p.cursor)
	}
	if p.toggle.Focused() {
		t.Error("离开 toggle 后应失焦")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 1 {
		t.Errorf("down 应到 state_dir(1), got %d", p.cursor)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 2 {
		t.Errorf("down 应到 sessions_dir(2), got %d", p.cursor)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 2 {
		t.Errorf("最后字段再 down 应停在 2(不越界), got %d", p.cursor)
	}
	// up: 2 → 1 → 0 → -1(toggle),然后停在 -1
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != 1 {
		t.Errorf("up 应回 state_dir(1), got %d", p.cursor)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != 0 {
		t.Errorf("up 应回 router(0), got %d", p.cursor)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != -1 {
		t.Errorf("up 应到 toggle(-1), got %d", p.cursor)
	}
	if !p.toggle.Focused() {
		t.Error("回到 toggle 应聚焦")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != -1 {
		t.Errorf("toggle 再 up 应停在 -1, got %d", p.cursor)
	}
}

// TestClientDetailPage_ToggleFocusTogglesEnabled 验证 cursor 在 toggle(-1)时, space/enter
// 翻转 enabled 开关并同步 toggle.Value。
func TestClientDetailPage_ToggleFocusTogglesEnabled(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: false}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "codex")
	if p.cursor != -1 {
		t.Fatalf("cursor 应在 toggle(-1), got %d", p.cursor)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if !p.toggle.Value() {
		t.Error("space 在 toggle 应翻转 enabled 为 true")
	}
	p.commit()
	if !a.draft.Clients["codex"].Enabled {
		t.Error("commit 后 enabled 应为 true")
	}
	// enter 也应翻转 toggle
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.toggle.Value() {
		t.Error("enter 在 toggle 应翻转为 false")
	}
}

// ---- router 绑定字段从「无 + RegisteredRouters()」选择 ----

// TestClientDetailPage_RouterFieldSelectsFromRegistry 验证 router 字段从「无 + 注册 router」
// 循环选择,不接受自由文本输入:按 space/enter 循环,无 cc_switch→cc_switch→无。
func TestClientDetailPage_RouterFieldSelectsFromRegistry(t *testing.T) {
	// router 字段仅支持归因回填的客户端提供,用 claude 验证
	edit := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "claude")
	// router 字段是 fields[0],初始值为空(无)
	p.cursor = 0
	if p.fields[0].input.Value() != "" {
		t.Errorf("无 router 时字段应为空, got %q", p.fields[0].input.Value())
	}
	// space 循环: 无 → cc_switch(RegisteredRouters 唯一)
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if p.fields[0].input.Value() != "cc_switch" {
		t.Errorf("第一次 space 应选 cc_switch, got %q", p.fields[0].input.Value())
	}
	// 再 space 回到 无
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if p.fields[0].input.Value() != "" {
		t.Errorf("第二次 space 应回到 无(空), got %q", p.fields[0].input.Value())
	}
	p.commit()
	if a.draft.Clients["claude"].Router != "" {
		t.Errorf("空 router commit 应写回空, got %q", a.draft.Clients["claude"].Router)
	}
}

// TestClientDetailPage_RouterFieldUnregisteredCannotEnterDraft 验证未注册 router 无法进入草稿:
// router 字段只接受 registry 枚举,手输未注册名(如 fake_router)不应被 commit 写入。
func TestClientDetailPage_RouterFieldUnregisteredCannotEnterDraft(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "claude")
	// 尝试直接写未注册 router(模拟旧自由文本路径)
	p.fields[0].input.SetValue("fake_router")
	p.commit()
	if a.draft.Clients["claude"].Router != "" {
		t.Errorf("未注册 router 不应进入草稿, got %q(应保持空或归一化)", a.draft.Clients["claude"].Router)
	}
}

// TestClientDetailPage_RouterFieldPreselectsExisting 验证 client 已有合法 router(cc_switch)
// 时打开页,router 字段预选该值且 commit 不 dirty。
func TestClientDetailPage_RouterFieldPreselectsExisting(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "claude")
	if p.fields[0].input.Value() != "cc_switch" {
		t.Errorf("已有 cc_switch 应预选, got %q", p.fields[0].input.Value())
	}
	p.commit()
	if a.draft.Clients["claude"].Router != "cc_switch" {
		t.Errorf("commit 应保持 cc_switch, got %q", a.draft.Clients["claude"].Router)
	}
	if a.dirty() {
		t.Error("未改动 router 不应 dirty")
	}
}

// ---- path keys 来自 runtimecfg registry(共享只读入口) ----

// TestClientDetailPage_PathKeysFromRegistry 验证详情页字段键来自 runtimecfg.RegisteredClientPathKeys,
// 不复制 canonicalPathKeys 白名单(二者应一致,验证来源正确)。
func TestClientDetailPage_PathKeysFromRegistry(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{"workbuddy": {Enabled: true}}}
	a := newAppForTest(edit, edit, nil)
	p := newClientDetailPage(a, "workbuddy")
	// workbuddy 注册 path keys: db, projects_dir(不支持 router,字段直接是 path 输入框)
	keys := []string{}
	for _, f := range p.fields {
		keys = append(keys, f.key)
	}
	want := []string{"db", "projects_dir"}
	if len(keys) != len(want) {
		t.Fatalf("workbuddy path keys 数量 = %d, want %d(%v)", len(keys), len(want), keys)
	}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("path key[%d] = %q, want %q(全部 keys=%v)", i, keys[i], k, keys)
		}
	}
}

// ---- 空列表安全 ----

// TestClientsPage_EmptyListEnterSpaceNoOpNoPanic 验证 client 列表为空时 enter/space 不 panic、不越界、不改 draft。
func TestClientsPage_EmptyListEnterSpaceNoOpNoPanic(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{}}
	a := newAppForTest(edit, edit, nil)
	p := newClientsPage(a)
	if len(p.names) != 0 {
		t.Fatalf("空配置应无 client, got %v", p.names)
	}
	// enter 应 no-op(不 push 详情页)
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.stack) != 1 {
		t.Errorf("空列表 enter 不应 push 详情页, 栈长 = %d", len(a.stack))
	}
	// space 应 no-op(不 panic)
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if a.dirty() {
		t.Error("空列表 space 不应改 draft/dirty")
	}
}

// TestClientsPage_EmptyListShowsReason 验证空列表 View 显示原因说明(无已启用客户端等)。
func TestClientsPage_EmptyListShowsReason(t *testing.T) {
	edit := &config.Config{Clients: map[string]config.Client{}}
	a := newAppForTest(edit, edit, nil)
	p := newClientsPage(a)
	view := p.View()
	if !contains(view, "无") {
		t.Errorf("空列表 View 应含「无」类说明, got:\n%s", view)
	}
}

// TestClientsPage_EmptyClientsMapNilSafe 验证 Clients map 为 nil(测试直构)时页不 panic。
func TestClientsPage_EmptyClientsMapNilSafe(t *testing.T) {
	edit := &config.Config{Clients: nil}
	a := newAppForTest(edit, edit, nil)
	// 不 panic 即通过
	p := newClientsPage(a)
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
}

// TestClientDetailPage_LegacyRouterFieldVisibleForClearing 验证 router 字段可见性：
// 支持 router 归因的 client（codex）恒显示；非 router client 默认不显示，
// 但存量配置已带非空 router 时字段仍显示（用户可清回「无」，保存校验拒绝非空值）。
func TestClientDetailPage_LegacyRouterFieldVisibleForClearing(t *testing.T) {
	// codex 支持 router：无存量也显示 router 字段
	codexClean := &config.Config{Clients: map[string]config.Client{"codex": {Enabled: true}}}
	a := newAppForTest(codexClean, codexClean, nil)
	p := newClientDetailPage(a, "codex")
	var codexRouterSeen bool
	for _, f := range p.fields {
		if f.isRouter {
			codexRouterSeen = true
		}
	}
	if !codexRouterSeen {
		t.Error("codex 支持 router 归因，应显示 router 字段")
	}

	// 非 router client 无存量 router:不显示 router 字段
	clean := &config.Config{Clients: map[string]config.Client{"workbuddy": {Enabled: true}}}
	b := newAppForTest(clean, clean, nil)
	q := newClientDetailPage(b, "workbuddy")
	for _, f := range q.fields {
		if f.isRouter {
			t.Error("workbuddy 无存量 router 时不应显示 router 字段")
		}
	}

	// 存量非空 router:字段显示且预选存量值
	legacy := &config.Config{Clients: map[string]config.Client{"workbuddy": {Enabled: true, Router: "cc_switch"}}}
	c := newAppForTest(legacy, legacy, nil)
	r := newClientDetailPage(c, "workbuddy")
	var routerField *detailField
	for i := range r.fields {
		if r.fields[i].isRouter {
			routerField = &r.fields[i]
			break
		}
	}
	if routerField == nil {
		t.Fatal("workbuddy 存量 router 时应显示 router 字段（供清除）")
	}
	if routerField.input.Value() != "cc_switch" {
		t.Errorf("存量 router 应预选, got %q", routerField.input.Value())
	}
}
