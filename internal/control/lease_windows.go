// internal/control/lease_windows.go
//go:build windows

// Windows 平台的父子 lease 实现。
//
// 机制：Windows 不支持 cmd.ExtraFiles（见 os/exec 文档）。改用标记为 inheritable 的
// 匿名 pipe read handle + SysProcAttr.AdditionalInheritedHandles 传递，并把实际 handle
// 数值写入 TOKEN_USAGE_LEASE_HANDLE 环境变量。
// 关键约束：
//   - read handle 必须 inheritable（child 才能继承）；
//   - write handle 及无关 handle 必须保持 non-inheritable（避免意外泄漏给 child）；
//   - Go 1.26.4 的 syscall.SysProcAttr.AdditionalInheritedHandles 可用，CreateProcess
//     通过 PROC_THREAD_ATTRIBUTE_HANDLE_LIST 限定只继承列表中的 handle（见 stdlib
//     exec_windows.go），满足「只继承列出的 handle」要求。
package control

import (
	"fmt"
	"strconv"
	"syscall"
	"unsafe"
)

// leaseReaderFromEnv 从 env 解析 Windows 父 lease：读取 TOKEN_USAGE_LEASE_HANDLE，
// 把 handle 数值包装成 leaseReader。返回 (reader, ok)；handle 缺失/非法时 ok=false。
func leaseReaderFromEnv(env []string) (leaseReader, bool) {
	hStr := lookupEnvValue(env, envLeaseHandle)
	if hStr == "" {
		return nil, false
	}
	h, err := strconv.ParseUint(hStr, 10, 64)
	if err != nil || h == 0 {
		return nil, false
	}
	return &handleLeaseReader{handle: syscall.Handle(h)}, true
}

// handleLeaseReader 包装 Windows handle 实现 leaseReader。
type handleLeaseReader struct {
	handle syscall.Handle
	closed bool
}

var readLeaseFile = syscall.ReadFile

// WaitForEOF 用 ReadFile 阻塞读直到 EOF（父关闭 write handle）或错误。
func (r *handleLeaseReader) WaitForEOF() {
	if r.handle == 0 {
		return
	}
	var buf [1]byte
	// ReadFile 阻塞读；EOF/错误时返回（lease 消失）。
	for {
		var n uint32
		// CreatePipe 创建的是同步匿名管道，OVERLAPPED 必须传 nil；非 nil 指针会让
		// ReadFile 失败并被误解释为父 lease 已消失。
		err := readLeaseFile(r.handle, buf[:], &n, nil)
		if err != nil || n == 0 {
			return
		}
	}
}

// Close 关闭 handle（child 不再需要 read end）。幂等。
func (r *handleLeaseReader) Close() {
	if r.handle != 0 && !r.closed {
		_ = syscall.CloseHandle(r.handle)
		r.closed = true
	}
}

// leasePipeHolderWindows 持有父进程侧的匿名 pipe 两端的 Windows handle。
type leasePipeHolderWindows struct {
	readHandle  syscall.Handle // inheritable，传给 child
	writeHandle syscall.Handle // non-inheritable，父持有
}

// newLeasePipe 创建 Windows lease pipe（实现 leaseHandle 工厂，由 newLeaseContext 调用）。
func newLeasePipe() (leaseHandle, error) {
	return newLeasePipeHolderWindows()
}

// reader 返回 read end 作为 syscall.Handle（daemon.SpawnDetached 放入 AdditionalInheritedHandles）。
func (h *leasePipeHolderWindows) reader() interface{} {
	return h.readHandle
}

// newLeasePipeHolderWindows 创建匿名 pipe，read handle 标记为 inheritable，
// write handle 保持 non-inheritable（Windows 默认 CreatePipe 两端都 inheritable，
// 需显式把 write 端设为 non-inheritable 避免意外继承）。
func newLeasePipeHolderWindows() (*leasePipeHolderWindows, error) {
	var rH, wH syscall.Handle
	// CreatePipe 默认两端都标记 SECURITY_ATTRIBUTES.bInheritHandle=TRUE。
	// 我们让 read 端 inheritable，write 端随后通过 SetHandleInformation 改为 non-inheritable。
	sa := syscall.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		InheritHandle: 1, // read 端可继承
	}
	if err := syscall.CreatePipe(&rH, &wH, &sa, 0); err != nil {
		return nil, fmt.Errorf("创建 lease pipe 失败: %w", err)
	}
	// write 端强制 non-inheritable：避免 child 意外继承 write handle（会破坏 lease 单向语义）。
	if err := setHandleNonInheritable(wH); err != nil {
		_ = syscall.CloseHandle(rH)
		_ = syscall.CloseHandle(wH)
		return nil, fmt.Errorf("设置 write handle non-inheritable 失败: %w", err)
	}
	return &leasePipeHolderWindows{readHandle: rH, writeHandle: wH}, nil
}

// setHandleNonInheritable 把 handle 标记为不可继承（清除 HANDLE_FLAG_INHERIT）。
func setHandleNonInheritable(h syscall.Handle) error {
	const HANDLE_FLAG_INHERIT = 0x00000001
	const HANDLE_FLAG_DONT_INHERIT = 0
	// SetHandleInformation(handle, mask, flags)：mask=INHERIT 表示只改继承位，
	// flags=0 表示清除继承位。
	return setHandleInformation(h, HANDLE_FLAG_INHERIT, HANDLE_FLAG_DONT_INHERIT)
}

// setHandleInformation 调用 kernel32 SetHandleInformation（syscall 未导出，手写）。
func setHandleInformation(h syscall.Handle, mask, flags uint32) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("SetHandleInformation")
	r1, _, err := proc.Call(uintptr(h), uintptr(mask), uintptr(flags))
	if r1 == 0 {
		return err
	}
	return nil
}

// appendEnv 把 lease handle 数值写入 env（只追加平台专属变量，instanceID 由 BuildChildEnv 统一负责）。
func (h *leasePipeHolderWindows) appendEnv(env []string) []string {
	return append(env, envLeaseHandle+"="+strconv.FormatUint(uint64(h.readHandle), 10))
}

// closeWrite 关闭父侧 write handle（触发 child read handle EOF）。
func (h *leasePipeHolderWindows) closeWrite() {
	if h.writeHandle != 0 {
		_ = syscall.CloseHandle(h.writeHandle)
		h.writeHandle = 0
	}
}

// closeRead 关闭父侧 read handle 副本（父不读，避免 handle 泄漏；child 已继承独立副本）。
func (h *leasePipeHolderWindows) closeRead() {
	if h.readHandle != 0 {
		_ = syscall.CloseHandle(h.readHandle)
		h.readHandle = 0
	}
}

// cleanup 父进程清理：关闭两端。
func (h *leasePipeHolderWindows) cleanup() {
	h.closeRead()
	h.closeWrite()
}

// leaseEnvSummary 用于测试断言。
func leaseEnvSummary(env []string) (instance, handleStr string) {
	return lookupEnvValue(env, envInstance), lookupEnvValue(env, envLeaseHandle)
}
