// internal/web/daemon_status.go
package web

// GET /api/daemon-status：独立的只读采集守护状态接口。不传 from/to 时只返回
// 全局 collection 对象；显式传入 from+to 时按用量页同规则校验区间并附加
// range_errors（区间内未解决普通采集失败摘要）。采集状态与用量查询互相独立
// 降级：状态读取失败只降级为 unknown，不影响用量查询，反之亦然。

import (
	"context"
	"net/http"
	"time"

	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// DaemonStatusProvider 提供采集守护状态判定（只读）。返回零值 Collection
// 视为不可判定，handler 按 unknown 输出。生产装配读 observation 与只读库。
type DaemonStatusProvider func(ctx context.Context) daemonstatus.Collection

// WithDaemonStatusProvider 注入采集状态来源（/api/daemon-status）。
func WithDaemonStatusProvider(p DaemonStatusProvider) ServerOption {
	return func(s *server) { s.daemonStatus = p }
}

// DaemonStatusResponse 是 GET /api/daemon-status 的载荷。collection 与
// daemon status --format json 共用同一对象与状态判定；range_errors 仅 Web 返回。
type DaemonStatusResponse struct {
	Collection  daemonstatus.Collection `json:"collection"`
	RangeErrors *rangeErrorsJSON        `json:"range_errors"`
}

// rangeErrorsJSON 是区间普通采集失败摘要；未请求区间或区间记录暂不可读时
// 为 null——不可解释为零错误。
type rangeErrorsJSON struct {
	Count  int             `json:"count"`
	Latest *rangeErrorItem `json:"latest"`
}

// rangeErrorItem 是最近一条未解决普通采集失败的安全摘要（不含原始错误、
// 路径、堆栈或日志；summary 为面向用户的通用简述）。
type rangeErrorItem struct {
	CreatedAt     string `json:"created_at"`
	Source        string `json:"source"`         // 机器可读的配置键
	SourceDisplay string `json:"source_display"` // 展示名（与 collection.interruption 同一映射）
	Summary       string `json:"summary"`
}

func (s *server) handleDaemonStatus(w http.ResponseWriter, r *http.Request) {
	resp := DaemonStatusResponse{
		Collection: daemonstatus.Collection{State: daemonstatus.StateUnknown},
	}
	if s.daemonStatus != nil {
		if col := s.daemonStatus(r.Context()); col.State != "" {
			resp.Collection = col
		}
	}
	// 区间摘要只在显式请求区间时查询（切换日期不会让全局中断状态消失）。
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from != "" && to != "" {
		dates, _, _, _, _, perr := parseRangeQuery(r, time.Now())
		if perr != nil {
			writeError(w, *perr)
			return
		}
		if summary, err := s.q.QueryUnresolvedErrorSummary(r.Context(), dates); err == nil {
			re := &rangeErrorsJSON{Count: summary.Count}
			if summary.HasLatest {
				re.Latest = &rangeErrorItem{
					CreatedAt:     summary.LatestAt,
					Source:        summary.LatestSource,
					SourceDisplay: model.ClientKeyDisplayName(summary.LatestSource),
					Summary:       ui.Bi("collection failed", "采集失败"),
				}
			}
			resp.RangeErrors = re
		}
		// 查询失败：保持 range_errors=null（不可解释为零错误）。
	}
	writeJSON(w, http.StatusOK, resp)
}
