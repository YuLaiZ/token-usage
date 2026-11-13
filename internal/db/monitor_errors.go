// internal/db/monitor_errors.go
package db

// collection_errors 的监控中断事件（error_type='monitor'）：与普通采集失败
// （'error'）分开登记、分开结案。普通采集的按日刷新/按日结案/按消息模式结案
// 全部只作用于 'error' 类型；监控中断仅由启动补采的独立恢复确认路径按
// error_type 与 detail.instance_id 结案，避免同日同源互相误结。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// MonitorErrorType 是监控中断事件的 error_type 取值。
const MonitorErrorType = "monitor"

// MonitorErrorDetail 是监控中断事件写入 collection_errors.detail 的结构化明细。
// instance_id 必填，作为幂等写入与结案的全部依据（不依赖摘要文案、日期或来源）。
type MonitorErrorDetail struct {
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind,omitempty"` // failed | interrupted_unknown
	OccurredAt string `json:"occurred_at,omitempty"`
	Source     string `json:"source,omitempty"`
}

// RecordMonitorError 幂等记录一条监控中断事件：在单条 INSERT…SELECT…WHERE
// NOT EXISTS 内按 error_type='monitor' 与 detail.instance_id 查重，历史空值/非
// JSON/缺字段的行取 NULL、不参与匹配。明细至少含非空 instance_id 与
// occurred_at（实际观测的事件时刻，表格优先展示它而非事后入库时间）；
// source 为空时归一为 daemon（不猜测客户端）。
func RecordMonitorError(ctx context.Context, d *DB, date, source, summary string, detail MonitorErrorDetail) error {
	if d == nil {
		return errors.New("记录监控中断时数据库不能为空")
	}
	if strings.TrimSpace(detail.InstanceID) == "" {
		return errors.New("监控中断明细的 instance_id 不能为空")
	}
	if strings.TrimSpace(detail.OccurredAt) == "" {
		return errors.New("监控中断明细的 occurred_at 不能为空")
	}
	if strings.TrimSpace(source) == "" {
		source = "daemon"
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("编码监控中断明细失败: %w", err)
	}
	_, err = d.execContextWithBusyTimeout(ctx, monitorWriteBusyTimeout, `
		INSERT INTO collection_errors (date, source, error_type, message, detail)
		SELECT ?, ?, 'monitor', ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM collection_errors
			WHERE error_type = 'monitor'
			  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
		)
	`, date, source, summary, string(payload), detail.InstanceID)
	return err
}

// ConfirmMonitorInterruption 持久化"该中断实例已由补采确认恢复"：先结案其
// 未解决事件；若库中没有任何该实例的记录（退出时写库失败、证据仅存在于观察
// 文件），补记一条 resolved=1 的确认行，使读端停止提示且历史可查。两条语句
// 都按实例键幂等，重复调用不产生重复行。
func ConfirmMonitorInterruption(ctx context.Context, d *DB, date, source, summary string, detail MonitorErrorDetail) error {
	if d == nil {
		return errors.New("确认监控中断恢复时数据库不能为空")
	}
	if strings.TrimSpace(detail.InstanceID) == "" {
		return errors.New("监控中断明细的 instance_id 不能为空")
	}
	if strings.TrimSpace(detail.OccurredAt) == "" {
		return errors.New("监控中断明细的 occurred_at 不能为空")
	}
	if strings.TrimSpace(source) == "" {
		source = "daemon"
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("编码监控中断明细失败: %w", err)
	}
	// UPDATE 与 INSERT 在同一连接上共享同一条短 busy 约束：补采确认发生在
	// 启动收尾的整批预算内，不能被写锁逐条拖满默认等待。
	if _, err := d.execContextsWithBusyTimeout(ctx, monitorWriteBusyTimeout, []string{
		`UPDATE collection_errors SET resolved = 1, updated_at = datetime('now')
		WHERE error_type = 'monitor' AND resolved = 0
		  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?`,
		`INSERT INTO collection_errors (date, source, error_type, message, detail, resolved)
		SELECT ?, ?, 'monitor', ?, ?, 1
		WHERE NOT EXISTS (
			SELECT 1 FROM collection_errors
			WHERE error_type = 'monitor'
			  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
		)`,
	}, [][]interface{}{
		{detail.InstanceID},
		{date, source, summary, string(payload), detail.InstanceID},
	}); err != nil {
		return fmt.Errorf("结案监控中断事件失败: %w", err)
	}
	return nil
}

