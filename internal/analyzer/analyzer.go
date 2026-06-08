// internal/analyzer/analyzer.go
package analyzer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
)

// ErrAnalyzerStopping 在 gate 关闭（shutdown）后由 Submit 返回。
// 它是稳定的：shutdown 期间及之后的所有新提交都返回此错误，且不对 collectWg 做 Add。
var ErrAnalyzerStopping = errors.New("analyzer 正在停止，不再接受采集请求")

// ErrAnalyzerAlreadyRun 表示同一个 Analyzer 已经启动过。Analyzer 的 watcher、
// poller、ready channel 与 stopOnce 都是单次生命周期资源，不能二次启动。
var ErrAnalyzerAlreadyRun = errors.New("analyzer 已经运行过，不能重复启动")

// ErrAnalyzerExecuteUnavailable 表示 Analyzer 未配置采集执行器。
var ErrAnalyzerExecuteUnavailable = errors.New("analyzer 采集执行器不能为空")

// MonitorSubmitFunc 是 watcher/poller 上报采集请求的统一入口（void）。
// watcher/poller 只产生 (client, req) 并调用本函数；不直接持有 ExecuteFunc。
// Analyzer 注入的 monitor wrapper 负责从 gate 取本次 runCtx 并同步调用 Submit。
type MonitorSubmitFunc func(client string, req collector.CollectRequest)

// ExecuteFunc 是实际执行采集并返回真实错误的同步函数。
// 由 daemon 提供：统一调用 engine.RunCollect(recordError=true, skipCollected=false) + retry +
// engine.ValidateResult。Submit 在 collect mutex 内同步调用本函数，并将其 error 原样返回。
// ctx 由提交方提供：monitor wrapper 传本次 runCtx；startup coordinator 传 daemon child ctx。
type ExecuteFunc func(ctx context.Context, client string, req collector.CollectRequest) error

// Analyzer 实时分析模块，管理所有监控 goroutine
type Analyzer struct {
	execute ExecuteFunc

	collectMu sync.Mutex      // 采集串行化锁：同一时刻只有一个采集在跑
	collectWg sync.WaitGroup  // 跟踪 in-flight 采集，供 Run 关闭时等待（见 Submit）
	gateMu    sync.Mutex      // accepting gate 锁：保护 accepting、runCtx 与 collectWg.Add 的原子性
	accepting bool            // 是否接受新采集；Run 启动前置 true，关闭前置 false（杜绝 gate 关闭后 Add）
	runCtx    context.Context // 本次 Run 安装的可取消 context；monitor wrapper 从 gate 取它传给 Submit
	runCancel context.CancelFunc
	started   bool // Run 已被调用；与 stopped 一起受 gateMu 保护
	stopped   bool // Stop 已请求；Analyzer 是单次运行对象，不允许 Stop 后重新 Run

	logger        *slog.Logger
	wg            sync.WaitGroup
	jsonlWatchers []*JSONLWatcher
	sqlitePollers []*SQLitePoller
	stopOnce      sync.Once

	// ready barrier：所有 watcher/poller 装配后总数冻结为 len(jsonlWatchers)+len(sqlitePollers)，
	// 反映为 readyWg 的初始计数。每个 monitor 通过其专属 signalOnce 恰好 signal 一次，
	// 全部 signal 后 readyWg 归零，Run 的 barrier goroutine 关闭 readyCh。
	readyWg sync.WaitGroup // 倒计时与 monitor 总数相等的 ready signal
	readyCh chan struct{}  // 只读 Ready() barrier

	readyErrMu sync.Mutex
	readyErr   error      // monitor 初始化失败；非 nil 时永不发布 ready
	monitorErr chan error // ready 后 monitor 异常退出也必须让 Analyzer.Run 失败
}

