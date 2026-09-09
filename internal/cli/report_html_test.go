package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

// reportHTMLFixture 建立报告页测试夹具:两天、两个 project(其一为恶意
// HTML 串)、两个不同 total 的会话(s1=300,s2=2x350=700)。时间戳用
// time.Local,与聚合核的时区归属口径一致。
func reportHTMLFixture(t *testing.T) *querier.Querier {
	t.Helper()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usageDB.Close() })

	day1 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.Local)
	messages := []model.Message{
		{ID: "r-1", SessionID: "s1", Client: model.ClientClaudeCode,
			Date: day1.Format("2006-01-02"), TS: day1.UnixMilli(),
			Model: "model-x", Project: "<img src=x onerror=alert(1)>", TotalTokens: 300},
		{ID: "r-2", SessionID: "s2", Client: model.ClientClaudeCode,
			Date: day2.Format("2006-01-02"), TS: day2.UnixMilli(),
			Model: "model-x", Project: "proj-beta", TotalTokens: 350},
		{ID: "r-3", SessionID: "s2", Client: model.ClientClaudeCode,
			Date: day2.Format("2006-01-02"), TS: day2.UnixMilli() + 1000,
			Model: "model-x", Project: "proj-beta", TotalTokens: 350},
	}
	if _, err := db.UpsertMessages(context.Background(), usageDB, messages); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientClaudeCode, Project: "<img src=x onerror=alert(1)>",
			Title: "session-one", FirstTS: day1.UnixMilli(), LastTS: day1.UnixMilli() + 65_000},
		{ID: "s2", Client: model.ClientClaudeCode, Project: "proj-beta",
			Title: "session-two", FirstTS: day2.UnixMilli(), LastTS: day2.UnixMilli() + 3_600_000},
	}); err != nil {
		t.Fatal(err)
	}
	return querier.New(usageDB)
}

// reportHTMLOf 从报告包文件列表中取出 index.html 的渲染产物。
func reportHTMLOf(t *testing.T, files []reportFile) string {
	t.Helper()
	found := false
	var html string
	for _, f := range files {
		if f.name != "index.html" {
			continue
		}
		found = true
		var err error
		html, err = f.render()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatalf("报告包应含 index.html,实际 %d 个文件", len(files))
	}
	return html
}

// htmlPlus 把 "+" 归一为 html/template 的实体转义形态(&#43;),使断言与
// 渲染产物对齐(浏览器中显示仍为 "+")。
func htmlPlus(s string) string {
	return strings.ReplaceAll(s, "+", "&#43;")
}

