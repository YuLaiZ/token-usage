//go:build !windows

package cli

import "github.com/YuLaiZ/token-usage/internal/update"

// buildUpdateInstaller 在非 Windows 平台返回 POSIX 事务性安装器。
// 该安装器在 control lock 内完成 backup + journal + 原子 rename，
// 失败可回滚到旧版本；daemon 重启由 installUnderLock 编排。
func buildUpdateInstaller() update.Installer {
	return update.NewPosixInstaller()
}
