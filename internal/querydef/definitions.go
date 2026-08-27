// Package querydef 把 query 视图的 raw 配置状态解析为强类型、已校验的只读定义。
// 它是纯函数包:不读取文件、不打开数据库,也不依赖 internal/config;
// CLI 与 TUI 负责把 config raw 状态适配为本包的 Input。
package querydef

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// BuiltinDimension 是受控的内置聚合维度,自定义视图只能从中组合。
type BuiltinDimension string

const (
	DimensionClient   BuiltinDimension = "client"
	DimensionModel    BuiltinDimension = "model"
	DimensionProvider BuiltinDimension = "provider"
	DimensionProject  BuiltinDimension = "project"
)

// builtinDimensions 是唯一允许的维度集合(亦用于错误信息中的允许集合)。
var builtinDimensions = []BuiltinDimension{
	DimensionClient, DimensionModel, DimensionProvider, DimensionProject,
}

func isBuiltinDimension(s string) bool {
	for _, d := range builtinDimensions {
		if string(d) == s {
			return true
		}
	}
	return false
}

// IsBuiltinDimension 报告 name 是否为内置维度(供调用方区分目标来源)。
func IsBuiltinDimension(name string) bool {
	return isBuiltinDimension(name)
}

func builtinDimensionList() string {
	parts := make([]string, len(builtinDimensions))
	for i, d := range builtinDimensions {
		parts[i] = string(d)
	}
	return strings.Join(parts, ", ")
}

// reservedNameOrder 是保留名的有序切片,也是错误文案中展示顺序的唯一来源:
// 六个内置视图名与 custom/list 两个固定入口名。
var reservedNameOrder = []string{
	"client", "model", "provider", "project", "session", "summary", "custom", "list",
}

// reservedNames 由 reservedNameOrder 派生的成员集合,供语义判断使用。
var reservedNames = func() map[string]bool {
	set := make(map[string]bool, len(reservedNameOrder))
	for _, name := range reservedNameOrder {
		set[name] = true
	}
	return set
}()

// IsReservedName 报告 name 是否为 query 视图保留名(内置视图名与 custom/list
// 固定入口)。TUI 即时校验等外部调用方应使用本谓词,不复制名单。
func IsReservedName(name string) bool {
	return reservedNames[name]
}

func reservedNameList() string {
	return strings.Join(reservedNameOrder, ", ")
}

// namePattern 自定义名:小写 ASCII 标识符,首字符为字母,后续允许字母、数字、_、-。
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// 顶层问题项类别(与 config 侧 raw 状态的类别字符串一致,由适配层直接转换)。
const (
	TopLevelNameConflict = "name_conflict"
	TopLevelRootNotTable = "root_not_table"
)

// TopLevelIssue 是一个 query 顶层问题项的名称与类别。
type TopLevelIssue struct {
	Name string
	Kind string
}

// Input 是解析输入:config raw query 状态的独立 DTO(本包不依赖 config 类型)。
type Input struct {
	RawQuery               map[string]any
	RawQueryTopLevelIssues map[string]TopLevelIssue
}

// TargetKind 区分可执行目标的三种来源。
type TargetKind int

const (
	TargetBuiltin TargetKind = iota
	TargetCustom
	TargetGroup
)

// Target 是一个可执行查询目标:内置单维视图、自定义多维表或组合查询。
type Target struct {
	Name string
	Kind TargetKind
}

// CustomSubquery 是一个自定义多维子查询;Dimensions 的声明顺序即维度列顺序。
type CustomSubquery struct {
	Name       string
	Dimensions []BuiltinDimension
}

// QueryGroup 是一个组合查询;Items 的声明顺序即输出表顺序。
type QueryGroup struct {
	Name  string
	Items []Target
}

