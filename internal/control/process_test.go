// internal/control/process_test.go
package control

import (
	"context"
	"errors"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// errNoRuntimeState runtime-state 文件不存在（fakeStateReader 默认，模拟 ready 前 state 缺失）。
var errNoRuntimeState = errors.New("runtime-state 文件不存在")

// ---- 测试 fakes（确定性：无真实 sleep；状态切换由「触发 → 下一次轮询就绪」驱动）----
//
// 设计：waitForStartReady/waitDaemonRelease 通过 deps.sleep 轮询；fakeClock.sleep 既推进虚拟时间
// （驱动 deadline），又通过 runtime.Gosched() 让出 CPU 让测试线程在状态切换前完成。
// 状态切换由 spawn / stopDaemonByPlatform 调用触发——fake 在下一次轮询读到就绪结果，无需 channel。

// fakeDaemonLock 实现 daemonLockJudge。
//   - running：初始判活结果。
//   - readyAfter：isRunning 在第 N 次（>readyAfter）调用后返回 runningWhenReady。
//     用于 start：spawn 后变 ready；用于 stop：bootout 后变 not-running。
type fakeDaemonLock struct {
	mu               sync.Mutex
	running          bool
	checks           int
	readyAfter       int
	runningWhenReady bool
}

func newFakeDaemonLock(running bool) *fakeDaemonLock {
	return &fakeDaemonLock{running: running}
}

func (f *fakeDaemonLock) isRunning(cfg *config.Config) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.readyAfter > 0 && f.checks > f.readyAfter {
		return f.runningWhenReady
	}
	return f.running
}

func (f *fakeDaemonLock) setRunning(running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = running
}

// fakePIDIO 实现 pidReader/writer/cleaner。read 在 readyAfter 次后返回 readyPID/readyInstance（模拟 child 写 PID）。
//   - readyAfter=-1（默认）：禁用 ready 分支，按 pid/instance 字段返回（缺文件返回 errNoPIDFile）。
//   - readyAfter>=0：read 第 (readyAfter+1) 次起返回 readyPID/readyInstance。
type fakePIDIO struct {
	mu            sync.Mutex
	pid           int
	instance      string // 非 ready 分支返回的 instanceID（inspect 路径用）
	bad           bool
	readErr       error
	reads         int
	writes        []int
	removes       int
	readyAfter    int
	readyPID      int
	readyInstance string // ready 分支返回的 instanceID（child 写入的本次代次）
}

// newFakePIDIO 构造默认禁用 ready 分支的 fake（readyAfter=-1）。
func newFakePIDIO() *fakePIDIO {
	return &fakePIDIO{readyAfter: -1}
}

func (f *fakePIDIO) read(cfg *config.Config) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return 0, "", f.readErr
	}
	// readyAfter>=0 表示「read 第 (readyAfter+1) 次起返回 readyPID/readyInstance」。
	// readyAfter=0：第 1 次即返回 readyPID（child spawn 后立即可读）。
	// 默认 readyAfter=-1（禁用此分支）。
	if f.readyAfter >= 0 && f.reads > f.readyAfter {
		f.pid = f.readyPID
		f.instance = f.readyInstance
		return f.readyPID, f.readyInstance, nil
	}
	if f.pid == 0 && !f.bad {
		return 0, "", errNoPIDFile
	}
	if f.bad {
		return 0, "", errPIDInvalid
	}
	return f.pid, f.instance, nil
}

func (f *fakePIDIO) write(cfg *config.Config, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, pid)
	f.pid = pid
	f.bad = false
	return nil
}

func (f *fakePIDIO) remove(cfg *config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	f.pid = 0
	return nil
}

// fakeSpawn 实现 spawner：记录 spawn 调用，返回 fakeProcess（PID=childPID）。
type fakeSpawn struct {
	mu       sync.Mutex
	calls    []spawnOptions
	childPID int
	err      error
	killed   []int
	released []int
}

func (f *fakeSpawn) spawn(opts spawnOptions) (spawnedProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	if f.err != nil {
		return nil, f.err
	}
	return &fakeProcess{pid: f.childPID, spawner: f}, nil
}

type fakeProcess struct {
	pid     int
	spawner *fakeSpawn
}

func (p *fakeProcess) PID() int { return p.pid }
func (p *fakeProcess) Kill() error {
	p.spawner.mu.Lock()
	defer p.spawner.mu.Unlock()
	p.spawner.killed = append(p.spawner.killed, p.pid)
	return nil
}
func (p *fakeProcess) Release() error {
	p.spawner.mu.Lock()
	defer p.spawner.mu.Unlock()
	p.spawner.released = append(p.spawner.released, p.pid)
	return nil
}

// fakeProcessKill 实现 processSignaler：记录 SIGTERM/taskkill 调用。
type fakeProcessKill struct {
	mu       sync.Mutex
	sigterm  []int
	taskkill []int
	err      error
}

func (f *fakeProcessKill) terminate(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if goruntime.GOOS == "windows" {
		f.taskkill = append(f.taskkill, pid)
	} else {
		f.sigterm = append(f.sigterm, pid)
	}
	return nil
}

// fakeMetadataCleaner 实现 staleMetadataCleaner：记录调用次数与 dataDir。
type fakeMetadataCleaner struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeMetadataCleaner) cleanup(dataDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dataDir)
	if f.err != nil {
		return f.err
	}
	return nil
}

// stubServiceManager 记录 StopCurrent 调用，可控 statusInstalled。
type stubServiceManager struct {
	mu           sync.Mutex
	statusResult bool
	stopErr      error
	stopCalls    int
}

func (s *stubServiceManager) platform() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusResult {
		return "launchd"
	}
	return "posix"
}

func (s *stubServiceManager) statusInstalled(opts serviceOptions) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusResult
}

func (s *stubServiceManager) stopCurrent(opts serviceOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	return s.stopErr
}

