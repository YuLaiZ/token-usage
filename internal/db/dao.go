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

// ResolveErrorsByDateSource 结案指定日期与来源的普通采集失败。
// 只作用于 error_type='error'：监控中断事件（monitor）由独立恢复确认路径
// 按实例结案，不得被按日成功冒充已恢复。
func ResolveErrorsByDateSource(ctx context.Context, q dbtx, date, source string) (int64, error) {
	result, err := q.ExecContext(ctx, `UPDATE collection_errors
		SET resolved = 1, updated_at = datetime('now')
		WHERE date = ? AND source = ? AND error_type = 'error' AND resolved = 0`, date, source)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ResolveErrorsByMessagePrefixes 解决 source 下 message 以任一前缀开头的未解决
// 普通采集错误，不受登记日期限制；只作用于 error_type='error'。返回解决行数。
// 空前缀会被丢弃（拼出来是只含 % 的模式，会越界结案该来源全部错误）；全部
// 为空时不执行 SQL。前缀直接拼成 LIKE 前缀模式（后接 % 锚定在 message 开头），
// 不得含通配字符 % 与 _，本函数不做转义；只结案 error_type='error' 是为了
// 不误结案监控中断事件（monitor）：监控中断由独立恢复确认路径按实例结案，
// 即使摘要命中同一前缀也不得被按消息前缀误结案。
func ResolveErrorsByMessagePrefixes(ctx context.Context, q dbtx, source string, prefixes []string) (int64, error) {
	var builder strings.Builder
	args := make([]interface{}, 0, len(prefixes)+1)
	args = append(args, source)
	builder.WriteString(`UPDATE collection_errors
		SET resolved = 1, updated_at = datetime('now')
		WHERE source = ? AND error_type = 'error' AND resolved = 0
		  AND (`)
	for _, prefix := range prefixes {
		// 空前缀会拼成只含 % 的模式、命中该来源全部未解决错误，必须丢弃。
		if prefix == "" {
			continue
		}
		if len(args) > 1 {
			builder.WriteString(" OR ")
		}
		builder.WriteString("message LIKE ?")
		// 前缀锚定在 message 开头（后接 %，无前置 %）。
		args = append(args, prefix+"%")
	}
	if len(args) == 1 {
		return 0, nil
	}
	builder.WriteString(")")
	result, err := q.ExecContext(ctx, builder.String(), args...)
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

// upsertMaxSQLParams 是单条 SQL 语句的参数（变量）数上限：SQLite 编译期
// SQLITE_MAX_VARIABLE_NUMBER 默认 999。多值 INSERT 分块按 行数×列数 ≤ 该值。
const upsertMaxSQLParams = 999

// 各写路径多值 INSERT 的每块行数推导（行×列 ≤ upsertMaxSQLParams），
// 提为具名常量供分块合同测试逐一锁定。
const (
	upsertMessagesChunkRows     = upsertMaxSQLParams / 21 // messages 21 列（含 agent_parent_id）
	upsertSessionsGenericRows   = upsertMaxSQLParams / 9  // sessions 通用 9 列（含 agent_parent_id）
	upsertSessionsCodexRows     = upsertMaxSQLParams / 11 // sessions codex 11 列（含 agent_parent_id）
	upsertRawRouterLogsChunkRow = upsertMaxSQLParams / 15 // raw_router_logs 15 列
)

// backfill 批量 UPDATE 的参数推导：backfillBatchSQLPrefix 含 SET 内 6 个
// CASE 占位符 + WHERE client 1 个（共 7），加 IN 内 id 占位符；id 数上限由
// 总上限扣除 7 得出（测试按前缀文本实数占位符锁定该口径）。
const backfillPrefixParams = 7

const maxBackfillIDsPerStmt = upsertMaxSQLParams - backfillPrefixParams

const backfillBatchSQLPrefix = `UPDATE messages SET
 router_provider=CASE WHEN ?!='' THEN ? ELSE router_provider END,
 router_model=CASE WHEN ?!='' THEN ? ELSE router_model END,
 router_name=CASE WHEN ?!='' THEN ? ELSE router_name END
WHERE client=? AND id IN (`

// multiValuesPlaceholders 生成 n 组每组 cols 个 ? 的多值 VALUES 片段。
func multiValuesPlaceholders(n, cols int) string {
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(group+",", n), ",")
}

// UpsertRawRouterLogs 批量写入路由中间件日志（staging 层）
// message_id = request_id 前缀提取（claude "session:" / codex "session:codex:{pid}:"）
// 主键 (request_id, router_name)，INSERT OR REPLACE 幂等（REPLACE 重置 collected_at
// 为当前时刻属既有副作用；data_source 语义由采集侧保证与 v3 迁移口径一致，
// REPLACE 不得把既有 'codex_session' 标记降级为 'proxy'）
func UpsertRawRouterLogs(ctx context.Context, q dbtx, logs []model.RouterLog) (int, error) {
	const cols = 15
	const prefix = `INSERT OR REPLACE INTO raw_router_logs (
		request_id, message_id, router_name, session_id, app_type, model,
		provider_id, provider_name, input_tokens, output_tokens,
		cache_read_tokens, cache_create_tokens, created_at, data_source, raw_data
	) VALUES `
	const chunkRows = upsertRawRouterLogsChunkRow

	count := 0
	for start := 0; start < len(logs); start += chunkRows {
		end := start + chunkRows
		if end > len(logs) {
			end = len(logs)
		}
		chunk := logs[start:end]
		args := make([]interface{}, 0, len(chunk)*cols)
		for _, l := range chunk {
			args = append(args,
				l.RequestID, l.MessageID, l.RouterName, l.SessionID, l.AppType, l.Model,
				l.ProviderID, l.ProviderName, l.InputTokens, l.OutputTokens,
				l.CacheReadTokens, l.CacheCreateTokens, l.CreatedAt, l.DataSource, l.RawData,
			)
		}
		stmt := prefix + multiValuesPlaceholders(len(chunk), cols)
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return count, fmt.Errorf("插入 raw_router_log 批次 %d..%d 失败（首行 %q）: %w",
				start, end-1, chunk[0].RequestID, err)
		}
		count += len(chunk)
	}
	return count, nil
}

