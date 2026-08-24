package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Read one config value (dotted key) / 读取单项配置(dotted key)",
		Long: ui.Bi("Read one config value (dotted key, script-friendly).\n\n"+
			"Reads the \"user config layer\": values explicitly written in the config file, without expanding ~, without default paths,\n"+
			"and without clamping numeric fields. Fields not explicitly configured return their zero value (e.g. poll_interval returns 0).\n\n"+
			"Use `token-usage config show` for the full effective config;\n"+
			"status/TUI only provide a human-oriented summary.\n\n"+
			"This command keeps its script output semantics and echoes the user-layer value as is.",
			"读取单项配置（dotted key，脚本友好）。\n\n"+
				"读取的是「用户配置层」：即配置文件中显式写入的值，不展开 ~ 、不补默认路径、\n"+
				"不 clamp 数值字段。因此未在文件中显式配置的字段会返回其零值（如 poll_interval 返回 0）。\n\n"+
				"完整 effective 配置请使用 `token-usage config show`；\n"+
				"status/TUI 只提供人机摘要。\n\n"+
				"本命令不改脚本输出语义，只如实回显用户层值。"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadUserConfigAuto()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
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
