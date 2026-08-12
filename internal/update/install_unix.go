//go:build !windows

package update

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// install_unix.go 实现 POSIX 平台的事务性安装器（darwin/linux/其它非 windows）。
//
// 事务保证：Install 把已验证 stage 原子替换到 target，全程用 backup + journal 保证可恢复：
//  1. 生成 nonce，在 target 同目录派生 stage/backup/journal 三类精确命名文件；
//  2. 备份旧 target：优先同目录 hard link；不支持时复制到 backup + Sync + 校验旧 hash；
//  3. 写 journal(prepared) 记录 nonce/三 basename/旧新 hash/原 daemon 运行态；
//  4. 同目录 rename 把 stage 原子替换为 target（禁止 delete 后 move 的空窗方案）；
//  5. 写 journal(installed)；对 target 与其目录尽可能 fsync；
//  6. 成功返回 target 路径作为 newBinPath；清理 backup/stage/journal 由调用方在
//     daemon 启动成功后触发（见 Commit/Rollback 方法）。
//
// 失败路径（任一 install 步骤失败）：
//   - 若已 rename（installed/started 阶段）：target 已是新版本，不回滚文件
//     （文件替换已成功，回滚只发生在 daemon 启动失败后由 installUnderLock 用旧路径重启）；
//   - 若未 rename（prepared 阶段）：target 仍是旧版本，删除 stage/backup/journal，
//     target 不受影响。
//
// 中断恢复（RecoverJournal）：apply 开始时检测遗留 journal，按 3 种可恢复状态处理；
// 模糊 journal（解析失败/状态不匹配）绝不删除，返回 error 要求人工处理。

// posixInstaller 是 POSIX 平台的事务性安装器，满足 Installer 接口。
// 零值不可用：NewPosixInstaller 构造生产实例（platform 取 runtime.GOOS）。
type posixInstaller struct {
	platform string // "darwin"/"linux"/其它非 windows

	// lastTxn 记录最近一次 Install 的事务状态，供 Commit/Rollback 使用。
	// Install 成功后置位，Commit/Rollback 后清空。mu 保护并发访问。
	mu      sync.Mutex
	lastTxn *posixTransaction
}

// posixTransaction 记录一次 Install 的事务状态，供调用方在 daemon Start 成功后
// Commit（清理 backup/journal）或 Start 失败时 Rollback（恢复 backup → target）。
type posixTransaction struct {
	target    string // 被替换的目标路径
	backup    string // backup 文件路径（旧版本）
	journal   string // journal 文件路径
	stage     string // 事务内部 stage 路径（rename 后已不存在，记录用于清理幂等）
	oldHash   string // 旧版本 SHA256
	newHash   string // 新版本 SHA256
	completed bool   // Commit/Rollback 是否已执行（防重复）
}

// NewPosixInstaller 构造 POSIX 平台的事务性安装器。
// platform 取 runtime.GOOS（darwin/linux 等）。供 CLI 装配层在非 windows 平台注入 Service.Installer。
func NewPosixInstaller() Installer {
	return &posixInstaller{platform: runtime.GOOS}
}

// Platform 返回安装器对应的 GOOS（"darwin"/"linux" 等）。
func (p *posixInstaller) Platform() string {
	if p == nil {
		return ""
	}
	return p.platform
}

