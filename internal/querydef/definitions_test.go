package querydef

import (
	"reflect"
	"strings"
	"testing"
)

// validInput 构造一份完整合法的输入(default + 自定义子查询 + 组合查询)。
func validInput() Input {
	return Input{RawQuery: map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"mpc": "model, provider,client"},
		"groups":     map[string]any{"group_q": "client,model,provider,mpc"},
	}}
}

// 内置维度、自定义子查询、组合查询与默认行为的成功解析;CSV TrimSpace 且声明顺序逐项保留。
func TestParse_SuccessfulDefinitions(t *testing.T) {
	defs, err := Parse(validInput())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if defs.Default.Name != "group_q" || defs.Default.Kind != TargetGroup {
		t.Errorf("Default = %+v, want group_q/TargetGroup", defs.Default)
	}
	if len(defs.Subqueries) != 1 {
		t.Fatalf("Subqueries 数量 = %d", len(defs.Subqueries))
	}
	sub := defs.Subqueries[0]
	if sub.Name != "mpc" {
		t.Errorf("sub.Name = %q", sub.Name)
	}
	// "model, provider,client" 与 "model,provider,client" 等价,声明顺序即维度列顺序。
	wantDims := []BuiltinDimension{DimensionModel, DimensionProvider, DimensionClient}
	if !reflect.DeepEqual(sub.Dimensions, wantDims) {
		t.Errorf("Dimensions = %v, want %v", sub.Dimensions, wantDims)
	}
	if len(defs.Groups) != 1 {
		t.Fatalf("Groups 数量 = %d", len(defs.Groups))
	}
	group := defs.Groups[0]
	if group.Name != "group_q" {
		t.Errorf("group.Name = %q", group.Name)
	}
	wantItems := []Target{
		{Name: "client", Kind: TargetBuiltin},
		{Name: "model", Kind: TargetBuiltin},
		{Name: "provider", Kind: TargetBuiltin},
		{Name: "mpc", Kind: TargetCustom},
	}
	if !reflect.DeepEqual(group.Items, wantItems) {
		t.Errorf("Items = %+v, want %+v", group.Items, wantItems)
	}
}

// 未配置(RawQuery nil 或空 map)时默认等价 client。
func TestParse_UnconfiguredFallsBackToClient(t *testing.T) {
	for name, in := range map[string]Input{
		"nil":   {},
		"empty": {RawQuery: map[string]any{}},
	} {
		defs, err := Parse(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if defs.Default.Name != "client" || defs.Default.Kind != TargetBuiltin {
			t.Errorf("%s: Default = %+v, want client/TargetBuiltin", name, defs.Default)
		}
	}
}

// 空 [query] 与空子表同样回退 client,不产生任何定义。
func TestParse_EmptySectionsFallBackToClient(t *testing.T) {
	defs, err := Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{},
		"groups":     map[string]any{},
	}})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if defs.Default.Name != "client" || defs.Default.Kind != TargetBuiltin {
		t.Errorf("Default = %+v, want client", defs.Default)
	}
	if len(defs.Subqueries) != 0 || len(defs.Groups) != 0 {
		t.Errorf("空子表不应产生定义: %+v / %+v", defs.Subqueries, defs.Groups)
	}
}

