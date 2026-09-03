package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// RunCollect 采集主循环（公共函数）。
// collect 命令和守护进程的 collectFunc 共用此函数。
//
// client 路径（req.Source="" 或 "client"）：collector 读取 → (CLI 模式) router 外部读取 →
// 单事务写入（persistClientBatch）。
// router 路径（req.Source="router"）：不调用 client collector，只补 router 字段。
//
// recordError=false 仅用于重试失败时避免新增错误记录；成功后恢复历史错误的逻辑始终执行。
// out 非 nil 时输出控制台反馈（collect 命令）；nil 时静默（守护进程，仅 log 入文件）。
// skipCollected=true 时按 collection_log 过滤已采集 date（仅 CLI/手工重试 Dates 非空时生效）。
func RunCollect(ctx context.Context, deps *Deps, usageDB *db.DB, log *slog.Logger,
	out io.Writer, client string, req collector.CollectRequest,
	recordError bool, skipCollected bool) Result {
	result := Result{}
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	if deps == nil || deps.cfg == nil {
		result.Err = errors.New(ui.Bi("collect deps or config must not be empty", "采集依赖或配置不能为空"))
		return result
	}
	if usageDB == nil {
		result.Err = errors.New(ui.Bi("usage DB must not be empty", "usage DB 不能为空"))
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	// recordFailure 记录失败到传入的 *result（供 runRouterOnlyCollect 用独立 result）。
	// stage 参数为自带失败语义的双语短语（英文极短动词式 + 中文原文字词）。
	// 错误消息会进入 collection_errors.message，query 警告与 errors 表格均按
	// 50 显示宽度截断（中文每字占 2 列），英文段必须极短以保住失败原因可见。
	recordFailure := func(res *Result, source string, failedDates []string, stage string, cause error) {
		wrapped := fmt.Errorf("%s %s: %w", source, stage, cause)
		res.Err = errors.Join(res.Err, wrapped)
		log.Error("collection stage failed", "client", source, "stage", stage, "dates", failedDates, "error", cause)
		if recordError {
			if err := db.RecordErrorsByDate(ctx, usageDB, failedDates, source, wrapped.Error(), ""); err != nil {
				res.Err = errors.Join(res.Err, err)
			}
		}
	}
	fail := func(source string, failedDates []string, stage string, cause error) {
		recordFailure(&result, source, failedDates, stage, cause)
	}

	// Source=router 专用路径：前置条件 client 非空且 deps.RouterFor(client) 非 nil。
	if req.Source == collector.CollectSourceRouter {
		return runRouterOnlyCollect(ctx, deps, usageDB, log, out, client, req, recordFailure)
	}

	for _, c := range deps.collectors {
		if client != "" && c.Name() != client {
			continue
		}
		result.Matched = true
		clientCfg, ok := deps.cfg.ClientConfig(c.Name())
		if !ok || !clientCfg.Enabled {
			continue
		}
		result.Attempted++

		// 按本 collector 装配独立请求（不共享可变 Cursors map），完成去重/游标加载。
		creq, allCollected, err := requestForCollector(ctx, usageDB, c, req, skipCollected)
		if err != nil {
			fail(c.Name(), failureDates(req, nil), ui.Bi("setup", "装配请求失败"), err)
			continue
		}
		if allCollected {
			result.Succeeded++
			log.Info("already collected, skipping", "client", c.Name(), "dates", req.Dates)
			if out != nil {
				fmt.Fprintf(out, "✓ %s: %s\n", c.Name(),
					ui.Bi(fmt.Sprintf("all %d dates already collected, skipping", len(req.Dates)),
						fmt.Sprintf("已采集 %d 个日期，跳过", len(req.Dates))))
			}
			continue
		}

		// 每次采集每 client 必打的开始心跳，属预期行为，降 Debug 保留排查轨迹。
		log.Debug("collection started", "client", c.Name(), "dates", creq.Dates,
			"changed_file", creq.ChangedFile, "incremental", creq.Incremental)
		// catch-up 全扫路径注入跳过门（唯一作用域，且仅对支持的 client）；
		// 门表预取失败时降级为无门全读（门是优化不是依赖，不得阻断采集）。
		if req.ScanExistingJSONL && scanGateSupported(c.Name()) {
			records, gerr := db.GetFileScanLogs(ctx, usageDB, c.Name())
			if gerr != nil {
				log.Warn("scan gate prefetch failed, collecting without gate", "client", c.Name(), "error", gerr)
			} else {
				creq.SkipGate = newScanGate(records)
			}
		}
		collected, err := c.Collect(ctx, creq, log)
		if err != nil {
			// collector 因 ctx 取消返回错误时，取消不是采集故障：不持久化错误，直接返回。
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				result.Err = errors.Join(result.Err,
					fmt.Errorf("%s %s: %w", c.Name(), ui.Bi("collection canceled", "采集已取消"), err))
				return result
			}
			fail(c.Name(), failureDates(creq, collected.Messages), ui.Bi("read", "读取数据源失败"), err)
			continue
		}
		if ctx.Err() != nil {
			result.Err = errors.Join(result.Err,
				fmt.Errorf("%s %s: %w", c.Name(), ui.Bi("collection canceled", "采集已取消"), ctx.Err()))
			return result
		}

		// CLI 模式（有 Dates）先采 router 日志（事务外外部读取），失败降级不阻断。
		router := deps.RouterFor(c.Name())
		var routerResult collector.RouterCollectResult
		routerFetched := false
		if router != nil && len(creq.Dates) > 0 {
			routerResult, err = router.CollectLogs(ctx, collector.RouterCollectRequest{Dates: creq.Dates}, log)
			if err != nil {
				// router 因 ctx 取消失败时必须立即中止（与 client 读阶段同策略）。
				if ctx.Err() != nil {
					result.Err = errors.Join(result.Err,
						fmt.Errorf("%s %s: %w", c.Name(), ui.Bi("collection canceled", "采集已取消"), err))
					return result
				}
				log.Warn("router collection failed, keeping raw client data", "router", router.Name(), "error", err)
			} else {
				routerFetched = true
			}
		}

		// 写阶段前再次检查 ctx 取消（daemon 关闭期间）。
		if ctx.Err() != nil {
			result.Err = errors.Join(result.Err,
				fmt.Errorf("%s %s: %w", c.Name(), ui.Bi("collection canceled", "采集已取消"), ctx.Err()))
			return result
		}

		if err := persistClientBatch(ctx, usageDB, c.Name(), creq, collected,
			routerFetched, routerResult, router, log); err != nil {
			// persistClientBatch 失败后再检查 ctx：若事务因 ctx 取消而失败，
			// 取消不是采集故障，不调用 RecordErrorsByDate（与读阶段取消语义一致）。
			if ctx.Err() != nil {
				result.Err = errors.Join(result.Err,
					fmt.Errorf("%s %s: %w", c.Name(), ui.Bi("collection canceled", "采集已取消"), err))
				return result
			}
			if collected.PartialErr != nil {
				fail(c.Name(), failureDates(creq, collected.Messages), ui.Bi("partial read", "读取部分数据源失败"), collected.PartialErr)
			}
			fail(c.Name(), failureDates(creq, collected.Messages), ui.Bi("write", "写入事务失败"), err)
			continue
		}

		if collected.PartialErr != nil {
			// 成功部分已经同批事务落库；随后记录部分失败，使 startup coordinator/CLI
			// 得到非零结果且 collection_errors 可观察具体损坏文件。
			fail(c.Name(), failureDates(creq, collected.Messages), ui.Bi("partial read", "读取部分数据源失败"), collected.PartialErr)
			if out != nil {
				fmt.Fprintf(out, "⚠ %s: %s\n", c.Name(),
					ui.Bi("saved the successful part, but some data sources failed to read",
						"已保存成功部分，但部分数据源读取失败"))
			}
			continue
		}

		result.Succeeded++
		msgCount := len(collected.Messages)
		// 成功采集的完成心跳与「开始采集」成对，同降 Debug。
		log.Debug("collection completed", "client", c.Name(), "messages", msgCount)
		// 门命中跳过的心跳（预期行为，Debug 级；0 不打）。与注入/写门同白名单，
		// 保持三处条件形态一致。
		if req.ScanExistingJSONL && scanGateSupported(c.Name()) {
			if n := countGateSkipped(collected.FileStatuses); n > 0 {
				log.Debug("scan gate skipped files", "client", c.Name(), "skipped", n)
			}
		}
		if out != nil {
			fmt.Fprintf(out, "✓ %s: %s\n", c.Name(),
				ui.Bi(fmt.Sprintf("collected %d messages/API requests", msgCount),
					fmt.Sprintf("采集 %d 条消息/API 请求", msgCount)))
		}
	}
	return result
}

