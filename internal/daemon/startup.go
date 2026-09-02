// internal/daemon/startup.go
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
)

// SubmitFunc 是 coordinator 调用的同步串行 Submit（来自 analyzer）。
type SubmitFunc func(ctx context.Context, client string, req collector.CollectRequest) error

// runmetaState 是 coordinator 写出的 runtime-state 快照（测试可直接断言字段）。
type runmetaState struct {
	pid             int
	instanceID      string
	monitorReady    bool
	catchUp         string
	catchUpFailures int
}

// stateWriterFunc 把 runmeta.WriteRuntimeState 适配成 coordinator 需要的签名（便于测试注入）。
type stateWriterFunc func(st runmetaState) error

// startupCoordinator 串联 monitor ready → runtime-state → catch-up。
type startupCoordinator struct {
	cfg          *config.Config
	submit       SubmitFunc
	writeStateFn stateWriterFunc
	pid          int
	instanceID   string
	log          *slog.Logger
}

// newStartupCoordinator 构造 coordinator。writeState 注入 runtime-state 写入函数
// （生产路径由 runAnalyzer 包装 runmeta.WriteRuntimeState；测试可注入 recorder）。
// writeState 为 nil 时 writeState 静默成功（仅用于不需要 state 的快速测试）。
func newStartupCoordinator(cfg *config.Config, submit SubmitFunc, writeState stateWriterFunc, pid int, instanceID string, log *slog.Logger) *startupCoordinator {
	if log == nil {
		log = slog.Default()
	}
	return &startupCoordinator{
		cfg:          cfg,
		submit:       submit,
		writeStateFn: writeState,
		pid:          pid,
		instanceID:   instanceID,
		log:          log,
	}
}

