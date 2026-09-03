// internal/tui/datadir_test.go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func newDataDirPageForTest() (*App, *dataDirPage) {
	a := newAppForTest(&config.Config{DataDir: "/custom/dd"}, &config.Config{DataDir: "/custom/dd"}, nil)
	p := newDataDirPage(a)
	a.push(p)
	return a, p
}

// TestDataDirPage_QKeyReturns 说明页 q 与 esc 同效:返回主菜单(只读,无 commit)。
func TestDataDirPage_QKeyReturns(t *testing.T) {
	a, p := newDataDirPageForTest()
	p.Update(keyMsg("q"))
	if len(a.stack) != 1 {
		t.Errorf("data_dir 说明页 q 应 pop 回主菜单, 栈长=%d", len(a.stack))
	}
}

// TestDataDirPage_OtherKeysDoNotPop 说明页仅 esc/q 返回,其他按键不弹栈。
func TestDataDirPage_OtherKeysDoNotPop(t *testing.T) {
	a, p := newDataDirPageForTest()
	p.Update(keyMsg("j"))
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.stack) != 2 {
		t.Errorf("data_dir 说明页其他按键不得 pop, 栈长=%d", len(a.stack))
	}
}

// TestDataDirPage_IgnoresNonKeyMessages 非键盘消息不弹栈、不改变状态。
func TestDataDirPage_IgnoresNonKeyMessages(t *testing.T) {
	a, p := newDataDirPageForTest()
	updated, _ := p.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if updated != p {
		t.Error("data_dir 说明页应原样返回自身模型")
	}
	if len(a.stack) != 2 {
		t.Errorf("非键盘消息不得 pop, 栈长=%d", len(a.stack))
	}
}

// TestDataDirPage_ViewShowsCurrentDataDir View 展示当前 display.DataDir 值。
func TestDataDirPage_ViewShowsCurrentDataDir(t *testing.T) {
	_, p := newDataDirPageForTest()
	view := p.View()
	if !strings.Contains(view, "/custom/dd") {
		t.Errorf("data_dir 说明页应展示当前 data_dir 值, got:\n%s", view)
	}
}

// TestDataDirPage_ViewExplainsReadOnlyReason View 说明只读原因(迁移对象/停 daemon/失败风险)。
func TestDataDirPage_ViewExplainsReadOnlyReason(t *testing.T) {
	_, p := newDataDirPageForTest()
	view := p.View()
	for _, want := range []string{
		"logs",      // 迁移对象含 logs/ 目录
		"daemon",    // 迁移前须停 daemon
		"esc",       // 返回键提示
		"read-only", // 只读定位
	} {
		if !strings.Contains(view, want) {
			t.Errorf("data_dir 说明页应含 %q, got:\n%s", want, view)
		}
	}
}
