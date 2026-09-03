// internal/model/stats.go
package model

type CollectionLog struct {
	Date         string
	Source       string
	SessionCount int
	CollectedAt  string
}

// FileScanLog 是 startup 跳过门的文件级状态记录（file_scan_log 表，主键 (client, file_path)）。
// 一条记录即「该文件在该快照三元组下已被完整采集（fullyParsed + 快照一致 + 同事务落库）」
// 的凭证；含坏行/尾行未完成/文件级失败的文件不写记录，每次 catch-up 全读。
type FileScanLog struct {
	Client        string
	FilePath      string
	FileIdentity  string // 文件实体标识（dev:ino / 卷序列号:file index）；空 = identity 不可用，不写门
	MtimeNS       int64
	FileSize      int64
	ParserVersion int64
	UpdatedAt     string
}

// FileSnapshot 文件实体证据三元组的单次快照。
// Identity 为文件实体标识的规范化串（Unix "dev:ino"、Windows "卷序列号:file index"）；
// 获取失败或值无效时为空串——调用方必须将其视为「不可推进跳过门」的证据，
// 所有不确定都倒向重读。
type FileSnapshot struct {
	Identity string
	MtimeNS  int64
	Size     int64
}

type CollectionError struct {
	ID         int
	Date       string
	Source     string
	ErrorType  string
	Message    string
	Detail     string
	RetryCount int
	Resolved   bool
	CreatedAt  string
	UpdatedAt  string
}

// RouterLog 路由中间件请求日志（staging 层）
type RouterLog struct {
	RequestID         string
	MessageID         string // 关联键：claude 去 session: 前缀 / codex 去 session:codex:{pid}: 前缀取末段
	RouterName        string
	SessionID         string
	AppType           string
	Model             string
	ProviderID        string
	ProviderName      string
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	CreatedAt         int64
	// DataSource 区分 cc-switch 写入路径：'proxy'（代理实时落库，含路由信息，
	// 参与归因）与 'codex_session'（会话同步落库，无路由价值，排除在归因外）。
	DataSource string
	RawData    string
}

// SyncCursor 增量同步游标，记录上次同步位置
type SyncCursor struct {
	Value int64
	ID    string
}

// RouterAttribution 路由归因结果，将路由日志中的信息关联回单条消息
type RouterAttribution struct {
	Client     string
	MessageID  string
	Provider   string
	Model      string
	RouterName string
	CreatedAt  int64
	RequestID  string
}

// RawClientSession 当前生产路径不使用的 staging 结构（对应 raw_client_sessions 表，未接入 engine，不是 token 真相源）。
type RawClientSession struct {
	SessionID         string
	Client            string
	Directory         string
	Model             string
	Title             string
	CreatedAt         int64
	LastActiveAt      int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	TotalTokens       int64
	RawData           string
	SourceFile        string
	FileMtime         int64
	FileSize          int64
}
