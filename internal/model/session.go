// internal/model/session.go
package model

const (
	ClientClaudeCode    = "Claude Code"
	ClientClaudeDesktop = "Claude Desktop"
	ClientOpenCode      = "OpenCode"
	ClientCodexCLI      = "Codex CLI"
	ClientCodexApp      = "Codex App"
	ClientWorkBuddy     = "WorkBuddy"
	ClientZCode         = "ZCode"
	ClientZhipuAutoClaw = "Zhipu-AutoClaw"
)

const (
	RawClientClaudeCode    = "claude_code"
	RawClientClaudeDesktop = "claude_desktop"
	RawClientOpenCode      = "opencode"
	RawClientCodexCLI      = "codex_cli"
	RawClientCodexApp      = "codex_app"
	RawClientWorkBuddy     = "workbuddy"
	RawClientZCode         = "zcode"
	RawClientZhipuAutoClaw = "zhipu_autoclaw"
)

var RawClientToClient = map[string]string{
	RawClientClaudeCode:    ClientClaudeCode,
	RawClientClaudeDesktop: ClientClaudeDesktop,
	RawClientOpenCode:      ClientOpenCode,
	RawClientCodexCLI:      ClientCodexCLI,
	RawClientCodexApp:      ClientCodexApp,
	RawClientWorkBuddy:     ClientWorkBuddy,
	RawClientZCode:         ClientZCode,
	RawClientZhipuAutoClaw: ClientZhipuAutoClaw,
}

// ClientToDisplayNames 配置 key（cfg.Clients map key，如 "claude"）→ 显示名列表的映射。
//
// 为什么需要这张表：messages.client 字段存的是显示名（经 RawClientToClient 映射后，
// 如 "Claude Code"），而非配置 key。router backfill 等场景需要按 client 查 messages 时，
// 必须传入显示名才能命中。
//
// 为什么不能从 RawClientToClient 自动反推：raw client 名与配置 key 名不同
// （如 "claude_code" vs "claude"），且一对多（claude → Claude Code + Claude Desktop），
// 无法自动建立映射。新增 client 时必须同步更新以下四处：
//  1. ClientXxx 常量（如 ClientTraeCN）
//  2. RawClientXxx 常量（如 RawClientTraeCN）
//  3. RawClientToClient
//  4. ClientToDisplayNames（本表）
var ClientToDisplayNames = map[string][]string{
	"claude":    {ClientClaudeCode, ClientClaudeDesktop},
	"opencode":  {ClientOpenCode},
	"codex":     {ClientCodexCLI, ClientCodexApp},
	"workbuddy": {ClientWorkBuddy},
	"zcode":     {ClientZCode},
	"autoclaw":  {ClientZhipuAutoClaw},
}

type Message struct {
	ID                string
	SessionID         string
	Client            string
	Date              string
	TS                int64
	Model             string
	Provider          string
	RouterProvider    string
	RouterModel       string
	RouterName        string
	Directory         string
	Project           string
	InputTokens       int64
	FreshInputTokens  int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	ReasoningTokens   int64
	TotalTokens       int64
}

// Session 消息账本 V1 最终会话元数据（不含 token 列，token 由 messages 聚合）。
type Session struct {
	ID        string
	Client    string
	Directory string
	Project   string
	Title     string
	ParentID  string
	FirstTS   int64
	LastTS    int64
}
