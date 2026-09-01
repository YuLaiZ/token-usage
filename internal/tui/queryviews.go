package tui

import (
	"errors"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// QueryAdapter 是 TUI 保存前 query 校验与 Query 子页编辑数据源的可注入适配器:
// 把 config raw query 状态适配为 querydef 强类型解析。CLI 层注入生产实现,
// 测试可注入 fake;接口暴露完整校验、完整定义与两类局部读取四种能力:
// Validate/Definitions 用完整 Parse(保存门槛),Views 只解析视图定义,
// OutputLayout 只解析输出布局——两个局部入口彼此隔离恢复态。
type QueryAdapter interface {
	// Validate 校验草稿的 raw query 状态(完整语义校验,失败返回定位错误)。
	Validate(cfg *config.Config) error
	// Definitions 返回草稿的强类型完整 query 定义(含输出布局)。
	Definitions(cfg *config.Config) (*querydef.QueryDefinitions, error)
	// Views 只解析视图定义(default/subqueries/groups),忽略 output 的
	// 合法/非法形态;顶层问题态返回共同前置诊断。
	Views(cfg *config.Config) (*querydef.ViewDefinitions, error)
	// OutputLayout 只解析 query.output,忽略视图定义与其他顶层键。
	OutputLayout(cfg *config.Config) ([]string, error)
}

// queryParentItems Query 父页的三个平级子项。
var queryParentItems = []string{
	ui.Bi("Views", "查询视图"),
	ui.Bi("Output columns", "输出列"),
	ui.Bi("Provider aliases", "供应商别名"),
}

// queryParentPage 是 Query 父页:Views、Output columns 与 Provider aliases
// 三个平级入口。父页自身不进入 query raw 恢复态——顶层问题与各自的
// 定义错误由子页处理;Provider aliases 永远可直接进入。
type queryParentPage struct {
	app    *App
	cursor int
}

func newQueryParentPage(app *App) *queryParentPage {
	return &queryParentPage{app: app}
}

func (p *queryParentPage) title() string { return ui.Bi("Query", "查询") }
func (p *queryParentPage) Init() tea.Cmd { return nil }

func (p *queryParentPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(queryParentItems)-1 {
			p.cursor++
		}
	case "enter":
		switch p.cursor {
		case 0:
			p.app.push(newQueryViewsPage(p.app))
		case 1:
			p.app.push(newOutputColumnsPage(p.app))
		case 2:
			p.app.push(newAliasesPage(p.app))
		}
	}
	return p, nil
}

func (p *queryParentPage) View() string {
	var b strings.Builder
	b.WriteString(ui.Bi("Query", "查询") + "\n\n")
	summaries := []string{queryViewsSummary(p.app.draft), outputColumnsSummary(p.app), aliasCountSummary(p.app.draft)}
	for i, item := range queryParentItems {
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		b.WriteString(cursor + item + "  ·  " + summaries[i] + "\n")
	}
	b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " + ui.Bi("enter Open", "enter 进入") + "   " + ui.Bi("esc Back", "esc 返回") + "\n")
	return b.String()
}

// aliasCountSummary 返回 provider alias 数量摘要。
func aliasCountSummary(c *config.Config) string {
	n := len(c.ProviderAliases)
	return ui.Bi(fmtCount(n, "mapping", "条映射"), fmtCountZh(n))
}

// outputColumnsSummary 返回 Output columns 子项摘要:布局合法时显示有效列数
// 及是否为默认布局;布局不合法或顶层问题态显示 recovery 标记,不显示列数。
// 只读取 draft(不做 display 回退)。
func outputColumnsSummary(app *App) string {
	if app.query == nil || len(app.draft.RawQueryTopLevelIssues) > 0 {
		return ui.Bi("needs recovery", "待恢复")
	}
	cols, err := app.query.OutputLayout(app.draft)
	if err != nil {
		return ui.Bi("needs recovery", "待恢复")
	}
	isDefault := len(cols) == 7 && strings.Join(cols, ",") == strings.Join(ui.DefaultOutputColumns(), ",")
	if isDefault {
		return ui.Bi("7 columns (default)", "7 列(默认)")
	}
	return ui.Bi(fmtCount(len(cols), "column", "列"), itoa(len(cols))+" 列")
}

// ---- query.output raw 读写(唯一内存形态:columns 为 []any 字符串数组) ----

