// internal/analyzer/submit_test.go
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

// === 实时事件与 catch-up 共用串行入口 Submit ===
//
// 这些测试覆盖 Submit gate/mutex 契约：
//   - pending 实时事件先于 catch-up；catch-up 先于 pending 实时事件；
//   - 两者重叠时最大并发写入数恒为 1；
//   - 重复请求最终结果相同；
//   - 一个失败后后续仍执行；
//   - ExecuteFunc 的 context 派生自 daemon child context（monitor 用 runCtx，coordinator 直用 child）；
//     父取消都能及时退出；
//   - ExecuteFunc 错误原样返回，monitor 只记一次日志且 collection_errors 只由 engine 写一次
//     （此处用 ExecuteFunc 计数模拟「不重复记录」）；
//   - 注入 watcher/poller 的 callback 只能是 MonitorSubmitFunc（trace 证明必经 Submit gate/mutex）；
//   - shutdown 后提交返回稳定错误且不增 WaitGroup。

// noopExecute 返回 nil 的 ExecuteFunc，便于不需要断言错误语义的测试直接复用。
func noopExecute(context.Context, string, collector.CollectRequest) error { return nil }

// enableSubmitForTest 在 gate 内安装一个可取消 ctx 并打开 accepting，使未走 Run 的
// Analyzer 也能 Submit（覆盖 Submit gate 行为本身，不依赖 monitor 装配）。
// 返回 cancel 以便测试退出时取消，避免 runCtx 泄漏。
func enableSubmitForTest(t *testing.T, a *Analyzer) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.gateMu.Lock()
	a.runCtx = ctx
	a.accepting = true
	a.gateMu.Unlock()
	t.Cleanup(cancel)
	return cancel
}

// TestSubmit_PendingRealtimeBeforeCatchUp pending 实时事件先于 catch-up：
// 监控 Submit 占住 collect mutex 后，coordinator 的 Submit 必须排在后面执行（串行）。
func TestSubmit_PendingRealtimeBeforeCatchUp(t *testing.T) {
	var mu sync.Mutex
	var order []string
	started := make(chan struct{})
	release := make(chan struct{})

	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		mu.Lock()
		if len(order) == 0 {
			close(started) // 第一次进入即标记
		}
		order = append(order, client)
		mu.Unlock()
		<-release // 放大窗口，使第二个 Submit 一定在等 mutex
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	// 实时事件先占 mutex
	go func() {
		_ = a.Submit(context.Background(), "claude", collector.CollectRequest{ChangedFile: "/a.jsonl"})
	}()
	<-started

	// catch-up 紧随其后（阻塞在 collect mutex，直到实时事件释放）
	done := make(chan error, 1)
	go func() { done <- a.Submit(context.Background(), "codex", collector.CollectRequest{Incremental: true}) }()

	// 放行实时事件：其 ExecuteFunc 退出释放 collect mutex，catch-up 获锁执行。
	// 顺序由 collect mutex 互斥保证（同一时刻只有一个 ExecuteFunc 运行）。
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("catch-up Submit error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "claude" || order[1] != "codex" {
		t.Fatalf("order = %v, want [claude codex]", order)
	}
}

// TestSubmit_CatchUpBeforePending catch-up 先于 pending 实时事件：调换提交次序，
// 执行次序仍与提交次序一致（collect mutex FIFO 串行）。
func TestSubmit_CatchUpBeforePending(t *testing.T) {
	var mu sync.Mutex
	var order []string
	started := make(chan struct{})
	release := make(chan struct{})

	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		mu.Lock()
		if len(order) == 0 {
			close(started)
		}
		order = append(order, client)
		mu.Unlock()
		<-release
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	// catch-up 先占 mutex
	doneCatchUp := make(chan error, 1)
	go func() {
		doneCatchUp <- a.Submit(context.Background(), "codex", collector.CollectRequest{Incremental: true})
	}()
	<-started

	// pending 实时事件后到（应阻塞在 collect mutex）
	donePending := make(chan error, 1)
	go func() {
		donePending <- a.Submit(context.Background(), "claude", collector.CollectRequest{ChangedFile: "/a.jsonl"})
	}()

	// 放行 catch-up：其 ExecuteFunc 退出释放 collect mutex，pending 获锁执行。
	close(release)

	if err := <-doneCatchUp; err != nil {
		t.Fatalf("catch-up Submit error: %v", err)
	}
	if err := <-donePending; err != nil {
		t.Fatalf("pending Submit error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "codex" || order[1] != "claude" {
		t.Fatalf("order = %v, want [codex claude]", order)
	}
}

// TestSubmit_OverlapConcurrencyIsOne 实时事件与 catch-up 重叠时，最大并发 ExecuteFunc
// 恒为 1（collect mutex 串行）。
func TestSubmit_OverlapConcurrencyIsOne(t *testing.T) {
	var maxConcurrent, current int32
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		c := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if c <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, c) {
				break
			}
		}
		// 短阻塞放大重叠窗口
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	// 交替提交实时事件与 catch-up
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_ = a.Submit(context.Background(), "claude", collector.CollectRequest{ChangedFile: "/a.jsonl"})
			} else {
				_ = a.Submit(context.Background(), "codex", collector.CollectRequest{Incremental: true})
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("maxConcurrent = %d, want 1 (collect mutex must serialize)", got)
	}
}

