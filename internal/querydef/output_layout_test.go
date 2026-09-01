package querydef

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// defaultColumnIDs 是默认七列的预期序列(与 ui.DefaultOutputColumns 同源)。
var defaultColumnIDs = []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"}

// asDiagnostics 断言 err 是 ValidationError 并返回其诊断列表。
func asDiagnostics(t *testing.T, err error) []Diagnostic {
	t.Helper()
	if err == nil {
		t.Fatal("应报错")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("错误应为 *ValidationError,实际 %T: %v", err, err)
	}
	if len(ve.Issues) == 0 {
		t.Fatal("ValidationError.Issues 不应为空")
	}
	return ve.Issues
}

// outputRaw 构造一个含指定 output 值的 Input。
func outputRaw(output any) Input {
	return Input{RawQuery: map[string]any{"output": output}}
}

// 缺失 query.output、缺失 columns 时回退默认七列;两次调用互为独立副本。
func TestParseOutputLayout_DefaultFallback(t *testing.T) {
	cases := map[string]Input{
		"no raw query":                 {},
		"empty raw query":              {RawQuery: map[string]any{}},
		"views only":                   {RawQuery: map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}}},
		"output table without columns": outputRaw(map[string]any{}),
	}
	for name, in := range cases {
		cols, err := ParseOutputLayout(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(cols, defaultColumnIDs) {
			t.Errorf("%s: cols = %v, want %v", name, cols, defaultColumnIDs)
		}
	}
	first, err := ParseOutputLayout(Input{})
	if err != nil {
		t.Fatal(err)
	}
	first[0] = "mutated"
	second, err := ParseOutputLayout(Input{})
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != "requests" {
		t.Errorf("默认布局应为独立副本,再次解析被污染: %v", second)
	}
}

// 合法数组保留用户声明顺序并允许 cache_create;手写元素先 TrimSpace。
func TestParseOutputLayout_ValidCustomLayout(t *testing.T) {
	cols, err := ParseOutputLayout(outputRaw(map[string]any{"columns": []any{"total", "requests", "cache_create"}}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"total", "requests", "cache_create"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("cols = %v, want %v", cols, want)
	}

	trimmed, err := ParseOutputLayout(outputRaw(map[string]any{"columns": []any{" requests ", "input"}}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"requests", "input"}; !reflect.DeepEqual(trimmed, want) {
		t.Errorf("元素应 TrimSpace: %v, want %v", trimmed, want)
	}
}

// 全部错误形态定位到 query.output / query.output.columns,元素类文本含具体值。
func TestParseOutputLayout_Errors(t *testing.T) {
	tests := []struct {
		name     string
		output   any
		wantPath string
		wantKind DiagnosticKind
		wantSub  string
	}{
		{"output not a table (string)", "x", "query.output", KindOutputTableType, "query.output"},
		{"output not a table (array)", []any{"requests"}, "query.output", KindOutputTableType, ""},
		{"output not a table (int)", 42, "query.output", KindOutputTableType, ""},
		{"unknown output key", map[string]any{"foo": []any{"requests"}}, "query.output.foo", KindOutputUnknownKey, "foo"},
		{"misspelled colums", map[string]any{"colums": []any{"requests"}}, "query.output.colums", KindOutputUnknownKey, "colums"},
		{"columns not array (string)", map[string]any{"columns": "requests"}, "query.output.columns", KindColumnsType, ""},
		{"columns not array (int)", map[string]any{"columns": 42}, "query.output.columns", KindColumnsType, ""},
		{"columns not array (table)", map[string]any{"columns": map[string]any{"requests": true}}, "query.output.columns", KindColumnsType, ""},
		{"non-string element", map[string]any{"columns": []any{"requests", 42}}, "query.output.columns", KindColumnsElement, "42"},
		{"empty string element", map[string]any{"columns": []any{"requests", ""}}, "query.output.columns", KindColumnsElement, `""`},
		{"whitespace element", map[string]any{"columns": []any{"requests", "   "}}, "query.output.columns", KindColumnsElement, `"   "`},
		{"unknown id", map[string]any{"columns": []any{"requestz"}}, "query.output.columns", KindColumnsElement, "requestz"},
		{"case sensitive id", map[string]any{"columns": []any{"Requests"}}, "query.output.columns", KindColumnsElement, "Requests"},
		{"duplicate id", map[string]any{"columns": []any{"requests", "input", "requests"}}, "query.output.columns", KindColumnsDuplicate, "requests"},
		{"duplicate after trim", map[string]any{"columns": []any{"requests", " requests "}}, "query.output.columns", KindColumnsDuplicate, "requests"},
		{"empty array", map[string]any{"columns": []any{}}, "query.output.columns", KindColumnsEmpty, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, err := ParseOutputLayout(outputRaw(tt.output))
			if cols != nil {
				t.Error("出错时不得返回布局")
			}
			issues := asDiagnostics(t, err)
			var first Diagnostic
			for _, d := range issues {
				if d.Kind == tt.wantKind {
					first = d
					break
				}
			}
			if first.Kind != tt.wantKind {
				t.Fatalf("未找到类别 %s,实际 %v", tt.wantKind, issues)
			}
			if first.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", first.Path, tt.wantPath)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("错误文本应含具体值 %q: %q", tt.wantSub, err.Error())
			}
		})
	}
}

