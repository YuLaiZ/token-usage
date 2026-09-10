package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// MimoCodeCollector 从 Xiaomi MiMo Desktop / MiMo-Code CLI 共用的
// ~/.local/share/mimocode/mimocode.db 采集逐消息 token 用量。
// 单源采集：message 表 JOIN session 表，取 completed 且带 tokens.total 的
// assistant 行。event 表当前为空，不做双源；session 表无 model 列，无会话级
// 模型兜底。Desktop 与 CLI 共库且库内无来源标记，统一显示为 Xiaomi MiMo / MiMo Code。
type MimoCodeCollector struct {
	cfg    *config.Config
	dbPath string
}

// NewMimoCodeCollector 从 cfg.ClientConfig("mimocode").Paths["db"] 取库路径。
// 默认路径由 runtimecfg.ResolveEffectiveConfig 在装配期回填（effective config）。
func NewMimoCodeCollector(cfg *config.Config) *MimoCodeCollector {
	dbPath := ""
	if cfg != nil {
		if clientCfg, ok := cfg.ClientConfig("mimocode"); ok {
			if p, exists := clientCfg.Paths["db"]; exists {
				dbPath = p
			}
		}
	}
	return &MimoCodeCollector{cfg: cfg, dbPath: dbPath}
}

func (c *MimoCodeCollector) Name() string {
	return "mimocode"
}

func (c *MimoCodeCollector) SyncSources() []string {
	return []string{SyncSourceMimoCodeMessage}
}

// mimoCodeProviderDisplayNames 是 providerID → 显示名的内置映射。
// 只收录实测出现在消息中的 provider（xiaomi → Xiaomi）；查不到的 providerID
// 原值直传。不读外部模型目录文件：引擎侧缓存路径未验证存在，Desktop 的
// models-with-claude.json 又是 macOS Electron 壳特有路径。
var mimoCodeProviderDisplayNames = map[string]string{
	"xiaomi": "Xiaomi",
}

// ===== 源行结构 =====

// mimoCodeInfo 是 message.data JSON 的采集相关子集（MiMo-Code 为 OpenCode fork，
// 消息 envelope 键与 OpenCode 同构）。
type mimoCodeInfo struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// mimoSessionData 携带 session 表元数据。与 OpenCode 的差异：无 model 列，
// 因此无 modelJSON 会话级兜底。
type mimoSessionData struct {
	parentID    string
	directory   string
	title       string
	timeCreated int64
	timeUpdated int64
}

// ===== Collect 主流程 =====

// Collect 单源采集 completed assistant 消息。
//
// 全量模式（Incremental=false）：Dates 空 → 全量；Dates 非空 → 按 completed
// 日期（本机时区）SQL 过滤；不返回 NextCursors。
//
// 增量模式（Incremental=true）：按 time_updated 高水位增量，重放高水位所在的
// 完整毫秒桶，避免后到的同毫秒更新因 ID 排序落在已持久化游标之前而漏采。重复行由
// 上层按 (client,id) UPSERT 幂等覆盖；Dates 被忽略，返回 NextCursors[mimocode_message]。
//
// 不按 agent_id/agent 过滤（库中已出现 general-1 等非 main agent）。
// ctx 取消 → 返回 context error；db 缺失/读失败 → 终止性 error。
func (c *MimoCodeCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
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
		return CollectResult{}, fmt.Errorf("MimoCode collector 不能为空")
	}
	if c.dbPath == "" {
		return CollectResult{}, fmt.Errorf("mimocode DB 路径未配置")
	}
	dbInfo, statErr := os.Stat(c.dbPath)
	if os.IsNotExist(statErr) {
		return CollectResult{}, fmt.Errorf("mimocode DB 文件不存在: %s", c.dbPath)
	}
	if statErr != nil {
		return CollectResult{}, fmt.Errorf("访问 mimocode DB 失败: %w", statErr)
	}
	if !dbInfo.Mode().IsRegular() {
		return CollectResult{}, fmt.Errorf("mimocode DB 路径不是普通文件: %s", c.dbPath)
	}

	db, err := openSQLiteReadOnly(c.dbPath)
	if err != nil {
		return CollectResult{}, fmt.Errorf("打开 mimocode DB 失败: %w", err)
	}
	defer db.Close()

	query := `SELECT m.id,m.session_id,m.time_updated,m.data,
       COALESCE(s.parent_id,''),COALESCE(s.directory,''),COALESCE(s.title,''),
       COALESCE(s.time_created,0),COALESCE(s.time_updated,0)
FROM message m JOIN session s ON m.session_id=s.id
WHERE json_extract(m.data,'$.role')='assistant'
  AND json_extract(m.data,'$.time.completed') IS NOT NULL
  AND json_extract(m.data,'$.tokens.total') IS NOT NULL`
	args := []interface{}{}
	cursor := req.Cursors[SyncSourceMimoCodeMessage]

	if req.Incremental {
		// 不以 id 打破同毫秒的增量边界：消息可能在 cursor 已写入后，于同一
		// time_updated 毫秒完成/更新，而其 id 字典序小于 cursor.ID。重放最后一个
		// 毫秒桶会重复 UPSERT 少量已有行，却不会永久漏掉这种后到更新。
		query += ` AND m.time_updated>=?`
		args = append(args, cursor.Value)
	} else if len(req.Dates) > 0 {
		placeholders := strings.Repeat("?,", len(req.Dates)-1) + "?"
		query += fmt.Sprintf(` AND date(json_extract(m.data,'$.time.completed')/1000,'unixepoch','localtime') IN (%s)`, placeholders)
		for _, d := range req.Dates {
			args = append(args, d)
		}
	}
	query += ` ORDER BY m.time_updated,m.id`

	messages, sessions, next, err := scanMimoCodeRows(ctx, db, query, args, cursor)
	if err != nil {
		return CollectResult{}, err
	}
	sortMimoCodeMessages(messages)

	result := CollectResult{
		Messages: messages,
		Sessions: sessions,
	}
	if req.Incremental {
		result.NextCursors = map[string]model.SyncCursor{SyncSourceMimoCodeMessage: next}
	}
	return result, nil
}

