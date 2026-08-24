package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// newCollectRouterCmd 构造 `collect router --client X` 子命令：
// 仅 router 全量回填（不调用 client collector，不写 collection_log/collection_errors，不推进 cursor）。
//
// --client 必填且必须存在、enabled、声明 router 且 adapter 能装配。
func newCollectRouterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "router",
		Short: "Backfill router attribution for one client / 为指定客户端全量回填 router 归因",
		Long: `为指定客户端全量回填 router 归因。

全表读取该客户端配置的 router 日志，回填其全部历史 messages 的归因字段。
不调用 client collector，不写 collection_log/collection_errors，不推进 router cursor
（沿用 engine.RunRouterBackfill 语义）。

--client 必填（继承自 collect 父命令）。
`,
		Args: cobra.NoArgs,
		RunE: runCollectRouterCmd,
	}
}

func runCollectRouterCmd(cmd *cobra.Command, args []string) error {
	client, _ := cmd.Flags().GetString("client")

	cfg, err := loadCollectConfig(false)
	if err != nil {
		return err
	}

	if err := validateRouterTargetClient(cfg, client); err != nil {
		return err
	}

	log, usageDB, cleanup, err := openCollectRuntime(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	deps := newDepsFactory(cfg)
	return runCollectRouter(cmdContext(cmd), deps, usageDB, log, cmd.OutOrStdout(), client)
}

// validateRouterTargetClient 校验 --client：非空、存在、enabled、声明 router。
// adapter 是否能装配由 RunRouterBackfill 内部 deps.RouterFor 判断（路径无效/backfill 失败计入 router 阶段失败）。
func validateRouterTargetClient(cfg *config.Config, client string) error {
	if cfg == nil {
		return fmt.Errorf("有效配置不能为空")
	}
	if client == "" {
		return fmt.Errorf("未指定客户端：collect router 需要 --client <name>")
	}
	cc, ok := cfg.ClientConfig(client)
	if !ok {
		return fmt.Errorf("未知客户端: %s", client)
	}
	if !cc.Enabled {
		return fmt.Errorf("客户端 %s 已禁用，请先在 config 中启用", client)
	}
	if cc.Router == "" {
		return fmt.Errorf("客户端 %s 未配置 router", client)
	}
	return nil
}

// runCollectRouter 纯逻辑（便于测试注入 deps/db）。
// 全表读 router 日志回填指定 client 的全部历史 messages。
func runCollectRouter(ctx context.Context, deps *engine.Deps, usageDB *db.DB, log *slog.Logger, out io.Writer, client string) error {
	return engine.RunRouterBackfill(ctx, deps, usageDB, log, out, client)
}
