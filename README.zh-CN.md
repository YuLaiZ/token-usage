# token-usage

> 简体中文 | [English](README.md)

本地 LLM 使用数据统计 CLI：采集你正在使用的 AI 客户端 token 数据，以每次模型调用为独立记录保存，并在无需 SQL 的前提下生成可复用报表。

## 核心能力

- **把你真正想要的报表做出来，无需 SQL。** 从 `client`、`model`、`provider`、`project` 组合出具名多维视图，再把内置与自定义视图编排成可复用、按顺序输出的报表组合；设为默认、用 `query list` 查找、按名称运行，并统一选择表格型报表展示的指标列及其顺序。
- 按消息/API 请求统计，准确处理跨日、多模型、分支与 rewind 的归因。
- 支持 Claude Code/Desktop、OpenCode、Codex、WorkBuddy、ZCode 与 Zhipu-AutoClaw。
- 支持 Claude 系列的 CC-Switch router 归因：通过代理日志回填实际 provider/model。
- 可单次执行，也可使用实时后台监控守护进程；支持 macOS launchd 与 Windows 注册表自启。
- 纯 Go 单二进制 CLI，支持 macOS 和 Windows。

## 快速开始

### 1. 安装与更新

官方安装脚本会下载最新稳定 Release，校验其 SHA256 校验和，将二进制安装到 `~/.token-usage/bin`，并配置用户 PATH；不需要 `sudo` 或管理员权限。

**复制给 AI Agent**（它会安装并验证）：

```text
在本机用与当前平台匹配的官方安装脚本安装 token-usage：

- macOS：curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
- Windows PowerShell：
  irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
  powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"

新开终端后运行 `token-usage --help` 查看命令，再运行 `token-usage version` 验证安装。
```

macOS：

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

Windows PowerShell：

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

新开终端后验证：

```bash
token-usage version
```

官方脚本安装或手动下载并通过 SHA256 校验的官方 Release 资产，都支持原地自更新：

```bash
token-usage update                  # 更新到最新稳定 Release
token-usage update --check          # 只检查，不修改本地文件
token-usage update --version vX.Y.Z # 更新指定 Release
```

已重签的官方资产、源码构建产物（`Version = dev`）或通过 `go install` 安装的 Release tag 产物，需要先执行一次 `token-usage update --force`，将其替换为官方 Release 资产；之后即可正常更新。软链接副本和非官方 tag 不能通过这种方式转换。

`update` 成功升级首次跨过携带安装脚本补全自动配置功能的版本时，成功输出会追加一条一次性迁移提示并给出官方安装命令——重跑一次即可自动配置 Tab 补全（zsh 会交互确认）。确切触发条件见 [CLI 参考](docs/cli.zh-CN.md)。

手动安装、指定版本、源码构建、更新信任规则、卸载、迁移与平台差异见[安装指南](docs/install.zh-CN.md)。

### 2. 用 TUI 配置并采集

所有客户端默认关闭。推荐使用引导式配置 TUI：首次运行会自动初始化，可在其中开启实际使用的客户端，也可配置 router、守护进程、日志和查询设置（视图定义、输出列与别名）。

```bash
token-usage config
```

在 TUI 保存配置后，采集已启用客户端的历史数据：

```bash
token-usage collect all
```

`collect all` 会扫描所有已启用客户端；配置了 router 时也会完成归因回填。它不使用 `collection_log` 的日期去重，消息按 `(client, id)` UPSERT，可安全重复执行。要持续更新数据，启动守护进程：

```bash
token-usage start
```

### 3. 查询并创建专属报表

```bash
token-usage query             # 今天的默认报表；未配置时按 client 汇总
token-usage query model       # 按模型汇总
token-usage query 20260701-20260721
```

日期使用位置参数：`YYYYMMDD` 表示单日，`YYYYMM` 表示单月，`YYYY` 表示单年（仅单独使用），区间端点为日或月（如 `202607-202608`）。任何形态展开上限 366 天（一个闰年），更长范围请拆分多次执行。

真正的价值在于围绕你反复关心的问题构建报表：在 TUI 中进入 **查询**（主菜单按 `v`），创建具名多维视图，组合成按顺序输出的报表组合，选择其默认项，并挑选所有表格共用的指标列。也可在 `~/.token-usage/config.toml` 中定义可迁移的视图：

