package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newConfigShowCmd 构造只读的 config show 命令。
// 固定走 loadConfig(复用 runtimecfg 的单一解析边界,不复制默认路径/resolver),
// 用 config.MarshalConfig 序列化为 TOML 后写 stdout,不加任何标题或提示前缀。
// 只读、零运行时副作用:不开 DB、不初始化日志、不触发 daemon 元数据、不抢进程锁。
func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show effective runtime config / 显示运行时生效配置",
		Long: ui.Bi("Show the effective runtime config (read-only, pure TOML).\n\n"+
			"Output:\n"+
			"- The effective config: values after expanding ~ prefixed paths and filling in defaults and registry default paths;\n"+
			"- Path rules: `~` prefixes are expanded; explicit relative paths and default paths derived from them (e.g. log.dir derived from data_dir, sessions_dir derived from state_dir) stay relative; other home-based default paths are absolute.\n\n"+
			"Properties:\n"+
			"- Read-only: never modifies the user config file on disk;\n"+
			"- Pure TOML: the first output byte is TOML content, no title/hint/warning prefix, pipeable straight into a TOML parser;\n"+
			"- The output is not a template to overwrite the user config file (it contains filled-in defaults; writing it back would lose the \"use defaults\" semantics).",
			"显示运行时生效的 effective 配置(只读,纯 TOML)。\n\n"+
				"输出内容:\n"+
				"- 即 effective 配置:展开 ~ 前缀路径、补齐默认值与 registry 默认路径后的值;\n"+
				"- 路径规则:`~` 前缀会展开;显式相对路径及其派生的默认路径(如 data_dir 派生的 log.dir、state_dir 派生的 sessions_dir)保持相对;其余 home-based 默认路径为绝对路径。\n\n"+
				"特性:\n"+
				"- 只读:不修改磁盘上的用户配置文件;\n"+
				"- 纯 TOML:输出首字符即 TOML 内容,无标题/提示/warning 前缀,可直接管道给 toml 解析器;\n"+
				"- 输出并非建议覆盖回用户配置文件的模板(包含补全后的默认值,直接回写会丢失「使用默认值」语义)。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			data, err := config.MarshalConfig(cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to marshal config", "序列化配置失败"), err)
			}
			fmt.Fprint(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}
