package querier

import (
	"context"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// setupCacheCreateFixture 构造 fresh_input、cache_read、cache_create 均非零的消息账本:
//   - claude-sonnet-4 / Anthropic: fresh=700, read=300, create=200, output=50,
//     reasoning=50, total=1100, requests=1
//   - gpt-5.5 / OpenAI: fresh=700, read=300, create=100, output=50, reasoning=50,
//     total=1100, requests=1
//
// 两个模型分组的 Cache Hit:
//
//	300/(700+300+200) = 25.00%;  300/(700+300+100) = 27.27%
//
// 总计: fresh=1400, read=600, create=300 → Cache Hit 600/2300 = 26.09%。
func setupCacheCreateFixture(t *testing.T) *Querier {
	t.Helper()
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()
	if _, err := db.UpsertSessionMeta(ctx, testDB, []model.Session{
		{ID: "sess-alpha", Client: model.ClientClaudeCode, Directory: "/work", Project: "proj-A", Title: "fix-login", FirstTS: 1000, LastTS: 2000},
	}); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}
	msgs := []model.Message{
		{
			ID: "msg-cc-one", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
			Date: "2026-07-09", TS: 1000, Model: "claude-sonnet-4", Provider: "Anthropic", Directory: "/work", Project: "proj-A",
			FreshInputTokens: 700, OutputTokens: 50, CacheReadTokens: 300, CacheCreateTokens: 200, ReasoningTokens: 50, TotalTokens: 1100,
		},
		{
			ID: "msg-cc-two", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
			Date: "2026-07-10", TS: 2000, Model: "gpt-5.5", Provider: "OpenAI", Directory: "/work", Project: "proj-A",
			FreshInputTokens: 700, OutputTokens: 50, CacheReadTokens: 300, CacheCreateTokens: 100, ReasoningTokens: 50, TotalTokens: 1100,
		},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}
	return New(testDB)
}

// headerCellsOf 返回输出中首条含 │ 的表头英文行拆出的单元格。
func headerCellsOf(t *testing.T, out string) []string {
	t.Helper()
	var header string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "│") {
			header = ln
			break
		}
	}
	if header == "" {
		t.Fatalf("输出缺少表头行:\n%s", out)
	}
	cells := strings.Split(strings.Trim(header, "│"), "│")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// dataRows 返回除表头(前两个含 │ 行)与 Total 行外的数据行单元格。
func dataRows(t *testing.T, out string) [][]string {
	t.Helper()
	var rows [][]string
	seen := 0
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, "│") {
			continue
		}
		seen++
		if seen <= 2 { // 表头英文行 + 中文行
			continue
		}
		if strings.Contains(ln, "Total / 总计") {
			continue
		}
		cells := strings.Split(strings.Trim(ln, "│"), "│")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	return rows
}

// totalRow 返回 Total / 总计 行的单元格;缺失时 fatal。
func totalRow(t *testing.T, out string) []string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Total / 总计") {
			cells := strings.Split(strings.Trim(ln, "│"), "│")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			return cells
		}
	}
	t.Fatalf("输出缺少总计行:\n%s", out)
	return nil
}

// cacheCreateFixtureQuerier 按给定布局构造 Querier,布局非法时 fatal。
func cacheCreateFixtureQuerier(t *testing.T, columns []string) *Querier {
	t.Helper()
	q := setupCacheCreateFixture(t)
	if err := q.SetOutputColumns(columns); err != nil {
		t.Fatalf("SetOutputColumns(%v): %v", columns, err)
	}
	return q
}

