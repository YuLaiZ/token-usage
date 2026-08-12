package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// helper_runner.go 实现 Windows staged replacement 的后台 helper 编排逻辑。
//
// helper 在父进程（token-usage update）退出后被拉起，负责：
//  1. 严格校验计划（validateHelperPlan）——拒绝路径注入、symlink、nonce 不匹配；
//  2. 等待父进程退出（释放对旧 .exe 的句柄）；
//  3. 在 control lock 内：确认 daemon 未在停止后意外运行 → 备份旧 target →
//     MoveFileEx(stage→target) → 校验新 hash → 按 wasRunning 重启 daemon；
//  4. 把执行结果写入 result 文件，供父进程下一次 update --check 展示；
//  5. 任一步失败：从 backup 回滚 target、必要时重启旧 daemon、写失败 result。
//
// 所有外部依赖（等待父进程、文件移动、结果写入、control lock）经字段注入，
// 使核心逻辑可在 macOS 用 fake 单元测试；生产实现（Windows API）在 build tag windows
// 文件中装配。本文件平台无关（无 build tag）。

// helperResult 是 helper 写入 result 文件的执行结果（JSON 序列化）。
type helperResult struct {
	Success  bool   `json:"success"`            // 替换 + daemon 重启是否全部成功
	Error    string `json:"error,omitempty"`    // 主失败原因
	Rollback string `json:"rollback,omitempty"` // 回滚过程的错误（若有）
}

// helperRunner 串起后台 helper 的全部步骤。零值不可用：NewHelperRunner 构造。
type helperRunner struct {
	parentWaiter ParentWaiter         // 等待父进程退出
	fileMover    FileMover            // MoveFileEx 原子替换 stage → target
	resultWriter ResultWriter         // 写 result 文件
	controlMgr   ControlManager       // control lock（daemon 检查 / 重启）
	configLoader control.ConfigLoader // 锁内加载有效配置
}

// NewHelperRunner 构造后台 helper 编排器。所有依赖必须非空（否则 Run 返回装配错误）。
func NewHelperRunner(
	parentWaiter ParentWaiter,
	fileMover FileMover,
	resultWriter ResultWriter,
	controlMgr ControlManager,
	configLoader control.ConfigLoader,
) (*helperRunner, error) {
	if parentWaiter == nil || fileMover == nil || resultWriter == nil || controlMgr == nil || configLoader == nil {
		return nil, errors.New("helperRunner 所有依赖不能为空")
	}
	return &helperRunner{
		parentWaiter: parentWaiter,
		fileMover:    fileMover,
		resultWriter: resultWriter,
		controlMgr:   controlMgr,
		configLoader: configLoader,
	}, nil
}

// Run 执行后台 helper 的完整流程。selfExe 是 helper 自身路径（用于校验），
// planPath 是计划文件路径。
//
// 计划校验失败时无法确定 result 路径，直接返回 error（不写 result）。
// 校验通过后，任一后续失败都写 result 文件并返回 error。
func (r *helperRunner) Run(ctx context.Context, selfExe, planPath string) error {
	// 1. 校验计划。
	validated, verr := validateHelperPlan(selfExe, planPath)
	if verr != nil {
		return fmt.Errorf("helper 计划校验失败: %w", verr)
	}
	return r.execute(ctx, validated)
}

// execute 在计划校验通过后执行：等待父进程 → 加载配置 → control lock 内替换。
func (r *helperRunner) execute(ctx context.Context, validated validatedHelperPlan) error {
	plan, paths := validated.Plan, validated.Paths

	// 2. 等待父进程退出（据 plan 中的显式身份，杜绝 PID 复用 TOCTOU）。
	if err := r.parentWaiter.WaitParentExit(ctx, plan.Parent); err != nil {
		r.fail(paths.Result, fmt.Errorf("等待父进程失败: %w", err), "")
		return err
	}

	// 3. 加载有效配置（control lock 内复用）。
	cfg, err := r.configLoader()
	if err != nil {
		r.fail(paths.Result, fmt.Errorf("加载有效配置失败: %w", err), "")
		return err
	}
	if cfg == nil {
		r.fail(paths.Result, errors.New("加载有效配置返回 nil"), "")
		return errors.New("加载有效配置返回 nil")
	}

	// 4. control lock 内完成 daemon 检查 + 替换 + 重启。
	return r.controlMgr.WithLock(ctx, func(sess ControlSession) error {
		return r.executeUnderLock(ctx, sess, cfg, plan, paths)
	})
}

