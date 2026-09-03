package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// emDash 是时间字段缺数据时的占位符:范围内无消息事件 / 库中无成功采集记录。
const emDash = "—"

// queryView 标识 query 命令的互斥视图。裸 query 与 query client 共用 viewClient。
type queryView int

const (
	viewClient queryView = iota
	viewModel
	viewProvider
	viewProject
	viewSessions
	viewSummary
	// viewDefault 表示裸 query:执行 query.default 指向的对象(未配置时等价 client)。
	viewDefault
)

// queryBuiltinCmd 是一个内置查询命令的静态元数据。
type queryBuiltinCmd struct {
	name  string
	short string
	view  queryView
}

// queryBuiltinCmds 是六个内置查询命令的唯一元数据来源:子命令注册与
// query list 内置表渲染共用,避免 Short 与列表文案漂移。
// custom/list 是固定操作入口而非内置视图,不入此表。
var queryBuiltinCmds = []queryBuiltinCmd{
	{"client", "Group by client (default) / 按客户端分组（默认）", viewClient},
	{"model", "Group by model / 按模型分组", viewModel},
	{"provider", "Group by provider / 按供应商分组", viewProvider},
	{"project", "Group by project / 按项目分组", viewProject},
	{"session", "View session details / 查看会话明细", viewSessions},
	{"summary", "View summary / 查看总览摘要", viewSummary},
}

func newQueryCmd() *cobra.Command {
	return newQueryCmdWithDeps(loadConfig, dbOpener)
}

// newQueryCmdWithDeps 构造 query 根命令;load/open 可注入供包内测试根命令的
// 真实 RunE 接线(生产路径传入 loadConfig 与 dbOpener)。六个内置子命令与
// custom 保持既有执行路径;用户配置中的名称绝不动态 AddCommand。
func newQueryCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query [<name> [DATE|DATE-DATE] | DATE|DATE-DATE]",
		Short: "Query token usage statistics / 查询 token 使用统计",
		Long: ui.Bi(
			"Query token usage statistics. With no args, runs the default view (query.default, built-in fallback client). Accepts either a date for the default view or a configured view name plus an optional date: DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months. Examples: token-usage query <name> [date]; equivalent explicit form: token-usage query custom <name> [date].",
			"查询 token 使用统计。无参数时执行默认视图（query.default，内置回退 client）。可附加一个日期作用于默认视图，或指定已配置视图名加可选日期：DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月。示例：token-usage query <name> [date]；等价显式写法：token-usage query custom <name> [date]。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 {
				return queryUsageError(len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := parseQueryInvocation(args)
			if err != nil {
				return err
			}
			if inv.named {
				return runQueryNamedWithDeps(cmd, inv.name, inv.dates, load, open)
			}
			return runQueryWithDeps(cmd, args, viewDefault, load, open)
		},
	}

	for _, meta := range queryBuiltinCmds {
		cmd.AddCommand(newQuerySubCmd(meta.name, meta.short, meta.view))
	}
	cmd.AddCommand(newQueryCustomCmd())
	cmd.AddCommand(newQueryListCmdWithDeps(load))

	return cmd
}

// queryUsageError 是根命令位置参数超限的专用双语用法错误:
// 说明允许「无参数、一个日期、一个名称、名称加一个日期」四种形态。
func queryUsageError(got int) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("query accepts at most 2 positional args (no args, one date or date range, one view name, or a view name plus one date), got %d. Examples: token-usage query 20260701 | token-usage query <name> <date>", got),
		fmt.Sprintf("query 至多接受 2 个位置参数（无参数、一个日期或日期区间、一个视图名、视图名加一个日期），当前 %d 个。示例：token-usage query 20260701 | token-usage query <name> <date>", got),
	))
}

// queryFirstNameMustBeViewNameError 两参数形态且首参数以数字开头时的专用双语错误:
// 该位置须为视图名称,给出简写与纯日期两种示例,不再检查第二参数。
func queryFirstNameMustBeViewNameError(arg string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid arg %q: with two positional args the first must be a configured view name. Examples: token-usage query <name> <date> | token-usage query <date>", arg),
		fmt.Sprintf("无效参数 %q：两个位置参数时第一个必须是已配置视图名。示例：token-usage query <name> <date> | token-usage query <date>", arg),
	))
}

