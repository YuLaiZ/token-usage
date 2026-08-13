//go:build windows

package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows 进程创建标志（与 internal/daemon/spawn_windows.go 一致）。
// CREATE_NEW_PROCESS_GROUP 让 helper 脱离父 console；CREATE_NO_WINDOW 避免弹窗。
const (
	windowsCREATE_NEW_PROCESS_GROUP = 0x00000200
	windowsCREATE_NO_WINDOW         = 0x08000000
)

// install_windows.go 实现 Windows 平台的 staged replacement 安装器。
//
// Windows 不允许替换正在运行的可执行文件（父进程退出前 .exe 被占用）。
// 故 Install 不做同步文件替换，而是：
//  1. 把已下载并校验的 stage 复制为 nonce 派生的 stage 文件（helper 只认 nonce 派生路径）；
//  2. 写 helper 计划（记录 nonce / target basename / 旧新 hash / 原 daemon 运行态）；
//  3. 复制当前 target 为同目录 nonce 命名的 helper.exe（可独立运行的副本）；
//  4. spawn helper.exe 的隐藏内部命令（_update-helper），由它在父进程退出后完成
//     MoveFileEx 替换与 daemon 重启；
//  5. 返回 (targetBinPath, ErrDeferredToHelper)——installUnderLock 据此跳过
//     Start/Commit/Rollback（全部由 helper 负责）。
//
// 实际的 MoveFileEx / daemon 切换 / 回滚由后台 helper 经 helperRunner 完成
// （见 helper_runner.go + 生产 seam 实现 helper_seams_windows.go）。

// windowsInstaller 是 Windows staged replacement 安装器，满足 Installer 接口。
// selfIdentity 注入点用于测试（默认捕获真实自身身份）；零值可用（Install 回退到真实捕获）。
type windowsInstaller struct {
	selfIdentity func() (ProcessIdentity, error)
	logDir       string // 升级日志目录（经 SetLogDir 注入，用于重定向 helper stderr）
}

// NewWindowsInstaller 构造 Windows staged replacement 安装器。
// 供 CLI 装配层在 Windows 平台注入 Service.Installer。
func NewWindowsInstaller() Installer { return &windowsInstaller{selfIdentity: CaptureCurrentIdentity} }

// SetLogDir 注入升级日志目录，供 spawnUpdateHelper 重定向 helper stderr 到日志文件。
// 实现 HelperLogDirSetter 接口，供 CLI 工厂经类型断言注入。空字符串=不重定向。
func (inst *windowsInstaller) SetLogDir(dir string) {
	inst.logDir = dir
}

// Platform 返回 "windows"。
func (windowsInstaller) Platform() string { return "windows" }

