package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// OpenCodeCollector 从 OpenCode SQLite 库采集逐消息 token 用量。
// 双源合并：message 主源（当前 completed assistant 快照）+ event 补偿源（message.updated.1 终态）。
// message 主源优先：同 ID 冲突时取 message 值；event-only（message 不存在）时取 event 终态补回。
type OpenCodeCollector struct {
	cfg      *config.Config
	dbPath   string
	cacheDir string
}

func NewOpenCodeCollector(cfg *config.Config) *OpenCodeCollector {
	dbPath := ""
	if cfg != nil {
		if clientCfg, ok := cfg.ClientConfig("opencode"); ok {
			if p, exists := clientCfg.Paths["db"]; exists {
				dbPath = p
			}
		}
	}
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "opencode")
	return &OpenCodeCollector{cfg: cfg, dbPath: dbPath, cacheDir: cacheDir}
}

func (c *OpenCodeCollector) Name() string {
	return "opencode"
}

func (c *OpenCodeCollector) SyncSources() []string {
	return []string{SyncSourceOpenCodeMessage, SyncSourceOpenCodeEvent}
}

// ===== 源行结构 =====

type openCodeTokens struct {
	Total     int64 `json:"total"`
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

type openCodeInfo struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens openCodeTokens `json:"tokens"`
}

type openCodeEventEnvelope struct {
	Info openCodeInfo `json:"info"`
}

// ocSessionData 携带 session 表元数据，供消息转换与 Session 构造使用。
type ocSessionData struct {
	parentID    string
	directory   string
	title       string
	modelJSON   string
	timeCreated int64
	timeUpdated int64
}

// openCodeQueryer 使哨兵 helper 可接收 *sql.Tx（同一只读事务）。
type openCodeQueryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// ===== Collect 主流程 =====

