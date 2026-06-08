// Package control 封装进程控制层：在固定路径 (~/.token-usage/token-usage.control.lock)
// 上做短期串行化，保证配置落地与 start/stop/restart 的原子性。
//
// control lock 与 daemon lock (<data_dir>/token-usage.lock) 是不同概念：
//   - daemon lock 长期持有，证明守护进程在运行；
//   - control lock 仅在一次操作（数百毫秒~秒级）内持有，避免 CLI 并发改写状态。
//
// 本文件定义 control lock 基础设施，以及 Inspect/Start/Stop/Restart 共用的类型契约。
package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// ---- 对外错误（typed，供 errors.Is 判断）----

// ErrControlLockTimeout 在 WithLock 等待 control lock 超过 controlLockTimeout 时返回。
var ErrControlLockTimeout = errors.New("等待进程控制锁超时")

// ErrRestartNotRunning 在 restart 时守护进程未运行（未持有 daemon lock）时返回。
var ErrRestartNotRunning = errors.New("守护进程未运行，请使用 token-usage start")

// errNonAbsoluteHome NewManager 校验 home 时的内部错误，便于包内测试断言。
var errNonAbsoluteHome = errors.New("home 必须是绝对路径")

// ---- 类型定义 ----

// ConfigLoader 延迟加载原始用户配置。Start/Stop/Restart 在 control lock 内调用它，
// 避免在加锁前就提前读配置（防止 CLI 流程外加载导致的不一致）。
type ConfigLoader func() (*config.Config, error)

// RuntimeState 控制层快照：daemon lock 判活结果 + runmeta 读取的运行态。
type RuntimeState struct {
	Running         bool
	PID             int
	InstanceID      string
	MonitorReady    bool
	CatchUp         string
	CatchUpFailures int
	// PhaseAvailable 表示 MonitorReady/CatchUp/CatchUpFailures/InstanceID 已填充且可信：
	// 仅当 runtime-state 的 PID+instanceID 与 PID 文件全匹配时为 true。
	// 缺失/解析失败/不匹配/无 PID 元数据时为 false——调用方据此降级显示「启动阶段未知」，
	// 不推翻 daemon lock 的 Running 结论。阶段信息不参与 autostart 漂移判断。
	PhaseAvailable bool
}

// StartResult 是 start 的返回。
type StartResult struct {
	PID            int
	AlreadyRunning bool
}

// StopResult 是 stop 的返回。
type StopResult struct {
	PID        int
	WasRunning bool
}

// RestartResult 是 restart 的返回。
type RestartResult struct {
	OldPID int
	NewPID int
}

// ---- 常量 ----

const (
	// controlLockTimeout control lock 的最长等待时间。15 秒覆盖用户连续敲两条命令的
	// 极端场景，同时避免无界等待挂死 CLI。
	controlLockTimeout = 15 * time.Second
	// controlPollInterval 抢锁失败后的轮询间隔。100ms 在交互式 CLI 体感无感，
	// 又足够稀疏以避免空转。
	controlPollInterval = 100 * time.Millisecond
)

// ---- 路径 ----

// ControlLockPath 返回 control lock 固定路径 <home>/.token-usage/token-usage.control.lock。
// 路径仅由 home 决定，与 data_dir（可迁移）无关：这是为了把控制信号与数据目录解耦，
// 即使用户迁移 data_dir，control lock 仍稳定。
func ControlLockPath(home string) string {
	return filepath.Join(home, ".token-usage", "token-usage.control.lock")
}

// ---- Manager ----

// controlLocker 抽象文件锁，便于包内测试注入 fake；生产实现包装 gofrs/flock。
type controlLocker interface {
	tryLock() (acquired bool, err error)
	unlock() error
}

// flockLocker 生产用 controlLocker：每次 tryLock 调 flock 的非阻塞 TryLock。
type flockLocker struct {
	fl *flock.Flock
}

func (l *flockLocker) tryLock() (bool, error) {
	return l.fl.TryLock()
}

func (l *flockLocker) unlock() error {
	return l.fl.Unlock()
}

