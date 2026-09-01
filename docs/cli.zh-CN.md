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
│   ├── provider [YYYYMMDD|YYYYMMDD-YYYYMMDD]# 按供应商分组
│   ├── project [YYYYMMDD|YYYYMMDD-YYYYMMDD] # 按项目分组
│   ├── session [YYYYMMDD|YYYYMMDD-YYYYMMDD]# 会话明细
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
├── update                                # 从官方 GitHub Release 自更新（--check / --version / --force）
└── _run                                  # Hidden，由 start/launchd/注册表拉起，勿直接调用
```

设计要点：

- 无顶层 `router` 子命令；路由归因通过 `collect all`（隐含）或 `collect router`（仅归因层）触达。
- 日期是**位置参数**（`YYYYMMDD` 单日或 `YYYYMMDD-YYYYMMDD` 闭区间），无 `--date` 标志；`errors` 只接受单个 `YYYYMMDD`。
- `query` 没有 `--format`/`--by-*` 标志；视图由子命令选择，输出固定为表格。
- 直接执行 `token-usage`（不带任何参数）只打印帮助，既不启动 TUI 也不启动守护进程。
- 根命令带 `-v, --version` flag（单行短输出），同时提供 `version` 子命令（多行详细输出）；二者详见下文「version」。
- `completion` 是 Cobra 提供的内置命令，只向标准输出生成 bash/zsh/fish/PowerShell 补全脚本，不读取配置或数据库。
- `update` 是顶层自更新命令（标志 `--check`、`--version` 与 `--force`）；它是唯一会改写当前运行二进制的命令。默认仅当当前二进制是官方 Release 资产时才执行；`--force` 可显式覆盖已重签的官方资产、指定 tag 的 `go install` 产物或 dev 本地构建。详见下文「[update](#update)」。

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
| `--version`（`-v`） | 单行 `token-usage <version>\n`；本地开发为 `token-usage dev` |
| `version` | 严格五行详细输出（末尾换行）：`token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go版本>` / `platform: <os>/<arch>` |

详细输出示例（release 构建）：

```text
token-usage <version>
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
| `collect all` | 两阶段全采：阶段 A 逐个 client 扫描全历史 messages，且不读取 `collection_log`（单 client 失败不阻断其他）；阶段 B 对配置了 router 的 client 全量回填归因。消息按 `(client, id)` 幂等 UPSERT，可安全重复执行。 | `--client` |
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
token-usage query                      # 今日,执行 query.default,未配置时等价 client
token-usage query <日期>               # 默认视图的日期或区间
token-usage query client [日期]        # 内置视图
token-usage query model [日期]
token-usage query provider [日期]
token-usage query project [日期]
token-usage query session [日期]
token-usage query summary [日期]
token-usage query <name> [日期]        # 已配置子查询/组合查询的直接简写
token-usage query custom <name> [日期] # 上一行的等价显式写法,原样保留
token-usage query list                 # 列出已配置视图;只读配置,不打开数据库
```

每条接受日期参数的 query 命令的输出都以一个统一的统计信息区开始，无论后面输出多少张表都只打印一次（`query list` 只读配置、不打开数据库，没有统计信息区）：

```text
Usage statistics / 使用统计
Units / 单位:
  1 K = 1,000 (thousand / 一千)
  1 M = 1,000 K = 1,000,000 (million / 一百万)
  1 B = 1,000 M = 1,000,000,000 (billion / 十亿)
