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
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// retryGroup 按 (date, source) 分组的重试单元
type retryGroup struct {
	date   string
	source string
}

func groupByDateSource(errs []model.CollectionError) []retryGroup {
	seen := make(map[string]bool)
	var groups []retryGroup
	for _, e := range errs {
		key := e.Date + "|" + e.Source
		if !seen[key] {
			seen[key] = true
			groups = append(groups, retryGroup{date: e.Date, source: e.Source})
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].date == groups[j].date {
			return groups[i].source < groups[j].source
		}
		return groups[i].date < groups[j].date
	})
	return groups
}

// RunRetry 装配新依赖并执行重试
func RunRetry(cfg *config.Config, usageDB *db.DB, clientName string, log *slog.Logger, out io.Writer) error {
	return RunRetryWithDeps(NewDeps(cfg), usageDB, clientName, log, out)
}

// RunRetryWithDeps 接收已装配的依赖，便于测试注入 fixedCollector 覆盖成功/失败状态机。
func RunRetryWithDeps(deps *Deps, usageDB *db.DB, clientName string, log *slog.Logger, out io.Writer) error {
	return RunRetryWithDepsContext(context.Background(), deps, usageDB, clientName, log, out)
}

// RunRetryWithDepsContext 与 RunRetryWithDeps 相同，但沿用调用方 context，
// 使 CLI 取消能够中止 DB 更新和后续采集。
func RunRetryWithDepsContext(ctx context.Context, deps *Deps, usageDB *db.DB, clientName string, log *slog.Logger, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps == nil || deps.cfg == nil {
		return fmt.Errorf("采集依赖或配置不能为空")
	}
	if usageDB == nil {
		return fmt.Errorf("usage DB 不能为空")
	}
	if log == nil {
		log = slog.Default()
	}
	if clientName != "" && !hasCollector(deps, clientName) {
		return fmt.Errorf("未知客户端: %s（支持: claude, opencode, codex, workbuddy, zcode, autoclaw）", clientName)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	errs, err := db.GetErrorsContext(ctx, usageDB, db.ErrorFilter{Source: clientName, Type: "error", Unresolved: true})
	if err != nil {
		return fmt.Errorf("查询失败记录: %w", err)
	}
	if len(errs) == 0 {
		if out != nil {
			fmt.Fprintln(out, "暂无需要重试的失败记录")
		}
		return nil
	}

	groups := groupByDateSource(errs)
	if out != nil {
		fmt.Fprintf(out, "重试 %d 组失败采集...\n\n", len(groups))
	}
	retrySuccess := 0
	retryFailed := 0

	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 未知或禁用 collector 没有发生真实采集，不递增 retry_count。
		if !hasCollector(deps, group.source) || !collectorEnabled(deps, group.source) {
			if out != nil {
				fmt.Fprintf(out, "✗ %s (%s): 数据源不存在或未启用，未执行重试\n", group.source, group.date)
			}
			retryFailed++
			continue
		}
		updated, err := db.IncrementRetryCountByDateSource(ctx, usageDB, group.date, group.source)
		if err != nil {
			log.Error("原子更新重试次数失败", "client", group.source, "date", group.date, "error", err)
			retryFailed++
			continue
		}
		if updated == 0 {
			// 查询后到执行前记录已被其他成功采集解决；不再重复采集。
			if out != nil {
				fmt.Fprintf(out, "- %s (%s): 错误已被其他采集恢复，跳过\n", group.source, group.date)
			}
			retrySuccess++
			continue
		}

		log.Info("重试采集", "client", group.source, "date", group.date)
		result := RunCollect(ctx, deps, usageDB, log, nil, group.source,
			collector.CollectRequest{Dates: []string{group.date}}, false, false) // 失败时不新增错误；成功时 RunCollect 自动解决同组错误
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.Complete() {
			if out != nil {
				fmt.Fprintf(out, "✓ %s (%s): 重试成功\n", group.source, group.date)
			}
			retrySuccess++
		} else {
			reason := result.Err
			if reason == nil {
				reason = fmt.Errorf("采集未完整执行")
			}
			if out != nil {
				fmt.Fprintf(out, "✗ %s (%s): 重试失败: %v\n", group.source, group.date, reason)
			}
			retryFailed++
		}
	}

	if out != nil {
		fmt.Fprintf(out, "\n重试完成: %d 成功, %d 失败\n", retrySuccess, retryFailed)
	}
	if retryFailed > 0 {
		return fmt.Errorf("部分重试失败: %d 组", retryFailed)
	}
	return nil
}

// RunCollectWithRetry 对单次采集失败做指数退避重试（设计文档 8.6/9.2）。
// collectFn 通常是 RunCollect 的适配闭包；抽参数便于测试注入失败/成功状态机。
// maxRetries 为重试上限（不含首次）；backoff(retryAttempt) 返回第 retryAttempt 次重试前的等待（从 1 起）。
// ctx 取消时退避 sleep 立即被打断并返回。全失败时返回最后一次 Result。
func RunCollectWithRetry(ctx context.Context, collectFn func(context.Context) Result,
	maxRetries int, backoff func(retryAttempt int) time.Duration, log *slog.Logger) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if collectFn == nil {
		return Result{Err: errors.New("collectFn 不能为空")}
	}
	if err := ctx.Err(); err != nil {
		return Result{Err: err}
	}
	if backoff == nil {
		backoff = func(int) time.Duration { return 0 }
	}
	res := collectFn(ctx)
	if res.Complete() {
		return res
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// ctx 取消时不继续重试
		if err := ctx.Err(); err != nil {
			res.Err = errors.Join(res.Err, err)
			return res
		}
		wait := backoff(attempt)
		select {
		case <-ctx.Done():
			res.Err = errors.Join(res.Err, ctx.Err())
			return res
		case <-time.After(wait):
		}
		if log != nil {
			log.Info("守护进程重试采集", "attempt", attempt, "last_error", res.Err)
		}
		res = collectFn(ctx)
		if res.Complete() {
			return res
		}
	}
	return res
}