// QueryDefinitions 是解析成功的只读执行图,不与输入 raw map/slice 共享任何引用。
// Subqueries/Groups 按名称字节序稳定排列;两者的内部声明顺序逐项保留。
// DefaultIsFallback 表示默认项来自缺失/空白 query.default 的内置回退:
// true 时 Default 恒为 client;显式配置的 default(含内置视图)不标记回退。
type QueryDefinitions struct {
	Default           Target
	DefaultIsFallback bool
	Subqueries        []CustomSubquery
	Groups            []QueryGroup
}

// Parse 把 raw query 状态解析为已验证的定义。
//
// 顶层问题项非空时,不论 RawQuery 是否为 nil 都拒绝并列出全部原始名称与类别,
// 绝不把未配置误当作回退 client;未配置或空段时 Default 等价 client。
// 错误信息为双语,定位到具体配置键并给出值摘要与允许集合。
func Parse(in Input) (*QueryDefinitions, error) {
	if len(in.RawQueryTopLevelIssues) > 0 {
		names := make([]string, 0, len(in.RawQueryTopLevelIssues))
		for name := range in.RawQueryTopLevelIssues {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%q (%s)", name, in.RawQueryTopLevelIssues[name].Kind))
		}
		return nil, errors.New(ui.Bi(
			fmt.Sprintf("query config has invalid top-level entries: %s; remove or fix them in config.toml", strings.Join(parts, ", ")),
			fmt.Sprintf("query 配置存在顶层问题项: %s;请在 config.toml 中删除或修复", strings.Join(parts, ", ")),
		))
	}

	var errs []error

	// 未知顶层键。
	for _, key := range sortedKeys(in.RawQuery) {
		switch key {
		case "default", "subqueries", "groups":
		default:
			errs = append(errs, errors.New(ui.Bi(
				fmt.Sprintf("unknown query key %q (allowed: default, subqueries, groups)", "query."+key),
				fmt.Sprintf("未知 query 配置键 %q(允许: default, subqueries, groups)", "query."+key),
			)))
		}
	}

	// 跨表重名:subqueries 与 groups 的名称必须全局唯一(default 引用不允许歧义)。
	if subTable, ok := in.RawQuery["subqueries"].(map[string]any); ok {
		if groupTable, ok := in.RawQuery["groups"].(map[string]any); ok {
			for _, name := range sortedKeys(groupTable) {
				if _, dup := subTable[name]; dup {
					errs = append(errs, errors.New(ui.Bi(
						fmt.Sprintf("duplicate definition name %q in query.subqueries and query.groups", name),
						fmt.Sprintf("query.subqueries 与 query.groups 中存在重复名称 %q", name),
					)))
				}
			}
		}
	}

	subs, subErrs := parseSubqueries(in.RawQuery["subqueries"])
	errs = append(errs, subErrs...)
	groups, groupErrs := parseGroups(in.RawQuery["groups"], subs)
	errs = append(errs, groupErrs...)
	defTarget, defFallback, defErr := parseDefault(in.RawQuery["default"], subs, groups)
	if defErr != nil {
		errs = append(errs, defErr)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &QueryDefinitions{
		Default:           defTarget,
		DefaultIsFallback: defFallback,
		Subqueries:        subs,
		Groups:            groups,
	}, nil
}

// parseSubqueries 解析 query.subqueries;名称合法、值必须是 CSV 字符串。
func parseSubqueries(raw any) ([]CustomSubquery, []error) {
	if raw == nil {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, []error{typeError("query.subqueries", raw, "a table / 一个表")}
	}
	var errs []error
	subs := make([]CustomSubquery, 0, len(table))
	for _, name := range sortedKeys(table) {
		path := "query.subqueries." + name
		if err := checkDefinitionName("query.subqueries", name); err != nil {
			errs = append(errs, err)
			continue
		}
		csv, ok := table[name].(string)
		if !ok {
			errs = append(errs, typeError(path, table[name], "a comma-separated string / 一个逗号分隔字符串"))
			continue
		}
		// 错误判断按当前条目局部化:先出现条目的错误不得让后续合法定义被跳过,
		// 否则合法子查询会被连带报告为断开引用。
		before := len(errs)
		items := splitCSV(csv)
		var dims []BuiltinDimension
		seen := map[string]bool{}
		for _, item := range items {
			if !isBuiltinDimension(item) {
				errs = append(errs, itemError(path, item, builtinDimensionList()))
				continue
			}
			if seen[item] {
				errs = append(errs, duplicateItemError(path, item))
				continue
			}
			seen[item] = true
			dims = append(dims, BuiltinDimension(item))
		}
		if len(errs) > before {
			continue
		}
		if len(items) < 2 {
			errs = append(errs, errors.New(ui.Bi(
				fmt.Sprintf("%s requires at least 2 dimensions (got %d: %q)", path, len(items), csv),
				fmt.Sprintf("%s 至少需要 2 个维度(当前 %d 个: %q)", path, len(items), csv),
			)))
			continue
		}
		subs = append(subs, CustomSubquery{Name: name, Dimensions: dims})
	}
	// 名称排序输出保证确定性。
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	return subs, errs
}

// parseGroups 解析 query.groups;成员只能是内置维度或已定义的自定义子查询,禁止嵌套引用。
func parseGroups(raw any, subs []CustomSubquery) ([]QueryGroup, []error) {
	if raw == nil {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, []error{typeError("query.groups", raw, "a table / 一个表")}
	}
	subNames := map[string]bool{}
	for _, s := range subs {
		subNames[s.Name] = true
	}
	groupNames := map[string]bool{}
	for _, name := range sortedKeys(table) {
		groupNames[name] = true
	}

	var errs []error
	groups := make([]QueryGroup, 0, len(table))
	for _, name := range sortedKeys(table) {
		path := "query.groups." + name
		if err := checkDefinitionName("query.groups", name); err != nil {
			errs = append(errs, err)
			continue
		}
		csv, ok := table[name].(string)
		if !ok {
			errs = append(errs, typeError(path, table[name], "a comma-separated string / 一个逗号分隔字符串"))
			continue
		}
		// 同 parseSubqueries:错误判断按当前条目局部化,不因先出现条目的错误跳过本条目。
		before := len(errs)
		items := splitCSV(csv)
		var targets []Target
		seen := map[string]bool{}
		for _, item := range items {
			switch {
			case isBuiltinDimension(item):
				if seen[item] {
					errs = append(errs, duplicateItemError(path, item))
					continue
				}
				seen[item] = true
				targets = append(targets, Target{Name: item, Kind: TargetBuiltin})
			case subNames[item]:
				if seen[item] {
					errs = append(errs, duplicateItemError(path, item))
					continue
				}
				seen[item] = true
				targets = append(targets, Target{Name: item, Kind: TargetCustom})
			case groupNames[item]:
				errs = append(errs, errors.New(ui.Bi(
					fmt.Sprintf("%s: group cannot reference another group %q (groups may only contain built-in views (%s) and defined subqueries)", path, item, builtinDimensionList()),
					fmt.Sprintf("%s: 组合查询不能引用另一个组合查询 %q(组合成员只能是内置视图(%s)或已定义的自定义子查询)", path, item, builtinDimensionList()),
				)))
			default:
				errs = append(errs, itemError(path, item, builtinDimensionList()+", 以及已定义的 subqueries"))
			}
		}
		if len(errs) > before {
			continue
		}
		if len(items) < 2 {
			errs = append(errs, errors.New(ui.Bi(
				fmt.Sprintf("%s requires at least 2 items (got %d: %q)", path, len(items), csv),
				fmt.Sprintf("%s 至少需要 2 个成员(当前 %d 个: %q)", path, len(items), csv),
			)))
			continue
		}
		groups = append(groups, QueryGroup{Name: name, Items: targets})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, errs
}

// parseDefault 解析 query.default:TrimSpace 后匹配内置/自定义/组合;缺失、nil 或
// 空白回退 client,并以后续布尔值标记这一回退来源。
func parseDefault(raw any, subs []CustomSubquery, groups []QueryGroup) (Target, bool, error) {
	if raw == nil {
		return Target{Name: string(DimensionClient), Kind: TargetBuiltin}, true, nil
	}
	text, ok := raw.(string)
	if !ok {
		return Target{}, false, typeError("query.default", raw, "a string / 一个字符串")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Target{Name: string(DimensionClient), Kind: TargetBuiltin}, true, nil
	}
	if isBuiltinDimension(trimmed) {
		return Target{Name: trimmed, Kind: TargetBuiltin}, false, nil
	}
	for _, s := range subs {
		if s.Name == trimmed {
			return Target{Name: trimmed, Kind: TargetCustom}, false, nil
		}
	}
	for _, g := range groups {
		if g.Name == trimmed {
			return Target{Name: trimmed, Kind: TargetGroup}, false, nil
		}
	}
	allowed := builtinDimensionList()
	for _, s := range subs {
		allowed += ", " + s.Name
	}
	for _, g := range groups {
		allowed += ", " + g.Name
	}
	return Target{}, false, errors.New(ui.Bi(
		fmt.Sprintf("unknown query.default %q (allowed: %s)", trimmed, allowed),
		fmt.Sprintf("未知的 query.default %q(允许: %s)", trimmed, allowed),
	))
}

// checkDefinitionName 校验自定义/组合查询名:小写标识符且不与保留名冲突;
// 保留名错误列表从 reservedNameOrder 有序生成,不另写名单。
func checkDefinitionName(section, name string) error {
	path := section + "." + name
	if IsReservedName(name) {
		return errors.New(ui.Bi(
			fmt.Sprintf("reserved name in %s: %q (reserved: %s)", path, name, reservedNameList()),
			fmt.Sprintf("%s 使用了保留名 %q(保留: %s)", path, name, reservedNameList()),
		))
	}
	if !namePattern.MatchString(name) {
		return errors.New(ui.Bi(
			fmt.Sprintf("invalid name in %s: %q (lowercase identifier: letter first, then letters, digits, _ or -)", path, name),
			fmt.Sprintf("%s 中的名称 %q 不合法(小写标识符: 首字符为字母,后续为字母、数字、_ 或 -)", path, name),
		))
	}
	return nil
}

// splitCSV 按逗号分段并对每段 TrimSpace;空串/空段原样保留,由调用方按路径报错。
func splitCSV(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

// itemError 报告空段或未知项,携带值摘要与允许集合(空段即值摘要为 "" 的无效项)。
func itemError(path, item, allowed string) error {
	return errors.New(ui.Bi(
		fmt.Sprintf("invalid item %q in %q (allowed: %s)", item, path, allowed),
		fmt.Sprintf("%q 中的无效项 %q(允许: %s)", path, item, allowed),
	))
}

func duplicateItemError(path, item string) error {
	return errors.New(ui.Bi(
		fmt.Sprintf("duplicate item %q in %q", item, path),
		fmt.Sprintf("%q 存在重复项 %q", path, item),
	))
}

// typeError 报告值类型不匹配,含路径、实际类型摘要与期望形态。
func typeError(path string, value any, want string) error {
	return errors.New(ui.Bi(
		fmt.Sprintf("%s must be %s (got %T: %v)", path, want, value, summarize(value)),
		fmt.Sprintf("%s 必须是 %s(实际 %T: %v)", path, want, value, summarize(value)),
	))
}

// summarize 生成错误信息用的值摘要(限长,避免整个子树刷屏)。
func summarize(value any) string {
	switch v := value.(type) {
	case string:
		if len(v) > 32 {
			return v[:32] + "..."
		}
		return v
	case []any:
		return fmt.Sprintf("[%d items]", len(v))
	case map[string]any:
		return fmt.Sprintf("{%d keys}", len(v))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
