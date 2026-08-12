# CLI 参考

> 简体中文 | [English](cli.md)

本文件是 `token-usage` 命令行界面的权威参考：命令树、位置参数、标志、退出码与示例。行为描述以源码实现为准。

## 命令树

```text
token-usage
├── version                               # 查看版本与构建信息（详细五行输出）
├── completion [bash|zsh|fish|powershell] # 生成 Shell 补全脚本
├── collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]  # 今天或指定日期增量采集（含 router）
│   ├── all                               # 两阶段全采：messages 全历史 + router 全量回填
│   ├── router --client X                 # 仅 router 全量回填（不动 messages）
│   └── retry                             # 重试 collection_errors 中未解决失败组
├── query [YYYYMMDD|YYYYMMDD-YYYYMMDD]
│   ├── client [YYYYMMDD|YYYYMMDD-YYYYMMDD]  # 按客户端分组（默认视图）
│   ├── model [YYYYMMDD|YYYYMMDD-YYYYMMDD]   # 按模型分组
│   ├── project [YYYYMMDD|YYYYMMDD-YYYYMMDD] # 按项目分组
│   ├── sessions [YYYYMMDD|YYYYMMDD-YYYYMMDD]# 会话明细
│   └── summary [YYYYMMDD|YYYYMMDD-YYYYMMDD] # 总览摘要
├── errors [YYYYMMDD]
├── config                                # 无参数：打开交互式配置 TUI
│   ├── show                              # 输出完整 effective TOML（只读、纯 TOML）
│   ├── get <key>
│   ├── set <key> <value>
│   └── init
├── start
├── restart
├── status
├── stop
├── update                                # 从官方 GitHub Release 自更新（--check / --version）
└── _run                                  # Hidden，由 start/launchd/注册表拉起，勿直接调用
```

设计要点：