Query range / 统计范围: 2026-07-01 ~ 2026-07-21
Data through / 数据截至: 2026-07-21 23:59:59
Last successful collection / 最近成功采集: 2026-07-22 08:15:03
```

- `Units / 单位` 说明所有表格与总览摘要中 token 数量的缩写口径：数值达到 1,000 / 1,000,000 / 1,000,000,000 后分别以 K / M / B 显示，统一保留两位小数。
- `Query range / 统计范围` 回显本次实际日期参数：单日只显示该日，闭区间显示为 `YYYY-MM-DD ~ YYYY-MM-DD`。
- `Data through / 数据截至` 是统计日期范围内最新的消息事件时间（`messages.ts`），按本机时区显示到秒。消息事件时间才是统计数据的时间边界；范围内没有消息时显示 `—`。
- `Last successful collection / 最近成功采集` 是全库最近一次成功采集完成的时间（`collection_log.collected_at`，库内以 UTC 存储，展示时转换为本机时区）。它不代表每个客户端都已完整采集到该时刻；还没有任何成功采集记录时显示 `—`。

缺省日期为今天。若查询的日期区间在 `collection_errors` 中存在未解决记录，结果末尾会附「采集异常」提示并列出条目（组合查询输出多张表时，全部表结束后只提示一次），建议用 `errors` 查看详情、`collect retry` 重试。

所有分组视图（四个内置视图与全部自定义多维表）末行显示 `Total / 总计`，总计与表格使用同一日期范围独立聚合；会话明细与总览摘要不追加该行。

### 可配置查询视图

可选的 `[query]` 段配置裸 `query` 的执行对象与自定义视图：

```toml
[query]
default = "group_q"                    # 未配置或空白等价 client

[query.subqueries]
mpc = "model,provider,client"          # 一张多维表

