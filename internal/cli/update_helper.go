package cli

import (
	"errors"
	"fmt"

	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/YuLaiZ/token-usage/internal/update"
	"github.com/spf13/cobra"
)

// update_helper.go 定义 Windows staged replacement 的两个隐藏内部命令：
//
//   - _update-helper：由 Windows Installer.Install spawn 的后台 helper 进程入口。
//     在父进程（token-usage update）退出后完成 MoveFileEx 替换与 daemon 重启。
//   - _update-cleanup：由 helper 成功替换后 spawn，等待 helper 退出后清理临时文件
//     （helper.exe / plan / stage / backup）。
//
// 两者均为 Cobra Hidden（不出现在 README / help），缺失或不可信计划时直接失败。
// 命令解析平台无关；实际执行委托给平台专属函数（Windows 真实实现 / 非 Windows 拒绝）。

// errHelperNotSupported 报告 helper 命令仅在 Windows 受支持。
// 在平台无关文件中定义，供测试在所有平台引用 errors.Is 断言。
var errHelperNotSupported = errors.New(ui.Bi("self-update helper commands are only supported on Windows", "自更新 helper 命令仅在 Windows 平台受支持"))

// cleanupHelperIdentity 校验 _update-cleanup 收到的 helper 显式身份。
// cleanup 必须等待由 helper 传入的精确进程实例退出；缺失、负值或超出 Windows PID
// 范围的参数一律拒绝，不能降级为“不等待直接清理”。
func cleanupHelperIdentity(helperPID int, helperCreationTime uint64) (update.ProcessIdentity, error) {
	if helperPID <= 0 || uint64(helperPID) > uint64(^uint32(0)) {
		return update.ProcessIdentity{}, fmt.Errorf("cleanup %s: %d", ui.Bi("is missing a valid helper PID", "缺少合法 helper PID"), helperPID)
	}
	identity := update.ProcessIdentity{
		PID:          uint32(helperPID),
		CreationTime: helperCreationTime,
	}
	if !identity.Valid() {
		return update.ProcessIdentity{}, fmt.Errorf("cleanup %s（PID=%d CreationTime=%d）", ui.Bi("is missing a valid helper identity", "缺少合法 helper 身份"), helperPID, helperCreationTime)
	}
	return identity, nil
}

// newUpdateHelperCmd 构造隐藏的 _update-helper 内部命令。
func newUpdateHelperCmd() *cobra.Command {
	var planPath string
	cmd := &cobra.Command{
		Use:    "_update-helper",
		Short:  ui.Bi("Internal command (Windows self-update background helper; do not invoke directly)", "内部命令（Windows 自更新后台 helper，不直接调用）"),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if planPath == "" {
				return fmt.Errorf("%s", ui.Bi("missing --plan argument", "缺少 --plan 参数"))
			}
			return runUpdateHelperCmd(cmd.Context(), planPath)
		},
	}
	cmd.Flags().StringVar(&planPath, "plan", "", ui.Bi("helper plan file path", "helper 计划文件路径"))
	return cmd
}

// newUpdateCleanupCmd 构造隐藏的 _update-cleanup 内部命令。
func newUpdateCleanupCmd() *cobra.Command {
	var planPath string
	var helperPID int
	var helperCreationTime uint64
	cmd := &cobra.Command{
		Use:    "_update-cleanup",
		Short:  ui.Bi("Internal command (Windows self-update temp file cleanup; do not invoke directly)", "内部命令（Windows 自更新临时文件清理，不直接调用）"),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if planPath == "" {
				return fmt.Errorf("%s", ui.Bi("missing --plan argument", "缺少 --plan 参数"))
			}
			return runUpdateCleanupCmd(cmd.Context(), planPath, helperPID, helperCreationTime)
		},
	}
	cmd.Flags().StringVar(&planPath, "plan", "", ui.Bi("helper plan file path", "helper 计划文件路径"))
	cmd.Flags().IntVar(&helperPID, "helper-pid", 0, ui.Bi("PID of the helper process to clean up", "待清理的 helper 进程 PID"))
	cmd.Flags().Uint64Var(&helperCreationTime, "helper-creation-time", 0, ui.Bi("creation time of the helper process to clean up (raw FILETIME, paired with --helper-pid for identity verification)", "待清理的 helper 进程创建时间（FILETIME 原始值，与 --helper-pid 配套用于身份校验）"))
	return cmd
}
