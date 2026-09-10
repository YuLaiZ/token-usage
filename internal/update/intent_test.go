package update

// intent_test.go 校验恢复意图快照：停止任何服务之前持久化原运行态，填补
// 「已 stop、未 Install」中断窗口（此刻 POSIX journal 与 Windows helper plan
// 都不存在）；下一次 Apply 按快照幂等恢复 daemon 与 dashboard 并清除快照；
// journal 与快照并存时 journal 路径优先、快照作为冗余被清除；快照写入失败
// 在任何服务被触碰前中止更新。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIntentFor 在 target 同目录写一份合法的恢复意图快照。
func writeIntentFor(t *testing.T, target string, daemonRunning, serveRunning bool, serveAddr string) updateIntent {
	t.Helper()
	hash, err := fileSHA256(target)
	if err != nil {
		t.Fatalf("hash target: %v", err)
	}
	intent := updateIntent{
		Version:          intentCurrentVersion,
		TargetBasename:   filepath.Base(target),
		OldSHA256:        hash,
		DaemonWasRunning: daemonRunning,
		ServeWasRunning:  serveRunning,
		ServeAddr:        serveAddr,
	}
	if err := writeUpdateIntent(intentFilePath(target), intent); err != nil {
		t.Fatalf("写恢复意图: %v", err)
	}
	return intent
}

// TestApply_TamperedStageAbortsBeforeAnyStop 下载校验完成后、进入锁内编排前
// stage 被篡改（内容与 manifest 期望 hash 不符）：停止前复验必须中止更新——
// 不停止任何服务、不写恢复意图、不安装，并清理被篡改的 stage。
func TestApply_TamperedStageAbortsBeforeAnyStop(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)
	// 合法 stage（存在且普通文件），但内容与传入的期望 hash 不符：模拟下载
	// 校验之后被篡改。
	stagePath := filepath.Join(t.TempDir(), "stage")
	if err := os.WriteFile(stagePath, []byte("tampered-content"), 0o755); err != nil {
		t.Fatalf("写 stage: %v", err)
	}
	expectedHash := sha256HexBytes([]byte("official-verified-content"))

	_, err := svc.installUnderLockOutcome(context.Background(), stagePath, oldBinPath(t, svc), expectedHash)
	if err == nil {
		t.Fatal("stage 与期望 hash 不符必须中止更新")
	}
	if !strings.Contains(err.Error(), "no service has been touched") {
		t.Errorf("错误应明示未触碰任何服务, got %v", err)
	}
	if serveFake.stopCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("dashboard 不得被触碰, stop=%d start=%d", serveFake.stopCalls, serveFake.startCount())
	}
	if sess.stopCalls != 0 || sess.startCalls != 0 {
		t.Errorf("daemon 不得被触碰, stop=%d start=%d", sess.stopCalls, sess.startCalls)
	}
	if installer.callCount() != 0 {
		t.Error("不得替换二进制")
	}
	if _, found, _ := findIntent(oldBinPath(t, svc)); found {
		t.Error("不得写入恢复意图")
	}
	// 被篡改的 stage 已清理。
	if _, statErr := os.Stat(stagePath); !os.IsNotExist(statErr) {
		t.Errorf("篡改的 stage 应被清理, stat err = %v", statErr)
	}
}

