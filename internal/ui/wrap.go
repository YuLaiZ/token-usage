package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// WrapDisplay 按显示宽度把 s 折为多行：中文等宽字符按实际显示宽度（2 列）
// 计；达宽时优先回退到行内最近的词间空格折行（空格消耗），无可用空格断点
// 则硬折。前导缩进空格不构成折点。width<=0 或不超宽时原样返回单元素。
// 极窄宽度下单个宽字符可能略超 width（CJK 字符不可拆分），调用方需容忍。
func WrapDisplay(s string, width int) []string {
	if width <= 0 || runewidth.StringWidth(s) <= width {
		return []string{s}
	}
	var lines []string
	var cur []rune
	curW := 0
	lastSpace := -1
	hasWord := false // cur 内是否已出现词字符；前导缩进空格区不产生折点
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if curW+rw > width && len(cur) > 0 {
			if lastSpace > 0 {
				// 折到词间空格处：空格消耗，其后的词与放不下的字符一起进入新行。
				lines = append(lines, string(cur[:lastSpace]))
				rest := append([]rune(nil), cur[lastSpace+1:]...)
				cur = append(cur[:0], rest...)
			} else {
				// 无词间空格可回退：硬折整行（行尾空格一并消耗）。
				lines = append(lines, strings.TrimRight(string(cur), " "))
				cur = cur[:0]
			}
			curW = runewidth.StringWidth(string(cur))
			lastSpace = -1
			hasWord = strings.TrimLeft(string(cur), " ") != ""
		}
		cur = append(cur, r)
		curW += rw
		if r != ' ' {
			hasWord = true
		}
		// 仅词间空格可作折点：空格前必须已有词字符，前导缩进除外。
		if r == ' ' && hasWord {
			lastSpace = len(cur) - 1
		}
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}

// visibleRune 是一个可见字符及其前置挂载的 ANSI 转义序列（转义不计显示宽度，
// 折行只作用于可见文本；序列附着到其后首个可见字符，随字符落在同一段）。
type visibleRune struct {
	ch  rune
	pre []string
}

// parseANSILine 把含 ANSI 转义序列的行解析为可见字符流；转义序列挂到其后
// 首个可见字符前，行尾悬空序列挂到末尾（输出时 flush）。
func parseANSILine(line string) []visibleRune {
	var out []visibleRune
	var pending []string
	i := 0
	for i < len(line) {
		if line[i] == 0x1b {
			j := i + 1
			if j < len(line) && line[j] == '[' {
				j++
				// CSI 序列以 0x40-0x7E 的 final byte 结束。
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
					j++
				}
				if j < len(line) {
					j++
				}
			}
			pending = append(pending, line[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		out = append(out, visibleRune{ch: r, pre: pending})
		pending = nil
		i += size
	}
	// 行尾悬空转义（其后无可见字符）挂一个空字符，输出时顺延到最后一段末尾。
	if len(pending) > 0 {
		out = append(out, visibleRune{ch: 0, pre: pending})
	}
	return out
}

// WrapANSI 对可能含 ANSI 转义序列的行按显示宽度折行：可见文本经 WrapDisplay
// 折行，转义序列不计宽并按原位置重插（折点落在可见文本上）。重建按段内容
// 顺序消费源字符流——折点消耗的空格不在任何段中，其挂载转义顺延到后段行首。
// 纯文本行为与 WrapDisplay 一致。
func WrapANSI(line string, width int) []string {
	if !strings.ContainsRune(line, 0x1b) {
		return WrapDisplay(line, width)
	}
	runes := parseANSILine(line)
	var visible strings.Builder
	for _, vr := range runes {
		if vr.ch != 0 {
			visible.WriteRune(vr.ch)
		}
	}
	segs := WrapDisplay(visible.String(), width)
	out := make([]string, 0, len(segs))
	p := 0
	var carry []string // 段边界悬空的转义序列，顺延到后段行首
	for si, seg := range segs {
		last := si == len(segs)-1
		var b strings.Builder
		for _, seq := range carry {
			b.WriteString(seq)
		}
		carry = nil
		target := []rune(seg)
		ti := 0
		for ; p < len(runes) && ti < len(target); p++ {
			vr := runes[p]
			if vr.ch == 0 {
				// 行尾悬空转义占位：挂到最后一段末尾。
				carry = append(carry, vr.pre...)
				continue
			}
			if vr.ch != target[ti] {
				// 折点消耗的空格（不在任何段中）：跳过，其挂载转义顺延。
				carry = append(carry, vr.pre...)
				continue
			}
			// 悬空转义（来自折点消耗的空格等）必须在下一可见字符写出前
			// flush，否则若本段已是最后一段，序列（如 reset）会整段丢失、
			// 样式状态泄漏到折行之后。
			for _, seq := range carry {
				b.WriteString(seq)
			}
			carry = nil
			for _, seq := range vr.pre {
				b.WriteString(seq)
			}
			b.WriteRune(vr.ch)
			ti++
		}
		if last {
			for ; p < len(runes); p++ {
				for _, seq := range runes[p].pre {
					b.WriteString(seq)
				}
			}
			// 兜底：段内失配进入 carry 后再无匹配字符时也必须写出。
			for _, seq := range carry {
				b.WriteString(seq)
			}
			carry = nil
		}
		out = append(out, b.String())
	}
	return out
}