// fakeStateReader 实现 runtimeStateReader，模拟 coordinator 写 runtime-state。
//   - readErr：非 nil 时恒返回（模拟文件缺失/解析失败，测试降级路径）。
//   - readyAfter=-1（默认）：禁用 ready 分支，按 state 字段返回（默认空 state + readErr=errNoPIDFile）。
//   - readyAfter>=0：read 第 (readyAfter+1) 次起返回 readyState（模拟 monitor_ready=true 写入）。
type fakeStateReader struct {
	mu         sync.Mutex
	state      runmeta.RuntimeState // 非 ready 分支返回的 state
	readErr    error
	reads      int
	readyAfter int
	readyState runmeta.RuntimeState // ready 分支返回的 state
}

func newFakeStateReader() *fakeStateReader {
	// 默认：runtime-state 缺失（ready 握手降级，绝不误判 ready）。
	return &fakeStateReader{readErr: errNoRuntimeState, readyAfter: -1}
}

func (f *fakeStateReader) read(cfg *config.Config) (runmeta.RuntimeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil && !(f.readyAfter >= 0 && f.reads > f.readyAfter) {
		return runmeta.RuntimeState{}, f.readErr
	}
	if f.readyAfter >= 0 && f.reads > f.readyAfter {
		f.state = f.readyState
		return f.readyState, nil
	}
	return f.state, nil
}

// traceRecorder 记录公开 API 与依赖调用的时间顺序，用于强制断言锁顺序。
type traceRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *traceRecorder) record(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *traceRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

// tracedDaemonLock 包裹 fakeDaemonLock，把 isRunning 调用写入 trace。
type tracedDaemonLock struct {
	inner *fakeDaemonLock
	trace *traceRecorder
}

func (t *tracedDaemonLock) isRunning(cfg *config.Config) bool {
	t.trace.record("Inspect")
	return t.inner.isRunning(cfg)
}

// tracedConfigLoader 记录 loader 调用时机（必须发生在 AcquireControlLock 之后）。
type tracedConfigLoader struct {
	trace *traceRecorder
	cfg   *config.Config
	err   error
}

func (l *tracedConfigLoader) load() (*config.Config, error) {
	l.trace.record("LoadConfig")
	if l.err != nil {
		return nil, l.err
	}
	return l.cfg, nil
}

// newConfigWith 单测构造 config.Config。
func newConfigWith(dataDir string) *config.Config {
	return &config.Config{DataDir: dataDir}
}

// ---- 测试装配器 ----

type fakeDeps struct {
	clock    *fakeClock
	locker   *fakeLocker
	dlock    *tracedDaemonLock
	pid      *fakePIDIO
	state    *fakeStateReader
	spawn    *fakeSpawn
	kill     *fakeProcessKill
	metadata *fakeMetadataCleaner
	service  *stubServiceManager
	trace    *traceRecorder
	// instanceID 本次 start 的握手标识（instanceIDGen 返回它；ready fakes 据它匹配）。
	instanceID string
}

func newFakeDeps() *fakeDeps {
	trace := &traceRecorder{}
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	dl := &tracedDaemonLock{inner: newFakeDaemonLock(false), trace: trace}
	return &fakeDeps{
		clock: clk, locker: fl, dlock: dl,
		pid: newFakePIDIO(), state: newFakeStateReader(), spawn: &fakeSpawn{childPID: 4242},
		kill: &fakeProcessKill{}, metadata: &fakeMetadataCleaner{},
		service: &stubServiceManager{}, trace: trace,
		instanceID: "inst-" + tNextInstance(),
	}
}

// tNextInstance 生成测试用递增 instanceID 后缀（全局原子计数，避免多测试间碰撞）。
func tNextInstance() string {
	n := atomic.AddUint64(&tInstanceSeq, 1)
	return strconv.FormatUint(n, 16)
}

var tInstanceSeq uint64

func (f *fakeDeps) asManagerDeps() managerDependencies {
	return managerDependencies{
		now:               f.clock.now,
		sleep:             f.clock.sleep,
		newLocker:         func() controlLocker { return f.locker },
		daemonLock:        f.dlock,
		pidIO:             f.pid,
		stateReader:       f.state,
		spawner:           f.spawn,
		processKill:       f.kill,
		metadataCleaner:   f.metadata,
		serviceMgr:        f.service,
		startReadyTimeout: 5 * time.Second,
		stopWaitTimeout:   5 * time.Second,
		pollInterval:      100 * time.Millisecond,
		instanceIDGen:     func() string { return f.instanceID },
	}
}

// newTestProcessManager 装配带 fake 依赖的 Manager（过程测试专用）。
func newTestProcessManager(t *testing.T, home string, f *fakeDeps) *Manager {
	t.Helper()
	m := &Manager{home: home, deps: f.asManagerDeps()}
	// 用 traced newLocker 覆盖，记录 AcquireControlLock/ReleaseControlLock。
	base := f.locker
	tr := f.trace
	m.deps.newLocker = func() controlLocker {
		return &tracedLocker{inner: base, trace: tr}
	}
	return m
}

// enableStartReady 配置 fakeDeps 使 start 在 spawn 后满足六项 ready 条件：
// PID 文件 PID+instanceID、daemon lock、runtime-state PID+instanceID+monitor_ready。
// pidReadyAfter：fakePIDIO.read 第 (pidReadyAfter+1) 次起返回 ready（0=首次即 ready）。
// dlockReadyAfter：fakeDaemonLock.isRunning 第 (dlockReadyAfter+1) 次起返回 runningWhenReady。
// stateReadyAfter：fakeStateReader.read 第 (stateReadyAfter+1) 次起返回 readyState。
// 通常 pidReadyAfter=0（spawn 后立即可读 PID）、dlockReadyAfter=1（第 1 次 inspect=false，第 2 次起 ready）、
// stateReadyAfter 与 pidReadyAfter 对齐。catchUp 可由调用方在 readyState.CatchUp 覆盖（默认 pending）。
func enableStartReady(f *fakeDeps, childPID, pidReadyAfter, dlockReadyAfter, stateReadyAfter int, catchUp string) {
	f.spawn.childPID = childPID
	f.pid.readyAfter = pidReadyAfter
	f.pid.readyPID = childPID
	f.pid.readyInstance = f.instanceID
	f.dlock.inner.readyAfter = dlockReadyAfter
	f.dlock.inner.runningWhenReady = true
	f.state.readErr = nil
	f.state.readyAfter = stateReadyAfter
	f.state.readyState = runmeta.RuntimeState{
		PID:          childPID,
		InstanceID:   f.instanceID,
		MonitorReady: true,
		CatchUp:      catchUp,
	}
}

// enableStartReadyRestart 同上，但操作 restartFakeDeps（独立 instance 字段）。
// restart 的 daemon lock 由 scriptedDaemonLock 的 script 顺序驱动，此处只配 PID/state ready。
func enableStartReadyRestart(f *restartFakeDeps, childPID, pidReadyAfter, stateReadyAfter int) {
	f.spawn.childPID = childPID
	f.pid.readyAfter = pidReadyAfter
	f.pid.readyPID = childPID
	f.pid.readyInstance = f.instance
	f.state.readErr = nil
	f.state.readyAfter = stateReadyAfter
	f.state.readyState = runmeta.RuntimeState{
		PID:          childPID,
		InstanceID:   f.instance,
		MonitorReady: true,
		CatchUp:      "pending",
	}
}

// tracedLocker 记录加解锁事件到 trace，保证锁顺序测试可见。
type tracedLocker struct {
	inner *fakeLocker
	trace *traceRecorder
}

func (t *tracedLocker) tryLock() (bool, error) {
	acquired, err := t.inner.tryLock()
	if acquired {
		t.trace.record("AcquireControlLock")
	}
	return acquired, err
}

func (t *tracedLocker) unlock() error {
	err := t.inner.unlock()
	t.trace.record("ReleaseControlLock")
	return err
}

// indexOf 返回 s 在 slice 中第一个出现的 index，不存在返回 -1。
func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}