const upsertMessageCols = `id, session_id, client, date, ts, model, provider,
	router_provider, router_model, router_name, directory, project,
	input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens,
	cache_create_tokens, reasoning_tokens, total_tokens, duration_ms,
	agent_parent_id`

// upsertMessageConflictClause 是消息 UPSERT 的冲突子句（单行与多值分块两种
// 形态共用，逐列语义见 UpsertMessages 注释）。
const upsertMessageConflictClause = `
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
	total_tokens = excluded.total_tokens,
	duration_ms = excluded.duration_ms,
	agent_parent_id = CASE WHEN excluded.agent_parent_id != '' THEN excluded.agent_parent_id ELSE messages.agent_parent_id END`

// upsertMessageSQL 是单行形态（测试参照实现使用）；生产写路径走多值分块
// 形态（UpsertMessages），冲突键与 SET 子句与之一致。
const upsertMessageSQL = `INSERT INTO messages (` + upsertMessageCols + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)` + upsertMessageConflictClause

// UpsertMessages 批量 UPSERT 消息行（多值 INSERT 分块，行×列 ≤ 999 参数）。
// 任何一批失败立即返回，让调用方事务回滚；同批/跨批重复键按出现顺序后者
// 覆盖（多值语句内冲突按行序处理，与逐行执行语义一致）。
// 归因（session_id/date/directory/project）取较早 ts 的版本；token/model/provider 总以新值覆盖；
// router_* 仅在 excluded 非空时覆盖（空不清除）。
func UpsertMessages(ctx context.Context, q dbtx, messages []model.Message) (int, error) {
	const cols = 21
	const chunkRows = upsertMessagesChunkRows
	const prefix = `INSERT INTO messages (` + upsertMessageCols + `) VALUES `

	count := 0
	for start := 0; start < len(messages); start += chunkRows {
		end := start + chunkRows
		if end > len(messages) {
			end = len(messages)
		}
		chunk := messages[start:end]
		args := make([]interface{}, 0, len(chunk)*cols)
		for _, m := range chunk {
			args = append(args,
				m.ID, m.SessionID, m.Client, m.Date, m.TS, m.Model, m.Provider,
				m.RouterProvider, m.RouterModel, m.RouterName, m.Directory, m.Project,
				m.InputTokens, m.FreshInputTokens, m.OutputTokens, m.CacheReadTokens,
				m.CacheCreateTokens, m.ReasoningTokens, m.TotalTokens, m.DurationMS,
				m.AgentParentID,
			)
		}
		stmt := prefix + multiValuesPlaceholders(len(chunk), cols) + upsertMessageConflictClause
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return count, fmt.Errorf("upsert message 批次 %d..%d 失败（首行 %q/%q）: %w",
				start, end-1, chunk[0].Client, chunk[0].ID, err)
		}
		count += len(chunk)
	}
	return count, nil
}

const upsertSessionMetaSQL = `INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts,agent_parent_id)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(id,client) DO UPDATE SET
 directory=excluded.directory,
 project=excluded.project,
 title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE sessions.title END,
 parent_id=excluded.parent_id,
 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END,
 agent_parent_id=CASE WHEN excluded.agent_parent_id<>'' THEN excluded.agent_parent_id ELSE sessions.agent_parent_id END`

// upsertCodexSessionMetaSQL 是 Codex App / Codex CLI 行的会话 upsert：在通用语义
// 之上叠加 title/title_source/title_index_ts 的来源优先级合并（index > native >
// fallback，空标题保留库值、空来源（历史来源未知）等价 native 的可覆盖地位）。
// CASE 分支顺序即优先级判定：excluded 空标题 → 保留；excluded index → 仅当其
// title_index_ts 严格大于存量已应用时间才覆盖（索引是最新的用户改名意图；
// ts 不大于时保持存量——索引截断/重建回旧快照不得把已同步的新标题倒退回旧
// 标题；存量非 index 行的 title_index_ts 恒 0，任何正 ts 都能首写覆盖）；
// 存量 index → 保留（防止后续 state/rollout 重采把 App 标题打回 prompt 标题）；
// excluded fallback → 不覆盖任何非空非兜底存量标题。title_index_ts 与 title 同
// 取值侧（非 index 写入的 excluded ts 恒 0，且存量 index 行从不会被非 index 值
// 覆盖，归 0 无实际变化）。
const upsertCodexSessionMetaSQL = `INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts,title_source,title_index_ts,agent_parent_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id,client) DO UPDATE SET
 directory=excluded.directory,
 project=excluded.project,
 title=CASE
  WHEN excluded.title='' THEN sessions.title
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN excluded.title
  WHEN sessions.title_source='index' THEN sessions.title
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title
  ELSE excluded.title END,
 title_source=CASE
  WHEN excluded.title='' THEN sessions.title_source
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN 'index'
  WHEN sessions.title_source='index' THEN sessions.title_source
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title_source
  ELSE excluded.title_source END,
 title_index_ts=CASE
  WHEN excluded.title='' THEN sessions.title_index_ts
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN excluded.title_index_ts
  WHEN sessions.title_source='index' THEN sessions.title_index_ts
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title_index_ts
  ELSE 0 END,
 parent_id=excluded.parent_id,
 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END,
 agent_parent_id=CASE WHEN excluded.agent_parent_id<>'' THEN excluded.agent_parent_id ELSE sessions.agent_parent_id END`

// upsertSessionAgentParentChunkRows 是会话关系专用写入的分块行数（3 列：
// id/client/agent_parent_id；行×列 ≤ 999）。
const upsertSessionAgentParentChunkRows = upsertMaxSQLParams / 3

// UpsertSessionAgentParents 只写会话的子代理父关系（agent_parent_id），供
// 「无消息中间层代理」补父链：INSERT 仅带主键与父关系列（其余列取表默认值），
// 冲突时只更新 agent_parent_id——不触碰 directory/project/parent_id/title 等
// 完整元数据，避免按日期采集或空数据覆盖历史会话。
func UpsertSessionAgentParents(ctx context.Context, q dbtx, sessions []model.Session) (int, error) {
	const cols = 3
	count := 0
	for start := 0; start < len(sessions); start += upsertSessionAgentParentChunkRows {
		end := start + upsertSessionAgentParentChunkRows
		if end > len(sessions) {
			end = len(sessions)
		}
		chunk := sessions[start:end]
		args := make([]interface{}, 0, len(chunk)*cols)
		for _, s := range chunk {
			args = append(args, s.ID, s.Client, s.AgentParentID)
		}
		stmt := `INSERT INTO sessions (id,client,agent_parent_id) VALUES ` +
			multiValuesPlaceholders(len(chunk), cols) +
			` ON CONFLICT(id,client) DO UPDATE SET
 agent_parent_id=CASE WHEN excluded.agent_parent_id<>'' THEN excluded.agent_parent_id ELSE sessions.agent_parent_id END`
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return count, fmt.Errorf("upsert session agent parent 批次 %d..%d 失败（首行 %q/%q）: %w",
				start, end-1, chunk[0].Client, chunk[0].ID, err)
		}
		count += len(chunk)
	}
	return count, nil
}

// QuerySessionAgentParents 查询给定 codex 会话 id 的现有父关系，返回
// "client\x00id" → agent_parent_id 映射（不存在或空关系不出现）。供归因
// 触发源判定「本轮关系是否新增/变化」：未变化的关系只做幂等写入，不触发
// 家族重算（避免单日采集把全部历史子代理家族重算）。
func QuerySessionAgentParents(ctx context.Context, q dbtx, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	for start := 0; start < len(ids); start += routerLogChunkSize {
		end := start + routerLogChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, 0, len(chunk))
		args := make([]interface{}, 0, len(chunk))
		for _, id := range chunk {
			if id == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		if len(placeholders) == 0 {
			continue
		}
		query := `SELECT id, client, agent_parent_id FROM sessions
			WHERE client IN ('Codex App','Codex CLI')
			  AND id IN (` + strings.Join(placeholders, ",") + `)`
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("查询会话父关系失败: %w", err)
		}
		for rows.Next() {
			var id, client, parent string
			if err := rows.Scan(&id, &client, &parent); err != nil {
				rows.Close()
				return nil, fmt.Errorf("扫描会话父关系行失败: %w", err)
			}
			if id == "" || parent == "" {
				continue
			}
			out[client+"\x00"+id] = parent
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// isCodexDisplayClient 按 sessions.client 显示名判定 Codex 行（Codex 采集的
// 落库 client 是这两个显示名，不是配置 key "codex"，也不是字面值 "Codex"）。
func isCodexDisplayClient(client string) bool {
	return client == model.ClientCodexApp || client == model.ClientCodexCLI
}

// UpsertSessionMeta 写入会话最终元数据（directory/project/title/parent_id/first_ts/last_ts），
// 不写 token 列（token 统计由 messages 账本聚合）。
// title 为空时保留库中已有值：部分采集路径（如 Codex ChangedFile/rollout 全扫）在
// 解析侧拿不到标题，空值覆盖会把其他路径补齐的标题冲掉。
// Codex App / Codex CLI 行额外写 title_source 并按来源优先级合并（见
// upsertCodexSessionMetaSQL）；其余 client 走通用语句、不触碰 title_source 列
// （mimocode 兼容 trigger 的 INSERT 列清单冻结不含该列，语义不受影响）。
func UpsertSessionMeta(ctx context.Context, q dbtx, sessions []model.Session) (int, error) {
	// 按语句类型分两组（普通 9 列 / codex 11 列），组内保序分块批量执行；
	// 不同 (id,client) 主键互不干扰，组间顺序无关，语义与逐行一致。
	const genericCols, codexCols = 9, 11
	genericChunkRows := upsertSessionsGenericRows
	codexChunkRows := upsertSessionsCodexRows

	var generic, codex []model.Session
	for _, s := range sessions {
		if isCodexDisplayClient(s.Client) {
			codex = append(codex, s)
		} else {
			generic = append(generic, s)
		}
	}
	count := 0
	for _, group := range []struct {
		rows      []model.Session
		cols      int
		chunkRows int
	}{
		{generic, genericCols, genericChunkRows},
		{codex, codexCols, codexChunkRows},
	} {
		for start := 0; start < len(group.rows); start += group.chunkRows {
			end := start + group.chunkRows
			if end > len(group.rows) {
				end = len(group.rows)
			}
			chunk := group.rows[start:end]
			args := make([]interface{}, 0, len(chunk)*group.cols)
			for _, s := range chunk {
				args = append(args, s.ID, s.Client, s.Directory, s.Project, s.Title, s.ParentID, s.FirstTS, s.LastTS)
				if group.cols == codexCols {
					args = append(args, s.TitleSource, s.TitleIndexTS)
				}
				args = append(args, s.AgentParentID)
			}
			stmt := sessionMultiValuesPrefix(group.cols == codexCols) +
				multiValuesPlaceholders(len(chunk), group.cols) + sessionConflictClause(group.cols == codexCols)
			if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
				return count, fmt.Errorf("upsert session meta 批次 %d..%d 失败（首行 %q/%q）: %w",
					start, end-1, chunk[0].Client, chunk[0].ID, err)
			}
			count += len(chunk)
		}
	}
	return count, nil
}

// sessionMultiValuesPrefix 生成两种会话语句的多值 INSERT 前缀（含列清单）。
func sessionMultiValuesPrefix(codex bool) string {
	base := `INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts`
	if codex {
		base += `,title_source,title_index_ts`
	}
	base += `,agent_parent_id`
	return base + `) VALUES `
}

// sessionConflictClause 生成两种会话语句的 ON CONFLICT 子句（与单行常量一致）。
func sessionConflictClause(codex bool) string {
	if codex {
		return `
