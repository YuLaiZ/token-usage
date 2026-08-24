package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// newCollectRetryCmd 构造 `collect retry` 子命令：重试未解决失败组。
//
// 查询 collection_errors 中 unresolved 记录，按 (date, source) 分组，逐组日期采集。
// --client X 限定只处理 X 的未解决失败组（继承自 collect 父命令）。
// 不接受日期和 --force（NoArgs 拒绝位置参数；--force 不继承）。
func newCollectRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry",
		Short: "Retry failed collection records / 重试失败的采集记录",
		Long: `重试 collection_errors 中未解决的失败采集记录。

按 (date, source) 分组查询未解决错误，逐组按日期重新采集；
成功时自动恢复同组历史错误，失败时递增 retry_count。

--client X 限定只处理指定客户端的失败组（继承自 collect 父命令）。
`,
		Args: cobra.NoArgs,
		RunE: runCollectRetryCmd,
	}
}

func runCollectRetryCmd(cmd *cobra.Command, args []string) error {
	client, _ := cmd.Flags().GetString("client")

	cfg, err := loadCollectConfig(false)
	if err != nil {
		return err
	}

	// --client 存在性校验先于 logger/DB 初始化。
	if client != "" {
		if err := validateClientExists(cfg, client); err != nil {
			return err
		}
	}

	log, usageDB, cleanup, err := openCollectRuntime(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	deps := newDepsFactory(cfg)
	return runCollectRetry(cmdContext(cmd), deps, usageDB, log, cmd.OutOrStdout(), client)
}

// runCollectRetry 纯逻辑（便于测试注入 deps/db）。
func runCollectRetry(ctx context.Context, deps *engine.Deps, usageDB *db.DB, log *slog.Logger, out io.Writer, client string) error {
	return engine.RunRetryWithDepsContext(ctx, deps, usageDB, client, log, out)
}