// ParseViews 校验视图定义与除 output 外的未知顶层键,忽略 output 本身。
func TestParseViews_Scope(t *testing.T) {
	t.Run("valid views with invalid output succeeds", func(t *testing.T) {
		defs, err := ParseViews(Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
			"output":     map[string]any{"foo": "bar"},
		}})
		if err != nil {
			t.Fatalf("坏 output 不应影响 ParseViews: %v", err)
		}
		if len(defs.Subqueries) != 1 || defs.Subqueries[0].Name != "mpc" {
			t.Errorf("视图定义应正常解析: %+v", defs.Subqueries)
		}
	})
	t.Run("output key is allowed", func(t *testing.T) {
		_, err := ParseViews(Input{RawQuery: map[string]any{
			"output": map[string]any{"columns": []any{"total"}},
		}})
		if err != nil {
			t.Fatalf("合法 output 不应被报未知键: %v", err)
		}
	})
	t.Run("unknown top-level key still rejected", func(t *testing.T) {
		_, err := ParseViews(Input{RawQuery: map[string]any{"foo": "bar"}})
		issues := asDiagnostics(t, err)
		if issues[0].Kind != KindUnknownQueryKey || issues[0].Path != "query.foo" {
			t.Errorf("未知顶层键诊断 = %+v", issues[0])
		}
	})
	t.Run("bad view definitions still rejected", func(t *testing.T) {
		_, err := ParseViews(Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,"},
			"output":     map[string]any{"columns": []any{"total"}},
		}})
		if err == nil || !strings.Contains(err.Error(), "query.subqueries.mpc") {
			t.Errorf("坏视图定义应被 ParseViews 拒绝: %v", err)
		}
	})
}

// ParseOutputLayout 只校验 output:坏视图定义与其他顶层键不阻断布局。
func TestParseOutputLayout_Scope(t *testing.T) {
	cols, err := ParseOutputLayout(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,"},
		"default":    []any{"not-a-string"},
		"foo":        "bar",
		"output":     map[string]any{"columns": []any{"total"}},
	}})
	if err != nil {
		t.Fatalf("无关视图/顶层错误不应阻断布局解析: %v", err)
	}
	if want := []string{"total"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("cols = %v, want %v", cols, want)
	}
}

