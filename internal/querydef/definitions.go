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

// DiagnosticKind 是 query 配置诊断的封闭类别集合。Kind 是稳定契约:
// TUI 恢复项按 Path+Kind 构建恢复动作,不得以错误文本子串判断归属。
type DiagnosticKind string

const (
	// KindTopLevelProblem:顶层键名冲突或根值非表(共同前置,两载体互斥时由
	// ReclassifyRawQuery 转正或清除)。
	KindTopLevelProblem DiagnosticKind = "top_level_problem"
	// KindUnknownQueryKey:query 段内的未知顶层键(允许集合: default, subqueries, groups, output)。
	KindUnknownQueryKey DiagnosticKind = "unknown_query_key"
	// KindViewsTableType:subqueries/groups 本身不是表。
	KindViewsTableType DiagnosticKind = "views_table_type"
	// KindDefinitionName:定义名称保留名冲突或非法形态。
	KindDefinitionName DiagnosticKind = "definition_name"
	// KindDefinitionValueType:定义值不是 CSV 字符串。
	KindDefinitionValueType DiagnosticKind = "definition_value_type"
	// KindDefinitionItem:成员未知,或组合成员引用另一组合(恢复动作均为删除该条目后重建)。
	KindDefinitionItem DiagnosticKind = "definition_item"
	// KindDuplicateItem:成员重复。
	KindDuplicateItem DiagnosticKind = "duplicate_item"
	// KindMinimumItems:成员数不足下限。
	KindMinimumItems DiagnosticKind = "minimum_items"
	// KindCrossTableDuplicate:subqueries 与 groups 跨表重名(主路径固定 query.groups.<name>)。
	KindCrossTableDuplicate DiagnosticKind = "cross_table_duplicate"
	// KindDefault:query.default 非字符串或目标未知。
	KindDefault DiagnosticKind = "default"
	// KindOutputTableType:query.output 不是表。
	KindOutputTableType DiagnosticKind = "output_table_type"
	// KindOutputUnknownKey:query.output 含未知子键(恢复统一删除整表后重建)。
	KindOutputUnknownKey DiagnosticKind = "output_unknown_key"
	// KindColumnsType:query.output.columns 不是数组。
	KindColumnsType DiagnosticKind = "columns_type"
	// KindColumnsElement:元素非字符串、空白或未知 ID。
	KindColumnsElement DiagnosticKind = "columns_element"
	// KindColumnsDuplicate:重复列 ID。
	KindColumnsDuplicate DiagnosticKind = "columns_duplicate"
	// KindColumnsEmpty:空数组(不是恢复默认,恢复默认应删除该段)。
	KindColumnsEmpty DiagnosticKind = "columns_empty"
)

// Diagnostic 是一条结构化配置诊断:定位路径、稳定类别与双语渲染文案。
type Diagnostic struct {
	Path    string
	Kind    DiagnosticKind
	Message string
}

// ValidationError 有序聚合多个 query 配置问题。Error() 逐行拼接诊断文案,
// 与既有 errors.Join 的多错误输出形态一致,CLI 直接打印无需适配。
type ValidationError struct {
	Issues []Diagnostic
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Issues))
	for i, d := range e.Issues {
		msgs[i] = d.Message
	}
	return strings.Join(msgs, "\n")
}

func validationError(issues []Diagnostic) error {
	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues}
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

// ViewDefinitions 是视图定义部分的解析结果(default、subqueries、groups),
// 不含输出布局。ParseViews 成功时返回。
type ViewDefinitions struct {
	Default           Target
	DefaultIsFallback bool
	Subqueries        []CustomSubquery
	Groups            []QueryGroup
}

// QueryDefinitions 是解析成功的只读执行图,不与输入 raw map/slice 共享任何引用。
// Subqueries/Groups 按名称字节序稳定排列;两者的内部声明顺序逐项保留。
// DefaultIsFallback 表示默认项来自缺失/空白 query.default 的内置回退:
// true 时 Default 恒为 client;显式配置的 default(含内置视图)不标记回退。
// OutputColumns 是已校验的输出指标 ID 序列(缺失时为默认七列的独立副本)。
type QueryDefinitions struct {
	ViewDefinitions
	OutputColumns []string
}

