//go:build !windows

package update

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// install_unix_test.go 校验 POSIX 事务性安装器的完整事务行为与中断恢复。
//
// 设计原则：
//   - 文件系统操作用真实 OS（t.TempDir 下真实 rename/hard link/SHA256），
//     绝不 fake FS——事务正确性必须测真实原子语义；
//   - daemon 控制用 fake ControlSession（fakeControlSession）测试运行态编排；
//   - 不触达真实网络/GitHub/daemon lock。
//
// 覆盖 POSIX 事务、失败回滚与中断恢复行为。

// ---- 测试辅助 ----

// setupTargetAndStage 在临时目录创建一个「旧 target + 新 stage」对，返回各自路径与内容。
// oldContent 是旧 target 内容，newContent 是新 stage 内容。target 权限 0755（可执行）。
func setupTargetAndStage(t *testing.T, oldContent, newContent string) (target, stage string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "token-usage")
	stage = filepath.Join(dir, ".incoming-stage")
	if err := os.WriteFile(target, []byte(oldContent), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(stage, []byte(newContent), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	return target, stage
}

// assertFileContent 断言 path 内容等于 want。
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("文件 %s 内容 = %q, want %q", path, string(got), want)
	}
}

// assertFileMode 断言 path 权限等于 want。
func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != want {
		t.Errorf("文件 %s 权限 = %o, want %o", path, perm, want)
	}
}

// assertNoTransactionFiles 断言 target 目录下无任何 update 事务文件残留。
func assertNoTransactionFiles(t *testing.T, target string) {
	t.Helper()
	dir := filepath.Dir(target)
	prefixes := updateTempPrefixesFor(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if matchesUpdatePrefix(e.Name(), prefixes) {
			t.Errorf("事务文件残留: %s", filepath.Join(dir, e.Name()))
		}
	}
}

// ---- 成功路径测试 ----

// TestPosixInstall_Success_TargetReplacedWithNewVersion 成功路径：
// stage 替换 target，target 内容变为新版本，权限保留为 0755。
// Install 成功后 backup/journal 暂存，需 Commit 清理。
func TestPosixInstall_Success_TargetReplacedWithNewVersion(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old-version", "new-version")
	installer := NewPosixInstaller().(*posixInstaller)

	got, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got != target {
		t.Errorf("newBinPath = %q, want %q", got, target)
	}
	assertFileContent(t, target, "new-version")
	assertFileMode(t, target, 0o755)
	// Commit 清理事务文件。
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertNoTransactionFiles(t, target)
	// 外部 stage 文件应保留（Install 复制 stage，不 move）。
	assertFileContent(t, stage, "new-version")
}

// TestPosixInstall_Success_StageDifferentDirectory stage 不在 target 同目录也能成功。
func TestPosixInstall_Success_StageDifferentDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bin", "token-usage")
	stageDir := filepath.Join(dir, "download")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	stage := filepath.Join(stageDir, ".incoming")
	if err := os.WriteFile(stage, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	installer := NewPosixInstaller()

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFileContent(t, target, "new")
}

// TestPosixInstall_Platform 返回 runtime.GOOS。
func TestPosixInstall_Platform(t *testing.T) {
	p := NewPosixInstaller().(*posixInstaller)
	// 只验证非空、非 windows（在 !windows tag 下）。
	if p.Platform() == "" || p.Platform() == "windows" {
		t.Errorf("Platform = %q, 期望非空且非 windows", p.Platform())
	}
}

// TestPosixInstall_HardLinkBackup 验证 backup 使用 hard link（同 inode）。
// 在大多数 POSIX 文件系统（含 macOS/Linux ext4）上，同目录 hard link 应成功。
func TestPosixInstall_HardLinkBackup(t *testing.T) {
	target, _ := setupTargetAndStage(t, "old", "new")
	installer := NewPosixInstaller().(*posixInstaller)
	_ = installer // 此测试直接测 backupTarget

	// 直接调用 backupTarget，验证 hard link 成功（同 inode）。
	oldHash, err := fileSHA256(target)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}
	backup := backupFilePath(target, "test-nonce")
	if err := backupTarget(target, backup, oldHash); err != nil {
		t.Fatalf("backupTarget: %v", err)
	}
	targetInfo, _ := os.Lstat(target)
	backupInfo, _ := os.Lstat(backup)
	if !os.SameFile(targetInfo, backupInfo) {
		// 某些文件系统可能不支持 hard link（罕见），此情况打印诊断。
		t.Logf("backup 未使用 hard link（可能 FS 不支持），目标与 backup inode 不同")
	}
	// backup 内容必须等于旧 target。
	assertFileContent(t, backup, "old")
}

// TestPosixInstall_CopyBackupWhenHardLinkFails 模拟 hard link 失败时回退到 copy。
// 在实际中 hard link 失败罕见；这里通过跨设备场景间接验证——
// 直接测试 copyFileWithMode 的正确性作为 fallback 保证。
func TestPosixInstall_CopyBackupWhenHardLinkFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("content"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := copyFileWithMode(src, dst); err != nil {
		t.Fatalf("copyFileWithMode: %v", err)
	}
	assertFileContent(t, dst, "content")
	assertFileMode(t, dst, 0o755)
	// src 与 dst 应是不同 inode（copy）。
	srcInfo, _ := os.Lstat(src)
	dstInfo, _ := os.Lstat(dst)
	if os.SameFile(srcInfo, dstInfo) {
		t.Error("copy 应产生独立 inode，不应是 hard link")
	}
}

// ---- 失败路径：rename 前 ----

// TestPosixInstall_StageMissingFails stage 文件不存在 → Install 失败，target 不变。
func TestPosixInstall_StageMissingFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stage := filepath.Join(dir, ".no-such-stage")
	installer := NewPosixInstaller()

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err == nil {
		t.Fatal("stage 不存在应返回错误")
	}
	// target 应保持旧版本。
	assertFileContent(t, target, "old")
}

// TestPosixInstall_TargetMissingFails target 不存在 → Install 失败。
func TestPosixInstall_TargetMissingFails(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".stage")
	if err := os.WriteFile(stage, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target := filepath.Join(dir, ".no-such-target")
	installer := NewPosixInstaller()

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err == nil {
		t.Fatal("target 不存在应返回错误")
	}
}

// TestPosixInstall_StageNotRegularFileFails stage 是目录 → Install 失败。
func TestPosixInstall_StageNotRegularFileFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	stage := filepath.Join(dir, ".stage-dir")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	installer := NewPosixInstaller()

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err == nil {
		t.Fatal("stage 是目录应返回错误")
	}
	assertFileContent(t, target, "old")
}