// ---- Inspect ----

func TestInspect_NotRunning(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = false
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if st.Running {
		t.Error("未运行时 Running 应为 false")
	}
	if st.PID != 0 {
		t.Errorf("未运行时 PID 应为 0，实际 %d", st.PID)
	}
}

func TestInspect_RunningWithPID(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 1234
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if !st.Running {
		t.Error("daemon lock 持有时 Running 应为 true")
	}
	if st.PID != 1234 {
		t.Errorf("PID=%d want 1234", st.PID)
	}
}

// TestInspect_RunningButPIDMissing daemon lock 持有但 PID 不可读 → Running=true, PID=0（不报错）。
func TestInspect_RunningButPIDMissing(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 0
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect 应容忍 PID 缺失，err=%v", err)
	}
	if !st.Running {
		t.Error("daemon lock 持有即为 Running=true，无论 PID 是否可读")
	}
	if st.PID != 0 {
		t.Errorf("PID 缺失时 PID 应为 0，实际 %d", st.PID)
	}
}

// ---- Inspect 阶段读取 ----
//
// 阶段信息只在 runtime-state 的 PID+instanceID 与 PID 文件全匹配时可用。
// PhaseAvailable=true 表示 MonitorReady/CatchUp/CatchUpFailures 已填充且可信；
// 否则阶段未知（调用方降级显示「启动阶段未知」，不推翻 Running 结论）。

// TestInspect_PhaseMatched_PIDInstanceID全匹配 阶段字段填充且 PhaseAvailable=true。
func TestInspect_PhaseMatched_PIDInstanceID全匹配(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	// PID 文件 PID=4321 instanceID=inst-x（非 ready 路径：readyAfter=-1）。
	f.pid.pid = 4321
	f.pid.instance = "inst-x"
	// runtime-state 与 PID 文件全匹配 + 阶段字段已就绪。
	f.state.readErr = nil
	f.state.state = runmeta.RuntimeState{
		PID:             4321,
		InstanceID:      "inst-x",
		MonitorReady:    true,
		CatchUp:         "pending",
		CatchUpFailures: 0,
	}
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if !st.Running || st.PID != 4321 {
		t.Fatalf("应 Running=true PID=4321, 实际 %+v", st)
	}
	if !st.PhaseAvailable {
		t.Fatal("PID+instanceID 全匹配时 PhaseAvailable 应为 true")
	}
	if st.InstanceID != "inst-x" || !st.MonitorReady || st.CatchUp != "pending" {
		t.Errorf("阶段字段应填充: %+v", st)
	}
}

// TestInspect_PhaseMatched_catchUpFailed_FailuresCopied catch_up=failed 时计数被拷贝。
func TestInspect_PhaseMatched_catchUpFailed_FailuresCopied(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 7700
	f.pid.instance = "inst-fail"
	f.state.readErr = nil
	f.state.state = runmeta.RuntimeState{
		PID: 7700, InstanceID: "inst-fail", MonitorReady: true,
		CatchUp: "failed", CatchUpFailures: 2,
	}
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if !st.PhaseAvailable || st.CatchUp != "failed" || st.CatchUpFailures != 2 {
		t.Errorf("failed 阶段应拷贝计数, 实际 %+v", st)
	}
}

// TestInspect_PhaseMissing_RuntimeStateMissing runtime-state 缺失 → PhaseAvailable=false。
func TestInspect_PhaseMissing_RuntimeStateMissing(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 1111
	f.pid.instance = "inst-a"
	// 默认 fakeStateReader: readErr=errNoRuntimeState
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if !st.Running || st.PID != 1111 {
		t.Fatalf("应 Running PID=1111, 实际 %+v", st)
	}
	if st.PhaseAvailable {
		t.Error("runtime-state 缺失时 PhaseAvailable 应为 false（阶段未知）")
	}
	if st.CatchUp != "" || st.MonitorReady {
		t.Errorf("阶段字段不应填充: %+v", st)
	}
}

// TestInspect_PhaseMissing_PIDFileMismatch state PID 与 PID 文件不一致 → 阶段未知。
func TestInspect_PhaseMissing_PIDFileMismatch(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 2222
	f.pid.instance = "inst-a"
	f.state.readErr = nil
	// runtime-state PID 不同（stale 旧代）。
	f.state.state = runmeta.RuntimeState{PID: 9999, InstanceID: "inst-a", MonitorReady: true}
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if st.PhaseAvailable {
		t.Error("state PID 不匹配时 PhaseAvailable 应为 false（stale 旧代不采信）")
	}
}