// newQuerySubCmd 构造 query 的一个子命令。子命令仅选定视图，
// 配置加载/DB 打开/日期解析/输出/异常提示逻辑全部复用 runQuery。
func newQuerySubCmd(name, short string, view queryView) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [DATE|DATE-DATE]",
		Short: short,
		Long: short + " " + ui.Bi(
			"Accepts one optional positional arg: DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months; defaults to today.",
			"。可附加一个位置参数：DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月；缺省时默认今天。",
		),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQuery(cmd, args, view)
		},
	}
}

// newQueryCustomCmd 构造 query custom 子命令:<name> 引用 query.subqueries 或
// query.groups 中已定义的对象。独立命令(不复用单日期参数工厂),参数为
// RangeArgs(1,2) 且缺参/超参错误为双语;日期解析先于配置与数据库打开。
func newQueryCustomCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "custom <name> [DATE|DATE-DATE]",
		Short: "Run a configured custom or group query / 执行已配置的自定义或组合查询",
		Long: ui.Bi(
			"Run a custom or group query defined in config.toml ([query.subqueries] or [query.groups]). Accepts the view name plus one optional date arg: DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months; defaults to today.",
			"执行 config.toml 中定义的自定义或组合查询（[query.subqueries] 或 [query.groups]）。参数为视图名加一个可选日期：DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月；缺省时默认今天。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return errors.New(ui.Bi(
					"custom requires a query view name and accepts at most one optional date arg",
					"custom 需要一个查询视图名,至多再附加一个可选日期参数",
				))
			}
			if len(args) > 2 {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("custom accepts at most 2 args (view name and optional date), got %d", len(args)),
					fmt.Sprintf("custom 至多接受 2 个参数(视图名和可选日期),当前 %d 个", len(args)),
				))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var dateArgs []string
			if len(args) > 1 {
				dateArgs = args[1:]
			}
			return runQueryCustomWithDeps(cmd, args[0], dateArgs, loadConfig, dbOpener)
		},
	}
}

// runQuery 是 query 及其子命令的公共执行入口。
func runQuery(cmd *cobra.Command, args []string, view queryView) error {
	return runQueryWithDeps(cmd, args, view, loadConfig, dbOpener)
}

func runQueryWithDeps(
	cmd *cobra.Command,
	args []string,
	view queryView,
	load func() (*config.Config, error),
	open func(string) (*db.DB, error),
) error {
	// 参数错误必须在配置、日志或 DB 等运行时资源打开之前返回。
	dates, err := parseDateArgs(args, true, "query")
	if err != nil {
		return err
	}

	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}

	// 裸 query:解析 query.default 指向的对象;querydef 语义错误只在此路径拒绝。
	if view == viewDefault {
		defs, err := parseQueryDefinitions(cfg)
		if err != nil {
			return err
		}
		usageDB, err := open(queryDBPath(cfg))
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
		}
		defer usageDB.Close()
		return executeDefaultQuery(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, defs, cfg.ProviderAliases)
	}

	// summary 不读取输出布局,保持既有完整摘要。
	if view == viewSummary {
		usageDB, err := open(queryDBPath(cfg))
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
		}
		defer usageDB.Close()
		return executeQueryDatesWithAliases(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, view, cfg.ProviderAliases, nil)
	}

	// 静态表格命令只解析 query.output:无关的视图定义错误不阻断它们,
	// 但布局自身错误在打开数据库前直接失败(该段决定它们如何渲染,
	// 静默回退会掩盖用户配置错误)。顶层 query 问题态没有可信的
	// [query.output],静默使用默认七列,保持静态视图不被顶层坏 query 阻断。
	layout, err := staticTableOutputLayout(cfg)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := open(dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	return executeQueryDatesWithAliases(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, view, cfg.ProviderAliases, layout)
}

