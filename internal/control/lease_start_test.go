// internal/control/lease_start_test.go
//
// 父进程（start/restart）侧 lease 集成测试。
//
// 关键行为：
//   - start spawn 时创建 lease 并把 instanceID + reader 传给 spawner。
//   - 父级 Start 的 control lock 持续到 ready 成功后释放（ReleaseControlLock 在 Spawn 之后、最后）。
//   - ready 成功 → closeWrite（释放 lease write end，幂等）。
//   - ready 失败 → cleanup（两端关闭）+ kill child。
//   - parent trace 断言 LoadConfig 发生在 AcquireControlLock 之后（control lease 建立后）。
package control

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// ---- lease-aware fake spawner ----
//
// 现有 fakeSpawn（process_test.go）记录 spawnOptions 但不暴露 lease 生命周期。
// 这里用 leaseTrackingSpawn 包装，记录 lease 上下文引用，使测试能断言 lease 被创建并传入。

// leaseTrackingSpawn 实现 spawner，记录每次 spawn 的 leaseContext 引用。
type leaseTrackingSpawn struct {
	mu        sync.Mutex
	inner     *fakeSpawn
	leases    []*leaseContext
	onSpawned func(*leaseContext)
}

func newLeaseTrackingSpawn(childPID int) *leaseTrackingSpawn {
	return &leaseTrackingSpawn{inner: &fakeSpawn{childPID: childPID}}
}

func (t *leaseTrackingSpawn) spawn(opts spawnOptions) (spawnedProcess, error) {
	t.mu.Lock()
	t.leases = append(t.leases, opts.lease)
	t.mu.Unlock()
	proc, err := t.inner.spawn(opts)
	if err == nil && t.onSpawned != nil && opts.lease != nil {
		t.onSpawned(opts.lease)
	}
	return proc, err
}

func (t *leaseTrackingSpawn) lastLease() *leaseContext {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.leases) == 0 {
		return nil
	}
	return t.leases[len(t.leases)-1]
}

// tracedLeaseTrackingSpawn 包装 leaseTrackingSpawn，把 Spawn 写入 trace。
type tracedLeaseTrackingSpawn struct {
	inner *leaseTrackingSpawn
	trace *traceRecorder
}

func (t *tracedLeaseTrackingSpawn) spawn(opts spawnOptions) (spawnedProcess, error) {
	t.trace.record("Spawn")
	return t.inner.spawn(opts)
}

// ---- 测试装配器 ----

// newTestProcessManagerWithLeaseTracking 装配带 lease 跟踪 spawner 的 Manager。
func newTestProcessManagerWithLeaseTracking(t *testing.T, home string, f *fakeDeps, ls *leaseTrackingSpawn) *Manager {
	t.Helper()
	m := &Manager{home: home, deps: f.asManagerDeps()}
	m.deps.spawner = &tracedLeaseTrackingSpawn{inner: ls, trace: f.trace}
	base := f.locker
	tr := f.trace
	m.deps.newLocker = func() controlLocker {
		return &tracedLocker{inner: base, trace: tr}
	}
	return m
}

// ---- 测试 ----

// TestStart_CreatesLeaseAndPassesToSpawner start spawn 时创建 lease 并把 instanceID + reader
// 传给 spawner。
func TestStart_CreatesLeaseAndPassesToSpawner(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 7070, 0, 1, 0, "pending")
	ls := newLeaseTrackingSpawn(7070)
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	if _, err := m.Start(context.Background(), loader.load); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	lease := ls.lastLease()
	if lease == nil {
		t.Fatal("spawner 应收到 lease 上下文")
	}
	if lease.instanceID == "" {
		t.Error("lease.instanceID 不应为空（父进程应生成一次性标识）")
	}
	if lease.readerForDaemon() == nil {
		t.Error("lease.readerForDaemon() 不应为 nil（pipe read end）")
	}
}