// 完整 Parse 组合两者:成功携带 OutputColumns;两类错误按 Views 先、output 后聚合。
func TestParse_CombinesViewsAndOutputLayout(t *testing.T) {
	t.Run("success carries layout", func(t *testing.T) {
		defs, err := Parse(Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
			"output":     map[string]any{"columns": []any{"total", "cache_create"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"total", "cache_create"}; !reflect.DeepEqual(defs.OutputColumns, want) {
			t.Errorf("OutputColumns = %v, want %v", defs.OutputColumns, want)
		}
		if len(defs.Subqueries) != 1 {
			t.Errorf("视图定义应保留: %+v", defs.Subqueries)
		}
	})
	t.Run("missing output falls back to default layout", func(t *testing.T) {
		defs, err := Parse(Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,provider"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(defs.OutputColumns, defaultColumnIDs) {
			t.Errorf("OutputColumns = %v, want %v", defs.OutputColumns, defaultColumnIDs)
		}
	})
	t.Run("views errors precede output errors", func(t *testing.T) {
		_, err := Parse(Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,"},
			"output":     map[string]any{"columns": []any{"nope"}},
		}})
		issues := asDiagnostics(t, err)
		var viewsIdx, outputIdx = -1, -1
		for i, d := range issues {
			if d.Path == "query.subqueries.mpc" && viewsIdx < 0 {
				viewsIdx = i
			}
			if d.Path == "query.output.columns" && outputIdx < 0 {
				outputIdx = i
			}
		}
		if viewsIdx < 0 || outputIdx < 0 {
			t.Fatalf("两类错误都应报告: %+v", issues)
		}
		if viewsIdx > outputIdx {
			t.Errorf("Views 错误应在 output 错误之前: %+v", issues)
		}
	})
}

// 全部「产生点 → Kind」映射:每类问题都有稳定 Path 与 Kind。
func TestParse_DiagnosticKindMapping(t *testing.T) {
	tests := []struct {
		name     string
		in       Input
		wantKind DiagnosticKind
		wantPath string
	}{
		{
			"top-level name conflict",
			Input{RawQueryTopLevelIssues: map[string]TopLevelIssue{
				"Query": {Name: "Query", Kind: TopLevelNameConflict},
			}},
			KindTopLevelProblem, "query",
		},
		{
			"top-level root not table",
			Input{RawQueryTopLevelIssues: map[string]TopLevelIssue{
				"query": {Name: "query", Kind: TopLevelRootNotTable},
			}},
			KindTopLevelProblem, "query",
		},
		{"unknown query key", Input{RawQuery: map[string]any{"foo": "x"}},
			KindUnknownQueryKey, "query.foo"},
		{"subqueries not table", Input{RawQuery: map[string]any{"subqueries": "x"}},
			KindViewsTableType, "query.subqueries"},
		{"groups not table", Input{RawQuery: map[string]any{"groups": 42}},
			KindViewsTableType, "query.groups"},
		{"reserved definition name", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"list": "model,provider"}}},
			KindDefinitionName, "query.subqueries.list"},
		{"invalid definition name", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"BadName": "model,provider"}}},
			KindDefinitionName, "query.subqueries.BadName"},
		{"definition value not string", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": []any{"model", "provider"}}}},
			KindDefinitionValueType, "query.subqueries.mpc"},
		{"subquery unknown item", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,nope"}}},
			KindDefinitionItem, "query.subqueries.mpc"},
		{"group unknown item", Input{RawQuery: map[string]any{
			"groups": map[string]any{"g": "client,nope"}}},
			KindDefinitionItem, "query.groups.g"},
		{"group references group", Input{RawQuery: map[string]any{
			"groups": map[string]any{"g1": "client,model", "g2": "client,g1"}}},
			KindDefinitionItem, "query.groups.g2"},
		{"duplicate item", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"mpc": "model,model"}}},
			KindDuplicateItem, "query.subqueries.mpc"},
		{"minimum items", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"solo": "model"}}},
			KindMinimumItems, "query.subqueries.solo"},
		{"cross-table duplicate", Input{RawQuery: map[string]any{
			"subqueries": map[string]any{"dup": "model,provider"},
			"groups":     map[string]any{"dup": "client,model"}}},
			KindCrossTableDuplicate, "query.groups.dup"},
		{"default not string", Input{RawQuery: map[string]any{"default": []any{"mpc"}}},
			KindDefault, "query.default"},
		{"default unknown", Input{RawQuery: map[string]any{
			"default":    "nope",
			"subqueries": map[string]any{"mpc": "model,provider"}}},
			KindDefault, "query.default"},
		{"output not table", outputRaw("x"),
			KindOutputTableType, "query.output"},
		{"output unknown key", outputRaw(map[string]any{"foo": []any{"requests"}}),
			KindOutputUnknownKey, "query.output.foo"},
		{"columns not array", outputRaw(map[string]any{"columns": "requests"}),
			KindColumnsType, "query.output.columns"},
		{"columns bad element", outputRaw(map[string]any{"columns": []any{"nope"}}),
			KindColumnsElement, "query.output.columns"},
		{"columns duplicate", outputRaw(map[string]any{"columns": []any{"requests", "requests"}}),
			KindColumnsDuplicate, "query.output.columns"},
		{"columns empty", outputRaw(map[string]any{"columns": []any{}}),
			KindColumnsEmpty, "query.output.columns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			issues := asDiagnostics(t, err)
			for _, d := range issues {
				if d.Kind == tt.wantKind && d.Path == tt.wantPath {
					return
				}
			}
			t.Errorf("未找到 Kind=%s Path=%s 的诊断,实际: %+v", tt.wantKind, tt.wantPath, issues)
		})
	}
}

