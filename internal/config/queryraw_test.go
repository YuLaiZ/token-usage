package config

import (
	"reflect"
	"testing"
	"time"
)

// mustParseQueryRaw 解析 TOML 并断言成功,便于四态断言聚焦在 raw 载体上。
func mustParseQueryRaw(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := ParseUserConfig([]byte(content))
	if err != nil {
		t.Fatalf("ParseUserConfig: %v", err)
	}
	return cfg
}

// assertExclusive 断言两个 raw 载体互斥:任一配置快照只能有一个非空。
func assertExclusive(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.RawQuery != nil && cfg.RawQueryTopLevelIssues != nil {
		t.Fatalf("RawQuery 与 RawQueryTopLevelIssues 不得同时非 nil: %#v / %#v", cfg.RawQuery, cfg.RawQueryTopLevelIssues)
	}
}

// 缺失 query 顶层项:两个 raw 载体均为 nil(不是空 map),行为等价未配置。
func TestParseUserConfig_QueryAbsentLeavesCarriersNil(t *testing.T) {
	cfg := mustParseQueryRaw(t, "data_dir = \"/tmp/x\"\n[clients.codex]\nenabled = true\n")
	if cfg.RawQuery != nil {
		t.Errorf("RawQuery 应为 nil,实际 %#v", cfg.RawQuery)
	}
	if cfg.RawQueryTopLevelIssues != nil {
		t.Errorf("RawQueryTopLevelIssues 应为 nil,实际 %#v", cfg.RawQueryTopLevelIssues)
	}
	assertExclusive(t, cfg)
}

// 唯一精确小写 [query] 且值为表:只填 RawQuery,内部原始键大小写与嵌套值类型保留。
func TestParseUserConfig_QueryValidTableFillsRawQuery(t *testing.T) {
	cfg := mustParseQueryRaw(t, "[query]\ndefault = \"mpc\"\n[query.subqueries]\nmpc = \"model,provider,client\"\n")
	if cfg.RawQueryTopLevelIssues != nil {
		t.Fatalf("合法 query 不应产生 issues: %#v", cfg.RawQueryTopLevelIssues)
	}
	if cfg.RawQuery == nil {
		t.Fatal("RawQuery 不应为 nil")
	}
	if got := cfg.RawQuery["default"]; got != "mpc" {
		t.Errorf("RawQuery[default] = %#v, want \"mpc\"", got)
	}
	sub, ok := cfg.RawQuery["subqueries"].(map[string]any)
	if !ok {
		t.Fatalf("RawQuery[subqueries] 应为 map,实际 %T", cfg.RawQuery["subqueries"])
	}
	if got := sub["mpc"]; got != "model,provider,client" {
		t.Errorf("subqueries[mpc] = %#v", got)
	}
	assertExclusive(t, cfg)
}

// query 内部键大小写不被归一:大写内部名、混合类型数组、嵌套子表原样保留。
func TestParseUserConfig_QueryPreservesInnerKeyCaseAndValueTypes(t *testing.T) {
	cfg := mustParseQueryRaw(t, "[query]\nDefault = \"x\"\nMixed = [1, \"two\", true]\nWhen = 1979-05-27T07:32:00Z\n[query.Sub]\nInner = 2.5\n")
	if _, ok := cfg.RawQuery["default"]; ok {
		t.Error("内部键不应被小写归一(default 与 Default 是不同定义)")
	}
	if got := cfg.RawQuery["Default"]; got != "x" {
		t.Errorf("RawQuery[Default] = %#v, want \"x\"", got)
	}
	wantArr := []any{int64(1), "two", true}
	if got, ok := cfg.RawQuery["Mixed"].([]any); !ok || !reflect.DeepEqual(got, wantArr) {
		t.Errorf("RawQuery[Mixed] = %#v, want %#v", cfg.RawQuery["Mixed"], wantArr)
	}
	if _, ok := cfg.RawQuery["When"].(time.Time); !ok {
		t.Errorf("RawQuery[When] 应保留 time.Time,实际 %T", cfg.RawQuery["When"])
	}
	sub, ok := cfg.RawQuery["Sub"].(map[string]any)
	if !ok {
		t.Fatalf("RawQuery[Sub] 应为 map,实际 %T", cfg.RawQuery["Sub"])
	}
	if got := sub["Inner"]; got != float64(2.5) {
		t.Errorf("Sub[Inner] = %#v, want 2.5", got)
	}
}

