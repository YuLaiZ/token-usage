package update

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// apply_runstate_test.go 校验 ApplyResult.DaemonWasRunning 的填充合同：
// 锁内 Inspect 判定的替换前运行态沿 installOutcome 传出 Apply 边界，
// Installed 与 Deferred（Windows helper 接管）两路径都填充；
// 无更新等未触碰 daemon 的路径保持零值。

// TestApply_DaemonWasRunningFilledFromInspect 替换前运行态 × 安装结果矩阵。
func TestApply_DaemonWasRunningFilledFromInspect(t *testing.T) {
	cases := []struct {
		name          string
		running       bool
		deferred      bool
		wantInstalled bool
		wantDeferred  bool
	}{
		{"running installed", true, false, true, false},
		{"stopped installed", false, false, true, false},
		{"running deferred", true, true, false, true},
		{"stopped deferred", false, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := makeService(t)
			sess := &fakeControlSession{state: control.RuntimeState{Running: c.running}}
			svc.ControlManager = &fakeControlManager{session: sess}
			installer := newFakeInstaller()
			installer.deferred = c.deferred
			svc.Installer = installer
			svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

			got, err := svc.Apply(context.Background(), ApplyOptions{})
			if err != nil {
				t.Fatalf("Apply err=%v", err)
			}
			if got.Installed != c.wantInstalled || got.Deferred != c.wantDeferred {
				t.Fatalf("前置失败：Installed=%v Deferred=%v，want %v/%v", got.Installed, got.Deferred, c.wantInstalled, c.wantDeferred)
			}
			if got.DaemonWasRunning != c.running {
				t.Errorf("DaemonWasRunning=%v，want %v（锁内 Inspect 结果应传出 Apply 边界）", got.DaemonWasRunning, c.running)
			}
		})
	}
}

// TestApply_DaemonWasRunningZeroOnNoUpdate 无更新路径不触碰 daemon，
// DaemonWasRunning 保持零值（渲染不依赖该字段的分支）。
func TestApply_DaemonWasRunningZeroOnNoUpdate(t *testing.T) {
	svc := makeService(t)
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	rc.byTag[""] = makeCurrentRelease("v0.1.0")
	rc.release = makeCurrentRelease("v0.1.0")
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	svc.Installer = newFakeInstaller()
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("前置失败：本用例应无更新")
	}
	if got.DaemonWasRunning {
		t.Error("无更新路径 DaemonWasRunning 应为零值")
	}
}