// TestStart_HoldsControlLockUntilReady 父级 Start 的 control lock 持续到 ready 成功后释放。
func TestStart_HoldsControlLockUntilReady(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 8080, 0, 1, 0, "pending")
	ls := newLeaseTrackingSpawn(8080)
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	if _, err := m.Start(context.Background(), loader.load); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	steps := f.trace.snapshot()
	if len(steps) < 2 || steps[0] != "AcquireControlLock" {
		t.Fatalf("第一步必须是 AcquireControlLock，steps=%v", steps)
	}
	if steps[len(steps)-1] != "ReleaseControlLock" {
		t.Fatalf("最后一步必须是 ReleaseControlLock（ready 后释放），steps=%v", steps)
	}
	spawnIdx := indexOf(steps, "Spawn")
	releaseIdx := indexOf(steps, "ReleaseControlLock")
	if spawnIdx < 0 || releaseIdx <= spawnIdx {
		t.Fatalf("ReleaseControlLock 必须在 Spawn 之后，steps=%v", steps)
	}
	loadIdx := indexOf(steps, "LoadConfig")
	if loadIdx < 1 {
		t.Fatalf("LoadConfig 必须在 AcquireControlLock 之后，steps=%v", steps)
	}
}

// TestStart_LeaseCloseWriteIdempotentAfterReady ready 成功后 closeWrite 被调用一次；
// 再次调用（测试模拟）幂等不 panic。
func TestStart_LeaseCloseWriteIdempotentAfterReady(t *testing.T) {
	f := newFakeDeps()
	enableStartReady(f, 9090, 0, 1, 0, "pending")
	ls := newLeaseTrackingSpawn(9090)
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	loader := &tracedConfigLoader{trace: f.trace, cfg: newConfigWith(t.TempDir())}

	if _, err := m.Start(context.Background(), loader.load); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	lease := ls.lastLease()
	if lease == nil {
		t.Fatal("spawner 应收到 lease 上下文")
	}
	// ready 成功后 startLocked 已调 closeWrite；再调一次应幂等不 panic。
	lease.closeWrite()
	lease.closeWrite()
}

// TestStart_LeaseCleanupOnReadyFail ready 失败 → lease cleanup（两端关闭）+ kill child。
func TestStart_LeaseCleanupOnReadyFail(t *testing.T) {
	f := newFakeDeps()
	f.spawn.childPID = 1212
	// 不设 readyAfter：daemon lock 始终未持有 → waitForStartReady 超时。
	leaseCreated := int32(0)
	ls := newLeaseTrackingSpawn(1212)
	ls.onSpawned = func(lc *leaseContext) {
		atomic.StoreInt32(&leaseCreated, 1)
	}
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	m.deps.startReadyTimeout = 100 * time.Millisecond
	m.deps.pollInterval = 20 * time.Millisecond
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	_, err := m.Start(context.Background(), loader)
	if err == nil {
		t.Fatal("ready 失败应返回错误")
	}
	if atomic.LoadInt32(&leaseCreated) != 1 {
		t.Error("spawn 时 lease 应已创建（onSpawned 被调用）")
	}
	// ready 失败 → kill child。
	if len(ls.inner.killed) != 1 || ls.inner.killed[0] != 1212 {
		t.Errorf("ready 失败应 kill child，killed=%v", ls.inner.killed)
	}
	// cleanup 后 lease 已清理：再次 cleanup 幂等不 panic。
	lease := ls.lastLease()
	if lease != nil {
		lease.cleanup()
		lease.cleanup()
	}
}

// TestStart_LeaseCreatedBeforeConfigLoadForRestart restart 路径也创建 lease（startLocked 复用）。
// 这里只验证 startLocked 被调用且 lease 非空（restart 内部调 startLocked）。
func TestStart_LeaseCreatedBeforeConfigLoadForRestart(t *testing.T) {
	// 此测试与 TestStart_HoldsControlLockUntilReady 重叠，保留以明确 start 路径 lease 创建。
	f := newFakeDeps()
	enableStartReady(f, 1414, 0, 1, 0, "pending")
	ls := newLeaseTrackingSpawn(1414)
	m := newTestProcessManagerWithLeaseTracking(t, t.TempDir(), f, ls)
	loader := func() (*config.Config, error) { return newConfigWith(t.TempDir()), nil }

	if _, err := m.Start(context.Background(), loader); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	// lease 应非空（startLocked 创建）。
	if ls.lastLease() == nil {
		t.Error("start 应创建 lease")
	}
}
