package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// helper_runner.go 实现 Windows staged replacement 的后台 helper 编排逻辑。
//
// helper 在父进程（token-usage update）退出后被拉起，负责：
//  1. 严格校验计划（validateHelperPlan）——拒绝路径注入、symlink、nonce 不匹配；
//  2. 等待父进程退出（释放对旧 .exe 的句柄）；
//  3. 在 control lock 内：确认 daemon 未在停止后意外运行 → 备份旧 target →
//     MoveFileEx(stage→target) → 校验新 hash → 按 wasRunning 重启 daemon；
//  4. 把执行结果写入 result 文件，供下次完整 update（Apply）在来源校验通过后消费；
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
	helperLog    *stepLogger          // [helper] 步骤日志（从 logWriter 构造，nil=静默）
}

// NewHelperRunner 构造后台 helper 编排器。前五个依赖必须非空（否则返回装配错误）；
// logWriter 可为 nil（静默），生产由 CLI 注入 os.Stderr（父进程 spawn 时重定向到日志文件）。
func NewHelperRunner(
	parentWaiter ParentWaiter,
	fileMover FileMover,
	resultWriter ResultWriter,
	controlMgr ControlManager,
	configLoader control.ConfigLoader,
	logWriter io.Writer,
) (*helperRunner, error) {
	if parentWaiter == nil || fileMover == nil || resultWriter == nil || controlMgr == nil || configLoader == nil {
		return nil, errors.New(ui.Bi("all helperRunner dependencies must not be nil", "helperRunner 所有依赖不能为空"))
	}
	return &helperRunner{
		parentWaiter: parentWaiter,
		fileMover:    fileMover,
		resultWriter: resultWriter,
		controlMgr:   controlMgr,
		configLoader: configLoader,
		helperLog:    newStepLogger(logWriter, "helper", nil),
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
		return fmt.Errorf("%s: %w", ui.Bi("helper plan validation failed", "helper 计划校验失败"), verr)
	}
	return r.execute(ctx, validated)
}

