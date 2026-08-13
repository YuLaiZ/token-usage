package update

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

// install_common.go 实现 POSIX 事务性安装的平台无关部分：事务记录结构、
// 精确临时文件命名、journal 序列化与中断恢复检测。
//
// 事务模型（与 fileutil.ReplaceCompleteFile 独立，不复用其实现）：
//   - 所有事务文件都是 target 同目录的子项，basename 以 target 的 update 前缀开头；
//   - stage/backup/journal 三者通过同一个 nonce 绑定，便于恢复时配对；
//   - journal 记录 prepared/installed/started 三阶段，仅存 basename（不存完整路径，
//     避免嵌入任意外部路径），权限收紧为 0600。
//
// 事务文件命名约定（target basename 记为 base，nonce 为随机 hex）：
//
//	stage   = "." + base + ".update-stage-"   + nonce
//	backup  = "." + base + ".update-backup-"  + nonce
//	journal = "." + base + ".update-journal-" + nonce
//
// 这套前缀与 fileutil 的 ".tmp-" 前缀完全独立，便于互不干扰的精确清理。

// updateFileSuffixes 是三类事务文件的 basename 后缀（接在 "." + base 之后，再加 nonce）。
// 与 fileutil.TempPrefix（".tmp-"）完全独立，互不匹配。
const (
	updateStageSuffix   = ".update-stage-"   // stage 后缀：已验证的新版本二进制
	updateBackupSuffix  = ".update-backup-"  // backup 后缀：旧 target 的备份
	updateJournalSuffix = ".update-journal-" // journal 后缀：事务记录
)

// journalPhase 标识事务推进的阶段，写入 journal 的 phase 字段。
//
//	prepared：已创建 backup + stage 到位，尚未 rename；
//	installed：已原子 rename stage → target，新版本已落地；
//	started  ：新 daemon 已成功启动（事务完成，可清理）。
type journalPhase string

const (
	phasePrepared  journalPhase = "prepared"
	phaseInstalled journalPhase = "installed"
	phaseStarted   journalPhase = "started"
)

// journalRecord 是事务记录的磁盘序列化结构。
// 仅存 basename（不存完整路径），避免 journal 嵌入任意外部路径；
// nonce 把 stage/backup/journal 三者绑定，恢复时据此配对。
type journalRecord struct {
	Nonce          string       `json:"nonce"`           // 绑定 stage/backup/journal 的随机 hex
	Phase          journalPhase `json:"phase"`           // 事务当前阶段
	TargetBasename string       `json:"target_basename"` // target 的 basename（被替换的二进制名）
	StageBasename  string       `json:"stage_basename"`  // stage 文件的 basename
	BackupBasename string       `json:"backup_basename"` // backup 文件的 basename
	OldSHA256      string       `json:"old_sha256"`      // 旧 target 的 SHA256（64 位小写 hex）
	NewSHA256      string       `json:"new_sha256"`      // 新 stage/target 的 SHA256（64 位小写 hex）
	WasRunning     bool         `json:"was_running"`     // 原 daemon 是否在运行（决定恢复后是否重启）
}

// RecoveryState 描述遗留 journal 检测后识别出的事务状态。
type RecoveryState string

const (
	// RecoveryStateClean 无遗留 journal，正常流程。
	RecoveryStateClean RecoveryState = "clean"
	// RecoveryStateNewInstalled target 已是新版本（状态 1），按 wasRunning 恢复 daemon。
	RecoveryStateNewInstalled RecoveryState = "new_installed"
	// RecoveryStateOldIntact target 仍是旧版本（状态 2），事务文件已清理。
	RecoveryStateOldIntact RecoveryState = "old_intact"
	// RecoveryStateOldRestored target 缺失已从 backup 恢复为旧版本（状态 3）。
	RecoveryStateOldRestored RecoveryState = "old_restored"
	// RecoveryStateCleanupPending 状态已恢复但事务文件清理失败，需人工清理。
	RecoveryStateCleanupPending RecoveryState = "cleanup_pending"
	// RecoveryStateManual 状态无法识别（模糊 journal 或不匹配任何已知情况），
	// 保留全部文件要求人工处理，绝不删除。
	RecoveryStateManual RecoveryState = "manual"
)

