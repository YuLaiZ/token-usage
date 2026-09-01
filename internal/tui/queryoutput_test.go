package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// 缺配置时编辑页按「默认七列后接 cache_create」的候选顺序预选默认七列。
func TestOutputColumnsPage_DefaultSelection(t *testing.T) {
	a := newQueryApp(t, &config.Config{DataDir: "/x"})
	p := newOutputColumnsPage(a)
	if p.recovery != nil {
		t.Fatalf("干净配置应进入编辑态: %v", p.recovery.items)
	}
	if p.sel == nil {
		t.Fatal("编辑态应构造 orderedSelect")
	}
	wantCandidates := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit", "cache_create"}
	if !reflect.DeepEqual(p.sel.candidates, wantCandidates) {
		t.Errorf("候选顺序 = %v, want %v", p.sel.candidates, wantCandidates)
	}
	if !reflect.DeepEqual(p.sel.Selection(), ui.DefaultOutputColumns()) {
		t.Errorf("初始选择 = %v, want 默认七列", p.sel.Selection())
	}
	// 界面显示双语标签而非裸 ID。
	view := p.View()
	for _, label := range []string{"Requests", "请求数", "Cache Create", "缓存创建", "Cache Hit", "缓存命中"} {
		if !strings.Contains(view, label) {
			t.Errorf("编辑页应显示双语标签 %q:\n%s", label, view)
		}
	}
}

// Space/[/]/Enter 产出正确顺序的 []any 字符串数组并整表替换 query.output。
func TestOutputColumnsPage_SubmitWritesColumnsTable(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total", "requests"}},
	}}
	a := newQueryApp(t, draft)
	p := newOutputColumnsPage(a)

	// 初始选择来自当前布局 [total, requests]。
	if got := p.sel.Selection(); !reflect.DeepEqual(got, []string{"total", "requests"}) {
		t.Fatalf("初始选择 = %v", got)
	}
	// 取消选中 total(候选光标移到 total,space 取消),再追加 cache_create。
	for i, c := range p.sel.candidates {
		if c == "total" {
			p.sel.cursor = i
		}
	}
	p.Update(queryTestKeyMsg(" "))
	for i, c := range p.sel.candidates {
		if c == "cache_create" {
			p.sel.cursor = i
		}
	}
	p.Update(queryTestKeyMsg(" "))
	p.Update(queryTestKeyMsg("enter"))

	got, ok := draft.RawQuery["output"].(map[string]any)
	if !ok {
		t.Fatalf("query.output 应为表: %T", draft.RawQuery["output"])
	}
	if len(got) != 1 {
		t.Errorf("整表替换后只应含 columns: %v", got)
	}
	arr, ok := got["columns"].([]any)
	if !ok {
		t.Fatalf("columns 应为 []any: %T", got["columns"])
	}
	if !reflect.DeepEqual(arr, []any{"requests", "cache_create"}) {
		t.Errorf("提交序列 = %v, want [requests cache_create]", arr)
	}
}

// [ ] 调序:把已选项在已选序列中前后移动,提交序列随调序结果写盘。
func TestOutputColumnsPage_ReorderAndSubmit(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"input", "requests"}},
	}}
	a := newQueryApp(t, draft)
	p := newOutputColumnsPage(a)
	if got := p.sel.Selection(); !reflect.DeepEqual(got, []string{"input", "requests"}) {
		t.Fatalf("初始选择 = %v", got)
	}
	for i, c := range p.sel.candidates {
		if c == "requests" {
			p.sel.cursor = i
		}
	}
	p.Update(queryTestKeyMsg("[")) // requests 前移 → [requests, input]
	p.Update(queryTestKeyMsg("enter"))

	arr, _ := draft.RawQuery["output"].(map[string]any)["columns"].([]any)
	if !reflect.DeepEqual(arr, []any{"requests", "input"}) {
		t.Errorf("调序后提交序列 = %v, want [requests input]", arr)
	}
}

// 取消(Esc)不改草稿;至少一项限制拦截空选择。
func TestOutputColumnsPage_CancelAndEmptySelection(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total"}},
	}}
	a := newQueryApp(t, draft)

	// Esc 取消:未提交编辑不落盘。
	p := newOutputColumnsPage(a)
	p.Update(queryTestKeyMsg("esc"))
	if len(a.stack) != 0 && len(a.stack) != 1 {
		t.Fatalf("esc 应返回上层")
	}
	if got, _ := draft.RawQuery["output"].(map[string]any); got == nil {
		t.Fatal("esc 不得删除已有 output")
	}

	// 清空全部选择后 Enter:留在页面并提示双语错误。
	p2 := newOutputColumnsPage(a)
	for i, c := range p2.sel.candidates {
		if c == "total" {
			p2.sel.cursor = i
		}
	}
	p2.Update(queryTestKeyMsg(" ")) // 取消唯一的 total
	p2.Update(queryTestKeyMsg("enter"))
	if len(p2.sel.Selection()) != 0 {
		t.Fatalf("空选择应被拦截")
	}
	if p2.errMsg == "" || !strings.Contains(p2.errMsg, "/") {
		t.Errorf("空选择应提示双语错误: %q", p2.errMsg)
	}
	// 草稿未变。
	if arr, _ := draft.RawQuery["output"].(map[string]any); arr == nil {
		t.Fatal("被拦截的提交不得改草稿")
	}
}

