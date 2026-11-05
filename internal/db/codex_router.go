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
			COALESCE(router_name,''), COALESCE(created_at,0), COALESCE(request_id,''),
			COALESCE(json_extract(CASE WHEN json_valid(raw_data) THEN raw_data END,'$.request_model'),'')
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
		if err := rows.Scan(&l.SessionID, &l.ProviderName, &l.Model, &l.RouterName, &l.CreatedAt, &l.RequestID, &l.RequestModel); err != nil {
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
		query := `SELECT id, session_id, client, ts, model, agent_parent_id FROM messages
			WHERE client IN ('Codex App','Codex CLI')
			  AND session_id IN (` + strings.Join(placeholders, ",") + `)
			ORDER BY ts, id`
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("查询 codex messages 失败: %w", err)
		}
		for rows.Next() {
			var m model.Message
			if err := rows.Scan(&m.ID, &m.SessionID, &m.Client, &m.TS, &m.Model, &m.AgentParentID); err != nil {
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

// codexFamilyChunkSize 是家族关系查询的 session 分块大小。该查询含两组 IN
// 占位符（session_id 与 agent_parent_id 各一套），按 2×chunk ≤ 999 参数上限
// 取 400（SQLite 变量数上限的保守兼容）。
const codexFamilyChunkSize = 400

// QueryCodexSessionParents 查询 session 集合及其子代理会话的父子关系，供归因
// 家族展开迭代使用（engine.expandCodexFamily 反复调用直到无新会话）。
// 返回 map[会话]父会话，只含 agent_parent_id 非空的条目。
//
// 数据源顺序：sessions 表（权威——每个 rollout 都会产出会话元数据，即使该
// 子代理没有可入库消息，中间层关系也不丢，多层父链因此完整）→ messages 表
// （兜底：旧库或只有消息行的异常路径）。两套 IN 并列（成员自身 ∪ 父会话在
// 集合内的直接子代），命中「本轮只触达子代理」的入口。sessions 先写，同键
// 冲突时保留 sessions 的值。
func QueryCodexSessionParents(ctx context.Context, q dbtx, sessionIDs []string) (map[string]string, error) {
	out := make(map[string]string)
	sources := []struct {
		table      string
		sessionCol string
	}{
		{"sessions", "id"},
		{"messages", "session_id"},
	}
	for _, src := range sources {
		for start := 0; start < len(sessionIDs); start += codexFamilyChunkSize {
			end := start + codexFamilyChunkSize
			if end > len(sessionIDs) {
				end = len(sessionIDs)
			}
			chunk := sessionIDs[start:end]
			placeholders := make([]string, 0, len(chunk))
			args := make([]interface{}, 0, len(chunk)*2)
			for _, sid := range chunk {
				if sid == "" {
					continue // 空 session 不参与家族关系（与归因链路同口径）
				}
				placeholders = append(placeholders, "?")
				args = append(args, sid)
			}
			if len(placeholders) == 0 {
				continue
			}
			// 两套 IN 复用同一组参数：参数切片拼两遍。
			twice := make([]interface{}, 0, len(args)*2)
			twice = append(twice, args...)
			twice = append(twice, args...)
			ph := strings.Join(placeholders, ",")
			query := `SELECT DISTINCT ` + src.sessionCol + `, agent_parent_id FROM ` + src.table + `
				WHERE client IN ('Codex App','Codex CLI')
				  AND agent_parent_id != ''
				  AND (` + src.sessionCol + ` IN (` + ph + `) OR agent_parent_id IN (` + ph + `))
				ORDER BY ` + src.sessionCol + `, agent_parent_id`
			rows, err := q.QueryContext(ctx, query, twice...)
			if err != nil {
				return nil, fmt.Errorf("查询 codex 家族关系失败（%s）: %w", src.table, err)
			}
			for rows.Next() {
				var sid, parent string
				if err := rows.Scan(&sid, &parent); err != nil {
					rows.Close()
					return nil, fmt.Errorf("扫描 codex 家族关系行失败: %w", err)
				}
				if sid == "" || parent == "" || sid == parent {
					continue
				}
				if _, ok := out[sid]; !ok {
					out[sid] = parent // 同 session 多父属数据异常，取字典序首个稳定值
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	return out, nil
}

// NormalizeCodexRouterSessionID 剥单个 codex_ 前缀（cc-switch codex proxy 行的
// session_id = codex_{rollout 文件名 UUID}）。只用于 codex 归因链路的 session
// 归一，不改动 raw 存储原值。
func NormalizeCodexRouterSessionID(sid string) string {
	return strings.TrimPrefix(sid, "codex_")
}

// NormalizeCodexModelID 归一 codex 归因的模型 id 用于跨端比较：剥聚合模式的
// `ccs-<provider>/` 前缀（rollout 记录 ccs-opencode/deepseek-v4.1-flash 形态，
// cc-switch proxy 行记录真实上游模型 deepseek-v4.1-flash）、去掉方括号窗口
// 后缀（如 [1M]）、trim + lower。空串原样返回。
func NormalizeCodexModelID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "ccs-") {
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[i+1:]
		}
	}
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// ccsProviderPrefix 提取聚合模型 ID 的供应商段（`ccs-<provider>/<model>`），
// 非聚合形态返回空。用于区分「不同供应商提供的同名模型」。
func ccsProviderPrefix(modelID string) string {
	s := strings.TrimSpace(modelID)
	if !strings.HasPrefix(s, "ccs-") {
		return ""
	}
	i := strings.Index(s, "/")
	if i < 0 {
		return ""
	}
	return strings.ToLower(s[len("ccs-"):i])
}

// codexModelsCompatible 判断 message 与 proxy 行是否指向同一请求线路：
//   - **优先按请求模型比较**：`message.model` 是客户端请求的模型，
//     `request_model` 是 cc-switch 记录的同一请求模型——两者同源，必须相等
//     （归一后）。不能先比 `log.model`（上游实际模型）：cc-switch 支持把请求
//     模型映射为不同上游模型（如请求 gpt-5.6 → 上游 gpt-5.6-terra），上游名
//     与请求名本就不同，先比上游会错误拒绝合法归因。
//   - 旧数据无 `request_model` 时回退比较上游模型（`log.model`），与历史行为
//     一致；任一为空时放行（源缺失不构成拒绝证据）。
//   - 双方都带聚合供应商前缀时（message.model 与 request_model 均为
//     `ccs-<provider>/...`），前缀也必须一致：不同供应商的同名模型不互相配对。
func codexModelsCompatible(messageModel, logModel, requestModel string) bool {
	msg := NormalizeCodexModelID(messageModel)
	req := NormalizeCodexModelID(requestModel)
	if req != "" {
		// 请求模型优先：同源比较（映射场景下上游模型与请求模型可以不同）。
		if msg != "" && msg != req {
			return false
		}
	} else {
		// 旧库无 request_model：回退上游模型比较。
		upstream := NormalizeCodexModelID(logModel)
		if msg != "" && upstream != "" && msg != upstream {
			return false
		}
	}
	msgProvider := ccsProviderPrefix(messageModel)
	reqProvider := ccsProviderPrefix(requestModel)
	if msgProvider != "" && reqProvider != "" && msgProvider != reqProvider {
		return false
	}
	return true
}

// CodexFamilyRoot 返回会话在归因家族中的根会话 id：沿 parents 的父链向上
// （父 = 子代理 rollout 的 parent_thread_id），最大 32 跳防御成环/损坏数据；
// 无父关系的会话返回自身。归因桶与 proxy 行会话都经同一函数归一，保证
// 父会话与其子代理落在同一家族桶内。
func CodexFamilyRoot(sessionID string, parents map[string]string) string {
	current := sessionID
	for i := 0; i < 32; i++ {
		next, ok := parents[current]
		if !ok || next == "" || next == current {
			return current
		}
		current = next
	}
	return current
}

// MatchCodexRouterAttributions 按「同家族 + 时间窗最近邻」把 codex proxy 路由行
// 配对到 codex message，产出归因回填信息。时间单位先行归一：router log 的
// created_at 为 Unix 秒、message 的 ts 为 Unix 毫秒，比较基准统一毫秒
// （windowSec 参数按秒传入，入口换算）。配对规则：
//   - proxy 行按 (created_at, request_id) 升序迭代（纯函数内自排序，不依赖调用方序）；
//   - **家族桶**：message 的 AgentParentID（子代理 rollout 的 parent_thread_id）
//     让它与父会话共桶；proxy 行 session 恒为主会话，经 CodexFamilyRoot 归一
//     后进入同一桶（覆盖单层与多层子代理）；
//   - **模型校验**：message 与 proxy 行模型归一后必须一致才允许配对——子代理
//     使用与主会话不同的模型时，proxy 行不会被错的会话消费（异模型子代理
//     是 Codex 的常规用法，缺此校验会把子代理行配给主会话造成归因错误）；
//   - 每条 proxy 行在窗口内未消费的同家族、同模型 message 中选 |Δt| 最小者；
//   - |Δt| 相同按 message id 字典序取稳定结果；
//   - 每 message 在单次调用内至多消费一次，单次结果不重复归因
//     （跨轮次重算不撤销已回填的归因——纠偏依赖非空新值覆盖语义）。
//
// Client 取被匹配 message 行自身的 client 列（Codex App / Codex CLI 自然覆盖），
// Provider/Model/RouterName 取 proxy 行。
//
// 兼容入口：家族父子关系仅从 messages 行构建（单路径调用/测试）。生产归因走
// MatchCodexRouterAttributionsWithParents，由 sessions 表提供权威父关系
// （中间层代理没有消息时父链仍完整）。
func MatchCodexRouterAttributions(logs []model.RouterLog, messages []model.Message, windowSec int64) []model.RouterAttribution {
	return MatchCodexRouterAttributionsWithParents(logs, messages, parentsFromMessages(messages), windowSec)
}

// parentsFromMessages 从消息行提取 session → parent 关系。同一会话多行应一致，
// 取首个非空值；空值/自环不入表。
func parentsFromMessages(messages []model.Message) map[string]string {
	parents := make(map[string]string, len(messages))
	for _, m := range messages {
		if m.SessionID == "" || m.AgentParentID == "" || m.AgentParentID == m.SessionID {
			continue
		}
		if _, ok := parents[m.SessionID]; !ok {
			parents[m.SessionID] = m.AgentParentID
		}
	}
	return parents
}

// MatchCodexRouterAttributionsWithParents 是归因匹配主入口。parents 为家族
// 父子关系（session → parent）：生产由 sessions 表查询提供（见
// QueryCodexSessionParents），nil 时回退从 messages 行构建。
func MatchCodexRouterAttributionsWithParents(logs []model.RouterLog, messages []model.Message, parents map[string]string, windowSec int64) []model.RouterAttribution {
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
	if parents == nil {
		parents = parentsFromMessages(messages)
	}
	// 按家族根分桶把每条 proxy 行的最近邻扫描限制在其家族桶内
	// （全量回填入口下两侧规模随历史增长，避免 O(|logs|×|messages|) 整体双扫）；
	// 桶内保持 message 下标序，窗口、Δt、id 字典序 tie-break、单次消费语义
	// 与线性扫描完全一致。空 session 的 message 本就不可匹配，不入桶。
	buckets := make(map[string][]int, len(messages))
	for i, m := range messages {
		if m.SessionID == "" {
			continue
		}
		root := CodexFamilyRoot(m.SessionID, parents)
		buckets[root] = append(buckets[root], i)
	}
	var attributions []model.RouterAttribution
	for _, l := range sorted {
		session := NormalizeCodexRouterSessionID(l.SessionID)
		if session == "" {
			continue
		}
		// proxy 行 session 恒为主会话 id；经家族根归一同时兼容未来若源侧
		// 改为写子代理独立 id 的形态（归一到同一家族桶）。
		bucket := buckets[CodexFamilyRoot(session, parents)]
		logTsMs := l.CreatedAt * 1000
		best := -1
		var bestDelta int64
		for _, i := range bucket {
			m := messages[i]
			if consumed[i] {
				continue
			}
			if !codexModelsCompatible(m.Model, l.Model, l.RequestModel) {
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
