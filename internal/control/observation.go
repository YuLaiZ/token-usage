// internal/control/observation.go
package control

// 控制端对 daemon-observation.json 的受控停止写入：发停止信号前记录同实例
// 的停止意图，确认 daemon lock 释放后落定正常停止结果，失败则清除未完成
// 的意图。这些写入只用于状态可观测性，任何失败都不阻止停止流程本身。

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// observationLog 是观测写入失败时的日志出口（control 层无项目 logger 依赖）。
func observationLog() *slog.Logger { return slog.Default() }

// monitorQueryBusyTimeout 是控制端只读判定查询的连接级 busy 上限：这些查询
// 发生在观察文件写锁内（或停止收尾的关键路径），不能被数据库写锁拖满默认
// 5 秒等待。
const monitorQueryBusyTimeout = 500 * time.Millisecond

// requestControlledStop 在控制端发起停止前写入同实例的停止意图（instance_id
// 与 active 匹配才写；锁内读改写，避免与 daemon 退出写相互覆盖）。读端在
// 意图窗口内以此抑制"中断误报"；意图本身不等于正常停止。无可关联实例或
// 观测文件不可用时不阻止停止。
func requestControlledStop(cfg *config.Config, instanceID string, now time.Time) {
	if cfg == nil || instanceID == "" {
		return
	}
	if err := runmeta.UpdateObservation(runmeta.ObservationPath(cfg.DataDir), func(obs *runmeta.Observation) error {
		if obs.Active == nil || obs.Active.InstanceID != instanceID {
			return nil
		}
		obs.Active.StopRequestedAt = now.Format(time.RFC3339)
		return nil
	}); err != nil {
		observationLog().Warn("failed to record stop intent in daemon observation", "error", err)
	}
}

// clearControlledStopIntent 在停止操作失败时清除本次未完成的意图。
func clearControlledStopIntent(cfg *config.Config, instanceID string) {
	if cfg == nil || instanceID == "" {
		return
	}
	if err := runmeta.UpdateObservation(runmeta.ObservationPath(cfg.DataDir), func(obs *runmeta.Observation) error {
		if obs.Active == nil || obs.Active.InstanceID != instanceID || obs.Active.StopRequestedAt == "" {
			return nil
		}
		obs.Active.StopRequestedAt = ""
		return nil
	}); err != nil {
		observationLog().Warn("failed to clear stop intent in daemon observation", "error", err)
	}
}

// monitorFailure 是库中未结案监控故障的摘要投影（控制端写回观察文件用）。
type monitorFailure struct {
	Kind       string
	Source     string
	OccurredAt string
	Summary    string
}

// monitorFailureState 是库内故障查询的三态结果：只有查到真实故障才能覆盖
// 观察文件里已有的同实例退出结论；查询不可用时不得用合成的原因未知覆盖
// 可信摘要。
type monitorFailureState int

const (
	// monitorFailureAbsent 表示库可读且确认没有同实例未结案故障。
	monitorFailureAbsent monitorFailureState = iota
	// monitorFailureFound 表示查到同实例未结案故障（真实证据）。
	monitorFailureFound
	// monitorFailureUnavailable 表示库存在但打开/查询失败，结论不可用。
	monitorFailureUnavailable
)

