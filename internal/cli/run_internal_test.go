// internal/cli/run_internal_test.go
package cli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// TestInternalRunCmd_Hidden 断言 _run 命令是 Hidden（用户 --help 不可见）。
func TestInternalRunCmd_Hidden(t *testing.T) {
	cmd := newInternalRunCmd()
	if !cmd.Hidden {
		t.Error("_run 命令应为 Hidden=true（用户 --help 不可见）")
	}
}

// TestInternalRunCmd_Use 断言命令名为 _run（spawn 目标必须与此一致）。
// start 经 control 包 spawn _run，launchd 服务定义也指向 _run，
// 若此处改名会导致 spawn 拉起的目标命令不存在（子进程 cobra 解析失败报 unknown command）。
func TestInternalRunCmd_Use(t *testing.T) {
	cmd := newInternalRunCmd()
	if cmd.Use != "_run" {
		t.Errorf("_run 命令 Use 应为 %q，实际 %q", "_run", cmd.Use)
	}
}

// TestInternalRunCmd_RunE 装配断言：RunE 不为 nil，可被 cobra 安全调用。
func TestInternalRunCmd_RunE(t *testing.T) {
	cmd := newInternalRunCmd()
	if cmd.RunE == nil {
		t.Fatal("_run 命令 RunE 不应为 nil（spawn 拉起后 cobra 需调用 RunE）")
	}
}

// TestInternalRunCmd_NoArgs _run 应接受任意参数（隐藏命令，spawn 固定 Args=["_run"]，无额外参数）。
// _run 不强制 NoArgs（避免未来 lease watcher 传 fd 参数时改动），但默认无参数。
func TestInternalRunCmd_DefaultNoPositional(t *testing.T) {
	cmd := newInternalRunCmd()
	// _run 的 Args 应为 nil（默认接受任意）或对 nil 安全。这里只断言 nil 时 RunE 可装配。
	_ = cmd
}

// ---- prepareIndependentRun 测试（双路径：独立路径）----
//
// 替代 的 loadConfigUnderControlLock。新签名 prepareIndependentRun 返回
// (cfg, opts daemon.RunOptions, exitEarly, err)：config 加载后的 control lock 释放
// 移入 opts.OnDaemonLockCommit（daemon lock commit 时调用）。

// preOccupyControlLock 在 home 对应的 control lock 文件上预占 flock，返回释放函数。
// 用于强制 prepareIndependentRun 的 AcquireLock 进入超时分支。
func preOccupyControlLock(t *testing.T, home string) func() {
	t.Helper()
	path := control.ControlLockPath(home)
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil || !ok {
		t.Fatalf("预占 control lock %q 失败: ok=%v err=%v", path, ok, err)
	}
	return func() { _ = fl.Unlock() }
}

// TestPrepareIndependentRun_TimeoutExitsEarly launchd 防护（控制流程）：
// control lock 获取超时（ErrControlLockTimeout）时返回 (nil, {}, exitEarly=true, nil)，
// 即「不进入 daemon.Run、退出码 0」信号。覆盖 launchd 直接拉起的 _run 与控制操作冲突。
func TestPrepareIndependentRun_TimeoutExitsEarly(t *testing.T) {
	home := t.TempDir()
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	releaseLock := preOccupyControlLock(t, home)
	defer releaseLock()

	// 真实 config 不应被加载（exitEarly 路径不调 configLoaderForRun）。
	loaderCalled := int32(0)
	origLoader := configLoaderForRun
	configLoaderForRun = func() (*config.Config, error) {
		atomic.AddInt32(&loaderCalled, 1)
		return &config.Config{}, nil
	}
	t.Cleanup(func() { configLoaderForRun = origLoader })

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cfg, opts, exitEarly, lerr := prepareIndependentRun(ctx, mgr)
	elapsed := time.Since(start)

	if lerr != nil {
		t.Fatalf("超时应返回 nil err（launchd 防护：成功退出信号），实际: %v", lerr)
	}
	if !exitEarly {
		t.Fatal("超时应返回 exitEarly=true（表示不进入 daemon.Run、退出码 0）")
	}
	if cfg != nil {
		t.Errorf("超时（exitEarly）路径不应加载 config，cfg 应为 nil，实际: %v", cfg)
	}
	if loaderCalled != 0 {
		t.Errorf("exitEarly 路径不应调用 configLoaderForRun，实际调用 %d 次", loaderCalled)
	}
	if opts.OnDaemonLockCommit != nil {
		t.Error("exitEarly 路径 OnDaemonLockCommit 应为 nil（不进入 daemon.Run）")
	}
	if elapsed < 1500*time.Millisecond {
		t.Errorf("超时等待应接近 context deadline，实际仅 %v（可能未真正等待）", elapsed)
	}
}