[query.groups]
group_q = "client,model,provider,mpc"  # 按此顺序连续输出多张表
```

- `query <name> [日期]` 与 `query custom <name> [日期]` 是同一已配置子查询（一张表）或组合查询（按声明顺序多张表）的等价写法：目标与输出一致，并遵循同一套校验规则——名称解析、保留名拒绝、日期校验顺序（日期错误优先于名称/定义错误）、全部失败都发生在打开数据库之前。错误示例各自展示自身命令形态（`token-usage query 20260701` 与 `token-usage query custom 20260701`）。直接名称走根命令的位置参数分派——配置中的名称不会注册为动态子命令。两个位置参数时第一个必须是视图名；数字开头的首参数（如 `token-usage query 20260701 20260702`）会在加载配置前以双语用法错误拒绝，并给出两种可接受形态的示例。
- 未知名称与保留名在打开数据库之前被拒绝；日期错误优先于名称/定义错误。两种写法边界一致。
- 子查询从内置维度（`client`/`model`/`provider`/`project`）中至少选择 2 个不同维度，声明顺序即列顺序；组合查询从内置视图与已定义子查询中至少选择 2 个不同成员，组合查询不能引用组合查询。
- 视图名为小写标识符（首字符字母，后续字母、数字、`_`、`-`），不能与 `client`/`model`/`provider`/`project`/`session`/`summary`/`custom`/`list` 冲突。值按逗号分隔，每段自动去除首尾空格，`"model, provider"` 与 `"model,provider"` 等价。若历史手写的子查询或组合查询名为 `list`，升级前请先重命名：新版二进制会将该名称按保留名拒绝（`query list` 已成为静态发现子命令）。
- `query.default` 匹配前去除首尾空格，空白等同未设置并回退 client；可引用内置视图、子查询或组合查询，`session` 与 `summary` 不可引用。
- `query list` 不接受位置参数，单次固定顺序输出：默认行为（`token-usage query -> <name> (<类别>)`）、一次性的调用说明（简写与显式两形态等价）、六个内置命令及其用途，随后把每个已配置子查询/组合查询各渲染为一条今天即可复制执行的完整命令（如 `token-usage query mpc`）附维度或成员 CSV；空分区显示 `None`。它只读取有效配置并解析定义——不打开 `usage.db`、不打印统计信息区、不读取采集异常、不接受日期、不修改任何状态。定义损坏时仍按既有定位错误失败，不会伪装成空列表。

### 输出列布局

可选的 `[query.output]` 段定义一份全局、有序的指标列布局，所有 query 表格共用：

```toml
[query.output]
columns = ["requests", "input", "output", "total", "cache_hit"]
```

`columns` 是有序字符串数组：出现即显示、缺失即隐藏，数组顺序就是每张表的列顺序。允许的 ID（大小写敏感）：

| ID | 表头 | 含义 |
|---|---|---|
| `requests` | Requests / 请求数 | 消息数 |
| `input` | Input / 输入 | fresh input tokens |
| `output` | Output / 输出 | output tokens |
| `cache_read` | Cache Read / 缓存读取 | cache read tokens |
| `cache_create` | Cache Create / 缓存创建 | cache create tokens |
| `reasoning` | Reasoning / 推理 | reasoning tokens |
| `total` | Total / 总计 | 源 total tokens |
| `cache_hit` | Cache Hit / 缓存命中 | cache_read / (fresh input + cache_read + cache_create) |

- **适用范围**：布局作用于 `query client`、`model`、`provider`、`project`、`session`，以及裸 query、具名视图（`query <name>` / `query custom <name>`）与组合查询展开的每张表。`query summary` 不适用——它保持完整纵向摘要（含 Cache Create）；`query list` 不渲染数据表。维度列始终显示在每张表左侧（session 表固定先显示 Client/Project/Title），不参与布局。
- **默认值**：缺失 `[query.output]` 或缺失 `columns` 时使用 `requests, input, output, cache_read, reasoning, total, cache_hit` 七列，升级后既有配置与输出保持不变。`cache_create` 是首个可选但默认隐藏的指标；它始终计入缓存命中率分母，显示或隐藏都不改变任何统计值、排序与总计。
- **校验规则**：`query.output` 必须是表且只允许 `columns` 一个子键；数组非空、元素为上表中的字符串、不得重复（元素首尾空格自动去除）。空数组不是「恢复默认」——恢复默认应删除 `query.output`（或 `query.output.columns`）。错误会报出完整配置路径与具体值。`config set` 不支持写入 `query.output.columns`，请使用 TUI 的 Output columns 页或手工编辑 TOML。
- **错误边界**：无关的视图定义错误（`subqueries`/`groups`/`default`）不阻断五个受布局影响的静态表格命令——合法布局仍生效。`query.output` 自身不合法时，这五个命令在打开数据库前失败。顶层 query 问题（`[query]` 与 `[Query]` 并存、根值非表）下静态表格命令静默回退默认七列，裸 query、具名视图与 `query list` 仍按既有定位错误失败。TUI 保存始终执行完整 query 校验。

`query provider`（以及任何自定义视图中的 provider 维度）优先使用路由归因，其次使用采集器的供应商值；历史空值保持未归因，查询不会依据客户端推断供应商。`provider_aliases` 在组合键形成前生效：相同别名在每个视图中合并为同一行，且不会修改 `usage.db`。

query 配置是纯展示配置。语义错误（断开引用、CSV 写错、未知键、`[query]` 与 `[Query]` 并存等顶层冲突、`query = "x"` 根值非表）只会使默认路径（裸 `query` 与 `query <日期>`）、全部具名调用（`query <name>` / `query custom <name>`）、`query list` 与 TUI 保存失败并定位具体配置键；五个受布局影响的静态表格命令（`client`/`model`/`provider`/`project`/`session`）在顶层问题态回退默认七列、仅无关视图定义损坏时保持合法布局，`query summary` 不受影响，`collect`、`status`、`start`、守护进程、`config set`、`config show` 不受影响且原样保留问题项。TUI 主菜单按 `v` 进入 **Query** 页，含三个平级入口——**Views / 查询视图**（自定义子查询、组合查询、默认行为）、**Output columns / 输出列**（全局指标布局，`d` 恢复默认）、**Provider aliases / 供应商别名**——各自的部分无法解析时先显示自己的恢复列表。降级到不支持查询视图的旧版本前，请删除整个 `[query]`、`[query.subqueries]`、`[query.groups]`、`[query.output]` 段：旧版本会拒绝任何非空 query 段。

示例：

```bash
token-usage query                    # 今日，执行配置的默认视图（未配置时按客户端分组）
token-usage query 20260701-20260721  # 区间，默认视图
token-usage query mpc                # 今日，mpc 多维表（直接简写）
token-usage query custom group_q 20260701  # 显式写法：按声明顺序输出四张表
token-usage query summary 20260701   # 单日总览
token-usage query list               # 列出已配置视图，不触碰数据库
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

无参数时打开交互式配置 TUI（`bubbletea`）。配置文件不存在时先写默认模板再打开；可编辑客户端、路由、守护进程、日志和查询配置（`v` 进入的 Query 页归拢视图定义、输出列布局与 provider aliases），`data_dir` 在 TUI 中只读。保存统一走 `ApplyConfig`（见下文「config set」）。非 router 支持客户端（当前除 Claude 外全部）不展示「绑定路由」字段；此类客户端上存量非空 router 仍会显示（便于清回「无」），保存校验拒绝非空值（见下文「config set」的 router 拦截）。

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

