package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// newCollectAllCmd 构造 `collect all` 子命令：两阶段全采。
//
//	阶段 A：对 selectedClients 逐个执行全历史 messages 采集（单 client 失败不阻断其他）。
//	阶段 B：所有 client 的 messages 阶段尝试完后，对配置了 router 的 client 执行 RunRouterBackfill。
//	阶段 C：统一汇总 A/B 两阶段失败，按 client+stage 稳定排序；任一失败则非零退出。
//
// --client 经 PersistentFlag 继承；--force 不继承（NoArgs 拒绝位置参数）。
func newCollectAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "全量扫描历史消息并回填 router 归因",
		Long: `全量扫描所有已启用客户端的历史消息，并对配置了 router 的客户端做全量归因回填。

两阶段编排：
  1. messages 阶段：逐个 client 全量采集（Dates=nil 触发 collector 全扫）；
     单 client 失败不阻断其他 client。
  2. router 阶段：所有 client 的 messages 阶段尝试完后，对配置了 router 的 client
     逐个执行 RunRouterBackfill。messages 阶段失败的 client 仍会尝试 router
     （数据库无 messages 时自然回填 0 条）。

任一阶段有失败则退出非零，已完成数据不回滚。

--client X 限定单 client 全采（继承自 collect 父命令）。
`,
		Args: cobra.NoArgs,
		RunE: runCollectAllCmd,
	}
}

func runCollectAllCmd(cmd *cobra.Command, args []string) error {
	client, _ := cmd.Flags().GetString("client")

	cfg, err := loadCollectConfig(false)
	if err != nil {
		return err
	}

	// 确定目标 client 列表。
	var selected []string
	if client != "" {
		if err := validateClientExists(cfg, client); err != nil {
			return err
		}
		selected = []string{client}
	} else {
		selected = enabledClientNames(cfg)
		if len(selected) == 0 {
			return fmt.Errorf("没有已启用的客户端，未执行采集")
		}
	}

	log, usageDB, cleanup, err := openCollectRuntime(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	deps := newDepsFactory(cfg)
	return runCollectAll(cmdContext(cmd), deps, cfg, usageDB, log, cmd.OutOrStdout(), selected)
}

// runCollectAll 两阶段编排的纯逻辑（便于测试注入 deps/db/cfg）。
//
// 失败汇总按 client+stage 稳定排序：先按 client 名，再按 stage（messages/router）。
// 任一阶段失败返回非 nil error；已完成数据不回滚。
func runCollectAll(ctx context.Context, deps *engine.Deps, cfg *config.Config, usageDB *db.DB, log *slog.Logger, out io.Writer, selected []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("有效配置不能为空")
	}
	if len(selected) == 0 {
		return fmt.Errorf("没有已启用的客户端，未执行采集")
	}

	// 稳定排序：对输入也排一次，保证顺序与配置表书写顺序无关。
	clients := append([]string(nil), selected...)
	sort.Strings(clients)

	// stageFailure 收集 (client, stage, err)，最后统一汇总。
	type stageFail struct {
		client string
		stage  string // "messages" / "router"
		err    error
	}
	var failures []stageFail

	// 阶段 A：messages 全采。
	for _, c := range clients {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := runOneFullCollect(ctx, deps, usageDB, log, out, c); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures = append(failures, stageFail{client: c, stage: "messages", err: err})
		}
	}

	// 阶段 B：router 全量回填。对所有 selected 中配置声明了 router 的 client 执行；
	// 该 client 的 messages 失败也不跳过（数据库无 messages 时回填 0 条）。
	// 判定基准为配置层 cc.Router != ""：即使 adapter 装配失败（路径无效等）也进入本阶段，
	// 由 RunRouterBackfill 内部 deps.RouterFor 返回 nil 时报错，计入 router 阶段失败汇总。
	for _, c := range clients {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !clientHasRouter(cfg, c) {
			continue
		}
		if err := engine.RunRouterBackfill(ctx, deps, usageDB, log, out, c); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures = append(failures, stageFail{client: c, stage: "router", err: err})
		}
	}

	if len(failures) > 0 {
		// 稳定排序：先 client，再 stage。
		sort.SliceStable(failures, func(i, j int) bool {
			if failures[i].client != failures[j].client {
				return failures[i].client < failures[j].client
			}
			return failures[i].stage < failures[j].stage
		})
		var parts []string
		for _, f := range failures {
			parts = append(parts, fmt.Sprintf("%s/%s: %v", f.client, f.stage, f.err))
		}
		return fmt.Errorf("采集失败汇总:\n  %s", strings.Join(parts, "\n  "))
	}
	return nil
}

// clientHasRouter 判断该 client 在配置层（cc.Router）声明了 router。
// 仅看配置声明、不看 adapter 是否装配成功：
//   - 未配置 router（cc.Router == ""）→ 跳过阶段 B，不算错误。
//   - 配置了 router 但 adapter 装配失败（路径无效等）→ 仍进入阶段 B，
//     由 RunRouterBackfill 内部 deps.RouterFor 返回 nil 时返回 error，计入 router 阶段失败汇总。
func clientHasRouter(cfg *config.Config, client string) bool {
	if cfg == nil {
		return false
	}
	cc, ok := cfg.ClientConfig(client)
	if !ok {
		return false
	}
	return cc.Router != ""
}
