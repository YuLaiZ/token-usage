package daemonstatus

import (
	"context"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

func strPtr(v string) *string { return &v }

// collection.state 与 interruption 的判定矩阵（方案 §2 末段）：当前实例优先、
// 只在确认未运行读退出证据、中断提示独立于状态。
func TestEvaluateStateMatrix(t *testing.T) {
	base := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)
	failed := &ExitEvidence{InstanceID: "inst-1", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:35:02+08:00", Source: "claude", Summary: "Claude 目录监听失败"}
	stopped := &ExitEvidence{InstanceID: "inst-1", Kind: "stopped", DetectedAt: "2026-11-11T15:35:00+08:00", Summary: "采集已停止"}

	cases := []struct {
		name                 string
		in                   Inputs
		wantState            string
		wantCatch            string // "" = null
		wantInterr           bool
		wantInterruptionKind string // 非空时校验 interruption.kind
	}{
		{
			name:      "running with ready monitors",
			in:        Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded", NowUnixNano: base.UnixNano()},
			wantState: StateRunning, wantCatch: "succeeded",
		},
		{
			name:      "running during catch-up",
			in:        Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "running", NowUnixNano: base.UnixNano()},
			wantState: StateRunning, wantCatch: "running",
		},
		{
			name:      "running before monitor ready",
			in:        Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: false, CatchUp: "", NowUnixNano: base.UnixNano()},
			wantState: StateStarting, wantCatch: "unknown",
		},
		{
			name:      "running without phase (liveness unknown)",
			in:        Inputs{Probe: ProbeRunning, PhaseAvailable: false, NowUnixNano: base.UnixNano()},
			wantState: StateUnknown, wantCatch: "unknown",
		},
		{
			name:      "probe unknown",
			in:        Inputs{Probe: ProbeUnknown, NowUnixNano: base.UnixNano()},
			wantState: StateUnknown, wantCatch: "unknown",
		},
		{
			name:      "stopped without evidence",
			in:        Inputs{Probe: ProbeStopped, NowUnixNano: base.UnixNano()},
			wantState: StateStopped,
		},
		{
			name:      "stopped after recorded failure",
			in:        Inputs{Probe: ProbeStopped, ObservationReadable: true, LastExit: failed, NowUnixNano: base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true,
		},
		{
			name:      "stopped after recorded failure but resolved",
			in:        Inputs{Probe: ProbeStopped, ObservationReadable: true, LastExit: failed, LastExitResolved: true, NowUnixNano: base.UnixNano()},
			wantState: StateStopped,
		},
		{
			name:      "stopped after normal stop",
			in:        Inputs{Probe: ProbeStopped, ObservationReadable: true, LastExit: stopped, NowUnixNano: base.UnixNano()},
			wantState: StateStopped,
		},
		{
			name: "stopped with leftover active (unknown interruption)",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-2", NowUnixNano: base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true,
		},
		{
			name: "stopped with leftover active and fresh stop intent",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-2", ActiveStopRequestedUnixNano: base.Add(-10 * time.Second).UnixNano(), NowUnixNano: base.UnixNano()},
			wantState: StateUnknown,
		},
		{
			name: "leftover active takes precedence over older last_exit",
			// 历史 last_exit（上一代正常停止）不能让本代强杀残留被误判为 stopped。
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-killed",
				LastExit:         &ExitEvidence{InstanceID: "inst-earlier", Kind: "stopped", DetectedAt: "2026-11-11T10:00:00+08:00", Summary: "采集已停止"},
				NowUnixNano:      base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true,
		},
		{
			name: "same-generation last_exit with leftover active maps by kind",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-x",
				LastExit:         &ExitEvidence{InstanceID: "inst-x", Kind: "stopped", DetectedAt: "2026-11-11T10:00:00+08:00", Summary: "采集已停止"},
				NowUnixNano:      base.UnixNano()},
			wantState: StateStopped,
		},
		{
			name: "running instance keeps previous unresolved interruption visible",
			in: Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "running",
				ObservationReadable: true, LastExit: failed, NowUnixNano: base.UnixNano()},
			wantState: StateRunning, wantCatch: "running", wantInterr: true,
		},
		{
			// 当前实例正常运行、本代无库证据：历史未结案缺口必须继续提示，
			// 不因最新一代正常而隐藏（state 仍是 running，不冒充本代中断）。
			name: "running instance keeps historical pending visible",
			in: Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded",
				ObservationReadable: true,
				LastExit:            &ExitEvidence{InstanceID: "inst-B", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				HistoricalPending:   &PendingInterruption{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:         base.UnixNano()},
			wantState: StateRunning, wantCatch: "succeeded", wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			name: "running instance does not override state with old exit",
			in: Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded",
				ObservationReadable: true, LastExit: stopped, NowUnixNano: base.UnixNano()},
			wantState: StateRunning, wantCatch: "succeeded",
		},
		{
			// 无任何实例证据（观察文件缺失/写失败）时，库中未结案事件是唯一线索。
			name: "pending interruption is the only evidence",
			in: Inputs{Probe: ProbeStopped,
				LatestPending: &PendingInterruption{InstanceID: "inst-3", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true,
		},
		{
			// A 代正常停止后 B 代故障只成功写库（观察文件写失败）：故障发生
			// 时刻晚于 last_exit 的事件属于最近一次退出，必须显示已知故障。
			name: "db event newer than last_exit wins as latest exit",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:      &ExitEvidence{InstanceID: "inst-A", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				LatestPending: &PendingInterruption{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-11T16:30:00+08:00", DetectedAt: "2026-11-11T16:30:01+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			// 回填的旧故障：故障发生在停止之前、入库却在停止之后——入库顺序
			// 不能当成退出顺序，状态保持 stopped，缺口作为历史提示保留。
			name: "backfilled older failure does not override latest stop",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:      &ExitEvidence{InstanceID: "inst-B", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				LatestPending: &PendingInterruption{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:00:00+08:00", DetectedAt: "2026-11-11T17:00:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateStopped, wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			// 故障发生时刻缺失：不足以确定顺序，不冒充最近一次退出。
			name: "pending without occurrence time stays historical",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:      &ExitEvidence{InstanceID: "inst-B", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				LatestPending: &PendingInterruption{InstanceID: "inst-A", Kind: "failed", DetectedAt: "2026-11-11T17:00:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateStopped, wantInterr: true,
		},
		{
			// 同一秒的两次退出：严格相等不构成"更晚"，按历史缺口处理。
			name: "same-second events are not ordered by ingest time",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:      &ExitEvidence{InstanceID: "inst-A", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				LatestPending: &PendingInterruption{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-11T16:00:00+08:00", DetectedAt: "2026-11-11T16:00:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateStopped, wantInterr: true,
		},
		{
			// 本代 active 残留且库中有同实例已知故障：已知故障优先于推导的原因未知。
			name: "same-generation db event beats unknown-reason inference",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-B",
				LatestPending:    &PendingInterruption{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-11T16:30:00+08:00", DetectedAt: "2026-11-11T16:30:01+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:      base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			name: "pending interruption kind empty treated as interrupted",
			in: Inputs{Probe: ProbeStopped,
				LatestPending: &PendingInterruption{InstanceID: "inst-3", Summary: "x"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true,
		},
		{
			// 重启后推导的原因未知不得掩盖同实例的库内已知故障。
			name: "known failure beats derived unknown",
			in: Inputs{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded",
				ObservationReadable:  true,
				LastExit:             &ExitEvidence{InstanceID: "inst-A", Kind: "interrupted_unknown", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已中断，退出原因未知"},
				LastExitKnownPending: &PendingInterruption{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00.000", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:          base.UnixNano()},
			wantState: StateRunning, wantCatch: "succeeded", wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			// 观察文件里未持久化的历史缺口：不参与 state，只作为提示兜底。
			name: "observation pending exit stays visible",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:    &ExitEvidence{InstanceID: "inst-B", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				PendingExit: &ExitEvidence{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano: base.UnixNano()},
			wantState: StateStopped, wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			// A 代故障未结案、B 代正常停止：状态反映最近一代（stopped），
			// 历史缺口只作为提示保留。
			name: "historical pending does not override latest stopped exit",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true,
				LastExit:      &ExitEvidence{InstanceID: "inst-B", Kind: "stopped", DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止"},
				LatestPending: &PendingInterruption{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:   base.UnixNano()},
			wantState: StateStopped, wantInterr: true, wantInterruptionKind: "failed",
		},
		{
			// B 代意外消失、A 代故障未结案：状态与摘要都取最近一代（原因未知），
			// 不得把 A 代的已知故障冒充为本次退出原因。
			name: "leftover active takes precedence over historical pending",
			in: Inputs{Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
				ActiveInstanceID: "inst-killed",
				LatestPending:    &PendingInterruption{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"},
				NowUnixNano:      base.UnixNano()},
			wantState: StateInterrupted, wantInterr: true, wantInterruptionKind: "interrupted_unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.in)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			switch {
			case tc.wantCatch == "":
				if got.CatchUp != nil {
					t.Fatalf("catch_up = %q, want null", *got.CatchUp)
				}
			default:
				if got.CatchUp == nil || *got.CatchUp != tc.wantCatch {
					t.Fatalf("catch_up = %v, want %q", got.CatchUp, tc.wantCatch)
				}
			}
			if (got.Interruption != nil) != tc.wantInterr {
				t.Fatalf("interruption = %+v, want present=%v", got.Interruption, tc.wantInterr)
			}
			if tc.wantInterruptionKind != "" && got.Interruption != nil && got.Interruption.Kind != tc.wantInterruptionKind {
				t.Fatalf("interruption.kind = %q, want %q", got.Interruption.Kind, tc.wantInterruptionKind)
			}
		})
	}
}

// 中断摘要字段：时刻/来源不可得为 null，kind 归一。
func TestEvaluateInterruptionFields(t *testing.T) {
	got := Evaluate(Inputs{
		Probe:       ProbeStopped,
		LastExit:    &ExitEvidence{InstanceID: "i", Kind: "failed", Summary: "失败"},
		NowUnixNano: time.Now().UnixNano(),
	})
	if got.Interruption == nil {
		t.Fatal("expected interruption")
	}
	it := got.Interruption
	if it.OccurredAt != nil || it.DetectedAt != nil || it.Source != nil {
		t.Fatalf("不可得字段应为 null: %+v", it)
	}
	if it.Kind != "failed" || it.Summary != "失败" {
		t.Fatalf("interruption = %+v", it)
	}
	if got.Interruption.Kind == "" {
		t.Fatal("kind 不能为空")
	}
}

// 停止意图窗口：过期的残留意图不得掩盖中断。
func TestEvaluateStaleStopIntentDoesNotMask(t *testing.T) {
	now := time.Now()
	got := Evaluate(Inputs{
		Probe: ProbeStopped, ObservationReadable: true, HasActive: true,
		ActiveInstanceID: "i2", ActiveStopRequestedUnixNano: now.Add(-2 * time.Minute).UnixNano(),
		NowUnixNano: now.UnixNano(),
	})
	if got.State != StateInterrupted || got.Interruption == nil {
		t.Fatalf("过期意图不得掩盖中断: %+v", got)
	}
}

// DB 未结案事件的 detected_at 必须输出 RFC 3339（库内 created_at 是 UTC 无
// 时区字符串，直接透出会被读端按本地时间误解）。
func TestLoadPendingDetectedAtRFC3339(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.RecordMonitorError(context.Background(), d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-rfc", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	col := Load(context.Background(), t.TempDir(), Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.Interruption == nil || col.Interruption.DetectedAt == nil {
		t.Fatalf("interruption = %+v", col.Interruption)
	}
	if _, perr := time.Parse(time.RFC3339, *col.Interruption.DetectedAt); perr != nil {
		t.Fatalf("detected_at 必须是 RFC 3339: %q", *col.Interruption.DetectedAt)
	}
	if col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("source = %v", col.Interruption.Source)
	}
	if col.Interruption.SourceDisplay == nil || *col.Interruption.SourceDisplay != "Claude" {
		t.Fatalf("source_display = %v, want Claude", col.Interruption.SourceDisplay)
	}
}

// 当前实例运行时，历史未结案缺口不能被隐藏：C 代启动且只补采其他客户端时，
// A 代 Claude 的未结案故障仍要提示；state 保持 running，缺口不冒充本代中断。
func TestLoadKeepsHistoricalPendingWhileInstanceRuns(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-C", StartedAt: "2026-11-11T16:10:00+08:00"},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}

	col := Load(ctx, dir, Snapshot{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded"}, d, time.Now())
	if col.State != StateRunning || col.CatchUp == nil || *col.CatchUp != "succeeded" {
		t.Fatalf("当前实例运行时 state 应为 running: %+v", col)
	}
	if col.Interruption == nil {
		t.Fatal("历史未结案缺口不得在当前实例运行时被隐藏")
	}
	if col.Interruption.Kind != "failed" || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应提示 A 代 Claude 缺口: %+v", col.Interruption)
	}
	if col.Interruption.OccurredAt == nil || *col.Interruption.OccurredAt != "2026-11-11T15:35:00+08:00" {
		t.Fatalf("应提示 A 代故障发生时刻: %+v", col.Interruption)
	}
}

// 观察文件里尚未持久化的历史缺口（数据库不可写时）继续作为 interruption
// 提示：最近一次停止仍是 stopped，缺口不因库不可读而消失。
func TestLoadShowsObservationPendingExit(t *testing.T) {
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止",
		},
		PendingExits: []runmeta.ExitRecord{{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00+08:00",
			Source: "claude", Summary: "Claude 目录监听失败",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(context.Background(), dir, Snapshot{Probe: ProbeStopped}, nil, time.Now())
	if col.State != StateStopped {
		t.Fatalf("最近一次退出是 stopped: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "failed" || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("观察文件中的历史缺口应继续提示: %+v", col.Interruption)
	}
}

// 观察文件里的历史缺口在库中已结案时不再提示（补写确认恢复后文件条目可能
// 因移除写入失败而残留，库中的结案证据优先）。
func TestLoadHidesResolvedObservationPendingExit(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止",
		},
		PendingExits: []runmeta.ExitRecord{{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00+08:00",
			Source: "claude", Summary: "Claude 目录监听失败",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.Interruption == nil {
		t.Fatal("未结案前应提示观察文件中的历史缺口")
	}

	detail := db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}
	if err := db.ConfirmMonitorInterruption(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatal(err)
	}
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateStopped {
		t.Fatalf("state = %q, want stopped", col.State)
	}
	if col.Interruption != nil {
		t.Fatalf("已结案的历史缺口不应再提示: %+v", col.Interruption)
	}
}

// 较新的 pending 条目已结案时，读端不得掩盖更早的未结案缺口：先过滤已结案
// 候选再选最新；库不可读时保守提示最新一条。
func TestLoadSkipsResolvedPendingAndPrefersUnresolved(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-C", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T16:40:00+08:00", Summary: "采集已停止",
		},
		PendingExits: []runmeta.ExitRecord{
			{
				InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
				OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:36:00+08:00",
				Source: "claude", Summary: "Claude 目录监听失败",
			},
			{
				InstanceID: "inst-B", Kind: runmeta.ExitKindFailed,
				OccurredAt: "2026-11-11T16:30:00+08:00", DetectedAt: "2026-11-11T16:31:00+08:00",
				Source: "codex", Summary: "Codex 目录监听失败",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 较新的 B 已结案，较早的 A 仍未结案。
	detail := db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-11T16:30:00+08:00", Source: "codex"}
	if err := db.ConfirmMonitorInterruption(ctx, d, "2026-11-11", "codex", "Codex 目录监听失败", detail); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateStopped {
		t.Fatalf("state = %q, want stopped", col.State)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("已结案的较新条目不得掩盖更早的未结案缺口: %+v", col.Interruption)
	}

	// 库不可读：保守提示文件中的最新一条（B）。
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, nil, time.Now())
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "codex" {
		t.Fatalf("库不可读时应保守提示最新条目: %+v", col.Interruption)
	}
}

// 历史缺口按发生时刻合并来源：库中较旧的未结案事件不得遮住观察文件中更新
// 的待补写缺口；pending_exits 中的非中断记录不参与选择。
func TestLoadMergesHistoricalSourcesByOccurrence(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:00:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-C", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T17:00:00+08:00", Summary: "采集已停止",
		},
		PendingExits: []runmeta.ExitRecord{
			{
				InstanceID: "inst-B", Kind: runmeta.ExitKindFailed,
				OccurredAt: "2026-11-11T16:00:00+08:00", DetectedAt: "2026-11-11T16:01:00+08:00",
				Source: "codex", Summary: "Codex 目录监听失败",
			},
			{
				InstanceID: "inst-S", Kind: runmeta.ExitKindStopped,
				OccurredAt: "2026-11-11T16:30:00+08:00", DetectedAt: "2026-11-11T16:30:00+08:00",
				Summary: "采集已停止",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateStopped {
		t.Fatalf("state = %q, want stopped", col.State)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "codex" {
		t.Fatalf("应按发生时刻提示较新的文件缺口: %+v", col.Interruption)
	}

	// 库不可读：stopped 条目被过滤，仍提示 failed 缺口。
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, nil, time.Now())
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "codex" {
		t.Fatalf("库不可读时应过滤非中断条目并提示 failed: %+v", col.Interruption)
	}
}

// 重启后推导的「原因未知」不得掩盖库中同实例的已知故障：B 运行期间读端
// 应显示 A 的已知来源与发生时间；B 自身崩溃残留时仍优先显示 B 的原因未知。
func TestLoadPrefersKnownFailureOverDerivedUnknown(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: "2026-11-11T16:10:00+08:00"},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindInterruptedUnknown,
			DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已中断，退出原因未知",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// B 运行中：显示库中 A 的已知故障（来源与发生时间），而不是推导值。
	col := Load(ctx, dir, Snapshot{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded"}, d, time.Now())
	if col.State != StateRunning {
		t.Fatalf("state = %q, want running", col.State)
	}
	if col.Interruption == nil || col.Interruption.Kind != "failed" || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应显示同实例的已知故障: %+v", col.Interruption)
	}
	if col.Interruption.OccurredAt == nil || *col.Interruption.OccurredAt != "2026-11-11T15:35:00+08:00" {
		t.Fatalf("应保留已知故障发生时刻: %+v", col.Interruption)
	}

	// B 自身崩溃残留（ProbeStopped + active 残留）：B 的原因未知优先。
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("本次残留的未知中断应优先提示: %+v", col.Interruption)
	}
}

// 受控停止落定 stopped 后，库中的同实例未结案故障在库可读时仍覆盖停止
// 结论：读端以库证据为准判为 interrupted。
func TestLoadSameInstancePendingOverridesStopped(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-12", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-12T16:00:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-12T16:00:00+08:00", Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted {
		t.Fatalf("同实例库故障应覆盖 stopped 结论: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应显示库中故障来源: %+v", col.Interruption)
	}
}

// 陈旧 active 残留（启动严格早于 last_exit）不覆盖最近退出：state 按 last_exit
// 判定，陈旧未知中断只作为历史提示保留；同一秒视为无法区分、维持 active 优先。
func TestLoadStaleActiveDoesNotOverrideLatestExit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-old", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-newer", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(context.Background(), dir, Snapshot{Probe: ProbeStopped}, nil, now)
	if col.State != StateStopped {
		t.Fatalf("陈旧 active 不得覆盖最近退出: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("陈旧未知中断应作为历史提示: %+v", col.Interruption)
	}

	// 同一秒：无法区分顺序，维持既有优先（active 残留判为最近退出）。
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-same", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-prev", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col = Load(context.Background(), dir, Snapshot{Probe: ProbeStopped}, nil, now)
	if col.State != StateInterrupted {
		t.Fatalf("同秒无法区分时应维持 active 优先: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("应按残留 active 提示中断: %+v", col.Interruption)
	}
}

// 已判定过期的 active 不得参与库候选的实例匹配：最近一次退出（last_exit）
// 的同实例库故障必须能覆盖停止结论，而不是被过期 active 的实例筛选降为历史。
func TestLoadStaleActiveDoesNotFilterLatestPending(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	now := time.Now()
	// 库中 B 的未结案故障：B 是 last_exit 的实例（最近一次退出）。
	if err := db.RecordMonitorError(ctx, d, now.Format("2006-01-02"), "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: now.Add(-time.Minute).Format(time.RFC3339), Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A 的 active 残留属于更早一代（启动早于 B 的退出）；B 的 stopped 是库
	// 不可读期间落定的最近退出。
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-A", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, now)
	if col.State != StateInterrupted {
		t.Fatalf("过期 active 不得把最近退出的库故障降为历史: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "failed" || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应显示最近退出的同实例故障: %+v", col.Interruption)
	}

	// 对照：active 属于最近一代（启动晚于 last_exit）时仍按实例筛选，残留
	// active 的未知中断优先于 last_exit，B 的故障退为历史候选。
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-A", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, now)
	if col.State != StateInterrupted {
		t.Fatalf("最近一代 active 残留应判为中断: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("应按残留 active 提示原因未知: %+v", col.Interruption)
	}
}

// 同秒回填的旧故障不得挡住最近退出的同实例故障：无有效 active 时先认
// last_exit 的同实例库证据；只有其他实例故障时刻明确更晚才由它取代。
func TestLoadSameSecondBackfillDoesNotHideLatestExitFailure(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	occurred := t0.Format(time.RFC3339)
	// 先记录 B 的 Claude 故障，再回填同秒发生的 A 代 Codex 故障（入库更晚）。
	if err := db.RecordMonitorError(ctx, d, t0.Format("2006-01-02"), "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: occurred, Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, t0.Format("2006-01-02"), "codex", "Codex 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: occurred, Source: "codex"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-B", Kind: runmeta.ExitKindStopped,
			DetectedAt: occurred, Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted {
		t.Fatalf("同秒回填不得把最近退出的同实例故障降为历史: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("应显示 B 的 Claude 故障: %+v", col.Interruption)
	}

	// 对照：其他实例的故障发生时刻明确更晚（属于更晚的一次退出）→ 由它取代。
	later := t0.Add(time.Second).Format(time.RFC3339)
	if err := db.RecordMonitorError(ctx, d, t0.Format("2006-01-02"), "codex", "Codex 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A2", Kind: "failed", OccurredAt: later, Source: "codex"}); err != nil {
		t.Fatal(err)
	}
	col = Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted {
		t.Fatalf("明确更晚的其他实例故障应为最近退出证据: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "codex" {
		t.Fatalf("应显示更晚的 Codex 故障: %+v", col.Interruption)
	}
}

// selectPending 的归属矩阵：无有效 active 时同实例证据优先，只有其他实例
// 故障时刻严格更晚才取代；无 last_exit 时按发生时刻取最新；有 active 时
// 按实例匹配。
func TestSelectPendingAttributionMatrix(t *testing.T) {
	base := time.Date(2026, 11, 12, 16, 0, 0, 0, time.Local)
	at := func(offset time.Duration) string { return base.Add(offset).Format(time.RFC3339) }
	cands := []PendingInterruption{
		{InstanceID: "inst-B", Kind: "failed", OccurredAt: at(0), Source: "claude"},
		{InstanceID: "inst-A", Kind: "failed", OccurredAt: at(0), Source: "codex"},
	}
	cases := []struct {
		name       string
		candidates []PendingInterruption
		active     string
		lastExit   *ExitEvidence
		want       string // "" = latest nil
	}{
		{"same-instance wins over same-second backfill", cands, "", &ExitEvidence{InstanceID: "inst-B", DetectedAt: at(0)}, "inst-B"},
		{"clearly later other instance wins", []PendingInterruption{cands[0], {InstanceID: "inst-A", Kind: "failed", OccurredAt: at(time.Second), Source: "codex"}}, "", &ExitEvidence{InstanceID: "inst-B", DetectedAt: at(0)}, "inst-A"},
		{"earlier other instance does not win", []PendingInterruption{cands[0], {InstanceID: "inst-A", Kind: "failed", OccurredAt: at(-time.Second)}}, "", &ExitEvidence{InstanceID: "inst-B", DetectedAt: at(0)}, "inst-B"},
		{"later than same record but not later than last_exit keeps same instance", []PendingInterruption{cands[0], {InstanceID: "inst-A", Kind: "failed", OccurredAt: at(time.Second)}}, "", &ExitEvidence{InstanceID: "inst-B", OccurredAt: at(2 * time.Second), DetectedAt: at(2 * time.Second)}, "inst-B"},
		{"same instance without occurrence keeps same-instance evidence", []PendingInterruption{{InstanceID: "inst-B", Kind: "failed"}, cands[1]}, "", &ExitEvidence{InstanceID: "inst-B", DetectedAt: at(0)}, "inst-B"},
		{"no last_exit picks latest occurrence", cands, "", nil, "inst-B"},
		{"active matches same instance", cands, "inst-A", &ExitEvidence{InstanceID: "inst-B", DetectedAt: at(0)}, "inst-A"},
		{"active without same instance yields nil latest", []PendingInterruption{cands[0]}, "inst-X", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			latest, _ := selectPending(tc.candidates, tc.active, tc.lastExit)
			got := ""
			if latest != nil {
				got = latest.InstanceID
			}
			if got != tc.want {
				t.Fatalf("latest = %q, want %q（候选 %+v）", got, tc.want, tc.candidates)
			}
		})
	}
}

// 晚入库的旧故障不得挡住本代故障：候选选取先按本代实例匹配；没有可匹配
// 实例时按原始发生时刻取最新——入库顺序完全不参与（回填旧故障虽入库更晚，
// 也不会被当成最近退出或挡住本代已知故障）。
func TestLoadPendingSelection(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	// 先入库 B 代故障（发生 16:30），再回填更早的 A 代故障（发生 15:00，
	// created_at 更晚）。
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-B", Kind: "failed", OccurredAt: "2026-11-11T16:30:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:00:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}

	// 场景 1：B 的 active 残留 → 选出 B 的已知故障（而非推导的原因未知）。
	dir1 := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir1), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: "2026-11-11T16:00:00+08:00"},
	}); err != nil {
		t.Fatal(err)
	}
	col := Load(ctx, dir1, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted || col.Interruption == nil || col.Interruption.Kind != "failed" {
		t.Fatalf("本代已知故障应为 interrupted+failed: %+v", col)
	}
	if col.Interruption.OccurredAt == nil || *col.Interruption.OccurredAt != "2026-11-11T16:30:00+08:00" {
		t.Fatalf("应选出 B（发生 16:30）: %+v", col.Interruption)
	}

	// 场景 2：无 active、只有旧的正常停止摘要 → 仍选出发生时刻最新的 B，
	// 而不是晚入库的旧故障 A。
	dir2 := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir2), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-C", Kind: runmeta.ExitKindStopped,
			DetectedAt: "2026-11-11T16:00:00+08:00", Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	col = Load(ctx, dir2, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted || col.Interruption == nil || col.Interruption.Kind != "failed" {
		t.Fatalf("发生时刻最新的 B 应为最近退出证据: %+v", col)
	}
	if col.Interruption.OccurredAt == nil || *col.Interruption.OccurredAt != "2026-11-11T16:30:00+08:00" {
		t.Fatalf("应选出 B 而非晚入库的旧故障 A: %+v", col.Interruption)
	}
}

// 本代有库证据时只提示本代：同实例候选作为最近退出证据优先，历史缺口不在
// 同一个 interruption 字段里争位（本代结案后下一次读端再提示历史缺口）。
func TestLoadPrefersSameInstancePendingOverHistorical(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, e := range []struct {
		id         string
		occurredAt string
	}{
		{id: "inst-A", occurredAt: "2026-11-11T15:35:00+08:00"},
		{id: "inst-C", occurredAt: "2026-11-11T16:30:00+08:00"},
	} {
		if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
			db.MonitorErrorDetail{InstanceID: e.id, Kind: "failed", OccurredAt: e.occurredAt, Source: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-C", StartedAt: "2026-11-11T16:10:00+08:00"},
	}); err != nil {
		t.Fatal(err)
	}

	col := Load(ctx, dir, Snapshot{Probe: ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "running"}, d, time.Now())
	if col.State != StateRunning {
		t.Fatalf("state 应为 running: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.OccurredAt == nil || *col.Interruption.OccurredAt != "2026-11-11T16:30:00+08:00" {
		t.Fatalf("应提示本代 C 的故障而不是历史 A: %+v", col.Interruption)
	}
}

// 本代 active 残留但库中只有历史缺口：状态取最近一代（原因未知），历史
// 缺口不得冒充本次退出原因。
func TestLoadLeftoverActiveBeatsHistoricalPending(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-A", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dir), runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-killed", StartedAt: "2026-11-11T16:00:00+08:00"},
	}); err != nil {
		t.Fatal(err)
	}

	col := Load(ctx, dir, Snapshot{Probe: ProbeStopped}, d, time.Now())
	if col.State != StateInterrupted {
		t.Fatalf("active 残留应判为中断: %+v", col)
	}
	if col.Interruption == nil || col.Interruption.Kind != "interrupted_unknown" {
		t.Fatalf("历史缺口不得冒充本次退出原因: %+v", col.Interruption)
	}
}
