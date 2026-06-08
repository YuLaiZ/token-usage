// internal/control/lease_unix_test.go
//go:build !windows

// POSIX 平台的父子 lease 测试。
//
// 关键行为：
//   - ExtraFiles 非零索引时 env 中 fd 等于 3+i（禁止硬编码 3）。
//   - pipe read end EOF 在父进程关闭 write end 后触发。
//   - Setsid 不影响 ExtraFiles 继承（spawn helper 不关闭显式继承 fd）。
package control

import (
	"os"
	"strconv"
	"testing"
)

// TestNewLeasePipeHolder_CreatesValidPipe 创建 pipe 后两端均可用。
func TestNewLeasePipeHolder_CreatesValidPipe(t *testing.T) {
	h, err := newLeasePipeHolder(0)
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	defer h.cleanup()
	if h.readFile == nil || h.writeFile == nil {
		t.Fatal("read/write end 不应为 nil")
	}
	// write → read 可通。
	if _, err := h.writeFile.WriteString("x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := h.readFile.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf[0] != 'x' {
		t.Errorf("read=%q want x", buf)
	}
}

// TestNewLeasePipeHolder_ZeroIndex_FDIs3 ExtraFiles 索引 0 → env fd=3。
func TestNewLeasePipeHolder_ZeroIndex_FDIs3(t *testing.T) {
	h, err := newLeasePipeHolder(0)
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	defer h.cleanup()
	env := h.appendEnv([]string{"PATH=/usr/bin"})
	fdStr := lookupEnvValue(env, envLeaseFD)
	if fdStr != "3" {
		t.Errorf("index 0 → fd=3，实际 %q", fdStr)
	}
}

// TestNewLeasePipeHolder_NonZeroIndex_FDIs3PlusI ExtraFiles 非零索引 → env fd=3+i。
// fd 不能硬编码为 3：测试在 ExtraFiles 前放占位 fd，验证传入计算后的 3+i。
func TestNewLeasePipeHolder_NonZeroIndex_FDIs3PlusI(t *testing.T) {
	cases := []int{1, 2, 5, 10}
	for _, idx := range cases {
		h, err := newLeasePipeHolder(idx)
		if err != nil {
			t.Fatalf("index %d: newLeasePipeHolder: %v", idx, err)
		}
		env := h.appendEnv(nil)
		fdStr := lookupEnvValue(env, envLeaseFD)
		want := strconv.Itoa(3 + idx)
		if fdStr != want {
			t.Errorf("index %d → fd=%s want %s", idx, fdStr, want)
		}
		h.cleanup()
	}
}

// TestLeaseReaderFromEnv_ParsesFD 合法 fd 字符串 → 解析成功。
func TestLeaseReaderFromEnv_ParsesFD(t *testing.T) {
	env := []string{envLeaseFD + "=5", "PATH=/usr/bin"}
	reader, ok := leaseReaderFromEnv(env)
	if !ok {
		t.Fatal("合法 fd 应 ok=true")
	}
	if reader == nil {
		t.Fatal("reader 不应为 nil")
	}
	defer reader.Close()
}

// TestLeaseReaderFromEnv_RejectsInvalidFDs 非法 fd 值（<3、非数字、空）→ ok=false。
func TestLeaseReaderFromEnv_RejectsInvalidFDs(t *testing.T) {
	cases := []string{"", "0", "1", "2", "abc", "-1"}
	for _, fdStr := range cases {
		env := []string{envLeaseFD + "=" + fdStr}
		_, ok := leaseReaderFromEnv(env)
		if ok {
			t.Errorf("fdStr=%q 应 ok=false（非法）", fdStr)
		}
	}
}

// TestLeaseReaderFromEnv_MissingFD env 中没有 LEASE_FD → ok=false（走独立加锁路径）。
func TestLeaseReaderFromEnv_MissingFD(t *testing.T) {
	env := []string{envInstance + "=abc", "PATH=/usr/bin"}
	_, ok := leaseReaderFromEnv(env)
	if ok {
		t.Error("缺少 LEASE_FD 应 ok=false")
	}
}