- 无顶层 `router` 子命令；路由归因通过 `collect all`（隐含）或 `collect router`（仅归因层）触达。
- 日期是**位置参数**（`YYYYMMDD` 单日或 `YYYYMMDD-YYYYMMDD` 闭区间），无 `--date` 标志；`errors` 只接受单个 `YYYYMMDD`。
- `query` 没有 `--format`/`--by-*` 标志；视图由子命令选择，输出固定为表格。
- 直接执行 `token-usage`（不带任何参数）只打印帮助，既不启动 TUI 也不启动守护进程。
- 根命令带 `-v, --version` flag（单行短输出），同时提供 `version` 子命令（多行详细输出）；二者详见下文「version」。
- `completion` 是 Cobra 提供的内置命令，只向标准输出生成 bash/zsh/fish/PowerShell 补全脚本，不读取配置或数据库。
- `update` 是顶层自更新命令（标志 `--check` 与 `--version`）；它是唯一会改写当前运行二进制的命令，且仅当当前二进制是官方 Release 资产时才执行。详见下文「[update](#update)」。

## 通用约定

### 日期参数格式

| 命令 | 接受形式 | 缺省 |
|------|----------|------|
| `collect` / `query` 及其子命令 | `YYYYMMDD` 或 `YYYYMMDD-YYYYMMDD`（闭区间，含两端） | 今天 |
| `errors` | `YYYYMMDD` 单日（不接受区间） | 无日期且无 `--source` 时只看未解决 |

`YYYYMMDD` 为 8 位紧凑格式（如 `20260701`）。`YYYY-MM-DD`、多余位置参数、区间端点不足 8 位、结束早于开始均报错并给出命令示例。区间展开为逐日列表（含两端）。

### 退出码

`token-usage` 在 `main` 中把命令返回的 error 映射为退出码：

- `0`：成功（含幂等结果，如 start 时守护进程已在运行、stop 时未运行）。
- `1`：任意 error（参数校验失败、采集/查询失败、守护进程控制失败、revision 冲突、部分失败等）。

成功与失败的标准输出/标准错误合同见各命令章节。

### 标志作用域

- `--client`：`collect` 的 **PersistentFlag**，被 `all`/`router`/`retry` 三个子命令继承。
- `--force`：`collect` 的 **LocalFlag**，子命令**不继承**（子命令传 `--force` 会报 unknown flag）。
- `errors` 的 `--source`/`--unresolved`：`errors` 的 LocalFlag。
- 根命令的 `-v, --version`：根级 flag，输出单行短版本。

## version

查看版本与构建信息。由 `internal/buildinfo` 包统一规范化版本与构建元数据，`--version`/`version` 子命令共享同一份 `buildinfo.Info` 快照。

```text
token-usage --version        # 等价于 -v，单行短输出
token-usage version          # 多行详细输出
```

| 形式 | 输出 |
|------|------|
| `--version`（`-v`） | 单行 `token-usage <version>\n`，如 `token-usage v0.1.0`；本地开发为 `token-usage dev` |
| `version` | 严格五行详细输出（末尾换行）：`token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go版本>` / `platform: <os>/<arch>` |

详细输出示例（release 构建）：

```text
token-usage v0.1.0
commit: 59a8d55a1b2c
build_time: 2026-07-30T10:00:00Z
go: go1.26.4
platform: darwin/arm64
```

- `commit` 展示完整 revision 的前 12 位；工作树有修改时（`vcs.modified=true`）追加 `-dirty`。
- **版本来源优先级**：① Makefile `ldflags -X` 注入的 `Version` → ② `go install @version` 时 `debug.ReadBuildInfo().Main.Version` → ③ 本地默认 `dev`。
- **commit 来源**：① 注入的 `Commit` → ② `debug.BuildInfo` 的 `vcs.revision` → ③ `unknown`。**build_time 不使用 `vcs.time`**（那是 commit 时间，非构建时间），仅取注入值，未注入为 `unknown`。
- 直接 `go build` 未注入 ldflags 时：version 为 `dev`，build_time 为 `unknown`，commit 会在 Go 写入 VCS 信息时回退为 revision。直接 `go run` 的临时缓存可执行文件可能不带 VCS 信息，此时 commit 为 `unknown`；需验证完整构建元数据时使用 Makefile target。
- 纯静态命令：不读配置、不开数据库、不初始化日志、不访问网络。
- 根 `--help` 同时展示 `version` 子命令与可见的 `-v, --version` flag。

示例：

```bash
token-usage --version        # 单行短输出
token-usage -v               # 同上
token-usage version          # 多行详细输出
```

## completion

生成 Shell 补全脚本。脚本写到标准输出，可按所用 Shell 的安装方式保存或加载。

```text
token-usage completion <bash|zsh|fish|powershell>
```

例如在当前 zsh 会话加载：

```bash
source <(token-usage completion zsh)
```

各 Shell 的持久化安装说明由 `token-usage completion <shell> --help` 提供。该命令不读取配置、数据库或数据源。

## collect

采集 token 使用数据。`collect` 及其子命令在打开数据库前都会做**守护进程冲突预检**：若守护进程正在运行（持有 daemon lock），直接拒绝采集，避免并发写库。

```text
token-usage collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]
token-usage collect all
token-usage collect router --client <name>
token-usage collect retry
```

| 形式 | 作用 | 继承标志 |
|------|------|----------|
| `collect [日期]` | 对所有已启用客户端做一次增量采集（今天或指定日期），过程中同步读取 router 日志并回填归因 | `--client`、`--force` |
| `collect all` | 两阶段全采：阶段 A 逐个 client 全历史 messages 采集（单 client 失败不阻断其他）；阶段 B 对配置了 router 的 client 全量回填归因 | `--client` |
| `collect router --client <name>` | 仅 router 全量回填，不调用 client collector、不写 `collection_log`/`collection_errors`、不推进 cursor | `--client`（**必填**） |
| `collect retry` | 重试 `collection_errors` 中未解决记录，按 (date, source) 分组逐组重采 | `--client` |

标志：

- `--client <name>`：限定单客户端（必须是配置中存在且 `enabled=true` 的；不存在报「未知客户端」，已禁用报「已禁用」）。有效值即配置中的 client 段名（`claude`/`opencode`/`codex`/`workbuddy`/`zcode`/`autoclaw`）。
- `--force`（仅 `collect [日期]` 本身）：强制重新采集，忽略 `collection_log` 去重。子命令不接受此标志。

要点：

- `collect all` 已隐含包含 router 回填，通常无需再单独执行 `collect router`。
- `collect router` 的 `--client` 必须已配置 `router`（`clients.<name>.router` 非空），否则报「未配置 router」。
- `collect [日期]` 无日期时只采今天；要全量历史请用 `collect all`。
- 预检发现守护进程运行中时返回「守护进程正在运行，数据由守护进程维护」并退出非零。
- 采集失败按客户端/阶段汇总；任一失败则非零退出。部分源失败时已成功解析的数据仍落库，但不写 `collection_log`、不解决旧错误、不推进增量 cursor，后续普通采集或 retry 会幂等重放。

示例：

```bash
# 采集今天（所有已启用客户端，含 router）
token-usage collect

# 采集指定日期范围
token-usage collect 20260701-20260721

# 全量历史采集（含 router 阶段）
token-usage collect all
token-usage collect all --client claude

# 仅回填某客户端的 router 归因
token-usage collect router --client claude

# 重试失败组
token-usage collect retry
token-usage collect retry --client codex
```

## query

查询 token 使用统计，输出固定为表格（无 `--format`）。从 `messages` 实时聚合，不依赖物化汇总表。

```text
token-usage query [日期]
token-usage query client [日期]     # 默认视图，裸 query 与此等价
token-usage query model [日期]
token-usage query project [日期]
token-usage query sessions [日期]
token-usage query summary [日期]
```

缺省日期为今天。若查询的日期区间在 `collection_errors` 中存在未解决记录，结果末尾会附「采集异常」提示并列出条目，建议用 `errors` 查看详情、`collect retry` 重试。

示例：

```bash
token-usage query                    # 今日，按客户端分组
token-usage query model              # 今日，按模型分组
token-usage query 20260701-20260721  # 区间，按客户端分组
token-usage query summary 20260701   # 单日总览
```

## errors

查看采集异常。

```text
token-usage errors [YYYYMMDD]
```

- 无日期且无 `--source`：默认只看**未解决**异常。
- 给出日期或 `--source`：默认看**全部状态**（含已解决）。
- `--unresolved`：显式只看未解决，始终生效。

标志：

- `--source <name>`：按数据源过滤（`claude`/`opencode`/`codex`/`workbuddy`/`zcode`/`autoclaw`）。
- `--unresolved`：只看未解决。

示例：

```bash
token-usage errors                     # 未解决异常
token-usage errors 20260721            # 某日全部异常
token-usage errors --source codex      # 某数据源全部异常
token-usage errors --unresolved        # 显式只看未解决
```

## config

配置管理。

```text
token-usage config                     # 打开交互式配置 TUI
token-usage config show                # 输出完整 effective TOML（只读、纯 TOML）
token-usage config get <key>           # 读取单项配置（dotted key，用户配置层原值）
token-usage config set <key> <value>   # 写入单项配置
token-usage config init                # 初始化配置文件与数据库
```

> `config get` 与 `config show` 职责不同：前者读用户配置层原值（不展开 `~`、不补默认值），后者输出完整 effective TOML（展开 `~`、补默认值/默认路径）。要查看运行时生效配置请优先用 `config show`；`status`/TUI 只作为人机可读摘要。

### config（TUI）

无参数时打开交互式配置 TUI（`bubbletea`）。配置文件不存在时先写默认模板再打开；可编辑客户端、路由、守护进程、日志和 provider aliases，`data_dir` 在 TUI 中只读。保存统一走 `ApplyConfig`（见下文「config set」）。

### config show

输出完整 **effective 配置**（只读、纯 TOML）。

```text
token-usage config show
```

- **effective**：展开 `~` 前缀、补齐 `data_dir`/`daemon`/`log` 核心默认值与 client/router registry 默认路径后的运行时生效值，即与守护进程实际使用一致的解析结果。
- **纯 TOML**：stdout 首字符即 TOML 内容，无标题/提示/warning 前缀，可直接管道给 TOML 解析器或重定向到文件，便于脚本消费。
- **只读、零运行时副作用**：不修改磁盘上的用户配置文件，不创建 config/DB/日志/daemon 元数据，不抢进程锁，不同步自启。
- **单一解析链路**：复用 `cli.loadConfig()` → `runtimecfg.LoadEffectiveConfig`，不复制默认值逻辑。
- 配置缺失/为空/损坏/校验失败时返回明确 error 与非零退出码。
- **路径隐私**：输出含本机路径（`~` 会展开；显式相对路径及其派生的默认路径保持相对，如 `data_dir` 派生 `log.dir`、`state_dir` 派生 `sessions_dir`；其余 home-based 默认路径为绝对路径），对外分享前请检查是否含敏感信息。
- **不可直接覆盖写回**：输出并非建议覆盖回用户配置文件的模板——它含补全后的默认值，回写会冻结默认路径并丢失注释。

### config get

读取单项配置（dotted key，如 `daemon.poll_interval`、`clients.claude.enabled`）。

读取的是**用户配置层原值**：即配置文件中显式写入的值，不展开 `~`、不补默认路径、不 clamp 数值。因此未在文件中显式配置的字段返回零值（如未写的 `poll_interval` 返回 `0`）。要查看运行时实际生效值（展开 `~`、补默认值/默认路径的完整配置）请用 `config show`；`status` 与 TUI 仅作人机可读摘要。

### config set

写入单项配置（dotted key，脚本友好）。写入由 `configapp.ApplyConfig` 在**进程控制锁内原子完成**。

```
token-usage config set <key> <value>
token-usage config set <key> <value> --confirm-migrate   # 仅迁移 data_dir 时
```

**输出合同（便于脚本解析）：**

- 成功稳定行 `✓ <key> = <value>` 写 **stdout**。
- 动作建议（restart / collect）、说明与 warning 写 **stderr**。
- 退出码：成功 `0`，任意失败 `1`。

**revision 冲突保护：** 命令开始读取的配置 revision 与锁内重读的磁盘 revision 必须一致；不一致判定「配置已被其他进程修改，本次未写入」，stdout 不写成功行、退出非零。**冲突后直接重新执行命令即可**——会自动重新读取最新配置并重算 revision，无需手动干预。

**部分失败：** 配置已落盘但自启同步或残留清理失败时，stdout 仍写稳定成功行，stderr 写具体失败并退出非零（已落盘结果不会被描述为完全失败）。

**完整重写：** 实际发生配置变更时，`config set` 与 TUI 都会序列化完整用户配置文件；原有注释和 map 键书写顺序不会保留。需要保留手写说明时请先备份。

**data_dir 迁移：** 修改 `data_dir` 需 `--confirm-migrate` 确认；且要求旧 daemon **已停止**（运行中拒绝，写入前校验），迁移需手动搬运 `usage.db`/`logs`，PID/lock/runtime-state 不迁移（按 stale 协议清理）。

### 支持的 dotted key

| 范围 | 可写 key |
|------|----------|
| 数据目录 | `data_dir`（需 `--confirm-migrate`） |
| 守护进程 | `daemon.poll_interval`、`daemon.autostart` |
| 日志 | `log.level`、`log.dir`、`log.max_days` |
| 客户端 | `clients.<name>.enabled`、`clients.<name>.router`、`clients.<name>.paths.<path-key>` |
| 路由 | `routers.cc_switch.db_path` |
| Provider 别名 | `provider_aliases.<原始 provider 名>` |

受支持 client 为 `claude`、`opencode`、`codex`、`workbuddy`、`zcode`、`autoclaw`。path key 分别为：Claude `projects_dir`；OpenCode `db`；Codex `state_dir`/`sessions_dir`；WorkBuddy `db`/`projects_dir`；ZCode `db`；AutoClaw `sessions_dir`。

`provider_aliases` 用于把 CC Switch 回填的原始 provider 名规范为显示名；变更后按命令提示重新执行 router backfill。名称含 `.` 时使用引号段，例如：

```bash
token-usage config set 'provider_aliases."Zhipu AI Coding Plan"' 'Zhipu GLM'
```

### autostart 的语义边界（重要）

`config set daemon.autostart <bool>`（或 TUI 切换）只**同步开机自启服务定义**（macOS plist / Windows 注册表 Run 键），**绝不启停当前守护进程**：

- 开启 autostart：写自启定义，当前 daemon 状态不变；下次登录/开机按新定义加载。
- 关闭 autostart：删自启定义，当前 daemon 继续运行；下次登录/开机不再自启。

要让当前会话生效，需手动 `stop` + `start`（或 `restart`）。二者彻底解耦的详细说明见「[守护进程生命周期](#守护进程生命周期)」。

### config init

初始化配置文件（固定 `~/.token-usage/config.toml`）与数据库（`<data_dir>/usage.db`）。

- 配置文件：仅在不存在时写入默认模板（幂等，不覆盖已有配置）。
- 数据库：`usage.db` 始终初始化（即便配置已存在）。
- `data_dir` 沿用已有配置中的值；字段未显式配置时使用默认目录 `~/.token-usage`。已有配置无法解析或校验失败时命令报错，不静默覆盖。

示例：

```bash
token-usage config init
token-usage config get daemon.poll_interval
token-usage config set daemon.autostart true
token-usage config set clients.zcode.enabled true
```

## 守护进程生命周期

`start` / `stop` / `restart` / `status` 只管控**当前运行的守护进程**（采集/分析的实时监控进程），与**开机自启定义**（下次登录/开机是否自动启动）彻底解耦。

| 命令 | 作用 | 是否触碰 autostart 定义 |
|------|------|--------------------------|
| `start` | 后台启动守护进程，完成监听 ready 握手后返回；已在运行则幂等返回当前 PID | 否 |
| `stop` | 停止当前守护进程（不删 plist/注册表）；未运行则幂等返回 | 否 |
| `restart` | 在单次进程控制锁内停旧起新；未运行报错并提示用 `start` | 否 |
| `status` | 只读查看运行状态 + 开机自启漂移检测（5 态分类） | 否（只读） |

> 三者均不修改 config、plist 或注册表。autostart 定义由 `config set daemon.autostart` 或 TUI 保存触发收敛。

### start

```text
token-usage start
```

经 `control.Manager.Start`：在进程控制锁内加载配置 → 以 daemon lock 判活 → 已运行返回当前 PID（不重复 spawn，退出码 0）→ 未运行 detached spawn `_run` 子进程 → 在 5 秒内等待六项 ready 条件（PID 文件的 PID/instanceID、daemon lock、runtime-state 的 PID/instanceID/`monitor_ready=true`）→ 输出 `✓ 守护进程已启动（PID N）`。超时会尽力终止本次子进程；仅在 daemon lock 已释放且元数据仍属于本代时清理，避免误删活进程或其他代次的文件。

stdout：成功行（含幂等的「已在运行」）；stderr：真实失败。

### stop

```text
token-usage stop
```

经 `control.Manager.Stop`：进程控制锁内加载配置 → daemon lock 判活 → 未运行幂等返回「守护进程未运行」→ 运行中按平台停止（macOS：始终先尝试对当前 label 执行幂等 `bootout`，若 daemon lock 仍持有再对已读取的准确 PID 发 SIGTERM；Windows：taskkill 精确 PID）→ 以 **daemon lock 释放**为成功条件（轮询 5s），不靠删 PID 文件伪装成功。

stop **不删除** plist/注册表定义：当前会话停止，下次登录仍按 autostart 配置启动。关闭自启请用 `config set daemon.autostart false`。

### restart

```text
token-usage restart
```

经 `control.Manager.Restart`：单次进程控制锁内 stop 旧 + start 新。守护进程**未运行**时返回 `ErrRestartNotRunning`（stderr 含「请使用 token-usage start」），退出非零。

macOS 取舍：stop 会尝试 bootout 当前 job，随后以 detached 方式 start；plist 定义保留，但本次登录会话不再由 launchd KeepAlive 托管。由于保存配置只维护定义文件、不会主动 bootstrap，KeepAlive 会在下次登录加载该定义时恢复。

### status

```text
token-usage status
```

只读（`Inspect` 不抢进程控制锁，仅以 daemon lock 判活），返回一致快照：

- 运行状态：`● 守护进程运行中（PID N）` 或 `○ 守护进程未运行`。
- 启动阶段（运行中时追加一行）：`监听初始化中` / `监听已就绪，正在补采` / `补采部分失败（N），请执行 token-usage errors`；catch-up 成功不额外打印；PID 元数据不可用或阶段不匹配时降级为「启动阶段未知」。
- 数据目录、轮询间隔。
- 开机自启漂移检测（5 态）：已启用 / autostart=开但定义缺失 / 内容不一致 / autostart=关但定义残留 / 未启用。漂移只提示「建议重新保存配置」，不触发任何写操作。

autostart 只表达「下次登录/重启是否自动启动」，与当前 daemon 是否运行相互独立；当前 daemon 状态单独展示，两者不互相推断。

### startup catch-up（关闭 stop→collect→start 数据窗口）

`start` 建立监听后，守护进程会执行一次 **startup catch-up**，补齐「最后一次手工 `collect`/`collect all`」到「监听 ready」之间新增的数据，从而关闭 stop→collect→start 的数据窗口。

顺序契约（`daemon.startupCoordinator`）：

1. 等待 analyzer 所有 monitor 就绪（ready barrier）；ctx 取消则不写 state、不 catch-up。
2. 写 ready state（`monitor_ready=true, catch_up=pending`）。
3. 写 running state（`catch_up=running`）；写入失败时记录日志并继续，不停止 daemon。
4. 顺序 Submit catch-up 请求：按已启用 client 名升序，每个 client 先发 client-source 请求（opencode/zcode 走增量 cursor；claude/workbuddy/autoclaw 无日期扫现存 JSONL；codex 先 state 增量再 rollout 全扫），再发该 client 的 router 增量请求（若配置）。
5. 写 final state：0 失败 = `succeeded`，否则 = `failed` + 准确失败数。

catch-up 经 analyzer 的串行化锁 Submit（与实时触发同一路径，保证顺序与互斥）。因此只要 daemon 成功启动并完成 catch-up，stop→collect→start 之间产生的增量数据会被补采，不会因「监听未就绪」而遗漏。catch-up 部分失败会在 `status` 与 `errors` 中体现。

### _run（Hidden）

内部命令，由 `start` detached spawn、或 launchd / Windows 注册表 Run 键直接拉起，执行守护进程主循环。用户不应直接调用（`--help` 不可见）。两条启动路径都满足不变量「从读取 effective config 到获取 daemon lock 期间始终存在 control lease」：

- 父 lease 路径（`start` spawn 的 `_run`）：父进程持进程控制锁并通过 pipe lease 授权 child，child 不抢锁。
- 独立路径（launchd/注册表直接拉起）：无合法父 lease 时自行获取进程控制锁（15s 超时；超时则成功退出码 0 不进入主循环，避免与正在进行的控制操作冲突，并在 macOS 上避免 launchd KeepAlive 立即重拉）。

## update

从官方 GitHub Release 原地更新 `token-usage` 二进制。CLI 只解析参数、装配依赖、格式化结果，自更新核心位于 `internal/update`（见[架构设计](architecture.zh-CN.md)）。

```text
token-usage update
token-usage update --check
token-usage update --version <tag>
```

| 形式 | 作用 |
|------|------|
| `update` | 更新到最新稳定版。若当前二进制同目录存在一次中断的 POSIX 更新留下的受限事务 journal，先完成恢复；之后仅当目标严格高于当前版本且当前来源可信时才继续新替换：下载资产、与 `SHA256SUMS` 清单比对 SHA256、stage `--version` 二次校验、替换二进制并按原运行态恢复 daemon。 |
| `update --check` | 只读检查；不创建任何本地文件（不创建配置目录/锁/日志/数据库/服务定义）。 |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | 更新（或加 `--check` 后仅检查）指定精确版本 tag。`--version` 接受严格 Release tag（`v` 前缀、`MAJOR.MINOR.PATCH`、可选 `-rc.N`、无前导零）；非法值在任何网络请求前即报错。 |

`--check` 与 `--version` 可组合，如 `update --check --version v0.1.0-rc.1` 只检查候选版。

标志：

- `--check`（bool）：只读检查，不写本地文件。
- `--version`（string）：目标 Release tag。接受 `vMAJOR.MINOR.PATCH` 与 `vMAJOR.MINOR.PATCH-rc.N`（无前导零，`N >= 1`，无 build metadata）。

`update` 不接受位置参数（`Args: NoArgs`）。

### 稳定版 / RC 选择

默认 `update` 只解析最新**稳定版**，绝不选择 prerelease。只有用 `--version` 显式指定 rc tag（如 `--version v0.1.0-rc.1`）时才会查询/安装预发布版。

### 信任与来源校验

`update` 仅在目标严格更高且当前来源可信时才覆盖当前二进制。满足以下任一条件即判定当前来源**不可信**（`update` 输出人工安装指引，不原地覆盖）：

- 当前 `Version` 为 `dev` 或伪版本（如来自 `make build`、`make build-all` 或 `go install`）；
- 当前二进制不是普通文件，或为 symlink；
- 当前二进制的 SHA256 与当前版本的官方资产 hash 不一致。

唯一受信仓库为 `YuLaiZ/token-usage`；下载 URL 重构、清单与分阶段安装信任链见[架构设计](architecture.zh-CN.md)。

该来源安全门约束的是新的二进制替换。恢复已经记录的本地事务不会下载或接受新来源：只使用由当前 executable 与 journal nonce 推导出的同目录路径，并重新校验 journal 中记录的 hash 后恢复一致状态。

### 退出码

- `0`：已完成的预期状态——无稳定 Release、已是最新、发现可更新（`--check`）、Windows 后台替换已排队，或恢复确认上次中断时新二进制已经落地。
- 非 `0`：指定 tag 不存在、当前来源无法安全覆盖、下载/清单/checksum/stage `--version` 校验被拒绝、恢复后回到旧版本、安装尚未完成、安装/回滚/daemon 重启失败，或 `--version` 非法。

### 副作用边界

`update --check` 完全只读。真正 `update` 发现已有事务 journal 时先完成恢复；否则仅在确有更新且来源可信时才停 daemon → 替换二进制 → 重启；无更新时不启停 daemon，也不重写 `config.toml`、数据库、日志、macOS LaunchAgent plist 或 Windows 注册表。

### Windows 异步替换

Windows 上替换运行中的 `.exe` 受限，自更新把替换交给后台 helper 后返回。helper 成功启动后，命令会明确说明「后台替换已排队」，以 `0` 退出，并提示稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本，**不声称已完成**。macOS/POSIX 为同步原子替换（同目录 backup + rename + fsync，失败回滚 + 下一次 `update` 调用按 journal 恢复）。

## 配置文件

路径固定 `~/.token-usage/config.toml`（TOML，可手工添加注释）。开箱即用：client 只需声明 `enabled = true`，数据源路径由程序按各工具默认位置自动填充。用 dotted key 同段写法覆盖默认。`config set`/TUI 保存会完整重写配置，故不保留原有注释和 map 键书写顺序；完整字段与默认值见 `token-usage config init` 生成的模板。

`data_dir` 决定数据文件位置（`usage.db`、日志、PID、runtime-state、锁）；配置文件路径不随 `data_dir` 变化。`daemon.autostart` 控制开机自启（macOS launchd / Windows 注册表）。
