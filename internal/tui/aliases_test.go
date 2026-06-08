package tui

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	tea "github.com/charmbracelet/bubbletea"
)

// TestAliasesPage_AddDeleteEdit 基础增删改:成功返回 true,值落地 draft。
func TestAliasesPage_AddDeleteEdit(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{"A": "a"}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.add("B", "b") {
		t.Error("add 应成功")
	}
	if a.draft.ProviderAliases["B"] != "b" {
		t.Error("add 失败")
	}
	if !p.deleteKey("A") {
		t.Error("delete 应成功")
	}
	if _, ok := a.draft.ProviderAliases["A"]; ok {
		t.Error("delete 失败")
	}
	if !p.editValue("B", "b2") {
		t.Error("edit value 应成功")
	}
	if a.draft.ProviderAliases["B"] != "b2" {
		t.Error("edit value 失败")
	}
}

// TestAliasesPage_NilMapOpenCommitNotDirty 验证 ProviderAliases 为 nil 时(测试直构,
// 绕过 LoadUserConfig 的 initMaps),打开 aliases 页直接 commit 不应把 nil 转为 empty map
// 触发 dirty 误报。add 采用 lazy-init(仅 nil 才 init);delete 至空不归 nil;commit 不转换。
func TestAliasesPage_NilMapOpenCommitNotDirty(t *testing.T) {
	edit := &config.Config{ProviderAliases: nil}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	p.commit()
	if a.draft.ProviderAliases != nil {
		t.Errorf("nil ProviderAliases 打开即 commit 应保持 nil, got %v", a.draft.ProviderAliases)
	}
	if a.dirty() {
		t.Errorf("nil map 打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.ProviderAliases, a.diskBaseline.ProviderAliases)
	}
}

// TestAliasesPage_EmptyMapOpenCommitNotDirty 验证 ProviderAliases 为 empty non-nil 时,
// 打开页直接 commit 不应转换为 nil(或反向)触发 dirty 误报。
func TestAliasesPage_EmptyMapOpenCommitNotDirty(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	p.commit()
	if a.draft.ProviderAliases == nil {
		t.Errorf("empty non-nil ProviderAliases 打开即 commit 应保持 empty non-nil, 不应转 nil")
	}
	if len(a.draft.ProviderAliases) != 0 {
		t.Errorf("empty map 应保持 0 长度, got %v", a.draft.ProviderAliases)
	}
	if a.dirty() {
		t.Errorf("empty map 打开即 esc 不应 dirty, edit=%v initialEdit=%v", a.draft.ProviderAliases, a.diskBaseline.ProviderAliases)
	}
}

// TestAliasesPage_AddToNilLazyInits 验证向 nil ProviderAliases 新增时 lazy init 为 non-nil,
// 且新增成功。
func TestAliasesPage_AddToNilLazyInits(t *testing.T) {
	edit := &config.Config{ProviderAliases: nil}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.add("X", "x") {
		t.Fatal("向 nil add 应成功")
	}
	if a.draft.ProviderAliases == nil {
		t.Fatal("向 nil add 应 lazy init 为 non-nil")
	}
	if a.draft.ProviderAliases["X"] != "x" {
		t.Errorf("add 后 X 应为 x, got %q", a.draft.ProviderAliases["X"])
	}
}

// ---- key/value 非空校验 ----

// add: key trim 后为空 → 失败(false),不改 draft/dirty,反馈含提示。
func TestAliasesPage_AddEmptyKeyFails(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if p.add("  ", "v") {
		t.Error("空 key 应 add 失败(返回 false)")
	}
	if len(a.draft.ProviderAliases) != 0 {
		t.Errorf("失败 add 不应改 draft, got %v", a.draft.ProviderAliases)
	}
	if a.dirty() {
		t.Error("失败 add 不应 dirty")
	}
}

// add: value trim 后为空 → 失败(false),不改 draft/dirty。
func TestAliasesPage_AddEmptyValueFails(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if p.add("K", "   ") {
		t.Error("空 value 应 add 失败(返回 false)")
	}
	if len(a.draft.ProviderAliases) != 0 {
		t.Errorf("失败 add 不应改 draft, got %v", a.draft.ProviderAliases)
	}
	if a.dirty() {
		t.Error("失败 add 不应 dirty")
	}
}

// add: key 与 value 两端空白被 trim 后落库(trim 后均非空才成功)。
func TestAliasesPage_AddTrimsKeyAndValue(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.add("  K  ", "  v  ") {
		t.Fatal("带空白 key/value trim 后非空应 add 成功")
	}
	if a.draft.ProviderAliases["K"] != "v" {
		t.Errorf("add 应 trim 落库, K=%q", a.draft.ProviderAliases["K"])
	}
	if a.draft.ProviderAliases["  K  "] != "" {
		t.Error("不应残留未 trim 的 key")
	}
}

// editValue: value trim 后为空 → 失败(false),保留原值不改 draft/dirty。
func TestAliasesPage_EditEmptyValueFailsKeepsOriginal(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{"K": "orig"}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if p.editValue("K", "   ") {
		t.Error("空 value 应 edit 失败(返回 false)")
	}
	if a.draft.ProviderAliases["K"] != "orig" {
		t.Errorf("失败 edit 应保留原值, got %q", a.draft.ProviderAliases["K"])
	}
	if a.dirty() {
		t.Error("失败 edit 不应 dirty")
	}
}

// ---- 覆盖已有 key 反馈 ----

// add 覆盖已有 key → 成功且 feedback 含「已覆盖」。
func TestAliasesPage_AddOverwriteFeedback(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{"K": "old"}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.add("K", "new") {
		t.Fatal("覆盖已有 key 应 add 成功")
	}
	if a.draft.ProviderAliases["K"] != "new" {
		t.Errorf("覆盖后值应为 new, got %q", a.draft.ProviderAliases["K"])
	}
	if !strings.Contains(p.feedback, "已覆盖") {
		t.Errorf("覆盖 feedback 应含「已覆盖」, got %q", p.feedback)
	}
	if !strings.Contains(p.feedback, "K") {
		t.Errorf("覆盖 feedback 应含被覆盖 key, got %q", p.feedback)
	}
}

// add 新 key(非覆盖) → 成功, feedback 不含「已覆盖」。
func TestAliasesPage_AddNewKeyNoOverwriteFeedback(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.add("K", "v") {
		t.Fatal("新 key 应 add 成功")
	}
	if strings.Contains(p.feedback, "已覆盖") {
		t.Errorf("新 key 不应显示覆盖 feedback, got %q", p.feedback)
	}
}

// ---- 删除 alias 反馈 ----

// deleteKey 成功 → feedback 含「已删除」与 key。
func TestAliasesPage_DeleteFeedback(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{"K": "v"}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	if !p.deleteKey("K") {
		t.Fatal("删除存在 key 应成功")
	}
	if !strings.Contains(p.feedback, "已删除") {
		t.Errorf("删除 feedback 应含「已删除」, got %q", p.feedback)
	}
	if !strings.Contains(p.feedback, "K") {
		t.Errorf("删除 feedback 应含被删 key, got %q", p.feedback)
	}
	if _, ok := a.draft.ProviderAliases["K"]; ok {
		t.Error("删除后 key 应不存在")
	}
}

// ---- 保留输入(校验失败后输入可修正) ----

// add 失败(value trim 后空)后 editKey/valInput 保留待提交的输入,供修正:
// 通过 Update 走输入模式流程,提交空 value 时 add 失败,模式与 editKey 保留。
func TestAliasesPage_FailedAddKeepsInputForRetry(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	// a 进 key 输入模式
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if p.mode != 1 {
		t.Fatalf("按 a 应进 key 输入模式, mode=%d", p.mode)
	}
	p.keyInput.SetValue("mykey")
	// enter 进 value 输入模式
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.editKey != "mykey" {
		t.Fatalf("enter 后 editKey 应为 mykey, got %q", p.editKey)
	}
	// value 留空提交(enter) → 失败,保留输入
	p.valInput.SetValue("")
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.mode != 2 {
		t.Errorf("失败 add 应停留在 value 输入模式供修正, mode=%d", p.mode)
	}
	if p.editKey != "mykey" {
		t.Errorf("失败 add 应保留 editKey 供修正, got %q", p.editKey)
	}
	if len(a.draft.ProviderAliases) != 0 {
		t.Errorf("失败 add 不应改 draft, got %v", a.draft.ProviderAliases)
	}
}

// ---- alias 只应用草稿(不写盘) ----

// add/delete 只改 draft,不触发 ApplyConfig(apply==nil 也不会 panic),draft 与 diskBaseline
// 的差异即 dirty,但磁盘写入由主菜单 s 统一负责,aliases 页本身不写盘。
func TestAliasesPage_OnlyAppliesToDraftNotDisk(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	p.add("K", "v")
	// draft 已改,diskBaseline 未动(写盘由主菜单 s 负责)。
	if a.draft.ProviderAliases["K"] != "v" {
		t.Fatal("draft 应含新 alias")
	}
	if _, ok := a.diskBaseline.ProviderAliases["K"]; ok {
		t.Error("diskBaseline 不应被 aliases 页直接修改")
	}
	if !a.dirty() {
		t.Error("draft 与 diskBaseline 不一致应 dirty")
	}
}

// ---- 反馈 View 渲染 ----

// View 渲染 feedback。
func TestAliasesPage_ViewRendersFeedback(t *testing.T) {
	edit := &config.Config{ProviderAliases: map[string]string{"K": "v"}}
	a := newAppForTest(edit, edit, nil)
	p := newAliasesPage(a)
	p.deleteKey("K")
	view := p.View()
	if !strings.Contains(view, "已删除") {
		t.Errorf("View 应渲染删除 feedback, got:\n%s", view)
	}
}
