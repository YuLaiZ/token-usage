//go:build !windows

package cli

import "os"

// enableVT 在非 Windows 平台为空操作:主流终端默认支持 ANSI 转义序列。
func enableVT(f *os.File) {
	_ = f
}