// requestForCollector 按本 collector 装配独立 CollectRequest。
// 每次调用复制一份 base（不共享可变 Cursors map）；skipCollected 只在 Dates 非空时过滤。
// 返回 (req, allCollected, err)：allCollected=true 表示所有 date 已采集，调用方跳过但仍计 Succeeded。
func requestForCollector(
	ctx context.Context,
	usageDB *db.DB,
	c collector.Collector,
	base collector.CollectRequest,
	skipCollected bool,
) (collector.CollectRequest, bool, error) {
	req := base
	req.Dates = append([]string(nil), base.Dates...)
	if skipCollected && len(req.Dates) > 0 {
		collectedDates, err := db.GetCollectedDatesContext(ctx, usageDB, c.Name())
		if err != nil {
			return req, false, err
		}
		req.Dates = filterUncollected(req.Dates, collectedDates)
		if len(req.Dates) == 0 {
			return req, true, nil
		}
	}
	if !base.Incremental {
		req.Cursors = nil
		return req, false, nil
	}
	cursors, err := db.GetSyncCursors(ctx, usageDB, c.Name(), c.SyncSources())
	if err != nil {
		return req, false, err
	}
	req.Cursors = cursors
	return req, false, nil
}

// persistClientBatch 是单次 client batch 的事务边界。defer Rollback 只存在该 helper 内。
// 顺序：UpsertMessages → UpsertSessionMeta → UpsertRawRouterLogs（若有）→
// （router 消息回填）→ 完整成功时写完成状态与 cursor → Commit。
//
// collected.PartialErr != nil 时仍保存成功解析的数据，但不写 collection_log、不解决旧错误、
// 不推进 cursor。这样下次普通采集或 retry 会幂等重放仍有缺口的区间，而不会被完成状态跳过。
func persistClientBatch(
	ctx context.Context,
	usageDB *db.DB,
	client string,
	req collector.CollectRequest,
	collected collector.CollectResult,
	routerFetched bool,
	routerResult collector.RouterCollectResult,
	router collector.RouterAdapter,
	log *slog.Logger,
) error {
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("open write transaction", "开启写事务"), err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(collected.Messages) > 0 {
		if _, err := db.UpsertMessages(ctx, tx, collected.Messages); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("save messages", "保存 messages"), err)
		}
	}
	if len(collected.Sessions) > 0 {
		if _, err := db.UpsertSessionMeta(ctx, tx, collected.Sessions); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("save session metadata", "保存 session metadata"), err)
		}
	}
	if routerFetched && len(routerResult.Logs) > 0 {
		if _, err := db.UpsertRawRouterLogs(ctx, tx, routerResult.Logs); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("save router logs", "保存 router 日志"), err)
		}
	}

	// router 回填：client 支持 router 归因（Claude 系与 Codex，registry 单一真相源）
	// 且本轮有 messages 即执行。claude 系走 MessageID 路径，codex 走 session+时间窗
	// 路径（双侧全量：不区分入口来源，一律查 session 集合的全量 proxy 行与全量
	// messages——跨日交错下任一后续触达该 session 的采集轮都会以完整集合重算，
	// 经非空覆盖语义补上，不依赖全量 backfill）。
	// 查询对象是已入库的 raw_router_logs 表（不依赖本轮是否拉取 router 日志）：
	// CLI Dates 模式先 Upsert 本轮日志再查表（routerFetched=true，含本轮新日志）；
	// daemon 增量轮（Dates 恒空、routerFetched=false）借此覆盖「router 日志先入库、
	// message 后入库」的交错——该交错下 router 增量轮的 UPDATE 因 message 尚未入库而
	// 落空，且 cursor 已推过不再重读。
	// ClientSupportsRouter 门控维持「存量非 router client 配置只写 raw 日志、不回填
	// messages」的兼容合同（docs/architecture.md）：这类配置 RouterFor 仍返回
	// adapter，若放行回填，本轮消息 ID 与既有 Claude router 日志碰撞时会经 app_type
	// 映射误更新 Claude message。
	// backfilled 推迟到事务提交成功后才用于日志记录：后续写入或 Commit 失败会连同
	// 归因一起回滚，提前记录会留下回填成功的假轨迹。
	var backfilled int
	if router != nil && runtimecfg.ClientSupportsRouter(client) && len(collected.Messages) > 0 {
		if routerAttributionBySession(client) {
			sessionIDs := uniqueCodexSessionIDs(collected.Messages)
			if len(sessionIDs) > 0 {
				n, err := backfillCodexSessionAttributions(ctx, tx, router.Name(), sessionIDs)
				if err != nil {
					return fmt.Errorf("%s: %w", ui.Bi("backfill router attribution", "回填 router 归因"), err)
				}
				backfilled = n
			}
		} else {
			messageIDs := uniqueMessageIDs(collected.Messages)
			if len(messageIDs) > 0 {
				infos, err := db.QueryRouterLogsByMessageIDs(ctx, tx, router.Name(), messageIDs)
				if err != nil {
					return fmt.Errorf("%s: %w", ui.Bi("query router attribution", "查询 router 归因"), err)
				}
				if len(infos) > 0 {
					n, err := db.BackfillRouterFields(ctx, tx, infos)
					if err != nil {
						return fmt.Errorf("%s: %w", ui.Bi("backfill router attribution", "回填 router 归因"), err)
					}
					backfilled = n
				}
			}
		}
	}

	if collected.PartialErr == nil {
		counts := messageCounts(collected.Messages)
		for _, date := range datesToMark(req, counts) {
			if err := db.MarkCollected(ctx, tx, date, client, counts[date]); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("update collection_log", "更新 collection_log"), err)
			}
			if _, err := db.ResolveErrorsByDateSource(ctx, tx, date, client); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("resolve historical errors", "恢复历史错误状态"), err)
			}
		}
		if req.Incremental && len(collected.NextCursors) > 0 {
			if err := db.SetSyncCursors(ctx, tx, client, collected.NextCursors); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("save incremental cursors", "保存增量游标"), err)
			}
		}
	}
	// 跳过门记录与消息同事务提交（仅 catch-up 全扫路径且 client 受门支持，
	// 与注入侧同一白名单）：文件级判定，含坏行/尾行未完成/快照不一致的文件
	// 不写门，批次 PartialErr 不拖累同批好文件。
	if req.ScanExistingJSONL && scanGateSupported(client) {
		if rows := scanGateRowsFor(client, collected.FileStatuses); len(rows) > 0 {
			if err := db.UpsertFileScanLog(ctx, tx, rows); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("save file scan log", "保存文件扫描状态"), err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("commit write transaction", "提交写事务"), err)
	}
	// 回填是预期行为：命中才记 Debug 排查轨迹，与采集完成心跳同级；0 条不打。
	if backfilled > 0 {
		log.Debug("router attribution backfilled", "client", client, "count", backfilled)
	}
	return nil
}

