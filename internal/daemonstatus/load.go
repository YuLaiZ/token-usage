// internal/daemonstatus/load.go
package daemonstatus

// Load 是读端共享的证据采集与判定入口：daemon status（CLI）与 serve 的
// /api/daemon-status 都从这里取得 collection 对象，保证两个读端同源。
// 只读：不建锁文件、不改元数据、不写库。

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// Snapshot 是调用方采集的判活与当前实例阶段快照。
type Snapshot struct {
	Probe          Probe
	PhaseAvailable bool
	MonitorReady   bool
	CatchUp        string
}

// Load 读取退出观测与结案证据并计算 collection。usageDB 可为 nil（DB 不可读）：
// 此时结案证据不可得，LastExit 中断按"未确认恢复"保守提示。
func Load(ctx context.Context, dataDir string, snap Snapshot, usageDB *db.DB, now time.Time) Collection {
	in := Inputs{
		Probe:          snap.Probe,
		PhaseAvailable: snap.PhaseAvailable,
		MonitorReady:   snap.MonitorReady,
		CatchUp:        snap.CatchUp,
		NowUnixNano:    now.UnixNano(),
	}

	obs, err := runmeta.ReadObservation(runmeta.ObservationPath(dataDir))
	switch {
	case err == nil:
		in.ObservationReadable = true
		if obs.Active != nil {
			in.HasActive = true
			in.ActiveInstanceID = obs.Active.InstanceID
			if obs.Active.StartedAt != "" {
				if t, perr := time.Parse(time.RFC3339, obs.Active.StartedAt); perr == nil {
					in.ActiveStartedUnixNano = t.UnixNano()
				}
			}
			if obs.Active.StopRequestedAt != "" {
				if t, perr := time.Parse(time.RFC3339, obs.Active.StopRequestedAt); perr == nil {
					in.ActiveStopRequestedUnixNano = t.UnixNano()
				}
			}
		}
		if obs.LastExit != nil {
			in.LastExit = &ExitEvidence{
				InstanceID: obs.LastExit.InstanceID,
				Kind:       obs.LastExit.Kind,
				OccurredAt: obs.LastExit.OccurredAt,
				DetectedAt: obs.LastExit.DetectedAt,
				Source:     obs.LastExit.Source,
				Summary:    obs.LastExit.Summary,
			}
		}
		if len(obs.PendingExits) > 0 {
			in.PendingExit = selectPendingExit(ctx, usageDB, obs.PendingExits)
		}
	case errors.Is(err, fs.ErrNotExist):
		// 从未记录过：证据缺失，不构造故障。
	default:
		// 损坏/不可读：同样只降级。
	}

	if usageDB != nil {
		// 过期 active（属于更早一代）不参与实例匹配：否则最近退出的同实例
		// 库故障会被当成历史缺口，读端退回 stopped。与状态判定共用同一
		// activeIsLatest 口径。
		activeID := ""
		if in.HasActive && activeIsLatest(in) {
			activeID = in.ActiveInstanceID
		}
		candidates := loadPendingCandidates(ctx, usageDB)
		in.LatestPending, in.HistoricalPending = selectPending(candidates, activeID, in.LastExit)
		if in.LastExit != nil {
			if resolved, rerr := db.IsMonitorInstanceResolved(ctx, usageDB, in.LastExit.InstanceID); rerr == nil {
				in.LastExitResolved = resolved
			}
			// 推导出的"原因未知"若有同实例的持久故障证据，读端优先采用已知
			// 故障（来源与发生时间不丢失）；已结案的证据不再提示。
			if in.LastExit.Kind == "interrupted_unknown" && !in.LastExitResolved {
				for i := range candidates {
					if candidates[i].InstanceID == in.LastExit.InstanceID {
						in.LastExitKnownPending = &candidates[i]
						break
					}
				}
			}
		}
	}

	return Evaluate(in)
}

// loadPendingCandidates 把全部未结案监控事件解析为候选（跳过明细非法的行）；
// DB 读失败返回 nil（只按 observation 证据提示）。DetectedAt 统一转为
// RFC 3339（库内 created_at 是 UTC 无时区标记的 datetime 字符串，直接透出
// 会被读端按本地时间误解）。
func loadPendingCandidates(ctx context.Context, usageDB *db.DB) []PendingInterruption {
	errs, err := db.GetUnresolvedMonitorErrors(ctx, usageDB)
	if err != nil {
		return nil
	}
	out := make([]PendingInterruption, 0, len(errs))
	for _, e := range errs {
		detail, derr := db.ParseMonitorErrorDetail(e.Detail)
		if derr != nil {
			continue
		}
		out = append(out, PendingInterruption{
			InstanceID: detail.InstanceID,
			Kind:       detail.Kind,
			OccurredAt: detail.OccurredAt,
			DetectedAt: sqliteUTCToRFC3339(e.CreatedAt),
			Source:     detail.Source,
			Summary:    e.Message,
		})
	}
	return out
}