// Parse 把 raw query 状态解析为已验证的完整定义(视图 + 输出布局)。
//
// 诊断顺序固定:顶层问题存在时按原始顶层键名排序并独占返回;
// 否则先保留视图部分(未知顶层键、跨表重名、subqueries、groups、default)
// 的既有确定顺序,再追加 output 诊断(表类型、未知子键按 key 排序、
// columns 类型、元素按数组顺序、重复、空数组)。
// 错误信息为双语,定位到具体配置键并给出值摘要与允许集合。
func Parse(in Input) (*QueryDefinitions, error) {
	if issue := topLevelDiagnostics(in); issue != nil {
		return nil, validationError(issue)
	}

	var issues []Diagnostic
	views, viewsErr := ParseViews(in)
	if viewsErr != nil {
		var ve *ValidationError
		if errors.As(viewsErr, &ve) {
			issues = append(issues, ve.Issues...)
		} else {
			return nil, viewsErr
		}
	}
	cols, colsErr := ParseOutputLayout(in)
	if colsErr != nil {
		var ve *ValidationError
		if errors.As(colsErr, &ve) {
			issues = append(issues, ve.Issues...)
		} else {
			return nil, colsErr
		}
	}
	if err := validationError(issues); err != nil {
		return nil, err
	}
	return &QueryDefinitions{ViewDefinitions: *views, OutputColumns: cols}, nil
}

// ParseViews 只验证视图定义(default、subqueries、groups)与除 output 外的
// 未知 query 顶层键,忽略 output 自身的合法/非法形态。顶层问题态返回
// 共同前置诊断,不返回视图定义。
func ParseViews(in Input) (*ViewDefinitions, error) {
	if issue := topLevelDiagnostics(in); issue != nil {
		return nil, validationError(issue)
	}

	var issues []Diagnostic

	// 未知顶层键(output 是合法键,不在此报)。
	for _, key := range sortedKeys(in.RawQuery) {
		switch key {
		case "default", "subqueries", "groups", "output":
		default:
			issues = append(issues, diag(
				"query."+key,
				KindUnknownQueryKey,
				ui.Bi(
					fmt.Sprintf("unknown query key %q (allowed: default, subqueries, groups, output)", "query."+key),
					fmt.Sprintf("未知 query 配置键 %q(允许: default, subqueries, groups, output)", "query."+key),
				),
			))
		}
	}

	// 跨表重名:subqueries 与 groups 的名称必须全局唯一(default 引用不允许歧义)。
	// 主路径固定呈现为 query.groups.<name>,恢复动作删除该 group 以保留同名子查询。
	if subTable, ok := in.RawQuery["subqueries"].(map[string]any); ok {
		if groupTable, ok := in.RawQuery["groups"].(map[string]any); ok {
			for _, name := range sortedKeys(groupTable) {
				if _, dup := subTable[name]; dup {
					issues = append(issues, diag(
						"query.groups."+name,
						KindCrossTableDuplicate,
						ui.Bi(
							fmt.Sprintf("duplicate definition name %q in query.subqueries and query.groups", name),
							fmt.Sprintf("query.subqueries 与 query.groups 中存在重复名称 %q", name),
						),
					))
				}
			}
		}
	}

	subs, subIssues := parseSubqueries(in.RawQuery["subqueries"])
	issues = append(issues, subIssues...)
	groups, groupIssues := parseGroups(in.RawQuery["groups"], subs)
	issues = append(issues, groupIssues...)
	defTarget, defFallback, defIssue := parseDefault(in.RawQuery["default"], subs, groups)
	if defIssue != nil {
		issues = append(issues, *defIssue)
	}

	if err := validationError(issues); err != nil {
		return nil, err
	}
	return &ViewDefinitions{
		Default:           defTarget,
		DefaultIsFallback: defFallback,
		Subqueries:        subs,
		Groups:            groups,
	}, nil
}

