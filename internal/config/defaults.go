package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

const defaultConfigTemplate = `# token-usage 配置文件
#
# 所有客户端默认关闭（enabled = false）：按需开启后再采集。
# 开启方式：改为 enabled = true，或执行
#   token-usage config set clients.<name>.enabled true
# （可用客户端见下方各 [clients.*] 段；router 归因当前仅支持 claude）。

# 数据目录（数据库、日志存放位置）
data_dir = "~/.token-usage"

# 各客户端配置
[clients.claude]
enabled = false
# router = "cc_switch"  # 可选：路由归因（当前仅 Claude 家族支持），不设置即不使用

[clients.claude.paths]
projects_dir = "~/.claude/projects"

[clients.opencode]
enabled = false

[clients.opencode.paths]
db = "~/.local/share/opencode/opencode.db"

[clients.codex]
enabled = false

[clients.codex.paths]
state_dir = "~/.codex"                  # state_*.sqlite 所在目录
sessions_dir = "~/.codex/sessions"      # rollout JSONL 所在目录

[clients.workbuddy]
enabled = false

[clients.workbuddy.paths]
db = "~/.workbuddy/workbuddy.db"
projects_dir = "~/.workbuddy/projects"

[clients.zcode]
enabled = false

[clients.zcode.paths]
db = "~/.zcode/cli/db/db.sqlite"

[clients.autoclaw]
enabled = false

[clients.autoclaw.paths]
sessions_dir = "~/.openclaw-autoclaw/agents"

# 路由中间件配置（支持多个）
[routers.cc_switch]
db_path = "~/.cc-switch/cc-switch.db"
# 新增路由类型前必须先实现并注册对应代码

# 守护进程配置
[daemon]
poll_interval = 30  # SQLite 轮询间隔（秒），默认 30s
autostart = false   # 开机自启（macOS launchd / Windows 注册表）

# Provider 名称映射（可自定义，按需添加）
[provider_aliases]
# "Zhipu AI Coding Plan" = "Zhipu GLM"

# 日志配置
[log]
level = "info"        # info / debug / warn / error
dir = "~/.token-usage/logs"
max_days = 7          # 保留天数
`

func DefaultConfigTemplate() string {
	return defaultConfigTemplate
}

func WriteDefaultConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create config directory", "创建配置目录失败"), err)
	}

	if err := fileutil.ReplaceCompleteFile(path, []byte(defaultConfigTemplate), 0o644); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to write config file", "写入配置文件失败"), err)
	}

	return nil
}
