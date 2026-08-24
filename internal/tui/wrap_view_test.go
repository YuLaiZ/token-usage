package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// 窄终端下 App.View 的每一行（样式行除外）显示宽度不得超过终端宽度，
// 且折行不丢失内容——双语化后的说明/帮助长行依赖该兜底，否则被终端硬截断。
func TestAppViewWrapsToTerminalWidth(t *testing.T) {
	cfg := &config.Config{DataDir: "/some/data"}
	a := newAppForTest(cfg, cfg, nil)
	a.width = 40
	a.statusMsg = ui.Bi("changing data_dir requires migrating usage.db and the logs/ directory",
		"data_dir 变更需要迁移 usage.db 与 logs/ 目录")
	// 帮助层是超长行最密集的页面：经主菜单 View 输出。
	a.stack = []page{newMainMenu(a)}
	menu := a.stack[0].(*mainMenu)
	menu.showHelp = true

	view := a.View()
	for i, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "\x1b") {
			continue
		}
		if w := runewidth.StringWidth(line); w > 40 {
			t.Errorf("第 %d 行显示宽度 %d 超过终端宽度 40: %q", i, w, line)
		}
	}
	joined := strings.Join(strings.Split(view, "\n"), "")
	joined = strings.ReplaceAll(joined, " ", "")
	for _, want := range []string{"migrating", "usage.db", "变更需要迁移", "logs/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("折行后丢失内容 %q", want)
		}
	}
}

// width 未知（WindowSizeMsg 未到达）时保持原样，不折行。
func TestAppViewNoWidthUnchanged(t *testing.T) {
	cfg := &config.Config{DataDir: "/x"}
	a := newAppForTest(cfg, cfg, nil)
	if a.width != 0 {
		t.Skip("width 已初始化")
	}
	view := a.View()
	if !strings.Contains(view, "\n") {
		t.Fatal("View 应含多行内容")
	}
}

// 续行沿用首行前导空格，保持缩进折行后仍可读。
func TestWrapViewLinesKeepsIndent(t *testing.T) {
	got := wrapViewLines("  aaa bbb ccc ddd\nplain line", 10)
	want := "  aaa bbb\n  ccc ddd\nplain line"
	if got != want {
		t.Fatalf("wrapViewLines = %q, want %q", got, want)
	}
}

// header 含 bold ANSI 且 dirty/syncPending 追加长状态后，窄终端下也必须按
// 显示宽度折行（剥离转义后每行 ≤ 终端宽度），状态文本不丢。
func TestAppViewWrapsANSIHeaderWhenDirtyAndSyncPending(t *testing.T) {
	cfg := &config.Config{DataDir: "/x"}
	a := newAppForTest(cfg, cfg, nil)
	a.width = 40
	// dirty 由 draft 与磁盘基线不一致触发；syncPending 直接置位。
	a.diskBaseline = &config.Config{DataDir: "/other"}
	a.syncPending = true

	view := a.View()
	for i, line := range strings.Split(view, "\n") {
		if w := runewidth.StringWidth(stripANSI(line)); w > 40 {
			t.Errorf("第 %d 行剥离 ANSI 后显示宽度 %d 超过 40: %q", i, w, line)
		}
	}
	// 窄宽折行可能拆开词组（如 "Sync|retry pending"），按去空白比较整体内容。
	joined := strings.Join(strings.Fields(stripANSI(view)), " ")
	for _, want := range []string{
		"token-usage config",
		"Unsaved changes / 未保存改动",
		"Sync retry pending / 同步待重试",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("折行后丢失 header 状态 %q", want)
		}
	}
}

// stripANSI 去除 CSI 转义序列，供显示宽度断言。
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
