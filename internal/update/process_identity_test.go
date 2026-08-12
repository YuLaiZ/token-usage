package update

import (
	"context"
	"errors"
	"testing"
	"time"
)

// process_identity_test.go 校验「按显式进程身份（PID + 创建时间）等待进程退出」的
// 平台无关决策函数 WaitProcessIdentity。该函数是 Windows staged replacement
// 等待语义的安全核心：helper 等待父进程退出、cleanup 等待 helper 退出都复用它。
//
// 覆盖的五条规则（身份不匹配绝不等待；无法确认身份绝不当作已退出）：
//  (a) 进程不存在（probe 返回 errProcessGone）→ nil，不等待；
//  (b) 进程存在但创建时间不匹配（PID 已被复用）→ nil，关闭句柄，不等待；
//  (c) 进程存在且创建时间匹配 → 等待至 signaled，Wait 返回 nil → 函数 nil；
//  (d) OpenProcess 返回非 gone 错误（access denied 语义）→ 非 nil 错误；
//  (e) 句柄 CreationTime 返回错误 → 非 nil 错误；
//  (f) 句柄 Wait 返回错误 / ctx 取消 / 超时 → 非 nil 错误。
//
// 所有用例都用 fake probe + fake handle，不触碰真实进程，macOS 即可运行。

// fakeProcessWaitHandle 记录 Close/Wait 调用，按预置值响应。
// Wait 同时响应 ctx.Done() 与 signal channel：测试用例按需 close(signal) 让 Wait 立即
// 返回 waitErr；不 close 则 Wait 阻塞至 ctx 取消/超时（返回 ctx.Err()），用于覆盖超时/取消。
type fakeProcessWaitHandle struct {
	creationTime uint64
	creationErr  error
	waitErr      error
	signal       chan struct{} // close 后让 Wait 立即返回 waitErr
	closeCalls   int
	waitCalls    int
}

func newFakeProcessWaitHandle(ct uint64) *fakeProcessWaitHandle {
	return &fakeProcessWaitHandle{creationTime: ct, signal: make(chan struct{})}
}

func (h *fakeProcessWaitHandle) CreationTime() (uint64, error) {
	return h.creationTime, h.creationErr
}

func (h *fakeProcessWaitHandle) Wait(ctx context.Context) error {
	h.waitCalls++
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.signal:
		return h.waitErr
	}
}

func (h *fakeProcessWaitHandle) Close() {
	h.closeCalls++
}

// fakeProcessProbe 按预置 openErr / handle 响应 OpenForWait，记录调用 PID。
type fakeProcessProbe struct {
	handle    ProcessWaitHandle
	openErr   error
	openCalls int
	lastPID   uint32
}

func (p *fakeProcessProbe) OpenForWait(pid uint32) (ProcessWaitHandle, error) {
	p.openCalls++
	p.lastPID = pid
	if p.openErr != nil {
		return nil, p.openErr
	}
	return p.handle, nil
}

