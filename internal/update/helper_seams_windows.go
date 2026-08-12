//go:build windows

package update

import (
	"context"
	"fmt"
	"io/fs"
	"time"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"golang.org/x/sys/windows"
)

// helper_seams_windows.go 实现后台 helper 所需 seam 的生产版本（仅 Windows 编译）。
//
// 这些实现把 Windows 系统 API 封装成 helperRunner 依赖的 seam 接口：
//   - windowsProcessProbe / windowsProcessWaitHandle：OpenProcess + GetProcessTimes +
//     WaitForSingleObject，按显式进程身份（PID + 创建时间）等待，杜绝 PID 复用；
//   - windowsParentWaiter：复用平台无关 WaitProcessIdentity，按 plan.Parent 身份等待父进程；
//   - windowsFileMover：委托 fileutil.RenameReplace（MoveFileEx + MOVEFILE_REPLACE_EXISTING）；
//   - osResultWriter：经 fileutil 原子写 result 文件（0600）。
//
// macOS 上这些代码不编译（build tag windows），决策逻辑经 WaitProcessIdentity + fake
// probe 在 macOS 单元测试覆盖（见 process_identity_test.go）。

// defaultParentWaitTimeout 是 helper 等待父进程退出的默认有限超时。
// 超时后 helper 放弃替换并写失败 result（避免无限期驻留）。
const defaultParentWaitTimeout = 5 * time.Minute

// openForWaitAccess 是 OpenForWait 打开进程的权限：SYNCHRONIZE（等待退出）+
// PROCESS_QUERY_LIMITED_INFORMATION（查询创建时间）。后者权限收窄，避免要求管理员。
const openForWaitAccess = windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION

// windowsProcessWaitHandle 封装 OpenForWait 返回的进程句柄，实现 ProcessWaitHandle。
// 句柄绑定 OpenProcess 那一刻的进程实例，即便之后该 PID 被回收复用，原句柄仍指向
// 已退出的原进程，signaled 状态不变——这是身份匹配后安全等待的底层保证。
type windowsProcessWaitHandle struct {
	handle windows.Handle
}

// CreationTime 返回进程创建时刻（FILETIME 原始 uint64）。
func (h *windowsProcessWaitHandle) CreationTime() (uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h.handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("GetProcessTimes 失败: %w", err)
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

// Wait 阻塞至句柄 signaled（进程退出），轮询步长不超过 1s 以响应 ctx 取消/超时。
func (h *windowsProcessWaitHandle) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 每轮最多等 1s，便于及时响应 ctx。
		status, err := windows.WaitForSingleObject(h.handle, 1000)
		if err != nil {
			return fmt.Errorf("WaitForSingleObject 失败: %w", err)
		}
		if status == windows.WAIT_OBJECT_0 {
			return nil // 进程已退出。
		}
		// status == WAIT_TIMEOUT → 继续轮询。
	}
}

// Close 释放句柄。
func (h *windowsProcessWaitHandle) Close() {
	_ = windows.CloseHandle(h.handle)
}

// windowsProcessProbe 实现 ProcessProbe：按 PID 打开可等待且可查询创建时间的句柄。
type windowsProcessProbe struct{}

// OpenForWait 以 SYNCHRONIZE + PROCESS_QUERY_LIMITED_INFORMATION 打开 pid。
// 进程不存在（Windows 返回 ERROR_INVALID_PARAMETER）→ errProcessGone；
// 其它错误（含 access denied）→ 非 gone 错误，由 WaitProcessIdentity 判定为失败。
func (windowsProcessProbe) OpenForWait(pid uint32) (ProcessWaitHandle, error) {
	h, err := windows.OpenProcess(openForWaitAccess, false, pid)
	if err != nil {
		// Windows 对「进程不存在」类 OpenProcess 调用统一返回 ERROR_INVALID_PARAMETER(87)。
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil, errProcessGone
		}
		return nil, fmt.Errorf("OpenProcess(%d) 失败: %w", pid, err)
	}
	return &windowsProcessWaitHandle{handle: h}, nil
}

// windowsParentWaiter 复用平台无关 WaitProcessIdentity，按 plan.Parent 身份等待父进程退出。
type windowsParentWaiter struct {
	probe   ProcessProbe
	timeout time.Duration // 有限超时；<=0 表示用默认值
}

// newWindowsParentWaiter 构造默认超时 + 生产 probe 的父进程等待器。
func newWindowsParentWaiter() windowsParentWaiter {
	return windowsParentWaiter{probe: windowsProcessProbe{}, timeout: defaultParentWaitTimeout}
}

// WaitParentExit 按 identity 等待父进程退出，整体受 timeout 约束。
// 身份匹配/不匹配/gone 等决策由 WaitProcessIdentity 统一处理（见 process_identity.go）。
func (w windowsParentWaiter) WaitParentExit(ctx context.Context, identity ProcessIdentity) error {
	timeout := w.timeout
	if timeout <= 0 {
		timeout = defaultParentWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return WaitProcessIdentity(waitCtx, w.probe, identity)
}

// windowsFileMover 委托 fileutil.RenameReplace（MoveFileEx + MOVEFILE_REPLACE_EXISTING）
// 完成 stage → target 的原子替换。禁止 .bat/cmd/PowerShell 或 delete-then-rename。
type windowsFileMover struct{}

// MoveReplace 把 from 原子移动到 to，覆盖已存在的 to。
func (windowsFileMover) MoveReplace(from, to string) error {
	return fileutil.RenameReplace(from, to)
}

// osResultWriter 经 fileutil 原子写 result 文件（mode 仅在新建时生效）。
type osResultWriter struct{}

// WriteResult 把 data 以 mode 权限原子写入 path。
func (osResultWriter) WriteResult(path string, data []byte, mode fs.FileMode) error {
	return fileutil.ReplaceCompleteFile(path, data, mode)
}

// NewWindowsHelperSeams 装配 helper 的生产 Windows seam 集合，供 CLI helper 命令构造 helperRunner。
func NewWindowsHelperSeams() (ParentWaiter, FileMover, ResultWriter) {
	return newWindowsParentWaiter(), windowsFileMover{}, osResultWriter{}
}

// NewWindowsProcessProbe 返回生产 ProcessProbe，供 cleanup 命令按显式身份等待 helper 退出。
// cleanup 复用与父进程等待同一套（probe + WaitProcessIdentity）决策逻辑，语义对称。
func NewWindowsProcessProbe() ProcessProbe {
	return windowsProcessProbe{}
}
