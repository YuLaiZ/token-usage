package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// dbtx 抽象 *DB 与 *sql.Tx，使新 DAO 可在外部事务中复用。
// *DB 类型已有 ExecContext/QueryContext/QueryRowContext，天然满足此接口。
type dbtx interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// MarkCollected 记录采集日志。collection_log.session_count 实际为 message count。
func MarkCollected(ctx context.Context, q dbtx, date, source string, count int) error {
	_, err := q.ExecContext(ctx, `
		INSERT OR REPLACE INTO collection_log (date, source, session_count)
		VALUES (?, ?, ?)
	`, date, source, count)
	return err
}

// UpsertFileScanLog 写入 startup 跳过门的文件级状态记录（dbtx 形态，可在
// persistClientBatch 事务内与消息同事务提交）。调用方只应对 fullyParsed 且
// 读前读后快照一致（identity 有效）的文件写记录。
func UpsertFileScanLog(ctx context.Context, q dbtx, logs []model.FileScanLog) error {
	stmt := `INSERT OR REPLACE INTO file_scan_log (
		client, file_path, file_identity, mtime_ns, file_size, parser_version
	) VALUES (?, ?, ?, ?, ?, ?)`

	for _, l := range logs {
		_, err := q.ExecContext(ctx, stmt,
			l.Client, l.FilePath, l.FileIdentity, l.MtimeNS, l.FileSize, l.ParserVersion,
		)
		if err != nil {
			return fmt.Errorf("更新 file_scan_log 失败: %w", err)
		}
	}

	return nil
}

func RecordError(ctx context.Context, d *DB, date, source, msg, detail string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO collection_errors (date, source, error_type, message, detail)
		VALUES (?, ?, 'error', ?, ?)
	`, date, source, msg, detail)
	return err
}

func RecordWarning(ctx context.Context, d *DB, date, source, msg, detail string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO collection_errors (date, source, error_type, message, detail)
		VALUES (?, ?, 'warning', ?, ?)
	`, date, source, msg, detail)
	return err
}

func GetUnresolvedErrors(d *DB) ([]model.CollectionError, error) {
	return GetErrors(d, ErrorFilter{Unresolved: true})
}

func ResolveError(ctx context.Context, d *DB, id int) error {
	_, err := d.ExecContext(ctx, `
		UPDATE collection_errors SET resolved = 1, updated_at = datetime('now')
		WHERE id = ?
	`, id)
	return err
}

func IncrementRetryCount(ctx context.Context, d *DB, id int) error {
	_, err := d.ExecContext(ctx, `
		UPDATE collection_errors SET retry_count = retry_count + 1, updated_at = datetime('now')
		WHERE id = ?
	`, id)
	return err
}

type ErrorFilter struct {
	Dates      []string
	Source     string
	Type       string
	Unresolved bool
}

