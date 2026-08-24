// Package ui 表格排版：框线表格按显示宽度对齐（CJK 占 2 列），
// 文本列左对齐、数字列右对齐，边框段与列宽联动（列宽+2）。
package ui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Align 是表格列的对齐方向。
type Align int

const (
	AlignLeft Align = iota
	AlignRight
)

// Table 渲染 Box-Drawing 框线表格：列宽取表头与全部数据行的最大显示宽度，
// 不做终端宽度适配与自动换行（CLI 表格保持完整结构，窄终端由终端自身折行；
// 自动换行只属于 TUI 的 wrapViewLines 兜底）。
type Table struct {
	headers []string
	aligns  []Align
	limits  []int // 各列显示宽度上限（0 = 不限）；超限截断加省略号
	rows    [][]string
}

// NewTable 声明表头与各列对齐方向（aligns 长度须与 headers 一致）。
func NewTable(headers []string, aligns ...Align) *Table {
	if len(aligns) < len(headers) {
		padded := append(append([]Align(nil), aligns...), make([]Align, len(headers)-len(aligns))...)
		aligns = padded
	}
	return &Table{headers: headers, aligns: aligns[:len(headers)]}
}

// Limits 声明各列数据单元格的显示宽度上限（0 = 不限）：数据超限时按显示
// 宽度截断并追加省略号，防止超长值（长项目名/模型名）无限扩宽表格。表头
// 是受控常量不受限；列宽 = max(表头宽, min(数据宽, 上限))。
func (t *Table) Limits(widths ...int) *Table {
	t.limits = widths
	return t
}

// Row 追加一行数据；单元格数与列数不符时按缺失补空、多余忽略。
func (t *Table) Row(cells ...string) *Table {
	t.rows = append(t.rows, cells)
	return t
}

func (t *Table) cell(s string, w int, a Align) string {
	pad := w - runewidth.StringWidth(s)
	if pad < 0 {
		pad = 0
	}
	if a == AlignRight {
		return strings.Repeat(" ", pad) + s
	}
	return s + strings.Repeat(" ", pad)
}

// limited 返回第 i 列单元格经上限截断后的值。省略号自身宽 3：上限小于 3
// 时放不下省略号，改用空尾巴截到恰好上限宽；上限恰为 3 时省略号自身即满宽。
// 两种情况结果都不超过声明上限。
func (t *Table) limited(i int, s string) string {
	if i < len(t.limits) && t.limits[i] > 0 && runewidth.StringWidth(s) > t.limits[i] {
		if t.limits[i] < 3 {
			return runewidth.Truncate(s, t.limits[i], "")
		}
		return runewidth.Truncate(s, t.limits[i], "...")
	}
	return s
}

// headerSegments 把每个表头按换行拆为多段（两行表头形态），段数不足者补空，
// 返回按行组织的段矩阵；单行表头退化为一段。
func (t *Table) headerSegments() [][]string {
	rows := 1
	segs := make([][]string, len(t.headers))
	for i, h := range t.headers {
		parts := strings.Split(h, "\n")
		if len(parts) > rows {
			rows = len(parts)
		}
		segs[i] = parts
	}
	matrix := make([][]string, rows)
	for r := range matrix {
		matrix[r] = make([]string, len(t.headers))
		for i := range t.headers {
			if r < len(segs[i]) {
				matrix[r][i] = segs[i][r]
			}
		}
	}
	return matrix
}

func (t *Table) widths() []int {
	w := make([]int, len(t.headers))
	for i, h := range t.headers {
		for _, seg := range strings.Split(h, "\n") {
			if sw := runewidth.StringWidth(seg); sw > w[i] {
				w[i] = sw
			}
		}
	}
	for _, row := range t.rows {
		for i := 0; i < len(row) && i < len(w); i++ {
			if cw := runewidth.StringWidth(t.limited(i, row[i])); cw > w[i] {
				w[i] = cw
			}
		}
	}
	return w
}

// border 生成一条框线：left + 每列 (w+2) 个 ─ 以 mid 相连 + right。
func (t *Table) border(left, mid, right string) string {
	var parts []string
	for _, w := range t.widths() {
		parts = append(parts, strings.Repeat("─", w+2))
	}
	return left + strings.Join(parts, mid) + right
}

// String 渲染完整表格：顶边框、表头行、表头下分隔线、数据行、底边框。
func (t *Table) String() string {
	w := t.widths()
	var b strings.Builder
	b.WriteString(t.border("┌", "┬", "┐"))
	for _, row := range t.headerSegments() {
		b.WriteByte('\n')
		b.WriteString(t.headerString(row, w))
	}
	b.WriteByte('\n')
	b.WriteString(t.border("├", "┼", "┤"))
	for _, row := range t.rows {
		b.WriteByte('\n')
		b.WriteString(t.rowString(row, w))
	}
	b.WriteByte('\n')
	b.WriteString(t.border("└", "┴", "┘"))
	return b.String()
}

// headerString 渲染一行表头（表头为受控常量，不施加数据列上限截断）。
func (t *Table) headerString(cells []string, w []int) string {
	parts := make([]string, len(w))
	for i := range w {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		parts[i] = " " + t.cell(cell, w[i], t.aligns[i]) + " "
	}
	return "│" + strings.Join(parts, "│") + "│"
}

func (t *Table) rowString(cells []string, w []int) string {
	parts := make([]string, len(w))
	for i := range w {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		parts[i] = " " + t.cell(t.limited(i, cell), w[i], t.aligns[i]) + " "
	}
	return "│" + strings.Join(parts, "│") + "│"
}
