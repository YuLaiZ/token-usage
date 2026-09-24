package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

/* app.js 前端行为回归:Go 侧构造场景 JSON(存储预置 + fetch 路由表 + 交互步骤),
   驱动 node 跑 testdata/app_behavior_runner.js(vm 沙箱 + 最小 DOM 桩加载生产
   app.js)。runner 只负责执行与取证(分步元素状态、fetch 调用、存储快照、
   toast),判定全部在本文件;每个场景独立沙箱与路由表;node 缺席时跳过
   (GitHub 托管的三个 CI 平台 runner 均预装 node)。 */

/*
routeSpec 是 fetch 桩的单条响应;同一方法+前缀的多次不同响应用 []routeSpec 队列;

	Reject=true 表示网络失败(fetch 直接 reject,走调用方 rejection 分支)
*/
type routeSpec struct {
	Status  int  `json:"status"`
	Body    any  `json:"body"`
	Reject  bool `json:"reject,omitempty"`
	Pending bool `json:"pending,omitempty"`
}

/*
stepSpec 描述一次手动交互:nav=点导航进页,click/input=按 app.js 的监听

	形态触发(click 直派,input 设 value 后在 page-config 上派发委托事件)
*/
type stepSpec struct {
	Op    string `json:"op"`
	ID    string `json:"id,omitempty"`
	Page  string `json:"page,omitempty"`
	Value string `json:"value,omitempty"`
}

type scenarioDef struct {
	Name  string         `json:"name"`
	Opts  map[string]any `json:"opts"`
	Steps []stepSpec     `json:"steps,omitempty"`
}

type fetchCall struct {
	URL    string  `json:"url"`
	Method string  `json:"method"`
	Body   *string `json:"body"`
}

/* stepState 是每步交互后的关键 UI 快照,让"中间态"可断言(如脏→改回→复位) */
type stepState struct {
	Op            string `json:"op"`
	PollInput     string `json:"pollInput"`
	SaveDisabled  bool   `json:"saveDisabled"`
	SaveText      string `json:"saveText"`
	DirtyPill     string `json:"dirtyPill"`
	Toast         string `json:"toast"`
	DashSelect    string `json:"dashSelect"`
	WatchSelect   string `json:"watchSelect"`
	WatchCustom   string `json:"watchCustom"`
	RefreshState  string `json:"refreshState"`
	RefreshLabel  string `json:"refreshLabel"`
	RefreshDetail string `json:"refreshDetail"`
	Refreshing    string `json:"pageRefreshing"`
	DashBusy      string `json:"dashBusy"`
}

type appJSReport struct {
	Name       string      `json:"name"`
	Fatal      string      `json:"fatal"`
	Today      string      `json:"today"`
	Title      string      `json:"title"`
	Toast      string      `json:"toast"`
	FetchCalls []fetchCall `json:"fetchCalls"`
	RangeSeg   []struct {
		V       string `json:"v"`
		Pressed string `json:"pressed"`
	} `json:"rangeSeg"`
	RangeCustom struct {
		Pressed string `json:"pressed"`
		On      bool   `json:"on"`
		Text    string `json:"text"`
	} `json:"rangeCustom"`
	ChartSub  string `json:"chartSub"`
	PollInput string `json:"pollInput"`
	CfgSave   struct {
		Disabled bool   `json:"disabled"`
		Text     string `json:"text"`
	} `json:"cfgSave"`
	DirtyPill struct {
		Text string `json:"text"`
	} `json:"dirtyPill"`
	CfgActionsDirty   string `json:"cfgActionsDirty"`
	AsofText          string `json:"asofText"`
	AsofOffline       bool   `json:"asofOffline"`
	DvPopHidden       bool   `json:"dvPopHidden"`
	DvTriggerExpanded string `json:"dvTriggerExpanded"`
	DvCurText         string `json:"dvCurText"`
	ActiveElementID   string `json:"activeElementId"`
	ResetModalHidden  bool   `json:"resetModalHidden"`
	KpisHTML          string `json:"kpisHtml"`
	GroupsHTML        string `json:"groupsHtml"`
	ChartHTML         string `json:"chartHtml"`
	SessionsSub       string `json:"sessionsSub"`
	SessionsBodyHTML  string `json:"sessionsBodyHtml"`
	Locale            struct {
		Saved     *string `json:"saved"`
		ZhPressed string  `json:"zhPressed"`
		EnPressed string  `json:"enPressed"`
	} `json:"locale"`
	Theme struct {
		Saved *string `json:"saved"`
		Root  string  `json:"root"`
	} `json:"theme"`
	Palette struct {
		Saved   *string `json:"saved"`
		Root    string  `json:"root"`
		Name    string  `json:"name"`
		Choices []struct {
			Pal     string `json:"pal"`
			Pressed string `json:"pressed"`
		} `json:"choices"`
	} `json:"palette"`
	Side struct {
		Saved    *string `json:"saved"`
		Min      bool    `json:"min"`
		Expanded string  `json:"expanded"`
	} `json:"side"`
	Chips []struct {
		Text    string `json:"text"`
		Pressed string `json:"pressed"`
	} `json:"chips"`
	Refresh struct {
		State          string `json:"state"`
		Label          string `json:"label"`
		Detail         string `json:"detail"`
		PageRefreshing string `json:"pageRefreshing"`
		DashBusy       string `json:"dashBusy"`
	} `json:"refresh"`
	RefreshCfg struct {
		DashSelect        string `json:"dashSelect"`
		DashCustom        string `json:"dashCustom"`
		DashCustomHidden  bool   `json:"dashCustomHidden"`
		WatchSelect       string `json:"watchSelect"`
		WatchCustom       string `json:"watchCustom"`
		WatchCustomHidden bool   `json:"watchCustomHidden"`
	} `json:"refreshCfg"`
	PendingFetches int               `json:"pendingFetches"`
	Session        map[string]string `json:"session"`
	Local          map[string]string `json:"local"`
	StepStates     []stepState       `json:"stepStates"`
}

func metaBody(today string) map[string]any {
	return map[string]any{"version": "v9.9.9-test", "last_collection": "", "data_through": today}
}

/* metaOnlineBody 带 last_collection:在线态走「在线 · 最近同步 …」文案分支 */
func metaOnlineBody(today string) map[string]any {
	return map[string]any{"version": "v9.9.9-test", "last_collection": today + " 10:00:00", "data_through": today}
}

func dashboardBody() map[string]any {
	return map[string]any{
		"totals":       map[string]any{"requests": 12, "fresh_input": 1000, "output": 500, "cache_read": 2000, "cache_create": 100, "reasoning": 300, "total": 3900},
		"dimensions":   map[string]any{},
		"heatmap":      map[string]any{"days": []any{}},
		"custom_views": []any{},
		"sessions":     []any{},
	}
}

/*
dimRowsFor 构造 count 个独立维度行(total 递增 i*100),超出 9 的部分

	按后端合同收拢为 is_other 尾行(固定最后)。
*/
func dimRowsFor(count int) []any {
	raw := make([]any, 0, count)
	for i := 1; i <= count; i++ {
		raw = append(raw, map[string]any{
			"key": fmt.Sprintf("client-%02d", i), "requests": i,
			"fresh_input": i * 10, "output": i * 10, "cache_read": i * 10,
			"cache_create": 0, "reasoning": 0, "total": i * 100,
		})
	}
	if count <= 9 {
		return raw
	}
	var tail map[string]any
	sumReq, sumTotal := 0, 0
	for i := 10; i <= count; i++ {
		sumReq += i
		sumTotal += i * 100
	}
	tail = map[string]any{
		"key": "", "requests": sumReq, "fresh_input": 0, "output": 0,
		"cache_read": 0, "cache_create": 0, "reasoning": 0,
		"total": sumTotal, "is_other": true, "other_count": count - 9,
	}
	return append(raw[:9:9], tail)
}