// setQueryOutputColumns 把 query.output 整表替换为仅含 columns 的字符串数组。
// 整表替换(而非子键写入)确保恢复过的未知子键不会残留。
func setQueryOutputColumns(cfg *config.Config, columns []string) {
	arr := make([]any, len(columns))
	for i, c := range columns {
		arr[i] = c
	}
	ensureRawQuery(cfg)["output"] = map[string]any{"columns": arr}
}

// deleteQueryOutput 删除整个 query.output 表(等价恢复默认布局)。
func deleteQueryOutput(cfg *config.Config) {
	if cfg.RawQuery != nil {
		delete(cfg.RawQuery, "output")
	}
}

// queryTopMenuItems Query views 首页的三个操作项。
var queryTopMenuItems = []string{
	ui.Bi("Custom subqueries", "自定义子查询"),
	ui.Bi("Groups", "组合查询"),
	ui.Bi("Default view", "默认行为"),
}

// queryViewsPage 是 Query views 首页:
//   - 常规态:三项导航(自定义子查询/组合查询/默认行为);
//   - 恢复态:raw query 无法延迟解析时列出全部可修复错误项,逐项 Enter 修复。
//
// 恢复态每次修复后从当前 draft 重新执行顶层分类与完整 querydef 校验
// (不手工维护第二套错误列表);全部错误清除后自动回到常规态。
type queryViewsPage struct {
	app    *App
	cursor int
	// recovery 非 nil 时处于恢复态:可修复错误项列表。
	recovery *recoveryState
}

type recoveryState struct {
	items  []*recoveryItem
	cursor int
}

// recoveryItem 是恢复态的一个可修复错误项:摘要 + 修复动作。
type recoveryItem struct {
	desc   string
	action string
	apply  func(app *App)
}

func newQueryViewsPage(app *App) *queryViewsPage {
	p := &queryViewsPage{app: app}
	p.evaluate()
	return p
}

// evaluate 重新执行顶层分类与完整 querydef 校验,决定常规态/恢复态。
// 转正逻辑(删除大小写变体后唯一精确小写表转正)由统一重分类入口完成。
func (p *queryViewsPage) evaluate() {
	if p.app.query == nil {
		return
	}
	items := buildRecoveryItems(p.app)
	if len(items) == 0 {
		p.recovery = nil
		return
	}
	p.recovery = &recoveryState{items: items}
	if p.recovery.cursor >= len(items) {
		p.recovery.cursor = len(items) - 1
	}
}

func (p *queryViewsPage) title() string { return ui.Bi("Query views", "查询视图") }
func (p *queryViewsPage) Init() tea.Cmd { return nil }

func (p *queryViewsPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	if p.recovery != nil {
		return p.updateRecovery(k)
	}
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(queryTopMenuItems)-1 {
			p.cursor++
		}
	case "enter":
		switch p.cursor {
		case 0:
			p.app.push(newSubqueryListPage(p.app))
		case 1:
			p.app.push(newGroupListPage(p.app))
		case 2:
			p.app.push(newDefaultSelectPage(p.app))
		}
	}
	return p, nil
}

// updateRecovery 恢复态键盘合同:↑/k ↓/j 移动错误项光标;Enter 执行当前项修复;
// a/d 禁用(不响应);Esc 返回主菜单并保留 draft。
func (p *queryViewsPage) updateRecovery(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.recovery.cursor > 0 {
			p.recovery.cursor--
		}
	case "down", "j":
		if p.recovery.cursor < len(p.recovery.items)-1 {
			p.recovery.cursor++
		}
	case "enter":
		item := p.recovery.items[p.recovery.cursor]
		if item == nil || item.apply == nil {
			return p, nil
		}
		item.apply(p.app)
		p.evaluate()
	}
	return p, nil
}

func (p *queryViewsPage) View() string {
	if p.recovery != nil {
		var b strings.Builder
		b.WriteString(ui.Bi("Query views - recovery", "查询视图 - 恢复") + "\n\n")
		b.WriteString(ui.Bi(
			"The raw query config has errors; fix each item below (enter), then the guided pages unlock:",
			"query 配置存在错误;逐项修复(enter)后进入常规引导页:") + "\n\n")
		for i, item := range p.recovery.items {
			cursor := "  "
			if i == p.recovery.cursor {
				cursor = "▸ "
			}
			b.WriteString(cursor + item.desc + "\n      " + ui.Bi("enter:", "enter:") + " " + item.action + "\n")
		}
		b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " +
			ui.Bi("enter Fix selected item", "enter 修复选中项") + "   " + ui.Bi("esc Back to Query (draft kept)", "esc 返回 Query(保留草稿)") + "\n")
		return b.String()
	}
	var b strings.Builder
	b.WriteString(ui.Bi("Query views", "查询视图") + "\n\n")
	summaries := []string{subquerySummary(p.app.draft), groupSummary(p.app.draft), queryViewsSummary(p.app.draft)}
	for i, item := range queryTopMenuItems {
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		b.WriteString(cursor + item + "  ·  " + summaries[i] + "\n")
	}
	b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " + ui.Bi("enter Open", "enter 进入") + "   " + ui.Bi("esc Back", "esc 返回") + "\n")
	return b.String()
}