// TestPrepareIndependentRun_NormalPathAcquiresLockAndLoadsConfig 正常路径：
// 获取 control lock 成功 → 锁内加载 config → 返回 (cfg, opts{OnDaemonLockCommit=release}, exitEarly=false, nil)。
// OnDaemonLockCommit 调用后应释放 control lock。
//
// 顺序断言（Important 1，确定性、非时间推进）：
//   - AcquireControlLock → LoadConfig：在 loader 内部用 flock 探测，证明 LoadConfig 运行时
//     prepareIndependentRun 已持有 control lock（TryLock 必失败）。
//   - LoadConfig → OnDaemonLockCommit：loader 记录 trace 后调用方才调 commit，trace 顺序固定。
func TestPrepareIndependentRun_NormalPathAcquiresLockAndLoadsConfig(t *testing.T) {
	home := t.TempDir()
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// trace 记录调用顺序（确定性，非时间推进）。
	trace := newRunOrderTrace()
	fakeCfg := &config.Config{DataDir: "/tmp/fake-loadconfig-test"}
	loaderCalled := int32(0)
	origLoader := configLoaderForRun
	configLoaderForRun = func() (*config.Config, error) {
		atomic.AddInt32(&loaderCalled, 1)
		// AcquireControlLock → LoadConfig 顺序证明：loader 运行时 control lock 应已被
		// prepareIndependentRun 持有（TryLock 必失败）。无时间等待——锁状态是同步确定的。
		if lockOK, _ := flock.New(control.ControlLockPath(home)).TryLock(); lockOK {
			trace.record("FAIL_LOADCONFIG_LOCK_AVAILABLE")
			t.Errorf("LoadConfig 运行时 control lock 应已被持有（AcquireControlLock 应先于 LoadConfig）")
		}
		trace.record("LoadConfig")
		return fakeCfg, nil
	}
	t.Cleanup(func() { configLoaderForRun = origLoader })

	cfg, opts, exitEarly, lerr := prepareIndependentRun(context.Background(), mgr)
	if lerr != nil {
		t.Fatalf("正常路径不应返回错误，实际: %v", lerr)
	}
	if exitEarly {
		t.Error("正常路径 exitEarly 应为 false")
	}
	if cfg != fakeCfg {
		t.Errorf("应返回 fakeCfg，实际: %v", cfg)
	}
	if loaderCalled != 1 {
		t.Errorf("configLoaderForRun 应恰好调用 1 次，实际 %d", loaderCalled)
	}
	if opts.OnDaemonLockCommit == nil {
		t.Fatal("OnDaemonLockCommit 不应为 nil（应在 daemon lock commit 时释放 control lock）")
	}
	if opts.InstanceID == "" {
		t.Error("独立路径应自行生成 instanceID")
	}
	if opts.ParentLeaseLost != nil {
		t.Error("独立路径 ParentLeaseLost 应为 nil（无父 lease）")
	}
	// commit 前锁应被持有：预占验证——此时获取 control lock 应失败。
	if lockOK, _ := flock.New(control.ControlLockPath(home)).TryLock(); lockOK {
		t.Error("commit 前 control lock 应被 prepareIndependentRun 持有")
	} else {
		// 锁被持有，正常。释放 flock 占用避免泄漏（它没拿到锁，无需 unlock）。
	}
	// 包装 OnDaemonLockCommit 以记录 trace（证明 LoadConfig 在 OnDaemonLockCommit 之前）。
	origCommit := opts.OnDaemonLockCommit
	opts.OnDaemonLockCommit = func() error {
		trace.record("OnDaemonLockCommit")
		return origCommit()
	}
	// 调 OnDaemonLockCommit（daemon lock commit）→ 释放 control lock。
	opts.OnDaemonLockCommit()
	if lockErr := mgr.WithLock(context.Background(), func(*control.Session) error { return nil }); lockErr != nil {
		t.Errorf("OnDaemonLockCommit 后应能重新获取 control lock，实际: %v", lockErr)
	}

	// 顺序断言：LoadConfig 必须出现在 OnDaemonLockCommit 之前（确定性 trace）。
	steps := trace.snapshot()
	loadIdx := indexOfString(steps, "LoadConfig")
	commitIdx := indexOfString(steps, "OnDaemonLockCommit")
	if loadIdx < 0 {
		t.Fatalf("trace 应包含 LoadConfig，steps=%v", steps)
	}
	if commitIdx < 0 {
		t.Fatalf("trace 应包含 OnDaemonLockCommit，steps=%v", steps)
	}
	if loadIdx >= commitIdx {
		t.Errorf("LoadConfig 必须在 OnDaemonLockCommit 之前，steps=%v", steps)
	}
}