// 自定义布局下表头、每个数据行、总计行的列数与顺序完全一致:
// cache_create 插入中间、隐藏多列、仅留一个指标三种形态。
func TestLayout_HeaderRowsAndTotalStrictlyAligned(t *testing.T) {
	tests := []struct {
		name     string
		columns  []string
		wantTail []string
	}{
		{
			name:     "cache_create in the middle",
			columns:  []string{"requests", "cache_create", "total"},
			wantTail: []string{"Requests", "Cache Create", "Total"},
		},
		{
			name:     "hide many columns",
			columns:  []string{"total", "requests"},
			wantTail: []string{"Total", "Requests"},
		},
		{
			name:     "single metric only",
			columns:  []string{"input"},
			wantTail: []string{"Input"},
		},
		{
			name:     "all eight metrics",
			columns:  []string{"requests", "input", "output", "cache_read", "cache_create", "reasoning", "total", "cache_hit"},
			wantTail: []string{"Requests", "Input", "Output", "Cache Read", "Cache Create", "Reasoning", "Total", "Cache Hit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := cacheCreateFixtureQuerier(t, tt.columns)
			out, err := q.ByModel(context.Background(), []string{"2026-07-09", "2026-07-10"})
			if err != nil {
				t.Fatal(err)
			}
			header := headerCellsOf(t, out)
			dimCount := 1
			if got := strings.Join(header[dimCount:], "|"); got != strings.Join(tt.wantTail, "|") {
				t.Errorf("表头指标区 = %v, want %v\n%s", got, tt.wantTail, out)
			}
			wantCells := dimCount + len(tt.columns)
			if len(header) != wantCells {
				t.Errorf("表头列数 = %d, want %d:\n%s", len(header), wantCells, out)
			}
			rows := dataRows(t, out)
			if len(rows) != 2 {
				t.Fatalf("应有两个模型分组行:\n%s", out)
			}
			for i, row := range rows {
				if len(row) != wantCells {
					t.Errorf("第 %d 行列数 = %d, want %d:\n%s", i, len(row), wantCells, out)
				}
			}
			total := totalRow(t, out)
			if len(total) != wantCells {
				t.Errorf("总计行列数 = %d, want %d:\n%s", len(total), wantCells, out)
			}
		})
	}
}

// 同一布局在四个分组视图与 session 中一致;session 的 Client/Project/Title 固定在前。
func TestLayout_AppliesToAllGroupViewsAndSessions(t *testing.T) {
	columns := []string{"total", "cache_create", "cache_hit"}
	q := cacheCreateFixtureQuerier(t, columns)
	ctx := context.Background()
	dates := []string{"2026-07-09", "2026-07-10"}

	wantTail := "Total|Cache Create|Cache Hit"
	for name, run := range map[string]func() (string, error){
		"client":   func() (string, error) { return q.ByClient(ctx, dates) },
		"model":    func() (string, error) { return q.ByModel(ctx, dates) },
		"provider": func() (string, error) { return q.ByProvider(ctx, dates, nil) },
		"project":  func() (string, error) { return q.ByProject(ctx, dates) },
	} {
		out, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		header := headerCellsOf(t, out)
		if got := strings.Join(header[1:], "|"); got != wantTail {
			t.Errorf("%s 指标区 = %q, want %q\n%s", name, got, wantTail, out)
		}
	}

	sessions, err := q.Sessions(ctx, dates)
	if err != nil {
		t.Fatal(err)
	}
	header := headerCellsOf(t, sessions)
	if len(header) != 6 {
		t.Fatalf("session 表头列数 = %d, want 6:\n%s", len(header), sessions)
	}
	for i, want := range []string{"Client", "Project", "Title", "Total", "Cache Create", "Cache Hit"} {
		if header[i] != want {
			t.Errorf("session 表头第 %d 列 = %q, want %q:\n%s", i, header[i], want, sessions)
		}
	}
}