// Collect 双源采集：message 主源 + event 补偿，返回唯一 Messages 与双 NextCursor。
//
// CLI 模式（Incremental=false）：message 按日期 SQL 过滤，event 全量查 type='message.updated.1'
// 后在 Go 侧按 info.completed 日期过滤；不返回 NextCursors。
//
// 增量模式（Incremental=true）：message 按 (time_updated,id) 复合游标增量；
// event 先哨兵校验 cursor，再在同一只读事务中取 high-water 有界扫描 type='message.updated.1'。
// NextCursors[Event]=highWater（不是最后一条 token event），使纯非 token 尾部也能跨过。
func (c *OpenCodeCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if c == nil {
		return CollectResult{}, fmt.Errorf("OpenCode collector 不能为空")
	}
	if c.dbPath == "" {
		return CollectResult{}, fmt.Errorf("opencode DB 路径未配置")
	}
	dbInfo, statErr := os.Stat(c.dbPath)
	if os.IsNotExist(statErr) {
		return CollectResult{}, fmt.Errorf("opencode DB 文件不存在: %s", c.dbPath)
	}
	if statErr != nil {
		return CollectResult{}, fmt.Errorf("访问 opencode DB 失败: %w", statErr)
	}
	if !dbInfo.Mode().IsRegular() {
		return CollectResult{}, fmt.Errorf("opencode DB 路径不是普通文件: %s", c.dbPath)
	}

	sourceDB, err := openSQLiteReadOnly(c.dbPath)
	if err != nil {
		return CollectResult{}, fmt.Errorf("打开 opencode DB 失败: %w", err)
	}
	defer sourceDB.Close()

	providerMapping := loadProviderMapping(c.cacheDir, logger)

	sourceTx, err := sourceDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CollectResult{}, fmt.Errorf("开启 OpenCode 只读事务失败: %w", err)
	}
	defer func() { _ = sourceTx.Rollback() }()

	sessionInfos := make(map[string]ocSessionData)

	// Phase 1: message 主源
	currentMessages := make(map[string]model.Message)
	msgNext := req.Cursors[SyncSourceOpenCodeMessage] // 无新行时保持输入 cursor

	if req.Incremental {
		msgs, n, err := scanOCMessagesIncremental(ctx, sourceTx, msgNext, providerMapping, sessionInfos)
		if err != nil {
			return CollectResult{}, err
		}
		currentMessages = msgs
		msgNext = n
	} else {
		msgs, err := scanOCMessagesByDate(ctx, sourceTx, req.Dates, providerMapping, sessionInfos)
		if err != nil {
			return CollectResult{}, err
		}
		currentMessages = msgs
	}

	// Phase 2: event 源
	eventInfos, eventSessionIDs, eventNext, err := scanOCEvents(ctx, sourceTx, req)
	if err != nil {
		return CollectResult{}, err
	}

	// Phase 3: 批量回查 message 主表（同事务），把找到的当前 message 并入 currentMessages
	if len(eventInfos) > 0 {
		eventIDs := make([]string, 0, len(eventInfos))
		for id := range eventInfos {
			eventIDs = append(eventIDs, id)
		}
		lookedUp, err := batchLookupOCMessages(ctx, sourceTx, eventIDs, providerMapping, sessionInfos)
		if err != nil {
			return CollectResult{}, err
		}
		for id, msg := range lookedUp {
			currentMessages[id] = msg
		}
	}

	// Phase 4: event-only session 元数据（message 不存在时按 session ID 批量查 session 表）
	sessionIDsToLookup := make([]string, 0)
	for msgID, info := range eventInfos {
		if _, ok := currentMessages[msgID]; ok {
			continue // message 存在，sessionInfos 已由 JOIN 填充
		}
		sid := eventSessionIDs[msgID]
		if sid == "" {
			sid = info.SessionID
		}
		if sid != "" {
			if _, ok := sessionInfos[sid]; !ok {
				sessionIDsToLookup = append(sessionIDsToLookup, sid)
			}
		}
	}
	if err := batchLookupOCSessions(ctx, sourceTx, sessionIDsToLookup, sessionInfos); err != nil {
		return CollectResult{}, err
	}

	// Phase 5: event-only 消息转换（message 不存在时用 event 终态）
	eventMessages := make(map[string]model.Message, len(eventInfos))
	for msgID, info := range eventInfos {
		if _, ok := currentMessages[msgID]; ok {
			continue // message 主源优先，跳过
		}
		sid := eventSessionIDs[msgID]
		if sid == "" {
			sid = info.SessionID
		}
		if sid == "" {
			continue
		}
		info.SessionID = sid
		sess := sessionInfos[sid]
		eventMessages[msgID] = openCodeMessage(info, sess, providerMapping)
	}

	// Phase 6: 合并（message 覆盖 event）
	merged := make(map[string]model.Message, len(eventMessages)+len(currentMessages))
	for id, em := range eventMessages {
		merged[id] = em
	}
	for id, cm := range currentMessages {
		merged[id] = cm
	}

	// Phase 7: 按 (TS,ID) 排序
	messages := make([]model.Message, 0, len(merged))
	requestedDates := make(map[string]struct{}, len(req.Dates))
	for _, date := range req.Dates {
		requestedDates[date] = struct{}{}
	}
	for _, m := range merged {
		// event 回查到的当前 message 可能已更新到请求日期以外。
		// 合并后再次过滤，保证主源覆盖不会把跨日期数据带入结果。
		if len(requestedDates) > 0 {
			if _, ok := requestedDates[m.Date]; !ok {
				continue
			}
		}
		messages = append(messages, m)
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].TS != messages[j].TS {
			return messages[i].TS < messages[j].TS
		}
		return messages[i].ID < messages[j].ID
	})

	// Phase 8: Session 元数据
	sessions := buildOpenCodeSessions(messages, sessionInfos)

	result := CollectResult{
		Messages: messages,
		Sessions: sessions,
	}
	if req.Incremental {
		result.NextCursors = map[string]model.SyncCursor{
			SyncSourceOpenCodeMessage: msgNext,
			SyncSourceOpenCodeEvent:   eventNext,
		}
	}
	return result, nil
}

// ===== 转换函数 =====