// 顶层问题态是两个局部入口的共同前置:返回相同的顶层诊断,不返回可写结果。
func TestTopLevelIssues_SharedPrecondition(t *testing.T) {
	in := Input{
		RawQuery: map[string]any{"subqueries": map[string]any{"mpc": "model,provider"}},
		RawQueryTopLevelIssues: map[string]TopLevelIssue{
			"Query": {Name: "Query", Kind: TopLevelNameConflict},
			"query": {Name: "query", Kind: TopLevelRootNotTable},
		},
	}
	views, viewsErr := ParseViews(in)
	if views != nil {
		t.Error("顶层问题态不得返回视图定义")
	}
	layout, layoutErr := ParseOutputLayout(in)
	if layout != nil {
		t.Error("顶层问题态不得返回可写布局")
	}
	for name, err := range map[string]error{"ParseViews": viewsErr, "ParseOutputLayout": layoutErr} {
		if err == nil {
			t.Fatalf("%s 应报顶层诊断", name)
		}
		issues := asDiagnostics(t, err)
		for _, d := range issues {
			if d.Kind != KindTopLevelProblem {
				t.Errorf("%s 在顶层问题态只应返回顶层诊断: %+v", name, issues)
			}
		}
		msg := err.Error()
		for _, want := range []string{`"Query"`, "name_conflict", "root_not_table"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s 顶层诊断应含 %q: %q", name, want, msg)
			}
		}
	}
	if viewsErr.Error() != layoutErr.Error() {
		t.Errorf("两个局部入口的顶层诊断应一致:\n%q\n%q", viewsErr.Error(), layoutErr.Error())
	}
}

// 完整 Parse 的诊断顺序:顶层独占;Views 既有顺序在前;output 按固定子序追加。
func TestParse_DiagnosticOrder(t *testing.T) {
	t.Run("top-level issues are exclusive", func(t *testing.T) {
		_, err := Parse(Input{
			RawQuery: map[string]any{"foo": "bar", "output": map[string]any{"columns": "x"}},
			RawQueryTopLevelIssues: map[string]TopLevelIssue{
				"Query": {Name: "Query", Kind: TopLevelNameConflict},
			},
		})
		issues := asDiagnostics(t, err)
		for _, d := range issues {
			if d.Kind != KindTopLevelProblem {
				t.Errorf("顶层问题存在时其他诊断不得出现: %+v", issues)
			}
		}
	})

	t.Run("output internal order", func(t *testing.T) {
		// 未知子键按键名排序在前,columns 类型在后。
		_, err := Parse(outputRaw(map[string]any{
			"zzz":     1,
			"aaa":     2,
			"columns": "notarray",
		}))
		issues := asDiagnostics(t, err)
		var kinds []DiagnosticKind
		for _, d := range issues {
			kinds = append(kinds, d.Kind)
		}
		want := []DiagnosticKind{KindOutputUnknownKey, KindOutputUnknownKey, KindColumnsType}
		if !reflect.DeepEqual(kinds, want) {
			t.Errorf("output 诊断顺序 = %v, want %v (%+v)", kinds, want, issues)
		}
		if issues[0].Path != "query.output.aaa" || issues[1].Path != "query.output.zzz" {
			t.Errorf("未知子键应按键名排序: %+v", issues[:2])
		}
	})

	t.Run("element order follows array", func(t *testing.T) {
		_, err := Parse(outputRaw(map[string]any{"columns": []any{"bad1", 42, "bad2"}}))
		issues := asDiagnostics(t, err)
		var texts []string
		for _, d := range issues {
			if d.Kind != KindColumnsElement {
				t.Errorf("应只有元素诊断: %+v", issues)
			}
			texts = append(texts, d.Message)
		}
		for i, want := range []string{"bad1", "42", "bad2"} {
			if !strings.Contains(texts[i], want) {
				t.Errorf("第 %d 个元素诊断应含 %q: %+v", i, want, issues)
			}
		}
		// index 用数组枚举下标:前置被拒元素不得使后续 index 偏小。
		for i, want := range []string{"index 0", "index 1", "index 2"} {
			if !strings.Contains(issues[i].Message, want) {
				t.Errorf("第 %d 个元素诊断 index 应为 %q: %q", i, want, issues[i].Message)
			}
		}
	})

	t.Run("duplicate precedes empty is irrelevant but each kind ordered after elements", func(t *testing.T) {
		// 元素错误与重复错误按数组索引顺序交织。
		_, err := Parse(outputRaw(map[string]any{"columns": []any{"requests", "nope", "requests"}}))
		issues := asDiagnostics(t, err)
		if len(issues) != 2 {
			t.Fatalf("应有两个诊断: %+v", issues)
		}
		if issues[0].Kind != KindColumnsElement || issues[1].Kind != KindColumnsDuplicate {
			t.Errorf("诊断顺序 = %+v, want element 在前 duplicate 在后", issues)
		}
	})
}