// CSV 错误:空串、空段、尾逗号、重复项、非小写名称、未知项,均定位到具体配置键。
func TestParse_CSVErrors(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantKey string
		wantSub string
	}{
		{name: "empty string", value: "", wantKey: "query.subqueries.mpc", wantSub: `""`},
		{name: "trailing comma", value: "model,", wantKey: "query.subqueries.mpc", wantSub: `""`},
		{name: "empty segment", value: "model,,client", wantKey: "query.subqueries.mpc", wantSub: `""`},
		{name: "duplicate item", value: "model,model", wantKey: "query.subqueries.mpc", wantSub: "model"},
		{name: "non-lowercase item", value: "Model,client", wantKey: "query.subqueries.mpc", wantSub: `"Model"`},
		{name: "unknown item", value: "model,unknown", wantKey: "query.subqueries.mpc", wantSub: `"unknown"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{RawQuery: map[string]any{
				"subqueries": map[string]any{"mpc": tt.value},
			}}
			_, err := Parse(in)
			if err == nil {
				t.Fatal("应报错")
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantKey) {
				t.Errorf("错误未定位到 %q: %q", tt.wantKey, msg)
			}
			if !strings.Contains(msg, tt.wantSub) {
				t.Errorf("错误未包含问题值: %q", msg)
			}
		})
	}
}

// 允许集合出现在错误信息里(built-in 四维度)。
func TestParse_CSVErrorsListAllowedSet(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,unknown"},
	}})
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, dim := range []string{"client", "model", "provider", "project"} {
		if !strings.Contains(msg, dim) {
			t.Errorf("错误应包含允许集合项 %q: %q", dim, msg)
		}
	}
}

// 结构错误:少于两个维度/两项、组合嵌套、断开引用、保留名冲突、非法名称、跨表重名、值类型错误、未知顶层键。
func TestParse_StructuralErrors(t *testing.T) {
	tests := []struct {
		name    string
		raw     map[string]any
		wantSub []string
	}{
		{
			name:    "single dimension subquery",
			raw:     map[string]any{"subqueries": map[string]any{"solo": "model"}},
			wantSub: []string{"query.subqueries.solo", "2"},
		},
		{
			name:    "single item group",
			raw:     map[string]any{"groups": map[string]any{"g": "client"}},
			wantSub: []string{"query.groups.g", "2"},
		},
		{
			name:    "group references group",
			raw:     map[string]any{"groups": map[string]any{"g1": "client,model", "g2": "client,g1"}},
			wantSub: []string{"query.groups.g2", "g1"},
		},
		{
			name:    "group unresolved reference",
			raw:     map[string]any{"groups": map[string]any{"g": "client,mpc"}},
			wantSub: []string{"query.groups.g", `"mpc"`},
		},
		{
			name:    "duplicate group item",
			raw:     map[string]any{"groups": map[string]any{"g": "client,model,client"}},
			wantSub: []string{"query.groups.g", "client"},
		},
		{
			name:    "reserved name client",
			raw:     map[string]any{"subqueries": map[string]any{"client": "model,provider"}},
			wantSub: []string{"query.subqueries.client", "client"},
		},
		{
			name:    "reserved name session",
			raw:     map[string]any{"groups": map[string]any{"session": "client,model"}},
			wantSub: []string{"query.groups.session", "session"},
		},
		{
			name:    "reserved name custom",
			raw:     map[string]any{"subqueries": map[string]any{"custom": "model,provider"}},
			wantSub: []string{"query.subqueries.custom", "custom"},
		},
		{
			name:    "invalid name uppercase",
			raw:     map[string]any{"subqueries": map[string]any{"BadName": "model,provider"}},
			wantSub: []string{"query.subqueries.BadName", `"BadName"`},
		},
		{
			name:    "invalid name leading digit",
			raw:     map[string]any{"subqueries": map[string]any{"1abc": "model,provider"}},
			wantSub: []string{"query.subqueries.1abc", `"1abc"`},
		},
		{
			name: "cross-table duplicate name",
			raw: map[string]any{
				"subqueries": map[string]any{"dup": "model,provider"},
				"groups":     map[string]any{"dup": "client,model"},
			},
			wantSub: []string{"dup"},
		},
		{
			name:    "non-string subquery value",
			raw:     map[string]any{"subqueries": map[string]any{"mpc": []any{"model", "provider"}}},
			wantSub: []string{"query.subqueries.mpc"},
		},
		{
			name:    "unknown top-level key",
			raw:     map[string]any{"foo": "bar", "subqueries": map[string]any{"mpc": "model,provider"}},
			wantSub: []string{`query.foo`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(Input{RawQuery: tt.raw})
			if err == nil {
				t.Fatal("应报错")
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("错误 %q 未包含 %q", err.Error(), sub)
				}
			}
		})
	}
}

// default:空白回退 client;首尾空格匹配;未知值报路径、值与允许集合。
func TestParse_DefaultBehavior(t *testing.T) {
	base := func(def string) Input {
		return Input{RawQuery: map[string]any{
			"default":    def,
			"subqueries": map[string]any{"mpc": "model,provider"},
			"groups":     map[string]any{"g": "client,mpc"},
		}}
	}

	t.Run("whitespace falls back to client", func(t *testing.T) {
		defs, err := Parse(base("   "))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if defs.Default.Name != "client" || defs.Default.Kind != TargetBuiltin {
			t.Errorf("Default = %+v, want client", defs.Default)
		}
	})

	t.Run("trimmed match", func(t *testing.T) {
		defs, err := Parse(base(" mpc "))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if defs.Default.Name != "mpc" || defs.Default.Kind != TargetCustom {
			t.Errorf("Default = %+v, want mpc/TargetCustom", defs.Default)
		}
	})

	t.Run("group default", func(t *testing.T) {
		defs, err := Parse(base("g"))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if defs.Default.Kind != TargetGroup {
			t.Errorf("Default = %+v, want TargetGroup", defs.Default)
		}
	})

	t.Run("unknown default", func(t *testing.T) {
		_, err := Parse(base("nope"))
		if err == nil {
			t.Fatal("未知 default 应报错")
		}
		msg := err.Error()
		for _, sub := range []string{"query.default", `"nope"`, "client", "mpc", "g"} {
			if !strings.Contains(msg, sub) {
				t.Errorf("错误 %q 未包含 %q", msg, sub)
			}
		}
	})

	t.Run("session summary not referable", func(t *testing.T) {
		for _, def := range []string{"session", "summary"} {
			_, err := Parse(base(def))
			if err == nil {
				t.Errorf("default=%q 不应可引用", def)
			}
			if !strings.Contains(err.Error(), "query.default") {
				t.Errorf("错误 %q 未定位 query.default", err.Error())
			}
		}
	})

	t.Run("non-string default", func(t *testing.T) {
		_, err := Parse(Input{RawQuery: map[string]any{"default": []any{"mpc"}}})
		if err == nil {
			t.Fatal("非 string default 应报错")
		}
		if !strings.Contains(err.Error(), "query.default") {
			t.Errorf("错误 %q 未定位 query.default", err.Error())
		}
	})
}

// issues 非空时不论 RawQuery 是否为 nil 都拒绝,列出原始名称与类别,绝不回退 client。
func TestParse_TopLevelIssuesRejected(t *testing.T) {
	tests := []struct {
		name  string
		in    Input
		wantA string
		wantB string
	}{
		{
			name: "issues only",
			in: Input{RawQueryTopLevelIssues: map[string]TopLevelIssue{
				"Query": {Name: "Query", Kind: TopLevelNameConflict},
			}},
			wantA: `"Query"`,
			wantB: "name_conflict",
		},
		{
			name: "issues alongside raw",
			in: Input{
				RawQuery: map[string]any{"default": "mpc", "subqueries": map[string]any{"mpc": "model,provider"}},
				RawQueryTopLevelIssues: map[string]TopLevelIssue{
					"query": {Name: "query", Kind: TopLevelRootNotTable},
					"QUERY": {Name: "QUERY", Kind: TopLevelNameConflict},
				},
			},
			wantA: `"QUERY"`,
			wantB: "root_not_table",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defs, err := Parse(tt.in)
			if err == nil {
				t.Fatal("issues 非空必须拒绝,不得回退 client")
			}
			if defs != nil {
				t.Errorf("拒绝时不应返回定义: %+v", defs)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantA) {
				t.Errorf("错误未列出原始名称 %q: %q", tt.wantA, msg)
			}
			if !strings.Contains(msg, tt.wantB) {
				t.Errorf("错误未列出类别 %q: %q", tt.wantB, msg)
			}
		})
	}
}

// 输入 raw map/slice 在 Parse 后被修改时,QueryDefinitions 不改变(不共享引用)。
func TestParse_InputMutationDoesNotAffectDefinitions(t *testing.T) {
	raw := map[string]any{
		"default":    "g",
		"subqueries": map[string]any{"mpc": "model,provider"},
		"groups":     map[string]any{"g": "client,mpc"},
	}
	in := Input{RawQuery: raw}
	defs, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	snapshot := *defs

	raw["default"] = "mpc"
	delete(raw["subqueries"].(map[string]any), "mpc")
	delete(raw["groups"].(map[string]any), "g")

	if !reflect.DeepEqual(defs.Default, snapshot.Default) ||
		!reflect.DeepEqual(defs.Subqueries, snapshot.Subqueries) ||
		!reflect.DeepEqual(defs.Groups, snapshot.Groups) {
		t.Errorf("修改输入影响了已解析定义:\n got: %+v\nwant: %+v", defs, &snapshot)
	}
}

// 错误消息为双语,并包含具体 key 与值摘要。
func TestParse_ErrorMessagesBilingual(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"mpc": "model,unknown"},
	}})
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "query.subqueries.mpc") {
		t.Errorf("错误应含配置键: %q", msg)
	}
	if !strings.Contains(msg, "/") {
		t.Errorf("错误应为双语形态: %q", msg)
	}
	if !strings.Contains(msg, "无效项") && !strings.Contains(msg, "invalid") {
		t.Errorf("错误应含双语问题描述: %q", msg)
	}
}

// 较早条目的错误不扩散为后续合法定义的断开引用:
// aaa_bad(字节序在前)报错后,合法的 mpc 仍进入定义,
// group_q 与 default 不得被误报为未知引用。
func TestParse_EarlierErrorDoesNotPolluteLaterValidEntries(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"default":    "group_q",
		"subqueries": map[string]any{"aaa_bad": "model,", "mpc": "model,provider"},
		"groups":     map[string]any{"group_q": "client,mpc"},
	}})
	if err == nil {
		t.Fatal("aaa_bad 应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "query.subqueries.aaa_bad") {
		t.Errorf("错误应定位 aaa_bad: %q", msg)
	}
	for _, valid := range []string{"query.subqueries.mpc", "query.groups.group_q", "query.default"} {
		if strings.Contains(msg, valid) {
			t.Errorf("合法定义 %q 被错误扩散误报: %q", valid, msg)
		}
	}
}

// 错误子查询与合法组合混排时,只报错误条目本身。
func TestParse_EarlierErrorDoesNotPolluteLaterValidGroups(t *testing.T) {
	_, err := Parse(Input{RawQuery: map[string]any{
		"subqueries": map[string]any{"bad_sub": "model"},
		"groups": map[string]any{
			"aaa_bad_group": "client,bad_sub",
			"good_group":    "client,model",
		},
	}})
	if err == nil {
		t.Fatal("应报错(bad_sub 单维 + aaa_bad_group 断开引用)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "query.subqueries.bad_sub") || !strings.Contains(msg, "query.groups.aaa_bad_group") {
		t.Errorf("错误应只定位 bad_sub 与 aaa_bad_group: %q", msg)
	}
	if strings.Contains(msg, "query.groups.good_group") {
		t.Errorf("合法组合 good_group 被误报: %q", msg)
	}
}
