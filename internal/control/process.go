// internal/control/process.go
// 平台无关的进程控制编排：Inspect/Start/Stop 与锁内顺序。
// 平台专属的停止信号（SIGTERM/taskkill）在 process_unix.go / process_windows.go。
//
// control lock 与 daemon lock 的协作模型：
//   - control lock（本包）：短期串行化，保证一次 start/stop 的原子性（数百毫秒~秒级）。
//   - daemon lock（daemon 包）：长期持有，证明守护进程在运行。
//
// Inspect 只以 daemon lock 判活；PID 仅用于定位展示。Start/Stop 在 control lock 内
// load config → inspect daemon lock → spawn/stop → wait result。
package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// ---- 公开 API ----

// Inspect 只以 daemon lock 判活，PID 用于定位展示（读不到时 PID=0 不报错）。
// 不加 control lock（只读快照，调用方可在锁外或锁内调）。
func (m *Manager) Inspect(ctx context.Context, cfg *config.Config) (RuntimeState, error) {
	if m == nil {
		return RuntimeState{}, errors.New("进程控制管理器不能为空")
	}
	return m.inspect(ctx, cfg)
}

// Start 在 control lock 内启动守护进程：
//
//	Acquire control lock → load config → inspect daemon lock
//	  → 已运行：返回 AlreadyRunning=true + 准确 PID，不 spawn
//	  → 未运行：清理残留 PID → spawn _run → 等 PID+daemon lock 就绪
//	Release control lock
//
// lock 被持有但 PID 缺失/非法时不 spawn，返回安全错误（不能盲目对未知进程发停止信号）。
// spawn 后超时未就绪 → best-effort kill child 并返回错误。
func (m *Manager) Start(ctx context.Context, load ConfigLoader) (StartResult, error) {
	var res StartResult
	if load == nil {
		return res, errors.New("配置加载函数不能为空")
	}
	err := m.WithLock(ctx, func(s *Session) error {
		cfg, err := load()
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}
		res, err = s.startLocked(ctx, cfg)
		return err
	})
	return res, err
}

// Stop 在 control lock 内停止守护进程：
//
//	Acquire control lock → load config → inspect daemon lock
//	  → 未运行：返回 WasRunning=false（幂等）
//	  → 运行中：按平台停止（macOS bootout → 查 lock → 必要时 SIGTERM；Windows taskkill 准确 PID）
//	    并以 daemon lock 释放作为成功条件；超时返回错误，不删 PID 伪装成功。
//	Release control lock
func (m *Manager) Stop(ctx context.Context, load ConfigLoader) (StopResult, error) {
	var res StopResult
	if load == nil {
		return res, errors.New("配置加载函数不能为空")
	}
	err := m.WithLock(ctx, func(s *Session) error {
		cfg, err := load()
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}
		res, err = s.stopLocked(ctx, cfg)
		return err
	})
	return res, err
}

// Restart 在**单次** control lock Session 内停旧起新：
//
//	Acquire control lock → load config → inspect daemon lock
//	  → 未运行：返回 ErrRestartNotRunning（不 spawn）
//	  → 运行中：stopLocked(oldPID) 等旧 daemon lock 释放
//	            → startLocked() spawn 新 child 等 PID+daemon lock 就绪
//	Release control lock
//
// 关键：必须在同一 Session（单次 control lock）内完成 stop+start。
// 不能调公开 Start/Stop——它们各自 WithLock 会二次加锁死锁；本方法直接复用包内
// startLocked/stopLocked（不加锁的内部方法），由本次 WithLock 统一持锁。
// 全流程不触碰 config/plist/注册表（stop 是 bootout/SIGTERM，start 是 detached spawn）。
func (m *Manager) Restart(ctx context.Context, load ConfigLoader) (RestartResult, error) {
	var res RestartResult
	if load == nil {
		return res, errors.New("配置加载函数不能为空")
	}
	err := m.WithLock(ctx, func(s *Session) error {
		cfg, err := load()
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}

		// 先 Inspect：未运行直接返回 ErrRestartNotRunning（restart 前提是「在运行」）。
		st, err := s.manager.inspect(ctx, cfg)
		if err != nil {
			return err
		}
		if !st.Running {
			return ErrRestartNotRunning
		}
		oldPID := st.PID

		// stopLocked 等旧 daemon lock 释放（包内不加锁，本次 WithLock 已持 control lock）。
		// 失败直接透传，不继续 spawn（避免在旧进程未停干净时启新进程导致冲突）。
		if _, err := s.stopLocked(ctx, cfg); err != nil {
			return err
		}

		// startLocked spawn 新 child 等 ready（包内不加锁，本次 WithLock 已持 control lock）。
		startRes, err := s.startLocked(ctx, cfg)
		if err != nil {
			return err
		}
		res = RestartResult{OldPID: oldPID, NewPID: startRes.PID}
		return nil
	})
	return res, err
}

