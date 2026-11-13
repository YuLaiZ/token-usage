// internal/daemon/observation.go
package daemon

// daemon-observation.json 的生命周期管理：启动登记 active、退出写 last_exit、
// 可控停止的意图与结果写入。所有写入校验本代所有权（instance_id 匹配），
// 不让旧实例覆盖新实例；任何一步失败只记日志，不改变退出码、不阻塞退出。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// monitorErrorWriteTimeout 是退出边界尽力写 collection_errors 的短时限。
const monitorErrorWriteTimeout = 2 * time.Second

// pendingPersistBudget 是整批历史缺口补写的时间预算：数据库繁忙或锁等待时，
// 不能让历史补写耗尽启动就绪或退出落定的时间窗口；超预算的条目原样保留，
// 下次启动或写库恢复后再补。
const pendingPersistBudget = time.Second

// registerActiveObservation 在 daemon 取得实例锁、运行资源就绪后核对上一代
// 退出证据并登记本代 active（读-改-写在观测文件锁内完成）：
//   - 先尽力补写观察文件里历史待持久化证据（pending_exits），成功项移除、
//     失败项保留，等待写库恢复后再次补写。
//   - 上一代 active 残留且没有任何对应退出记录（last_exit、pending_exits 都
//     没有）→ 记录 interrupted_unknown（detected_at=now）并记日志；强杀/
//     断电的进程无法自己写日志，检测结果以本实例的解释器为准。
//   - 观察文件里唯一、来源已知但无法持久化的旧故障证据移入 pending_exits，
//     last_exit 空出，缺口提示不随覆盖丢失。
//   - 缺失/损坏/不匹配的证据只降级，不构造故障记录。
//
// 写入失败仅记日志，不影响启动。
func registerActiveObservation(cfg *config.Config, log *slog.Logger, usageDB *db.DB, instanceID string, now time.Time) {
	if cfg == nil || instanceID == "" {
		return
	}
	path := runmeta.ObservationPath(cfg.DataDir)
	var prevExit *runmeta.ExitRecord
	var pendingExits []runmeta.ExitRecord
	if obs, rerr := runmeta.ReadObservation(path); rerr == nil {
		prevExit = obs.LastExit
		pendingExits = obs.PendingExits
	}
	// 优先登记本次运行证据：先处理单条旧退出摘要、写 active；历史缺口整批
	// 补写延后到本代证据落盘之后（带统一预算），不能拖慢启动就绪。
	prevPersisted := true
	if prevExit != nil && prevExit.InstanceID != instanceID {
		prevPersisted = persistUnresolvedLastExit(context.Background(), log, usageDB, prevExit, now)
	}
	// 上一代 active 残留推导、旧证据迁移与 active 登记在同一 mutate 中完成：
	// 有界重试必须复用完整职责，不能只补写 active（否则锁竞争后的重试会跳过
	// interrupted_unknown 推导与证据迁移）。锁内读-改-写与各操作幂等，重复
	// 执行安全。
	mutate := func(obs *runmeta.Observation) error {
		if prevExit != nil && !prevPersisted && obs.LastExit != nil && obs.LastExit.InstanceID == prevExit.InstanceID {
			obs.AddPendingExit(*obs.LastExit)
			obs.LastExit = nil
		}
		// 上一代 active 残留：锁已由本实例持有，说明上一代已退出。
		if prev := obs.Active; prev != nil && prev.InstanceID != "" && prev.InstanceID != instanceID {
			if staleActiveBefore(prev, obs.LastExit) {
				// active 属于更早一代、last_exit 是更晚的已知退出：未知中断
				// 已被取代，清除过期 active，不覆盖更晚的退出证据。
				log.Warn("dropping stale active evidence superseded by later exit",
					"stale_instance_id", prev.InstanceID, "last_exit_instance_id", obs.LastExit.InstanceID)
			} else {
				recorded := (obs.LastExit != nil && obs.LastExit.InstanceID == prev.InstanceID) || obs.HasPendingExit(prev.InstanceID)
				if !recorded {
					obs.LastExit = &runmeta.ExitRecord{
						InstanceID: prev.InstanceID,
						Kind:       runmeta.ExitKindInterruptedUnknown,
						DetectedAt: now.Format(time.RFC3339),
						Summary:    ui.Bi("collection interrupted, exit reason unknown", "采集已中断，退出原因未知"),
					}
					log.Warn("previous daemon instance ended without an exit record",
						"instance_id", prev.InstanceID, "detected_at", obs.LastExit.DetectedAt)
				}
			}
		}
		obs.Active = &runmeta.ActiveRun{
			InstanceID: instanceID,
			StartedAt:  now.Format(time.RFC3339),
		}
		return nil
	}
	err := runmeta.UpdateObservation(path, mutate)
	// flushPending 在本代证据落盘后补写历史缺口（整批预算），成功项移除。
	// 移除是可选清理：只做一次非阻塞加锁尝试，拿不到锁就保留条目（补写
	// 幂等，下次启动或退出时再次移除），不把启动/退出拖进第二段锁等待。
	flushPending := func() {
		persistedPending := persistPendingExits(context.Background(), log, usageDB, pendingExits, now)
		if len(persistedPending) == 0 {
			return
		}
		if err := runmeta.UpdateObservationWithLockTimeout(path, 0, func(obs *runmeta.Observation) error {
			for id := range persistedPending {
				obs.RemovePendingExit(id)
			}
			return nil
		}); err != nil {
			log.Warn("failed to remove persisted pending exits", "error", err)
		}
	}
	if err != nil {
		// 文件本身仍可解析时，失败来自观察文件锁超时或写入 IO——不能覆盖
		// 已有证据（last_exit/pending_exits）。锁超时多为瞬时竞争：有界重试
		// 一次仅补写本代 active，成功后继续补写历史缺口；仍失败则保留原证据、
		// 放弃登记，避免把正常停止误报成中断。损坏/不可读才降级为全新证据。
		if _, rerr := runmeta.ReadObservation(path); rerr == nil {
			// 有界重试一次（较短锁等待，避免叠满两个默认上限挤占启动就绪
			// 窗口）：复用同一 mutate，保证中断推导与证据迁移职责不被跳过。
			if retryErr := runmeta.UpdateObservationWithLockTimeout(path, time.Second, mutate); retryErr != nil {
				log.Warn("failed to update daemon observation, keeping existing evidence",
					"error", errors.Join(err, retryErr))
				return
			}
			flushPending()
			return
		}
		log.Warn("daemon observation unreadable, starting with fresh evidence", "error", err)
		fresh := runmeta.Observation{
			Version: runmeta.ObservationVersion,
			Active:  &runmeta.ActiveRun{InstanceID: instanceID, StartedAt: now.Format(time.RFC3339)},
		}
		if werr := runmeta.WriteObservation(path, fresh); werr != nil {
			log.Warn("failed to write active daemon observation", "error", werr)
		}
		return
	}
	flushPending()
}

