package ui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestWrapDisplay(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"within width untouched", "short line", 40, []string{"short line"}},
		{"zero width returns as-is", "anything", 0, []string{"anything"}},
		{"space fold keeps both words legal", "aaa bbb ccc", 7, []string{"aaa", "bbb ccc"}},
		{"hard-wraps when no space", "abcdefgh", 3, []string{"abc", "def", "gh"}},
		{"cjk counts two columns", "中文字符", 4, []string{"中文", "字符"}},
		{"mixed width fold", "ab 中文 cd", 6, []string{"ab", "中文", "cd"}},
		{"leading indent is not a fold point", "   abcdef", 4, []string{"   a", "bcde", "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WrapDisplay(tc.in, tc.width)
			if len(got) != len(tc.want) {
				t.Fatalf("WrapDisplay = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("WrapDisplay = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// 折行不得丢失或改写任何字符（空格折点消耗除外），且每行显示宽度不超限。
func TestWrapDisplayPreservesRunes(t *testing.T) {
	s := "changing data_dir requires migrating usage.db and the logs/ directory / data_dir 变更需要迁移 usage.db 与 logs/ 目录"
	lines := WrapDisplay(s, 40)
	joined := strings.ReplaceAll(strings.Join(lines, ""), " ", "")
	orig := strings.ReplaceAll(s, " ", "")
	if joined != orig {
		t.Fatalf("折行丢失字符:\n got %q\nwant %q", joined, orig)
	}
	for i, ln := range lines {
		if w := runewidth.StringWidth(ln); w > 40 {
			t.Errorf("第 %d 行显示宽度 %d 超限: %q", i, w, ln)
		}
	}
}

// stripANSI 去除 CSI 转义序列，供宽度断言。
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// ANSI 感知折行：转义不计宽、序列按原位置保留、可见文本按显示宽度折行。
func TestWrapANSI(t *testing.T) {
	line := "\x1b[1mtoken-usage config\x1b[0m   ⚠ Unsaved changes / 未保存改动   ↻ Sync retry pending / 同步待重试"
	segs := WrapANSI(line, 40)
	if len(segs) < 2 {
		t.Fatalf("40 列下应折为多段: %q", segs)
	}
	joined := ""
	for i, seg := range segs {
		if w := runewidth.StringWidth(stripANSI(seg)); w > 40 {
			t.Errorf("第 %d 段可见宽度 %d 超限: %q", i, w, seg)
		}
		joined += stripANSI(seg)
	}
	// 折点消耗空格外不丢可见字符；转义序列全部保留。
	if joined != "token-usage config   ⚠ Unsaved changes / 未保存改动   ↻ Sync retry pending / 同步待重试" {
		// 折行消耗折点空格，逐段比较允许空格差异：改按去空格比较。
		if strings.ReplaceAll(joined, " ", "") != strings.ReplaceAll("token-usage config ⚠ Unsaved changes / 未保存改动 ↻ Sync retry pending / 同步待重试", " ", "") {
			t.Fatalf("可见文本丢失: %q", joined)
		}
	}
	all := strings.Join(segs, "")
	if !strings.Contains(all, "\x1b[1m") || !strings.Contains(all, "\x1b[0m") {
		t.Fatalf("ANSI 序列丢失: %q", all)
	}
	// 纯文本走 WrapDisplay 等价路径。
	if got := WrapANSI("plain text", 40); len(got) != 1 || got[0] != "plain text" {
		t.Fatalf("纯文本应原样: %q", got)
	}
}

// 折点消耗的空格携带 reset 序列时，序列必须在下一段可见字符前写出——
// 即使该段已是最后一段也不得丢失（否则粗体样式泄漏到折行之后）。
func TestWrapANSIPreservesEscapeOnFoldedSpace(t *testing.T) {
	segs := WrapANSI("\x1b[1mabc\x1b[0m def ghi", 7)
	all := strings.Join(segs, "")
	if !strings.Contains(all, "\x1b[1m") || !strings.Contains(all, "\x1b[0m") {
		t.Fatalf("折点空格上的序列丢失: %q", segs)
	}
	// reset 必须出现在可见文本 def 之前（先 flush carry 再写字符）。
	resetAt := strings.Index(all, "\x1b[0m")
	defAt := strings.Index(all, "def")
	if resetAt < 0 || defAt < 0 || resetAt > defAt {
		t.Fatalf("reset 应在 def 之前写出: %q", all)
	}
	for i, seg := range segs {
		if w := runewidth.StringWidth(stripANSI(seg)); w > 7 {
			t.Errorf("第 %d 段可见宽度 %d 超限: %q", i, w, seg)
		}
	}
}
