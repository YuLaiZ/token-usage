package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// exportViews 是 export 命令允许的内置视图白名单(有序,声明顺序即错误文案中的
// 允许集合顺序)。导出面向机器消费,行 schema 无法承载 summary 的键值摘要与
// heatmap 的矩阵渲染;配置视图(query.subqueries/query.groups)另行支持。
var exportViews = []string{"client", "model", "provider", "project", "day", "month", "hour", "weekday", "session"}

// exportMetricColumns 是导出的固定指标列(列名与顺序),与 messages 聚合列的
// 源顺序一致。导出是固定机器 schema:不应用 [query.output.columns] 输出布局,
// 值为原始整数(不做 K/M 缩写),不含总计行。
var exportMetricColumns = []string{"requests", "input", "output", "cache_read", "cache_create", "reasoning", "total"}

// exportSessionColumns 是 session 视图的固定键列:duration_ms 是会话首末消息
// 的毫秒差(请求跨度),机器消费方按原始整数读取。
var exportSessionColumns = []string{"client", "project", "title", "duration_ms"}

// exportKeyColumn 把视图名映射为导出的键列名:day 列在机器 schema 中固定为
// date,month 等其余视图键列名与视图名一致,走默认分支即可。
func exportKeyColumn(view string) string {
	if view == "day" {
		return "date"
	}
	return view
}

// exportMetricValues 按固定指标列顺序返回聚合的原始整数值。
func exportMetricValues(agg querier.GroupAggregate) []int64 {
	return []int64{
		agg.Requests, agg.FreshInput, agg.OutputTokens,
		agg.CacheRead, agg.CacheCreate, agg.Reasoning, agg.TotalTokens,
	}
}

func newExportCmd() *cobra.Command {
	return newExportCmdWithDeps(loadConfig, dbOpener)
}

// newExportCmdWithDeps 构造 export 命令;load/open 可注入供包内测试真实 RunE
// 接线(生产路径传入 loadConfig 与 dbOpener,与 query 命令一致)。
func newExportCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export [view] [DATE|DATE-DATE]",
		Short: "Export usage data as CSV or JSON / 以 CSV 或 JSON 导出使用数据",
		Long: ui.Bi(
			"Export aggregated usage data as machine-readable CSV or JSON to standard output. View selection matches `token-usage query`: with no view, the default view is exported (query.default, built-in fallback client); view accepts a built-in view (client, model, provider, project, day, month, hour, weekday, session) or a configured name from query.subqueries/query.groups. A custom subquery exports as one row per dimension combination with one key column per dimension; a group exports member views as CSV sections in declaration order separated by blank lines, or as a JSON object mapping member names to their row arrays (keys alphabetical). summary and heatmap have no row schema and are not exportable. The date argument accepts the same forms as query: a day (YYYYMMDD), month (YYYYMM), year (YYYY; single arg only), or an inclusive DATE-DATE range; it defaults to today. --format selects csv (default) or json. Output is pure data on stdout: no statistics header, no total row, raw integers without K/M abbreviations, a fixed column set that ignores the [query.output.columns] layout, UTF-8 without BOM; redirect it to save a file. Collection-error warnings go to stderr.",
			"将用量聚合数据以机器可读的 CSV 或 JSON 导出到标准输出。视图选择与 `token-usage query` 一致：不带视图时导出默认视图（query.default，内置回退 client）；视图可为内置视图（client、model、provider、project、day、month、hour、weekday、session）或 query.subqueries/query.groups 中已配置的名称。自定义子查询按维度组合逐行导出，每个维度一列键列；组合查询导出成员视图——CSV 为多段、按声明顺序排列且段间空行分隔，JSON 为成员名到行数组的对象映射（键按字母序）。summary 与 heatmap 没有行 schema，不支持导出。日期参数与 query 相同：日 YYYYMMDD、月 YYYYMM、年 YYYY（年仅单独使用）或闭区间 DATE-DATE，缺省今天。--format 选择 csv（默认）或 json。stdout 为纯数据：无统计信息区、无总计行、整数为原始值（不做 K/M 缩写）、列固定且不应用 [query.output.columns] 输出布局、UTF-8 无 BOM；可用重定向保存文件。采集异常警告写 stderr。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 {
				return exportUsageError(len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			return runExportWithDeps(cmd, args, format, load, open)
		},
	}
	// --format 是本地 flag,缺省 csv;取值白名单在配置加载与开库之前校验。
	cmd.Flags().String("format", "csv", ui.Bi("output format: csv or json", "输出格式：csv 或 json"))
	return cmd
}

