package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// 双模型加载:draft 不回填、display 回填
func TestNewApp_DualModels(t *testing.T) {
	draft := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{}}
	display := &config.Config{DataDir: "/x", Daemon: config.DaemonConfig{PollInterval: 30}}
	a := newAppForTest(draft, display, nil)
	if a.draft.Daemon.PollInterval != 0 {
		t.Error("draft model 应不回填(PollInterval=0)")
	}
	if a.display.Daemon.PollInterval != 30 {
		t.Error("display model 应回填(PollInterval=30)")
	}
}

// dirty 标志:draft 与 diskBaseline 不一致时为 dirty
func TestApp_DirtyDetection(t *testing.T) {
	draft := &config.Config{DataDir: "/x", Clients: map[string]config.Client{"codex": {Enabled: true}}}
	a := newAppForTest(draft, draft, nil)
	if a.dirty() {
		t.Error("未改动应 not dirty")
	}
	a.draft.Clients["codex"] = config.Client{Enabled: false}
	if !a.dirty() {
		t.Error("改 enabled 后应 dirty")
	}
}

// TestApp_Update_SubpageEscPopRestoresMainMenu 验证 App.Update 委托栈顶子页后,
// 子页 esc→commit+pop 致栈缩时,不把子页回写覆盖到主菜单槽位。
func TestApp_Update_SubpageEscPopRestoresMainMenu(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	mainMenu := a.stack[0]
	a.push(newAliasesPage(a))
	if len(a.stack) != 2 {
		t.Fatalf("push 后栈长 = %d, want 2", len(a.stack))
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_ = cmd
	if len(a.stack) != 1 {
		t.Fatalf("esc 后栈长 = %d, want 1(子页应 pop)", len(a.stack))
	}
	if a.stack[0] != mainMenu {
		t.Fatalf("esc 后栈底被覆盖:got %#v, want 原主菜单 %#v", a.stack[0], mainMenu)
	}
	_, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(a.stack) != 1 {
		t.Errorf("主菜单 esc 后栈长 = %d, want 1", len(a.stack))
	}
}

// saveSkippedMsg 不重置 saving(保存仍在进行)
func TestApp_Update_SaveSkippedDoesNotResetSaving(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	a.saving = true
	a.Update(saveSkippedMsg{})
	if !a.saving {
		t.Error("saveSkippedMsg 不应重置 saving")
	}
}

// contains helper(部分 statusMsg 断言复用)
func TestContains_Helper(t *testing.T) {
	if !contains("abc其他进程修改xyz", "其他进程修改") {
		t.Error("contains 应匹配子串")
	}
	if contains("abc", "其他进程修改") {
		t.Error("contains 不应误匹配")
	}
}

// cloneConfig 对两个 raw 载体递归深拷贝,mutation probe 无共享引用。
func TestCloneConfig_RawQueryMutationProbe(t *testing.T) {
	src := &config.Config{
		DataDir:  "/d",
		RawQuery: map[string]any{"sub": map[string]any{"list": []any{int64(1)}}},
		RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
			"Query": {Name: "Query", Value: map[string]any{"k": []any{"v"}}, Kind: config.RawQueryIssueNameConflict},
		},
	}
	clone := cloneConfig(src)
	if clone.RawQuery == nil || clone.RawQueryTopLevelIssues == nil {
		t.Fatal("clone 应保留两个 raw 载体")
	}
	src.RawQuery["sub"].(map[string]any)["list"].([]any)[0] = int64(9)
	if got := clone.RawQuery["sub"].(map[string]any)["list"].([]any)[0]; got != int64(1) {
		t.Errorf("clone RawQuery 深层共享引用: got %v", got)
	}
	src.RawQueryTopLevelIssues["Query"].Value.(map[string]any)["k"].([]any)[0] = "mutated"
	if got := clone.RawQueryTopLevelIssues["Query"].Value.(map[string]any)["k"].([]any)[0]; got != "v" {
		t.Errorf("clone issues 深层共享引用: got %v", got)
	}
	clone.RawQuery["sub"].(map[string]any)["new"] = "x"
	if _, ok := src.RawQuery["sub"].(map[string]any)["new"]; ok {
		t.Error("clone 侧写入泄漏到源")
	}
}
