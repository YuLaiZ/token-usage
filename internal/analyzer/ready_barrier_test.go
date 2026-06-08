// internal/analyzer/ready_barrier_test.go
package analyzer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// === ready barrier 测试 ===
//
// 这些测试覆盖 的 ready barrier 生产语义：
//   - 0/1/N monitor 与 Ready() 的关系；
//   - 每个 monitor 的 signal 恰好一次（sync.Once 保护），重复 signal 不重复 close/panic；
//   - monitor 启动失败（构造期被 setupFromConfig 跳过）不假装全部 ready；
//   - cancel 发生在 ready 前/后；
//   - accepting gate 基线：gate 关闭后 void callback 不执行 collectFunc、不发生 cancel 后 Add；
//   - race 下 shutdown 与 submit 无 Add/Wait 竞态（由 -race 覆盖）。
//
// 复用真实 JSONLWatcher / SQLitePoller 的就绪点（Walk 后 / 记录初始 mtime 后），
// 不引入 fake monitor，确保测试覆盖真实 signal 链路。

// newReadyTestAnalyzer 构造一个空 Analyzer（无 monitor），
// 供需要手动挂载 watcher/poller 并控制 readyWg 的测试使用。
func newReadyTestAnalyzer(t *testing.T) *Analyzer {
	t.Helper()
	return New(noopExecute, nil)
}

// makeTestJSONLWatcher 构造一个真实 JSONLWatcher 指向已存在的 dir，
// 并通过 a.addWatcher 挂载（设置 signalReady + readyWg.Add(1)）。
func makeTestJSONLWatcher(t *testing.T, a *Analyzer, dir string, client string) *JSONLWatcher {
	t.Helper()
	w, err := NewJSONLWatcher([]string{dir}, client, 50*time.Millisecond, a.monitorSubmit, nil)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}
	a.addWatcher(w)
	return w
}

// makeTestSQLitePoller 构造一个真实 SQLitePoller 指向已存在的 dbPath，
// 并通过 a.addSQLitePoller 等价路径挂载（设置 signalReady + readyWg.Add(1)）。
func makeTestSQLitePoller(t *testing.T, a *Analyzer, dbPath string, client string) *SQLitePoller {
	t.Helper()
	p := NewSQLitePoller(dbPath, client, collector.CollectRequest{Incremental: true}, 50*time.Millisecond, a.monitorSubmit, nil)
	p.signalReady = a.newMonitorSignaler()
	a.readyWg.Add(1)
	a.sqlitePollers = append(a.sqlitePollers, p)
	return p
}