// execute 在计划校验通过后执行：等待父进程 → 加载配置 → control lock 内替换。
func (r *helperRunner) execute(ctx context.Context, validated validatedHelperPlan) error {
	plan, paths := validated.Plan, validated.Paths
	r.helperLog.step("started (nonce=%s)", plan.Nonce)

	// 2. 等待父进程退出（据 plan 中的显式身份，杜绝 PID 复用 TOCTOU）。
	if err := r.parentWaiter.WaitParentExit(ctx, plan.Parent); err != nil {
		r.fail(paths.Result, fmt.Errorf("%s: %w", ui.Bi("failed to wait for parent process", "等待父进程失败"), err), "")
		return err
	}
	r.helperLog.step("parent exited")

	// 3. 加载有效配置（control lock 内复用）。
	cfg, err := r.configLoader()
	if err != nil {
		r.fail(paths.Result, fmt.Errorf("%s: %w", ui.Bi("failed to load effective config", "加载有效配置失败"), err), "")
		return err
	}
	if cfg == nil {
		nilCfgErr := errors.New(ui.Bi("loading effective config returned nil", "加载有效配置返回 nil"))
		r.fail(paths.Result, nilCfgErr, "")
		return nilCfgErr
	}

	// 4. control lock 内完成 daemon 检查 + 替换 + 重启。
	// WithLock 在锁获取失败（超时/context 取消）时不执行回调——此时 executeUnderLock 不会被
	// 调用，fail/succeed 也不会写 result。用 lockEntered 跟踪回调是否已进入：未进入时补写
	// 失败 result，使下次 update 能消费该失败（父命令已报告 Deferred，用户以为升级在进行中）；
	// 已进入时 executeUnderLock 内部已通过 fail/succeed 写了 result，不覆盖。
	lockEntered := false
	lockErr := r.controlMgr.WithLock(ctx, func(sess ControlSession) error {
		lockEntered = true
		return r.executeUnderLock(ctx, sess, cfg, plan, paths)
	})
	if lockErr != nil && !lockEntered {
		r.fail(paths.Result, fmt.Errorf("%s: %w", ui.Bi("failed to acquire control lock", "获取 control lock 失败"), lockErr), "")
		return lockErr
	}
	return lockErr
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
		inspectErr := fmt.Errorf("%s: %w", ui.Bi("Inspect failed under lock", "锁内 Inspect 失败"), ierr)
		r.fail(paths.Result, inspectErr, "")
		return inspectErr
	}
	if st.Running {
		// daemon 在停止后意外重启：为安全起见放弃替换（避免与运行中的 daemon 冲突）。
		runningErr := errors.New(ui.Bi("daemon is unexpectedly running after stop; aborting replacement", "daemon 在停止后意外运行，放弃替换"))
		r.fail(paths.Result, runningErr, "")
		return errors.New(ui.Bi("daemon is running; aborting replacement", "daemon 运行中，放弃替换"))
	}
	r.helperLog.step("daemon wasRunning=%v", plan.WasRunning)

	// b. 备份旧 target → backup，校验旧 hash。
	if err := backupForHelper(paths.Target, paths.Backup, plan.OldSHA256); err != nil {
		backupErr := fmt.Errorf("%s: %w", ui.Bi("failed to back up old target", "备份旧 target 失败"), err)
		r.fail(paths.Result, backupErr, "")
		return backupErr
	}
	r.helperLog.step("backup OK")

	// c. MoveFileEx(stage → target)。
	if err := r.fileMover.MoveReplace(paths.Stage, paths.Target); err != nil {
		// 移动失败：target 仍是旧版本（MoveFileEx 原子，未成功则不变），回滚 backup 覆盖。
		rbErr := rollbackForHelper(paths.Target, paths.Backup, plan.OldSHA256)
		r.fail(paths.Result, fmt.Errorf("%s: %w", ui.Bi("MoveFileEx replacement failed", "MoveFileEx 替换失败"), err), errToString(rbErr))
		return fmt.Errorf("%s: %w", ui.Bi("MoveFileEx replacement failed (rolled back)", "MoveFileEx 替换失败（已回滚）"), errors.Join(err, rbErr))
	}
	r.helperLog.step("MoveFileEx OK")

	// 校验新 target hash（防御移动过程中损坏）。
	if err := verifyFileHash(paths.Target, plan.NewSHA256); err != nil {
		rbErr := rollbackForHelper(paths.Target, paths.Backup, plan.OldSHA256)
		r.fail(paths.Result, fmt.Errorf("%s: %w", ui.Bi("new target hash verification failed after replacement", "替换后新 target 校验失败"), err), errToString(rbErr))
		return fmt.Errorf("%s: %w", ui.Bi("new target hash verification failed after replacement (rolled back)", "替换后新 target 校验失败（已回滚）"), errors.Join(err, rbErr))
	}
	r.helperLog.step("hash verified")

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
				fmt.Errorf("%s: %w", ui.Bi("failed to start new daemon", "启动新 daemon 失败"), serr),
				fmt.Sprintf("rollback=%v restart=%v", rbErr, restartErr))
			return fmt.Errorf("%s: %w", ui.Bi("failed to start new daemon (rolled back and restarted)", "启动新 daemon 失败（已回滚重启）"), errors.Join(serr, rbErr, restartErr))
		}
		r.helperLog.step("daemon restarted")
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
		return fmt.Errorf("%s: %w", ui.Bi("target hash before backup does not match the expectation", "备份前 target hash 与预期不一致"), err)
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
		return fmt.Errorf("%s: %w", ui.Bi("backup verification failed before rollback", "回滚前 backup 校验失败"), err)
	}
	if err := copyFileWithMode(backup, target); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to overwrite target during rollback", "回滚覆盖 target 失败"), err)
	}
	return verifyFileHash(target, expectedOldHash)
}

// succeed 写成功 result，并据写入结果记终态日志（result=ok 或 result write failed）。
func (r *helperRunner) succeed(resultPath string) {
	if err := r.writeResult(resultPath, helperResult{Success: true}); err != nil {
		r.helperLog.step("result write failed: %v", err)
	} else {
		r.helperLog.step("result=ok")
	}
}

// fail 写失败 result（error + 可选 rollback 信息），并据写入结果记终态日志。
// 注意：result=ok 表示 result 文件写入成功（终态日志三态：ok/failure recorded/write failed），
// fail 路径用 result=failure recorded 区分于 succeed 路径的 result=ok。
func (r *helperRunner) fail(resultPath string, cause error, rollback string) {
	if err := r.writeResult(resultPath, helperResult{Success: false, Error: cause.Error(), Rollback: rollback}); err != nil {
		r.helperLog.step("result write failed: %v", err)
	} else {
		r.helperLog.step("result=failure recorded")
	}
}

// writeResult 把 result JSON 序列化后写入 resultPath（0600 权限），返回写入错误。
// 序列化失败或写入失败返回 error；调用方据此记终态日志。result 写入失败不改变已完成
// 替换的主成功结果（仅记日志），下次 consume 读不到则留待人工。
func (r *helperRunner) writeResult(resultPath string, res helperResult) error {
	data, err := marshalHelperResult(res)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to marshal result", "序列化 result 失败"), err)
	}
	return r.resultWriter.WriteResult(resultPath, data, 0o600)
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
