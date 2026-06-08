// internal/control/process_ready_test.go
//
// start ready 六项条件握手与失败清理的测试。
//
// 覆盖：
//   - 六项 ready 条件逐个缺失时不 ready（超时）。
//   - 相同 PID 不同 instanceID 不 ready（防 PID 复用误判）。
//   - runtime-state 旧代晚到不 ready。
//   - pending/failed catch-up 均可完成 start（只要 monitor_ready=true）。
//   - restart 新 child 同样满足 monitor-ready 握手且 new PID != old PID。
//   - 超时不杀其他代 child（归属不匹配时不清理他代 metadata）。
//   - env 过滤无残留旧 lease（无回归）。
//   - ready 等待期间并发 ApplyConfig 无法获取 control lock，Start 返回后才可获取。
package control

import (
	"context"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// readyBaseline 装配 fakeDeps 使 start 满足全部六项 ready 条件（childPID），
// 然后返回装配后的 f 与 manager。各条件缺失测试在此基础上破坏单一条件。
func readyBaseline(t *testing.T, childPID int) (*fakeDeps, *Manager) {
	t.Helper()
	f := newFakeDeps()
	enableStartReady(f, childPID, 0, 1, 0, "pending")
	m := newTestProcessManager(t, t.TempDir(), f)
	m.deps.startReadyTimeout = 200 * time.Millisecond
	m.deps.pollInterval = 20 * time.Millisecond
	return f, m
}

// ---- 六项条件逐个缺失 ----

// TestStart_ReadyConditions 各条件单独缺失时不 ready（超时）。
// 表驱动：每行破坏一个条件，期望 start 超时返回错误。
func TestStart_ReadyConditions(t *testing.T) {
	childPID := 5100
	cases := []struct {
		name    string
		breakIt func(f *fakeDeps)
	}{
		{
			name: "条件1缺失:PID文件PID不等于childPID",
			breakIt: func(f *fakeDeps) {
				f.pid.readyPID = childPID + 1 // PID 文件指向别的 PID
			},
		},
		{
			name: "条件2缺失:PID文件instanceID不匹配",
			breakIt: func(f *fakeDeps) {
				f.pid.readyInstance = "other-generation-inst"
			},
		},
		{
			name: "条件3缺失:daemon lock未持有",
			breakIt: func(f *fakeDeps) {
				f.dlock.inner.runningWhenReady = false
			},
		},
		{
			name: "条件4缺失:runtime-state PID不等于childPID",
			breakIt: func(f *fakeDeps) {
				f.state.readyState.PID = childPID + 1
			},
		},
		{
			name: "条件5缺失:runtime-state instanceID不匹配",
			breakIt: func(f *fakeDeps) {
				f.state.readyState.InstanceID = "stale-inst"
			},
		},
		{
			name: "条件6缺失:runtime-state monitor_ready=false",
			breakIt: func(f *fakeDeps) {
				f.state.readyState.MonitorReady = false
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, m := readyBaseline(t, childPID)
			tc.breakIt(f)
			loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

			_, err := m.Start(context.Background(), loader)
			if err == nil {
				t.Fatalf("条件缺失时应超时返回错误，实际 nil")
			}
			if !strings.Contains(err.Error(), "超时") {
				t.Errorf("应为就绪超时错误，实际: %v", err)
			}
		})
	}
}

// ---- 相同 PID 不同 instanceID 不 ready（防 PID 复用误判）----

// TestStart_Ready_SamePIDDifferentInstanceID PID 文件 PID 相同但 instanceID 不同 → 不 ready。
// 模拟旧代 daemon 进程退出后被复用同 PID 启动新代：start 不得据 PID 相同就判定就绪。
func TestStart_Ready_SamePIDDifferentInstanceID(t *testing.T) {
	childPID := 5200
	f, m := readyBaseline(t, childPID)
	// PID 文件 PID == childPID，但 instanceID 是旧代（child 复用 PID 但本次 instanceID 不同）。
	f.pid.readyPID = childPID
	f.pid.readyInstance = "old-generation-inst"
	// runtime-state 也指向旧代（与 PID 文件一致，但都不是本次 instanceID）。
	f.state.readyState.PID = childPID
	f.state.readyState.InstanceID = "old-generation-inst"
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatalf("相同 PID 不同 instanceID 应超时不 ready")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("应为超时错误，实际: %v", err)
	}
}

// ---- runtime-state 旧代晚到不 ready ----

