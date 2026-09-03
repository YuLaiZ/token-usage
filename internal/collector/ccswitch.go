// internal/collector/ccswitch.go
package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

// ccSwitchRouterName CC Switch router 的统一名称标识
const ccSwitchRouterName = "cc_switch"

type CCSwitchAdapter struct {
	name   string // 配置里的 router 名（通常为 "cc_switch"，未来可多实例）
	dbPath string
	cfg    *config.Config // 预留：CollectLogs 可能需要其他 cfg 读取
}

// NewCCSwitchAdapter 接收配置名、路由配置与全局配置。
// name 为 routers 表中的 key；dbPath 取自 rc（不再写死读 routers.cc_switch）。
func NewCCSwitchAdapter(name string, rc config.RouterConfig, cfg *config.Config) *CCSwitchAdapter {
	return &CCSwitchAdapter{name: name, dbPath: rc.DBPath, cfg: cfg}
}

func (a *CCSwitchAdapter) Name() string { return a.name }

func (a *CCSwitchAdapter) Capabilities() RouterCapabilities {
	return RouterCapabilities{
		Provider:     true, // 从 providers 表获取准确 provider name
		Model:        true, // 路由后真实 model
		InputTokens:  true, // 用于与 JSONL 一致性校验
		OutputTokens: true,
		CacheTokens:  true,
	}
}

func (a *CCSwitchAdapter) SyncSource() string { return SyncSourceCCSwitchRouter }