func openCodeMessage(info openCodeInfo, session ocSessionData, providerMapping map[string]string) model.Message {
	modelID := info.ModelID
	providerID := info.ProviderID
	if modelID == "" || providerID == "" {
		fallbackModel, fallbackProvider := parseModelJSON(session.modelJSON)
		if modelID == "" {
			modelID = fallbackModel
		}
		if providerID == "" {
			providerID = fallbackProvider
		}
	}
	provider := providerID
	if mapped := providerMapping[providerID]; mapped != "" {
		provider = mapped
	}
	return model.Message{
		ID:                info.ID,
		SessionID:         info.SessionID,
		Client:            model.ClientOpenCode,
		Date:              time.UnixMilli(info.Time.Completed).Format("2006-01-02"),
		TS:                info.Time.Completed,
		Model:             modelID,
		Provider:          provider,
		Directory:         session.directory,
		Project:           projectNameFromDir(session.directory),
		InputTokens:       info.Tokens.Input,
		FreshInputTokens:  info.Tokens.Input,
		OutputTokens:      info.Tokens.Output,
		CacheReadTokens:   info.Tokens.Cache.Read,
		CacheCreateTokens: info.Tokens.Cache.Write,
		ReasoningTokens:   info.Tokens.Reasoning,
		TotalTokens:       info.Tokens.Total,
	}
}

// ===== message 主源查询 =====

const ocMessageBaseQuery = `SELECT m.id,m.session_id,m.time_updated,m.data,
       COALESCE(s.parent_id,''),COALESCE(s.directory,''),COALESCE(s.title,''),
       COALESCE(s.model,'{}'),COALESCE(s.time_created,0),COALESCE(s.time_updated,0)
FROM message m JOIN session s ON m.session_id=s.id`

const ocMessageFilters = ` AND json_extract(m.data,'$.role')='assistant'
  AND json_extract(m.data,'$.time.completed') IS NOT NULL
  AND json_extract(m.data,'$.tokens.total') IS NOT NULL`

// scanOCMessagesByDate CLI Dates 模式：按 completed 日期 SQL 过滤，逐行扫描 completed assistant。
func scanOCMessagesByDate(ctx context.Context, tx *sql.Tx, dates []string,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, error) {
	query := ocMessageBaseQuery + ` WHERE 1=1` + ocMessageFilters
	args := []interface{}{}
	if len(dates) > 0 {
		placeholders := strings.Repeat("?,", len(dates)-1) + "?"
		query += fmt.Sprintf(` AND date(json_extract(m.data,'$.time.completed')/1000,'unixepoch','localtime') IN (%s)`, placeholders)
		for _, d := range dates {
			args = append(args, d)
		}
	}
	query += ` ORDER BY m.time_updated,m.id`
	return scanOCMessageRows(ctx, tx, query, args, providerMapping, sessionInfos)
}

// scanOCMessagesIncremental 增量模式：按 (time_updated,id) 复合游标增量。
func scanOCMessagesIncremental(ctx context.Context, tx *sql.Tx, cursor model.SyncCursor,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, model.SyncCursor, error) {
	query := ocMessageBaseQuery + ` WHERE 1=1` + ocMessageFilters +
		` AND (m.time_updated>? OR (m.time_updated=? AND m.id>?))` +
		` ORDER BY m.time_updated,m.id`
	args := []interface{}{cursor.Value, cursor.Value, cursor.ID}
	return scanOCMessageRowsWithCursor(ctx, tx, query, args, providerMapping, sessionInfos, cursor)
}

// scanOCMessageRowsWithCursor 扫描行、转换、记录 (time_updated,id) 最大游标。
func scanOCMessageRowsWithCursor(ctx context.Context, q openCodeQueryer, query string, args []interface{},
	providerMapping map[string]string, sessionInfos map[string]ocSessionData, init model.SyncCursor,
) (map[string]model.Message, model.SyncCursor, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, model.SyncCursor{}, fmt.Errorf("查询 OpenCode message 失败: %w", err)
	}
	defer rows.Close()

	messages := make(map[string]model.Message)
	next := init

	for rows.Next() {
		var id, sessionID, data string
		var timeUpdated int64
		var sessParentID, sessDirectory, sessTitle, sessModelJSON string
		var sessTimeCreated, sessTimeUpdated int64
		if err := rows.Scan(
			&id, &sessionID, &timeUpdated, &data,
			&sessParentID, &sessDirectory, &sessTitle, &sessModelJSON,
			&sessTimeCreated, &sessTimeUpdated,
		); err != nil {
			return nil, model.SyncCursor{}, fmt.Errorf("扫描 OpenCode message 行失败: %w", err)
		}
		// SQL 过滤已经确认该行是 completed assistant 且 tokens.total 非 NULL。
		// 即使 JSON 内容随后因零 token 等原因被 Go 侧跳过，也要跨过该源行，
		// 否则增量轮询会永久重复扫描同一尾行。
		if timeUpdated > next.Value || (timeUpdated == next.Value && id > next.ID) {
			next = model.SyncCursor{Value: timeUpdated, ID: id}
		}
		var info openCodeInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			continue
		}
		if info.Role != "assistant" || info.Time.Completed <= 0 || info.Tokens.Total == 0 {
			continue
		}
		if info.ID == "" {
			info.ID = id
		}
		if info.SessionID == "" {
			info.SessionID = sessionID
		}
		if _, ok := sessionInfos[sessionID]; !ok {
			sessionInfos[sessionID] = ocSessionData{
				parentID:    sessParentID,
				directory:   sessDirectory,
				title:       sessTitle,
				modelJSON:   sessModelJSON,
				timeCreated: sessTimeCreated,
				timeUpdated: sessTimeUpdated,
			}
		}
		messages[id] = openCodeMessage(info, sessionInfos[sessionID], providerMapping)
	}
	if err := rows.Err(); err != nil {
		return nil, model.SyncCursor{}, fmt.Errorf("遍历 OpenCode message 行失败: %w", err)
	}
	return messages, next, nil
}