// ParseOutputLayout 只验证 query.output,忽略视图定义与其他顶层键的
// 合法/非法形态。缺失 query.output 或缺失 columns 时返回默认七列的
// 独立副本。顶层问题态返回共同前置诊断,不返回可写布局。
func ParseOutputLayout(in Input) ([]string, error) {
	if issue := topLevelDiagnostics(in); issue != nil {
		return nil, validationError(issue)
	}

	raw := in.RawQuery["output"]
	if raw == nil {
		return ui.DefaultOutputColumns(), nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, validationError([]Diagnostic{typeDiagnostic("query.output", raw, "a table / 一个表", KindOutputTableType)})
	}

	var issues []Diagnostic
	// 未知子键按键名排序在前;恢复动作统一删除整张 query.output 表后重建。
	for _, key := range sortedKeys(table) {
		if key == "columns" {
			continue
		}
		issues = append(issues, diag(
			"query.output."+key,
			KindOutputUnknownKey,
			ui.Bi(
				fmt.Sprintf("unknown key %q in query.output (only \"columns\" is allowed)", key),
				fmt.Sprintf("query.output 中的未知键 %q(只允许 \"columns\")", key),
			),
		))
	}

	columnsRaw, hasColumns := table["columns"]
	if !hasColumns {
		if err := validationError(issues); err != nil {
			return nil, err
		}
		return ui.DefaultOutputColumns(), nil
	}
	items, ok := columnsRaw.([]any)
	if !ok {
		issues = append(issues, typeDiagnostic("query.output.columns", columnsRaw,
			"an array of strings / 一个字符串数组", KindColumnsType))
		return nil, validationError(issues)
	}

	cols := make([]string, 0, len(items))
	seen := map[string]bool{}
	// index 用数组枚举下标而非已收列数:被拒元素不进入 cols,否则后续 index 偏小。
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			issues = append(issues, diag(
				"query.output.columns",
				KindColumnsElement,
				ui.Bi(
					fmt.Sprintf("invalid element at index %d in query.output.columns: must be a string (got %T: %v)", index, item, summarize(item)),
					fmt.Sprintf("query.output.columns 第 %d 个元素必须是字符串(实际 %T: %v)", index, item, summarize(item)),
				),
			))
			continue
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			issues = append(issues, diag(
				"query.output.columns",
				KindColumnsElement,
				ui.Bi(
					fmt.Sprintf("invalid element %q at index %d in query.output.columns (allowed: %s)", text, index, ui.OutputColumnIDList()),
					fmt.Sprintf("query.output.columns 第 %d 个元素 %q 无效(允许: %s)", index, text, ui.OutputColumnIDList()),
				),
			))
			continue
		}
		if _, known := ui.OutputMetricHeader(trimmed); !known {
			issues = append(issues, diag(
				"query.output.columns",
				KindColumnsElement,
				ui.Bi(
					fmt.Sprintf("invalid element %q at index %d in query.output.columns (allowed: %s)", trimmed, index, ui.OutputColumnIDList()),
					fmt.Sprintf("query.output.columns 第 %d 个元素 %q 无效(允许: %s)", index, trimmed, ui.OutputColumnIDList()),
				),
			))
			continue
		}
		if seen[trimmed] {
			issues = append(issues, diag(
				"query.output.columns",
				KindColumnsDuplicate,
				ui.Bi(
					fmt.Sprintf("duplicate column %q in query.output.columns", trimmed),
					fmt.Sprintf("query.output.columns 存在重复列 %q", trimmed),
				),
			))
			continue
		}
		seen[trimmed] = true
		cols = append(cols, trimmed)
	}
	if len(items) == 0 {
		issues = append(issues, diag(
			"query.output.columns",
			KindColumnsEmpty,
			ui.Bi(
				"query.output.columns must contain at least one column ID; remove query.output to restore the default layout",
				"query.output.columns 至少包含一个指标 ID;恢复默认布局应删除 query.output",
			),
		))
	}
	if err := validationError(issues); err != nil {
		return nil, err
	}
	return cols, nil
}

// topLevelDiagnostics 在顶层问题态生成聚合诊断(保留既有聚合双语文本,
// CLI 输出兼容);Path 固定为 query,恢复由 TUI 依 raw 状态构建。
func topLevelDiagnostics(in Input) []Diagnostic {
	if len(in.RawQueryTopLevelIssues) == 0 {
		return nil
	}
	names := make([]string, 0, len(in.RawQueryTopLevelIssues))
	for name := range in.RawQueryTopLevelIssues {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%q (%s)", name, in.RawQueryTopLevelIssues[name].Kind))
	}
	return []Diagnostic{diag(
		"query",
		KindTopLevelProblem,
		ui.Bi(
			fmt.Sprintf("query config has invalid top-level entries: %s; remove or fix them in config.toml", strings.Join(parts, ", ")),
			fmt.Sprintf("query 配置存在顶层问题项: %s;请在 config.toml 中删除或修复", strings.Join(parts, ", ")),
		),
	)}
}