// Install 执行 POSIX 事务性替换：把已验证 stagePath 原子替换到 targetBinPath。
//
// 流程（见文件头事务保证）。oldBinPath 供回滚诊断（通常等于 targetBinPath）。
// wasRunning 是替换前 daemon 运行态，写入 journal 供中断恢复时按原运行态重启 daemon
// （中断可能发生在 Stop 已执行、Start 未成功之间，此时 Inspect 报 not-running 会丢失原态）。
// 成功返回 targetBinPath 作为 newBinPath（POSIX rename 后路径不变）。
// 任一失败须保证 target 处于可恢复状态（旧版本或已回滚）。
//
// 关键不变量：
//   - 禁止 delete-then-move：原子 rename 是唯一的替换手段；
//   - target 始终保留为「旧版本或新版本」之一，绝不处于「缺失/半写」中间态；
//   - rename 前的任何失败 → target 不变（仍是旧版本）；
//   - rename 后的失败（fsync/journal 写入失败）→ 内部 rollback（恢复 backup → target），target 回到旧版本；
//   - Install 成功后 backup/journal 暂不删除，记录为 lastTxn 供调用方在 daemon Start
//     成功后 Commit（清理）或 Start 失败时 Rollback（恢复旧版本）。
//
// 调用方契约（installUnderLock）：
//   - Install 成功 → StartWithExecutable(newBinPath)；Start 成功 → Commit；Start 失败 → Rollback。
//   - Install 失败 → target 已是旧版本（或已内部 rollback），调用方重启 oldBinPath 即恢复旧版本。
func (p *posixInstaller) Install(ctx context.Context, stagePath, oldBinPath, targetBinPath string, wasRunning bool) (string, error) {
	// 入参校验：stagePath 必须存在且为普通文件，targetBinPath 必须存在（覆盖已有）。
	if err := validateInstallInputs(stagePath, targetBinPath); err != nil {
		return "", err
	}

	// 生成 nonce 绑定本次事务的三类文件。
	nonce, err := generateNonce()
	if err != nil {
		return "", err
	}

	// 在 target 同目录派生 stage/backup/journal 的精确路径。
	// 注意：stagePath 是外部传入的已验证 stage（DownloadAsset 产出），
	// 这里派生的 stageFile 是「事务内部命名的 stage 副本」——我们把外部 stage
	// 复制/链接到事务内部命名，再 rename 内部 stage 到 target。
	// 这样事务文件命名全部受控（同一 nonce 绑定），且不依赖外部 stage 的命名。
	stageFile := stageFilePath(targetBinPath, nonce)
	backupFile := backupFilePath(targetBinPath, nonce)
	journalFile := journalFilePath(targetBinPath, nonce)

	// 计算旧 target hash（事务前快照，用于 backup 校验与恢复判定）。
	oldHash, err := fileSHA256(targetBinPath)
	if err != nil {
		return "", fmt.Errorf("事务前校验旧 target hash 失败: %w", err)
	}

	// 计算新 stage hash（DownloadAsset 已校验过，这里重新计算用于 journal 记录
	// 与恢复时的状态判定——避免依赖外部传入的 hash）。
	newHash, err := fileSHA256(stagePath)
	if err != nil {
		return "", fmt.Errorf("事务前校验新 stage hash 失败: %w", err)
	}

	// 步骤 1：备份旧 target 到 backup（优先 hard link，否则 copy+sync+verify）。
	// 目标文件始终保留（备份用 link 或 copy，不 move 旧 target）。
	if err := backupTarget(targetBinPath, backupFile, oldHash); err != nil {
		return "", fmt.Errorf("备份旧 target 失败: %w", err)
	}

	// 步骤 2：把外部 stage 复制到事务内部命名的 stageFile（同目录，保证后续 rename 原子）。
	// 复制而非 rename 外部 stage——外部 stage 可能被调用方保留用于诊断或重试，
	// 不应在事务中破坏它。复制后校验 hash 与 newHash 一致（防止复制过程中损坏）。
	if err := copyStageWithMode(stagePath, stageFile); err != nil {
		_ = cleanupTransactionFiles("", backupFile, journalFile)
		return "", fmt.Errorf("复制 stage 到事务文件失败: %w", err)
	}
	if err := verifyFileHash(stageFile, newHash); err != nil {
		_ = cleanupTransactionFiles(stageFile, backupFile, journalFile)
		return "", fmt.Errorf("事务 stage 校验失败: %w", err)
	}

	// 步骤 3：写 journal(prepared)——记录三 basename + 旧新 hash + nonce + 原 daemon 运行态。
	// journal 写在 rename 之前，使中断后能据 journal 判断事务进度并按原运行态恢复 daemon。
	rec := journalRecord{
		Nonce:          nonce,
		Phase:          phasePrepared,
		TargetBasename: filepath.Base(targetBinPath),
		StageBasename:  filepath.Base(stageFile),
		BackupBasename: filepath.Base(backupFile),
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     wasRunning,
	}
	if err := writeJournal(journalFile, rec); err != nil {
		_ = cleanupTransactionFiles(stageFile, backupFile, journalFile)
		return "", fmt.Errorf("写 journal(prepared) 失败: %w", err)
	}

	// 步骤 4：原子 rename stageFile → targetBinPath。
	// 这是事务的提交点：rename 成功后 target 即为新版本。
	// 任何无法确认的写入错误必须走恢复分支（见下方）。
	if err := os.Rename(stageFile, targetBinPath); err != nil {
		// rename 失败：target 仍是旧版本（rename 原子，不会半完成）。
		// 清理事务文件，保留旧 target 不变。
		cleanupErr := cleanupTransactionFiles(stageFile, backupFile, journalFile)
		return "", errors.Join(
			fmt.Errorf("原子替换 stage → target 失败: %w", err),
			cleanupErr,
		)
	}
	// rename 成功后 stageFile 已不存在（rename 把它移走），无需再删 stageFile。

	// 步骤 5：对 target 与目录尽力 fsync（持久化保证）。
	// fsync 失败视为「无法确认写入」——走恢复分支：
	// 恢复 backup → target（回滚到旧版本），清理事务文件，返回 error。
	// 这样 installUnderLock 回滚时用 oldBinPath（= target，已是旧版本）重启，保持旧版本运行。
	if err := syncAfterRename(targetBinPath); err != nil {
		rollbackErr := rollbackToBackup(targetBinPath, backupFile, oldHash, journalFile)
		return "", errors.Join(
			fmt.Errorf("替换后 fsync 失败（持久化未确认）: %w", err),
			rollbackErr,
		)
	}

	// 步骤 6：写 journal(installed)——事务已提交，target 是新版本。
	// journal 更新失败属于「无法确认事务记录」，走恢复分支：
	// 恢复 backup → target（回滚），清理事务文件。target 回到旧版本。
	if err := updateJournalPhase(journalFile, phaseInstalled); err != nil {
		rollbackErr := rollbackToBackup(targetBinPath, backupFile, oldHash, journalFile)
		return "", errors.Join(
			fmt.Errorf("更新 journal(installed) 失败: %w", err),
			rollbackErr,
		)
	}

	// 步骤 7：Install 成功——target 已是新版本。backup/journal 暂不删除，
	// 记录为 lastTxn 供调用方在 daemon Start 成功后 Commit（清理）或 Start 失败时 Rollback。
	// 这满足「只有 ready 成功才提交（删除 backup/stage/journal）」的语义：
	// 文件事务已提交（rename），但事务文件的最终清理延迟到 daemon 确认运行后。
	p.mu.Lock()
	p.lastTxn = &posixTransaction{
		target:    targetBinPath,
		backup:    backupFile,
		journal:   journalFile,
		stage:     stageFile,
		oldHash:   oldHash,
		newHash:   newHash,
		completed: false,
	}
	p.mu.Unlock()

	return targetBinPath, nil
}