// catchUpRequestsFor 返回某 client 的 client-source startup catch-up 请求。
//
// 与 analyzer.setupFromConfig 的 client 名硬编码装配对称（claude=JSONL、opencode=SQLite 等），
// 新增 client 时两处同步更新。必须返回复数请求以表达 Codex 的 state incremental + rollout full scan。
// router 请求不在本函数产出（由 runCatchUp 按 client 单独追加，Source=router）。
//
// 请求矩阵：
//   - opencode、zcode：Incremental=true，从持久化 SQLite cursor 继续。
//   - claude、workbuddy、autoclaw：无日期扫描现存 JSONL（Incremental=false，Dates 空，
//     ScanExistingJSONL=true——现存 JSONL 全扫的显式合同，与 codex rollout 全扫同语义）。
//   - codex：两个串行请求——先 Incremental=true 推进 state cursor，再 ScanExistingJSONL=true
//     无日期全扫 rollout JSONL。
//
// 未知 client 返回 nil（已启用但分类未登记的 client 不会被 coordinator 处理）。
func catchUpRequestsFor(client string) []collector.CollectRequest {
	switch client {
	case "opencode", "zcode":
		return []collector.CollectRequest{{Source: collector.CollectSourceClient, Incremental: true}}
	case "claude", "workbuddy", "autoclaw":
		// 无日期扫描现存 JSONL：Dates 空、Incremental=false、ChangedFile 空。
		// ScanExistingJSONL 使「catch-up 全扫」成为显式合同而非 Source 字段的巧合差异。
		return []collector.CollectRequest{{Source: collector.CollectSourceClient, ScanExistingJSONL: true}}
	case "codex":
		// 先 state incremental（推进 cursor），再 rollout full scan（无日期全扫）。
		// 第一个失败也必须继续第二个（由 runCatchUp 保证）。
		return []collector.CollectRequest{
			{Source: collector.CollectSourceClient, Incremental: true},       // state incremental
			{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, // rollout full scan
		}
	default:
		return nil
	}
}

// runCatchUp 执行 startup catch-up：按确定性契约顺序 Submit 所有请求，返回失败请求数。
//
// 请求顺序契约：
//  1. 只遍历 effective config 中已启用 client，按 client 名升序。
//  2. 每个 client 先按 catchUpRequestsFor 返回顺序执行全部 client-source 请求；
//     Codex 固定 state incremental → rollout full scan。
//  3. 当前 client 的 client-source 请求全部尝试后，若配置 router，再执行该 client 的一个
//     router incremental 请求（Source=router, Incremental=true）。不采用「全部 client 完成后再统一 router」。
//  4. 任一请求失败只累计该请求一次 failure，不跳过当前 client 后续请求、router 请求或后续 client。
//
// ctx 取消语义：cancel 后不再发起新的 Submit（「cancel 后无新增 submit/state 写入」）。
// Submit 内部接收 ctx：正在跑的请求经 ctx.Done() 及时退出。返回 context.Canceled 时停止
// 后续请求且不计为采集失败（取消不是采集错误）。真实采集失败（非取消）才累计 failure count
// 并继续后续请求（含 Codex 第一个失败也执行 rollout full scan）。
//
// 返回累计失败请求数（用于 final state 的 catch_up_failures）。
func (c *startupCoordinator) runCatchUp(ctx context.Context) int {
	failures := 0
	for _, clientName := range enabledClientNamesSorted(c.cfg) {
		// cancel 后停止：不再发起新 client 的任何请求。
		if ctx.Err() != nil {
			return failures
		}
		// client-source 请求（按 catchUpRequestsFor 顺序）。
		for _, req := range catchUpRequestsFor(clientName) {
			if stopped := c.submitOne(ctx, clientName, req, &failures, "client-source"); stopped {
				return failures
			}
		}
		// router incremental 请求（若该 client 配置了 router 且 router 配置有效）。
		if req, ok := routerCatchUpRequest(c.cfg, clientName); ok {
			if stopped := c.submitOne(ctx, clientName, req, &failures, "router"); stopped {
				return failures
			}
		}
	}
	return failures
}

// submitOne 提交单个 catch-up 请求并累计失败。返回 true 表示 ctx 已取消，调用方应停止后续请求。
// ctx 取消（context.Canceled）返回 stopped=true 且不计 failure；真实采集错误累计 failure 且 stopped=false。
func (c *startupCoordinator) submitOne(ctx context.Context, clientName string, req collector.CollectRequest, failures *int, kind string) (stopped bool) {
	if ctx.Err() != nil {
		return true // cancel 后不发起新请求。
	}
	err := c.submit(ctx, clientName, req)
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// ctx 取消/超时：不是采集失败，停止后续请求。
		// DeadlineExceeded 与 Canceled 同等处理：两者都代表「本次运行生命周期结束」而非采集错误，
		// 失败计数应保持干净（final state 的 catch_up_failures 只反映真实采集失败）。
		c.log.Info("startup catch-up stopped by ctx cancellation",
			"client", clientName, "kind", kind, "error", err)
		return true
	}
	*failures++
	c.log.Error("startup catch-up failed",
		"client", clientName, "kind", kind, "incremental", req.Incremental, "source", req.Source, "error", err)
	return false
}

