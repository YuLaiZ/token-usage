// internal/analyzer/debounce.go
package analyzer

import (
	"sync"
	"time"
)

// Debounce 合并同一 key 的多次触发，只执行最后一次。
// 除静默触发（duration 内无新事件）外，还设最大延迟 maxWait：
// 即使持续触发（活跃 JSONL 不断追加、idleTimer 被反复重置永不静默），
// 距首次触发超过 maxWait 也会强制触发一次，避免长活跃会话长期不被采集。
// maxWait = 12 × duration（生产 duration=5s → maxWait≈60s），随 duration 缩放便于测试。
//
// in-flight 语义：fire 由 time.AfterFunc 在独立 goroutine 触发，不在调用方的 wg 内。
// 故 Debounce 自带 wg 跟踪正在执行的回调：fire 持锁 Add(1)、回调结束 Done；
// Stop 清空 pending 并停掉所有 timer 后 Wait，确保优雅关闭等齐 in-flight 采集，
// 避免关闭后仍有后台回调继续写库。
// stopTimeout 是 Debounce.Stop 等待 in-flight 回调的超时上限。
// 防止单个阻塞的回调使 Stop 永不返回，进而让 Analyzer.Run 的 10s 关闭兜底
// （位于 a.Stop 之后）失效、进程挂死。默认 8s（< Analyzer.Run 的 10s 兜底，留余量
// 给其他关闭步骤）；测试可临时调小加速。
var stopTimeout = 8 * time.Second

type Debounce struct {
	mu       sync.Mutex
	wg       sync.WaitGroup // 跟踪 in-flight 回调，供 Stop 等待
	stopped  bool           // Stop 后拒绝新 Trigger，杜绝关闭竞态中的 late fire
	duration time.Duration
	maxWait  time.Duration
	callback func(string)
	pending  map[string]*pendingKey
}

// pendingKey 单个 key 的待触发状态：idleTimer 每次触发重置，maxTimer 仅首次启动
type pendingKey struct {
	idleTimer *time.Timer
	maxTimer  *time.Timer
}

// NewDebounce 创建 debounce 实例
// duration: 静默防抖间隔（生产 5 秒）
// callback: 防抖后执行的回调函数
func NewDebounce(duration time.Duration, callback func(string)) *Debounce {
	if duration <= 0 {
		duration = 5 * time.Second
	}
	maxWait := duration
	if duration <= time.Duration(1<<63-1)/12 {
		maxWait = 12 * duration
	}
	return &Debounce{
		duration: duration,
		maxWait:  maxWait,
		callback: callback,
		pending:  make(map[string]*pendingKey),
	}
}

// Trigger 触发防抖
// 已有 pending：只重置 idleTimer（不重置 maxTimer，保证 maxWait 兜底不被无限推迟）
// 新 key：同时启动 idleTimer（静默触发）与 maxTimer（强制触发）
func (d *Debounce) Trigger(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}

	if p, ok := d.pending[key]; ok {
		p.idleTimer.Reset(d.duration)
		return
	}

	// 先登记 pending，再启动 timer。即使 duration 极短，timer 回调也会先等待
	// 当前持有的 d.mu；解锁后它一定能看到已登记的 key，不会留下永不触发的残项。
	p := &pendingKey{}
	d.pending[key] = p
	p.idleTimer = time.AfterFunc(d.duration, d.fire(key))
	p.maxTimer = time.AfterFunc(d.maxWait, d.fire(key))
}

// fire 返回该 key 的触发闭包：靠 pending 存在性去重，保证 idleTimer 与 maxTimer 只有一个真正执行回调。
// 持锁段内 wg.Add(1)：与 Stop 的「持锁清空 pending」互斥，
// 保证 Stop 持锁期间任何新 fire 要么拿不到 key（pending 已清）直接返回，
// 要么已 Add 进 wg 被 Stop 后续的 Wait 等到，不会出现「Stop 已 Wait 返回、fire 才 Add」的漏等。
func (d *Debounce) fire(key string) func() {
	return func() {
		d.mu.Lock()
		p, ok := d.pending[key]
		if !ok {
			d.mu.Unlock()
			return
		}
		delete(d.pending, key)
		p.idleTimer.Stop()
		p.maxTimer.Stop()
		d.wg.Add(1)
		d.mu.Unlock()

		defer d.wg.Done()
		d.callback(key)
	}
}

// Stop 停止所有 pending 的 timer，并等待正在执行的回调（in-flight 采集）完成。
// 注意：不能用 defer Unlock——必须先 Unlock 再 Wait，否则 Wait 等 fire、fire 等 mu 会死锁。
// Wait 带 stopTimeout 超时上限：避免单个阻塞回调（如 collector 不响应 ctx）使 Stop 永不返回，
// 进而让 Analyzer.Run 的 10s 关闭兜底（位于 a.Stop 之后）失效、进程挂死。
// 正常路径回调远快于超时，不会被切断。
func (d *Debounce) Stop() {
	d.mu.Lock()
	d.stopped = true
	for key, p := range d.pending {
		p.idleTimer.Stop()
		p.maxTimer.Stop()
		delete(d.pending, key)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
	}
}
