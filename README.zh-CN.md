# token-usage

> 简体中文 | [English](README.md)

本地 LLM 使用数据统计工具，用于采集、分析和查询各 AI 客户端的 token 使用情况。

## 功能特性

- **消息/API 请求级统计**：每次实际模型调用独立入库，不再按会话聚合 token
- **跨日/多模型准确归因**：同一条消息跨日、同一会话多模型、fork/branch 均不漏计、不重复
- **rewind 实际消耗保留**：rewind 前已完成的调用与 rewind 后的新调用各自保留，不去重
- **Codex rollout 重播去重**：限流桶切换时重播的同一完整 token 快照不会重复计入，合法的重置和多轮调用仍会保留
- **多数据源支持**：Claude Code/Desktop、OpenCode、Codex、WorkBuddy、ZCode、Zhipu-AutoClaw
- **CC Switch 路由归因**：从 CC-Switch 代理日志回填真实 provider/model（当前仅 Claude 系列生效）
- **双运行模式**：CLI 命令（单次执行）+ 守护进程（实时监控，nginx 风格后台启动）
- **开机自启**：macOS launchd / Windows 注册表，config TUI 或 `config set` 一键开关
- **按需启用**：所有客户端默认关闭，开启你实际使用的即可；数据源路径仍有开箱即用默认值
- **单二进制分发**：Go 编译（纯 Go SQLite，无需 CGO），支持 macOS 与 Windows

## 快速开始

### 安装

推荐方式 A（官方 Release 二进制，经官方脚本安装）：无需 Go 环境、无需 sudo 或管理员权限——也是唯一支持原地自更新的安装来源。所有文件统一收纳于 `~/.token-usage`（配置、数据库、日志与二进制 `~/.token-usage/bin`），经用户 PATH 暴露命令。

#### 方式 A：官方 Release 二进制（推荐——支持自更新）

**复制给 AI Agent**（它运行安装脚本并完成验证）：

```text
在本机安装 token-usage CLI：运行与当前平台匹配的官方安装脚本——

- macOS：curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
- Windows PowerShell（两条命令）：
  irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
  powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"

脚本会下载最新官方 Release，按该 Release 的 SHA256SUMS 校验 SHA256，把二进制
安装到 ~/.token-usage/bin 并配置用户 PATH。完成后新开终端运行
token-usage version 确认（运行 token-usage --help 查看可用命令）。
```

或手动执行命令：

macOS：

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

Windows——两条命令：先下载安装脚本，再执行（两行粘贴到同一 PowerShell 窗口依次运行）：

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

脚本自动检测平台、下载最新稳定版官方 Release、按官方 `SHA256SUMS` 校验后无需 sudo/管理员权限安装到 `~/.token-usage/bin`（Windows 为 `%USERPROFILE%\.token-usage\bin`），配置用户 PATH，并处理旧布局遗留副本（macOS 自动删除；Windows 检测到残留时提示手动移除）。新开终端运行 `token-usage version` 确认。指定版本安装（包括 RC），以及 PATH 语义、旧 TLS 环境、非登录 shell 等细节见[安装指南](docs/install.zh-CN.md)。

从官方 Release 安装的二进制可原地自更新：

```bash
token-usage update                  # 更新到最新稳定版
token-usage update --check          # 只检查，不写任何本地文件
token-usage update --version vX.Y.Z # 更新（或检查）指定版本 tag
```

完整标志、退出码与副作用边界见 [CLI 参考](docs/cli.zh-CN.md)。

#### 其他安装方式

