package cli

import (
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "配置管理(交互编辑 / 初始化 / 读写单项)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigTUIContext(cmdContext(cmd))
		},
	}
	cmd.AddCommand(newConfigShowCmd(), newConfigGetCmd(), newConfigSetCmd(), newInitCmd())
	return cmd
}
