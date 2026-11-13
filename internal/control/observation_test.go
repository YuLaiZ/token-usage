package control

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

func testObservationCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{DataDir: t.TempDir()}
}

// 受控停止意图：同实例才写；失败清除；确认锁释放后落定 stopped 并结束 active；
// 已有同实例异常证据时保留异常证据；文件缺失时不创建。
func TestControlledStopObservation(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)

	// 文件缺失：所有写入均为 no-op，且不创建文件。
	requestControlledStop(cfg, "inst-1", now)
	clearControlledStopIntent(cfg, "inst-1")
	finishControlledStop(cfg, "inst-1", now, nil, nil)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("观测文件缺失时停止路径不得创建文件: %v", err)
	}

	// 正常停止：写意图 → 落定 stopped → active 清空。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-1", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	requestControlledStop(cfg, "inst-other", now) // 不匹配：不写
	obs, _ := runmeta.ReadObservation(path)
	if obs.Active.StopRequestedAt != "" {
		t.Fatal("非本实例不得写停止意图")
	}
	requestControlledStop(cfg, "inst-1", now)
	obs, _ = runmeta.ReadObservation(path)
	if obs.Active.StopRequestedAt != now.Format(time.RFC3339) {
		t.Fatalf("停止意图未写入: %+v", obs.Active)
	}
	finishControlledStop(cfg, "inst-1", now.Add(time.Second), nil, nil)
	obs, _ = runmeta.ReadObservation(path)
	if obs.Active != nil {
		t.Fatalf("停止结果落定后 active 应清空: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != "inst-1" {
		t.Fatalf("正常停止应记 stopped: %+v", obs.LastExit)
	}

	// 停止失败清除意图。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-2", StartedAt: now.Format(time.RFC3339), StopRequestedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	clearControlledStopIntent(cfg, "inst-2")
	obs, _ = runmeta.ReadObservation(path)
	if obs.Active.StopRequestedAt != "" {
		t.Fatalf("失败后应清除未完成的意图: %+v", obs.Active)
	}

	// 已有同实例异常证据：停止结果不得用 stopped 覆盖。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-3", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-3", Kind: runmeta.ExitKindFailed,
			DetectedAt: now.Format(time.RFC3339), Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-3", now.Add(2*time.Second), nil, nil)
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("已知异常证据必须保留: %+v", obs.LastExit)
	}
	if obs.Active != nil {
		t.Fatalf("active 仍应结束: %+v", obs.Active)
	}
}

// 同实例的故障仅存于数据库（退出摘要写失败、故障已入库）时，受控停止不得
// 写成 stopped：保留库中的已知异常结论，只结束 active；补采确认结案后同样
// 的受控停止才能落定 stopped。
func TestFinishControlledStopKeepsPersistedSameInstanceFailure(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	detail := db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: now.Add(-time.Minute).Format(time.RFC3339), Source: "claude"}
	d, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-12", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// 观察文件：active=B（退出摘要写失败，last_exit 没有 B 的异常记录）。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfg, id)
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "claude" {
		t.Fatalf("仅存于库的同实例故障应写回 last_exit 保留归属: %+v", obs.LastExit)
	}
	if obs.LastExit.OccurredAt != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("应保留库中故障的发生时刻: %+v", obs.LastExit)
	}

	// 补采确认结案（异常已恢复）后，同样的受控停止可以落定 stopped。
	ro, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmMonitorInterruption(ctx, ro, "2026-11-12", "claude", "Claude 目录监听失败", detail); err != nil {
		ro.Close()
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(2*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfg, id)
	})
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("已结案后受控停止应落定 stopped: %+v", obs.LastExit)
	}
}