// cache_create 显示值正确:分组值 200/100、总计 300;命中率口径含 cache_create。
func TestLayout_CacheCreateValuesAndHitRate(t *testing.T) {
	allEight := []string{"requests", "input", "output", "cache_read", "cache_create", "reasoning", "total", "cache_hit"}
	q := cacheCreateFixtureQuerier(t, allEight)
	ctx := context.Background()
	dates := []string{"2026-07-09", "2026-07-10"}

	out, err := q.ByModel(ctx, dates)
	if err != nil {
		t.Fatal(err)
	}
	rows := dataRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("两个模型分组:\n%s", out)
	}
	// claude-sonnet-4 (25.00%) 字节序在 gpt-5.5 (27.27%) 前;同 total 按键升序。
	byModel := map[string][]string{}
	for _, row := range rows {
		byModel[row[0]] = row
	}
	// 列: model | requests input output cache_read cache_create reasoning total cache_hit
	cc := byModel["claude-sonnet-4"][5]
	if cc != "200" {
		t.Errorf("claude-sonnet-4 的 Cache Create = %q, want 200:\n%s", cc, out)
	}
	hit := byModel["claude-sonnet-4"][8]
	if hit != "25.00%" {
		t.Errorf("claude-sonnet-4 的 Cache Hit = %q, want 25.00%%:\n%s", hit, out)
	}
	if got := byModel["gpt-5.5"][5]; got != "100" {
		t.Errorf("gpt-5.5 的 Cache Create = %q, want 100:\n%s", got, out)
	}
	if got := byModel["gpt-5.5"][8]; got != "27.27%" {
		t.Errorf("gpt-5.5 的 Cache Hit = %q, want 27.27%%:\n%s", got, out)
	}
	total := totalRow(t, out)
	if got := total[5]; got != "300" {
		t.Errorf("总计 Cache Create = %q, want 300:\n%s", got, out)
	}
	if got := total[8]; got != "26.09%" {
		t.Errorf("总计 Cache Hit = %q, want 26.09%%:\n%s", got, out)
	}

	// session 按 session 聚合:两条消息同一 session,cache create 合计 300,
	// Cache Hit 与分组总计同公式 26.09%。
	sessions, err := q.Sessions(ctx, dates)
	if err != nil {
		t.Fatal(err)
	}
	sessionRows := dataRows(t, sessions)
	if len(sessionRows) != 1 {
		t.Fatalf("session 应恰一行:\n%s", sessions)
	}
	// 列: client project title | requests input output cache_read cache_create reasoning total cache_hit
	if got := sessionRows[0][7]; got != "300" {
		t.Errorf("session Cache Create = %q, want 300:\n%s", got, sessions)
	}
	if got := sessionRows[0][10]; got != "26.09%" {
		t.Errorf("session Cache Hit = %q, want 26.09%%:\n%s", got, sessions)
	}
}

// 隐藏 cache_create 后只移除该列,其余列值与命中率百分比不变。
func TestLayout_HidingCacheCreateKeepsHitRate(t *testing.T) {
	ctx := context.Background()
	dates := []string{"2026-07-09", "2026-07-10"}

	withCreate := cacheCreateFixtureQuerier(t, []string{"cache_read", "cache_create", "cache_hit"})
	outWith, err := withCreate.ByModel(ctx, dates)
	if err != nil {
		t.Fatal(err)
	}

	withoutCreate := cacheCreateFixtureQuerier(t, []string{"cache_read", "cache_hit"})
	outWithout, err := withoutCreate.ByModel(ctx, dates)
	if err != nil {
		t.Fatal(err)
	}

	// 显示态含 Cache Create 列,隐藏态只移除该列。
	if !strings.Contains(outWith, "Cache Create") {
		t.Errorf("显示态应含 Cache Create 列:\n%s", outWith)
	}
	if strings.Contains(outWithout, "Cache Create") || strings.Contains(outWithout, "缓存创建") {
		t.Errorf("隐藏后不得残留 Cache Create 列:\n%s", outWithout)
	}
	// 隐藏前后命中率百分比与缓存读取值不变。
	for _, want := range []string{"25.00%", "27.27%", "26.09%", "600"} {
		if !strings.Contains(outWith, want) || !strings.Contains(outWithout, want) {
			t.Errorf("隐藏 cache_create 后命中率/缓存读取应不变,缺 %q:\n%s\n%s", want, outWith, outWithout)
		}
	}
}