// TestInspect_PhaseMissing_InstanceIDMismatch state instanceID 与 PID 文件不一致 → 阶段未知。
func TestInspect_PhaseMissing_InstanceIDMismatch(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 3333
	f.pid.instance = "inst-curr"
	f.state.readErr = nil
	// 同 PID 不同 instanceID（PID 复用为他代）。
	f.state.state = runmeta.RuntimeState{PID: 3333, InstanceID: "inst-old", MonitorReady: true}
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect err=%v", err)
	}
	if st.PhaseAvailable {
		t.Error("state instanceID 不匹配时 PhaseAvailable 应为 false")
	}
}

// TestInspect_PhaseMissing_PIDMetadataUnavailable daemon lock 持有但 PID 不可读
// → Running=true, PID=0, PhaseAvailable=false（无 PID 无法做 state 匹配）。
func TestInspect_PhaseMissing_PIDMetadataUnavailable(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 0 // PID 文件缺失
	m := newTestProcessManager(t, t.TempDir(), f)

	st, err := m.Inspect(context.Background(), newConfigWith(t.TempDir()))
	if err != nil {
		t.Fatalf("Inspect 应容忍 PID 缺失, err=%v", err)
	}
	if !st.Running || st.PID != 0 {
		t.Fatalf("应 Running PID=0, 实际 %+v", st)
	}
	if st.PhaseAvailable {
		t.Error("PID 不可用时 PhaseAvailable 应为 false（无法做 PID 匹配）")
	}
}

// ---- 锁顺序 trace ----

// TestStart_LockOrderTrace 强制断言 start 的锁内顺序：
// AcquireControlLock → LoadConfig → Inspect → ... → ReleaseControlLock。
func TestStart_LockOrderTrace(t *testing.T) {
	f := newFakeDeps()
	// spawn 后下一次轮询六项 ready 条件全部满足（确定性，无真实 sleep）。
	enableStartReady(f, 5050, 0, 1, 0, "pending")
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	if _, err := m.Start(context.Background(), loader.load); err != nil {
		t.Fatalf("Start err=%v", err)
	}

	steps := f.trace.snapshot()
	if len(steps) < 2 || steps[0] != "AcquireControlLock" {
		t.Fatalf("第一步必须是 AcquireControlLock，实际 steps=%v", steps)
	}
	if steps[len(steps)-1] != "ReleaseControlLock" {
		t.Fatalf("最后一步必须是 ReleaseControlLock，实际 steps=%v", steps)
	}
	loadIdx := indexOf(steps, "LoadConfig")
	if loadIdx < 1 {
		t.Fatalf("LoadConfig 必须在 AcquireControlLock 之后调用，steps=%v", steps)
	}
	inspectIdx := indexOf(steps, "Inspect")
	if inspectIdx <= loadIdx {
		t.Fatalf("Inspect 必须在 LoadConfig 之后，steps=%v", steps)
	}
}

// TestStop_LockOrderTrace 强制断言 stop 的锁内顺序。
func TestStop_LockOrderTrace(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 5555
	// check#1 (inspect)=true → SIGTERM；check#2 (waitDaemonRelease)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	if _, err := m.Stop(context.Background(), loader.load); err != nil {
		t.Fatalf("Stop err=%v", err)
	}

	steps := f.trace.snapshot()
	if len(steps) < 2 || steps[0] != "AcquireControlLock" {
		t.Fatalf("第一步必须是 AcquireControlLock，steps=%v", steps)
	}
	if steps[len(steps)-1] != "ReleaseControlLock" {
		t.Fatalf("最后一步必须是 ReleaseControlLock，steps=%v", steps)
	}
	loadIdx := indexOf(steps, "LoadConfig")
	inspectIdx := indexOf(steps, "Inspect")
	if loadIdx < 1 || inspectIdx <= loadIdx {
		t.Fatalf("锁顺序错：steps=%v", steps)
	}
}

// TestStart_LoaderCalledExactlyOnce loader 在锁内恰好调用一次。
func TestStart_LoaderCalledExactlyOnce(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 6060, 0, 1, 0, "pending")
	m := newTestProcessManager(t, t.TempDir(), f)
	calls := 0
	loader := func() (*config.Config, error) {
		calls++
		return newConfigWith(t.TempDir()), nil
	}

	if _, err := m.Start(context.Background(), loader); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	if calls != 1 {
		t.Errorf("loader 应恰好调用一次，实际 %d", calls)
	}
}

// ---- Start 业务用例 ----

// TestStart_AlreadyRunningNoSpawn 已运行 → 不 spawn，返回 AlreadyRunning=true + 准确 PID。
func TestStart_AlreadyRunningNoSpawn(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 7777
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Start(context.Background(), loader)
	if err != nil {
		t.Fatalf("已运行应返回 AlreadyRunning，err=%v", err)
	}
	if !res.AlreadyRunning {
		t.Error("已运行时 AlreadyRunning 应为 true")
	}
	if res.PID != 7777 {
		t.Errorf("PID=%d want 7777", res.PID)
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("已运行不应 spawn，实际 calls=%d", len(f.spawn.calls))
	}
}

// TestStart_NotRunningSpawnsAndWaits 未运行 → spawn + 等六项 ready 条件，返回 PID。
func TestStart_NotRunningSpawnsAndWaits(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 9898, 0, 1, 0, "pending")
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Start(context.Background(), loader)
	if err != nil {
		t.Fatalf("Start err=%v", err)
	}
	if res.AlreadyRunning {
		t.Error("未运行 spawn 后 AlreadyRunning 应为 false")
	}
	if res.PID != 9898 {
		t.Errorf("PID=%d want 9898", res.PID)
	}
	if len(f.spawn.calls) != 1 {
		t.Errorf("应 spawn 一次，实际 %d", len(f.spawn.calls))
	}
}