// exportInvocation 是 export 命令位置参数的分派结果。
type exportInvocation struct {
	// named 区分「视图名来自位置参数」与「未传视图」:显式空串按未知名拒绝,
	// 只有未传视图才走缺省路径(query.default,内置回退 client)。
	named bool
	view  string   // 视图名;named 为 false 时不消费
	dates []string // 规范化后的 YYYY-MM-DD 列表
}

// parseExportInvocation 是 export 命令的纯参数分派函数,镜像 query 根命令形态:
// 只处理参数个数与日期,不加载配置、不初始化日志、不打开 DB。
//
//   - 零参数:缺省视图 + 今天;
//   - 一个参数:以 ASCII 数字开头按日期/区间解析(缺省视图),其余视为
//     视图名并使用今天;
//   - 两个参数:仅允许「视图名 + 日期」;首参数以数字开头时固定优先报
//     「此位置须为视图名称」的双语用法错误,且不再检查第二参数;
//   - 三个及以上:同一份双语超参用法错误。
func parseExportInvocation(args []string) (*exportInvocation, error) {
	switch len(args) {
	case 0:
		return &exportInvocation{dates: []string{todayDate()}}, nil
	case 1:
		if startsWithASCIIDigit(args[0]) {
			dates, err := parseDateArgs(args, true, "export")
			if err != nil {
				return nil, err
			}
			return &exportInvocation{dates: dates}, nil
		}
		return &exportInvocation{named: true, view: args[0], dates: []string{todayDate()}}, nil
	case 2:
		if startsWithASCIIDigit(args[0]) {
			return nil, exportFirstNameMustBeViewNameError(args[0])
		}
		// 第二日期以单元素切片校验,复用日期形态错误而不触发
		// parseDateArgs 的「仅接受 0 或 1 个参数」分支。
		dates, err := parseDateArgs([]string{args[1]}, true, "export")
		if err != nil {
			return nil, err
		}
		return &exportInvocation{named: true, view: args[0], dates: dates}, nil
	default:
		return nil, exportUsageError(len(args))
	}
}

// exportUsageError 是位置参数超限的专用双语用法错误:
// 说明允许「无参数、一个日期、一个视图名、视图名加一个日期」四种形态。
func exportUsageError(got int) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("export accepts at most 2 positional args (no args, one date or date range, one view name, or a view name plus one date), got %d. Examples: token-usage export 20260701 | token-usage export day 20260701", got),
		fmt.Sprintf("export 至多接受 2 个位置参数（无参数、一个日期或日期区间、一个视图名、视图名加一个日期），当前 %d 个。示例：token-usage export 20260701 | token-usage export day 20260701", got),
	))
}

// exportFirstNameMustBeViewNameError 两参数形态且首参数以数字开头时的专用双语
// 错误,语义与 query 同类错误一致,文案使用 export 的命令形态;不再检查第二参数。
func exportFirstNameMustBeViewNameError(arg string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid arg %q: with two positional args the first must be an export view name. Examples: token-usage export <view> <date> | token-usage export <date>", arg),
		fmt.Sprintf("无效参数 %q：两个位置参数时第一个必须是导出视图名。示例：token-usage export <view> <date> | token-usage export <date>", arg),
	))
}