// persistUnresolvedLastExit 在观察文件里唯一的故障证据即将被覆盖前，把来源
// 已知、尚未结案的 failed 摘要持久化到 collection_errors（按实例键幂等）；
// 缺口提示与历史记录由库承载，直到对应来源确实补采成功才被结案。返回 false
// 表示无法确认持久化（写库失败、库不可用，或来源未知且库中无该实例记录）——
// 调用方必须把证据移入 pending_exits 保留，不能覆盖丢失。
func persistUnresolvedLastExit(ctx context.Context, log *slog.Logger, usageDB *db.DB, le *runmeta.ExitRecord, now time.Time) bool {
	if le == nil || le.Kind != runmeta.ExitKindFailed {
		return true
	}
	if le.Source == "" {
		// 来源未知无法按来源补采结案：库中已有该实例记录时视为已处理；否则
		// 观察文件是唯一证据，保留待补写（读端继续提示）。
		if usageDB == nil {
			log.Warn("usage DB unavailable, unknown-source exit evidence kept for retry")
			return false
		}
		checkCtx, cancel := context.WithTimeout(ctx, monitorErrorWriteTimeout)
		defer cancel()
		recorded, err := db.HasMonitorInstanceRecord(checkCtx, usageDB, le.InstanceID)
		if err != nil {
			log.Warn("failed to check unknown-source exit evidence in DB, keeping it", "error", err)
			return false
		}
		if !recorded {
			log.Warn("unknown-source exit evidence kept for pending exits", "instance_id", le.InstanceID)
		}
		return recorded
	}
	if usageDB == nil {
		log.Warn("usage DB unavailable, unresolved exit evidence kept for retry")
		return false
	}
	date := now.Format("2006-01-02")
	if t, err := time.Parse(time.RFC3339, le.OccurredAt); err == nil {
		date = t.Local().Format("2006-01-02")
	}
	ctx, cancel := context.WithTimeout(ctx, monitorErrorWriteTimeout)
	defer cancel()
	detail := db.MonitorErrorDetail{
		InstanceID: le.InstanceID,
		Kind:       le.Kind,
		OccurredAt: le.OccurredAt,
		Source:     le.Source,
	}
	if err := db.RecordMonitorError(ctx, usageDB, date, le.Source, le.Summary, detail); err != nil {
		log.Warn("failed to persist unresolved exit evidence before overwriting observation", "error", err)
		return false
	}
	return true
}