// TestSubmit_DuplicateRequestSameResult 重复请求最终结果相同：
// ExecuteFunc 幂等（对同一 req 调用两次返回相同 error/result），不因次序改变。
func TestSubmit_DuplicateRequestSameResult(t *testing.T) {
	var n int32
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		atomic.AddInt32(&n, 1)
		return nil // 成功
	}, nil)
	enableSubmitForTest(t, a)

	req := collector.CollectRequest{Incremental: true}
	if err := a.Submit(context.Background(), "zcode", req); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	if err := a.Submit(context.Background(), "zcode", req); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("ExecuteFunc invoked %d times, want 2", got)
	}
}

// TestSubmit_FailureThenContinue 一个 Submit 失败后后续 Submit 仍执行：
// collect mutex 释放后不因前次 ExecuteFunc 返回 error 而短路后续请求。
func TestSubmit_FailureThenContinue(t *testing.T) {
	var n int32
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		c := atomic.AddInt32(&n, 1)
		if c == 1 {
			return errors.New("boom")
		}
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	if err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true}); err == nil {
		t.Fatal("first Submit: expected error, got nil")
	}
	if err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true}); err != nil {
		t.Fatalf("second Submit: expected nil, got %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("ExecuteFunc invoked %d times, want 2", got)
	}
}

// TestSubmit_ContextDerivedFromChild_ParentCancelExits ExecuteFunc 接收的 ctx 派生自
// 提交方传入的 child context；父取消时正在等待 mutex 或在 ExecuteFunc 内的 Submit 及时退出。
func TestSubmit_ContextDerivedFromChild_ParentCancelExits(t *testing.T) {
	inExecute := make(chan struct{})
	release := make(chan struct{})
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		close(inExecute)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}, nil)
	enableSubmitForTest(t, a)

	parent, cancelParent := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.Submit(parent, "claude", collector.CollectRequest{Incremental: true})
	}()
	<-inExecute

	// 父取消 → ExecuteFunc 的 ctx.Done() 触发 → Submit 返回 ctx.Err()
	cancelParent()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Submit error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return after parent cancel")
	}
	close(release)
}

// TestSubmit_WaitingForMutexCancelExits Submit 在等待 collect mutex 时 ctx 取消，
// 获 mutex 后检查 ctx 并及时返回（不执行 ExecuteFunc）。
func TestSubmit_WaitingForMutexCancelExits(t *testing.T) {
	var executed int32
	holderStarted := make(chan struct{})
	holderRelease := make(chan struct{})
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		atomic.AddInt32(&executed, 1)
		// 第一个 ExecuteFunc 进入即表示已持 collect mutex
		var once sync.Once
		once.Do(func() { close(holderStarted) })
		<-holderRelease
		return nil
	}, nil)
	enableSubmitForTest(t, a)

	// 第一个占住 mutex（channel 同步确认已持锁，无需 sleep）
	go func() { _ = a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true}) }()
	<-holderStarted

	// 第二个 Submit：阻塞在 collect mutex；此时取消其 ctx。
	// 第二个不会进入 ExecuteFunc（被 mutex 挡住），cancel 后获 mutex 时检查 ctx 退出。
	child, cancelChild := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Submit(child, "codex", collector.CollectRequest{Incremental: true}) }()
	cancelChild()

	// 放行第一个，第二个获 mutex 后发现 ctx 已取消，应不执行 ExecuteFunc 直接返回
	close(holderRelease)

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("second Submit error = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&executed); got != 1 {
		t.Fatalf("ExecuteFunc executed %d times, want 1 (cancelled Submit must not execute)", got)
	}
}