// executeUnderLock 在 control lock 持有期内完成替换与 daemon 切换。
//
// 顺序：
//
//	a. Inspect 确认 daemon 未运行（停止后不应意外重启；运行则安全失败）；
//	b. 备份旧 target → backup，校验旧 hash；
//	c. MoveFileEx(stage → target)，校验新 hash；
//	d. wasRunning → StartWithExecutable(target) 启动新 daemon；
//	e. 成功写 result；任一失败从 backup 回滚并写失败 result。
func (r *helperRunner) executeUnderLock(ctx context.Context, sess ControlSession, cfg *config.Config, plan helperPlan, paths helperPaths) error {
	// a. 确认 daemon 未在停止后意外运行。
	st, ierr := sess.Inspect(ctx, cfg)
	if ierr != nil {
		r.fail(paths.Result, fmt.Errorf("锁内 Inspect 失败: %w", ierr), "")
		return fmt.Errorf("锁内 Inspect 失败: %w", ierr)
	}
	if st.Running {
		// daemon 在停止后意外重启：为安全起见放弃替换（避免与运行中的 daemon 冲突）。
		r.fail(paths.Result, errors.New("daemon 在停止后意外运行，放弃替换"), "")
		return errors.New("daemon 运行中，放弃替换")
	}

	// b. 备份旧 target → backup，校验旧 hash。
	if err := backupForHelper(paths.Target, paths.Backup, plan.OldSHA256); err != nil {
		r.fail(paths.Result, fmt.Errorf("备份旧 target 失败: %w", err), "")
		return fmt.Errorf("备份旧 target 失败: %w", err)
	}

	// c. MoveFileEx(stage → target)。
	if err := r.fileMover.MoveReplace(paths.Stage, paths.Target); err != nil {
		// 移动失败：target 仍是旧版本（MoveFileEx 原子，未成功则不变），回滚 backup 覆盖。
		rbErr := rollbackForHelper(paths.Target, paths.Backup, plan.OldSHA256)
		r.fail(paths.Result, fmt.Errorf("MoveFileEx 替换失败: %w", err), errToString(rbErr))
		return fmt.Errorf("MoveFileEx 替换失败（已回滚）: %w", errors.Join(err, rbErr))
	}

	// 校验新 target hash（防御移动过程中损坏）。
	if err := verifyFileHash(paths.Target, plan.NewSHA256); err != nil {
		rbErr := rollbackForHelper(paths.Target, paths.Backup, plan.OldSHA256)
		r.fail(paths.Result, fmt.Errorf("替换后新 target 校验失败: %w", err), errToString(rbErr))
		return fmt.Errorf("替换后新 target 校验失败（已回滚）: %w", errors.Join(err, rbErr))
	}

	// d. wasRunning → 启动新 daemon。
	if plan.WasRunning {
		if serr := sess.StartWithExecutable(ctx, cfg, paths.Target); serr != nil {
			// 启动失败：回滚到旧版本，再用旧 target 重启 daemon，写失败 result。
			rbErr := rollbackForHelper(paths.Target, paths.Backup, plan.OldSHA256)
			var restartErr error
			if rerr := sess.StartWithExecutable(ctx, cfg, paths.Target); rerr != nil {
				restartErr = rerr
			}
			r.fail(paths.Result,
				fmt.Errorf("启动新 daemon 失败: %w", serr),
				fmt.Sprintf("rollback=%v restart=%v", rbErr, restartErr))
			return fmt.Errorf("启动新 daemon 失败（已回滚重启）: %w", errors.Join(serr, rbErr, restartErr))
		}
	}

	// e. 成功。
	r.succeed(paths.Result)
	return nil
}

// backupForHelper 把 target 复制为 backup 并校验其 hash == expectedOldHash。
// 复制而非移动：target 始终保留，backup 作为回滚源。
func backupForHelper(target, backup, expectedOldHash string) error {
	// 先校验当前 target 确为旧版本（hash 一致），防止对非预期文件做备份。
	if err := verifyFileHash(target, expectedOldHash); err != nil {
		return fmt.Errorf("备份前 target hash 与预期不一致: %w", err)
	}
	if err := copyFileWithMode(target, backup); err != nil {
		return err
	}
	return verifyFileHash(backup, expectedOldHash)
}

// rollbackForHelper 把 backup 覆盖回 target（回滚到旧版本），并校验恢复后 hash。
// backup 保留用于诊断（不删除）。
func rollbackForHelper(target, backup, expectedOldHash string) error {
	if err := verifyFileHash(backup, expectedOldHash); err != nil {
		return fmt.Errorf("回滚前 backup 校验失败: %w", err)
	}
	if err := copyFileWithMode(backup, target); err != nil {
		return fmt.Errorf("回滚覆盖 target 失败: %w", err)
	}
	return verifyFileHash(target, expectedOldHash)
}

// succeed 写成功 result。
func (r *helperRunner) succeed(resultPath string) {
	r.writeResult(resultPath, helperResult{Success: true})
}

// fail 写失败 result（error + 可选 rollback 信息）。
func (r *helperRunner) fail(resultPath string, cause error, rollback string) {
	r.writeResult(resultPath, helperResult{Success: false, Error: cause.Error(), Rollback: rollback})
}

// writeResult 把 result JSON 序列化后写入 resultPath（0600 权限）。
func (r *helperRunner) writeResult(resultPath string, res helperResult) {
	data, err := marshalHelperResult(res)
	if err != nil {
		// 序列化失败属于编程错误；result 写不了，只能放弃（不影响主流程错误）。
		return
	}
	_ = r.resultWriter.WriteResult(resultPath, data, 0o600)
}

// marshalHelperResult 序列化 helperResult 为 JSON。
func marshalHelperResult(res helperResult) ([]byte, error) {
	return json.Marshal(res)
}

// errToString 把 error 转为字符串（nil → 空串）。
func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