// ---- Session 内方法（不加 control lock，由公开 API 的 WithLock 持有）----

// Inspect 在 Session 内调用，复用依赖（不加 control lock，只读）。
func (s *Session) Inspect(ctx context.Context, cfg *config.Config) (RuntimeState, error) {
	if s == nil || s.manager == nil {
		return RuntimeState{}, errors.New("进程控制会话不能为空")
	}
	return s.manager.inspect(ctx, cfg)
}

// CleanupStaleMetadata 在 Session 内清理 stale PID/runtime-state（锁内，避免与并发操作竞争）。
func (s *Session) CleanupStaleMetadata(ctx context.Context, dataDir string) error {
	if s == nil || s.manager == nil {
		return errors.New("进程控制会话不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.manager.deps.metadataCleaner.cleanup(dataDir)
}

// Stop 在 control lock 持有期内停止守护进程（供 update 等需要在锁内编排
// Inspect → Stop → install → Start 的流程使用）。
//
// 语义与 stopLocked 完全一致（ready/PID/lease/daemon lock/超时）：
//   - 未运行：幂等返回 nil；
//   - 运行中：按平台停止 + 等 daemon lock 释放；
//   - 超时：返回错误，不删 PID 伪装成功。
//
// 不再获取 control lock——调用方必须在 Manager.WithLock 回调内调用本方法，
// 否则 stop 操作无锁保护。返回值只表达成功/失败（更新流程只需 success/failure），
// StopResult 的 PID/WasRunning 细节被丢弃。
func (s *Session) Stop(ctx context.Context, cfg *config.Config) error {
	if s == nil {
		return errors.New("进程控制会话不能为空")
	}
	if s.released {
		return errSessionReleased
	}
	if s.manager == nil {
		return errors.New("进程控制会话不能为空")
	}
	if cfg == nil {
		return errors.New("有效配置不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.stopLocked(ctx, cfg)
	return err
}

// StartWithExecutable 在 control lock 持有期内用显式 binPath 启动守护进程。
//
// 供 update 替换二进制后的「启动新版本」步骤使用：替换完成后目标二进制可能与当前进程不同，
// 不能探测 os.Executable（会指向旧路径），故要求调用方传入新二进制绝对路径。
// Windows 替换助手运行临时 helper.exe 时尤其不能重新拉起自身。
//
// 语义与 startLocked 完全一致（复用 startLockedWithBinPath 核心的 ready/PID/lease/
// daemon lock/超时编排）。区别仅在于 binPath 来源：本方法用显式路径，startLocked 自动探测。
//
// 不再获取 control lock——调用方必须在 Manager.WithLock 回调内调用本方法。
// 已运行的 daemon 视为成功（幂等，不 spawn）。
func (s *Session) StartWithExecutable(ctx context.Context, cfg *config.Config, binPath string) error {
	if s == nil {
		return errors.New("进程控制会话不能为空")
	}
	if s.released {
		return errSessionReleased
	}
	if s.manager == nil {
		return errors.New("进程控制会话不能为空")
	}
	if cfg == nil {
		return errors.New("有效配置不能为空")
	}
	if binPath == "" {
		return errors.New("可执行文件路径不能为空")
	}
	if !filepath.IsAbs(binPath) {
		return fmt.Errorf("可执行文件路径必须为绝对路径，当前 %q", binPath)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.startLockedWithBinPath(ctx, cfg, binPath)
	return err
}

// ---- 内部编排 ----

// inspect 共享判活 + PID 读取逻辑（Manager/Session 复用）。
//
// 阶段读取：Running 时读 PID 文件得到 (pid, instanceID)，再读 runtime-state。
// 仅当 runtime-state 的 PID+instanceID 与 PID 文件全匹配时填充阶段字段并置
// PhaseAvailable=true；否则（state 缺失/非法/PID 或 instanceID 不匹配/PID 元数据不可用）
// PhaseAvailable=false，调用方据「阶段未知」降级展示，不推翻 daemon lock 的 Running 结论。
// instanceID 仍不参与 Inspect 判活（已运行路径幂等，不要求本次 start 启动）。
func (m *Manager) inspect(ctx context.Context, cfg *config.Config) (RuntimeState, error) {
	if m == nil {
		return RuntimeState{}, errors.New("进程控制管理器不能为空")
	}
	if cfg == nil {
		return RuntimeState{}, errors.New("有效配置不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RuntimeState{}, err
	}
	running := m.deps.daemonLock.isRunning(cfg)
	st := RuntimeState{Running: running}
	if !running {
		return st, nil
	}
	// PID + instanceID 来自 PID 文件；读不到时 PID=0 不报错（调用方据 Running=true 判活）。
	pid, instanceID, _ := m.deps.pidIO.read(cfg)
	if pid > 0 {
		st.PID = pid
	}
	// 无 PID 元数据无法做 state 匹配 → 阶段未知（PhaseAvailable 保持 false）。
	if pid <= 0 || instanceID == "" {
		return st, nil
	}
	// runtime-state 的 PID+instanceID 全匹配时才采信阶段字段（杜绝 stale 旧代/PID 复用）。
	rst, serr := m.deps.stateReader.read(cfg)
	if serr != nil || rst.PID != pid || rst.InstanceID != instanceID {
		return st, nil
	}
	st.InstanceID = rst.InstanceID
	st.MonitorReady = rst.MonitorReady
	st.CatchUp = rst.CatchUp
	st.CatchUpFailures = rst.CatchUpFailures
	st.PhaseAvailable = true
	return st, nil
}

// startLocked 包内不加锁的 start 实现（公开 API 已持 control lock）。
// 自动探测当前可执行文件路径（os.Executable）作为 spawn 目标——供 Manager.Start /
// Manager.Restart 等「启动当前二进制」的场景使用。
func (s *Session) startLocked(ctx context.Context, cfg *config.Config) (StartResult, error) {
	if s == nil || s.manager == nil {
		return StartResult{}, errors.New("进程控制会话不能为空")
	}
	if cfg == nil {
		return StartResult{}, errors.New("有效配置不能为空")
	}
	// 探测当前可执行文件路径（os.Executable）。失败或为空时透传 buildSpawnOptions 的错误。
	bin, err := os.Executable()
	if err != nil {
		return StartResult{}, fmt.Errorf("探测可执行文件路径失败: %w", err)
	}
	return s.startLockedWithBinPath(ctx, cfg, bin)
}

// startLockedWithBinPath 是 start 的共享核心：用显式 binPath 执行 inspect → 清理 → lease →
// spawn → wait ready 全流程。binPath 由调用方负责（startLocked 探测 os.Executable；
// Session.StartWithExecutable 接收更新后的新二进制路径）。
//
// 复用同一核心保证两条路径的 ready/PID/lease/daemon lock/超时语义完全一致——避免两份
// 几乎相同的 spawn+ready 编排各自漂移。
//
// 在 control lock 持有期内执行（由调用方持锁）；不加 control lock。
func (s *Session) startLockedWithBinPath(ctx context.Context, cfg *config.Config, binPath string) (StartResult, error) {
	if s == nil || s.manager == nil {
		return StartResult{}, errors.New("进程控制会话不能为空")
	}
	if cfg == nil {
		return StartResult{}, errors.New("有效配置不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return StartResult{}, err
	}

	st, err := s.manager.inspect(ctx, cfg)
	if err != nil {
		return StartResult{}, err
	}
	if st.Running {
		// lock 持有但 PID 不可读 → 不能盲目 spawn（可能冲突已运行进程），返回安全错误。
		if st.PID == 0 {
			return StartResult{}, errPIDMetadataUnavailable
		}
		return StartResult{PID: st.PID, AlreadyRunning: true}, nil
	}

	// 未运行：daemon lock 已确认无存活实例，可安全清理上代遗留的
	// PID/runtime-state 及其已知 temp，避免 ready 握手误读旧代元数据。
	if err := s.manager.deps.metadataCleaner.cleanup(cfg.DataDir); err != nil {
		return StartResult{}, fmt.Errorf("清理残留运行元数据失败: %w", err)
	}

	// 父进程 lease：在持有 control lock 时创建 lease pipe + instanceID，
	// spawn child 时通过 ExtraFiles/AdditionalInheritedHandles 授权 child。父继续持有
	// control lock + pipe write end，直到 ready 成功（释放 write end）或失败（cleanup）。
	// instanceID 经 managerDependencies.instanceIDGen 生成（生产用 crypto/rand；测试注入确定性值），
	// 作为本次启动代次的握手标识，纳入 ready 六项条件的 PID/runtime-state 匹配。
	gen := s.manager.deps.instanceIDGen
	if gen == nil {
		gen = GenerateInstanceID
	}
	lease, err := newLeaseContext(gen())
	if err != nil {
		return StartResult{}, fmt.Errorf("创建父子 lease 失败: %w", err)
	}

	// 构造层用显式 binPath（不再在此处探测 os.Executable）：startLocked 在调用前已探测；
	// StartWithExecutable 由调用方传入更新后的新二进制路径。
	opts, err := buildSpawnOptionsForBin(cfg, binPath)
	if err != nil {
		lease.cleanup() // spawn 准备失败也清理 lease。
		return StartResult{}, err
	}
	opts.lease = lease

	proc, err := s.manager.deps.spawner.spawn(opts)
	if err != nil {
		lease.cleanup()
		return StartResult{}, fmt.Errorf("启动守护进程失败: %w", err)
	}
	// child 已成功继承 read end；父进程立即关闭自己的副本，只保留 write end 到 ready。
	// read end 的所有权由 control 层单点管理，避免 spawn helper 先关、失败清理再按已复用
	// 的 fd/handle 数值二次关闭。
	lease.closeRead()
	childPID := proc.PID()

	// 等 child 就绪：六项 ready 条件全部成立。
	// 用 lease.instanceID 作为本次启动代次的握手标识（spawn 前生成、传给 child）。
	if err := s.manager.waitForStartReady(ctx, cfg, childPID, lease.instanceID); err != nil {
		// 超时：proc 必是我们 spawn 的 child，尽力终止避免 detached 孤儿继续持有 daemon lock
		// 导致后续 start 的 Inspect 误判「已在运行」。
		killErr := proc.Kill()
		// 失败清理：只有 PID/instanceID 仍属于本次 child 时才清 PID/runtime-state，
		// 且 daemon lock 已确认释放时才执行。Kill 是异步 best-effort，不能在进程仍持锁
		// （或 Kill 失败）时删掉活进程的元数据并伪装成已停止；若此刻尚未释放，
		// 元数据留给进程正常退出或下次 start 在确认无运行实例后清理。
		var cleanupErr error
		if !s.manager.deps.daemonLock.isRunning(cfg) &&
			s.manager.startReadyOwnershipOurs(cfg, childPID, lease.instanceID) {
			cleanupErr = s.manager.deps.metadataCleaner.cleanup(cfg.DataDir)
		}
		_ = proc.Release()
		lease.cleanup()
		timeoutErr := fmt.Errorf("守护进程启动超时，请检查 %s/daemon.err.log: %w", cfg.DataDir, err)
		if killErr != nil {
			killErr = fmt.Errorf("终止未就绪子进程 PID %d 失败: %w", childPID, killErr)
		}
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("清理未就绪子进程元数据失败: %w", cleanupErr)
		}
		return StartResult{}, errors.Join(timeoutErr, killErr, cleanupErr)
	}

	// 确认就绪后：释放 lease write end（child 已接管，EOF 只表示父命令结束）+ 放弃 wait 权。
	lease.closeWrite()
	_ = proc.Release()
	return StartResult{PID: childPID}, nil
}

// waitForStartReady 轮询六项 ready 条件，全部成立才返回 nil；超时返回错误。
//
// ready 条件：
//  1. PID 文件 PID == child PID
//  2. PID 文件 instanceID == 本次 instanceID
//  3. daemon lock 已持有
//  4. runtime-state PID == child PID
//  5. runtime-state instanceID == 本次 instanceID
//  6. runtime-state monitor_ready == true
//
// stale/旧代/短暂缺失/解析失败只继续有界重试，绝不误判 ready（reader 降级不当 ready）。
// start 不等待 catch-up 完成（只要 monitor_ready=true 即可，无论 catch_up 是 pending/running/succeeded/failed）。
//
// 用 deps.now/deps.sleep 驱动轮询：生产是真实 wall clock，测试用 fake clock 确定性推进。
func (m *Manager) waitForStartReady(ctx context.Context, cfg *config.Config, expectPID int, expectInstanceID string) error {
	timeout := m.deps.startReadyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	interval := m.deps.pollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	deadline := m.deps.now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.startReadyConditionsMet(cfg, expectPID, expectInstanceID) {
			return nil
		}
		if !m.deps.now().Before(deadline) {
			return fmt.Errorf("等待守护进程就绪超时（期望 PID %d instanceID %s）", expectPID, expectInstanceID)
		}
		m.deps.sleep(interval)
	}
}

// startReadyConditionsMet 检查六项 ready 条件是否同时成立（不轮询、不看 deadline）。
// 任何一项缺失/读取失败/不匹配都返回 false（绝不把降级当 ready）。
func (m *Manager) startReadyConditionsMet(cfg *config.Config, expectPID int, expectInstanceID string) bool {
	// 条件 1+2：PID 文件 PID+instanceID 匹配。
	pid, inst, perr := m.deps.pidIO.read(cfg)
	if perr != nil || pid != expectPID || inst != expectInstanceID {
		return false
	}
	// 条件 3：daemon lock 已持有。
	if !m.deps.daemonLock.isRunning(cfg) {
		return false
	}
	// 条件 4+5+6：runtime-state PID+instanceID 匹配且 monitor_ready=true。
	st, serr := m.deps.stateReader.read(cfg)
	if serr != nil || st.PID != expectPID || st.InstanceID != expectInstanceID || !st.MonitorReady {
		return false
	}
	return true
}

// startReadyOwnershipOurs 超时后重新核对所有权：PID 文件或 runtime-state 中任一仍记录
// 本次 child 的 (PID, instanceID) 时返回 true，调用方据此决定是否尽力终止+清理。
//
// 语义：只要任一文件的归属仍指向本次 child，就视为「这是本次 spawn 出的进程」，
// 可以安全 kill + 清理。二者都已不指向本次 child（可能被并发 start 覆盖、或 child 已退出
// 被他代复用 PID）时返回 false，不误杀其他代次。
func (m *Manager) startReadyOwnershipOurs(cfg *config.Config, expectPID int, expectInstanceID string) bool {
	pid, inst, perr := m.deps.pidIO.read(cfg)
	if perr == nil && pid == expectPID && inst == expectInstanceID {
		return true
	}
	if st, serr := m.deps.stateReader.read(cfg); serr == nil && st.PID == expectPID && st.InstanceID == expectInstanceID {
		return true
	}
	return false
}

// stopLocked 包内不加锁的 stop 实现（公开 API 已持 control lock）。
func (s *Session) stopLocked(ctx context.Context, cfg *config.Config) (StopResult, error) {
	if s == nil || s.manager == nil {
		return StopResult{}, errors.New("进程控制会话不能为空")
	}
	if cfg == nil {
		return StopResult{}, errors.New("有效配置不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return StopResult{}, err
	}

	st, err := s.manager.inspect(ctx, cfg)
	if err != nil {
		return StopResult{}, err
	}
	if !st.Running {
		// daemon lock 已确认未运行，可幂等清理任意 stale PID/runtime-state。
		if err := s.manager.deps.metadataCleaner.cleanup(cfg.DataDir); err != nil {
			return StopResult{WasRunning: false}, fmt.Errorf("清理残留运行元数据失败: %w", err)
		}
		return StopResult{WasRunning: false}, nil
	}

	pid := st.PID
	if pid == 0 {
		// lock 持有但 PID 不可读：无法安全向准确进程发停止信号。
		return StopResult{}, errPIDMetadataUnavailable
	}

	if err := s.manager.stopDaemonByPlatform(ctx, cfg, pid); err != nil {
		return StopResult{PID: pid, WasRunning: true}, err
	}

	// 以 daemon lock 释放作为成功条件；超时返回错误，不删 PID 伪装成功。
	if err := s.manager.waitDaemonRelease(ctx, cfg); err != nil {
		return StopResult{PID: pid, WasRunning: true}, err
	}
	// lock 已释放后才允许清理强杀或异常退出遗留的双文件元数据。
	if err := s.manager.deps.metadataCleaner.cleanup(cfg.DataDir); err != nil {
		return StopResult{PID: pid, WasRunning: true}, fmt.Errorf("守护进程已停止，但清理运行元数据失败: %w", err)
	}

	return StopResult{PID: pid, WasRunning: true}, nil
}

// stopDaemonByPlatform 按平台发送停止信号（不加 control lock）。
// macOS：先 best-effort bootout 当前 job（保留 plist），「job 未加载」视为可继续，
//
//	其他 launchctl 错误必须返回；随后若 daemon lock 仍持有再向准确 PID 发 SIGTERM。
//
// POSIX 手工 daemon（未托管）：直接对准确 PID 发 SIGTERM。
// Windows：只对准确 PID 调 taskkill /PID <pid> /F。
func (m *Manager) stopDaemonByPlatform(ctx context.Context, cfg *config.Config, pid int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := buildServiceOptions(cfg)
	if m.deps.serviceMgr.platform() == "launchd" {
		// macOS 无条件尝试 bootout 当前 job（保留定义）。是否 loaded 与 plist 是否存在
		// 都不是进程存活真相；StopCurrent 对「job 未加载」幂等返回 nil。
		if err := m.deps.serviceMgr.stopCurrent(opts); err != nil {
			return fmt.Errorf("停止托管守护进程失败: %w", err)
		}
		// bootout 后查 lock；仍持有说明 bootout 未停掉手工 daemon（plist 存在但 job 未加载场景），
		// 需对准确 PID 发 SIGTERM 补刀。
		if m.deps.daemonLock.isRunning(cfg) {
			if err := m.deps.processKill.terminate(pid); err != nil {
				if !m.deps.daemonLock.isRunning(cfg) {
					return nil
				}
				return fmt.Errorf("发送停止信号失败: %w", err)
			}
		}
		return nil
	}
	// Windows 与普通 POSIX：直接对 Inspect 得到的准确 PID 发停止信号。
	if err := m.deps.processKill.terminate(pid); err != nil {
		if !m.deps.daemonLock.isRunning(cfg) {
			return nil
		}
		return fmt.Errorf("发送停止信号失败: %w", err)
	}
	return nil
}

// waitDaemonRelease 轮询 daemon lock 直到释放或超时。
// 用 deps.now/deps.sleep 驱动轮询：生产是真实 wall clock，测试用 fake clock 确定性推进。
// 超时返回 errDaemonStillRunning，调用方据超时不删 PID 伪装成功。
func (m *Manager) waitDaemonRelease(ctx context.Context, cfg *config.Config) error {
	timeout := m.deps.stopWaitTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	interval := m.deps.pollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	deadline := m.deps.now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.deps.daemonLock.isRunning(cfg) {
			return nil
		}
		if !m.deps.now().Before(deadline) {
			return errDaemonStillRunning
		}
		m.deps.sleep(interval)
	}
}

// ---- 生产依赖装配 ----

// productionDaemonLock 包装 daemon.IsDaemonRunning 做判活（不抢锁、仅检测）。
type productionDaemonLock struct{}

func (productionDaemonLock) isRunning(cfg *config.Config) bool {
	return cfg != nil && daemon.IsDaemonRunning(filepath.Join(cfg.DataDir, "token-usage.lock"))
}

// productionPIDIO 直接读写文件系统的 PID 文件实现。
type productionPIDIO struct{}

// read 经 runmeta.ReadPIDFile 读 PID 文件，返回 (pid, instanceID, err)。
// 新格式 "<pid> <instanceID>" 同时返回二者；旧格式 "<pid>" 返回 (pid, "", nil)。
// ready 握手与 stop 的信号目标都用它；Inspect 只取 PID。
func (productionPIDIO) read(cfg *config.Config) (int, string, error) {
	pid, instanceID, err := runmeta.ReadPIDFile(filepath.Join(cfg.DataDir, "token-usage.pid"))
	if err != nil {
		return 0, "", err
	}
	return pid, instanceID, nil
}

// productionStateReader 直接读文件系统的 runtime-state 实现。
type productionStateReader struct{}

func (productionStateReader) read(cfg *config.Config) (runmeta.RuntimeState, error) {
	return runmeta.ReadRuntimeState(filepath.Join(cfg.DataDir, "token-usage.runtime.json"))
}

func (productionPIDIO) write(cfg *config.Config, pid int) error {
	// 经 runmeta.WritePIDFile：新格式 "<pid> <instanceID>" + ReplaceCompleteFile 原子写。
	// 此处是 control 侧的 best-effort 写入（如迁移/调试），instanceID 留空；
	// 正式 PID 写入由 daemon.Run 经 opts.InstanceID 完成。
	return runmeta.WritePIDFile(filepath.Join(cfg.DataDir, "token-usage.pid"), pid, "")
}

func (productionPIDIO) remove(cfg *config.Config) error {
	return os.Remove(filepath.Join(cfg.DataDir, "token-usage.pid"))
}

// productionSpawner 包装 daemon.SpawnDetached，把 *exec.Cmd 适配为 spawnedProcess。
type productionSpawner struct{}

func (productionSpawner) spawn(opts spawnOptions) (spawnedProcess, error) {
	dopts := daemon.SpawnOptions{
		BinPath:    opts.BinPath,
		Args:       opts.Args,
		StdoutPath: opts.StdoutPath,
		StderrPath: opts.StderrPath,
	}
	// 父进程 lease 上下文 → daemon.LeaseSpawnInput。
	if opts.lease != nil {
		dopts.Lease = &daemon.LeaseSpawnInput{
			InstanceID: opts.lease.instanceID,
			Reader:     opts.lease.readerForDaemon(),
		}
	}
	cmd, err := daemon.SpawnDetached(dopts)
	if err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

// execProcess 适配 *exec.Cmd 到 spawnedProcess。
type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) PID() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *execProcess) Release() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Release()
}

// productionMetadataCleaner 清理 stale PID/runtime-state 文件。
type productionMetadataCleaner struct{}

func (productionMetadataCleaner) cleanup(dataDir string) error {
	// 委托 runmeta.CleanupStaleMetadata：清 PID + runtime-state + 两类精确 temp。
	// 前置条件不变（调用方 Session.CleanupStaleMetadata 在 control lock 内、
	// 已确认 daemon lock 未持有后才调本实现）。
	return runmeta.CleanupStaleMetadata(dataDir)
}

// productionServiceMgr 由 serviceAdapter 实现（在 process_service.go，包装 service.Manager，
// 避免本文件直接 import service）。

// buildSpawnOptions 是包装层：探测当前可执行文件路径（os.Executable）后委托给构造层。
// 供 startLocked / Manager.Start / Manager.Restart 等沿用「当前二进制」的路径使用——
// 这些场景下被启动的 daemon 就是当前进程对应的二进制，故自动探测即可。
func buildSpawnOptions(cfg *config.Config) (spawnOptions, error) {
	if cfg == nil {
		return spawnOptions{}, errors.New("有效配置不能为空")
	}
	bin, err := os.Executable()
	if err != nil {
		return spawnOptions{}, fmt.Errorf("探测可执行文件路径失败: %w", err)
	}
	if bin == "" {
		return spawnOptions{}, errors.New("当前可执行文件路径为空")
	}
	return buildSpawnOptionsForBin(cfg, bin)
}

// buildSpawnOptionsForBin 是构造层：用显式 binPath 组装 spawnOptions（Args 固定 ["_run"]，
// 日志重定向到 DataDir）。不调用 os.Executable，binPath 由调用方负责。
//
// 供更新替换后显式启动「新二进制」的流程使用（Session.StartWithExecutable）——
// 替换完成后目标二进制可能与当前进程不同，不能再探测 os.Executable（会指向旧路径）；
// Windows 替换助手运行临时 helper.exe 时尤其不能重新拉起自身。故要求调用方传入绝对路径。
func buildSpawnOptionsForBin(cfg *config.Config, binPath string) (spawnOptions, error) {
	if cfg == nil {
		return spawnOptions{}, errors.New("有效配置不能为空")
	}
	if binPath == "" {
		return spawnOptions{}, errors.New("可执行文件路径不能为空")
	}
	if !filepath.IsAbs(binPath) {
		return spawnOptions{}, fmt.Errorf("可执行文件路径必须为绝对路径，当前 %q", binPath)
	}
	return spawnOptions{
		BinPath:    binPath,
		Args:       []string{"_run"},
		StdoutPath: filepath.Join(cfg.DataDir, "daemon.out.log"),
		StderrPath: filepath.Join(cfg.DataDir, "daemon.err.log"),
	}, nil
}

// buildServiceOptions 构造 serviceOptions（自启服务检测用）。Label 与 Args 固定。
func buildServiceOptions(cfg *config.Config) serviceOptions {
	bin, _ := os.Executable()
	return serviceOptions{
		Label:   daemonServiceLabel,
		BinPath: bin,
		DataDir: cfg.DataDir,
		Args:    []string{"_run"},
	}
}
