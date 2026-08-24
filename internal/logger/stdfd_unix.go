//go:build !windows

package logger

import (
	"sync"
	"syscall"
)

// MirrorStdOutput 把当前进程的 fd 1/2 接管到当日结构化日志文件，并启动连续
// 重排的日界 timer（跨午夜后自动指向新当日文件）。panic 等 runtime 直接写
// stderr 的输出由此并入 logs/token-usage-*.log。
//
// 仅限 daemon 路径调用（CLI 前台进程的 stdout 是终端，不能接管）；重复调用
// 返回空 restore（首个 mirror 生效）。
//
// restore 完全撤销接管（幂等）：同一 writer 锁内置 mirrorStd=false（此后
// Write/日切/timer 回调不再重做接管）→ 停止 timer → fd 1/2 dup2 回接管前
// 副本并关闭副本。生产路径不调用 restore（daemon 生命周期 = 进程生命周期），
// 测试用 defer restore 验证两侧行为，避免污染测试进程 stderr。
func MirrorStdOutput() (restore func()) {
	w := globalWriter
	if w == nil {
		return func() {}
	}
	return w.enableMirrorStd()
}

// enableMirrorStd 是 MirrorStdOutput 的 writer 层实现（unix）。
func (w *rotatingWriter) enableMirrorStd() (restore func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.mirrorStd {
		return func() {}
	}
	saved1, err1 := syscall.Dup(1)
	saved2, err2 := syscall.Dup(2)
	if err1 != nil || err2 != nil {
		if saved1 >= 0 {
			syscall.Close(saved1)
		}
		if saved2 >= 0 {
			syscall.Close(saved2)
		}
		return func() {}
	}
	if err := w.ensureFileLocked(); err != nil {
		syscall.Close(saved1)
		syscall.Close(saved2)
		return func() {}
	}
	w.mirrorStd = true
	w.dupStdToFdLocked()
	w.scheduleNextDayBoundaryLocked()

	var once sync.Once
	return func() {
		once.Do(func() { w.disableMirrorStd(saved1, saved2) })
	}
}

// disableMirrorStd 恢复接管前的 fd 1/2 并停止日界 timer（内部获取 writer 锁）。
func (w *rotatingWriter) disableMirrorStd(saved1, saved2 int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mirrorStd = false
	if w.dayTimer != nil {
		w.dayTimer.Stop()
		w.dayTimer = nil
	}
	if saved1 >= 0 {
		syscall.Dup2(saved1, 1)
		syscall.Close(saved1)
	}
	if saved2 >= 0 {
		syscall.Dup2(saved2, 2)
		syscall.Close(saved2)
	}
}

// dupStdToFdLocked 把 fd 1/2 指向当前日志文件（须持锁调用；mirrorStd 已置真）。
func (w *rotatingWriter) dupStdToFdLocked() {
	if w.file == nil {
		return
	}
	fd := int(w.file.Fd())
	syscall.Dup2(fd, 1)
	syscall.Dup2(fd, 2)
}

// remirrorStdFDsLocked 供 ensureFileLocked 在文件切换后同步更新接管目标。
func (w *rotatingWriter) remirrorStdFDsLocked() {
	if !w.mirrorStd {
		return
	}
	w.dupStdToFdLocked()
}