// finishControlledStop 在控制端确认 daemon lock 释放后落定正常停止结果，
// 与守护进程退出路径共用同一套保留规则：同实例的已知异常证据（观察文件或
// 数据库中未结案的监控故障）不得用 stopped 覆盖——库内真实故障会把摘要
// 写回 last_exit，保留本次退出的归属（只清 active 会让旧摘要通过秒级时间
// 比较把本次故障降级为历史缺口）。其他代的 failed 证据：已持久化时缺口由
// 库继续提示、直接覆盖为本次 stopped；未持久化的（含来源未知）统一移入
// pending_exits——读端继续提示历史缺口、补写端只重试来源已知条目——随后
// 照常落定 stopped。sameInstanceFailure 为 nil 或确认无记录时按无同实例
// 故障处理；查询不可用时，观察文件已有同实例摘要则保留该可信结论，缺失
// 摘要时按已确认的受控停止落定 stopped（库恢复后读端会以查到的同实例故障
// 覆盖）。evidencePersisted 为 nil 或返回 false 时按未持久化处理。
func finishControlledStop(cfg *config.Config, instanceID string, now time.Time, evidencePersisted func(prevInstanceID string) bool, sameInstanceFailure func(instanceID string) (monitorFailure, monitorFailureState)) {
	if cfg == nil || instanceID == "" {
		return
	}
	path := runmeta.ObservationPath(cfg.DataDir)
	// 观测文件缺失：没有可落定的实例证据，不创建文件（daemon 若自己活着退出
	// 会写 stopped；强杀场景由下次启动的残留检测兜底）。
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return
	}
	// 库查询在观察文件写锁外完成：只读连接可能等待数据库写锁，锁内只做
	// 内存判定，避免放大观察文件锁持有时间。
	var failure monitorFailure
	failureState := monitorFailureAbsent
	if sameInstanceFailure != nil {
		failure, failureState = sameInstanceFailure(instanceID)
	}
	// 其他代 failed 证据的持久化判定同样在锁外预取：锁内仅按实例键采用结果，
	// 快照缺失/被并发改写/未持久化时按未持久化保守迁移到 pending。
	var prevFailedID string
	prevFailedPersisted := false
	if snap, rerr := runmeta.ReadObservation(path); rerr == nil && snap.LastExit != nil &&
		snap.LastExit.Kind == runmeta.ExitKindFailed && snap.LastExit.InstanceID != instanceID {
		prevFailedID = snap.LastExit.InstanceID
		prevFailedPersisted = evidencePersisted != nil && evidencePersisted(prevFailedID)
	}
	previousFailedPersisted := func(id string) bool {
		return id != "" && id == prevFailedID && prevFailedPersisted
	}
	if err := runmeta.UpdateObservation(path, func(obs *runmeta.Observation) error {
		preserve := func() error {
			if obs.Active != nil && obs.Active.InstanceID == instanceID {
				obs.Active = nil
			}
			return nil
		}
		// daemon lock 已确认释放：其他代的 active 必为陈旧残留，转为历史
		// unknown 移入 pending，避免残留 active 永久压过最近一次退出。
		obs.MigrateStaleActive(instanceID, now.Format(time.RFC3339),
			ui.Bi("collection interrupted, exit reason unknown", "采集已中断，退出原因未知"))
		// 同实例的真实故障仅存于数据库（退出摘要写失败、故障已入库）：把
		// 故障摘要写回 last_exit，保留本次退出的实例归属与已知来源/时间。
		effFailure := failure
		effState := failureState
		if effState == monitorFailureUnavailable {
			// 查询不可用：观察文件已有同实例的可信退出摘要（failed/stopped）
			// 时不得用合成的"原因未知"覆盖。
			if obs.LastExit != nil && obs.LastExit.InstanceID == instanceID {
				observationLog().Warn("monitor failure evidence unavailable; keeping existing exit summary for stopped instance",
					"instance_id", instanceID, "kind", obs.LastExit.Kind)
				return preserve()
			}
			// 没有同实例摘要：受控停止已确认（daemon lock 已释放），按正常
			// 停止落定；数据库中的故障证据暂不可核实，库恢复可读后读端会以
			// 同实例的未结案故障为准覆盖停止结论。已确认的停止不报故障。
			effState = monitorFailureAbsent
			effFailure = monitorFailure{}
		}
		if effState == monitorFailureFound {
			// 写回不得吞掉观察文件里其他代的 failed 证据：可确认已持久化的
			// 才允许丢弃，否则移入 pending_exits，缺口继续提示/待补写。
			if obs.LastExit != nil && obs.LastExit.InstanceID != instanceID && obs.LastExit.Kind == runmeta.ExitKindFailed {
				if !previousFailedPersisted(obs.LastExit.InstanceID) {
					obs.AddPendingExit(*obs.LastExit)
				}
			}
			kind := effFailure.Kind
			if kind != runmeta.ExitKindFailed && kind != runmeta.ExitKindInterruptedUnknown {
				kind = ""
			}
			source := effFailure.Source
			occurredAt := effFailure.OccurredAt
			// 发生时刻必须是合法 RFC3339，否则整次写入会被校验拒绝；非法值
			// 视为缺失并回退观察文件的证据，确实没有时留空（读端按缺失降级）。
			if occurredAt != "" {
				if _, err := time.Parse(time.RFC3339, occurredAt); err != nil {
					occurredAt = ""
				}
			}
			summary := effFailure.Summary
			// 观察文件里同实例的既有证据为底：库行缺字段时不把有效信息降级。
			if obs.LastExit != nil && obs.LastExit.InstanceID == instanceID {
				prev := obs.LastExit
				if kind == "" && (prev.Kind == runmeta.ExitKindFailed || prev.Kind == runmeta.ExitKindInterruptedUnknown) {
					kind = prev.Kind
				}
				if source == "" {
					source = prev.Source
				}
				if occurredAt == "" {
					occurredAt = prev.OccurredAt
				}
				if strings.TrimSpace(summary) == "" {
					summary = prev.Summary
				}
			}
			if kind == "" {
				kind = runmeta.ExitKindInterruptedUnknown
			}
			if strings.TrimSpace(summary) == "" {
				summary = ui.Bi("collection interrupted, exit reason unknown", "采集已中断，退出原因未知")
			}
			obs.LastExit = &runmeta.ExitRecord{
				InstanceID: instanceID,
				Kind:       kind,
				OccurredAt: occurredAt,
				DetectedAt: now.Format(time.RFC3339),
				Source:     source,
				Summary:    summary,
			}
			if obs.Active != nil && obs.Active.InstanceID == instanceID {
				obs.Active = nil
			}
			observationLog().Warn("recorded persisted failure evidence for stopped instance; controlled stop not recorded as stopped",
				"instance_id", instanceID, "kind", kind)
			return nil
		}
		// 同代的已知异常证据：保留、只结束 active。
		if obs.LastExit != nil && obs.LastExit.InstanceID == instanceID && obs.LastExit.Kind != runmeta.ExitKindStopped {
			return preserve()
		}
		// 其他代的 failed 证据：已持久化 → 缺口由库承担提示，直接覆盖为本次
		// stopped；未持久化（含来源未知）→ 移入 pending_exits（读端继续提示、
		// 补写端按来源规则处理），最近一次停止与历史缺口都不丢。
		if obs.LastExit != nil && obs.LastExit.Kind == runmeta.ExitKindFailed && obs.LastExit.InstanceID != instanceID {
			if !previousFailedPersisted(obs.LastExit.InstanceID) {
				obs.AddPendingExit(*obs.LastExit)
			}
		}
		obs.LastExit = &runmeta.ExitRecord{
			InstanceID: instanceID,
			Kind:       runmeta.ExitKindStopped,
			OccurredAt: now.Format(time.RFC3339),
			DetectedAt: now.Format(time.RFC3339),
			Summary:    ui.Bi("collection stopped", "采集已停止"),
		}
		if obs.Active != nil && obs.Active.InstanceID == instanceID {
			obs.Active = nil
		}
		return nil
	}); err != nil {
		observationLog().Warn("failed to record controlled stop result", "error", err)
	}
}