- [手动二进制安装（同布局）](docs/install.zh-CN.md#手动二进制安装同布局)：手动下载官方资产并做 SHA256 校验、自行配置 PATH——与方式 A 等价，含自更新。
- [go install / 源码构建](docs/install.zh-CN.md)：需要 Go 环境；产物 `Version = dev` 或伪版本，不能自更新（见 CLI 参考的[信任与来源校验](docs/cli.zh-CN.md#信任与来源校验)）。

卸载或从旧布局迁移见[卸载与迁移](docs/install.zh-CN.md#卸载与迁移)。

### 首次使用

```bash
# 1. 初始化配置文件（写入默认值到 ~/.token-usage/config.toml）与数据库
token-usage config init
#    或直接打开交互式配置 TUI 编辑（不存在则自动初始化）
token-usage config

# 2. 开启你使用的客户端（默认全部关闭），例如
token-usage config set clients.claude.enabled true

# 3. 采集历史全数据（首次必做一次）
#    方式 A（推荐）：自动全量采集，无需手动指定日期范围（已隐含 router 回填）
token-usage collect all
#    方式 B：手动指定日期范围
token-usage collect 20260101-20260721

# 若配置时已开启 daemon.autostart（开机自启），daemon 可能已在后台运行，
# 需先停止再做首次全采（collect 会检测守护进程冲突并拒绝并发写库）：
#   token-usage stop && token-usage collect all && token-usage start

# 若需单独为某客户端回填 router 归因（CC Switch 代理日志），可另跑：
#   token-usage collect router --client claude
# 注意：collect all 已隐含包含 router 回填，通常无需单独执行 collect router

# 4. 之后保持当天数据更新有两种方式：
#    方式 A：启动守护进程，自动实时监控各数据源变化（推荐）
token-usage start
#    方式 B：手动采集当天
token-usage collect
```

> **关于首次历史采集**：`collect all` 全量扫描所有已启用客户端的历史数据（不因 `collection_log` 跳过，按消息主键 upsert），并对配置了 router 的客户端做全量归因回填。
> `collect <日期范围>` 默认按 `collection_log` 去重，只补采缺失日期；加 `--force` 强制重采覆盖。

## 命令速查

直接执行 `token-usage`（不带参数）只打印帮助。完整参数、标志、退出码与示例见 **[CLI 参考](docs/cli.zh-CN.md)**。

### 采集与查询

| 命令 | 作用 |
|------|------|
| `collect [日期]` | 增量采集（今天或指定日期，含 router）；`--client X` 限定客户端，`--force` 强制重采 |
| `collect all` | 两阶段全采：messages 全历史 + router 全量回填（`--client X` 限定单客户端） |
| `collect router --client X` | 仅 router 全量回填（不动 messages；`--client` 必填且须已配置 router） |
| `collect retry` | 重试 `collection_errors` 中未解决失败组（`--client X` 限定） |
| `query [日期]` | 查询统计（默认今日，执行 `query.default` 配置的视图，未配置时等价按客户端分组）；分组视图末行显示 `Total / 总计` |
| `query client/model/provider/project/session/summary [日期]` | 对应内置视图查询 |
| `query <name> [日期]` | 直接按名称执行已配置的子查询或组合查询（根命令位置参数分派，配置名称不会注册为动态子命令）；与下方显式写法目标与输出一致，并遵循同一套校验规则（用法示例各自展示自身命令形态） |
| `query custom <name> [日期]` | `query <name> [日期]` 的等价显式写法；执行 `[query.subqueries]`（一张多维表）或 `[query.groups]`（按顺序多张表）中定义的视图 |
| `query list` | 只读列出已配置查询视图（默认行为、内置命令、自定义子查询、组合查询）；只读取配置，不打开 usage 数据库，也不接受日期 |
| `errors [YYYYMMDD]` | 查看采集异常（`--source X` / `--unresolved`） |

日期为位置参数：`YYYYMMDD` 单日或 `YYYYMMDD-YYYYMMDD` 闭区间；无 `--date` 标志。

每条 query 命令的输出以一个双语统计信息区开始（多表组合也只打印一次）：统计范围、范围内最新消息事件时间（`数据截至`）与最近一次成功采集完成时间（`最近成功采集`）；无数据的项显示 `—`。各字段的确切含义见 **[CLI 参考](docs/cli.zh-CN.md)**。

### 配置

| 命令 | 作用 |
|------|------|
| `config` | 打开交互式配置 TUI（含开机自启开关） |
| `config init` | 初始化配置文件与数据库；输出默认未启用任何客户端，并给出开启示例命令 |
| `config get <key>` | 读取单项配置（dotted key，用户配置层原值，不展开 `~`、不补默认值） |
| `config show` | 输出完整 effective TOML（展开 `~`、补默认值/默认路径，只读、纯 TOML） |
| `config set <key> <value>` | 写入单项配置（原子写盘 + 自启同步 + 动作建议） |

> `config set daemon.autostart` 只同步开机自启定义，**不启停当前 daemon**；要让当前会话生效需手动 `stop` + `start`（或 `restart`）。
>
> `config get` 返回用户配置层原值（不展开 `~`、不补默认值，未显式写的字段返回零值）；要查看运行时生效的完整配置（展开 `~`、补齐核心默认值与默认路径）用 `config show`。

### 版本

| 命令 | 作用 |
|------|------|
| `--version`（或 `-v`） | 单行短输出 `token-usage <version>`；本地开发为 `token-usage dev` |
| `version` | 多行详细输出（version/commit/build_time/go/platform） |

> `version`/`--version` 是纯静态命令：不读配置、不开数据库、不初始化日志、不访问网络。版本与构建元数据由 `internal/buildinfo` 规范化（`make build`/`make build-all`/`make install` 经 `-ldflags` 注入）。

### 自更新

| 命令 | 作用 |
|------|------|
| `update` | 当当前二进制是官方 Release 资产且来源可信时，自更新到最新稳定版；`--check` 只检查，`--version vX.Y.Z[-rc.N]` 指定版本 tag |

> 只有从官方 Release 安装的二进制才能自更新；`make build`/`go install`/软链副本会回退到人工安装指引。标志、退出码、副作用与 Windows 异步替换说明见 [CLI 参考](docs/cli.zh-CN.md)。

### Shell 补全（可选）

`token-usage completion <bash|zsh|fish|powershell>` 将对应 Shell 的补全脚本写到标准输出。例如，当前 zsh 会话可执行：

```bash
source <(token-usage completion zsh)
```

持久化安装方式请查看 `token-usage completion <shell> --help`；完整命令说明见 [CLI 参考](docs/cli.zh-CN.md)。

### 守护进程

| 命令 | 作用 |
|------|------|
| `start` | 后台启动守护进程（完成监听 ready 握手后返回；已在运行则幂等返回 PID） |
| `status` | 查看运行状态 + 启动阶段 + 开机自启漂移检测（只读，5 态分类） |
| `stop` | 停止当前守护进程（不删自启定义；未运行则幂等返回） |
| `restart` | 单次进程控制锁内停旧起新（未运行报错提示用 `start`） |

> `start`/`stop`/`restart`/`status` 均不修改 config、plist 或注册表，只管控当前运行的守护进程。开机自启定义由 `config set daemon.autostart` 或 TUI 触发收敛。

## 典型场景

### 首次初始化

```bash
token-usage config init
token-usage collect all
token-usage start
```

### 新增普通客户端

```bash
token-usage config set clients.zcode.enabled true
token-usage collect all --client zcode
```

这里的「新增」是把已有 collector 类型补进用户配置或从 disabled 改为 enabled。未知 client 名会在 `config set` 时拒绝；新增一种 collector 不是纯配置动作。无 router 时命令正常完成，不执行 router backfill。

### 新增带 router 的客户端

```bash
token-usage config set clients.claude.enabled true
token-usage config set clients.claude.router cc_switch
token-usage config set routers.cc_switch.db_path ~/.cc-switch/cc-switch.db
token-usage collect all --client claude
```

`collect all --client claude` 一条命令完成 messages 全扫和 router 全量回填。

如果执行 collect 前 daemon 正在运行，`config set`/TUI 的动作建议会合并给出：

```bash
token-usage stop
token-usage collect all --client claude
token-usage start
```

`start` 建立监听后还会执行 startup catch-up，补齐最后一次手工 collect 到监听 ready 之间的新增数据。

### 已有客户端后来增加 router

```bash
token-usage collect router --client claude
```

只补 router，不重采 client messages。

### 修复失败组

```bash
token-usage errors
token-usage collect retry
token-usage collect retry --client codex
```

### 临时停止但保留下次自启

```bash
token-usage stop
```

`stop` 不删除 plist/注册表。当前会话保持停止，下次登录仍按 autostart 配置启动。

### 重载配置

```bash
token-usage config set daemon.poll_interval 60
token-usage restart
```

如果 daemon 未运行，`restart` 报错并提示：

```bash
token-usage start
```

### 开启自启但不立即启动

```bash
token-usage config set daemon.autostart true
# 自启定义立即保存，当前进程不变
token-usage start  # 如需当前立即运行
```

### 关闭自启但保持当前运行

```bash
token-usage config set daemon.autostart false
# 当前 daemon 继续运行，下次登录不再自启
```

## 守护进程生命周期

守护进程分为**运行态**与**自启态**两层，彻底解耦：

- **运行态**（`start`/`stop`/`restart`/`status`）：管控当前运行的守护进程。daemon lock（`<data_dir>/token-usage.lock`）是存活的**唯一真相源**；PID/runtime-state 文件是可降级的定位/状态元数据。
- **自启态**（`config set daemon.autostart` / TUI）：只同步开机自启服务定义（macOS plist / Windows 注册表 Run 键），**绝不启停当前守护进程**。

因此：

- `stop` 当前会话停止，但下次登录仍按 autostart 配置启动（定义未删）。
- `config set daemon.autostart false` 当前 daemon 继续运行，但下次登录不再自启。
- 要让 autostart 改动在当前会话生效，需手动 `stop` + `start`（或 `restart`）。

`start` 启动后会执行 **startup catch-up**：在监听就绪后补采「最后一次手工 collect」到「监听 ready」之间的增量数据，关闭 stop→collect→start 的数据窗口。catch-up 部分失败会在 `status` 与 `errors` 中体现。

更详细的进程控制模型（control lock / daemon lock / 父子 lease / PID+runtime-state / startup catch-up 顺序契约）见 [架构设计](docs/architecture.zh-CN.md)，命令级细节见 [CLI 参考](docs/cli.zh-CN.md)。

## 配置

配置文件路径：`~/.token-usage/config.toml`，TOML 格式，可手工添加注释。所有客户端默认关闭：用 `clients.<name>.enabled = true` 开启你使用的客户端，路径由程序按各工具默认位置自动填充。

> `config set` 和 TUI 保存会完整重写用户配置文件，因此不会保留既有注释和 map 键书写顺序；需要保留手写说明时请先备份。

查看配置有两种只读方式，职责不同：

- `config get <key>`：读取**用户配置层原值**（dotted key），即配置文件中显式写入的值，不展开 `~`、不补默认路径、不 clamp 数值；未显式配置的字段返回零值。
- `config show`：输出完整 **effective TOML**（展开 `~`、补 `data_dir`/`daemon`/`log` 核心默认值与 client/router registry 默认路径后的运行时生效配置），纯 TOML 无前缀，便于脚本解析与重定向。只读，不创建 config/DB/日志、不抢进程锁。

> `config show` 输出含本机路径：`~` 会展开；显式相对路径及其派生的默认路径（如 `data_dir` 派生的 `log.dir`、`state_dir` 派生的 `sessions_dir`）保持相对；其余 home-based 默认路径为绝对路径。对外分享前请检查是否含敏感信息。其输出也不是建议覆盖回用户配置文件的模板（回写会冻结默认路径并丢失注释）。

下方示例展示的是客户端已启用状态；`config init` 生成的默认模板中所有客户端 `enabled = false`（`router` 行与 provider 别名均为注释示例）。

```toml
# 数据目录（数据库、日志、PID、锁存放位置）
data_dir = "~/.token-usage"

[clients.claude]
enabled = true
router = "cc_switch"          # 路由归因（当前仅 Claude 系列生效）

[clients.opencode]
enabled = true

[clients.codex]
enabled = true
# paths.state_dir = "~/.codex"  # dotted key 覆盖默认示例（不写即用默认）

[clients.workbuddy]
enabled = true

[clients.zcode]
enabled = true

[clients.autoclaw]
enabled = true

# 路由中间件（表名即实现类型）
[routers.cc_switch]
# db_path 不写即用默认 ~/.cc-switch/cc-switch.db

# CC Switch 路由归因的 provider 显示名映射（原始名 = 显示名）
[provider_aliases]
"Zhipu AI Coding Plan" = "Zhipu GLM"

# 自定义查询视图（可选；可用 TUI「查询视图」页引导配置，也可手工编辑）
[query]
default = "group_q"                    # 裸 query 的执行对象，未配置时等价按客户端分组
[query.subqueries]
mpc = "model,provider,client"          # 一张多维表，顺序即列顺序（至少 2 个内置维度）
[query.groups]
group_q = "client,model,provider,mpc"  # 按声明顺序连续输出多张表（至少 2 项，不能嵌套）

[daemon]
poll_interval = 30            # SQLitePoller 轮询间隔（秒）
autostart = false             # 开机自启（macOS launchd / Windows 注册表）
                              # 首次使用请先 token-usage collect all 初始化历史数据，再开启

[log]
level = "info"
dir = "~/.token-usage/logs"
max_days = 7
```

> **路由归因现状**：当前只有 Claude（Code/Desktop）配置 `router = "cc_switch"` 会做消息级归因回填。配置入口会拒绝其他客户端（OpenCode/Codex/WorkBuddy/ZCode/AutoClaw）设置非空 `router`：`config set clients.X.router` 直接报错，TUI 也不提供该字段且保存校验拒绝；存量配置中已存在的值读取不受影响，其原始日志仍会写入 `raw_router_logs` 但不会回填 `messages`，因为 CC Switch 的 `app_type` 只识别 Claude 系列。
>
> **供应商别名**：`provider_aliases` 只影响 `query provider` 的供应商标签与分组；key 必须与采集或路由回填得到的原始 provider 名完全一致。修改不会改写 `usage.db`，也无需采集或回填；下一次查询立即生效。历史空值保持“未归因”。
>
> **查询视图**：`[query]` 是纯展示配置。视图名须为小写标识符，且不能与 `client`/`model`/`provider`/`project`/`session`/`summary`/`custom`/`list` 冲突；若历史手写的子查询或组合查询名为 `list`，升级前请先重命名（新版二进制把该名称保留给静态的 `query list` 发现子命令）。已配置视图可直接 `query <name> [日期]` 执行，也可显式 `query custom <name> [日期]`——两者等价，直接名称是位置参数分派而非动态子命令。语义错误（断开引用、CSV 写错、未知键、`[query]` 与 `[Query]` 并存等顶层冲突）只会让默认路径（裸 `query` 与 `query <日期>`）、具名调用（`query <name>` / `query custom <name>`）、`query list` 与 TUI 保存失败并定位到具体配置键；六个静态内置视图以及 `collect`、`status`、`start`、守护进程、`config set`、`config show` 不受影响，且原样保留问题项。TUI 提供「查询视图」引导页（主菜单按 `v`）完成子查询、组合查询与默认视图的选择式配置，无需手写 CSV；raw 段无法解析时先进入恢复列表，逐项按 `enter` 修复。query 配置出错不会阻塞无关单项修改，可继续用 `config set`；`config set`/`config get` 不支持通过 dotted key 直接读写 query 定义（`config set` 仍会整体重写配置文件并原样保留 raw query 段）。降级到不支持查询视图的旧版本前，请删除整个 `[query]`、`[query.subqueries]`、`[query.groups]` 段：旧版本会拒绝任何非空 query 段（空 `[query]` 段可放行）。

## 平台支持

| 平台 | 编译 | 守护进程 | 开机自启 |
|------|------|----------|----------|
| macOS | ✅ | ✅ | ✅ launchd |
| Windows | ✅ | ✅ | ✅ 注册表 Run 键 |

## 开发

```bash
# 编译当前平台
make build

# 交叉编译（darwin arm64/amd64 + windows amd64）
make build-all

# 运行测试
go test ./...

# 竞争检测
go test -race ./...
```

`make build`/`make build-all`/`make install` 经 `-ldflags -X` 注入 `internal/buildinfo` 包的 `Version`/`Commit`/`BuildTime`（默认 `VERSION=dev`），供 `--version`/`version` 子命令展示。直接 `go build` 未注入时 version 为 `dev`、build_time 为 `unknown`，commit 会在 Go 写入 VCS 信息时回退为 revision；直接 `go run` 使用临时缓存可执行文件，可能没有 VCS 信息而显示 `commit: unknown`，不适合作为构建元数据验证方式。

安装细节见 [docs/install.zh-CN.md](docs/install.zh-CN.md)，详细架构设计见 [docs/architecture.zh-CN.md](docs/architecture.zh-CN.md)，CLI 命令参考见 [docs/cli.zh-CN.md](docs/cli.zh-CN.md)。

## 贡献

欢迎提交 Issue 和 Pull Request。提交 PR 前请阅读[贡献指南](CONTRIBUTING.zh-CN.md)，并确保：

1. 确保相关模块测试通过：`go test ./...`
2. 提交信息用英文一句话描述，不加 `feat`/`fix` 等前缀
3. 一个 PR 聚焦一个改动主题

## 许可证

本项目基于 [MIT License](LICENSE) 发布。