// Commit 在 daemon Start 成功后清理事务文件（backup + journal）。
// 清理失败不回滚已成功的新版本——返回可诊断的「更新完成、清理待处理」错误。
// 多次调用安全（completed 标志防重复清理）。
func (p *posixInstaller) Commit() error {
	p.mu.Lock()
	txn := p.lastTxn
	p.lastTxn = nil
	p.mu.Unlock()
	if txn == nil || txn.completed {
		return nil
	}
	txn.completed = true
	if err := cleanupTransactionFiles("", txn.backup, txn.journal); err != nil {
		return fmt.Errorf("更新完成，清理待处理: %w", err)
	}
	return nil
}

// Rollback 在 daemon Start 失败时恢复 backup → target（回滚到旧版本），并清理事务文件。
// 调用方随后用 target（已是旧版本）重启 daemon，保持替换前运行状态。
// 多次调用安全（completed 标志防重复回滚）。
// 无 lastTxn（Install 未成功或已 Commit/Rollback）时返回 nil（无操作）。
func (p *posixInstaller) Rollback() error {
	p.mu.Lock()
	txn := p.lastTxn
	p.lastTxn = nil
	p.mu.Unlock()
	if txn == nil || txn.completed {
		return nil
	}
	txn.completed = true
	return rollbackToBackup(txn.target, txn.backup, txn.oldHash, txn.journal)
}