// 同实例库内未结案故障优先于其他代证据：查到时把故障摘要写回 last_exit
// （保留本次退出归属，不改写/迁移其他代证据）；库文件不存在时可安全落定
// stopped；库存在但不可读时写回原因未知的归属记录。
func TestFinishControlledStopKeepsPreviousEvidenceWhenSameInstanceUnresolved(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	d, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-12", "codex", "Codex 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: now.Add(-time.Minute).Format(time.RFC3339), Source: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfg, id)
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "codex" {
		t.Fatalf("同实例库内故障应写回 last_exit 保留归属: %+v", obs.LastExit)
	}
	// 其他代 A 的 failed 未持久化（evidencePersisted=nil）：必须移入 pending，
	// 缺口不能因写回被静默丢弃。
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("未持久化的其他代证据应移入 pending: %+v", obs.PendingExits)
	}

	// 对照：其他代证据已持久化时可安全丢弃。
	cfgPersisted := testObservationCfg(t)
	pathPersisted := runmeta.ObservationPath(cfgPersisted.DataDir)
	if err := runmeta.WriteObservation(pathPersisted, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfgPersisted, "inst-B", now.Add(time.Minute), func(string) bool { return true }, func(id string) (monitorFailure, monitorFailureState) {
		return monitorFailure{Kind: "failed", Source: "codex", OccurredAt: now.Add(-time.Minute).Format(time.RFC3339), Summary: "Codex 目录监听失败"}, monitorFailureFound
	})
	obsPersisted, err := runmeta.ReadObservation(pathPersisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(obsPersisted.PendingExits) != 0 {
		t.Fatalf("已持久化的其他代证据不应进入 pending: %+v", obsPersisted.PendingExits)
	}
	if obsPersisted.LastExit == nil || obsPersisted.LastExit.InstanceID != "inst-B" || obsPersisted.LastExit.Source != "codex" {
		t.Fatalf("应写回本次故障归属: %+v", obsPersisted.LastExit)
	}
	if obsPersisted.LastExit.Summary != "Codex 目录监听失败" || obsPersisted.LastExit.OccurredAt != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("写回应保留库摘要与发生时刻: %+v", obsPersisted.LastExit)
	}

	// 库文件不存在：没有持久故障可查，安全落定 stopped。
	cfgNoDB := testObservationCfg(t)
	pathNoDB := runmeta.ObservationPath(cfgNoDB.DataDir)
	if err := runmeta.WriteObservation(pathNoDB, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-E", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfgNoDB, "inst-E", now.Add(2*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfgNoDB, id)
	})
	obs, _ = runmeta.ReadObservation(pathNoDB)
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != "inst-E" {
		t.Fatalf("库文件不存在时应落定 stopped: %+v", obs.LastExit)
	}

	// 库存在但不可读（损坏）：已确认的受控停止按正常停止落定 stopped，不报
	// 中断；观察文件里其他代的未持久化 failed 证据同时移入 pending。
	cfgCorrupt := testObservationCfg(t)
	pathCorrupt := runmeta.ObservationPath(cfgCorrupt.DataDir)
	if err := os.WriteFile(filepath.Join(cfgCorrupt.DataDir, "usage.db"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteObservation(pathCorrupt, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-F", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfgCorrupt, "inst-F", now.Add(3*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfgCorrupt, id)
	})
	obs, _ = runmeta.ReadObservation(pathCorrupt)
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-F" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("受控停止已确认时库不可读应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("库不可读时其他代未持久化证据必须移入 pending: %+v", obs.PendingExits)
	}
}

// 数据库不可读但受控停止已确认（锁已释放、无异常证据）时按正常停止落定，
// CLI 与用量页的共享读端不得报中断。
func TestFinishControlledStopUnavailableDBRecordsConfirmedStop(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "usage.db"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 受控停止意图已记录（子进程未自行写退出摘要）。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339), StopRequestedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(30*time.Second), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfg, id)
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("受控停止已确认时应落定 stopped: %+v", obs.LastExit)
	}
	col := daemonstatus.Load(context.Background(), cfg.DataDir, daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, nil, now.Add(time.Minute))
	if col.State != daemonstatus.StateStopped {
		t.Fatalf("受控停止不应报中断: %+v", col)
	}
	if col.Interruption != nil {
		t.Fatalf("不应有 interruption: %+v", col.Interruption)
	}
}

// 上一次正常停止摘要与本次库内故障落在同一秒时，收尾写回故障归属后读端仍
// 应判为 interrupted，而不是退回秒级时间排序把故障降为历史缺口。
func TestFinishControlledStopSameSecondKeepsFailureAttribution(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	t0 := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	detail := db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: t0.Format(time.RFC3339), Source: "claude"}
	d, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-12", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// A 正常停止摘要与 B 的库内故障同一秒；B 的退出摘要写失败。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: t0.Add(-time.Hour).Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindStopped,
			DetectedAt: t0.Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", t0.Add(time.Second), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfg, id)
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("收尾应写回本次故障归属: %+v", obs.LastExit)
	}
	ro, err := db.OpenReadOnly(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	col := daemonstatus.Load(ctx, cfg.DataDir, daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, ro, t0.Add(2*time.Second))
	if col.State != daemonstatus.StateInterrupted {
		t.Fatalf("收尾后读端仍应判为 interrupted: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应显示本次故障来源: %+v", col.Interruption)
	}
}