// 跨表重名固定呈现 query.groups.<name>(恢复动作删 group 保留同名子查询);
// 定义值类型保留单条目路径(恢复只删该条目而非整表)。
func TestParse_RecoveryPathShapes(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"dup": "model,provider"},
		"groups":     map[string]any{"dup": "client,model"},
	}})
	issues := asDiagnostics(t, err)
	found := false
	for _, d := range issues {
		if d.Kind == KindCrossTableDuplicate {
			found = true
			if d.Path != "query.groups.dup" {
				t.Errorf("跨表重名 Path = %q, want query.groups.dup", d.Path)
			}
		}
	}
	if !found {
		t.Fatalf("应含跨表重名诊断: %+v", issues)
	}
	// 既有双语文本保持兼容(仍描述两表重名)。
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("跨表重名文本应含名称: %q", err.Error())
	}

	_, err = Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": []any{"model"}},
	}})
	issues = asDiagnostics(t, err)
	if issues[0].Kind != KindDefinitionValueType || issues[0].Path != "query.subqueries.mpc" {
		t.Errorf("定义值类型诊断 = %+v, want query.subqueries.mpc/definition_value_type", issues[0])
	}
}

// 解析结果不与 raw []any 或调用方切片共享引用。
func TestParseOutputLayout_ResultNotAliased(t *testing.T) {
	rawColumns := []any{"total", "requests"}
	raw := map[string]any{"output": map[string]any{"columns": rawColumns}}
	cols, err := ParseOutputLayout(Input{RawQuery: raw})
	if err != nil {
		t.Fatal(err)
	}
	rawColumns[0] = "mutated"
	if cols[0] != "total" {
		t.Errorf("布局与 raw 共享引用: %v", cols)
	}
	cols[0] = "mutated"
	if rawColumns[1] != "requests" {
		t.Errorf("布局修改泄漏回 raw: %v", rawColumns)
	}
}

// ValidationError.Error() 逐行拼接双语诊断,与既有 errors.Join 形态一致。
func TestValidationErrorErrorJoinsMessages(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"foo":    "bar",
		"output": map[string]any{"columns": "x"},
	}})
	issues := asDiagnostics(t, err)
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != len(issues) {
		t.Errorf("Error() 行数 %d 应与诊断数 %d 一致: %q", len(lines), len(issues), err.Error())
	}
	for i, d := range issues {
		if lines[i] != d.Message {
			t.Errorf("第 %d 行 %q 应为诊断 %q", i, lines[i], d.Message)
		}
	}
}