// TestStart_Ready_RuntimeStateStaleGenerationLateArrival runtime-state 旧代晚到不 ready。
// 模拟：runtime-state 文件出现且 monitor_ready=true，但 PID/instanceID 属于旧代
// （旧代 daemon 退出前的残留），不是本次 child → 不 ready。
func TestStart_Ready_RuntimeStateStaleGenerationLateArrival(t *testing.T) {
	childPID := 5300
	f, m := readyBaseline(t, childPID)
	// PID 文件指向本次 child（PID+instanceID 正确），daemon lock 持有。
	// 但 runtime-state 是旧代残留：monitor_ready=true 但 PID/instanceID 是旧代。
	f.state.readyState = runmeta.RuntimeState{
		PID:          9999, // 旧代 PID
		InstanceID:   "stale-old-inst",
		MonitorReady: true,
		CatchUp:      "succeeded",
	}
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatalf("runtime-state 旧代晚到应超时不 ready")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("应为超时错误，实际: %v", err)
	}
}

// ---- pending/failed catch-up 均可完成 start（只要 monitor ready）----

// TestStart_Ready_CatchUpPhases_Accepted 只要 monitor_ready=true，
// catch_up 为 pending/running/succeeded/failed 均可完成 start（不等 catch-up）。
func TestStart_Ready_CatchUpPhases_Accepted(t *testing.T) {
	phases := []string{"pending", "running", "succeeded", "failed"}
	for i, phase := range phases {
		childPID := 5400 + i
		t.Run(phase, func(t *testing.T) {
			f := newFakeDeps()
			enableStartReady(f, childPID, 0, 1, 0, phase)
			m := newTestProcessManager(t, t.TempDir(), f)
			loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

			res, err := m.Start(context.Background(), loader)
			if err != nil {
				t.Fatalf("catch_up=%s 应完成 start，err=%v", phase, err)
			}
			if res.PID != childPID {
				t.Errorf("catch_up=%s PID=%d want %d", phase, res.PID, childPID)
			}
		})
	}
}

// ---- restart 新 child monitor-ready 握手，new PID != old PID ----

// TestRestart_NewChildMonitorReadyHandshake restart spawn 新 child 后等六项 ready 握手；
// new PID != old PID，且 state 指向新 child 的 instanceID。
func TestRestart_NewChildMonitorReadyHandshake(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX stop 路径不在 Windows 上测")
	}
	// daemon lock 脚本：1.inspect=true 2.stop.inspect=true 3.release=false 4.start.inspect=false 5+.ready=true
	script := []bool{true, true, false, false, true}
	f := newRestartFakeDeps(script)
	f.service.statusResult = false
	f.pid.pid = 3333
	enableStartReadyRestart(f, 7777, 2, 2)
	m := newRestartManager(t, t.TempDir(), f)
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

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
	if res.NewPID == res.OldPID {
		t.Error("new PID 必须不等于 old PID")
	}
	// 新 child 的 runtime-state instanceID 应是本次（restart）instanceID，不是旧代。
	if f.state.readyState.InstanceID != f.instance {
		t.Errorf("restart 新 child state instanceID 应是本次 instanceID，实际 %q", f.state.readyState.InstanceID)
	}
}

// ---- 超时不杀其他代 child（归属不匹配时不清理他代 metadata）----

// TestStart_Timeout_DoesNotCleanupOtherGeneration 超时后 PID/runtime-state 已指向他代
// （如并发 start 覆盖、或本次 child 退出被复用为他代）→ 不清理 metadata，避免误删他代文件。
// proc 必是我们 spawn 的 child，仍 kill（防孤儿），但 metadata cleanup 不执行。
func TestStart_Timeout_DoesNotCleanupOtherGeneration(t *testing.T) {
	f := newFakeDeps()
	// 不设 ready：六项条件始终不满足 → 超时。
	f.spawn.childPID = 5500
	// 超时后 PID 文件 + runtime-state 都指向「他代」（不同 PID/instanceID），
	// 模拟并发 start 已覆盖文件、或 PID 被复用为他代。
	f.pid.pid = 9900
	f.pid.instance = "other-gen-inst"
	f.state.readErr = nil
	f.state.state = runmeta.RuntimeState{PID: 9900, InstanceID: "other-gen-inst", MonitorReady: true}
	m := newTestProcessManager(t, t.TempDir(), f)
	m.deps.startReadyTimeout = 100 * time.Millisecond
	m.deps.pollInterval = 20 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatal("应超时返回错误")
	}
	// proc 是我们 spawn 的 child，超时后仍 kill（防 detached 孤儿持有 daemon lock）。
	if len(f.spawn.killed) != 1 || f.spawn.killed[0] != 5500 {
		t.Errorf("应 kill 本次 spawn 的 child，killed=%v", f.spawn.killed)
	}
	// start 前会在确认 daemon 未运行后清理一次上代残留；超时后的归属不匹配，
	// 不得再清理他代 PID/runtime-state。
	if len(f.metadata.calls) != 1 {
		t.Errorf("归属他代时超时后不应追加清理，calls=%v", f.metadata.calls)
	}
}

