// internal/daemon/daemon.go
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
	"github.com/YuLaiZ/token-usage/internal/logger"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// RunOptions 描述 daemon.Run 的可选参数。
//
// 字段分两类：
//   - 阶段 2：OnDaemonLockCommit + OpenResources。
//     OnDaemonLockCommit 在 daemon lock 获取后调用恰好一次（独立 _run 在回调中释放 control lock）。
//     OpenResources 在 lock commit 后打开 DB/logger 等会写状态的资源；nil 走生产 factory。
//   - InstanceID + ParentLeaseLost（父子 lease 状态机）。
//     ParentLeaseLost 是父 lease 丢失信号（child 侧 lease watcher + 状态机）：EOF 先于 daemon
//     lock commit 时关闭，daemon.Run 据此取消启动（不写 PID/runtime-state、不开 DB）。
//     nil=无父 lease（独立加锁路径，不取消）。
type RunOptions struct {
	// InstanceID 本次守护进程实例标识（父进程生成或独立 _run 自行生成）。
	InstanceID string
	// ParentLeaseLost 父进程 lease 丢失信号。child 的 lease 状态机在「EOF 先于 daemon lock
	// commit」时关闭此 channel。daemon.Run 在 AcquireLock 前后检查它：若已关闭则取消启动
	// （释放刚获取的 daemon lock、不调 OnDaemonLockCommit、不开 DB）。nil=无父 lease。
	ParentLeaseLost <-chan struct{}
	// OnDaemonLockCommit 在 daemon lock 成功获取且 ParentLeaseLost 未先关闭时调用一次。
	// 独立 _run 在此回调中释放 control lock；父 lease 路径 child 在此回调中推进 lease 状态机。
	OnDaemonLockCommit func() error
	// OpenResources 在 lock commit 后打开 DB/logger 等。nil=用生产 factory（打开 usage.db + 配置日志）。
	OpenResources OpenRuntimeResources
}

// ErrParentLeaseLost 在 ParentLeaseLost 先于 daemon lock commit 关闭时由 Run 返回，
// 表示「父级 control lease 已消失，取消启动，不写 PID/runtime-state」。
// 这是 lease EOF 先到的语义；调用方据此退出（不进入 analyzer）。
var ErrParentLeaseLost = errors.New("父进程 control lease 已丢失，取消守护进程启动")

// RuntimeResources 由 OpenResources 返回的运行时资源。Close 由 Run 在退出时调用，
// 其错误与主运行错误用 errors.Join 合并（不得覆盖主错误）。
type RuntimeResources struct {
	DB    *db.DB
	Log   *slog.Logger
	Close func() error
}

// OpenRuntimeResources 打开运行时资源的工厂签名。
type OpenRuntimeResources func(cfg *config.Config) (RuntimeResources, error)

// productionOpenResources 生产用 OpenResources：初始化 logger + 打开 usage DB。
// 由 _run 经 OpenResources=nil 间接触发。
func productionOpenResources(cfg *config.Config) (RuntimeResources, error) {
	log, err := logger.Init(cfg.Log.Level, cfg.Log.Dir, cfg.Log.MaxDays)
	if err != nil {
		return RuntimeResources{}, fmt.Errorf("初始化日志失败: %w", err)
	}
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		logger.Close()
		return RuntimeResources{}, fmt.Errorf("打开数据库失败: %w", err)
	}
	return RuntimeResources{
		DB:  usageDB,
		Log: log,
		Close: func() error {
			dbErr := usageDB.Close()
			logger.Close()
			if dbErr != nil {
				return fmt.Errorf("关闭数据库失败: %w", dbErr)
			}
			return nil
		},
	}, nil
}