// 顶层大小写变体并存(或单独存在):全部相关原始项进 issues,类别 name_conflict。
func TestParseUserConfig_QueryNameConflictVariants(t *testing.T) {
	tests := []struct {
		name    string
		content string
		names   []string
	}{
		{
			name:    "two variants",
			content: "[query]\ndefault = \"a\"\n[Query]\ndefault = \"b\"\n",
			names:   []string{"query", "Query"},
		},
		{
			name:    "three variants",
			content: "[query]\na = 1\n[Query]\nb = 2\n[QUERY]\nc = 3\n",
			names:   []string{"query", "Query", "QUERY"},
		},
		{
			name:    "single non-lowercase variant",
			content: "[Query]\ndefault = \"b\"\n",
			names:   []string{"Query"},
		},
		{
			name:    "two uppercase variants without lowercase",
			content: "[Query]\nb = 2\n[QUERY]\nc = 3\n",
			names:   []string{"Query", "QUERY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParseQueryRaw(t, tt.content)
			if cfg.RawQuery != nil {
				t.Fatalf("名称冲突时 RawQuery 必须为 nil,实际 %#v", cfg.RawQuery)
			}
			issues := cfg.RawQueryTopLevelIssues
			if issues == nil {
				t.Fatal("RawQueryTopLevelIssues 不应为 nil")
			}
			if len(issues) != len(tt.names) {
				t.Fatalf("issues 数量 = %d, want %d: %#v", len(issues), len(tt.names), issues)
			}
			for _, name := range tt.names {
				issue, ok := issues[name]
				if !ok {
					t.Fatalf("issues 缺少原始名称 %q: %#v", name, issues)
				}
				if issue.Name != name {
					t.Errorf("issue.Name = %q, want %q", issue.Name, name)
				}
				if issue.Kind != RawQueryIssueNameConflict {
					t.Errorf("issues[%q].Kind = %q, want name_conflict", name, issue.Kind)
				}
				if _, ok := issue.Value.(map[string]any); !ok {
					t.Errorf("issues[%q].Value 应保留原始表值,实际 %T", name, issue.Value)
				}
			}
			assertExclusive(t, cfg)
		})
	}
}

// 唯一精确小写 query 但值不是表:类别 root_not_table;与变体并存时逐项分别标注。
func TestParseUserConfig_QueryRootNotTable(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "string", content: "query = \"x\"\n"},
		{name: "empty array", content: "query = []\n"},
		{name: "array", content: "query = [\"a\", \"b\"]\n"},
		{name: "int", content: "query = 42\n"},
		{name: "datetime", content: "query = 1979-05-27T07:32:00Z\n"},
		{
			// 非表值与变体并存:精确小写项标 root_not_table,变体项标 name_conflict。
			name:    "scalar plus variant",
			content: "query = \"x\"\n[Query]\ndefault = \"b\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParseQueryRaw(t, tt.content)
			if cfg.RawQuery != nil {
				t.Fatalf("根值非表时 RawQuery 必须为 nil,实际 %#v", cfg.RawQuery)
			}
			issues := cfg.RawQueryTopLevelIssues
			if issues == nil {
				t.Fatal("RawQueryTopLevelIssues 不应为 nil")
			}
			queryIssue, ok := issues["query"]
			if !ok {
				t.Fatalf("issues 缺少 \"query\": %#v", issues)
			}
			if queryIssue.Kind != RawQueryIssueRootNotTable {
				t.Errorf("issues[query].Kind = %q, want root_not_table", queryIssue.Kind)
			}
			if queryIssue.Name != "query" {
				t.Errorf("issues[query].Name = %q, want \"query\"", queryIssue.Name)
			}
			if variant, ok := issues["Query"]; ok {
				if variant.Kind != RawQueryIssueNameConflict {
					t.Errorf("issues[Query].Kind = %q, want name_conflict", variant.Kind)
				}
			}
			assertExclusive(t, cfg)
		})
	}
}

// query = [] 的空数组值按原始形态保留在 issue 中。
func TestParseUserConfig_QueryRootNotTablePreservesEmptyArray(t *testing.T) {
	cfg := mustParseQueryRaw(t, "query = []\n")
	issue := cfg.RawQueryTopLevelIssues["query"]
	got, ok := issue.Value.([]any)
	if !ok || len(got) != 0 {
		t.Errorf("issue.Value 应为空数组,实际 %#v", issue.Value)
	}
	if _, isNil := issue.Value.([]any); !isNil {
		t.Errorf("issue.Value 类型断言失败: %T", issue.Value)
	}
}

