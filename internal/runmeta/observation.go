// internal/runmeta/observation.go
package runmeta

// daemon-observation.json 是跨代保留的守护进程退出证据文件：与 PID/runtime-state
// 不同，它不参与旧代元数据清理——active 记录当前实例证据，last_exit 保留最近一条
// 退出摘要，供 daemon status 与 serve 用量页在进程被强杀/断电后推断采集是否意外
// 中断。写入沿用 fileutil.ReplaceCompleteFile 原子替换；读端对缺失/损坏/不匹配的
// 证据只降级，不构造故障记录。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

const observationFileName = "daemon-observation.json"

// 退出摘要 kind 封闭值域（与方案一致）。
const (
	ExitKindStopped            = "stopped"
	ExitKindFailed             = "failed"
	ExitKindInterruptedUnknown = "interrupted_unknown"
)

// ObservationVersion 是 daemon-observation.json 的当前格式版本。
const ObservationVersion = 1

// ActiveRun 是当前代次的运行证据（instance_id 与启动时刻，可选的受控停止意图）。
type ActiveRun struct {
	InstanceID      string `json:"instance_id"`
	StartedAt       string `json:"started_at"`
	StopRequestedAt string `json:"stop_requested_at,omitempty"`
}

// ExitRecord 是最近一条退出摘要。Kind 取 stopped/failed/interrupted_unknown；
// 时间均为 RFC 3339。OccurredAt 是实际观测到的故障发生时刻（不可得则省略），
// DetectedAt 是记录检测时刻；Summary 是面向用户的安全简述，不含路径/堆栈/原始日志。
type ExitRecord struct {
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"`
	OccurredAt string `json:"occurred_at,omitempty"`
	DetectedAt string `json:"detected_at"`
	Source     string `json:"source,omitempty"`
	Summary    string `json:"summary"`
}

// Observation 是 daemon-observation.json 的完整内容。
type Observation struct {
	Version  int         `json:"version"`
	Active   *ActiveRun  `json:"active,omitempty"`
	LastExit *ExitRecord `json:"last_exit,omitempty"`
	// PendingExits 是尚未持久化到库的历史退出证据（按写入顺序保留）。数据库
	// 写入持续失败时，最近一次退出仍写入 last_exit，更早的已知故障移入这里，
	// 两者分开保存、互不丢失；下次启动或写库恢复后按实例键补写并移除。
	// 来源未知的条目（无法按来源补采结案）会一直保留到被容量驱逐，读端持续
	// 提示，需人工清理或等待容量回收；超过 MaxPendingExits 时丢弃最旧条目
	// 并输出 WARN（含被驱逐实例）。
	// 格式版本保持 1（新增可选字段）：旧版本读端忽略本字段；旧版本写端重写
	// 文件时会丢弃本字段，属于降级混用的可接受窗口。
	PendingExits []ExitRecord `json:"pending_exits,omitempty"`
}

// MaxPendingExits 限制待补写证据条数：数据库持续失败时防止文件无限增长，
// 超出丢弃最旧的条目（较近的历史缺口优先保留）。
const MaxPendingExits = 16

// AddPendingExit 追加一条待补写退出证据：按 InstanceID 幂等去重，超过
// MaxPendingExits 时丢弃最旧的条目。来源已知的 failed 证据可在写库恢复后
// 补写；来源未知的条目也允许暂存（补写端跳过保留，读端仍会继续提示）。
func (o *Observation) AddPendingExit(rec ExitRecord) {
	for i := range o.PendingExits {
		if o.PendingExits[i].InstanceID == rec.InstanceID {
			return
		}
	}
	o.PendingExits = append(o.PendingExits, rec)
	if len(o.PendingExits) > MaxPendingExits {
		evicted := o.PendingExits[0]
		slog.Default().Warn("pending exit evidence evicted at capacity",
			"instance_id", evicted.InstanceID, "kind", evicted.Kind, "max", MaxPendingExits)
		o.PendingExits = append([]ExitRecord(nil), o.PendingExits[len(o.PendingExits)-MaxPendingExits:]...)
	}
}

// RemovePendingExit 按实例键移除已补写的证据。
func (o *Observation) RemovePendingExit(instanceID string) {
	if len(o.PendingExits) == 0 {
		return
	}
	kept := o.PendingExits[:0]
	for _, rec := range o.PendingExits {
		if rec.InstanceID != instanceID {
			kept = append(kept, rec)
		}
	}
	o.PendingExits = kept
}

// MigrateStaleActive 把属于其他实例的 active 残留转成原因未知的 pending 证据
// 并清除：调用方确认 daemon lock 已由本代/控制端持有或释放，其他代 active
// 必然已退出，不能让它永久压过最近一次退出。返回是否发生迁移。
func (o *Observation) MigrateStaleActive(instanceID, detectedAt, summary string) bool {
	if o.Active == nil || o.Active.InstanceID == "" || o.Active.InstanceID == instanceID {
		return false
	}
	o.AddPendingExit(ExitRecord{
		InstanceID: o.Active.InstanceID,
		Kind:       ExitKindInterruptedUnknown,
		DetectedAt: detectedAt,
		Summary:    summary,
	})
	o.Active = nil
	return true
}

// HasPendingExit 报告某实例的待补写证据是否存在（判断该实例是否已有退出
// 记录时使用：pending 里的已知故障不应再被推导为"原因未知"）。
func (o *Observation) HasPendingExit(instanceID string) bool {
	for i := range o.PendingExits {
		if o.PendingExits[i].InstanceID == instanceID {
			return true
		}
	}
	return false
}

// ObservationPath 返回 <dataDir>/daemon-observation.json。
func ObservationPath(dataDir string) string {
	return filepath.Join(dataDir, observationFileName)
}

