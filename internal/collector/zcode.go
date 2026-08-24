package collector

import (
	"context"
	"database/sql"
	"encoding/json"
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

// ZCodeCollector 从 ~/.zcode/cli/db/db.sqlite 采集逐请求 token 用量。
// 数据源：model_usage（一行=一次模型 API 请求）LEFT JOIN session 取 directory/parent_id/title；
// status='completed' 行逐行产出一条 Message，按 completed_at（不用 started_at）归日期/增量。
type ZCodeCollector struct {
	cfg    *config.Config
	dbPath string
}

// NewZCodeCollector 从 cfg.ClientConfig("zcode").Paths["db"] 取库路径。
// 默认路径由 runtimecfg.ResolveEffectiveConfig 在装配期回填（effective config）。
func NewZCodeCollector(cfg *config.Config) *ZCodeCollector {
	dbPath := ""
	if cfg != nil {
		if clientCfg, ok := cfg.ClientConfig("zcode"); ok {
			if p, exists := clientCfg.Paths["db"]; exists {
				dbPath = p
			}
		}
	}
	return &ZCodeCollector{cfg: cfg, dbPath: dbPath}
}

func (c *ZCodeCollector) Name() string {
	return "zcode"
}

func (c *ZCodeCollector) SyncSources() []string { return []string{SyncSourceZCodeModelUsage} }

// Collect 逐请求采集 completed model_usage。
//
// 全量模式（Incremental=false）：
//   - Dates 空 → 不加 completed_at 范围，全量扫描；
//   - Dates 非空 → 每个日期各执行一次左闭右开范围查询，结果按 (client,messageID) 去重。
//
// 增量模式（Incremental=true）：
//   - 只执行复合游标查询 completed_at>? OR (completed_at=? AND id>?)，cursor 取
//     req.Cursors[SyncSourceZCodeModelUsage]；Dates 被忽略。
//
// 日期/增量锚点统一用 completed_at（不用 started_at/rowid），保证 running→completed 行能被捕获。
// ctx 取消 → 返回 context error，不把不完整批次伪装成成功。
// db 不存在/读失败 → 终止性 error（zcode 单库，无多库降级空间）。
func (c *ZCodeCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
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
		return CollectResult{}, fmt.Errorf("ZCode collector 不能为空")
	}
	if c.dbPath == "" {
		return CollectResult{}, fmt.Errorf("zcode DB 路径未配置")
	}
	dbInfo, statErr := os.Stat(c.dbPath)
	if os.IsNotExist(statErr) {
		return CollectResult{}, fmt.Errorf("zcode DB 文件不存在: %s", c.dbPath)
	}
	if statErr != nil {
		return CollectResult{}, fmt.Errorf("访问 zcode DB 失败: %w", statErr)
	}
	if !dbInfo.Mode().IsRegular() {
		return CollectResult{}, fmt.Errorf("zcode DB 路径不是普通文件: %s", c.dbPath)
	}

	db, err := openSQLiteReadOnly(c.dbPath)
	if err != nil {
		return CollectResult{}, fmt.Errorf("打开 zcode DB 失败: %w", err)
	}
	defer db.Close()

	providerMap := loadZCodeProviderMap(zcodeCachePathFromDB(c.dbPath))
	// provider 映射缺失是预期降级（历史 provider 不在缓存），聚合到 Collect 层
	// 输出唯一一条汇总，避免逐消息刷屏；count 仍能暴露 schema 漂移面。
	misses := &providerMissStats{}

	cursor := req.Cursors[SyncSourceZCodeModelUsage]
	var (
		messages    []model.Message
		sessionMeta []zcodeSessionMeta
		next        = cursor // 没有新行时保持输入 cursor，避免回退
	)

	if req.Incremental {
		msgs, metas, n, err := c.queryIncremental(ctx, db, cursor, providerMap, misses, logger)
		if err != nil {
			return CollectResult{}, err
		}
		messages = msgs
		sessionMeta = metas
		next = n
	} else {
		// 构造日期范围列表：(0,0) 哨兵表示全量（不加 completed_at WHERE）。
		type rng struct{ start, end int64 }
		ranges := make([]rng, 0, len(req.Dates))
		if len(req.Dates) == 0 {
			ranges = append(ranges, rng{0, 0})
		} else {
			for _, d := range req.Dates {
				startMS, endMS, err := zcodeDateToMillisecondRange(d)
				if err != nil {
					return CollectResult{}, fmt.Errorf("解析日期 %q 失败: %w", d, err)
				}
				ranges = append(ranges, rng{startMS, endMS})
			}
		}
		// 多 Dates 分别执行范围查询，最终按 message ID 去重。
		seen := make(map[string]bool)
		for _, r := range ranges {
			if err := ctx.Err(); err != nil {
				return CollectResult{}, err
			}
			msgs, metas, _, err := c.queryRange(ctx, db, r.start, r.end, providerMap, misses, logger)
			if err != nil {
				return CollectResult{}, err
			}
			for _, m := range msgs {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				messages = append(messages, m)
				next = maxSyncCursor(next, model.SyncCursor{Value: m.TS, ID: m.ID})
			}
			sessionMeta = append(sessionMeta, metas...)
		}
	}

	// Session 元数据：按 (client,sessionID) 去重，写 ParentID/title/directory/first/last。
	sessions := buildZCodeSessions(messages, sessionMeta)
	misses.logSummary(logger)

	result := CollectResult{
		Messages: messages,
		Sessions: sessions,
	}
	// 只在 Incremental 结果中返回 NextCursor（全量模式不设置，避免误导）。
	if req.Incremental {
		result.NextCursors = map[string]model.SyncCursor{
			SyncSourceZCodeModelUsage: next,
		}
	}
	return result, nil
}