// runRouterOnlyCollect 处理 Source=router 专用路径：
// 不调用任何 client collector，只补 router 字段。同事务内 UpsertRawRouterLogs →
// 归因回填（claude 系按非空 MessageID 查 attribution；codex 走 session+时间窗
// 双侧全量路径）→ SetSyncCursors。不写 collection_log，不改 messages token 字段。
func runRouterOnlyCollect(
	ctx context.Context,
	deps *Deps,
	usageDB *db.DB,
	log *slog.Logger,
	out io.Writer,
	client string,
	req collector.CollectRequest,
	recordFailure func(res *Result, source string, failedDates []string, stage string, cause error),
) (result Result) {
	result = Result{Matched: hasCollector(deps, client)}
	if client == "" {
		return result
	}
	router := deps.RouterFor(client)
	if router == nil {
		return result
	}
	clientCfg, ok := deps.cfg.ClientConfig(client)
	if !ok || !clientCfg.Enabled {
		return result
	}
	result.Attempted++

	// 读取 router cursor（事务外）
	cursors, err := db.GetSyncCursors(ctx, usageDB, client, []string{router.SyncSource()})
	if err != nil {
		recordFailure(&result, client, failureDates(req, nil), ui.Bi("read cursor", "读取 router 游标失败"), err)
		return result
	}
	routerReq := collector.RouterCollectRequest{
		Incremental: true, // router 路径始终增量（按 cursor 推进）
		Cursor:      cursors[router.SyncSource()],
	}
	routerResult, err := router.CollectLogs(ctx, routerReq, log)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			result.Err = errors.Join(result.Err,
				fmt.Errorf("%s %s: %w", client, ui.Bi("router collection canceled", "router 采集已取消"), err))
			return result
		}
		recordFailure(&result, client, failureDates(req, nil), ui.Bi("read logs", "读取 router 日志失败"), err)
		return result
	}
	if ctx.Err() != nil {
		result.Err = errors.Join(result.Err,
			fmt.Errorf("%s %s: %w", client, ui.Bi("router collection canceled", "router 采集已取消"), ctx.Err()))
		return result
	}

	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		recordFailure(&result, client, failureDates(req, nil), ui.Bi("open write", "开启写事务失败"), err)
		return result
	}

	// txErr 记录事务内首个失败；defer 先回滚再 recordFailure，
	// 避免 RecordErrorsByDate 在 tx 仍持锁时开新事务造成死锁。
	var txErr error
	defer func() {
		_ = tx.Rollback()
		if txErr != nil {
			if ctx.Err() != nil {
				result.Err = errors.Join(result.Err,
					fmt.Errorf("%s %s: %w", client, ui.Bi("router collection canceled", "router 采集已取消"), txErr))
				return
			}
			recordFailure(&result, client, failureDates(req, nil), ui.Bi("router write", "router 写入事务失败"), txErr)
		}
	}()

	if len(routerResult.Logs) > 0 {
		if _, err := db.UpsertRawRouterLogs(ctx, tx, routerResult.Logs); err != nil {
			txErr = fmt.Errorf("%s: %w", ui.Bi("save router logs", "保存 router 日志"), err)
			return result
		}
	}

	// 归因回填。仅对支持 router 归因的 client（Claude 系与 Codex）执行：
	// cc-switch 日志按 app_type 混存同一 db，存量非 router client 配置的 router 轮
	// 也会读到 Claude 类型日志（message_id 非空），照日志自身 ID 回填会经 app_type
	// 映射直接更新 Claude messages——跨 client 写入违背「legacy 配置只写 raw、
	// 不回填」合同（docs/architecture.md）。
	// claude 系走 MessageID 路径；codex 走 session+时间窗路径且**不做** claude 日志的
	// message_id 提取回填（跨 client 防御），session 集合只取本轮 codex proxy 行
	//（DataSource=='proxy'，codex_session 同步行不进集合），双侧全量匹配。
	// 日志 Upsert 与 cursor 推进不受此门控影响。
	if runtimecfg.ClientSupportsRouter(client) {
		if routerAttributionBySession(client) {
			sessionIDs := uniqueCodexProxySessionIDs(routerResult.Logs)
			if len(sessionIDs) > 0 {
				if _, err := backfillCodexSessionAttributions(ctx, tx, router.Name(), sessionIDs); err != nil {
					txErr = fmt.Errorf("%s: %w", ui.Bi("backfill router attribution", "回填 router 归因"), err)
					return result
				}
			}
		} else {
			messageIDs := uniqueRouterMessageIDs(routerResult.Logs)
			if len(messageIDs) > 0 {
				infos, qerr := db.QueryRouterLogsByMessageIDs(ctx, tx, router.Name(), messageIDs)
				if qerr != nil {
					txErr = fmt.Errorf("%s: %w", ui.Bi("query router attribution", "查询 router 归因"), qerr)
					return result
				}
				if len(infos) > 0 {
					if _, err := db.BackfillRouterFields(ctx, tx, infos); err != nil {
						txErr = fmt.Errorf("%s: %w", ui.Bi("backfill router attribution", "回填 router 归因"), err)
						return result
					}
				}
			}
		}
	}

	if routerResult.NextCursor != (model.SyncCursor{}) {
		if err := db.SetSyncCursors(ctx, tx, client, map[string]model.SyncCursor{
			router.SyncSource(): routerResult.NextCursor,
		}); err != nil {
			txErr = fmt.Errorf("%s: %w", ui.Bi("save router cursor", "保存 router 游标"), err)
			return result
		}
	}

	if err := tx.Commit(); err != nil {
		txErr = fmt.Errorf("%s: %w", ui.Bi("commit write transaction", "提交写事务"), err)
		return result
	}

	result.Succeeded++
	// router 轮完成心跳与 client 路径「采集完成」同级别语义，同降 Debug。
	log.Debug("router collection completed", "client", client, "logs", len(routerResult.Logs))
	if out != nil {
		fmt.Fprintf(out, "✓ %s: %s\n", client,
			ui.Bi(fmt.Sprintf("router backfilled %d attributions", len(routerResult.Logs)),
				fmt.Sprintf("router 回填 %d 条归因", len(routerResult.Logs))))
	}
	return result
}