// run 串联 monitor ready → runtime-state → catch-up。
//
// 顺序：
//  1. 等待 ready barrier（analyzer 所有 monitor 就绪）；ctx 取消则直接返回（不写 state、不 catch-up）。
//  2. 再次检查 ctx（ready 与 cancel 的竞态）。
//  3. 写 ready state {monitor_ready:true, catch_up:pending, failures:0}：失败 → 用 fatalCh（容量 1）
//     回传真实错误，daemon 立即 cancel analyzer child ctx。不 Submit catch-up。返回。
//  4. 写 running state：失败时记录日志并继续 catch-up，不停 daemon。
//  5. 顺序 Submit catch-up，累计失败请求数。
//  6. 写 final state（0 失败=succeeded，否则=failed + 准确 failure count）：失败时记录日志，不停 daemon。
//
// 三类独立结果：
//   - ready 前 state 发布失败 → start 不得成功，daemon fatal 退出（fatalCh）。
//   - ready 后阶段更新失败 → 日志可观察，不停 daemon，后续写入仍尝试。
//   - 单项采集失败 → 继续剩余请求，final state 记录 failed + 失败数。
func (c *startupCoordinator) run(ctx context.Context, ready <-chan struct{}, fatalCh chan<- error) {
	// 1. 等待 monitor ready（或 ctx 取消）。
	select {
	case <-ready:
	case <-ctx.Done():
		return // cancel 在 ready 前/时：不写 state、不 catch-up。
	}

	// 2. ready 与 cancel 的竞态再检。
	if err := ctx.Err(); err != nil {
		return
	}

	// 3. 写 ready state（pending）。失败 → fatal，不 catch-up。
	if err := c.writeState(runmetaState{
		pid:          c.pid,
		instanceID:   c.instanceID,
		monitorReady: true,
		catchUp:      phasePending,
	}); err != nil {
		c.log.Error("failed to write runtime-state(ready/pending), returning fatal and cancelling analyzer",
			"error", err)
		select {
		case fatalCh <- err:
		default:
			// fatalCh 容量 1，daemon 可能已退出不再读；保证不阻塞（非阻塞写）。
		}
		return
	}

	// 4. 写 running state。失败不停 daemon，继续 catch-up，后续写入仍尝试。
	if err := c.writeState(runmetaState{
		pid:          c.pid,
		instanceID:   c.instanceID,
		monitorReady: true,
		catchUp:      phaseRunning,
	}); err != nil {
		c.log.Error("failed to write runtime-state(running), continuing catch-up",
			"phase", phaseRunning, "error", err)
	}

	// 5. catch-up（顺序 Submit，累计失败请求数）。
	failures := c.runCatchUp(ctx)

	// 6. 写 final state。失败不停 daemon。
	// cancel 后不再写 state：
	// runCatchUp 返回后若 ctx 已取消（catch-up 中途被取消），直接返回，不写 final。
	if ctx.Err() != nil {
		return
	}
	phase := phaseSucceeded
	if failures > 0 {
		phase = phaseFailed
	}
	if err := c.writeState(runmetaState{
		pid:             c.pid,
		instanceID:      c.instanceID,
		monitorReady:    true,
		catchUp:         phase,
		catchUpFailures: failures,
	}); err != nil {
		c.log.Error("failed to write runtime-state(final), daemon continues",
			"phase", phase, "failures", failures, "error", err)
	}
}

// writeState 通过注入的 writeStateFn 写 runtime-state（生产侧为 runmeta.WriteRuntimeState 适配）。
func (c *startupCoordinator) writeState(st runmetaState) error {
	if c.writeStateFn == nil {
		return nil // 测试未注入时静默成功（生产路径必注入）
	}
	return c.writeStateFn(st)
}

// catch-up 阶段常量（与 runmeta RuntimeState.CatchUp 取值一致）。
const (
	phasePending   = "pending"
	phaseRunning   = "running"
	phaseSucceeded = "succeeded"
	phaseFailed    = "failed"
)

func enabledClientNamesSorted(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Clients))
	for name, c := range cfg.Clients {
		if c.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// routerCatchUpRequest 返回 clientName 的 router incremental 请求。
// 仅当 client 已启用、配置了 router 且该 router 配置存在（DBPath 非空）时返回 (req, true)。
// 与 analyzer.setupFromConfig 的 router 装配判断对称。
func routerCatchUpRequest(cfg *config.Config, clientName string) (collector.CollectRequest, bool) {
	clientCfg, ok := cfg.ClientConfig(clientName)
	if !ok || !clientCfg.Enabled || clientCfg.Router == "" {
		return collector.CollectRequest{}, false
	}
	routerCfg, ok := cfg.RouterConfig(clientCfg.Router)
	if !ok || routerCfg.DBPath == "" {
		return collector.CollectRequest{}, false
	}
	return collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true}, true
}
