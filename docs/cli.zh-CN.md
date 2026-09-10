# CLI 参考

> 简体中文 | [English](cli.md)

本文件是 `token-usage` 命令行界面的权威参考：命令树、位置参数、标志、退出码与示例。行为描述以源码实现为准。

## 命令树

```text
token-usage
├── version                               # 查看版本与构建信息（详细五行输出）
├── completion [bash|zsh|fish|powershell] # 生成 Shell 补全脚本
├── collect [DATE|DATE-DATE]              # 今天或指定日期（日/月/年粒度）增量采集（含 router）
│   ├── all                               # 两阶段全采：messages 全历史 + router 全量回填
│   ├── router --client X                 # 仅 router 全量回填（不动 messages）
│   └── retry                             # 重试 collection_errors 中未解决失败组
├── query [<name> [DATE|DATE-DATE] | DATE|DATE-DATE]
│   ├── client [DATE|DATE-DATE]    # 按客户端分组（默认视图）
│   ├── model [DATE|DATE-DATE]     # 按模型分组
│   ├── provider [DATE|DATE-DATE]  # 按供应商分组
│   ├── project [DATE|DATE-DATE]   # 按项目分组
│   ├── day [DATE|DATE-DATE]       # 按天用量
│   ├── month [DATE|DATE-DATE]     # 按月用量
│   ├── hour [DATE|DATE-DATE]      # 按小时用量
│   ├── weekday [DATE|DATE-DATE]   # 按星期用量
│   ├── heatmap [DATE|DATE-DATE]   # 星期×小时热力图
│   ├── session [DATE|DATE-DATE]   # 会话明细
│   ├── summary [DATE|DATE-DATE]   # 总览摘要
│   ├── custom <name> [DATE|DATE-DATE] # 显式运行已配置视图
│   └── list                        # 列出已配置视图（只读配置，不开数据库）
├── errors [DATE|DATE-DATE]
├── watch [DATE|DATE-DATE]                # 以固定间隔刷新 query 输出（视图选择与 query 一致）
├── doctor                                # 只读健康自检（配置、数据目录、数据库、客户端、采集、异常）
├── daemon                                # 管理采集守护进程（裸执行只显示帮助）
│   ├── start                             # 后台启动守护进程（nginx 风格；拉起 _run）
│   ├── status                            # 查看守护进程运行状态与配置摘要
│   ├── stop                              # 停止守护进程
│   └── restart                           # 在单把进程控制锁内停旧起新
├── serve                                 # 管理本地只读仪表板 HTTP 服务（裸执行只显示帮助）
│   ├── start                             # 后台运行仪表板（nginx 风格；日志写入 serve.log）
│   ├── status                            # 查看后台仪表板运行状态
│   ├── stop                              # 停止后台仪表板
│   └── restart                           # 停止运行中的仪表板并拉起全新后台实例
├── config                                # 无参数：打开交互式配置 TUI
│   ├── show                              # 输出完整 effective TOML（只读、纯 TOML）
│   ├── get <key>
│   ├── set <key> <value>
│   └── init
├── update                                # 从官方 GitHub Release 自更新（--check / --version / --force）
├── _run                                  # Hidden，由 daemon start/launchd/注册表拉起，勿直接调用
└── _serve-run                            # Hidden，serve start 拉起的后台仪表板服务主体，勿直接调用
```

设计要点：

