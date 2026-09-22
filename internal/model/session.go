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
	// ClientMiMoCode 是 mimocode 的当前正式 client 名：collector 落库
	// （messages.client / sessions.client 及主键成分）、RawClientToClient 映射
	// 与查询过滤全部使用它。当前数据源（Desktop/CLI 共库）无法区分两个产品，
	// 统一归为 MiMo Code；若未来数据源可可靠区分，可能像 Claude Desktop 一样
	// 另行拆分 MiMo Desktop。
	ClientMiMoCode = "MiMo Code"
)

// LegacyClientXiaomiMiMoCode 是 v0.1.10 及之前版本写入 messages/sessions 的
// mimocode 落库名。仅用于：db.migrateV4 存量迁移、旧版二进制回滚后继续写旧名
// 的兼容 trigger（BEFORE INSERT 改写为 ClientMiMoCode）、以及 ClientDisplayName
// 对异常残留旧数据的防御性渲染。它不代表任何当前正式 client。
const LegacyClientXiaomiMiMoCode = "Xiaomi MiMo / MiMo Code"

// ClientDisplayName 是防御性的历史兼容兜底：migrateV4 已把存量旧名改名、
// trigger 已把旧版二进制回写的旧名改写，正常数据落库即 ClientMiMoCode；
// 此映射仅防御异常残留（如迁移未完成的库被直接查询），不得作为主实现依赖。
func ClientDisplayName(c string) string {
	if c == LegacyClientXiaomiMiMoCode {
		return ClientMiMoCode
	}
	return c
}

const (
	RawClientClaudeCode    = "claude_code"
	RawClientClaudeDesktop = "claude_desktop"
	RawClientOpenCode      = "opencode"
	RawClientCodexCLI      = "codex_cli"
	RawClientCodexApp      = "codex_app"
	RawClientWorkBuddy     = "workbuddy"
	RawClientZCode         = "zcode"
	RawClientZhipuAutoClaw = "zhipu_autoclaw"
	RawClientMimoCode      = "mimocode"
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
	RawClientMimoCode:      ClientMiMoCode,
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
	// mimocode 一个 key 覆盖 Xiaomi MiMo Desktop 与 MiMo Code CLI（两者共用同一
	// ~/.local/share/mimocode/mimocode.db，库内无 Desktop/CLI 标记，统一归为
	// ClientMiMoCode；未来可可靠区分时可能另行拆分 MiMo Desktop）。
	"mimocode": {ClientMiMoCode},
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