// New 创建 Analyzer 实例。
// execute 是实际采集执行函数（由 daemon 提供）；logger 为 nil 时用 slog.Default()。
// accepting 默认 false：只有 Run 启动并在 gate 内安装 runCtx 后才置 true。
// 这保证 Submit 在 Run 之外（如直接构造用于单元测试）必须显式调用 enableSubmitForTest
// 或经过 Run；避免「提交但未 Run」导致 runCtx 为 nil。
func New(execute ExecuteFunc, logger *slog.Logger) *Analyzer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Analyzer{
		execute:    execute,
		logger:     logger,
		readyCh:    make(chan struct{}),
		monitorErr: make(chan error, 1),
		accepting:  false,
	}
}

// Ready 返回只读 ready barrier channel：所有 watcher/poller 完成初始化后关闭。
// 0 个存活 monitor 时 Run 直接返回 error，此 channel 永不关闭。
func (a *Analyzer) Ready() <-chan struct{} {
	return a.readyCh
}

// Submit 是实时事件与启动补采共用的同步串行入口。
//
// 单一 gate + WaitGroup + 单一 collect mutex 契约：
//  1. gateMu 锁内检查 accepting：false 返回 ErrAnalyzerStopping（稳定、不再 Add）；
//     true 则 collectWg.Add(1)，快照本次 runCtx，释放 gate。
//  2. 等 collectMu（串行化：同一时刻只有一个 ExecuteFunc 在跑）。
//  3. 获 mutex 后再次检查 ctx：已取消则返回 ctx.Err()（不执行 ExecuteFunc）。
//  4. 同步执行 ExecuteFunc，返回其真实 error；defer Done()。
//
// monitor wrapper 从 gate 取本次 runCtx 传给 Submit；startup coordinator 直接把 daemon
// child context 传给同一个 Submit。这样 watcher/poller 与 catch-up 共用同一套 gate/mutex，
// 不存在第二套采集入口或第二套 WaitGroup。
//
// shutdown 顺序：gateMu 内 accepting=false → runCancel（使在跑/等 mutex 的 Submit 可退出）
// → 停 monitor/debounce → 等 collectWg。新提交返回 ErrAnalyzerStopping 且不再 Add。
func (a *Analyzer) Submit(ctx context.Context, client string, req collector.CollectRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.gateMu.Lock()
	if !a.accepting {
		a.gateMu.Unlock()
		return ErrAnalyzerStopping
	}
	a.collectWg.Add(1)
	a.gateMu.Unlock()
	defer a.collectWg.Done()

	a.collectMu.Lock()
	defer a.collectMu.Unlock()

	// 获 mutex 后再次检查 ctx：shutdown 已 runCancel 或 startup child ctx 已取消时，
	// 不执行 ExecuteFunc，直接返回，让等待方及时退出。
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.execute == nil {
		return ErrAnalyzerExecuteUnavailable
	}
	return a.execute(ctx, client, req)
}

// newMonitorSignaler 为单个 monitor 返回恰好执行一次的 signalReady 回调。
// 每个 monitor 调用它一次表示自身就绪（JSONL watcher 在 Walk+watch 注册后；
// SQLite poller 在记录初始 mtime 后）。重复调用是 no-op（sync.Once 保护）。
// readyWg 由 setupFromConfig 预置为 monitorCount；Run 启动 monitor 后起一个
// goroutine 等待 readyWg 归零并关闭 readyCh（见 Run）。
func (a *Analyzer) newMonitorSignaler() func() {
	ready, _ := a.newMonitorSignalers()
	return ready
}

// newMonitorSignalers 为可能在初始化阶段失败的 monitor 返回一对共享 sync.Once 的
// success/failure 回调。无论成功还是失败，都恰好消费一个 readyWg 计数；失败会先记录
// 原因再 Done，因此 barrier waiter 不会把失败误判为 ready。
func (a *Analyzer) newMonitorSignalers() (func(), func(error)) {
	var once sync.Once
	complete := func(err error) {
		once.Do(func() {
			if err != nil {
				a.recordReadyError(err)
			}
			a.readyWg.Done()
		})
	}
	return func() { complete(nil) }, complete
}

