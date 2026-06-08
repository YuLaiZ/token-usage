package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// ---- daemonPage.commit() error: poll_interval 校验 ----

// TestDaemonPage_CommitZeroPollIntervalOK 0 作为「使用默认值」合法可提交。
func TestDaemonPage_CommitZeroPollIntervalOK(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 30}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("0")
	if err := p.commit(); err != nil {
		t.Fatalf("0 应为合法默认值, commit 返回 error: %v", err)
	}
	if a.draft.Daemon.PollInterval != 0 {
		t.Errorf("commit 0 应写回 0, got %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_CommitPositivePollIntervalOK 正整数合法可提交。
func TestDaemonPage_CommitPositivePollIntervalOK(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("42")
	if err := p.commit(); err != nil {
		t.Fatalf("正整数应合法, commit 返回 error: %v", err)
	}
	if a.draft.Daemon.PollInterval != 42 {
		t.Errorf("commit 42 应写回 42, got %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_CommitNegativePollIntervalRejected 负数拒绝,保留旧值,返回 error。
func TestDaemonPage_CommitNegativePollIntervalRejected(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("-5")
	err := p.commit()
	if err == nil {
		t.Fatal("负数 poll_interval 应被拒绝(commit 返回 error)")
	}
	if a.draft.Daemon.PollInterval != 15 {
		t.Errorf("非法输入应保留旧值 15, 实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_CommitNonNumericPollIntervalRejected 非数字拒绝,保留旧值,返回 error。
func TestDaemonPage_CommitNonNumericPollIntervalRejected(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("abc")
	err := p.commit()
	if err == nil {
		t.Fatal("非数字 poll_interval 应被拒绝(commit 返回 error)")
	}
	if a.draft.Daemon.PollInterval != 15 {
		t.Errorf("非法输入应保留旧值 15, 实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_CommitOverflowPollIntervalRejected 溢出(超出 int 范围)拒绝。
func TestDaemonPage_CommitOverflowPollIntervalRejected(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 20}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("99999999999999999999")
	err := p.commit()
	if err == nil {
		t.Fatal("溢出 poll_interval 应被拒绝(commit 返回 error)")
	}
	if a.draft.Daemon.PollInterval != 20 {
		t.Errorf("溢出输入应保留旧值 20, 实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_EscBlocksOnInvalidInput esc 在输入无效时不应 pop 页面,且保留输入与旧值。
func TestDaemonPage_EscBlocksOnInvalidInput(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	a.push(p)
	p.setValue("-5")
	// esc 应触发 commit, 失败时不 pop
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 2 {
		t.Errorf("无效输入 esc 不应 pop, stack len = %d, want 2", len(a.stack))
	}
	if a.draft.Daemon.PollInterval != 15 {
		t.Errorf("无效输入 esc 应保留旧值 15, 实际 %d", a.draft.Daemon.PollInterval)
	}
	// 输入应保留(用户可修正)
	if p.input.Value() != "-5" {
		t.Errorf("无效输入 esc 应保留输入 '-5', 实际 %q", p.input.Value())
	}
}

// TestDaemonPage_EscLeavesOnValidInput 输入有效时 esc 正常 pop。
func TestDaemonPage_EscLeavesOnValidInput(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	a.push(p)
	p.setValue("42")
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 1 {
		t.Errorf("有效输入 esc 应 pop, stack len = %d, want 1", len(a.stack))
	}
	if a.draft.Daemon.PollInterval != 42 {
		t.Errorf("有效输入 esc 应写回 42, 实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestDaemonPage_FeedbackShowsReasonOnInvalid commit 失败时 feedback 展示校验原因。
func TestDaemonPage_FeedbackShowsReasonOnInvalid(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("not-int")
	err := p.commit()
	if err == nil {
		t.Fatal("非法输入应返回 error")
	}
	if p.feedback == "" {
		t.Error("commit 失败应设置 feedback 展示原因")
	}
	view := p.View()
	if !strings.Contains(view, p.feedback) {
		t.Errorf("View 应展示 feedback %q, 实际:\n%s", p.feedback, view)
	}
}

// TestDaemonPage_AutoStartAlwaysWritten toggle(AutoStart) 无需校验, commit 即使
// poll 校验失败也应尝试保留 toggle 状态？(裁决:校验失败时不写任何字段,保留旧值;AutoStart
// 由下次有效 commit 写入。)此测试守护:poll 无效时 AutoStart 也不写。
func TestDaemonPage_AutoStartNotWrittenWhenPollInvalid(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15, AutoStart: false}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("-5")
	p.toggle = NewToggle("开机自启", true) // 模拟翻转
	if err := p.commit(); err == nil {
		t.Fatal("负数 poll 应返回 error")
	}
	if a.draft.Daemon.AutoStart {
		t.Error("poll 校验失败时 AutoStart 也不应写入(整体回滚)")
	}
}

// ---- logPage.commit() error: max_days + level 校验 ----

// TestLogPage_CommitZeroMaxDaysOK 0 作为默认值合法。
func TestLogPage_CommitZeroMaxDaysOK(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("0")
	if err := p.commit(); err != nil {
		t.Fatalf("0 max_days 应合法, error: %v", err)
	}
	if a.draft.Log.MaxDays != 0 {
		t.Errorf("commit 0 应写回 0, got %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_CommitNegativeMaxDaysRejected 负数拒绝。
func TestLogPage_CommitNegativeMaxDaysRejected(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("-1")
	err := p.commit()
	if err == nil {
		t.Fatal("负数 max_days 应被拒绝")
	}
	if a.draft.Log.MaxDays != 7 {
		t.Errorf("负数应保留旧值 7, 实际 %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_CommitNonNumericMaxDaysRejected 非数字拒绝。
func TestLogPage_CommitNonNumericMaxDaysRejected(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("abc")
	err := p.commit()
	if err == nil {
		t.Fatal("非数字 max_days 应被拒绝")
	}
	if a.draft.Log.MaxDays != 7 {
		t.Errorf("非数字应保留旧值 7, 实际 %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_CommitOverflowMaxDaysRejected 溢出拒绝。
func TestLogPage_CommitOverflowMaxDaysRejected(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("99999999999999999999")
	err := p.commit()
	if err == nil {
		t.Fatal("溢出 max_days 应被拒绝")
	}
	if a.draft.Log.MaxDays != 7 {
		t.Errorf("溢出应保留旧值 7, 实际 %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_CommitUnknownLogLevelRejected 未知 log level 拒绝(模拟直接置 level)。
func TestLogPage_CommitUnknownLogLevelRejected(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{Level: "info"}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.level = "trace" // 未注册级别
	if err := p.commit(); err == nil {
		t.Fatal("未知 log level 应被拒绝")
	}
	if a.draft.Log.Level != "info" {
		t.Errorf("未知 level 应保留旧值 info, 实际 %q", a.draft.Log.Level)
	}
}

// TestLogPage_ToggleLevelFullCycle 验证 level 五态循环覆盖 warn/error:
// "" → info → debug → warn → error → ""。
func TestLogPage_ToggleLevelFullCycle(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	want := []string{"info", "debug", "warn", "error", ""}
	for _, w := range want {
		p.toggleLevel()
		if p.level != w {
			t.Errorf("toggle 后 level = %q, want %q", p.level, w)
		}
	}
}

// TestLogPage_CommitAllRegisteredLevelsOK 各注册 level(含 warn/error)均可提交。
func TestLogPage_CommitAllRegisteredLevelsOK(t *testing.T) {
	for _, lv := range []string{"", "info", "debug", "warn", "error"} {
		edit := &config.Config{}
		a := newAppForTest(edit, edit, nil)
		p := newLogPage(a)
		p.level = lv
		p.setMaxDays("3")
		if err := p.commit(); err != nil {
			t.Errorf("level %q 应可提交, error: %v", lv, err)
		}
		if a.draft.Log.Level != lv {
			t.Errorf("commit level %q 未写回, 实际 %q", lv, a.draft.Log.Level)
		}
	}
}

// TestLogPage_EscBlocksOnInvalidMaxDays max_days 无效时 esc 不 pop, 保留输入与旧值。
func TestLogPage_EscBlocksOnInvalidMaxDays(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	a.push(p)
	p.setMaxDays("abc")
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 2 {
		t.Errorf("无效 max_days esc 不应 pop, stack len = %d, want 2", len(a.stack))
	}
	if a.draft.Log.MaxDays != 7 {
		t.Errorf("应保留旧值 7, 实际 %d", a.draft.Log.MaxDays)
	}
	if p.maxDaysInput.Value() != "abc" {
		t.Errorf("应保留输入 'abc', 实际 %q", p.maxDaysInput.Value())
	}
}

// TestLogPage_EscLeavesOnValidInput 输入有效时 esc 正常 pop 并应用。
func TestLogPage_EscLeavesOnValidInput(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	a.push(p)
	p.setMaxDays("14")
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 1 {
		t.Errorf("有效输入 esc 应 pop, stack len = %d, want 1", len(a.stack))
	}
	if a.draft.Log.MaxDays != 14 {
		t.Errorf("应写回 14, 实际 %d", a.draft.Log.MaxDays)
	}
}

// TestLogPage_FeedbackShowsReasonOnInvalid commit 失败时 View 展示校验原因。
func TestLogPage_FeedbackShowsReasonOnInvalid(t *testing.T) {
	edit := &config.Config{}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	p.setMaxDays("not-int")
	if err := p.commit(); err == nil {
		t.Fatal("非法输入应返回 error")
	}
	if p.feedback == "" {
		t.Error("commit 失败应设置 feedback")
	}
	if !strings.Contains(p.View(), p.feedback) {
		t.Errorf("View 应展示 feedback %q", p.feedback)
	}
}

// TestLogPage_FixThenApply 错误状态修正后可应用到草稿。
func TestLogPage_FixThenApply(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{MaxDays: 7}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	// 先输入非法
	p.setMaxDays("bad")
	if err := p.commit(); err == nil {
		t.Fatal("非法 max_days 应被拒绝")
	}
	// 修正为合法
	p.setMaxDays("30")
	if err := p.commit(); err != nil {
		t.Fatalf("修正后应可提交, error: %v", err)
	}
	if a.draft.Log.MaxDays != 30 {
		t.Errorf("修正后应写回 30, 实际 %d", a.draft.Log.MaxDays)
	}
}

// TestDaemonPage_FixThenApply daemon 错误状态修正后可应用。
func TestDaemonPage_FixThenApply(t *testing.T) {
	edit := &config.Config{Daemon: config.DaemonConfig{PollInterval: 15}}
	a := newAppForTest(edit, edit, nil)
	p := newDaemonPage(a)
	p.setValue("bad")
	if err := p.commit(); err == nil {
		t.Fatal("非法 poll 应被拒绝")
	}
	p.setValue("60")
	if err := p.commit(); err != nil {
		t.Fatalf("修正后应可提交, error: %v", err)
	}
	if a.draft.Daemon.PollInterval != 60 {
		t.Errorf("修正后应写回 60, 实际 %d", a.draft.Daemon.PollInterval)
	}
}

// TestLogPage_EscBlocksOnInvalidLevel level 无效时 esc 不 pop。
func TestLogPage_EscBlocksOnInvalidLevel(t *testing.T) {
	edit := &config.Config{Log: config.LogConfig{Level: "info"}}
	a := newAppForTest(edit, edit, nil)
	p := newLogPage(a)
	a.push(p)
	p.level = "bogus"
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 2 {
		t.Errorf("无效 level esc 不应 pop, stack len = %d, want 2", len(a.stack))
	}
	if a.draft.Log.Level != "info" {
		t.Errorf("应保留旧值 info, 实际 %q", a.draft.Log.Level)
	}
}