// staleActiveBefore 报告残留 active 是否早于 last_exit 证据（属于更早一代）：
// 两者时刻都可解析且 active 启动更早时返回 true——更晚的已知退出已取代其
// 未知中断，不能再覆盖 last_exit。
func staleActiveBefore(prev *runmeta.ActiveRun, le *runmeta.ExitRecord) bool {
	if prev == nil || le == nil || prev.StartedAt == "" {
		return false
	}
	pt, perr := time.Parse(time.RFC3339, prev.StartedAt)
	if perr != nil {
		return false
	}
	ref := le.OccurredAt
	if ref == "" {
		ref = le.DetectedAt
	}
	lt, lerr := time.Parse(time.RFC3339, ref)
	if lerr != nil {
		return false
	}
	return pt.Before(lt)
}

// persistPendingExits 尽力补写观察文件里的历史待持久化证据，返回成功补写的
// 实例键集合（调用方在观察文件锁内按集合移除）。整批共享 pendingPersistBudget
// 时间预算（继承 ctx 的截止时间），超出即停止、剩余条目保留；库不可用时全部保留。
func persistPendingExits(ctx context.Context, log *slog.Logger, usageDB *db.DB, pending []runmeta.ExitRecord, now time.Time) map[string]struct{} {
	if usageDB == nil || len(pending) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pendingPersistBudget)
	defer cancel()
	out := make(map[string]struct{}, len(pending))
	for i := range pending {
		if ctx.Err() != nil {
			log.Warn("pending exit persistence budget exhausted, remaining entries kept",
				"remaining", len(pending)-i)
			break
		}
		rec := pending[i]
		// 只有来源已知的 failed 证据可补写；其他条目（手工编辑、未来调用点）
		// 原样保留，不能当作"已补写"移除。
		if rec.Kind != runmeta.ExitKindFailed || rec.Source == "" {
			continue
		}
		if persistUnresolvedLastExit(ctx, log, usageDB, &rec, now) {
			out[rec.InstanceID] = struct{}{}
		}
	}
	return out
}

// finishExitBudget 是退出收尾文件/数据库写入的总时间预算：控制端等待
// daemon lock 释放的窗口有限，观察文件摘要、旧证据持久化、中断记录与历史
// 缺口补写不能把各自的固定等待串联叠满窗口；超预算时保留证据、跳过可选步骤。
const finishExitBudget = 4 * time.Second

