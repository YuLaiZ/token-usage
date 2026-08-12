//go:build windows

package update

import (
	"context"
	"testing"
	"time"
)

// process_identity_windows_test.go 是 Windows 生产 probe/waiter 的最小接线测试。
// 仅在 Windows 运行；macOS 仅保证 GOOS=windows GOARCH=amd64 go test -c 交叉编译通过。
// 真实 handle 的等待/ signaled 行为留待 Windows RC 实机验收，此处不伪造。

// TestCaptureCurrentIdentity_NonZero 当前进程身份必须为合法非零值
// （GetCurrentProcessId 与 GetProcessTimes 自洽：PID>0 且创建时间>0）。
func TestCaptureCurrentIdentity_NonZero(t *testing.T) {
	id, err := CaptureCurrentIdentity()
	if err != nil {
		t.Fatalf("CaptureCurrentIdentity: %v", err)
	}
	if !id.Valid() {
		t.Fatalf("当前进程身份应为合法非零值: %+v", id)
	}
	if id.PID == 0 {
		t.Error("PID 不应为 0")
	}
	if id.CreationTime == 0 {
		t.Error("CreationTime 不应为 0")
	}
}

// TestNewWindowsProcessProbe_NonNil 生产 probe 可构造（接线自洽）。
func TestNewWindowsProcessProbe_NonNil(t *testing.T) {
	if p := NewWindowsProcessProbe(); p == nil {
		t.Fatal("NewWindowsProcessProbe 不应返回 nil")
	}
}

// TestNewWindowsHelperSeams_AssemblesWaiter seam 装配返回非 nil ParentWaiter。
func TestNewWindowsHelperSeams_AssemblesWaiter(t *testing.T) {
	w, _, _ := NewWindowsHelperSeams()
	if w == nil {
		t.Fatal("NewWindowsHelperSeams 的 ParentWaiter 不应为 nil")
	}
}

// TestWindowsParentWaiter_GonePIDReturnsNil 对显然不存在的极大 PID 等待应判定为 gone
// （进程不存在 → nil，不阻塞）。这是规则2的接线级验证：OpenForWait 把
// ERROR_INVALID_PARAMETER 映射为 errProcessGone，WaitProcessIdentity 据此返回 nil。
func TestWindowsParentWaiter_GonePIDReturnsNil(t *testing.T) {
	w := newWindowsParentWaiter()
	// 0xCFFFFFFF 几乎不可能是存活进程的 PID，OpenProcess 预期返回 ERROR_INVALID_PARAMETER。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 用极短的 timeout waiter：身份不匹配/gone 时 probe 在 OpenProcess 阶段即返回，
	// 不会真正等待；若实现错误地等待，会因 ctx 超时失败。
	err := w.WaitParentExit(ctx, ProcessIdentity{PID: 0xCFFFFFFF, CreationTime: 1})
	if err != nil {
		t.Fatalf("对不存在的 PID 应返回 nil（进程已退出），got %v", err)
	}
}