// 隐藏 total 后,分组行排序仍按真实 total_tokens 降序;总计行仍用独立聚合。
func TestLayout_HidingTotalKeepsSortAndIndependentTotal(t *testing.T) {
	q := setupCacheCreateFixture(t)
	// 第三条消息使 claude-sonnet-4 组 total 更大(2000 > 1100),排序必须在前。
	if _, err := db.UpsertMessages(context.Background(), q.db, []model.Message{{
		ID: "msg-cc-three", SessionID: "sess-alpha", Client: model.ClientClaudeCode,
		Date: "2026-07-09", TS: 1500, Model: "claude-sonnet-4", Provider: "Anthropic", Directory: "/work", Project: "proj-A",
		FreshInputTokens: 100, OutputTokens: 10, TotalTokens: 900,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetOutputColumns([]string{"requests", "input"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := q.ByModel(ctx, []string{"2026-07-09", "2026-07-10"})
	if err != nil {
		t.Fatal(err)
	}
	rows := dataRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("两个分组行:\n%s", out)
	}
	if rows[0][0] != "claude-sonnet-4" {
		t.Errorf("隐藏 total 后排序仍按真实 total_tokens,首行应为 claude-sonnet-4:\n%s", out)
	}
	// 总计行 = 独立聚合:requests=3, fresh=1500 → 1.50 K。
	total := totalRow(t, out)
	if total[1] != "3" || total[2] != "1.50 K" {
		t.Errorf("总计行应为独立聚合(requests=3, fresh=1.50 K): %v\n%s", total, out)
	}
}

// SetOutputColumns 防御性校验:空、未知 ID、重复 ID 拒绝;合法布局被独立拷贝。
func TestSetOutputColumns_ValidationAndCopy(t *testing.T) {
	q := setupMessageFixture(t)
	for name, columns := range map[string][]string{
		"empty":         {},
		"unknown id":    {"requests", "nope"},
		"duplicate id":  {"requests", "requests"},
		"wrong case":    {"Requests"},
		"whitespace id": {" requests"},
	} {
		if err := q.SetOutputColumns(columns); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
	if err := q.SetOutputColumns([]string{"total", "requests"}); err != nil {
		t.Fatalf("合法布局: %v", err)
	}
	// 修改入参切片不影响已设置的布局:布局仍为 [total],不得出现 requests 列。
	columns := []string{"total"}
	if err := q.SetOutputColumns(columns); err != nil {
		t.Fatal(err)
	}
	columns[0] = "mutated"
	out, err := q.ByClient(context.Background(), bothDates)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "总计") {
		t.Errorf("布局 [total] 的 Total 列应渲染:\n%s", out)
	}
	if strings.Contains(out, "Requests") || strings.Contains(out, "请求数") {
		t.Errorf("布局 [total] 不应出现 requests 列(入参修改泄漏):\n%s", out)
	}
}

// 默认布局锁定:无布局参数时保持七列逐字输出(升级不变合同)。
func TestDefaultLayout_LockedSevenColumns(t *testing.T) {
	q := setupCacheCreateFixture(t)
	out, err := q.ByModel(context.Background(), []string{"2026-07-09", "2026-07-10"})
	if err != nil {
		t.Fatal(err)
	}
	header := headerCellsOf(t, out)
	wantTail := []string{"Requests", "Input", "Output", "Cache Read", "Reasoning", "Total", "Cache Hit"}
	if got := header[len(header)-7:]; strings.Join(got, "|") != strings.Join(wantTail, "|") {
		t.Errorf("默认布局表头尾 = %v, want %v:\n%s", got, wantTail, out)
	}
	if strings.Contains(out, "Cache Create") || strings.Contains(out, "缓存创建") {
		t.Errorf("默认布局不得出现 Cache Create 列:\n%s", out)
	}
	// 与 ui 默认序列同源。
	defaults := ui.DefaultOutputColumns()
	if len(defaults) != 7 {
		t.Errorf("默认列数 = %d, want 7", len(defaults))
	}
}
