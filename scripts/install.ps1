# token-usage 官方安装脚本（Windows）。
#
# 功能：下载官方 Release 二进制（默认取最新稳定版；可用 -Tag 指定版本）、
# 用官方 SHA256SUMS 校验、安装到 %USERPROFILE%\.token-usage\bin\token-usage.exe、
# 验证版本，并把 bin 目录加入用户 PATH（注册表直写 HKCU\Environment，保留
# REG_EXPAND_SZ 类型与 %VAR% 未展开原文，写入后广播 WM_SETTINGCHANGE）。
#
# 用法（两步形态：先保存为文件再执行。本脚本带 UTF-8 BOM，BOM 前导字符会使
# “下载内容直接交给 iex 执行”的一行命令形态不可解析，故不提供该形态）：
#   irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
#   powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1" -Tag vX.Y.Z
#
# 兼容 Windows PowerShell 5.1 与 PowerShell 7+。所有失败统一 throw 而非 exit：
# throw 产生可见的错误信息（含失败原因），且在 -File 形态下脚本以非零退出码
# 结束，便于调用方（用户、CI）明确感知安装失败。
param(
    [string]$Tag = ''
)

# ---- 平台检查：官方资产仅覆盖 Windows x64（token-usage-windows-amd64.exe）。----
# Windows PowerShell 5.1 没有 $IsWindows 自动变量，用 OSVersion.Platform 兼容判定。
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw "错误：官方资产仅支持 Windows x64；当前平台不是 Windows。macOS 用户请使用 install.sh，其他平台请从源码构建。"
}
# WOW64 下的 32 位 PowerShell 进程须看 PROCESSOR_ARCHITEW6432（真实机器架构）。
$osArch = $env:PROCESSOR_ARCHITEW6432
if (-not $osArch) { $osArch = $env:PROCESSOR_ARCHITECTURE }
# ARM64 Windows（原生 ARM64 PowerShell 进程下本变量为 ARM64）经系统 x64 仿真运行
# amd64 资产，与 AMD64 同为可安装形态；其余架构（32 位 Windows 等）无官方资产。
if ($osArch -ne 'AMD64' -and $osArch -ne 'ARM64') {
    throw "错误：官方资产仅支持 Windows x64（token-usage-windows-amd64.exe；ARM64 Windows 经 x64 仿真运行），当前架构为 $osArch。"
}

$Repo = 'YuLaiZ/token-usage'
$Asset = 'token-usage-windows-amd64.exe'
# USERPROFILE 未定义的病态会话下 Join-Path 抛裸英文语句级错误且脚本继续，后续步骤连锁失败
# 难以定位，包 try-catch 统一转中文 throw。
try {
    $BinDir = Join-Path $env:USERPROFILE '.token-usage\bin'
    $TargetPath = Join-Path $BinDir 'token-usage.exe'
} catch {
    throw "错误：构造安装路径失败（USERPROFILE 未定义或异常）：$($_.Exception.Message)"
}
# 网络超时分层：轻量元数据/清单请求 30 秒；二进制资产按慢网预留 20 分钟
# （实测慢代理 ~150KB/s 下 24MB 二进制约需 3 分钟，留足余量避免慢网误杀）。
$QueryTimeoutSec = 30
$DownloadTimeoutSec = 1200