// finishDaemonExit 是可捕获退出边界：先写守护进程级 ERROR 日志（含影响与
// 底层错误），再原子写 last_exit 并结束本代 active，最后以短时限尽力把监控
// 中断事件写入 collection_errors（按实例幂等）。正常取消/停止记 stopped，
// 不写错误记录；其余错误记 failed，来源从 analyzer.MonitorFatalError 结构化
// 提取，无法定位用 daemon，不从双语错误字符串猜客户端。
func finishDaemonExit(cfg *config.Config, log *slog.Logger, usageDB *db.DB, instanceID string, exitErr error, now time.Time) {
	finishDaemonExitWithBudget(cfg, log, usageDB, instanceID, exitErr, now, finishExitBudget)
}

// finishDaemonExitWithBudget 与 finishDaemonExit 相同，但收尾总预算可注入
// （测试用短预算验证各段等待共享封顶）。
func finishDaemonExitWithBudget(cfg *config.Config, log *slog.Logger, usageDB *db.DB, instanceID string, exitErr error, now time.Time, exitBudget time.Duration) {
	if cfg == nil || instanceID == "" {
		return
	}
	deadline := time.Now().Add(exitBudget)
	// remaining 返回该段可用等待上限：不超过段自身固定上限，也不超过退出
	// 收尾总预算的剩余时间。
	remaining := func(limit time.Duration) time.Duration {
		if r := time.Until(deadline); r < limit {
			return r
		}
		return limit
	}
	record := runmeta.ExitRecord{
		InstanceID: instanceID,
		DetectedAt: now.Format(time.RFC3339),
	}
	interrupted := exitErr != nil && !errors.Is(exitErr, context.Canceled)
	if !interrupted {
		record.Kind = runmeta.ExitKindStopped
		record.OccurredAt = record.DetectedAt
		record.Summary = ui.Bi("collection stopped", "采集已停止")
	} else {
		source, occurredAt, summary := describeMonitorExit(exitErr)
		record.Kind = runmeta.ExitKindFailed
		record.Source = source
		if occurredAt.IsZero() {
			record.OccurredAt = record.DetectedAt
		} else {
			record.OccurredAt = occurredAt.Format(time.RFC3339)
		}
		record.Summary = summary
		log.Error("daemon exiting on a fatal monitor error: collection is interrupted",
			"instance_id", instanceID, "source", source, "impact", ui.Bi("all clients stop collecting until the daemon is started again", "所有客户端停止采集，直到手动重新启动守护进程"), "error", exitErr)
	}

	// 写观测（锁内读改写）：先落定本次退出证据（单条旧摘要先尝试持久化，
	// 失败则移入 pending_exits），再结束本代 active；失败只记日志。
	var prevExit *runmeta.ExitRecord
	var pendingExits []runmeta.ExitRecord
	if obs, rerr := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir)); rerr == nil {
		prevExit = obs.LastExit
		pendingExits = obs.PendingExits
	}
	// 优先落定本次退出：先尝试持久化将被覆盖的旧故障证据（单条上限，且
	// 不超过退出收尾总预算）。
	prevPersisted := true
	if prevExit != nil && prevExit.InstanceID != instanceID {
		if budget := remaining(monitorErrorWriteTimeout); budget > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			prevPersisted = persistUnresolvedLastExit(ctx, log, usageDB, prevExit, now)
			cancel()
		} else {
			prevPersisted = false
		}
	}
	movedToPending := false
	werr := runmeta.UpdateObservationWithLockTimeout(runmeta.ObservationPath(cfg.DataDir), remaining(2*time.Second), func(obs *runmeta.Observation) error {
		if obs.Active != nil && obs.Active.InstanceID == instanceID {
			obs.Active = nil
		}
		// daemon lock 由本代持有：其他代的 active 必为陈旧残留，转为历史
		// unknown 移入 pending，避免残留 active 永久压过最近一次退出。
		obs.MigrateStaleActive(instanceID, now.Format(time.RFC3339),
			ui.Bi("collection interrupted, exit reason unknown", "采集已中断，退出原因未知"))
		// 旧证据无法持久化时移入待补写缺口：最近一次退出与历史缺口分开
		// 保存，数据库持续失败时本次退出仍要落定、旧缺口也不丢失。
		if prevExit != nil && prevExit.InstanceID != instanceID && !prevPersisted &&
			obs.LastExit != nil && obs.LastExit.InstanceID == prevExit.InstanceID {
			obs.AddPendingExit(*obs.LastExit)
			movedToPending = true
		}
		obs.LastExit = &record
		return nil
	})
	if werr != nil {
		log.Warn("failed to record daemon exit observation", "error", werr)
	}
	if werr == nil && movedToPending {
		log.Warn("previous unresolved exit evidence moved to pending exits; this exit summary recorded",
			"instance_id", instanceID, "previous_instance_id", prevExit.InstanceID)
	}
	if werr == nil && !movedToPending && prevExit != nil && !prevPersisted {
		log.Warn("previous unresolved exit evidence changed concurrently; current exit summary recorded",
			"instance_id", instanceID, "previous_instance_id", prevExit.InstanceID)
	}

	if interrupted {
		if budget := remaining(monitorErrorWriteTimeout); budget > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			recordMonitorInterruption(ctx, log, usageDB, record, now)
			cancel()
		}
	}
	// 本次退出证据落定后，再补写历史缺口（整批预算，受退出收尾总预算约束），
	// 成功项移除。移除是可选清理：只做一次非阻塞加锁尝试，拿不到锁就保留
	// 条目（补写幂等，下次启动时再次移除），不把退出拖进第二段锁等待。
	flushCtx, flushCancel := context.WithDeadline(context.Background(), deadline)
	persistedPending := persistPendingExits(flushCtx, log, usageDB, pendingExits, now)
	flushCancel()
	if len(persistedPending) == 0 {
		return
	}
	if err := runmeta.UpdateObservationWithLockTimeout(runmeta.ObservationPath(cfg.DataDir), 0, func(obs *runmeta.Observation) error {
		for id := range persistedPending {
			obs.RemovePendingExit(id)
		}
		return nil
	}); err != nil {
		log.Warn("failed to remove persisted pending exits", "error", err)
	}
}

