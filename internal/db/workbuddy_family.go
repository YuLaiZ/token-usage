// internal/db/workbuddy_family.go
package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// 本文件实现 WorkBuddy 家族（精确 "WorkBuddy" + 严格合法的
// "WorkBuddy Expert:<hex>" 编码）在用量库内的读写原语，供 engine 的
// WorkBuddy 写事务（重归属、合并、清理与校验）调用。
//
// SQL 侧只用「精确 WorkBuddy OR 前缀粗筛」缩小候选，合法性由 Go 侧的
// model.IsWorkBuddyFamilyClient 严格校验；非法编码按错误返回（不静默纳入
// 家族迁移或删除），调用方整轮回滚并报告异常数据。所有 IN 集合分块参数化，
// 不把 client 原值拼入 SQL。

// workBuddyFamilyChunk 是家族 IN 查询的单块会话数上限（SQLite 变量余量充裕，
// 取与 mimocode reconciliation 批次一致的量级）。
const workBuddyFamilyChunk = 100

// workBuddyFamilyFilter 是家族粗筛的参数化条件：第一个 ? 绑定普通 client 名，
// 前缀 LIKE 无通配符语义风险（字面前缀匹配）。粗筛结果必须再经 Go 侧严格校验。
const workBuddyFamilyFilter = "(client = ? OR client LIKE 'WorkBuddy Expert:%')"

// ErrWorkBuddyIllegalFamilyClient 表示家族粗筛命中了非法 client 编码
// （不满足 model.IsWorkBuddyFamilyClient 的严格校验）。该错误属数据异常：
// 调用方不得继续迁移或删除，须整轮失败并报告。
type ErrWorkBuddyIllegalFamilyClient struct {
	Client string
}

func (e *ErrWorkBuddyIllegalFamilyClient) Error() string {
	return fmt.Sprintf("非法的 WorkBuddy 家族 client 编码: %q", e.Client)
}

// validateWorkBuddyFamilyClients 对粗筛出的 client 集合做严格校验。
func validateWorkBuddyFamilyClients(clients []string) error {
	for _, c := range clients {
		if !model.IsWorkBuddyFamilyClient(c) {
			return &ErrWorkBuddyIllegalFamilyClient{Client: c}
		}
	}
	return nil
}