func (a *Analyzer) recordReadyError(err error) {
	if err == nil {
		return
	}
	a.readyErrMu.Lock()
	a.readyErr = errors.Join(a.readyErr, err)
	a.readyErrMu.Unlock()
}

func (a *Analyzer) readyError() error {
	a.readyErrMu.Lock()
	defer a.readyErrMu.Unlock()
	return a.readyErr
}

func (a *Analyzer) reportMonitorError(err error) {
	if err == nil {
		return
	}
	select {
	case a.monitorErr <- err:
	default:
		a.logger.Error("monitor 异常退出（另一个错误已在处理）", "error", err)
	}
}

// NewFromConfig 从配置创建 Analyzer。
// execute 是实际采集执行函数（daemon 提供：RunCollect+retry+ValidateResult）；
// debounceDuration 是 JSONL watcher 的防抖间隔；生产路径传 5*time.Second，
// 集成测试传短值（如 100ms）以确保 jsonl→采集链路在测试等待窗口内真正触发。
//
// watcher/poller 只接收 MonitorSubmitFunc（不接收 ExecuteFunc）。Analyzer 注入的 monitor
// wrapper 从 gate 取本次 runCtx，调用 Submit(runCtx, client, req)；其返回错误只记日志，
// 不再插入 collection_errors（collection_errors 由 engine.RunCollect 唯一记录）。
//
// 装配结束后 monitor 总数冻结为 jsonlWatchers+sqlitePollers，ready barrier 据此建立。
func NewFromConfig(cfg *config.Config, execute ExecuteFunc, logger *slog.Logger, debounceDuration time.Duration) *Analyzer {
	if debounceDuration <= 0 {
		debounceDuration = 5 * time.Second
	}
	a := New(execute, logger)
	a.setupFromConfig(cfg, debounceDuration)
	return a
}

// monitorSubmit 构造注入 watcher/poller 的 MonitorSubmitFunc：从 gate 取本次 runCtx，
// 调用同步 Submit；其错误只记日志（monitor 继续监控，不因单次采集失败退出）。
// runCtx 由 Run 在 gate 内安装；Submit 之前的 watcher/poller 调用此 wrapper 时 runCtx 必非 nil
// （Run 在安装 runCtx 并打开 accepting 后才启动 monitor goroutine）。
func (a *Analyzer) monitorSubmit(client string, req collector.CollectRequest) {
	a.gateMu.Lock()
	runCtx := a.runCtx
	a.gateMu.Unlock()
	if runCtx == nil {
		// 防御性：monitor 只在 Run 打开 accepting 后才跑，runCtx 不应为 nil。
		// 到达这里说明调用时序错误；New() 默认 accepting=false，Submit 必因 accepting=false
		// 返回 ErrAnalyzerStopping，context.Background() 永远到不了 ExecuteFunc（死代码）。
		// 故直接放弃提交：不调 Submit，不引入生产路径的 Background。
		a.logger.Error("runCtx 未初始化，放弃提交", "client", client)
		return
	}
	if err := a.Submit(runCtx, client, req); err != nil && !errors.Is(err, ErrAnalyzerStopping) {
		a.logger.Error("monitor 采集提交失败", "client", client, "error", err)
	}
}

