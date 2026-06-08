package collector

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

const maxJSONLLineSize = 16 * 1024 * 1024

func projectBase(directory string) string {
	if strings.TrimSpace(directory) == "" {
		return ""
	}
	normalized := strings.ReplaceAll(directory, `\`, "/")
	return filepath.Base(filepath.Clean(filepath.FromSlash(normalized)))
}

type Collector interface {
	Name() string
	SyncSources() []string
	Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error)
}

type RouterAdapter interface {
	Name() string
	Capabilities() RouterCapabilities
	SyncSource() string
	CollectLogs(ctx context.Context, req RouterCollectRequest, logger *slog.Logger) (RouterCollectResult, error)
}

// RouterCapabilities 声明路由中间件能提供的数据维度
// 调用方根据能力决定合并策略：Provider/Model 用于补充，Token 用于一致性校验
type RouterCapabilities struct {
	Provider     bool // 是否能提供准确的 provider 名称（如 CC-Switch 从 providers 表获取）
	Model        bool // 是否能提供路由后的真实 model 名
	InputTokens  bool // 是否能提供 input_tokens（用于与 JSONL 一致性校验）
	OutputTokens bool // 是否能提供 output_tokens
	CacheTokens  bool // 是否能提供 cache_read/cache_create_tokens
}

const (
	CollectSourceClient = "client"
	CollectSourceRouter = "router"

	SyncSourceZCodeModelUsage = "zcode_model_usage"
	SyncSourceOpenCodeMessage = "opencode_message"
	SyncSourceOpenCodeEvent   = "opencode_event"
	SyncSourceCodexState      = "codex_state"
	SyncSourceCCSwitchRouter  = "ccswitch_router"
)

// CollectRequest 统一采集请求，兼容 CLI 按日期、文件级、增量、路由等多种模式
type CollectRequest struct {
	Dates             []string
	ChangedFile       string
	Incremental       bool
	ScanExistingJSONL bool // startup catch-up：递归扫描现存 JSONL，不依赖索引 DB
	Source            string
	Cursors           map[string]model.SyncCursor
}

// CollectResult 采集结果，包含消息、会话与下次增量同步游标
type CollectResult struct {
	Messages    []model.Message
	Sessions    []model.Session
	NextCursors map[string]model.SyncCursor
	// PartialErr 表示部分源文件读取失败，但 Messages/Sessions 中的成功部分仍应事务落库。
	// engine 在落库后记录该错误并令本次请求返回失败，避免“丢弃好数据”和“静默成功”二选一。
	PartialErr error
}

// RouterCollectRequest 路由中间件采集请求
type RouterCollectRequest struct {
	Dates       []string
	Incremental bool
	Cursor      model.SyncCursor
}

// RouterCollectResult 路由中间件采集结果，包含日志与下次增量同步游标
type RouterCollectResult struct {
	Logs       []model.RouterLog
	NextCursor model.SyncCursor
}