// managerDependencies 可注入的依赖。生产用真实 time.Now/time.Sleep + flock；
// 包内测试用私有构造器注入 fake locker/clock，从而做到无真实 sleep 的确定性测试。
//
// spawn/process/pid/daemonlock/metadata/service 均可注入，使 Inspect/Start/Stop
// 能在包内被 fake 驱动，做到无真实进程、无真实文件 IO 的确定性测试。
type managerDependencies struct {
	now       func() time.Time
	sleep     func(time.Duration)
	newLocker func() controlLocker // 每次新建一个 locker 实例（与 WithLock 生命周期绑定）

	// daemonLock 判活（生产用 daemon.IsDaemonRunning）。Inspect/Start/Stop 共用，
	// 保证「只以 daemon lock 判活」语义在 control 层唯一。
	daemonLock daemonLockJudge
	// pidIO 读写/清理 PID 文件（生产用 runmeta.ReadPIDFile + 文件 IO）。
	// read 返回 (pid, instanceID, err)：instanceID 是 PID 文件新格式的第二字段，
	// 旧格式 "<pid>" 时为空。ready 握手据此判本次启动代次。
	pidIO pidIO
	// stateReader 读 runtime-state（生产用 runmeta.ReadRuntimeState）。
	// ready 握手要求 runtime-state 的 PID/instanceID 与本次 child 匹配且 monitor_ready=true。
	stateReader runtimeStateReader
	// spawner 拉起 _run 子进程（生产用 daemon.SpawnDetached）。
	spawner spawner
	// processKill 按平台向准确 PID 发停止信号（POSIX SIGTERM / Windows taskkill）。
	processKill processSignaler
	// metadataCleaner 清理 stale PID/runtime-state（生产用文件操作）。
	metadataCleaner staleMetadataCleaner
	// serviceMgr 自启服务管理器（macOS bootout 用），可注入。
	serviceMgr serviceManagerLike
	// startReadyTimeout start 等 child 就绪的最大时长。start 在此超时内轮询 PID + daemon lock。
	startReadyTimeout time.Duration
	// stopWaitTimeout stop 等待 daemon lock 释放的最大时长。
	stopWaitTimeout time.Duration
	// now/poll 内部辅助：轮询间隔。生产与测试共用（测试用 fake clock 推进）。
	pollInterval time.Duration
	// instanceIDGen 生成一次性守护进程实例标识。生产用 GenerateInstanceID；
	// 测试注入确定性值，便于 ready 握手用可预测的 instanceID 匹配 PID/runtime-state。
	instanceIDGen func() string
}

// ---- 依赖接口（均为包内私有，生产实现由 process.go 装配）----

// daemonLockJudge 仅以 daemon lock 判活。不抢锁、不探测进程，避免与 control lock 形成嵌套。
// 生产实现包装 daemon.IsDaemonRunning(lockPath)。
type daemonLockJudge interface {
	isRunning(cfg *config.Config) bool
}

// pidIO 抽象 PID 文件的读写与清理。生产实现直接用文件系统。
// read 返回 (pid, instanceID, err)：新格式 "<pid> <instanceID>" 同时返回二者，
// 旧格式 "<pid>" 返回 (pid, "", nil)。ready 握手与 stop 的信号目标都用它。
type pidIO interface {
	read(cfg *config.Config) (pid int, instanceID string, err error)
	write(cfg *config.Config, pid int) error
	remove(cfg *config.Config) error
}

// runtimeStateReader 抽象 runtime-state 读取。生产实现用 runmeta.ReadRuntimeState。
// ready 握手校验 runtime-state 的 PID/instanceID 与本次 child 匹配且 monitor_ready=true；
// 缺失/解析失败返回错误，调用方据「绝不把降级当 ready」语义只重试，不误判就绪。
type runtimeStateReader interface {
	read(cfg *config.Config) (runmeta.RuntimeState, error)
}

// spawnedProcess 抽象已 spawn 的子进程句柄。生产实现包装 *exec.Cmd。
type spawnedProcess interface {
	PID() int
	Kill() error    // best-effort 终止（start 超时清孤儿子进程用）
	Release() error // 放弃 wait 权（detached 子进程由系统收养）
}

// spawner 拉起 _run 子进程。生产实现包装 daemon.SpawnDetached。
type spawner interface {
	spawn(opts spawnOptions) (spawnedProcess, error)
}

// spawnOptions 跨平台共用的 spawn 参数（镜像 daemon.SpawnOptions，但属 control 包，
// 避免 control 直接依赖 *exec.Cmd 类型）。
type spawnOptions struct {
	BinPath    string
	Args       []string
	StdoutPath string
	StderrPath string
	// lease 是父进程 control lease 上下文。nil=无父 lease（独立加锁路径，不应出现在 start）。
	// startLocked 创建 lease 并传入；productionSpawner 把它转成 daemon.LeaseSpawnInput。
	lease *leaseContext
}

// processSignaler 按平台向准确 PID 发停止信号。POSIX=SIGTERM，Windows=taskkill /PID。
type processSignaler interface {
	terminate(pid int) error
}

// staleMetadataCleaner 清理 stale PID/runtime-state 文件。
type staleMetadataCleaner interface {
	cleanup(dataDir string) error
}

// serviceOptions 镜像 service.Options 的子集，control 包内独立定义，避免引入 service 包到接口签名。
// serviceManagerLike 把 control 与 service 包的耦合限制在生产装配点（process.go）。
type serviceOptions struct {
	Label   string
	BinPath string
	DataDir string
	Args    []string
}