// setupFromConfig 根据配置设置监控
func (a *Analyzer) setupFromConfig(cfg *config.Config, debounceDuration time.Duration) {
	if cfg == nil {
		a.recordReadyError(errors.New("analyzer 配置不能为空"))
		return
	}
	// watcher/poller 统一注入 monitorSubmit（从 gate 取 runCtx 调同步 Submit）。
	submit := a.monitorSubmit

	// 为每个启用的 JSONL 客户端创建独立的 watcher
	// 这样每个 watcher 的 clientName 是真实的客户端名，Submit 可以正确匹配。
	// ChangedFile 由 watcher 运行期动态填充，此处无需固定 request。
	jsonlClients := []struct {
		name    string
		pathKey string
	}{
		{"claude", "projects_dir"},
		{"codex", "sessions_dir"},
		{"workbuddy", "projects_dir"},
		{"autoclaw", "sessions_dir"},
	}

	for _, jc := range jsonlClients {
		clientCfg, ok := cfg.ClientConfig(jc.name)
		if !ok || !clientCfg.Enabled {
			continue
		}
		dir, ok := clientCfg.Paths[jc.pathKey]
		if !ok || dir == "" {
			continue
		}

		watcher, err := NewJSONLWatcher(
			[]string{dir},
			jc.name,          // 使用真实的客户端名
			debounceDuration, // 防抖间隔（生产 5s，测试可调短）
			submit,
			a.logger, // 透传项目 logger
		)
		if err != nil {
			wrapped := fmt.Errorf("%s JSONL watcher 初始化失败: %w", jc.name, err)
			a.recordReadyError(wrapped)
			a.logger.Error("创建 JSONL watcher 失败", "client", jc.name, "error", err)
			continue
		}
		a.addWatcher(watcher)
	}

	// 创建 SQLite 轮询器
	interval := time.Duration(cfg.Daemon.PollInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	// client 源 SQLite poller 固定 Incremental 请求（OpenCode/ZCode/Codex state DB）
	clientReq := collector.CollectRequest{Incremental: true}

	// OpenCode：监控 opencode.db
	if opencode, ok := cfg.ClientConfig("opencode"); ok && opencode.Enabled {
		if dbPath, ok := opencode.Paths["db"]; ok && dbPath != "" {
			a.addSQLitePoller("opencode", dbPath, clientReq, interval)
		}
	}

	// ZCode：监控 db.sqlite（WAL 模式取 max(db, -wal) mtime，复用 GetSQLiteMtime）
	if zcode, ok := cfg.ClientConfig("zcode"); ok && zcode.Enabled {
		if dbPath, ok := zcode.Paths["db"]; ok && dbPath != "" {
			a.addSQLitePoller("zcode", dbPath, clientReq, interval)
		}
	}

	// Codex 双重监控取舍：
	// state DB 是主源（包含完整 session 元数据），rollout JSONL 是辅助（记录原始 API 调用）
	// 两者都监控，串行化锁下并发重复不造成数据错误（UpsertMessage 主键 upsert 幂等）
	if codex, ok := cfg.ClientConfig("codex"); ok && codex.Enabled {
		if stateDir, ok := codex.Paths["state_dir"]; ok && stateDir != "" {
			// 按字面值读取目录，再对文件名应用模式。Codex 升级或首次启动后可能新建
			// state_*.sqlite；每次 tick 重新扫描可覆盖新增、替换和删除，同时不会把
			// stateDir 中的 [, ? 等合法字符误当成 glob 语法。
			a.addSQLiteDirGlobPoller("codex", stateDir, "state_*.sqlite", clientReq, interval)
		}
	}

	// 注意：WorkBuddy 不建 SQLite poller。
	// workbuddy.db 是 title 只读查询库（workbuddy.go:60-67 queryWorkBuddyTitles），
	// 不由 token-usage 写入，其 mtime 变化不对应「有新 token 数据」；
	// WorkBuddy 的真实数据源是 projects_dir 下的 JSONL，已由上面的 JSONL watcher 覆盖。

	// Router DB 轮询：遍历所有启用 client，按 client.Router 查找 router 配置，
	// 为 SQLite 型 router 建立 poller 并触发对应 client 采集。
	// router poller 固定 Source=router, Incremental=true（路由中间件日志增量采集）。
	routerReq := collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true}
	for clientName, clientCfg := range cfg.Clients {
		if !clientCfg.Enabled || clientCfg.Router == "" {
			continue
		}
		routerCfg, ok := cfg.RouterConfig(clientCfg.Router)
		if !ok || routerCfg.DBPath == "" {
			continue
		}
		switch clientCfg.Router {
		case "cc_switch":
			a.addSQLitePoller(clientName, routerCfg.DBPath, routerReq, interval)
		}
	}
}