// exportUnknownViewError 未知视图:错误含动态允许集合(内置视图+已配置视图名),
// 在完整解析与打开数据库之前拒绝。
func exportUnknownViewError(view string, defs *querydef.QueryDefinitions) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unknown export view %q (allowed: %s)", view, configuredViewNames(exportViews, defs)),
		fmt.Sprintf("未知的导出视图 %q(允许: %s)", view, configuredViewNames(exportViews, defs)),
	))
}

// exportFormatError 非法 --format 取值:在加载配置与开库之前拒绝。
func exportFormatError(value string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("invalid --format %q (allowed: csv, json)", value),
		fmt.Sprintf("无效的 --format %q（允许：csv、json）", value),
	))
}

// exportViewAllowed 判定视图名是否在导出白名单内。
func exportViewAllowed(view string) bool {
	for _, v := range exportViews {
		if v == view {
			return true
		}
	}
	return false
}

// exportFailedErr 统一包装聚合与渲染错误。
func exportFailedErr(err error) error {
	return fmt.Errorf("%s: %w", ui.Bi("export failed", "导出失败"), err)
}

// runExportWithDeps 是 export 的公共执行入口:位置参数分派 → --format 白名单
// (加载配置与打开数据库之前拒绝)→ 加载配置 → 视图解析 → 打开数据库 →
// 内存聚合 → 一次性写 stdout(错误路径不写半成品数据)→ 采集异常警告写 stderr。
//
// 视图解析与 query 命令族同一合同:显式内置视图名走静态路径(无关的视图定义
// 错误不阻断);缺省视图与配置视图名走完整解析(视图定义错误在此拒绝);
// 两者皆非的名字在打开数据库之前以动态允许集合拒绝。
func runExportWithDeps(
	cmd *cobra.Command,
	args []string,
	format string,
	load func() (*config.Config, error),
	open func(string) (*db.DB, error),
) error {
	inv, err := parseExportInvocation(args)
	if err != nil {
		return err
	}
	if format != "csv" && format != "json" {
		return exportFormatError(format)
	}

	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}

	// 目标分派:builtinTarget 为空时表示「显式内置白名单之外的路径」,
	// 需要完整解析视图定义后按 target 执行。
	var builtinTarget string
	var defs *querydef.QueryDefinitions
	var target querydef.Target
	if inv.named && exportViewAllowed(inv.view) {
		builtinTarget = inv.view
	} else if inv.named {
		var err error
		defs, err = parseQueryDefinitions(cfg)
		if err != nil {
			return err
		}
		resolved, ok := resolveTarget(defs, inv.view)
		if !ok {
			return exportUnknownViewError(inv.view, defs)
		}
		target = resolved
	} else {
		var err error
		defs, err = parseQueryDefinitions(cfg)
		if err != nil {
			return err
		}
		target = defs.Default
	}

	usageDB, err := open(queryDBPath(cfg))
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	ctx := cmdContext(cmd)
	q := querier.New(usageDB)
	var payload string
	switch {
	case builtinTarget == "session":
		payload, err = exportSessionPayload(ctx, q, inv.dates, format)
	case builtinTarget != "":
		payload, err = exportDimensionPayload(ctx, q, builtinTarget, inv.dates, format, cfg.ProviderAliases)
	default:
		switch target.Kind {
		case querydef.TargetBuiltin:
			payload, err = exportDimensionPayload(ctx, q, target.Name, inv.dates, format, cfg.ProviderAliases)
		case querydef.TargetCustom:
			payload, err = exportCustomPayload(ctx, q, defs, target.Name, inv.dates, format, cfg.ProviderAliases)
		default:
			payload, err = exportGroupPayload(ctx, q, defs, target.Name, inv.dates, format, cfg.ProviderAliases)
		}
	}
	if err != nil {
		return err
	}

	// stdout 纯数据契约:数据先在内存聚合并渲染完成,再一次性写出。
	if _, err := fmt.Fprint(cmd.OutOrStdout(), payload); err != nil {
		return exportFailedErr(err)
	}
	// 采集异常警告改写 stderr,保持 stdout 只有纯数据(与 query 共用实现)。
	return showErrorWarningsContext(ctx, cmd.ErrOrStderr(), usageDB, inv.dates)
}