- 无顶层 `router` 子命令；路由归因通过 `collect all`（隐含）或 `collect router`（仅归因层）触达。
- 日期是**位置参数**：`DATE` 为日（`YYYYMMDD`）、月（`YYYYMM`）或年（`YYYY`，仅单独使用）；`DATE-DATE` 为闭区间，端点为日或月。任何形态展开上限 366 天。无 `--date` 标志；`errors` 接受相同形态。
- `query` 没有 `--format`/`--by-*` 标志；视图由子命令选择，输出固定为表格。
- 直接执行 `token-usage`（不带任何参数）只打印帮助，既不启动 TUI 也不启动守护进程。
- 根命令带 `-v, --version` flag（单行短输出），同时提供 `version` 子命令（多行详细输出）；二者详见下文「version」。
- `completion` 是 Cobra 提供的内置命令，只向标准输出生成 bash/zsh/fish/PowerShell 补全脚本，不读取配置或数据库。
- `update` 是顶层自更新命令（标志 `--check`、`--version` 与 `--force`）；它是唯一会改写当前运行二进制的命令。默认仅当当前二进制是官方 Release 资产时才执行；`--force` 可显式覆盖已重签的官方资产、指定 tag 的 `go install` 产物或 dev 本地构建。详见下文「[update](#update)」。

## 从 v0.1.8 迁移（破坏性变更）

本次命令面做了有意的收敛，面向 v0.1.8 编写的脚本需要更新：

- 六个分析命令 `chart`、`compare`、`export`、`forecast`、`report`、`top` 已**删除**（无兼容别名）。可视化分析——SVG 图表、用量预测、两期对比、最重会话排行——由 `serve` 仪表板经只读 HTTP 数据面延续，含页面范围内的 CSV 导出；终端查询继续由 `query` 与 `watch` 承担。机器可读的 CLI export 合同与离线 HTML 报告包已有意移除，没有完整直接替代。
- 顶层生命周期命令已移入 `daemon` 命令组：`token-usage start` → `token-usage daemon start`，`status`、`stop`、`restart` 同理。
- `token-usage serve` 不再前台启动服务：裸 `serve` 只打印命令组帮助，仪表板用 `serve start` / `serve restart` 启动。

| v0.1.8 | 现在 |
|---|---|
| `token-usage start` / `status` / `stop` / `restart` | `token-usage daemon start` / `daemon status` / `daemon stop` / `daemon restart` |
| `token-usage serve`（前台） | `token-usage serve start`（后台） |
| `token-usage chart` / `forecast` / `compare` / `top` | `token-usage serve start`，然后打开仪表板 |
| `token-usage export` / `report` | 终端查询继续用 `query` / `watch`；仪表板提供页面范围内的 CSV 导出。机器可读 CLI export 与离线报告包已移除，无完整直接替代 |

## 通用约定

### 日期参数格式

| 命令 | 接受形式 | 缺省 |
|------|----------|------|
| `collect`、`query`（含子命令）、`watch` | `DATE`（日 `YYYYMMDD`、月 `YYYYMM` 或年 `YYYY`，年仅单独使用）或 `DATE-DATE`（日/月端点，闭区间） | 今天 |
| `errors` | `DATE` 或 `DATE-DATE`（与 `collect`/`query` 相同形态） | 无日期且无 `--source` 时只看未解决 |

`YYYYMMDD` 为 8 位紧凑格式（如 `20260701`）；`YYYYMM` 表示一个自然月，`YYYY` 表示一个自然年（年形态仅单独使用）。`YYYY-MM-DD`、多余位置参数、年做区间端点、结束早于开始均报错并给出命令示例。单参数或区间统一归一化为逐日列表（含两端），上限 366 天（一个闰年），更长范围请拆分多次执行。

### 退出码

`token-usage` 在 `main` 中把命令返回的 error 映射为退出码：

- `0`：成功（含幂等结果，如 `daemon start` 时守护进程已在运行、`daemon stop` 时未运行）。
- `1`：任意 error（参数校验失败、采集/查询失败、守护进程控制失败、revision 冲突、部分失败等）。

成功与失败的标准输出/标准错误合同见各命令章节。

### 标志作用域

- `--client`：`collect` 的 **PersistentFlag**，被 `all`/`router`/`retry` 三个子命令继承。
- `--force`：`collect` 的 **LocalFlag**，子命令**不继承**（子命令传 `--force` 会报 unknown flag）。
- `errors` 的 `--source`/`--unresolved`/`--format`：`errors` 的 LocalFlag。
- 根命令的 `-v, --version`：根级 flag，输出单行短版本。

## version

查看版本与构建信息。由 `internal/buildinfo` 包统一规范化版本与构建元数据，`--version`/`version` 子命令共享同一份 `buildinfo.Info` 快照。

```text
token-usage --version        # 等价于 -v，单行短输出
token-usage version          # 多行详细输出
```

| 形式 | 输出 |
|------|------|
| `--version`（`-v`） | 单行 `token-usage <version>\n`；本地开发为 `token-usage v0.1.8-dev`（伪版本归一显示） |
| `version` | 严格五行详细输出（末尾换行）：`token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go版本>` / `platform: <os>/<arch>` |

详细输出示例（release 构建）：

```text
token-usage <version>
commit: 59a8d55a1b2c
build_time: 2026-07-30 18:00:00
go: go1.26.4
platform: darwin/arm64
```

- `commit` 展示完整 revision 的前 12 位；工作树有修改时（`vcs.modified=true`）追加 `-dirty`。
- **版本来源优先级**：① Makefile `ldflags -X` 注入的 `Version` → ② `go install @version` 时 `debug.ReadBuildInfo().Main.Version` → ③ 本地默认 `dev`。
- **commit 来源**：① 注入的 `Commit` → ② `debug.BuildInfo` 的 `vcs.revision` → ③ `unknown`。**build_time 不使用 `vcs.time`**（那是 commit 时间，非构建时间），仅取注入值，未注入为 `unknown`。
- **build_time 渲染**：注入值为 UTC RFC3339（发布构建 stamp），显示时转本机时区、空格分隔（`YYYY-MM-DD HH:MM:SS`）；无法按 RFC3339 解析的值原样显示。
- **本地构建版本显示**：在打了 tag 的检出上直接 `go build` 时，`Main.Version` 是 Go 伪版本（如 `v0.1.8-0.20260908085159-1280e3f00e99`），显示归一为 `v0.1.8-dev`（base 指向该检出目标发布的版本，精确 commit 见 commit 行）；首个 tag 之前的检出显示 `v0.0.0-dev`。两种边界形态原样保留工具链值、不归一为 `-dev`：恰好在 tag 提交上构建显示 `v<tag>`（如 `v0.1.8`），同提交但工作树有修改显示 `v<tag>+dirty`（如 `v0.1.8+dirty`）。`-dev` 显示与字面 `dev` 共享 update 守卫语义：`update` 拒绝之，需 `update --force` 才能切换到官方 Release 资产。
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

各 Shell 的前置条件：

- **zsh** 需先初始化补全系统（`compinit`）。加载脚本时报 `compdef: command not found` 即说明 `compinit` 尚未运行：请在 rc 文件（如 `~/.zshrc`）中、加载行之前加入 `autoload -U compinit` 与 `compinit`。部分配置为保持启动安静有意不启用 `compinit`，改动前先查看 rc 文件。
- **bash** 依赖 bash-completion 包（生成的脚本使用其中的 `_init_completion`）。macOS 系统 bash 3.2 未带该包：请经 Homebrew 安装 bash 4+ 与 `bash-completion@2`，然后重启 shell。
- **fish** 与 **PowerShell** 无前置条件：把脚本写入对应补全位置后，新会话即生效（fish 自动加载 `~/.config/fish/completions/`，并遵循绝对路径的 `XDG_CONFIG_HOME`；PowerShell 把脚本输出追加进 `$PROFILE` 即可）。

zsh 上 `compinit` 可能报告不安全目录（insecure directories，即补全搜索路径上 group/其他用户可写的目录——老 Homebrew 安装上最常见的是 `/opt/homebrew/share/zsh` 及其 `site-functions`）并询问是否继续。三种处理方式：

1. 在提示出现时输入 `y`；每次新开 shell 都会再次提示。
2. 一次性修复目录后重跑：对每个被报告且归你所有的目录执行 `chmod go-w <目录>`（Homebrew 官方同样建议的做法；异属主目录须由管理员处理——chmod 修不了属主问题）。
3. 永久跳过安全检查：执行 `compinit -u`，或在 rc 文件中把 `compinit` 写成 `compinit -u`（须自觉接受跳过检查）。

官方安装脚本可自动完成整套配置——zsh 上会交互询问是修复目录、跳过检查还是跳过补全；见[安装指南](install.zh-CN.md)。

各 Shell 的持久化安装说明由 `token-usage completion <shell> --help` 提供。该命令不读取配置、数据库或数据源。

## collect

采集 token 使用数据。`collect` 及其子命令在打开数据库前都会做**守护进程冲突预检**：若守护进程正在运行（持有 daemon lock），直接拒绝采集，避免并发写库。

```text
token-usage collect [DATE|DATE-DATE]
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
token-usage query day [日期]
token-usage query month [日期]
token-usage query hour [日期]
token-usage query weekday [日期]
token-usage query heatmap [日期]
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
- `Query range / 统计范围` 回显解析后的日期范围：单日只显示该日，闭区间显示为 `YYYY-MM-DD ~ YYYY-MM-DD`（月/年参数显示其归一化展开范围）。
- `Data through / 数据截至` 是统计日期范围内最新的消息事件时间（`messages.ts`），按本机时区显示到秒。消息事件时间才是统计数据的时间边界；范围内没有消息时显示 `—`。
- `Last successful collection / 最近成功采集` 是全库最近一次成功采集完成的时间（`collection_log.collected_at`，库内以 UTC 存储，展示时转换为本机时区）。它不代表每个客户端都已完整采集到该时刻；还没有任何成功采集记录时显示 `—`。

缺省日期为今天。若查询的日期区间在 `collection_errors` 中存在未解决记录，结果末尾会附「采集异常」提示并列出条目（组合查询输出多张表时，全部表结束后只提示一次），建议用 `errors` 查看详情、`collect retry` 重试。

所有分组视图（九个内置视图与全部自定义多维表）末行显示 `Total / 总计`，总计与表格使用同一日期范围独立聚合；会话明细与总览摘要不追加该行。

`query day`、`query month`、`query hour`、`query weekday` 及任何含 `day`、`month`、`hour` 或 `weekday` 时间维度的视图按时间升序呈现时间轴：行按时间维度升序排列（日 `YYYY-MM-DD`、月 `YYYY-MM`、小时 `00:00`..`23:00` 或 ISO 周序的星期名，而非按总量降序；多个时间维度并存时按声明首个为排序主轴），`Trend / 趋势` 条形列以区间内最繁忙的行为基准对比各行总量。纯 `day` 与 `month` 视图为请求区间内无数据的日期/月份插入零值行；纯 `hour` 与 `weekday` 视图按本机时区折算小时/星期（与 date 列同一时区语义），任一请求区间都呈现整日 24 小时固定刻度与 ISO 周序（周一在首）的整周 7 天固定刻度，为无数据小时/星期补零值行，时间轴不留缺口。

`query heatmap` 渲染星期×小时矩阵：行为 ISO 周序（周一在首）的 7 个星期，列为 `00`..`23` 的 24 个小时（均按本机时区折算，与 date 列同一时区语义）。单元格为该交点 total 相对全表最大值的密度字符（` .:-=+*#%@`，0..9 级），尾列合计各星期，尾行 `Total / 总计` 合计各小时与全表。矩阵不参与 `[query.output.columns]` 布局；`heatmap` 是保留视图名——与 `session`、`summary` 一样，不可被 `query.default`、子查询或组合查询引用。

`query summary` 输出固定的纵向摘要：`Clients / 客户端数`、`Total requests / 请求总数`、`Active days / 活跃天数`（区间内有数据的天数）、每个 token 列一行（`Input` 至 `Total`，恒含 `Cache Create`）；区间内至少有一天有数据时，追加 `Peak day / 单日峰值`（源 total 最高的日期，同分取日期最早者）与 `Daily average / 日均总量`（区间总量除以活跃天数，整数除法向下取整）。区间内无数据时最后两行不渲染。

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
- 子查询从内置维度（`client`/`model`/`provider`/`project`/`day`/`month`/`hour`/`weekday`）中至少选择 2 个不同维度，声明顺序即列顺序；组合查询从内置视图与已定义子查询中至少选择 2 个不同成员，组合查询不能引用组合查询。
- 视图名为小写标识符（首字符字母，后续字母、数字、`_`、`-`），不能与 `client`/`model`/`provider`/`project`/`session`/`summary`/`day`/`month`/`hour`/`weekday`/`heatmap`/`custom`/`list` 冲突。值按逗号分隔，每段自动去除首尾空格，`"model, provider"` 与 `"model,provider"` 等价。若历史手写的子查询或组合查询名为 `list`，升级前请先重命名：新版二进制会将该名称按保留名拒绝（`query list` 已成为静态发现子命令）。
- `query.default` 匹配前去除首尾空格，空白等同未设置并回退 client；可引用内置视图、子查询或组合查询，`session` 与 `summary` 不可引用。
- `query list` 不接受位置参数，单次固定顺序输出：默认行为（`token-usage query -> <name> (<类别>)`）、一次性的调用说明（简写与显式两形态等价）、十一个内置命令及其用途，随后把每个已配置子查询/组合查询各渲染为一条今天即可复制执行的完整命令（如 `token-usage query mpc`）附维度或成员 CSV；空分区显示 `None`。它只读取有效配置并解析定义——不打开 `usage.db`、不打印统计信息区、不读取采集异常、不接受日期、不修改任何状态。定义损坏时仍按既有定位错误失败，不会伪装成空列表。

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

- **适用范围**：布局作用于 `query client`、`model`、`provider`、`project`、`day`、`month`、`hour`、`weekday`、`session`，以及裸 query、具名视图（`query <name>` / `query custom <name>`）与组合查询展开的每张表，外加对应的 `watch` 帧（其视图表与 `query` 走同一执行链；`watch --by summary` 保持完整纵向摘要）。`query summary` 不适用——它保持完整纵向摘要（含 Cache Create）；`query list` 不渲染数据表。维度列始终显示在每张表左侧（session 表固定先显示 Client/Project/Title/Duration），不参与布局。
- **默认值**：缺失 `[query.output]` 或缺失 `columns` 时使用 `requests, input, output, cache_read, reasoning, total, cache_hit` 七列，升级后既有配置与输出保持不变。`cache_create` 是首个可选但默认隐藏的指标；它始终计入缓存命中率分母，显示或隐藏都不改变任何统计值、排序与总计。
- **校验规则**：`query.output` 必须是表且只允许 `columns` 一个子键；数组非空、元素为上表中的字符串、不得重复（元素首尾空格自动去除）。空数组不是「恢复默认」——恢复默认应删除 `query.output`（或 `query.output.columns`）。错误会报出完整配置路径与具体值。`config set` 不支持写入 `query.output.columns`，请使用 TUI 的 Output columns 页或手工编辑 TOML。
- **错误边界**：无关的视图定义错误（`subqueries`/`groups`/`default`）不阻断九个受布局影响的静态表格命令——合法布局仍生效。`query.output` 自身不合法时，这九个命令在打开数据库前失败。顶层 query 问题（`[query]` 与 `[Query]` 并存、根值非表）下静态表格命令静默回退默认七列，裸 query、具名视图与 `query list` 仍按既有定位错误失败。TUI 保存始终执行完整 query 校验。

`query provider`（以及任何自定义视图中的 provider 维度）优先使用路由归因，其次使用采集器的供应商值；历史空值保持未归因，查询不会依据客户端推断供应商。`provider_aliases` 在组合键形成前生效：相同别名在每个视图中合并为同一行，且不会修改 `usage.db`。

query 配置是纯展示配置。语义错误（断开引用、CSV 写错、未知键、`[query]` 与 `[Query]` 并存等顶层冲突、`query = "x"` 根值非表）只会使默认路径（裸 `query` 与 `query <日期>`）、全部具名调用（`query <name>` / `query custom <name>`）、`query list` 与 TUI 保存失败并定位具体配置键；九个受布局影响的静态表格命令（`client`/`model`/`provider`/`project`/`day`/`month`/`hour`/`weekday`/`session`）在顶层问题态回退默认七列、仅无关视图定义损坏时保持合法布局，`query summary` 不受影响，`collect`、`daemon status`、`daemon start`、守护进程本体、`config set`、`config show` 不受影响且原样保留问题项。`watch` 遵循同一分界：其缺省视图与配置视图路径与裸 `query` 同样以本地化诊断失败，而显式内置视图名（`watch --by client`）与静态表格命令一致，忽略无关的视图定义错误。TUI 主菜单按 `v` 进入 **Query** 页，含三个平级入口——**Views / 查询视图**（自定义子查询、组合查询、默认行为）、**Output columns / 输出列**（全局指标布局，`d` 恢复默认）、**Provider aliases / 供应商别名**——各自的部分无法解析时先显示自己的恢复列表。降级到不支持查询视图的旧版本前，请删除整个 `[query]`、`[query.subqueries]`、`[query.groups]`、`[query.output]` 段：旧版本会拒绝任何非空 query 段。

示例：

```bash
token-usage query                    # 今日，执行配置的默认视图（未配置时按客户端分组）
token-usage query 20260701-20260721  # 区间，默认视图
token-usage query 202608             # 单月，默认视图
token-usage query mpc                # 今日，mpc 多维表（直接简写）
token-usage query custom group_q 20260701  # 显式写法：按声明顺序输出四张表
token-usage query summary 20260701   # 单日总览
token-usage query list               # 列出已配置视图，不触碰数据库
```

## errors

查看采集异常。

```text
token-usage errors [DATE|DATE-DATE]
```

日期参数与 `collect`/`query` 同形态：单日 `YYYYMMDD`、月 `YYYYMM`、年 `YYYY` 或 `日期-日期` 区间（逐日展开，与其他日期范围一样最多 366 天）。

- 无日期且无 `--source`：默认只看**未解决**异常。
- 给出日期（单日、月、年或区间）或 `--source`：默认看**全部状态**（含已解决）。
- `--unresolved`：显式只看未解决，始终生效。

标志：

- `--source <name>`：按数据源过滤（`claude`/`opencode`/`codex`/`workbuddy`/`zcode`/`autoclaw`）。
- `--unresolved`：只看未解决。
- `--format <fmt>`：输出格式，`table`（默认，框线表加重试提示）或 `json`；非法值在打开数据库之前即被拒绝。

`--format json` 时 stdout 为纯数据——错误记录 JSON 数组，无统计头、无「暂无异常记录」、无重试提示行（错误照常走 stderr 并返回非零）。每条记录只投影 `table` 各列可见的字段：

| 字段 | 类型 | 含义 |
|---|---|---|
| `id` | 数字 | 记录 ID |
| `date` | 字符串 | 日期（`YYYY-MM-DD`） |
| `source` | 字符串 | 数据源 |
| `message` | 字符串 | 错误信息 |
| `retry_count` | 数字 | 已重试次数 |
| `resolved` | 布尔 | 是否已解决 |

两种格式的过滤与记录顺序（最新在前，与 `table` 一致）完全相同。空结果输出 `[]`。两空格缩进，尾随换行。

示例：

```bash
token-usage errors                     # 未解决异常
token-usage errors 20260721            # 某日全部异常
token-usage errors 20260701-20260707   # 区间内全部异常
token-usage errors --source codex      # 某数据源全部异常
token-usage errors --unresolved        # 显式只看未解决
token-usage errors --format json | jq .  # 机器可读记录
```

## doctor

运行只读健康检查，逐项输出一行结果（格式为「标签: 状态 描述」，状态为 `OK / 正常`、`WARN / 警告`、`FAIL / 失败`，另有 `SKIPPED / 跳过` 与信息性的 `INFO / 提示` 行），最后给出汇总。严格只读：**绝不启动/停止/重启守护进程，绝不修改配置；不写业务数据**——打开数据库的行为（journal 模式设置与 schema 迁移）与其它读取类命令一致，doctor 自身不执行任何特有的写操作。数据目录可写性探针仅创建一个临时文件并立即删除。

```text
token-usage doctor
token-usage doctor --format json
```

`--format json` 把同一批检查项与汇总输出为单个机器可读的 JSON 文档（两空格缩进、尾随换行）：`checks` 是 `{id, label, status, detail}` 数组——`id` 是稳定的机器键（`config`、`data_directory`、`database`、`clients`、`last_collection`、`data_freshness`、`date_consistency`、`unresolved_errors`、`query_definitions`、`daemon`、`dashboard`），`status` 取封闭值域（`ok`/`warn`/`fail`/`skipped`/`info`，与 table 状态词语义一致），`label`/`detail` 与 table 行是相同的双语字符串。`summary` 携带 `result`（`ok`/`warn`/`fail`，FAIL 优先于 WARN）、`warnings` 与 `problems`。非法 `--format` 在任何检查执行之前即被拒绝。

| 检查项 | OK | WARN | FAIL |
|---|---|---|---|
| `Config / 配置` | 有效配置加载成功，显示配置路径 | — | 配置缺失或非法 |
| `Data directory / 数据目录` | 目录存在且可写（探针文件即建即删） | — | 目录缺失、不是目录或不可写 |
| `Database / 数据库` | 可打开且 `PRAGMA quick_check` 通过，显示路径与消息行数 | 文件尚未创建（运行 `collect` 后生成） | 打开失败或 quick_check 未通过 |
| `Clients / 客户端` | 已启用客户端数量与名字 | 未启用任何客户端 | — |
| `Last collection / 最近采集` | 最近成功采集时间（本机时区） | 尚无成功采集记录 | 查询失败 |
| `Data freshness / 数据新鲜度` | 距最近成功采集的人性化时长（`刚刚`、`N 小时前` 或 `N 天前`）；7 天内 OK | 最近采集超过 7 天（7 天覆盖周末与短假）；提示运行 `token-usage collect` 刷新；仅警告不失败 | — |
| `Date consistency / 日期一致性` | 全部消息行的 date 与按 ts 毫秒重算的本地日期一致，显示消息总数 | N 条消息日期与时间戳不一致；请检查系统时区是否变更或数据是否被直接修改；仅警告不失败——无自动修复 | 查询失败 |
| `Unresolved errors / 未解决异常` | 无 | 数量，并提示 `token-usage errors` 与 `token-usage collect retry` | 查询失败 |
| `Query definitions / 查询视图` | 已配置的子查询/组合查询/默认行为语义合法（未配置时同样 OK） | 问题计数、首个诊断路径并指向 `token-usage query list`；仅警告——坏视图定义不会阻断采集与静态表格命令 | 配置加载失败 |
| `Daemon / 守护进程` | 纯提示：指向 `token-usage daemon status`;doctor 绝不探测或操作守护进程（探测会创建锁文件/配置目录） | | |
| `Dashboard / 仪表板` | `serve.json` 存在且 `/api/meta` 有响应；显示记录的 URL 与 PID | 状态损坏或陈旧（`serve.json` 无法读取，或记录实例不再应答）；建议 `token-usage serve status` 清理 | — |

`Dashboard / 仪表板` 探测按构造只读：读 `serve.json`，文件存在时发一次 `GET /api/meta`（不存在时不发任何请求）。与守护进程检查不同，它可以安全探测——读状态文件与 HTTP GET 不创建锁或文件——但 doctor 绝不自行删除陈旧或损坏的状态文件；WARN 指向 `token-usage serve status`，其陈旧/损坏清理会在状态锁内删除残留。

- 因上游检查失败而无法执行的检查项输出 `SKIPPED / 跳过`，不重复计数（上游 FAIL 已计数）：配置失败跳过全部依赖配置的检查项；数据库缺失或损坏跳过采集、数据新鲜度、日期一致性与异常四项；最近采集查询失败时数据新鲜度以「无法获取」跳过（最近采集一项已失败）；完全无采集记录时数据新鲜度跳过（最近采集一项已告警）。
- 汇总行（`Result / 结果`）为 `OK / 一切正常`、`N warnings / N 项警告` 或 `N problems / N 项失败`（FAIL 优先于 WARN）。
- v1 退出码恒为 0：FAIL/WARN 仅体现在报告。

示例：

```bash
token-usage doctor
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

> `config get` 与 `config show` 职责不同：前者读用户配置层原值（不展开 `~`、不补默认值），后者输出完整 effective TOML（展开 `~`、补默认值/默认路径）。要查看运行时生效配置请优先用 `config show`；`daemon status`/TUI 只作为人机可读摘要。

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

读取的是**用户配置层原值**：即配置文件中显式写入的值，不展开 `~`、不补默认路径、不 clamp 数值。因此未在文件中显式配置的字段返回零值（如未写的 `poll_interval` 返回 `0`）。要查看运行时实际生效值（展开 `~`、补默认值/默认路径的完整配置）请用 `config show`；`daemon status` 与 TUI 仅作人机可读摘要。

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

**router 拦截：** `config set clients.<name>.router <value>` 在 `<value>` 非空且 `<name>` 不是 router 支持客户端（当前为 Claude 与 Codex）时直接报错拒绝写入，退出非零；设为空字符串表示清除，始终放行。读取链路（`config show`、采集、daemon）对存量配置中其他客户端的非空 router 仍容忍。

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

要让当前会话生效，需手动 `daemon stop` + `daemon start`（或 `daemon restart`）。二者彻底解耦的详细说明见「[daemon](#daemon)」。

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

## daemon

`daemon` 命令组管理**采集/分析守护进程**——后台监控各 AI 客户端会话日志、持续保持用量数据更新的进程。它的四个动作只管控**当前运行的守护进程**，与**开机自启定义**（下次登录/开机是否自动启动）彻底解耦。仪表板服务是另一个独立程序实例，由 [serve](#serve) 命令组单独管理；两者不共享 PID、锁、状态文件、日志与端口，停止或重启其中一个绝不影响另一个。裸执行 `token-usage daemon` 只打印命令组帮助：不启动任何进程，也不创建状态文件。

| 命令 | 作用 | 是否触碰 autostart 定义 |
|------|------|--------------------------|
| `daemon start` | 后台启动守护进程，完成监听 ready 握手后返回；已在运行则幂等返回当前 PID | 否 |
| `daemon stop` | 停止当前守护进程（不删 plist/注册表）；未运行则幂等返回 | 否 |
| `daemon restart` | 在单次进程控制锁内停旧起新；未运行报错并提示用 `daemon start` | 否 |
| `daemon status` | 只读查看运行状态 + 开机自启漂移检测（5 态分类） | 否（只读） |

> 这些命令均不修改 config、plist 或注册表。autostart 定义由 `config set daemon.autostart` 或 TUI 保存触发收敛。

### daemon start

```text
token-usage daemon start
```

经 `control.Manager.Start`：在进程控制锁内加载配置 → 以 daemon lock 判活 → 已运行返回当前 PID（不重复 spawn，退出码 0）→ 未运行 detached spawn `_run` 子进程 → 在 5 秒内等待六项 ready 条件（PID 文件的 PID/instanceID、daemon lock、runtime-state 的 PID/instanceID/`monitor_ready=true`）→ 输出 `✓ 守护进程已启动（PID N）`。超时会尽力终止本次子进程；仅在 daemon lock 已释放且元数据仍属于本代时清理，避免误删活进程或其他代次的文件。

stdout：成功行（含幂等的「已在运行」）；stderr：真实失败。

### daemon stop

```text
token-usage daemon stop
```

经 `control.Manager.Stop`：进程控制锁内加载配置 → daemon lock 判活 → 未运行幂等返回「守护进程未运行」→ 运行中按平台停止（macOS：始终先尝试对当前 label 执行幂等 `bootout`，若 daemon lock 仍持有再对已读取的准确 PID 发 SIGTERM；Windows：taskkill 精确 PID）→ 以 **daemon lock 释放**为成功条件（轮询 5s），不靠删 PID 文件伪装成功。

stop **不删除** plist/注册表定义：当前会话停止，下次登录仍按 autostart 配置启动。关闭自启请用 `config set daemon.autostart false`。

### daemon restart

```text
token-usage daemon restart
```

经 `control.Manager.Restart`：单次进程控制锁内 stop 旧 + start 新。守护进程**未运行**时返回 `ErrRestartNotRunning`（stderr 含「请使用 token-usage daemon start」），退出非零。

macOS 取舍：stop 会尝试 bootout 当前 job，随后以 detached 方式 start；plist 定义保留，但本次登录会话不再由 launchd KeepAlive 托管。由于保存配置只维护定义文件、不会主动 bootstrap，KeepAlive 会在下次登录加载该定义时恢复。

### daemon status

```text
token-usage daemon status
token-usage daemon status --format json
```

`--format json` 把同一份快照输出为单个机器可读的 JSON 文档：来自 daemon lock 的 `running`/`pid`、`startup_phase`（未运行时为 `null`；含 `available`、`monitor_ready`、`catch_up`——runtime-state 的未知值降级为 `unknown`——与 `catch_up_failures`）、`data_dir`、`poll_interval_seconds`，以及 `autostart`（`configured`、`definition_exists`、`spec_matches`、布尔 `detect_failed`、携带原因的 `detect_error`，`status` 取封闭值域：`enabled`/`missing`/`drift`/`residual`/`disabled`，检测不可行时为 `unknown`）。`startup_phase` 中阶段元数据不可得（`available=false`）与未知 `catch_up` 值都会降级为 `catch_up: "unknown"`。

只读（`Inspect` 不抢进程控制锁，仅以 daemon lock 判活），返回一致快照：

- 运行状态：`● 守护进程运行中（PID N）` 或 `○ 守护进程未运行`。
- 启动阶段（运行中时追加一行）：`监听初始化中` / `监听已就绪，正在补采` / `补采部分失败（N），请执行 token-usage errors`；catch-up 成功不额外打印；PID 元数据不可用或阶段不匹配时降级为「启动阶段未知」。
- 数据目录、轮询间隔。
- 开机自启漂移检测（5 态）：已启用 / autostart=开但定义缺失 / 内容不一致 / autostart=关但定义残留 / 未启用。漂移只提示「建议重新保存配置」，不触发任何写操作。

autostart 只表达「下次登录/重启是否自动启动」，与当前 daemon 是否运行相互独立；当前 daemon 状态单独展示，两者不互相推断。

### startup catch-up（关闭 daemon stop→collect→daemon start 数据窗口）

`daemon start` 建立监听后，守护进程会执行一次 **startup catch-up**，补齐「最后一次手工 `collect`/`collect all`」到「监听 ready」之间新增的数据，从而关闭 daemon stop→collect→daemon start 的数据窗口。

顺序契约（`daemon.startupCoordinator`）：

1. 等待 analyzer 所有 monitor 就绪（ready barrier）；ctx 取消则不写 state、不 catch-up。
2. 写 ready state（`monitor_ready=true, catch_up=pending`）。
3. 写 running state（`catch_up=running`）；写入失败时记录日志并继续，不停止 daemon。
4. 顺序 Submit catch-up 请求：按已启用 client 名升序，每个 client 先发 client-source 请求（opencode/zcode 走增量 cursor；claude/workbuddy/autoclaw 无日期扫现存 JSONL；codex 先 state 增量再 rollout 全扫），再发该 client 的 router 增量请求（若配置）。
5. 写 final state：0 失败 = `succeeded`，否则 = `failed` + 准确失败数。

catch-up 经 analyzer 的串行化锁 Submit（与实时触发同一路径，保证顺序与互斥）。因此只要 daemon 成功启动并完成 catch-up，daemon stop→collect→daemon start 之间产生的增量数据会被补采，不会因「监听未就绪」而遗漏。catch-up 部分失败会在 `daemon status` 与 `errors` 中体现。

### _run（Hidden）

内部命令，由 `daemon start` detached spawn、或 launchd / Windows 注册表 Run 键直接拉起，执行守护进程主循环。用户不应直接调用（`--help` 不可见）。两条启动路径都满足不变量「从读取 effective config 到获取 daemon lock 期间始终存在 control lease」：

- 父 lease 路径（`daemon start` spawn 的 `_run`）：父进程持进程控制锁并通过 pipe lease 授权 child，child 不抢锁。
- 独立路径（launchd/注册表直接拉起）：无合法父 lease 时自行获取进程控制锁（15s 超时；超时则成功退出码 0 不进入主循环，避免与正在进行的控制操作冲突，并在 macOS 上避免 launchd KeepAlive 立即重拉）。

## watch

渲染某个窗口的 `query` 输出并以固定间隔刷新，直到 Ctrl+C 中断。帧体与 query 输出完全一致——统计信息区、应用 `[query.output.columns]` 布局与 `provider_aliases` 的视图表，以及采集异常警告——视图选择与 `query` 同一规则：不带 `--by` 时执行默认视图（`query.default`，内置回退 `client`）；watch 只额外加上 `Live watch` 横幅、固定间隔刷新与帧间清屏。

```bash
token-usage watch                      # 今天，默认视图，每 5 秒刷新
token-usage watch 20260901 --interval 10s
token-usage watch --by group           # query.groups 中已配置的视图名
token-usage watch --once               # 只渲染一帧后退出（对管道友好）
```

- `--by` 选择帧内视图：内置视图（`client`、`model`、`provider`、`project`、`day`、`month`、`hour`、`weekday`、`heatmap`、`session`、`summary`）或 `query.subqueries` / `query.groups` 中已配置的视图名。不带 `--by` 时执行默认视图——`query.default`（内置回退 `client`），已配置组合查询时每帧渲染全部成员表。显式内置名与 `query` 静态子命令一致，忽略无关的视图定义错误；不带标志与配置视图名路径走完整 query 校验，与裸 `query` 同样以本地化诊断失败。未知 `--by` 值在打开数据库之前按动态允许集合拒绝。
- 日期参数与 `query` 同形态；不带日期时帧跟随今天，每次刷新重算，跨午夜自动切换；显式指定的日期或区间保持固定（监视历史区间是合法用法）。
- `--interval` 接受 Go 时长，下限 1 秒；更小的值在打开数据库之前即被拒绝。
- 交互式循环在每帧之间清屏（Windows 控制台会自动启用虚拟终端处理）；`--once` 只渲染一帧且不含转义序列，重定向输出保持纯文本。
- 严格只读：与其他读取类命令相同的开库语义，不与守护进程交互，Ctrl+C 不残留任何状态。

## serve

管理提供内嵌仪表板的只读本地 HTTP 服务：`/` 的内嵌 HTML 页面、JSON 接口（`/api/meta`、`/api/dashboard`）与 SVG 图表（`/api/chart/{kind}.svg`），图表与内嵌页面共用同一构建核（标题、副标题与悬停文案完全一致）。仪表板**始终以后台方式运行**：`serve start` 拉起 detached 服务进程后返回，`serve status` / `serve stop` 查看与停止，`serve restart` 以全新后台实例接管运行中的实例。裸执行 `token-usage serve` 只打印命令组帮助——不监听端口、不启动进程、不创建状态文件。采集守护进程是另一个独立程序实例，由 [daemon](#daemon) 命令组单独管理；两者不共享 PID、锁、状态文件、日志与端口。

HTTP 数据面严格只读——不设 CORS 头、不写数据库与配置；`serve.json` 是共用的生命周期状态，`serve.log` 用于后台日志，`serve.lock`、`serve-state.lock` 与 `serve-start.lock` 负责生命周期协调。

- `--addr` 修改监听地址（默认 `127.0.0.1:8619`），作用于 `serve start` / `serve restart` 新启动的实例。绑定 `0.0.0.0` 等公网地址**会把用量数据暴露给局域网**——服务只读但无鉴权——请保持回环绑定。
- `--open` 在 `serve start` / `serve restart` 确认后台服务就绪后用默认浏览器打开仪表板；打开浏览器失败只是警告，服务继续运行。
- 启动时写 `serve.json` 状态文件失败（如数据目录只读）服务即报错退出，不做无状态运行。
- 同一请求的全部数据查询共享一个读快照，并发采集写入下 totals、维度行与会话行相互一致。
- 内嵌页面全部由前端按这些数值行自绘：KPI 卡（相对基线窗口的增减 chips）、对齐所配置 query 输出列的指标条（除已升格 KPI 卡的 requests/total/cache-hit 外的 token 类别列——默认布局即输入/输出/缓存读/推理，布局含缓存写时才会出现该列）、堆叠/单系列柱状图（点击按天/按月柱条即聚焦对应区间；超过 92 天按天柱自动按 ISO 周聚合并停用钻取）、占比环形图一行四张、带行列合计的星期×小时热力矩阵（对齐 `query heatmap` 的尾行/尾列合计）、带 token 占比条与 CSV 导出的会话排行、逐维度数据表（可排序、一键导出 CSV——按当前行序、精确整数）、整行环比对比（左侧逐日对比曲线、右侧指标表）与整行预估、范围预设与自定义起止日期（首次进入默认 Today、跨刷新记忆）以及自动刷新。按天桶不足 2 个时隐藏按天维度图、按月桶不足 2 个时隐藏按月维度图，单日选区只保留按小时图（星期视图对单日无意义）并隐藏环比对比表——单桶形态不携带信息——KPI 增减 chips 仍指向基线窗口（单日即前一日）。

| 接口 | 参数 | 返回 |
|------|------|------|
| `GET /api/meta` | — | 版本、`min_date`/`max_date`（全库）、`data_through`、`last_collection`；后三项缺数据时为 `null` |
| `GET /api/dashboard` | `from`、`to`（`YYYY-MM-DD`；缺省为截至今天的 30 天；跨度至多 366 天） | 统计区间、totals（整数，含 `active_days`）、compare（基线窗口按所选区间推导——结束于区间开始日前一天的等长窗口，单日区间退化为前一天；含基线 totals、8 行预计算行（显示串、带符号变化、pos/neg 着色 class，基线为 0 时变化% 显示 `--`），以及 `daily`——基线窗口逐日行（按窗口缺口填充、键即日期、纯整数，供前端绘制当前 vs 基线逐日对比曲线））、forecast（固定回看窗口：`today_so_far` 与恒 2 行的最近 7/30 天——窗口不含今天，日均按活跃天整数除法，预估为日均×未来天数；显示串预计算，窗口无数据时各格显示 `—`；不随 `from`/`to` 选区变化）、8 个固定维度行数组（`day`/`hour`/`weekday`/`month`/`client`/`model`/`provider`/`project`）、前 10 条会话，以及 `heatmap`——7×24 的 token 矩阵（`weekdays` 为 ISO 周序周一在首、`hours` 为 `00:00`..`23:00`、`values` 为 7×24 数组，空交点为 `0`），与总量同一读快照读取，单次刷新不可能混用快照；内嵌页面全部图表由前端按这些数值行自绘，`GET /api/chart/{kind}.svg` 仍可独立取图 |
| `GET /api/chart/{kind}.svg` | 日期参数与 `/api/dashboard` 一致；`kind` ∈ `day`/`hour`/`weekday`/`month`（柱状）、`client`/`model`/`provider`/`project`（饼图）、`heatmap` | 一份 SVG 文档（`image/svg+xml`） |
| `GET /`、`GET /assets/…` | — | 内嵌 HTML 页面与静态资产（`Cache-Control: no-store`） |

- 错误统一为 JSON `{"error":{"message":"…"}}`：参数非法 `400`、图表类别或资产不存在 `404`、查询失败 `500`。
- 数据面严格只读：不设 CORS 头（按同源使用）、不与守护进程交互、不写数据库与配置。持久状态为 `serve.json`，后台输出写入 `serve.log`，`serve.lock`、`serve-state.lock` 与 `serve-start.lock` 负责生命周期迁移协调。维度行为原始整数，K/M/B 格式化交给前端；`provider` 行与查询视图一样应用 `[provider_aliases]`。

### serve start / serve status / serve stop / serve restart（后台，nginx 风格）

`serve start` 拉起一个 detached 子进程并在其报告就绪后返回，`serve status` 查看状态，`serve stop` 停止——这是运行仪表板的唯一方式。

```bash
token-usage serve start
token-usage serve start --addr 127.0.0.1:9000
token-usage serve start --open
token-usage serve status
token-usage serve stop
token-usage serve restart
```

- 状态文件：数据目录下的 `serve.json`（默认 `~/.token-usage/serve.json`），在服务完成监听时原子写出 `{"pid":…,"addr":…,"started_at":…}`（记录的 `addr` 为实际绑定的地址）。优雅停止时自动删除（`serve stop`，或 restart 的停止段）；崩溃或 `SIGKILL` 遗留的文件由 `serve status`/`serve stop` 的陈旧探活与下一次 `serve start` 的单实例守卫兜底删除。状态迁移由数据目录下的 `serve-state.lock` 文件锁串行化，陈旧清理为条件删除：只有与判定所据内容仍一致的陈旧状态才会被移除——若新实例已接管，其新写出的 `serve.json` 绝不会被误删。
- 日志文件：数据目录下的 `serve.log`（默认 `~/.token-usage/serve.log`）。每次 `serve start` 都会截断；子进程的 stdout 与 stderr 都写入其中，为纯文本（无终端超链接）。启动失败时错误信息会附带日志末尾 10 行。
- `serve start` 在已记录状态于 `/api/meta` 上仍有响应时报告已在运行并以退出码 0 幂等返回（要重启请用 `token-usage serve restart`，或先 `token-usage serve stop` 停止）；不再响应的陈旧状态与损坏的状态文件会被删除并照常启动。子进程 5s 内未就绪则启动失败，并指向日志末尾。并发的 `serve start` 由数据目录下的 `serve-start.lock` 文件锁串行化（仅用于启动协调——运行中的实例由 `serve.json` 描述、以 `serve.lock` 生命周期锁持有）：另一个 start 尚在执行时，第二个以非零退出码报错并提示稍后重试。
- `serve status` 的所有状态结论均以退出码 0 返回（只有意外的 I/O 失败才非零）：`/api/meta` 有响应时报告 URL、PID 与启动时间；无响应（或状态文件损坏无法辨识）时删除陈旧/损坏文件并报告未运行。状态迁移由 `serve-state.lock` 串行化；锁被并发的 `serve status`/`serve stop` 持有超过带界重试窗口时，命令以非零退出并提示稍后重试。
- `serve status --format json` 把同一判定输出为机器可读文档（两空格缩进 + 尾随换行，与 `doctor --format json`、`daemon status --format json` 同一约定）：`state` 取封闭值域——`running`、`not_running`、`not_running_stale_removed`、`not_running_corrupt_removed`；`running` 是 state 对应的布尔值；`pid`/`addr`/`url`/`started_at`（RFC3339，serve.json 原值）仅在运行中出现；`data_dir` 为配置的数据目录。非法 `--format` 值报错并列出允许值，与 `daemon status` 同型。
- `serve stop` 在 Unix 上发送 SIGTERM 并给 3s 优雅窗口，超时以 SIGKILL 兜底；在 Windows 上使用 `taskkill /F`——Windows 控制台进程没有跨进程的优雅停止通道，对严格只读的服务可接受。是否停止成功仅以 `/api/meta` 不再响应为准（记录的 PID 可能已被无关进程复用，探活的结论优先于信号发送结果——信号投递失败也不会短路探活等待）。只有探活确认下线（或信号发送前就无响应——陈旧/损坏状态被清理）才会删除状态文件。若强杀兜底后服务仍在响应（无论强杀本身是否报错），命令以非零退出码报错并列出记录的 URL 与 PID，保留 `serve.json` 供人工检查进程/端口。若停止进行期间有新实例接管（旧实例下线后状态文件被改写），命令会如实说明并转而停止新实例，而不是报告旧实例已停止。对已停止的服务重复执行是幂等空操作，退出码仍为 0。
- `serve restart` 以与 `serve stop` 完全相同的编排停止运行中的实例（以探活为判据），随后以与 `serve start` 完全相同的编排拉起全新后台实例（`--addr`/`--open` 作用于新实例）。当前没有实例在运行时等价于直接启动。若运行中的实例在 SIGKILL 兜底后仍在响应，重启以非零错误中止——旧实例继续服务，此类场景请用 `serve stop` 排查。
- 单实例契约：任意时刻至多一个仪表板实例在运行。第二个 `serve start`——无论请求哪个地址——都会在监听之前被单实例守卫拒绝：打印运行中实例的 URL 与 PID 并以退出码 0 幂等返回（要重启请用 `token-usage serve restart`，或先 `token-usage serve stop` 停止）；若撞上另一实例正在启动的窗口，守卫报错并提示稍后重试。服务主体在其整个生命周期持有数据目录下的 `serve.lock` 生命周期锁。由于守卫先于监听执行，与运行中实例的同端口冲突不会再表现为监听失败——监听失败只剩「请求的端口被一个没有留下 `serve.json` 记录的无关进程占用」这一种场景。因此 `serve status` / `serve stop` 始终管理唯一实例。
- `--open` 由 `serve start` 与 `serve restart` 支持：仅在确认后台服务就绪后打开浏览器（打开失败只是警告）。

## update

从官方 GitHub Release 原地更新 `token-usage` 二进制。CLI 只解析参数、装配依赖、格式化结果，自更新核心位于 `internal/update`（见[架构设计](architecture.zh-CN.md)）。

`update` 执行期间会逐步输出过程：先打印「正在检查更新…」行，发现新版本即给出当前/目标版本对（先于来源校验，拒绝路径同样可见），随后依次输出下载、校验、停止 daemon 与 dashboard、安装、重启二者的步骤行。交互终端上下载会渲染单行实时进度（百分比、已传输/总字节数、平均速度）；stdout 被重定向或接管道时省略进度行、只保留步骤行，下载失败也总会干净地收尾换行。更新前正在运行的 daemon 会被自动停止并用新二进制重启。dashboard 以同样方式保持：运行中的 dashboard（对 `/api/meta` 有应答）会在替换前被停止——Windows 上这一步同时释放旧 `.exe` 供后台 helper 完成替换——替换完成后以原监听地址、用新二进制后台恢复；自动恢复绝不打开浏览器。只有有应答的 dashboard 才算在运行：缺失、损坏或陈旧的 `serve.json` 一律按未运行处理且不被改动。dashboard 无法停止时更新中止、二进制保持原样；其后任一步失败时更新回滚到旧二进制与更新前的 daemon/dashboard 运行态，并同时保留主失败与回滚失败信息。`update --check` 只输出检查行与结果；它绝不读取、停止、启动或清理 daemon/dashboard 运行态。

```text
token-usage update
token-usage update --check
token-usage update --version <tag>
token-usage update --force
```

| 形式 | 作用 |
|------|------|
| `update` | 更新到最新稳定 Release。若当前二进制同目录存在一次中断的 POSIX 更新留下的受限事务 journal，先完成恢复（journal 同时记录 dashboard 是否在运行及其监听地址，恢复时一并还原）；之后仅当目标严格高于当前版本且当前来源可信时才继续新替换：下载资产、与 `SHA256SUMS` 清单比对 SHA256、stage `--version` 二次校验、替换二进制。更新前正在运行的 daemon 会用新二进制自动重启；原本已停止的 daemon 保持停止，成功输出会提示 `token-usage daemon start`。更新前正在运行的 dashboard 同样会用新二进制按原监听地址后台恢复；更新的生命周期动作与 `serve stop`/`serve start` 复用同一套内部编排（不经 CLI 子进程、不经 shell），恢复绝不打开浏览器，Windows 上停止 dashboard 同时释放旧 `.exe` 供后台 helper 完成替换。 |
| `update --check` | 只读检查；不创建任何本地文件（不创建配置目录/锁/日志/数据库/服务定义）。 |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | 更新（或加 `--check` 后仅检查）指定精确版本 tag。`--version` 接受严格 Release tag（`v` 前缀、`MAJOR.MINOR.PATCH`、可选 `-rc.N`、无前导零）；非法值在任何网络请求前即报错。 |
| `update --force` | 当前二进制来源非官方 Release 资产时仍强制覆盖，仅限两种豁免：与所报告版本官方资产 hash 不一致（按安装指引重签过的二进制、或 `go install pkg@vX.Y.Z` 产物），以及 dev 本地构建（`Version = dev`，或直接构建伪版本归一显示的 `vX.Y.Z-dev`——两种形态同判）。全部结构检查与目标资产的 SHA256 / stage `--version` 校验照常执行；软链副本与非官方 tag 不可被 force。 |

`--check` 与 `--version` 可组合，如 `update --check --version vX.Y.Z-rc.N` 只检查候选版。`--force` 不能与 `--check` 组合（该组合被显式拒绝）。

标志：

- `--check`（bool）：只读检查，不写本地文件。
- `--version`（string）：目标 Release tag。接受 `vMAJOR.MINOR.PATCH` 与 `vMAJOR.MINOR.PATCH-rc.N`（无前导零，`N >= 1`，无 build metadata）。
- `--force`（bool）：当前二进制非官方 Release 资产（已重签、go install、dev 本地构建）时仍强制覆盖；确切豁免边界见[信任与来源校验](#信任与来源校验)。

`update` 不接受位置参数（`Args: NoArgs`）。

### 稳定版 / RC 选择

默认 `update` 只解析最新**稳定** Release，绝不选择 prerelease。只有用 `--version` 显式指定 rc tag（如 `--version vX.Y.Z-rc.N`）时才会查询/安装预发布版。

本地版本严格高于所请求或最新的 Release 时（rc 领先稳定通道，或显式 `--version` 请求更低版本），`update` 与 `update --check` 都会报告本地/目标版本对，且不做任何变更。

### 补全迁移提示

`update` 成功跨越补全自动配置功能的引入版本——当前版本低于该版本（不可解析的本地构建版本——`dev` 或 `vX.Y.Z-dev` 显示——视为低于）且目标为该版本或更高——时，成功输出追加一条一次性迁移提示，按平台给出官方安装命令。重跑一次安装脚本即可自动配置补全（zsh 会交互确认）。Windows 后台替换（Deferred）出口的提示要求先用 `token-usage version` 确认最终版本再重跑安装脚本，避免两者竞争同一二进制。提示无状态：每次跨越门槛都会打印，显式 `--version` 降级后再升级会再次出现。`update --check`、全部失败与拒绝分支、以及中断事务恢复（Recovered）出口均不打印。

### 信任与来源校验

`update` 仅在目标严格更高且当前来源可信时才覆盖当前二进制。满足以下任一条件即判定当前来源**不可信**（默认 `update` 拒绝覆盖，输出人工安装指引）：

- 当前 `Version` 为 `dev` 或伪版本（如来自 `make build`、`make build-all` 或 `go install`）；
- 当前二进制不是普通文件，或为 symlink；
- 当前二进制的 SHA256 与当前版本的官方资产 hash 不一致（如按安装指引重签过的二进制、`go install pkg@vX.Y.Z` 产物）。

拒绝携带 `--force` 出口，但仅限两种豁免：

- **hash 失配**（当前版本存在官方 Release 与清单，但本地内容不一致）：用 `--force` 再次执行即用官方资产覆盖，自动更新恢复正常；
- **dev 本地构建**（`Version = dev`，或直接构建伪版本归一显示的 `vX.Y.Z-dev`——两种形态 `update --force` 同等接受；不存在可比的官方 Release 与清单，从未发生 hash 比较）：`update --force` 把安装切换为官方 Release 资产。

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

Windows 上替换运行中的 `.exe` 受限，自更新把替换交给后台 helper 后返回。helper 成功启动后，命令会明确说明「后台替换已排队」，以 `0` 退出，并提示稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本，**不声称已完成**。更新前 daemon 已停止时，输出会要求先确认替换完成再运行 `token-usage daemon start`——提前启动会使 helper 放弃替换（daemon 运行期间它拒绝改动二进制）。macOS/POSIX 为同步原子替换（同目录 backup + rename + fsync，失败回滚 + 下一次 `update` 调用按 journal 恢复）。

## 配置文件

路径固定 `~/.token-usage/config.toml`（TOML，可手工添加注释）。所有客户端默认关闭：用 `clients.<name>.enabled = true` 开启需要的客户端，数据源路径由程序按各工具默认位置自动填充。用 dotted key 同段写法覆盖默认。`config set`/TUI 保存会完整重写配置，故不保留原有注释和 map 键书写顺序；完整字段与默认值见 `token-usage config init` 生成的模板。

`data_dir` 决定数据文件位置（`usage.db`、日志、PID、runtime-state、锁）；配置文件路径不随 `data_dir` 变化。`daemon.autostart` 控制开机自启（macOS launchd / Windows 注册表）。