// scanMimoCodeRows 扫描查询结果行，转换并记录 time_updated 高水位游标。
//
// SQL 过滤已确认每行是 completed assistant 且 tokens.total 非 NULL；即使 JSON
// 内容随后因解析失败或零 token 被跳过，也要跨过该源行推进游标，否则增量轮询
// 会永久重复扫描同一尾行。无新行时保持输入 cursor。
func scanMimoCodeRows(ctx context.Context, db *sql.DB, query string, args []interface{}, init model.SyncCursor) ([]model.Message, []model.Session, model.SyncCursor, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, model.SyncCursor{}, fmt.Errorf("查询 mimocode message 失败: %w", err)
	}
	defer rows.Close()

	next := init
	messages := make([]model.Message, 0, 16)
	sessionInfos := make(map[string]mimoSessionData)
	sessionOrder := make([]string, 0, 4)

	for rows.Next() {
		var id, sessionID, data string
		var timeUpdated int64
		var sessParentID, sessDirectory, sessTitle string
		var sessTimeCreated, sessTimeUpdated int64
		if err := rows.Scan(
			&id, &sessionID, &timeUpdated, &data,
			&sessParentID, &sessDirectory, &sessTitle,
			&sessTimeCreated, &sessTimeUpdated,
		); err != nil {
			return nil, nil, model.SyncCursor{}, fmt.Errorf("扫描 mimocode message 行失败: %w", err)
		}
		if timeUpdated > next.Value || (timeUpdated == next.Value && id > next.ID) {
			next = model.SyncCursor{Value: timeUpdated, ID: id}
		}
		var info mimoCodeInfo
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
			sessionInfos[sessionID] = mimoSessionData{
				parentID:    sessParentID,
				directory:   sessDirectory,
				title:       sessTitle,
				timeCreated: sessTimeCreated,
				timeUpdated: sessTimeUpdated,
			}
			sessionOrder = append(sessionOrder, sessionID)
		}
		messages = append(messages, mimoCodeMessage(info, sessionInfos[sessionID]))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, model.SyncCursor{}, fmt.Errorf("遍历 mimocode message 行失败: %w", err)
	}

	// 按 (client,sessionID) 去重生成 Session 元数据；first/last 从 session 表取，
	// 不从 token 明细反推。sessionOrder 保持首次出现顺序（time_updated 升序扫描）。
	sessions := make([]model.Session, 0, len(sessionOrder))
	for _, sid := range sessionOrder {
		sd := sessionInfos[sid]
		sessions = append(sessions, model.Session{
			ID:        sid,
			Client:    model.ClientXiaomiMiMoCode,
			Directory: sd.directory,
			Project:   projectNameFromDir(sd.directory),
			Title:     sd.title,
			ParentID:  sd.parentID,
			FirstTS:   sd.timeCreated,
			LastTS:    sd.timeUpdated,
		})
	}
	return messages, sessions, next, nil
}

// mimoCodeMessage 把源行转换为 model.Message。
// FreshInputTokens 直赋 tokens.input（实测恒等式 total==input+output+cache_read，
// input 为纯新输入），不做 SubtractCache。
func mimoCodeMessage(info mimoCodeInfo, session mimoSessionData) model.Message {
	provider := info.ProviderID
	if mapped := mimoCodeProviderDisplayNames[info.ProviderID]; mapped != "" {
		provider = mapped
	}
	return model.Message{
		ID:                info.ID,
		SessionID:         info.SessionID,
		Client:            model.ClientXiaomiMiMoCode,
		Date:              time.UnixMilli(info.Time.Completed).Format("2006-01-02"),
		TS:                info.Time.Completed,
		Model:             info.ModelID,
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

// sortMimoCodeMessages 按 (TS,ID) 稳定排序（SQL 已按 (time_updated,id) 排序，
// 输出按消息时间轴排列便于上层落库与对账）。
func sortMimoCodeMessages(messages []model.Message) {
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].TS != messages[j].TS {
			return messages[i].TS < messages[j].TS
		}
		return messages[i].ID < messages[j].ID
	})
}