// RecoveryOutcome 是 RecoverJournal 的返回结果，描述恢复后的事务状态与后续动作。
type RecoveryOutcome struct {
	// State 恢复后的事务状态。
	State RecoveryState
	// WasRunning journal 记录的原 daemon 运行态，调用方据此决定是否重启 daemon。
	WasRunning bool
	// NewBinPath 恢复后应启动的二进制路径。只要 RestartDaemon 为 true，调用方
	// 就使用该路径恢复原先运行的 daemon。
	NewBinPath string
	// RestartDaemon 表示恢复已使原先停止的 daemon 回到可启动状态，调用方应按
	// WasRunning 恢复它。状态 1/3 始终为 true；状态 2 仅在 journal 记录原
	// daemon 运行、因而已在 Install 前被 Stop 时为 true。
	RestartDaemon bool
}

// JournalRecoverer 抽象「在 apply 开始时检测并处理遗留 journal」的能力。
// 生产实现是 POSIX posixInstaller.RecoverJournal（Windows 由专属 helper 处理）。
// installUnderLock 在 Inspect 之前调用本接口（若有），清理上次中断的事务残留。
//
// 该接口独立于 Installer，因为不是所有 Installer 都支持 journal 恢复
// （如 Windows helper 的恢复语义不同，或测试 fakeInstaller 不做恢复）。
type JournalRecoverer interface {
	// RecoverJournal 检测 target 同目录的遗留 journal 并按可恢复状态处理。
	// 返回 RecoveryOutcome 描述后续动作；State=Manual 时保留文件要求人工处理。
	RecoverJournal(target string) (RecoveryOutcome, error)
}

// TransactionHandler 抽象「Install 成功后的事务收尾」：
// 调用方（installUnderLock）在 daemon Start 成功后调 Commit 清理事务文件，
// 在 Start 失败时调 Rollback 恢复旧版本。
//
// 生产实现是 POSIX posixInstaller（持有 lastTxn 状态）。未实现本接口的 Installer
// （如测试 fakeInstaller）由 installUnderLock 跳过收尾，保持原有重启回滚语义。
type TransactionHandler interface {
	// Commit 在 daemon Start 成功后清理事务文件（backup/journal）。
	// 清理失败不回滚已成功的新版本，返回「清理待处理」错误。
	Commit() error
	// Rollback 在 daemon Start 失败时恢复 backup → target（回滚到旧版本）。
	Rollback() error
}

// generateNonce 生成 16 字节随机 hex（32 字符），用于绑定一次事务的三类文件。
// 生成失败（极少见）直接返回 error——事务不能在无 nonce 下进行（恢复无法配对）。
func generateNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成事务 nonce 失败: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// updateTempPrefix 返回 target 的事务文件 basename 前缀（含点号）。
// 形如 ".token-usage"，后续接 updateStageSuffix/updateBackupSuffix/updateJournalSuffix + nonce。
// 与 fileutil.TempPrefix（".token-usage.tmp-"）完全独立，互不匹配。
func updateTempPrefix(target string) string {
	return "." + filepath.Base(target)
}

// stageFilePath 返回 target 同目录下的 stage 文件完整路径。
func stageFilePath(target, nonce string) string {
	return filepath.Join(filepath.Dir(target), updateTempPrefix(target)+updateStageSuffix+nonce)
}

// backupFilePath 返回 target 同目录下的 backup 文件完整路径。
func backupFilePath(target, nonce string) string {
	return filepath.Join(filepath.Dir(target), updateTempPrefix(target)+updateBackupSuffix+nonce)
}

// journalFilePath 返回 target 同目录下的 journal 文件完整路径。
func journalFilePath(target, nonce string) string {
	return filepath.Join(filepath.Dir(target), updateTempPrefix(target)+updateJournalSuffix+nonce)
}