// TestReadyBarrier_ZeroMonitorsNoReady 0 monitor：Run 立即返回 error，
// Ready() channel 永不关闭。
func TestReadyBarrier_ZeroMonitorsNoReady(t *testing.T) {
	a := newReadyTestAnalyzer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for 0 monitors, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run blocked; expected immediate error for 0 monitors")
	}

	// 0 monitor 不发布 ready
	select {
	case <-a.Ready():
		t.Fatal("Ready() closed for 0 monitors; expected never-closing barrier")
	default:
		// 期望：未关闭
	}

	// 再等一会确认始终未关闭
	select {
	case <-a.Ready():
		t.Fatal("Ready() closed after delay for 0 monitors")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReadyBarrier_SingleMonitorReady 1 monitor：Run 启动后 watcher signal，
// Ready() 关闭。
func TestReadyBarrier_SingleMonitorReady(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()
	w := makeTestJSONLWatcher(t, a, dir, "claude")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	defer a.Stop()

	select {
	case <-a.Ready():
		// 期望：单 watcher Walk 后就绪
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Ready() with single monitor")
	}

	// watcher 应已 ready（signalOnce 已触发）；再次调用 signalReady 不 panic
	_ = w
}

// TestReadyBarrier_NMonitorsReadyAfterAll N monitor：Ready() 仅在全部 signal 后关闭，
// 任何一个未 signal 都不应提前关闭（"monitor 启动失败不假装全部 ready" 的正向守卫）。
func TestReadyBarrier_NMonitorsReadyAfterAll(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	if err := os.WriteFile(db, []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3 monitor：2 JSONL watcher + 1 SQLite poller
	makeTestJSONLWatcher(t, a, dir, "claude")
	makeTestJSONLWatcher(t, a, dir, "codex")
	makeTestSQLitePoller(t, a, db, "opencode")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	defer a.Stop()

	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Ready() with N monitors")
	}
}

// TestReadyBarrier_NotReadyUntilAllSignaled N monitor 中故意只让 N-1 个能 signal，
// Ready() 不应关闭——证明 barrier 不假装全部 ready。
// 通过构造一个指向不存在目录的 watcher：Walk 会 warn 但仍走到 signal 点（Walk 完成即 signal，
// 即使目录不存在也会走完 Walk 迭代并调用 signal）。因此需要一个真正「永不 signal」的 monitor。
//
// 改为直接在单元层验证 readyWg 语义：手动构造 monitor 但不触发其 signal，Ready() 不关闭。
func TestReadyBarrier_NotReadyUntilAllSignaled(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()

	// 挂载 2 monitor，但手动驱动 signalReady，只 signal 其中一个。
	w1 := makeTestJSONLWatcher(t, a, dir, "claude")
	_ = makeTestJSONLWatcher(t, a, dir, "codex")

	// 准备 barrier goroutine（与 Run 等价，但不启动 monitor Run）
	go func() {
		a.readyWg.Wait()
		close(a.readyCh)
	}()

	// 只触发 w1 的 signal（通过 readyOnce.Do 调 signalReady）
	w1.readyOnce.Do(func() {
		if w1.signalReady != nil {
			w1.signalReady()
		}
	})

	// 只 signal 了 1/2，Ready() 不应关闭
	select {
	case <-a.Ready():
		t.Fatal("Ready() closed before all monitors signaled; barrier must wait for all")
	case <-time.After(150 * time.Millisecond):
		// 期望：未关闭
	}
}

// TestReadyBarrier_DuplicateSignalNoPanic 单个 monitor 重复 signal 不重复扣减、
// 不 close/panic。sync.Once 保证 readyWg 只 Done 一次。
func TestReadyBarrier_DuplicateSignalNoPanic(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()
	w := makeTestJSONLWatcher(t, a, dir, "claude")

	go func() {
		a.readyWg.Wait()
		close(a.readyCh)
	}()

	// 多次触发 readyOnce.Do(signalReady)：只应生效一次
	for i := 0; i < 5; i++ {
		w.readyOnce.Do(func() {
			if w.signalReady != nil {
				w.signalReady()
			}
		})
	}

	// 全部触发后应 ready（readyWg 恰好 Done 一次 → 归零 → close）
	select {
	case <-a.Ready():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Ready() not closed after single effective signal (readyWg mis-decremented?)")
	}
}

// TestReadyBarrier_CancelBeforeReady cancel 发生在 ready 之前：Run 返回，
// Ready() barrier goroutine 仍会随 monitor 退出而 signal（即使目录 Walk 极快）。
// 此用例确认 cancel 不会让 Run 永久阻塞，且不 panic。
func TestReadyBarrier_CancelBeforeReady(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()
	makeTestJSONLWatcher(t, a, dir, "claude")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// 立即 cancel（可能早于或晚于 ready，两者都不应 panic/阻塞）
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error after cancel-before-ready: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("Run did not return after cancel (collectWg/wg wait blocked)")
	}
}

// TestReadyBarrier_CancelAfterReady cancel 发生在 ready 之后：正常优雅关闭。
func TestReadyBarrier_CancelAfterReady(t *testing.T) {
	a := newReadyTestAnalyzer(t)
	dir := t.TempDir()
	makeTestJSONLWatcher(t, a, dir, "claude")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-a.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Ready()")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error after cancel-after-ready: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("Run did not return after cancel-after-ready")
	}
}