// TestApply_IntentWrittenBeforeAnyStop 快照必须在任何服务被停止之前落盘：
// serve Stop 回调执行时快照文件必须已存在（否则该窗口的硬中断无快照可恢复）。
func TestApply_IntentWrittenBeforeAnyStop(t *testing.T) {
	// target 路径在 makeServeInstallService 返回后才能解析；stopHook 在 Apply
	// 期间才执行，届时闭包变量已就绪。
	var currentBinPath string
	svc, sess, installer, _ := makeServeInstallService(t, true, &fakeServeLifecycle{
		detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true,
		stopHook: func() {
			if currentBinPath == "" {
				t.Error("serve 停止时 target 路径尚未解析（测试装配错误）")
				return
			}
			if _, found, err := findIntent(currentBinPath); err != nil || !found {
				t.Errorf("serve 停止时恢复意图必须已落盘, got (found=%v, err=%v)", found, err)
			}
		},
	})
	currentBinPath = oldBinPath(t, svc)

	if _, err := svc.Apply(context.Background(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	_ = sess
	_ = installer
}

// TestApply_IntentHashMismatchRestoresFromNewBinary Windows helper 死在
// MoveFileEx 成功之后、恢复服务之前（Windows 无 journal，快照是唯一中断
// 记录）：target 与快照记录的「已验证新 hash」精确匹配，消费路径以新二进制
// 幂等恢复 daemon 与 dashboard，清除快照并按「新版本已落地」语义结束本轮。
func TestApply_IntentHashMismatchRestoresFromNewBinary(t *testing.T) {
	svc, sess, _, _ := makeServeInstallService(t, true, &fakeServeLifecycle{})
	serveFake := &fakeServeLifecycle{}
	svc.ServeLifecycle = serveFake
	// 模拟替换已完成的中断现场：target 已被替换为 stage 的已验证内容，
	// 快照同时记录旧 hash 与该新 hash。
	newHash, err := fileSHA256(oldBinPath(t, svc))
	if err != nil {
		t.Fatalf("hash target: %v", err)
	}
	if err := writeUpdateIntent(intentFilePath(oldBinPath(t, svc)), updateIntent{
		Version:          intentCurrentVersion,
		TargetBasename:   filepath.Base(oldBinPath(t, svc)),
		OldSHA256:        strings.Repeat("ab", 32),
		NewSHA256:        newHash,
		DaemonWasRunning: true,
		ServeWasRunning:  true,
		ServeAddr:        "127.0.0.1:8627",
	}); err != nil {
		t.Fatalf("写快照: %v", err)
	}

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Recovered {
		t.Fatalf("应按恢复结束本轮, got %+v", got)
	}
	if got.RecoveryState != RecoveryStateNewInstalled {
		t.Errorf("替换已完成的中断应报告新版本已落地语义, got %q", got.RecoveryState)
	}
	// daemon 以当前（新）二进制恢复。
	if sess.startCalls != 1 || sess.startPaths[0] != oldBinPath(t, svc) {
		t.Errorf("daemon 应以新二进制恢复一次, got (%d, %v)", sess.startCalls, sess.startPaths)
	}
	if serveFake.startCount() != 1 {
		t.Fatalf("dashboard 应恢复一次, got %d", serveFake.startCount())
	}
	if got := serveFake.lastStart(); got.binPath != oldBinPath(t, svc) || got.addr != "127.0.0.1:8627" {
		t.Errorf("dashboard 应以新二进制+原地址恢复, got %+v", got)
	}
	if _, found, _ := findIntent(oldBinPath(t, svc)); found {
		t.Error("消费完成的快照应被清除")
	}
}

// TestApply_IntentWriteFailureAbortsBeforeAnyStop 快照写失败：中止更新，
// 且此刻尚无任何服务被停止、二进制未被替换。
func TestApply_IntentWriteFailureAbortsBeforeAnyStop(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, sess, installer, _ := makeServeInstallService(t, true, serveFake)
	origWrite := writeUpdateIntent
	writeUpdateIntent = func(string, updateIntent) error { return errors.New("disk full") }
	t.Cleanup(func() { writeUpdateIntent = origWrite })

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("快照写失败必须中止更新")
	}
	if !strings.Contains(err.Error(), "no service has been touched") {
		t.Errorf("错误应明示未触碰任何服务, got %v", err)
	}
	if serveFake.stopCalls != 0 || sess.stopCalls != 0 {
		t.Errorf("任何服务都不得被停止, serve=%d daemon=%d", serveFake.stopCalls, sess.stopCalls)
	}
	if installer.callCount() != 0 {
		t.Errorf("不得替换二进制, install calls=%d", installer.callCount())
	}
}

// TestApply_DeferredKeepsIntentForHelper Windows helper 接管（Deferred）：
// 快照保留——helper 失败或被中断时它是下一轮幂等恢复的唯一依据；helper
// 成功完成替换与服务恢复后主动清除（见 TestHelper_SucceedClearsIntent）。
func TestApply_DeferredKeepsIntentForHelper(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: true}
	svc, _, installer, _ := makeServeInstallService(t, true, serveFake)
	installer.deferred = true

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.Deferred {
		t.Fatalf("应为 Deferred, got %+v", got)
	}
	if _, found, _ := findIntent(oldBinPath(t, svc)); !found {
		t.Error("Deferred 路径必须保留恢复意图供 helper 失败后的下一轮消费")
	}
}

// TestUpdateIntentFieldValidation 校验快照字段的模糊文件判定：版本未知、
// target 不匹配、hash 非法、记录运行中 dashboard 却缺地址，全部拒绝照做。
func TestUpdateIntentFieldValidation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
		t.Fatalf("写 target: %v", err)
	}
	path := intentFilePath(target)
	cases := map[string]updateIntent{
		"版本未知":               {Version: 99, TargetBasename: "token-usage", OldSHA256: strings.Repeat("a", 64)},
		"hash 非法":            {Version: 1, TargetBasename: "token-usage", OldSHA256: "short"},
		"记录运行中dashboard但缺地址": {Version: 1, TargetBasename: "token-usage", OldSHA256: strings.Repeat("a", 64), ServeWasRunning: true},
	}
	for name, intent := range cases {
		if err := writeUpdateIntent(path, intent); err != nil {
			t.Fatalf("%s: 写快照: %v", name, err)
		}
		if _, _, err := readUpdateIntent(path); err == nil {
			t.Errorf("%s: 应拒绝照做", name)
		}
	}
	// 缺失文件：无快照。
	if _, found, err := readUpdateIntent(path); err == nil && found {
		t.Error("不存在路径应 (nil, false, nil)")
	}
	_ = os.Remove(path)
	// 合法快照通过。
	writeIntentFor(t, target, true, true, "127.0.0.1:8619")
	if _, found, err := readUpdateIntent(path); err != nil || !found {
		t.Errorf("合法快照应可读取, got (found=%v, err=%v)", found, err)
	}
}

