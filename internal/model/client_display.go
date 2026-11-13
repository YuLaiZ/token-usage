// internal/model/client_display.go
package model

// ClientKeyDisplayName 是配置键（cfg.Clients 的 key，如 "claude"）到单一展示名
// 的唯一映射，用于监控来源、错误记录等面向用户的短名（例如「Claude 目录监听
// 失败」）。与 ClientToDisplayNames（数据维度过滤用的多显示名列表）语义不同：
// 这里每个配置键只有一个展示名，未知键原样返回。
//
// 新增客户端（尤其新的 JSONL watcher）时必须在此登记；analyzer 的
// TestJSONLMonitorClientsHaveDisplayNames 会在遗漏时转红。
var clientKeyDisplayNames = map[string]string{
	"claude":    "Claude",
	"codex":     "Codex",
	"workbuddy": "WorkBuddy",
	"autoclaw":  "Zhipu-AutoClaw",
	"dsh":       "DeepSeek Harness",
	"opencode":  "OpenCode",
	"zcode":     "ZCode",
	"mimocode":  "MiMo Code",
}

// ClientKeyDisplayName 返回配置键的展示名；未知键原样返回（不隐藏原始键，
// 便于发现未登记的新客户端）。
func ClientKeyDisplayName(key string) string {
	if name, ok := clientKeyDisplayNames[key]; ok {
		return name
	}
	return key
}
