package cli

import (
	"bytes"
	"html/template"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/fmtx"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// reportHTMLInput 是 buildReportHTML 的渲染输入:报告包已取齐的同一读快照
// 数据(区间文本/新鲜度/两期窗口与总量/按日行/8 张维度聚合/全部 SVG/会话
// 排行)。构建过程纯内存,不再触达数据库。
type reportHTMLInput struct {
	rangeText string
	fresh     querier.Freshness
	curStart  string
	curEnd    string
	baseStart string
	baseEnd   string
	cur       querier.RangeStats
	base      querier.RangeStats
	dayRows   []querier.DimensionRow
	dimCharts map[string]reportDimChart
	svgs      map[string]string
	topRows   []querier.SessionRow
}

// reportKPI 是总览区的一张指标卡:双语标签、主值与辅助说明。ElemID 是可选
// 的主值元素锚点(仅 Total tokens 卡携带 "kpi-total-value"),供快照一致性
// 测试从渲染产物中提取精确口径。
type reportKPI struct {
	Label   string
	LabelZh string
	ElemID  string
	Value   string
	Sub     string
}

// reportCompareRow 是两期对比表中的一行:双语指标名、两期值、带符号差值、
// 差值着色 class(pos/neg/空)与百分比(基线为 0 时 "--")。
type reportCompareRow struct {
	Metric      string
	Current     string
	Base        string
	Change      string
	ChangeClass string
	ChangePct   string
}

// reportTopRow 是 Top sessions 表中的一行:排名、会话描述与聚合值。
// DurationMS 是时长排序用的毫秒原值(Duration 为显示文本)。
type reportTopRow struct {
	Rank         int
	Title        string
	Client       string
	Project      string
	DurationMS   int64
	Duration     string
	Requests     int64
	RequestsText string
	Total        string
	TotalRaw     int64
}

// reportDataCell 是数据明细表的一个数值单元格:千分位显示文本与排序用的
// 精确整数(data-v)。
type reportDataCell struct {
	Text string
	Raw  int64
}

// reportDataRow 是数据明细表中的一行:维度键与 7 个指标单元格。
type reportDataRow struct {
	Key   string
	Cells []reportDataCell
}

// reportDataBlock 是数据明细区的一个可折叠维度块:双语维度名、行数、表头
// (Key + 7 指标)与数据行(行序保持聚合核返回序)。
type reportDataBlock struct {
	Name    string
	NameZh  string
	Count   int
	Headers []string
	Rows    []reportDataRow
}

// reportHTMLData 是报告页模板的渲染数据:全部字段已格式化为显示文本,
// 模板本身不做计算(SVG 经 template.HTML 原样内嵌,其余文本一律自动转义)。
type reportHTMLData struct {
	Title          string
	RangeText      string
	DataThrough    string
	LastCollection string
	CurStart       string
	CurEnd         string
	BaseStart      string
	BaseEnd        string
	KPIs           []reportKPI
	CompareRows    []reportCompareRow
	HeatmapSVG     template.HTML
	TrendSVGs      []template.HTML
	ShareSVGs      []template.HTML
	TopRows        []reportTopRow
	DataBlocks     []reportDataBlock
}

// reportHTMLTemplate 是报告页模板:自包含单文件,样式取自 serve 仪表板的
// 设计语言(深色计量仪表台、全等宽字体、圆角卡片、KPI 卡、面板、表格、
// details 折叠块;内嵌 SVG 图表同为深色底,二者同底色无缝拼接),
// 交互为一段内联 vanilla JS(表格排序 + 数据表一键导出 CSV,按当前排序
// 所见行、data-v 精确整数,与 serve 仪表板同一导出口径)。零外部资源:
// 无外链、无外脚本、无字体/图片引用,可离线双击打开。
const reportHTMLTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark">
<title>{{.Title}}</title>
<link rel="icon" href="data:image/svg+xml;base64,PHN2ZyB4bWxucz0naHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmcnIHZpZXdCb3g9JzAgMCAzMiAzMic+PHJlY3Qgd2lkdGg9JzMyJyBoZWlnaHQ9JzMyJyByeD0nNycgZmlsbD0nIzEzMWEyMicvPjxyZWN0IHg9JzYnIHk9JzE1JyB3aWR0aD0nNScgaGVpZ2h0PScxMScgcng9JzEuNScgZmlsbD0nIzVjYzhmZicvPjxyZWN0IHg9JzEzLjUnIHk9JzknIHdpZHRoPSc1JyBoZWlnaHQ9JzE3JyByeD0nMS41JyBmaWxsPScjZmZiNDU0Jy8+PHJlY3QgeD0nMjEnIHk9JzUnIHdpZHRoPSc1JyBoZWlnaHQ9JzIxJyByeD0nMS41JyBmaWxsPScjNTdkOWEzJy8+PC9zdmc+">
<style>
:root{--bg:#0c1117;--surface:#131a22;--surface-2:#182129;--line:#223041;--line-strong:#31435a;--ink:#dee8f2;--muted:#8ca0b4;--faint:#5b6e80;--accent:#5cc8ff;--pos:#57d9a3;--neg:#ff8a7a;--mono:ui-monospace,"SF Mono","Cascadia Code",Menlo,Consolas,"Liberation Mono",monospace}
*{box-sizing:border-box}
html{scroll-behavior:smooth;scroll-padding-top:76px}
body{margin:0;background:var(--bg);color:var(--ink);font:13px/1.6 var(--mono);-webkit-font-smoothing:antialiased}
.wrap{max-width:1240px;margin:0 auto;padding:0 22px}
.muted{color:var(--muted)}
.topbar{position:sticky;top:0;z-index:10;background:rgba(12,17,23,.88);border-bottom:1px solid var(--line);backdrop-filter:blur(10px)}
.bar-inner{display:flex;align-items:center;gap:18px;min-height:52px;flex-wrap:wrap;padding:8px 22px}
.brand{display:flex;align-items:center;gap:9px;font-weight:700;font-size:13.5px;letter-spacing:.02em;white-space:nowrap}
.brand::before{content:"";width:9px;height:9px;border-radius:2px;background:linear-gradient(135deg,var(--accent) 0%,#7a9eff 100%);box-shadow:0 0 8px rgba(92,200,255,.55)}
.sec-nav{display:flex;gap:10px;flex-wrap:wrap;margin-left:auto}
.sec-nav a{font-size:11.5px;color:var(--muted);text-decoration:none;letter-spacing:.03em}
.sec-nav a:hover,.sec-nav a.active{color:var(--accent)}
main{padding:26px 0 0}
section{margin:34px 0}
h2{display:flex;align-items:baseline;gap:10px;font-size:12px;font-weight:700;letter-spacing:.14em;text-transform:uppercase;margin:0 0 14px;line-height:1.3}
h2::before{content:"";width:14px;height:3px;border-radius:2px;background:var(--accent);align-self:center}
h2 .zh{color:var(--muted);font-weight:400;letter-spacing:.02em;text-transform:none}
.meta{font-size:11px;color:var(--muted);margin:0 0 16px}
.meta b{color:var(--ink);font-weight:600}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}
.kpi{background:var(--surface);border:1px solid var(--line);border-radius:10px;padding:12px 16px}
.kpi .label{font-size:10px;text-transform:uppercase;letter-spacing:.1em;color:var(--muted)}
.kpi .label .zh{text-transform:none;letter-spacing:0}
.kpi .value{font-size:22px;font-weight:700;font-variant-numeric:tabular-nums;letter-spacing:-.02em;margin-top:5px;line-height:1.2;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.kpi .sub{font-size:10.5px;color:var(--faint);margin-top:2px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.panel{background:var(--surface);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}
@media (max-width:900px){.grid{grid-template-columns:1fr}}
.chart{background:var(--surface);border:1px solid var(--line);border-radius:10px;padding:10px;min-height:120px}
.chart svg{display:block;width:100%;height:auto}
table{border-collapse:collapse;width:100%;font-size:12px;font-variant-numeric:tabular-nums}
th{font-size:10px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--faint);text-align:left;border-bottom:1px solid var(--line);padding:8px 10px;white-space:nowrap}
td{padding:7px 10px;border-bottom:1px solid rgba(34,48,65,.5);vertical-align:top}
tbody tr:last-child td{border-bottom:none}
tbody tr:hover td{background:rgba(92,200,255,.045)}
th.num,td.num{text-align:right}
td.num{white-space:nowrap}
th.sortable{cursor:pointer;user-select:none}
th.sortable::after{content:"↕";opacity:.25;margin-left:4px}
th.sorted-asc::after{content:"↑";opacity:1;color:var(--accent)}
th.sorted-desc::after{content:"↓";opacity:1;color:var(--accent)}
.pos{color:var(--pos)}.neg{color:var(--neg)}
td .bar-track{display:block;height:6px;border-radius:3px;background:var(--surface-2);overflow:hidden;min-width:90px}
td.title-cell{max-width:340px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.export-btn{
  font-family:var(--mono);font-size:10px;color:var(--muted);
  background:none;border:1px solid var(--line);border-radius:6px;
  padding:2px 9px;cursor:pointer;
}
.export-btn:hover{color:var(--accent);border-color:var(--accent)}
.export-btn:disabled{opacity:.35;cursor:not-allowed}
.sum-right{display:inline-flex;align-items:center;gap:10px}
details.data-block{background:var(--surface);border:1px solid var(--line);border-radius:10px;margin-bottom:10px;overflow:hidden}
details.data-block summary{font-size:12px;font-weight:600;cursor:pointer;padding:11px 16px;list-style:none;display:flex;justify-content:space-between;align-items:center}
details.data-block summary:hover{background:var(--surface-2)}
details.data-block summary::-webkit-details-marker{display:none}
details.data-block summary::after{content:"+";color:var(--faint);font-weight:400}
details.data-block[open] summary::after{content:"–"}
details.data-block .tbl-wrap{padding:0 16px 12px;overflow-x:auto}
.empty{font-size:11.5px;color:var(--faint);text-align:center;padding:18px 0}
footer{margin:52px 0 34px;text-align:center;font-size:10.5px;color:var(--faint)}
</style>
</head>
<body>
<header class="topbar">
  <div class="wrap bar-inner">
    <span class="brand">token-usage · report</span>
    <nav class="sec-nav">
      <a href="#overview">Overview</a><a href="#heatmap">Heatmap</a><a href="#trends">Trends</a><a href="#share">Share</a><a href="#sessions">Sessions</a><a href="#data">Data</a>
    </nav>
  </div>
</header>
<main class="wrap">

<section id="overview">
  <h2>Overview <span class="zh">/ 总览</span></h2>
  <p class="meta">Range <b>{{.RangeText}}</b> · Data through <b>{{.DataThrough}}</b> · Last successful collection <b>{{.LastCollection}}</b></p>
  <div class="kpis">
  {{range .KPIs}}<div class="kpi"><div class="label">{{.Label}} <span class="zh">{{.LabelZh}}</span></div><div class="value"{{if .ElemID}} id="{{.ElemID}}"{{end}}>{{.Value}}</div>{{if .Sub}}<div class="sub">{{.Sub}}</div>{{end}}</div>
  {{end}}</div>
  <div class="panel" style="margin-top:16px">
    <p class="meta">Current / 当前: <b>{{.CurStart}} .. {{.CurEnd}}</b> · Base / 基线: <b>{{.BaseStart}} .. {{.BaseEnd}}</b></p>
    <table>
      <thead><tr>
        <th>Metric / 指标</th><th class="num">Current / 当前</th><th class="num">Base / 基线</th><th class="num">Change / 变化</th><th class="num">Change % / 变化%</th>
      </tr></thead>
      <tbody id="compare-body">
      {{range .CompareRows}}<tr><td>{{.Metric}}</td><td class="num">{{.Current}}</td><td class="num">{{.Base}}</td><td class="{{.ChangeClass}}">{{.Change}}</td><td class="num">{{.ChangePct}}</td></tr>
      {{end}}</tbody>
    </table>
  </div>
</section>

<section id="heatmap">
  <h2>Heatmap <span class="zh">/ 热力图</span></h2>
  <div class="chart">{{.HeatmapSVG}}</div>
</section>

<section id="trends">
  <h2>Trends <span class="zh">/ 时间趋势</span></h2>
  <div class="grid">
  {{range .TrendSVGs}}<div class="chart">{{.}}</div>
  {{end}}</div>
</section>

<section id="share">
  <h2>Share <span class="zh">/ 构成占比</span></h2>
  <div class="grid">
  {{range .ShareSVGs}}<div class="chart">{{.}}</div>
  {{end}}</div>
</section>

<section id="sessions">
  <h2>Top sessions <span class="zh">/ 最重会话</span></h2>
  <div class="panel">
    <table>
      <thead><tr>
        <th class="sortable num">#</th><th class="sortable">Title / 标题</th><th class="sortable">Client / 客户端</th><th class="sortable">Project / 项目</th><th class="sortable num">Duration / 时长</th><th class="sortable num">Requests / 请求数</th><th class="sortable num">Total / 总计</th>
      </tr></thead>
      <tbody id="sessions-body">
      {{if .TopRows}}{{range .TopRows}}<tr><td class="num" data-v="{{.Rank}}">{{.Rank}}</td><td class="title-cell" title="{{.Title}}">{{.Title}}</td><td>{{.Client}}</td><td>{{.Project}}</td><td class="num" data-v="{{.DurationMS}}">{{.Duration}}</td><td class="num" data-v="{{.Requests}}">{{.RequestsText}}</td><td class="num" data-v="{{.TotalRaw}}">{{.Total}}</td></tr>
      {{end}}{{else}}<tr><td class="empty" colspan="7">no data / 无数据</td></tr>
      {{end}}</tbody>
    </table>
  </div>
</section>

<section id="data">
  <h2>Data tables <span class="zh">/ 数据明细</span></h2>
  {{range .DataBlocks}}<details class="data-block">
    <summary><span>{{.Name}} <span class="zh">{{.NameZh}}</span></span><span class="sum-right"><button type="button" class="export-btn" data-dim="{{.Name}}" data-range="{{$.RangeText}}"{{if eq .Count 0}} disabled{{end}}>Export CSV</button><span class="muted">{{.Count}} rows</span></span></summary>
    <div class="tbl-wrap">
      <table>
        <thead><tr><th class="sortable">Key</th>{{range .Headers}}<th class="sortable num">{{.}}</th>{{end}}</tr></thead>
        <tbody>
        {{if .Rows}}{{range .Rows}}<tr><td>{{.Key}}</td>{{range .Cells}}<td class="num" data-v="{{.Raw}}">{{.Text}}</td>{{end}}</tr>
        {{end}}{{else}}<tr><td class="empty" colspan="8">no data / 无数据</td></tr>
        {{end}}</tbody>
      </table>
    </div>
  </details>
  {{end}}</section>

</main>
<footer>token-usage report · self-contained offline bundle / 自包含离线报告页</footer>
<script>
(function(){
  'use strict';
  // 锚点导航随滚动高亮:以顶栏下方判定线取当前章节。轮询而非监听 scroll:
  // 嵌 WebView/自动化环境下 scroll 事件可能不派发、rAF 可能被节流,而
  // 400ms 一次的 6 次几何读取开销可忽略且在任何环境都能工作。
  var navLinks = Array.prototype.slice.call(document.querySelectorAll('.sec-nav a'));
  var navIds = navLinks.map(function(a){ return a.getAttribute('href').slice(1); });
  function navUpdate(){
    var current = navIds[0];
    navIds.forEach(function(id){
      var sec = document.getElementById(id);
      if (sec && sec.getBoundingClientRect().top <= 90) current = id;
    });
    navLinks.forEach(function(a){
      a.classList.toggle('active', a.getAttribute('href').slice(1) === current);
    });
  }
  navUpdate();
  setInterval(navUpdate, 400);
  document.querySelectorAll('th.sortable').forEach(function(th){
    th.addEventListener('click', function(){
      var table = th.closest('table');
      var tbody = table.querySelector('tbody');
      var rows = Array.prototype.slice.call(tbody.querySelectorAll('tr'));
      if (rows.length <= 1) return;
      var idx = Array.prototype.indexOf.call(th.parentNode.children, th);
      var desc = !th.classList.contains('sorted-desc');
      table.querySelectorAll('th.sortable').forEach(function(h){
        h.classList.remove('sorted-asc', 'sorted-desc');
      });
      th.classList.add(desc ? 'sorted-desc' : 'sorted-asc');
      rows.sort(function(a, b){
        var ac = a.children[idx], bc = b.children[idx];
        var an = parseFloat(ac.dataset.v), bn = parseFloat(bc.dataset.v);
        var cmp;
        if (!isNaN(an) && !isNaN(bn)) {
          cmp = an === bn ? 0 : (an < bn ? -1 : 1);
        } else {
          var at = ac.textContent.trim(), bt = bc.textContent.trim();
          cmp = at < bt ? -1 : (at > bt ? 1 : 0);
        }
        return desc ? -cmp : cmp;
      });
      rows.forEach(function(row){ tbody.appendChild(row); });
    });
  });

  // 数据明细一键导出 CSV:按当前排序所见行,键列取文本、数值列取 data-v
  // 精确整数(与 serve 仪表板同一导出口径);RFC 4180 转义,CRLF + UTF-8 BOM。
  function csvField(v){
    var s = String(v);
    return /[",\r\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  }
  document.querySelectorAll('.export-btn').forEach(function(btn){
    btn.addEventListener('click', function(e){
      e.preventDefault();
      e.stopPropagation();
      var block = btn.closest('.data-block');
      if (!block) return;
      var tbody = block.querySelector('tbody');
      if (!tbody || tbody.querySelector('td.empty')) return;
      var table = block.querySelector('table');
      var headers = Array.prototype.map.call(table.querySelectorAll('thead th'), function(th){
        return th.textContent.trim();
      });
      var lines = [headers.map(csvField).join(',')];
      Array.prototype.forEach.call(tbody.rows, function(tr){
        var fields = [];
        Array.prototype.forEach.call(tr.cells, function(td, i){
          fields.push(csvField(i === 0 ? td.textContent.trim() : (td.dataset.v || '0')));
        });
        lines.push(fields.join(','));
      });
      var dim = (btn.dataset.dim || 'view').toLowerCase().replace(/[^a-z0-9]+/gi, '-');
      var range = (btn.dataset.range || '').replace(/[^a-z0-9]+/gi, '-').replace(/^-+|-+$/g, '');
      var blob = new Blob(['\uFEFF' + lines.join('\r\n') + '\r\n'], { type: 'text/csv;charset=utf-8' });
      var url = URL.createObjectURL(blob);
      var a = document.createElement('a');
      a.href = url;
      a.download = 'token-usage-' + dim + '-' + range + '.csv';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(function(){ URL.revokeObjectURL(url); }, 1000);
    });
  });
})();
</script>
</body>
</html>`

// buildReportHTML 渲染报告包的自包含交互式 HTML 报告页(index.html):
// KPI 总览、两期对比、内嵌报告包全部 9 份 SVG、Top sessions、逐维度数据表
// 与锚点导航,全部数据来自同一读快照的输入,纯内存渲染。动态文本经
// html/template 自动转义(维度键/会话标题等自由文本中的 HTML 不生效),
// 仅内嵌 SVG 经 inlineSVG 以 template.HTML 原样输出。
func buildReportHTML(in reportHTMLInput) (string, error) {
	data := reportHTMLData{
		Title:     "token-usage report · " + in.rangeText,
		RangeText: in.rangeText,
		CurStart:  in.curStart,
		CurEnd:    in.curEnd,
		BaseStart: in.baseStart,
		BaseEnd:   in.baseEnd,
	}

	// 头部 meta 与 summary.txt 同源:数据截至/最近成功采集,缺数据为 em dash。
	data.DataThrough = emDash
	if in.fresh.MaxMessageTS > 0 {
		data.DataThrough = time.UnixMilli(in.fresh.MaxMessageTS).Local().Format(time.DateTime)
	}
	data.LastCollection = emDash
	if !in.fresh.LastCollection.IsZero() {
		data.LastCollection = in.fresh.LastCollection.Format(time.DateTime)
	}

	// KPI 卡:主值用 K/M/B 缩写口径,Total 附千分位精确值;日均按活跃天整数
	// 除法(与 forecast/summary 同口径);单日峰值扫按日行严格大于取首个,
	// 无正峰值(空库仅剩零值填充行)时为 em dash;与 summary 的边界差异:
	// activeDays>0 但全库 total 全 0 的极端场景下 summary 仍打印峰值(0),
	// 本页以「无正值即无峰值」为准,语义更直白。
	peakValue, peakSub := emDash, ""
	if peak, key := peakDayOf(in.dayRows); peak > 0 {
		peakValue = querier.FormatTokens(peak)
		peakSub = key
	}
	data.KPIs = []reportKPI{
		{Label: "Total tokens", LabelZh: "总 token", ElemID: "kpi-total-value", Value: querier.FormatTokens(in.cur.Total.TotalTokens), Sub: fmtx.Thousands(in.cur.Total.TotalTokens)},
		{Label: "Requests", LabelZh: "请求数", Value: fmtx.Thousands(in.cur.Total.Requests)},
		{Label: "Active days", LabelZh: "活跃天数", Value: fmtx.Thousands(in.cur.ActiveDays)},
		{Label: "Avg per active day", LabelZh: "活跃日均", Value: avgPerActiveDay(in.cur), Sub: "by active day / 按活跃天"},
		{Label: "Peak day", LabelZh: "单日峰值", Value: peakValue, Sub: peakSub},
	}

	// 两期对比表镜像 renderCompare 的 8 行:计数行千分位、token 行缩写,
	// 差值按符号着色,百分比与表格/JSON 同口径(base=0 为 "--")。
	appendCount := func(label string, curV, baseV int64) {
		data.CompareRows = append(data.CompareRows, reportCompareRow{
			Metric:      label,
			Current:     fmtx.Thousands(curV),
			Base:        fmtx.Thousands(baseV),
			Change:      fmtx.CountChange(curV - baseV),
			ChangeClass: fmtx.ChangeClass(curV - baseV),
			ChangePct:   fmtx.ChangePercent(curV, baseV),
		})
	}
	appendToken := func(label string, curV, baseV int64) {
		data.CompareRows = append(data.CompareRows, reportCompareRow{
			Metric:      label,
			Current:     querier.FormatTokens(curV),
			Base:        querier.FormatTokens(baseV),
			Change:      fmtx.SignedTokens(curV - baseV),
			ChangeClass: fmtx.ChangeClass(curV - baseV),
			ChangePct:   fmtx.ChangePercent(curV, baseV),
		})
	}
	appendCount(ui.Bi("Active days", "活跃天"), in.cur.ActiveDays, in.base.ActiveDays)
	appendCount(ui.ColRequests, in.cur.Total.Requests, in.base.Total.Requests)
	appendToken(ui.ColInput, in.cur.Total.FreshInput, in.base.Total.FreshInput)
	appendToken(ui.ColOutput, in.cur.Total.OutputTokens, in.base.Total.OutputTokens)
	appendToken(ui.ColCacheRead, in.cur.Total.CacheRead, in.base.Total.CacheRead)
	appendToken(ui.ColCacheCreate, in.cur.Total.CacheCreate, in.base.Total.CacheCreate)
	appendToken(ui.ColReasoning, in.cur.Total.Reasoning, in.base.Total.Reasoning)
	appendToken(ui.ColTotal, in.cur.Total.TotalTokens, in.base.Total.TotalTokens)

	// 内嵌 SVG:热力 1 份 + 柱状 4 份 + 饼图 4 份,与独立 .svg 文件同源。
	data.HeatmapSVG = inlineSVG(in.svgs["heatmap.svg"])
	for _, name := range []string{"daily.svg", "hourly.svg", "weekday.svg", "monthly.svg"} {
		data.TrendSVGs = append(data.TrendSVGs, inlineSVG(in.svgs[name]))
	}
	for _, name := range []string{"by-client.svg", "by-model.svg", "by-provider.svg", "by-project.svg"} {
		data.ShareSVGs = append(data.ShareSVGs, inlineSVG(in.svgs[name]))
	}

	// Top sessions:入参已是 SortTopRows+截断后的排行行,行序即展示序。
	for i, r := range in.topRows {
		project := r.Project
		if project == "" {
			project = ui.Bi("(uncategorized)", "(未分类)")
		}
		data.TopRows = append(data.TopRows, reportTopRow{
			Rank:         i + 1,
			Title:        r.Title,
			Client:       r.Client,
			Project:      project,
			DurationMS:   r.LastTS - r.FirstTS,
			Duration:     querier.FormatDuration(r.LastTS - r.FirstTS),
			Requests:     r.Agg.Requests,
			RequestsText: fmtx.Thousands(r.Agg.Requests),
			Total:        querier.FormatTokens(r.Agg.TotalTokens),
			TotalRaw:     r.Agg.TotalTokens,
		})
	}

	// 数据明细:8 个维度各一个折叠块,行序保持聚合核返回序,数值单元格
	// 同时携带千分位显示与精确整数(供排序)。
	metricLabels := []string{ui.ColRequests, ui.ColInput, ui.ColOutput, ui.ColCacheRead, ui.ColCacheCreate, ui.ColReasoning, ui.ColTotal}
	for _, def := range []struct{ key, name, zh string }{
		{"day", "Day", "按天"},
		{"hour", "Hour", "按小时"},
		{"weekday", "Weekday", "按星期"},
		{"month", "Month", "按月"},
		{"client", "Client", "客户端"},
		{"model", "Model", "模型"},
		{"provider", "Provider", "供应商"},
		{"project", "Project", "项目"},
	} {
		chart := in.dimCharts[def.key]
		block := reportDataBlock{Name: def.name, NameZh: def.zh, Count: len(chart.rows), Headers: metricLabels}
		for _, row := range chart.rows {
			if len(row.Keys) == 0 {
				continue
			}
			values := []int64{
				row.Agg.Requests, row.Agg.FreshInput, row.Agg.OutputTokens,
				row.Agg.CacheRead, row.Agg.CacheCreate, row.Agg.Reasoning, row.Agg.TotalTokens,
			}
			cells := make([]reportDataCell, len(values))
			for i, v := range values {
				cells[i] = reportDataCell{Text: fmtx.Thousands(v), Raw: v}
			}
			block.Rows = append(block.Rows, reportDataRow{Key: row.Keys[0], Cells: cells})
		}
		data.DataBlocks = append(data.DataBlocks, block)
	}

	tpl, err := template.New("report").Parse(reportHTMLTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// peakDayOf 扫描按日维度行求单日 TotalTokens 峰值:严格大于保证并列时取
// 先出现行(与 summary 的单日峰值同口径);无正峰值(空库仅剩零值填充行)
// 时返回 peak=0,展示层据此渲染 em dash。
func peakDayOf(rows []querier.DimensionRow) (int64, string) {
	var peak int64
	var key string
	for _, row := range rows {
		if len(row.Keys) == 0 {
			continue
		}
		if row.Agg.TotalTokens > peak {
			peak = row.Agg.TotalTokens
			key = row.Keys[0]
		}
	}
	return peak, key
}

// avgPerActiveDay 渲染活跃日均总量:TotalTokens/ActiveDays 整数除法,与
// forecast 的日均口径一致;无活跃天时为 em dash。
func avgPerActiveDay(s querier.RangeStats) string {
	if s.ActiveDays <= 0 {
		return emDash
	}
	return querier.FormatTokens(s.Total.TotalTokens / s.ActiveDays)
}

// inlineSVG 把 charts 包生成的 SVG 文本转为可内嵌 HTML 的标记:定位首个
// "<svg" 截掉 <?xml ...?> 序言(序言在内嵌 HTML 中非法);SVG 文本由
// charts 侧自行转义,经 template.HTML 原样输出。
func inlineSVG(s string) template.HTML {
	if i := strings.Index(s, "<svg"); i >= 0 {
		s = s[i:]
	}
	return template.HTML(s)
}
