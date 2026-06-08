// internal/analyzer/debounce_test.go
package analyzer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebounce_CallsCallbackOnce(t *testing.T) {
	var count int64
	d := NewDebounce(50*time.Millisecond, func(key string) {
		atomic.AddInt64(&count, 1)
	})
	defer d.Stop()

	// 快速触发多次同一 key
	d.Trigger("file1.jsonl")
	d.Trigger("file1.jsonl")
	d.Trigger("file1.jsonl")

	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt64(&count) != 1 {
		t.Errorf("expected 1 callback, got %d", count)
	}
}

func TestDebounce_DifferentKeys(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	d := NewDebounce(50*time.Millisecond, func(key string) {
		mu.Lock()
		calls[key]++
		mu.Unlock()
	})
	defer d.Stop()

	d.Trigger("file1.jsonl")
	d.Trigger("file2.jsonl")

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls["file1.jsonl"] != 1 {
		t.Errorf("expected file1 called once, got %d", calls["file1.jsonl"])
	}
	if calls["file2.jsonl"] != 1 {
		t.Errorf("expected file2 called once, got %d", calls["file2.jsonl"])
	}
}

func TestDebounce_ResetTimer(t *testing.T) {
	var count int64
	d := NewDebounce(50*time.Millisecond, func(key string) {
		atomic.AddInt64(&count, 1)
	})
	defer d.Stop()

	d.Trigger("file1.jsonl")
	time.Sleep(30 * time.Millisecond) // 不到 50ms
	d.Trigger("file1.jsonl")          // 重置 timer
	time.Sleep(80 * time.Millisecond) // 从最后一次触发算起 50ms

	if atomic.LoadInt64(&count) != 1 {
		t.Errorf("expected 1 callback after reset, got %d", count)
	}
}

func TestDebounce_Stop(t *testing.T) {
	var count int64
	d := NewDebounce(50*time.Millisecond, func(key string) {
		atomic.AddInt64(&count, 1)
	})

	d.Trigger("file1.jsonl")
	d.Stop()

	time.Sleep(100 * time.Millisecond)

	// Stop 后不应再触发回调
	if atomic.LoadInt64(&count) != 0 {
		t.Errorf("expected 0 callbacks after stop, got %d", count)
	}
}

func TestDebounce_ConcurrentSafety(t *testing.T) {
	var count int64
	d := NewDebounce(50*time.Millisecond, func(key string) {
		atomic.AddInt64(&count, 1)
	})
	defer d.Stop()

	// 并发触发多个 goroutine
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				d.Trigger(fmt.Sprintf("file-%d.jsonl", idx))
			}
		}(i)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	// 每个 key 应该只触发一次回调
	if atomic.LoadInt64(&count) != 10 {
		t.Errorf("expected 10 callbacks (one per key), got %d", count)
	}
}

// TestDebounce_StopWaitsForInFlight 守护「Stop 等待正在执行的回调完成」语义：
// 既定契约 与 debounce.go 要求 Stop 优雅关闭——清空 pending 后必须等齐 in-flight 采集，
// 不能让后台回调在关闭后继续写库。实现靠 wg.Wait()，此处用阻塞式回调验证。
//
// 流程：
//  1. 回调启动后通过 callbackStarted 通知主 goroutine，再在 release 上阻塞，模拟 in-flight 采集
//  2. 主 goroutine 确认回调 in-flight 后起 goroutine 跑 Stop，并用超时断言此时 Stop 尚未返回（在等回调）
//  3. close(release) 让回调完成，断言 Stop 最终返回
//
// 注意：回调会持续阻塞到 release，故本测试不能用 defer d.Stop()（会卡住 t.Cleanup），
// 只能在受控位置手动 Stop，并保证 release 在 Stop 之前关闭以避免泄漏。
func TestDebounce_StopWaitsForInFlight(t *testing.T) {
	callbackStarted := make(chan struct{})
	release := make(chan struct{})

	d := NewDebounce(20*time.Millisecond, func(key string) {
		// 通知主 goroutine：回调已 in-flight（此时已进入 wg）
		close(callbackStarted)
		// 阻塞，模拟一次耗时的 in-flight 采集
		<-release
	})

	// 触发防抖，等待 idleTimer 到期、回调进入 in-flight
	d.Trigger("inflight.jsonl")
	select {
	case <-callbackStarted:
		// 回调已在独立 goroutine 运行
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not start in time")
	}

	// 起 goroutine 跑 Stop；此时回调仍阻塞在 release，Stop 应卡在 wg.Wait()
	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()

	// 断言 Stop 此时仍在阻塞（等 in-flight 回调），不立即返回
	select {
	case <-stopDone:
		t.Fatal("Stop returned before in-flight callback finished; wg.Wait not honored")
	case <-time.After(100 * time.Millisecond):
		// 预期：Stop 仍在阻塞，符合「等 in-flight」语义
	}

	// 放行回调完成，Stop 应随后返回
	close(release)
	select {
	case <-stopDone:
		// Stop 已优雅等待 in-flight 回调完成后返回
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after in-flight callback finished")
	}
}

// TestDebounce_StopTimesOutOnStuckCallback 守护 Stop 不永久阻塞：
// 即使 in-flight 回调永不返回（模拟 collector 卡死、不响应 ctx），Stop 也应在超时后返回，
// 使 Analyzer.Run 的 10s 关闭兜底（位于 a.Stop 之后）能推进，而非进程挂死。
func TestDebounce_StopTimesOutOnStuckCallback(t *testing.T) {
	origTimeout := stopTimeout
	stopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stopTimeout = origTimeout })

	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // 保证放行卡死回调，避免 goroutine 泄漏

	d := NewDebounce(20*time.Millisecond, func(key string) {
		<-block // 永不关闭，模拟卡死的 in-flight 回调
	})

	d.Trigger("stuck.jsonl")
	time.Sleep(80 * time.Millisecond) // 等 idleTimer 到期、回调进入 in-flight

	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop 因超时返回（预期）
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within timeout despite stuck callback (would hang without stopTimeout)")
	}
}

// TestDebounce_MaxWaitForcesFire 验证 maxWait 兜底：
// 持续触发（间隔 < duration，idleTimer 被反复重置永不静默）时，
// maxWait 到期应强制触发至少一次回调，避免活跃文件长期饥饿。
func TestDebounce_MaxWaitForcesFire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ~1.6s maxWait test in short mode")
	}

	var count int64
	// duration=100ms → maxWait=12*100=1200ms
	d := NewDebounce(100*time.Millisecond, func(key string) {
		atomic.AddInt64(&count, 1)
	})
	defer d.Stop()

	// 间隔 40ms 持续触发（< duration 100ms），idleTimer 永不静默
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				d.Trigger("active.jsonl")
			}
		}
	}()

	// 等待 maxWait（1200ms）+ 余量
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	time.Sleep(100 * time.Millisecond) // 等待回调落定

	if atomic.LoadInt64(&count) == 0 {
		t.Error("expected maxWait to force at least one callback during continuous triggering")
	}
}
