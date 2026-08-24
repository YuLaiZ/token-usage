//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// utf8CodePage 是 Windows 控制台 UTF-8 (65001) 代码页编号。
const utf8CodePage = 65001

// InitConsole 在交互式控制台会话下把控制台代码页切换为 UTF-8：输出侧切换
// 后中文输出在 cmd 与 PowerShell 5.1 的默认代码页下正常显示，输入侧切换后
// TUI 文本输入可携带中文路径。任一侧是真实控制台即切换对应方向；全部重定向
// （管道/文件）时不做任何设置——写字节流恒为 UTF-8，由捕获侧自行解码。
// 返回恢复原代码页的函数；无需切换时返回 nil。进程被强杀时恢复不会执行，
// 残留 UTF-8 代码页可由用户 chcp 936 手动恢复，属可接受的罕见边界。
func InitConsole() func() {
	outConsole := isWindowsConsole(os.Stdout) || isWindowsConsole(os.Stderr)
	inConsole := isWindowsConsole(os.Stdin)
	if !outConsole && !inConsole {
		return nil
	}
	return switchConsoleEncoding(outConsole, inConsole)
}

// isWindowsConsole 依据 GetConsoleMode 是否成功判断文件是否绑定真实控制台；
// 管道与文件重定向的句柄该调用失败。
func isWindowsConsole(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// switchConsoleEncoding 按方向把代码页切换为 UTF-8，返回恢复函数。
// 原值已是 UTF-8 或设置失败的方向不切换、不产生恢复步骤。
func switchConsoleEncoding(outConsole, inConsole bool) func() {
	var restores []func()
	if outConsole {
		prev, err := windows.GetConsoleOutputCP()
		if err == nil && prev != utf8CodePage {
			if err := windows.SetConsoleOutputCP(utf8CodePage); err == nil {
				restores = append(restores, func() { _ = windows.SetConsoleOutputCP(prev) })
			}
		}
	}
	if inConsole {
		prev, err := windows.GetConsoleCP()
		if err == nil && prev != utf8CodePage {
			if err := windows.SetConsoleCP(utf8CodePage); err == nil {
				restores = append(restores, func() { _ = windows.SetConsoleCP(prev) })
			}
		}
	}
	if len(restores) == 0 {
		return nil
	}
	return func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}
}