# ---- 基础环境设置：TLS 1.2 兜底与 UTF-8 输出编码。----
# 显式补 TLS 1.2 仅能救脚本已落地的 -File 形态（网络获取发生在脚本执行之前，
# 旧 TLS 环境在脚本运行前即握手失败）。
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }
try {
    [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
} catch { }

# ---- 版本解析：未指定 -Tag 时取最新稳定版 Release。----
# 用 /releases/latest；候选版必须通过 -Tag 显式指定。
if (-not $Tag) {
    try {
        $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -TimeoutSec $QueryTimeoutSec -ErrorAction Stop
    } catch {
        throw "错误：无法获取最新稳定版 Release tag：$($_.Exception.Message)"
    }
    if ($null -eq $latest -or -not $latest.tag_name) {
        throw "错误：无法获取最新稳定版 Release tag。"
    }
    $Tag = [string]$latest.tag_name
}

Write-Output "安装 token-usage $Tag（$Asset）到 $TargetPath ..."

# ---- 下载 + SHA256 校验 + 三段式安装：临时落地 -> 校验 -> 移动覆盖目标。----
# 校验通过前不触碰既有目标文件（传输损坏或篡改时旧版本必须完好可回退）。
# 下载用字节流写入（Invoke-WebRequest -UseBasicParsing 取 .Content 字节 + WriteAllBytes
# 落地 $env:TEMP），不用 -OutFile（PowerShell 7.4+ 会写 Zone.Identifier，MOTW 随安装残留）；
# 也不用 Invoke-RestMethod 取资产——PowerShell 7 对 octet-stream 二进制响应会按文本解码为
# 有损字符串（出现 U+FFFD 替换字符），字节数组守卫必然失败、安装中断。
# 关闭进度条渲染：Windows PowerShell 5.1 的控制台进度条会显著拖慢大体积资产下载。
$ProgressPreference = 'SilentlyContinue'
$Base = "https://github.com/$Repo/releases/download/$Tag"
# TEMP 未定义的病态会话下 Join-Path 抛裸英文语句级错误且脚本继续，包 try-catch 统一转中文 throw。
try {
    $TmpAsset = Join-Path $env:TEMP ("token-usage-install-{0}.exe" -f [IO.Path]::GetRandomFileName())
} catch {
    throw "错误：构造临时文件路径失败（TEMP 未定义或异常）：$($_.Exception.Message)"
}

try {
    try {
        $sumsRaw = (Invoke-WebRequest -UseBasicParsing -Uri "$Base/SHA256SUMS" -TimeoutSec $QueryTimeoutSec -ErrorAction Stop).Content
    } catch {
        throw "错误：下载 SHA256SUMS 失败：$($_.Exception.Message)"
    }
    # GitHub 对 Release 资产统一按 octet-stream 二进制流返回，.Content 为字节数组，
    # 归一为 UTF8 文本再解析（防御非字节数组形态，如中间层改写为文本）。
    if ($sumsRaw -is [byte[]]) {
        $sumsText = [System.Text.Encoding]::UTF8.GetString($sumsRaw)
    } else {
        $sumsText = [string]$sumsRaw
    }

    try {
        $assetBytes = (Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Asset" -TimeoutSec $DownloadTimeoutSec -ErrorAction Stop).Content
    } catch {
        throw "错误：下载 $Asset 失败：$($_.Exception.Message)"
    }
    if ($null -eq $assetBytes -or $assetBytes -isnot [byte[]] -or $assetBytes.Length -eq 0) {
        throw "错误：下载 $Asset 失败：返回内容异常。"
    }
    # TEMP 所在盘已满或无写入权限时 .NET WriteAllBytes 抛裸英文 IO 异常，包 try-catch 统一转中文 throw。
    try {
        [System.IO.File]::WriteAllBytes($TmpAsset, $assetBytes)
    } catch {
        throw "错误：写入临时文件失败（$TmpAsset，常见于磁盘已满或无写入权限）：$($_.Exception.Message)"
    }

    # SHA256SUMS 为两个空格分隔（<hash>  <资产名>），按资产名精确匹配。
    $expectedHash = $null
    foreach ($line in ($sumsText -split "`r?`n")) {
        if ($line -match '^([0-9a-fA-F]{64})  (.+)$') {
            if ($Matches[2].Trim() -eq $Asset) {
                $expectedHash = $Matches[1].ToLowerInvariant()
                break
            }
        }
    }
    if (-not $expectedHash) {
        throw "错误：SHA256SUMS 中未找到资产 $Asset 的校验条目。"
    }
    # Get-FileHash 失败（如杀毒软件独占锁定 TEMP 文件）时 .Hash 为 $null，链式 ToLowerInvariant
    # 会抛无从定位成因的英文空值异常，包 try-catch 统一转中文 throw。
    try {
        $actualHash = (Get-FileHash -LiteralPath $TmpAsset -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
    } catch {
        throw "错误：读取临时文件哈希失败（$TmpAsset，常见于杀毒软件独占锁定临时文件）：$($_.Exception.Message)"
    }
    if ($actualHash -ne $expectedHash) {
        throw "错误：SHA256 校验失败（期望 $expectedHash，实际 $actualHash），已放弃安装，既有文件未被改动。"
    }
    Write-Output "SHA256 校验通过。"

    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null

    # 目标占用检测：Windows 对运行中的 exe 持文件锁，独占打开失败即中止（不强杀进程）；
    # 目标不存在时跳过占用检测（FileNotFound 不是占用）。
    if (Test-Path -LiteralPath $TargetPath) {
        # 目标存在但不是普通文件（如目录等病态形态）时，独占打开同样失败，
        # 但成因与运行占用无关，先判形态避免误导性的“正在运行”提示。
        if (-not (Test-Path -LiteralPath $TargetPath -PathType Leaf)) {
            throw "错误：目标位置 $TargetPath 被目录或其他非普通文件对象占用，请手动确认并处理后重试。"
        }
        $probe = $null
        try {
            $probe = [System.IO.File]::Open($TargetPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::None)
        } catch {
            throw "错误：目标文件被占用（$TargetPath）。token-usage 正在运行，请先执行 token-usage stop（或 & `"$TargetPath`" stop，PATH 未生效的旧终端用绝对路径形态）后重试；若未在运行，请检查文件占用/权限后重试。"
        } finally {
            if ($probe) { $probe.Dispose() }
        }
    }

    # TEMP 与 USERPROFILE 同盘时 Move-Item 为原子 rename；TEMP 被重定向到其他盘的罕见
    # 配置下退化为 copy+delete，复制中途 IO 故障理论上可半写既有目标，此处接受该边界
    # （上方校验阶段已保证损坏资产不触碰目标，此风险仅限已校验资产移动阶段的极端 IO 故障）。
    try {
        Move-Item -LiteralPath $TmpAsset -Destination $TargetPath -Force -ErrorAction Stop
    } catch {
        throw "错误：写入目标文件失败：$($_.Exception.Message)"
    }
} finally {
    if (Test-Path -LiteralPath $TmpAsset) {
        Remove-Item -LiteralPath $TmpAsset -Force -ErrorAction SilentlyContinue
    }
}

# ---- 安装后验证：以绝对路径执行 version（对等 install.sh 的既有步骤）。----
# 用户 profile（-File 形态默认加载）常含外部命令调用（conda hook 等），会在脚本执行前
# 把 $LASTEXITCODE 预置为 0，架空下方“启动失败保持 $null”的判定（启动失败的原生调用
# 不更新该变量），调用前显式重置以恢复判定语义。
$global:LASTEXITCODE = $null
& $TargetPath version
# 启动失败（如安全软件拦截、可执行文件损坏）时首次原生调用不设置 $LASTEXITCODE（保持 $null），
# 须与进程已运行但退出码非零区分，避免打印空退出码。
if ($null -eq $LASTEXITCODE) {
    throw "错误：安装后版本验证失败：无法启动 $TargetPath（可能被安全软件拦截、可执行文件损坏，或当前系统无法运行 x64 程序——如 Windows 10 on ARM 无 x64 仿真）。"
}
if ($LASTEXITCODE -ne 0) {
    throw "错误：安装后版本验证失败（退出码 $LASTEXITCODE）。"
}

Write-Output "完成：token-usage 已安装到 $TargetPath"
Write-Output "运行 token-usage --help 查看可用命令。"

# ---- 用户 PATH 追加：注册表直写 HKCU\Environment 的 Path 值。----
# 用 Microsoft.Win32.Registry 直读直写并保留 REG_EXPAND_SZ 类型与 %VAR% 未展开原文；
# 不用 [Environment]::Get/SetEnvironmentVariable（Get 展开变量、Set 以 REG_SZ 写回，类型退化），
# 也不用 setx（1024 字符截断）。读取用 DoNotExpandEnvironmentNames 取原始值。
function Invoke-EnvChangeBroadcast {
    # 直写注册表不带广播，须自行补发 WM_SETTINGCHANGE(lParam="Environment")：
    # explorer 收到后才刷新自身环境，之后从开始菜单新开的终端才能拿到新 PATH。
    # Add-Type 在同一会话内重复加载同名类型会报错（保存的脚本在同一会话内再次执行的场景），捕获后既有类型仍可直接调用。
    $memberDef = @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    try {
        Add-Type -Namespace TokenUsageInstall -Name NativeMethods -MemberDefinition $memberDef -ErrorAction Stop
    } catch { }
    try {
        # HWND_BROADCAST=0xffff，WM_SETTINGCHANGE=0x1A，SMTO_ABORTIFHUNG=0x2；返回非零表示成功。
        $result = [UIntPtr]::Zero
        $ret = [TokenUsageInstall.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x001A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
        return ($ret -ne [IntPtr]::Zero)
    } catch {
        return $false
    }
}

$envKey = $null
$needRelogon = $false   # 广播失败时 PATH 生效须注销重登，结尾提示措辞随之切换
$pathPersisted = $true  # 注册表打开失败降级时置 false：PATH 未写入，结尾提示不依赖新终端
try {
    try {
        $envKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    } catch { }
    if ($null -eq $envKey) {
        # 注册表打开失败不中止安装（二进制已落地可用），降级为人工指引，
        # 与 macOS 侧 rc 写入失败的降级口径对称；PATH 未写入，后续提示不引导依赖新终端。
        $pathPersisted = $false
        Write-Output "提示：无法打开注册表 HKCU\Environment，PATH 未写入（二进制已安装）。请手动把 $BinDir 加入用户 PATH（HKCU\Environment 的 Path 值，保持 REG_EXPAND_SZ 类型）。"
    }
    if ($null -ne $envKey) {
        # 读取或比较段失败（病态注册表值形态等运行时异常）同走降级，不中止安装，
        # 与打开失败、写入失败的降级口径一致：PATH 未写入，后续提示不引导依赖新终端。
        try {
            $hasPathValue = ($envKey.GetValueNames() -contains 'Path')
            $rawPath = ''
            $rawKind = $null
            if ($hasPathValue) {
                $rawPath = [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                $rawKind = $envKey.GetValueKind('Path')
            }

            # 已含检测：条目比较按大小写不敏感（-eq 默认）+ 去尾部 \ + 两端空白 trim。
            # 展开形态匹配仅对 REG_EXPAND_SZ 值生效；REG_SZ 值不展开变量，只做字面匹配——
            # REG_SZ 中病态的未展开 %USERPROFILE% 条目永不可解析，不得当作已含跳过追加。
            $targetNorm = $BinDir.TrimEnd('\')
            $alreadyContained = $false
            if ($hasPathValue -and $rawPath) {
                foreach ($entry in ($rawPath -split ';')) {
                    $literal = $entry.Trim().TrimEnd('\')
                    if ($literal -eq '') { continue }
                    if ($literal -eq $targetNorm) { $alreadyContained = $true; break }
                    if ($rawKind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) {
                        $expanded = [Environment]::ExpandEnvironmentVariables($entry).Trim().TrimEnd('\')
                        if ($expanded -eq $targetNorm) { $alreadyContained = $true; break }
                    }
                }
            }

            if ($alreadyContained) {
                # 已含则跳过：不重写值、不广播（幂等重跑不产生副作用）。
                Write-Output "用户 PATH 已包含 $BinDir，跳过写入。"
            } else {
                # 重写语义：按 ; 拆分 -> 过滤空条目 -> 重组（消除连续 ;;、首分号空首字段、空值原值），
                # 再在尾部追加；既有条目保留原文。空条目语义为当前目录，有轻微安全隐患，一并清理。
                # 本次追加的新条目一律写展开后的绝对路径：REG_SZ 不做变量展开，
                # 写 %USERPROFILE% 形态在“原值 REG_SZ 保持原类型”分支下将永远无法解析。
                $kept = @()
                if ($rawPath) {
                    foreach ($entry in ($rawPath -split ';')) {
                        if ($entry.Trim() -ne '') { $kept += $entry }
                    }
                }
                if ($kept.Count -gt 0) {
                    $newPath = ($kept -join ';') + ';' + $BinDir
                } else {
                    $newPath = $BinDir
                }
                # 值不存在时以 REG_EXPAND_SZ 类型创建；原值为 REG_SZ 时保持原类型不升级。
                # 写入失败不中止安装（二进制已落地可用），降级为人工指引，
                # 与上方注册表打开失败的降级口径一致；PATH 未写入，后续提示不引导依赖新终端。
                try {
                    if ($null -eq $rawKind) {
                        $envKey.SetValue('Path', $newPath, [Microsoft.Win32.RegistryValueKind]::ExpandString)
                    } else {
                        $envKey.SetValue('Path', $newPath, $rawKind)
                    }
                } catch {
                    $pathPersisted = $false
                    Write-Output "提示：写入注册表 Path 值失败（PATH 未写入，二进制已安装）。请手动把 $BinDir 加入用户 PATH（HKCU\Environment 的 Path 值，保持 REG_EXPAND_SZ 类型）。"
                }
                if ($pathPersisted) {
                    Write-Output "已将 $BinDir 加入用户 PATH。"

                    # 广播失败不重试，降级为注销重登提示（PATH 写入本身已成功）。
                    if (-not (Invoke-EnvChangeBroadcast)) {
                        $needRelogon = $true
                        Write-Output "警告：环境变量变更通知失败，PATH 修改将在注销后重新登录时生效。"
                    }
                }
            }
        } catch {
            $pathPersisted = $false
            Write-Output "提示：读取注册表 Path 值失败（PATH 未写入，二进制已安装）。请手动把 $BinDir 加入用户 PATH（HKCU\Environment 的 Path 值，保持 REG_EXPAND_SZ 类型）。"
        }
    }
} finally {
    if ($envKey) { $envKey.Close() }
}

# ---- Shell 补全配置：把 Tab 补全写入 PowerShell profile（提示级动作，不 throw）。----
# 与主流程失败统一 throw 的口径有意不同：补全环节任何失败只 Write-Output 提示并
# 继续，不改变安装退出码（对齐 install.sh 侧 PATH/补全环节的既有降级口径）。
# $PROFILE 按运行时解析：Windows PowerShell 5.1 与 PowerShell 7 的配置文件路径不同，
# 各自运行一次安装脚本即各自写入（OneDrive 重定向时 $PROFILE 已是解析后的实际路径）。
$TUCompletionHead = '# >>> token-usage completion >>>'
$TUCompletionTail = '# <<< token-usage completion <<<'
# marker 块内部换行恒为 LF（与检测侧的 CRLF 归一语义配套）；仅块尾与既有内容的
# 衔接换行按既有内容主导形态。固定绝对路径 + -LiteralPath/-PathType Leaf 守卫：
# 裸 Test-Path 对同名目录也返回 true，& 执行目录会产生红字；Get-Command 会被旧
# 副本/alias/function 骗过，故不用。Windows 安装位置固定，原地 update 同路径替换
# 后补全天然生效、无需重装。
$TUCompletionBlock = @(
    $TUCompletionHead,
    'if (Test-Path -LiteralPath "$env:USERPROFILE\.token-usage\bin\token-usage.exe" -PathType Leaf) {',
    '    & "$env:USERPROFILE\.token-usage\bin\token-usage.exe" completion powershell | Out-String | Invoke-Expression',
    '}',
    $TUCompletionTail
) -join "`n"

# Get-TUProfileCompletionState 判定 profile 的补全 marker 状态与编码形态。
# 返回 'missing'（不存在）/ 'occupied'（路径被目录等占用）/ 'utf16'（UTF-16
# LE/BE BOM——向宽字符流尾部追加 ASCII 会产生混合编码，PowerShell 按 BOM 以
# UTF-16 解析全文、marker 段成乱码、profile 启动报错，保守不改写）/ 'canonical'
#（恰好一块且逐字匹配）/ 'invalid'（残缺/重复/被编辑的块）/ 'none'（无 marker，
# 可写入）/ 'unreadable'。字节级读取 + Latin-1（28591）1:1 解码：GBK/ANSI 等任何
# ASCII 兼容编码的内容按原字节参与匹配（marker 本身纯 ASCII），不做字符串解码/
# 重编码；CRLF 先归一为 LF 再与 LF 模板比对，BOM 原样保留、不计入比对。
function Get-TUProfileCompletionState {
    param([string]$ProfilePath)
    if (Test-Path -LiteralPath $ProfilePath -PathType Container) { return 'occupied' }
    if (-not (Test-Path -LiteralPath $ProfilePath -PathType Leaf)) {
        if (Test-Path -LiteralPath $ProfilePath) { return 'occupied' }
        return 'missing'
    }
    $bytes = $null
    try { $bytes = [System.IO.File]::ReadAllBytes($ProfilePath) } catch { return 'unreadable' }
    if ($bytes.Length -ge 2 -and $bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE) { return 'utf16' }
    if ($bytes.Length -ge 2 -and $bytes[0] -eq 0xFE -and $bytes[1] -eq 0xFF) { return 'utf16' }
    $offset = 0
    if ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) { $offset = 3 }
    $text = [System.Text.Encoding]::GetEncoding(28591).GetString($bytes, $offset, $bytes.Length - $offset)
    $norm = $text -replace "`r`n", "`n"
    $exactHead = [regex]::Matches($norm, '(?m)^' + [regex]::Escape($TUCompletionHead) + '$').Count
    $exactTail = [regex]::Matches($norm, '(?m)^' + [regex]::Escape($TUCompletionTail) + '$').Count
    $loose = [regex]::Matches($norm, 'token-usage completion (>>>|<<<)').Count
    if ($exactHead -eq 0 -and $exactTail -eq 0 -and $loose -eq 0) { return 'none' }
    if ($exactHead -eq 1 -and $exactTail -eq 1 -and $loose -eq 2 -and $norm.Contains($TUCompletionBlock)) { return 'canonical' }
    return 'invalid'
}

# Write-TUCompletionToProfile 执行写入。顺序与防线：
#   1. profile 不存在时先 New-Item -ItemType Directory -Force 创建父目录并确认
#      成功，再创建 profile（New-Item File -Force 不保证创建不存在的父目录，首次
#      使用 PowerShell 的用户恰是该目录不存在的场景，会静默降级为「安装成功但
#      补全未写入」）；
#   2. profile 为符号链接（ReparsePoint，判定不跟随目标）或硬链接（fsutil hardlink
#      list 多行；输出为空视为无法判定、同样保守拒绝——rename 会分离硬链接两端、
#      把符号链接替换成普通文件）时不改写，原文件与另一端均不动；
#   3. ASCII 兼容形态字节级追加（BOM 原样保留；末行换行按尾字节 0x0A 判定，仅块
#      尾衔接换行按既有内容主导形态——存在 CRLF 序列用 CRLF，否则 LF）；
#   4. 同目录临时文件 + Move-Item 原子提交（多次追加/直接写 profile 在磁盘满或
#      进程中断时会留半块，canonical 防线随后会把它判为非规范并拒绝修复——
#      安装器自身制造的残留）；提交前校验候选头尾行齐全。
# 无返回值：成功与降级路径都以 Write-Output 提示可察觉（提示级动作不得被调用
# 点吞掉），profile 失败时字节不变。
function Write-TUCompletionToProfile {
    $profileDir = Split-Path -Parent $PROFILE
    if (-not (Test-Path -LiteralPath $profileDir -PathType Container)) {
        try {
            New-Item -ItemType Directory -Path $profileDir -Force -ErrorAction Stop | Out-Null
        } catch {
            Write-Output "提示：无法创建 PowerShell 配置文件目录（$profileDir）：$($_.Exception.Message)。补全未写入（安装不受影响）。"
            return
        }
        if (-not (Test-Path -LiteralPath $profileDir -PathType Container)) {
            Write-Output "提示：无法创建 PowerShell 配置文件目录（$profileDir），补全未写入（安装不受影响）。"
            return
        }
    }
    if (Test-Path -LiteralPath $PROFILE) {
        $item = Get-Item -LiteralPath $PROFILE -Force -ErrorAction SilentlyContinue
        # 链接类判定只覆盖「会被 rename 断开/分离」的形态：符号链接与 junction
        #（及任何 reparse point）；硬链接不经此处（Windows 上 LinkType 为空且无
        # reparse point），交给下方 fsutil 链接数检测分流。
        $linkKind = [string]$item.LinkType
        $isSymlinkLike = ($null -ne $item) -and (
            $linkKind -eq 'SymbolicLink' -or $linkKind -eq 'Junction' -or
            ($item.Attributes -band [IO.FileAttributes]::ReparsePoint))
        if ($isSymlinkLike) {
            Write-Output "$PROFILE 为符号链接，安装脚本保守不改写；请手动编辑链接目标文件加入下方 marker 模板，或解除链接后重跑安装脚本："
            Write-Output $TUCompletionBlock
            return
        }
        # fsutil 输出为空（执行失败等）时按无法判定处理、保守拒绝：硬链接经 rename
        # 写入会静默分离两端，漏检的代价大于误拒。
        $hardlinkLines = @()
        try { $hardlinkLines = @(fsutil hardlink list "$PROFILE" | Where-Object { $_ }) } catch { }
        if ($hardlinkLines.Count -ne 1) {
            if ($hardlinkLines.Count -gt 1) {
                Write-Output "$PROFILE 与其他文件为硬链接，安装脚本保守不改写；请手动编辑其中一个文件加入下方 marker 模板，或解除链接后重跑安装脚本："
            } else {
                Write-Output "提示：无法判定 $PROFILE 的硬链接状态，补全未写入（安装不受影响）；可手动配置或解除链接后重跑安装脚本。"
            }
            return
        }
    }
    $existing = [byte[]]@()
    if (Test-Path -LiteralPath $PROFILE -PathType Leaf) {
        try { $existing = [System.IO.File]::ReadAllBytes($PROFILE) } catch {
            Write-Output "提示：无法读取 $PROFILE：$($_.Exception.Message)。补全未写入（安装不受影响）。"
            return
        }
    }
    $latin1 = [System.Text.Encoding]::GetEncoding(28591)
    $existingText = $latin1.GetString($existing)
    $dominantEol = "`n"
    if ($existingText.Contains("`r`n")) { $dominantEol = "`r`n" }
    $eolBytes = $latin1.GetBytes($dominantEol)
    $prefixBytes = [byte[]]@()
    if ($existing.Length -gt 0 -and $existing[$existing.Length - 1] -ne 0x0A) {
        $prefixBytes = $eolBytes
    }
    $suffixBytes = $latin1.GetBytes($TUCompletionBlock + $dominantEol)
    $candidate = New-Object byte[] ($existing.Length + $prefixBytes.Length + $suffixBytes.Length)
    [Array]::Copy($existing, 0, $candidate, 0, $existing.Length)
    if ($prefixBytes.Length -gt 0) { [Array]::Copy($prefixBytes, 0, $candidate, $existing.Length, $prefixBytes.Length) }
    [Array]::Copy($suffixBytes, 0, $candidate, $existing.Length + $prefixBytes.Length, $suffixBytes.Length)
    $candidateText = $latin1.GetString($candidate)
    if (-not $candidateText.Contains($TUCompletionHead) -or -not $candidateText.Contains($TUCompletionTail)) {
        Write-Output "提示：补全 marker 写入校验未通过，$PROFILE 保持不变（安装不受影响）。"
        return
    }
    $tmpProfile = Join-Path $profileDir ('.profile-token-usage-{0}.tmp' -f [IO.Path]::GetRandomFileName())
    try {
        [System.IO.File]::WriteAllBytes($tmpProfile, $candidate)
        Move-Item -LiteralPath $tmpProfile -Destination $PROFILE -Force -ErrorAction Stop
    } catch {
        if (Test-Path -LiteralPath $tmpProfile) { Remove-Item -LiteralPath $tmpProfile -Force -ErrorAction SilentlyContinue }
        Write-Output "提示：补全配置写入失败：$($_.Exception.Message)。$PROFILE 保持不变（安装不受影响）；可按 token-usage completion powershell --help 手动配置。"
        return
    }
    Write-Output "已把 Tab 补全写入 $PROFILE（新开的 PowerShell 会话生效）。"
}

# 开关：仅 INSTALL_COMPLETION=yes 触发非交互安装（等价交互默认 Y）；未设置按默认
# 询问；其他非空值打印警告并按默认处理（防拼写错误静默走错路径）。
$tuCompletionAuto = $false
if ($env:INSTALL_COMPLETION) {
    if ($env:INSTALL_COMPLETION -ne 'yes') {
        Write-Output "警告：INSTALL_COMPLETION 仅支持 yes（当前值 `"$($env:INSTALL_COMPLETION)`"），已按默认询问流程处理。"
    } else {
        $tuCompletionAuto = $true
    }
}

switch (Get-TUProfileCompletionState $PROFILE) {
    'canonical' {
        Write-Output "检测到 PowerShell 配置文件已包含 token-usage 补全（$PROFILE），无需重复写入。"
    }
    'invalid' {
        Write-Output "检测到非规范 completion marker，请手动处理（删除旧块后重跑安装脚本）；$PROFILE 未被修改。"
    }
    'utf16' {
        Write-Output "$PROFILE 为 UTF-16 编码（带 BOM），安装脚本保守不改写；请先把它转为 UTF-8 后重跑安装脚本，或按 token-usage completion powershell --help 手动配置补全。"
    }
    'occupied' {
        Write-Output "提示：$PROFILE 被目录或其他非普通文件对象占用，补全未写入（安装不受影响）。"
    }
    'unreadable' {
        Write-Output "提示：无法读取 $PROFILE 判定补全状态，补全未写入（安装不受影响）。"
    }
    default {
        # missing / none：进入写入决策。非交互判定不依赖 Read-Host 超时行为：
        # UserInteractive 为假（CI、服务上下文）或宿主不是 ConsoleHost 时直接跳过。
        if (-not $tuCompletionAuto) {
            $interactiveHost = ([Environment]::UserInteractive -eq $true) -and ($Host.Name -eq 'ConsoleHost')
            if (-not $interactiveHost) {
                Write-Output "提示：当前会话非交互终端，未写入 PowerShell 补全（安装不受影响）；可稍后重跑安装脚本，或按 token-usage completion powershell --help 手动配置。"
                break
            }
            Write-Output 'Windows PowerShell 5.1 与 PowerShell 7 配置文件相互独立，双版本用户各运行一次安装脚本。'
            # 交互询问：默认 Y（回车即装，主路径默认增强且无任何权限/安全权衡）；
            # 非法输入重问一次，再非法按 Y。非交互 EOF 下 Read-Host 可能返回
            # $null（-eq '' 不匹配会误报无效输入），用 IsNullOrEmpty 归一。
            $tuAnswer = ''
            foreach ($tuAttempt in 1..2) {
                $tuAnswer = Read-Host ("是否把 Tab 补全写入 PowerShell 配置文件（{0}）？[Y/n]（直接回车 = Y）" -f $PROFILE)
                if ([string]::IsNullOrEmpty($tuAnswer) -or $tuAnswer -match '^[Yy]') { break }
                if ($tuAnswer -match '^[Nn]$') { break }
                if ($tuAttempt -eq 1) { Write-Output '无效输入，请输入 Y 或 n（直接回车 = Y）。' }
            }
            if ($tuAnswer -match '^[Nn]$') {
                Write-Output '已跳过 PowerShell 补全（安装正常完成）；稍后可重跑安装脚本，或按 token-usage completion powershell --help 手动配置。'
                break
            }
        }
        Write-TUCompletionToProfile
    }
}

# ---- 结尾提示。----
# 单次脚本执行无法感知跨进程的 stop 历史，恢复提示无条件打印（本次安装前 stop 的 daemon 不会自动恢复）。
# 按与自启迁移提示相同的 PATH 生效维度分派：PATH 未写入时任何终端都解析不到裸 token-usage，
# 须给绝对路径形态；广播失败时新终端要注销重登后才有 PATH。
if (-not $pathPersisted) {
    Write-Output "若此前 token-usage 在运行（如本次安装前执行过 stop），重装后如需继续实时采集，请执行 & `"$TargetPath`" start（PATH 未写入，须用绝对路径形态）。"
} elseif (-not $needRelogon) {
    Write-Output "若此前 token-usage 在运行（如本次安装前执行过 stop），重装后如需继续实时采集，请在新终端执行 token-usage start。"
} else {
    Write-Output "若此前 token-usage 在运行（如本次安装前执行过 stop），重装后如需继续实时采集，请在注销重登后（PATH 生效后）执行 token-usage start。"
}

# 旧安装残留检测：旧版教程曾在 %LOCALAPPDATA%\Microsoft\WindowsApps 建 mklink 软链，
# 旧文档也曾引导把普通 exe 副本放进 PATH 目录；两者同样遮蔽新布局（用户 PATH 默认
# 包含该目录且先于追加条目，软链残留还会触发自更新来源校验拒绝）。
# 用目录枚举判定、不跟随链接目标——Test-Path 对断链软链（目标已删）返回 false 会漏检；
# 枚举命中后再按 LinkType/ReparsePoint 区分链接类与普通文件（LinkType 为空但 ReparsePoint
# 置位的非软链形态按链接类同路处理，断链软链同样命中，判定不跟随目标），分别给出对应措辞。
# LOCALAPPDATA 未定义的病态会话（精简 CI 容器、服务上下文）下 Join-Path 抛两条裸英文语句级
# 错误且脚本继续，残留检测被静默跳过；改为打印降级提示后跳过检测（检测未执行须可察觉，
# 不影响安装结果与退出码语义）。
$windowsAppsDir = $null
try {
    $windowsAppsDir = Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps'
} catch {
    Write-Output "提示：无法定位 WindowsApps 目录检测旧安装残留（LOCALAPPDATA 未定义），请自行确认其中是否存在旧 token-usage.exe（旧软链或旧副本会遮蔽新布局）。"
}
$legacyResiduePath = $null
if ($windowsAppsDir) {
    $legacyResiduePath = Join-Path $windowsAppsDir 'token-usage.exe'
}
$legacyLinkFound = $false
$legacyFileFound = $false
$legacyDirFound = $false
if ($windowsAppsDir -and (Test-Path -LiteralPath $windowsAppsDir -PathType Container)) {
    try {
        # Stop 把 ACL 拒绝等非终止错误升级为终止错误，确保下方 catch 降级提示真实可达（检测未执行须可察觉）。
        # -Filter 走文件系统 8.3 短名匹配，`token-usage.exe` 会误命中 `token-usage.exe.bak` 等长名变体，
        # 故改用全枚举 + 精确名等值匹配。
        $residueHits = Get-ChildItem -LiteralPath $windowsAppsDir -Force -ErrorAction Stop | Where-Object { $_.Name -eq 'token-usage.exe' }
        foreach ($hit in @($residueHits)) {
            if ($hit.PSIsContainer) {
                # 同名目录属病态形态：Remove-Item -Force 对非空目录必然失败，不给出删除命令，
                # 改为请用户手动确认处理。
                $legacyDirFound = $true
            } elseif ($hit.LinkType -or ($hit.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                $legacyLinkFound = $true
            } else {
                $legacyFileFound = $true
            }
        }
    } catch {
        # 枚举被 ACL 拒绝等失败只降级提示，不影响安装（残留检测未执行须可察觉）。
        Write-Output "提示：无法枚举 $windowsAppsDir 检测旧安装残留，请自行确认是否存在 $legacyResiduePath（旧软链或旧副本会遮蔽新布局）。"
    }
}

# 验证提示（PATH 未写入时措辞不依赖新终端；广播失败时切换为注销重登形态，与 PATH 实际生效方式一致）。
if (-not $pathPersisted) {
    Write-Output "PATH 未写入（见上方提示）：请先手动把 $BinDir 加入用户 PATH 并注销重登，之后执行 token-usage version 验证（PATH 未写入前新终端解析不到 token-usage，不能作为验证依据；若 PATH 中更靠前处存在旧副本——任意目录旧普通 exe 与 WindowsApps 软链同样构成遮蔽，先删除该旧副本再执行自启迁移步骤，否则新终端解析到旧 exe、BinPath 与既有定义恰好匹配，迁移静默失效）。"
} elseif (-not $needRelogon) {
    Write-Output "请从开始菜单/任务栏启动新终端窗口，执行 token-usage version 验证，并确认 Get-Command token-usage 指向 $BinDir（否则 PATH 中更靠前处存在旧副本——任意目录旧普通 exe 与 WindowsApps 软链同样构成遮蔽，先删除该旧副本再执行自启迁移步骤，否则新终端解析到旧 exe、BinPath 与既有定义恰好匹配，迁移静默失效）。注意：已打开的 Windows Terminal 中新开标签页继承旧环境，不能作为验证依据。"
} else {
    Write-Output "请注销后重新登录，再从开始菜单/任务栏启动新终端窗口，执行 token-usage version 验证，并确认 Get-Command token-usage 指向 $BinDir（否则 PATH 中更靠前处存在旧副本——任意目录旧普通 exe 与 WindowsApps 软链同样构成遮蔽，先删除该旧副本再执行自启迁移步骤，否则新终端解析到旧 exe、BinPath 与既有定义恰好匹配，迁移静默失效）。"
}

# 残留删除指引（若检测到）：WindowsApps 目录 ACL 特殊，不自动删除，由用户执行。
if ($legacyLinkFound) {
    Write-Output "检测到旧版链接类安装残留：$legacyResiduePath（软链或其他 reparse point 文件；用户 PATH 默认包含该目录且先于新安装位置，会遮蔽新布局并导致自更新被拒绝）。请手动删除（WindowsApps 目录权限特殊，脚本不自动删除）：Remove-Item -LiteralPath `"$legacyResiduePath`" -Force"
}
if ($legacyFileFound) {
    Write-Output "检测到 $legacyResiduePath 存在同名残留文件（旧副本，用户 PATH 默认包含该目录且先于新安装位置，会遮蔽新布局）。请手动删除（WindowsApps 目录权限特殊，脚本不自动删除）：Remove-Item -LiteralPath `"$legacyResiduePath`" -Force"
}
if ($legacyDirFound) {
    Write-Output "检测到 $legacyResiduePath 位置被同名目录占用（病态形态），请手动确认处理。"
}

# 自启迁移提示（无条件打印；PATH 未写入或广播失败时切换为对应形态）。
if (-not $pathPersisted) {
    Write-Output "若此前开启过开机自启，请在手动加入用户 PATH 并注销重登（PATH 生效后）执行 token-usage config set daemon.autostart true 确保定义指向新位置（若配置文件不存在，先执行 token-usage config init）。"
} elseif (-not $needRelogon) {
    Write-Output "若此前开启过开机自启，请在新终端（PATH 生效后）执行 token-usage config set daemon.autostart true 确保定义指向新位置（若配置文件不存在，先执行 token-usage config init）。"
} else {
    Write-Output "若此前开启过开机自启，请在注销重登后（PATH 生效后）执行 token-usage config set daemon.autostart true 确保定义指向新位置（若配置文件不存在，先执行 token-usage config init）。"
}
if ($legacyLinkFound -or $legacyFileFound) {
    Write-Output "若上方提示了 WindowsApps 残留（旧软链或旧副本文件），请先删除该残留再执行自启迁移步骤（不删残留时命令解析到旧位置、BinPath 与既有定义恰好匹配，迁移会静默失效）。"
}