// scanOCMessageRows 不带游标扫描（CLI 模式与批量回查共用）。
func scanOCMessageRows(ctx context.Context, q openCodeQueryer, query string, args []interface{},
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, error) {
	msgs, _, err := scanOCMessageRowsWithCursor(ctx, q, query, args, providerMapping, sessionInfos, model.SyncCursor{})
	return msgs, err
}

// ===== event 高水位有界扫描 =====

// ocEventEntry 保留同一 messageID 在 event 源中最大 rowid 的 completed 终态快照。
type ocEventEntry struct {
	rowid int64
	info  openCodeInfo
	sid   string
}

// validOpenCodeEventCursor 哨兵校验：cursor.Value 对应的 rowid 上的 id 是否仍等于 cursor.ID。
// 不一致（行被删除或 rowid 被复用）或 Value=0（初始）时返回 true（允许从 0 扫描）。
func validOpenCodeEventCursor(ctx context.Context, q openCodeQueryer, cursor model.SyncCursor) bool {
	if cursor.Value == 0 {
		return true
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM event WHERE rowid=?`, cursor.Value).Scan(&id)
	return err == nil && id == cursor.ID
}

// scanOCEvents 扫描 event 源：增量模式做 high-water 有界扫描；CLI 模式全量查后 Go 侧按日期过滤。
// 返回 map[messageID]openCodeInfo（每 ID 保留最大 rowid completed 终态）、map[messageID]sessionID、event NextCursor。
func scanOCEvents(ctx context.Context, tx *sql.Tx, req CollectRequest,
) (map[string]openCodeInfo, map[string]string, model.SyncCursor, error) {
	latest := make(map[string]*ocEventEntry)

	dateSet := make(map[string]bool, len(req.Dates))
	for _, d := range req.Dates {
		dateSet[d] = true
	}

	if req.Incremental {
		eventCursor := req.Cursors[SyncSourceOpenCodeEvent]
		if !validOpenCodeEventCursor(ctx, tx, eventCursor) {
			eventCursor = model.SyncCursor{}
		}
		highWater := model.SyncCursor{}
		err := tx.QueryRowContext(ctx, `
			SELECT rowid,id FROM event ORDER BY rowid DESC LIMIT 1`).
			Scan(&highWater.Value, &highWater.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, model.SyncCursor{}, fmt.Errorf("读取 OpenCode event 高水位失败: %w", err)
		}
		query := `SELECT rowid,id,aggregate_id,seq,data
FROM event
WHERE rowid>?
  AND rowid<=?
  AND type='message.updated.1'
ORDER BY rowid;`
		if err := scanOCEventRows(ctx, tx, query, []interface{}{eventCursor.Value, highWater.Value}, latest); err != nil {
			return nil, nil, model.SyncCursor{}, err
		}
		// NextCursor 取 highWater（不是最后一条 token event），使纯非 token 尾部也能跨过
		return ocCollectEventEntries(latest, dateSet, len(req.Dates) == 0), ocCollectSessionIDs(latest), highWater, nil
	}

	// CLI 模式：全量查 type='message.updated.1'，不保存 cursor
	query := `SELECT rowid,id,aggregate_id,seq,data FROM event WHERE type='message.updated.1' ORDER BY rowid`
	if err := scanOCEventRows(ctx, tx, query, nil, latest); err != nil {
		return nil, nil, model.SyncCursor{}, err
	}
	return ocCollectEventEntries(latest, dateSet, len(req.Dates) == 0), ocCollectSessionIDs(latest), model.SyncCursor{}, nil
}

// scanOCEventRows 扫描 event 查询结果行，解析、过滤，保留每 messageID 最大 rowid completed 终态。
func scanOCEventRows(ctx context.Context, q openCodeQueryer, query string, args []interface{},
	latest map[string]*ocEventEntry,
) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("查询 OpenCode event 失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rowid int64
		var eventID, aggregateID string
		var seq int64
		var data string
		if err := rows.Scan(&rowid, &eventID, &aggregateID, &seq, &data); err != nil {
			return fmt.Errorf("扫描 OpenCode event 行失败: %w", err)
		}
		var envelope openCodeEventEnvelope
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			continue // 解析失败跳过
		}
		info := envelope.Info
		if info.Role != "assistant" || info.Time.Completed <= 0 || info.Tokens.Total == 0 {
			continue // 非 assistant / 未 completed / 无 total 不产出
		}
		sid := info.SessionID
		if sid == "" {
			sid = aggregateID
		}
		msgID := info.ID
		if msgID == "" {
			msgID = eventID
		}
		if msgID == "" {
			continue
		}
		info.ID = msgID
		if info.SessionID == "" {
			info.SessionID = sid
		}
		if existing, ok := latest[msgID]; !ok || rowid > existing.rowid {
			latest[msgID] = &ocEventEntry{rowid: rowid, info: info, sid: sid}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历 OpenCode event 行失败: %w", err)
	}
	return nil
}

// ocCollectEventEntries 从 latest map 提取 info，可选按日期过滤（CLI 模式）。
func ocCollectEventEntries(latest map[string]*ocEventEntry, dateSet map[string]bool, allDates bool) map[string]openCodeInfo {
	result := make(map[string]openCodeInfo, len(latest))
	for msgID, e := range latest {
		if !allDates {
			date := time.UnixMilli(e.info.Time.Completed).Format("2006-01-02")
			if !dateSet[date] {
				continue
			}
		}
		result[msgID] = e.info
	}
	return result
}

// ocCollectSessionIDs 从 latest map 提取 session IDs。
func ocCollectSessionIDs(latest map[string]*ocEventEntry) map[string]string {
	result := make(map[string]string, len(latest))
	for msgID, e := range latest {
		result[msgID] = e.sid
	}
	return result
}

// ===== 批量回查 =====

// batchLookupOCMessages 按 message ID 分块（500）回查 message 主表（同事务），返回有效行。
// 不加 role/completed/total SQL 过滤；Go 侧过滤非 assistant / 未 completed / 无 total 的行。
func batchLookupOCMessages(ctx context.Context, tx *sql.Tx, ids []string,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, error) {
	result := make(map[string]model.Message)
	if len(ids) == 0 {
		return result, nil
	}
	const chunkSize = 500
	for _, chunk := range chunkStrings(ids, chunkSize) {
		placeholders := strings.Repeat("?,", len(chunk)-1) + "?"
		query := fmt.Sprintf(`%s WHERE m.id IN (%s)`, ocMessageBaseQuery, placeholders)
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		msgs, err := scanOCMessageRows(ctx, tx, query, args, providerMapping, sessionInfos)
		if err != nil {
			return nil, err
		}
		for id, msg := range msgs {
			result[id] = msg
		}
	}
	return result, nil
}

// batchLookupOCSessions 按 session ID 分块查 session 表元数据，填充 sessionInfos。
func batchLookupOCSessions(ctx context.Context, tx *sql.Tx, ids []string, sessionInfos map[string]ocSessionData) error {
	if len(ids) == 0 {
		return nil
	}
	uniqueIDs := uniqueStrings(ids)
	if len(uniqueIDs) == 0 {
		return nil
	}
	const chunkSize = 500
	for _, chunk := range chunkStrings(uniqueIDs, chunkSize) {
		placeholders := strings.Repeat("?,", len(chunk)-1) + "?"
		query := fmt.Sprintf(`SELECT id,COALESCE(parent_id,''),COALESCE(directory,''),COALESCE(title,''),
       COALESCE(model,'{}'),COALESCE(time_created,0),COALESCE(time_updated,0)
FROM session WHERE id IN (%s)`, placeholders)
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查询 OpenCode session 元数据失败: %w", err)
		}
		for rows.Next() {
			var id, parentID, directory, title, modelJSON string
			var timeCreated, timeUpdated int64
			if err := rows.Scan(&id, &parentID, &directory, &title, &modelJSON, &timeCreated, &timeUpdated); err != nil {
				rows.Close()
				return fmt.Errorf("扫描 OpenCode session 行失败: %w", err)
			}
			if _, ok := sessionInfos[id]; !ok {
				sessionInfos[id] = ocSessionData{
					parentID:    parentID,
					directory:   directory,
					title:       title,
					modelJSON:   modelJSON,
					timeCreated: timeCreated,
					timeUpdated: timeUpdated,
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("遍历 OpenCode session 行失败: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭 OpenCode session 查询结果失败: %w", err)
		}
	}
	return nil
}

// ===== Session 构建 =====

// buildOpenCodeSessions 按 (client,sessionID) 去重生成 Session 元数据。
// first/last 从 session 表取，不从 token 明细反推。
func buildOpenCodeSessions(messages []model.Message, sessionInfos map[string]ocSessionData) []model.Session {
	type key struct {
		client    string
		sessionID string
	}
	seen := make(map[key]*model.Session)
	order := make([]key, 0)
	for _, m := range messages {
		k := key{m.Client, m.SessionID}
		if _, ok := seen[k]; !ok {
			sd := sessionInfos[m.SessionID]
			sess := model.Session{
				ID:        m.SessionID,
				Client:    m.Client,
				Directory: sd.directory,
				Project:   projectNameFromDir(sd.directory),
				Title:     sd.title,
				ParentID:  sd.parentID,
				FirstTS:   sd.timeCreated,
				LastTS:    sd.timeUpdated,
			}
			seen[k] = &sess
			order = append(order, k)
		}
	}
	sessions := make([]model.Session, 0, len(order))
	for _, k := range order {
		sessions = append(sessions, *seen[k])
	}
	return sessions
}

// ===== 辅助函数 =====

func projectNameFromDir(directory string) string {
	return projectBase(directory)
}

// parseModelJSON 解析 OpenCode session.model JSON 字段
// 格式: {"id": "xxx", "providerID": "xxx"}
func parseModelJSON(modelJSON string) (modelID, providerID string) {
	if modelJSON == "" {
		return "", ""
	}
	var m struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	}
	if err := json.Unmarshal([]byte(modelJSON), &m); err != nil {
		return "", ""
	}
	return m.ID, m.ProviderID
}

// loadProviderMapping 加载 ~/.cache/opencode/models.json 中的 provider 映射。
// 兼容两种结构：旧扁平结构（provider ID → 显示名字符串）与新 provider 注册表
// （provider ID → {id, name, models: {...}}，取顶层对象的 name 作显示名）。
// 文件缺失/不可读/结构无法识别 → 返回空映射，不阻断采集：
// 消息侧查不到映射时回退原始 provider 值。
func loadProviderMapping(cacheDir string, logger *slog.Logger) map[string]string {
	if logger == nil {
		logger = slog.Default()
	}
	modelsPath := filepath.Join(cacheDir, "models.json")
	data, err := os.ReadFile(modelsPath)
	if err != nil {
		return map[string]string{}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Warn("opencode models.json parse failed, provider mapping degraded to empty", "path", modelsPath, "error", err)
		return map[string]string{}
	}
	mapping := make(map[string]string, len(raw))
	for id, value := range raw {
		var display string
		if err := json.Unmarshal(value, &display); err == nil && strings.TrimSpace(display) != "" {
			mapping[id] = display
			continue
		}
		var entry struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(value, &entry); err == nil && strings.TrimSpace(entry.Name) != "" {
			mapping[id] = entry.Name
		}
	}
	return mapping
}

func chunkStrings(s []string, size int) [][]string {
	if len(s) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(s)
	}
	chunks := make([][]string, 0, (len(s)+size-1)/size)
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		chunks = append(chunks, s[i:end])
	}
	return chunks
}

func uniqueStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	result := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}
