//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/YuLaiZ/token-usage/internal/update"
)

// update_helper_windows.go 实现 _update-helper / _update-cleanup 的 Windows 生产逻辑。
//
// _update-helper 流程：
//  1. 解析自身路径（os.Executable，即 nonce 命名的 helper.exe 副本）；
//  2. 装配 Windows seam（等待父进程 / MoveFileEx / result 写入）+ control.Manager + ConfigLoader；
//  3. 经 helperRunner 完成校验计划 → 等父进程退出 → 锁内 backup → MoveFileEx → 重启 daemon；
//  4. 成功后 spawn _update-cleanup（新 target 等待 helper 退出后清理临时文件）。
//
// _update-cleanup 流程：
//  1. 等待 helper PID 退出（helper.exe 仍在运行时无法删除）；
//  2. 读计划取 nonce/targetBasename，按派生路径删除 helper.exe/plan/stage/backup（仅普通文件）。

// cleanupWaitTimeout 是 _update-cleanup 等待 helper 退出的有限超时。
const cleanupWaitTimeout = 2 * time.Minute

// runUpdateHelperCmd 执行后台 helper 替换流程。
func runUpdateHelperCmd(ctx context.Context, planPath string) error {
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to resolve helper own path", "解析 helper 自身路径失败"), err)
	}

	// 装配 Windows seam + control.Manager + ConfigLoader。
	parent, mover, result := update.NewWindowsHelperSeams()
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}
	cm := update.NewControlManager(mgr)

	runner, err := update.NewHelperRunner(parent, mover, result, cm, loadConfig, os.Stderr)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to assemble helper runner", "装配 helper runner 失败"), err)
	}

	if err := runner.Run(ctx, selfExe, planPath); err != nil {
		// runner 已在能确定 result 路径的阶段写入失败 result；此处返回错误供进程退出码诊断。
		return err
	}

	// 成功：spawn _update-cleanup（新 target 等待 helper 退出后清理）。
	// 新 target 路径由计划派生（= selfExe 同目录的 target basename）。
	validated, verr := update.ValidateHelperPlan(selfExe, planPath)
	if verr != nil {
		// 替换已成功但无法派生 cleanup 路径——临时文件将残留，不影响更新结果。
		return nil
	}
	// 捕获 helper 自身身份（PID + 创建时间）传给 cleanup，供其按显式身份等待 helper 退出，
	// 杜绝 helper PID 被复用后 cleanup 误等无关进程。
	helperID, err := update.CaptureCurrentIdentity()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to capture helper own identity", "捕获 helper 自身身份失败"), err)
	}
	return spawnUpdateCleanup(validated.Paths.Target, validated.Paths.Plan, helperID.PID, helperID.CreationTime)
}

// runUpdateCleanupCmd 按 helper 身份等待其退出，随后清理临时文件。
func runUpdateCleanupCmd(ctx context.Context, planPath string, helperPID int, helperCreationTime uint64) error {
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to resolve cleanup own path", "解析 cleanup 自身路径失败"), err)
	}
	validated, err := update.ValidateCleanupPlan(selfExe, planPath)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to validate cleanup plan", "校验 cleanup 计划失败"), err)
	}

	// 按 helper 身份（PID + 创建时间）等待其退出（helper.exe 运行时无法删除）。
	// 必须用显式身份，杜绝 PID 复用导致误等无关进程或误删文件。
	identity, err := cleanupHelperIdentity(helperPID, helperCreationTime)
	if err != nil {
		return err
	}
	probe := update.NewWindowsProcessProbe()
	waitCtx, cancel := context.WithTimeout(ctx, cleanupWaitTimeout)
	defer cancel()
	if err := update.WaitProcessIdentity(waitCtx, probe, identity); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to wait for helper exit", "等待 helper 退出失败"), err)
	}
	return update.CleanupHelperTempFiles(filepath.Dir(validated.Paths.Target), validated.Plan.TargetBasename, validated.Plan.Nonce)
}

// spawnUpdateCleanup 拉起新 target 的隐藏 _update-cleanup 命令，等待 helper（自身）退出后清理。
// detached + 无窗口；放弃 wait 权（cleanup 由系统收养）。helperPID + helperCreationTime
// 是 helper 自身身份，供 cleanup 按显式身份等待。
func spawnUpdateCleanup(newTarget, planPath string, helperPID uint32, helperCreationTime uint64) error {
	cmd := exec.Command(newTarget, "_update-cleanup",
		"--plan", planPath,
		"--helper-pid", fmt.Sprintf("%d", helperPID),
		"--helper-creation-time", fmt.Sprintf("%d", helperCreationTime))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windowsCREATE_NEW_PROCESS_GROUP | windowsCREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to spawn cleanup", "spawn cleanup 失败"), err)
	}
	_ = cmd.Process.Release()
	return nil
}

// Windows 进程创建标志（与 internal/update/install_windows.go 一致）。
const (
	windowsCREATE_NEW_PROCESS_GROUP = 0x00000200
	windowsCREATE_NO_WINDOW         = 0x08000000
)