// uniqueMessageIDs 返回去重后的 message ID 列表（保留首次出现顺序）。
func uniqueMessageIDs(messages []model.Message) []string {
	seen := make(map[string]struct{}, len(messages))
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.ID == "" {
			continue
		}
		if _, ok := seen[m.ID]; ok {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, m.ID)
	}
	return out
}

// routerAttributionBySession 报告该 client 的 router 归因是否走 codex 的
// session+时间窗路径（与 claude 系 MessageID 路径分派）。
func routerAttributionBySession(client string) bool {
	return client == "codex"
}

// uniqueCodexSessionIDs 从 codex messages 提取去重非空 session 集合并归一
// （剥 codex_ 前缀，幂等——rollout UUID 本为裸形态，双形态源亦安全）。
func uniqueCodexSessionIDs(messages []model.Message) []string {
	seen := make(map[string]struct{}, len(messages))
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.SessionID == "" {
			continue
		}
		sid := db.NormalizeCodexRouterSessionID(m.SessionID)
		if sid == "" {
			continue
		}
		if _, ok := seen[sid]; ok {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out
}

// uniqueCodexProxySessionIDs 从 router logs 提取 app_type='codex' 且
// DataSource=='proxy' 的行，session 剥 codex_ 前缀归一去重。
// codex_session 同步行（无路由价值）不进集合。
func uniqueCodexProxySessionIDs(logs []model.RouterLog) []string {
	seen := make(map[string]struct{}, len(logs))
	out := make([]string, 0, len(logs))
	for _, l := range logs {
		if l.AppType != "codex" || l.DataSource != "proxy" {
			continue
		}
		sid := db.NormalizeCodexRouterSessionID(l.SessionID)
		if sid == "" {
			continue
		}
		if _, ok := seen[sid]; ok {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out
}

// backfillCodexSessionAttributions 执行 codex session 路径归因（三入口统一模式，
// 调用方事务内）：session 集合 → 双侧全量查询（proxy 行 + codex messages）→
// 时间窗最近邻匹配 → 回填。返回回填行数。
func backfillCodexSessionAttributions(ctx context.Context, tx *sql.Tx, routerName string, sessionIDs []string) (int, error) {
	logs, err := db.QueryCodexRouterLogsBySessions(ctx, tx, routerName, sessionIDs)
	if err != nil {
		return 0, err
	}
	messages, err := db.QueryCodexMessagesBySessions(ctx, tx, sessionIDs)
	if err != nil {
		return 0, err
	}
	infos := db.MatchCodexRouterAttributions(logs, messages, db.CodexRouterMatchWindowSec)
	if len(infos) == 0 {
		return 0, nil
	}
	return db.BackfillRouterFields(ctx, tx, infos)
}

// uniqueRouterMessageIDs 从 router logs 提取去重的非空 message_id。
func uniqueRouterMessageIDs(logs []model.RouterLog) []string {
	seen := make(map[string]struct{}, len(logs))
	out := make([]string, 0, len(logs))
	for _, l := range logs {
		if l.MessageID == "" {
			continue
		}
		if _, ok := seen[l.MessageID]; ok {
			continue
		}
		seen[l.MessageID] = struct{}{}
		out = append(out, l.MessageID)
	}
	return out
}

// messageCounts 按 (client,id,date) 去重统计每个 date 的消息数。
func messageCounts(messages []model.Message) map[string]int {
	seen := map[string]struct{}{}
	counts := map[string]int{}
	for _, message := range messages {
		if message.ID == "" || message.Date == "" {
			continue
		}
		key := message.Client + "\x00" + message.ID + "\x00" + message.Date
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		counts[message.Date]++
	}
	return counts
}

// datesToMark 合并 req.Dates 与实际消息日期（去重排序）。
// CLI 因 Dates 非空会标记请求日期和实际日期；daemon 只标记 actual dates。
func datesToMark(req collector.CollectRequest, counts map[string]int) []string {
	set := make(map[string]struct{}, len(req.Dates)+len(counts))
	if len(req.Dates) > 0 {
		for _, date := range req.Dates {
			if date == "" {
				continue
			}
			set[date] = struct{}{}
		}
	}
	for date := range counts {
		if date == "" {
			continue
		}
		set[date] = struct{}{}
	}
	dates := make([]string, 0, len(set))
	for date := range set {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	return dates
}

// failureDates 决定失败记录用的 dates：
// Dates 非空用 Dates；已有 Messages 用实际 Message.Date；两者都空用今天。
func failureDates(req collector.CollectRequest, messages []model.Message) []string {
	if len(req.Dates) > 0 {
		return append([]string(nil), req.Dates...)
	}
	if len(messages) > 0 {
		set := make(map[string]struct{}, len(messages))
		var out []string
		for _, m := range messages {
			if m.Date == "" {
				continue
			}
			if _, ok := set[m.Date]; ok {
				continue
			}
			set[m.Date] = struct{}{}
			out = append(out, m.Date)
		}
		sort.Strings(out)
		if len(out) > 0 {
			return out
		}
	}
	return []string{time.Now().Format("2006-01-02")}
}

// filterUncollected 返回 dates 中不在 collected 集合内的日期，保留原顺序。
// collected 为空时直接返回 dates（避免无谓分配）。
func filterUncollected(dates, collected []string) []string {
	if len(collected) == 0 {
		return dates
	}
	set := make(map[string]struct{}, len(collected))
	for _, d := range collected {
		set[d] = struct{}{}
	}
	out := make([]string, 0, len(dates))
	for _, d := range dates {
		if _, ok := set[d]; !ok {
			out = append(out, d)
		}
	}
	return out
}
