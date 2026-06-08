package cli

import (
	"sort"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// enabledClientNames 返回配置中所有 enabled=true 的客户端名，按字典序排序。
//
// 用于 collect all（无 --client 时）：遍历所有已启用客户端做两阶段全采。
// 字典序排序保证输出顺序稳定（autoclaw → claude → codex → opencode → workbuddy → zcode），
// 与 config.toml 中表的书写顺序一致，便于测试断言。
func enabledClientNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var names []string
	for name, cc := range cfg.Clients {
		if cc.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
