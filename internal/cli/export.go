package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// exportViews 是 export 命令允许的内置视图白名单(有序,声明顺序即错误文案中的
// 允许集合顺序)。导出面向机器消费,不含 summary 与自定义视图。
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
			"Export aggregated usage data as machine-readable CSV or JSON to standard output. Views: client (default), model, provider, project, day, month, hour, weekday, session; summary and custom views are not supported. The date argument accepts the same forms as query: a day (YYYYMMDD), month (YYYYMM), year (YYYY; single arg only), or an inclusive DATE-DATE range; it defaults to today. --format selects csv (default) or json. Output is pure data on stdout: no statistics header, no total row, raw integers without K/M abbreviations, a fixed column set that ignores the [query.output.columns] layout, UTF-8 without BOM; redirect it to save a file. Collection-error warnings go to stderr.",
			"将用量聚合数据以机器可读的 CSV 或 JSON 导出到标准输出。视图集合：client（默认）、model、provider、project、day、month、hour、weekday、session；不支持 summary 与自定义视图。日期参数与 query 相同：日 YYYYMMDD、月 YYYYMM、年 YYYY（年仅单独使用）或闭区间 DATE-DATE，缺省今天。--format 选择 csv（默认）或 json。stdout 为纯数据：无统计信息区、无总计行、整数为原始值（不做 K/M 缩写）、列固定且不应用 [query.output.columns] 输出布局、UTF-8 无 BOM；可用重定向保存文件。采集异常警告写 stderr。",
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
	view  string
	dates []string // 规范化后的 YYYY-MM-DD 列表
}

// parseExportInvocation 是 export 命令的纯参数分派函数,镜像 query 根命令形态:
// 只处理参数个数与日期,不加载配置、不初始化日志、不打开 DB。
//
//   - 零参数:client 视图 + 今天;
//   - 一个参数:以 ASCII 数字开头按日期/区间解析(视图固定 client),其余视为
//     视图名并使用今天;
//   - 两个参数:仅允许「视图名 + 日期」;首参数以数字开头时固定优先报
//     「此位置须为视图名称」的双语用法错误,且不再检查第二参数;
//   - 三个及以上:同一份双语超参用法错误。
func parseExportInvocation(args []string) (*exportInvocation, error) {
	switch len(args) {
	case 0:
		return &exportInvocation{view: "client", dates: []string{todayDate()}}, nil
	case 1:
		if startsWithASCIIDigit(args[0]) {
			dates, err := parseDateArgs(args, true, "export")
			if err != nil {
				return nil, err
			}
			return &exportInvocation{view: "client", dates: dates}, nil
		}
		return &exportInvocation{view: args[0], dates: []string{todayDate()}}, nil
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
		return &exportInvocation{view: args[0], dates: dates}, nil
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

// exportUnknownViewError 未知视图:列出允许集合,在加载配置与开库之前拒绝。
func exportUnknownViewError(view string) error {
	return fmt.Errorf("%s", ui.Bi(
		fmt.Sprintf("unknown export view %q (allowed: %s)", view, strings.Join(exportViews, ", ")),
		fmt.Sprintf("未知的导出视图 %q(允许: %s)", view, strings.Join(exportViews, ", ")),
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

// runExportWithDeps 是 export 的公共执行入口:位置参数分派 → 视图与格式白名单
// (均在加载配置与打开数据库之前拒绝)→ 加载配置 → 打开数据库 → 内存聚合 →
// 一次性写 stdout(错误路径不写半成品数据)→ 采集异常警告写 stderr。
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
	if !exportViewAllowed(inv.view) {
		return exportUnknownViewError(inv.view)
	}
	if format != "csv" && format != "json" {
		return exportFormatError(format)
	}

	cfg, err := load()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	usageDB, err := open(queryDBPath(cfg))
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	defer usageDB.Close()

	ctx := cmdContext(cmd)
	q := querier.New(usageDB)
	var payload string
	if inv.view == "session" {
		payload, err = exportSessionPayload(ctx, q, inv.dates, format)
	} else {
		payload, err = exportDimensionPayload(ctx, q, inv.view, inv.dates, format, cfg.ProviderAliases)
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
		payload, err = renderDimensionJSON(exportKeyColumn(view), rows)
	} else {
		payload, err = renderDimensionCSV(exportKeyColumn(view), rows)
	}
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

// renderDimensionCSV 把维度聚合行渲染为 CSV:表头为键列加固定指标列,值为原始
// 整数,不含总计行;LF 行尾、UTF-8 无 BOM,字段引号转义由 csv.Writer 负责。
func renderDimensionCSV(keyCol string, rows []querier.DimensionRow) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append([]string{keyCol}, exportMetricColumns...)); err != nil {
		return "", err
	}
	for _, row := range rows {
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

// renderDimensionJSON 把维度聚合行渲染为 JSON 对象数组:键名与 CSV 列名一致
// (provider 视图键名固定 provider),整数为 JSON number、键为 string,
// 两空格缩进加尾随换行。
func renderDimensionJSON(keyCol string, rows []querier.DimensionRow) (string, error) {
	objects := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if len(row.Keys) != 1 {
			return "", fmt.Errorf("%s", ui.Bi(
				"export requires a single-dimension view",
				"导出视图必须是单一维度",
			))
		}
		object := map[string]any{keyCol: row.Keys[0]}
		appendExportMetrics(object, row.Agg)
		objects = append(objects, object)
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
