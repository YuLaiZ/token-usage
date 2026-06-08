package cli

import (
	"testing"
)

// TestRootCommand_HasSubcommands 收口：root 必须列出且仅列出
// collect/config/errors/query/restart/start/status/stop/version 九个用户可见子命令。
//
// 这是 strict 集合断言：多余或缺失任一项均失败。使用从未执行过
// Execute()/ExecuteC() 的独立 NewRootCmd() 实例，避免 Cobra 延迟
// 注入的 completion/help 污染断言。Hidden 的 _run 不计入
// （用户侧 CLI 表面无 run），由 TestRootCommand_HiddenInternalRun 单独覆盖。
func TestRootCommand_HasSubcommands(t *testing.T) {
	cmd := NewRootCmd()

	want := map[string]bool{
		"config":  true,
		"collect": true,
		"query":   true,
		"errors":  true,
		"start":   true,
		"status":  true,
		"stop":    true,
		"restart": true,
		"version": true,
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
			t.Errorf("unexpected user-visible subcommand %q at root (收口集合为 collect/config/errors/query/restart/start/status/stop/version)", name)
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
