// internal/model/session.go
package model

import "regexp"

const (
	ClientClaudeCode    = "Claude Code"
	ClientClaudeDesktop = "Claude Desktop"
	ClientOpenCode      = "OpenCode"
	ClientCodexCLI      = "Codex CLI"
	ClientCodexApp      = "Codex App"
	ClientWorkBuddy     = "WorkBuddy"
	ClientZCode         = "ZCode"
	ClientZhipuAutoClaw = "Zhipu-AutoClaw"
	// ClientMiMoCode 是 mimocode 数据源中 MiMo Code CLI 的正式 client 名。
	// 除严格 desktop-<hash> 标记外的全部会话（含 0.x CLI 版本、2.1.x 序列、
	// 空值与未知形态）统一归入，见 MiMoSessionClient。
	ClientMiMoCode = "MiMo Code"
	// ClientMiMoDesktop 是 mimocode 数据源中 Xiaomi MiMo Desktop 的正式
	// client 名：仅当 session.version 严格匹配 ^desktop-[0-9a-f]+$ 时归属
	// （MiMoSessionClient 单一判别来源）。
	ClientMiMoDesktop = "MiMo Desktop"
)

// LegacyClientXiaomiMiMoCode 是 v0.1.10 及之前版本写入 messages/sessions 的
// mimocode 落库名。仅用于：db.migrateV4 存量迁移、旧版二进制回滚后继续写旧名
// 的兼容 trigger（BEFORE INSERT 改写为 ClientMiMoCode）、以及 ClientDisplayName
// 对异常残留旧数据的防御性渲染。它不代表任何当前正式 client。
const LegacyClientXiaomiMiMoCode = "Xiaomi MiMo / MiMo Code"

// mimoDesktopVersionRe 是 MiMo Desktop 会话 version 的严格匹配：desktop- 前缀
// + 十六进制 hash。这是当前唯一明确、可作为 MiMo Desktop 正式身份依据的
// 正向标记（Desktop 应用自报 InstallationVersion）；其余形态一律归
// ClientMiMoCode——包括 0.x CLI 语义版本、2.1.x 小版本序列、空值、未知形态
// 与非 hash 的 desktop-* 变体，provider/model 不参与判别。
var mimoDesktopVersionRe = regexp.MustCompile(`^desktop-[0-9a-f]+$`)

// MiMoSessionClient 按 session.version 判定会话归属的正式 client：
// 严格 desktop-<hash> → ClientMiMoDesktop，其余全部 → ClientMiMoCode。
// 未见过的新 version 形态在正式确认前统一归 ClientMiMoCode（默认规则），
// 不得凭推测扩分类；调用方可用日志观察未知形态，但不得改变本归属。
func MiMoSessionClient(version string) string {
	if mimoDesktopVersionRe.MatchString(version) {
		return ClientMiMoDesktop
	}
	return ClientMiMoCode
}

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
	// ~/.local/share/mimocode/mimocode.db）；会话按 session.version 经
	// MiMoSessionClient 判别归属 MiMo Code 或 MiMo Desktop（严格 desktop-<hash>
	// 才归 Desktop，其余默认 MiMo Code）。
	"mimocode": {ClientMiMoCode, ClientMiMoDesktop},
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