func RecordErrorsByDate(ctx context.Context, d *DB, dates []string, source, message, detail string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启错误记录事务失败: %w", err)
	}
	defer tx.Rollback()
	for _, date := range dates {
		result, err := tx.ExecContext(ctx, `UPDATE collection_errors
			SET detail = ?, updated_at = datetime('now')
			WHERE date = ? AND source = ? AND error_type = 'error'
			  AND message = ? AND resolved = 0`, detail, date, source, message)
		if err != nil {
			return fmt.Errorf("刷新 %s/%s 采集错误失败: %w", source, date, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取 %s/%s 错误刷新结果失败: %w", source, date, err)
		}
		if updated > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO collection_errors
			(date, source, error_type, message, detail)
			VALUES (?, ?, 'error', ?, ?)`, date, source, message, detail); err != nil {
			return fmt.Errorf("记录 %s/%s 采集错误失败: %w", source, date, err)
		}
	}
	return tx.Commit()
}

func IncrementRetryCountByDateSource(ctx context.Context, d *DB, date, source string) (int64, error) {
	result, err := d.ExecContext(ctx, `UPDATE collection_errors
		SET retry_count = retry_count + 1, updated_at = datetime('now')
		WHERE date = ? AND source = ? AND error_type = 'error' AND resolved = 0`, date, source)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func ResolveErrorsByDateSource(ctx context.Context, q dbtx, date, source string) (int64, error) {
	result, err := q.ExecContext(ctx, `UPDATE collection_errors
		SET resolved = 1, updated_at = datetime('now')
		WHERE date = ? AND source = ? AND resolved = 0`, date, source)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func GetErrors(d *DB, filter ErrorFilter) ([]model.CollectionError, error) {
	return GetErrorsContext(context.Background(), d, filter)
}

func GetErrorsContext(ctx context.Context, d *DB, filter ErrorFilter) ([]model.CollectionError, error) {
	if d == nil {
		return nil, fmt.Errorf("查询 collection_errors 时数据库不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, date, source, error_type, message, detail,
		retry_count, resolved, created_at, updated_at
		FROM collection_errors WHERE 1=1`
	var args []interface{}
	if len(filter.Dates) > 0 {
		marks := make([]string, len(filter.Dates))
		for i, date := range filter.Dates {
			marks[i] = "?"
			args = append(args, date)
		}
		query += " AND date IN (" + strings.Join(marks, ",") + ")"
	}
	if filter.Source != "" {
		query += " AND source = ?"
		args = append(args, filter.Source)
	}
	if filter.Type != "" {
		query += " AND error_type = ?"
		args = append(args, filter.Type)
	}
	if filter.Unresolved {
		query += " AND resolved = 0"
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.CollectionError
	for rows.Next() {
		var e model.CollectionError
		var resolved int
		if err := rows.Scan(&e.ID, &e.Date, &e.Source, &e.ErrorType, &e.Message,
			&e.Detail, &e.RetryCount, &resolved, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Resolved = resolved != 0
		result = append(result, e)
	}
	return result, rows.Err()
}

// UpsertRawClientSessions 批量写入客户端原始会话数据（staging 层，未接入 engine，不是 token 真相源）。
func UpsertRawClientSessions(ctx context.Context, d *DB, sessions []model.RawClientSession) (int, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt := `INSERT OR REPLACE INTO raw_client_sessions (
		session_id, client, directory, model, title,
		created_at, last_active_at,
		input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens,
		raw_data, source_file, file_mtime, file_size
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	count := 0
	for _, s := range sessions {
		_, err := tx.ExecContext(ctx, stmt,
			s.SessionID, s.Client, s.Directory, s.Model, s.Title,
			s.CreatedAt, s.LastActiveAt,
			s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheCreateTokens, s.TotalTokens,
			s.RawData, s.SourceFile, s.FileMtime, s.FileSize,
		)
		if err != nil {
			return count, fmt.Errorf("插入 raw_client_session %q 失败: %w", s.SessionID, err)
		}
		count++
	}

	return count, tx.Commit()
}

// GetCollectedDates 查询已采集的日期列表
func GetCollectedDates(d *DB, source string) ([]string, error) {
	return GetCollectedDatesContext(context.Background(), d, source)
}

func GetCollectedDatesContext(ctx context.Context, d *DB, source string) ([]string, error) {
	if d == nil {
		return nil, fmt.Errorf("查询 collection_log 时数据库不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := d.QueryContext(ctx, `SELECT date FROM collection_log WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dates []string
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			return nil, err
		}
		dates = append(dates, date)
	}

	return dates, rows.Err()
}

// GetFileScanLogs 查询 client 的跳过门状态记录（dbtx 形态），键为 file_path。
func GetFileScanLogs(ctx context.Context, q dbtx, client string) (map[string]model.FileScanLog, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT client, file_path, file_identity, mtime_ns, file_size, parser_version, updated_at FROM file_scan_log WHERE client = ?`, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make(map[string]model.FileScanLog)
	for rows.Next() {
		var l model.FileScanLog
		if err := rows.Scan(&l.Client, &l.FilePath, &l.FileIdentity, &l.MtimeNS, &l.FileSize, &l.ParserVersion, &l.UpdatedAt); err != nil {
			return nil, err
		}
		logs[l.FilePath] = l
	}

	return logs, rows.Err()
}

// UpsertRawRouterLogs 批量写入路由中间件日志（staging 层）
// message_id = request_id 前缀提取（claude "session:" / codex "session:codex:{pid}:"）
// 主键 (request_id, router_name)，INSERT OR REPLACE 幂等（REPLACE 重置 collected_at
// 为当前时刻属既有副作用；data_source 语义由采集侧保证与 v3 迁移口径一致，
// REPLACE 不得把既有 'codex_session' 标记降级为 'proxy'）
func UpsertRawRouterLogs(ctx context.Context, q dbtx, logs []model.RouterLog) (int, error) {
	stmt := `INSERT OR REPLACE INTO raw_router_logs (
		request_id, message_id, router_name, session_id, app_type, model,
		provider_id, provider_name, input_tokens, output_tokens,
		cache_read_tokens, cache_create_tokens, created_at, data_source, raw_data
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	count := 0
	for _, l := range logs {
		_, err := q.ExecContext(ctx, stmt,
			l.RequestID, l.MessageID, l.RouterName, l.SessionID, l.AppType, l.Model,
			l.ProviderID, l.ProviderName, l.InputTokens, l.OutputTokens,
			l.CacheReadTokens, l.CacheCreateTokens, l.CreatedAt, l.DataSource, l.RawData,
		)
		if err != nil {
			return count, fmt.Errorf("插入 raw_router_log %q 失败: %w", l.RequestID, err)
		}
		count++
	}
	return count, nil
}

const upsertMessageSQL = `INSERT INTO messages (
	id, session_id, client, date, ts, model, provider,
	router_provider, router_model, router_name, directory, project,
	input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens,
	cache_create_tokens, reasoning_tokens, total_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(client, id) DO UPDATE SET
	ts = CASE WHEN excluded.ts < messages.ts THEN excluded.ts ELSE messages.ts END,
	date = CASE WHEN excluded.ts < messages.ts THEN excluded.date ELSE messages.date END,
	session_id = CASE WHEN excluded.ts < messages.ts THEN excluded.session_id ELSE messages.session_id END,
	directory = CASE WHEN excluded.ts < messages.ts THEN excluded.directory ELSE messages.directory END,
	project = CASE WHEN excluded.ts < messages.ts THEN excluded.project ELSE messages.project END,
	model = excluded.model,
	provider = excluded.provider,
	router_provider = CASE WHEN excluded.router_provider != '' THEN excluded.router_provider ELSE messages.router_provider END,
	router_model = CASE WHEN excluded.router_model != '' THEN excluded.router_model ELSE messages.router_model END,
	router_name = CASE WHEN excluded.router_name != '' THEN excluded.router_name ELSE messages.router_name END,
	input_tokens = excluded.input_tokens,
	fresh_input_tokens = excluded.fresh_input_tokens,
	output_tokens = excluded.output_tokens,
	cache_read_tokens = excluded.cache_read_tokens,
	cache_create_tokens = excluded.cache_create_tokens,
	reasoning_tokens = excluded.reasoning_tokens,
	total_tokens = excluded.total_tokens`

// UpsertMessages 批量 UPSERT 消息行。任何一行失败立即返回，让调用方事务回滚。
// 归因（session_id/date/directory/project）取较早 ts 的版本；token/model/provider 总以新值覆盖；
// router_* 仅在 excluded 非空时覆盖（空不清除）。
func UpsertMessages(ctx context.Context, q dbtx, messages []model.Message) (int, error) {
	count := 0
	for _, m := range messages {
		_, err := q.ExecContext(ctx, upsertMessageSQL,
			m.ID, m.SessionID, m.Client, m.Date, m.TS, m.Model, m.Provider,
			m.RouterProvider, m.RouterModel, m.RouterName, m.Directory, m.Project,
			m.InputTokens, m.FreshInputTokens, m.OutputTokens, m.CacheReadTokens,
			m.CacheCreateTokens, m.ReasoningTokens, m.TotalTokens,
		)
		if err != nil {
			return count, fmt.Errorf("upsert message %q/%q 失败: %w", m.Client, m.ID, err)
		}
		count++
	}
	return count, nil
}

const upsertSessionMetaSQL = `INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(id,client) DO UPDATE SET
 directory=excluded.directory,
 project=excluded.project,
 title=excluded.title,
 parent_id=excluded.parent_id,
 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END`

// UpsertSessionMeta 写入会话最终元数据（directory/project/title/parent_id/first_ts/last_ts），
// 不写 token 列（token 统计由 messages 账本聚合）。
func UpsertSessionMeta(ctx context.Context, q dbtx, sessions []model.Session) (int, error) {
	count := 0
	for _, s := range sessions {
		_, err := q.ExecContext(ctx, upsertSessionMetaSQL,
			s.ID, s.Client, s.Directory, s.Project, s.Title, s.ParentID, s.FirstTS, s.LastTS,
		)
		if err != nil {
			return count, fmt.Errorf("upsert session meta %q/%q 失败: %w", s.Client, s.ID, err)
		}
		count++
	}
	return count, nil
}

// SetSyncCursors 批量写入同步游标。调用方负责事务原子性（传入 dbtx 可为 *DB 或 *sql.Tx）。
func SetSyncCursors(ctx context.Context, q dbtx, client string, cursors map[string]model.SyncCursor) error {
	for source, c := range cursors {
		_, err := q.ExecContext(ctx, `INSERT INTO sync_state(client,source,cursor_value,cursor_id,updated_at)
VALUES(?,?,?,?,datetime('now'))
ON CONFLICT(client,source) DO UPDATE SET
 cursor_value=excluded.cursor_value,
 cursor_id=excluded.cursor_id,
 updated_at=datetime('now');`, client, source, c.Value, c.ID)
		if err != nil {
			return fmt.Errorf("写入 sync_state %q/%q 失败: %w", client, source, err)
		}
	}
	return nil
}

// GetSyncCursors 查询指定 client 下多个 source 的游标，缺失 source 返回零值 cursor。
func GetSyncCursors(ctx context.Context, q dbtx, client string, sources []string) (map[string]model.SyncCursor, error) {
	out := make(map[string]model.SyncCursor, len(sources))
	for _, source := range sources {
		var c model.SyncCursor
		row := q.QueryRowContext(ctx, `SELECT cursor_value, cursor_id FROM sync_state WHERE client=? AND source=?`, client, source)
		err := row.Scan(&c.Value, &c.ID)
		if err == sql.ErrNoRows {
			out[source] = model.SyncCursor{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("查询 sync_state %q/%q 失败: %w", client, source, err)
		}
		out[source] = c
	}
	return out, nil
}

const routerLogChunkSize = 500

// routerAppTypeToClient 将 raw_router_logs.app_type 映射到 model.Client 常量。
// 仅 claude / claude-desktop 参与 message 级归因；其余 app_type 一律忽略。
func routerAppTypeToClient(appType string) (string, bool) {
	switch appType {
	case "claude":
		return model.ClientClaudeCode, true
	case "claude-desktop":
		return model.ClientClaudeDesktop, true
	default:
		return "", false
	}
}

// QueryRouterLogsByMessageIDs 按 router_name + message_id 集合查询路由归因。
// 每 500 个 ID 分块；按 (created_at, request_id) 排序；每个 (client, message_id) 首条优先。
// 测试插入另一个 router_name 的同 message_id，断言不会串线。
func QueryRouterLogsByMessageIDs(ctx context.Context, q dbtx, routerName string, messageIDs []string) ([]model.RouterAttribution, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	type firstKey struct {
		client    string
		messageID string
	}
	first := make(map[firstKey]model.RouterAttribution)

	for start := 0; start < len(messageIDs); start += routerLogChunkSize {
		end := start + routerLogChunkSize
		if end > len(messageIDs) {
			end = len(messageIDs)
		}
		chunk := messageIDs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, 1+len(chunk))
		args = append(args, routerName)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := `SELECT message_id, app_type, provider_name, model, router_name, created_at, request_id
FROM raw_router_logs
WHERE router_name=?
  AND message_id IN (` + strings.Join(placeholders, ", ") + `)
  AND app_type IN ('claude','claude-desktop')
ORDER BY created_at, request_id;`
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("查询 router logs 失败: %w", err)
		}
		for rows.Next() {
			var messageID, appType, providerName, mdl, rName, requestID string
			var createdAt int64
			if err := rows.Scan(&messageID, &appType, &providerName, &mdl, &rName, &createdAt, &requestID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("扫描 router log 失败: %w", err)
			}
			client, ok := routerAppTypeToClient(appType)
			if !ok {
				continue
			}
			key := firstKey{client: client, messageID: messageID}
			if _, exists := first[key]; !exists {
				first[key] = model.RouterAttribution{
					Client:     client,
					MessageID:  messageID,
					Provider:   providerName,
					Model:      mdl,
					RouterName: rName,
					CreatedAt:  createdAt,
					RequestID:  requestID,
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("遍历 router log 失败: %w", err)
		}
		rows.Close()
	}

	result := make([]model.RouterAttribution, 0, len(first))
	for _, v := range first {
		result = append(result, v)
	}
	return result, nil
}

// GetMessageIDsByDisplayNames 按显示名列表查询 messages.id。
//
// 注意（C2 修复）：传入的是显示名（如 "Claude Code"），非配置 key（如 "claude"）。
// messages.client 字段存的是显示名（collector 写入时经 model.RawClientToClient 映射）。
// 配置 key → 显示名列表的映射见 model.ClientToDisplayNames。
//
// 利用 messages 表主键 (client, id) 的前缀匹配（schema.go:102）。
// 空列表返回空切片（不生成非法的 IN () SQL）。
func GetMessageIDsByDisplayNames(ctx context.Context, q dbtx, displayNames []string) ([]string, error) {
	if len(displayNames) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(displayNames))
	args := make([]interface{}, 0, len(displayNames))
	for i, name := range displayNames {
		placeholders[i] = "?"
		args = append(args, name)
	}
	query := `SELECT id FROM messages WHERE client IN (` + strings.Join(placeholders, ", ") + `)`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 messages id 失败: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描 message id 失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 message id 失败: %w", err)
	}
	return ids, nil
}

const backfillRouterSQL = `UPDATE messages SET
 router_provider=CASE WHEN ?!='' THEN ? ELSE router_provider END,
 router_model=CASE WHEN ?!='' THEN ? ELSE router_model END,
 router_name=CASE WHEN ?!='' THEN ? ELSE router_name END
WHERE client=? AND id=?;`

// BackfillRouterFields 将路由归因（provider/model/router_name）回填到 messages 表。
// 空字符串不覆盖已有值。返回更新行数。
func BackfillRouterFields(ctx context.Context, q dbtx, infos []model.RouterAttribution) (int, error) {
	count := 0
	for _, info := range infos {
		res, err := q.ExecContext(ctx, backfillRouterSQL,
			info.Provider, info.Provider,
			info.Model, info.Model,
			info.RouterName, info.RouterName,
			info.Client, info.MessageID,
		)
		if err != nil {
			return count, fmt.Errorf("回填 router 字段 %q/%q 失败: %w", info.Client, info.MessageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return count, fmt.Errorf("读取回填结果 %q/%q 失败: %w", info.Client, info.MessageID, err)
		}
		count += int(n)
	}
	return count, nil
}