```toml
[query]
default = "daily_stack"

[query.subqueries]
model_provider_client = "model,provider,client"

[query.groups]
daily_stack = "client,model,provider,model_provider_client"

# 一份全局、有序的指标列布局，所有 query 表格共用
# （可选；缺省保持现有七列）。
[query.output]
columns = ["requests", "input", "output", "total", "cache_hit"]
```

```bash
token-usage query model_provider_client
token-usage query daily_stack 20260701-20260721
token-usage query list
```

`query list` 仅读取配置、不打开 usage 数据库，适合安全查看内置和已配置视图。可选的 `[query.output]` 布局决定每张 query 表格显示哪些指标列及其顺序（`cache_create` 可选但默认隐藏；`query summary` 保持完整摘要）。校验规则和完整命令契约见 [CLI 参考](docs/cli.zh-CN.md#可配置查询视图)。

## 命令速查

| 命令 | 用途 |
|---|---|
| `config` / `config init` | 打开配置 TUI / 创建初始配置和数据库。 |
| `config set <key> <value>` | 修改一项配置。 |
| `collect [date]` | 增量采集今天或指定日期范围。 |
| `collect all` | 全量采集历史数据，不使用 `collection_log` 日期去重；可安全重复执行。 |
| `collect retry` | 重试未解决的采集失败。 |
| `query [date]` | 运行默认报表。 |
| `query client/model/provider/project/session/summary [date]` | 运行内置报表。 |
| `query <name> [date]` | 运行已配置视图或报表组合。 |
| `query list` | 不打开 usage 数据库，列出视图。 |
| `errors` | 查看采集失败。 |
| `version` / `--version` | 查看多行详细 / 单行简要的版本信息。 |
| `start` / `status` / `stop` / `restart` | 控制后台守护进程。 |
| `completion <shell>` | 输出 Bash、Zsh、Fish 或 PowerShell 的补全脚本。 |
| `update` | 对官方 Release 资产原地自更新；符合条件的已重签、源码构建或通过 `go install` 安装的二进制，可先用一次 `update --force` 转换。 |

运行 `token-usage --help` 查看命令概览；标志、退出码、副作用边界、配置行为与守护进程生命周期见 [CLI 参考](docs/cli.zh-CN.md)。

## Shell 补全

`completion` 将 Shell 补全脚本写到标准输出。例如，当前 Zsh 会话可直接加载：

```bash
source <(token-usage completion zsh)
```

各 Shell 的前置条件：**zsh** 需先初始化补全系统（`compinit`）——未初始化时加载脚本会报 `compdef: command not found`，请先在 rc 文件中加入 `autoload -U compinit; compinit` 再加载（部分配置有意不启用 `compinit`，改动前先查看 rc 文件）。**bash** 依赖 bash-completion 包（macOS 系统 bash 3.2 不满足，需经 Homebrew 安装 bash 4+ 与 `bash-completion@2`）。**fish** 与 **PowerShell** 无前置条件。

zsh 上 `compinit` 可能报告不安全目录（insecure directories，即 group/其他用户可写的补全目录——老 Homebrew 安装最常见）并询问是否继续。三种处理方式：在提示出现时输入 `y`（每次新开 shell 会再次提示）、用 `chmod go-w <目录>` 一次性修复各目录（Homebrew 官方同样建议的做法）、或改用 `compinit -u` 永久跳过安全检查。

官方安装脚本可自动完成以上配置（zsh 会交互确认），见[安装指南](docs/install.zh-CN.md)。持久化安装请运行 `token-usage completion <bash|zsh|fish|powershell> --help`。

## 文档导航

| 文档 | 内容 |
|---|---|
| [安装指南](docs/install.zh-CN.md) | 全部安装方式、自更新、卸载与迁移、PATH 与平台差异。 |
| [CLI 参考](docs/cli.zh-CN.md) | 命令树、参数、标志、示例、配置和守护进程行为。 |
| [架构说明](docs/architecture.zh-CN.md) | 数据流、存储、进程控制、更新设计与扩展点。 |
| [贡献指南](CONTRIBUTING.zh-CN.md) | 开发环境、测试、文档、提交与 PR 规范。 |

## 平台支持

| 平台 | 构建 | 守护进程 | 自启 |
|---|---|---|---|
| macOS | 支持 | 支持 | launchd |
| Windows | 支持 | 支持 | 注册表 Run key |

## 开发

```bash
make build
go test ./...
go test -race ./...
```

使用 `make build-all` 构建全部支持的 macOS 与 Windows 目标。完整开发流程见[贡献指南](CONTRIBUTING.zh-CN.md)。

## 许可证

本项目以 [MIT License](LICENSE) 发布。
