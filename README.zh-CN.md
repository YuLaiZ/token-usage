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
- **可配置化**：数据源路径、客户端开关、路由均可配置，开箱即用默认值
- **单二进制分发**：Go 编译（纯 Go SQLite，无需 CGO），支持 macOS 与 Windows

## 快速开始

### 安装

任选下列一种安装方式。推荐方式 A（官方 Release 二进制）：无需 Go 环境、一条命令即可安装官方 Release 二进制——唯一支持原地自更新的来源。所有文件统一收纳于 `~/.token-usage`——配置、数据库、日志与二进制（`~/.token-usage/bin/token-usage`）——经用户 PATH 暴露命令，安装与更新全程无需 sudo（Windows 无需管理员权限）。

#### 方式 A：官方 Release 二进制（推荐——支持自更新）

**复制给 AI Agent**（它读到这段指令后自行完成安装）：

```text
请在本机安装 token-usage CLI：从 github.com/YuLaiZ/token-usage/releases 下载与当前
平台匹配的官方 Release 二进制（macOS Apple Silicon 用 token-usage-darwin-arm64，
macOS Intel 用 token-usage-darwin-amd64，Windows 用 token-usage-windows-amd64.exe），
用该 Release 的 SHA256SUMS 校验 SHA256，安装为真实文件
~/.token-usage/bin/token-usage（Windows 为
%USERPROFILE%\.token-usage\bin\token-usage.exe），并把 bin 目录（macOS 为
~/.token-usage/bin，Windows 为 %USERPROFILE%\.token-usage\bin）加入用户
PATH：macOS 向 shell rc 文件追加 export PATH="$HOME/.token-usage/bin:$PATH"
（zsh 写 ~/.zshrc；bash 写登录 shell 读取的第一个文件）；Windows 用
Microsoft.Win32.Registry 保类型注册表直写用户 Path 值，保留 REG_EXPAND_SZ
值类型与 %VAR% 原文——不得使用 setx 或 [Environment]::SetEnvironmentVariable——
再广播 lParam 为 `Environment` 的 WM_SETTINGCHANGE；广播失败时，先注销并重新
登录再新开终端。然后新开终端（Windows 从开始菜单/任务栏启动新窗口）运行
token-usage version 确认（运行 token-usage --help 查看可用命令）。
```

或手动执行命令：

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

脚本自动检测 CPU 架构、下载最新官方 Release（含预发布）、按官方 `SHA256SUMS` 校验后无需 sudo 安装到 `~/.token-usage/bin/token-usage`，并自动清理旧布局安装位置的遗留副本（目录不可写时给出手动移除指引，该位置被目录占用或删除失败等其他情形打印对应人工处理指引，均不影响安装），再向 shell rc 文件追加 marker 块把 `~/.token-usage/bin` 加入用户 PATH（仅支持 zsh 与 bash——zsh 写 `~/.zshrc`；bash 写登录 shell 读取的第一个文件——`~/.bash_profile` 优先，其次 `.bash_login`、再次 `.profile`；其他 shell 下脚本会打印人工 PATH 配置指引）。新开终端运行 `token-usage version` 确认。

> 非登录交互 shell 环境（部分 IDE 集成终端读取 `~/.bashrc` 而非登录文件）不会加载登录文件，需要时请自行补一行 `export PATH="$HOME/.token-usage/bin:$PATH"`；zsh 交互终端用户全部命中 `~/.zshrc`。