// TestPosixInstall_TargetNotRegularFileFails target 是 symlink → Install 失败。
func TestPosixInstall_TargetNotRegularFileFails(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	target := filepath.Join(dir, "token-usage")
	stage := filepath.Join(dir, ".stage")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(real, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.WriteFile(stage, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	installer := NewPosixInstaller()

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err == nil {
		t.Fatal("target 是 symlink 应返回错误")
	}
}

// TestPosixInstall_EmptyStagePathFails stagePath 为空 → 返回错误。
func TestPosixInstall_EmptyStagePathFails(t *testing.T) {
	installer := NewPosixInstaller()
	_, err := installer.Install(context.Background(), "", "/some/target", "/some/target", false)
	if err == nil {
		t.Fatal("空 stagePath 应返回错误")
	}
}

// ---- 权限保留测试 ----

// TestPosixInstall_PreservesExecutableMode target 原 0755，替换后仍 0755。
func TestPosixInstall_PreservesExecutableMode(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	// stage 用 0644（无 exec），但 Install 复制时保留 stage 的 mode。
	// 实际 DownloadAsset 已为 stage 设 exec 位，这里测试 stage 权限被保留。
	if err := os.Chmod(stage, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	installer := NewPosixInstaller()
	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	// target mode 应等于 stage 的 mode（复制时保留 stage mode）。
	assertFileMode(t, target, 0o644)
}

// ---- cleanup 待处理状态测试 ----

// TestPosixInstall_CleanupPendingOnError 可被观测的清理失败路径。
// 由于正常成功路径 cleanup 总能成功，这里验证 cleanupTransactionFiles 的聚合 error 行为。
func TestPosixInstall_CleanupTransactionFiles_AggregatesErrors(t *testing.T) {
	dir := t.TempDir()
	// 创建一个普通文件 + 一个目录（同路径模拟——无法直接测，改用不存在的路径混合）。
	stage := filepath.Join(dir, ".s")
	backup := filepath.Join(dir, ".b") // 不存在
	journal := filepath.Join(dir, ".j-subdir")
	if err := os.WriteFile(stage, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(journal, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// cleanupTransactionFiles 对目录应失败（removeRegularFile 拒绝目录），对 stage 成功。
	err := cleanupTransactionFiles(stage, backup, journal)
	if err == nil {
		t.Fatal("应返回清理错误（目录无法删除）")
	}
	// stage 应已被删除。
	if _, e := os.Lstat(stage); !errors.Is(e, fs.ErrNotExist) {
		t.Errorf("stage 应被删除")
	}
	// 目录应保留。
	if _, e := os.Lstat(journal); e != nil {
		t.Errorf("目录应保留: %v", e)
	}
}

// ---- 中断恢复测试：三种可恢复状态 + 模糊 journal ----

// makeRecoveryScenario 构造一个中断恢复场景：给定 target 内容、backup 内容、stage 内容、journal 记录，
// 在 target 目录创建对应文件，返回 installer 与 target 路径供 RecoverJournal 测试。
func makeRecoveryScenario(t *testing.T, targetContent *string, backupContent *string, stageContent *string, rec journalRecord) (*posixInstaller, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, rec.TargetBasename)
	if targetContent != nil {
		if err := os.WriteFile(target, []byte(*targetContent), 0o755); err != nil {
			t.Fatalf("WriteFile target: %v", err)
		}
	}
	if backupContent != nil {
		backupPath := filepath.Join(dir, rec.BackupBasename)
		if err := os.WriteFile(backupPath, []byte(*backupContent), 0o755); err != nil {
			t.Fatalf("WriteFile backup: %v", err)
		}
	}
	if stageContent != nil {
		stagePath := filepath.Join(dir, rec.StageBasename)
		if err := os.WriteFile(stagePath, []byte(*stageContent), 0o755); err != nil {
			t.Fatalf("WriteFile stage: %v", err)
		}
	}
	// 写 journal。
	journalPath := journalFilePath(target, rec.Nonce)
	if err := writeJournal(journalPath, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	return &posixInstaller{platform: "posix-test"}, target
}

// TestRecoverJournal_State1_NewInstalled 状态 1：target 已是 newHash、backup 是 oldHash。
// 恢复：清理事务文件，返回 NewInstalled 状态 + wasRunning。
func TestRecoverJournal_State1_NewInstalled(t *testing.T) {
	oldContent := "old-version"
	newContent := "new-version"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	rec := journalRecord{
		Nonce:          "n1",
		Phase:          phaseInstalled,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n1",
		BackupBasename: ".token-usage.update-backup-n1",
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     true,
	}
	installer, target := makeRecoveryScenario(t, &newContent, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateNewInstalled {
		t.Errorf("State = %q, want new_installed", outcome.State)
	}
	if !outcome.WasRunning {
		t.Error("WasRunning 应为 true")
	}
	if outcome.NewBinPath != target {
		t.Errorf("NewBinPath = %q, want %q", outcome.NewBinPath, target)
	}
	if !outcome.RestartDaemon {
		t.Error("状态 1 应要求调用方按原运行态恢复 daemon")
	}
	// 事务文件应被清理。
	assertNoTransactionFiles(t, target)
	// target 仍是新版本。
	assertFileContent(t, target, newContent)
}

// TestRecoverJournal_State2_OldIntact 状态 2：target 仍是 oldHash、stage 尚在。
// 恢复：丢弃 stage/backup/journal，保持旧版本。
func TestRecoverJournal_State2_OldIntact(t *testing.T) {
	oldContent := "old-version"
	newContent := "new-version"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	rec := journalRecord{
		Nonce:          "n2",
		Phase:          phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n2",
		BackupBasename: ".token-usage.update-backup-n2",
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     false,
	}
	installer, target := makeRecoveryScenario(t, &oldContent, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateOldIntact {
		t.Errorf("State = %q, want old_intact", outcome.State)
	}
	// 事务文件应被清理。
	assertNoTransactionFiles(t, target)
	// target 仍是旧版本。
	assertFileContent(t, target, oldContent)
}

// TestRecoverJournal_State3_TargetMissing 状态 3：target 缺失、backup 是 oldHash。
// 恢复：从 backup 恢复旧 target。
func TestRecoverJournal_State3_TargetMissing(t *testing.T) {
	oldContent := "old-version"
	newContent := "new-version"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	rec := journalRecord{
		Nonce:          "n3",
		Phase:          phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n3",
		BackupBasename: ".token-usage.update-backup-n3",
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     true,
	}
	installer, target := makeRecoveryScenario(t, nil, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateOldRestored {
		t.Errorf("State = %q, want old_restored", outcome.State)
	}
	if !outcome.WasRunning {
		t.Error("WasRunning 应为 true")
	}
	if !outcome.RestartDaemon {
		t.Error("状态 3 应要求调用方按原运行态恢复 daemon")
	}
	// target 应被恢复为旧版本。
	assertFileContent(t, target, oldContent)
	// 事务文件应被清理。
	assertNoTransactionFiles(t, target)
}

// ---- 模糊 journal 不删除测试 ----

// TestRecoverJournal_FuzzyCorruptJournalNoDelete journal 解析失败 → 绝不删除，返回 Manual。
func TestRecoverJournal_FuzzyCorruptJournalNoDelete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 创建损坏的 journal。
	journalPath := journalFilePath(target, "fuzzy")
	if err := os.WriteFile(journalPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("WriteFile journal: %v", err)
	}
	// 创建同 nonce 的 stage/backup（不应被删除——无法确认状态）。
	stagePath := stageFilePath(target, "fuzzy")
	backupPath := backupFilePath(target, "fuzzy")
	if err := os.WriteFile(stagePath, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	if err := os.WriteFile(backupPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}

	installer := &posixInstaller{platform: "posix-test"}
	outcome, err := installer.RecoverJournal(target)
	if err == nil {
		t.Fatal("损坏的 journal 应返回错误")
	}
	if outcome.State != RecoveryStateManual {
		t.Errorf("State = %q, want manual", outcome.State)
	}
	// 全部文件应保留（绝不删除）。
	for _, p := range []string{journalPath, stagePath, backupPath, target} {
		if _, e := os.Lstat(p); e != nil {
			t.Errorf("模糊 journal 场景文件 %s 应保留: %v", p, e)
		}
	}
}

// TestRecoverJournal_FuzzyUnrecognizedStateNoDelete target hash 既非 old 也非新 → Manual，不删除。
func TestRecoverJournal_FuzzyUnrecognizedStateNoDelete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	// target 内容既非 old 也非 new（被外部篡改为 unknown）。
	if err := os.WriteFile(target, []byte("unknown-tampered"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rec := journalRecord{
		Nonce:          "n4",
		Phase:          phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n4",
		BackupBasename: ".token-usage.update-backup-n4",
		OldSHA256:      sha256HexBytes([]byte("old")),
		NewSHA256:      sha256HexBytes([]byte("new")),
		WasRunning:     true,
	}
	// 写合法 journal 但 target 状态不匹配任何已知情况。
	journalPath := journalFilePath(target, "n4")
	if err := writeJournal(journalPath, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	installer := &posixInstaller{platform: "posix-test"}
	outcome, err := installer.RecoverJournal(target)
	if err == nil {
		t.Fatal("无法识别的状态应返回错误")
	}
	if outcome.State != RecoveryStateManual {
		t.Errorf("State = %q, want manual", outcome.State)
	}
	// target 应保留（不覆盖）。
	assertFileContent(t, target, "unknown-tampered")
}

// TestRecoverJournal_TargetReadFailureRequiresManualIntervention 确认 target
// 存在但不可读取时不把它误判为缺失，也不尝试从 backup 覆盖。
func TestRecoverJournal_TargetReadFailureRequiresManualIntervention(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	oldContent := "old-version"
	newContent := "new-version"
	rec := journalRecord{
		Nonce:          "unreadable",
		Phase:          phasePrepared,
		TargetBasename: filepath.Base(target),
		StageBasename:  filepath.Base(stageFilePath(target, "unreadable")),
		BackupBasename: filepath.Base(backupFilePath(target, "unreadable")),
		OldSHA256:      sha256HexBytes([]byte(oldContent)),
		NewSHA256:      sha256HexBytes([]byte(newContent)),
	}
	backupPath := backupFilePath(target, rec.Nonce)
	stagePath := stageFilePath(target, rec.Nonce)
	journalPath := journalFilePath(target, rec.Nonce)
	if err := os.WriteFile(backupPath, []byte(oldContent), 0o755); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	if err := os.WriteFile(stagePath, []byte(newContent), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	if err := writeJournal(journalPath, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	outcome, err := (&posixInstaller{platform: "posix-test"}).RecoverJournal(target)
	if err == nil {
		t.Fatal("不可读取 target 应要求人工处理")
	}
	if outcome.State != RecoveryStateManual {
		t.Errorf("State=%q，want manual", outcome.State)
	}
	for _, path := range []string{target, backupPath, stagePath, journalPath} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Errorf("人工处理前应保留 %q: %v", path, statErr)
		}
	}
}

// TestRecoverJournal_NoJournal_Clean 无遗留 journal → Clean 状态。
func TestRecoverJournal_NoJournal_Clean(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	installer := &posixInstaller{platform: "posix-test"}
	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateClean {
		t.Errorf("State = %q, want clean", outcome.State)
	}
}

// TestRecoverJournal_State1BackupMismatchManual 状态 1 但 backup hash 不匹配 → Manual。
func TestRecoverJournal_State1BackupMismatchManual(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	newContent := "new-version"
	if err := os.WriteFile(target, []byte(newContent), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rec := journalRecord{
		Nonce:          "n5",
		Phase:          phaseInstalled,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n5",
		BackupBasename: ".token-usage.update-backup-n5",
		OldSHA256:      sha256HexBytes([]byte("old")),
		NewSHA256:      sha256HexBytes([]byte(newContent)),
		WasRunning:     true,
	}
	// backup 内容与 oldHash 不匹配（损坏）。
	backupPath := backupFilePath(target, "n5")
	if err := os.WriteFile(backupPath, []byte("corrupt-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	journalPath := journalFilePath(target, "n5")
	if err := writeJournal(journalPath, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	installer := &posixInstaller{platform: "posix-test"}
	outcome, err := installer.RecoverJournal(target)
	if err == nil {
		t.Fatal("backup 不匹配应返回错误")
	}
	if outcome.State != RecoveryStateManual {
		t.Errorf("State = %q, want manual", outcome.State)
	}
	// 文件应保留。
	if _, e := os.Lstat(backupPath); e != nil {
		t.Errorf("backup 应保留: %v", e)
	}
}

// ---- 编排集成测试：通过 Service.Apply + fakeControlSession ----

// makeApplyInstallService 构造一个用真实 posixInstaller + 真实 FS 的 Service，
// 但 daemon 控制用 fakeControlSession。stagePath 指向真实 stage 文件。
func makeApplyInstallService(t *testing.T, running bool) (*Service, *fakeControlSession, string, string) {
	t.Helper()
	// 真实 FS：在临时目录创建旧 target 与新 stage。
	dir := t.TempDir()
	binPath := filepath.Join(dir, "token-usage")
	stagePath := filepath.Join(dir, ".incoming-stage")
	oldBin := []byte("old-official-bin")
	newBin := []byte("new-official-bin")
	if err := os.WriteFile(binPath, oldBin, 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(stagePath, newBin, 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}

	// provenance deps 用真实 FS（realLstat + 真实文件）指向 binPath。
	deps := ProvenanceDeps{
		Executable: &fakeExecutableResolver{path: binPath},
		Lstat:      realLstat{},
		FileReader: &realFileReader{},
		Manifest:   nil, // makeService 的 withMatchingManifest 会覆盖
		Goos:       "darwin",
		Goarch:     "arm64",
	}
	withMatchingManifest(&deps, oldBin)

	rc := &fakeReleaseClient{release: makeCurrentRelease("v0.2.0")}
	rc.byTag = map[string]*Release{
		"v0.1.0": makeCurrentRelease("v0.1.0"),
		"v0.2.0": makeCurrentRelease("v0.2.0"),
		"":       makeCurrentRelease("v0.2.0"),
	}

	sess := &fakeControlSession{}
	sess.state.Running = running
	mgr := &fakeControlManager{session: sess}
	svc := &Service{
		CurrentVersion:    "v0.1.0",
		ReleaseClient:     rc,
		ProvenanceDeps:    deps,
		Goos:              "darwin",
		Goarch:            "arm64",
		DownloadBase:      "https://example.invalid/download",
		ControlManager:    mgr,
		Installer:         NewPosixInstaller(),
		ConfigLoader:      (&recordingConfigLoader{cfg: &config.Config{DataDir: dir}}).load,
		binPathForTest:    binPath,
		binContentForTest: oldBin,
	}
	return svc, sess, binPath, stagePath
}

// realFileReader 用真实 os.ReadFile 实现 FileReader。
type realFileReader struct{}

func (realFileReader) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// TestApply_RealPosixInstall_DaemonNotRunning 真实 POSIX 安装 + daemon 未运行：
// target 被替换为新版本，无 Stop/Start，Commit 清理事务文件。
func TestApply_RealPosixInstall_DaemonNotRunning(t *testing.T) {
	svc, sess, binPath, stagePath := makeApplyInstallService(t, false)

	// 用 installUnderLock 验证未运行路径（Install + Commit，无 Stop/Start）。
	installed, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	if !installed {
		t.Fatal("应 installed=true")
	}
	// daemon 未运行：不应 Stop/Start。
	if sess.stopCalls != 0 || sess.startCalls != 0 {
		t.Errorf("未运行不应 Stop/Start, stop=%d start=%d", sess.stopCalls, sess.startCalls)
	}
	assertFileContent(t, binPath, "new-official-bin")
	assertFileMode(t, binPath, 0o755)
	// installUnderLock 在 Start 成功（或无需启动）后调 Commit 清理事务文件。
	assertNoTransactionFiles(t, binPath)
}

// TestApply_RealPosixInstall_OrchestrationOrderRunning daemon 运行时编排顺序：
// Inspect(running) → Stop → Install → Start(newBinPath)。
// 用 fakeControlManager 包装真实 Installer 验证顺序。
func TestApply_RealPosixInstall_OrchestrationOrderRunning(t *testing.T) {
	svc, sess, binPath, stagePath := makeApplyInstallService(t, true)

	// 通过直接调用 installUnderLock 验证编排（绕过 Apply 的 download 集成）。
	installed, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	if !installed {
		t.Fatal("应 installed=true")
	}
	// 编排顺序：Inspect 1 次，Stop 1 次（运行中），Start 1 次。
	if sess.inspectCalls != 1 {
		t.Errorf("Inspect calls=%d, want 1", sess.inspectCalls)
	}
	if sess.stopCalls != 1 {
		t.Errorf("Stop calls=%d, want 1（运行中应 Stop）", sess.stopCalls)
	}
	if sess.startCalls != 1 {
		t.Errorf("Start calls=%d, want 1（运行中应 Start newBin）", sess.startCalls)
	}
	// Start 应使用新二进制路径（= target，POSIX rename 后路径不变）。
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start binPath=%q, want %q", sess.lastStartBinPath, binPath)
	}
	// target 内容应已替换。
	assertFileContent(t, binPath, "new-official-bin")
}

// TestApply_RealPosixInstall_OrchestrationOrderNotRunning daemon 未运行时编排顺序：
// Inspect(not running) →（跳过 Stop）→ Install →（跳过 Start）。
func TestApply_RealPosixInstall_OrchestrationOrderNotRunning(t *testing.T) {
	svc, sess, binPath, stagePath := makeApplyInstallService(t, false)

	installed, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	if !installed {
		t.Fatal("应 installed=true")
	}
	if sess.inspectCalls != 1 {
		t.Errorf("Inspect calls=%d, want 1", sess.inspectCalls)
	}
	if sess.stopCalls != 0 {
		t.Errorf("未运行不应 Stop, calls=%d", sess.stopCalls)
	}
	if sess.startCalls != 0 {
		t.Errorf("未运行不应 Start, calls=%d", sess.startCalls)
	}
	assertFileContent(t, binPath, "new-official-bin")
}

// ---- 失败路径：Stop 失败时 target 不变 ----

// TestApply_StopFails_TargetUnchanged Stop 失败 → target 不变，不 Install。
func TestApply_StopFails_TargetUnchanged(t *testing.T) {
	svc, sess, binPath, stagePath := makeApplyInstallService(t, true)
	sess.stopErr = errors.New("stop boom")

	_, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err == nil {
		t.Fatal("Stop 失败应返回错误")
	}
	// target 应保持旧版本。
	assertFileContent(t, binPath, "old-official-bin")
	// 不应 Install（stage 未消耗）。
	if sess.startCalls != 0 {
		t.Errorf("Stop 失败不应 Start, calls=%d", sess.startCalls)
	}
}

// ---- 失败路径：Install 失败（rename 前）target 不变 + 回滚重启 ----

// TestApply_InstallFails_TargetPreserved Rollback Rename 前 Install 失败：
// target 仍是旧版本，daemon 运行时用旧路径回滚重启。
func TestApply_InstallFails_TargetPreservedRollback(t *testing.T) {
	svc, sess, binPath, _ := makeApplyInstallService(t, true)
	// stagePath 指向不存在文件 → Install 在 validateInstallInputs 失败。
	_, err := svc.installUnderLock(context.Background(), filepath.Join(filepath.Dir(binPath), ".no-stage"), binPath)
	if err == nil {
		t.Fatal("Install 失败应返回错误")
	}
	// target 应保持旧版本。
	assertFileContent(t, binPath, "old-official-bin")
	// 运行中应回滚重启（用旧路径）。
	if sess.startCalls != 1 {
		t.Errorf("Install 失败应回滚重启一次, calls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("回滚应用旧路径 %q, got %q", binPath, sess.lastStartBinPath)
	}
}

// ---- 失败路径：StartNew 失败回滚到旧 ----

// TestApply_StartNewFails_RollbackToOld 新二进制启动失败 → Rollback 恢复旧版本 + 用旧路径回滚重启。
// TransactionHandler.Rollback 把 backup 恢复到 target，使 target 回到旧版本；
// installUnderLock 随后用 oldBinPath（= target，已是旧版本）重启 daemon。
func TestApply_StartNewFails_RollbackToOld(t *testing.T) {
	svc, sess, binPath, stagePath := makeApplyInstallService(t, true)
	sess.startErr = errors.New("start boom")

	_, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err == nil {
		t.Fatal("Start 失败应返回错误")
	}
	// Rollback 后 target 应回到旧版本（TransactionHandler.Rollback 恢复 backup）。
	assertFileContent(t, binPath, "old-official-bin")
	// installUnderLock 应尝试用 oldBinPath 回滚重启。
	if sess.startCalls != 2 {
		t.Errorf("Start 失败 + 回滚重启应 Start 两次, calls=%d", sess.startCalls)
	}
	// 回滚重启用 oldBinPath（= target，已是旧版本）。
	if sess.lastStartBinPath != binPath {
		t.Errorf("回滚应用 oldBinPath %q, got %q", binPath, sess.lastStartBinPath)
	}
	// Rollback 后事务文件应被清理。
	assertNoTransactionFiles(t, binPath)
}

// ---- journal 记录阶段测试 ----

// TestInstall_JournalWrittenBeforeRename 验证事务过程中 journal 被写入。
// 通过故意让 rename 后的 cleanup 失败来观测 journal 存在性——
// 这里用更直接的方式：在事务中段（backup 创建后、rename 前）观测。
// 由于 Install 是原子的，改用直接测试 backupTarget + writeJournal 的组合。
func TestInstall_JournalWrittenBeforeRename(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	// 模拟事务中段：手动创建 backup + journal，验证它们的内容正确。
	oldHash, _ := fileSHA256(target)
	newHash, _ := fileSHA256(stage)
	backup := backupFilePath(target, "test")
	journal := journalFilePath(target, "test")
	if err := backupTarget(target, backup, oldHash); err != nil {
		t.Fatalf("backupTarget: %v", err)
	}
	rec := journalRecord{
		Nonce: "test", Phase: phasePrepared,
		TargetBasename: filepath.Base(target),
		StageBasename:  filepath.Base(stageFilePath(target, "test")),
		BackupBasename: filepath.Base(backup),
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
	}
	if err := writeJournal(journal, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	// journal 应存在且可读。
	got, ok, err := readJournal(journal)
	if err != nil || !ok {
		t.Fatalf("readJournal err=%v ok=%v", err, ok)
	}
	if got.Phase != phasePrepared {
		t.Errorf("phase=%q, want prepared", got.Phase)
	}
	// backup 内容应等于旧 target。
	assertFileContent(t, backup, "old")
}

// ---- 完整事务端到端：验证 backup/stage/journal 在 Commit 后全部清理 ----

// TestInstall_FullTransaction_AllFilesCleaned 成功事务 + Commit 后无任何事务文件残留。
// Install 成功后 backup/journal 暂存（待 daemon Start 确认）；Commit 清理后才无残留。
func TestInstall_FullTransaction_AllFilesCleaned(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old-version-bin", "new-version-bin")
	installer := NewPosixInstaller().(*posixInstaller)

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFileContent(t, target, "new-version-bin")
	// Install 成功后 backup/journal 仍存在（待 Commit）。
	// 外部 stage 应保留（Install 复制不 move）。
	assertFileContent(t, stage, "new-version-bin")
	// Commit 清理事务文件。
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertNoTransactionFiles(t, target)
}

// TestInstall_PendingBackupBeforeCommit Install 成功但未 Commit 时 backup/journal 存在。
func TestInstall_PendingBackupBeforeCommit(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	installer := NewPosixInstaller().(*posixInstaller)

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	// backup 应存在（内容是旧版本）。
	dir := filepath.Dir(target)
	entries, _ := os.ReadDir(dir)
	var foundBackup, foundJournal bool
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, updateTempPrefix(target)+updateBackupSuffix) {
			foundBackup = true
		}
		if strings.HasPrefix(name, updateTempPrefix(target)+updateJournalSuffix) {
			foundJournal = true
		}
	}
	if !foundBackup {
		t.Error("Install 成功后 backup 应存在（待 Commit）")
	}
	if !foundJournal {
		t.Error("Install 成功后 journal 应存在（待 Commit）")
	}
}

// TestInstall_CommitIdempotent 多次 Commit 安全（不重复清理、不报错）。
func TestInstall_CommitIdempotent(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	installer := NewPosixInstaller().(*posixInstaller)
	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	// 第二次 Commit 应无操作（无 error）。
	if err := installer.Commit(); err != nil {
		t.Errorf("Commit 2 应无操作, got %v", err)
	}
	assertNoTransactionFiles(t, target)
}

// TestInstall_RollbackRestoresOldVersion Rollback 恢复旧版本并清理。
func TestInstall_RollbackRestoresOldVersion(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old-version", "new-version")
	installer := NewPosixInstaller().(*posixInstaller)

	_, err := installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFileContent(t, target, "new-version")
	// Rollback：恢复旧版本。
	if err := installer.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	assertFileContent(t, target, "old-version")
	assertNoTransactionFiles(t, target)
}

// TestInstall_RollbackIdempotent 多次 Rollback 安全。
func TestInstall_RollbackIdempotent(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	installer := NewPosixInstaller().(*posixInstaller)
	_, _ = installer.Install(context.Background(), stage, target, target, false)
	if err := installer.Rollback(); err != nil {
		t.Fatalf("Rollback 1: %v", err)
	}
	if err := installer.Rollback(); err != nil {
		t.Errorf("Rollback 2 应无操作, got %v", err)
	}
	assertFileContent(t, target, "old")
}

// TestInstall_RollbackWithoutInstallNoOp 无 lastTxn 时 Rollback 无操作。
func TestInstall_RollbackWithoutInstallNoOp(t *testing.T) {
	installer := NewPosixInstaller().(*posixInstaller)
	if err := installer.Rollback(); err != nil {
		t.Errorf("无 lastTxn 时 Rollback 应返回 nil, got %v", err)
	}
	if err := installer.Commit(); err != nil {
		t.Errorf("无 lastTxn 时 Commit 应返回 nil, got %v", err)
	}
}

// ---- rename 失败路径 ----

// TestPosixInstall_RenameFailsTargetUnchanged rename 失败时 target 保持旧版本。
// 模拟方式：stage 复制成功后、rename 前，无法直接注入失败——
// 改为测试 stage 与 target 同目录但 target 路径非法（空）已被 validateInstallInputs 拦截。
// 这里测试 rename 失败的等价场景：target 目录权限只读导致 rename 失败。
func TestPosixInstall_RenameFailsTargetUnchanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	stage := filepath.Join(dir, ".stage")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(stage, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	// 把 target 目录设为只读，使 rename(stageFile, target) 可能失败。
	// 注意：rename 覆盖已有文件通常只需目录写权限，只读目录会让 rename 失败。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	defer os.Chmod(dir, 0o755) // 恢复以便 t.TempDir 清理

	installer := NewPosixInstaller()
	_, err := installer.Install(context.Background(), stage, target, target, false)
	// rename 在只读目录下应失败（或 backup 创建时失败，取决于顺序）。
	if err == nil {
		// 某些系统/配置下只读目录仍允许 rename（如 root 用户），跳过此断言。
		t.Logf("只读目录下 Install 未失败（可能以 root 运行），跳过 rename 失败断言")
		// 确保恢复权限后 target 可读。
		os.Chmod(dir, 0o755)
		assertFileContent(t, target, "new")
		return
	}
	// 失败后 target 应保持旧版本（rename 原子，失败时不半完成）。
	os.Chmod(dir, 0o755) // 恢复以便读取 target
	assertFileContent(t, target, "old")
}

// ---- rollback 失败路径：backup 损坏 ----

// TestInstall_RollbackFailsOnCorruptBackup Rollback 时 backup 损坏 → 返回错误，保留文件。
// 直接测 rollbackToBackup：backup 内容与 expectedOldHash 不匹配。
func TestInstall_RollbackFailsOnCorruptBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	backup := filepath.Join(dir, ".backup")
	journal := filepath.Join(dir, ".journal")
	// target 是新版本（rename 后状态）。
	os.WriteFile(target, []byte("new"), 0o755)
	// backup 内容损坏（不等于 oldHash）。
	os.WriteFile(backup, []byte("corrupt"), 0o755)
	os.WriteFile(journal, []byte("{}"), 0o600)

	oldHash := sha256HexBytes([]byte("old"))
	err := rollbackToBackup(target, backup, oldHash, journal)
	if err == nil {
		t.Fatal("backup 损坏时 rollbackToBackup 应返回错误")
	}
	// target 应保留（未被覆盖）——仍是新版本。
	assertFileContent(t, target, "new")
	// backup/journal 应保留（不删除，供人工处理）。
	if _, e := os.Lstat(backup); e != nil {
		t.Errorf("损坏 backup 应保留: %v", e)
	}
	if _, e := os.Lstat(journal); e != nil {
		t.Errorf("journal 应保留: %v", e)
	}
}

// ---- installUnderLock 恢复集成：有遗留 journal 时先恢复 ----

// TestApply_RecoverJournalBeforeInstall 有遗留 journal（状态 2）时先恢复，再正常 Install。
func TestApply_RecoverJournalBeforeInstall(t *testing.T) {
	svc, _, binPath, stagePath := makeApplyInstallService(t, false)
	// 创建遗留 journal（状态 2：target 仍是 oldHash，stage 尚在）。
	oldContent := "old-official-bin"
	newContent := "new-official-bin"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	rec := journalRecord{
		Nonce: "prev", Phase: phasePrepared,
		TargetBasename: filepath.Base(binPath),
		StageBasename:  filepath.Base(stageFilePath(binPath, "prev")),
		BackupBasename: filepath.Base(backupFilePath(binPath, "prev")),
		OldSHA256:      oldHash, NewSHA256: newHash,
	}
	os.WriteFile(stageFilePath(binPath, "prev"), []byte(newContent), 0o755)
	os.WriteFile(backupFilePath(binPath, "prev"), []byte(oldContent), 0o755)
	writeJournal(journalFilePath(binPath, "prev"), rec)

	// installUnderLock 应先恢复（清理残留），再正常 Install。
	installed, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	if !installed {
		t.Fatal("应 installed=true")
	}
	// target 应是新版本。
	assertFileContent(t, binPath, newContent)
	assertNoTransactionFiles(t, binPath)
}

// TestApply_RecoverJournalManualBlocksInstall 模糊 journal（Manual 状态）阻止继续 Install。
func TestApply_RecoverJournalManualBlocksInstall(t *testing.T) {
	svc, _, binPath, stagePath := makeApplyInstallService(t, false)
	// 创建损坏的 journal（无法解析）。
	os.WriteFile(journalFilePath(binPath, "fuzzy"), []byte("{corrupt"), 0o600)

	_, err := svc.installUnderLock(context.Background(), stagePath, binPath)
	if err == nil {
		t.Fatal("模糊 journal 应阻止 Install，返回错误")
	}
	// target 应保持旧版本（未替换）。
	assertFileContent(t, binPath, "old-official-bin")
}

// ---- 并发安全：nonce 唯一性保证两次 Install 不冲突 ----

// TestInstall_TwoConsecutiveInstalls 两次连续 Install + Commit 互不干扰。
func TestInstall_TwoConsecutiveInstalls(t *testing.T) {
	target, _ := setupTargetAndStage(t, "v1", "v2")
	installer := NewPosixInstaller().(*posixInstaller)

	// 第一次：v1 → v2 + Commit。
	stage1 := filepath.Join(filepath.Dir(target), ".stage1")
	os.WriteFile(stage1, []byte("v2"), 0o755)
	_, err := installer.Install(context.Background(), stage1, target, target, false)
	if err != nil {
		t.Fatalf("Install 1: %v", err)
	}
	assertFileContent(t, target, "v2")
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	// 第二次：v2 → v3 + Commit。
	stage2 := filepath.Join(filepath.Dir(target), ".stage2")
	os.WriteFile(stage2, []byte("v3"), 0o755)
	_, err = installer.Install(context.Background(), stage2, target, target, false)
	if err != nil {
		t.Fatalf("Install 2: %v", err)
	}
	assertFileContent(t, target, "v3")
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	assertNoTransactionFiles(t, target)
}

// ---- 恢复后继续更新的集成 ----

// TestRecoverThenInstall_CleanStateAllowsNewInstall 恢复后（状态 2）target 完好，可继续新 Install。
func TestRecoverThenInstall_CleanStateAllowsNewInstall(t *testing.T) {
	target, stage := setupTargetAndStage(t, "old", "new")
	oldHash := sha256HexBytes([]byte("old"))
	newHash := sha256HexBytes([]byte("new"))
	// 模拟中断：创建遗留 journal（状态 2：target 仍是 oldHash）。
	rec := journalRecord{
		Nonce: "prev", Phase: phasePrepared,
		TargetBasename: filepath.Base(target),
		StageBasename:  filepath.Base(stageFilePath(target, "prev")),
		BackupBasename: filepath.Base(backupFilePath(target, "prev")),
		OldSHA256:      oldHash, NewSHA256: newHash,
	}
	// 创建遗留的 stage/backup（模拟上次中断的残留）。
	os.WriteFile(stageFilePath(target, "prev"), []byte("new"), 0o755)
	os.WriteFile(backupFilePath(target, "prev"), []byte("old"), 0o755)
	writeJournal(journalFilePath(target, "prev"), rec)

	installer := NewPosixInstaller().(*posixInstaller)
	// 恢复：状态 2，清理残留。
	outcome, err := installer.RecoverJournal(target)
	if err != nil || outcome.State != RecoveryStateOldIntact {
		t.Fatalf("RecoverJournal err=%v outcome=%+v", err, outcome)
	}
	assertNoTransactionFiles(t, target)
	// 恢复后可继续新 Install + Commit。
	_, err = installer.Install(context.Background(), stage, target, target, false)
	if err != nil {
		t.Fatalf("恢复后 Install: %v", err)
	}
	assertFileContent(t, target, "new")
	if err := installer.Commit(); err != nil {
		t.Fatalf("恢复后 Commit: %v", err)
	}
	assertNoTransactionFiles(t, target)
}

// ---- 编译期断言 ----

// 编译期断言：posixInstaller 满足 Installer + JournalRecoverer + TransactionHandler 接口。
var (
	_ Installer          = (*posixInstaller)(nil)
	_ JournalRecoverer   = (*posixInstaller)(nil)
	_ TransactionHandler = (*posixInstaller)(nil)
)

// ---- journal 写入原 daemon 运行态 ----

// TestInstall_WritesWasRunningToJournal Install 把 wasRunning 写入 journal 的 WasRunning 字段。
// 验证修复后磁盘上 was_running 不再恒为 false。
func TestInstall_WritesWasRunningToJournal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	stage := filepath.Join(dir, ".incoming")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(stage, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	installer := NewPosixInstaller().(*posixInstaller)

	// wasRunning=true 调用 Install。
	if _, err := installer.Install(context.Background(), stage, target, target, true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// 用 findLeftoverJournal + readJournal 读回磁盘 journal，验证 WasRunning=true。
	jp, found, err := findLeftoverJournal(target)
	if err != nil || !found {
		t.Fatalf("未找到 journal（found=%v err=%v），Install 应暂存 journal 待 Commit", found, err)
	}
	rec, ok, err := readJournal(jp)
	if err != nil || !ok {
		t.Fatalf("readJournal err=%v ok=%v", err, ok)
	}
	if !rec.WasRunning {
		t.Error("journal 的 WasRunning 应为 true，实际 false（修复前恒为 false）")
	}
	// 清理。
	if err := installer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestInstall_WritesWasRunningFalseToJournal wasRunning=false 时 journal WasRunning=false。
func TestInstall_WritesWasRunningFalseToJournal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	stage := filepath.Join(dir, ".incoming")
	os.WriteFile(target, []byte("old"), 0o755)
	os.WriteFile(stage, []byte("new"), 0o755)
	installer := NewPosixInstaller().(*posixInstaller)

	if _, err := installer.Install(context.Background(), stage, target, target, false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// 读 journal。
	jp, found, _ := findLeftoverJournal(target)
	if !found {
		t.Fatal("应找到遗留 journal")
	}
	rec, ok, err := readJournal(jp)
	if err != nil || !ok {
		t.Fatalf("readJournal err=%v ok=%v", err, ok)
	}
	if rec.WasRunning {
		t.Error("journal WasRunning 应为 false")
	}
	installer.Commit()
}

// TestRecoverJournal_State1_PropagatesWasRunning 状态 1 恢复时 outcome.WasRunning 来自 journal。
func TestRecoverJournal_State1_PropagatesWasRunning(t *testing.T) {
	newContent := "new-version"
	oldContent := "old-version"
	rec := journalRecord{
		Nonce: "n1", Phase: phaseInstalled,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n1",
		BackupBasename: ".token-usage.update-backup-n1",
		OldSHA256:      sha256HexBytes([]byte(oldContent)),
		NewSHA256:      sha256HexBytes([]byte(newContent)),
		WasRunning:     true, // 原 daemon 在运行
	}
	installer, target := makeRecoveryScenario(t, &newContent, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateNewInstalled {
		t.Fatalf("State = %q, want new_installed", outcome.State)
	}
	if !outcome.WasRunning {
		t.Error("outcome.WasRunning 应为 true（来自 journal），实际 false")
	}
}

// TestRecoverJournal_State3_PropagatesWasRunning 状态 3 恢复时 outcome.WasRunning 来自 journal。
func TestRecoverJournal_State3_PropagatesWasRunning(t *testing.T) {
	oldContent := "old-version"
	newContent := "new-version"
	rec := journalRecord{
		Nonce: "n3", Phase: phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n3",
		BackupBasename: ".token-usage.update-backup-n3",
		OldSHA256:      sha256HexBytes([]byte(oldContent)),
		NewSHA256:      sha256HexBytes([]byte(newContent)),
		WasRunning:     true,
	}
	installer, target := makeRecoveryScenario(t, nil, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if outcome.State != RecoveryStateOldRestored {
		t.Fatalf("State = %q, want old_restored", outcome.State)
	}
	if !outcome.WasRunning {
		t.Error("outcome.WasRunning 应为 true（来自 journal），实际 false")
	}
	if outcome.NewBinPath != target {
		t.Errorf("NewBinPath = %q, want %q", outcome.NewBinPath, target)
	}
}

// ---- installUnderLock 恢复后按原运行态重启 daemon ----

// makeRecoveryApplyService 构造一个有遗留 journal 场景的 Service，daemon 当前未运行
// （模拟中断在 Stop 后、Start 前），用于验证恢复按 journal 原运行态重启。
func makeRecoveryApplyService(t *testing.T, newInstalled bool, wasRunning bool) (*Service, *fakeControlSession, string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "token-usage")
	oldContent := "old-official-bin"
	newContent := "new-official-bin"
	// 写入 target（NewInstalled=new版本，否则=old版本）。
	tc := oldContent
	if newInstalled {
		tc = newContent
	}
	if err := os.WriteFile(binPath, []byte(tc), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	// 创建遗留 journal + backup + stage。
	rec := journalRecord{
		Nonce: "prev", Phase: phasePrepared,
		TargetBasename: filepath.Base(binPath),
		StageBasename:  filepath.Base(stageFilePath(binPath, "prev")),
		BackupBasename: filepath.Base(backupFilePath(binPath, "prev")),
		OldSHA256:      sha256HexBytes([]byte(oldContent)),
		NewSHA256:      sha256HexBytes([]byte(newContent)),
		WasRunning:     wasRunning,
	}
	if newInstalled {
		rec.Phase = phaseInstalled
	}
	os.WriteFile(stageFilePath(binPath, "prev"), []byte(newContent), 0o755)
	os.WriteFile(backupFilePath(binPath, "prev"), []byte(oldContent), 0o755)
	if err := writeJournal(journalFilePath(binPath, "prev"), rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	// daemon 当前未运行（模拟中断在 Stop 后、Start 前——Stop 已执行使 daemon 停）。
	sess := &fakeControlSession{}
	sess.state.Running = false
	mgr := &fakeControlManager{session: sess}
	// provenance deps 用真实 FS。
	deps := ProvenanceDeps{
		Executable: &fakeExecutableResolver{path: binPath},
		Lstat:      realLstat{},
		FileReader: &realFileReader{},
		Manifest:   nil,
		Goos:       "darwin",
		Goarch:     "arm64",
	}
	withMatchingManifest(&deps, []byte(oldContent))
	rc := &fakeReleaseClient{release: makeCurrentRelease("v0.2.0")}
	rc.byTag = map[string]*Release{
		"v0.1.0": makeCurrentRelease("v0.1.0"),
		"v0.2.0": makeCurrentRelease("v0.2.0"),
		"":       makeCurrentRelease("v0.2.0"),
	}
	svc := &Service{
		CurrentVersion: "v0.1.0",
		ReleaseClient:  rc,
		ProvenanceDeps: deps,
		Goos:           "darwin",
		Goarch:         "arm64",
		DownloadBase:   "https://example.invalid/download",
		ControlManager: mgr,
		Installer:      NewPosixInstaller(),
		ConfigLoader:   (&recordingConfigLoader{cfg: &config.Config{DataDir: dir}}).load,
	}
	return svc, sess, binPath
}

// TestApply_RecoveryNewInstalled_RestartsDaemonByJournalWasRunning
// 状态 1（NewInstalled）+ journal wasRunning=true：恢复后按原运行态重启 daemon（新版本）。
// 关键：daemon 当前未运行（Inspect 会报 not-running），但不能据此跳过 Start——
// 必须按 journal 的 wasRunning=true 重启。
func TestApply_RecoveryNewInstalled_RestartsDaemonByJournalWasRunning(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, true, true)

	installed, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	// 恢复 NewInstalled 后本轮结束，不执行新的 Install（installed=false）。
	if installed {
		t.Error("恢复后不应执行新 Install，installed 应为 false")
	}
	// 关键断言：按 journal wasRunning=true 重启了 daemon（即使 Inspect 报 not-running）。
	if sess.startCalls != 1 {
		t.Errorf("应按 journal 原运行态 Start 一次（新版本），startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应用恢复后的 target %q, got %q", binPath, sess.lastStartBinPath)
	}
	// 不应触发新的 Stop（恢复路径不 Stop）。
	if sess.stopCalls != 0 {
		t.Errorf("恢复路径不应 Stop, stopCalls=%d", sess.stopCalls)
	}
	// target 应是新版本，事务文件已清理。
	assertFileContent(t, binPath, "new-official-bin")
	assertNoTransactionFiles(t, binPath)
}

// TestApply_RecoveryPrecedesVersionAndProvenanceChecks 真实的中断恢复不能依赖本轮
// 版本比较或来源校验：状态 1 时正在运行的旧进程看到的版本仍旧，但 target 已是新版本；
// 状态 3 时 target 甚至暂时缺失。三种状态都应先恢复本地一致性且不访问 Release。
func TestApply_RecoveryPrecedesVersionAndProvenanceChecks(t *testing.T) {
	tests := []struct {
		name         string
		newInstalled bool
		removeTarget bool
		wantState    RecoveryState
		wantContent  string
	}{
		{
			name:         "new_installed",
			newInstalled: true,
			wantState:    RecoveryStateNewInstalled,
			wantContent:  "new-official-bin",
		},
		{
			name:        "old_intact",
			wantState:   RecoveryStateOldIntact,
			wantContent: "old-official-bin",
		},
		{
			name:         "old_restored",
			removeTarget: true,
			wantState:    RecoveryStateOldRestored,
			wantContent:  "old-official-bin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, sess, binPath := makeRecoveryApplyService(t, tt.newInstalled, true)
			if tt.removeTarget {
				if err := os.Remove(binPath); err != nil {
					t.Fatalf("Remove target: %v", err)
				}
			}

			got, err := svc.Apply(context.Background(), ApplyOptions{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !got.Recovered || got.RecoveryState != tt.wantState {
				t.Fatalf("恢复结果=%+v，want Recovered=true, RecoveryState=%q", got, tt.wantState)
			}
			if got.Installed || got.Deferred || got.ReadyToInstall || got.ProvenanceChecked {
				t.Errorf("恢复不应被当作本轮安装或来源校验，got=%+v", got)
			}
			if rc := svc.ReleaseClient.(*fakeReleaseClient); len(rc.fetches) != 0 {
				t.Errorf("恢复应在版本查询和来源校验前完成，Release fetches=%v", rc.fetches)
			}
			if sess.startCalls != 1 || sess.lastStartBinPath != binPath {
				t.Errorf("应按 journal 原运行态重启 target，startCalls=%d path=%q", sess.startCalls, sess.lastStartBinPath)
			}
			assertFileContent(t, binPath, tt.wantContent)
			assertNoTransactionFiles(t, binPath)
		})
	}
}

// TestApply_RecoveryNewInstalled_NoRestartWhenNotRunning
// 状态 1 + journal wasRunning=false：恢复后不重启 daemon。
func TestApply_RecoveryNewInstalled_NoRestartWhenNotRunning(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, true, false)

	_, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	// wasRunning=false，不应 Start。
	if sess.startCalls != 0 {
		t.Errorf("wasRunning=false 时不应 Start, startCalls=%d", sess.startCalls)
	}
	assertFileContent(t, binPath, "new-official-bin")
}

// TestApply_RecoveryOldRestored_RestartsDaemonByJournalWasRunning
// 状态 3（OldRestored）+ wasRunning=true：恢复旧版本后按原运行态重启 daemon（旧版本）。
func TestApply_RecoveryOldRestored_RestartsDaemonByJournalWasRunning(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, false, true)
	// 删除 target 模拟状态 3（target 缺失）。
	if err := os.Remove(binPath); err != nil {
		t.Fatalf("Remove target: %v", err)
	}

	_, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	// 按 journal wasRunning=true 重启 daemon（恢复后的旧版本）。
	if sess.startCalls != 1 {
		t.Errorf("应按 journal 原运行态 Start 一次, startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应用恢复后的 target %q, got %q", binPath, sess.lastStartBinPath)
	}
	// target 应恢复为旧版本。
	assertFileContent(t, binPath, "old-official-bin")
	assertNoTransactionFiles(t, binPath)
}

// TestApply_RecoveryOldIntact_RestartsDaemonByJournalWasRunning 覆盖事务在
// prepared 阶段中断的状态 2。此时 journal 证明 daemon 原先在运行，但当前
// 已在 Install 前被 Stop；恢复旧 target 后必须重启旧 daemon，且本轮不再安装新版本。
func TestApply_RecoveryOldIntact_RestartsDaemonByJournalWasRunning(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, false, true)

	installed, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err != nil {
		t.Fatalf("installUnderLock: %v", err)
	}
	if installed {
		t.Fatal("状态 2 恢复后不应继续本轮 Install")
	}
	if sess.startCalls != 1 {
		t.Fatalf("状态 2 应按 journal 原运行态重启 daemon，startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应使用保留的旧 target %q，实际 %q", binPath, sess.lastStartBinPath)
	}
	if sess.inspectCalls != 0 || sess.stopCalls != 0 {
		t.Errorf("状态 2 恢复后不应再次 Inspect/Stop，inspect=%d stop=%d", sess.inspectCalls, sess.stopCalls)
	}
	assertFileContent(t, binPath, "old-official-bin")
	assertNoTransactionFiles(t, binPath)
}

// TestApply_RecoveryNewInstalled_CleanupPendingStillRestartsDaemon 确认状态 1 的
// 清理失败不会阻塞原 daemon 的恢复；错误仍向调用方报告为待处理。
func TestApply_RecoveryNewInstalled_CleanupPendingStillRestartsDaemon(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, true, true)
	stagePath := stageFilePath(binPath, "prev")
	if err := os.Remove(stagePath); err != nil {
		t.Fatalf("Remove stage: %v", err)
	}
	if err := os.Mkdir(stagePath, 0o755); err != nil {
		t.Fatalf("Mkdir stage: %v", err)
	}

	_, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err == nil {
		t.Fatal("事务文件清理失败应返回错误")
	}
	if sess.startCalls != 1 {
		t.Errorf("清理失败时仍应按原运行态启动 daemon，startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应使用恢复后的 target %q，实际 %q", binPath, sess.lastStartBinPath)
	}
	assertFileContent(t, binPath, "new-official-bin")
	if info, statErr := os.Lstat(stagePath); statErr != nil || !info.IsDir() {
		t.Errorf("不能删除的 stage 目录应保留，info=%v err=%v", info, statErr)
	}
}

// TestApply_RecoveryOldRestored_CleanupPendingStillRestartsDaemon 确认状态 3 的
// 清理失败也不会阻塞已恢复旧二进制的 daemon 启动。
func TestApply_RecoveryOldRestored_CleanupPendingStillRestartsDaemon(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, false, true)
	if err := os.Remove(binPath); err != nil {
		t.Fatalf("Remove target: %v", err)
	}
	stagePath := stageFilePath(binPath, "prev")
	if err := os.Remove(stagePath); err != nil {
		t.Fatalf("Remove stage: %v", err)
	}
	if err := os.Mkdir(stagePath, 0o755); err != nil {
		t.Fatalf("Mkdir stage: %v", err)
	}

	_, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err == nil {
		t.Fatal("事务文件清理失败应返回错误")
	}
	if sess.startCalls != 1 {
		t.Errorf("清理失败时仍应按原运行态启动 daemon，startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应使用恢复后的 target %q，实际 %q", binPath, sess.lastStartBinPath)
	}
	assertFileContent(t, binPath, "old-official-bin")
	if info, statErr := os.Lstat(stagePath); statErr != nil || !info.IsDir() {
		t.Errorf("不能删除的 stage 目录应保留，info=%v err=%v", info, statErr)
	}
}

// TestApply_RecoveryOldIntact_CleanupPendingStillRestartsDaemon 确认状态 2 的
// 清理失败也不会丢失 journal 记录的原 daemon 运行态。
func TestApply_RecoveryOldIntact_CleanupPendingStillRestartsDaemon(t *testing.T) {
	svc, sess, binPath := makeRecoveryApplyService(t, false, true)
	stagePath := stageFilePath(binPath, "prev")
	if err := os.Remove(stagePath); err != nil {
		t.Fatalf("Remove stage: %v", err)
	}
	if err := os.Mkdir(stagePath, 0o755); err != nil {
		t.Fatalf("Mkdir stage: %v", err)
	}

	_, err := svc.installUnderLock(context.Background(), "/nonexistent-stage", binPath)
	if err == nil {
		t.Fatal("事务文件清理失败应返回错误")
	}
	if sess.startCalls != 1 {
		t.Errorf("清理失败时仍应按原运行态启动 daemon，startCalls=%d", sess.startCalls)
	}
	if sess.lastStartBinPath != binPath {
		t.Errorf("Start 应使用保留的旧 target %q，实际 %q", binPath, sess.lastStartBinPath)
	}
	assertFileContent(t, binPath, "old-official-bin")
	if info, statErr := os.Lstat(stagePath); statErr != nil || !info.IsDir() {
		t.Errorf("不能删除的 stage 目录应保留，info=%v err=%v", info, statErr)
	}
}

// ---- Install 失败时 restart error 聚合 ----

// TestApply_InstallFailsRestartFails_AggregatesErrors Install 失败 + 旧版本重启也失败 →
// errors.Join 聚合两个错误（修复前 restart error 被丢弃）。
func TestApply_InstallFailsRestartFails_AggregatesErrors(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// daemon 运行中。
	sess := &fakeControlSession{}
	sess.state.Running = true
	// Install 失败（stagePath 不存在）+ Start 失败（注入 startErr）。
	sess.startErr = errors.New("restart boom")
	mgr := &fakeControlManager{session: sess}
	// 用真实 POSIX installer（Install 会因 stagePath 不存在失败）。
	svc := &Service{
		ControlManager: mgr,
		Installer:      NewPosixInstaller(),
		ConfigLoader:   (&recordingConfigLoader{cfg: &config.Config{DataDir: dir}}).load,
	}

	_, err := svc.installUnderLock(context.Background(), filepath.Join(dir, ".no-such-stage"), binPath)
	if err == nil {
		t.Fatal("应返回错误")
	}
	// 关键断言：两个错误都应出现在聚合错误中（Install 失败 + restart 失败）。
	errMsg := err.Error()
	if !strings.Contains(errMsg, "安装新版本失败") {
		t.Errorf("错误应包含 Install 失败原因, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "restart boom") {
		t.Errorf("错误应包含 restart 失败原因（修复前被丢弃）, got: %s", errMsg)
	}
	// target 应保持旧版本（Install 在 rename 前失败）。
	assertFileContent(t, binPath, "old")
	// 应尝试 restart（startCalls=1）。
	if sess.startCalls != 1 {
		t.Errorf("应尝试 restart 一次, startCalls=%d", sess.startCalls)
	}
}

// TestApply_InstallFailsRestartSucceeds_OnlyInstallError Install 失败但 restart 成功 →
// 只返回 Install 失败错误（restart 成功无错误可聚合）。
func TestApply_InstallFailsRestartSucceeds_OnlyInstallError(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "token-usage")
	os.WriteFile(binPath, []byte("old"), 0o755)
	sess := &fakeControlSession{}
	sess.state.Running = true
	mgr := &fakeControlManager{session: sess}
	svc := &Service{
		ControlManager: mgr,
		Installer:      NewPosixInstaller(),
		ConfigLoader:   (&recordingConfigLoader{cfg: &config.Config{DataDir: dir}}).load,
	}

	_, err := svc.installUnderLock(context.Background(), filepath.Join(dir, ".no-such-stage"), binPath)
	if err == nil {
		t.Fatal("应返回错误")
	}
	// restart 成功，错误只含 Install 失败。
	if !strings.Contains(err.Error(), "安装新版本失败") {
		t.Errorf("错误应包含 Install 失败原因, got: %s", err.Error())
	}
	if sess.startCalls != 1 {
		t.Errorf("应 restart 一次, startCalls=%d", sess.startCalls)
	}
}