// ResolveMonitorErrorsByInstance 按中断实例结案未解决的监控中断事件。
// detail 一律经 CASE WHEN json_valid(detail) THEN json_extract(...) END 取实例键，
// 不对 SQL 布尔短路作假设；历史非 JSON 明细取 NULL、不会误匹配。
func ResolveMonitorErrorsByInstance(ctx context.Context, q dbtx, instanceID string) (int64, error) {
	if strings.TrimSpace(instanceID) == "" {
		return 0, nil
	}
	res, err := q.ExecContext(ctx, `
		UPDATE collection_errors SET resolved = 1, updated_at = datetime('now')
		WHERE error_type = 'monitor' AND resolved = 0
		  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
	`, instanceID)
	if err != nil {
		return 0, fmt.Errorf("结案监控中断事件失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取监控中断结案结果失败: %w", err)
	}
	return n, nil
}

// IsMonitorInstanceResolved 报告某中断实例是否已有结案记录（resolved=1）。
// 读端用它区分"提供补采已确认恢复"与"从未记录/记录写入失败"：只有明确的
// 结案证据才停止提示。
func IsMonitorInstanceResolved(ctx context.Context, d *DB, instanceID string) (bool, error) {
	if d == nil || strings.TrimSpace(instanceID) == "" {
		return false, nil
	}
	var n int
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM collection_errors
		WHERE error_type = 'monitor' AND resolved = 1
		  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
	`, instanceID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// FindUnresolvedMonitorInstance 查找某实例的未结案监控中断记录（按最近优先），
// 返回解析后的明细与事件摘要。控制端受控停止需要把仅存于库的故障摘要写回
// 观察文件，保留本次退出的实例归属。明细非法的历史行跳过；不存在返回
// found=false。
func FindUnresolvedMonitorInstance(ctx context.Context, d *DB, instanceID string) (MonitorErrorDetail, string, bool, error) {
	if d == nil || strings.TrimSpace(instanceID) == "" {
		return MonitorErrorDetail{}, "", false, nil
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT message, detail FROM collection_errors
		WHERE error_type = 'monitor' AND resolved = 0
		  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
		ORDER BY created_at DESC, id DESC
	`, instanceID)
	if err != nil {
		return MonitorErrorDetail{}, "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var message, detailJSON string
		if err := rows.Scan(&message, &detailJSON); err != nil {
			return MonitorErrorDetail{}, "", false, err
		}
		detail, derr := ParseMonitorErrorDetail(detailJSON)
		if derr != nil {
			continue
		}
		return detail, message, true, nil
	}
	return MonitorErrorDetail{}, "", false, rows.Err()
}

// HasMonitorInstanceRecord 报告某中断实例是否已有监控中断记录（任意 resolved
// 状态）。控制端在受控停止覆盖观察文件里其他代的 failed 证据前用它判断证据
// 是否已持久化：已入库的缺口由库中的未结案事件继续提示，观察文件不再是唯一
// 来源，可以安全落定本次停止结果。
func HasMonitorInstanceRecord(ctx context.Context, d *DB, instanceID string) (bool, error) {
	if d == nil || strings.TrimSpace(instanceID) == "" {
		return false, nil
	}
	var n int
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM collection_errors
		WHERE error_type = 'monitor'
		  AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.instance_id') END = ?
	`, instanceID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetUnresolvedMonitorErrors 返回全部未解决的监控中断事件（按最近优先），
// 供启动协调器判定本轮补采是否覆盖对应来源、以及读端提示当前缺口。
func GetUnresolvedMonitorErrors(ctx context.Context, d *DB) ([]model.CollectionError, error) {
	return GetErrorsContext(ctx, d, ErrorFilter{Type: MonitorErrorType, Unresolved: true})
}

// ParseMonitorErrorDetail 解析监控中断事件的 detail 明细；非 JSON 或缺失
// instance_id 返回错误（历史行不参与结案与提示推导）。
func ParseMonitorErrorDetail(detail string) (MonitorErrorDetail, error) {
	var out MonitorErrorDetail
	if strings.TrimSpace(detail) == "" {
		return out, errors.New("监控中断明细为空")
	}
	if err := json.Unmarshal([]byte(detail), &out); err != nil {
		return out, fmt.Errorf("解析监控中断明细失败: %w", err)
	}
	if strings.TrimSpace(out.InstanceID) == "" {
		return out, errors.New("监控中断明细缺少 instance_id")
	}
	return out, nil
}