// exportDimensionPayload 聚合并渲染维度视图(client/model/provider/project/day)。
// 复用 RunDimensionView 的聚合核,保证行集合与排序一致;provider 别名与 query
// 行为一致(仅 provider 维度实际消费),总计不导出。
func exportDimensionPayload(ctx context.Context, q *querier.Querier, view string, dates []string, format string, aliases map[string]string) (string, error) {
	rows, _, err := q.AggregateDimensionView(ctx, dates, querier.DimensionView{
		Dimensions: []string{view},
		Aliases:    aliases,
	})
	if err != nil {
		return "", exportFailedErr(err)
	}
	var payload string
	if format == "json" {
		payload, err = renderDimensionJSON([]string{exportKeyColumn(view)}, rows)
	} else {
		payload, err = renderDimensionCSV([]string{exportKeyColumn(view)}, rows)
	}
	if err != nil {
		return "", exportFailedErr(err)
	}
	return payload, nil
}

// exportKeyColumns 把视图维度名序列映射为导出键列名(day 列固定为 date,
// 其余维度键列名与维度名一致)。
func exportKeyColumns(dims []string) []string {
	cols := make([]string, len(dims))
	for i, dim := range dims {
		cols[i] = exportKeyColumn(dim)
	}
	return cols
}

// exportViewSpec 是导出的一个视图段:成员名(JSON 对象键)与键列维度序列。
type exportViewSpec struct {
	name string
	dims []string
}

// lookupSubquery 按名查找已定义子查询。
func lookupSubquery(defs *querydef.QueryDefinitions, name string) (querydef.CustomSubquery, bool) {
	for _, s := range defs.Subqueries {
		if s.Name == name {
			return s, true
		}
	}
	return querydef.CustomSubquery{}, false
}

// exportTargetSpecs 把配置视图目标展开为导出段序列:custom 是单段多维;
// group 按声明顺序展开成员(builtin 单维、custom 多维)。名称在解析期已保证
// 唯一(builtin 保留名与自定义名不相交)。
func exportTargetSpecs(defs *querydef.QueryDefinitions, target querydef.Target) ([]exportViewSpec, error) {
	switch target.Kind {
	case querydef.TargetCustom:
		if s, ok := lookupSubquery(defs, target.Name); ok {
			dims := make([]string, len(s.Dimensions))
			for i, d := range s.Dimensions {
				dims[i] = string(d)
			}
			return []exportViewSpec{{name: s.Name, dims: dims}}, nil
		}
	case querydef.TargetGroup:
		for _, g := range defs.Groups {
			if g.Name != target.Name {
				continue
			}
			specs := make([]exportViewSpec, 0, len(g.Items))
			for _, item := range g.Items {
				switch item.Kind {
				case querydef.TargetBuiltin:
					specs = append(specs, exportViewSpec{name: item.Name, dims: []string{item.Name}})
				case querydef.TargetCustom:
					if sub, ok := lookupSubquery(defs, item.Name); ok {
						dims := make([]string, len(sub.Dimensions))
						for i, d := range sub.Dimensions {
							dims[i] = string(d)
						}
						specs = append(specs, exportViewSpec{name: sub.Name, dims: dims})
					} else {
						// 解析合同保证 group 成员引用的子查询存在;
						// 不变式破坏时 fail loud,不得静默丢成员段。
						return nil, fmt.Errorf("%s", ui.Bi(
							fmt.Sprintf("group %q references unknown subquery %q", g.Name, item.Name),
							fmt.Sprintf("组合查询 %q 引用了未知的子查询 %q", g.Name, item.Name),
						))
					}
				}
			}
			return specs, nil
		}
	}
	return nil, fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unknown export target %q", target.Name),
		fmt.Sprintf("未知的导出目标 %q", target.Name),
	))
}

