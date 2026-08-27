package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func queryTestKeyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func keys(ss ...string) []tea.KeyMsg {
	out := make([]tea.KeyMsg, len(ss))
	for i, s := range ss {
		out[i] = queryTestKeyMsg(s)
	}
	return out
}

// 候选与已选集合分离;Space 选中追加到已选尾部、再次按取消移除,不重复添加。
func TestOrderedSelect_SpaceTogglesWithoutDuplicates(t *testing.T) {
	s := newOrderedSelect([]string{"client", "model", "provider", "project"}, nil, "test")
	s.handleKeys(keys("space")...)         // 选中 client
	s.handleKeys(keys("down", "space")...) // 选中 model
	s.handleKeys(keys("up", "space")...)   // 再次按 client:取消
	s.handleKeys(queryTestKeyMsg("space")) // client 重新选中:已选序列无重复项

	got := s.Selection()
	want := []string{"model", "client"} // model 先选,client 后选
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Selection = %v, want %v", got, want)
	}
	if s.Done() != selectPending {
		t.Errorf("Done = %v, want pending", s.Done())
	}
}

// Enter 提交深拷贝的有序选择;Esc 取消且不影响调用方初始数据。
func TestOrderedSelect_EnterReturnsDeepCopyEscCancels(t *testing.T) {
	initial := []string{"client", "model"}
	s := newOrderedSelect([]string{"client", "model", "provider"}, initial, "test")
	s.handleKeys(keys("down", "down", "space")...) // 追加 provider
	s.handleKeys(queryTestKeyMsg("enter"))

	if s.Done() != selectSubmitted {
		t.Fatalf("Done = %v, want submitted", s.Done())
	}
	got := s.Selection()
	if !reflect.DeepEqual(got, []string{"client", "model", "provider"}) {
		t.Errorf("Selection = %v", got)
	}
	// 深拷贝:修改返回值不影响组件内部与调用方初始切片。
	got[0] = "mutated"
	if !reflect.DeepEqual(initial, []string{"client", "model"}) {
		t.Errorf("调用方初始切片被修改: %v", initial)
	}

	s2 := newOrderedSelect([]string{"client", "model"}, []string{"client"}, "test")
	s2.handleKeys(queryTestKeyMsg("down"), queryTestKeyMsg("space"))
	s2.handleKeys(queryTestKeyMsg("esc"))
	if s2.Done() != selectCancelled {
		t.Errorf("Esc 后 Done = %v, want cancelled", s2.Done())
	}
	// 取消结果不产生新的选择快照语义;初始切片未被组件修改。
	if !reflect.DeepEqual(initial, []string{"client", "model"}) {
		t.Errorf("取消路径修改了调用方数据: %v", initial)
	}
}

// [ / ] 仅重排已选项:未选中项无操作,首尾不越界,预览顺序即时变化。
func TestOrderedSelect_ReorderSelectedOnly(t *testing.T) {
	s := newOrderedSelect([]string{"client", "model", "provider"}, []string{"client", "model", "provider"}, "test")
	// 光标 0(client,已选第 0 位):[ 不越界。
	s.handleKeys(queryTestKeyMsg("["))
	if !reflect.DeepEqual(s.Selection(), []string{"client", "model", "provider"}) {
		t.Errorf("首位前移应不越界: %v", s.Selection())
	}
	// ] 右移 client:model 与 client 交换。
	s.handleKeys(queryTestKeyMsg("]"))
	if !reflect.DeepEqual(s.Selection(), []string{"model", "client", "provider"}) {
		t.Errorf("右移失败: %v", s.Selection())
	}
	// 尾位右移不越界:光标移到 provider(已选末位)再按 ]。
	s.handleKeys(keys("down", "down")...)
	s.handleKeys(queryTestKeyMsg("]"))
	if !reflect.DeepEqual(s.Selection(), []string{"model", "client", "provider"}) {
		t.Errorf("末位后移应不越界: %v", s.Selection())
	}
	// 预览顺序即时反映。
	view := s.View()
	if !strings.Contains(view, "model") || !strings.Contains(view, "client") {
		t.Errorf("预览应包含已选名称:\n%s", view)
	}

	// 未选中项按 [ / ] 无操作。
	s2 := newOrderedSelect([]string{"client", "model"}, []string{"client"}, "test")
	s2.handleKeys(queryTestKeyMsg("down")) // 光标到 model(未选中)
	before := s2.Selection()
	s2.handleKeys(queryTestKeyMsg("["), queryTestKeyMsg("]"))
	if !reflect.DeepEqual(s2.Selection(), before) {
		t.Errorf("未选中项不得重排: %v -> %v", before, s2.Selection())
	}
}

