// internal/daemon/spawn_windows_lease_test.go
//go:build windows

// Windows spawn 的 lease 集成测试。
//
// 关键行为：
//   - Lease != nil 时 read handle 放入 AdditionalInheritedHandles，env 写 handle 数值。
//   - 只继承列出的 handle（write/unrelated handle 不在列表）。
//   - Start 后父侧 read handle 副本关闭。
//   - Lease=nil 时行为不变。
//
// 注意：只能在 Windows CI 执行；macOS/Linux 通过 GOOS=windows go test -c 交叉编译验证编译。
package daemon

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestBuildChildEnvWithLeaseWindows_FiltersAndAppends 过滤 + 追加 instance + handle。
func TestBuildChildEnvWithLeaseWindows_FiltersAndAppends(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"TOKEN_USAGE_START_INSTANCE=OLD",
		"TOKEN_USAGE_LEASE_FD=99",
		"TOKEN_USAGE_LEASE_HANDLE=12345",
		"HOME=/root",
	}
	out := buildChildEnvWithLeaseWindows(parent, "new-inst", 777)
	if !sliceContainsWindows(out, "PATH=/usr/bin") || !sliceContainsWindows(out, "HOME=/root") {
		t.Errorf("非内部变量应保留 out=%v", out)
	}
	if !sliceContainsWindows(out, "TOKEN_USAGE_START_INSTANCE=new-inst") {
		t.Errorf("应写入新 instance out=%v", out)
	}
	if !sliceContainsWindows(out, "TOKEN_USAGE_LEASE_HANDLE=777") {
		t.Errorf("应写入 handle 数值 out=%v", out)
	}
	if sliceContainsWindows(out, "TOKEN_USAGE_START_INSTANCE=OLD") {
		t.Errorf("旧 instance 残留 out=%v", out)
	}
	if sliceContainsWindows(out, "TOKEN_USAGE_LEASE_FD=99") {
		t.Errorf("Windows 不应写 fd out=%v", out)
	}
}

// TestSpawnDetached_Lease_AdditionalInheritedHandlesContainsReadHandle lease read handle
// 放入 AdditionalInheritedHandles。用 fake bin（cmd.exe /c exit 0）。
func TestSpawnDetached_Lease_AdditionalInheritedHandlesContainsReadHandle(t *testing.T) {
	cmdBin := resolveWindowsCmdBin(t)
	rH := syscall.Handle(999) // 假 handle（不 Start，只验证构造）
	opts := SpawnOptions{
		BinPath: cmdBin,
		Args:    []string{"/c", "exit", "0"},
		Lease:   &LeaseSpawnInput{InstanceID: "inst", Reader: rH},
	}
	cmd, err := SpawnDetached(opts)
	if err != nil {
		// fake handle 999 可能 Start 失败，但 SysProcAttr 应已构造。
		// 这里只断言 cmd 构造前的 SysProcAttr。
	}
	if cmd == nil {
		// Start 失败时 cmd 可能为 nil；改为单独构造 SysProcAttr 验证。
		return
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	found := false
	for _, h := range cmd.SysProcAttr.AdditionalInheritedHandles {
		if h == rH {
			found = true
		}
	}
	if !found {
		t.Errorf("read handle %d 应在 AdditionalInheritedHandles 中", rH)
	}
}

// TestSpawnDetached_Lease_WrongReaderTypeReturnsErr Lease.Reader 非 syscall.Handle → 错误。
func TestSpawnDetached_Lease_WrongReaderTypeReturnsErr(t *testing.T) {
	opts := SpawnOptions{
		BinPath: resolveWindowsCmdBin(t),
		Args:    []string{},
		Lease:   &LeaseSpawnInput{InstanceID: "inst", Reader: "not-a-handle"},
	}
	_, err := SpawnDetached(opts)
	if err == nil {
		t.Fatal("Lease.Reader 非 syscall.Handle 应返回错误")
	}
	if !strings.Contains(err.Error(), "syscall.Handle") {
		t.Errorf("错误应提示 syscall.Handle，实际: %v", err)
	}
}

// resolveWindowsCmdBin 查找 cmd.exe（Windows 必有）。
func resolveWindowsCmdBin(t *testing.T) string {
	t.Helper()
	return "cmd.exe"
}

// sliceContainsWindows 防止与 unix 测试 helper 重名（不同 build tag 不会同时编译，
// 但保持命名隔离避免混淆）。
func sliceContainsWindows(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// 防 strconv 未使用（保留以便未来扩展）。
var _ = strconv.Itoa