// 写回摘要的归一与合并：库摘要缺字段时以观察文件同实例证据为底，非法 kind、
// 缺失/非法时刻与空摘要都必须归一为可写入的合法记录。
func TestFinishControlledStopWriteBackNormalization(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	occurred := now.Add(-time.Hour).Format(time.RFC3339)
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred, DetectedAt: occurred,
			Source: "claude", Summary: "观察文件摘要",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return monitorFailure{Kind: "crashed", OccurredAt: "2026/11/12 15:00:00"}, monitorFailureFound
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("非法 kind 应回退到观察文件的有效值: %+v", obs.LastExit)
	}
	if obs.LastExit.Source != "claude" || obs.LastExit.OccurredAt != occurred || obs.LastExit.Summary != "观察文件摘要" {
		t.Fatalf("库摘要缺字段时不得降级观察文件证据: %+v", obs.LastExit)
	}

	// 无观察文件同实例证据、库摘要 kind/summary 缺失且时刻非法：写回仍须合法。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-C", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-C", now.Add(2*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return monitorFailure{OccurredAt: "not-a-time"}, monitorFailureFound
	})
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-C" || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown || obs.LastExit.OccurredAt != "" {
		t.Fatalf("非法摘要应归一为原因未知且时刻留空: %+v", obs.LastExit)
	}
	if obs.LastExit.Summary == "" {
		t.Fatalf("summary 必须有兜底: %+v", obs.LastExit)
	}

	// 库摘要 kind 合法（interrupted_unknown）时优先于观察文件的 failed：
	// 以库证据为准确认本次退出的原因级别。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-D", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-D", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred, DetectedAt: occurred,
			Source: "claude", Summary: "观察文件摘要",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-D", now.Add(3*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return monitorFailure{Kind: runmeta.ExitKindInterruptedUnknown, Summary: "库原因未知"}, monitorFailureFound
	})
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-D" || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown || obs.LastExit.Summary != "库原因未知" {
		t.Fatalf("库摘要的合法 kind 应优先: %+v", obs.LastExit)
	}
}

// 库不可读时不得覆盖观察文件已有的同实例可信摘要：已记录的 failed 保留
// 来源与时刻，已记录的正常 stopped 不会被改成异常中断。
func TestFinishControlledStopUnavailableDBKeepsTrustedSummary(t *testing.T) {
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	corrupt := func(cfg *config.Config) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "usage.db"), []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 已记录 failed：保留来源与时刻。
	cfgFailed := testObservationCfg(t)
	corrupt(cfgFailed)
	pathFailed := runmeta.ObservationPath(cfgFailed.DataDir)
	occurred := now.Add(-time.Minute).Format(time.RFC3339)
	if err := runmeta.WriteObservation(pathFailed, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred, DetectedAt: occurred,
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfgFailed, "inst-B", now.Add(time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfgFailed, id)
	})
	obs, err := runmeta.ReadObservation(pathFailed)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "claude" ||
		obs.LastExit.Summary != "Claude 目录监听失败" || obs.LastExit.OccurredAt != occurred {
		t.Fatalf("库不可读时不得覆盖同实例的 failed 摘要: %+v", obs.LastExit)
	}

	// 已记录 stopped：不得改成异常中断。
	cfgStopped := testObservationCfg(t)
	corrupt(cfgStopped)
	pathStopped := runmeta.ObservationPath(cfgStopped.DataDir)
	if err := runmeta.WriteObservation(pathStopped, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-C", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-C", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfgStopped, "inst-C", now.Add(2*time.Minute), nil, func(id string) (monitorFailure, monitorFailureState) {
		return productionMonitorFailure(cfgStopped, id)
	})
	obs, err = runmeta.ReadObservation(pathStopped)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != "inst-C" {
		t.Fatalf("库不可读时不得把同实例 stopped 改成异常中断: %+v", obs.LastExit)
	}
}