// 内部数组、嵌套表、未知子键保持 raw,解析不失败(语义校验延迟到 query 子系统)。
func TestParseUserConfig_QueryInnerUnknownShapesStayRaw(t *testing.T) {
	cfg := mustParseQueryRaw(t, "data_dir = \"/tmp/x\"\n[query]\nunknown_key = [1, 2]\n[query.nested]\ninner = \"v\"\n[query.subqueries]\nMPC = \"model\"\n")
	if cfg.RawQueryTopLevelIssues != nil {
		t.Fatalf("内部形态不产生顶层 issues: %#v", cfg.RawQueryTopLevelIssues)
	}
	if got, ok := cfg.RawQuery["unknown_key"].([]any); !ok || len(got) != 2 {
		t.Errorf("unknown_key 应保留数组,实际 %#v", cfg.RawQuery["unknown_key"])
	}
	nested, ok := cfg.RawQuery["nested"].(map[string]any)
	if !ok || nested["inner"] != "v" {
		t.Errorf("nested 应保留子表,实际 %#v", cfg.RawQuery["nested"])
	}
	sub, ok := cfg.RawQuery["subqueries"].(map[string]any)
	if !ok {
		t.Fatalf("subqueries 应为 map,实际 %T", cfg.RawQuery["subqueries"])
	}
	if _, ok := sub["mpc"]; ok {
		t.Error("内部键 MPC 不应被小写归一")
	}
	if got := sub["MPC"]; got != "model" {
		t.Errorf("subqueries[MPC] = %#v, want \"model\"", got)
	}
}

// 对内存 draft 的重分类复用初始四态规则。
func TestReclassifyRawQuery_FourStates(t *testing.T) {
	lowerTable := map[string]any{"default": "a"}
	upperTable := map[string]any{"default": "b"}

	t.Run("delete uppercase variant promotes lowercase table", func(t *testing.T) {
		cfg := &Config{}
		ReclassifyRawQuery(cfg, map[string]any{"query": lowerTable, "Query": upperTable})
		if cfg.RawQuery != nil || cfg.RawQueryTopLevelIssues == nil {
			t.Fatalf("删除前应为问题态: %#v / %#v", cfg.RawQuery, cfg.RawQueryTopLevelIssues)
		}
		ReclassifyRawQuery(cfg, map[string]any{"query": lowerTable})
		if cfg.RawQueryTopLevelIssues != nil {
			t.Fatalf("唯一精确小写表应转正,issues 应清空: %#v", cfg.RawQueryTopLevelIssues)
		}
		if !reflect.DeepEqual(cfg.RawQuery, lowerTable) {
			t.Errorf("转正后 RawQuery = %#v, want %#v", cfg.RawQuery, lowerTable)
		}
	})

	t.Run("delete lowercase table never silently falls back", func(t *testing.T) {
		cfg := &Config{}
		ReclassifyRawQuery(cfg, map[string]any{"Query": upperTable})
		if cfg.RawQuery != nil {
			t.Fatalf("仅剩大写变体时 RawQuery 必须为 nil(不得静默回退),实际 %#v", cfg.RawQuery)
		}
		issue, ok := cfg.RawQueryTopLevelIssues["Query"]
		if !ok {
			t.Fatalf("大写变体应保持 issue: %#v", cfg.RawQueryTopLevelIssues)
		}
		if issue.Kind != RawQueryIssueNameConflict {
			t.Errorf("issue.Kind = %q, want name_conflict", issue.Kind)
		}
	})

	t.Run("empty entries clear both carriers", func(t *testing.T) {
		cfg := &Config{}
		ReclassifyRawQuery(cfg, map[string]any{"query": lowerTable, "Query": upperTable})
		ReclassifyRawQuery(cfg, nil)
		if cfg.RawQuery != nil || cfg.RawQueryTopLevelIssues != nil {
			t.Fatalf("空 entries 应等价未配置: %#v / %#v", cfg.RawQuery, cfg.RawQueryTopLevelIssues)
		}
	})

	t.Run("non-table root value stays root_not_table", func(t *testing.T) {
		cfg := &Config{}
		ReclassifyRawQuery(cfg, map[string]any{"query": "x"})
		if cfg.RawQuery != nil {
			t.Fatalf("非表根值不得进入 RawQuery: %#v", cfg.RawQuery)
		}
		issue := cfg.RawQueryTopLevelIssues["query"]
		if issue.Kind != RawQueryIssueRootNotTable {
			t.Errorf("issue.Kind = %q, want root_not_table", issue.Kind)
		}
		if issue.Value != "x" {
			t.Errorf("issue.Value = %#v, want \"x\"", issue.Value)
		}
	})
}
