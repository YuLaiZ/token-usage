//go:build windows

package cli

import "github.com/YuLaiZ/token-usage/internal/update"

// buildUpdateInstaller 在 Windows 平台返回 staged replacement 安装器。
//
// Windows 不允许替换正在运行的可执行文件：该安装器在 Install 内构造 helper 计划、
// 复制 helper.exe 并 spawn 后台 helper，返回 ErrDeferredToHelper sentinel。
// installUnderLock 检测到该 sentinel 后跳过 Start/Commit/Rollback（由 helper 在父进程
// 退出后完成 MoveFileEx 替换与 daemon 重启）。renderApplyResult 据此提示用户
// 「后台替换已排队」，稍后用 `token-usage version` / `update --check` 确认最终版本。
func buildUpdateInstaller() update.Installer {
	return update.NewWindowsInstaller()
}
