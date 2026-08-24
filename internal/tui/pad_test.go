package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// ---- pad 显示宽度对齐(runewidth.StringWidth 真相源) ----

// TestPad_DisplayWidthAlignment pad 按显示宽度补空格:
// 中文占 2 列,按字节 len 补齐会误判已满导致错位。
func TestPad_DisplayWidthAlignment(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "abc   "}, // 纯英文:3 列补到 6
		{"中文", "中文  "},    // 纯中文:显示 4 列(字节 len=6)补到 6
		{"a中b", "a中b  "},  // 中英混排
	}
	for _, c := range cases {
		got := pad(c.in, 6)
		if got != c.want {
			t.Errorf("pad(%q, 6) = %q, want %q", c.in, got, c.want)
		}
		if w := runewidth.StringWidth(got); w != 6 {
			t.Errorf("pad(%q, 6) 显示宽度 = %d, want 6", c.in, w)
		}
	}
	// 已满/超宽:原样返回,不截断不加空格
	if got := pad("超过宽度", 3); got != "超过宽度" {
		t.Errorf("超宽应原样返回, got %q", got)
	}
}

// TestPad_MixedTextSameColumnWidth 英文与中文项 pad 后显示宽度一致(同列对齐)。
func TestPad_MixedTextSameColumnWidth(t *testing.T) {
	w1 := runewidth.StringWidth(pad("abc", 16))
	w2 := runewidth.StringWidth(pad("中文", 16))
	if w1 != w2 {
		t.Errorf("pad 后显示宽度不一致: abc=%d, 中文=%d", w1, w2)
	}
}

// ---- 菜单列宽(menuColWidth) ----

// TestMainMenu_ItemsFitColumnWidth 双语菜单项均不超过列宽常量。
func TestMainMenu_ItemsFitColumnWidth(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	if len(m.items) == 0 {
		t.Fatal("主菜单应有菜单项")
	}
	for _, item := range m.items {
		if w := runewidth.StringWidth(item); w > menuColWidth {
			t.Errorf("菜单项 %q 显示宽度 %d 超过列宽 %d", item, w, menuColWidth)
		}
	}
}

// TestMainMenu_RowsAlignedByDisplayWidth 菜单行按显示宽度对齐:
// 每行 = 指示符(2 列) + pad(item, menuColWidth) + summary,
// summary 起始显示列恒为 2+menuColWidth,不因中文/双语项漂移。
func TestMainMenu_RowsAlignedByDisplayWidth(t *testing.T) {
	a := newAppForTest(&config.Config{DataDir: "/x"}, &config.Config{DataDir: "/x"}, nil)
	m := newMainMenu(a)
	lines := strings.Split(m.View(), "\n")
	for i, item := range m.items {
		padded := pad(item, menuColWidth)
		if w := runewidth.StringWidth(padded); w != menuColWidth {
			t.Errorf("菜单项 %q 补齐后显示宽度 %d != 列宽 %d", item, w, menuColWidth)
		}
		cur := "  "
		if i == m.cursor {
			cur = "▸ "
		}
		if !strings.HasPrefix(lines[i], cur+padded) {
			t.Errorf("菜单行 %d 应以 指示符+按列宽补齐的菜单项 开头:\n%q", i, lines[i])
		}
	}
}

// ---- 详情页 label 列宽(detailLabelColWidth) ----

// TestClientDetail_LabelsFitColumnWidth 详情页 label(含双语 router 标签与
// 最长 paths.* 键)均不超过列宽常量。
func TestClientDetail_LabelsFitColumnWidth(t *testing.T) {
	if w := runewidth.StringWidth(ui.Bi("Router", "绑定路由")); w > detailLabelColWidth {
		t.Errorf("双语 router 标签显示宽度 %d 超过列宽 %d", w, detailLabelColWidth)
	}
	// 实例检查:claude 详情页含 router 字段 + 各 path 字段
	edit := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}}}
	display := &config.Config{Clients: map[string]config.Client{"claude": {}}}
	a := newAppForTest(edit, display, nil)
	p := newClientDetailPage(a, "claude")
	if len(p.fields) == 0 {
		t.Fatal("claude 详情页应至少含 router 字段")
	}
	for _, f := range p.fields {
		if w := runewidth.StringWidth(f.label); w > detailLabelColWidth {
			t.Errorf("label %q 显示宽度 %d 超过列宽 %d", f.label, w, detailLabelColWidth)
		}
	}
}
