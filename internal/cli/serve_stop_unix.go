//go:build !windows

package cli

import "syscall"

// serveSignalProcPlatform 向后台仪表板进程发送 SIGTERM：serveDashboard 内已
// 注册 SIGINT/SIGTERM 处理，进程会走 3s Shutdown 宽限的优雅关停并自清理
// serve.json。
func serveSignalProcPlatform(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// serveKillProcPlatform 发送 SIGKILL 兜底强杀：优雅窗口内 /api/meta 仍响应时
// 由 serve stop 调用。
func serveKillProcPlatform(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