// TestStart_PidMissingWhileLockHeld lock 持有但 PID 缺失/非法 → 安全错误，不 spawn。
func TestStart_PidMissingWhileLockHeld(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 0
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatal("lock 持有但 PID 不可用应返回安全错误")
	}
	if !errors.Is(err, errPIDMetadataUnavailable) {
		t.Errorf("应返回 errPIDMetadataUnavailable，实际: %v", err)
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("PID 不可用时不应 spawn，实际 calls=%d", len(f.spawn.calls))
	}
}

// TestStart_SpawnTimeoutKillsChild spawn 后超时未就绪 → best-effort kill child 并返回错误。
func TestStart_SpawnTimeoutKillsChild(t *testing.T) {
	f := newFakeDeps()
	f.spawn.childPID = 1111
	// 不设置 readyAfter/readyPID/runningWhenReady：daemon lock 始终未持有、PID 文件不出现。
	// fakeClock.sleep 推进虚拟时间直至超过 startReadyTimeout deadline。
	m := newTestProcessManager(t, t.TempDir(), f)
	m.deps.startReadyTimeout = 500 * time.Millisecond
	m.deps.pollInterval = 100 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatal("spawn 超时应返回错误")
	}
	if len(f.spawn.killed) != 1 || f.spawn.killed[0] != 1111 {
		t.Errorf("超时应 best-effort kill child，killed=%v", f.spawn.killed)
	}
}

// ---- Stop 业务用例 ----

// TestStop_NotRunningIdempotent 未运行 → WasRunning=false，不调任何停止路径。
func TestStop_NotRunningIdempotent(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Stop(context.Background(), loader)
	if err != nil {
		t.Fatalf("未运行应幂等成功，err=%v", err)
	}
	if res.WasRunning {
		t.Error("未运行时 WasRunning 应为 false")
	}
	if f.service.stopCalls != 0 || len(f.kill.sigterm) != 0 {
		t.Errorf("未运行不应触发任何停止路径，stopCalls=%d sigterm=%v", f.service.stopCalls, f.kill.sigterm)
	}
}

// TestStop_RunningPosixManual_Sigterm POSIX + 未托管 → SIGTERM 准确 PID，等 lock 释放成功。
func TestStop_RunningPosixManual_Sigterm(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 手动路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 3333
	f.service.statusResult = false // 未托管
	// isRunning: check#1 (inspect)=true → SIGTERM；check#2 (waitDaemonRelease 第1轮)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Stop(context.Background(), loader)
	if err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	if !res.WasRunning {
		t.Error("运行中停止 WasRunning 应为 true")
	}
	if res.PID != 3333 {
		t.Errorf("PID=%d want 3333", res.PID)
	}
	if len(f.kill.sigterm) != 1 || f.kill.sigterm[0] != 3333 {
		t.Errorf("应对准确 PID SIGTERM，sigterm=%v", f.kill.sigterm)
	}
}

// TestStop_RunningManaged_BootoutThenSigtermIfNeeded 受托管 → bootout 后 lock 仍持有 → SIGTERM 补刀。
func TestStop_RunningManaged_BootoutThenSigtermIfNeeded(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("macOS 托管路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 8888
	f.service.statusResult = true // 受托管
	// check#1 (inspect)=true → bootout；check#2 (bootout 后)=true → SIGTERM 补刀；
	// check#3 (waitDaemonRelease 第1轮)=false → 释放成功。
	f.dlock.inner.readyAfter = 2
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Stop(context.Background(), loader)
	if err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	if !res.WasRunning {
		t.Error("运行中停止 WasRunning 应为 true")
	}
	if f.service.stopCalls != 1 {
		t.Errorf("受托管应调一次 bootout (StopCurrent)，实际 %d", f.service.stopCalls)
	}
	if len(f.kill.sigterm) != 1 || f.kill.sigterm[0] != 8888 {
		t.Errorf("bootout 后 lock 仍持有应对准确 PID SIGTERM，sigterm=%v", f.kill.sigterm)
	}
}

// TestStop_RunningManaged_BootoutOnly bootout 后 lock 已释放 → 不再 SIGTERM。
func TestStop_RunningManaged_BootoutOnly(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("macOS 托管路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 9999
	f.service.statusResult = true
	// check#1 (inspect)=true → bootout；check#2 (bootout 后)=false → 不 SIGTERM；
	// check#3 (waitDaemonRelease 第1轮)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Stop(context.Background(), loader)
	if err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	if !res.WasRunning {
		t.Error("WasRunning 应为 true")
	}
	if f.service.stopCalls != 1 {
		t.Errorf("应调一次 bootout，实际 %d", f.service.stopCalls)
	}
	if len(f.kill.sigterm) != 0 {
		t.Errorf("bootout 后 lock 已释放不应再 SIGTERM，sigterm=%v", f.kill.sigterm)
	}
}

// TestStop_Timeout_DoesNotFakeSuccess 超时 → 返回错误，不删 PID 伪装成功。
func TestStop_Timeout_DoesNotFakeSuccess(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 超时路径不在 Windows 上测")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 4321
	f.service.statusResult = false
	// isRunning 始终 true（readyAfter=0 不触发分支）→ waitDaemonRelease 轮询至超时。
	m := newTestProcessManager(t, t.TempDir(), f)
	m.deps.stopWaitTimeout = 500 * time.Millisecond
	m.deps.pollInterval = 100 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Stop(context.Background(), loader)
	if err == nil {
		t.Fatal("超时应返回错误，不伪装成功")
	}
	if !errors.Is(err, errDaemonStillRunning) {
		t.Errorf("超时应返回 errDaemonStillRunning，实际: %v", err)
	}
	if f.pid.removes != 0 {
		t.Errorf("超时不应删 PID 伪装成功，removes=%d", f.pid.removes)
	}
}

// ---- Session.Inspect / CleanupStaleMetadata ----

// TestSession_Inspect_InLock 在 Session 内调 Inspect 不二次加锁。
func TestSession_Inspect_InLock(t *testing.T) {
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 2020
	m := newTestProcessManager(t, t.TempDir(), f)
	cfg := newConfigWith(t.TempDir())

	var st RuntimeState
	err := m.WithLock(context.Background(), func(s *Session) error {
		var ierr error
		st, ierr = s.Inspect(context.Background(), cfg)
		return ierr
	})
	if err != nil {
		t.Fatalf("Session.Inspect err=%v", err)
	}
	if !st.Running || st.PID != 2020 {
		t.Errorf("Session.Inspect 状态错: %+v", st)
	}
}

// TestSession_CleanupStaleMetadata 记录清理调用。
func TestSession_CleanupStaleMetadata(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)

	err := m.WithLock(context.Background(), func(s *Session) error {
		return s.CleanupStaleMetadata(context.Background(), "/data/x")
	})
	if err != nil {
		t.Fatalf("CleanupStaleMetadata err=%v", err)
	}
	if len(f.metadata.calls) != 1 || f.metadata.calls[0] != "/data/x" {
		t.Errorf("应记录清理调用，calls=%v", f.metadata.calls)
	}
}

// ---- 控制锁路径不随 data_dir 变化 ----

func TestControlLockPath_IndependentOfDataDir(t *testing.T) {
	home := t.TempDir()
	p1 := ControlLockPath(home)
	// control lock 路径只依赖 home，与 data_dir 无关。
	if ControlLockPath(home) != p1 {
		t.Fatal("control lock 路径不应随 data_dir 变化")
	}
	if !filepath.IsAbs(p1) {
		t.Errorf("control lock 路径应为绝对路径: %q", p1)
	}
}

// ---- 平台专属断言 ----

// TestStop_Windows_UsesExactPID Windows 路径只对准确 PID 调 taskkill（禁止按名称）。
func TestStop_Windows_UsesExactPID(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows 路径需 Windows CI 执行")
	}
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 5555
	// check#1 (inspect)=true → taskkill；check#2 (waitDaemonRelease)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Stop(context.Background(), loader)
	if err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	if len(f.kill.taskkill) != 1 || f.kill.taskkill[0] != 5555 {
		t.Errorf("Windows 应只对准确 PID 调 taskkill，taskkill=%v", f.kill.taskkill)
	}
}