// exportCustomPayload 聚合并渲染自定义子查询:按维度组合逐行导出,每个维度
// 一列键列,列顺序与子查询声明顺序一致。
func exportCustomPayload(ctx context.Context, q *querier.Querier, defs *querydef.QueryDefinitions, name string, dates []string, format string, aliases map[string]string) (string, error) {
	specs, err := exportTargetSpecs(defs, querydef.Target{Name: name, Kind: querydef.TargetCustom})
	if err != nil {
		return "", err
	}
	return exportSpecPayload(ctx, q, specs[0], dates, format, aliases)
}

// exportGroupPayload 聚合并渲染组合查询:按声明顺序导出成员视图。
// JSON 为成员名到行数组的对象映射;CSV 为多段,段间空行分隔,各段表头键列
// 随成员视图类型(单维/多维)。
func exportGroupPayload(ctx context.Context, q *querier.Querier, defs *querydef.QueryDefinitions, name string, dates []string, format string, aliases map[string]string) (string, error) {
	specs, err := exportTargetSpecs(defs, querydef.Target{Name: name, Kind: querydef.TargetGroup})
	if err != nil {
		return "", err
	}
	if format == "json" {
		members := make(map[string]any, len(specs))
		for _, spec := range specs {
			rows, _, err := q.AggregateDimensionView(ctx, dates, querier.DimensionView{
				Dimensions: spec.dims,
				Aliases:    aliases,
			})
			if err != nil {
				return "", exportFailedErr(err)
			}
			objects, err := dimensionRowObjects(exportKeyColumns(spec.dims), rows)
			if err != nil {
				return "", exportFailedErr(err)
			}
			members[spec.name] = objects
		}
		payload, err := marshalExportJSON(members)
		if err != nil {
			return "", exportFailedErr(err)
		}
		return payload, nil
	}
	var buf bytes.Buffer
	for i, spec := range specs {
		if i > 0 {
			buf.WriteString("\n")
		}
		section, err := exportSpecPayload(ctx, q, spec, dates, "csv", aliases)
		if err != nil {
			return "", err
		}
		buf.WriteString(section)
	}
	return buf.String(), nil
}

// exportSpecPayload 聚合并渲染单个视图段(单维或多维),格式路由复用维度
// 渲染函数;每段独立调用聚合,段序与行序互不影响。
func exportSpecPayload(ctx context.Context, q *querier.Querier, spec exportViewSpec, dates []string, format string, aliases map[string]string) (string, error) {
	rows, _, err := q.AggregateDimensionView(ctx, dates, querier.DimensionView{
		Dimensions: spec.dims,
		Aliases:    aliases,
	})
	if err != nil {
		return "", exportFailedErr(err)
	}
	keyCols := exportKeyColumns(spec.dims)
	if format == "json" {
		payload, err := renderDimensionJSON(keyCols, rows)
		if err != nil {
			return "", exportFailedErr(err)
		}
		return payload, nil
	}
	payload, err := renderDimensionCSV(keyCols, rows)
	if err != nil {
		return "", exportFailedErr(err)
	}
	return payload, nil
}

// exportSessionPayload 聚合并渲染会话视图。
func exportSessionPayload(ctx context.Context, q *querier.Querier, dates []string, format string) (string, error) {
	rows, err := q.SessionRows(ctx, dates)
	if err != nil {
		return "", exportFailedErr(err)
	}
	var payload string
	if format == "json" {
		payload, err = renderSessionJSON(rows)
	} else {
		payload, err = renderSessionCSV(rows)
	}
	if err != nil {
		return "", exportFailedErr(err)
	}
	return payload, nil
}

