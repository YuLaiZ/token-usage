package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestNewCollectCmd_HasSubcommands collect 应注册 all/router/retry 三个子命令。
func TestNewCollectCmd_HasSubcommands(t *testing.T) {
	cmd := newCollectCmd()
	want := map[string]bool{"all": false, "router": false, "retry": false}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("collect 缺少子命令 %q", name)
		}
	}
}

// TestCollect_HelpListsSubcommands collect help 输出应列出 all/router/retry。
func TestCollect_HelpListsSubcommands(t *testing.T) {
	cmd := newCollectCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})
	_ = cmd.Execute()
	out := buf.String()
	for _, name := range []string{"all", "router", "retry"} {
		if !strings.Contains(out, name) {
			t.Errorf("collect --help 输出缺少子命令 %q:\n%s", name, out)
		}
	}
}

// TestCollect_ClientFlagIsPersistentAndInherited --client 是 collect 的 PersistentFlag，
// 且被三个子命令继承。
func TestCollect_ClientFlagIsPersistentAndInherited(t *testing.T) {
	parent := newCollectCmd()
	if parent.PersistentFlags().Lookup("client") == nil {
		t.Fatal("collect 应将 --client 注册为 PersistentFlag")
	}
	for _, name := range []string{"all", "router", "retry"} {
		sub := findSub(parent, name)
		if sub == nil {
			t.Fatalf("collect 缺少子命令 %q", name)
		}
		// 子命令经 cobra 自动继承父命令的 PersistentFlags。
		if sub.InheritedFlags().Lookup("client") == nil {
			t.Errorf("子命令 %q 未继承 --client", name)
		}
	}
}

// TestCollect_ForceFlagLocalOnly --force 只在 collect 本身的 LocalFlag，
// 三个子命令 LocalFlag 与 PersistentFlag 都查不到，inherited 也没有。
func TestCollect_ForceFlagLocalOnly(t *testing.T) {
	parent := newCollectCmd()
	if parent.Flags().Lookup("force") == nil {
		t.Fatal("collect 本身应注册 --force LocalFlag")
	}
	if parent.PersistentFlags().Lookup("force") != nil {
		t.Error("--force 不应注册为 PersistentFlag")
	}
	for _, name := range []string{"all", "router", "retry"} {
		sub := findSub(parent, name)
		if sub == nil {
			t.Fatalf("collect 缺少子命令 %q", name)
		}
		if sub.Flags().Lookup("force") != nil {
			t.Errorf("子命令 %q 不应注册自己的 --force flag", name)
		}
		if sub.InheritedFlags().Lookup("force") != nil {
			t.Errorf("子命令 %q 不应继承 --force", name)
		}
	}
}

// TestCollect_NoObsoleteFlags --all/--retry flag 应已移除（改为子命令分发）。
func TestCollect_NoObsoleteFlags(t *testing.T) {
	cmd := newCollectCmd()
	for _, name := range []string{"all", "retry"} {
		if cmd.Flags().Lookup(name) != nil || cmd.PersistentFlags().Lookup(name) != nil {
			t.Errorf("collect 不应再注册 --%s flag（已改为子命令）", name)
		}
	}
}

// TestCollect_SubcommandsRejectPositionalArgs 三个子命令必须使用 cobra.NoArgs：
// 传入位置参数应被 cobra 在 RunE 之前拒绝。
//
// 注意：不使用 sub.Execute()——子命令作为 parent 执行时，cobra 在 NoArgs 失败前的
// 解析路径在某些场景会触发 parent RunE（runCollectDefault）→ 打开真实 DB（goroutine 泄漏），
// 在 -race 下挂起（遗留缺陷）。直接调用子命令的 Args 验证函数（cobra.NoArgs），
// 完全不执行 RunE、不碰 DB，确定性通过。
func TestCollect_SubcommandsRejectPositionalArgs(t *testing.T) {
	parent := newCollectCmd()
	for _, name := range []string{"all", "router", "retry"} {
		sub := findSub(parent, name)
		if sub == nil {
			t.Fatalf("collect 缺少子命令 %q", name)
		}
		if sub.Args == nil {
			t.Fatalf("子命令 %q 未设置 Args 校验（应为 cobra.NoArgs）", name)
		}
		// 直接调用 Args 验证函数，断言位置参数被拒绝。不执行 RunE、不打开 DB。
		if err := sub.Args(sub, []string{"20260701"}); err == nil {
			t.Errorf("子命令 %q 的 Args 应拒绝位置参数（cobra.NoArgs 语义）", name)
		}
	}
}

// TestCollect_SubcommandsRejectForceFlag 子命令上 --force 应是 unknown flag。
func TestCollect_SubcommandsRejectForceFlag(t *testing.T) {
	parent := newCollectCmd()
	for _, name := range []string{"all", "router", "retry"} {
		sub := findSub(parent, name)
		if sub == nil {
			t.Fatalf("collect 缺少子命令 %q", name)
		}
		sub.SilenceUsage = true
		sub.SilenceErrors = true
		sub.SetArgs([]string{"--force"})
		err := sub.Execute()
		if err == nil {
			t.Errorf("子命令 %q 应拒绝 --force（unknown flag）", name)
		}
	}
}

// findSub 在 cmd 的子命令里按 Name 查找。
func findSub(cmd *cobra.Command, name string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}