// validateInstallInputs 校验 Install 入参：stagePath 存在且为普通文件，
// targetBinPath 存在且为普通文件，二者非空且为绝对路径。
// （已移至 install_common.go，供 POSIX 与 Windows 安装器共用。）

// backupTarget 把 target 备份到 backupPath。
// 优先同目录 hard link（os.Link）——零拷贝、保留权限；若 hard link 不支持
// （跨设备/文件系统不支持），回退到 copy + Sync + hash 校验。
// 目标文件始终保留（backup 是 link 或 copy，不 move target）。
func backupTarget(target, backupPath, expectedOldHash string) error {
	// 优先 hard link：同目录同卷，几乎总是支持。
	if err := os.Link(target, backupPath); err == nil {
		// hard link 成功：权限与内容自动与 target 一致，无需额外校验。
		// 但需确认 backup 的 hash 与预期一致（防止 link 到错误 inode）。
		return verifyFileHash(backupPath, expectedOldHash)
	}
	// hard link 失败（ENOTSUP/EPERM/跨设备等）→ 回退 copy + sync + verify。
	if err := copyFileWithMode(target, backupPath); err != nil {
		return fmt.Errorf("复制 backup 失败（hard link 不支持）: %w", err)
	}
	if err := verifyFileHash(backupPath, expectedOldHash); err != nil {
		return err
	}
	return nil
}

// copyStageWithMode 把 src 复制到 dst，保留 src 的权限位（含可执行位）。
// 复制后 fsync dst 保证落盘。用于把外部 stage 复制到事务内部命名的 stageFile。
func copyStageWithMode(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("读取 stage 元信息失败: %w", err)
	}
	return copyFile(src, dst, srcInfo.Mode())
}

