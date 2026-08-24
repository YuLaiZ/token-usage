package cli

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

// TestHelpOutputBilingualLongAndFlagsButEnglishSkeleton 收口：--help 输出中
// 自定义文案（Long、flag usage）必须双语，cobra 框架骨架（Usage/Flags 布局）
// 保持英文。
func TestHelpOutputBilingualLongAndFlagsButEnglishSkeleton(t *testing.T) {
	cases := []struct {
		args          []string
		wantBilingual []string // 双语断言片段：英文与中文各取代表
	}{
		{[]string{"start", "--help"}, []string{"returns immediately", "后台启动守护进程"}},
		{[]string{"collect", "--help"}, []string{"client X limits to one client", "限定单客户端"}},
	}
	han := regexp.MustCompile(`\p{Han}`)
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := NewRootCmd()
			root.SetArgs(tc.args)
			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			got := buf.String()
			for _, want := range tc.wantBilingual {
				if !strings.Contains(got, want) {
					t.Errorf("--help 输出缺少 %q:\n%s", want, got)
				}
			}
			// 自定义 flag usage 双语：collect --client 描述含中文与英文。
			if strings.Join(tc.args, " ") == "collect --help" {
				if !strings.Contains(got, "--client") || !han.MatchString(got) {
					t.Errorf("--client usage 应为双语:\n%s", got)
				}
			}
			// cobra 骨架保持英文。
			for _, skeleton := range []string{"Usage:", "Flags:"} {
				if !strings.Contains(got, skeleton) {
					t.Errorf("--help 骨架 %q 缺失（应保持英文骨架）:\n%s", skeleton, got)
				}
			}
		})
	}
}

// TestFailureCausePrintedOnce 收口：start/stop/restart/update 失败时，cause
// 文本在完整 cobra 输出（含 Error: 行与 usage）中恰好出现一次——命令不得
// 手写 stderr 后再返回同一错误（双打）。
func TestFailureCausePrintedOnce(t *testing.T) {
	restoreCfg := injectStartConfig(startCfgEnabled(), nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			startErr:   errStartBoom,
			stopErr:    errors.New("stop boom"),
			restartErr: errors.New("restart boom"),
		}, nil
	}
	// update --version 非法值校验先于工厂调用，真实校验路径不会用到 stub；
	// --check 失败路径经工厂注入确定性错误。
	withStubUpdateService(t, &stubUpdateService{checkErr: errors.New("update check boom")})

	for _, tc := range []struct {
		name  string
		args  []string
		cause string
	}{
		{"start", []string{"start"}, "start boom"},
		{"stop", []string{"stop"}, "stop boom"},
		{"restart", []string{"restart"}, "restart boom"},
		{"update invalid version", []string{"update", "--version", "invalid"}, "missing the v prefix"},
		{"update check failure", []string{"update", "--check"}, "update check boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetArgs(tc.args)
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			if err := root.Execute(); err == nil {
				t.Fatal("expected command failure")
			}
			combined := out.String() + errOut.String()
			if n := strings.Count(combined, tc.cause); n != 1 {
				t.Errorf("失败 cause %q 出现 %d 次，应恰好一次（cobra 单次输出）:\n%s", tc.cause, n, combined)
			}
		})
	}
}
