//go:build !windows

package update

// intent_unix_test.go 校验恢复意图消费中依赖 POSIX journal 场景的分支
//（makeApplyInstallService/makeRecoveryScenario 等辅助定义在 !windows 的
// install_unix_test.go）；跨平台的快照消费测试在 intent_test.go。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApply_InterruptedBeforeInstallRestoresServicesFromIntent 核心回归：
// 进程在「已 stop、未 Install」窗口被硬中断后，journal/plan 均不存在，
// 下一次 Apply 凭意图快照幂等恢复 daemon（旧二进制）与 dashboard（原地址），
// 并清除快照、按恢复结果结束本轮。
func TestApply_InterruptedBeforeInstallRestoresServicesFromIntent(t *testing.T) {
	svc, sess, binPath, _ := makeApplyInstallService(t, false)
	serveFake := &fakeServeLifecycle{}
	svc.ServeLifecycle = serveFake
	writeIntentFor(t, binPath, true, true, "127.0.0.1:8625")

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应按恢复结束本轮, got %+v", got)
	}
	if got.RecoveryState != RecoveryStateOldRestored {
		t.Errorf("恢复状态应为旧版本完好语义, got %q", got.RecoveryState)
	}
	// daemon 以旧二进制恢复（替换未发生）。
	if sess.startCalls != 1 || sess.startPaths[0] != binPath {
		t.Errorf("daemon 应以旧二进制恢复一次, got (%d, %v)", sess.startCalls, sess.startPaths)
	}
	// dashboard 按快照地址恢复。
	if serveFake.startCount() != 1 {
		t.Fatalf("dashboard 应恢复一次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.binPath != binPath || got.addr != "127.0.0.1:8625" {
		t.Errorf("dashboard 应以旧二进制+原地址恢复, got %+v", got)
	}
	// 快照已消费清除。
	if _, found, err := findIntent(binPath); err != nil || found {
		t.Errorf("快照应被消费删除, got (found=%v, err=%v)", found, err)
	}
}

// TestApply_JournalRecoveryClearsIntent journal 与快照并存（中断点晚于
// journal 落盘）：journal 路径精确恢复后，快照作为冗余被清除。
func TestApply_JournalRecoveryClearsIntent(t *testing.T) {
	const oldContent, newContent = "old-official-bin", "new-official-bin"
	oldHash := sha256HexBytes([]byte(oldContent))
	newHash := sha256HexBytes([]byte(newContent))
	svc, sess, binPath, _ := makeApplyInstallService(t, false)
	serveFake := &fakeServeLifecycle{}
	svc.ServeLifecycle = serveFake
	rec := journalRecord{
		Nonce:           "jnt1",
		Phase:           phaseInstalled,
		TargetBasename:  filepath.Base(binPath),
		StageBasename:   filepath.Base(stageFilePath(binPath, "jnt1")),
		BackupBasename:  filepath.Base(backupFilePath(binPath, "jnt1")),
		OldSHA256:       oldHash,
		NewSHA256:       newHash,
		WasRunning:      true,
		ServeWasRunning: true,
		ServeAddr:       "127.0.0.1:8626",
	}
	installPOSIXJournalScenario(t, binPath, rec, newContent, oldContent, newContent)
	writeIntentFor(t, binPath, true, true, "127.0.0.1:8626")

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应按 journal 恢复结束, got %+v", got)
	}
	// journal 与快照各恢复一次服务不发生：journal 路径恢复后快照已被清除，
	// 锁内编排的快照再查不会重复恢复。
	if sess.startCalls != 1 {
		t.Errorf("daemon 应只被 journal 恢复路径拉起一次, got %d", sess.startCalls)
	}
	if serveFake.startCount() != 1 {
		t.Errorf("dashboard 应只被恢复一次, got %d", serveFake.startCount())
	}
	if _, found, _ := findIntent(binPath); found {
		t.Error("journal 恢复完成后快照应被清除")
	}
}

// TestApply_IntentConsumedWithoutServeDeps daemon-only 快照（dashboard 未
// 运行）：未装配 ServeLifecycle 的调用方也能恢复 daemon（不涉及 dashboard）。
func TestApply_IntentConsumedWithoutServeDeps(t *testing.T) {
	svc, sess, binPath, _ := makeApplyInstallService(t, false)
	writeIntentFor(t, binPath, true, false, "")

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应按恢复结束, got %+v", got)
	}
	if sess.startCalls != 1 || sess.startPaths[0] != binPath {
		t.Errorf("daemon 应恢复, got (%d, %v)", sess.startCalls, sess.startPaths)
	}
}

// TestApply_CorruptIntentBlocksUpdate 损坏的快照（无法辨识）：报错保留文件
// 要求人工处理，绝不照做（错误的快照照做会启动用户没有启动的服务）。
func TestApply_CorruptIntentBlocksUpdate(t *testing.T) {
	svc, _, binPath, _ := makeApplyInstallService(t, false)
	svc.ServeLifecycle = &fakeServeLifecycle{}
	if err := os.WriteFile(intentFilePath(binPath), []byte("{not-json"), 0600); err != nil {
		t.Fatalf("写损坏快照: %v", err)
	}

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("损坏快照必须报错")
	}
	if !strings.Contains(err.Error(), "manual handling") {
		t.Errorf("错误应指向人工处理, got %v", err)
	}
	if data, rerr := os.ReadFile(intentFilePath(binPath)); rerr != nil || string(data) != "{not-json" {
		t.Errorf("损坏快照必须保留供人工处理, got (%q, %v)", data, rerr)
	}
}