// ReadObservation 读并校验 observation 文件。缺失返回包装了 fs.ErrNotExist 的
// 错误；解析失败或字段无效返回普通错误——调用方据此降级，绝不构造故障记录。
func ReadObservation(path string) (Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		return Observation{}, fmt.Errorf("解析 daemon observation 失败: %w", err)
	}
	if err := validateObservation(obs); err != nil {
		return Observation{}, fmt.Errorf("daemon observation 字段无效: %w", err)
	}
	return obs, nil
}

// WriteObservation 以完整文件替换原子写入 observation。
func WriteObservation(path string, obs Observation) error {
	if err := validateObservation(obs); err != nil {
		return fmt.Errorf("daemon observation 字段无效: %w", err)
	}
	data, err := json.Marshal(obs)
	if err != nil {
		return fmt.Errorf("编码 daemon observation 失败: %w", err)
	}
	return fileutil.ReplaceCompleteFile(path, data, 0o644)
}

// UpdateObservation 在观测文件锁内执行读-改-写：daemon 退出写与控制端的
// 停止意图/结果写并发时，整文件原子替换会相互覆盖（如控制端用旧快照写回，
// 回退 daemon 刚写下的异常退出记录）。锁用独立的 <path>.lock 文件（flock
// 语义，残留无含义），mutate 抛错时不写回。锁等待超时返回错误，由调用方
// 记日志降级——观测写入失败不改变退出码或控制命令结果。
func UpdateObservation(path string, mutate func(*Observation) error) error {
	return UpdateObservationWithLockTimeout(path, observationLockTimeout, mutate)
}

// observationLockTimeout 是观察文件写锁的默认等待上限。
const observationLockTimeout = 2 * time.Second

// UpdateObservationWithLockTimeout 与 UpdateObservation 相同，但允许调用方
// 指定更短的锁等待上限：启动路径的有界重试必须在就绪窗口内尽快返回。
func UpdateObservationWithLockTimeout(path string, lockTimeout time.Duration, mutate func(*Observation) error) error {
	return UpdateObservationContext(context.Background(), path, lockTimeout, mutate)
}

// UpdateObservationContext 在锁等待期间响应 ctx 取消：调用方的等待预算被
// 取消（如停止信号）时立即返回错误且不写回，避免把已无意义的等待拖到超时。
func UpdateObservationContext(ctx context.Context, path string, lockTimeout time.Duration, mutate func(*Observation) error) error {
	lock := flock.New(path + ".lock")
	deadline := time.Now().Add(lockTimeout)
	for {
		locked, err := lock.TryLock()
		if err != nil {
			return fmt.Errorf("获取 daemon observation 写入锁失败: %w", err)
		}
		if locked {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("获取 daemon observation 写入锁超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer func() { _ = lock.Unlock() }()
	// 获得锁后再确认一次取消：取消发生在等待以外时同样不执行写回。
	if err := ctx.Err(); err != nil {
		return err
	}

	obs, err := ReadObservation(path)
	missing := false
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		obs = Observation{Version: ObservationVersion}
		missing = true
	default:
		return err
	}
	if err := mutate(&obs); err != nil {
		return err
	}
	// 文件原本缺失且没有任何证据产生：不创建空文件（只读路径保持无副作用）。
	if missing && obs.Active == nil && obs.LastExit == nil && len(obs.PendingExits) == 0 {
		return nil
	}
	obs.Version = ObservationVersion
	return WriteObservation(path, obs)
}

func validateObservation(obs Observation) error {
	if obs.Version != ObservationVersion {
		return fmt.Errorf("未知版本 %d", obs.Version)
	}
	if obs.Active != nil {
		if err := validateInstanceID("active.instance_id", obs.Active.InstanceID); err != nil {
			return err
		}
		if err := validateRFC3339("active.started_at", obs.Active.StartedAt); err != nil {
			return err
		}
		if obs.Active.StopRequestedAt != "" {
			if err := validateRFC3339("active.stop_requested_at", obs.Active.StopRequestedAt); err != nil {
				return err
			}
		}
	}
	if obs.LastExit != nil {
		if err := validateExitRecord("last_exit", *obs.LastExit); err != nil {
			return err
		}
	}
	for i := range obs.PendingExits {
		if err := validateExitRecord(fmt.Sprintf("pending_exits[%d]", i), obs.PendingExits[i]); err != nil {
			return err
		}
	}
	return nil
}

// validateExitRecord 校验单条退出摘要的字段；last_exit 与 pending_exits
// 共用同一套封闭值域与格式要求。
func validateExitRecord(field string, rec ExitRecord) error {
	if err := validateInstanceID(field+".instance_id", rec.InstanceID); err != nil {
		return err
	}
	switch rec.Kind {
	case ExitKindStopped, ExitKindFailed, ExitKindInterruptedUnknown:
	default:
		return fmt.Errorf("%s 含未知 kind %q", field, rec.Kind)
	}
	if err := validateRFC3339(field+".detected_at", rec.DetectedAt); err != nil {
		return err
	}
	if rec.OccurredAt != "" {
		if err := validateRFC3339(field+".occurred_at", rec.OccurredAt); err != nil {
			return err
		}
	}
	if strings.TrimSpace(rec.Summary) == "" {
		return fmt.Errorf("%s.summary 不能为空", field)
	}
	return nil
}

func validateInstanceID(field, id string) error {
	if id == "" || strings.TrimSpace(id) != id || len(strings.Fields(id)) != 1 {
		return fmt.Errorf("%s 必须是非空且不含空白的单字段: %q", field, id)
	}
	return nil
}

func validateRFC3339(field, value string) error {
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return fmt.Errorf("%s 不是 RFC3339 时间: %q", field, value)
	}
	return nil
}
