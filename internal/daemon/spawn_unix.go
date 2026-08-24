// internal/daemon/spawn_unix.go
//go:build !windows

package daemon

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// SpawnDetached 拉起一个脱离当前终端会话的子进程（detached）。
// macOS 用 Setsid 创建新会话，脱离控制终端，子进程不再收 SIGHUP。
// 返回 *exec.Cmd 供调用方获取 PID + Release。
//
// Lease 集成（opts.Lease != nil 时）：
//   - read end（opts.Lease.Reader，类型 *os.File）放入 cmd.ExtraFiles，child 中成为 fd 3+i；
//   - child env 先过滤三项内部 lease 变量，再追加 instanceID 与 fd=3+i（禁止硬编码 3）；
//   - cmd.Start() 成功后由调用方关闭父侧 read end 副本；本 helper 不争夺该 fd 的关闭所有权；
//   - Setsid=true 只创建新 session，不影响 ExtraFiles 显式继承 fd。
func SpawnDetached(opts SpawnOptions) (*exec.Cmd, error) {
	cmd := exec.Command(opts.BinPath, opts.Args...)
	cmd.SysProcAttr = detachedSysProcAttr()
	if opts.StdoutPath != "" {
		f, err := os.OpenFile(opts.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to open stdout log", "打开 stdout 日志失败"), err)
		}
		cmd.Stdout = f
		// defer f.Close() 在 SpawnDetached 返回时关闭父进程侧 fd，
		// 子进程已 Start 并继承了 fd，不受影响（Go exec 包惯用法）。
		defer f.Close()
	} else {
		cmd.Stdout = io.Discard
	}
	if opts.StderrPath != "" {
		f, err := os.OpenFile(opts.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to open stderr log", "打开 stderr 日志失败"), err)
		}
		cmd.Stderr = f
		defer f.Close()
	} else {
		cmd.Stderr = io.Discard
	}

	if opts.Lease != nil {
		rf, ok := opts.Lease.Reader.(*os.File)
		if !ok {
			return nil, fmt.Errorf("%s: %T", ui.Bi("on POSIX, Lease.Reader must be *os.File, got", "POSIX 平台 Lease.Reader 必须是 *os.File，实际"), opts.Lease.Reader)
		}
		// ExtraFiles[i] 在 child 中成为 fd 3+i。记录索引供 env 写值。
		cmd.ExtraFiles = append(cmd.ExtraFiles, rf)
		fdIndex := len(cmd.ExtraFiles) - 1
		// 构造 child env：先过滤三项内部变量，再追加 instance + fd。
		cmd.Env = buildChildEnvWithLease(os.Environ(), opts.Lease.InstanceID, fdIndex)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to start detached child process", "启动 detached 子进程失败"), err)
	}
	return cmd, nil
}

// leaseEnvVarNames 是需要从 child env 中过滤的内部 lease 变量（与 control 包常量一致）。
// 这里独立定义避免 daemon 包 import control（依赖方向：control → daemon，不可反向）。
var leaseEnvVarNames = []string{
	"TOKEN_USAGE_START_INSTANCE",
	"TOKEN_USAGE_LEASE_FD",
	"TOKEN_USAGE_LEASE_HANDLE",
}

// buildChildEnvWithLease 构造 child env：
//  1. 过滤三项内部 lease 变量（清除残留）；
//  2. 追加 TOKEN_USAGE_START_INSTANCE=<instanceID>；
//  3. 追加 TOKEN_USAGE_LEASE_FD=<3+fdIndex>（POSIX：read end 在 ExtraFiles 的索引）。
//
// fdIndex 是 read end 在 cmd.ExtraFiles 中的位置；child 中对应 fd = 3 + fdIndex。
func buildChildEnvWithLease(parentEnv []string, instanceID string, fdIndex int) []string {
	out := make([]string, 0, len(parentEnv)+2)
	for _, kv := range parentEnv {
		if isInternalLeaseVar(kv) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TOKEN_USAGE_START_INSTANCE="+instanceID)
	out = append(out, "TOKEN_USAGE_LEASE_FD="+strconv.Itoa(3+fdIndex))
	return out
}

// isInternalLeaseVar 判断 kv 是否是三项内部 lease 变量之一（按 "KEY=" 前缀精确匹配）。
func isInternalLeaseVar(kv string) bool {
	for _, name := range leaseEnvVarNames {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// detachedSysProcAttr 构造 POSIX detached 的 SysProcAttr（Setsid=true）。
// 单测可见（同 package daemon 测试可直接调用）。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