// productionMonitorEvidencePersisted 只读查询 usage.db 是否已有该实例的监控
// 中断记录（任意 resolved 状态）：已入库的缺口由库承担提示。库文件缺失、
// 不可读或查询失败按"未持久化"返回 false——观察文件里的证据仍是唯一来源，
// 调用方必须保留。
func productionMonitorEvidencePersisted(cfg *config.Config, instanceID string) bool {
	return queryMonitorInstance(cfg, instanceID, db.HasMonitorInstanceRecord)
}

// productionMonitorFailure 查询同实例未结案故障的摘要投影，返回三态结果：
// 库文件从未创建或确认无记录返回 absent（安全落定停止结论）；查到的真实
// 故障返回 found；库存在但打开/查询失败返回 unavailable（结论不可用，不得
// 覆盖观察文件已有的同实例可信摘要）。
func productionMonitorFailure(cfg *config.Config, instanceID string) (monitorFailure, monitorFailureState) {
	if cfg == nil || instanceID == "" || strings.TrimSpace(cfg.DataDir) == "" {
		return monitorFailure{}, monitorFailureAbsent
	}
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		return monitorFailure{}, monitorFailureAbsent
	}
	ro, err := db.OpenReadOnlyWithBusyTimeout(dbPath, monitorQueryBusyTimeout)
	if err != nil {
		return monitorFailure{}, monitorFailureUnavailable
	}
	defer ro.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	detail, message, found, err := db.FindUnresolvedMonitorInstance(ctx, ro, instanceID)
	if err != nil {
		return monitorFailure{}, monitorFailureUnavailable
	}
	if !found {
		return monitorFailure{}, monitorFailureAbsent
	}
	return monitorFailure{
		Kind:       detail.Kind,
		Source:     detail.Source,
		OccurredAt: detail.OccurredAt,
		Summary:    message,
	}, monitorFailureFound
}

// queryMonitorInstance 以只读连接和短时限执行一次实例判定查询。
func queryMonitorInstance(cfg *config.Config, instanceID string, check func(context.Context, *db.DB, string) (bool, error)) bool {
	if cfg == nil || instanceID == "" || strings.TrimSpace(cfg.DataDir) == "" {
		return false
	}
	ro, err := db.OpenReadOnlyWithBusyTimeout(filepath.Join(cfg.DataDir, "usage.db"), monitorQueryBusyTimeout)
	if err != nil {
		return false
	}
	defer ro.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	found, err := check(ctx, ro, instanceID)
	if err != nil {
		return false
	}
	return found
}