// ---- Restart 专用 fakes ----
//
// restart 的 daemon lock 状态序列比单次 start/stop 复杂：必须 true（stop 判活）
// → false（stop 释放完成 + start 判未运行）→ true（start spawn 后就绪）。
// 现有 fakeDaemonLock 只有单段 readyAfter，无法表达「先降后升」。
// 这里用 scriptedDaemonLock 按布尔脚本顺序返回，确定性地驱动 restart 状态机。

// scriptedDaemonLock 按 script 顺序返回 isRunning；脚本耗尽后返回最后一个值。
type scriptedDaemonLock struct {
	mu     sync.Mutex
	script []bool
	idx    int
}

func (s *scriptedDaemonLock) isRunning(cfg *config.Config) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.script) == 0 {
		return false
	}
	v := s.script[s.idx]
	if s.idx < len(s.script)-1 {
		s.idx++
	}
	return v
}

// tracedScriptedDaemonLock 把 scriptedDaemonLock 的 isRunning 调用写入 trace（独立类型，
// 避免改动 tracedDaemonLock 的 *fakeDaemonLock 字段从而影响 start/stop 测试）。
type tracedScriptedDaemonLock struct {
	inner *scriptedDaemonLock
	trace *traceRecorder
}

func (t *tracedScriptedDaemonLock) isRunning(cfg *config.Config) bool {
	t.trace.record("Inspect")
	return t.inner.isRunning(cfg)
}

// tracedSpawn 包裹 fakeSpawn，把 spawn 调用写入 trace（用于断言 restart 内 spawn 出现在 stop 之后）。
type tracedSpawn struct {
	inner *fakeSpawn
	trace *traceRecorder
}

func (t *tracedSpawn) spawn(opts spawnOptions) (spawnedProcess, error) {
	t.trace.record("Spawn")
	return t.inner.spawn(opts)
}

// tracedProcessKill 包裹 fakeProcessKill，把 terminate 调用写入 trace。
type tracedProcessKill struct {
	inner *fakeProcessKill
	trace *traceRecorder
}

func (t *tracedProcessKill) terminate(pid int) error {
	t.trace.record("StopSignal")
	return t.inner.terminate(pid)
}

// restartFakeDeps 装配 restart 专用依赖：scripted+traced daemon lock + traced spawn/kill。
// 独立于 fakeDeps（其 dlock 是 *tracedDaemonLock 持 *fakeDaemonLock），因为 restart
// 需要状态序列更复杂的 scripted daemon lock 来驱动 stop→release→start→ready。
type restartFakeDeps struct {
	clock    *fakeClock
	locker   *fakeLocker
	dlock    *tracedScriptedDaemonLock
	pid      *fakePIDIO
	state    *fakeStateReader
	spawn    *fakeSpawn
	kill     *fakeProcessKill
	metadata *fakeMetadataCleaner
	service  *stubServiceManager
	trace    *traceRecorder
	instance string
}

func newRestartFakeDeps(script []bool) *restartFakeDeps {
	trace := &traceRecorder{}
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	fl := newFakeLocker(true)
	dl := &tracedScriptedDaemonLock{inner: &scriptedDaemonLock{script: script}, trace: trace}
	pid := newFakePIDIO()
	pid.pid = 3333 // 旧 daemon PID，供 stopLocked.inspect 读到（否则报 PID 元数据不可用）。
	return &restartFakeDeps{
		clock: clk, locker: fl, dlock: dl,
		pid: pid, state: newFakeStateReader(), spawn: &fakeSpawn{childPID: 6464},
		kill: &fakeProcessKill{}, metadata: &fakeMetadataCleaner{},
		service: &stubServiceManager{}, trace: trace,
		instance: "restart-inst-" + tNextInstance(),
	}
}

