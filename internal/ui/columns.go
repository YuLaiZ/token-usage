// Package ui 补充：query/errors 等表格的列名与标签常量。
// 集中定义防止同一概念在命令间漂移（如「客户端/数据源」两词混用）。
package ui

import "strings"

// 列名常量：全部经 Bi 双语，命令表格直接引用。
var (
	ColClient      = Bi("Client", "客户端")
	ColDate        = Bi("Date", "日期")
	ColRequests    = Bi("Requests", "请求数")
	ColInput       = Bi("Input", "输入")
	ColOutput      = Bi("Output", "输出")
	ColCacheRead   = Bi("Cache Read", "缓存读取")
	ColCacheCreate = Bi("Cache Create", "缓存创建")
	ColCacheHit    = Bi("Cache Hit", "缓存命中")
	ColTotal       = Bi("Total", "总计")
)

// ColCacheHit 缓存命中率列：cache_read 占全部输入（fresh input + cache read
// + cache create）的比例，衡量前缀缓存的有效性。

// ColReasoning 推理 token 列：思考输出与最终答案输出分开统计的维度。
var ColReasoning = Bi("Reasoning", "推理")

// HeaderLines 返回两行表头（上行英文、下行中文）：表格列宽取两行中较宽者，
// 相比单行「English / 中文」形态显著收窄列宽；供 query 系表格视图使用。
func HeaderLines(en, zh string) string {
	return en + "\n" + zh
}

// query 表格两行表头（与上方单行列名常量同源同译文）。
var (
	HClient      = HeaderLines("Client", "客户端")
	HProvider    = HeaderLines("Provider", "供应商")
	HModel       = HeaderLines("Model", "模型")
	HProject     = HeaderLines("Project", "项目")
	HTitle       = HeaderLines("Title", "标题")
	HRequests    = HeaderLines("Requests", "请求数")
	HInput       = HeaderLines("Input", "输入")
	HOutput      = HeaderLines("Output", "输出")
	HCacheRead   = HeaderLines("Cache Read", "缓存读取")
	HCacheCreate = HeaderLines("Cache Create", "缓存创建")
	HReasoning   = HeaderLines("Reasoning", "推理")
	HTotal       = HeaderLines("Total", "总计")
	HCacheHit    = HeaderLines("Cache Hit", "缓存命中")
)

// 输出指标列的稳定 ID：写入 query.output.columns 的有序字符串数组使用
// 这些字面量，大小写敏感；ID 是公开配置契约，不得重命名。
const (
	MetricRequests    = "requests"
	MetricInput       = "input"
	MetricOutput      = "output"
	MetricCacheRead   = "cache_read"
	MetricCacheCreate = "cache_create"
	MetricReasoning   = "reasoning"
	MetricTotal       = "total"
	MetricCacheHit    = "cache_hit"
)

// OutputMetric 描述一个可配置的输出指标列：稳定 ID 与双语两行表头。
type OutputMetric struct {
	ID     string
	Header string
}

// outputMetrics 以固定候选顺序枚举全部输出指标：默认七列在前、
// cache_create 殿后（默认隐藏）。该顺序同时是 TUI 候选列表顺序与
// 校验错误信息中允许集合的展示顺序，单一来源不得在别处复制。
var outputMetrics = []OutputMetric{
	{MetricRequests, HRequests},
	{MetricInput, HInput},
	{MetricOutput, HOutput},
	{MetricCacheRead, HCacheRead},
	{MetricReasoning, HReasoning},
	{MetricTotal, HTotal},
	{MetricCacheHit, HCacheHit},
	{MetricCacheCreate, HCacheCreate},
}

// DefaultOutputColumns 返回默认七列 ID 序列的独立副本（不含 cache_create）。
// 缺失 query.output 或缺失 columns 时按该序列渲染，保证升级后输出不变。
func DefaultOutputColumns() []string {
	ids := make([]string, 0, len(outputMetrics))
	for _, m := range outputMetrics {
		if m.ID == MetricCacheCreate {
			continue
		}
		ids = append(ids, m.ID)
	}
	return ids
}

// OutputMetricIDs 返回全部候选指标 ID 的独立副本（默认七列在前、
// cache_create 殿后），供 TUI 候选列表与校验允许集合使用。
func OutputMetricIDs() []string {
	ids := make([]string, len(outputMetrics))
	for i, m := range outputMetrics {
		ids[i] = m.ID
	}
	return ids
}

// OutputMetricHeader 返回指标 ID 对应的双语两行表头；未知 ID 返回 false。
func OutputMetricHeader(id string) (string, bool) {
	for _, m := range outputMetrics {
		if m.ID == id {
			return m.Header, true
		}
	}
	return "", false
}

// OutputColumnIDList 返回逗号分隔的候选 ID 列表（错误信息的允许集合）。
func OutputColumnIDList() string {
	return strings.Join(OutputMetricIDs(), ", ")
}

// OutputMetricLabel 返回指标 ID 的双语单行标签(如 "Requests / 请求数"),
// 供 TUI 输出列页渲染;未知 ID 原样返回。
func OutputMetricLabel(id string) string {
	header, ok := OutputMetricHeader(id)
	if !ok {
		return id
	}
	lines := strings.SplitN(header, "\n", 2)
	if len(lines) != 2 {
		return id
	}
	return Bi(lines[0], lines[1])
}