// serviceManagerLike control 需要的 service.Manager 子集。
// platform 只用于区分 launchd（必须先 bootout）与其他平台；Windows/普通 POSIX
// 都由 control 已掌握的准确 PID 直接停止，不能重新按定义或进程名推断。
type serviceManagerLike interface {
	platform() string
	stopCurrent(opts serviceOptions) error
}

// ---- 包内错误（typed，便于 errors.Is）----

// errDaemonStillRunning stop 等待 daemon lock 释放超时时返回（语义：daemon 仍在运行）。
var errDaemonStillRunning = errors.New("守护进程仍在运行")

// errNoPIDFile PID 文件不存在（read 失败的可识别原因之一）。
var errNoPIDFile = errors.New("PID 文件不存在")

// errPIDInvalid PID 文件格式非法（read 失败的可识别原因之二）。
var errPIDInvalid = errors.New("PID 文件格式无效")

// errPIDMetadataUnavailable lock 持有但 PID 元数据不可用，无法安全 spawn/stop（安全错误）。
var errPIDMetadataUnavailable = errors.New("守护进程正在运行但 PID 元数据不可用")

// Manager 进程控制管理器。home 在 CLI bootstrap 时解析并传入；不可变。
type Manager struct {
	home string
	deps managerDependencies
}

// Session 一次 control lock 持有期内的操作上下文。WithLock 创建并在结束时释放。
type Session struct {
	manager *Manager
	locker  controlLocker
	// released 保证 release 幂等（即使 fn 返回错误、panic recover、或显式多次调用）。
	released bool
}

// NewManager 创建生产用 Manager。
//   - home 必须非空且为绝对路径；
//   - 创建固定配置目录 <home>/.token-usage/，失败直接返回（不退回 data_dir 或当前目录）。
func NewManager(home string) (*Manager, error) {
	if home == "" {
		return nil, errNonAbsoluteHome
	}
	if !filepath.IsAbs(home) {
		return nil, errNonAbsoluteHome
	}

	configDir := filepath.Join(home, ".token-usage")
	// 0755：目录需可进入；文件权限由具体写入方按需收紧。
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建配置目录 %q 失败: %w", configDir, err)
	}

	lockPath := ControlLockPath(home)
	return &Manager{
		home: home,
		deps: managerDependencies{
			now:       time.Now,
			sleep:     time.Sleep,
			newLocker: func() controlLocker { return &flockLocker{fl: flock.New(lockPath)} },
			// 生产依赖装配在 productionDeps()（process.go），保证 lock.go 不直接 import
			// daemon/service，便于本文件聚焦 lock 基础设施。
			daemonLock:        productionDaemonLock{},
			pidIO:             productionPIDIO{},
			stateReader:       productionStateReader{},
			spawner:           productionSpawner{},
			processKill:       newProductionProcessSignaler(),
			metadataCleaner:   productionMetadataCleaner{},
			serviceMgr:        newProductionServiceMgr(),
			startReadyTimeout: 5 * time.Second,
			stopWaitTimeout:   5 * time.Second,
			pollInterval:      100 * time.Millisecond,
			instanceIDGen:     GenerateInstanceID,
		},
	}, nil
}

// ConfigHome 返回固定配置目录 <home>/.token-usage。
func (m *Manager) ConfigHome() string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.home, ".token-usage")
}