// newRestartManager 装配带 scripted daemon lock + traced spawn/kill 的 Manager。
// traced spawn/kill 包装保留原 fake 行为（childPID/记录），同时把调用写入 trace，
// 使 restart 锁顺序测试可见 stop 信号 → spawn 的先后。
func newRestartManager(t *testing.T, home string, f *restartFakeDeps) *Manager {
	t.Helper()
	m := &Manager{home: home, deps: managerDependencies{
		now:               f.clock.now,
		sleep:             f.clock.sleep,
		newLocker:         func() controlLocker { return &tracedLocker{inner: f.locker, trace: f.trace} },
		daemonLock:        f.dlock,
		pidIO:             f.pid,
		stateReader:       f.state,
		spawner:           &tracedSpawn{inner: f.spawn, trace: f.trace},
		processKill:       &tracedProcessKill{inner: f.kill, trace: f.trace},
		metadataCleaner:   f.metadata,
		serviceMgr:        f.service,
		startReadyTimeout: 5 * time.Second,
		stopWaitTimeout:   5 * time.Second,
		pollInterval:      100 * time.Millisecond,
		instanceIDGen:     func() string { return f.instance },
	}}
	return m
}

// ---- Restart ----

// TestRestart_LockOrderTrace 强制断言 restart 的完整锁内顺序：
// AcquireControlLock → LoadConfig → Inspect → StopSignal（停旧）→ ... → Spawn → ... → ReleaseControlLock。
// 单次 control lock 内完成 stop+start（无二次 Acquire）。
func TestRestart_LockOrderTrace(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX stop 路径（SIGTERM）确定性脚本不在 Windows 上测")
	}
	// daemon lock 脚本（每次 isRunning 调用按序取，traced 全记为 Inspect）：
	//  1. Restart.inspect        → true（运行中）
	//  2. stopLocked.inspect     → true（运行中，触发 SIGTERM）
	//  3. waitDaemonRelease 第1轮 → false（释放完成）
	//  4. startLocked.inspect    → false（未运行，触发 spawn）
	//  5+. waitForStartReady 轮询 → true（新 child 就绪）
	script := []bool{true, true, false, false, true}
	f := newRestartFakeDeps(script)
	f.service.statusResult = false // 未托管 → 直接 SIGTERM
	// PID 读取序列：read#1 (Restart.inspect)=3333、read#2 (stopLocked.inspect)=3333；
	// startLocked.remove 清掉后，read#3+ (waitForStartReady) 返回新 child PID 6464 + 匹配 state。
	enableStartReadyRestart(f, 6464, 2, 2)
	m := newRestartManager(t, t.TempDir(), f)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	res, err := m.Restart(context.Background(), loader.load)
	if err != nil {
		t.Fatalf("Restart err=%v", err)
	}
	if res.NewPID != 6464 {
		t.Errorf("NewPID=%d want 6464", res.NewPID)
	}

	steps := f.trace.snapshot()
	if len(steps) < 2 || steps[0] != "AcquireControlLock" {
		t.Fatalf("第一步必须是 AcquireControlLock，steps=%v", steps)
	}
	if steps[len(steps)-1] != "ReleaseControlLock" {
		t.Fatalf("最后一步必须是 ReleaseControlLock，steps=%v", steps)
	}
	// 单次锁：只出现一次 Acquire/Release。
	if cnt := countOccur(steps, "AcquireControlLock"); cnt != 1 {
		t.Errorf("restart 应单次 control lock，Acquire 次数=%d", cnt)
	}
	loadIdx := indexOf(steps, "LoadConfig")
	if loadIdx < 1 {
		t.Fatalf("LoadConfig 必须在 AcquireControlLock 之后，steps=%v", steps)
	}
	spawnIdx := indexOf(steps, "Spawn")
	stopIdx := indexOf(steps, "StopSignal")
	if stopIdx < 0 || spawnIdx < 0 {
		t.Fatalf("restart trace 必须包含 StopSignal 与 Spawn，steps=%v", steps)
	}
	if stopIdx <= loadIdx || spawnIdx <= stopIdx {
		t.Fatalf("顺序应为 Load → StopSignal → Spawn，steps=%v", steps)
	}
}

// countOccur 统计 s 在 slice 中出现次数。
func countOccur(slice []string, s string) int {
	n := 0
	for _, v := range slice {
		if v == s {
			n++
		}
	}
	return n
}

