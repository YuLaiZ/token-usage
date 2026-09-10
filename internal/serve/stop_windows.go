//go:build windows

package serve

import (
	"os/exec"
	"strconv"
)

// Windows 控制台进程没有跨进程的优雅停止通道（无 POSIX 信号语义），
// 停止编排（serve stop 命令与 update 的更新前停止共用）在 Windows 上统一用
// taskkill /F 强杀。仪表板服务严格只读、无在途事务，强杀不引入数据风险；
// 后台服务主体的状态文件 defer 自清理因此不会执行，但 serve stop/status 与
// 更新前探测的陈旧探活清理会兜底删除 serve.json。
func signalProcPlatform(pid int) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F").Run()
}

// killProcPlatform 在 Windows 上与优雅路径同一实现：taskkill /F 本身即强杀。
func killProcPlatform(pid int) error {
	return signalProcPlatform(pid)
}
