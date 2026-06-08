// internal/daemon/daemonpid.go
package daemon

import "github.com/YuLaiZ/token-usage/internal/runmeta"

// ReadDaemonPID 读 pidPath（daemon 写入的 token-usage.pid）取守护进程 PID。
// 仅读文件 + 格式校验，不做存活探测——调用方应先用 IsDaemonRunning(lock) 判活性；
// 活性为 true 时 pid 文件即为当前守护进程。pid 文件缺失/格式无效返回 error，调用方降级无 PID 提示。
//
// 委托 runmeta.ReadPIDFile：兼容新格式 "<pid> <instanceID>" 与旧格式 "<pid>"，
// 这里只返回 pid（instanceID 由 runmeta 双文件协议的 ready 握手路径使用，不在本函数暴露）。
func ReadDaemonPID(pidPath string) (int, error) {
	pid, _, err := runmeta.ReadPIDFile(pidPath)
	return pid, err
}