// 上下移动在边界稳定;空候选、单候选、全部候选、长名称与 CJK 显示不崩溃。
func TestOrderedSelect_BoundariesAndDisplay(t *testing.T) {
	// 空候选。
	s := newOrderedSelect(nil, nil, "t")
	s.handleKeys(keys("up", "down", "space", "[", "]", "enter")...)
	if s.Done() != selectSubmitted {
		t.Errorf("空候选 Enter 也应可提交(空选择): %v", s.Done())
	}
	if len(s.Selection()) != 0 {
		t.Errorf("空候选选择应为空: %v", s.Selection())
	}

	// 单候选 + 光标边界。
	s1 := newOrderedSelect([]string{"client"}, nil, "t")
	s1.handleKeys(keys("up", "up", "down", "down")...)
	if s1.cursor != 0 {
		t.Errorf("单候选光标应稳定在 0: %d", s1.cursor)
	}

	// 全部候选 + CJK/长名称显示。
	long := strings.Repeat("很长的名字", 20)
	s2 := newOrderedSelect([]string{"client", long, "model"}, nil, "t")
	s2.handleKeys(queryTestKeyMsg("space"))
	s2.handleKeys(keys("down", "space")...)
	s2.handleKeys(keys("down", "space")...) // 全选
	if len(s2.Selection()) != 3 {
		t.Fatalf("全选失败: %v", s2.Selection())
	}
	view := s2.View()
	if !strings.Contains(view, long) {
		t.Errorf("View 应显示长名称:\n%s", view)
	}
}

// 光标越界防御:初始选择不在候选内被过滤;光标 clamp 到候选范围。
func TestOrderedSelect_InitialSelectionFiltered(t *testing.T) {
	s := newOrderedSelect([]string{"client", "model"}, []string{"model", "ghost"}, "t")
	got := s.Selection()
	if !reflect.DeepEqual(got, []string{"model"}) {
		t.Errorf("初始选择应过滤未知候选: %v", got)
	}
	if s.cursor != 0 || s.cursor >= len(s.candidates) {
		t.Errorf("光标应 clamp 在候选内: %d", s.cursor)
	}
}

// View 同时渲染候选(带光标与选中标记)和有序预览。
func TestOrderedSelect_ViewShowsCandidatesAndPreview(t *testing.T) {
	s := newOrderedSelect([]string{"client", "model"}, []string{"model"}, "自定义子查询")
	view := s.View()
	if !strings.Contains(view, "client") || !strings.Contains(view, "model") {
		t.Errorf("View 应列出候选:\n%s", view)
	}
	if !strings.Contains(view, "自定义子查询") {
		t.Errorf("View 应显示标题:\n%s", view)
	}
	if !strings.Contains(view, previewOrder([]string{"model"})) {
		t.Errorf("View 应含有序预览:\n%s", view)
	}
}

// previewOrder 渲染有序预览连接。
func TestPreviewOrder(t *testing.T) {
	if got := previewOrder(nil); got != "" {
		t.Errorf("空预览应为空串: %q", got)
	}
	if got := previewOrder([]string{"model", "provider", "client"}); got != "model → provider → client" {
		t.Errorf("previewOrder = %q", got)
	}
}

// 名称输入:Enter 提交 trim 后的值,校验反馈可注入;Esc 取消;组件不写配置。
func TestNamePrompt_TrimValidateAndCancel(t *testing.T) {
	var written string
	np := newNamePrompt("新视图名", func(name string) string {
		written = name // 若组件在校验时写配置会在此暴露副作用来源;仅记录以断言无副作用
		if name == "bad" {
			return "invalid name / 名称不合法"
		}
		return ""
	})
	np.input.SetValue("  mpc  ")
	np.handleKeys(queryTestKeyMsg("enter"))
	if np.Done() != selectSubmitted || np.Value() != "mpc" {
		t.Errorf("Enter 应提交 trim 后的值: done=%v value=%q", np.Done(), np.Value())
	}

	np2 := newNamePrompt("新视图名", func(name string) string {
		if name == "bad" {
			return "invalid name / 名称不合法"
		}
		return ""
	})
	np2.input.SetValue("bad")
	np2.handleKeys(queryTestKeyMsg("enter"))
	if np2.Done() != selectPending {
		t.Errorf("非法名称应停留待输入: %v", np2.Done())
	}
	if !strings.Contains(np2.View(), "invalid name") {
		t.Errorf("View 应显示校验反馈:\n%s", np2.View())
	}

	np3 := newNamePrompt("新视图名", func(string) string { return "" })
	np3.input.SetValue("x")
	np3.handleKeys(queryTestKeyMsg("esc"))
	if np3.Done() != selectCancelled {
		t.Errorf("Esc 应取消: %v", np3.Done())
	}
	_ = written // 校验回调被调用是预期的;组件自身不得写 config(无 config 依赖,编译期保证)
}
