// internal/daemon/lock.go
package daemon

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"github.com/gofrs/flock"
)

// acquireLockRetryWindow 是 AcquireLock 对短命共享读锁（只读状态探测）的让行
// 窗口：读探测在毫秒级释放，不应让真实的启动拿锁失败；真正的排他持有者会让
// 重试持续失败，窗口耗尽后按"已在运行"返回 false。
const (
	acquireLockRetryWindow   = time.Second
	acquireLockRetryInterval = 10 * time.Millisecond
)

// AcquireLock 尝试获取排他锁（阻塞至多 acquireLockRetryWindow）。
// 成功返回 *flock.Flock（守护进程未运行），失败返回 nil（守护进程正在运行
// 或锁不可用）。
// 跨平台：用 gofrs/flock 而非 syscall.Flock（windows 无此符号）。
func AcquireLock(lockPath string) (*flock.Flock, bool) {
	fl := flock.New(lockPath)
	deadline := time.Now().Add(acquireLockRetryWindow)
	for {
		locked, err := fl.TryLock()
		if err != nil {
			return nil, false
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(acquireLockRetryInterval)
	}

	// 写入 PID 便于调试（lock 文件仅调试用，不做活性检测）
	if f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
		f.Write(PidContent())
		f.Close()
	}

	return fl, true
}

// ReleaseLock 释放文件锁。
func ReleaseLock(fl *flock.Flock) error {
	if fl == nil {
		return nil
	}
	return fl.Unlock()
}

// IsDaemonRunning 检查守护进程是否正在运行（不抢锁、仅检测）。
// 供 collect 命令的 checkDaemonConflict、config TUI 的 daemonChecker 与
// control 层判活使用。
// 探测使用共享锁：只有 daemon 的排他锁会让它失败；状态查询等其他只读探测
// 持有的是共享锁、互不排斥——读者不会被误判成守护进程，也就不会让
// `daemon start` 的控制路径误报"已在运行"。
func IsDaemonRunning(lockPath string) bool {
	fl := flock.New(lockPath)
	locked, err := fl.TryRLock()
	if err != nil {
		return true // 保守判断为在运行（如锁文件不可创建）
	}
	if locked {
		// 获取到共享锁，无排他持有者，立即释放
		if err := fl.Unlock(); err != nil {
			return true
		}
		return false
	}
	return true
}

// ProbeResult 是 daemon lock 判活的三态结果。
type ProbeResult int

const (
	// ProbeRunning：锁被持有（有进程在运行）。
	ProbeRunning ProbeResult = iota
	// ProbeStopped：确认未运行。
	ProbeStopped
	// ProbeUnknown：锁探测失败，无法判定（与"确认未运行"严格区分）。
	ProbeUnknown
)

// ProbeDaemonLock 以三态方式探测 daemon lock，供只读状态接口使用：
// 锁错误（权限、只读文件系统等）返回 ProbeUnknown，而不是保守判成运行中。
// 只读约束：锁文件不存在时直接返回未运行，不创建锁文件；存在时以只读探测
// 打开既有文件，不修改内容。
// 探测使用共享锁：daemon 持排他锁时获取失败（确认运行中）；其他只读探测者
// 之间的共享锁互不排斥，并发探测不会被误判成"已运行"。
func ProbeDaemonLock(lockPath string) ProbeResult {
	if _, err := os.Stat(lockPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ProbeStopped
		}
		return ProbeUnknown
	}
	fl := flock.New(lockPath)
	locked, err := fl.TryRLock()
	if err != nil {
		return ProbeUnknown
	}
	if !locked {
		return ProbeRunning
	}
	if err := fl.Unlock(); err != nil {
		return ProbeUnknown
	}
	return ProbeStopped
}

// PidContent 返回当前进程 PID 的字节内容，用于写 lock 文件（仅调试，不做活性检测）。
// 注意：这是 lock 文件内容，不是 PID 协议文件；PID 协议文件由 runmeta.WritePIDFile 维护。
func PidContent() []byte {
	return []byte(strconv.Itoa(os.Getpid()))
}

// WritePID 写入 PID 文件（供 cat 调试，不用于活性检测）。
// 委托 runmeta.WritePIDFile：新格式 "<pid> <instanceID>"，用 ReplaceCompleteFile 原子写。
// instanceID 为空时写 "<pid> "（兼容调试场景；正式路径经 daemon.Run 传入真实 instanceID）。
func WritePID(pidPath string) error {
	return runmeta.WritePIDFile(pidPath, os.Getpid(), "")
}
