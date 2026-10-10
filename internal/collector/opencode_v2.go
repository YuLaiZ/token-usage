package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// OpenCode 2.x V2 布局适配：OpenCode 2.x 把会话数据迁移到 session_v2 /
// session_message 表并停止写 V1 表（V1 表保留但内容冻结）。两表同时存在才
// 视为迁移完成（V2 布局）；仅预建的空 session_message（1.18 中间版本形态）
// 仍是 V1 布局，防止误判造成漏采。
// V2 表列结构为合成 fixture 形态（真实 2.x 完整列清单与 total 构成待实测闭合）。

// SyncSourceOpenCodeLayout 是布局标记的 sync_state 内部 source 键：cursor_value
// 承载标记值（0=未标记零值、1=V1、2=V2），与消息、message 游标在 persistClientBatch
// 同一写事务内经 SetSyncCursors 提交（失败整轮回滚，下一轮自然重试）。
const SyncSourceOpenCodeLayout = "opencode_layout"

// 布局标记值（sync_state cursor_value）。
const (
	ocLayoutMarkV1 int64 = 1
	ocLayoutMarkV2 int64 = 2
)

// ocLayout 是 OpenCode 源库的消息表布局。
type ocLayout int

const (
	ocLayoutV1 ocLayout = iota
	ocLayoutV2
)

// ocTablePresence 记录源库关键表的存在性（每轮采集同一只读事务内探测一次）。
type ocTablePresence struct {
	session        bool
	message        bool
	event          bool
	sessionV2      bool
	sessionMessage bool
}

// layout 判定：session_v2 与 session_message 两表同时存在为 V2，否则 V1
// （含仅预建 session_message 的 1.18 形态、V1 表被清理的无表形态）。
func (p ocTablePresence) layout() ocLayout {
	if p.sessionV2 && p.sessionMessage {
		return ocLayoutV2
	}
	return ocLayoutV1
}