// describeMonitorExit 从退出错误构造结构化来源与面向用户的安全简述。
// 只暴露客户端显示名与阶段，不携带路径、堆栈或原始日志；显示名统一取自
// model.ClientKeyDisplayName（与读端 source_display 单一来源）。
func describeMonitorExit(exitErr error) (source string, occurredAt time.Time, summary string) {
	var mfe *analyzer.MonitorFatalError
	if errors.As(exitErr, &mfe) {
		display := model.ClientKeyDisplayName(mfe.Client)
		return mfe.Client, mfe.OccurredAt,
			ui.Bi(fmt.Sprintf("%s directory monitor failed", display), fmt.Sprintf("%s 目录监听失败", display))
	}
	return "", time.Time{}, ui.Bi("daemon exited unexpectedly", "采集守护进程异常退出")
}

// recordMonitorInterruption 以短时限尽力写入监控中断事件；失败只记日志，
// 不覆盖原错误、不改变退出码、不无限等待。date 分桶取实际观测的发生时刻
// （本地日期，与普通采集失败的本地分桶口径一致）——退出处理跨午夜时不得
// 把记录落到次日；发生时刻不可解析时回退检测时刻。
func recordMonitorInterruption(ctx context.Context, log *slog.Logger, usageDB *db.DB, record runmeta.ExitRecord, now time.Time) {
	if usageDB == nil {
		log.Warn("usage DB unavailable, monitor interruption not recorded in collection_errors")
		return
	}
	date := now.Format("2006-01-02")
	if t, err := time.Parse(time.RFC3339, record.OccurredAt); err == nil {
		date = t.Local().Format("2006-01-02")
	}
	ctx, cancel := context.WithTimeout(ctx, monitorErrorWriteTimeout)
	defer cancel()
	detail := db.MonitorErrorDetail{
		InstanceID: record.InstanceID,
		Kind:       record.Kind,
		OccurredAt: record.OccurredAt,
		Source:     record.Source,
	}
	if err := db.RecordMonitorError(ctx, usageDB, date, record.Source, record.Summary, detail); err != nil {
		log.Warn("failed to record monitor interruption in collection_errors", "error", err)
	}
}
