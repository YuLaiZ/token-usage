//go:build !windows

package cli

import (
	"context"
)

// update_helper_posix.go 提供 _update-helper / _update-cleanup 在非 Windows 平台的桩实现。
//
// 这两个命令是 Windows staged replacement 专属（Windows 不允许替换运行中的 .exe）。
// 非 Windows 平台用 POSIX 事务性安装器同步完成替换，无需后台 helper。
// 命令在所有平台注册（Hidden），但非 Windows 调用时返回错误，避免误用。
// errHelperNotSupported 在平台无关文件 update_helper.go 中定义。

// runUpdateHelperCmd 非 Windows 平台拒绝执行。
func runUpdateHelperCmd(ctx context.Context, planPath string) error {
	return errHelperNotSupported
}

// runUpdateCleanupCmd 非 Windows 平台拒绝执行。
func runUpdateCleanupCmd(ctx context.Context, planPath string, helperPID int, helperCreationTime uint64) error {
	return errHelperNotSupported
}