ON CONFLICT(id,client) DO UPDATE SET
 directory=excluded.directory,
 project=excluded.project,
 title=CASE
  WHEN excluded.title='' THEN sessions.title
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN excluded.title
  WHEN sessions.title_source='index' THEN sessions.title
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title
  ELSE excluded.title END,
 title_source=CASE
  WHEN excluded.title='' THEN sessions.title_source
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN 'index'
  WHEN sessions.title_source='index' THEN sessions.title_source
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title_source
  ELSE excluded.title_source END,
 title_index_ts=CASE
  WHEN excluded.title='' THEN sessions.title_index_ts
  WHEN excluded.title_source='index' AND excluded.title_index_ts>COALESCE(sessions.title_index_ts,0) THEN excluded.title_index_ts
  WHEN sessions.title_source='index' THEN sessions.title_index_ts
  WHEN excluded.title_source='fallback' AND sessions.title<>'' AND sessions.title_source<>'fallback' THEN sessions.title_index_ts
  ELSE 0 END,
 parent_id=excluded.parent_id,
 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END,
 agent_parent_id=CASE WHEN excluded.agent_parent_id<>'' THEN excluded.agent_parent_id ELSE sessions.agent_parent_id END`
	}
	return `
ON CONFLICT(id,client) DO UPDATE SET
 directory=excluded.directory,
 project=excluded.project,
 title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE sessions.title END,
 parent_id=excluded.parent_id,
 first_ts=CASE WHEN sessions.first_ts=0 OR (excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts) THEN excluded.first_ts ELSE sessions.first_ts END,
 last_ts=CASE WHEN excluded.last_ts>sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END,
 agent_parent_id=CASE WHEN excluded.agent_parent_id<>'' THEN excluded.agent_parent_id ELSE sessions.agent_parent_id END`
}

// ApplyCodexIndexTitles 把标题索引命中的记录以参数化 SQL 同步进 sessions 的
// 既有 Codex 行（title/title_source='index'/title_index_ts=记录的 updated_at）。
//
// 写入门是单一的时间门：记录时间严格晚于该行已应用的索引时间即写入——包括
// 标题与来源都没变、只有时间更新的记录（A@100 → A@300 必须推进 title_index_ts，
// 否则随后的旧快照 B@200 能凭 200>100 通过时间门把标题从 A 倒退为 B）。时间
// <= 已应用时间的记录（含截断/重建回旧快照、同时间戳重放）一律不写，天然幂等。
//
// 同时间戳跨轮的显式决策：解析器在单轮读取内「同时间戳后行胜出」，但前行已
// 落库后、跨轮到达的同时间戳不同标题记录**不覆盖**（严格 >）——仅凭时间戳无法
// 区分「文件内后追加」与「重建快照里的旧行」，而后者覆盖会破坏防倒退；updated_at
// 为纳秒精度，真实同时间戳二次改名几乎不存在，且该缝隙中下一次任何时间更新的
// 改名仍会正常覆盖。持久层不支持跨轮的后行胜出，由测试锁定该行为。
//
// UPDATE-only：不插入新 session、不重放 messages、不改变 token 与时间列；索引
// 未命中（无对应行）与非 Codex 行不受影响。返回更新行数（含纯时间推进行）。
// 调用方负责事务边界（dbtx 可为 *DB 或 *sql.Tx）。
func ApplyCodexIndexTitles(ctx context.Context, q dbtx, titles map[string]model.CodexTitleIndexRecord) (int64, error) {
	const stmt = `UPDATE sessions SET title=?, title_source='index', title_index_ts=?