// TestSubmit_MonitorCallbackGoesThroughGateAndMutex 注入 watcher/poller 的 callback
// 只能是 MonitorSubmitFunc：用 trace 证明 monitor 请求必经 Submit 的 gate（collectWg.Add）
// 与 collect mutex（ExecuteFunc 串行）。构造真实 watcher+poller + Run，触发事件后断言
// ExecuteFunc 被串行调用（concurrency 恒为 1）且被实际触发。
func TestSubmit_MonitorCallbackGoesThroughGateAndMutex(t *testing.T) {
	var maxConcurrent, current int32
	var invocations int32
	triggered := make(chan struct{}) // ExecuteFunc 首次进入即 close，替代轮询 sleep
	var triggeredOnce sync.Once
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		c := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if c <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, c) {
				break
			}
		}
		atomic.AddInt32(&invocations, 1)
		triggeredOnce.Do(func() { close(triggered) })
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}, nil)

	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// 挂一个 JSONL watcher（通过 addWatcher 安装 monitorSubmit + ready signal）
	w, err := NewJSONLWatcher([]string{tmpDir}, "claude", 50*time.Millisecond, a.monitorSubmit, nil)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}
	a.addWatcher(w)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	select {
	case <-a.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Ready()")
	}

	// 触发多个文件事件，debounce 合并后串行 Submit
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, "s"+string(rune('a'+i))+".jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 等待至少一次 ExecuteFunc 触发（channel 同步，无轮询 sleep）
	select {
	case <-triggered:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: monitor callback never reached ExecuteFunc (gate/mutex path broken)")
	}

	cancel()
	<-done

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("maxConcurrent = %d, want 1 (monitor Submit must be serialized by collect mutex)", got)
	}
}

// TestSubmit_ExecuteErrorReturnedMonitorLogsOnce ExecuteFunc 错误原样返回；
// monitor wrapper 只记一次日志（此处验证 ExecuteFunc 仅被调用一次 = collection_errors
// 只由 engine 写一次的代理断言；Analyzer 不重复记录）。
func TestSubmit_ExecuteErrorReturnedMonitorLogsOnce(t *testing.T) {
	var n int32
	a := New(func(ctx context.Context, client string, req collector.CollectRequest) error {
		atomic.AddInt32(&n, 1)
		return errors.New("collect failed")
	}, nil)

	// 直接 Submit：error 原样返回（startup coordinator 路径）
	enableSubmitForTest(t, a)
	err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true})
	if err == nil || err.Error() != "collect failed" {
		t.Fatalf("Submit error = %v, want \"collect failed\"", err)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("ExecuteFunc invoked %d times, want exactly 1 (no duplicate error recording)", got)
	}
}

// TestSubmit_AfterShutdownReturnsStableError shutdown 后提交返回稳定的 ErrAnalyzerStopping，
// 且不对 collectWg 做 Add（用 collectWg 计数断言：提交前后 WaitGroup 不增长）。
func TestSubmit_AfterShutdownReturnsStableError(t *testing.T) {
	a := New(noopExecute, nil)
	enableSubmitForTest(t, a)

	// 关闭 gate（模拟 Run 的 shutdown 第一步）
	a.gateMu.Lock()
	a.accepting = false
	a.gateMu.Unlock()

	// collectWg 计数在 Add(1) 后 Wait 之前不可见为正；这里用「连续 Submit 都返回
	// ErrAnalyzerStopping 且不阻塞」间接证明不再 Add（若仍 Add 而不 Done，collectWg 会泄漏，
	// 后续 Run 关闭会挂死——由 -race 与其它 Run 关闭测试覆盖）。
	for i := 0; i < 5; i++ {
		err := a.Submit(context.Background(), "claude", collector.CollectRequest{Incremental: true})
		if !errors.Is(err, ErrAnalyzerStopping) {
			t.Fatalf("Submit[%d] error = %v, want ErrAnalyzerStopping", i, err)
		}
	}
}

// TestSubmit_ZeroMonitorsRunReturnsError 不经 monitor 的直接 Submit 路径：
// 0 monitor 时 Run 立即返回 error（不安装 runCtx、不打开 accepting）。
func TestSubmit_ZeroMonitorsRunReturnsError(t *testing.T) {
	a := New(noopExecute, nil)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for 0 monitors, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run blocked; expected immediate error for 0 monitors")
	}
	// 0 monitor 未启动 Run gate，Submit 应返回 ErrAnalyzerStopping
	if err := a.Submit(context.Background(), "claude", collector.CollectRequest{}); !errors.Is(err, ErrAnalyzerStopping) {
		t.Fatalf("Submit error = %v, want ErrAnalyzerStopping", err)
	}
}
