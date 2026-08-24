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
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newCollectRouterCmd 构造 `collect router --client X` 子命令：
// 仅 router 全量回填（不调用 client collector，不写 collection_log/collection_errors，不推进 cursor）。
//
// --client 必填且必须存在、enabled、声明 router 且 adapter 能装配。
func newCollectRouterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "router",
		Short: "Backfill router attribution for one client / 为指定客户端全量回填 router 归因",
		Long: ui.Bi(`Backfill router attribution for one client.

Reads the client's configured router log in full and backfills attribution fields on all its historical messages.
Does not invoke client collectors, does not write collection_log/collection_errors, and does not advance the router cursor
(following engine.RunRouterBackfill semantics).

--client is required (inherited from the collect parent).
`, `为指定客户端全量回填 router 归因。

全表读取该客户端配置的 router 日志，回填其全部历史 messages 的归因字段。
不调用 client collector，不写 collection_log/collection_errors，不推进 router cursor
（沿用 engine.RunRouterBackfill 语义）。

--client 必填（继承自 collect 父命令）。
`),
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
		return fmt.Errorf("%s", ui.Bi("valid config must not be empty", "有效配置不能为空"))
	}
	if client == "" {
		return fmt.Errorf("%s", ui.Bi("no client given: collect router requires --client <name>", "未指定客户端：collect router 需要 --client <name>"))
	}
	cc, ok := cfg.ClientConfig(client)
	if !ok {
		return fmt.Errorf("%s: %s", ui.Bi("unknown client", "未知客户端"), client)
	}
	if !cc.Enabled {
		return fmt.Errorf("%s %s %s", ui.Bi("client", "客户端"), client, ui.Bi("is disabled; enable it in config first", "已禁用，请先在 config 中启用"))
	}
	if cc.Router == "" {
		return fmt.Errorf("%s %s %s", ui.Bi("client", "客户端"), client, ui.Bi("has no router configured", "未配置 router"))
	}
	return nil
}

// runCollectRouter 纯逻辑（便于测试注入 deps/db）。
// 全表读 router 日志回填指定 client 的全部历史 messages。
func runCollectRouter(ctx context.Context, deps *engine.Deps, usageDB *db.DB, log *slog.Logger, out io.Writer, client string) error {
	return engine.RunRouterBackfill(ctx, deps, usageDB, log, out, client)
}
