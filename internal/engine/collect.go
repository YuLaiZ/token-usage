package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
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
		result.Err = errors.New("采集依赖或配置不能为空")
		return result
	}
	if usageDB == nil {
		result.Err = errors.New("usage DB 不能为空")
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	// recordFailure 记录失败到传入的 *result（供 runRouterOnlyCollect 用独立 result）。
	recordFailure := func(res *Result, source string, failedDates []string, stage string, cause error) {
		wrapped := fmt.Errorf("%s %s 失败: %w", source, stage, cause)
		res.Err = errors.Join(res.Err, wrapped)
		log.Error("采集阶段失败", "client", source, "stage", stage, "dates", failedDates, "error", cause)
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
			fail(c.Name(), failureDates(req, nil), "装配请求", err)
			continue
		}
		if allCollected {
			result.Succeeded++
			log.Info("已采集，跳过", "client", c.Name(), "dates", req.Dates)
			if out != nil {
				fmt.Fprintf(out, "✓ %s: 已采集 %d 个日期，跳过\n", c.Name(), len(req.Dates))
			}
			continue
		}

		log.Info("开始采集", "client", c.Name(), "dates", creq.Dates,
			"changed_file", creq.ChangedFile, "incremental", creq.Incremental)
		collected, err := c.Collect(ctx, creq, log)
		if err != nil {
			// collector 因 ctx 取消返回错误时，取消不是采集故障：不持久化错误，直接返回。
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				result.Err = errors.Join(result.Err,
					fmt.Errorf("%s 采集已取消: %w", c.Name(), err))
				return result
			}
			fail(c.Name(), failureDates(creq, collected.Messages), "读取数据源", err)
			continue
		}
		if ctx.Err() != nil {
			result.Err = errors.Join(result.Err,
				fmt.Errorf("%s 采集已取消: %w", c.Name(), ctx.Err()))
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
						fmt.Errorf("%s 采集已取消: %w", c.Name(), err))
					return result
				}
				log.Warn("采集 router 数据失败，保留客户端原始数据", "router", router.Name(), "error", err)
			} else {
				routerFetched = true
			}
		}

		// 写阶段前再次检查 ctx 取消（daemon 关闭期间）。
		if ctx.Err() != nil {
			result.Err = errors.Join(result.Err,
				fmt.Errorf("%s 采集已取消: %w", c.Name(), ctx.Err()))
			return result
		}

		providerAliases := deps.cfg.ProviderAliases
		if err := persistClientBatch(ctx, usageDB, c.Name(), creq, collected,
			routerFetched, routerResult, providerAliases, router, log); err != nil {
			// persistClientBatch 失败后再检查 ctx：若事务因 ctx 取消而失败，
			// 取消不是采集故障，不调用 RecordErrorsByDate（与读阶段取消语义一致）。
			if ctx.Err() != nil {
				result.Err = errors.Join(result.Err,
					fmt.Errorf("%s 采集已取消: %w", c.Name(), err))
				return result
			}
			if collected.PartialErr != nil {
				fail(c.Name(), failureDates(creq, collected.Messages), "读取部分数据源", collected.PartialErr)
			}
			fail(c.Name(), failureDates(creq, collected.Messages), "写入事务", err)
			continue
		}

		if collected.PartialErr != nil {
			// 成功部分已经同批事务落库；随后记录部分失败，使 startup coordinator/CLI
			// 得到非零结果且 collection_errors 可观察具体损坏文件。
			fail(c.Name(), failureDates(creq, collected.Messages), "读取部分数据源", collected.PartialErr)
			if out != nil {
				fmt.Fprintf(out, "⚠ %s: 已保存成功部分，但部分数据源读取失败\n", c.Name())
			}
			continue
		}

		result.Succeeded++
		msgCount := len(collected.Messages)
		log.Info("采集完成", "client", c.Name(), "messages", msgCount)
		if out != nil {
			fmt.Fprintf(out, "✓ %s: 采集 %d 条消息/API 请求\n", c.Name(), msgCount)
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
	providerAliases map[string]string,
	router collector.RouterAdapter,
	log *slog.Logger,
) error {
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启写事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(collected.Messages) > 0 {
		if _, err := db.UpsertMessages(ctx, tx, collected.Messages); err != nil {
			return fmt.Errorf("保存 messages: %w", err)
		}
	}
	if len(collected.Sessions) > 0 {
		if _, err := db.UpsertSessionMeta(ctx, tx, collected.Sessions); err != nil {
			return fmt.Errorf("保存 session metadata: %w", err)
		}
	}
	if routerFetched && len(routerResult.Logs) > 0 {
		if _, err := db.UpsertRawRouterLogs(ctx, tx, routerResult.Logs); err != nil {
			return fmt.Errorf("保存 router 日志: %w", err)
		}
	}

	// router 回填：仅在有 messages 且 router 已成功时执行。
	if router != nil && routerFetched && len(collected.Messages) > 0 {
		messageIDs := uniqueMessageIDs(collected.Messages)
		if len(messageIDs) > 0 {
			infos, err := db.QueryRouterLogsByMessageIDs(ctx, tx, router.Name(), messageIDs)
			if err != nil {
				return fmt.Errorf("查询 router 归因: %w", err)
			}
			for i := range infos {
				if alias, ok := providerAliases[infos[i].Provider]; ok {
					infos[i].Provider = alias
				}
			}
			if len(infos) > 0 {
				if _, err := db.BackfillRouterFields(ctx, tx, infos); err != nil {
					return fmt.Errorf("回填 router 归因: %w", err)
				}
			}
		}
	}

	if collected.PartialErr == nil {
		counts := messageCounts(collected.Messages)
		for _, date := range datesToMark(req, counts) {
			if err := db.MarkCollected(ctx, tx, date, client, counts[date]); err != nil {
				return fmt.Errorf("更新 collection_log: %w", err)
			}
			if _, err := db.ResolveErrorsByDateSource(ctx, tx, date, client); err != nil {
				return fmt.Errorf("恢复历史错误状态: %w", err)
			}
		}
		if req.Incremental && len(collected.NextCursors) > 0 {
			if err := db.SetSyncCursors(ctx, tx, client, collected.NextCursors); err != nil {
				return fmt.Errorf("保存增量游标: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交写事务: %w", err)
	}
	return nil
}

// runRouterOnlyCollect 处理 Source=router 专用路径：
// 不调用任何 client collector，只补 router 字段。同事务内 UpsertRawRouterLogs →
// 按非空 MessageID 查 attribution → 应用 alias → BackfillRouterFields → SetSyncCursors。
// 不写 collection_log，不改 messages token 字段。
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
		recordFailure(&result, client, failureDates(req, nil), "读取 router 游标", err)
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
				fmt.Errorf("%s router 采集已取消: %w", client, err))
			return result
		}
		recordFailure(&result, client, failureDates(req, nil), "读取 router 日志", err)
		return result
	}
	if ctx.Err() != nil {
		result.Err = errors.Join(result.Err,
			fmt.Errorf("%s router 采集已取消: %w", client, ctx.Err()))
		return result
	}

	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		recordFailure(&result, client, failureDates(req, nil), "开启写事务", err)
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
					fmt.Errorf("%s router 采集已取消: %w", client, txErr))
				return
			}
			recordFailure(&result, client, failureDates(req, nil), "router 写入事务", txErr)
		}
	}()

	if len(routerResult.Logs) > 0 {
		if _, err := db.UpsertRawRouterLogs(ctx, tx, routerResult.Logs); err != nil {
			txErr = fmt.Errorf("保存 router 日志: %w", err)
			return result
		}
	}

	// 收集非空 MessageID 做归因回填。
	messageIDs := uniqueRouterMessageIDs(routerResult.Logs)
	if len(messageIDs) > 0 {
		infos, qerr := db.QueryRouterLogsByMessageIDs(ctx, tx, router.Name(), messageIDs)
		if qerr != nil {
			txErr = fmt.Errorf("查询 router 归因: %w", qerr)
			return result
		}
		aliases := deps.cfg.ProviderAliases
		for i := range infos {
			if alias, ok := aliases[infos[i].Provider]; ok {
				infos[i].Provider = alias
			}
		}
		if len(infos) > 0 {
			if _, err := db.BackfillRouterFields(ctx, tx, infos); err != nil {
				txErr = fmt.Errorf("回填 router 归因: %w", err)
				return result
			}
		}
	}

	if routerResult.NextCursor != (model.SyncCursor{}) {
		if err := db.SetSyncCursors(ctx, tx, client, map[string]model.SyncCursor{
			router.SyncSource(): routerResult.NextCursor,
		}); err != nil {
			txErr = fmt.Errorf("保存 router 游标: %w", err)
			return result
		}
	}

	if err := tx.Commit(); err != nil {
		txErr = fmt.Errorf("提交写事务: %w", err)
		return result
	}

	result.Succeeded++
	log.Info("router 采集完成", "client", client, "logs", len(routerResult.Logs))
	if out != nil {
		fmt.Fprintf(out, "✓ %s: router 回填 %d 条归因\n", client, len(routerResult.Logs))
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