// selectPending 从候选中分离两类证据：
//   - latest：参与最近退出判定的本代证据。有 active 残留时只认同实例
//     （历史候选不冒充本代），无同实例候选时为 nil；activeInstanceID 为空
//     时先认 last_exit 的同实例库证据——同秒或回填顺序不影响归属，只有其他
//     实例的故障发生时刻**严格晚于 last_exit 的参照时刻**（与 pendingIsLatest
//     同一口径：发生时刻优先、回退检测时刻）时才由它取代；没有同实例证据时
//     按原始故障发生时刻取最新（绝不按入库顺序：回填的旧故障入库更晚，不能
//     挡住本代故障）。全部候选都缺发生时刻时保留第一条作为历史证据
//     （pendingIsLatest 会因缺时刻按历史处理）。
//   - historical：有 active 但本代没有库证据时仍未结案的历史缺口。当前实例
//     正常运行不代表旧缺口已补齐：它要继续作为 interruption 提示，直到
//     对应来源确认结案。它不参与 state 判定（不能冒充本代中断）。
func selectPending(candidates []PendingInterruption, activeInstanceID string, lastExit *ExitEvidence) (latest, historical *PendingInterruption) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if activeInstanceID != "" {
		for i := range candidates {
			if candidates[i].InstanceID == activeInstanceID {
				return &candidates[i], nil
			}
		}
		return nil, latestPendingByOccurredAt(candidates)
	}
	// 无有效 active：先认 last_exit 的同实例库证据。同实例证据就是最近一次
	// 退出的已知故障，不能因同秒回填的其他代旧故障入库更晚而被裁掉。
	if lastExit != nil && lastExit.InstanceID != "" {
		same := -1
		for i := range candidates {
			if candidates[i].InstanceID == lastExit.InstanceID {
				same = i
				break
			}
		}
		if same != -1 {
			best := -1
			var bestT time.Time
			// 与 pendingIsLatest 使用同一参照：last_exit 的发生时刻，缺失时
			// 回退检测时刻。只有其他实例的故障严格晚于该参照（明确属于更晚
			// 的一次退出）才取代同实例证据。
			if ref, refOK := lastExitReferenceTime(lastExit); refOK {
				for i := range candidates {
					if i == same {
						continue
					}
					t, err := time.Parse(time.RFC3339, candidates[i].OccurredAt)
					if err != nil || !t.After(ref) {
						continue
					}
					if best == -1 || t.After(bestT) {
						best, bestT = i, t
					}
				}
			}
			if best != -1 {
				return &candidates[best], nil
			}
			return &candidates[same], nil
		}
	}
	return latestPendingByOccurredAt(candidates), nil
}

// lastExitReferenceTime 返回 last_exit 的参照时刻（发生时刻优先，回退检测
// 时刻）：与 pendingIsLatest 的排序口径一致。
func lastExitReferenceTime(le *ExitEvidence) (time.Time, bool) {
	if le == nil {
		return time.Time{}, false
	}
	ref := le.OccurredAt
	if ref == "" {
		ref = le.DetectedAt
	}
	t, err := time.Parse(time.RFC3339, ref)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// latestPendingByOccurredAt 按原始故障发生时刻取最新；全部候选都缺发生
// 时刻时保留第一条（历史证据）。
func latestPendingByOccurredAt(candidates []PendingInterruption) *PendingInterruption {
	best := -1
	var bestT time.Time
	for i := range candidates {
		t, err := time.Parse(time.RFC3339, candidates[i].OccurredAt)
		if err != nil {
			continue
		}
		if best == -1 || t.After(bestT) {
			best = i
			bestT = t
		}
	}
	if best == -1 {
		return &candidates[0]
	}
	return &candidates[best]
}

// selectPendingExit 从观察文件的待补写证据中选出仍应提示的一条：先按中断
// 类型过滤（stopped 不参与提示，与写入端口径一致），库可读时再排除已结案
// （补写确认恢复）的实例，最后按发生时刻从剩余记录中取最新——较新的已结案
// 条目不能掩盖更早的未结案缺口；库不可读或查询失败时按未结案保守提示。
func selectPendingExit(ctx context.Context, usageDB *db.DB, recs []runmeta.ExitRecord) *ExitEvidence {
	candidates := make([]runmeta.ExitRecord, 0, len(recs))
	for _, rec := range recs {
		if !isInterruptedKind(rec.Kind) {
			continue
		}
		if usageDB != nil {
			if resolved, err := db.IsMonitorInstanceResolved(ctx, usageDB, rec.InstanceID); err == nil && resolved {
				continue
			}
		}
		candidates = append(candidates, rec)
	}
	if len(candidates) == 0 {
		return nil
	}
	pe := latestExitRecord(candidates)
	return &ExitEvidence{
		InstanceID: pe.InstanceID,
		Kind:       pe.Kind,
		OccurredAt: pe.OccurredAt,
		DetectedAt: pe.DetectedAt,
		Source:     pe.Source,
		Summary:    pe.Summary,
	}
}

// latestExitRecord 返回待补写证据中最近的一条：按原始故障发生时刻取最新
// （回填/补写顺序不参与）；全部缺发生时刻时取最后写入的一条。
func latestExitRecord(recs []runmeta.ExitRecord) *runmeta.ExitRecord {
	best := -1
	var bestT time.Time
	for i := range recs {
		t, err := time.Parse(time.RFC3339, recs[i].OccurredAt)
		if err != nil {
			continue
		}
		if best == -1 || t.After(bestT) {
			best = i
			bestT = t
		}
	}
	if best == -1 {
		return &recs[len(recs)-1]
	}
	return &recs[best]
}

// sqliteUTCToRFC3339 把 SQLite datetime('now')（UTC，无时区标记）转为 RFC 3339；
// 解析失败原样返回（读端对非 RFC 3339 值按原字符串降级展示）。
func sqliteUTCToRFC3339(v string) string {
	if v == "" {
		return ""
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", v, time.UTC)
	if err != nil {
		return v
	}
	return t.Format(time.RFC3339)
}
