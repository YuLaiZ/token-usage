// internal/cli/serve_run.go
package cli

// serve_run.go 定义 Hidden 内部命令 _serve-run：`serve start` 经
// daemon.SpawnDetached 拉起的后台仪表板服务主体。用户不直接接触
// （Hidden=true，--help 不可见），与守护进程的 _run 一样遵循
// 「内部命令以 "_" 前缀命名」的先例。
//
// SpawnDetached 已把子进程 stdout/stderr 重定向到 serve.log，因此本命令
// 直接使用 cmd.OutOrStdout()/ErrOrStderr()——日志 writer 非 TTY，
// serveDashboard 的 OSC 8 链接逻辑自动降级为纯文本，无需特判。

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// configLoaderForServeRun 是 _serve-run 的配置加载 seam：生产路径与 foreground
// 一致走 loadConfig；测试注入临时 DataDir，避免依赖真实用户目录。
var configLoaderForServeRun = loadConfig

func newServeRunCmd(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "_serve-run",
		Short:        ui.Bi("Internal command (background dashboard body, spawned by serve start; do not invoke directly)", "内部命令（后台仪表板服务主体，由 serve start 拉起，不直接调用）"),
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true, // 失败详情写 serve.log 供 start 附带,Usage 块是噪声
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			// --addr 必填语义：空值直接报错并附可照抄示例（cobra-args 教训：
			// 报错文案自带示例），不进入 spawn 后才失败的路径。
			if strings.TrimSpace(addr) == "" {
				return fmt.Errorf("%s: %s",
					ui.Bi("missing required --addr", "缺少必填的 --addr"),
					ui.Bi("example: token-usage _serve-run --addr 127.0.0.1:8619", "示例：token-usage _serve-run --addr 127.0.0.1:8619"))
			}
			cfg, err := configLoaderForServeRun()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			// 后台路径：autoOpen 恒 false（--open 由 serve start 父进程在确认
			// 启动成功后执行），输出全部落入 serve.log。
			return serveDashboard(cfg, usageDB, version, addr, cmd.OutOrStdout(), cmd.ErrOrStderr(), false)
		},
	}
	cmd.Flags().String("addr", "", ui.Bi(
		"Listen address (required; passed in by serve start)",
		"监听地址（必填；由 serve start 传入）",
	))
	return cmd
}