// TestStart_Timeout_CleanupOwnedWhenOwnershipMatches 超时后归属仍属于本次 child，
// 但 daemon lock 仍持有时只 kill，不删除仍可能属于活进程的 metadata。
func TestStart_Timeout_CleanupOwnedWhenOwnershipMatches(t *testing.T) {
	f := newFakeDeps()
	childPID := 5600
	f.spawn.childPID = childPID
	// 不设 ready 六项全部满足（缺 monitor_ready=false 等）→ 超时。
	// 但 PID 文件 + runtime-state 都指向本次 child（归属匹配），模拟 child 启动但卡在 monitor ready 前。
	f.pid.readyAfter = 0
	f.pid.readyPID = childPID
	f.pid.readyInstance = f.instanceID
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = true
	// runtime-state 写入但 monitor_ready=false（卡在 monitor ready 阶段）。
	f.state.readErr = nil
	f.state.readyAfter = 0
	f.state.readyState = runmeta.RuntimeState{
		PID:          childPID,
		InstanceID:   f.instanceID,
		MonitorReady: false, // 关键：monitor 尚未 ready → 六项不满足 → 超时
	}
	m := newTestProcessManager(t, t.TempDir(), f)
	m.deps.startReadyTimeout = 100 * time.Millisecond
	m.deps.pollInterval = 20 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatal("monitor 未 ready 应超时")
	}
	// 归属匹配 → kill；lock 未确认释放前不追加清理 metadata。
	if len(f.spawn.killed) != 1 || f.spawn.killed[0] != childPID {
		t.Errorf("应 kill 本次 child，killed=%v", f.spawn.killed)
	}
	if len(f.metadata.calls) != 1 {
		t.Errorf("归属匹配时应清理 metadata，calls=%v", f.metadata.calls)
	}
}

// ---- env 过滤无残留旧 lease（无回归）----

// TestStart_Environment_NoResidualLeaseVars start 路径 spawn 的 child env 不残留旧 lease 变量。
// 复用 BuildChildEnv/FilterLeaseEnvVars 契约，ready 握手不应破坏 env 过滤。
// 这里用 leaseTrackingSpawn 捕获 spawnOptions，验证 lease.instanceID 非空（env 由 production 路径
// 经 BuildChildEnv 注入，单测此处只验证 instanceID 生成与传递不断链）。
func TestStart_Environment_NoResidualLeaseVars(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 5700, 0, 1, 0, "pending")
	ls := newLeaseTrackingSpawn(5700)
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	if _, err := m.Start(context.Background(), loader); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	lease := ls.lastLease()
	if lease == nil {
		t.Fatal("spawner 应收到 lease 上下文")
	}
	// instanceID 必须是本次 instanceIDGen 生成的值（与 ready fakes 匹配的同一标识）。
	if lease.instanceID != f.instanceID {
		t.Errorf("lease.instanceID=%q 应等于 fakeDeps.instanceID=%q（spawn 前 instanceIDGen 生成并传 child）",
			lease.instanceID, f.instanceID)
	}
	// 直接验证 env 过滤契约：FilterLeaseEnvVars 清掉三项残留后只剩普通变量。
	// 这是父子 lease 的核心契约，升级握手后仍必须成立。
	in := []string{
		"PATH=/usr/bin",
		"TOKEN_USAGE_START_INSTANCE=STALE",
		"TOKEN_USAGE_LEASE_FD=99",
		"HOME=/root",
	}
	out := FilterLeaseEnvVars(in)
	for _, kv := range out {
		if isLeaseEnvVar(kv) {
			t.Errorf("env 过滤后仍残留 lease 变量: %q", kv)
		}
	}
	if len(out) != 2 {
		t.Errorf("应只剩 2 个普通变量，实际 %d: %v", len(out), out)
	}
}

// ---- ready 等待期间并发 ApplyConfig 无法获取 control lock；Start 返回后才可获取 ----

// mutexFakeLocker 是真正互斥的 controlLocker 测试替身：tryLock 在已被持有时返回 false，
// unlock 后才可再次获取。多个 newLocker 返回的实例共享同一 held 状态（模拟同一 flock 文件）。
// 现有 fakeLocker（script=[true]）恒返回 true 不互斥，无法测并发阻塞，故此处单独实现。
type mutexFakeLocker struct {
	mu        sync.Mutex
	held      bool
	tryCnt    int
	unlockCnt int
}

func (l *mutexFakeLocker) tryLock() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tryCnt++
	if l.held {
		return false, nil
	}
	l.held = true
	return true, nil
}

func (l *mutexFakeLocker) unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unlockCnt++
	l.held = false
	return nil
}

