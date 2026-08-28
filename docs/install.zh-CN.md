# 安装指南

> 简体中文 | [English](install.md)

本文件完整覆盖各种安装方式：推荐的安装脚本、同布局的手动二进制安装、`go install`、源码构建、自更新与卸载/迁移。最短安装路径见 [README 快速开始](../README.zh-CN.md#快速开始)。

## 安装布局

配置、数据库与日志始终位于 `~/.token-usage`（Windows 为 `%USERPROFILE%\.token-usage`）。脚本安装与手动二进制安装还把二进制也放在这里——`~/.token-usage/bin/token-usage`（Windows 为 `%USERPROFILE%\.token-usage\bin\token-usage.exe`）——经用户 PATH 暴露命令，无需 sudo（Windows 无需管理员权限）；`go install` 与开发用直接构建（`go build`）的二进制位置见各自小节。各方式在升级语义上不同：

- **官方 Release 二进制**（经下方脚本、AI Agent 指令或手动安装）：支持原地自更新——二进制是 PATH 上的真实文件，正是自更新来源校验所要求的形态。
- **源码构建**（`make build` / `go build`）：`Version = dev` 或伪版本，不能自更新；升级=重新构建后手动替换文件。

已发布的资产（有官方二进制的平台——也是自更新支持的平台）：

- `token-usage-darwin-arm64`（macOS Apple Silicon）
- `token-usage-darwin-amd64`（macOS Intel）
- `token-usage-windows-amd64.exe`（Windows）

## 用官方脚本安装（推荐）

### macOS

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | TAG=vX.Y.Z bash
```

脚本自动检测 CPU 架构、下载最新稳定版官方 Release、按官方 `SHA256SUMS` 校验后无需 sudo 安装到 `~/.token-usage/bin/token-usage`，并自动清理旧布局安装位置的遗留副本（目录不可写时给出手动移除指引，该位置被目录占用或删除失败等其他情形打印对应人工处理指引，均不影响安装），再向 shell rc 文件追加 marker 块把 `~/.token-usage/bin` 加入用户 PATH（仅支持 zsh 与 bash——zsh 写 `~/.zshrc`；bash 写登录 shell 读取的第一个文件——`~/.bash_profile` 优先，其次 `.bash_login`、再次 `.profile`；其他 shell 下脚本会打印人工 PATH 配置指引）。新开终端运行 `token-usage version` 确认。需要安装 RC 时，显式传入准确 tag：`TAG=vX.Y.Z-rc.N`。

> 非登录交互 shell 环境（部分 IDE 集成终端读取 `~/.bashrc` 而非登录文件）不会加载登录文件，需要时请自行补一行 `export PATH="$HOME/.token-usage/bin:$PATH"`；zsh 交互终端用户全部命中 `~/.zshrc`。

### Windows

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

两条命令依次执行：第一条把安装脚本下载到临时文件，第二条执行它——两行粘贴到同一 PowerShell 窗口即可。安装脚本刻意不用 `irm ... | iex` 一步管道形态：下载的脚本带 UTF-8 BOM，`Invoke-Expression` 无法解析。

指定版本：对已下载的脚本以 `-Tag` 运行：

```powershell
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1" -Tag vX.Y.Z
```

脚本下载最新稳定版官方 Release、按官方 `SHA256SUMS` 校验后无需管理员权限安装到 `%USERPROFILE%\.token-usage\bin\token-usage.exe`，并以保类型注册表直写把 `%USERPROFILE%\.token-usage\bin` 追加到用户 PATH（保持既有 `REG_EXPAND_SZ` 值类型与 `%VAR%` 条目原文），随后广播 `WM_SETTINGCHANGE` 使新终端无需注销即可获得新 PATH（广播失败时脚本会提示注销重登）。从开始菜单/任务栏启动新终端窗口，运行 `token-usage version` 确认。需要安装 RC 时，显式以 `-Tag vX.Y.Z-rc.N` 指定准确 tag。

> 旧 Windows 环境（TLS 1.2 以下）下第一步 `irm` 仍发生在脚本执行之前，脚本内的 TLS 兜底救不了它：请先在当前会话执行 `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12`，再运行第一步下载。

## 自更新

从官方 Release 安装的二进制可原地自更新：

```bash
token-usage update                  # 更新到最新稳定版
token-usage update --check          # 只检查，不写任何本地文件
token-usage update --version vX.Y.Z # 更新（或检查）指定版本 tag
```

`update` 只在当前二进制是所报告版本的官方 Release 资产时才覆盖（当前二进制的 SHA256 必须等于该版本官方资产的 hash）。开发构建（`make build`、`make build-all` 或 `go install` 产物的 `dev` / 伪版本）、软链副本、版本或 hash 不匹配均判定来源不可信：`update` 不覆盖，而是输出人工安装指引。自更新仅支持有官方资产的平台（`darwin/arm64`、`darwin/amd64`、`windows/amd64`）；其他平台 `update` 会提示无官方资产、要求手动安装。完整信任规则、退出码、副作用与 Windows 异步替换说明见 CLI 参考的[信任与来源校验](cli.zh-CN.md#信任与来源校验)。

> **为何不软链进 PATH**：自更新来源校验要求当前可执行文件是真实二进制文件；经软链调用时解析到软链路径会被拒绝。因此本布局直接把 `~/.token-usage/bin` 加入 PATH，而不是在其他目录放置链接。

## 手动二进制安装（同布局）

手动安装即手工完成同一布局——二进制位于 `~/.token-usage/bin/token-usage`（Windows 为 `%USERPROFILE%\.token-usage\bin\token-usage.exe`），bin 目录加入用户 PATH。经 SHA256 校验的官方资产与脚本安装等价，支持原地自更新。

### macOS——官方 Release 资产

```bash
# releases/latest/download/... 始终指向最新稳定版，不会解析到预发布。
# 如需安装指定版本，改用 Releases 页面对应 tag 的下载 URL。
curl -fsSL -o token-usage-darwin-arm64 https://github.com/YuLaiZ/token-usage/releases/latest/download/token-usage-darwin-arm64
curl -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/latest/download/SHA256SUMS
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

> **用浏览器（而非 `curl`）下载的二进制？** 浏览器保存的文件带 `com.apple.quarantine` 隔离属性，而官方二进制是 ad-hoc 签名，Gatekeeper 不接受隔离文件的 ad-hoc 签名：首次运行会被静默杀掉（无任何输出，退出码 137）。就地重签即可修复：
>
> ```bash
> codesign --sign - --force ~/.token-usage/bin/token-usage
> ```
>
> 仅移除隔离属性（`xattr -d com.apple.quarantine ...`）可能不够——Gatekeeper 会缓存判定结果。`curl`（或官方脚本）下载的文件不带该属性，不受影响。

### Windows——官方 Release 资产

```powershell
# 从最新稳定版下载（latest/download 不会解析到预发布；指定版本用 Releases
# 页面对应 tag 的 URL），再按该 Release 的 SHA256SUMS 校验 SHA256：
curl.exe -fsSL -o token-usage-windows-amd64.exe https://github.com/YuLaiZ/token-usage/releases/latest/download/token-usage-windows-amd64.exe
curl.exe -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/latest/download/SHA256SUMS
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

### 源码构建（macOS 或 Windows）

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
make build   # 产出 ./token-usage（make build-all 产 dist/token-usage-windows-amd64.exe）
```

把构建产物按上方官方资产步骤放进 bin 目录并加入 PATH（macOS 为 `~/.token-usage/bin/token-usage`，Windows 为 `%USERPROFILE%\.token-usage\bin\token-usage.exe`）。源码构建产物 `Version = dev` 或伪版本，不能自更新；升级=重新构建后手动替换 bin 目录下的文件。

## go install（需 Go 环境）

```bash
go install github.com/YuLaiZ/token-usage/cmd/token-usage@latest
```

二进制装到 `$GOBIN`（默认 `~/go/bin`），需自行确保该目录在 PATH 中。配置和日志仍在 `~/.token-usage/`。安装后可用 `token-usage --version` 验证。

## 直接 go build（开发用）

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
go build -o token-usage ./cmd/token-usage
./token-usage --help
./token-usage --version
```

## 卸载与迁移

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