// renderDimensionCSV 把维度聚合行渲染为 CSV:表头为键列序列加固定指标列
// (单维视图键列恰为一列),值为原始整数,不含总计行;LF 行尾、UTF-8 无 BOM,
// 字段引号转义由 csv.Writer 负责。行键数与键列数的一致性在此守卫,
// 与 JSON 路径的 dimensionRowObjects 同一不变式。
func renderDimensionCSV(keyCols []string, rows []querier.DimensionRow) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append(append([]string{}, keyCols...), exportMetricColumns...)); err != nil {
		return "", err
	}
	for _, row := range rows {
		if len(row.Keys) != len(keyCols) {
			return "", fmt.Errorf("%s", ui.Bi(
				"export row keys do not match the view dimensions",
				"导出行键数与视图维度数不一致",
			))
		}
		record := exportRecord(row.Keys, row.Agg)
		if err := w.Write(record); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderSessionCSV 把会话行渲染为 CSV:表头固定 client,project,title 加固定
// 指标列;Project/Title 原样输出(空串就是空串),由 csv.Writer 负责引号转义。
func renderSessionCSV(rows []querier.SessionRow) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append(append([]string{}, exportSessionColumns...), exportMetricColumns...)); err != nil {
		return "", err
	}
	for _, row := range rows {
		record := exportRecord([]string{row.Client, row.Project, row.Title,
			strconv.FormatInt(row.LastTS-row.FirstTS, 10)}, row.Agg)
		if err := w.Write(record); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// exportRecord 把一行键序列与聚合值拼成导出记录:键列在前、固定指标列在后,
// 整数为原始十进制(不做 formatTokens 缩写)。
func exportRecord(keys []string, agg querier.GroupAggregate) []string {
	record := make([]string, 0, len(keys)+len(exportMetricColumns))
	record = append(record, keys...)
	for _, v := range exportMetricValues(agg) {
		record = append(record, strconv.FormatInt(v, 10))
	}
	return record
}

// dimensionRowObjects 把维度聚合行构造为 JSON 行对象数组:键名与 CSV 列名
// 一致(单维视图键名即 keyCols 唯一列,多维视图逐维度一列),整数为 JSON
// number、键为 string。
func dimensionRowObjects(keyCols []string, rows []querier.DimensionRow) ([]map[string]any, error) {
	objects := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if len(row.Keys) != len(keyCols) {
			return nil, fmt.Errorf("%s", ui.Bi(
				"export row keys do not match the view dimensions",
				"导出行键数与视图维度数不一致",
			))
		}
		object := make(map[string]any, len(keyCols)+len(exportMetricColumns))
		for i, col := range keyCols {
			object[col] = row.Keys[i]
		}
		appendExportMetrics(object, row.Agg)
		objects = append(objects, object)
	}
	return objects, nil
}

// renderDimensionJSON 把维度聚合行渲染为 JSON 对象数组,两空格缩进加尾随换行。
func renderDimensionJSON(keyCols []string, rows []querier.DimensionRow) (string, error) {
	objects, err := dimensionRowObjects(keyCols, rows)
	if err != nil {
		return "", err
	}
	return marshalExportJSON(objects)
}

// renderSessionJSON 把会话行渲染为 JSON 对象数组,Project/Title 保持源字段
// 原值(空串就是空串)。
func renderSessionJSON(rows []querier.SessionRow) (string, error) {
	objects := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		object := map[string]any{
			"client":      row.Client,
			"project":     row.Project,
			"title":       row.Title,
			"duration_ms": row.LastTS - row.FirstTS,
		}
		appendExportMetrics(object, row.Agg)
		objects = append(objects, object)
	}
	return marshalExportJSON(objects)
}

// appendExportMetrics 把固定指标列以原始整数写入导出对象。
func appendExportMetrics(object map[string]any, agg querier.GroupAggregate) {
	values := exportMetricValues(agg)
	for i, col := range exportMetricColumns {
		object[col] = values[i]
	}
}

// marshalExportJSON 以两空格缩进序列化并追加尾随换行。map 键由 encoding/json
// 按字母序输出:JSON 对象本无序,键集合与 CSV 列名一致且同一数据输出确定。
func marshalExportJSON(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
