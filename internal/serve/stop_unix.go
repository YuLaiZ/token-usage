//go:build !windows

package serve

import "syscall"

// signalProcPlatform 向后台仪表板进程发送 SIGTERM：后台服务主体内已
// 注册 SIGINT/SIGTERM 处理，进程会走 3s Shutdown 宽限的优雅关停并自清理
// serve.json。
func signalProcPlatform(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killProcPlatform 发送 SIGKILL 兜底强杀：优雅窗口内 /api/meta 仍响应时
// 由停止编排调用。
func killProcPlatform(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