**router 拦截：** `config set clients.<name>.router <value>` 在 `<value>` 非空且 `<name>` 不是 router 支持客户端（当前仅 Claude）时直接报错拒绝写入，退出非零；设为空字符串表示清除，始终放行。读取链路（`config show`、采集、daemon）对存量配置中其他客户端的非空 router 仍容忍。

**data_dir 迁移：** 修改 `data_dir` 需 `--confirm-migrate` 确认；且要求旧 daemon **已停止**（运行中拒绝，写入前校验），迁移需手动搬运 `usage.db`/`logs`，PID/lock/runtime-state 不迁移（按 stale 协议清理）。

### 支持的 dotted key

| 范围 | 可写 key |
|------|----------|
| 数据目录 | `data_dir`（需 `--confirm-migrate`） |
| 守护进程 | `daemon.poll_interval`、`daemon.autostart` |
| 日志 | `log.level`、`log.dir`、`log.max_days` |
| 客户端 | `clients.<name>.enabled`、`clients.<name>.router`、`clients.<name>.paths.<path-key>` |
| 路由 | `routers.cc_switch.db_path` |
| 供应商别名 | `provider_aliases.<原始 provider 名>` |

受支持 client 为 `claude`、`opencode`、`codex`、`workbuddy`、`zcode`、`autoclaw`。path key 分别为：Claude `projects_dir`；OpenCode `db`；Codex `state_dir`/`sessions_dir`；WorkBuddy `db`/`projects_dir`；ZCode `db`；AutoClaw `sessions_dir`。