// staticTableOutputLayout 为受布局影响的静态表格命令解析输出布局:
// 顶层问题态返回默认布局(不报错),否则只把 query.output 的诊断作为
// 该路径的开库前错误;视图定义错误被有意隔离(由完整 query 路径报告)。
func staticTableOutputLayout(cfg *config.Config) ([]string, error) {
	if len(cfg.RawQueryTopLevelIssues) > 0 {
		return ui.DefaultOutputColumns(), nil
	}
	return querydef.ParseOutputLayout(querydef.Input{RawQuery: cfg.RawQuery})
}

// queryInvocation 是 query 根命令位置参数的分派结果。
type queryInvocation struct {
	// named 为 true 时按具名已配置视图执行(name 来自 query.subqueries/groups);
	// false 时走既有默认目标路径(query.default)。
	named bool
	name  string
	dates []string // 规范化后的 YYYY-MM-DD 列表
}

// parseQueryInvocation 是 query 根命令的纯参数分派函数:
// 只处理参数个数与日期,不加载配置、不初始化日志、不打开 DB。
//
//   - 零参数:默认目标,今天;
//   - 一个参数:以 ASCII 数字开头按日期/区间解析(非法时复用既有单参数日期错误),
//     其余视为视图名并使用今天;
//   - 两个参数:仅允许「视图名 + 日期」;首参数以数字开头时固定优先报
//     「此位置须为视图名称」的双语用法错误,且不再检查第二参数;
//   - 三个及以上:同一份双语超参用法错误。
func parseQueryInvocation(args []string) (*queryInvocation, error) {
	switch len(args) {
	case 0:
		return &queryInvocation{dates: []string{todayDate()}}, nil
	case 1:
		if startsWithASCIIDigit(args[0]) {
			dates, err := parseDateArgs(args, true, "query")
			if err != nil {
				return nil, err
			}
			return &queryInvocation{dates: dates}, nil
		}
		return &queryInvocation{named: true, name: args[0], dates: []string{todayDate()}}, nil
	default:
		if len(args) > 2 {
			return nil, queryUsageError(len(args))
		}
		if startsWithASCIIDigit(args[0]) {
			return nil, queryFirstNameMustBeViewNameError(args[0])
		}
		// 第二日期以单元素切片校验,复用日期形态错误而不触发
		// parseDateArgs 的「仅接受 0 或 1 个参数」分支。
		dates, err := parseDateArgs([]string{args[1]}, true, "query")
		if err != nil {
			return nil, err
		}
		return &queryInvocation{named: true, name: args[0], dates: dates}, nil
	}
}

func startsWithASCIIDigit(s string) bool {
	if s == "" {
		return false
	}
	return s[0] >= '0' && s[0] <= '9'
}

func todayDate() string {
	return time.Now().Format("2006-01-02")
}

// runQueryNamedWithDeps 是直接写法与 custom 写法共用的具名执行链:
// 入口各自完成日期校验后调用;依次加载配置 → 解析 query 定义 → 按名解析
// target → 打开 DB → executeTargetQuery。配置名合法性判定仍收敛在
// querydef.Parse 与 resolveTarget,坏定义错误不打开 DB 且可定位。
func runQueryNamedWithDeps(
	cmd *cobra.Command,
	name string,
	dates []string,
	load func() (*config.Config, error),
	open func(string) (*db.DB, error),
) error {
	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}

	defs, err := parseQueryDefinitions(cfg)
	if err != nil {
		return err
	}
	target, ok := resolveTarget(defs, name)
	if !ok {
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("unknown query view %q (use a name defined in query.subqueries or query.groups)", name),
			fmt.Sprintf("未知的查询视图 %q(请使用 query.subqueries 或 query.groups 中定义的名称)", name),
		))
	}

	usageDB, err := open(queryDBPath(cfg))
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	return executeTargetQuery(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, defs, target, cfg.ProviderAliases)
}

// runQueryCustomWithDeps 执行 query custom <name> [date]:
// 日期先解析(示例文案为完整命令形态 "query custom"),随后进入与直接写法
// 共用的具名执行链。
func runQueryCustomWithDeps(
	cmd *cobra.Command,
	name string,
	dateArgs []string,
	load func() (*config.Config, error),
	open func(string) (*db.DB, error),
) error {
	dates, err := parseDateArgs(dateArgs, true, "query custom")
	if err != nil {
		return err
	}
	return runQueryNamedWithDeps(cmd, name, dates, load, open)
}

