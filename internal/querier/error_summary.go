// internal/querier/error_summary.go
package querier

// 采集失败摘要查询（serve 的 /api/daemon-status 区间摘要专用）：只统计
// error_type='error' 的未解决普通采集失败，不混入监控中断（monitor）与警告。

import (
	"context"
)

// UnresolvedErrorSummary 是区间内未解决普通采集失败的摘要。
type UnresolvedErrorSummary struct {
	Count        int
	LatestAt     string // 最近一条的入库时刻（SQLite UTC datetime 字符串）
	LatestSource string
	HasLatest    bool
}

// QueryUnresolvedErrorSummary 统计指定日期集合内未解决的普通采集失败数与
// 最近一条的入库时刻/来源。只读；dates 为空返回零值。
func (q *Querier) QueryUnresolvedErrorSummary(ctx context.Context, dates []string) (UnresolvedErrorSummary, error) {
	var out UnresolvedErrorSummary
	if len(dates) == 0 {
		return out, nil
	}
	placeholders, args := buildPlaceholders(dates)
	if err := q.queryRowContext(ctx,
		`SELECT COUNT(*) FROM collection_errors
		 WHERE error_type = 'error' AND resolved = 0 AND date IN (`+placeholders+`)`,
		args...).Scan(&out.Count); err != nil {
		return out, err
	}
	if out.Count == 0 {
		return out, nil
	}
	row := q.queryRowContext(ctx,
		`SELECT created_at, source FROM collection_errors
		 WHERE error_type = 'error' AND resolved = 0 AND date IN (`+placeholders+`)
		 ORDER BY created_at DESC, id DESC LIMIT 1`, args...)
	if err := row.Scan(&out.LatestAt, &out.LatestSource); err == nil {
		out.HasLatest = true
	}
	return out, nil
}