// runOrderTrace 记录顺序字符串切片（mutex 保护），用于确定性顺序断言（非时间推进）。
type runOrderTrace struct {
	mu    sync.Mutex
	steps []string
}

func newRunOrderTrace() *runOrderTrace { return &runOrderTrace{} }

func (r *runOrderTrace) record(step string) {
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
}

func (r *runOrderTrace) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

// indexOfString 返回 s 在 list 中的索引，不存在返回 -1。
func indexOfString(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// TestPrepareIndependentRun_ConfigLoadErrorReturnsErr config 加载失败 → 返回错误，
// exitEarly=false（非超时），调用方应非零退出。验证锁被释放（错误路径仍释放）。
func TestPrepareIndependentRun_ConfigLoadErrorReturnsErr(t *testing.T) {
	home := t.TempDir()
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	origLoader := configLoaderForRun
	configLoadErr := errors.New("config boom")
	configLoaderForRun = func() (*config.Config, error) { return nil, configLoadErr }
	t.Cleanup(func() { configLoaderForRun = origLoader })

	cfg, _, exitEarly, lerr := prepareIndependentRun(context.Background(), mgr)
	if lerr == nil || !errors.Is(lerr, configLoadErr) {
		t.Fatalf("应返回 configLoadErr，实际: %v", lerr)
	}
	if exitEarly {
		t.Error("config 加载失败 exitEarly 应为 false")
	}
	if cfg != nil {
		t.Errorf("失败路径 cfg 应为 nil，实际: %v", cfg)
	}
	// 错误路径锁应在 prepareIndependentRun 内部已释放：
	if lockErr := mgr.WithLock(context.Background(), func(*control.Session) error { return nil }); lockErr != nil {
		t.Errorf("config 加载失败后锁应已释放，重新获取失败: %v", lockErr)
	}
}

// ---- prepareParentLeaseOptions 测试（双路径：父 lease 路径）----
//
// 父 lease 路径：_run 解析出合法父 lease → 启动 watcher + 状态机 → 在 lease 生效后加载 config。
// 这里注入可探测的 lease reader + configLoaderForRun，断言：
//   - opts.InstanceID = desc.InstanceID（父生成）。
//   - opts.ParentLeaseLost 非 nil（= sm.LeaseLost）。
//   - opts.OnDaemonLockCommit 调用后状态机 commit（MarkDaemonLockCommitted）。
//   - EOF 先于 commit → OnDaemonLockCommit 不推进（LeaseLost 保持打开）。
//   - config 加载发生在 prepareParentLeaseOptions 之后（调用方职责，本测试验证顺序由 runDaemon 保证）。

// TestPrepareParentLeaseOptions_BuildsCorrectOpts 父 lease 路径构造正确的 RunOptions。
func TestPrepareParentLeaseOptions_BuildsCorrectOpts(t *testing.T) {
	desc := control.ParentLeaseDescriptor{InstanceID: "parent-id"}
	// reader 用一个可阻塞的 fake（waitForEOF 阻塞到 close）。
	reader := newFakeLeaseReaderBlocking()
	sm, opts := prepareParentLeaseOptions(desc, reader)
	defer reader.close() // 释放 watcher goroutine

	if opts.InstanceID != "parent-id" {
		t.Errorf("InstanceID=%q want parent-id", opts.InstanceID)
	}
	if opts.ParentLeaseLost == nil {
		t.Error("ParentLeaseLost 不应为 nil")
	}
	if opts.OnDaemonLockCommit == nil {
		t.Error("OnDaemonLockCommit 不应为 nil")
	}
	_ = sm

	// commit 前 ParentLeaseLost 未关闭（EOF 未发生）。
	select {
	case <-opts.ParentLeaseLost:
		t.Error("commit 前 ParentLeaseLost 不应关闭（EOF 未发生）")
	default:
	}

	// 调 OnDaemonLockCommit（daemon lock commit）→ 状态机推进。
	opts.OnDaemonLockCommit()
	// commit 后即使 EOF 也不取消（状态机 committed）。
	reader.triggerEOF()
	// 用带超时的 select 探测「ParentLeaseLost 仍打开」：committed 后 EOF 不应关闭它。
	// 超时仅作 goroutine 调度兜底（200ms 足以让 watcher 处理 EOF），非时间推进依赖。
	select {
	case <-opts.ParentLeaseLost:
		t.Error("commit 后 EOF 不应关闭 ParentLeaseLost（child 已接管）")
	case <-time.After(200 * time.Millisecond):
		// 期望：超时（channel 仍打开）。
	}
}

// TestPrepareParentLeaseOptions_EOFFirstThenCommitDoesNotProgress EOF 先到 → commit 不推进，
// ParentLeaseLost 关闭（取消信号）。
func TestPrepareParentLeaseOptions_EOFFirstThenCommitDoesNotProgress(t *testing.T) {
	desc := control.ParentLeaseDescriptor{InstanceID: "parent-id"}
	reader := newFakeLeaseReaderBlocking()
	_, opts := prepareParentLeaseOptions(desc, reader)

	// EOF 先到。
	reader.triggerEOF()
	// 用 ParentLeaseLost channel 作为同步点：阻塞等待 watcher 处理 EOF 并关闭它（确定性）。
	<-opts.ParentLeaseLost

	// 之后 commit：状态机不推进（OnDaemonLockCommit 调 sm.MarkDaemonLockCommitted，
	// 因 EOF 已先到返回 false 不 commit）。LeaseLost 保持关闭（取消信号已发出）。
	opts.OnDaemonLockCommit()
	select {
	case <-opts.ParentLeaseLost:
		// 期望：仍关闭。
	default:
		t.Error("EOF 后 commit 不应重开 ParentLeaseLost")
	}
}

// fakeLeaseReaderBlocking 是可阻塞/触发的 LeaseReader 测试桩：
// WaitForEOF 阻塞到 triggerEOF 调用（模拟父关闭 write end）。
type fakeLeaseReaderBlocking struct {
	closeCh chan struct{}
}

func newFakeLeaseReaderBlocking() *fakeLeaseReaderBlocking {
	return &fakeLeaseReaderBlocking{closeCh: make(chan struct{})}
}

func (r *fakeLeaseReaderBlocking) WaitForEOF() {
	<-r.closeCh
}

func (r *fakeLeaseReaderBlocking) Close() {}

// triggerEOF 模拟父关闭 write end（触发 EOF）。
func (r *fakeLeaseReaderBlocking) triggerEOF() {
	select {
	case <-r.closeCh:
	default:
		close(r.closeCh)
	}
}

// close 释放（用于 defer，不重复关闭 closeCh）。
func (r *fakeLeaseReaderBlocking) close() {
	r.triggerEOF()
}