// syncAfterRename 在 rename 后对 target 文件与所在目录尽力 fsync。
// 文件 fsync 失败视为持久化未确认（返回 error）；目录 fsync 失败降级为 nil
// （某些文件系统不支持目录 fsync，不应阻塞事务）。
func syncAfterRename(target string) error {
	// 文件 fsync：打开 target、fsync、关闭。
	f, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("fsync 前打开 target 失败: %w", err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	// 目录 fsync：尽力而为，失败不阻塞（部分文件系统不支持）。
	syncDirBestEffort(filepath.Dir(target))
	return nil
}

// syncDirBestEffort 尽力对目录 fsync，失败静默忽略。
// 某些文件系统（如 tmpfs/网络 fs）不支持目录 fsync，不应视为事务失败。
func syncDirBestEffort(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// rollbackToBackup 在 rename 后失败时把 target 恢复为旧版本。
// 流程：
//  1. 校验 backup 的 hash == expectedOldHash（确保 backup 一致）；
//  2. 用 backup 覆盖 target（copy，保留 backup 用于诊断）；
//  3. 校验恢复后的 target hash == expectedOldHash；
//  4. 清理事务文件（backup + journal；stage 已在 rename 中移走）。
//
// 失败语义：backup 校验/恢复失败属于严重情况（旧版本可能不可用），
// 返回 error 但不删除文件（保留 backup/journal 供人工恢复）。
// 调用方用 errors.Join 把主失败与 rollback 失败合并。
func rollbackToBackup(target, backupPath, expectedOldHash, journalPath string) error {
	// 校验 backup 一致性。
	if err := verifyFileHash(backupPath, expectedOldHash); err != nil {
		return fmt.Errorf("回滚前 backup 校验失败，保留文件供人工处理: %w", err)
	}
	// 用 backup 覆盖 target：copy（不 move backup，保留诊断）。
	backupInfo, err := os.Lstat(backupPath)
	if err != nil {
		return fmt.Errorf("回滚前读取 backup 元信息失败: %w", err)
	}
	if err := copyFile(backupPath, target, backupInfo.Mode()); err != nil {
		return fmt.Errorf("回滚覆盖 target 失败: %w", err)
	}
	// fsync 恢复后的 target（尽力持久化）。
	syncDirBestEffort(filepath.Dir(target))
	// 校验恢复后的 target hash。
	if err := verifyFileHash(target, expectedOldHash); err != nil {
		return fmt.Errorf("回滚后 target 校验失败，保留文件供人工处理: %w", err)
	}
	// 回滚成功：清理 backup 与 journal（target 已确认为旧版本，事务文件不再需要）。
	if err := cleanupTransactionFiles("", backupPath, journalPath); err != nil {
		return fmt.Errorf("回滚成功但清理事务文件失败（待人工清理）: %w", err)
	}
	return nil
}

// RecoverJournal 在 apply 开始时检测并处理遗留 journal。
// 若发现遗留 journal，按 3 种可恢复状态处理；模糊 journal（解析失败/
// 状态不匹配任何已知情况）绝不删除，返回 error 要求人工处理。
//
// 返回的 RecoveryOutcome 描述恢复后的后续动作（是否需要重启 daemon）。
// 调用方（installUnderLock 或 apply 入口）据此在恢复后决定 daemon 处置。
//
// 三种可恢复状态（targetDir 指 target 所在目录）：
//  1. target 已是 newHash、backup 是 oldHash → 新版本已落地，按 wasRunning 恢复 daemon；
//  2. target 仍是 oldHash、stage 尚在 → 旧版本未动，丢弃 stage/backup/journal；若
//     journal 记录原 daemon 在运行，则调用方随后用旧 target 恢复它；
//  3. target 缺失而 backup 是 oldHash → 旧 target 丢失，从 backup 恢复旧版本。
func (p *posixInstaller) RecoverJournal(target string) (RecoveryOutcome, error) {
	journalPath, found, err := findLeftoverJournal(target)
	if err != nil {
		return RecoveryOutcome{}, err
	}
	if !found {
		// 无遗留 journal：正常流程。
		return RecoveryOutcome{State: RecoveryStateClean}, nil
	}

	rec, ok, err := readJournal(journalPath)
	if err != nil || !ok {
		// journal 解析失败或字段缺失 → 模糊 journal，绝不删除，要求人工处理。
		return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
			"遗留 journal 解析失败，保留文件要求人工处理: %s（错误: %v）", journalPath, err)
	}

	targetDir := filepath.Dir(target)
	backupPath := filepath.Join(targetDir, rec.BackupBasename)
	stagePath := filepath.Join(targetDir, rec.StageBasename)

	targetHash, targetErr := fileSHA256(target)
	if targetErr != nil && !errors.Is(targetErr, fs.ErrNotExist) {
		return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
			"读取遗留事务 target hash 失败，保留文件要求人工处理: %w", targetErr)
	}
	targetExists := targetErr == nil

	// 状态 1：target 已是 newHash（新版本已落地）。
	if targetExists && targetHash == rec.NewSHA256 {
		return recoverState1NewInstalled(target, backupPath, stagePath, journalPath, rec)
	}

	// 状态 2：target 仍是 oldHash（旧版本未动）。
	if targetExists && targetHash == rec.OldSHA256 {
		return recoverState2OldIntact(target, backupPath, stagePath, journalPath, rec)
	}

	// 状态 3：target 缺失，但 backup 是 oldHash。
	if !targetExists {
		return recoverState3TargetMissing(target, backupPath, stagePath, journalPath, rec)
	}

	// 其它任何状态（target 存在但 hash 既非 old 也非 new、backup/stage 异常等）：
	// 绝不猜测删除或覆盖，保留全部文件要求人工处理。
	return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
		"遗留 journal 状态无法识别（target hash=%s，期望 old=%s new=%s），保留文件要求人工处理",
		targetHash, rec.OldSHA256, rec.NewSHA256)
}