`provider_aliases` 只改变 `query provider` 中的标签与分组，不修改采集或路由回填的数据，下一次查询立即生效。名称含 `.` 时使用引号段，例如：

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
- 新建配置文件时，完成提示会说明默认未启用任何客户端，并给出开启示例命令（`token-usage config set clients.<name>.enabled true`）；默认模板所有客户端 `enabled = false`，`router` 行与 provider 别名为注释示例。

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
token-usage update --force
```

| 形式 | 作用 |
|------|------|
| `update` | 更新到最新稳定 Release。若当前二进制同目录存在一次中断的 POSIX 更新留下的受限事务 journal，先完成恢复；之后仅当目标严格高于当前版本且当前来源可信时才继续新替换：下载资产、与 `SHA256SUMS` 清单比对 SHA256、stage `--version` 二次校验、替换二进制并按原运行态恢复 daemon。 |
| `update --check` | 只读检查；不创建任何本地文件（不创建配置目录/锁/日志/数据库/服务定义）。 |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | 更新（或加 `--check` 后仅检查）指定精确版本 tag。`--version` 接受严格 Release tag（`v` 前缀、`MAJOR.MINOR.PATCH`、可选 `-rc.N`、无前导零）；非法值在任何网络请求前即报错。 |
| `update --force` | 当前二进制来源非官方 Release 资产时仍强制覆盖，仅限两种豁免：与所报告版本官方资产 hash 不一致（按安装指引重签过的二进制、或 `go install pkg@vX.Y.Z` 产物），以及 dev 本地构建（`Version = dev`；直接构建的伪版本会被规范化为 `dev`）。全部结构检查与目标资产的 SHA256 / stage `--version` 校验照常执行；软链副本与非官方 tag 不可被 force。 |

`--check` 与 `--version` 可组合，如 `update --check --version vX.Y.Z-rc.N` 只检查候选版。`--force` 不能与 `--check` 组合（该组合被显式拒绝）。

标志：

- `--check`（bool）：只读检查，不写本地文件。
- `--version`（string）：目标 Release tag。接受 `vMAJOR.MINOR.PATCH` 与 `vMAJOR.MINOR.PATCH-rc.N`（无前导零，`N >= 1`，无 build metadata）。
- `--force`（bool）：当前二进制非官方 Release 资产（已重签、go install、dev 本地构建）时仍强制覆盖；确切豁免边界见[信任与来源校验](#信任与来源校验)。

`update` 不接受位置参数（`Args: NoArgs`）。

### 稳定版 / RC 选择

默认 `update` 只解析最新**稳定** Release，绝不选择 prerelease。只有用 `--version` 显式指定 rc tag（如 `--version vX.Y.Z-rc.N`）时才会查询/安装预发布版。

### 信任与来源校验

`update` 仅在目标严格更高且当前来源可信时才覆盖当前二进制。满足以下任一条件即判定当前来源**不可信**（默认 `update` 拒绝覆盖，输出人工安装指引）：

- 当前 `Version` 为 `dev` 或伪版本（如来自 `make build`、`make build-all` 或 `go install`）；
- 当前二进制不是普通文件，或为 symlink；
- 当前二进制的 SHA256 与当前版本的官方资产 hash 不一致（如按安装指引重签过的二进制、`go install pkg@vX.Y.Z` 产物）。

拒绝携带 `--force` 出口，但仅限两种豁免：

- **hash 失配**（当前版本存在官方 Release 与清单，但本地内容不一致）：用 `--force` 再次执行即用官方资产覆盖，自动更新恢复正常；
- **dev 本地构建**（`Version = dev`；直接构建的伪版本会被规范化为 `dev`，这是 `update --force` 唯一接受的 dev 形态；不存在可比的官方 Release 与清单，从未发生 hash 比较）：`update --force` 把安装切换为官方 Release 资产。

软链副本与非官方 tag 不可被 force——其余一切拒绝原因都只能手动安装。`--force` 不跳过任何检查：结构前置仍然把关，目标资产仍要下载、与 `SHA256SUMS` 比对 SHA256、并经 stage `--version` 二次校验后才可能替换当前二进制。`--force` 安装完成以注明 `--force` 的成功提示退出 0；绝不谎报来源可信。

在 macOS 上，拒绝信息会区分「带本地 ad-hoc 签名的二进制」（经签名探测识别）并明确列出重签官方资产的可能性；其他平台及探测不可用时降级为通用文案，但同样列出已重签可能项与相同的 `--force` 出口。

唯一受信仓库为 `YuLaiZ/token-usage`；下载 URL 重构、清单与分阶段安装信任链见[架构设计](architecture.zh-CN.md)。

该来源安全门约束的是新的二进制替换。恢复已经记录的本地事务不会下载或接受新来源：只使用由当前 executable 与 journal nonce 推导出的同目录路径，并重新校验 journal 中记录的 hash 后恢复一致状态。

### 退出码

- `0`：已完成的预期状态——无稳定 Release、已是最新、发现可更新（`--check`）、Windows 后台替换已排队，或恢复确认上次中断时新二进制已经落地。
- 非 `0`：指定 tag 不存在、当前来源未通过校验且未携带 `--force`（hash 失配或 dev 本地构建）、或来源根本不可被 force（软链 / 非官方 tag）、下载/清单/checksum/stage `--version` 校验被拒绝、恢复后回到旧版本、安装尚未完成、安装/回滚/daemon 重启失败，或 `--version` 非法。

### 副作用边界

`update --check` 完全只读。真正 `update` 发现已有事务 journal 时先完成恢复；否则仅在确有更新且来源检查通过——可信，或经 `--force` 显式覆盖——时才停 daemon → 替换二进制 → 重启；无更新时不启停 daemon，也不重写 `config.toml`、数据库、日志、macOS LaunchAgent plist 或 Windows 注册表。

### Windows 异步替换

Windows 上替换运行中的 `.exe` 受限，自更新把替换交给后台 helper 后返回。helper 成功启动后，命令会明确说明「后台替换已排队」，以 `0` 退出，并提示稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本，**不声称已完成**。macOS/POSIX 为同步原子替换（同目录 backup + rename + fsync，失败回滚 + 下一次 `update` 调用按 journal 恢复）。

## 配置文件

路径固定 `~/.token-usage/config.toml`（TOML，可手工添加注释）。所有客户端默认关闭：用 `clients.<name>.enabled = true` 开启需要的客户端，数据源路径由程序按各工具默认位置自动填充。用 dotted key 同段写法覆盖默认。`config set`/TUI 保存会完整重写配置，故不保留原有注释和 map 键书写顺序；完整字段与默认值见 `token-usage config init` 生成的模板。

`data_dir` 决定数据文件位置（`usage.db`、日志、PID、runtime-state、锁）；配置文件路径不随 `data_dir` 变化。`daemon.autostart` 控制开机自启（macOS launchd / Windows 注册表）。