// newQueryListCmd 构造生产用 list 子命令(固定使用 loadConfig)。
func newQueryListCmd() *cobra.Command {
	return newQueryListCmdWithDeps(loadConfig)
}

// newQueryListCmdWithDeps 构造 query list 静态子命令;load 可注入供包内测试。
// 它只读取有效配置并渲染已解析定义:不调用 dbOpener、不查询 usage.db、
// 不打印统计信息区、不读取采集错误、不修改任何配置或运行状态。
func newQueryListCmdWithDeps(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured query views / 列出已配置查询视图",
		Long: ui.Bi(
			"List configured query views from the effective config: default behavior, built-in query commands, custom subqueries and groups, plus how to invoke each view. This command only reads configuration; it never opens the usage database or reads collection data.",
			"列出有效配置中的已配置查询视图：默认行为、内置查询命令、自定义子查询与组合查询，以及各视图的调用方式。本命令只读取配置；不打开使用数据库、不读取采集数据。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("list accepts no positional args (got %d)", len(args)),
					fmt.Sprintf("list 不接受位置参数(当前 %d 个)", len(args)),
				))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueryListWithDeps(cmd.OutOrStdout(), load)
		},
	}
}

// queryDefaultCategory 把默认项映射为「来源 × 目标类别」的双语显示类别:
// 回退来源固定为内置回退,其余按目标 Kind 区分内置视图/自定义子查询/组合查询。
func queryDefaultCategory(defs *querydef.QueryDefinitions) string {
	switch {
	case defs.DefaultIsFallback:
		return ui.Bi("built-in fallback", "内置回退")
	case defs.Default.Kind == querydef.TargetBuiltin:
		return ui.Bi("built-in view", "内置视图")
	case defs.Default.Kind == querydef.TargetCustom:
		return ui.Bi("custom subquery", "自定义子查询")
	default:
		return ui.Bi("group", "组合查询")
	}
}

// runQueryListWithDeps 是 query list 的内部 runner:仅加载配置并渲染
// parseQueryDefinitions 的结果,供直接执行与可注入测试共用。输出顺序固定:
// 标题 → 默认行为 → 调用说明 → 内置表 → 自定义子查询 → 组合查询。
func runQueryListWithDeps(out io.Writer, load func() (*config.Config, error)) error {
	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	defs, err := parseQueryDefinitions(cfg)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, ui.Bi("Configured query views", "已配置查询视图"))

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi("Default behavior", "默认行为"))
	fmt.Fprintf(out, "token-usage query -> %s (%s)\n", defs.Default.Name, queryDefaultCategory(defs))

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi("Configured view invocation", "已配置视图调用"))
	fmt.Fprintf(out, "%s: token-usage query <name> [date]\n", ui.Bi("Direct", "简写"))
	fmt.Fprintf(out, "%s: token-usage query custom <name> [date] (%s)\n", ui.Bi("Explicit", "显式"), ui.Bi("equivalent", "等价"))

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi("Built-in query commands", "内置查询命令"))
	builtin := ui.NewTable([]string{ui.Bi("Command", "命令"), ui.Bi("Purpose", "用途")}, ui.AlignLeft, ui.AlignLeft)
	for _, meta := range queryBuiltinCmds {
		builtin.Row("token-usage query "+meta.name, meta.short)
	}
	queryListWriteTable(out, builtin)

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi("Custom subqueries", "自定义子查询"))
	if len(defs.Subqueries) == 0 {
		fmt.Fprintln(out, ui.Bi("None", "无"))
	} else {
		subs := ui.NewTable([]string{ui.Bi("Command", "命令"), ui.Bi("Dimensions", "维度")}, ui.AlignLeft, ui.AlignLeft)
		for _, s := range defs.Subqueries {
			dims := make([]string, len(s.Dimensions))
			for i, d := range s.Dimensions {
				dims[i] = string(d)
			}
			subs.Row("token-usage query "+s.Name, strings.Join(dims, ","))
		}
		queryListWriteTable(out, subs)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Bi("Groups", "组合查询"))
	if len(defs.Groups) == 0 {
		fmt.Fprintln(out, ui.Bi("None", "无"))
	} else {
		groups := ui.NewTable([]string{ui.Bi("Command", "命令"), ui.Bi("Members", "成员")}, ui.AlignLeft, ui.AlignLeft)
		for _, g := range defs.Groups {
			members := make([]string, len(g.Items))
			for i, item := range g.Items {
				members[i] = item.Name
			}
			groups.Row("token-usage query "+g.Name, strings.Join(members, ","))
		}
		queryListWriteTable(out, groups)
	}
	return nil
}

