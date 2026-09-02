package collector

import (
	"context"
	"log/slog"
	"os"
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
	Dates       []string
	ChangedFile string
	Incremental bool
	// ScanExistingJSONL 是 daemon startup catch-up 现存 JSONL 全扫的显式合同：
	// 递归扫描源目录现存 JSONL，不依赖索引 DB。唯一置 true 的生产点是 daemon
	// catch-up 请求（claude/workbuddy/autoclaw/codex rollout）；CLI collect / retry /
	// collect all / ChangedFile / SQLite poller 等其余入口一律不带。claude/workbuddy/
	// autoclaw collector 自身不读该标志（行为仍由 ChangedFile/Dates 决定）。
	ScanExistingJSONL bool
	Source            string
	Cursors           map[string]model.SyncCursor
	// SkipGate 是 startup catch-up 路径注入的跳过门判定回调：返回 true 表示该文件
	// 的门记录与读前快照完全一致（文件未变、上次完整采集），collector 须跳过读取
	// （不 Open 内容、不解析、不产出消息）。nil = 无门（所有非 catch-up 入口的
	// 现状行为，恒全读）。判定为纯函数：输入门记录预取快照与读前文件证据快照。
	SkipGate FileSkipGate
}

// FileSkipGate 判定文件是否可跳过（startup 跳过门命中回调）。
type FileSkipGate func(path string, before model.FileSnapshot) bool

// skipGateHit 报告文件是否被门命中（无门时恒 false；命中时调用方记录 Skipped
// 状态并跳过该文件的全部读取）。
func skipGateHit(gate FileSkipGate, path string, before model.FileSnapshot) bool {
	return gate != nil && gate(path, before)
}

// CollectResult 采集结果，包含消息、会话与下次增量同步游标
type CollectResult struct {
	Messages    []model.Message
	Sessions    []model.Session
	NextCursors map[string]model.SyncCursor
	// PartialErr 表示部分源文件读取失败，但 Messages/Sessions 中的成功部分仍应事务落库。
	// engine 在落库后记录该错误并令本次请求返回失败，避免“丢弃好数据”和“静默成功”二选一。
	PartialErr error
	// FileStatuses 逐文件采集状态（JSONL 类 collector 填充），供 startup 跳过门
	// 判定「该文件本次采集是否完整、文件证据是否稳定」。SQLite 型 collector 不填充。
	FileStatuses []FileScanStatus
}

// FileScanStatus 单文件采集状态：startup 跳过门推进判定的逐文件证据。
// collector 填充 Path/BadLines/FirstBad*/TrailingNewline/Err（parse 层证据），
// Before/After 快照由 Collect 层在读前读后各取一次（fsident）。
type FileScanStatus struct {
	Path string
	// Skipped 表示该文件被注入的跳过门命中（未读内容、未解析、未产出消息）。
	// 此时 Before 为命中判定所用的读前快照，After/BadLines/TrailingNewline 无意义。
	Skipped bool
	// Before/After 读前与读后文件证据快照；两者一致（且 Identity 有效）才允许推进门。
	Before model.FileSnapshot
	After  model.FileSnapshot
	// BadLines 行级数据异常计数：本应是合法记录、因字段异常/解析失败被丢弃的行
	// （Unmarshal 失败、content 未知形态、timestamp 非法等）。结构性过滤
	// （非 assistant、无 usage、user 字符串形态等设计内不产出的行）不计入。
	BadLines     int
	FirstBadLine int
	FirstBadErr  error
	// TrailingNewline 文件最后一行是否以 \n 终结；false 表示尾行可能仍在写，
	// 即使恰好可解析也不得推进门（下次整读自愈）。
	TrailingNewline bool
	// Err 文件级失败（打开/读取终止性错误）：该文件本次无完整产出，不推进门。
	Err error
}

// FullyParsed 报告该文件本次采集产出是否完整：无文件级错误、无坏行且尾行
// 以换行终结。被门跳过的文件本次没有读取，不视为 fully parsed。
func (s FileScanStatus) FullyParsed() bool {
	return !s.Skipped && s.Err == nil && s.BadLines == 0 && s.TrailingNewline
}

// parseFileOutcome 是 parse 层单文件行级解析结果的内部载体（FileScanStatus 的
// 解析层子集，供 parse 函数与 Collect 层组装之间传递）。
type parseFileOutcome struct {
	badLines        int
	firstBadLine    int
	firstBadErr     error
	trailingNewline bool
}

// addBad 记录一行数据异常（保持首个坏行的行号与错误作定位线索）。
func (o *parseFileOutcome) addBad(line int, err error) {
	if o.badLines == 0 {
		o.firstBadLine = line
		o.firstBadErr = err
	}
	o.badLines++
}

// fillStatus 把解析层结果写入 FileScanStatus 的对应字段。
func (o parseFileOutcome) fillStatus(st *FileScanStatus) {
	st.BadLines = o.badLines
	st.FirstBadLine = o.firstBadLine
	st.FirstBadErr = o.firstBadErr
	st.TrailingNewline = o.trailingNewline
}

// tailHasNewline 报告 size 字节的文件最后一个字节是否为 '\n'。
// size<=0 视为 true（空文件没有未完成的尾行）；读取失败按 false 处理
// （不确定一律倒向「不推进门」）。调用方须在 scanner 读完之后调用。
func tailHasNewline(f *os.File, size int64) bool {
	if size <= 0 {
		return true
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return false
	}
	return b[0] == '\n'
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