WHERE id=? AND client IN (?,?)
  AND COALESCE(title_index_ts,0)<?`
	var updated int64
	for id, rec := range titles {
		if id == "" || rec.Name == "" {
			continue
		}
		res, err := q.ExecContext(ctx, stmt,
			rec.Name, rec.UpdatedAtUnixNano, id, model.ClientCodexApp, model.ClientCodexCLI, rec.UpdatedAtUnixNano)
		if err != nil {
			return updated, fmt.Errorf("同步 Codex 标题索引 %q 失败: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return updated, fmt.Errorf("读取 Codex 标题同步结果 %q 失败: %w", id, err)
		}
		updated += n
	}
	return updated, nil
}

// ApplyCodexFallbackTitles 把子线程兜底标题写进 sessions 中仍是空标题的既有
// Codex 行（title_source 置 'fallback'）。WHERE 条件保证只在空标题行写入：
// 不覆盖任何非空标题（含非兜底与兜底）、不插入新 session、不动 messages/token/
// 时间。该步骤服务存量空标题子线程（增量游标不会重触达旧线程、rollout 全扫可
// 能被跳过门略过，兜底必须由独立同步闭合）；后续更好的来源（索引命中、
// state/rollout 标题）经 upsert 优先级自然替换。返回更新行数。
func ApplyCodexFallbackTitles(ctx context.Context, q dbtx, fallbacks map[string]string) (int64, error) {
	const stmt = `UPDATE sessions SET title=?, title_source='fallback'