// queryListWriteTable 写出框线表并补足尾随换行;分区之间由调用方再输出空行,
// 保证「空分区显示 None / 无」与表格分区的版式节奏一致。
func queryListWriteTable(out io.Writer, table *ui.Table) {
	s := table.String()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	fmt.Fprint(out, s)
}

// queryDBPath 返回查询用数据库路径。
func queryDBPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "usage.db")
}

// querydefInput 把 config raw query 状态适配为 querydef 输入的统一转换点:
// config 侧顶层问题项映射为 querydef.TopLevelIssue,完整与局部解析入口
// 共用同一顶层共同前置语义。静态表格命令的布局路径有意不经过本转换——
// 它先预检顶层问题并提前返回默认布局,不消费诊断。
func querydefInput(cfg *config.Config) querydef.Input {
	issues := make(map[string]querydef.TopLevelIssue, len(cfg.RawQueryTopLevelIssues))
	for name, issue := range cfg.RawQueryTopLevelIssues {
		issues[name] = querydef.TopLevelIssue{Name: issue.Name, Kind: string(issue.Kind)}
	}
	return querydef.Input{
		RawQuery:               cfg.RawQuery,
		RawQueryTopLevelIssues: issues,
	}
}

// parseQueryDefinitions 把 config raw query 状态适配为 querydef 输入并完整解析。
func parseQueryDefinitions(cfg *config.Config) (*querydef.QueryDefinitions, error) {
	return querydef.Parse(querydefInput(cfg))
}

// executeDefaultQuery 执行 query.default 指向的对象。
func executeDefaultQuery(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, defs *querydef.QueryDefinitions, aliases map[string]string) error {
	return executeTargetQuery(ctx, out, usageDB, dates, defs, defs.Default, aliases)
}

// resolveTarget 按名称解析 custom 子命令的执行对象。
// 只允许 query.subqueries 与 query.groups 中定义的名称;内置视图与保留名
// 不属于 custom 的可引用集合,统一按未知名称报错。
func resolveTarget(defs *querydef.QueryDefinitions, name string) (querydef.Target, bool) {
	for _, s := range defs.Subqueries {
		if s.Name == name {
			return querydef.Target{Name: name, Kind: querydef.TargetCustom}, true
		}
	}
	for _, g := range defs.Groups {
		if g.Name == name {
			return querydef.Target{Name: name, Kind: querydef.TargetGroup}, true
		}
	}
	return querydef.Target{}, false
}

// newLayoutQuerier 构造应用输出列布局的 Querier。columns 为 nil 时使用默认七列;
// 非空序列经完整/局部解析已校验,此处防御性校验失败即内部不变式破坏。
func newLayoutQuerier(usageDB *db.DB, columns []string) (*querier.Querier, error) {
	q := querier.New(usageDB)
	if len(columns) == 0 {
		return q, nil
	}
	if err := q.SetOutputColumns(columns); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("invalid output column layout", "输出列布局不合法"), err)
	}
	return q, nil
}

// executeTargetQuery 把目标展开为渲染序列并逐表输出;全部表完成后统一输出一次异常警告。
// 统一统计信息区只在全部表格前打印一次,不随表数量重复;同一布局作用于每张表。
func executeTargetQuery(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, defs *querydef.QueryDefinitions, target querydef.Target, aliases map[string]string) error {
	views, err := targetDimensionViews(defs, target)
	if err != nil {
		return err
	}
	q, err := newLayoutQuerier(usageDB, defs.OutputColumns)
	if err != nil {
		return err
	}
	if err := printQueryStatisticsHeader(ctx, out, q, dates); err != nil {
		return err
	}
	for _, view := range views {
		view.Aliases = aliases
		result, err := q.RunDimensionView(ctx, dates, view)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
		}
		fmt.Fprintln(out, result)
	}
	return showErrorWarningsContext(ctx, out, usageDB, dates)
}