// recoverState1NewInstalled 处理「target 已是 newHash」状态：
// 新版本已落地，按 journal 的 wasRunning 标记恢复 daemon，然后清理事务文件。
func recoverState1NewInstalled(target, backupPath, stagePath, journalPath string, rec journalRecord) (RecoveryOutcome, error) {
	// 校验 backup 是 oldHash（确保 backup 一致，可用于诊断）。
	if backupHash, berr := fileSHA256(backupPath); berr == nil && backupHash == rec.OldSHA256 {
		// backup 一致：清理 stage/backup/journal，保留新 target。
		cleanupErr := cleanupTransactionFiles(stagePath, backupPath, journalPath)
		if cleanupErr != nil {
			return RecoveryOutcome{
				State:         RecoveryStateCleanupPending,
				WasRunning:    rec.WasRunning,
				NewBinPath:    target,
				RestartDaemon: true,
			}, fmt.Errorf("新版本已落地，清理遗留事务文件失败（需人工清理）: %w", cleanupErr)
		}
		return RecoveryOutcome{
			State:         RecoveryStateNewInstalled,
			WasRunning:    rec.WasRunning,
			NewBinPath:    target,
			RestartDaemon: true,
		}, nil
	}
	// backup 缺失或 hash 不一致：无法完全确认状态，保守要求人工处理。
	return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
		"新版本已落地但 backup 不一致（hash 校验失败），保留文件要求人工处理")
}

// recoverState2OldIntact 处理「target 仍是 oldHash、stage 尚在」状态：
// 旧版本未动，事务中断在 rename 之前。若 journal 记录原 daemon 在运行，
// 它已在 Install 前被 Stop，调用方必须用旧 target 恢复运行态并结束本轮更新。
func recoverState2OldIntact(target, backupPath, stagePath, journalPath string, rec journalRecord) (RecoveryOutcome, error) {
	cleanupErr := cleanupTransactionFiles(stagePath, backupPath, journalPath)
	if cleanupErr != nil {
		return RecoveryOutcome{
			State:         RecoveryStateCleanupPending,
			WasRunning:    rec.WasRunning,
			NewBinPath:    target,
			RestartDaemon: rec.WasRunning,
		}, fmt.Errorf("旧版本完好，清理遗留事务文件失败（需人工清理）: %w", cleanupErr)
	}
	return RecoveryOutcome{
		State:         RecoveryStateOldIntact,
		WasRunning:    rec.WasRunning,
		NewBinPath:    target,
		RestartDaemon: rec.WasRunning,
	}, nil
}

// recoverState3TargetMissing 处理「target 缺失、backup 是 oldHash」状态：
// 旧 target 丢失（可能在 rename 中途异常），从 backup 恢复旧版本。
func recoverState3TargetMissing(target, backupPath, stagePath, journalPath string, rec journalRecord) (RecoveryOutcome, error) {
	backupHash, berr := fileSHA256(backupPath)
	if berr != nil || backupHash != rec.OldSHA256 {
		// backup 不一致或缺失：无法恢复，要求人工处理。
		return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
			"target 缺失且 backup 不可用（%v），保留文件要求人工处理", berr)
	}
	// 从 backup 恢复旧 target：copy backup → target（不 move backup，保留用于诊断）。
	if err := copyFileWithMode(backupPath, target); err != nil {
		return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
			"从 backup 恢复旧 target 失败，保留文件要求人工处理: %w", err)
	}
	// 校验恢复后的 target hash == oldHash。
	if err := verifyFileHash(target, rec.OldSHA256); err != nil {
		return RecoveryOutcome{State: RecoveryStateManual}, fmt.Errorf(
			"恢复后 target hash 校验失败，保留文件要求人工处理: %w", err)
	}
	// 恢复成功：清理事务文件。
	cleanupErr := cleanupTransactionFiles(stagePath, backupPath, journalPath)
	if cleanupErr != nil {
		return RecoveryOutcome{
			State:         RecoveryStateCleanupPending,
			WasRunning:    rec.WasRunning,
			NewBinPath:    target,
			RestartDaemon: true,
		}, fmt.Errorf("旧 target 已恢复，清理遗留事务文件失败（需人工清理）: %w", cleanupErr)
	}
	// 旧版本已恢复，按 wasRunning 标记调用方应重启旧 daemon。
	return RecoveryOutcome{
		State:         RecoveryStateOldRestored,
		WasRunning:    rec.WasRunning,
		NewBinPath:    target,
		RestartDaemon: true,
	}, nil
}