// providerMissStats 聚合一次 Collect 内的 provider 映射缺失（跨多日期范围/增量查询），
// 由 Collect 末尾输出唯一一条汇总——逐消息打印会让全量扫描刷屏，而汇总的 count
// 仍能暴露缓存文件 schema 漂移导致的映射面收窄。
type providerMissStats struct {
	count int
	ids   map[string]struct{}
}

func (s *providerMissStats) record(providerID string) {
	s.count++
	if s.ids == nil {
		s.ids = make(map[string]struct{})
	}
	s.ids[providerID] = struct{}{}
}

func (s *providerMissStats) logSummary(logger *slog.Logger) {
	if s.count == 0 {
		return
	}
	ids := make([]string, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	logger.Debug("ZCode provider mapping missing, kept original value",
		"count", s.count, "provider_ids", strings.Join(ids, ","))
}

// queryRange 执行单次全量/范围查询：startMS/endMS 均为 0 时不加 completed_at 范围（全量）。
// 逐行扫描 completed 行（不再 SUM/GROUP BY），每行一条 Message。
// 返回 messages、session 元数据片段与本批最大 (completed_at, id) 游标。
// 没有新行时 next 保持输入 init cursor，避免回退。
func (c *ZCodeCollector) queryRange(ctx context.Context, db *sql.DB, startMS, endMS int64,
	providerMap map[string]string, misses *providerMissStats, logger *slog.Logger) ([]model.Message, []zcodeSessionMeta, model.SyncCursor, error) {
	query := zcodeBaseSelect + ` WHERE m.status='completed'`
	args := []any{}
	if startMS != 0 || endMS != 0 {
		query += ` AND m.completed_at>=? AND m.completed_at<?`
		args = append(args, startMS, endMS)
	}
	query += ` ORDER BY m.completed_at,m.id`
	return c.scanRows(ctx, db, query, args, providerMap, misses, logger, model.SyncCursor{})
}

// queryIncremental 执行增量复合游标查询：
// completed_at>? OR (completed_at=? AND id>?)。
// 没有新行时 next 保持输入 cursor，避免回退。
func (c *ZCodeCollector) queryIncremental(ctx context.Context, db *sql.DB, cursor model.SyncCursor,
	providerMap map[string]string, misses *providerMissStats, logger *slog.Logger) ([]model.Message, []zcodeSessionMeta, model.SyncCursor, error) {
	query := zcodeBaseSelect +
		` WHERE m.status='completed'` +
		` AND (m.completed_at>? OR (m.completed_at=? AND m.id>?))` +
		` ORDER BY m.completed_at,m.id`
	args := []any{cursor.Value, cursor.Value, cursor.ID}
	return c.scanRows(ctx, db, query, args, providerMap, misses, logger, cursor)
}

// zcodeBaseSelect 逐行查询的列定义（不再 SUM/GROUP BY）。
// provider_total_tokens 用 sql.NullInt64 扫描（可为 NULL），其余 NULL 列 COALESCE 到 0/”。
const zcodeBaseSelect = `SELECT COALESCE(m.id,''), COALESCE(m.session_id,''),
       COALESCE(m.model_id,''), COALESCE(m.provider_id,''), COALESCE(m.completed_at,0),
       COALESCE(m.input_tokens,0), COALESCE(m.output_tokens,0),
       COALESCE(m.reasoning_tokens,0),
       COALESCE(m.cache_creation_input_tokens,0),
       COALESCE(m.cache_read_input_tokens,0),
       m.provider_total_tokens, COALESCE(m.computed_total_tokens,0),
       COALESCE(s.directory,''), COALESCE(s.parent_id,''),
       COALESCE(s.title,''), COALESCE(s.time_created,0), COALESCE(s.time_updated,0)
FROM model_usage m
LEFT JOIN session s ON m.session_id=s.id`

// zcodeSessionMeta 携带 session 表元数据（parent_id/title/directory），与 Message 一同返回，
// 供 buildZCodeSessions 在去重时填充 Session.ParentID/Title。
type zcodeSessionMeta struct {
	sessionID string
	parentID  string
	title     string
	directory string
}

// scanRows 扫描查询结果，逐行生成 Message，记录本批最大 (completed_at,id) 游标。
// 没有新行时 next 保持传入的 init cursor，避免回退。
func (c *ZCodeCollector) scanRows(ctx context.Context, db *sql.DB, query string, args []any,
	providerMap map[string]string, misses *providerMissStats, logger *slog.Logger, init model.SyncCursor) ([]model.Message, []zcodeSessionMeta, model.SyncCursor, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, model.SyncCursor{}, fmt.Errorf("查询 zcode model_usage 失败: %w", err)
	}
	defer rows.Close()

	var (
		messages    []model.Message
		sessionMeta []zcodeSessionMeta
		next        = init // 没有新行时保持 init cursor，避免回退
		hasNext     bool
	)
	for rows.Next() {
		var (
			id            string
			sessionID     string
			modelID       string
			providerID    string
			completedAt   int64
			input         int64
			output        int64
			reasoning     int64
			cacheCreate   int64
			cacheRead     int64
			providerTotal sql.NullInt64
			computedTotal int64
			directory     string
			parentID      string
			title         string
			timeCreated   int64
			timeUpdated   int64
		)
		if err := rows.Scan(
			&id, &sessionID, &modelID, &providerID, &completedAt,
			&input, &output, &reasoning, &cacheCreate, &cacheRead,
			&providerTotal, &computedTotal,
			&directory, &parentID, &title, &timeCreated, &timeUpdated,
		); err != nil {
			return nil, nil, model.SyncCursor{}, fmt.Errorf("扫描 zcode 行失败: %w", err)
		}
		// 即使源行无效也跨过其复合键，避免增量轮询反复读取同一行。
		if !hasNext || completedAt > next.Value || (completedAt == next.Value && id > next.ID) {
			next = model.SyncCursor{Value: completedAt, ID: id}
			hasNext = true
		}
		if id == "" || sessionID == "" || completedAt <= 0 {
			logger.Debug("ZCode completed line missing required identifiers or timestamp, skipped",
				"usage_id", id, "session_id", sessionID, "completed_at", completedAt)
			continue
		}
		// total：provider_total 非 NULL 优先，NULL 回退 computed_total。
		total := computedTotal
		if providerTotal.Valid {
			total = providerTotal.Int64
		}
		fresh := model.SubtractCache(input, cacheRead, cacheCreate)
		if input < cacheRead+cacheCreate {
			logger.Warn("ZCode cache token exceeds input, fresh input clamped to zero",
				"usage_id", id, "input", input, "cache_read", cacheRead, "cache_create", cacheCreate)
		}
		provider := providerID
		if name, ok := providerMap[providerID]; ok && name != "" {
			provider = name
		} else {
			misses.record(providerID)
		}
		project := projectBase(directory)
		messages = append(messages, model.Message{
			ID:                id,
			SessionID:         sessionID,
			Client:            model.ClientZCode,
			Date:              zcodeTsMsToDate(completedAt),
			TS:                completedAt,
			Model:             modelID,
			Provider:          provider,
			Directory:         directory,
			Project:           project,
			InputTokens:       input,
			FreshInputTokens:  fresh,
			OutputTokens:      output,
			CacheReadTokens:   cacheRead,
			CacheCreateTokens: cacheCreate,
			ReasoningTokens:   reasoning,
			TotalTokens:       total,
		})
		sessionMeta = append(sessionMeta, zcodeSessionMeta{
			sessionID: sessionID, parentID: parentID, title: title, directory: directory,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, model.SyncCursor{}, fmt.Errorf("遍历 zcode 行失败: %w", err)
	}
	return messages, sessionMeta, next, nil
}

// buildZCodeSessions 按 (client,sessionID) 去重生成 Session 元数据。
// 写入 ParentID/title/directory/project/first/last。
// first/last 取 message 的 TS min/max；parent_id/title/directory 优先取 sessionMeta 中非空的值。
func buildZCodeSessions(messages []model.Message, sessionMeta []zcodeSessionMeta) []model.Session {
	type sessKey struct {
		client    string
		sessionID string
	}
	type acc struct {
		sess     model.Session
		parentID string
		title    string
	}
	seen := make(map[sessKey]*acc)
	order := make([]sessKey, 0)
	// 先从 sessionMeta 注入 parent_id/title/directory。
	for _, sm := range sessionMeta {
		key := sessKey{client: model.ClientZCode, sessionID: sm.sessionID}
		a, ok := seen[key]
		if !ok {
			seen[key] = &acc{}
			order = append(order, key)
			a = seen[key]
		}
		if a.parentID == "" && sm.parentID != "" {
			a.parentID = sm.parentID
		}
		if a.title == "" && sm.title != "" {
			a.title = sm.title
		}
	}
	// 再用 message 的 TS 合并 first/last、directory/project。
	for _, m := range messages {
		key := sessKey{client: m.Client, sessionID: m.SessionID}
		a, ok := seen[key]
		if !ok {
			seen[key] = &acc{}
			order = append(order, key)
			a = seen[key]
		}
		if a.sess.ID == "" {
			a.sess = model.Session{
				ID:        m.SessionID,
				Client:    m.Client,
				Directory: m.Directory,
				Project:   m.Project,
				FirstTS:   m.TS,
				LastTS:    m.TS,
			}
		}
		if m.TS < a.sess.FirstTS {
			a.sess.FirstTS = m.TS
		}
		if m.TS > a.sess.LastTS {
			a.sess.LastTS = m.TS
		}
	}
	sessions := make([]model.Session, 0, len(order))
	for _, k := range order {
		a := seen[k]
		a.sess.ParentID = a.parentID
		a.sess.Title = a.title
		sessions = append(sessions, a.sess)
	}
	return sessions
}

// maxSyncCursor 返回两个复合游标 (completed_at,id) 的较大者（字典序比较：先 Value 再 ID）。
func maxSyncCursor(a, b model.SyncCursor) model.SyncCursor {
	if b.Value > a.Value || (b.Value == a.Value && b.ID > a.ID) {
		return b
	}
	return a
}

// zcodeDateToMillisecondRange 日期字符串 → 毫秒左闭右开区间（start=当日 00:00，end=次日 00:00）。
// 时区口径：time.ParseInLocation(..., time.Local) 与 zcodeTsMsToDate 同口径。
func zcodeDateToMillisecondRange(date string) (int64, int64, error) {
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, 0, err
	}
	return t.UnixMilli(), t.AddDate(0, 0, 1).UnixMilli(), nil
}

// zcodeTsMsToDate 毫秒时间戳 → 2006-01-02。
func zcodeTsMsToDate(tsMs int64) string {
	if tsMs <= 0 {
		return ""
	}
	return time.UnixMilli(tsMs).Format("2006-01-02")
}

// zcodeCachePathFromDB 由 db 路径推导 bots-model-cache.v2.json 路径。
// db = ~/.zcode/cli/db/db.sqlite → 根 ~/.zcode/ + v2/bots-model-cache.v2.json。
func zcodeCachePathFromDB(dbPath string) string {
	zcodeRoot := filepath.Dir(filepath.Dir(filepath.Dir(dbPath)))
	return filepath.Join(zcodeRoot, "v2", "bots-model-cache.v2.json")
}

// loadZCodeProviderMap 读 bots-model-cache.v2.json，返回 provider_id→name 映射。
// 兼容两种 schema：version 1 的顶层 providers[].id/name；version 2 起顶层 providers
// 字段移除，provider 显示名挪到 workspaceConfigOptions.{workspace::tab}.configOptions[]
// .options[].modelProviderId/modelProviderName。两路同一次 Unmarshal 合并解析。
// 只解码需要字段（apiKey 等敏感字段不在 struct 中，json.Unmarshal 忽略未知字段）。
// 文件不存在/解析失败返回空 map（不 fatal，Collect 时 fallback provider_id 原值）。
func loadZCodeProviderMap(cachePath string) map[string]string {
	m := make(map[string]string)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return m
	}
	var doc struct {
		Providers []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"providers"`
		WorkspaceConfigOptions map[string]struct {
			ConfigOptions []struct {
				Options []struct {
					ModelProviderID   string `json:"modelProviderId"`
					ModelProviderName string `json:"modelProviderName"`
				} `json:"options"`
			} `json:"configOptions"`
		} `json:"workspaceConfigOptions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return m
	}
	for _, p := range doc.Providers {
		if p.ID != "" {
			m[p.ID] = p.Name
		}
	}
	for _, ws := range doc.WorkspaceConfigOptions {
		for _, opt := range ws.ConfigOptions {
			for _, model := range opt.Options {
				if model.ModelProviderID != "" {
					m[model.ModelProviderID] = model.ModelProviderName
				}
			}
		}
	}
	return m
}