func diag(path string, kind DiagnosticKind, message string) Diagnostic {
	return Diagnostic{Path: path, Kind: kind, Message: message}
}

// parseSubqueries 解析 query.subqueries;名称合法、值必须是 CSV 字符串。
func parseSubqueries(raw any) ([]CustomSubquery, []Diagnostic) {
	if raw == nil {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, []Diagnostic{typeDiagnostic("query.subqueries", raw, "a table / 一个表", KindViewsTableType)}
	}
	var issues []Diagnostic
	subs := make([]CustomSubquery, 0, len(table))
	for _, name := range sortedKeys(table) {
		path := "query.subqueries." + name
		if d := checkDefinitionName("query.subqueries", name); d != nil {
			issues = append(issues, *d)
			continue
		}
		csv, ok := table[name].(string)
		if !ok {
			issues = append(issues, typeDiagnostic(path, table[name], "a comma-separated string / 一个逗号分隔字符串", KindDefinitionValueType))
			continue
		}
		// 错误判断按当前条目局部化:先出现条目的错误不得让后续合法定义被跳过,
		// 否则合法子查询会被连带报告为断开引用。
		before := len(issues)
		items := splitCSV(csv)
		var dims []BuiltinDimension
		seen := map[string]bool{}
		for _, item := range items {
			if !isBuiltinDimension(item) {
				issues = append(issues, itemDiagnostic(path, item, builtinDimensionList()))
				continue
			}
			if seen[item] {
				issues = append(issues, duplicateItemDiagnostic(path, item))
				continue
			}
			seen[item] = true
			dims = append(dims, BuiltinDimension(item))
		}
		if len(issues) > before {
			continue
		}
		if len(items) < 2 {
			issues = append(issues, diag(
				path,
				KindMinimumItems,
				ui.Bi(
					fmt.Sprintf("%s requires at least 2 dimensions (got %d: %q)", path, len(items), csv),
					fmt.Sprintf("%s 至少需要 2 个维度(当前 %d 个: %q)", path, len(items), csv),
				),
			))
			continue
		}
		subs = append(subs, CustomSubquery{Name: name, Dimensions: dims})
	}
	// 名称排序输出保证确定性。
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	return subs, issues
}