// writeJournal 把 rec 以 JSON 写入 path，权限 0600，原子替换（经 fileutil）。
// 失败返回 error，调用方据此走恢复分支。
func writeJournal(path string, rec journalRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 journal 失败: %w", err)
	}
	// journal 用 fileutil 原子写（0600 权限），保证 journal 自身的完整替换语义。
	// 这里复用 fileutil 是合理的：journal 不需要 backup/rollback 语义，
	// 只需「写完整 bytes 后原子替换」。事务语义（backup/rename/rollback）由本套实现独立承担。
	return writeJournalFile(path, data)
}

// readJournal 读取并解析 path 处的 journal 记录。
// 文件不存在返回 (zero, false, nil)——表示无遗留 journal，正常流程。
// 文件存在但解析失败返回 (zero, false, err)——调用方应走「模糊 journal」分支（不删除）。
func readJournal(path string) (journalRecord, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return journalRecord{}, false, nil
		}
		return journalRecord{}, false, fmt.Errorf("读取 journal 失败: %w", err)
	}
	var rec journalRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return journalRecord{}, false, fmt.Errorf("解析 journal 失败: %w", err)
	}
	if rec.Nonce == "" || rec.TargetBasename == "" {
		return journalRecord{}, false, fmt.Errorf("journal 缺少必填字段 nonce/target_basename")
	}
	return rec, true, nil
}

// updateJournalPhase 把 path 处的 journal 更新到新阶段（原子重写整份文件）。
// 用于事务推进时更新 phase。失败返回 error，调用方据此走恢复分支。
func updateJournalPhase(path string, phase journalPhase) error {
	rec, ok, err := readJournal(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("journal 不存在，无法更新阶段: %s", path)
	}
	rec.Phase = phase
	return writeJournal(path, rec)
}

// fileSHA256 计算文件内容的 SHA256，返回 64 位小写 hex。
// 用于事务前校验旧 target hash、事务后校验新 target hash，以及恢复时比对状态。
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取文件计算 SHA256 失败 %s: %w", path, err)
	}
	return sha256HexBytes(data), nil
}

// findLeftoverJournal 扫描 target 同目录，返回任意一个匹配 update journal 前缀的
// 普通文件的完整路径。无遗留返回 ("", false, nil)。
//
// 恢复检测的关键入口：apply 开始时调用本函数，若发现遗留 journal 则走恢复流程。
// 多个 journal 并存（理论上不会发生，因 control lock 串行化）时返回第一个匹配项。
// 只匹配普通文件，跳过目录与 symlink（避免误读 symlink target）。
func findLeftoverJournal(target string) (string, bool, error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	prefix := "." + base + updateJournalSuffix

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("扫描遗留 journal 失败: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// 跳过目录与 symlink：只认普通文件，杜绝跟随 symlink target。
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		return filepath.Join(dir, name), true, nil
	}
	return "", false, nil
}

// cleanupTransactionFiles 删除指定的一组事务文件（stage/backup/journal）。
// 严格校验：只删普通文件，跳过目录/symlink/不存在；使用精确路径而非前缀扫描。
//
// 清理失败不回滚已成功的新版本——调用方据此返回「更新完成、清理待处理」状态。
// 返回 nil 表示全部清理成功；返回聚合 error 表示部分清理失败（含具体路径）。
func cleanupTransactionFiles(stagePath, backupPath, journalPath string) error {
	var cleanupErr error
	for _, p := range []string{stagePath, backupPath, journalPath} {
		if p == "" {
			continue
		}
		if err := removeRegularFile(p); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理事务文件失败 %s: %w", p, err))
		}
	}
	return cleanupErr
}

// removeRegularFile 删除一个普通文件：校验其为普通文件（非目录/symlink）后删除。
// 不存在视为成功（幂等）。目录/symlink 拒绝删除（防止误删目录或跟随 symlink target）。
func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("拒绝删除非普通文件 %s（mode %s）", path, info.Mode())
	}
	return os.Remove(path)
}