WHERE id=? AND client IN (?,?) AND COALESCE(title,'')=''`
	var updated int64
	for id, title := range fallbacks {
		if id == "" || title == "" {
			continue
		}
		res, err := q.ExecContext(ctx, stmt,
			title, id, model.ClientCodexApp, model.ClientCodexCLI)
		if err != nil {
			return updated, fmt.Errorf("回填 Codex 子线程兜底 %q 失败: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return updated, fmt.Errorf("读取 Codex 兜底回填结果 %q 失败: %w", id, err)
		}
		updated += n
	}
	return updated, nil
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

// routerLogChunkSize 是 IN 查询按集合分块的单块上限：messageID 集合与 session
// 集合均按此分块，控制单条语句的占位符规模。
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
// 注意（C2 修复）：传入的是显示名（如 "Claude Code"、"MiMo Code"），非配置 key
// （如 "claude"、"mimocode"）。messages.client 字段存的是显示名（collector 写入
// 时经 model.RawClientToClient 映射）。配置 key → 显示名列表的映射见
// model.ClientToDisplayNames。历史 legacy 长名 "Xiaomi MiMo / MiMo Code" 已由
// migrateV4 改名并经兼容 trigger 阻止回写，正常查询不会遇到。
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

// backfillGroupKey 是归因回填的按值分组键（同组一条批量 UPDATE）。
type backfillGroupKey struct {
	client, provider, model, routerName string
}

// claudeSiblingClient 返回 Claude 系 client 的另一侧：Claude Code 与
// Claude Desktop 是同一份本地 transcript 的两种判定结果，改判会把消息行在
// 两者之间挪动。非 Claude 系返回空串（不参与放宽匹配）。
func claudeSiblingClient(client string) string {
	switch client {
	case model.ClientClaudeCode:
		return model.ClientClaudeDesktop
	case model.ClientClaudeDesktop:
		return model.ClientClaudeCode
	default:
		return ""
	}
}

// claudeCrossClientFallbackSQL 是 Claude 系的归因跨 client 兜底：当日志 app_type
// 对应的 client 已没有该 id 的行（改判后旧行被收敛删除），把归因写到同 id
// 唯一保留的另一侧 Claude 行上；该 client 仍有行时不动，保持精确匹配语义。
//
// 必要性：router 日志的 app_type（claude / claude-desktop）直接决定回填的
// client，而消息的 client 来自 transcript 的 entrypoint 判定。一条消息被改判
// 后（如 Claude Code → Claude Desktop，旧行随即被收敛删除），改判之前产生与
// 之后晚到的日志仍带旧 app_type，精确匹配落空，实际供应商与模型归因会永久
// 缺失——收敛时迁移归因只覆盖「改判前已写入旧行」的部分，晚到的日志没有这个
// 通道。只填空列，不覆盖既有归因，也不新建行。
const claudeCrossClientFallbackSQL = `UPDATE messages SET
	router_provider = CASE WHEN COALESCE(messages.router_provider,'') = '' THEN ? ELSE messages.router_provider END,
	router_model    = CASE WHEN COALESCE(messages.router_model,'')    = '' THEN ? ELSE messages.router_model END,
	router_name     = CASE WHEN COALESCE(messages.router_name,'')     = '' THEN ? ELSE messages.router_name END
