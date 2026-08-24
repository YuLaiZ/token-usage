package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Read one config value (dotted key) / 读取单项配置(dotted key)",
		Long: "读取单项配置（dotted key，脚本友好）。\n\n" +
			"读取的是「用户配置层」：即配置文件中显式写入的值，不展开 ~ 、不补默认路径、\n" +
			"不 clamp 数值字段。因此未在文件中显式配置的字段会返回其零值（如 poll_interval 返回 0）。\n\n" +
			"完整 effective 配置请使用 `token-usage config show`；\n" +
			"status/TUI 只提供人机摘要。\n\n" +
			"本命令不改脚本输出语义，只如实回显用户层值。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadUserConfigAuto()
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}
			val, err := config.Get(cfg, args[0])
			if err != nil {
				return err
			}
			cmd.Println(val)
			return nil
		},
	}
}
