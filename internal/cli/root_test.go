package cli

import (
	"github.com/spf13/cobra"
	"regexp"
	"strings"
	"testing"
)

// TestRootCommand_HasSubcommands 收口：root 必须列出且仅列出
// collect/config/errors/query/restart/start/status/stop/update/version/help/completion 十个用户可见子命令。
//
// 这是 strict 集合断言：多余或缺失任一项均失败。newRootCmd 现在显式
// InitDefaultHelpCmd/InitDefaultCompletionCmd（为改写双语 Short），因此
// help/completion 恒在集合内。Hidden 的 _run 不计入
// （用户侧 CLI 表面无 run），由 TestRootCommand_HiddenInternalRun 单独覆盖。
func TestRootCommand_HasSubcommands(t *testing.T) {
	cmd := NewRootCmd()

	want := map[string]bool{
		"config":     true,
		"collect":    true,
		"query":      true,
		"errors":     true,
		"start":      true,
		"status":     true,
		"stop":       true,
		"restart":    true,
		"update":     true,
		"version":    true,
		"help":       true,
		"completion": true,
	}

	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		// Hidden 命令（_run）不计入用户可见集合。
		if sub.Hidden {
			continue
		}
		got[sub.Name()] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("expected subcommand %q not found", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected user-visible subcommand %q at root (收口集合为 collect/config/errors/query/restart/start/status/stop/update/version/help/completion)", name)
		}
	}
}

// TestRootCommand_NoTopLevelRouter 旧语法负向回归守卫：
// 已删除顶层 newRouterCmd()，旧 `token-usage router backfill`
// 路径必须不存在。若有人误加回来，本测试立即失败。
func TestRootCommand_NoTopLevelRouter(t *testing.T) {
	root := NewRootCmd()
	for _, sub := range root.Commands() {
		if sub.Name() == "router" {
			t.Errorf("顶层不应再有 router 命令（已子命令化为 `collect router`）；旧 `token-usage router backfill` 路径已移除")
		}
	}
}

// TestRootCommand_RestartMounted 正向守卫（翻转自 的负向守卫）：
// 已实现 control.Restart，restart 必须挂载到 root 且用户可见（非 Hidden）。
func TestRootCommand_RestartMounted(t *testing.T) {
	root := NewRootCmd()
	for _, sub := range root.Commands() {
		if sub.Name() == "restart" {
			if sub.Hidden {
				t.Error("restart 应为用户可见命令（非 Hidden）")
			}
			return
		}
	}
	t.Error("restart 应已挂载到 root（实现 control.Restart 后必须添加）")
}

// TestRootCommand_HiddenInternalRun _run 是 daemon 子进程入口，必须 Hidden。
// 用户侧 CLI 表面不应出现 run 命令。
func TestRootCommand_HiddenInternalRun(t *testing.T) {
	root := NewRootCmd()
	for _, sub := range root.Commands() {
		if sub.Name() == "_run" && !sub.Hidden {
			t.Error("_run 必须保持 Hidden（用户侧 CLI 表面无 run）")
		}
	}
}

// TestAllVisibleCommandsShortBilingual 收口：所有用户可见命令（含 cobra 生成的
// help/completion 及其 shell 子命令）的 Short 必须是 English / 中文 双语并列。
// 递归整棵命令树逐个断言，防止将来新增命令或 cobra 升级引入的生成命令漏配双语。
// Hidden 命令（_run/_update-helper/_update-cleanup/__complete 等）不在用户可见面，不计入。
func TestAllVisibleCommandsShortBilingual(t *testing.T) {
	root := NewRootCmd()
	han := regexp.MustCompile(`\p{Han}`)
	for _, sub := range root.Commands() {
		checkShortBilingual(t, sub, han)
	}
}

// checkShortBilingual 递归断言 c 及其全部子命令中非 Hidden 者的 Short 含 "/" 与中文。
func checkShortBilingual(t *testing.T, c *cobra.Command, han *regexp.Regexp) {
	t.Helper()
	if !c.Hidden {
		if !strings.Contains(c.Short, "/") || !han.MatchString(c.Short) {
			t.Errorf("%q Short 应为 English / 中文 双语并列, got: %q", c.CommandPath(), c.Short)
		}
	}
	for _, sub := range c.Commands() {
		checkShortBilingual(t, sub, han)
	}
}