WHERE messages.client = ?
  AND NOT EXISTS (SELECT 1 FROM messages AS precise WHERE precise.id = messages.id AND precise.client = ?)
  AND messages.id IN (`

// BackfillRouterFields 将路由归因（provider/model/router_name）回填到 messages 表。
// 空字符串不覆盖已有值。返回更新行数。
//
// 批量形态：按 (client,provider,model,router_name) 四元组分组，每组一条
// `UPDATE ... WHERE client=? AND id IN (...)`（组参数 7+ids 个：SET 内
// CASE 占位符 6 + WHERE client 1；ids ≤ 992）。
// 唯一键守卫：逐项执行对重复输入键按顺序重复生效（A→B→A 终值 A、计数 3），
// 按值分组批量执行不等价（IN 不重复计数且改变交错序）——检测到输入中
// (client,message_id) 存在重复时整批回退逐项执行，不做去重预处理（去重会
// 改变终值与计数语义，归属调用方数据问题）。
// Claude 系额外做跨 client 兜底（见 claudeCrossClientFallbackSQL）：精确匹配
// 因改判落空的 id，按 id 到另一侧 Claude 行补归因，只填空列。重复日志按输入顺序补齐，首个非空值保留；
// 精确匹配仍按后续非空值覆盖。
// 计数口径：累加每条批量语句的 RowsAffected（唯一键前提下与逐行累加严格
// 一致），该口径是「UPDATE 命中行数」而非「字段实际变化次数」（现状即如此）。
func BackfillRouterFields(ctx context.Context, q dbtx, infos []model.RouterAttribution) (int, error) {
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		key := info.Client + "\x00" + info.MessageID
		if seen[key] {
			return backfillRouterFieldsRowByRow(ctx, q, infos)
		}
		seen[key] = true
	}

	// 分组保持首现顺序（map 迭代无序，order 记录组序）。
	var order []backfillGroupKey
	groups := make(map[backfillGroupKey][]string)
	for _, info := range infos {
		k := backfillGroupKey{info.Client, info.Provider, info.Model, info.RouterName}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], info.MessageID)
	}

	const maxIDsPerStmt = maxBackfillIDsPerStmt
	count := 0
	for _, k := range order {
		ids := groups[k]
		for start := 0; start < len(ids); start += maxIDsPerStmt {
			end := start + maxIDsPerStmt
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			stmt := backfillBatchSQLPrefix + placeholders + `)`
			args := make([]interface{}, 0, 7+len(chunk))
			args = append(args, k.provider, k.provider, k.model, k.model, k.routerName, k.routerName, k.client)
			for _, id := range chunk {
				args = append(args, id)
			}
			res, err := q.ExecContext(ctx, stmt, args...)
			if err != nil {
				return count, fmt.Errorf("回填 router 字段批次失败（client %q，首条 %q）: %w", k.client, chunk[0], err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return count, fmt.Errorf("读取回填结果批次失败（client %q，首条 %q）: %w", k.client, chunk[0], err)
			}
			count += int(n)
		}
		// Claude 系跨 client 兜底：日志的 app_type 对应 client 已无行时，回填到
		// 同 id 唯一保留的另一侧（改判后的晚到日志）。
		if sibling := claudeSiblingClient(k.client); sibling != "" {
			n, err := backfillRouterFieldsCrossClient(ctx, q, k, sibling, ids)
			if err != nil {
				return count, err
			}
			count += n
		}
	}
	return count, nil
}

// backfillRouterFieldsCrossClient 对精确匹配落空的 Claude 系 id 做跨 client
// 兜底（见 claudeCrossClientFallbackSQL）：日志对应的 client 已无该 id 的行时，
// 把归因写到同 id 唯一保留的另一侧。不新建行、不清空既有归因。
func backfillRouterFieldsCrossClient(ctx context.Context, q dbtx, k backfillGroupKey, sibling string, ids []string) (int, error) {
	const fixedParams = 5
	const chunkSize = upsertMaxSQLParams - fixedParams
	count := 0
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, fixedParams+len(chunk))
		args = append(args, k.provider, k.model, k.routerName, sibling, k.client)
		for _, id := range chunk {
			args = append(args, id)
		}
		res, err := q.ExecContext(ctx, claudeCrossClientFallbackSQL+placeholders+")", args...)
		if err != nil {
			return count, fmt.Errorf("跨 client 归因兜底失败（%q → %q，首条 %q）: %w", k.client, sibling, chunk[0], err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return count, fmt.Errorf("读取跨 client 归因兜底结果失败（%q，首条 %q）: %w", sibling, chunk[0], err)
		}
		count += int(n)
	}
	return count, nil
}

// backfillRouterFieldsRowByRow 是逐项执行形态：重复输入键的顺序语义
// （后行覆盖前行）与单行计数口径的唯一保真实现，兼作守卫回退路径。
func backfillRouterFieldsRowByRow(ctx context.Context, q dbtx, infos []model.RouterAttribution) (int, error) {
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
		if sibling := claudeSiblingClient(info.Client); sibling != "" && n == 0 {
			k := backfillGroupKey{info.Client, info.Provider, info.Model, info.RouterName}
			updated, err := backfillRouterFieldsCrossClient(ctx, q, k, sibling, []string{info.MessageID})
			if err != nil {
				return count, err
			}
			count += updated
		}
	}
	return count, nil
}

// updateSessionMetaIfExistsSQL 是门命中场景的会话元数据 UPDATE-only 语句。
// 只更新已存在的会话行，不建行、不动 parent_id/first_ts/last_ts/token 列。
// title/directory/project 传入空字符串时保留库值；与
// ApplyCodexIndexTitles 同构，零消息文件无行可更新，幽灵行与先查后写窗口一并消除。
const updateSessionMetaIfExistsSQL = `UPDATE sessions SET
	title     = CASE WHEN ? <> '' THEN ? ELSE title END,
	directory = CASE WHEN ? <> '' THEN ? ELSE directory END,
	project   = CASE WHEN ? <> '' THEN ? ELSE project END