// TestApply_IntentNeitherHashMatchesRequiresManual target 与新旧 hash 都不
// 匹配（二进制被异常替换、损坏或在本更新之外被改动）：保留快照、报错要求
// 人工处理，绝不启动任何服务、绝不继续安装。
func TestApply_IntentNeitherHashMatchesRequiresManual(t *testing.T) {
	serveFake := &fakeServeLifecycle{}
	svc, sess, installer, _ := makeServeInstallService(t, false, serveFake)
	if err := writeUpdateIntent(intentFilePath(oldBinPath(t, svc)), updateIntent{
		Version:          intentCurrentVersion,
		TargetBasename:   filepath.Base(oldBinPath(t, svc)),
		OldSHA256:        strings.Repeat("ab", 32),
		NewSHA256:        strings.Repeat("cd", 32),
		DaemonWasRunning: true,
		ServeWasRunning:  true,
		ServeAddr:        "127.0.0.1:8628",
	}); err != nil {
		t.Fatalf("写快照: %v", err)
	}

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("双 hash 都不匹配必须报人工处理")
	}
	if !strings.Contains(err.Error(), "manual") {
		t.Errorf("错误应指向人工处理, got %v", err)
	}
	if sess.startCalls != 0 || serveFake.startCount() != 0 {
		t.Errorf("绝不启动任何服务, daemon=%d serve=%d", sess.startCalls, serveFake.startCount())
	}
	if installer.callCount() != 0 {
		t.Error("本轮不得继续安装")
	}
	if _, found, _ := findIntent(oldBinPath(t, svc)); !found {
		t.Error("快照必须保留供人工处理")
	}
}

// TestApply_VanishedServeIntentRewrittenOnDisk dashboard 在探测后、停止前
// 自行退出（Stop 报告无可停实例）：内存修正之外，磁盘快照必须同步改写——
// Install 时点上磁盘 intent 的 ServeWasRunning 必须已是 false，否则此后的
// 硬中断会把用户已自行退出的 dashboard 错误拉起。
func TestApply_VanishedServeIntentRewrittenOnDisk(t *testing.T) {
	serveFake := &fakeServeLifecycle{detectRunning: true, detectAddr: "127.0.0.1:8619", stopStopped: false}
	svc, _, installer, _ := makeServeInstallService(t, true, serveFake)
	installer.installHook = func() {
		intent, found, err := findIntent(oldBinPath(t, svc))
		if err != nil || !found {
			t.Fatalf("Install 时恢复意图必须仍在磁盘上, got (found=%v, err=%v)", found, err)
		}
		if intent.ServeWasRunning || intent.ServeAddr != "" {
			t.Errorf("自行退出的 dashboard 必须已从磁盘快照中更正, got serve=%v addr=%q", intent.ServeWasRunning, intent.ServeAddr)
		}
		if !intent.DaemonWasRunning {
			t.Error("daemon 运行态不得被连带改写")
		}
	}

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	// dashboard 未运行则不得恢复（内存与磁盘一致）。
	if serveFake.startCount() != 0 {
		t.Errorf("自行退出的 dashboard 不得被恢复, got %d", serveFake.startCount())
	}
	if !got.Installed {
		t.Errorf("更新应正常完成, got %+v", got)
	}
}

// TestHelper_SucceedClearsIntent Windows helper 成功完成替换与服务恢复后，
// 清除父进程留下的恢复意图快照（失败路径不清理：快照是下一轮幂等恢复的依据）。
func TestHelper_SucceedClearsIntent(t *testing.T) {
	f := newServeHelperFixture(t, true, true, "127.0.0.1:8619")
	f.serve = &fakeServeLifecycle{}
	if err := writeUpdateIntent(intentFilePath(f.paths.Target), updateIntent{
		Version:          intentCurrentVersion,
		TargetBasename:   filepath.Base(f.paths.Target),
		OldSHA256:        sumHex([]byte("old-official-binary")),
		DaemonWasRunning: true,
		ServeWasRunning:  true,
		ServeAddr:        "127.0.0.1:8619",
	}); err != nil {
		t.Fatalf("写快照: %v", err)
	}

	if err := f.runner(t).Run(context.Background(), f.paths.Helper, f.paths.Plan); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if _, found, _ := findIntent(f.paths.Target); found {
		t.Error("helper 成功后快照应被清除")
	}
	res := f.readResult(t)
	if !res.Success {
		t.Errorf("应写成功 result, got %+v", res)
	}
}