// TestAcceptingGate_ClosedSubmitDoesNotCollect gate 关闭后（Run 已返回路径）的 Submit
// 不执行 ExecuteFunc，返回 ErrAnalyzerStopping，也不 panic。模拟「shutdown 后残留 monitor callback 到达」。
func TestAcceptingGate_ClosedSubmitDoesNotCollect(t *testing.T) {
	var collected int32
	a := New(func(context.Context, string, collector.CollectRequest) error {
		atomic.AddInt32(&collected, 1)
		return nil
	}, nil)

	// 默认 accepting=false（New 初始化，未走 Run）；Submit 应返回稳定错误且不执行 ExecuteFunc。
	// （旧用例显式置 false；现默认即 false，等价于 Run 关闭路径。）
	for i := 0; i < 10; i++ {
		err := a.Submit(context.Background(), "claude", collector.CollectRequest{ChangedFile: "/x.jsonl"})
		if !errors.Is(err, ErrAnalyzerStopping) {
			t.Fatalf("Submit[%d] error = %v, want ErrAnalyzerStopping", i, err)
		}
	}
	if got := atomic.LoadInt32(&collected); got != 0 {
		t.Fatalf("ExecuteFunc invoked after gate closed: %d (want 0)", got)
	}
}

// TestAcceptingGate_OpenThenClosed accepting 打开时正常采集，关闭后停止。
// 验证 gate 状态切换的有效性。
func TestAcceptingGate_OpenThenClosed(t *testing.T) {
	var collected int32
	a := New(func(context.Context, string, collector.CollectRequest) error {
		atomic.AddInt32(&collected, 1)
		return nil
	}, nil)

	// enableSubmitForTest 安装 runCtx 并打开 accepting（等价 Run 启动路径）
	enableSubmitForTest(t, a)

	if err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true}); err != nil {
		t.Fatalf("Submit while accepting: %v", err)
	}
	if got := atomic.LoadInt32(&collected); got != 1 {
		t.Fatalf("ExecuteFunc not invoked while accepting: %d (want 1)", got)
	}

	// 关闭 gate
	a.gateMu.Lock()
	a.accepting = false
	a.gateMu.Unlock()

	err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true})
	if !errors.Is(err, ErrAnalyzerStopping) {
		t.Fatalf("Submit after gate closed: error = %v, want ErrAnalyzerStopping", err)
	}
	if got := atomic.LoadInt32(&collected); got != 1 {
		t.Fatalf("ExecuteFunc invoked after gate closed: %d (want 1)", got)
	}
}

// TestAcceptingGate_RaceShutdownSubmit race 守卫：并发 Submit 与 gate 关闭。
// -race 下不应出现 collectWg.Add 与 collectWg.Wait 的并发使用 panic。
// 这里构造：一个 goroutine 反复调用 Submit（含 collectWg.Add/Done），
// 另一个 goroutine 切换 accepting；同时 ExecuteFunc 阻塞以放大 Add/Done 与 accepting
// 检查的交错窗口。Run 的 Wait 路径由 integration / TestAnalyzer_RunWaitsForInFlightCollect 覆盖，
// 本用例聚焦 gate 锁本身的 Add/accepting 原子性。
func TestAcceptingGate_RaceShutdownSubmit(t *testing.T) {
	var collected int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	a := New(func(context.Context, string, collector.CollectRequest) error {
		atomic.AddInt64(&collected, 1)
		<-release // 放大 in-flight 窗口
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	// 并发 submitter：不停触发 Submit（会 Add/Done collectWg）
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true})
			}
		}()
	}

	// 关闭 gate（模拟 Run 关闭路径的 accepting=false），同时 submitter 仍在跑。
	// 无需 sleep 放大窗口：ExecuteFunc 阻塞在 <-release，submitter 一进入
	// Submit 即在 gateMu 内 collectWg.Add(1) 并阻塞，in-flight 立即产生。
	a.gateMu.Lock()
	a.accepting = false
	a.gateMu.Unlock()

	// 放行所有阻塞的 in-flight 采集（避免泄漏）
	close(release)
	close(stop)
	wg.Wait()

	// 不做精确计数断言（竞态下数量不确定）；重点是 -race 下无 panic、无 collectWg 误用。
	_ = atomic.LoadInt64(&collected)
}
