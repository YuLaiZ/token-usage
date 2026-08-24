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
// 只有 claude/claude-desktop app_type 的 session:<id> 生成 MessageID；
// 其他 app_type 的 raw log 仍保存但不参与消息关联（Debug 日志记录）。
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

	query, args := buildCCSwitchProxyQuery(req)
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
			isStreaming                                                  int
			inputTokens, outputTokens, cacheRead, cacheCreate            int64
			totalCostUSD                                                 float64
			latencyMs, statusCode                                        int
			createdAt                                                    int64
		)
		if err := rows.Scan(
			&requestID, &sessionID, &modelName, &providerID, &appType, &isStreaming,
			&inputTokens, &outputTokens, &cacheRead, &cacheCreate,
			&totalCostUSD, &latencyMs, &statusCode, &errMsg, &createdAt,
		); err != nil {
			return RouterCollectResult{}, fmt.Errorf("扫描 proxy_request_logs 行失败: %w", err)
		}

		// 无效源行不能写入 raw_router_logs；仍推进游标以避免重复读取。
		if createdAt > next.Value || (createdAt == next.Value && requestID > next.ID) {
			next = model.SyncCursor{Value: createdAt, ID: requestID}
		}
		if requestID == "" || createdAt <= 0 {
			logger.Debug("CC Switch 日志缺少 request_id 或 created_at，跳过",
				"request_id", requestID, "created_at", createdAt)
			continue
		}

		// app_type 非 claude/claude-desktop 时不参与消息关联是结构性预期行为
		// （未关联量可经 raw_router_logs 查询），不留日志；raw 照常落库。
		messageID := extractMessageIDFromRequestID(appType, requestID)

		// json.Marshal 仅含基础类型（string/int/bool/float64/[]string），不会失败；忽略 error。
		rawData, _ := json.Marshal(map[string]any{
			"is_streaming":   isStreaming,
			"total_cost_usd": totalCostUSD,
			"latency_ms":     latencyMs,
			"status_code":    statusCode,
			"error_message":  errMsg,
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
// 只有 claude/claude-desktop 的 session:<id> 前缀生成 MessageID；
// opencode/codex/未知 app_type 返回空串（raw log 仍保留，但不参与消息关联）。
func extractMessageIDFromRequestID(appType, requestID string) string {
	if appType != "claude" && appType != "claude-desktop" {
		return ""
	}
	if !strings.HasPrefix(requestID, "session:") {
		return ""
	}
	return strings.TrimPrefix(requestID, "session:")
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
// 用 COALESCE(error_message, ”) 在 SQL 层兜空串
func buildCCSwitchProxyQuery(req RouterCollectRequest) (string, []interface{}) {
	const base = `SELECT COALESCE(request_id,''), COALESCE(session_id,''),
		COALESCE(model,''), COALESCE(provider_id,''), COALESCE(app_type,''), COALESCE(is_streaming,0),
		COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		COALESCE(cache_read_tokens,0), COALESCE(cache_creation_tokens,0),
		COALESCE(total_cost_usd,0), COALESCE(latency_ms,0), COALESCE(status_code,0),
		COALESCE(error_message, ''), COALESCE(created_at,0)
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
