// Package ui 补充：query/errors 等表格的列名与标签常量。
// 集中定义防止同一概念在命令间漂移（如「客户端/数据源」两词混用）。
package ui

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
	HClient    = HeaderLines("Client", "客户端")
	HProvider  = HeaderLines("Provider", "供应商")
	HModel     = HeaderLines("Model", "模型")
	HProject   = HeaderLines("Project", "项目")
	HTitle     = HeaderLines("Title", "标题")
	HRequests  = HeaderLines("Requests", "请求数")
	HInput     = HeaderLines("Input", "输入")
	HOutput    = HeaderLines("Output", "输出")
	HCacheRead = HeaderLines("Cache Read", "缓存读取")
	HReasoning = HeaderLines("Reasoning", "推理")
	HTotal     = HeaderLines("Total", "总计")
	HCacheHit  = HeaderLines("Cache Hit", "缓存命中")
)
