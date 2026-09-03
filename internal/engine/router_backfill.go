package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// RunRouterBackfill 全量回填指定 client 的 router 归因到所有历史 messages。
//
// 流程：router.CollectLogs(Dates=nil) 全表读 router 日志 →
// 单事务（UpsertRawRouterLogs → 归因回填：claude 系按显示名查全部 messageIDs
// 再按 MessageID 匹配；codex 走 session+时间窗双侧全量路径 → BackfillRouterFields）→ Commit。
//
// 关键决策：
//   - 不写 collection_log（router backfill 无日期概念）
//   - 不写 collection_errors（失败只返回 error，用户看 CLI 退出码 + stderr）
//   - 不更新 sync cursor（避免影响 daemon 增量；daemon 下次增量会幂等重扫已 backfill 数据）
//   - client 参数是配置 key（如 "claude"），内部经 ClientToDisplayNames 转显示名列表
//
// 错误处理：
//   - router 未配置（deps.RouterFor 返回 nil）→ 返回 error
//   - 配置 key 不在 ClientToDisplayNames → 返回 error（I-v4-1：不静默返回空）
//   - ctx 取消 → 返回 ctx.Err，已读数据不 persist
func RunRouterBackfill(ctx context.Context, deps *Deps, usageDB *db.DB, log *slog.Logger, out io.Writer, client string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deps == nil || deps.cfg == nil {
		return errors.New(ui.Bi("collect deps or config must not be empty", "采集依赖或配置不能为空"))
	}
	if usageDB == nil {
		return errors.New(ui.Bi("usage DB must not be empty", "usage DB 不能为空"))
	}
	if log == nil {
		log = slog.Default()
	}
	router := deps.RouterFor(client)
	if router == nil {
		return errors.New(ui.Bi(
			fmt.Sprintf("client %s has no router configured", client),
			fmt.Sprintf("客户端 %s 未配置 router", client)))
	}

	// 配置 key → 显示名列表（C2 修复 + I-v4-1 错误处理）
	displayNames, ok := model.ClientToDisplayNames[client]
	if !ok {
		return errors.New(ui.Bi(
			fmt.Sprintf("config key for client %s is not registered in model.ClientToDisplayNames"+
				" (update ClientToDisplayNames when adding a new client)", client),
			fmt.Sprintf("客户端 %s 的配置 key 未登记到 model.ClientToDisplayNames"+
				"（新增 client 时需同步更新 ClientToDisplayNames）", client)))
	}

	// 全表读 router 日志（Incremental=false，Dates=nil 触发 ccswitch.go:208-209 全表分支）
	routerResult, err := router.CollectLogs(ctx, collector.RouterCollectRequest{}, log)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s %s: %w", client, ui.Bi("router backfill canceled", "router backfill 已取消"), err)
		}
		return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to read router logs", "读取 router 日志失败"), err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s: %w", client, ui.Bi("router backfill canceled", "router backfill 已取消"), ctx.Err())
	}

	// 单事务：UpsertRawRouterLogs → 查归因 → BackfillRouterFields
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to open write transaction", "开启写事务失败"), err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(routerResult.Logs) > 0 {
		if _, err := db.UpsertRawRouterLogs(ctx, tx, routerResult.Logs); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%s %s: %w", client, ui.Bi("router backfill canceled", "router backfill 已取消"), err)
			}
			return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to save router logs", "保存 router 日志失败"), err)
		}
	}

	// 归因回填仅对支持 router 归因的 client（Claude 系与 Codex）执行：存量非
	// router client 的 router 配置按兼容合同只写 raw 日志、不回填 messages——其
	// 消息 ID 与既有 Claude router 日志同 ID 时若照常回填，会经 app_type 映射
	// 跨 client 改写 Claude message 的归因。raw 日志读取与 Upsert 不受此门控
	// 影响；0 条回填由下方输出如实反映。
	// claude 系走 MessageID 路径；codex 走 session+时间窗路径（session 集合 =
	// 全量日志中的 codex proxy 行，codex_session 行不进集合）。
	var n int
	if runtimecfg.ClientSupportsRouter(client) {
		if routerAttributionBySession(client) {
			sessionIDs := uniqueCodexProxySessionIDs(routerResult.Logs)
			if len(sessionIDs) > 0 {
				n, err = backfillCodexSessionAttributions(ctx, tx, router.Name(), sessionIDs)
				if err != nil {
					return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to backfill router attribution", "回填 router 归因失败"), err)
				}
			}
		} else {
			// 按显示名查该 client 所有历史 messageIDs
			messageIDs, err := db.GetMessageIDsByDisplayNames(ctx, tx, displayNames)
			if err != nil {
				return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to query message ids", "查询 message ids 失败"), err)
			}

			// 复用现有 DAO（已内置 500 分块、app_type 过滤、routerAppTypeToClient 映射、首条优先）
			if len(messageIDs) > 0 {
				infos, err := db.QueryRouterLogsByMessageIDs(ctx, tx, router.Name(), messageIDs)
				if err != nil {
					return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to query router attribution", "查询 router 归因失败"), err)
				}
				if len(infos) > 0 {
					n, err = db.BackfillRouterFields(ctx, tx, infos)
					if err != nil {
						return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to backfill router attribution", "回填 router 归因失败"), err)
					}
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s %s: %w", client, ui.Bi("failed to commit transaction", "提交事务失败"), err)
	}

	log.Info("router backfill completed", "client", client, "messages_backfilled", n)
	if out != nil {
		fmt.Fprintf(out, "✓ %s: %s\n", client,
			ui.Bi(fmt.Sprintf("router backfilled %d attributions", n),
				fmt.Sprintf("router 回填 %d 条归因", n)))
	}
	return nil
}