// cleanupUpdateTempByPrefix 删除 dir 下与 target basename 派生的三类 update 前缀
// 匹配的普通文件（精确前缀，非模糊匹配）。
//
// 与 fileutil.CleanupKnownTempFiles 语义对齐但前缀独立：只删普通文件，跳过目录/symlink，
// 忽略文件不存在。用于恢复流程的「丢弃 stage/backup/journal」场景。
func cleanupUpdateTempByPrefix(dir string, prefixes []string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("事务清理目录不能为空")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取事务清理目录失败 %q: %w", dir, err)
	}
	var cleanupErr error
	for _, e := range entries {
		name := e.Name()
		if !matchesUpdatePrefix(name, prefixes) {
			continue
		}
		// 跳过目录与 symlink：只删普通文件。
		info, err := e.Info()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stat %q: %w", filepath.Join(dir, name), err))
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %q: %w", path, err))
			}
		}
	}
	return cleanupErr
}

// matchesUpdatePrefix 报告 name 是否以 prefixes 中任一项为精确 basename 前缀。
// 空前缀视为无效（非通配），避免误删全部文件。
func matchesUpdatePrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// updateTempPrefixesFor 返回 target 派生的三类事务文件 basename 前缀（含点号开头）。
// 供 cleanupUpdateTempByPrefix 使用，覆盖 stage/backup/journal 三类。
func updateTempPrefixesFor(target string) []string {
	base := updateTempPrefix(target)
	return []string{
		base + updateStageSuffix,
		base + updateBackupSuffix,
		base + updateJournalSuffix,
	}
}

// sha256HexBytes 计算数据的 SHA256，返回 64 位小写 hex。
// 供 install_common.go / install_unix.go 的文件 hash 校验使用。
func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeJournalFile 把 data 以 0600 权限原子写入 path（经 fileutil 完整替换）。
// journal 不需要 backup/rollback 语义，只需完整 bytes + 原子替换，故复用 fileutil。
func writeJournalFile(path string, data []byte) error {
	return fileutil.ReplaceCompleteFile(path, data, 0o600)
}

// copyFile 把 src 复制到 dst，设置 mode，并 fsync dst。
// 创建 dst 时用 mode 限制权限，写完后 fsync 确保落盘。
// 本函数是平台无关的通用文件复制（POSIX 安装器与 Windows helper 共用）。
func copyFile(src, dst string, mode fs.FileMode) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败 %s: %w", src, err)
	}
	defer srcF.Close()

	dstF, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("创建目标文件失败 %s: %w", dst, err)
	}
	// 用 defer 保证关闭，但需捕获 close 错误（写入未确认）。
	writeErr := func() error {
		defer dstF.Close()
		if _, err := io.Copy(dstF, srcF); err != nil {
			return fmt.Errorf("复制内容失败: %w", err)
		}
		if err := dstF.Sync(); err != nil {
			return fmt.Errorf("fsync 目标文件失败: %w", err)
		}
		return nil
	}()
	if writeErr != nil {
		// 复制/fsync 失败：清理半成品 dst。
		_ = os.Remove(dst)
		return writeErr
	}
	// OpenFile 已按 mode 创建，但 umask 可能影响；显式 chmod 确保权限精确。
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("chmod 目标文件失败: %w", err)
	}
	return nil
}

// copyFileWithMode 把 src 复制到 dst，保留 src 的权限位。
// 复制后 fsync dst 保证落盘。平台无关：POSIX backup 回退与 Windows helper backup 共用。
func copyFileWithMode(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("读取源文件元信息失败: %w", err)
	}
	return copyFile(src, dst, srcInfo.Mode())
}

// verifyFileHash 校验 path 的 SHA256 等于 expectedHex。
// 用于 backup 校验、stage 校验与替换后 target 校验，确保复制/移动后内容一致。
func verifyFileHash(path, expectedHex string) error {
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if got != expectedHex {
		return fmt.Errorf("文件 hash 校验失败 %s: got %s want %s", path, got, expectedHex)
	}
	return nil
}

// StepLogWriter 由需要接收步骤日志 writer 的安装器实现（如 POSIX 安装器输出 [install] 行）。
// CLI 工厂经类型断言注入，使平台无关的工厂代码无需知道安装器具体类型。
type StepLogWriter interface {
	SetLogWriter(w io.Writer)
}