WHERE id = ? AND client = ?`

// updateMessageAttributionForSessionSQL 把会话名下消息的归因列对齐到会话行。
// 消息的 directory/project 与会话同口径，而跳过门命中的文件不会重读，映射的
// 晚到（新增或删除）只能在 UPDATE-only 通道里一并修好，否则会话行与会话列表
// 一致、消息仍停留在旧值，按项目汇总的用量长期分叉。
// 取值直接读会话行的最终值（子查询），而不是入参：入参的空值表示「保留库值」，
// 会话行的实际值才是消息应对齐的口径。
const updateMessageAttributionForSessionSQL = `UPDATE messages SET
	directory = COALESCE((SELECT directory FROM sessions WHERE id = ? AND client = ?), directory),
	project   = COALESCE((SELECT project FROM sessions WHERE id = ? AND client = ?), project)
WHERE client = ? AND session_id = ?`

// UpdateSessionMetaIfExists 只更新已存在会话行的 title/directory/project（UPDATE-only）。
// 与 UpsertSessionMeta 分开：不建行、不覆盖时间/token/父关系；空值保留库值。
// 同一会话下 messages.directory/project 一并按会话行对齐（映射新增与删除都
// 需要这条通道），保持两表同口径。调用方负责事务边界（dbtx 可为 *DB 或 *sql.Tx）。
// 返回更新的会话行数。
func UpdateSessionMetaIfExists(ctx context.Context, q dbtx, sessions []model.Session) (int64, error) {
	var updated int64
	for _, s := range sessions {
		if s.ID == "" {
			continue
		}
		res, err := q.ExecContext(ctx, updateSessionMetaIfExistsSQL,
			s.Title, s.Title, s.Directory, s.Directory, s.Project, s.Project, s.ID, s.Client)
		if err != nil {
			return updated, fmt.Errorf("更新会话元数据 %q/%q 失败: %w", s.Client, s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return updated, fmt.Errorf("读取会话元数据更新结果 %q/%q 失败: %w", s.Client, s.ID, err)
		}
		updated += n
		// 无论入参是否为空都要同步：入参为空时会话行保留库值，消息仍要对齐到该值
		// （映射被删除后重读会清空归属，两表必须一起清空）。
		if _, err := q.ExecContext(ctx, updateMessageAttributionForSessionSQL,
			s.ID, s.Client, s.ID, s.Client, s.Client, s.ID); err != nil {
			return updated, fmt.Errorf("同步会话 %q/%q 的消息归因失败: %w", s.Client, s.ID, err)
		}
	}
	return updated, nil
}

// migrateRouterSQL 是跨 client 收敛时的归因迁移语句：语义是「目标列为空才写」，
// 只在新行对应列为空时填入旧行非空归因，不覆盖新行已有值。
const migrateRouterSQL = `UPDATE messages SET
	router_provider = CASE WHEN COALESCE(router_provider,'') = '' THEN ? ELSE router_provider END,
	router_model    = CASE WHEN COALESCE(router_model,'')    = '' THEN ? ELSE router_model END,
	router_name     = CASE WHEN COALESCE(router_name,'')     = '' THEN ? ELSE router_name END
