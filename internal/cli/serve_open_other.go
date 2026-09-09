//go:build !windows

package cli

import "os/exec"

// openBrowser 用系统默认浏览器打开 URL:非 Windows 平台统一走 open 命令
// (macOS 内置;其余 Unix-like 平台若无同名工具会启动失败,由调用方打印
// 双语警告继续服务,不致命)。Start 异步启动,不阻塞服务主循环。
func openBrowser(url string) error {
	return exec.Command("open", url).Start()
}