// CollectLogs 查询 proxy_request_logs，返回 raw RouterLog 列表与增量 NextCursor。
// MessageID 提取：claude/claude-desktop 取 session: 前缀后整段，codex 取
// session:codex:{pid}: 前缀后末段；其余形态的 raw log 仍保存但不参与消息关联。
func (a *CCSwitchAdapter) CollectLogs(ctx context.Context, req RouterCollectRequest, logger *slog.Logger) (RouterCollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RouterCollectResult{}, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if a == nil {
		return RouterCollectResult{}, fmt.Errorf("CC Switch adapter 不能为空")
	}
	if a.dbPath == "" {
		return RouterCollectResult{}, fmt.Errorf("CC Switch 数据库路径未配置")
	}
	if !req.Incremental {
		for _, date := range req.Dates {
			if _, err := time.ParseInLocation("2006-01-02", date, time.Local); err != nil {
				return RouterCollectResult{}, fmt.Errorf("解析日期 %q 失败: %w", date, err)
			}
		}
	}
	dbInfo, statErr := os.Stat(a.dbPath)
	if os.IsNotExist(statErr) {
		return RouterCollectResult{}, fmt.Errorf("CC Switch 数据库文件不存在: %s", a.dbPath)
	}
	if statErr != nil {
		return RouterCollectResult{}, fmt.Errorf("访问 CC Switch 数据库失败: %w", statErr)
	}
	if !dbInfo.Mode().IsRegular() {
		return RouterCollectResult{}, fmt.Errorf("CC Switch 数据库路径不是普通文件: %s", a.dbPath)
	}
	// 只读打开（与 codex/opencode/workbuddy 一致），避免对用户 CC-Switch 库产生写锁/副作用
	db, err := openSQLiteReadOnly(a.dbPath)
	if err != nil {
		return RouterCollectResult{}, fmt.Errorf("打开 CC Switch 数据库失败: %w", err)
	}
	defer db.Close()

	providerNames, err := loadCCSwitchProviderNames(ctx, db)
	if err != nil {
		return RouterCollectResult{}, fmt.Errorf("加载 providers 失败: %w", err)
	}

	// data_source / request_model / input_token_semantics 三列在 cc-switch 上游
	// 并非同版本引入，按列粒度探测存在性、独立决定采集或降级（无整体豁免）。
	columns, err := probeCCSwitchColumns(ctx, db)
	if err != nil {
		return RouterCollectResult{}, err
	}
	if missing := columns.missingNames(); len(missing) > 0 {
		logger.Warn("CC Switch proxy_request_logs missing optional columns, using defaults",
			"columns", strings.Join(missing, ", "))
	}

	query, args := buildCCSwitchProxyQuery(req, columns)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return RouterCollectResult{}, fmt.Errorf("查询 proxy_request_logs 失败: %w", err)
	}
	defer rows.Close()

	var logs []model.RouterLog
	// 增量模式：next 初始为输入 cursor（无新行时保持，避免回退）；
	// 非增量模式：next 保持零值。
	next := req.Cursor
	for rows.Next() {
		var (
			requestID, sessionID, modelName, providerID, appType, errMsg string
			dataSource, requestModel                                     string
			isStreaming, inputTokenSemantics                             int
			inputTokens, outputTokens, cacheRead, cacheCreate            int64
			totalCostUSD                                                 float64
			latencyMs, statusCode                                        int
			createdAt                                                    int64
		)
		if err := rows.Scan(
			&requestID, &sessionID, &modelName, &providerID, &appType, &isStreaming,
			&inputTokens, &outputTokens, &cacheRead, &cacheCreate,
			&totalCostUSD, &latencyMs, &statusCode, &errMsg, &createdAt,
			&dataSource, &requestModel, &inputTokenSemantics,
		); err != nil {
			return RouterCollectResult{}, fmt.Errorf("扫描 proxy_request_logs 行失败: %w", err)
		}

		// 无效源行不能写入 raw_router_logs；仍推进游标以避免重复读取。
		if createdAt > next.Value || (createdAt == next.Value && requestID > next.ID) {
			next = model.SyncCursor{Value: createdAt, ID: requestID}
		}
		if requestID == "" || createdAt <= 0 {
			logger.Debug("CC Switch log missing request_id or created_at, skipped",
				"request_id", requestID, "created_at", createdAt)
			continue
		}

		// 无可提取前缀的形态不参与消息关联是结构性预期行为（未关联量可经
		// raw_router_logs 查询），不留日志；raw 照常落库。
		messageID := extractMessageIDFromRequestID(appType, requestID)

		// 旧版 cc-switch 无 data_source 列时按严格 codex_session: 前缀本地分类
		// （与 schema v3 迁移的 GLOB 判定同一口径）：不能一律视为 'proxy'，否则
		// 会话同步行会被误标并经 INSERT OR REPLACE 覆盖既有 'codex_session' 标记。
		if !columns.dataSource {
			dataSource = classifyDataSource(requestID)
		}

		// json.Marshal 仅含基础类型（string/int/bool/float64/[]string），不会失败；忽略 error。
		rawData, _ := json.Marshal(map[string]any{
			"is_streaming":          isStreaming,
			"total_cost_usd":        totalCostUSD,
			"latency_ms":            latencyMs,
			"status_code":           statusCode,
			"error_message":         errMsg,
			"request_model":         requestModel,
			"input_token_semantics": inputTokenSemantics,
		})

		logs = append(logs, model.RouterLog{
			RequestID:         requestID,
			MessageID:         messageID,
			RouterName:        a.name,
			SessionID:         sessionID,
			AppType:           appType,
			Model:             modelName,
			ProviderID:        providerID,
			ProviderName:      ccSwitchProviderName(providerNames, providerID, appType),
			InputTokens:       inputTokens,
			OutputTokens:      outputTokens,
			CacheReadTokens:   cacheRead,
			CacheCreateTokens: cacheCreate,
			CreatedAt:         createdAt,
			DataSource:        dataSource,
			RawData:           string(rawData),
		})

	}
	if err := rows.Err(); err != nil {
		return RouterCollectResult{}, fmt.Errorf("遍历 proxy_request_logs 失败: %w", err)
	}

	result := RouterCollectResult{Logs: logs}
	if req.Incremental {
		result.NextCursor = next
	}
	return result, nil
}