WHERE client = ? AND id = ?`

// ClaudeConvergeStats 是 Claude 系跨 client 收敛的执行统计（供 engine 记 Debug）。
type ClaudeConvergeStats struct {
	MigratedRouter  int
	DeletedMessages int64
	DeletedSessions int64
}

// ClaudeConvergePlan 是 Claude 跨 client 收敛的本轮产出集合：
// MessageTargets/SessionTargets 均为「id → 目标 Claude 系 client」，分别用于
// 消息（含归因迁移）与会话行的收敛。同批多 client 冲突由 engine 在构造时跳过。
type ClaudeConvergePlan struct {
	MessageTargets map[string]string
	SessionTargets map[string]string
}

const (
	claudeConvergeMessageChunkIDs = upsertMaxSQLParams - 1
	// 会话删除和消息引用检查各绑定一次 client。
	claudeConvergeSessionChunkIDs = upsertMaxSQLParams - 2
)

// ConvergeClaudeClient 把本轮产出的消息/会话从「另一 Claude 系 client」收敛到目标 client：
// 先迁移旧行非空 router_* 到新消息行（目标列为空才写），再删除旧 client 的同 id
// 消息与会话行。调用方负责事务边界；收敛在 Upsert 之后同一事务内执行。
func ConvergeClaudeClient(ctx context.Context, q dbtx, plan ClaudeConvergePlan) (ClaudeConvergeStats, error) {
	var stats ClaudeConvergeStats
	type pair struct{ target, other string }
	msgGroups := make(map[pair][]string)
	sessGroups := make(map[pair][]string)
	groupKey := func(target string) (pair, bool) {
		switch target {
		case model.ClientClaudeDesktop:
			return pair{model.ClientClaudeDesktop, model.ClientClaudeCode}, true
		case model.ClientClaudeCode:
			return pair{model.ClientClaudeCode, model.ClientClaudeDesktop}, true
		default:
			return pair{}, false
		}
	}
	for id, target := range plan.MessageTargets {
		if p, ok := groupKey(target); ok {
			msgGroups[p] = append(msgGroups[p], id)
		}
	}
	for id, target := range plan.SessionTargets {
		if p, ok := groupKey(target); ok {
			sessGroups[p] = append(sessGroups[p], id)
		}
	}
	for p, ids := range msgGroups {
		for start := 0; start < len(ids); start += claudeConvergeMessageChunkIDs {
			end := start + claudeConvergeMessageChunkIDs
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			if err := migrateClaudeRouterFields(ctx, q, chunk, p.other, p.target, &stats); err != nil {
				return stats, err
			}
			if err := deleteClaudeRows(ctx, q, chunk, p.other, true, &stats); err != nil {
				return stats, err
			}
		}
	}
	for p, ids := range sessGroups {
		for start := 0; start < len(ids); start += claudeConvergeSessionChunkIDs {
			end := start + claudeConvergeSessionChunkIDs
			if end > len(ids) {
				end = len(ids)
			}
			if err := deleteClaudeRows(ctx, q, ids[start:end], p.other, false, &stats); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

// migrateClaudeRouterFields 读取旧 client 的 id 归因并迁移到新行（目标列为空才写）。
func migrateClaudeRouterFields(ctx context.Context, q dbtx, ids []string, fromClient, toClient string, stats *ClaudeConvergeStats) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := `SELECT id, router_provider, router_model, router_name FROM messages WHERE client=? AND id IN (` + placeholders + `)`
	args := make([]interface{}, 0, 1+len(ids))
	args = append(args, fromClient)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("查询旧 client 归因失败: %w", err)
	}
	type routerVals struct{ provider, model, name string }
	vals := make(map[string]routerVals)
	for rows.Next() {
		var id string
		var v routerVals
		if err := rows.Scan(&id, &v.provider, &v.model, &v.name); err != nil {
			rows.Close()
			return fmt.Errorf("扫描旧 client 归因失败: %w", err)
		}
		vals[id] = v
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("遍历旧 client 归因失败: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		v, ok := vals[id]
		if !ok || (v.provider == "" && v.model == "" && v.name == "") {
			continue
		}
		res, err := q.ExecContext(ctx, migrateRouterSQL, v.provider, v.model, v.name, toClient, id)
		if err != nil {
			return fmt.Errorf("迁移归因 %q/%q 失败: %w", toClient, id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取归因迁移结果 %q/%q 失败: %w", toClient, id, err)
		}
		stats.MigratedRouter += int(n)
	}
	return nil
}

// deleteClaudeRows 删除旧 client 的同 id 消息或会话行。messages=true 删 messages，
// 否则删 sessions（两者 id 口径不同：message.id 与 session.id）。
//
// 会话行的删除额外带 NOT EXISTS 保护：只有当该 client 在 messages 中已无引用该
// session 的消息时才删会话行。否则 ChangedFile 单文件轮（Dates 为空、本轮 id
// 集合只含被触达文件）会删掉旧 client 的会话行，而该会话另一日期/另一文件的
// 消息仍留在 messages —— 成为无会话行的孤儿，在 sessions JOIN messages 的会话
// 排行中彻底消失（本轮 id 集合只是删除单位，覆盖不到这种常态场景）。
// 消息在本轮 id 集合内先行删除（msgGroups 循环先于 sessGroups），因此 UPDATE
// 触达的消息删除后，无残留引用的会话行可安全删除；仍有其它消息引用的会话行
// 保留，其消息由后续各自文件的收敛轮逐步迁走，最后一条迁走时该行自然被删。
func deleteClaudeRows(ctx context.Context, q dbtx, ids []string, client string, messages bool, stats *ClaudeConvergeStats) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, 0, 1+len(ids))
	args = append(args, client)
	for _, id := range ids {
		args = append(args, id)
	}
	query := "DELETE FROM messages WHERE client=? AND id IN (" + placeholders + ")"
	table := "messages"
	if !messages {
		table = "sessions"
		query = "DELETE FROM sessions WHERE client=? AND id IN (" + placeholders + ")" +
			" AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.client=? AND messages.session_id=sessions.id)"
		args = append(args, client)
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("删除旧 client %s 失败: %w", table, err)
	}
	if n, e := res.RowsAffected(); e == nil {
		if messages {
			stats.DeletedMessages += n
		} else {
			stats.DeletedSessions += n
		}
	}
	return nil
}