// subquerySummary/groupSummary 返回条目数量摘要。
func subquerySummary(c *config.Config) string {
	n := len(queryRawTable(c, "subqueries"))
	return ui.Bi(fmtCount(n, "subquery", "个自定义子查询"), fmtCountZh(n))
}

func groupSummary(c *config.Config) string {
	n := len(queryRawTable(c, "groups"))
	return ui.Bi(fmtCount(n, "group", "个组合查询"), fmtCountZh(n))
}

func fmtCount(n int, en, zh string) string {
	if n == 1 {
		return "1 " + en
	}
	return strings.Repeat("", 0) + itoa(n) + " " + en + "s"
}

func fmtCountZh(n int) string {
	if n == 0 {
		return "无"
	}
	return itoa(n) + " 个"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// queryRawTable 返回 draft.RawQuery 里的指定子表(无则空 map)。
func queryRawTable(c *config.Config, key string) map[string]any {
	if c == nil || c.RawQuery == nil {
		return nil
	}
	if t, ok := c.RawQuery[key].(map[string]any); ok {
		return t
	}
	return nil
}

// ---- raw draft 写入(唯一内存形态:string 标量、map[string]any 子表) ----

// ensureRawQuery 确保 draft.RawQuery 存在(问题态/未配置时建立合法空表)。
func ensureRawQuery(cfg *config.Config) map[string]any {
	if cfg.RawQuery == nil {
		cfg.RawQuery = map[string]any{}
	}
	return cfg.RawQuery
}

// setQuerySubquery 写入/更新一个自定义子查询:值为逗号分隔 string(声明顺序保留)。
func setQuerySubquery(cfg *config.Config, name string, dims []string) {
	q := ensureRawQuery(cfg)
	sub, ok := q["subqueries"].(map[string]any)
	if !ok {
		sub = map[string]any{}
		q["subqueries"] = sub
	}
	sub[name] = strings.Join(dims, ",")
}

// deleteQuerySubquery 删除一个自定义子查询;表空时连表键一起删除。
func deleteQuerySubquery(cfg *config.Config, name string) bool {
	sub := queryRawTable(cfg, "subqueries")
	if sub == nil {
		return false
	}
	if _, ok := sub[name]; !ok {
		return false
	}
	delete(sub, name)
	if len(sub) == 0 {
		delete(ensureRawQuery(cfg), "subqueries")
	}
	return true
}

// setQueryGroup 写入/更新一个组合查询。
func setQueryGroup(cfg *config.Config, name string, items []string) {
	q := ensureRawQuery(cfg)
	groups, ok := q["groups"].(map[string]any)
	if !ok {
		groups = map[string]any{}
		q["groups"] = groups
	}
	groups[name] = strings.Join(items, ",")
}

// deleteQueryGroup 删除一个组合查询;表空时连表键一起删除。
func deleteQueryGroup(cfg *config.Config, name string) bool {
	groups := queryRawTable(cfg, "groups")
	if groups == nil {
		return false
	}
	if _, ok := groups[name]; !ok {
		return false
	}
	delete(groups, name)
	if len(groups) == 0 {
		delete(ensureRawQuery(cfg), "groups")
	}
	return true
}

// setQueryDefault 写入显式 default。
func setQueryDefault(cfg *config.Config, name string) {
	ensureRawQuery(cfg)["default"] = name
}

// clearQueryDefault 删除显式 default(等价「使用默认 client」)。
func clearQueryDefault(cfg *config.Config) {
	if cfg.RawQuery != nil {
		delete(cfg.RawQuery, "default")
	}
}

// subqueryReferences 枚举引用指定子查询的组合查询名。
func subqueryReferences(c *config.Config, name string) []string {
	var refs []string
	for gname, value := range queryRawTable(c, "groups") {
		if csv, ok := value.(string); ok {
			for _, item := range strings.Split(csv, ",") {
				if strings.TrimSpace(item) == name {
					refs = append(refs, gname)
					break
				}
			}
		}
	}
	sort.Strings(refs)
	return refs
}

// groupReferencedByDefault 报告组合查询是否被显式 default 引用。
func groupReferencedByDefault(c *config.Config, name string) bool {
	def, ok := queryDefaultRaw(c)
	return ok && def == name
}

func subqueryReferencedByDefault(c *config.Config, name string) bool {
	def, ok := queryDefaultRaw(c)
	return ok && def == name
}

// queryDefaultRaw 返回显式 default(TrimSpace 后非空才算)。
func queryDefaultRaw(c *config.Config) (string, bool) {
	if c == nil || c.RawQuery == nil {
		return "", false
	}
	def, ok := c.RawQuery["default"].(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(def)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// ---- 恢复态错误项构建 ----

// buildRecoveryItems 从当前 draft 构建恢复态错误项:
// 顶层问题项、未知顶层键、错误 default、错误条目;全部清除后返回空(常规态)。
// 视图错误来自 ParseViews 局部解析(query.output 的错误被隔离在 Output columns
// 页);恢复项仅由 Diagnostic 的 Path/Kind 构建,不以错误文本子串判断归属。
func buildRecoveryItems(app *App) []*recoveryItem {
	// 1. 顶层问题项:raw 状态直接构建(Views 与 Output columns 的共同前置)。
	items := topLevelRecoveryItems(app.draft)

	// 其余错误依赖 querydef 局部解析;无适配器时只处理顶层问题项。
	if app.query == nil {
		return items
	}
	_, viewErr := app.query.Views(app.draft)
	if viewErr == nil {
		return items
	}
	for _, d := range diagnosticsOf(viewErr) {
		if item := viewRecoveryItem(d); item != nil {
			items = append(items, item)
		}
	}
	return items
}

// topLevelRecoveryItems 从 raw 顶层问题态逐项构建删除恢复项(按名称排序)。
func topLevelRecoveryItems(draft *config.Config) []*recoveryItem {
	var items []*recoveryItem
	issueNames := make([]string, 0, len(draft.RawQueryTopLevelIssues))
	for name := range draft.RawQueryTopLevelIssues {
		issueNames = append(issueNames, name)
	}
	sort.Strings(issueNames)
	for _, name := range issueNames {
		issueName := name
		items = append(items, &recoveryItem{
			desc:   ui.Bi("top-level entry \""+issueName+"\" ("+string(draft.RawQueryTopLevelIssues[issueName].Kind)+")", "顶层项 \""+issueName+"\" ("+string(draft.RawQueryTopLevelIssues[issueName].Kind)+")"),
			action: ui.Bi("delete this top-level entry", "删除此顶层项"),
			apply: func(app *App) {
				entries := currentQueryTopLevelEntries(app.draft)
				delete(entries, issueName)
				config.ReclassifyRawQuery(app.draft, entries)
			},
		})
	}
	return items
}

// diagnosticsOf 从解析错误中提取结构化诊断列表。
func diagnosticsOf(err error) []querydef.Diagnostic {
	var ve *querydef.ValidationError
	if errors.As(err, &ve) {
		return ve.Issues
	}
	return nil
}

// viewRecoveryItem 按诊断的 Path/Kind 构建一个视图错误恢复项:
//   - 未知 query 键(KindUnknownQueryKey):删除该键;
//   - subqueries/groups 非表(KindViewsTableType):删除整表后重建;
//   - default(KindDefault):删除 query.default 回退 client;
//   - 条目级错误(名称/值类型/成员/重复/下限,Path 为 query.subqueries.<name>
//     或 query.groups.<name>):只删该条目——跨表重名 Path 固定为
//     query.groups.<name>,恢复动作删 group 以保留同名子查询。
func viewRecoveryItem(d querydef.Diagnostic) *recoveryItem {
	switch d.Kind {
	case querydef.KindUnknownQueryKey:
		key := strings.TrimPrefix(d.Path, "query.")
		if key == d.Path {
			return nil
		}
		unknownKey := key
		return &recoveryItem{
			desc:   ui.Bi("unknown query key \""+unknownKey+"\"", "未知 query 键 \""+unknownKey+"\""),
			action: ui.Bi("delete this unknown key", "删除此未知键"),
			apply: func(app *App) {
				if app.draft.RawQuery != nil {
					delete(app.draft.RawQuery, unknownKey)
				}
			},
		}
	case querydef.KindViewsTableType:
		switch d.Path {
		case "query.subqueries":
			return &recoveryItem{
				desc:   ui.Bi("query.subqueries is not a table", "query.subqueries 不是表"),
				action: ui.Bi("delete the subqueries table", "删除 subqueries 表"),
				apply:  func(app *App) { delete(ensureRawQuery(app.draft), "subqueries") },
			}
		case "query.groups":
			return &recoveryItem{
				desc:   ui.Bi("query.groups is not a table", "query.groups 不是表"),
				action: ui.Bi("delete the groups table", "删除 groups 表"),
				apply:  func(app *App) { delete(ensureRawQuery(app.draft), "groups") },
			}
		}
		return nil
	case querydef.KindDefault:
		return &recoveryItem{
			desc:   ui.Bi("invalid query.default", "query.default 不合法"),
			action: ui.Bi("reset default to client (delete query.default)", "恢复 default 为 client(删除 query.default)"),
			apply:  func(app *App) { clearQueryDefault(app.draft) },
		}
	case querydef.KindDefinitionName, querydef.KindDefinitionValueType,
		querydef.KindDefinitionItem, querydef.KindDuplicateItem, querydef.KindMinimumItems,
		querydef.KindCrossTableDuplicate:
		// 跨表重名 Path 固定为 query.groups.<name>:删 group 保留同名子查询。
		if name := strings.TrimPrefix(d.Path, "query.subqueries."); name != d.Path {
			subName := name
			return &recoveryItem{
				desc:   ui.Bi("invalid subquery \""+subName+"\"", "自定义子查询 \""+subName+"\" 不合法"),
				action: ui.Bi("delete this entry and recreate it via guided pages", "删除此条目后按引导重新创建"),
				apply:  func(app *App) { deleteQuerySubquery(app.draft, subName) },
			}
		}
		if name := strings.TrimPrefix(d.Path, "query.groups."); name != d.Path {
			gName := name
			return &recoveryItem{
				desc:   ui.Bi("invalid group \""+gName+"\"", "组合查询 \""+gName+"\" 不合法"),
				action: ui.Bi("delete this entry and recreate it via guided pages", "删除此条目后按引导重新创建"),
				apply:  func(app *App) { deleteQueryGroup(app.draft, gName) },
			}
		}
		return nil
	}
	return nil
}

// currentQueryTopLevelEntries 汇总当前 draft 的全部 query 顶层项(两载体并集)。
func currentQueryTopLevelEntries(cfg *config.Config) map[string]any {
	entries := map[string]any{}
	for k, v := range cfg.RawQuery {
		entries[k] = v
	}
	for name, issue := range cfg.RawQueryTopLevelIssues {
		entries[name] = issue.Value
	}
	return entries
}

func sortedQueryRawKeys(c *config.Config) []string {
	if c.RawQuery == nil {
		return nil
	}
	keys := make([]string, 0, len(c.RawQuery))
	for k := range c.RawQuery {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedTableKeys(t map[string]any) []string {
	if t == nil {
		return nil
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isStringTable(v any) bool {
	t, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, item := range t {
		if !isString(item) {
			return false
		}
	}
	return true
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// ---- 子查询列表页 ----

// subqueryListPage 自定义子查询列表:a 新增(名称输入) d 删除(引用阻止)
// enter 编辑 esc 返回。完成操作直接写 draft(草稿模型)。
type subqueryListPage struct {
	app      *App
	cursor   int
	adding   bool
	nameIn   *namePrompt
	feedback string
}

func newSubqueryListPage(app *App) *subqueryListPage {
	return &subqueryListPage{app: app}
}

func (p *subqueryListPage) title() string { return ui.Bi("Custom subqueries", "自定义子查询") }
func (p *subqueryListPage) Init() tea.Cmd { return nil }

func (p *subqueryListPage) names() []string {
	return sortedTableKeys(queryRawTable(p.app.draft, "subqueries"))
}

func (p *subqueryListPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		if p.adding {
			p.nameIn.handleKeys(msg.(tea.KeyMsg))
		}
		return p, nil
	}
	if p.adding {
		p.nameIn.handleKeys(k)
		switch p.nameIn.Done() {
		case selectCancelled:
			p.adding = false
		case selectSubmitted:
			name := p.nameIn.Value()
			if _, exists := queryRawTable(p.app.draft, "subqueries")[name]; exists {
				p.feedback = ui.Bi("name already exists", "名称已存在")
				p.adding = false
				return p, nil
			}
			p.adding = false
			p.app.push(newSubqueryEditPage(p.app, name, nil))
		}
		return p, nil
	}
	names := p.names()
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(names)-1 {
			p.cursor++
		}
	case "a":
		p.adding = true
		p.nameIn = newNamePrompt(ui.Bi("new subquery name", "新子查询名"), func(s string) string {
			return validateQueryName(p.app.draft, s, "subqueries")
		})
	case "d":
		if p.cursor < len(names) {
			name := names[p.cursor]
			if refs := subqueryReferences(p.app.draft, name); len(refs) > 0 {
				p.feedback = ui.Bi(
					"referenced by group(s): "+strings.Join(refs, ", "),
					"被组合查询引用: "+strings.Join(refs, ", "))
				return p, nil
			}
			if subqueryReferencedByDefault(p.app.draft, name) {
				p.feedback = ui.Bi("referenced by query.default", "被 query.default 引用")
				return p, nil
			}
			deleteQuerySubquery(p.app.draft, name)
			if p.cursor >= len(p.names()) {
				p.cursor = len(p.names()) - 1
			}
		}
	case "enter":
		if p.cursor < len(names) {
			name := names[p.cursor]
			var initial []string
			if defs, err := p.app.query.Definitions(p.app.draft); err == nil {
				for _, s := range defs.Subqueries {
					if s.Name == name {
						for _, d := range s.Dimensions {
							initial = append(initial, string(d))
						}
					}
				}
			}
			p.app.push(newSubqueryEditPage(p.app, name, initial))
		}
	}
	return p, nil
}

func (p *subqueryListPage) View() string {
	var b strings.Builder
	b.WriteString(ui.Bi("Custom subqueries", "自定义子查询") + "\n\n")
	names := p.names()
	if len(names) == 0 {
		b.WriteString("  (" + ui.Bi("none", "无") + ")\n")
	}
	for i, name := range names {
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		csv := ""
		if v, ok := queryRawTable(p.app.draft, "subqueries")[name].(string); ok {
			csv = v
		}
		b.WriteString(cursor + name + " = " + csv + "\n")
	}
	if p.adding {
		b.WriteString("\n  " + p.nameIn.View() + "\n")
	}
	if p.feedback != "" {
		b.WriteString("\n  " + p.feedback + "\n")
	}
	b.WriteString("\n  " + ui.Bi("a Add", "a 新增") + "   " + ui.Bi("d Delete", "d 删除") + "   " +
		ui.Bi("enter Edit", "enter 编辑") + "   " + ui.Bi("esc Back", "esc 返回") + "\n")
	return b.String()
}

// validateQueryName 校验新增名称:非空、合法标识符、不与保留名/既有定义冲突。
// 保留名判断复用 querydef 单一来源(list 等新增保留名随 querydef 同步生效)。
func validateQueryName(draft *config.Config, name string, table string) string {
	if name == "" {
		return ui.Bi("name must not be empty", "名称不能为空")
	}
	if querydef.IsReservedName(name) {
		return ui.Bi("reserved name", "保留名不可用")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return ui.Bi("lowercase identifier expected (letter first, then letters/digits/_/-)", "应为小写标识符(首字符字母,后续字母/数字/_/-)")
	}
	first := name[0]
	if first < 'a' || first > 'z' {
		return ui.Bi("name must start with a letter", "名称须以字母开头")
	}
	other := "groups"
	if table == "groups" {
		other = "subqueries"
	}
	if _, exists := queryRawTable(draft, other)[name]; exists {
		return ui.Bi("name already used by the other table", "名称已被另一张表使用")
	}
	return ""
}

// ---- 子查询编辑页 ----

// subqueryEditPage 用有序多选编辑一个自定义子查询(候选仅四个内置维度);
// Enter 提交(≥2 维)直接写 draft;Esc 取消未提交选择(草稿不变)。
type subqueryEditPage struct {
	app    *App
	name   string
	sel    *orderedSelect
	errMsg string
}

func newSubqueryEditPage(app *App, name string, initial []string) *subqueryEditPage {
	return &subqueryEditPage{
		app:  app,
		name: name,
		sel: newOrderedSelect([]string{"client", "model", "provider", "project"}, initial,
			ui.Bi("Custom subquery "+name, "自定义子查询 "+name)),
	}
}

func (p *subqueryEditPage) title() string {
	return ui.Bi("Edit subquery "+p.name, "编辑子查询 "+p.name)
}
func (p *subqueryEditPage) Init() tea.Cmd { return nil }

func (p *subqueryEditPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	p.sel.handleKeys(k)
	switch p.sel.Done() {
	case selectCancelled:
		p.app.pop()
	case selectSubmitted:
		dims := p.sel.Selection()
		if len(dims) < 2 {
			p.errMsg = ui.Bi("a subquery requires at least 2 dimensions", "子查询至少需要 2 个维度")
			p.sel.done = selectPending
			return p, nil
		}
		setQuerySubquery(p.app.draft, p.name, dims)
		p.app.pop()
	}
	return p, nil
}

func (p *subqueryEditPage) View() string {
	s := p.sel.View()
	if p.errMsg != "" {
		s += "\n  " + p.errMsg + "\n"
	}
	return s
}

// ---- 组合查询列表页 ----

type groupListPage struct {
	app      *App
	cursor   int
	adding   bool
	nameIn   *namePrompt
	feedback string
}

func newGroupListPage(app *App) *groupListPage {
	return &groupListPage{app: app}
}

func (p *groupListPage) title() string { return ui.Bi("Groups", "组合查询") }
func (p *groupListPage) Init() tea.Cmd { return nil }

func (p *groupListPage) names() []string {
	return sortedTableKeys(queryRawTable(p.app.draft, "groups"))
}

func (p *groupListPage) candidates() []string {
	// 组合查询候选 = 四个内置视图 + 已定义自定义子查询(不含组合查询自身)。
	cands := []string{"client", "model", "provider", "project"}
	cands = append(cands, sortedTableKeys(queryRawTable(p.app.draft, "subqueries"))...)
	return cands
}

func (p *groupListPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	if p.adding {
		p.nameIn.handleKeys(k)
		switch p.nameIn.Done() {
		case selectCancelled:
			p.adding = false
		case selectSubmitted:
			name := p.nameIn.Value()
			if _, exists := queryRawTable(p.app.draft, "groups")[name]; exists {
				p.feedback = ui.Bi("name already exists", "名称已存在")
				p.adding = false
				return p, nil
			}
			p.adding = false
			p.app.push(newGroupEditPage(p.app, name, nil))
		}
		return p, nil
	}
	names := p.names()
	switch k.String() {
	case "esc":
		p.app.pop()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(names)-1 {
			p.cursor++
		}
	case "a":
		p.adding = true
		p.nameIn = newNamePrompt(ui.Bi("new group name", "新组合查询名"), func(s string) string {
			return validateQueryName(p.app.draft, s, "groups")
		})
	case "d":
		if p.cursor < len(names) {
			name := names[p.cursor]
			if groupReferencedByDefault(p.app.draft, name) {
				p.feedback = ui.Bi("referenced by query.default", "被 query.default 引用")
				return p, nil
			}
			deleteQueryGroup(p.app.draft, name)
			if p.cursor >= len(p.names()) {
				p.cursor = len(p.names()) - 1
			}
		}
	case "enter":
		if p.cursor < len(names) {
			name := names[p.cursor]
			var initial []string
			if v, ok := queryRawTable(p.app.draft, "groups")[name].(string); ok {
				for _, item := range strings.Split(v, ",") {
					if t := strings.TrimSpace(item); t != "" {
						initial = append(initial, t)
					}
				}
			}
			p.app.push(newGroupEditPage(p.app, name, initial))
		}
	}
	return p, nil
}

func (p *groupListPage) View() string {
	var b strings.Builder
	b.WriteString(ui.Bi("Groups", "组合查询") + "\n\n")
	names := p.names()
	if len(names) == 0 {
		b.WriteString("  (" + ui.Bi("none", "无") + ")\n")
	}
	for i, name := range names {
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		csv := ""
		if v, ok := queryRawTable(p.app.draft, "groups")[name].(string); ok {
			csv = v
		}
		b.WriteString(cursor + name + " = " + csv + "\n")
	}
	if p.adding {
		b.WriteString("\n  " + p.nameIn.View() + "\n")
	}
	if p.feedback != "" {
		b.WriteString("\n  " + p.feedback + "\n")
	}
	b.WriteString("\n  " + ui.Bi("a Add", "a 新增") + "   " + ui.Bi("d Delete", "d 删除") + "   " +
		ui.Bi("enter Edit", "enter 编辑") + "   " + ui.Bi("esc Back", "esc 返回") + "\n")
	return b.String()
}

// groupEditPage 用有序多选编辑一个组合查询(候选 = 内置 + 已定义自定义,不含组合)。
type groupEditPage struct {
	app    *App
	name   string
	sel    *orderedSelect
	errMsg string
}

func newGroupEditPage(app *App, name string, initial []string, candidatesOverride ...string) *groupEditPage {
	list := &groupListPage{app: app}
	return &groupEditPage{
		app:  app,
		name: name,
		sel:  newOrderedSelect(list.candidates(), initial, ui.Bi("Group "+name, "组合查询 "+name)),
	}
}

func (p *groupEditPage) title() string {
	return ui.Bi("Edit group "+p.name, "编辑组合查询 "+p.name)
}
func (p *groupEditPage) Init() tea.Cmd { return nil }

func (p *groupEditPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	p.sel.handleKeys(k)
	switch p.sel.Done() {
	case selectCancelled:
		p.app.pop()
	case selectSubmitted:
		items := p.sel.Selection()
		if len(items) < 2 {
			p.errMsg = ui.Bi("a group requires at least 2 items", "组合查询至少需要 2 个成员")
			p.sel.done = selectPending
			return p, nil
		}
		setQueryGroup(p.app.draft, p.name, items)
		p.app.pop()
	}
	return p, nil
}

func (p *groupEditPage) View() string {
	s := p.sel.View()
	if p.errMsg != "" {
		s += "\n  " + p.errMsg + "\n"
	}
	return s
}

// ---- 默认行为单选页 ----

// defaultSelectPage 默认行为单选:候选 = 内置 + 自定义 + 组合 + 「使用默认 client」。
// Space 把当前候选设为唯一待提交选择;Enter 提交;Esc 放弃该次未提交选择。
type defaultSelectPage struct {
	app     *App
	items   []string // 候选名;空串哨兵表示「使用默认 client」
	cursor  int
	chosen  int // 待提交选择索引;-1 未选择
	pending bool
}

const useDefaultClientSentinel = "\x00default-client"

func newDefaultSelectPage(app *App) *defaultSelectPage {
	items := []string{"client", "model", "provider", "project"}
	items = append(items, sortedTableKeys(queryRawTable(app.draft, "subqueries"))...)
	items = append(items, sortedTableKeys(queryRawTable(app.draft, "groups"))...)
	items = append(items, useDefaultClientSentinel)
	return &defaultSelectPage{app: app, items: items, chosen: -1}
}

func (p *defaultSelectPage) title() string { return ui.Bi("Default view", "默认行为") }
func (p *defaultSelectPage) Init() tea.Cmd { return nil }

func (p *defaultSelectPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	switch k.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.items)-1 {
			p.cursor++
		}
	case " ":
		// 唯一待提交选择:重复 Space 同项不产生多选。
		p.chosen = p.cursor
		p.pending = true
	case "enter":
		if p.pending && p.chosen >= 0 {
			name := p.items[p.chosen]
			if name == useDefaultClientSentinel {
				clearQueryDefault(p.app.draft)
			} else {
				setQueryDefault(p.app.draft, name)
			}
		}
		p.app.pop()
	case "esc":
		// 放弃该次未提交选择,不修改 draft。
		p.app.pop()
	}
	return p, nil
}

func (p *defaultSelectPage) View() string {
	var b strings.Builder
	b.WriteString(ui.Bi("Default view", "默认行为") + "\n\n")
	current, hasCurrent := queryDefaultRaw(p.app.draft)
	for i, name := range p.items {
		cursor := " "
		if i == p.cursor {
			cursor = "▸"
		}
		mark := "  "
		if p.pending && i == p.chosen {
			mark = "● "
		} else if hasCurrent && name == current || (!hasCurrent && name == useDefaultClientSentinel) {
			mark = "◇ "
		}
		display := name
		if name == useDefaultClientSentinel {
			display = ui.Bi("Use default client", "使用默认 client")
		}
		b.WriteString("  " + cursor + " " + mark + display + "\n")
	}
	b.WriteString("\n  " + ui.Bi("↑/k ↓/j Move", "↑/k ↓/j 移动") + "   " + ui.Bi("space Set choice", "space 设定选择") + "   " +
		ui.Bi("enter Submit", "enter 提交") + "   " + ui.Bi("esc Discard", "esc 放弃") + "\n")
	return b.String()
}

// 编译期保证页面实现 page 接口。
var (
	_ page = (*queryViewsPage)(nil)
	_ page = (*subqueryListPage)(nil)
	_ page = (*subqueryEditPage)(nil)
	_ page = (*groupListPage)(nil)
	_ page = (*groupEditPage)(nil)
	_ page = (*defaultSelectPage)(nil)
)
