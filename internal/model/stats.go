// internal/model/stats.go
package model

type CollectionLog struct {
	Date         string
	Source       string
	SessionCount int
	CollectedAt  string
}

type FileScanLog struct {
	FilePath       string
	SessionID      string
	Client         string
	SourceType     string
	LastModified   int64
	FileSize       int64
	LastLineOffset int64
	ScannedAt      string
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
	MessageID         string // 关联键：去 RequestID 的 session: 前缀，等于 Claude JSONL message.id
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
	RawData           string
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
