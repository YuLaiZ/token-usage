//go:build windows

package logger

// MirrorStdOutput 在 Windows 上是 no-op：注册表 Run 拉起的进程 std handles 为空、
// Go runtime 无 fd 级接管手段，panic 输出不可捕获（平台局限，兜底依赖
// spawn 路径的 Stdout/StderrPath 重定向，见 control.buildSpawnOptionsForBin）。
func MirrorStdOutput() (restore func()) { return func() {} }

// enableMirrorStd 不会被调用（MirrorStdOutput 已 no-op），提供实现以满足
// logger.go 对平台钩子的引用。
func (w *rotatingWriter) enableMirrorStd() (restore func()) { return func() {} }

// remirrorStdFDsLocked Windows no-op（见 stdfd_unix.go 的 unix 实现）。
func (w *rotatingWriter) remirrorStdFDsLocked() {}
