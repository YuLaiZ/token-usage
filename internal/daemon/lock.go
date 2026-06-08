// internal/daemon/lock.go
package daemon

import (
	"os"
	"strconv"

	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"github.com/gofrs/flock"
)

// AcquireLock 尝试获取排他锁（非阻塞）。
// 成功返回 *flock.Flock（守护进程未运行），失败返回 nil（守护进程正在运行）。
// 跨平台：用 gofrs/flock 而非 syscall.Flock（windows 无此符号）。
func AcquireLock(lockPath string) (*flock.Flock, bool) {
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil || !locked {
		return nil, false
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
// 供 collect 命令的 checkDaemonConflict、config TUI 的 daemonChecker 使用。
func IsDaemonRunning(lockPath string) bool {
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return true // 保守判断为在运行（如锁文件不可创建）
	}
	if locked {
		// 获取到锁，无进程运行，立即释放
		if err := fl.Unlock(); err != nil {
			return true
		}
		return false
	}
	return true
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