// detectOCTables 探测源库表布局（一次 sqlite_master 查询）。
func detectOCTables(ctx context.Context, q openCodeQueryer) (ocTablePresence, error) {
	var p ocTablePresence
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master
WHERE type='table' AND name IN ('session','message','event','session_v2','session_message')`)
	if err != nil {
		return p, fmt.Errorf("探测 OpenCode 表布局失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return p, fmt.Errorf("扫描 OpenCode 表布局失败: %w", err)
		}
		switch name {
		case "session":
			p.session = true
		case "message":
			p.message = true
		case "event":
			p.event = true
		case "session_v2":
			p.sessionV2 = true
		case "session_message":
			p.sessionMessage = true
		}
	}
	if err := rows.Err(); err != nil {
		return p, fmt.Errorf("遍历 OpenCode 表布局失败: %w", err)
	}
	return p, nil
}

// openCodeModelRef 是 V2 data.model 的对象形态。
type openCodeModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// resolveOCModelRef 解析消息 data 的模型引用双形态：modelID 字符串字段优先
// （V1 形态）；modelID 缺失时解析 model 字段（V2 形态：{id,providerID,variant}
// 对象，或纯字符串——纯字符串只提供模型 id）。对象 providerID 缺失、model 字段
// 缺失或不可解析时保留 providerID 原值（交由 session.model fallback 链兜底）。
func resolveOCModelRef(modelID, providerID string, modelRaw json.RawMessage) (string, string) {
	if modelID != "" {
		return modelID, providerID
	}
	if len(modelRaw) == 0 {
		return "", providerID
	}
	var ref openCodeModelRef
	if err := json.Unmarshal(modelRaw, &ref); err == nil && ref.ID != "" {
		if ref.ProviderID != "" {
			return ref.ID, ref.ProviderID
		}
		return ref.ID, providerID
	}
	var s string
	if err := json.Unmarshal(modelRaw, &s); err == nil && s != "" {
		return s, providerID
	}
	return "", providerID
}

// openCodeV2Message 把 V2 行转换为 model.Message。assistant 的 TS/Date 取
// data.time.completed；compaction 行没有 completed 时刻，取请求发起的
// data.time.created（压缩请求的用量归属到发起时刻）。
func openCodeV2Message(id, sessionID, msgType string, info openCodeInfo, session ocSessionData, providerMapping map[string]string) model.Message {
	modelID, providerID := resolveOCModelRef(info.ModelID, info.ProviderID, info.Model)
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
	ts := info.Time.Completed
	if msgType == "compaction" {
		ts = info.Time.Created
	}
	if info.ID == "" {
		info.ID = id
	}
	if info.SessionID == "" {
		info.SessionID = sessionID
	}
	return model.Message{
		ID:                info.ID,
		SessionID:         info.SessionID,
		Client:            model.ClientOpenCode,
		Date:              time.UnixMilli(ts).Format("2006-01-02"),
		TS:                ts,
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

// ===== V2 message 主源查询 =====

// ocV2MessageBaseQuery 与 V1 查询形态对称：session 元数据改从 session_v2 取，
// parent_id/model 列在 V2 布局下降级为空（model 以 assistant data 为主来源）。
const ocV2MessageBaseQuery = `SELECT m.id,m.session_id,m.time_updated,m.data,m.type,
       COALESCE(s.directory,''),COALESCE(s.title,''),
       COALESCE(s.time_created,0),COALESCE(s.time_updated,0)
FROM session_message m JOIN session_v2 s ON m.session_id=s.id`

// ocV2TerminalFilter 终态分流过滤：assistant 沿用 time.completed 非空；
// compaction 用 data.status ∈ {completed,failed}（V2 无 time.completed）。
const ocV2TerminalFilter = ` AND (
  (m.type='assistant' AND json_extract(m.data,'$.time.completed') IS NOT NULL)
  OR (m.type='compaction' AND json_extract(m.data,'$.status') IN ('completed','failed'))
)`

// ocV2DateFilter 日期过滤：assistant 走 completed、compaction 走 created（同落账口径）。
func ocV2DateFilter(dates []string) (string, []interface{}) {
	placeholders := strings.Repeat("?,", len(dates)-1) + "?"
	clause := fmt.Sprintf(` AND (
  (m.type='assistant' AND date(json_extract(m.data,'$.time.completed')/1000,'unixepoch','localtime') IN (%s))
  OR (m.type='compaction' AND date(json_extract(m.data,'$.time.created')/1000,'unixepoch','localtime') IN (%s))
)`, placeholders, placeholders)
	args := make([]interface{}, 0, len(dates)*2)
	for _, d := range dates {
		args = append(args, d)
	}
	for _, d := range dates {
		args = append(args, d)
	}
	return clause, args
}

// scanOCV2MessageRows 扫描 V2 查询行、解析、转换，记录 (time_updated,id) 最大游标。
// Go 侧跳过规则：assistant 未 completed（completed<=0）跳过；五分项全零跳过。
// 游标推进与 V1 对称：扫描行即推进，Go 侧跳过不回退游标。
// total 键缺失/null 的行（V2 total 构成是未闭合未知项）经 protectedIDs
// 返回：该行不落账（防未知语义按 0 覆盖既有账目），调用方以 PartialErr
// 语义处理——成功行照常入库，游标与完成状态不推进，下一轮幂等重试，待
// 真实语义闭合后由升级实现正确落账。protectedIDs 还用于把「主源存在但
// total 未知」与「主源不存在」区分开：受保护 ID 不得被 event 补偿源以
// 旧终态重新带入（否则旧值覆盖既有账目，保护被绕过）。
func scanOCV2MessageRows(ctx context.Context, q openCodeQueryer, query string, args []interface{},
	providerMapping map[string]string, sessionInfos map[string]ocSessionData, init model.SyncCursor,
) (map[string]model.Message, model.SyncCursor, map[string]bool, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, model.SyncCursor{}, nil, fmt.Errorf("查询 OpenCode session_message 失败: %w", err)
	}
	defer rows.Close()

	messages := make(map[string]model.Message)
	next := init
	protectedIDs := make(map[string]bool)

	for rows.Next() {
		var id, sessionID, data, msgType string
		var timeUpdated int64
		var sessDirectory, sessTitle string
		var sessTimeCreated, sessTimeUpdated int64
		if err := rows.Scan(
			&id, &sessionID, &timeUpdated, &data, &msgType,
			&sessDirectory, &sessTitle, &sessTimeCreated, &sessTimeUpdated,
		); err != nil {
			return nil, model.SyncCursor{}, nil, fmt.Errorf("扫描 OpenCode session_message 行失败: %w", err)
		}
		if timeUpdated > next.Value || (timeUpdated == next.Value && id > next.ID) {
			next = model.SyncCursor{Value: timeUpdated, ID: id}
		}
		var info openCodeInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			continue
		}
		switch msgType {
		case "assistant":
			if info.Time.Completed <= 0 {
				continue
			}
		case "compaction":
			if info.Status != "completed" && info.Status != "failed" {
				continue
			}
		default:
			continue
		}
		if info.Tokens.Input == 0 && info.Tokens.Output == 0 && info.Tokens.Reasoning == 0 &&
			info.Tokens.Cache.Read == 0 && info.Tokens.Cache.Write == 0 {
			continue // 五分项全零：无用量载荷，不落账（游标已推进）
		}
		if !info.Tokens.TotalPresent {
			// total 键缺失/null：构成未知，不得按 0 落账覆盖既有账目；
			// 记入受保护集合，阻止 event 补偿以旧终态绕过保护。
			protectedIDs[id] = true
			continue
		}
		if _, ok := sessionInfos[sessionID]; !ok {
			sessionInfos[sessionID] = ocSessionData{
				directory:   sessDirectory,
				title:       sessTitle,
				timeCreated: sessTimeCreated,
				timeUpdated: sessTimeUpdated,
			}
		}
		messages[id] = openCodeV2Message(id, sessionID, msgType, info, sessionInfos[sessionID], providerMapping)
	}
	if err := rows.Err(); err != nil {
		return nil, model.SyncCursor{}, nil, fmt.Errorf("遍历 OpenCode session_message 行失败: %w", err)
	}
	return messages, next, protectedIDs, nil
}

// scanOCV2MessagesIncremental 增量常态：主源仅扫 session_message（V1 已冻结）。
func scanOCV2MessagesIncremental(ctx context.Context, tx *sql.Tx, cursor model.SyncCursor,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, model.SyncCursor, map[string]bool, error) {
	query := ocV2MessageBaseQuery + ` WHERE m.type IN ('assistant','compaction')` + ocV2TerminalFilter +
		` AND (m.time_updated>? OR (m.time_updated=? AND m.id>?))` +
		` ORDER BY m.time_updated,m.id`
	args := []interface{}{cursor.Value, cursor.Value, cursor.ID}
	msgs, next, protected, err := scanOCV2MessageRows(ctx, tx, query, args, providerMapping, sessionInfos, cursor)
	return msgs, next, protected, err
}

// scanOCV2MessagesFull 全量扫描路径（布局兜底轮与 CLI Dates/全量模式）：
// V2 全量（零游标重建）+ V1 未迁移行兜底扫描合并（V2 优先，同 id 不双计）。
// V1 兜底仅在 message 表存在时执行（V2 布局下 V1 表可能已被上游清理）。
// 返回的 message 游标只从 V2 侧扫描重建（V1 兜底不是该布局的游标来源）。
func scanOCV2MessagesFull(ctx context.Context, tx *sql.Tx, dates []string,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData, tables ocTablePresence,
) (map[string]model.Message, model.SyncCursor, map[string]bool, error) {
	query := ocV2MessageBaseQuery + ` WHERE m.type IN ('assistant','compaction')` + ocV2TerminalFilter
	var args []interface{}
	if len(dates) > 0 {
		clause, dateArgs := ocV2DateFilter(dates)
		query += clause
		args = dateArgs
	}
	query += ` ORDER BY m.time_updated,m.id`
	v2Msgs, next, protected, err := scanOCV2MessageRows(ctx, tx, query, args, providerMapping, sessionInfos, model.SyncCursor{})
	if err != nil {
		return nil, model.SyncCursor{}, nil, err
	}
	if tables.message && tables.session {
		v1Msgs, err := scanOCV1Unmigrated(ctx, tx, dates, providerMapping, sessionInfos)
		if err != nil {
			return nil, model.SyncCursor{}, nil, err
		}
		for id, m := range v1Msgs {
			if _, ok := v2Msgs[id]; ok {
				continue // V2 优先，同 id 不双计
			}
			v2Msgs[id] = m
		}
	}
	return v2Msgs, next, protected, nil
}

// ocV1UnmigratedFilter 圈定 V1 未迁移行：会话在 session_v2 无对应行。
// 迁移可能渐进发生（部分会话仍只在 V1 表），全量扫描须把这部分历史补齐。
const ocV1UnmigratedFilter = ` AND NOT EXISTS (SELECT 1 FROM session_v2 sv WHERE sv.id=s.id)`

// scanOCV1Unmigrated 扫描 V1 message 中未迁移到 session_v2 的行（V1 形态解析）。
func scanOCV1Unmigrated(ctx context.Context, tx *sql.Tx, dates []string,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, error) {
	query := ocMessageBaseQuery + ` WHERE 1=1` + ocMessageFilters + ocV1UnmigratedFilter
	args := []interface{}{}
	if len(dates) > 0 {
		placeholders := strings.Repeat("?,", len(dates)-1) + "?"
		query += fmt.Sprintf(` AND date(json_extract(m.data,'$.time.completed')/1000,'unixepoch','localtime') IN (%s)`, placeholders)
		for _, d := range dates {
			args = append(args, d)
		}
	}
	query += ` ORDER BY m.time_updated,m.id`
	msgs, err := scanOCMessageRows(ctx, tx, query, args, providerMapping, sessionInfos)
	return msgs, err
}

// batchLookupOCV2Messages 是 batchLookupOCV2 的 V2 布局形态：按 message ID 分块
// （500）回查 session_message 主表（同事务）。不加 type/终态 SQL 过滤；Go 侧
// 过滤由 scanOCV2MessageRows 承担（与 V1 回查对称）。
func batchLookupOCV2Messages(ctx context.Context, tx *sql.Tx, ids []string,
	providerMapping map[string]string, sessionInfos map[string]ocSessionData,
) (map[string]model.Message, map[string]bool, error) {
	result := make(map[string]model.Message)
	if len(ids) == 0 {
		return result, nil, nil
	}
	protectedIDs := make(map[string]bool)
	const chunkSize = 500
	for _, chunk := range chunkStrings(ids, chunkSize) {
		placeholders := strings.Repeat("?,", len(chunk)-1) + "?"
		query := fmt.Sprintf(`%s WHERE m.id IN (%s)`, ocV2MessageBaseQuery, placeholders)
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		msgs, _, pt, err := scanOCV2MessageRows(ctx, tx, query, args, providerMapping, sessionInfos, model.SyncCursor{})
		if err != nil {
			return nil, nil, err
		}
		for id := range pt {
			protectedIDs[id] = true
		}
		for id, msg := range msgs {
			result[id] = msg
		}
	}
	return result, protectedIDs, nil
}

// batchLookupOCV2Sessions 是 batchLookupOCSessions 的 V2 布局形态：按 session ID
// 分块查 session_v2 元数据（无 parent_id/model 列，对应字段保持零值）。
func batchLookupOCV2Sessions(ctx context.Context, tx *sql.Tx, ids []string, sessionInfos map[string]ocSessionData) error {
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
		query := fmt.Sprintf(`SELECT id,COALESCE(directory,''),COALESCE(title,''),
       COALESCE(time_created,0),COALESCE(time_updated,0)
FROM session_v2 WHERE id IN (%s)`, placeholders)
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查询 OpenCode session_v2 元数据失败: %w", err)
		}
		for rows.Next() {
			var id, directory, title string
			var timeCreated, timeUpdated int64
			if err := rows.Scan(&id, &directory, &title, &timeCreated, &timeUpdated); err != nil {
				rows.Close()
				return fmt.Errorf("扫描 OpenCode session_v2 行失败: %w", err)
			}
			if _, ok := sessionInfos[id]; !ok {
				sessionInfos[id] = ocSessionData{
					directory:   directory,
					title:       title,
					timeCreated: timeCreated,
					timeUpdated: timeUpdated,
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("遍历 OpenCode session_v2 行失败: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭 OpenCode session_v2 查询结果失败: %w", err)
		}
	}
	return nil
}