// Run 是守护进程的核心：
//
//	（若有 ParentLeaseLost）检查 lease 是否已丢失 → 丢失则取消（不获取 lock）
//	Acquire daemon lock
//	  → 再次检查 ParentLeaseLost（race window）→ 丢失则释放 lock + 取消
//	  → OnDaemonLockCommit（恰好一次，仅在 lease 未先丢失时）
//	  → 写入 PID
//	  → OpenResources 打开 DB/logger（lock commit 后，避免在 lock 冲突时提前打开资源）
//	  → 启动 analyzer 阻塞跑实时监控
//	Release daemon lock（defer）
//
// DB/logger 只能在 daemon lock commit 后打开，
// 保证 lock 冲突时 OpenResources 调用 0 次（不污染数据库、不重复初始化 logger）。
// OpenResources=nil 时用生产 factory。Close 的错误与主运行错误用 errors.Join 合并。
//
// ParentLeaseLost：child 侧 lease 状态机在 EOF 先于 commit 时关闭此 channel。
// Run 在 AcquireLock 前后检查它，确保 EOF 先到时取消启动（不写 PID/runtime-state、不开 DB）。
func Run(ctx context.Context, cfg *config.Config, opts RunOptions) (err error) {
	if cfg == nil {
		return errors.New("daemon config 不能为 nil")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return errors.New("daemon data_dir 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lockPath := filepath.Join(cfg.DataDir, "token-usage.lock")
	pidPath := runmeta.PIDPath(cfg.DataDir)

	// lease 预检：ParentLeaseLost 在 AcquireLock 前已关闭 → 父 lease 已消失，
	// 取消启动（不抢 daemon lock、不写 PID）。覆盖「EOF 远早于 daemon lock」的常见情形。
	if leaseCancelled(opts.ParentLeaseLost) {
		return ErrParentLeaseLost
	}
	if opts.InstanceID == "" ||
		strings.TrimSpace(opts.InstanceID) != opts.InstanceID ||
		len(strings.Fields(opts.InstanceID)) != 1 {
		return fmt.Errorf("InstanceID 必须是非空且不含空白的单字段: %q", opts.InstanceID)
	}

	// 获取排他锁：失败即说明守护进程正在运行。
	// 不前置 IsDaemonRunning（双重加锁 TOCTOU）；AcquireLock 返回 (nil,false) 已能判定「正在运行」。
	f, ok := AcquireLock(lockPath)
	if !ok {
		return fmt.Errorf("守护进程正在运行或获取锁失败，请先停止后再启动")
	}
	defer func() {
		if releaseErr := ReleaseLock(f); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("释放 daemon lock 失败: %w", releaseErr))
		}
	}()

	// lease 竞态再检：EOF 可能在预检与 AcquireLock 之间发生。
	// 此时已持有 daemon lock，但父 lease 已消失 → 释放 lock + 取消（不 commit、不开 DB）。
	if leaseCancelled(opts.ParentLeaseLost) {
		return ErrParentLeaseLost
	}

	// daemon lock 已 commit：恰好一次回调（独立 _run 在此释放 control lock）。
	if opts.OnDaemonLockCommit != nil {
		if commitErr := opts.OnDaemonLockCommit(); commitErr != nil {
			return fmt.Errorf("提交 daemon lock 后释放 control lease 失败: %w", commitErr)
		}
	}

	// lease commit 后再检：OnDaemonLockCommit 内部会推进 lease 状态机
	// （markDaemonLockCommitted）。若 EOF 在 commit 调用瞬间先到，状态机不 commit 且
	// ParentLeaseLost 仍可能被 notifyEOF 关闭 → 这里捕获并取消。
	// 注意：commit 成功后 EOF 不再关闭 ParentLeaseLost（状态机 committed=true 分支），故此检查
	// 只在「EOF 恰好与 commit 竞争且 EOF 略早」时触发。
	if leaseCancelled(opts.ParentLeaseLost) {
		return ErrParentLeaseLost
	}

	// daemon lock 已 commit：无条件清理上一代 PID/state + 精确 temp 残留，再写本次 PID。
	// lock 已确保上一代 daemon 已退出，残留 PID/state 必为 stale，可安全清理。
	if cerr := runmeta.CleanupStaleMetadata(cfg.DataDir); cerr != nil {
		return fmt.Errorf("清理上一代元数据失败: %w", cerr)
	}

	// 写入 PID 文件（新格式 "<pid> <instanceID>"，供 cat 调试 + start ready 握手定位）。
	pid := os.Getpid()
	if werr := runmeta.WritePIDFile(pidPath, pid, opts.InstanceID); werr != nil {
		return fmt.Errorf("写入 PID 文件失败: %w", werr)
	}
	// 正常退出按「确认 instanceID 所有权 → state → PID → daemon lock」顺序清理；
	// defer 在返回时执行，此时 daemon lock 即将由外层 defer Release 释放。
	// 清理失败并入最终返回值，同时残留仍可由下次 start/stop 收敛。
	defer func() {
		if cerr := runmeta.CleanupOwnedMetadata(cfg.DataDir, pid, opts.InstanceID); cerr != nil {
			err = errors.Join(err, fmt.Errorf("清理本实例运行元数据失败: %w", cerr))
		}
	}()

	// OpenResources 在 lock commit 后打开 DB/logger；nil 走生产 factory。
	openFn := opts.OpenResources
	if openFn == nil {
		openFn = productionOpenResources
	}
	resources, oerr := openFn(cfg)
	if oerr != nil {
		return fmt.Errorf("打开运行时资源失败: %w", oerr)
	}
	// Close 在退出时调用；其错误与主错误用 errors.Join 合并，不得覆盖主错误。
	// 通过命名返回值 err：Close 错误 join 到最终返回值，主错误仍由后续 runAnalyzer 写入 err。
	defer func() {
		if resources.Close != nil {
			if cerr := resources.Close(); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
	}()
	if resources.DB == nil {
		return errors.New("运行时资源未提供 usage DB")
	}

	log := resources.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("守护进程启动（后台模式）", "pid", os.Getpid(), "data_dir", cfg.DataDir)

	err = runAnalyzer(ctx, cfg, resources.DB, log, pid, opts.InstanceID)
	return err
}

// leaseCancelled 用非阻塞 select 检查 ParentLeaseLost 是否已关闭。
// nil channel（无父 lease）永远不取消。
func leaseCancelled(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// runAnalyzer 装配 analyzer、启动 startup coordinator 并阻塞跑实时监控。
//
// daemon 提供统一的 ExecuteFunc：调用 engine.RunCollect(recordError=true, skipCollected=false)
// + retry + engine.ValidateResult，并把 ValidateResult 的 error 原样返回。
// 这样实时事件和 startup catch-up 共用同一套错误语义：engine.RunCollect 是
// collection_errors 的唯一持久化责任方；Analyzer 对 monitor Submit 的返回错误只记日志，
// 不再次插入 collection_errors；startup coordinator 用同一 error 累计 failure count。
//
// 顺序：
//  1. 装配 analyzer（NewFromConfig）。
//  2. 为本次 Analyzer.Run 派生可取消 child context。
//  3. 启动并登记 coordinator（属 daemon 关闭 WaitGroup）：等 Analyzer.Ready() → 写 runtime-state
//     → 经 Submit 顺序执行 startup catch-up → 更新阶段状态。
//  4. 阻塞调 a.Run(childCtx)。Analyzer.Run 返回（含 0 monitor 错误）立即 cancel coordinator 的 child ctx。
//  5. 若 coordinator 回传 fatal（ready state 发布失败），把真实错误作为主返回值；
//     否则返回 a.Run 的结果。coordinator 已计入 WaitGroup，此处等其收尾后再返回。
func runAnalyzer(ctx context.Context, cfg *config.Config, usageDB *db.DB, log *slog.Logger, pid int, instanceID string) error {
	deps := engine.NewDeps(cfg)
	// 退避序列：1s→2s→4s，最多 3 次（设计文档 8.6/9.2）
	backoff := func(attempt int) time.Duration {
		return (1 << (attempt - 1)) * time.Second
	}
	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		collectFn := func(ctx context.Context) engine.Result {
			return engine.RunCollect(ctx, deps, usageDB, log, nil, client, req, true, false)
		}
		result := engine.RunCollectWithRetry(ctx, collectFn, 3, backoff, log)
		return engine.ValidateResult(client, result)
	}

	a := analyzer.NewFromConfig(cfg, execute, log, 5*time.Second)

	// coordinator 用 runmeta.WriteRuntimeState 写 runtime-state（适配为 stateWriterFunc）。
	statePath := runmeta.StatePath(cfg.DataDir)
	writeState := stateWriterFunc(func(st runmetaState) error {
		return runmeta.WriteRuntimeState(statePath, runmeta.RuntimeState{
			PID:             st.pid,
			InstanceID:      st.instanceID,
			MonitorReady:    st.monitorReady,
			CatchUp:         st.catchUp,
			CatchUpFailures: st.catchUpFailures,
		})
	})

	return runAnalyzerWithCoordinator(ctx, cfg, a, writeState, pid, instanceID, log)
}

// runAnalyzerWithCoordinator 串联 coordinator 与 Analyzer.Run 的阻塞/关闭协调。
// 从 runAnalyzer 抽出，便于测试注入失败的 state writer 验证 fatal→cancel→return 路径。
//
// 顺序：
//  1. 为本次 Analyzer.Run 派生可取消 child context：coordinator 与 a.Run 共用它。
//  2. 启动并登记 coordinator（属 daemon 关闭 WaitGroup）：等 Analyzer.Ready() → 写 runtime-state
//     → 经 Submit 顺序执行 startup catch-up → 更新阶段状态。
//  3. 阻塞调 a.Run(childCtx)。Analyzer.Run 返回（含 0 monitor 错误）立即 cancel coordinator 的 child ctx。
//  4. 若 coordinator 回传 fatal（ready state 发布失败），立即 cancel child ctx 使 a.Run 进入 shutdown，
//     等其与 coordinator 收尾后以 fatal 为返回值；否则返回 a.Run 的结果。
func runAnalyzerWithCoordinator(ctx context.Context, cfg *config.Config, a *analyzer.Analyzer, writeState stateWriterFunc, pid int, instanceID string, log *slog.Logger) error {
	// 为本次 Analyzer.Run 派生可取消 child context：coordinator 与 a.Run 共用它。
	// Analyzer.Run 返回（含 0 monitor 错误）或 ready state 发布失败时立即 cancel，
	// 使 coordinator 的 Submit 经 ctx.Err() 退出、ready 后的写入被 gate 关闭。
	childCtx, childCancel := context.WithCancel(ctx)
	defer childCancel()

	coord := newStartupCoordinator(cfg, a.Submit, writeState, pid, instanceID, log)

	// 容量 1 的 fatal result channel：ready state 发布失败时 coordinator 把真实错误回传 daemon。
	// daemon 收到后立即 cancel child ctx（不只是 defer），使阻塞的 a.Run 进入 shutdown。
	fatalCh := make(chan error, 1)

	// coordinator 属 daemon 关闭 WaitGroup：Analyzer.Run 返回并 cancel child ctx 后，
	// coordinator 退出（ctx 取消后不再 Submit 或写 state）。
	var coordWg sync.WaitGroup
	coordWg.Add(1)
	go func() {
		defer coordWg.Done()
		coord.run(childCtx, a.Ready(), fatalCh)
	}()

	// a.Run 在单独 goroutine 跑，便于 fatal 时主动 cancel 它（ready state 发布失败路径）。
	// Analyzer.Run 返回（含 0 monitor 错误的 error）或被 fatal-cancel 后退出。
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- a.Run(childCtx)
	}()

	// 等待「fatal 或 a.Run 返回」二者先发生的一个。
	// fatal：ready state 发布失败 → 立即 cancel child ctx，让阻塞的 a.Run 进入 shutdown
	//        （不能等 a.Run 自然返回，否则死锁：a.Run 等 ctx 取消，ctx 取消等 a.Run 返回）。
	// a.Run 先返回：正常 ctx 取消 / 0 monitor 错误；cancel 以让 coordinator 收尾。
	var (
		fatal     error
		runErr    error
		haveFatal bool
	)
	select {
	case fatal = <-fatalCh:
		haveFatal = true
		childCancel()
		runErr = <-runErrCh
	case runErr = <-runErrCh:
		childCancel()
		// 与 fatal 的竞态：a.Run 返回的瞬间 coordinator 也可能写 fatal。优先 fatal。
		select {
		case fatal = <-fatalCh:
			haveFatal = true
		default:
		}
	}
	coordWg.Wait()
	if !haveFatal {
		// a.Run 可能略早返回，而 coordinator 在上面的非阻塞检查之后才发布 fatal。
		// 等 coordinator 收尾后再检查一次，避免把 ready state 发布失败漏报为正常退出。
		select {
		case fatal = <-fatalCh:
			haveFatal = true
		default:
		}
	}

	// ready state 发布失败：start 不得成功，daemon fatal 退出。
	if haveFatal && fatal != nil {
		return fatal
	}
	return runErr
}
