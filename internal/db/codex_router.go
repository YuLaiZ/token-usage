package db

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// CodexRouterMatchWindowSec 是 codex proxy 行与 message 的最近邻配对时间窗（秒）。
// 覆盖 cc-switch 异步写库延迟 + 60s 会话同步节拍下的常见偏差（其自身防双算指纹
// 窗口 ±600s 的上半区）；过大有相邻请求错配风险（Codex 会话基本串行，相邻 message
// 间隔远大于完成偏差）。未经真实数据标定的工程估值：错配/漏配反馈时第一调参点，
// 单点可调。
const CodexRouterMatchWindowSec = 300

// QueryCodexRouterLogsBySessions 查询 session 集合关联的全部 codex proxy 路由行
// （不限本轮、不限日期，供三入口「双侧全量」匹配）。session_id 为 cc-switch 的
// codex_{uuid} 前缀形态或裸 uuid，Go 侧双形态展开后 IN 匹配；行限定
// router_name + app_type='codex' + data_source='proxy'（codex_session 同步行与
// claude 行天然排除）。session 集合按 routerLogChunkSize 分块，块间结果拼接
// （仅块内有序，跨块无全局时间序；MatchCodexRouterAttributions 自行排序，
// 不依赖本函数返回序）。
func QueryCodexRouterLogsBySessions(ctx context.Context, q dbtx, routerName string, sessionIDs []string) ([]model.RouterLog, error) {
	var all []model.RouterLog
	for start := 0; start < len(sessionIDs); start += routerLogChunkSize {
		end := start + routerLogChunkSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		chunk := sessionIDs[start:end]
		barePh := make([]string, 0, len(chunk))
		prefixedPh := make([]string, 0, len(chunk))
		args := make([]interface{}, 0, len(chunk)*2+1)
		args = append(args, routerName)
		for _, sid := range chunk {
			if sid == "" {
				continue // 空 session 不参与匹配（源行 session_id 为空属不可关联）
			}
			barePh = append(barePh, "?")
			prefixedPh = append(prefixedPh, "'codex_'||?")
			args = append(args, sid)
		}
		if len(barePh) == 0 {
			continue
		}
		for _, sid := range chunk {
			if sid == "" {
				continue
			}
			args = append(args, sid)
		}
		query := `SELECT COALESCE(session_id,''), COALESCE(provider_name,''), COALESCE(model,''),
			COALESCE(router_name,''), COALESCE(created_at,0), COALESCE(request_id,'')
			FROM raw_router_logs
			WHERE router_name=? AND app_type='codex' AND data_source='proxy'
			  AND (session_id IN (` + strings.Join(barePh, ",") + `)
			       OR session_id IN (` + strings.Join(prefixedPh, ",") + `))
			ORDER BY created_at, request_id`
		logs, err := scanCodexRouterLogs(ctx, q, query, args)
		if err != nil {
			return nil, err
		}
		all = append(all, logs...)
	}
	return all, nil
}

