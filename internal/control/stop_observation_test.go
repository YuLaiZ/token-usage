//go:build !windows

package control

// 受控停止的观测接线：stopLocked 在发信号前记录停止意图、确认锁释放后落定
// 正常停止结果并结束 active；清理元数据失败时停止本身已成立，仍要落定
// stopped（否则残留意图过期后读端会把手动停止误报为中断）。
// 删除/跳过这三个调用中的任一个，本测试转红。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

const stopObsInstanceID = "inst-stop-obs"

// runStopWithObservation 构造一个「运行中的手工 posix daemon」并执行 Stop。
func runStopWithObservation(t *testing.T, cleanupErr error) (*config.Config, error) {
	t.Helper()
	f := newFakeDeps()
	f.dlock.inner.running = true
	f.pid.pid = 3333
	f.pid.instance = stopObsInstanceID
	f.state.readErr = nil
	f.state.state = runmeta.RuntimeState{PID: 3333, InstanceID: stopObsInstanceID, MonitorReady: true, CatchUp: "succeeded"}
	f.service.statusResult = false // 非托管：直接 SIGTERM
	// isRunning: check#1 (inspect)=true → SIGTERM；check#2 (waitDaemonRelease)=false → 释放成功。
	f.dlock.inner.readyAfter = 1
	f.dlock.inner.runningWhenReady = false
	f.metadata.err = cleanupErr
	dataDir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dataDir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: stopObsInstanceID, StartedAt: time.Now().Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestProcessManager(t, t.TempDir(), f)
	cfg := newConfigWith(dataDir)
	loader := func() (*config.Config, error) { return cfg, nil }
	_, err := m.Stop(context.Background(), loader)
	return cfg, err
}

func TestStop_RecordsObservationEvidence(t *testing.T) {
	cfg, err := runStopWithObservation(t, nil)
	if err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	obs, rerr := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if obs.Active != nil {
		t.Fatalf("停止成功后 active 应清空: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != stopObsInstanceID {
		t.Fatalf("停止成功后应落定 stopped: %+v", obs.LastExit)
	}
}

func TestStop_CleanupFailureStillRecordsStopped(t *testing.T) {
	cfg, err := runStopWithObservation(t, errors.New("cleanup boom"))
	if err == nil {
		t.Fatal("清理元数据失败应返回错误")
	}
	obs, rerr := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if obs.Active != nil {
		t.Fatalf("锁已释放的停止必须结束 active: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("锁已释放的停止必须落定 stopped（不得留下过期意图被误报为中断）: %+v", obs.LastExit)
	}
}