// chunkStringSlice 按 size 分块遍历，fn 返回错误时停止并返回该错误。
func chunkStringSlice(all []string, size int, fn func(chunk []string) error) error {
	for start := 0; start < len(all); start += size {
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		if err := fn(all[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// WorkBuddyFamilyMessages 读取指定会话在家族粗筛下的全部 messages 行
// （按 session_id/client/id 升序，供确定性合并）。返回行已通过 client 严格校验。
func WorkBuddyFamilyMessages(ctx context.Context, q dbtx, sessionIDs []string) ([]model.Message, error) {
	var out []model.Message
	err := chunkStringSlice(sessionIDs, workBuddyFamilyChunk, func(chunk []string) error {
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, len(chunk)+2)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		// 占位符顺序与 SQL 一致：IN 的 id 在前，家族过滤的普通 client 名最后。
		args = append(args, model.ClientWorkBuddy)
		query := fmt.Sprintf(`
			SELECT id, session_id, client, date, ts, model, provider,
			       router_provider, router_model, router_name, directory, project,
			       input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens,
			       cache_create_tokens, reasoning_tokens, total_tokens
			FROM messages
			WHERE session_id IN (%s) AND %s
			ORDER BY session_id, client, id`,
			strings.Join(placeholders, ","), workBuddyFamilyFilter)
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查询 WorkBuddy 家族 messages 失败: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m model.Message
			if err := rows.Scan(
				&m.ID, &m.SessionID, &m.Client, &m.Date, &m.TS, &m.Model, &m.Provider,
				&m.RouterProvider, &m.RouterModel, &m.RouterName, &m.Directory, &m.Project,
				&m.InputTokens, &m.FreshInputTokens, &m.OutputTokens, &m.CacheReadTokens,
				&m.CacheCreateTokens, &m.ReasoningTokens, &m.TotalTokens,
			); err != nil {
				return fmt.Errorf("扫描 WorkBuddy 家族 messages 失败: %w", err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// 粗筛命中行经严格校验：非法编码不进入合并。
	seen := make(map[string]struct{}, 8)
	for _, m := range out {
		seen[m.Client] = struct{}{}
	}
	clients := make([]string, 0, len(seen))
	for c := range seen {
		clients = append(clients, c)
	}
	if err := validateWorkBuddyFamilyClients(clients); err != nil {
		return nil, err
	}
	return out, nil
}

// WorkBuddyFamilySessions 读取指定会话在家族粗筛下的全部 sessions 行
// （按 id/client 升序）。返回行已通过 client 严格校验。title_source/
// title_index_ts 是 Codex 专用列，WorkBuddy 家族行不读取（合并走通用
// upsertSessionMetaSQL，不触碰该列）。
func WorkBuddyFamilySessions(ctx context.Context, q dbtx, sessionIDs []string) ([]model.Session, error) {
	var out []model.Session
	err := chunkStringSlice(sessionIDs, workBuddyFamilyChunk, func(chunk []string) error {
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, len(chunk)+2)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		// 占位符顺序与 SQL 一致：IN 的 id 在前，家族过滤的普通 client 名最后。
		args = append(args, model.ClientWorkBuddy)
		query := fmt.Sprintf(`
			SELECT id, client, directory, project, title, parent_id, first_ts, last_ts
			FROM sessions
			WHERE id IN (%s) AND %s
			ORDER BY id, client`,
			strings.Join(placeholders, ","), workBuddyFamilyFilter)
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查询 WorkBuddy 家族 sessions 失败: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s model.Session
			if err := rows.Scan(
				&s.ID, &s.Client, &s.Directory, &s.Project, &s.Title, &s.ParentID,
				&s.FirstTS, &s.LastTS,
			); err != nil {
				return fmt.Errorf("扫描 WorkBuddy 家族 sessions 失败: %w", err)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, 8)
	for _, s := range out {
		seen[s.Client] = struct{}{}
	}
	clients := make([]string, 0, len(seen))
	for c := range seen {
		clients = append(clients, c)
	}
	if err := validateWorkBuddyFamilyClients(clients); err != nil {
		return nil, err
	}
	return out, nil
}

// WorkBuddyMessageOwner 是消息 id 在家族内的归属会话（身份冲突检测用）。
type WorkBuddyMessageOwner struct {
	MessageID string
	SessionID string
}

// QueryWorkBuddyMessageOwners 返回指定消息 id 在家族粗筛下的全部归属
// （同 id 允许多行：不同会话/不同 client 副本）。调用方据 session_id 判定
// 是否与本批归属冲突；非家族 client 的同 id 行不在此结果中。
func QueryWorkBuddyMessageOwners(ctx context.Context, q dbtx, messageIDs []string) ([]WorkBuddyMessageOwner, error) {
	var out []WorkBuddyMessageOwner
	err := chunkStringSlice(messageIDs, workBuddyFamilyChunk, func(chunk []string) error {
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, len(chunk)+2)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		// 占位符顺序与 SQL 一致：IN 的 id 在前，家族过滤的普通 client 名最后。
		args = append(args, model.ClientWorkBuddy)
		query := fmt.Sprintf("SELECT id, session_id FROM messages WHERE id IN (%s) AND %s",
			strings.Join(placeholders, ","), workBuddyFamilyFilter)
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查询 WorkBuddy 家族消息归属失败: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o WorkBuddyMessageOwner
			if err := rows.Scan(&o.MessageID, &o.SessionID); err != nil {
				return fmt.Errorf("扫描 WorkBuddy 家族消息归属失败: %w", err)
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteWorkBuddyFamilyRows 删除指定会话的家族粗筛行（messages 按 session_id、
// sessions 按 id）。调用方必须在同一事务内先读出并校验家族行、确认合并结果
// 将随后写回，删除才不丢数据；非家族 client 的同 id/session 行不受影响。
func DeleteWorkBuddyFamilyRows(ctx context.Context, q dbtx, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	for _, tableKey := range []struct{ table, keyCol string }{
		{"messages", "session_id"},
		{"sessions", "id"},
	} {
		err := chunkStringSlice(sessionIDs, workBuddyFamilyChunk, func(chunk []string) error {
			placeholders := make([]string, len(chunk))
			args := make([]interface{}, 0, len(chunk)+2)
			for i, id := range chunk {
				placeholders[i] = "?"
				args = append(args, id)
			}
			// 占位符顺序与 SQL 一致：IN 的 id 在前，家族过滤的普通 client 名最后。
			args = append(args, model.ClientWorkBuddy)
			query := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s) AND %s",
				tableKey.table, tableKey.keyCol, strings.Join(placeholders, ","), workBuddyFamilyFilter)
			if _, err := q.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("删除 %s WorkBuddy 家族行失败: %w", tableKey.table, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// WorkBuddyErrorResolvePatterns 是 WorkBuddy 元数据/重归属错误在
// collection_errors.message 中的固定可识别片段。完整无日期复核成功后按片段
// 解决该前缀下的未解决错误，不受错误最初登记日期限制；部分失败不得调用。
var WorkBuddyErrorResolvePatterns = []string{
	"workbuddy metadata failed:",
	"workbuddy rekey failed:",
}

// ResolveWorkBuddyErrorsByMessagePattern 解决 source=workbuddy 且 message 命中
// 任一固定片段的未解决采集错误（不限登记日期）。仅在完整无日期全量复核成功的
// 同一事务内调用：源库整体失败登记为当天、恢复后只有历史消息日期时，按日期
// 解决会永久残留，本函数补上该缺口。返回解决行数。
func ResolveWorkBuddyErrorsByMessagePattern(ctx context.Context, q dbtx) (int64, error) {
	var builder strings.Builder
	args := make([]interface{}, 0, len(WorkBuddyErrorResolvePatterns)+1)
	builder.WriteString("UPDATE collection_errors SET resolved = 1, updated_at = datetime('now') WHERE source = 'workbuddy' AND resolved = 0 AND (")
	for i, pattern := range WorkBuddyErrorResolvePatterns {
		if i > 0 {
			builder.WriteString(" OR ")
		}
		builder.WriteString("message LIKE ?")
		args = append(args, "%"+pattern+"%")
	}
	builder.WriteString(")")
	res, err := q.ExecContext(ctx, builder.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("解决 WorkBuddy 采集错误失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取 WorkBuddy 错误解决结果失败: %w", err)
	}
	return n, nil
}

// WorkBuddyFamilySessionIDsWithRows 返回用量库中存在家族粗筛行（messages 或
// sessions 任一）的会话 ID 升序列表，供无日期全量请求判定「仅历史库数据」的
// 刷新范围（完全无数据的源库行不创建空会话）。结果不经过严格校验：仅当这些
// 会话随后进入家族读/删路径时才校验其 client 编码。
func WorkBuddyFamilySessionIDsWithRows(ctx context.Context, q dbtx) ([]string, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`
		SELECT session_id FROM (
			SELECT DISTINCT session_id FROM messages WHERE %s
			UNION
			SELECT DISTINCT id AS session_id FROM sessions WHERE %s
		) ORDER BY session_id`,
		workBuddyFamilyFilter, workBuddyFamilyFilter),
		model.ClientWorkBuddy, model.ClientWorkBuddy)
	if err != nil {
		return nil, fmt.Errorf("查询 WorkBuddy 家族会话清单失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描 WorkBuddy 家族会话清单失败: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