// extractMessageIDFromRequestID 绑定 app_type 提取 message_id 关联键。
// claude/claude-desktop 的 session:<id> 前缀取剩余整段；
// codex 的 session:codex:{provider_id}:{message_id} 前缀取末段 message_id
// （provider_id 可含冒号，取最后一个冒号后的段）；其余形态（codex_session:
// 同步行前缀、opencode、随机 UUID、未知 app_type）返回空串——raw log 仍保留，
// 但不参与消息关联。
func extractMessageIDFromRequestID(appType, requestID string) string {
	switch appType {
	case "claude", "claude-desktop":
		if !strings.HasPrefix(requestID, "session:") {
			return ""
		}
		return strings.TrimPrefix(requestID, "session:")
	case "codex":
		const prefix = "session:codex:"
		if !strings.HasPrefix(requestID, prefix) {
			return ""
		}
		rest := requestID[len(prefix):]
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			return rest[i+1:]
		}
		return ""
	default:
		return ""
	}
}

// ccSwitchOptionalColumns 记录 proxy_request_logs 可选列的存在性（PRAGMA table_info
// 探测，每连接一次）。三列在 cc-switch 上游非同版本引入，因此按列粒度独立分支。
type ccSwitchOptionalColumns struct {
	dataSource          bool
	requestModel        bool
	inputTokenSemantics bool
}

// missingNames 按稳定顺序列出缺失列名（供 Warn 汇总；全存在时为 nil）。
func (c ccSwitchOptionalColumns) missingNames() []string {
	var missing []string
	if !c.dataSource {
		missing = append(missing, "data_source")
	}
	if !c.requestModel {
		missing = append(missing, "request_model")
	}
	if !c.inputTokenSemantics {
		missing = append(missing, "input_token_semantics")
	}
	return missing
}

// probeCCSwitchColumns 用 PRAGMA table_info 探测可选列存在性。
// PRAGMA 不支持参数绑定，语句为固定字面量。
func probeCCSwitchColumns(ctx context.Context, db *sql.DB) (ccSwitchOptionalColumns, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(proxy_request_logs)")
	if err != nil {
		return ccSwitchOptionalColumns{}, fmt.Errorf("探测 proxy_request_logs 列失败: %w", err)
	}
	defer rows.Close()
	var cols ccSwitchOptionalColumns
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return ccSwitchOptionalColumns{}, fmt.Errorf("扫描 table_info 行失败: %w", err)
		}
		switch name {
		case "data_source":
			cols.dataSource = true
		case "request_model":
			cols.requestModel = true
		case "input_token_semantics":
			cols.inputTokenSemantics = true
		}
	}
	return cols, rows.Err()
}

// classifyDataSource 在源库缺 data_source 列时按 request_id 严格前缀本地分类：
// codex_session: 前缀 → 'codex_session'，其余（含近似前缀 codexXsession:）→ 'proxy'。
// 与 schema v3 迁移的 GLOB 'codex_session:*' 判定保持同一口径。
func classifyDataSource(requestID string) string {
	if strings.HasPrefix(requestID, "codex_session:") {
		return "codex_session"
	}
	return "proxy"
}