// Install 构造 helper 计划、复制 helper.exe、spawn 后台 helper，返回 sentinel。
//
// stagePath 是 DownloadAsset 产出并校验过 SHA256 的新版本二进制绝对路径；
// oldBinPath / targetBinPath 均为当前二进制路径（被覆盖的目标）；
// wasRunning 是替换前 daemon 运行态，写入计划供 helper 据此决定是否重启 daemon。
//
// 成功返回 (targetBinPath, ErrDeferredToHelper)；任一前置步骤失败返回普通 error
// （此时未 spawn helper，调用方按普通安装失败回滚）。
func (inst windowsInstaller) Install(ctx context.Context, stagePath, oldBinPath, targetBinPath string, wasRunning bool) (string, error) {
	if err := validateInstallInputs(stagePath, targetBinPath); err != nil {
		return "", err
	}

	nonce, err := generateNonce()
	if err != nil {
		return "", err
	}

	targetDir := filepath.Dir(targetBinPath)
	targetBase := filepath.Base(targetBinPath)
	paths := deriveHelperPaths(targetDir, targetBase, nonce)

	// 旧 / 新 hash：旧供 helper backup 校验与回滚，新供替换后 target 校验。
	oldHash, err := fileSHA256(targetBinPath)
	if err != nil {
		return "", fmt.Errorf("事务前校验旧 target hash 失败: %w", err)
	}
	newHash, err := fileSHA256(stagePath)
	if err != nil {
		return "", fmt.Errorf("事务前校验新 stage hash 失败: %w", err)
	}

	// 捕获父进程（自身）身份。顺序硬约束：先捕获身份 → 写入 plan.Parent → 再 spawn helper。
	// helper 据此身份等待精确的父进程实例退出，杜绝 PID 复用导致的 TOCTOU。
	identityProvider := inst.selfIdentity
	if identityProvider == nil {
		identityProvider = CaptureCurrentIdentity
	}
	parentIdentity, err := identityProvider()
	if err != nil {
		return "", fmt.Errorf("捕获父进程身份失败: %w", err)
	}

	// 把下载的 stage 复制为 nonce 派生的 stage 文件（helper 只认 nonce 派生路径，杜绝路径注入）。
	if err := copyFileWithMode(stagePath, paths.Stage); err != nil {
		return "", fmt.Errorf("复制 stage 到 helper 派生路径失败: %w", err)
	}
	if err := verifyFileHash(paths.Stage, newHash); err != nil {
		_ = removeRegularFile(paths.Stage)
		return "", fmt.Errorf("helper stage 校验失败: %w", err)
	}

	// 写 helper 计划（0600 原子写）。Parent 携带捕获的父进程身份，plan 写后不再改写。
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: targetBase,
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     wasRunning,
		Parent:         parentIdentity,
	}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		_ = removeRegularFile(paths.Stage)
		return "", fmt.Errorf("写 helper 计划失败: %w", err)
	}

	// 复制当前 target 为 helper.exe（helper 自身是当前二进制的独立副本，可独立运行）。
	if err := copyFileWithMode(targetBinPath, paths.Helper); err != nil {
		_ = cleanupTransactionFiles(paths.Stage, "", paths.Plan)
		return "", fmt.Errorf("复制 helper.exe 失败: %w", err)
	}

	// spawn helper.exe 隐藏进程：_update-helper --plan <planPath>。
	// helper 从校验过的 plan.Parent 取父进程身份，按 PID + 创建时间等待其退出。
	// logDir 非空时 helper stderr 重定向到升级日志文件（append），使 helper 的 [helper]
	// 步骤日志落入同一文件。
	if err := spawnUpdateHelper(paths.Helper, paths.Plan, inst.logDir); err != nil {
		_ = cleanupTransactionFiles(paths.Stage, "", paths.Plan)
		_ = removeRegularFile(paths.Helper)
		return "", fmt.Errorf("spawn 后台 helper 失败: %w", err)
	}

	// helper 已接管：返回 sentinel，installUnderLock 据此跳过 Start/Commit/Rollback。
	return targetBinPath, ErrDeferredToHelper
}

// CaptureCurrentIdentity 返回当前进程的身份：PID + 创建时间。
// 用 GetCurrentProcess 伪句柄调用 GetProcessTimes（伪句柄无需 Close）。
// 两处复用：install_windows 的父进程身份捕获（写入 plan.Parent）+
// cli cleanup 的 helper 自身身份捕获（传给 _update-cleanup）。
func CaptureCurrentIdentity() (ProcessIdentity, error) {
	pid := windows.GetCurrentProcessId()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return ProcessIdentity{}, fmt.Errorf("GetProcessTimes(自身) 失败: %w", err)
	}
	return ProcessIdentity{
		PID:          pid,
		CreationTime: uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime),
	}, nil
}

// spawnUpdateHelper 拉起 helper.exe 的隐藏 _update-helper 内部命令。
// CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW，detached（参考 daemon.SpawnDetached）。
// logDir 非空时把 helper stderr 重定向到升级日志文件（append），使 helper 的 [helper]
// 步骤日志与父进程的 [update] 日志落入同一文件。打开失败 best-effort 降级（不重定向）。
func spawnUpdateHelper(helperExe, planPath, logDir string) error {
	cmd := exec.Command(helperExe, "_update-helper", "--plan", planPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windowsCREATE_NEW_PROCESS_GROUP | windowsCREATE_NO_WINDOW,
		HideWindow:    true,
	}

	// best-effort 重定向 helper stderr 到升级日志文件。跨午夜极罕见情况下工厂与 spawn
	// 各自解析日期可能产生两个日志文件，可接受。
	var logFile *os.File
	if logDir != "" {
		if f, _, err := OpenUpdateLogFile(logDir, time.Now()); err == nil {
			logFile = f
			cmd.Stderr = f
		}
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return err
	}
	// Start 成功后关闭本次 spawn 复制的句柄（子进程已继承副本；父进程的 LogSink 不在此关闭）。
	if logFile != nil {
		_ = logFile.Close()
	}
	// 放弃 wait 权：helper 由系统收养，独立运行至父进程退出后完成替换。
	_ = cmd.Process.Release()
	return nil
}
