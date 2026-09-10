//go:build !windows

package update

// journal_serve_test.go 校验 POSIX journal 对 dashboard 运行态的记录与中断
// 恢复：journal 记录 dashboard 原在运行时，RecoverJournal 传递快照，恢复编排
// （recoverJournalWithSession）按记录的原监听地址、用落地二进制恢复 dashboard；
// 状态 2（旧版本完好、daemon 原未运行）分支先恢复 dashboard 再继续本轮安装；
// 旧版本 journal（无 serve 字段）反序列化为零值、不触发恢复（向后兼容）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// recWithServe 返回带 dashboard 快照的 journal 记录。
func recWithServe(nonce string, phase journalPhase, wasRunning, serveWasRunning bool, serveAddr string) journalRecord {
	return journalRecord{
		Nonce:           nonce,
		Phase:           phase,
		TargetBasename:  "token-usage",
		StageBasename:   ".token-usage.update-stage-" + nonce,
		BackupBasename:  ".token-usage.update-backup-" + nonce,
		OldSHA256:       "old-hash",
		NewSHA256:       "new-hash",
		WasRunning:      wasRunning,
		ServeWasRunning: serveWasRunning,
		ServeAddr:       serveAddr,
	}
}

// TestRecoverJournal_ServeState1NewInstalled 状态 1（新版本已落地）：
// journal 的 dashboard 快照原样传递给恢复编排。
func TestRecoverJournal_ServeState1NewInstalled(t *testing.T) {
	oldContent, newContent := "old-version", "new-version"
	rec := recWithServe("sv1", phaseInstalled, true, true, "127.0.0.1:8619")
	rec.OldSHA256 = sha256HexBytes([]byte(oldContent))
	rec.NewSHA256 = sha256HexBytes([]byte(newContent))
	installer, target := makeRecoveryScenario(t, &newContent, &oldContent, &newContent, rec)

	outcome, err := installer.RecoverJournal(target)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if !outcome.ServeWasRunning || outcome.ServeAddr != "127.0.0.1:8619" {
		t.Errorf("快照应原样传递, got (serve=%v addr=%q)", outcome.ServeWasRunning, outcome.ServeAddr)
	}
	if outcome.NewBinPath != target {
		t.Errorf("dashboard 应以落地新版本恢复, got %q", outcome.NewBinPath)
	}
}

// TestApply_RecoverJournalWithServeRestoresDashboard 中断恢复编排端到端：
// journal 记录 dashboard 原在运行（daemon 也在运行）→ 恢复路径用 journal 落地
// 二进制按原地址恢复 dashboard，并把 Recovered 结果连同快照返回。
func TestApply_RecoverJournalWithServeRestoresDashboard(t *testing.T) {
	const oldContent, newContent = "old-official-bin", "new-official-bin"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	svc, sess, binPath, _ := makeApplyInstallService(t, false)
	serveFake := &fakeServeLifecycle{}
	svc.ServeLifecycle = serveFake
	// 中断场景：上次更新的 daemon 停止发生在 journal 之前——本轮恢复由 journal
	// 驱动，而非锁内 Inspect。
	rec := journalRecord{
		Nonce:           "srv1",
		Phase:           phaseInstalled,
		TargetBasename:  filepath.Base(binPath),
		StageBasename:   filepath.Base(stageFilePath(binPath, "srv1")),
		BackupBasename:  filepath.Base(backupFilePath(binPath, "srv1")),
		OldSHA256:       oldHash,
		NewSHA256:       newHash,
		WasRunning:      true,
		ServeWasRunning: true,
		ServeAddr:       "127.0.0.1:8621",
	}
	installPOSIXJournalScenario(t, binPath, rec, newContent, oldContent, newContent)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应命中遗留事务恢复, got %+v", got)
	}
	if got.ServeWasRunning != true || got.ServeAddr != "127.0.0.1:8621" {
		t.Errorf("恢复结果应携带 dashboard 快照, got %+v", got)
	}
	// daemon 已按 journal 落地二进制重启。
	if sess.startCalls != 1 {
		t.Errorf("应恢复 daemon 一次, got %d", sess.startCalls)
	}
	// dashboard 按记录地址恢复。
	if serveFake.startCount() != 1 {
		t.Fatalf("应恢复 dashboard 一次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.addr != "127.0.0.1:8621" {
		t.Errorf("dashboard 应按 journal 记录地址恢复, got %+v", got)
	}
}