// loadCCSwitchProviderNames 一次性加载 providers 表，返回 "providerID|appType" -> name
// 同时写入 "providerID" -> name 的 fallback 键（应对 CC Switch app_type 错标 issue #3985，
// 某 provider 只在 claude-desktop 注册但请求被错标为 claude 时，精确键 miss 可回退）
// 避免 CollectLogs 内逐行查表（N+1）
func loadCCSwitchProviderNames(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(id,''), COALESCE(app_type,''), COALESCE(name,'') FROM providers`)
	if err != nil {
		return nil, fmt.Errorf("查询 providers 失败: %w", err)
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var id, appType, name string
		if err := rows.Scan(&id, &appType, &name); err != nil {
			return nil, fmt.Errorf("扫描 providers 行失败: %w", err)
		}
		if id == "" {
			continue
		}
		m[ccSwitchProviderKey(id, appType)] = name
		if _, ok := m[id]; !ok {
			m[id] = name // id-only fallback 键（首次写入，多 app_type 同名时一致）
		}
	}
	return m, rows.Err()
}

// ccSwitchProviderName 查 provider name：先用 (providerID, appType) 精确键，
// miss 时回退到 providerID-only（应对 CC Switch app_type 错标 issue #3985）
func ccSwitchProviderName(m map[string]string, providerID, appType string) string {
	if n, ok := m[ccSwitchProviderKey(providerID, appType)]; ok && n != "" {
		return n
	}
	return m[providerID] // fallback；map 未命中时为零值空串
}

func ccSwitchProviderKey(providerID, appType string) string {
	return providerID + "|" + appType
}

// buildCCSwitchProxyQuery 构造 proxy_request_logs 查询。
// 优先级：Incremental 为 true 时忽略 Dates，按复合游标 (created_at,request_id) 增量过滤；
// 否则按 Dates 本地日秒级左闭右开 OR 过滤；两者都没有时全量（用于显式全量测试）。
// 三种模式都追加 ORDER BY created_at,request_id 保证稳定顺序与游标推进。
// 注意：error_message 列可空（fixture 与真实库都有 NULL 行），Scan 到 *string 遇 NULL 会报错，
// 用 COALESCE(error_message, ”) 在 SQL 层兜空串。
// 可选列（data_source/request_model/input_token_semantics）按探测结果进 SELECT，
// 缺失列用等值字面量占位（扫描侧无感，缺 data_source 时行组装层另行本地分类）。
func buildCCSwitchProxyQuery(req RouterCollectRequest, columns ccSwitchOptionalColumns) (string, []interface{}) {
	dataSourceSel := `'proxy'`
	if columns.dataSource {
		dataSourceSel = `COALESCE(data_source,'proxy')`
	}
	requestModelSel := `''`
	if columns.requestModel {
		requestModelSel = `COALESCE(request_model,'')`
	}
	semanticsSel := `0`
	if columns.inputTokenSemantics {
		semanticsSel = `COALESCE(input_token_semantics,0)`
	}
	base := `SELECT COALESCE(request_id,''), COALESCE(session_id,''),
		COALESCE(model,''), COALESCE(provider_id,''), COALESCE(app_type,''), COALESCE(is_streaming,0),
		COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		COALESCE(cache_read_tokens,0), COALESCE(cache_creation_tokens,0),
		COALESCE(total_cost_usd,0), COALESCE(latency_ms,0), COALESCE(status_code,0),
		COALESCE(error_message, ''), COALESCE(created_at,0),
		` + dataSourceSel + `, ` + requestModelSel + `, ` + semanticsSel + `
		FROM proxy_request_logs`
	if req.Incremental {
		return base + ` WHERE created_at>? OR (created_at=? AND request_id>?) ORDER BY created_at,request_id`,
			[]interface{}{req.Cursor.Value, req.Cursor.Value, req.Cursor.ID}
	}
	if len(req.Dates) == 0 {
		return base + ` ORDER BY created_at,request_id`, nil
	}
	var filters []string
	var args []interface{}
	for _, d := range req.Dates {
		start, end := dateRangeUnix(d, time.Local)
		filters = append(filters, "(created_at >= ? AND created_at < ?)")
		args = append(args, start, end)
	}
	return base + " WHERE " + strings.Join(filters, " OR ") + ` ORDER BY created_at,request_id`, args
}

// dateToUnix 将 "YYYY-MM-DD" 转为 Local 当天 00:00 的 unix 秒
// 与 Claude 侧 tsMsToDate（time.UnixMilli，Local）保持同一时区口径，
// 保证「用户视角的同一天」在 Claude session 与 router created_at 上对齐
func dateToUnix(date string) int64 {
	start, _ := dateRangeUnix(date, time.Local)
	return start
}

func dateRangeUnix(date string, loc *time.Location) (start, end int64) {
	t, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return 0, 0
	}
	return t.Unix(), t.AddDate(0, 0, 1).Unix()
}