// WithLock 获取 control lock 后在锁内执行 fn，结束时释放。
//
// 语义：
//   - 默认等待 controlLockTimeout（15s），以 controlPollInterval（100ms）轮询；
//   - 抢锁期间若 context 被主动取消（context.Canceled），立即返回 ctx.Err()
//     （即使已超 deadline）：主动取消表示用户中断，必须让调用方区分取消与超时；
//   - 抢锁期间因超时而放弃（自身 controlLockTimeout 到期 或 传入 context 的
//     DeadlineExceeded，二者取先者）返回 ErrControlLockTimeout，便于调用方用
//     errors.Is(err, ErrControlLockTimeout) 统一判断「等待超时」；
//   - fn 的返回值透传，无论 fn 是否出错都释放锁（release 幂等）。
//
// 「超时统一映射到 ErrControlLockTimeout」是关键：调用方传入带 deadline 的 context
// （如调用方显式传入更短 deadline）时，context 先到期也会被识别为超时放弃，
// 不会因返回 context.DeadlineExceeded 而错过降级/launchd 防护分支。
// 主动取消（Ctrl+C）仍返回 ctx.Err()，保证可区分。
//
// 测试通过注入 fake clock 推进虚拟时间，杜绝真实 time.Sleep。
func (m *Manager) WithLock(ctx context.Context, fn func(*Session) error) (retErr error) {
	if m == nil {
		return errors.New("进程控制管理器不能为空")
	}
	if fn == nil {
		return errors.New("进程控制锁回调不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := m.deps.now().Add(controlLockTimeout)

	for {
		// context 状态优先检查：主动取消返回 ctx.Err()，超时（DeadlineExceeded）
		// 映射到 ErrControlLockTimeout（无论来自自身 deadline 还是传入 context）。
		if err := lockWaitError(ctx); err != nil {
			return err
		}

		locker := m.deps.newLocker()
		acquired, err := locker.tryLock()
		if err != nil {
			return fmt.Errorf("获取进程控制锁失败: %w", err)
		}
		if acquired {
			sess := &Session{manager: m, locker: locker}
			defer func() {
				retErr = errors.Join(retErr, sess.release())
			}()
			return fn(sess)
		}

		// 自身 controlLockTimeout 超时：映射到 ErrControlLockTimeout。
		if !m.deps.now().Before(deadline) {
			return ErrControlLockTimeout
		}

		m.deps.sleep(controlPollInterval)
	}
}

// lockWaitError 把抢锁等待期间的 context 状态统一成返回错误：
//   - nil：context 未结束，可继续抢锁；
//   - context.Canceled：主动取消（如用户 Ctrl+C），原样返回，调用方可区分取消与超时；
//   - context.DeadlineExceeded：超时（context 自带 deadline 到期），同时包装
//     ErrControlLockTimeout 与底层 context 错误，使 errors.Is(err, ErrControlLockTimeout)
//     与 errors.Is(err, context.DeadlineExceeded) 都成立。
//
// 这样调用方传入带 deadline 的 context 时，deadline 到期被视为「超时放弃」，
// 与自身 controlLockTimeout 超时走同一分支，避免降级/防护分支因错误类型不匹配而漏触发；
// 同时保留底层 context 错误供需要时检查。Go 1.20+ 的多 %w 动词让两个错误都能被
// errors.Is 命中。
func lockWaitError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	switch ctx.Err() {
	case context.Canceled:
		// 主动取消：原样返回，保证可区分（取消优先于超时）。
		return ctx.Err()
	case context.DeadlineExceeded:
		// 超时：统一映射到 ErrControlLockTimeout，同时 wrap 底层 context 错误。
		return fmt.Errorf("%w: %w", ErrControlLockTimeout, ctx.Err())
	default:
		return nil
	}
}

// release 释放 control lock，幂等。Session 由 WithLock 创建并唯一持有，
// 重复调用（含 fn 出错路径）安全。
func (s *Session) release() error {
	if s == nil || s.released {
		return nil
	}
	s.released = true
	if s.locker != nil {
		if err := s.locker.unlock(); err != nil {
			return fmt.Errorf("释放进程控制锁失败: %w", err)
		}
	}
	return nil
}

// Close 释放 Session 持有的 control lock（幂等）。供需要跨多次外部调用持有 control lock 的
// 调用方使用（例如独立 _run：在 daemon lock commit 回调里显式释放 control lock）。
// 与 WithLock 的自动释放互补：WithLock 适合「锁内完成所有工作」的同步流程，
// Close 适合「锁跨越外部阻塞调用、需在事件回调中释放」的异步流程。
func (s *Session) Close() error {
	return s.release()
}

// AcquireLock 获取 control lock 并返回持有该锁的 Session（调用方负责 Close）。
// 语义与 WithLock 的获取阶段一致：
//   - context 被主动取消（Canceled）返回 ctx.Err()；
//   - 超时（自身 controlLockTimeout 到期 或 传入 context 的 DeadlineExceeded，取先者）
//     返回 ErrControlLockTimeout。
//
// 供独立 _run 这类「获取锁后跨越外部阻塞调用、由事件回调显式释放」的流程使用。
// 「超时统一映射 ErrControlLockTimeout」让 _run 用 errors.Is 判断超时放弃时，
// 无论自身 deadline 还是传入 context deadline 先到期都能正确触发降级/launchd 防护分支。
// 普通同步流程应优先用 WithLock，避免忘记 Close。
func (m *Manager) AcquireLock(ctx context.Context) (*Session, error) {
	if m == nil {
		return nil, errors.New("进程控制管理器不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := m.deps.now().Add(controlLockTimeout)
	for {
		if err := lockWaitError(ctx); err != nil {
			return nil, err
		}
		locker := m.deps.newLocker()
		acquired, err := locker.tryLock()
		if err != nil {
			return nil, fmt.Errorf("获取进程控制锁失败: %w", err)
		}
		if acquired {
			return &Session{manager: m, locker: locker}, nil
		}
		if !m.deps.now().Before(deadline) {
			return nil, ErrControlLockTimeout
		}
		m.deps.sleep(controlPollInterval)
	}
}