// reportIndexHTML 报告包第 12 个文件 index.html:自包含交互式双语报告页。
// 覆盖文件清单、结构约束(6 个区段锚点/恰 9 份内嵌 SVG/文档完整结尾)、
// KPI 口径、自由文本转义、两期对比镜像、会话排行与零外部资源。
func TestReportFiles_IndexHTML(t *testing.T) {
	q := reportHTMLFixture(t)
	dates := []string{"2026-09-02", "2026-09-03"}
	files, err := reportFiles(context.Background(), q, dates, "2026-09-02 ~ 2026-09-03", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 12 {
		t.Fatalf("报告包应含 12 个文件,实际 %d 个", len(files))
	}
	html := reportHTMLOf(t, files)

	// 结构:6 个区段锚点、恰 9 份内嵌 SVG(热力 1 + 柱状 4 + 饼图 4)、
	// 文档以 </html> 结尾。
	for _, id := range []string{"overview", "heatmap", "trends", "share", "sessions", "data"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 应含区段锚点 id=%q", id)
		}
	}
	if n := strings.Count(html, "<svg"); n != 9 {
		t.Errorf("index.html 应恰内嵌 9 份 SVG,实际 %d", n)
	}
	if !strings.HasSuffix(html, "</html>") {
		t.Errorf("index.html 应以 </html> 结尾,实际结尾: %q", html[len(html)-40:])
	}

	// KPI(overview 区):总量千分位精确值、峰值日日期与 FormatTokens 主值。
	start := strings.Index(html, `id="overview"`)
	end := strings.Index(html, `id="heatmap"`)
	if start < 0 || end <= start {
		t.Fatalf("index.html 区段锚点顺序异常: overview=%d heatmap=%d", start, end)
	}
	overview := html[start:end]
	// 区间 [09-02, 09-03] 总量 1000 → 精确 "1,000";峰值 2026-09-03 700。
	for _, want := range []string{"1,000", "700", "2026-09-03"} {
		if !strings.Contains(overview, want) {
			t.Errorf("overview 应含 KPI 值 %q:\n%s", want, overview)
		}
	}

	// 转义:恶意 project 串必须以实体形态出现,裸 HTML 标签不得出现。
	if !strings.Contains(html, "&lt;img src=x") {
		t.Errorf("index.html 应含转义后的恶意串 &lt;img src=x")
	}
	if strings.Contains(html, "<img src=x") {
		t.Errorf("index.html 不得含未转义的 <img src=x")
	}

	// 两期对比:恰 8 个数据行;缺省基线(2026-08-31..2026-09-01)无数据,
	// 差值=当前值(如 Active days +2),百分比恒 "--"。
	cmpStart := strings.Index(html, `id="compare-body"`)
	cmpEnd := strings.Index(html[cmpStart:], "</tbody>")
	if cmpStart < 0 || cmpEnd < 0 {
		t.Fatal("index.html 应含 compare-body 表体")
	}
	compareSeg := html[cmpStart : cmpStart+cmpEnd]
	if n := strings.Count(compareSeg, "<tr>"); n != 8 {
		t.Errorf("compare 表应有 8 个数据行,实际 %d:\n%s", n, compareSeg)
	}
	for _, want := range []string{"+2", "+1.00 K", "--"} {
		if !strings.Contains(compareSeg, htmlPlus(want)) {
			t.Errorf("compare 表应含 %q(base=0 差值=当前值):\n%s", want, compareSeg)
		}
	}

	// 会话排行:total 降序,排名 1/2 归属正确;data-v 为精确整数。
	sessStart := strings.Index(html, `id="sessions-body"`)
	sessEnd := strings.Index(html[sessStart:], "</tbody>")
	if sessStart < 0 || sessEnd < 0 {
		t.Fatal("index.html 应含 sessions-body 表体")
	}
	sessSeg := html[sessStart : sessStart+sessEnd]
	iTwo, iOne := strings.Index(sessSeg, "session-two"), strings.Index(sessSeg, "session-one")
	if iTwo < 0 || iOne < 0 {
		t.Fatalf("sessions 表应含两个会话标题:\n%s", sessSeg)
	}
	if iTwo > iOne {
		t.Errorf("sessions 表应 total 降序(session-two 700 在前):\n%s", sessSeg)
	}
	for _, want := range []string{`data-v="700"`, `data-v="300"`, `title="session-two"`} {
		if !strings.Contains(sessSeg, want) {
			t.Errorf("sessions 表应含 %q:\n%s", want, sessSeg)
		}
	}

	// 零外部资源:剔除 SVG 的 xmlns 命名空间声明(非资源引用)后,
	// 不得出现任何 http(s) 外链或外挂脚本。
	scrubbed := strings.ReplaceAll(html, `xmlns="http://www.w3.org/2000/svg"`, "")
	for _, banned := range []string{"http://", "https://", "<script src"} {
		if strings.Contains(scrubbed, banned) {
			t.Errorf("index.html 不得含外部资源引用 %q", banned)
		}
	}
}

// reportIndexHTML_KnownDiff 已知差值:单日 2026-09-03 的缺省基线为 2026-09-02
// (有数据),Requests 2 vs 1 → "+1"、Total 700 vs 300 → "+400"/"+133.3%",
// 验证 change 列与百分比的真实差值口径(区别于 base=0 的退化形态)。
func TestReportFiles_IndexHTML_KnownDiff(t *testing.T) {
	q := reportHTMLFixture(t)
	files, err := reportFiles(context.Background(), q, []string{"2026-09-03"}, "2026-09-03", 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	html := reportHTMLOf(t, files)
	cmpStart := strings.Index(html, `id="compare-body"`)
	cmpEnd := strings.Index(html[cmpStart:], "</tbody>")
	if cmpStart < 0 || cmpEnd < 0 {
		t.Fatal("index.html 应含 compare-body 表体")
	}
	compareSeg := html[cmpStart : cmpStart+cmpEnd]
	for _, want := range []string{"+1", "+400", "+133.3%"} {
		if !strings.Contains(compareSeg, htmlPlus(want)) {
			t.Errorf("compare 表应含已知差值 %q:\n%s", want, compareSeg)
		}
	}
}