// 页面级 d:立即删除 output 表、丢弃未提交选择并返回上层,恢复默认布局。
func TestOutputColumnsPage_ResetToDefault(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total"}},
	}}
	a := newQueryApp(t, draft)
	p := newOutputColumnsPage(a)
	// 先做未提交编辑(取消 total),再按 d。
	for i, c := range p.sel.candidates {
		if c == "total" {
			p.sel.cursor = i
		}
	}
	p.Update(queryTestKeyMsg(" "))
	p.Update(queryTestKeyMsg("d"))

	if _, exists := draft.RawQuery["output"]; exists {
		t.Fatal("d 应删除 query.output 表")
	}
	if len(a.stack) < 1 {
		t.Fatalf("d 应返回上层")
	}
	// 不写入显式默认数组:RawQuery 不新增 output 键。
	if _, exists := draft.RawQuery["output"]; exists {
		t.Fatal("d 不得写入显式默认数组")
	}
}

// 非表 output、未知子键、非数组 columns、非法元素分别在可定位恢复态中
// 处理;元素类提示包含出错具体值;恢复动作统一删除整张 query.output 表,
// 执行后该页按局部解析重算回到编辑态。
func TestOutputColumnsPage_RecoveryStates(t *testing.T) {
	cases := []struct {
		name    string
		output  any
		wantSub string
	}{
		{"output not a table", "x", "query.output"},
		{"unknown subkey", map[string]any{"colums": []any{"total"}}, "colums"},
		{"columns not array", map[string]any{"columns": "total"}, "query.output.columns"},
		{"bad element", map[string]any{"columns": []any{"totals"}}, "totals"},
		{"empty array", map[string]any{"columns": []any{}}, ""},
		{"duplicate", map[string]any{"columns": []any{"total", "total"}}, "total"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{"output": tc.output}}
			a := newQueryApp(t, draft)
			p := newOutputColumnsPage(a)
			if p.recovery == nil {
				t.Fatalf("坏 output 应进恢复态")
			}
			view := p.View()
			if tc.wantSub != "" && !strings.Contains(view, tc.wantSub) {
				t.Errorf("恢复态应包含 %q:\n%s", tc.wantSub, view)
			}
			if !strings.Contains(view, "query.output") {
				t.Errorf("恢复动作应说明删除整张 query.output 表:\n%s", view)
			}
			// 执行恢复:删除整表,页面重算回编辑态且默认布局预选。
			p.recovery.cursor = 0
			p.Update(queryTestKeyMsg("enter"))
			if _, exists := draft.RawQuery["output"]; exists {
				t.Fatal("恢复动作应删除整张 output 表")
			}
			if p.recovery != nil {
				t.Fatalf("恢复后应回编辑态: %v", p.recovery.items)
			}
			if !reflect.DeepEqual(p.sel.Selection(), ui.DefaultOutputColumns()) {
				t.Errorf("恢复后初始选择应为默认七列: %v", p.sel.Selection())
			}
		})
	}
}

// 坏 subqueries/groups/default 不阻断 Output columns(局部解析隔离)。
func TestOutputColumnsPage_UnrelatedViewErrorsIgnored(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"default":    "ghost",
		"subqueries": map[string]any{"mpc": "model,"},
		"output":     map[string]any{"columns": []any{"total", "cache_create"}},
	}}
	p := newOutputColumnsPage(newQueryApp(t, draft))
	if p.recovery != nil {
		t.Fatalf("坏视图定义不得使 Output columns 进恢复态: %v", p.recovery.items)
	}
	if got := p.sel.Selection(); !reflect.DeepEqual(got, []string{"total", "cache_create"}) {
		t.Errorf("有效布局应可编辑: %v", got)
	}
}

// TUI 保存拒绝非法布局(完整校验);[]any columns 的 clone mutation 不共享引用。
func TestOutputColumnsPage_CloneAndSaveGuard(t *testing.T) {
	draft := &config.Config{DataDir: "/x", RawQuery: map[string]any{
		"output": map[string]any{"columns": []any{"total", "requests"}},
	}}
	a := newQueryApp(t, draft)

	// clone 深拷贝:修改 draft raw 不影响基线快照,数组往返不变。
	baseline := cloneConfig(draft)
	raw := draft.RawQuery["output"].(map[string]any)
	raw["columns"].([]any)[0] = "mutated"
	if got := baseline.RawQuery["output"].(map[string]any)["columns"].([]any); got[0] != "total" {
		t.Errorf("clone 与 draft 共享 []any 引用: %v", got)
	}
	raw["columns"].([]any)[0] = "total"

	// 合法布局可保存;非法布局(直接写入坏 raw)被完整校验拒绝。
	draft.RawQuery["output"] = map[string]any{"foo": 1}
	a.apply = func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		t.Fatal("非法布局保存必须被拒绝")
		return configapp.ApplyConfigResult{}, nil
	}
	if cmd := a.save(); cmd != nil {
		t.Fatal("非法布局保存应被拒绝")
	}
	if !strings.Contains(a.statusMsg, "query.output") {
		t.Errorf("拒绝提示应定位 query.output: %q", a.statusMsg)
	}
}