// targetDimensionViews 把目标展开为维度视图渲染序列:
// builtin 是一张单维表;custom 是一张多维表;group 按声明顺序展开成员。
func targetDimensionViews(defs *querydef.QueryDefinitions, target querydef.Target) ([]querier.DimensionView, error) {
	switch target.Kind {
	case querydef.TargetBuiltin:
		view, err := builtinDimensionView(target.Name)
		return []querier.DimensionView{view}, err
	case querydef.TargetCustom:
		for _, s := range defs.Subqueries {
			if s.Name != target.Name {
				continue
			}
			dims := make([]string, len(s.Dimensions))
			for i, d := range s.Dimensions {
				dims[i] = string(d)
			}
			return []querier.DimensionView{{
				Dimensions: dims,
				TitleEn:    "Custom view " + s.Name,
				TitleZh:    "自定义视图 " + s.Name,
			}}, nil
		}
	case querydef.TargetGroup:
		for _, g := range defs.Groups {
			if g.Name != target.Name {
				continue
			}
			var views []querier.DimensionView
			for _, item := range g.Items {
				sub, err := targetDimensionViews(defs, item)
				if err != nil {
					return nil, err
				}
				views = append(views, sub...)
			}
			return views, nil
		}
	}
	return nil, fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unknown query target %q", target.Name),
		fmt.Sprintf("未知的查询目标 %q", target.Name),
	))
}

// builtinDimensionView 返回内置单维视图的渲染定义(标题与内置子命令一致)。
func builtinDimensionView(name string) (querier.DimensionView, error) {
	switch name {
	case "client":
		return querier.DimensionView{Dimensions: []string{"client"}, TitleEn: "Group by client", TitleZh: "按客户端分组"}, nil
	case "model":
		return querier.DimensionView{Dimensions: []string{"model"}, TitleEn: "Group by model", TitleZh: "按模型分组"}, nil
	case "provider":
		return querier.DimensionView{Dimensions: []string{"provider"}, TitleEn: "Group by provider", TitleZh: "按供应商分组"}, nil
	case "project":
		return querier.DimensionView{Dimensions: []string{"project"}, TitleEn: "Group by project", TitleZh: "按项目分组"}, nil
	}
	return querier.DimensionView{}, fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unknown query dimension %q", name),
		fmt.Sprintf("未知查询维度 %q", name),
	))
}

// executeQuery 执行 query 的视图分发核心逻辑（不依赖 cobra，便于单测）：
// 参数解析 → querier 分发 → 输出 → 异常提示。
// 警告统一写到与结果相同的 out；query 固定使用 table 输出，
// 所有视图固定 table 输出，不再有 JSON/CSV 分流。
func executeQuery(out io.Writer, usageDB *db.DB, args []string, view queryView) error {
	return executeQueryContext(context.Background(), out, usageDB, args, view)
}

func executeQueryContext(ctx context.Context, out io.Writer, usageDB *db.DB, args []string, view queryView) error {
	// 参数解析先于分发，非法日期即时报错。
	dates, err := parseDateArgs(args, true, "query")
	if err != nil {
		return err
	}
	return executeQueryDates(ctx, out, usageDB, dates, view)
}

func executeQueryDates(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, view queryView) error {
	return executeQueryDatesWithAliases(ctx, out, usageDB, dates, view, nil, nil)
}

