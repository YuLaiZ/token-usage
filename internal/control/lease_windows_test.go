// internal/control/lease_windows_test.go
//go:build windows

// Windows 平台的父子 lease 测试。
//
// 关键行为：
//   - read handle inheritable，write handle non-inheritable。
//   - 只继承列出的 handle（AdditionalInheritedHandles）；write/unrelated handle 不继承。
//   - TOKEN_USAGE_LEASE_HANDLE 写入实际 handle 数值。
//   - 父关闭 write handle → child read 端 EOF。
//
// 注意：这些测试只能在 Windows CI 执行；macOS/Linux 通过 GOOS=windows go test -c 交叉编译验证。
package control

import (
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// TestNewLeasePipeHolderWindows_CreatesValidHandles 创建 pipe 后两端 handle 非零。
func TestNewLeasePipeHolderWindows_CreatesValidHandles(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	defer h.cleanup()
	if h.readHandle == 0 || h.writeHandle == 0 {
		t.Fatal("read/write handle 不应为 0")
	}
}

// TestNewLeasePipeHolderWindows_WriteHandleNonInheritable write handle 必须不可继承。
// 验证：GetHandleInformation(writeHandle) 的 INHERIT 位为 0。
func TestNewLeasePipeHolderWindows_WriteHandleNonInheritable(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	defer h.cleanup()
	if isHandleInheritable(h.writeHandle) {
		t.Error("write handle 必须为 non-inheritable（避免 child 意外继承）")
	}
}

// TestNewLeasePipeHolderWindows_ReadHandleInheritable read handle 必须可继承。
func TestNewLeasePipeHolderWindows_ReadHandleInheritable(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	defer h.cleanup()
	if !isHandleInheritable(h.readHandle) {
		t.Error("read handle 必须为 inheritable（child 才能继承）")
	}
}

// TestNewLeasePipeHolderWindows_AppendEnv appendEnv 写入 handle 数值。
func TestNewLeasePipeHolderWindows_AppendEnv(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	defer h.cleanup()
	out := h.appendEnv([]string{"PATH=/usr/bin"})
	handleStr := lookupEnvValue(out, envLeaseHandle)
	want := strconv.FormatUint(uint64(h.readHandle), 10)
	if handleStr != want {
		t.Errorf("handle=%q want %q", handleStr, want)
	}
}

// TestLeaseReaderFromEnv_Windows_ParsesHandle 合法 handle 字符串 → 解析成功。
func TestLeaseReaderFromEnv_Windows_ParsesHandle(t *testing.T) {
	env := []string{envLeaseHandle + "=12345", "PATH=/usr/bin"}
	reader, ok := leaseReaderFromEnv(env)
	if !ok {
		t.Fatal("合法 handle 应 ok=true")
	}
	if reader == nil {
		t.Fatal("reader 不应为 nil")
	}
	// 注意：这里 reader.Close() 会关闭一个伪造的 handle 值，不调用 close（避免误关真实 handle）。
}

// TestLeaseReaderFromEnv_Windows_RejectsInvalidHandles 非法 handle 值 → ok=false。
func TestLeaseReaderFromEnv_Windows_RejectsInvalidHandles(t *testing.T) {
	cases := []string{"", "0", "abc", "-1"}
	for _, hs := range cases {
		env := []string{envLeaseHandle + "=" + hs}
		_, ok := leaseReaderFromEnv(env)
		if ok {
			t.Errorf("handleStr=%q 应 ok=false", hs)
		}
	}
}

// TestLeaseReaderFromEnv_Windows_MissingHandle 缺少 LEASE_HANDLE → ok=false。
func TestLeaseReaderFromEnv_Windows_MissingHandle(t *testing.T) {
	env := []string{envInstance + "=abc", "PATH=/usr/bin"}
	_, ok := leaseReaderFromEnv(env)
	if ok {
		t.Error("缺少 LEASE_HANDLE 应 ok=false")
	}
}

// TestHandleLeaseReader_EOFOnCloseWrite 父关闭 write handle → child read 端 EOF。
func TestHandleLeaseReader_EOFOnCloseWrite(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	defer h.cleanup()

	reader := &handleLeaseReader{handle: h.readHandle}
	done := make(chan struct{})
	go func() {
		reader.WaitForEOF()
		close(done)
	}()

	h.closeWrite()
	<-done
}

// TestLeasePipeHolderWindows_CleanupClosesBothHandles cleanup 后两端 handle 置零。
func TestLeasePipeHolderWindows_CleanupClosesBothHandles(t *testing.T) {
	h, err := newLeasePipeHolderWindows()
	if err != nil {
		t.Fatalf("newLeasePipeHolderWindows: %v", err)
	}
	rH := h.readHandle
	h.cleanup()
	if h.readHandle != 0 || h.writeHandle != 0 {
		t.Errorf("cleanup 后两端应置 0，read=%d write=%d", h.readHandle, h.writeHandle)
	}
	// 重复 cleanup 安全。
	h.cleanup()
	// 关闭已关闭的 handle 应失败（验证确实关闭了）。
	if err := syscall.CloseHandle(rH); err == nil {
		t.Error("read handle 应已关闭，再次 CloseHandle 应失败")
	}
}

// TestParseParentLease_Windows_ValidCombo Windows 平台合法组合（instance + handle）→ ok=true。
func TestParseParentLease_Windows_ValidCombo(t *testing.T) {
	env := []string{envInstance + "=abc", envLeaseHandle + "=12345", "PATH=/usr/bin"}
	desc, ok := parseParentLease(env)
	if !ok {
		t.Fatal("Windows 合法组合应 ok=true")
	}
	if desc.InstanceID != "abc" {
		t.Errorf("InstanceID=%q want abc", desc.InstanceID)
	}
	if desc.reader == nil {
		t.Error("reader 不应为 nil")
	}
}

// TestParseParentLease_Windows_FDOnly_Fails Windows 平台只出现 POSIX 的 fd 变量 →
// 平台不匹配，ok=false。
func TestParseParentLease_Windows_FDOnly_Fails(t *testing.T) {
	env := []string{envInstance + "=abc", envLeaseFD + "=5"}
	_, ok := parseParentLease(env)
	if ok {
		t.Error("Windows 平台不应接受 POSIX 的 LEASE_FD")
	}
}

// isHandleInheritable 用 GetHandleInformation 查询 handle 的 INHERIT 位。
func isHandleInheritable(h syscall.Handle) bool {
	const HANDLE_FLAG_INHERIT = 0x00000001
	flags, err := getHandleInformation(h)
	if err != nil {
		return false
	}
	return flags&HANDLE_FLAG_INHERIT != 0
}

// getHandleInformation 调用 kernel32 GetHandleInformation。
func getHandleInformation(h syscall.Handle) (uint32, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetHandleInformation")
	var flags uint32
	r1, _, err := proc.Call(uintptr(h), uintptr(unsafe.Pointer(&flags)))
	if r1 == 0 {
		return 0, err
	}
	return flags, nil
}
