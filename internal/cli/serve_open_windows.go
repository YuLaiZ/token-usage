//go:build windows

package cli

import "os/exec"

// openBrowser 用 rundll32 调用 url.dll 的 FileProtocolHandler 打开系统
// 默认浏览器(Windows 上无 open 命令的等价内建路径)。Start 异步启动,
// 不阻塞服务主循环;失败由调用方打印双语警告继续服务,不致命。
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
