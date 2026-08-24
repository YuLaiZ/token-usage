//go:build !windows

package cli

// InitConsole 在非 Windows 平台无控制台代码页概念，返回 nil 表示无需恢复。
func InitConsole() func() { return nil }