// TestStart_ReadyHoldsControlLock_BlocksConcurrentApplyConfig ready 等待期间
// parent 持续持有 control lock；并发 ApplyConfig 无法获取 control lock；
// Start 返回（release lock）后才可获取。
//
// 确定性设计：用真正互斥的 mutexFakeLocker 模拟同一 control lock 文件。
// 关键断言基于 trace 顺序（调度无关）：mutexTracedLocker 在成功 tryLock 记 AcquireControlLock、
// unlock 记 ReleaseControlLock。Start 与并发 ApplyConfig 都走同一 newLocker → 同一 trace。
// 预期 trace 顺序：Start-Acquire → ... → Start-Release → Apply-Acquire → Apply-Release。
// 即「第一个 Release 必须早于第二个 Acquire」——证明 ApplyConfig 在 Start 持锁期间被阻塞，
// Start 释放后才抢到。此断言只依赖锁互斥与 trace 顺序，不依赖 goroutine 调度时序。
func TestStart_ReadyHoldsControlLock_BlocksConcurrentApplyConfig(t *testing.T) {
	f := newFakeDeps()
	// ready 在第 3 次 PID 读取后才满足（waitForStartReady 多轮轮询，给并发抢锁充分重试机会）。
	enableStartReady(f, 5800, 2, 1, 2, "pending")
	shared := &mutexFakeLocker{}
	m := &Manager{home: t.TempDir(), deps: f.asManagerDeps()}
	tr := f.trace
	m.deps.newLocker = func() controlLocker { return &mutexTracedLocker{inner: shared, trace: tr} }
	// sleep 既推进虚拟时间（fakeClock）又让出 CPU（runtime.Gosched），让 Start 的 ready-wait
	// 与 ApplyConfig 的抢锁重试交替推进。
	baseSleep := f.clock.sleep
	m.deps.sleep = func(d time.Duration) {
		baseSleep(d)
		goruntime.Gosched()
	}
	// 并发调用使用独立时钟，避免它等待同一锁时推进 Start 的虚拟 deadline。
	// 两个 manager 仍共享同一个 locker 与 trace，因此互斥关系保持不变。
	applyMgr := &Manager{home: m.home, deps: m.deps}
	applyMgr.deps.now = time.Now
	applyMgr.deps.sleep = func(time.Duration) { goruntime.Gosched() }

	loader := func() (*config.Config, error) { return newConfigWith("/data"), nil }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 等 Start 已持锁（dlock.checks>0 证明 Start 已通过 inspect 阶段、必已 AcquireControlLock）
		// 后再尝试抢锁，避免「goroutine 先抢到锁」的非预期路径。
		for {
			f.dlock.inner.mu.Lock()
			n := f.dlock.inner.checks
			f.dlock.inner.mu.Unlock()
			if n > 0 {
				break
			}
			goruntime.Gosched()
		}
		_ = applyMgr.WithLock(context.Background(), func(s *Session) error { return nil })
	}()

	if _, err := m.Start(context.Background(), loader); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	wg.Wait()

	steps := f.trace.snapshot()
	acq, rel := indexOfCount(steps, "AcquireControlLock"), indexOfCount(steps, "ReleaseControlLock")
	// 必须恰好 2 次 Acquire 与 2 次 Release（Start + ApplyConfig 各一次）。
	if acq != 2 || rel != 2 {
		t.Fatalf("应恰好 2 次 Acquire/Release，实际 Acquire=%d Release=%d steps=%v", acq, rel, steps)
	}
	// 第一个 Release（Start 释放）必须早于第二个 Acquire（ApplyConfig 获取）。
	firstRelease := indexOf(steps, "ReleaseControlLock")
	secondAcquire := indexOfNth(steps, "AcquireControlLock", 2)
	if secondAcquire <= firstRelease {
		t.Errorf("ApplyConfig 应在 Start 释放锁后获取锁：secondAcquire=%d 应 > firstRelease=%d steps=%v",
			secondAcquire, firstRelease, steps)
	}
}

// indexOfCount 统计 s 在 slice 出现次数。
func indexOfCount(slice []string, s string) int {
	n := 0
	for _, v := range slice {
		if v == s {
			n++
		}
	}
	return n
}

// indexOfNth 返回 s 第 n 次出现的 index（n 从 1 起），不存在返回 -1。
func indexOfNth(slice []string, s string, n int) int {
	seen := 0
	for i, v := range slice {
		if v == s {
			seen++
			if seen == n {
				return i
			}
		}
	}
	return -1
}

// mutexTracedLocker 包装 mutexFakeLocker，把 Acquire/Release 写入 trace。
type mutexTracedLocker struct {
	inner *mutexFakeLocker
	trace *traceRecorder
}

func (t *mutexTracedLocker) tryLock() (bool, error) {
	acquired, err := t.inner.tryLock()
	if acquired {
		t.trace.record("AcquireControlLock")
	}
	return acquired, err
}

func (t *mutexTracedLocker) unlock() error {
	err := t.inner.unlock()
	t.trace.record("ReleaseControlLock")
	return err
}
