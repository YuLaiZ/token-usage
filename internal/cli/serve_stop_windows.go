//go:build windows

package cli

import (
	"os/exec"
	"strconv"
)

// Windows 控制台进程没有跨进程的优雅停止通道（无 POSIX 信号语义），
// serve stop 在 Windows 上统一用 taskkill /F 强杀。仪表板服务严格只读、
// 无在途事务，强杀不引入数据风险；serveDashboard 的状态文件 defer 自清理
// 因此不会执行，但 serve stop/status 的陈旧探活清理会兜底删除 serve.json。
func serveSignalProcPlatform(pid int) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F").Run()
}

// serveKillProcPlatform 在 Windows 上与优雅路径同一实现：taskkill /F 本身即强杀。
func serveKillProcPlatform(pid int) error {
	return serveSignalProcPlatform(pid)
}