// addWatcher 注册一个 JSONL watcher，预置它的 ready signal（setupFromConfig 装配期），
// 并加入 readyWg 倒计时。signalOnce 保证重复 signal 不重复扣减（不重复 close/panic）。
func (a *Analyzer) addWatcher(w *JSONLWatcher) {
	a.readyWg.Add(1)
	w.signalReady, w.signalFailure = a.newMonitorSignalers()
	a.jsonlWatchers = append(a.jsonlWatchers, w)
}

// addSQLitePoller 构造并注册一个 SQLite 轮询器，集中 NewSQLitePoller + append 样板，
// 使 setupFromConfig 各 SQLite 源（opencode/codex state/cc_switch）只关心「路径从哪来」。
// request 携带该源的固定采集语义（client 源 Incremental；router 源 Source=router）。
func (a *Analyzer) addSQLitePoller(clientName, dbPath string, request collector.CollectRequest, interval time.Duration) {
	poller := NewSQLitePoller(dbPath, clientName, request, interval, a.monitorSubmit, a.logger)
	poller.signalReady = a.newMonitorSignaler()
	a.readyWg.Add(1)
	a.sqlitePollers = append(a.sqlitePollers, poller)
}

func (a *Analyzer) addSQLiteDirGlobPoller(clientName, dir, pattern string, request collector.CollectRequest, interval time.Duration) {
	poller := newSQLiteDirGlobPoller(dir, pattern, clientName, request, interval, a.monitorSubmit, a.logger)
	poller.signalReady = a.newMonitorSignaler()
	a.readyWg.Add(1)
	a.sqlitePollers = append(a.sqlitePollers, poller)
}

