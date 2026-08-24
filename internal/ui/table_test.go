package ui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// 表格渲染：中英混排按显示宽度对齐、边框与行严格同宽、数字列右对齐。
func TestTableRendersAlignedBoxTable(t *testing.T) {
	tbl := NewTable(
		[]string{ColClient, ColRequests, ColInput},
		AlignLeft, AlignRight, AlignRight,
	)
	tbl.Row("ZCode", "1548", "5.31 M")
	tbl.Row("Codex App / Codex 应用", "123", "1.78 M")

	out := tbl.String()
	lines := strings.Split(out, "\n")
	if len(lines) != 6 { // 顶边框+表头+分隔线+2数据行+底边框
		t.Fatalf("表格行数 = %d, want 6:\n%s", len(lines), out)
	}
	wantWidth := runewidth.StringWidth(lines[0])
	for i, ln := range lines {
		if got := runewidth.StringWidth(ln); got != wantWidth {
			t.Errorf("第 %d 行显示宽度 %d != 边框 %d:\n%s", i, got, wantWidth, out)
		}
	}
	// 表头行与数据行的 │ 分隔列逐列对齐。
	bars := func(ln string) []int {
		var cols []int
		w := 0
		for _, r := range ln {
			if r == '│' {
				cols = append(cols, w)
			}
			w += runewidth.RuneWidth(r)
		}
		return cols
	}
	ref := bars(lines[1])
	for i, ln := range lines {
		b := bars(ln)
		if len(b) == 0 {
			continue // 顶/底边框行用 ┬/┴ 连接，无 │；整体宽度已由宽度断言覆盖
		}
		if len(b) != len(ref) {
			t.Fatalf("第 %d 行 │ 数量 %d != %d:\n%s", i, len(b), len(ref), out)
		}
		for j := range b {
			if b[j] != ref[j] {
				t.Fatalf("第 %d 行第 %d 个 │ 列位 %d != %d:\n%s", i, j, b[j], ref[j], out)
			}
		}
	}
	// 数字列右对齐：数据行的数字前有补齐空格（左填充）。
	if !strings.Contains(lines[3], " 1548") {
		t.Errorf("数字列应右对齐（左补空格）:\n%s", out)
	}
	// 中英混排不穿透：CJK 单元格所在列按 2 列宽度参与计算。
	if !strings.Contains(out, "Codex App / Codex 应用") {
		t.Errorf("宽单元格内容丢失:\n%s", out)
	}
}

func TestTableEmpty(t *testing.T) {
	out := NewTable([]string{ColClient, ColTotal}, AlignLeft, AlignRight).String()
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("空表应只有边框+表头，行数 = %d:\n%s", len(lines), out)
	}
}

// 数据单元格超上限按显示宽度截断加省略号；表头不受限、列宽下限由表头兜底。
func TestTableLimitsTruncateDataOnly(t *testing.T) {
	tbl := NewTable([]string{ColClient, ColTotal}, AlignLeft, AlignRight).
		Limits(18, 12)
	tbl.Row("claude-haiku-4-5-20251001-long-name", "9,930.24 M")
	tbl.Row("中", "1.28 M")

	out := tbl.String()
	if !strings.Contains(out, "...") {
		t.Errorf("超限数据应截断加省略号:\n%s", out)
	}
	if strings.Contains(out, "Requests ...") || strings.Contains(out, "Total ...") {
		t.Errorf("表头不应被截断:\n%s", out)
	}
	for i, ln := range strings.Split(out, "\n") {
		if got := runewidth.StringWidth(ln); got != runewidth.StringWidth(strings.Split(out, "\n")[0]) {
			t.Errorf("第 %d 行宽度与边框不一致:\n%s", i, out)
		}
	}
	if !strings.Contains(out, "中") {
		t.Errorf("未超限数据应保留:\n%s", out)
	}
}

// 省略号宽 3：上限 1/2 放不下省略号，须截到恰好上限宽而非突破上限；上限 3
// 恰好容纳省略号。
func TestTableLimitsTinyWidths(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		tbl := NewTable([]string{"H"}, AlignLeft).Limits(limit)
		tbl.Row("abcdef")
		out := tbl.String()
		for _, ln := range strings.Split(out, "\n") {
			if !strings.Contains(ln, "│") {
				continue // 边框行
			}
			for _, cell := range strings.Split(ln, "│") {
				cell = strings.TrimSpace(cell)
				if cell == "H" {
					continue // 表头不受限
				}
				if w := runewidth.StringWidth(cell); w > limit {
					t.Errorf("limit=%d 数据格宽 %d 突破上限: %q", limit, w, ln)
				}
			}
		}
		if limit == 3 && !strings.Contains(out, "...") {
			t.Errorf("limit=3 应显示省略号:\n%s", out)
		}
	}
}

// 两行表头：列宽取两行中较宽者，表头区渲染两行且逐列对齐，单行表头退化正常。
func TestTableTwoLineHeader(t *testing.T) {
	tbl := NewTable(
		[]string{HeaderLines("Client", "客户端"), HeaderLines("Cache Hit", "缓存命中")},
		AlignLeft, AlignRight,
	)
	tbl.Row("Codex App", "81.84%")

	out := tbl.String()
	lines := strings.Split(out, "\n")
	if len(lines) != 6 { // 边框+表头2行+分隔线+数据行+底边框
		t.Fatalf("两行表头总行数 = %d, want 6:\n%s", len(lines), out)
	}
	wantWidth := runewidth.StringWidth(lines[0])
	for i, ln := range lines {
		if got := runewidth.StringWidth(ln); got != wantWidth {
			t.Errorf("第 %d 行宽度 %d != %d:\n%s", i, got, wantWidth, out)
		}
	}
	if !strings.Contains(lines[1], "Client") || !strings.Contains(lines[2], "客户端") {
		t.Errorf("表头应上行英文下行中文:\n%s", out)
	}
	if !strings.Contains(lines[4], "Codex App") {
		t.Errorf("数据行缺失:\n%s", out)
	}
}

// 两行表头列宽 = max(英文行, 中文行, 数据) 而非两行拼接宽。
func TestTableTwoLineHeaderNarrowColumns(t *testing.T) {
	tbl := NewTable([]string{HeaderLines("Requests", "请求数")}, AlignRight)
	tbl.Row("474")
	out := tbl.String()
	// 列宽应为 8（Requests），而不是单行形态 "Requests / 请求数" 的 18
	if strings.Contains(out, "Requests / 请求数") {
		t.Errorf("两行表头不应出现单行拼接形态:\n%s", out)
	}
	for _, ln := range strings.Split(out, "\n") {
		inner := strings.Trim(ln, "│┌┐├┤└┘┬┼┴")
		if w := runewidth.StringWidth(strings.TrimSpace(inner)); w > 10 {
			t.Errorf("列宽超过两行表头预期:\n%s", out)
		}
	}
}