// executeQueryDatesWithAliases 执行静态视图;layout 为 nil 时使用默认七列布局。
func executeQueryDatesWithAliases(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, view queryView, aliases map[string]string, layout []string) error {
	q, err := newLayoutQuerier(usageDB, layout)
	if err != nil {
		return err
	}

	if err := printQueryStatisticsHeader(ctx, out, q, dates); err != nil {
		return err
	}

	var result string
	switch view {
	case viewModel:
		result, err = q.ByModel(ctx, dates)
	case viewProvider:
		result, err = q.ByProvider(ctx, dates, aliases)
	case viewProject:
		result, err = q.ByProject(ctx, dates)
	case viewSessions:
		result, err = q.Sessions(ctx, dates)
	case viewSummary:
		result, err = q.Summary(ctx, dates)
	default:
		result, err = q.ByClient(ctx, dates)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
	}

	fmt.Fprintln(out, result)
	return showErrorWarningsContext(ctx, out, usageDB, dates)
}

// printQueryStatisticsHeader 查询新鲜度指标并把统一信息区写到 out。
// 每个 query 执行入口在最开头调用一次;调用方保证 dates 非空(日期解析先于 DB 打开)。
func printQueryStatisticsHeader(ctx context.Context, out io.Writer, q *querier.Querier, dates []string) error {
	fresh, err := q.Freshness(ctx, dates)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to query data freshness", "查询数据新鲜度失败"), err)
	}
	first, last := querier.DateBounds(dates)
	fmt.Fprint(out, queryStatisticsHeader(first, last, fresh))
	return nil
}

// queryStatisticsHeader 渲染所有 query 输出共用的统计信息区:固定标题、
// 本次查询的实际日期范围、范围内消息事件边界(数据截至)与全库最近一次
// 成功采集完成时间。缺数据的两项分别显示 em dash。标题不携带执行时间或日期。
func queryStatisticsHeader(first, last string, fresh querier.Freshness) string {
	rangeText := first
	if first != last {
		rangeText = first + " ~ " + last
	}
	through := emDash
	if fresh.MaxMessageTS > 0 {
		through = time.UnixMilli(fresh.MaxMessageTS).Local().Format(time.DateTime)
	}
	collected := emDash
	if !fresh.LastCollection.IsZero() {
		collected = fresh.LastCollection.Format(time.DateTime)
	}
	var sb strings.Builder
	sb.WriteString(ui.Bi("Usage statistics", "使用统计") + "\n")
	sb.WriteString(ui.Bi("Units", "单位") + ":\n")
	sb.WriteString("  1 K = 1,000 (thousand / 一千)\n")
	sb.WriteString("  1 M = 1,000 K = 1,000,000 (million / 一百万)\n")
	sb.WriteString("  1 B = 1,000 M = 1,000,000,000 (billion / 十亿)\n")
	fmt.Fprintf(&sb, "%s: %s\n", ui.Bi("Query range", "统计范围"), rangeText)
	fmt.Fprintf(&sb, "%s: %s\n", ui.Bi("Data through", "数据截至"), through)
	fmt.Fprintf(&sb, "%s: %s\n", ui.Bi("Last successful collection", "最近成功采集"), collected)
	sb.WriteString("\n")
	return sb.String()
}

func showErrorWarnings(out io.Writer, usageDB *db.DB, dates []string) error {
	return showErrorWarningsContext(context.Background(), out, usageDB, dates)
}

func showErrorWarningsContext(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string) error {
	errs, err := db.GetErrorsContext(ctx, usageDB, db.ErrorFilter{Dates: dates, Unresolved: true})
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to query collection errors", "查询采集异常失败"), err)
	}
	if len(errs) == 0 {
		return nil
	}

	fmt.Fprintf(out, "\n⚠️  %s：\n", ui.Bi(
		fmt.Sprintf("collection errors (%d)", len(errs)),
		fmt.Sprintf("采集异常（%d 条）", len(errs)),
	))
	for _, e := range errs {
		msg := truncateRunes(e.Message, 50)
		fmt.Fprintf(out, "  - %s (%s): %s", e.Source, e.Date, msg)
		if e.RetryCount > 0 {
			fmt.Fprintf(out, "，%s", ui.Bi(fmt.Sprintf("retried %d times", e.RetryCount), fmt.Sprintf("已重试 %d 次", e.RetryCount)))
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, ui.Bi("  Run `token-usage errors` for details, `token-usage collect retry` to retry", "  运行 `token-usage errors` 查看详情，`token-usage collect retry` 重试"))
	return nil
}