// Run 启动所有监控
func (a *Analyzer) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.logger.Info("starting analyzer")

	// Analyzer 内部的 monitor/ready/stop 资源均为单次使用。先原子占用本次生命周期，
	// 防止并发或顺序二次 Run 重复启动 goroutine、重复 close readyCh。
	a.gateMu.Lock()
	if a.started {
		a.gateMu.Unlock()
		return ErrAnalyzerAlreadyRun
	}
	if a.stopped {
		a.gateMu.Unlock()
		return ErrAnalyzerStopping
	}
	a.started = true
	a.gateMu.Unlock()

	// 构造期失败意味着已配置 monitor 未能装配。即使另有存活 monitor，也不能发布
	// “全部 monitor ready”；否则 start 会在部分数据源实际未受监控时错误成功。
	if err := a.readyError(); err != nil {
		a.Stop()
		return fmt.Errorf("monitor 初始化失败: %w", err)
	}

	// 0 监控目标时返回 error：所有客户端禁用、路径缺失或监控初始化失败会导致守护进程
	// 无事可做，立即返回 error 使 launchd 能感知配置错误（告警/重启），而非静默空转。
	// 不发布 ready（Ready() channel 永不关闭）。
	if len(a.jsonlWatchers) == 0 && len(a.sqlitePollers) == 0 {
		return fmt.Errorf("无存活监控：所有客户端禁用、路径缺失或监控初始化失败，请检查 config.toml [clients.*].enabled 与 paths")
	}

	// 派生本次 Run 自己可取消的 runCtx/runCancel，安装到同一 gate 并打开 accepting。
	// runCtx 由 monitor wrapper 从 gate 取出传给 Submit；runCancel 用于 shutdown 时使
	// 正在执行/等待 collect mutex 的 Submit 通过 ctx.Err() 及时退出。
	runCtx, runCancel := context.WithCancel(ctx)
	a.gateMu.Lock()
	if a.stopped {
		a.gateMu.Unlock()
		runCancel()
		return ErrAnalyzerStopping
	}
	a.runCtx = runCtx
	a.runCancel = runCancel
	a.accepting = true
	a.gateMu.Unlock()

	// 启动 JSONL 监控
	for _, watcher := range a.jsonlWatchers {
		a.wg.Add(1)
		go func(w *JSONLWatcher) {
			defer a.wg.Done()
			if err := w.Run(runCtx); err != nil {
				a.reportMonitorError(fmt.Errorf("%s JSONL watcher 失败: %w", w.clientName, err))
			}
		}(watcher)
	}

	// 启动 SQLite 轮询
	for _, poller := range a.sqlitePollers {
		a.wg.Add(1)
		go func(p *SQLitePoller) {
			defer a.wg.Done()
			p.Run(runCtx)
		}(poller)
	}

	// ready barrier：所有 monitor 已启动并各持有一个 ready signal 预置位，
	// 起一个 goroutine 等待 readyWg 归零后关闭 readyCh（恰好一次）。
	readyDone := make(chan struct{})
	go func() {
		a.readyWg.Wait()
		if a.readyError() == nil {
			close(a.readyCh)
		}
		close(readyDone)
	}()

	// 正常路径在 ready 后继续等待父 context；初始化失败或 ready 后 monitor 异常退出
	// 都会结束本次 Run，避免 daemon 在监控链已断时静默存活。
	var runErr error
	select {
	case <-runCtx.Done():
	case <-readyDone:
		if err := a.readyError(); err != nil {
			runErr = fmt.Errorf("monitor 初始化失败: %w", err)
			break
		}
		select {
		case <-runCtx.Done():
		case err := <-a.monitorErr:
			runErr = err
		}
	case err := <-a.monitorErr:
		runErr = err
	}

	// 优雅关闭（顺序至关重要）：
	// 1. gateMu 内 accepting=false：之后到达的 Submit 返回 ErrAnalyzerStopping 且不再 Add。
	// 2. runCancel：使正在执行/等待 collect mutex 的 Submit 通过 ctx.Err() 退出。
	//    必须先关 accepting 再 runCancel——不能先阻塞等 debounce callback 再取消 context
	//    （debounce.Stop 内部等待回调，回调走 Submit 时若 context 已取消仍需能进入并退出）。
	// 3. Stop monitor + debounce：停事件源，杜绝 late fire。
	// 4. 等 collectWg：gate 关闭前已 Add 的 in-flight 采集在此被等到（无 Add/Wait 竞态）。
	a.logger.Info("shutting down analyzer")
	a.gateMu.Lock()
	a.accepting = false
	a.gateMu.Unlock()
	runCancel()
	a.Stop()

	// 等待所有监控 goroutine 退出，且所有 in-flight 采集完成（带超时防挂死）。
	// 必须等 collectWg：debounce.Stop 的 stopTimeout 可能因慢回调超时放弃等待，
	// 若此处不等，Run 返回后外层 defer cleanup() 会关闭 usageDB，残留采集向已关闭 DB 写入丢数据。
	// 30s 远大于常态采集耗时；超时后接受该批次丢失（记录 warning），不无限阻塞。
	done := make(chan struct{})
	go func() {
		a.collectWg.Wait()
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		a.logger.Info("all goroutines and in-flight collections stopped")
	case <-time.After(30 * time.Second):
		a.logger.Warn("timeout waiting for in-flight collections to stop; some data may be lost")
	}

	// monitorErr 与 readyDone 可能同时到达；优先保留初始化失败的完整原因。
	if err := a.readyError(); err != nil {
		return fmt.Errorf("monitor 初始化失败: %w", err)
	}
	return runErr
}

// Stop 优雅停止所有监控（可安全多次调用）
func (a *Analyzer) Stop() {
	a.stopOnce.Do(func() {
		a.gateMu.Lock()
		a.stopped = true
		a.accepting = false
		cancel := a.runCancel
		a.gateMu.Unlock()
		if cancel != nil {
			cancel()
		}
		for _, watcher := range a.jsonlWatchers {
			watcher.Stop()
		}
		for _, poller := range a.sqlitePollers {
			poller.Stop()
		}
	})
}
