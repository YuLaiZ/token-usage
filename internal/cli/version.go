package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
)

// newVersionCmd 构造 version 子命令，输出固定构建信息快照的详情。
//
// info 由调用方（newRootCmd）注入，保证 version 子命令与 root 的
// --version/-v 来自同一份 buildinfo.Info 快照；本命令不重新读取
// Git/文件/环境，也不调用 buildinfo.Current()。
func newVersionCmd(info buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "查看版本与构建信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), info.Detail())
			return nil
		},
	}
}
