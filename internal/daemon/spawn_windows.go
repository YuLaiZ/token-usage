// internal/daemon/spawn_windows.go
//go:build windows

package daemon

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Windows 常量（syscall 包未全部导出）
const (
	windowsCREATE_NEW_PROCESS_GROUP = 0x00000200
	windowsCREATE_NO_WINDOW         = 0x08000000
)

// SpawnDetached 拉起一个脱离父 console 的子进程（Windows）。
// CREATE_NEW_PROCESS_GROUP + CREATE_NO_WINDOW，不组合 DETACHED_PROCESS（会使前者被忽略）。
//
// Lease 集成（opts.Lease != nil 时）：
//   - read handle（opts.Lease.Reader，类型 syscall.Handle，已标记 inheritable）放入
//     SysProcAttr.AdditionalInheritedHandles；
//   - child env 先过滤三项内部 lease 变量，再追加 instanceID 与 handle 数值；
//   - Start 成功后由调用方关闭父侧 read handle 副本；本 helper 不争夺关闭所有权。
//   - write handle 及无关 handle 保持 non-inheritable（由调用方在创建 pipe 时保证）。
func SpawnDetached(opts SpawnOptions) (*exec.Cmd, error) {
	cmd := exec.Command(opts.BinPath, opts.Args...)
	cmd.SysProcAttr = detachedSysProcAttr()
	if opts.StdoutPath != "" {
		f, err := os.OpenFile(opts.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("打开 stdout 日志失败: %w", err)
		}
		cmd.Stdout = f
		defer f.Close()
	} else {
		cmd.Stdout = io.Discard
	}
	if opts.StderrPath != "" {
		f, err := os.OpenFile(opts.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("打开 stderr 日志失败: %w", err)
		}
		cmd.Stderr = f
		defer f.Close()
	} else {
		cmd.Stderr = io.Discard
	}

	if opts.Lease != nil {
		rh, ok := opts.Lease.Reader.(syscall.Handle)
		if !ok {
			return nil, fmt.Errorf("Windows 平台 Lease.Reader 必须是 syscall.Handle，实际 %T", opts.Lease.Reader)
		}
		// AdditionalInheritedHandles：CreateProcess 通过 PROC_THREAD_ATTRIBUTE_HANDLE_LIST
		// 限定只继承列表中的 handle（见 stdlib exec_windows.go）。write/unrelated handle
		// 因不在列表中且标记 non-inheritable 而不继承。
		cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, rh)
		// 构造 child env：过滤 + 追加 instance + handle。
		cmd.Env = buildChildEnvWithLeaseWindows(os.Environ(), opts.Lease.InstanceID, rh)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 detached 子进程失败: %w", err)
	}
	return cmd, nil
}

// leaseEnvVarNamesWindows 是需要从 child env 中过滤的内部 lease 变量。
var leaseEnvVarNamesWindows = []string{
	"TOKEN_USAGE_START_INSTANCE",
	"TOKEN_USAGE_LEASE_FD",
	"TOKEN_USAGE_LEASE_HANDLE",
}

// buildChildEnvWithLeaseWindows 构造 child env（Windows）：
//  1. 过滤三项内部 lease 变量；
//  2. 追加 TOKEN_USAGE_START_INSTANCE=<instanceID>；
//  3. 追加 TOKEN_USAGE_LEASE_HANDLE=<handle 数值>。
func buildChildEnvWithLeaseWindows(parentEnv []string, instanceID string, handle syscall.Handle) []string {
	out := make([]string, 0, len(parentEnv)+2)
	for _, kv := range parentEnv {
		if isInternalLeaseVarWindows(kv) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TOKEN_USAGE_START_INSTANCE="+instanceID)
	out = append(out, "TOKEN_USAGE_LEASE_HANDLE="+strconv.FormatUint(uint64(handle), 10))
	return out
}

// isInternalLeaseVarWindows 判断 kv 是否是三项内部 lease 变量之一。
func isInternalLeaseVarWindows(kv string) bool {
	for _, name := range leaseEnvVarNamesWindows {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// detachedSysProcAttr 构造 Windows detached 的 SysProcAttr。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windowsCREATE_NEW_PROCESS_GROUP | windowsCREATE_NO_WINDOW,
	}
}
