//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT 把 stdout 绑定的真实控制台启用虚拟终端处理(ANSI 转义序列):
// watch 的每帧清屏/归位依赖该模式,Windows 10 之前的 conhost 默认关闭。
// stdout 重定向到管道/文件时句柄无控制台模式,直接跳过(转义序列写字节流
// 由捕获方自行处理);设置失败同样静默跳过——乱码可读性优于不可用。
func enableVT(f *os.File) {
	var mode uint32
	h := windows.Handle(f.Fd())
	if windows.GetConsoleMode(h, &mode) != nil {
		return
	}
	const enableVirtualTerminalProcessing = 0x0004
	_ = windows.SetConsoleMode(h, mode|enableVirtualTerminalProcessing)
}