// TestApply_RecoverJournalState2ServeOnlyRestoresAndContinues 状态 2 且 daemon
// 原未运行（RestartDaemon=false）：先按 journal 恢复 dashboard，随后继续本轮
// 安装（handled=false）——本轮的 dashboard 探测会再次发现并接管它。
func TestApply_RecoverJournalState2ServeOnlyRestoresAndContinues(t *testing.T) {
	const oldContent, newContent = "old-official-bin", "new-official-bin"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	svc, sess, binPath, stagePath := makeApplyInstallService(t, false)
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8622", stopStopped: true}
	svc.ServeLifecycle = serveFake
	rec := journalRecord{
		Nonce:           "srv2",
		Phase:           phasePrepared,
		TargetBasename:  filepath.Base(binPath),
		StageBasename:   filepath.Base(stageFilePath(binPath, "srv2")),
		BackupBasename:  filepath.Base(backupFilePath(binPath, "srv2")),
		OldSHA256:       oldHash,
		NewSHA256:       newHash,
		WasRunning:      false,
		ServeWasRunning: true,
		ServeAddr:       "127.0.0.1:8622",
	}
	installPOSIXJournalScenario(t, binPath, rec, oldContent, oldContent, newContent)

	// 无 AssetDownloader 装配下完整 Apply 不会下载 stage，锁内编排直接以预置
	// stage 驱动（与 TestApply_RecoverJournalBeforeInstall 同法）。
	outcome, err := svc.installUnderLockOutcome(context.Background(), stagePath, binPath, "")
	if err != nil {
		t.Fatalf("installUnderLockOutcome err=%v", err)
	}
	if outcome.Recovered {
		t.Fatal("状态 2 且 daemon 原未运行应继续本轮安装而非按恢复结束")
	}
	if !outcome.Installed {
		t.Fatalf("本轮安装应完成, got %+v", outcome)
	}
	// 恢复（旧 target）+ 本轮停止 + 本轮恢复（新 target）：至少两次 Start。
	if serveFake.startCount() < 2 {
		t.Errorf("dashboard 应先按 journal 恢复、再按本轮原态恢复, got %d 次", serveFake.startCount())
	}
	if serveFake.startCalls[0].addr != "127.0.0.1:8622" {
		t.Errorf("首次恢复应按 journal 记录地址, got %+v", serveFake.startCalls[0])
	}
	if sess.startCalls != 0 {
		t.Errorf("daemon 原未运行，全程不得启动 daemon, got %d", sess.startCalls)
	}
}

// TestApply_RecoverJournalLegacyRecordNoServe 旧版本 journal（无 serve 字段）：
// 反序列化为零值，恢复路径不触碰 dashboard（向后兼容）。
func TestApply_RecoverJournalLegacyRecordNoServe(t *testing.T) {
	const oldContent, newContent = "old-official-bin", "new-official-bin"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	svc, sess, binPath, _ := makeApplyInstallService(t, false)
	serveFake := &fakeServeLifecycle{}
	svc.ServeLifecycle = serveFake
	rec := journalRecord{
		Nonce:          "leg1",
		Phase:          phaseInstalled,
		TargetBasename: filepath.Base(binPath),
		StageBasename:  filepath.Base(stageFilePath(binPath, "leg1")),
		BackupBasename: filepath.Base(backupFilePath(binPath, "leg1")),
		OldSHA256:      oldHash,
		NewSHA256:      newHash,
		WasRunning:     true,
	}
	installPOSIXJournalScenario(t, binPath, rec, newContent, oldContent, newContent)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应命中遗留事务恢复, got %+v", got)
	}
	if got.ServeWasRunning || serveFake.startCount() != 0 {
		t.Errorf("旧 journal 不得触发 dashboard 恢复, got (%v, %d)", got.ServeWasRunning, serveFake.startCount())
	}
	if sess.startCalls != 1 {
		t.Errorf("daemon 应照常恢复, got %d", sess.startCalls)
	}
}

// installPOSIXJournalScenario 在 target 同目录布置一份遗留 journal 与
// stage/backup 文件，构造 makeRecoveryScenario 同型的磁盘状态（供 Apply
// 入口的 recoverPendingJournal 命中）。targetContent 是 target 现内容，
// backupContent 是 backup 内容（决定命中状态 1/2/3）。
func installPOSIXJournalScenario(t *testing.T, target string, rec journalRecord, targetContent, backupContent, stageContent string) {
	t.Helper()
	dir := filepath.Dir(target)
	if err := os.WriteFile(target, []byte(targetContent), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.BackupBasename), []byte(backupContent), 0o755); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.StageBasename), []byte(stageContent), 0o755); err != nil {
		t.Fatalf("WriteFile stage: %v", err)
	}
	if err := writeJournal(journalFilePath(target, rec.Nonce), rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
}
