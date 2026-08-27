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
		Use:   "query [<name> [YYYYMMDD|YYYYMMDD-YYYYMMDD] | YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: "Query token usage statistics / 查询 token 使用统计",
		Long: ui.Bi(
			"Query token usage statistics. With no args, runs the default view (query.default, built-in fallback client). Accepts either a date for the default view or a configured view name plus an optional date: a single date YYYYMMDD or a range YYYYMMDD-YYYYMMDD. Examples: token-usage query <name> [date]; equivalent explicit form: token-usage query custom <name> [date].",
			"查询 token 使用统计。无参数时执行默认视图（query.default，内置回退 client）。可附加一个日期作用于默认视图，或指定已配置视图名加可选日期：单个日期 YYYYMMDD 或区间 YYYYMMDD-YYYYMMDD。示例：token-usage query <name> [date]；等价显式写法：token-usage query custom <name> [date]。",
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

	return cmd
}

// queryUsageError 是根命令位置参数超限的专用双语用法错误:
// 说明允许「无参数、一个日期、一个名称、名称加一个日期」四种形态。
func queryUsageError(got int) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("query accepts at most 2 positional args (no args, one date YYYYMMDD or range, one view name, or a view name plus one date), got %d. Examples: token-usage query 20260701 | token-usage query <name> <date>", got),
		fmt.Sprintf("query 至多接受 2 个位置参数（无参数、一个日期 YYYYMMDD 或区间、一个视图名、视图名加一个日期），当前 %d 个。示例：token-usage query 20260701 | token-usage query <name> <date>", got),
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
		Use:   name + " [YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: short,
		Long: short + " " + ui.Bi(
			"Accepts one optional positional arg: a single date YYYYMMDD or a range YYYYMMDD-YYYYMMDD; defaults to today.",
			"。可附加一个位置参数：单个日期 YYYYMMDD 或日期范围 YYYYMMDD-YYYYMMDD；缺省时默认今天。",
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
		Use:   "custom <name> [YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: "Run a configured custom or group query / 执行已配置的自定义或组合查询",
		Long: ui.Bi(
			"Run a custom or group query defined in config.toml ([query.subqueries] or [query.groups]). Accepts the view name plus one optional date arg: a single date YYYYMMDD or a range YYYYMMDD-YYYYMMDD; defaults to today.",
			"执行 config.toml 中定义的自定义或组合查询（[query.subqueries] 或 [query.groups]）。参数为视图名加一个可选日期：单个日期 YYYYMMDD 或日期范围 YYYYMMDD-YYYYMMDD；缺省时默认今天。",
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

	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := open(dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	return executeQueryDatesWithAliases(cmdContext(cmd), cmd.OutOrStdout(), usageDB, dates, view, cfg.ProviderAliases)
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

// queryDBPath 返回查询用数据库路径。
func queryDBPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "usage.db")
}

// parseQueryDefinitions 把 config raw query 状态适配为 querydef 输入并解析。
func parseQueryDefinitions(cfg *config.Config) (*querydef.QueryDefinitions, error) {
	issues := make(map[string]querydef.TopLevelIssue, len(cfg.RawQueryTopLevelIssues))
	for name, issue := range cfg.RawQueryTopLevelIssues {
		issues[name] = querydef.TopLevelIssue{Name: issue.Name, Kind: string(issue.Kind)}
	}
	return querydef.Parse(querydef.Input{
		RawQuery:               cfg.RawQuery,
		RawQueryTopLevelIssues: issues,
	})
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

// executeTargetQuery 把目标展开为渲染序列并逐表输出;全部表完成后统一输出一次异常警告。
// 统一统计信息区只在全部表格前打印一次,不随表数量重复。
func executeTargetQuery(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, defs *querydef.QueryDefinitions, target querydef.Target, aliases map[string]string) error {
	views, err := targetDimensionViews(defs, target)
	if err != nil {
		return err
	}
	q := querier.New(usageDB)
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
	return executeQueryDatesWithAliases(ctx, out, usageDB, dates, view, nil)
}

func executeQueryDatesWithAliases(ctx context.Context, out io.Writer, usageDB *db.DB, dates []string, view queryView, aliases map[string]string) error {
	q := querier.New(usageDB)

	if err := printQueryStatisticsHeader(ctx, out, q, dates); err != nil {
		return err
	}

	var result string
	var err error
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