// parseGroups 解析 query.groups;成员只能是内置维度或已定义的自定义子查询,禁止嵌套引用。
func parseGroups(raw any, subs []CustomSubquery) ([]QueryGroup, []Diagnostic) {
	if raw == nil {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, []Diagnostic{typeDiagnostic("query.groups", raw, "a table / 一个表", KindViewsTableType)}
	}
	subNames := map[string]bool{}
	for _, s := range subs {
		subNames[s.Name] = true
	}
	groupNames := map[string]bool{}
	for _, name := range sortedKeys(table) {
		groupNames[name] = true
	}

	var issues []Diagnostic
	groups := make([]QueryGroup, 0, len(table))
	for _, name := range sortedKeys(table) {
		path := "query.groups." + name
		if d := checkDefinitionName("query.groups", name); d != nil {
			issues = append(issues, *d)
			continue
		}
		csv, ok := table[name].(string)
		if !ok {
			issues = append(issues, typeDiagnostic(path, table[name], "a comma-separated string / 一个逗号分隔字符串", KindDefinitionValueType))
			continue
		}
		// 同 parseSubqueries:错误判断按当前条目局部化,不因先出现条目的错误跳过本条目。
		before := len(issues)
		items := splitCSV(csv)
		var targets []Target
		seen := map[string]bool{}
		for _, item := range items {
			switch {
			case isBuiltinDimension(item):
				if seen[item] {
					issues = append(issues, duplicateItemDiagnostic(path, item))
					continue
				}
				seen[item] = true
				targets = append(targets, Target{Name: item, Kind: TargetBuiltin})
			case subNames[item]:
				if seen[item] {
					issues = append(issues, duplicateItemDiagnostic(path, item))
					continue
				}
				seen[item] = true
				targets = append(targets, Target{Name: item, Kind: TargetCustom})
			case groupNames[item]:
				issues = append(issues, diag(
					path,
					KindDefinitionItem,
					ui.Bi(
						fmt.Sprintf("%s: group cannot reference another group %q (groups may only contain built-in views (%s) and defined subqueries)", path, item, builtinDimensionList()),
						fmt.Sprintf("%s: 组合查询不能引用另一个组合查询 %q(组合成员只能是内置视图(%s)或已定义的自定义子查询)", path, item, builtinDimensionList()),
					),
				))
			default:
				issues = append(issues, itemDiagnostic(path, item, builtinDimensionList()+", 以及已定义的 subqueries"))
			}
		}
		if len(issues) > before {
			continue
		}
		if len(items) < 2 {
			issues = append(issues, diag(
				path,
				KindMinimumItems,
				ui.Bi(
					fmt.Sprintf("%s requires at least 2 items (got %d: %q)", path, len(items), csv),
					fmt.Sprintf("%s 至少需要 2 个成员(当前 %d 个: %q)", path, len(items), csv),
				),
			))
			continue
		}
		groups = append(groups, QueryGroup{Name: name, Items: targets})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, issues
}

// parseDefault 解析 query.default:TrimSpace 后匹配内置/自定义/组合;缺失、nil 或
// 空白回退 client,并以后续布尔值标记这一回退来源。
func parseDefault(raw any, subs []CustomSubquery, groups []QueryGroup) (Target, bool, *Diagnostic) {
	if raw == nil {
		return Target{Name: string(DimensionClient), Kind: TargetBuiltin}, true, nil
	}
	text, ok := raw.(string)
	if !ok {
		return Target{}, false, ptrDiagnostic(typeDiagnostic("query.default", raw, "a string / 一个字符串", KindDefault))
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
	return Target{}, false, ptrDiagnostic(diag(
		"query.default",
		KindDefault,
		ui.Bi(
			fmt.Sprintf("unknown query.default %q (allowed: %s)", trimmed, allowed),
			fmt.Sprintf("未知的 query.default %q(允许: %s)", trimmed, allowed),
		),
	))
}

// ptrDiagnostic 取一个诊断的地址,供返回 *Diagnostic 的解析函数使用。
func ptrDiagnostic(d Diagnostic) *Diagnostic {
	return &d
}

// checkDefinitionName 校验自定义/组合查询名:小写标识符且不与保留名冲突;
// 保留名错误列表从 reservedNameOrder 有序生成,不另写名单。
func checkDefinitionName(section, name string) *Diagnostic {
	path := section + "." + name
	if IsReservedName(name) {
		return ptrDiagnostic(diag(
			path,
			KindDefinitionName,
			ui.Bi(
				fmt.Sprintf("reserved name in %s: %q (reserved: %s)", path, name, reservedNameList()),
				fmt.Sprintf("%s 使用了保留名 %q(保留: %s)", path, name, reservedNameList()),
			),
		))
	}
	if !namePattern.MatchString(name) {
		return ptrDiagnostic(diag(
			path,
			KindDefinitionName,
			ui.Bi(
				fmt.Sprintf("invalid name in %s: %q (lowercase identifier: letter first, then letters, digits, _ or -)", path, name),
				fmt.Sprintf("%s 中的名称 %q 不合法(小写标识符: 首字符为字母,后续为字母、数字、_ 或 -)", path, name),
			),
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

// itemDiagnostic 报告空段或未知项,携带值摘要与允许集合(空段即值摘要为 "" 的无效项)。
func itemDiagnostic(path, item, allowed string) Diagnostic {
	return diag(
		path,
		KindDefinitionItem,
		ui.Bi(
			fmt.Sprintf("invalid item %q in %q (allowed: %s)", item, path, allowed),
			fmt.Sprintf("%q 中的无效项 %q(允许: %s)", path, item, allowed),
		),
	)
}

func duplicateItemDiagnostic(path, item string) Diagnostic {
	return diag(
		path,
		KindDuplicateItem,
		ui.Bi(
			fmt.Sprintf("duplicate item %q in %q", item, path),
			fmt.Sprintf("%q 存在重复项 %q", path, item),
		),
	)
}

// typeDiagnostic 报告值类型不匹配,含路径、实际类型摘要与期望形态。
func typeDiagnostic(path string, value any, want string, kind DiagnosticKind) Diagnostic {
	return diag(
		path,
		kind,
		ui.Bi(
			fmt.Sprintf("%s must be %s (got %T: %v)", path, want, value, summarize(value)),
			fmt.Sprintf("%s 必须是 %s(实际 %T: %v)", path, want, value, summarize(value)),
		),
	)
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