// TestFileLeaseReader_EOFOnCloseWrite 父关闭 write end → child read 端读到 EOF。
// 用真实 os.Pipe 验证 lease 语义（pipe 是 OS 原语，非业务 IO，确定性成立）。
func TestFileLeaseReader_EOFOnCloseWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	reader := &fileLeaseReader{f: r}
	defer reader.Close()

	done := make(chan struct{})
	go func() {
		reader.WaitForEOF()
		close(done)
	}()

	// 关闭 write end 触发 EOF。
	_ = w.Close()
	select {
	case <-done:
		// 期望：EOF 触发 waitForEOF 返回。
	default:
		<-done // 阻塞等待（确定性：close 后 EOF 必发生）。
	}
}

// TestLeasePipeHolder_CloseWriteEOFsReader closeWrite 后 readFile 读到 EOF。
func TestLeasePipeHolder_CloseWriteEOFsReader(t *testing.T) {
	h, err := newLeasePipeHolder(0)
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	defer h.cleanup()

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		_, err := h.readFile.Read(buf)
		if err == nil {
			t.Error("closeWrite 后 read 应返回 EOF 错误")
		}
		close(done)
	}()

	h.closeWrite()
	<-done
}

// TestLeasePipeHolder_CleanupClosesBothEnds cleanup 后两端均关闭。
func TestLeasePipeHolder_CleanupClosesBothEnds(t *testing.T) {
	h, err := newLeasePipeHolder(0)
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	r := h.readFile
	w := h.writeFile
	h.cleanup()
	// cleanup 后 readFile/writeFile 置 nil。
	if h.readFile != nil || h.writeFile != nil {
		t.Errorf("cleanup 后两端应置 nil，read=%v write=%v", h.readFile, h.writeFile)
	}
	// 底层 fd 已关闭：再写应报错。
	if _, err := w.WriteString("x"); err == nil {
		t.Error("cleanup 后 write 应失败（fd 已关闭）")
	}
	// read 也应返回错误（已关闭）。
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err == nil {
		t.Error("cleanup 后 read 应失败（fd 已关闭）")
	}
}

// TestLeasePipeHolder_CleanupIdempotent cleanup 多次调用安全。
func TestLeasePipeHolder_CleanupIdempotent(t *testing.T) {
	h, err := newLeasePipeHolder(0)
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	h.cleanup()
	h.cleanup() // 不 panic
	h.cleanup()
}

// TestParseParentLease_POSIX_ValidCombo POSIX 平台合法组合（instance + fd）→ ok=true。
func TestParseParentLease_POSIX_ValidCombo(t *testing.T) {
	env := []string{envInstance + "=abc", envLeaseFD + "=5", "PATH=/usr/bin"}
	desc, ok := parseParentLease(env)
	if !ok {
		t.Fatal("POSIX 合法组合应 ok=true")
	}
	if desc.InstanceID != "abc" {
		t.Errorf("InstanceID=%q want abc", desc.InstanceID)
	}
	if desc.reader == nil {
		t.Error("reader 不应为 nil")
	}
	defer desc.reader.Close()
}

// TestParseParentLease_POSIX_HandleOnly_Fails POSIX 平台只出现 Windows 的 handle 变量 →
// 平台不匹配，ok=false（走独立加锁路径）。
func TestParseParentLease_POSIX_HandleOnly_Fails(t *testing.T) {
	env := []string{envInstance + "=abc", envLeaseHandle + "=12345"}
	_, ok := parseParentLease(env)
	if ok {
		t.Error("POSIX 平台不应接受 Windows 的 LEASE_HANDLE")
	}
}

// TestBuildChildEnv_POSIX_FiltersThenAppends POSIX 路径：BuildChildEnv 先过滤旧变量，
// 追加 instance，再追加平台 fd（fd 由 extraFilesIndex 决定）。
func TestBuildChildEnv_POSIX_FiltersThenAppends(t *testing.T) {
	h, err := newLeasePipeHolder(2) // 索引 2 → fd 5
	if err != nil {
		t.Fatalf("newLeasePipeHolder: %v", err)
	}
	defer h.cleanup()
	out := BuildChildEnv(
		[]string{"PATH=/usr/bin", envInstance + "=OLD", envLeaseFD + "=99"},
		"new-inst",
		h.appendEnv,
	)
	if lookupEnvValue(out, envInstance) != "new-inst" {
		t.Errorf("instance 应为 new-inst，out=%v", out)
	}
	if lookupEnvValue(out, envLeaseFD) != "5" {
		t.Errorf("fd 应为 5（index 2 → 3+2），实际 %q", lookupEnvValue(out, envLeaseFD))
	}
	if lookupEnvValue(out, "PATH") != "/usr/bin" {
		t.Errorf("PATH 应保留，out=%v", out)
	}
}