指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | TAG=v0.1.0-rc.12 bash
```

已发布的资产：

- `token-usage-darwin-arm64`（macOS Apple Silicon）
- `token-usage-darwin-amd64`（macOS Intel）
- `token-usage-windows-amd64.exe`（Windows）

Windows——或手动执行命令（先保存脚本再执行）：

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"; powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

脚本下载最新官方 Release、按官方 `SHA256SUMS` 校验后无需管理员权限安装到 `%USERPROFILE%\.token-usage\bin\token-usage.exe`，并以保类型注册表直写把 `%USERPROFILE%\.token-usage\bin` 追加到用户 PATH（保持既有 `REG_EXPAND_SZ` 值类型与 `%VAR%` 条目原文）。从开始菜单/任务栏启动新终端窗口，运行 `token-usage version` 确认。指定版本：对已下载的脚本以 `-Tag` 运行：

```powershell
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1" -Tag v0.1.0-rc.12
```

> 旧 Windows 环境（TLS 1.2 以下）下第一步 `irm` 仍发生在脚本执行之前，脚本内的 TLS 兜底救不了它：请先在当前会话执行 `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12`，再运行第一步下载。

Windows 手动安装步骤（下载 → SHA256 校验 → 把二进制放进 bin 目录 → 加入用户 PATH）见下方方式 B。

从官方 Release 安装的二进制——无论经上方脚本、AI Agent 指令，还是方式 B 的官方资产路径手动安装——都可原地自更新：二进制是 PATH 上的真实文件，位于 `~/.token-usage/bin`，正是自更新来源校验所要求的形态。

```bash
token-usage update                  # 更新到最新稳定版
token-usage update --check          # 只检查，不写任何本地文件
token-usage update --version v0.2.0 # 更新（或检查）指定版本 tag
```

完整标志、退出码与副作用边界见 [CLI 参考](docs/cli.zh-CN.md)。

> **开发构建不能自动更新**：`make build`、`make build-all` 或 `go install` 产物的 `Version` 为 `dev`（或伪版本），`update` 会判定来源不可信并给出人工安装指引，不会原地覆盖。
>
> **自更新支持平台**：`darwin/arm64`、`darwin/amd64`、`windows/amd64` 有官方资产；其他平台 `update` 会提示「无官方资产，请手动安装」。
>
> **人工升级边界**：`update` 只在当前二进制是所报告版本的官方 Release 资产时才覆盖（当前二进制的 SHA256 必须等于该版本官方资产的 hash）。若当前二进制来自 `go install`/本地构建/软链，或版本/hash 不匹配，`update` 不会覆盖，而是输出人工安装指引。
>
> **为何不软链进 PATH**：自更新来源校验要求当前可执行文件是真实二进制文件；经软链调用时解析到软链路径会被拒绝（即上方边界中的「软链副本」）。因此本布局直接把 `~/.token-usage/bin` 加入 PATH，而不是在其他目录放置链接。
>
> **中断更新恢复**：来源安全门约束的是新的 Release 下载。若此前 POSIX 更新遗留了受限的本地事务 journal，后续 `update` 会先把这笔已记录的事务恢复为一致状态；恢复期间不会接受或下载新的二进制。

卸载或从旧布局迁移见下方[卸载与迁移](#卸载与迁移)。

#### 方式 B：手动二进制安装（同布局）

方式 B 手动完成与方式 A 相同的布局——二进制位于 `~/.token-usage/bin/token-usage`（Windows 为 `%USERPROFILE%\.token-usage\bin\token-usage.exe`），bin 目录加入用户 PATH。两条子路径的升级语义不同：

- **官方 Release 资产**（下方含 SHA256 校验的下载步骤）：与方式 A 等价，支持原地自更新。
- **源码构建**（`make build` / `go build`）：`Version=dev` 或伪版本，不能自更新；升级=重新构建后手动替换文件（见上方开发构建警示）。

**macOS——官方 Release 资产**：

```bash
# latest 链接需要稳定版存在；当前仅有预发布（prerelease）时它返回 404。
# 请用 Releases 页面最新 tag 的下载链接（下方示例固定为 v0.1.0-rc.12）：
curl -fsSL -o token-usage-darwin-arm64 https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/token-usage-darwin-arm64
curl -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/SHA256SUMS
# 按该 Release 的 SHA256SUMS 校验 SHA256：
shasum -a 256 -c SHA256SUMS --ignore-missing
chmod u+x token-usage-darwin-arm64
mkdir -p ~/.token-usage/bin
mv token-usage-darwin-arm64 ~/.token-usage/bin/token-usage
```

再把 `~/.token-usage/bin` 加入 PATH：向 shell rc 文件追加以下块（zsh 写 `~/.zshrc`；bash 写登录 shell 读取的第一个文件——`~/.bash_profile` 优先，其次 `.bash_login`、再次 `.profile`）：

```sh
# >>> token-usage path >>>
export PATH="$HOME/.token-usage/bin:$PATH"
# <<< token-usage path <<<
```

> 非登录交互 shell 环境（部分 IDE 集成终端读取 `~/.bashrc` 而非登录文件）不会加载登录文件，需要时请自行补一行 `export PATH="$HOME/.token-usage/bin:$PATH"`；zsh 交互终端用户全部命中 `~/.zshrc`。

新开终端，运行 `token-usage --help` 与 `token-usage version` 验证。

**Windows——官方 Release 资产**：

```powershell
# 从 Releases 页面最新 tag 下载（下方示例固定为 v0.1.0-rc.12），
# 再按该 Release 的 SHA256SUMS 校验 SHA256：
curl.exe -fsSL -o token-usage-windows-amd64.exe https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/token-usage-windows-amd64.exe
curl.exe -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/SHA256SUMS
# 按该 Release 的 SHA256SUMS 校验 SHA256：若没有本资产的精确条目或 hash 不匹配，
# 必须在安装前中止：
$sumsEntry = Select-String -Path SHA256SUMS -Pattern '^[0-9a-fA-F]{64}  token-usage-windows-amd64\.exe$' | Select-Object -First 1
if ($null -eq $sumsEntry) { throw 'SHA256SUMS 中未找到 token-usage-windows-amd64.exe 的 hash。' }
$expected = ($sumsEntry.Line -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash .\token-usage-windows-amd64.exe).Hash.ToLower()
if ($actual -ne $expected) { throw "SHA256 MISMATCH: 期望 $expected，实际 $actual" }
'SHA256 OK'
New-Item -ItemType Directory -Force $env:USERPROFILE\.token-usage\bin | Out-Null
Move-Item token-usage-windows-amd64.exe $env:USERPROFILE\.token-usage\bin\token-usage.exe -Force
```

再用保类型注册表直写把 `%USERPROFILE%\.token-usage\bin` 追加到用户 PATH，保持既有 `REG_EXPAND_SZ` 值类型与 `%VAR%` 条目原文。下方「已含」检测会先把未展开条目（如 `%USERPROFILE%\.token-usage\bin`）展开后再匹配，与安装脚本语义一致——展开匹配仅对 `REG_EXPAND_SZ` 值生效。**不得**使用 `setx` 或 `[Environment]::SetEnvironmentVariable`：`setx` 会截断超长值，`SetEnvironmentVariable` 会把值以 `REG_SZ` 写回、`%VAR%` 条目被永久展开。

```powershell
$dir  = "$env:USERPROFILE\.token-usage\bin"
$key  = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$raw  = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
$kind = if ($key.GetValueNames() -contains 'Path') { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
$norm = ($raw -split ';') | ForEach-Object {
    $lit = $_.Trim().TrimEnd('\')
    $exp = if ($kind -eq 'ExpandString') { [Environment]::ExpandEnvironmentVariables($_).Trim().TrimEnd('\') } else { $lit }
    @($lit, $exp)
}
if ($norm -notcontains $dir) {
    $new = if ([string]::IsNullOrEmpty($raw)) { $dir } else { $raw.TrimEnd(';') + ';' + $dir }
    $key.SetValue('Path', $new, $kind)
}
$key.Close()
```

> 注册表直写不会广播环境变更：请注销重登，再从开始菜单/任务栏启动新终端窗口，运行 `token-usage --help` 与 `token-usage version` 验证。

**源码构建**（macOS 或 Windows）：

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
make build   # 产出 ./token-usage（make build-all 产 dist/token-usage-windows-amd64.exe）
```

把构建产物按上方官方资产步骤放进 bin 目录并加入 PATH（macOS 为 `~/.token-usage/bin/token-usage`，Windows 为 `%USERPROFILE%\.token-usage\bin\token-usage.exe`）。源码构建产物 `Version=dev` 或伪版本，不能自更新；升级=重新构建后手动替换 bin 目录下的文件。

#### 方式 C：go install（需 Go 环境）

```bash
go install github.com/YuLaiZ/token-usage/cmd/token-usage@latest
```

二进制装到 `$GOBIN`（默认 `~/go/bin`），需自行确保该目录在 PATH 中。配置和日志仍在 `~/.token-usage/`。安装后可用 `token-usage --version` 验证。

#### 方式 D：直接 go build（开发用）

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
go build -o token-usage ./cmd/token-usage
./token-usage --help
./token-usage --version
```

#### 卸载与迁移

卸载后无系统级残留：

1. 若守护进程正在运行，先停止：`token-usage stop`。
2. 若开启过开机自启，先执行 `token-usage config set daemon.autostart false`：移除自启定义（macOS 的 `~/Library/LaunchAgents/<label>.plist` 文件、Windows 的注册表 Run 值），避免删除目录后定义继续指向已不存在的二进制、每次登录触发启动失败。
3. 删除应用目录：`rm -rf ~/.token-usage`（Windows 为 `Remove-Item -Recurse -Force $env:USERPROFILE\.token-usage`）。当前终端的命令缓存可能仍指向已删二进制，执行 `hash -r` 后确认 `token-usage` 已不可用，或直接新开终端确认。
4. 移除 PATH 配置。

   macOS：删除 shell rc 文件中的 marker 块：

   ```sh
   # >>> token-usage path >>>
   export PATH="$HOME/.token-usage/bin:$PATH"
   # <<< token-usage path <<<
   ```

   Windows：优先用与安装时相同的保类型 `Microsoft.Win32.Registry` 注册表直写，移除 `bin` 目录条目后把剩余条目写回（若已无其他条目则直接删除 `Path` 值），保持既有 `REG_EXPAND_SZ` 值类型与 `%VAR%` 条目原文。与安装片段一致，条目匹配会先把未展开条目展开（仅对 `REG_EXPAND_SZ` 值生效），`%USERPROFILE%` 形态的既有条目同样会被移除：

   ```powershell
   $dir  = "$env:USERPROFILE\.token-usage\bin"
   $key  = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
   if ($key.GetValueNames() -contains 'Path') {
       $raw  = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
       $kind = $key.GetValueKind('Path')
       $kept = ($raw -split ';') | Where-Object {
           $lit = $_.Trim().TrimEnd('\')
           $exp = if ($kind -eq 'ExpandString') { [Environment]::ExpandEnvironmentVariables($_).Trim().TrimEnd('\') } else { $lit }
           $_ -and ($lit -ne $dir) -and ($exp -ne $dir)
       }
       if (@($kept).Count -gt 0) {
           $key.SetValue('Path', ($kept -join ';'), $kind)
       } else {
           $key.DeleteValue('Path')
           Write-Output '已无其他条目，Path 值已删除。'
       }
   } else {
       Write-Output 'Path 值不存在，无需清理。'
   }
   $key.Close()
   ```

   或经现代「编辑用户环境变量」对话框删除 `%USERPROFILE%\.token-usage\bin` 条目，并在使用 UI 后核对 Path 值类型仍为 `REG_EXPAND_SZ`（旧式列表编辑器存在写回 `REG_SZ` 的已知问题）。**不得**使用 `setx` 或 `[Environment]::SetEnvironmentVariable`（前者截断、后者类型退化）。注册表直写后需注销重登（或经「编辑用户环境变量」对话框确定），新终端窗口方能生效。
5. 曾按旧软链教程安装的，一并删除残留软链：macOS 的 `/usr/local/bin/token-usage`、Windows 的 `%LOCALAPPDATA%\Microsoft\WindowsApps\token-usage.exe`。

迁移提示：

- 若 PATH 中更靠前位置存在旧 `token-usage` 副本（旧 Windows 教程曾把 exe 放进任意目录），用 `which token-usage` / `Get-Command token-usage` 定位并移除，避免遮蔽新布局。
- 曾开启过开机自启的用户，重装后在新终端（PATH 生效后）执行一次 `token-usage config set daemon.autostart true`，确保定义重建指向新位置；若配置文件已随卸载删除，先执行 `token-usage config init`（配置文件不存在时 `config set` 会直接报错）。

### 首次使用

```bash
# 1. 初始化配置文件（写入默认值到 ~/.token-usage/config.toml）与数据库
token-usage config init
#    或直接打开交互式配置 TUI 编辑（不存在则自动初始化）
token-usage config

# 2. 采集历史全数据（首次必做一次）
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

# 3. 之后保持当天数据更新有两种方式：
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
| `query [日期]` | 查询统计（默认今日，按客户端分组） |
| `query client/model/project/sessions/summary [日期]` | 对应视图查询 |
| `errors [YYYYMMDD]` | 查看采集异常（`--source X` / `--unresolved`） |

日期为位置参数：`YYYYMMDD` 单日或 `YYYYMMDD-YYYYMMDD` 闭区间；无 `--date` 标志。

### 配置

| 命令 | 作用 |
|------|------|
| `config` | 打开交互式配置 TUI（含开机自启开关） |
| `config init` | 初始化配置文件与数据库 |
| `config get <key>` | 读取单项配置（dotted key，用户配置层原值，不展开 `~`、不补默认值） |
| `config show` | 输出完整 effective TOML（展开 `~`、补默认值/默认路径，只读、纯 TOML） |
| `config set <key> <value>` | 写入单项配置（原子写盘 + 自启同步 + 动作建议） |

> `config set daemon.autostart` 只同步开机自启定义，**不启停当前 daemon**；要让当前会话生效需手动 `stop` + `start`（或 `restart`）。
>
> `config get` 返回用户配置层原值（不展开 `~`、不补默认值，未显式写的字段返回零值）；要查看运行时生效的完整配置（展开 `~`、补齐核心默认值与默认路径）用 `config show`。

### 版本

| 命令 | 作用 |
|------|------|
| `--version`（或 `-v`） | 单行短输出 `token-usage <version>`（如 `token-usage v0.1.0`，本地开发为 `token-usage dev`） |
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

配置文件路径：`~/.token-usage/config.toml`，TOML 格式，可手工添加注释。开箱即用：client 只需声明 `enabled = true`，路径由程序按各工具默认位置自动填充。

> `config set` 和 TUI 保存会完整重写用户配置文件，因此不会保留既有注释和 map 键书写顺序；需要保留手写说明时请先备份。

查看配置有两种只读方式，职责不同：

- `config get <key>`：读取**用户配置层原值**（dotted key），即配置文件中显式写入的值，不展开 `~`、不补默认路径、不 clamp 数值；未显式配置的字段返回零值。
- `config show`：输出完整 **effective TOML**（展开 `~`、补 `data_dir`/`daemon`/`log` 核心默认值与 client/router registry 默认路径后的运行时生效配置），纯 TOML 无前缀，便于脚本解析与重定向。只读，不创建 config/DB/日志、不抢进程锁。

> `config show` 输出含本机路径：`~` 会展开；显式相对路径及其派生的默认路径（如 `data_dir` 派生的 `log.dir`、`state_dir` 派生的 `sessions_dir`）保持相对；其余 home-based 默认路径为绝对路径。对外分享前请检查是否含敏感信息。其输出也不是建议覆盖回用户配置文件的模板（回写会冻结默认路径并丢失注释）。

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

[daemon]
poll_interval = 30            # SQLitePoller 轮询间隔（秒）
autostart = false             # 开机自启（macOS launchd / Windows 注册表）
                              # 首次使用请先 token-usage collect all 初始化历史数据，再开启

[log]
level = "info"
dir = "~/.token-usage/logs"
max_days = 7
```

> **路由归因现状**：当前只有 Claude（Code/Desktop）配置 `router = "cc_switch"` 会做消息级归因回填。其他客户端（OpenCode/Codex/WorkBuddy/ZCode/AutoClaw）即使配置 `router`，原始日志仍会写入 `raw_router_logs`，但不会回填 `messages`，因为 CC Switch 的 `app_type` 只识别 Claude 系列。
>
> **Provider 别名**：`provider_aliases` 只规范 CC Switch 回填的 provider 显示名；key 必须与原始 provider 名完全一致。修改后按命令提示执行 `collect router --client <name>`（或 `collect all --client <name>`）回填既有归因数据。

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

详细架构设计见 [docs/architecture.zh-CN.md](docs/architecture.zh-CN.md)，CLI 命令参考见 [docs/cli.zh-CN.md](docs/cli.zh-CN.md)。

## 贡献

欢迎提交 Issue 和 Pull Request。提交 PR 前请阅读[贡献指南](CONTRIBUTING.zh-CN.md)，并确保：

1. 确保相关模块测试通过：`go test ./...`
2. 提交信息用中文一句话描述，不加 `feat`/`fix` 等前缀
3. 一个 PR 聚焦一个改动主题

## 许可证

本项目基于 [MIT License](LICENSE) 发布。