/*
richDashboardBody 是载荷 v4 形态的仪表板数据:12 行维度(9+尾行)、

	逐日趋势桶、day_block 热力与前 20 会话。total 标记 marker 供 KPI 断言。
*/
func richDashboardBody(totalMarker int) map[string]any {
	hours := make([]any, 24)
	for i := range hours {
		hours[i] = 0
	}
	hours[10] = 300
	blocks := make([]any, 6)
	for i := range blocks {
		blocks[i] = 0
	}
	blocks[2] = 300
	return map[string]any{
		"totals":  map[string]any{"requests": 33, "fresh_input": 100, "output": 100, "cache_read": 100, "cache_create": 10, "reasoning": 0, "total": totalMarker},
		"columns": []any{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"},
		"dimensions": map[string]any{
			"client":   dimRowsFor(12),
			"provider": dimRowsFor(12),
			"model":    dimRowsFor(12),
			"project":  dimRowsFor(12),
		},
		"trend": map[string]any{
			"granularity": "day",
			"buckets": []any{
				map[string]any{"key": "2026-09-01", "total": 700},
				map[string]any{"key": "2026-09-02", "total": 0},
				map[string]any{"key": "2026-09-03", "total": 300},
			},
			"peak_index": 0, "peak_total": 700, "empty": false,
		},
		"heatmap": map[string]any{
			"from": "2026-09-01", "to": "2026-09-03", "mode": "day_block",
			"days": []any{
				map[string]any{"date": "2026-09-01", "total": 700, "blocks": blocks},
				map[string]any{"date": "2026-09-02", "total": 0, "blocks": blocks},
				map[string]any{"date": "2026-09-03", "total": 300, "blocks": blocks},
			},
		},
		"custom_views": []any{},
		"sessions":     []any{},
	}
}

/*
emptyDashboardBody 全零用量:单日(今天)24 小时全零——序列非空但值全零,

	触发趋势图空态(「该区间暂无用量」)而非柱状渲染
*/
func emptyDashboardBody(today string) map[string]any {
	hours := make([]any, 24)
	for i := range hours {
		hours[i] = 0
	}
	return map[string]any{
		"totals":       map[string]any{"requests": 0, "fresh_input": 0, "output": 0, "cache_read": 0, "cache_create": 0, "reasoning": 0, "total": 0},
		"dimensions":   map[string]any{},
		"heatmap":      map[string]any{"days": []any{map[string]any{"date": today, "total": 0, "hours": hours}}},
		"custom_views": []any{},
		"sessions":     []any{},
	}
}

/*
配置 GET fixture:daemon.poll_interval=30、log.level 为空、一条 claude 客户端、

	一条别名、默认视图 client、规范七列输出
*/
func configGetBody(pollInterval int) map[string]any {
	return configGetBodyWithRefresh(pollInterval, 30, 30)
}

/* configGetBodyWithRefresh 允许场景指定 [refresh] 草稿值(默认 30/30)。 */
func configGetBodyWithRefresh(pollInterval, dashInterval, watchInterval int) map[string]any {
	return map[string]any{
		"revision": "rev-1",
		"config": map[string]any{
			"daemon":  map[string]any{"poll_interval": pollInterval, "autostart": false},
			"log":     map[string]any{"level": "", "dir": "", "max_days": 7},
			"refresh": map[string]any{"dashboard_interval": dashInterval, "watch_interval": watchInterval},
			"clients": []any{
				map[string]any{"name": "claude", "enabled": true, "router": "", "paths": map[string]any{"/tmp/claude-projects": "~/.claude"}},
			},
			"routers":          []any{},
			"provider_aliases": []any{map[string]any{"key": "account:pro", "value": "Provider Pro"}},
			"query": map[string]any{
				"default":        "client",
				"subqueries":     map[string]any{},
				"groups":         map[string]any{},
				"output_columns": []any{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"},
			},
		},
	}
}

/* 保存 200 响应:服务端以响应 config+revision 为新真相 */
func saveOKBody(revision string, pollInterval int, changed bool) map[string]any {
	body := configGetBody(pollInterval)
	body["revision"] = revision
	body["changed"] = changed
	return body
}

/* saveOKBodyWithRefresh 在保存响应中携带指定 refresh 草稿值。 */
func saveOKBodyWithRefresh(revision string, pollInterval, dashInterval, watchInterval int, changed bool) map[string]any {
	body := configGetBodyWithRefresh(pollInterval, dashInterval, watchInterval)
	body["revision"] = revision
	body["changed"] = changed
	return body
}

/*
brokenQueryGetBody 是「问题态 query」场景的 GET fixture:query 为回退草稿
(default=client、空 map、默认七列)并附解析诊断。与 /api/config?defaults=1
返回的全默认编辑模型 deliberately 全同——reset 载入后 diff 为 0,只有
forceQueryRewrite 能让保存仍携带 query、dirty 不清零
*/
func brokenQueryGetBody() map[string]any {
	body := configGetBody(30)
	query := body["config"].(map[string]any)["query"].(map[string]any)
	query["diagnostics"] = []any{"query.groups: parse error flagged for this config"}
	return body
}

func brokenQueryDefaultsBody() map[string]any {
	return map[string]any{"config": configGetBody(30)["config"]}
}

/*
configWithColsBody 在标准已保存配置上替换 output_columns(如只留 total),

	用于验证 KPI 概览不受输出列影响、明细表跟随
*/
func configWithColsBody(cols ...string) map[string]any {
	body := configGetBody(30)
	cfg := body["config"].(map[string]any)
	query := cfg["query"].(map[string]any)
	colsAny := make([]any, len(cols))
	for i, c := range cols {
		colsAny[i] = c
	}
	query["output_columns"] = colsAny
	return body
}

/*
configWithDiagBody 是「问题态 query」fixture:groups 携带已存在的历史定义并
附带解析诊断——用户未编辑 query 段时保存,PUT 不应回传 query(避免回退草稿
静默清掉既有查询定义)
*/
func configWithDiagBody() map[string]any {
	body := configGetBody(30)
	cfg := body["config"].(map[string]any)
	query := cfg["query"].(map[string]any)
	query["groups"] = map[string]any{"legacygrp": "client,day"}
	query["diagnostics"] = []any{"query.groups.legacygrp: member 'day' resolves but group was flagged"}
	return body
}

/*
defaultsBody 是 /api/config?defaults=1 的全默认编辑模型(无 revision 键):

	与已保存 fixture 各处取值不同,保证载入后 diff>0
*/
func defaultsBody() map[string]any {
	return map[string]any{
		"config": map[string]any{
			"daemon":           map[string]any{"poll_interval": 60, "autostart": false},
			"log":              map[string]any{"level": "debug", "dir": "", "max_days": 14},
			"refresh":          map[string]any{"dashboard_interval": 30, "watch_interval": 30},
			"clients":          []any{},
			"routers":          []any{},
			"provider_aliases": []any{},
			"query": map[string]any{
				"default":        "day",
				"subqueries":     map[string]any{},
				"groups":         map[string]any{},
				"output_columns": []any{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"},
			},
		},
	}
}

func countFetch(r appJSReport, method, prefix string) int {
	n := 0
	for _, c := range r.FetchCalls {
		if c.Method == method && strings.HasPrefix(c.URL, prefix) {
			n++
		}
	}
	return n
}

/* fetchIndex 返回首个匹配调用的序号(用于断言请求顺序),未找到返回 -1 */
func fetchIndex(r appJSReport, method, prefix string) int {
	for i, c := range r.FetchCalls {
		if c.Method == method && strings.HasPrefix(c.URL, prefix) {
			return i
		}
	}
	return -1
}

func firstFetch(t *testing.T, r appJSReport, method, prefix string) fetchCall {
	t.Helper()
	for _, c := range r.FetchCalls {
		if c.Method == method && strings.HasPrefix(c.URL, prefix) {
			return c
		}
	}
	t.Fatalf("no %s %s call among %+v", method, prefix, r.FetchCalls)
	return fetchCall{}
}

func dashboardURL(t *testing.T, r appJSReport) string {
	t.Helper()
	return firstFetch(t, r, "GET", "/api/dashboard").URL
}

func putCalls(r appJSReport) []fetchCall {
	out := []fetchCall{}
	for _, c := range r.FetchCalls {
		if c.Method == "PUT" && strings.HasPrefix(c.URL, "/api/config") {
			out = append(out, c)
		}
	}
	return out
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

/*
今日默认区间的一致断言:URL 恰为今天(本地时区,Go 动态计算)、

	七个预设里只有 data-v=0 选中、自定义入口不亮、meta 恰好拉一次
*/
func assertTodayDefaults(t *testing.T, r appJSReport, today string) {
	t.Helper()
	if r.Today != today {
		t.Errorf("sandbox today = %q, want Go-computed local %q", r.Today, today)
	}
	want := "/api/dashboard?from=" + today + "&to=" + today
	if got := dashboardURL(t, r); got != want {
		t.Errorf("dashboard URL = %q, want %q", got, want)
	}
	for _, b := range r.RangeSeg {
		wantPressed := "false"
		if b.V == "0" {
			wantPressed = "true"
		}
		if b.Pressed != wantPressed {
			t.Errorf("preset v=%s pressed=%q, want %q", b.V, b.Pressed, wantPressed)
		}
	}
	if r.RangeCustom.Pressed != "false" || r.RangeCustom.On {
		t.Errorf("range-custom pressed=%q on=%v, want unselected for preset Today", r.RangeCustom.Pressed, r.RangeCustom.On)
	}
	if n := countFetch(r, "GET", "/api/meta"); n != 1 {
		t.Errorf("meta fetches = %d, want 1", n)
	}
}

/* putTopKeys 解析 PUT body 顶层键集,配合键数断言可发现混入的多余键(如 diagnostics) */
func putTopKeys(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("decode PUT body %q: %v", body, err)
	}
	return top
}

func TestAppJSBehaviorScenarios(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH; skipping app.js behavior scenarios")
	}
	appSource, err := staticRoot.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	runnerPath, err := filepath.Abs(filepath.Join("testdata", "app_behavior_runner.js"))
	if err != nil {
		t.Fatalf("resolve runner path: %v", err)
	}

	/* 期望值按本地时区动态计算,不写死日期 */
	today := time.Now().Format("2006-01-02")

	baseRoutes := func() map[string]any {
		/* init 链(fetchMeta→fetchConfigState→loadDashboard)会先 GET /api/config
		   预载输出列:每个场景都必须提供 config stub,否则请求 500 虽被前端容错,
		   但与生产行为不符 */
		return map[string]any{
			"GET /api/meta":      routeSpec{Status: 200, Body: metaBody(today)},
			"GET /api/dashboard": routeSpec{Status: 200, Body: dashboardBody()},
			"GET /api/config":    routeSpec{Status: 200, Body: configGetBody(30)},
		}
	}
	dashErrorRoutes := baseRoutes()
	dashErrorRoutes["GET /api/dashboard"] = routeSpec{Status: 400, Body: map[string]any{"error": map[string]any{"message": "Range / 区间"}}}
	cfgRoutes := baseRoutes()
	cfgRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBody(30)}
	saveRoutes := baseRoutes()
	saveRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBody(30)}
	/* 两次保存的响应队列:第一次回写 newrev,第二次回写 newrev2 */
	saveRoutes["PUT /api/config"] = []routeSpec{
		{Status: 200, Body: saveOKBody("newrev", 42, true)},
		{Status: 200, Body: saveOKBody("newrev2", 55, false)},
	}
	conflictRoutes := baseRoutes()
	conflictRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBody(30)}
	conflictRoutes["PUT /api/config"] = routeSpec{Status: 409, Body: map[string]any{"error": map[string]any{"message": "conflict"}, "revision": "other"}}
	save400Routes := baseRoutes()
	save400Routes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBody(30)}
	save400Routes["PUT /api/config"] = routeSpec{Status: 400, Body: map[string]any{"error": map[string]any{"message": "invalid level / 无效级别"}}}
	resetRoutes := baseRoutes()
	resetRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBody(30)}
	resetRoutes["GET /api/config?defaults=1"] = routeSpec{Status: 200, Body: defaultsBody()}
	colsRoutes := baseRoutes()
	colsRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configWithColsBody("total")}
	diagRoutes := baseRoutes()
	diagRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configWithDiagBody()}
	diagRoutes["PUT /api/config"] = routeSpec{Status: 200, Body: saveOKBody("newrev", 30, true)}
	emptyRoutes := baseRoutes()
	emptyRoutes["GET /api/dashboard"] = routeSpec{Status: 200, Body: emptyDashboardBody(today)}
	resetBrokenRoutes := baseRoutes()
	resetBrokenRoutes["GET /api/config"] = routeSpec{Status: 200, Body: brokenQueryGetBody()}
	resetBrokenRoutes["GET /api/config?defaults=1"] = routeSpec{Status: 200, Body: brokenQueryDefaultsBody()}
	/* 两次保存队列:第一次携带 query 修复问题态;第二次验证 force 已清除 */
	resetBrokenRoutes["PUT /api/config"] = []routeSpec{
		{Status: 200, Body: saveOKBody("newrev", 30, true)},
		{Status: 200, Body: saveOKBody("newrev2", 30, false)},
	}
	offlineRoutes := baseRoutes()
	offlineRoutes["GET /api/meta"] = routeSpec{Reject: true}
	onlineRoutes := baseRoutes()
	onlineRoutes["GET /api/meta"] = routeSpec{Status: 200, Body: metaOnlineBody(today)}
	/* 统一刷新状态机:两次 dashboard 响应都 gate(先发后答),允许场景在
	   请求重叠点断言中间态(loading/旧结果标识/过期响应不回写)。 */
	manualRefreshRoutes := baseRoutes()
	manualRefreshRoutes["GET /api/dashboard"] = []routeSpec{
		{Status: 200, Body: richDashboardBody(1111), Pending: true},
		{Status: 200, Body: richDashboardBody(2222), Pending: true},
	}
	staleRoutes := baseRoutes()
	staleRoutes["GET /api/dashboard"] = []routeSpec{
		{Status: 200, Body: richDashboardBody(1111), Pending: true},
		{Status: 200, Body: richDashboardBody(2222), Pending: true},
	}
	failureAfterSuccessRoutes := baseRoutes()
	failureAfterSuccessRoutes["GET /api/dashboard"] = []routeSpec{
		{Status: 200, Body: richDashboardBody(1111)},
		{Status: 400, Body: map[string]any{"error": map[string]any{"message": "boom / 爆炸"}}},
	}
	autoRefreshRoutes := baseRoutes()
	autoRefreshRoutes["GET /api/dashboard"] = []routeSpec{
		{Status: 200, Body: richDashboardBody(1111)},
		{Status: 200, Body: richDashboardBody(2222), Pending: true},
	}
	composeRoutes := baseRoutes()
	composeRoutes["GET /api/dashboard"] = routeSpec{Status: 200, Body: richDashboardBody(1111)}
	refreshCfgRoutes := baseRoutes()
	refreshCfgRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBodyWithRefresh(30, 20, 90)}
	refreshCfgRoutes["PUT /api/config"] = routeSpec{Status: 200, Body: saveOKBodyWithRefresh("newrev", 30, 20, 60, true)}
	/* 旧配置无 [refresh] 段(GET 原值 0/0)+无关保存:PUT 必须保持 0/0,
	   不得把隐式默认固化成显式 30/30;下拉按有效值显示 30 预设。 */
	oldConfigNoRefreshRoutes := baseRoutes()
	oldConfigNoRefreshRoutes["GET /api/config"] = routeSpec{Status: 200, Body: configGetBodyWithRefresh(30, 0, 0)}
	oldConfigNoRefreshRoutes["PUT /api/config"] = routeSpec{Status: 200, Body: saveOKBodyWithRefresh("newrev", 30, 0, 0, true)}

	scenarios := []scenarioDef{
		{
			Name: "reload restores saved custom range",
			Opts: map[string]any{"navType": "reload", "savedRange": map[string]string{"s": "2026-08-01", "e": "2026-08-15"}, "routes": baseRoutes()},
		},
		{
			Name: "navigate defaults to today",
			Opts: map[string]any{"navType": "navigate", "routes": baseRoutes()},
		},
		{
			Name: "performance absent degrades to today",
			Opts: map[string]any{"routes": baseRoutes()},
		},
		{
			Name: "empty navigation entries degrade to today",
			Opts: map[string]any{"navEntries": []any{}, "routes": baseRoutes()},
		},
		{
			Name: "future custom range submitted as is",
			Opts: map[string]any{"savedRange": map[string]string{"s": "2027-01-01", "e": "2027-01-31"}, "routes": baseRoutes()},
		},
		{
			Name: "dashboard 400 error message surfaces",
			Opts: map[string]any{"routes": dashErrorRoutes},
		},
		{
			Name: "config dirty state transitions",
			Opts: map[string]any{"routes": cfgRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-poll-interval", Value: "42"},
				{Op: "input", ID: "cfg-poll-interval", Value: "30"},
			},
		},
		{
			Name: "save 200 round trip uses get revision",
			Opts: map[string]any{"routes": saveRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-poll-interval", Value: "42"},
				{Op: "click", ID: "cfg-save"},
				{Op: "input", ID: "cfg-poll-interval", Value: "55"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "save 409 keeps draft without retry",
			Opts: map[string]any{"routes": conflictRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-poll-interval", Value: "42"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "locale switch persists without dirtying config",
			Opts: map[string]any{"routes": cfgRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-poll-interval", Value: "42"},
				{Op: "locale", Value: "en"},
			},
		},
		{
			Name: "appearance toggles persist without touching config",
			Opts: map[string]any{"routes": baseRoutes()},
			Steps: []stepSpec{
				{Op: "theme"},
				{Op: "palette", Value: "azure"},
				{Op: "side"},
			},
		},
		{
			Name: "save 400 keeps draft without retry",
			Opts: map[string]any{"routes": save400Routes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-poll-interval", Value: "42"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "reset all loads server defaults without saving",
			Opts: map[string]any{"routes": resetRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "click", ID: "cfg-reset"},
				{Op: "click", ID: "reset-confirm"},
			},
		},
		{
			Name: "output columns do not touch kpi rail",
			Opts: map[string]any{"routes": colsRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "locale", Value: "en"},
			},
		},
		{
			Name: "default view listbox escape returns focus",
			Opts: map[string]any{"routes": cfgRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "click", ID: "q-default"},
				{Op: "key", ID: "q-default", Value: "Escape"},
			},
		},
		{
			Name: "meta network failure shows offline",
			Opts: map[string]any{"routes": offlineRoutes},
		},
		{
			Name: "meta success shows online",
			Opts: map[string]any{"routes": onlineRoutes},
		},
		{
			Name: "output columns apply on first dashboard load",
			Opts: map[string]any{"routes": colsRoutes},
		},
		{
			Name: "unchanged query omitted from save",
			Opts: map[string]any{"routes": diagRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-max-days", Value: "21"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "empty range shows empty state",
			Opts: map[string]any{"routes": emptyRoutes},
		},
		{
			Name: "reset all with broken query forces rewrite",
			Opts: map[string]any{"routes": resetBrokenRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "click", ID: "cfg-reset"},
				{Op: "click", ID: "reset-confirm"},
				{Op: "click", ID: "cfg-save"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "unified manual refresh shares loading state",
			Opts: map[string]any{"routes": manualRefreshRoutes},
			Steps: []stepSpec{
				{Op: "resolve"},
				{Op: "click", ID: "dash-refresh"},
				{Op: "resolve"},
			},
		},
		{
			Name: "stale range response does not overwrite",
			Opts: map[string]any{"routes": staleRoutes},
			Steps: []stepSpec{
				{Op: "resolve"},
				{Op: "seg", ID: "range-seg", Value: "4"},
				{Op: "resolve"},
			},
		},
		{
			Name: "failure after success clears stale data",
			Opts: map[string]any{"routes": failureAfterSuccessRoutes},
			Steps: []stepSpec{
				{Op: "click", ID: "dash-refresh"},
			},
		},
		{
			Name: "auto refresh fires after interval",
			Opts: map[string]any{"routes": autoRefreshRoutes, "timers": "manual"},
			Steps: []stepSpec{
				{Op: "tick", Value: "30000"},
				{Op: "resolve"},
			},
		},
		{
			Name: "compose shares dimension rows with list",
			Opts: map[string]any{"routes": composeRoutes},
			Steps: []stepSpec{
				{Op: "seg", ID: "view-seg", Value: "compose"},
			},
		},
		{
			Name: "refresh interval selects and save round trip",
			Opts: map[string]any{"routes": refreshCfgRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "setSelect", ID: "cfg-watch-refresh", Value: "60"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "unrelated save keeps implicit refresh unwritten",
			Opts: map[string]any{"routes": oldConfigNoRefreshRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "input", ID: "cfg-max-days", Value: "21"},
				{Op: "click", ID: "cfg-save"},
			},
		},
		{
			Name: "restore defaults sets refresh drafts to 30",
			Opts: map[string]any{"routes": resetRoutes},
			Steps: []stepSpec{
				{Op: "nav", Page: "config"},
				{Op: "click", ID: "cfg-reset"},
				{Op: "click", ID: "reset-confirm"},
			},
		},
	}

	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.js")
	if err := os.WriteFile(appPath, appSource, 0o644); err != nil {
		t.Fatalf("write app.js copy: %v", err)
	}
	scenariosPath := filepath.Join(dir, "scenarios.json")
	defsJSON, err := json.Marshal(scenarios)
	if err != nil {
		t.Fatalf("marshal scenarios: %v", err)
	}
	if err := os.WriteFile(scenariosPath, defsJSON, 0o644); err != nil {
		t.Fatalf("write scenarios: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, node, runnerPath, appPath, scenariosPath)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run node behavior runner: %v\nstderr: %s", err, stderr.String())
	}
	var reports []appJSReport
	if err := json.Unmarshal(out, &reports); err != nil {
		t.Fatalf("decode runner output: %v\nstdout: %s", err, out)
	}
	if len(reports) != len(scenarios) {
		t.Fatalf("runner produced %d reports, want %d", len(reports), len(scenarios))
	}
	byName := make(map[string]appJSReport, len(reports))
	for _, r := range reports {
		byName[r.Name] = r
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			r, ok := byName[sc.Name]
			if !ok {
				t.Fatalf("runner produced no report")
			}
			if r.Fatal != "" {
				t.Fatalf("app.js threw during scenario: %s", r.Fatal)
			}
			switch sc.Name {
			case "reload restores saved custom range":
				/* 区分度:URL 精确等于记忆区间——若 restoreSavedRange 丢失/解析坏
				   (回退 Today),URL 与 range-custom 高亮双双失败;若重复发起请求
				   (比如 meta/dashboard 重拉),计数断言失败 */
				if got := dashboardURL(t, r); got != "/api/dashboard?from=2026-08-01&to=2026-08-15" {
					t.Errorf("dashboard URL = %q, want saved custom range", got)
				}
				for _, b := range r.RangeSeg {
					if b.Pressed != "false" {
						t.Errorf("preset v=%s pressed=%q, want none selected for custom range", b.V, b.Pressed)
					}
				}
				if r.RangeCustom.Pressed != "true" || !r.RangeCustom.On {
					t.Errorf("range-custom pressed=%q on=%v, want pressed/on for custom range", r.RangeCustom.Pressed, r.RangeCustom.On)
				}
				/* init 链 = meta + config 预载 + dashboard 各一次 */
				if n := countFetch(r, "GET", "/api/meta"); n != 1 {
					t.Errorf("meta fetches = %d, want exactly 1", n)
				}
				if n := countFetch(r, "GET", "/api/config"); n != 1 {
					t.Errorf("config fetches = %d, want exactly 1", n)
				}
				if n := countFetch(r, "GET", "/api/dashboard"); n != 1 {
					t.Errorf("dashboard fetches = %d, want exactly 1", n)
				}
				if len(r.FetchCalls) != 3 {
					t.Errorf("total fetches = %d, want exactly meta+config+dashboard", len(r.FetchCalls))
				}
				var saved struct {
					S string `json:"s"`
					E string `json:"e"`
				}
				if err := json.Unmarshal([]byte(r.Session["tu-range"]), &saved); err != nil || saved.S != "2026-08-01" || saved.E != "2026-08-15" {
					t.Errorf("sessionStorage tu-range = %q, want preserved %s..%s", r.Session["tu-range"], "2026-08-01", "2026-08-15")
				}

			case "navigate defaults to today":
				/* 区分度:opener 子页环境(navigate)不得继承/臆造任何非今日区间;
				   若初始化默认 range 不为 0(如误读 storage 或写死其它预设),
				   URL 与今日按钮高亮失败 */
				assertTodayDefaults(t, r, today)

			case "performance absent degrades to today":
				/* 区分度:沙箱无 performance 对象——若新版重新引入 Navigation
				   Timing 且不带存在性守卫,app.js 会在 init 抛错(fatal 非空);
				   若降级逻辑回退到非今日区间,URL 断言失败 */
				assertTodayDefaults(t, r, today)

			case "empty navigation entries degrade to today":
				/* 区分度:performance 在但 navigation 条目为空——与整体缺失是两条
				   分支,同样必须安全降级今日;坏分支会让 fatal 或 URL 断言失败 */
				assertTodayDefaults(t, r, today)

			case "future custom range submitted as is":
				/* 区分度:未来区间必须原样携带——若前端加了日期 min/max 或按
				   data_through 截断,URL 将不是 2027 原值 */
				if got := dashboardURL(t, r); got != "/api/dashboard?from=2027-01-01&to=2027-01-31" {
					t.Errorf("dashboard URL = %q, want future range passed through unclamped", got)
				}
				if r.RangeCustom.Pressed != "true" || !r.RangeCustom.On {
					t.Errorf("range-custom pressed=%q on=%v, want pressed/on for custom range", r.RangeCustom.Pressed, r.RangeCustom.On)
				}

			case "dashboard 400 error message surfaces":
				/* 区分度:错误落点是 chart-sub、toast 与整页容器——showDashError
				   失败态覆盖 kpis/groups/cviews(page-note)并清空 sessions-body、
				   sessions-sub 同步错误文案;若错误被吞退化成通用文案或只写单点,
				   对应断言失败 */
				if !strings.Contains(r.ChartSub, "Range / 区间") {
					t.Errorf("chart-sub = %q, want server error message fragment", r.ChartSub)
				}
				if !strings.Contains(r.Toast, "Range / 区间") {
					t.Errorf("toast = %q, want error surfaced via toast", r.Toast)
				}
				if !strings.Contains(r.KpisHTML, "Range / 区间") {
					t.Errorf("kpis html = %q, want error note (stale stats must be cleared)", r.KpisHTML)
				}
				if !strings.Contains(r.GroupsHTML, "Range / 区间") {
					t.Errorf("groups html = %q, want error note (stale tables must be cleared)", r.GroupsHTML)
				}
				if r.SessionsSub != "Range / 区间" {
					t.Errorf("sessions-sub = %q, want error message", r.SessionsSub)
				}
				if r.SessionsBodyHTML != "" {
					t.Errorf("sessions body = %q, want emptied on load failure", r.SessionsBodyHTML)
				}
				if n := countFetch(r, "GET", "/api/dashboard"); n != 1 {
					t.Errorf("dashboard fetches = %d, want 1 (no retry on error)", n)
				}

			case "config dirty state transitions":
				/* 区分度:CFG 状态由 init 预载(fetchConfigState 填 saved/draft/
				   querySnapshot),且预载成功即渲染配置页控件——st[0] 断言渲染后
				   干净态(若渲染时机回退或 diff 误算,失败);编辑后 diff 必须算出
				   「1 项修改」(预载失败没填 CFG.saved 或 diff 恒 0 则失败);
				   改回原值必须复位(diff 只增不减则失败) */
				if n := countFetch(r, "GET", "/api/config"); n != 1 {
					t.Errorf("config GETs = %d, want 1 (preloaded by init, reused on nav)", n)
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0 (edit-only scenario)", n)
				}
				if len(r.StepStates) != 3 {
					t.Fatalf("step states = %d, want 3", len(r.StepStates))
				}
				st := r.StepStates
				if st[0].PollInput != "30" || st[0].DirtyPill != "已保存" || !st[0].SaveDisabled || st[0].SaveText != "已保存" || st[0].Toast != "" {
					t.Errorf("after config load = %+v, want rendered clean state (preload renders controls)", st[0])
				}
				if st[1].DirtyPill != "1 项修改" || st[1].SaveDisabled || st[1].SaveText != "保存" {
					t.Errorf("after poll 42 = %+v, want 1 dirty change with save enabled", st[1])
				}
				if st[2].DirtyPill != "已保存" || !st[2].SaveDisabled || st[2].SaveText != "已保存" || st[2].PollInput != "30" {
					t.Errorf("after revert to 30 = %+v, want restored clean state", st[2])
				}

			case "save 200 round trip uses get revision":
				/* 区分度:PUT body 必须带 GET 的 revision、被编辑值;场景只编辑
				   daemon 段,queryTouched 为 false——config 键集必须恰好五个且
				   无 query/无 diagnostics(发现把回退草稿的 query 原样回传之类
				   的回归);200 后草稿复位为已保存;再次保存 revision 必须换成
				   响应回写的 newrev */
				puts := putCalls(r)
				if len(puts) != 2 {
					t.Fatalf("config PUTs = %d, want 2", len(puts))
				}
				if puts[0].Body == nil || puts[1].Body == nil {
					t.Fatalf("PUT bodies missing: %+v", puts)
				}
				top := putTopKeys(t, *puts[0].Body)
				if len(top) != 2 || top["revision"] == nil || top["config"] == nil {
					t.Errorf("PUT#1 top-level keys = %v, want exactly {revision,config}", keysOf(top))
				}
				var body1 struct {
					Revision string                     `json:"revision"`
					Config   map[string]json.RawMessage `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body1); err != nil {
					t.Fatalf("decode PUT#1: %v", err)
				}
				if body1.Revision != "rev-1" {
					t.Errorf("PUT#1 revision = %q, want GET revision rev-1", body1.Revision)
				}
				wantConfigKeys := map[string]bool{"daemon": true, "log": true, "refresh": true, "clients": true, "routers": true, "provider_aliases": true}
				if len(body1.Config) != len(wantConfigKeys) {
					t.Errorf("PUT#1 config keys = %v, want exactly daemon/log/refresh/clients/routers/provider_aliases", keysOf(body1.Config))
				}
				for k := range wantConfigKeys {
					if _, ok := body1.Config[k]; !ok {
						t.Errorf("PUT#1 config missing key %q", k)
					}
				}
				if _, ok := body1.Config["diagnostics"]; ok {
					t.Errorf("PUT#1 config must not carry diagnostics key")
				}
				if _, ok := body1.Config["query"]; ok {
					t.Errorf("PUT#1 config must omit query key when query segment is untouched")
				}
				var daemon1 struct {
					PollInterval float64 `json:"poll_interval"`
					Autostart    bool    `json:"autostart"`
				}
				if err := json.Unmarshal(body1.Config["daemon"], &daemon1); err != nil {
					t.Fatalf("decode PUT#1 daemon: %v", err)
				}
				if daemon1.PollInterval != 42 {
					t.Errorf("PUT#1 daemon.poll_interval = %v, want edited 42", daemon1.PollInterval)
				}
				var body2 struct {
					Revision string `json:"revision"`
					Config   struct {
						Daemon struct {
							PollInterval float64 `json:"poll_interval"`
						} `json:"daemon"`
					} `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[1].Body), &body2); err != nil {
					t.Fatalf("decode PUT#2: %v", err)
				}
				if body2.Revision != "newrev" {
					t.Errorf("PUT#2 revision = %q, want newrev echoed by first save", body2.Revision)
				}
				if body2.Config.Daemon.PollInterval != 55 {
					t.Errorf("PUT#2 daemon.poll_interval = %v, want 55", body2.Config.Daemon.PollInterval)
				}
				var cfg2 struct {
					Config map[string]json.RawMessage `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[1].Body), &cfg2); err != nil {
					t.Fatalf("decode PUT#2: %v", err)
				}
				if _, ok := cfg2.Config["query"]; ok {
					t.Errorf("PUT#2 config must omit query key when query segment is untouched")
				}
				if len(r.StepStates) != 5 {
					t.Fatalf("step states = %d, want 5", len(r.StepStates))
				}
				st := r.StepStates
				if st[2].Toast != "配置已保存" || !st[2].SaveDisabled || st[2].SaveText != "已保存" || st[2].PollInput != "42" {
					t.Errorf("after first save = %+v, want saved+reset draft from response", st[2])
				}
				if st[4].Toast != "配置未变化" {
					t.Errorf("after second save toast = %q, want 配置未变化 (changed=false)", st[4].Toast)
				}

			case "save 409 keeps draft without retry":
				/* 区分度:409 必须只发一次 PUT(自动重试会让计数失败);toast 是 409
				   专属文案而非通用「保存失败」(吞掉冲突分支走 else 的回归会失败);
				   草稿保留——dirty-pill 仍是 1 项修改、save 仍可点 */
				puts := putCalls(r)
				if len(puts) != 1 {
					t.Fatalf("config PUTs = %d, want exactly 1 (no automatic retry)", len(puts))
				}
				if puts[0].Body == nil {
					t.Fatalf("PUT body missing")
				}
				var body struct {
					Revision string `json:"revision"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body); err != nil {
					t.Fatalf("decode PUT: %v", err)
				}
				if body.Revision != "rev-1" {
					t.Errorf("PUT revision = %q, want GET revision rev-1", body.Revision)
				}
				if !strings.Contains(r.Toast, "别处被修改") {
					t.Errorf("toast = %q, want 409-specific conflict notice", r.Toast)
				}
				if strings.Contains(r.Toast, "保存失败") {
					t.Errorf("toast = %q, must not fall back to generic save-failure message", r.Toast)
				}
				if !strings.Contains(r.DirtyPill.Text, "1") {
					t.Errorf("dirty pill = %q, want draft preserved with 1 change", r.DirtyPill.Text)
				}
				if r.CfgSave.Disabled {
					t.Errorf("save disabled after 409, want enabled (local edits kept)")
				}
				if len(r.StepStates) != 3 || !strings.Contains(r.StepStates[2].Toast, "别处被修改") {
					t.Errorf("step states = %+v, want conflict toast right after save click", r.StepStates)
				}

			case "locale switch persists without dirtying config":
				/* 区分度:语言切换只写 tu-locale 与 UI——若它污染配置草稿(重置 diff
				   或覆盖 poll_interval),dirty 数/poll 值/save 状态断言失败;若没写
				   localStorage 或 aria-pressed,持久化断言失败 */
				if r.Locale.Saved == nil || *r.Locale.Saved != "en" {
					t.Errorf("tu-locale = %v, want en persisted", r.Locale.Saved)
				}
				if r.Title != "Token Usage Dashboard" {
					t.Errorf("document.title = %q, want EN title", r.Title)
				}
				if r.Locale.EnPressed != "true" || r.Locale.ZhPressed != "false" {
					t.Errorf("locale buttons zh=%q en=%q, want en selected", r.Locale.ZhPressed, r.Locale.EnPressed)
				}
				if len(r.StepStates) != 3 {
					t.Fatalf("step states = %d, want 3", len(r.StepStates))
				}
				st := r.StepStates
				if !strings.Contains(st[1].DirtyPill, "1") || st[1].SaveDisabled {
					t.Errorf("before locale switch = %+v, want 1 pending change", st[1])
				}
				if !strings.Contains(st[2].DirtyPill, "1") || st[2].SaveDisabled || st[2].PollInput != "42" {
					t.Errorf("after locale switch = %+v, want same dirty count and untouched draft", st[2])
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0 (locale switch must not save)", n)
				}

			case "appearance toggles persist without touching config":
				/* 区分度:三个偏好各写各的 localStorage 键并翻转对应 UI 状态——主题
				   从 dark 翻到 light(方向反了 root/saved 断言失败);配色选中态迁移
				   到 azure;侧栏收起;config 流量只有 init 预载的 1 次 GET、零 PUT,
				   零 toast(外观操作误触配置流/保存的回归会失败) */
				if r.Theme.Saved == nil || *r.Theme.Saved != "light" {
					t.Errorf("tu-theme = %v, want light after toggle from dark", r.Theme.Saved)
				}
				if r.Theme.Root != "light" {
					t.Errorf("root data-theme = %q, want light", r.Theme.Root)
				}
				if r.Palette.Saved == nil || *r.Palette.Saved != "azure" {
					t.Errorf("tu-palette = %v, want azure persisted", r.Palette.Saved)
				}
				if r.Palette.Root != "azure" {
					t.Errorf("root data-palette = %q, want azure", r.Palette.Root)
				}
				pressedByPal := map[string]string{}
				for _, c := range r.Palette.Choices {
					pressedByPal[c.Pal] = c.Pressed
				}
				if pressedByPal["azure"] != "true" || pressedByPal["cobalt"] != "false" {
					t.Errorf("palette pressed state = %v, want azure on / cobalt off", pressedByPal)
				}
				if r.Palette.Name != "湛蓝" {
					t.Errorf("palette name = %q, want 湛蓝", r.Palette.Name)
				}
				if r.Side.Saved == nil || *r.Side.Saved != "min" {
					t.Errorf("tu-side = %v, want min persisted", r.Side.Saved)
				}
				if !r.Side.Min || r.Side.Expanded != "false" {
					t.Errorf("side state = min=%v expanded=%q, want collapsed", r.Side.Min, r.Side.Expanded)
				}
				if n := countFetch(r, "GET", "/api/config"); n != 1 || countFetch(r, "PUT", "/api/config") != 0 {
					t.Errorf("config traffic GET=%d PUT=%d, want exactly the init preload GET and no PUT",
						countFetch(r, "GET", "/api/config"), countFetch(r, "PUT", "/api/config"))
				}
				/* 配置页从未打开:recomputeDirty 因 CFG.saved 为空直接返回,pill 保持
				   index.html 静态初始文案;判定信号是 data-dirty 维持 false 且 pill
				   未出现任何修改计数 */
				if r.CfgActionsDirty != "false" {
					t.Errorf("cfg-actions data-dirty = %q, want false (appearance toggles must not dirty config)", r.CfgActionsDirty)
				}
				if strings.Contains(r.DirtyPill.Text, "项修改") || strings.Contains(r.DirtyPill.Text, "change") {
					t.Errorf("dirty pill = %q, want no change count from appearance toggles", r.DirtyPill.Text)
				}
				if r.Toast != "" {
					t.Errorf("toast = %q, want empty (appearance toggles must not toast)", r.Toast)
				}

			case "save 400 keeps draft without retry":
				/* 区分度:400 必须只发一次 PUT(自动重试让计数失败);toast 用服务端
				   message 而非通用「保存失败」(分支被吞走 else 时失败);草稿完全
				   保留——若 400 被误当 200 处理覆盖草稿,pill/poll 值/save 状态断言
				   全部失败 */
				puts := putCalls(r)
				if len(puts) != 1 {
					t.Fatalf("config PUTs = %d, want exactly 1 (no retry after 400)", len(puts))
				}
				if !strings.Contains(r.Toast, "invalid level / 无效级别") {
					t.Errorf("toast = %q, want server-side 400 message fragment", r.Toast)
				}
				if !strings.Contains(r.DirtyPill.Text, "1") {
					t.Errorf("dirty pill = %q, want draft preserved with 1 change", r.DirtyPill.Text)
				}
				if r.CfgSave.Disabled {
					t.Errorf("save disabled after 400, want enabled (local edits kept)")
				}
				if r.PollInput != "42" {
					t.Errorf("poll input = %q, want 42 (draft must not be reset by 400)", r.PollInput)
				}
				if len(r.StepStates) != 3 || !strings.Contains(r.StepStates[2].Toast, "invalid level") {
					t.Errorf("step states = %+v, want 400 message toast right after save click", r.StepStates)
				}

			case "reset all loads server defaults without saving":
				/* 区分度:确认后必须拉取 /api/config?defaults=1 并把默认草稿落到
				   编辑态——若实现退化为 clone(saved)(不发请求),defaults URL 与
				   poll 值断言失败;若直接 PUT 落盘,PUT 计数失败;若不复位为可保存
				   的脏草稿,save 状态断言失败 */
				foundDefaults := false
				for _, c := range r.FetchCalls {
					if c.Method == "GET" && c.URL == "/api/config?defaults=1" {
						foundDefaults = true
					}
				}
				if !foundDefaults {
					t.Errorf("no GET /api/config?defaults=1 among %+v", r.FetchCalls)
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0 (defaults load must not save)", n)
				}
				if r.PollInput != "60" {
					t.Errorf("poll input = %q, want 60 from server default draft", r.PollInput)
				}
				/* 已保存 fixture(30/空 level/7/claude/1 别名/client)与全默认
				   (60/debug/14/无客户端/无别名/day)逐字段不同,diff 必然 >0 */
				if r.DirtyPill.Text == "已保存" || !strings.Contains(r.DirtyPill.Text, "项修改") {
					t.Errorf("dirty pill = %q, want non-zero change count after defaults load", r.DirtyPill.Text)
				}
				if r.CfgSave.Disabled || r.CfgSave.Text != "保存" {
					t.Errorf("save state = %+v, want enabled 保存 (defaults pending save)", r.CfgSave)
				}
				if !r.ResetModalHidden {
					t.Errorf("reset modal still open after confirm")
				}
				if r.Toast != "已载入全部默认值（未保存）" {
					t.Errorf("toast = %q, want defaults-loaded notice", r.Toast)
				}
				/* 顺带覆盖 dvLabel:query.default=day 经本地化显示为「日期」 */
				if r.DvCurText != "日期" {
					t.Errorf("dv-cur = %q, want localized 日期 for default view day", r.DvCurText)
				}

			case "output columns do not touch kpi rail":
				/* 区分度:config 直接下发 output_columns=[total],语言切换触发
				   renderAll 重渲 KPI——若 renderKPIs 被改成跟随 colIds(回归),
				   KPI 只剩 Total,七标签断言失败;反之明细表必须只剩 total 列
				   (出现 Req 表头说明明细未跟随输出列) */
				for _, label := range []string{"Requests", "Input", "Output", "Cache Read", "Reasoning", "Cache Hit", "Total"} {
					if !strings.Contains(r.KpisHTML, label) {
						t.Errorf("kpis html missing label %q (output columns must not affect overview)", label)
					}
				}
				if !strings.Contains(r.GroupsHTML, ">Total<") {
					t.Errorf("groups html = %q, want detail table with only Total column", r.GroupsHTML)
				}
				if strings.Contains(r.GroupsHTML, ">Req<") {
					t.Errorf("groups html contains Req column, want output_columns=[total] applied to detail tables")
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0", n)
				}

			case "default view listbox escape returns focus":
				/* 区分度:Escape 必须关 listbox 并把焦点还给触发器——若 Escape
				   处理丢失,pop 仍展开且 aria-expanded 不断言失败;若 dvClose 丢掉
				   refocus,pop 关了但 activeElement 断言失败;若 dvOpen 崩溃
				   (closest('.config-card') 缺失),fatal 非空 */
				if !r.DvPopHidden {
					t.Errorf("dv-pop still open after Escape")
				}
				if r.DvTriggerExpanded != "false" {
					t.Errorf("q-default aria-expanded = %q, want false after Escape", r.DvTriggerExpanded)
				}
				if r.ActiveElementID != "q-default" {
					t.Errorf("activeElement = %q, want focus returned to q-default", r.ActiveElementID)
				}
				/* dvLabel:client 是内置维度,显示名本地化为「客户端」 */
				if r.DvCurText != "客户端" {
					t.Errorf("dv-cur = %q, want localized 客户端", r.DvCurText)
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0 (open/close must not save)", n)
				}

			case "meta network failure shows offline":
				/* 区分度:meta 请求失败必须显示离线态——若 renderMeta 退回写死
				   「在线」或漏加 offline class,两个断言分别失败;同时仪表板拉取
				   不得被 meta 失败阻断 */
				if !strings.Contains(r.AsofText, "离线 · 无法连接本地服务") {
					t.Errorf("asof text = %q, want offline message", r.AsofText)
				}
				if !r.AsofOffline {
					t.Errorf(".asof missing offline class on meta failure")
				}
				if n := countFetch(r, "GET", "/api/dashboard"); n != 1 {
					t.Errorf("dashboard fetches = %d, want 1 (meta failure must not block init)", n)
				}

			case "meta success shows online":
				/* 区分度:meta 成功必须走「在线 · 最近同步」分支且不带 offline
				   class——在线时误加 offline class 或 last 拼接丢失都会失败 */
				if !strings.Contains(r.AsofText, "在线 · 最近同步") {
					t.Errorf("asof text = %q, want online message with last sync", r.AsofText)
				}
				if r.AsofOffline {
					t.Errorf(".asof has offline class despite meta success")
				}

			case "output columns apply on first dashboard load":
				/* 区分度:明细列头必须在首开就跟随 config 预载的 output_columns=
				   [total]——若 colIds 恒用默认七列(不读 CFG.draft),出现请求列头,
				   断言失败;若 init 链并行化让 dashboard 先于 config 到达(或渲染
				   早于配置就绪),顺序断言与列头断言双双失败;KPI 七项是固定口径,
				   不得跟随输出列收缩 */
				cfgIdx := fetchIndex(r, "GET", "/api/config")
				dashIdx := fetchIndex(r, "GET", "/api/dashboard")
				if cfgIdx < 0 || dashIdx < 0 {
					t.Fatalf("config/dashboard calls missing: %+v", r.FetchCalls)
				}
				if dashIdx < cfgIdx {
					t.Errorf("dashboard fetch (idx %d) fired before config fetch (idx %d), want config preload first", dashIdx, cfgIdx)
				}
				if !strings.Contains(r.GroupsHTML, ">总量<") {
					t.Errorf("groups html = %q, want detail table with only 总量 column on first load", r.GroupsHTML)
				}
				for _, bad := range []string{">请求<", ">输入<", ">缓存读<", ">推理<", ">命中率<"} {
					if strings.Contains(r.GroupsHTML, bad) {
						t.Errorf("groups html contains column header %s, want output_columns=[total] applied on first load", bad)
					}
				}
				for _, label := range []string{"请求数", "输入", "输出", "缓存读取", "推理", "缓存命中率", "总量"} {
					if !strings.Contains(r.KpisHTML, label) {
						t.Errorf("kpis html missing label %q (overview must not follow output columns)", label)
					}
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0", n)
				}

			case "unchanged query omitted from save":
				/* 区分度:GET 的 query 段带历史 groups+diagnostics(问题态),用户只改
				   log.max_days——queryTouched 必须为 false,PUT body 的 config 不得
				   携带 query 键(回传回退草稿会静默清掉既有查询定义);若总是回传
				   query,键集断言失败;若 max_days 编辑丢失,值断言失败 */
				puts := putCalls(r)
				if len(puts) != 1 {
					t.Fatalf("config PUTs = %d, want 1", len(puts))
				}
				if puts[0].Body == nil {
					t.Fatalf("PUT body missing")
				}
				var body struct {
					Revision string                     `json:"revision"`
					Config   map[string]json.RawMessage `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body); err != nil {
					t.Fatalf("decode PUT: %v", err)
				}
				if body.Revision != "rev-1" {
					t.Errorf("PUT revision = %q, want GET revision rev-1", body.Revision)
				}
				wantConfigKeys := map[string]bool{"daemon": true, "log": true, "refresh": true, "clients": true, "routers": true, "provider_aliases": true}
				if len(body.Config) != len(wantConfigKeys) {
					t.Errorf("PUT config keys = %v, want exactly daemon/log/refresh/clients/routers/provider_aliases (query omitted)", keysOf(body.Config))
				}
				if _, ok := body.Config["query"]; ok {
					t.Errorf("PUT config must omit query key (query segment untouched)")
				}
				if _, ok := body.Config["diagnostics"]; ok {
					t.Errorf("PUT config must not carry diagnostics key")
				}
				var log1 struct {
					MaxDays float64 `json:"max_days"`
				}
				if err := json.Unmarshal(body.Config["log"], &log1); err != nil {
					t.Fatalf("decode PUT log: %v", err)
				}
				if log1.MaxDays != 21 {
					t.Errorf("PUT log.max_days = %v, want edited 21", log1.MaxDays)
				}
				if len(r.StepStates) != 3 {
					t.Fatalf("step states = %d, want 3", len(r.StepStates))
				}
				st := r.StepStates
				if !strings.Contains(st[1].DirtyPill, "1") || st[1].SaveDisabled {
					t.Errorf("after max_days 21 = %+v, want 1 dirty change with save enabled", st[1])
				}
				if st[2].Toast != "配置已保存" {
					t.Errorf("after save toast = %q, want 配置已保存", st[2].Toast)
				}

			case "empty range shows empty state":
				/* 区分度:全零序列必须走空态分支——若恢复渲染全零柱+「峰值 0」,
				   chart 会重新出现 bar-t 柱与 ax-strong 峰值文本且空态文案消失,
				   三处断言失败 */
				if !strings.Contains(r.ChartHTML, "该区间暂无用量") {
					t.Errorf("chart html = %q, want empty-state message 该区间暂无用量", r.ChartHTML)
				}
				if strings.Contains(r.ChartHTML, "bar-t") {
					t.Errorf("chart html contains zero-value bars, want no bars for an empty range")
				}
				if strings.Contains(r.ChartHTML, "ax-strong") {
					t.Errorf("chart html contains peak label, want no fake peak for an empty range")
				}

			case "reset all with broken query forces rewrite":
				/* 区分度:GET 的 query 是回退草稿且 defaults 与其全同——reset 后
				   diff 为 0,只有 forceQueryRewrite 能让 dirty 计 1、保存携带 query
				   覆盖磁盘问题态。若 reset 不置位(回归),st[2] 变「已保存」且
				   PUT#1 无 query 键,双处失败;若保存成功后 force 未清除,
				   PUT#2 仍带 query,失败 */
				foundDefaults := false
				for _, c := range r.FetchCalls {
					if c.Method == "GET" && c.URL == "/api/config?defaults=1" {
						foundDefaults = true
					}
				}
				if !foundDefaults {
					t.Errorf("no GET /api/config?defaults=1 among %+v", r.FetchCalls)
				}
				if len(r.StepStates) != 5 {
					t.Fatalf("step states = %d, want 5", len(r.StepStates))
				}
				st := r.StepStates
				/* b) diff 为 0 但 force 置位:必须按 1 项修改显示、save 可用 */
				if st[2].DirtyPill != "1 项修改" || st[2].SaveDisabled || st[2].SaveText != "保存" {
					t.Errorf("after defaults load = %+v, want 1 change counted by forceQueryRewrite", st[2])
				}
				if st[2].Toast != "已载入全部默认值（未保存）" {
					t.Errorf("toast after defaults load = %q, want defaults-loaded notice", st[2].Toast)
				}
				/* c) 第一次保存必须携带 query(覆盖磁盘问题态)且不带 diagnostics */
				puts := putCalls(r)
				if len(puts) != 2 {
					t.Fatalf("config PUTs = %d, want 2", len(puts))
				}
				if puts[0].Body == nil || puts[1].Body == nil {
					t.Fatalf("PUT bodies missing")
				}
				var body1 struct {
					Revision string                     `json:"revision"`
					Config   map[string]json.RawMessage `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body1); err != nil {
					t.Fatalf("decode PUT#1: %v", err)
				}
				if body1.Revision != "rev-1" {
					t.Errorf("PUT#1 revision = %q, want rev-1", body1.Revision)
				}
				if _, ok := body1.Config["query"]; !ok {
					t.Errorf("PUT#1 config must carry query key (forced rewrite over broken on-disk query)")
				}
				if _, ok := body1.Config["diagnostics"]; ok {
					t.Errorf("PUT#1 config must not carry diagnostics key")
				}
				var query1 struct {
					Default string         `json:"default"`
					Groups  map[string]any `json:"groups"`
				}
				if err := json.Unmarshal(body1.Config["query"], &query1); err != nil {
					t.Fatalf("decode PUT#1 query: %v", err)
				}
				if query1.Default != "client" {
					t.Errorf("PUT#1 query.default = %q, want client from default draft", query1.Default)
				}
				if len(query1.Groups) != 0 {
					t.Errorf("PUT#1 query.groups = %v, want empty default draft", query1.Groups)
				}
				/* d) 保存 200 回写后 force 必须清除:再次保存原样草稿不再回传 query */
				var body2 struct {
					Config map[string]json.RawMessage `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[1].Body), &body2); err != nil {
					t.Fatalf("decode PUT#2: %v", err)
				}
				if _, ok := body2.Config["query"]; ok {
					t.Errorf("PUT#2 config must omit query key (force cleared after successful save)")
				}
				if st[4].Toast != "配置未变化" {
					t.Errorf("after second save toast = %q, want 配置未变化", st[4].Toast)
				}

			case "unified manual refresh shares loading state":
				/* 区分度:手动刷新与首开共用同一入口与状态——两次响应都被
				   gate,点击刷新后必须出现 loading 态(手动来源、目标区间、
				   旧结果标识),旧数据仍可见;放行后 done 态一次性替换。若手动
				   刷新绕开统一入口自己 fetch,gate 与状态断言双双失败。 */
				if len(r.StepStates) != 3 {
					t.Fatalf("step states = %d, want 3", len(r.StepStates))
				}
				st := r.StepStates
				if st[1].RefreshState != "loading" || !strings.Contains(st[1].RefreshLabel, "正在手动刷新") {
					t.Errorf("after manual click = %+v, want loading with manual label", st[1])
				}
				if !strings.Contains(st[1].RefreshDetail, "下方暂为上次结果") {
					t.Errorf("loading detail = %q, want previous-result marker", st[1].RefreshDetail)
				}
				if st[1].Refreshing != "true" || st[1].DashBusy != "true" {
					t.Errorf("after manual click refreshing=%q busy=%q, want both true", st[1].Refreshing, st[1].DashBusy)
				}
				if st[2].RefreshState != "done" || st[2].Refreshing == "true" {
					t.Errorf("after settle = %+v, want done state and dimming cleared", st[2])
				}
				if n := countFetch(r, "GET", "/api/dashboard"); n != 2 {
					t.Errorf("dashboard fetches = %d, want 2 (init + manual)", n)
				}
				urls := []string{}
				for _, c := range r.FetchCalls {
					if c.Method == "GET" && strings.HasPrefix(c.URL, "/api/dashboard") {
						urls = append(urls, c.URL)
					}
				}
				if len(urls) != 2 || urls[0] != urls[1] {
					t.Errorf("manual refresh should re-request the same range, urls=%v", urls)
				}
				/* KPI 数值按 fmtTok 渲染并拆分单位 span:断言数值段 ">1.11<"→">2.22<" */
				if !strings.Contains(r.KpisHTML, ">2.22<") {
					t.Errorf("final kpis = %q, want second response marker 2.22", r.KpisHTML)
				}
				if strings.Contains(r.KpisHTML, ">1.11<") {
					t.Errorf("final kpis still show stale marker 1.11: %q", r.KpisHTML)
				}

			case "stale range response does not overwrite":
				/* 区分度:两个响应都挂起、快速切区间后再统一放行——旧范围响应
				   (先发出、seq 较小)必须被丢弃,页面只呈现新范围数据;若过期
				   响应回写,KPI 会退回旧标记。 */
				if n := countFetch(r, "GET", "/api/dashboard"); n != 2 {
					t.Errorf("dashboard fetches = %d, want 2", n)
				}
				urls := []string{}
				for _, c := range r.FetchCalls {
					if c.Method == "GET" && strings.HasPrefix(c.URL, "/api/dashboard") {
						urls = append(urls, c.URL)
					}
				}
				if len(urls) != 2 || urls[0] == urls[1] {
					t.Fatalf("range switch should change the query range, urls=%v", urls)
				}
				if strings.Contains(r.KpisHTML, ">1.11<") || strings.Contains(r.GroupsHTML, ">1.11<") {
					t.Errorf("stale response must not overwrite: kpis=%q", r.KpisHTML)
				}
				if r.Refresh.PageRefreshing == "true" {
					t.Errorf("data-refreshing must be cleared after settle, got %q", r.Refresh.PageRefreshing)
				}

			case "failure after success clears stale data":
				/* 区分度:先成功(旧标记可见)再失败——失败必须整页清旧数据并
				   显示错误,不能保留旧区间统计;刷新 pill 回 idle。 */
				if !strings.Contains(r.KpisHTML, "boom / 爆炸") {
					t.Errorf("kpis = %q, want full-page error (stale stats cleared)", r.KpisHTML)
				}
				if strings.Contains(r.KpisHTML, ">1.11<") {
					t.Errorf("kpis still contain stale stats: %q", r.KpisHTML)
				}
				if r.Refresh.State != "idle" {
					t.Errorf("refresh state = %q, want idle after failure", r.Refresh.State)
				}
				if !strings.Contains(r.Toast, "boom") {
					t.Errorf("toast = %q, want error surfaced", r.Toast)
				}

			case "auto refresh fires after interval":
				/* 区分度:手动虚拟时钟推进到间隔后必须自动发起同入口请求
				   (loading 态、放行后落新数据);若自动刷新未接状态机或漏排程,
				   fetch 计数与数据断言失败。 */
				if n := countFetch(r, "GET", "/api/dashboard"); n != 2 {
					t.Errorf("dashboard fetches = %d, want 2 (init + auto)", n)
				}
				if !strings.Contains(r.KpisHTML, ">2.22<") {
					t.Errorf("kpis = %q, want second response marker 2.22", r.KpisHTML)
				}

			case "compose shares dimension rows with list":
				/* 区分度:构成图必须与列表消费同一组后端行(12 行夹具=9 独立
				   +1 尾行)——图例恰好 10 项、尾行只出现一次、不做 5% 二次合并。 */
				legendCount := strings.Count(r.GroupsHTML, "lg-name")
				if legendCount != 40 { /* 四张维度卡 × 10 行(9 独立 + 1 尾行) */
					t.Errorf("legend entries = %d, want exactly 40 (4 cards x 10 rows)", legendCount)
				}
				if strings.Count(r.GroupsHTML, `class="lg-name">其他（3 项）`) != 4 {
					t.Errorf("groups html = %q, want exactly one tail per card from is_other/other_count", r.GroupsHTML)
				}
				for i := 1; i <= 9; i++ {
					if !strings.Contains(r.GroupsHTML, fmt.Sprintf("client-%02d", i)) {
						t.Errorf("legend missing independent row client-%02d", i)
					}
				}
				if strings.Contains(r.GroupsHTML, "client-10") {
					t.Errorf("rows beyond top 9 must not render independently: %q", r.GroupsHTML)
				}

			case "refresh interval selects and save round trip":
				/* 区分度:GET 草稿 20/90 渲染为「20 预设选中 + watch 其他…
				   (90 可见)」;下拉只改草稿,保存把 refresh 两值写进 PUT。 */
				if len(r.StepStates) != 3 {
					t.Fatalf("step states = %d, want 3", len(r.StepStates))
				}
				st := r.StepStates
				/* GET 草稿 20/90 的初始渲染:dashboard 命中 20 预设、watch 落「其他…」并回填 90 */
				if st[0].DashSelect != "20" {
					t.Errorf("dashboard select after load = %q, want preset 20", st[0].DashSelect)
				}
				if st[0].WatchSelect != "custom" || st[0].WatchCustom != "90" {
					t.Errorf("watch select after load = %s/%s, want custom 90", st[0].WatchSelect, st[0].WatchCustom)
				}
				puts := putCalls(r)
				if len(puts) != 1 || puts[0].Body == nil {
					t.Fatalf("config PUTs = %d, want 1", len(puts))
				}
				var body struct {
					Config struct {
						Refresh struct {
							DashboardInterval int `json:"dashboard_interval"`
							WatchInterval     int `json:"watch_interval"`
						} `json:"refresh"`
					} `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body); err != nil {
					t.Fatalf("decode PUT: %v", err)
				}
				if body.Config.Refresh.DashboardInterval != 20 || body.Config.Refresh.WatchInterval != 60 {
					t.Errorf("PUT refresh = %+v, want dashboard 20 kept + watch changed to 60", body.Config.Refresh)
				}
				/* 保存成功后以响应草稿重渲染:watch 落 60 预设、自定义输入隐藏 */
				if r.RefreshCfg.WatchSelect != "60" || !r.RefreshCfg.WatchCustomHidden {
					t.Errorf("after save = %+v, want watch preset 60 with custom hidden", r.RefreshCfg)
				}

			case "unrelated save keeps implicit refresh unwritten":
				/* 区分度:旧配置无 [refresh] 段(GET 原值 0/0),用户只改日志——
				   下拉按有效值显示 30 预设但草稿保持 0,PUT 必须回传 0/0(键值
				   为 0,服务端零值段整体省略不落盘);若 normalizeDraft 把 0
				   映射成 30 再回传,旧配置的无关保存就会固化显式 30/30。 */
				if len(r.StepStates) < 2 {
					t.Fatalf("step states = %d, want >= 2", len(r.StepStates))
				}
				if st := r.StepStates[0]; st.DashSelect != "30" || st.WatchSelect != "30" {
					t.Errorf("implicit default should render as the 30 preset, got %+v", st)
				}
				puts := putCalls(r)
				if len(puts) != 1 || puts[0].Body == nil {
					t.Fatalf("config PUTs = %d, want 1", len(puts))
				}
				var body struct {
					Config struct {
						Refresh struct {
							DashboardInterval int `json:"dashboard_interval"`
							WatchInterval     int `json:"watch_interval"`
						} `json:"refresh"`
					} `json:"config"`
				}
				if err := json.Unmarshal([]byte(*puts[0].Body), &body); err != nil {
					t.Fatalf("decode PUT: %v", err)
				}
				if body.Config.Refresh.DashboardInterval != 0 || body.Config.Refresh.WatchInterval != 0 {
					t.Errorf("PUT refresh = %+v, want 0/0 (user-layer raw values, no default solidification)", body.Config.Refresh)
				}
				/* 无关保存的脏计数只含被编辑项(0/0 与 saved 0/0 同值不计脏):
				   编辑后为「1 项修改」,保存成功后回到「已保存」。 */
				if st := r.StepStates[1]; !strings.Contains(st.DirtyPill, "1") {
					t.Errorf("dirty pill after edit = %q, want exactly the edited max_days change", st.DirtyPill)
				}
				if r.DirtyPill.Text != "已保存" {
					t.Errorf("dirty pill after save = %q, want 已保存", r.DirtyPill.Text)
				}

			case "restore defaults sets refresh drafts to 30":
				/* 区分度:服务端全默认草稿的 refresh 恒为 30/30——重置确认后
				   两个下拉都落在 30 预设;载入默认不落盘(PUT 计数为 0)。 */
				if r.RefreshCfg.DashSelect != "30" || r.RefreshCfg.WatchSelect != "30" {
					t.Errorf("refresh selects after defaults = %+v, want both 30", r.RefreshCfg)
				}
				if n := countFetch(r, "PUT", "/api/config"); n != 0 {
					t.Errorf("config PUTs = %d, want 0 (defaults load must not save)", n)
				}
			}
		})
	}
}