// HelperLogDirSetter 由需要日志目录的安装器实现（如 Windows 安装器重定向 helper stderr）。
// CLI 工厂经类型断言注入，使平台无关的工厂代码无需知道安装器具体类型。
type HelperLogDirSetter interface {
	SetLogDir(dir string)
}

// consumePendingResult 扫描 target 所在目录的上次 helper result 文件，读取并记录到日志，
// 然后删除（一次性消费）。仅在 VerifyProvenance 可信后调用——不可信来源不被触碰。
//
// result 文件命名：.<base>.update-result-<nonce>.json（仅 Windows helper 写入，
// POSIX 无命中自动 no-op）。复用 helperResult 解析：
//   - Success=true → 日志记"上次升级成功"；
//   - Success=false → 日志记 Error / Rollback；
//   - 解析失败也删（防止损坏 result 永久残留）。
//
// LogSink（经 stepLogger 的 writer）为 nil 时仅删不记。consume 是 best-effort 的：
// 目录不存在、扫描失败均静默跳过。生产 result 经原子替换写入，consume 不会读到半份 JSON。
func consumePendingResult(target string, l *stepLogger) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	resultPrefix := "." + base + resultSuffix

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, resultPrefix) || !strings.HasSuffix(name, resultExt) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		data, rerr := os.ReadFile(path)
		if rerr == nil {
			var res helperResult
			if jerr := json.Unmarshal(data, &res); jerr == nil {
				if res.Success {
					l.step("consumed previous result: success")
				} else {
					detail := res.Error
					if res.Rollback != "" {
						detail = fmt.Sprintf("%s (rollback=%s)", res.Error, res.Rollback)
					}
					l.step("consumed previous result: failed: %s", detail)
				}
			} else {
				l.step("consumed previous result: parse error, deleted")
			}
		}
		// 无论读取/解析是否成功，都删除（一次性消费），防止残留。
		_ = removeRegularFile(path)
	}
}

// SweepStaleTempFiles 清理 target 同目录下的 POSIX nonce 事务残留（stage/backup/journal）。
// 仅按精确前缀匹配普通文件，跳过目录/symlink（继承 cleanupUpdateTempByPrefix 语义）。
// best-effort：扫描/删除失败不阻塞升级。
//
// 仅在 POSIX 平台调用（goos != windows）。Windows 放弃跨事务 sweep（control lock 不足以
// 串行化 helper/cleanup，见方案设计 §2.3），故 Windows 事务文件不会被误删。
func SweepStaleTempFiles(target string) error {
	return cleanupUpdateTempByPrefix(filepath.Dir(target), updateTempPrefixesFor(target))
}

// validateInstallInputs 校验 Install 入参：stagePath 与 targetBinPath 非空、为绝对路径，
// 且都是普通文件（非 symlink / 目录）。平台无关：POSIX 与 Windows 安装器共用。
func validateInstallInputs(stagePath, targetBinPath string) error {
	if stagePath == "" {
		return errors.New("stagePath 不能为空")
	}
	if targetBinPath == "" {
		return errors.New("targetBinPath 不能为空")
	}
	if !filepath.IsAbs(stagePath) {
		return fmt.Errorf("stagePath 必须为绝对路径，当前 %q", stagePath)
	}
	if !filepath.IsAbs(targetBinPath) {
		return fmt.Errorf("targetBinPath 必须为绝对路径，当前 %q", targetBinPath)
	}
	stageInfo, err := os.Lstat(stagePath)
	if err != nil {
		return fmt.Errorf("stage 文件不可读 %s: %w", stagePath, err)
	}
	if !stageInfo.Mode().IsRegular() {
		return fmt.Errorf("stage 文件不是普通文件 %s（mode %s）", stagePath, stageInfo.Mode())
	}
	targetInfo, err := os.Lstat(targetBinPath)
	if err != nil {
		return fmt.Errorf("target 文件不可读 %s: %w", targetBinPath, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("target 文件不是普通文件 %s（mode %s）", targetBinPath, targetInfo.Mode())
	}
	return nil
}