// TestProcessIdentity_Valid 合法身份要求 PID>0 且 CreationTime>0。
func TestProcessIdentity_Valid(t *testing.T) {
	cases := []struct {
		name string
		id   ProcessIdentity
		want bool
	}{
		{"both nonzero", ProcessIdentity{PID: 1, CreationTime: 1}, true},
		{"zero pid", ProcessIdentity{PID: 0, CreationTime: 1}, false},
		{"zero creation", ProcessIdentity{PID: 1, CreationTime: 0}, false},
		{"both zero", ProcessIdentity{}, false},
	}
	for _, c := range cases {
		if got := c.id.Valid(); got != c.want {
			t.Errorf("%s: Valid()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestIsProcessGone 识别 errProcessGone 哨兵（含包装）。
func TestIsProcessGone(t *testing.T) {
	if !IsProcessGone(errProcessGone) {
		t.Error("errProcessGone 应被识别为 gone")
	}
	// errors.Join 组合的错误支持 errors.Is 遍历，验证 IsProcessGone 能穿透复合错误。
	compound := errors.Join(errProcessGone, errors.New("附加上下文"))
	if !IsProcessGone(compound) {
		t.Error("复合错误中的 errProcessGone 应被识别为 gone")
	}
	other := errors.New("access denied")
	if IsProcessGone(other) {
		t.Error("普通错误不应被识别为 gone")
	}
	if IsProcessGone(nil) {
		t.Error("nil 不应被识别为 gone")
	}
}

// TestWaitProcessIdentity_ProcessGone 规则(a)：进程不存在 → nil，不打开句柄、不等待。
func TestWaitProcessIdentity_ProcessGone(t *testing.T) {
	probe := &fakeProcessProbe{openErr: errProcessGone}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err != nil {
		t.Fatalf("进程不存在应返回 nil，got %v", err)
	}
	if probe.openCalls != 1 {
		t.Errorf("应调用 OpenForWait 一次，got %d", probe.openCalls)
	}
	if probe.lastPID != 1234 {
		t.Errorf("OpenForWait 应收到 PID=1234，got %d", probe.lastPID)
	}
}

// TestWaitProcessIdentity_CreationTimeMismatch 规则(b)：创建时间不匹配 → nil，
// 关闭句柄，不等待。错误实现「匹配则等待」会让 Wait 被调用，本用例断言 Wait 不被调用 → 失败。
func TestWaitProcessIdentity_CreationTimeMismatch(t *testing.T) {
	handle := newFakeProcessWaitHandle(111)
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999} // 不匹配 111
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err != nil {
		t.Fatalf("创建时间不匹配应返回 nil（PID 复用，原进程已退出），got %v", err)
	}
	if handle.waitCalls != 0 {
		t.Errorf("创建时间不匹配绝不等待，Wait 被调用了 %d 次", handle.waitCalls)
	}
	if handle.closeCalls != 1 {
		t.Errorf("不匹配时应关闭句柄一次，closeCalls=%d", handle.closeCalls)
	}
}

// TestWaitProcessIdentity_CreationTimeMatch 规则(c)：创建时间匹配 → 等待至 signaled。
func TestWaitProcessIdentity_CreationTimeMatch(t *testing.T) {
	handle := newFakeProcessWaitHandle(999)
	close(handle.signal) // 立即 signaled，Wait 返回 waitErr(nil)
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err != nil {
		t.Fatalf("匹配且 Wait 成功应返回 nil，got %v", err)
	}
	if handle.waitCalls != 1 {
		t.Errorf("匹配应等待一次，waitCalls=%d", handle.waitCalls)
	}
	if handle.closeCalls != 1 {
		t.Errorf("等待后应关闭句柄，closeCalls=%d", handle.closeCalls)
	}
}

// TestWaitProcessIdentity_NonGoneOpenError 规则(d)：OpenProcess 非 gone 错误 → 非 nil。
// 错误实现「access denied 当作已退出 continue」会让本用例返回 nil → 断言失败。
func TestWaitProcessIdentity_NonGoneOpenError(t *testing.T) {
	probe := &fakeProcessProbe{openErr: errors.New("access denied")}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err == nil {
		t.Fatal("非 gone 的 OpenProcess 错误应返回非 nil（不得当作已退出）")
	}
}

// TestWaitProcessIdentity_CreationTimeError 规则(e)：句柄 CreationTime 错误 → 非 nil。
func TestWaitProcessIdentity_CreationTimeError(t *testing.T) {
	handle := newFakeProcessWaitHandle(999)
	handle.creationErr = errors.New("GetProcessTimes failed")
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err == nil {
		t.Fatal("CreationTime 错误应返回非 nil")
	}
	if handle.waitCalls != 0 {
		t.Errorf("CreationTime 错误时不应等待，waitCalls=%d", handle.waitCalls)
	}
	if handle.closeCalls != 1 {
		t.Errorf("应关闭句柄，closeCalls=%d", handle.closeCalls)
	}
}

// TestWaitProcessIdentity_WaitError 规则(f)：句柄 Wait 返回错误 → 非 nil。
func TestWaitProcessIdentity_WaitError(t *testing.T) {
	handle := newFakeProcessWaitHandle(999)
	handle.waitErr = errors.New("wait failed")
	close(handle.signal) // 立即返回 waitErr
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	err := WaitProcessIdentity(context.Background(), probe, id)
	if err == nil {
		t.Fatal("Wait 错误应返回非 nil")
	}
}

// TestWaitProcessIdentity_ContextCancelled 规则(f) ctx：匹配但 ctx 已取消 → 非 nil。
func TestWaitProcessIdentity_ContextCancelled(t *testing.T) {
	handle := newFakeProcessWaitHandle(999)
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 提前取消
	err := WaitProcessIdentity(ctx, probe, id)
	if err == nil {
		t.Fatal("ctx 取消应返回非 nil")
	}
}

// TestWaitProcessIdentity_WaitTimeout 规则(f) 超时：Wait 因 ctx 超时返回 → 非 nil。
func TestWaitProcessIdentity_WaitTimeout(t *testing.T) {
	handle := newFakeProcessWaitHandle(999)
	probe := &fakeProcessProbe{handle: handle}
	id := ProcessIdentity{PID: 1234, CreationTime: 999}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := WaitProcessIdentity(ctx, probe, id)
	if err == nil {
		t.Fatal("超时应返回非 nil")
	}
}