func scanCodexRouterLogs(ctx context.Context, q dbtx, query string, args []interface{}) ([]model.RouterLog, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 codex router logs 失败: %w", err)
	}
	defer rows.Close()
	var logs []model.RouterLog
	for rows.Next() {
		var l model.RouterLog
		if err := rows.Scan(&l.SessionID, &l.ProviderName, &l.Model, &l.RouterName, &l.CreatedAt, &l.RequestID); err != nil {
			return nil, fmt.Errorf("扫描 codex router log 行失败: %w", err)
		}
		l.AppType = "codex"
		l.DataSource = "proxy"
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// QueryCodexMessagesBySessions 查询 session 集合关联的全部 codex messages
// （Codex App / Codex CLI 两显示名；不限本轮、不限日期）。
// session 集合按 routerLogChunkSize 分块（仅块内有序，跨块无全局序）。
func QueryCodexMessagesBySessions(ctx context.Context, q dbtx, sessionIDs []string) ([]model.Message, error) {
	var all []model.Message
	for start := 0; start < len(sessionIDs); start += routerLogChunkSize {
		end := start + routerLogChunkSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		chunk := sessionIDs[start:end]
		placeholders := make([]string, 0, len(chunk))
		args := make([]interface{}, 0, len(chunk))
		for _, sid := range chunk {
			if sid == "" {
				continue // 空 session 不参与匹配（源行 session_id 为空属不可关联）
			}
			placeholders = append(placeholders, "?")
			args = append(args, sid)
		}
		if len(placeholders) == 0 {
			continue
		}
		query := `SELECT id, session_id, client, ts FROM messages
			WHERE client IN ('Codex App','Codex CLI')
			  AND session_id IN (` + strings.Join(placeholders, ",") + `)
			ORDER BY ts, id`
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("查询 codex messages 失败: %w", err)
		}
		for rows.Next() {
			var m model.Message
			if err := rows.Scan(&m.ID, &m.SessionID, &m.Client, &m.TS); err != nil {
				rows.Close()
				return nil, fmt.Errorf("扫描 codex message 行失败: %w", err)
			}
			all = append(all, m)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return all, nil
}

// NormalizeCodexRouterSessionID 剥单个 codex_ 前缀（cc-switch codex proxy 行的
// session_id = codex_{rollout 文件名 UUID}）。只用于 codex 归因链路的 session
// 归一，不改动 raw 存储原值。
func NormalizeCodexRouterSessionID(sid string) string {
	return strings.TrimPrefix(sid, "codex_")
}

// MatchCodexRouterAttributions 按「同 session + 时间窗最近邻」把 codex proxy 路由行
// 配对到 codex message，产出归因回填信息。时间单位先行归一：router log 的
// created_at 为 Unix 秒、message 的 ts 为 Unix 毫秒，比较基准统一毫秒
// （windowSec 参数按秒传入，入口换算）。配对规则：
//   - proxy 行按 (created_at, request_id) 升序迭代（纯函数内自排序，不依赖调用方序）；
//   - 每条 proxy 行在窗口内未消费的同 session message 中选 |Δt| 最小者；
//   - |Δt| 相同按 message id 字典序取稳定结果；
//   - 每 message 在单次调用内至多消费一次，单次结果不重复归因
//     （跨轮次重算不撤销已回填的归因——纠偏依赖非空新值覆盖语义）。
//
// Client 取被匹配 message 行自身的 client 列（Codex App / Codex CLI 自然覆盖），
// Provider/Model/RouterName 取 proxy 行。
func MatchCodexRouterAttributions(logs []model.RouterLog, messages []model.Message, windowSec int64) []model.RouterAttribution {
	sorted := make([]model.RouterLog, len(logs))
	copy(sorted, logs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt != sorted[j].CreatedAt {
			return sorted[i].CreatedAt < sorted[j].CreatedAt
		}
		return sorted[i].RequestID < sorted[j].RequestID
	})

	windowMs := windowSec * 1000
	consumed := make([]bool, len(messages))
	// 按 session 分桶把每条 proxy 行的最近邻扫描限制在其 session 桶内
	// （全量回填入口下两侧规模随历史增长，避免 O(|logs|×|messages|) 整体双扫）；
	// 桶内保持 message 下标序，窗口、Δt、id 字典序 tie-break、单次消费语义
	// 与线性扫描完全一致。空 session 的 message 本就不可匹配，不入桶。
	buckets := make(map[string][]int, len(messages))
	for i, m := range messages {
		if m.SessionID == "" {
			continue
		}
		buckets[m.SessionID] = append(buckets[m.SessionID], i)
	}
	var attributions []model.RouterAttribution
	for _, l := range sorted {
		session := NormalizeCodexRouterSessionID(l.SessionID)
		if session == "" {
			continue
		}
		logTsMs := l.CreatedAt * 1000
		best := -1
		var bestDelta int64
		for _, i := range buckets[session] {
			m := messages[i]
			if consumed[i] {
				continue
			}
			delta := m.TS - logTsMs
			if delta < 0 {
				delta = -delta
			}
			if delta > windowMs {
				continue
			}
			if best < 0 || delta < bestDelta || (delta == bestDelta && m.ID < messages[best].ID) {
				best = i
				bestDelta = delta
			}
		}
		if best < 0 {
			continue
		}
		consumed[best] = true
		attributions = append(attributions, model.RouterAttribution{
			Client:     messages[best].Client,
			MessageID:  messages[best].ID,
			Provider:   l.ProviderName,
			Model:      l.Model,
			RouterName: l.RouterName,
			CreatedAt:  l.CreatedAt,
			RequestID:  l.RequestID,
		})
	}
	return attributions
}