// TestRestart_NotRunningNoSpawn 未运行 → 返回 ErrRestartNotRunning，不 spawn。
func TestRestart_NotRunningNoSpawn(t *testing.T) {
	f := newRestartFakeDeps([]bool{false}) // inspect 立即 false
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Restart(context.Background(), loader)
	if !errors.Is(err, ErrRestartNotRunning) {
		t.Fatalf("未运行应返回 ErrRestartNotRunning，实际: %v", err)
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("未运行不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestRestart_StopFailsNoSpawn stop 失败（SIGTERM 失败）→ 不 spawn，保留原错误。
func TestRestart_StopFailsNoSpawn(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX SIGTERM 路径不在 Windows 上测")
	}
	// 1. Restart.inspect true → 2. stopLocked.inspect true → SIGTERM（f.kill.err 触发失败）。
	script := []bool{true, true, true}
	f := newRestartFakeDeps(script)
	f.service.statusResult = false
	f.kill.err = errors.New("sigterm boom") // stop 发信号失败
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Restart(context.Background(), loader)
	if err == nil {
		t.Fatal("stop 失败应返回错误，不 spawn")
	}
	if !strings.Contains(err.Error(), "sigterm boom") {
		t.Errorf("应保留 stop 错误，实际: %v", err)
	}
	if len(f.spawn.calls) != 0 {
		t.Errorf("stop 失败不应 spawn，calls=%d", len(f.spawn.calls))
	}
}

// TestRestart_StartFailsPreservesError start 失败（spawn 超时）→ 保留原错误，不掩盖。
func TestRestart_StartFailsPreservesError(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 路径不在 Windows 上测")
	}
	// 1. Restart.inspect true → 2. stopLocked.inspect true → SIGTERM；
	// 3. waitDaemonRelease 第1轮 false（释放成功）；4. startLocked.inspect false → spawn；
	// 5+. waitForStartReady 始终 false（不设 readyPID → 读不到就绪 PID）→ 超时。
	script := []bool{true, true, false, false, false}
	f := newRestartFakeDeps(script)
	f.service.statusResult = false
	m := newRestartManager(t, t.TempDir(), f)
	m.deps.startReadyTimeout = 500 * time.Millisecond
	m.deps.pollInterval = 100 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Restart(context.Background(), loader)
	if err == nil {
		t.Fatal("start 失败应返回错误")
	}
	if !strings.Contains(err.Error(), "启动守护进程失败") && !strings.Contains(err.Error(), "超时") {
		t.Errorf("应保留 start 失败原因，实际: %v", err)
	}
	if len(f.spawn.killed) != 1 {
		t.Errorf("start 超时应 best-effort kill child，killed=%v", f.spawn.killed)
	}
}

// TestRestart_NoConfigPlistRegistryWrites restart 全流程不触碰 config/plist/注册表：
// metadata cleaner 只清理运行元数据，不触碰 config/plist/注册表，
// service 写入 0 次（statusInstalled 只读 + stopCurrent 是 bootout 不改定义）。
func TestRestart_NoConfigPlistRegistryWrites(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 路径不在 Windows 上测")
	}
	// 受托管（statusInstalled=true）的 isRunning 调用序列：
	//  1. Restart.inspect true → 2. stopLocked.inspect true → bootout；
	//  3. bootout 后 stopDaemonByPlatform 查 lock → false（bootout 已停掉，不补 SIGTERM）；
	//  4. waitDaemonRelease 第1轮 false（已释放）；5. startLocked.inspect false → spawn；
	//  6+. waitForStartReady true（就绪）。
	script := []bool{true, true, false, false, false, true}
	f := newRestartFakeDeps(script)
	f.service.statusResult = true // 受托管 → 调 stopCurrent（bootout，不改 plist 定义）
	// PID 读取序列同 POSIX：read#1/#2 (两次 inspect)=3333；startLocked.remove 后
	// read#3+ (waitForStartReady) 返回新 child PID 6464 + 匹配 state。
	enableStartReadyRestart(f, 6464, 2, 2)
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Restart(context.Background(), loader)
	if err != nil {
		t.Fatalf("Restart err=%v", err)
	}
	if res.NewPID != 6464 {
		t.Errorf("NewPID=%d want 6464", res.NewPID)
	}
	// stop 成功后清理一次，随后 start 在确认无运行实例后再清理一次上代残留。
	if len(f.metadata.calls) != 2 {
		t.Errorf("restart 应在 stop/start 边界清理两次运行元数据，calls=%v", f.metadata.calls)
	}
	// stopCurrent 是 bootout（不改 plist 定义）；这里只验证它被调到/未调到，
	// 不存在 write/install 类 fake，故无写入可计 0。
}

// TestRestart_SuccessReturnsOldAndNewPID 成功 → RestartResult{OldPID, NewPID} 正确。
func TestRestart_SuccessReturnsOldAndNewPID(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX 路径不在 Windows 上测")
	}
	// 1. Restart.inspect true → 2. stopLocked.inspect true → SIGTERM oldPID；
	// 3. waitDaemonRelease 第1轮 false（释放）；4. startLocked.inspect false → spawn；
	// 5+. waitForStartReady true（就绪）。
	script := []bool{true, true, false, false, true}
	f := newRestartFakeDeps(script)
	f.service.statusResult = false
	f.pid.pid = 3333 // 旧 daemon PID（inspect 读到）
	// PID 读取序列：read#1 (Restart.inspect)=3333、read#2 (stopLocked.inspect)=3333；
	// startLocked.remove 清掉后，read#3+ (waitForStartReady) 返回新 child PID 7777 + 匹配 state。
	enableStartReadyRestart(f, 7777, 2, 2)
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	res, err := m.Restart(context.Background(), loader)
	if err != nil {
		t.Fatalf("Restart err=%v", err)
	}
	if res.OldPID != 3333 {
		t.Errorf("OldPID=%d want 3333", res.OldPID)
	}
	if res.NewPID != 7777 {
		t.Errorf("NewPID=%d want 7777", res.NewPID)
	}
	if len(f.kill.sigterm) != 1 || f.kill.sigterm[0] != 3333 {
		t.Errorf("应 SIGTERM 旧 PID 3333，sigterm=%v", f.kill.sigterm)
	}
	if len(f.spawn.calls) != 1 {
		t.Errorf("应 spawn 一次，calls=%d", len(f.spawn.calls))
	}
}

// TestRestart_LoadFailsInsideLock load 在锁内失败 → 透传错误，仍释放锁一次。
func TestRestart_LoadFailsInsideLock(t *testing.T) {
	f := newRestartFakeDeps([]bool{true})
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return nil, errors.New("boom restart") }

	_, err := m.Restart(context.Background(), loader)
	if err == nil || !strings.Contains(err.Error(), "boom restart") {
		t.Fatalf("应透传 loader 错误，err=%v", err)
	}
	if f.locker.unlockCount != 1 {
		t.Errorf("loader 失败也应释放锁一次，unlock=%d", f.locker.unlockCount)
	}
}

// ---- 错误路径仍释放锁 ----

// TestStart_LoadFailsInsideLock load 在锁内失败 → 透传错误，仍释放锁。
func TestStart_LoadFailsInsideLock(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return nil, errors.New("boom") }

	_, err := m.Start(context.Background(), loader)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("应透传 loader 错误，err=%v", err)
	}
	if f.locker.unlockCount != 1 {
		t.Errorf("loader 失败也应释放锁，unlock=%d", f.locker.unlockCount)
	}
}

// TestStop_LoadFailsInsideLock 同上（stop 路径）。
func TestStop_LoadFailsInsideLock(t *testing.T) {
	f := newFakeDeps()
	m := newTestProcessManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return nil, errors.New("boom2") }

	_, err := m.Stop(context.Background(), loader)
	if err == nil || !strings.Contains(err.Error(), "boom2") {
		t.Fatalf("应透传 loader 错误，err=%v", err)
	}
	if f.locker.unlockCount != 1 {
		t.Errorf("loader 失败也应释放锁，unlock=%d", f.locker.unlockCount)
	}
}