// 受控停止时观察文件残留其他代 active：daemon lock 已释放说明其必然已退出，
// 将其转成历史 unknown 移入 pending 后落定本次 stopped，避免残留 active 压过
// 最近一次退出。
func TestFinishControlledStopMigratesStaleActive(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-old", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-new", now, nil, func(id string) (monitorFailure, monitorFailureState) {
		return monitorFailure{}, monitorFailureAbsent
	})
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("残留 active 应清除: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-new" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("受控停止应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-old" || obs.PendingExits[0].Kind != runmeta.ExitKindInterruptedUnknown {
		t.Fatalf("残留 active 应转为历史 unknown 移入 pending: %+v", obs.PendingExits)
	}
	col := daemonstatus.Load(context.Background(), cfg.DataDir, daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, nil, now.Add(time.Minute))
	if col.State != daemonstatus.StateStopped {
		t.Fatalf("残留 active 不得压过最近一次停止: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("历史未知中断应作为缺口提示保留: %+v", col.Interruption)
	}
}

// 其他代 failed 证据的持久化判定必须在观察文件写锁外完成：锁被占用期间
// 预取查询并行进行，总耗时接近两者较大值而非相加。
func TestFinishControlledStopPersistedLookupOutsideLock(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = lock.Unlock()
	}()
	// 模拟库查询耗时 200ms：锁外执行时与 150ms 的锁等待重叠。
	persisted := func(string) bool {
		time.Sleep(200 * time.Millisecond)
		return true
	}
	start := time.Now()
	finishControlledStop(cfg, "inst-B", now, persisted, nil)
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("持久化判定必须在观察文件锁外完成（耗时应接近 max 而非相加），耗时 %v", elapsed)
	}
}

// 观测文件损坏：停止路径不 panic、不覆盖损坏文件（无法安全合并）。
func TestControlledStopCorruptFile(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	requestControlledStop(cfg, "inst-1", time.Now())
	clearControlledStopIntent(cfg, "inst-1")
	finishControlledStop(cfg, "inst-1", time.Now(), nil, nil)
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil || string(data) != "{broken" {
		t.Fatalf("损坏文件不应被停止路径改写: %q err=%v", string(data), err)
	}
}

// 控制端停止时的其他代 failed 证据：可移入 pending_exits 或已持久化时不再
// 阻止本次 stopped 落定；来源未知的 failed 也移入 pending 保留（读端继续
// 提示、补写端跳过），最近一次停止与历史缺口都不丢。
func TestFinishControlledStopPreservesOtherGenerationFailure(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339),
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source:     "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(time.Minute), nil, nil)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("其他代证据可移入 pending 时本次 stopped 应落定: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" || obs.PendingExits[0].Source != "claude" {
		t.Fatalf("历史缺口应保留在 pending_exits: %+v", obs.PendingExits)
	}

	// 来源未知的其他代 failed：移入 pending_exits 保留（读端继续提示、补写端
	// 按来源规则跳过），本次 stopped 照常落定。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B2", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-X", Kind: runmeta.ExitKindFailed,
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "采集守护进程异常退出",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B2", now.Add(90*time.Second), nil, nil)
	obs, _ = runmeta.ReadObservation(path)
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B2" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("来源未知证据移入 pending 后应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-X" {
		t.Fatalf("来源未知的 failed 证据必须保留在 pending: %+v", obs.PendingExits)
	}

	// 对照：没有未结证据时正常停止照常落定 stopped。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-D", StartedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-D", now.Add(2*time.Minute), nil, nil)
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != "inst-D" {
		t.Fatalf("无未结证据时应照常落定 stopped: %+v", obs.LastExit)
	}
}

// 其他代的 failed 证据已持久化到库时，正常停止必须落定 stopped：缺口由库
// 的未结案事件继续提示，观察文件不再是唯一来源。强制停止路径尤其依赖这条
// 收尾（被强杀的实例不会自行写 stopped）。
func TestFinishControlledStopRecordsStoppedWhenPreviousFailurePersisted(t *testing.T) {
	cfg := testObservationCfg(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)

	d, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339),
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source:     "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishControlledStop(cfg, "inst-B", now.Add(time.Minute), func(prev string) bool {
		return productionMonitorEvidencePersisted(cfg, prev)
	}, nil)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("被停止实例的 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("旧故障已持久化时应落定本次 stopped: %+v", obs.LastExit)
	}
}

// 库文件缺失/不可读时按未持久化保守处理：观察文件里的唯一证据必须保留。
func TestProductionMonitorEvidencePersistedUnavailable(t *testing.T) {
	cfg := testObservationCfg(t)
	if productionMonitorEvidencePersisted(cfg, "inst-A") {
		t.Fatal("库文件缺失时应按未持久化处理")
	}

	// 库文件存在但不可查询（损坏）：同样保守按未持久化处理。
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "usage.db"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if productionMonitorEvidencePersisted(cfg, "inst-A") {
		t.Fatal("库不可查询时应按未持久化处理")
	}
}
