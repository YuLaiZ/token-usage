#!/usr/bin/env bash
# token-usage 官方安装脚本（macOS）。
#
# 自动检测 CPU 架构、下载最新稳定版官方 Release 二进制、
# 用官方 SHA256SUMS 校验、安装到 ~/.token-usage/bin 并验证版本。
# 默认布局下自动把安装目录写入登录 shell 的 PATH 配置（幂等 marker 块），
# 并检测清理旧布局遗留的 /usr/local/bin 副本、提示自启定义迁移。
# 用法（README 一句话安装）：
#   curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
# 可选环境变量：
#   TAG=vX.Y.Z                  指定版本（默认自动取最新稳定版）
#   INSTALL_DIR=/path                 安装目录（默认 ~/.token-usage/bin；测试用）
#   LEGACY_BIN=/path/to/token-usage   旧布局副本检测路径（默认 /usr/local/bin/token-usage；仅供受控测试使用）
#   INSTALL_COMPLETION=yes            非交互自动配置 Shell 补全（默认按交互询问；
#                                     语义 = 非交互执行交互流程的默认推荐路径，
#                                     面向 AI agent 一句话安装等无人应答场景）
set -euo pipefail

REPO="YuLaiZ/token-usage"
DEFAULT_INSTALL_DIR="$HOME/.token-usage/bin"
INSTALL_DIR="${INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
LEGACY_BIN="${LEGACY_BIN:-/usr/local/bin/token-usage}"
TAG="${TAG:-}"

# 平台检测：仅官方资产覆盖的 macOS（Apple Silicon / Intel）。
os="$(uname -s)"
arch="$(uname -m)"
case "${os}-${arch}" in
  Darwin-arm64)      asset="token-usage-darwin-arm64" ;;
  Darwin-x86_64|Darwin-amd64) asset="token-usage-darwin-amd64" ;;
  *)
    echo "错误：官方资产仅支持 macOS（Apple Silicon / Intel）；当前为 ${os}-${arch}。" >&2
    echo 'Windows 用户可在 PowerShell 中依次执行两条命令：' >&2
    echo '  irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"' >&2
    echo '  powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"' >&2
    echo '其他平台请从源码构建。' >&2
    exit 1
    ;;
esac

# 未显式指定版本时，从 GitHub API 的 latest 端点取最新稳定版 tag。
if [ -z "${TAG}" ]; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  if [ -z "${TAG}" ]; then
    echo "错误：无法获取最新稳定版 Release tag。" >&2
    exit 1
  fi
fi

echo "安装 token-usage ${TAG}（${asset}）到 ${INSTALL_DIR} ..."

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

base="https://github.com/${REPO}/releases/download/${TAG}"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"

# 按资产名精确匹配 SHA256SUMS 并校验（文件名与资产名一致）。
(cd "${tmp}" && grep "  ${asset}$" SHA256SUMS | shasum -a 256 -c -)

chmod u+x "${tmp}/${asset}"
mkdir -p "${INSTALL_DIR}"
if [ -w "${INSTALL_DIR}" ]; then
  mv "${tmp}/${asset}" "${INSTALL_DIR}/token-usage"
else
  sudo mv "${tmp}/${asset}" "${INSTALL_DIR}/token-usage"
fi

# 去掉路径末尾的斜杠（用于路径间字面比较与输出展示，兼容用户传入的尾斜杠变体）。
normalize_dir() {
  local p="$1"
  while [ "${p}" != "/" ] && [ "${p%/}" != "${p}" ]; do
    p="${p%/}"
  done
  printf '%s' "${p}"
}

# INSTALL_DIR 先归一再拼接：完成提示与下方指向确认提示输出同一路径形态，
# 避免尾斜杠输入下出现 bin//token-usage 这类展示瑕疵（多尾斜杠同样归一）。
BIN_PATH="$(normalize_dir "${INSTALL_DIR}")/token-usage"
"${BIN_PATH}" version
echo "完成：token-usage 已安装到 ${BIN_PATH}"

# ---------- 补全配置段（Tab 补全自动安装） ----------
# 分层自动化：zsh 因涉及用户决策（compinit 初始化与目录权限处理）采用交互，
# INSTALL_COMPLETION=yes 时非交互执行默认推荐路径（chmod 修复 + 标准 compinit，
# 均无安全让步；-u 跳过安全检查永远只能经交互显式选择）；fish 零前置全自动；
# bash 的瓶颈是 bash-completion 包缺失、仅指引；其他 shell 不打印。本段任何
# 失败只打印提示，不影响安装主流程结果与退出码。非默认布局（覆盖安装目录）
# 跳过本段，口径与下方 PATH 段一致。

# 布局判定：是否为默认安装布局（去尾斜杠后字面比较，尾斜杠变体视同默认布局）。
normalized_install_dir="$(normalize_dir "${INSTALL_DIR}")"
normalized_default_dir="$(normalize_dir "${DEFAULT_INSTALL_DIR}")"
if [ "${normalized_install_dir}" = "${normalized_default_dir}" ]; then
  is_default_layout=1
else
  is_default_layout=0
fi

# INSTALL_COMPLETION 开关：仅 yes 一个触发值；未设置按默认询问；其他非空值打印
# 警告并按默认处理（防拼写错误静默走错路径）。
completion_auto=0
if [ -n "${INSTALL_COMPLETION:-}" ] && [ "${INSTALL_COMPLETION}" != "yes" ]; then
  echo "警告：INSTALL_COMPLETION 仅支持 yes（当前值 \"${INSTALL_COMPLETION}\"），已按默认询问流程处理。"
fi
if [ "${INSTALL_COMPLETION:-}" = "yes" ]; then
  completion_auto=1
fi

cmpl_uid="$(id -u)"
cmpl_user="$(id -un)"

# cmpl_sh_quote：指引命令的 shell 安全拼装——路径与 ACL 表达式一律单引号包裹，
# 内含单引号以 '\'' 转义（POSIX/zsh 单引号内零展开零解释，fish 同一转义结果
# 一致），保证产出的 chmod 命令可整行复制执行（路径含空格/单引号均不拆参）。
# 单引号替换用 sed 而非 bash 的 ${var//}：macOS 系统 bash 3.2 对替换串中
# 反斜杠转义的解析与 bash 4+ 不同，实测产出错误转义形态。
cmpl_sh_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# cmpl_list_inline：把换行分隔的目录清单转为空格分隔单行（提示行内展示用）。
cmpl_list_inline() {
  printf '%s\n' "$1" | sed '/^$/d' | tr '\n' ' ' | sed 's/ $//'
}

# cmpl_dir_owner_trusted：目录属主是否 ∈ {当前用户, root}。第三方属主的任何
# 权限形态都等效可写（chmod 只校验所有权：555 属主一步 u+w 即恢复写位，
# group/other 写位检查完全看不到属主维度）。
cmpl_dir_owner_trusted() {
  local owner
  owner="$(stat -f %u "$1" 2>/dev/null)" || return 1
  [ "${owner}" = "0" ] || [ "${owner}" = "${cmpl_uid}" ]
}

# cmpl_dir_sticky_owner_exempt：sticky 豁免仅在目录属主为当前用户或 root 时成立
# （sticky 原文语义：目录属主对其子项有删除/重命名权——第三方属主的 sticky
# 目录对其属主等价于无 sticky 的可写目录）。
cmpl_dir_sticky_owner_exempt() {
  local mode
  mode="$(stat -f %p "$1" 2>/dev/null)" || return 1
  [ $(( 8#${mode} & 8#1000 )) -ne 0 ] && cmpl_dir_owner_trusted "$1"
}

# cmpl_acl_threat：macOS ACL 威胁判定。解析 ls -lde 条目行（N: principal
# allow|deny perm[,flag...]）：deny 不授予任何权限、一律跳过；allow 且主体非
# 信任方（信任方仅 user:当前用户 与 user:root；group:* 与 everyone 一律非信任
# ——组成员无法静态枚举）且权限集与威胁类有交集即命中。flag 保留字（inherited
# / file_inherit / directory_inherit / only_inherit / limit_inherit）从权限集
# 剥离、不影响命中（带继承标志的 allow 威胁同样命中）。compaudit 对 ACL 完全
# 盲区（0755 + 属主正确 + everyone allow 写类 → rc=0），ACL 必须与写位、属主
# 并行判定。威胁类：目录 = add_file/add_subdirectory/delete_child/delete +
# 管理类 writeattr/writeextattr/writesecurity/chown（writesecurity 可改 ACL
# = 间接全控）；文件 = write/append/delete + 管理类（继承 ACL 使 umask 022 的
# 新建文件仍带 everyone allow write）。
# 返回：0 无威胁；1 有威胁（全局 cmpl_acl_hits 每行一个命中条目，格式
# 「显示原文<TAB>剥离序号前缀的 ACL 表达式」——chmod -a 只接受剥离 N: 前缀的
# 表达式，整行显示原文实测报错且不删除）；2 无法判定（ls 失败）。
cmpl_acl_threat() {
  local path="$1" kind="$2" acl_out hits="" rc=0
  acl_out="$(ls -lde "${path}" 2>/dev/null)" || return 2
  hits="$(printf '%s\n' "${acl_out}" | tail -n +2 | awk \
    -v user_name="${cmpl_user}" -v is_file="$([ "${kind}" = "file" ] && printf 1 || printf 0)" '
    /^[[:space:]]*[0-9]+:/ {
      raw = $0
      sub(/^[[:space:]]+/, "", raw)
      expr = raw
      sub(/^[0-9]+:[[:space:]]*/, "", expr)
      if (expr !~ / allow /) next
      principal = expr; sub(/ allow .*/, "", principal)
      perms = expr; sub(/.* allow /, "", perms)
      if (principal == "user:" user_name || principal == "user:root") next
      hit = 0
      n = split(perms, arr, ",")
      for (i = 1; i <= n; i++) {
        p = arr[i]
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", p)
        if (p == "inherited" || p == "file_inherit" || p == "directory_inherit" || p == "only_inherit" || p == "limit_inherit") continue
        if (is_file) {
          if (p == "write" || p == "append" || p == "delete" || p == "writeattr" || p == "writeextattr" || p == "writesecurity" || p == "chown") { hit = 1; break }
        } else {
          if (p == "add_file" || p == "add_subdirectory" || p == "delete_child" || p == "delete" || p == "writeattr" || p == "writeextattr" || p == "writesecurity" || p == "chown") { hit = 1; break }
        }
      }
      if (hit) { printf "%s\t%s\n", raw, expr; found = 1 }
    }
    END { exit found ? 1 : 0 }
  ')" || rc=$?
  case "${rc}" in
    0) return 0 ;;
    1)
      if [ -n "${hits}" ]; then
        cmpl_acl_hits="${hits}"
        return 1
      fi
      return 0
      ;;
    *) return 2 ;;
  esac
}

# cmpl_wd_verdict：敌方可写 W(D) 三维度判定，输出 ok / owner / acl / writable /
# error（stat 失败的竞态，按无法证明安全处理）。多维并发按优先级报告首个命中：
# 属主类 > ACL 类 > 可写类（属主与 ACL 类的 chmod go-w 均无效、各自专用指引）。
cmpl_wd_verdict() {
  local dir="$1" perms rc=0
  cmpl_dir_owner_trusted "${dir}" || { printf 'owner'; return 0; }
  cmpl_acl_threat "${dir}" dir || rc=$?
  [ "${rc}" -eq 0 ] || { printf 'acl'; return 0; }
  perms="$(stat -f %Lp "${dir}" 2>/dev/null)" || { printf 'error'; return 0; }
  if [ $(( 8#${perms} & 8#022 )) -ne 0 ]; then
    if cmpl_dir_sticky_owner_exempt "${dir}"; then
      printf 'ok'
    else
      printf 'writable'
    fi
    return 0
  fi
  printf 'ok'
}

# cmpl_print_acl_reject：ACL 类命中的降级提示。chmod go-w 对 ACL 无效、不给
# go-w 建议；修复命令按 shell 安全拼装规则整行可复制执行（chmod -a 精确移除
# 主选；chmod -N 清空备选——注意会一并移除 deny 保护条目）。属主非当前用户时
# 改属主异常口径（chmod 对非属主必失败）。
cmpl_print_acl_reject() {
  local path="$1" raw expr
  echo "检测到 ${path} 的 ACL 授予其他用户写入/删除权限，补全自动配置已跳过。"
  if [ "$(stat -f %u "${path}" 2>/dev/null)" = "${cmpl_uid}" ]; then
    cmpl_acl_threat "${path}" dir || true
    while IFS=$'\t' read -r raw expr; do
      [ -n "${raw}" ] || continue
      echo "  条目：${raw}"
      echo "  请执行 chmod -a $(cmpl_sh_quote "${expr}") $(cmpl_sh_quote "${path}") 精确移除该条目。"
    done <<CMPL_ACL_EOF
${cmpl_acl_hits}
CMPL_ACL_EOF
    echo "  或执行 chmod -N $(cmpl_sh_quote "${path}") 清空全部 ACL（注意会一并移除 deny 保护条目）。处理后重跑安装脚本。"
  else
    echo "该目录属主非当前用户，请人工核查属主与用途（必要时以管理员修复属主，fish XDG 场景亦可改用默认落点）后重跑。"
  fi
}

# cmpl_run_with_timeout：探测命令超时守护（macOS 无 timeout 命令）。用系统
# perl 的 alarm + exec 形态（alarm 定时器与 SIGALRM 默认处置跨 exec 保留，
# 到时杀死被 exec 的探测命令）——不引入后台 watcher 子 shell：kill 掉
# watcher 不会终止其子 sleep，探测完成后会遗留整段计时期的孤儿 sleep 进程。
# stdout 写入全局 cmpl_probe_out，退出码写入 cmpl_probe_rc（超时被杀时为 142，
# 由调用方按「非预期退出码」归入无法判定）。perl 不可用时退回无守护直跑
#（探测命令均为本地 zsh 短命令，阻塞风险来自极端环境，unknown 降级兜底）。
cmpl_run_with_timeout() {
  local timeout_sec="$1"; shift
  cmpl_probe_rc=0
  cmpl_probe_out=""
  if command -v perl >/dev/null 2>&1; then
    perl -e 'alarm shift; exec @ARGV' "${timeout_sec}" "$@" >"${tmp}/cmpl-probe.out" 2>"${tmp}/cmpl-probe.err" || cmpl_probe_rc=$?
  else
    "$@" >"${tmp}/cmpl-probe.out" 2>"${tmp}/cmpl-probe.err" || cmpl_probe_rc=$?
  fi
  cmpl_probe_out="$(cat "${tmp}/cmpl-probe.out" 2>/dev/null)" || cmpl_probe_out=""
}

# cmpl_chain_boundary：完整替换链信任边界（补全段第一步，先于 mkdir 与全部
# 探测）。自链顶层起（含其自身）逐级上溯检查至文件系统根，每层组件四态分类
#（lstat 语义）：symlink（含断链）→ 保守降级（词法链与解析后真实链分离）；
# 目录 → 入判定集；ENOENT → 待建层（缺失层尚无任何可被替换或加载的组件，
# 不构成判定对象）；其余形态（lstat 成功但既非 symlink 又非目录，如被劣质
# 工具建成普通文件的 ~/.config）与 ENOENT 以外的 stat 错误 → 降级。对判定集
# 每层 L 双条件并行判定：① W(L)（含链顶层自身写位）；② W(parent(L))——L 可
# 被整体 rename、连同其下全部子树（任何中间安全目录都不是独立信任锚点：
# parent=777 → anchor=755 → cfg=755 时 anchor 仍可被 parent 整体替换）。
# 命中 → 打印降级提示（多维优先级属主类 > ACL 类 > 可写类）并返回 1。
# top_kind=home 且链顶 ENOENT → 主目录缺失专属降级（家目录是登录语义实体，
# 绝不 mkdir $HOME）；top_kind=cfg 的链顶/链内 ENOENT → 待建层（返回 0，由
# 调用方进入安全 mkdir）。
cmpl_chain_boundary() {
  local chain_top="$1" top_kind="$2"
  local layer stat_out stat_rc verdict d parent
  local -a judge=()
  local first_owner="" first_acl="" first_writable="" first_error=""
  layer="${chain_top}"
  while :; do
    stat_out="$(stat -f %HT "${layer}" 2>&1)"
    stat_rc=$?
    if [ "${stat_rc}" -ne 0 ]; then
      case "${stat_out}" in
        *"No such file or directory"*)
          if [ "${layer}" = "${chain_top}" ]; then
            case "${top_kind}" in
              home)
                echo "检测到主目录 ${chain_top} 不存在，环境异常，补全自动配置已跳过；请修复主目录环境后重跑安装脚本。"
                return 1
                ;;
              zdot)
                # ZDOTDIR 启动根缺失属环境异常而非待建层：自动创建会在用户
                # 未配置的位置制造启动文件，降级导向用户核查。
                echo "检测到 ZDOTDIR 指向的目录 ${chain_top} 不存在，补全自动配置已跳过；请核查 ZDOTDIR 配置后重跑安装脚本。"
                return 1
                ;;
            esac
          fi
          # 待建层：不构成判定对象，继续上溯
          ;;
        *)
          echo "检测到 ${layer} 存在无法判定安全的组件形态（symlink / 非目录实体 / 无法访问），补全自动配置已跳过；请人工核查该路径后重跑安装脚本。"
          return 1
          ;;
      esac
    else
      case "${stat_out}" in
        "Directory") judge+=("${layer}") ;;
        *)
          echo "检测到 ${layer} 存在无法判定安全的组件形态（symlink / 非目录实体 / 无法访问），补全自动配置已跳过；请人工核查该路径后重跑安装脚本。"
          return 1
          ;;
      esac
    fi
    [ "${layer}" = "/" ] && break
    layer="$(dirname "${layer}")"
  done
  for d in "${judge[@]}"; do
    verdict="$(cmpl_wd_verdict "${d}")"
    case "${verdict}" in
      owner) [ -n "${first_owner}" ] || first_owner="${d}" ;;
      acl) [ -n "${first_acl}" ] || first_acl="${d}" ;;
      writable) [ -n "${first_writable}" ] || first_writable="${d}" ;;
      error) [ -n "${first_error}" ] || first_error="${d}" ;;
    esac
    parent="$(dirname "${d}")"
    verdict="$(cmpl_wd_verdict "${parent}")"
    case "${verdict}" in
      owner) [ -n "${first_owner}" ] || first_owner="${parent}" ;;
      acl) [ -n "${first_acl}" ] || first_acl="${parent}" ;;
      writable) [ -n "${first_writable}" ] || first_writable="${parent}" ;;
      error) [ -n "${first_error}" ] || first_error="${parent}" ;;
    esac
  done
  if [ -n "${first_error}" ]; then
    echo "检测到 ${first_error} 存在无法判定安全的组件形态（symlink / 非目录实体 / 无法访问），补全自动配置已跳过；请人工核查该路径后重跑安装脚本。"
    return 1
  fi
  if [ -n "${first_owner}" ]; then
    echo "检测到 ${first_owner} 属主为其他用户，补全自动配置已跳过；请人工核查该目录属主与用途（必要时以管理员修复属主，fish XDG 场景亦可改用默认落点）后重跑。"
    return 1
  fi
  if [ -n "${first_acl}" ]; then
    cmpl_print_acl_reject "${first_acl}"
    return 1
  fi
  if [ -n "${first_writable}" ]; then
    # 可写类命中且属主非当前用户（如 root 属主）时 chmod 对非属主必失败，
    # 改属主异常口径、不给 chmod go-w 建议。
    if [ "$(stat -f %u "${first_writable}" 2>/dev/null)" = "${cmpl_uid}" ]; then
      echo "检测到 ${first_writable} 对组/其他用户可写，补全自动配置已跳过；请手动执行 chmod go-w $(cmpl_sh_quote "${first_writable}") 收紧后重跑安装脚本。"
    else
      echo "检测到 ${first_writable} 属主为其他用户，补全自动配置已跳过；请人工核查该目录属主与用途（必要时以管理员修复属主，fish XDG 场景亦可改用默认落点）后重跑。"
    fi
    return 1
  fi
  return 0
}

# cmpl_resolve_zdotdir：P0 三态解析。① 环境存在且为有效绝对路径 → 启动根 =
# $ZDOTDIR；② 环境存在但为空、字面 ~/...（zsh 使用变量时不展开 ~）或相对
# 路径 → 解析失败（环境已显式定义，静态文件无法推翻，也不扫 .zshenv——空值
# 是「已定义为空」而非未设置，实测空值拼接得 /.zshrc ≠ $HOME/.zshrc）；
# ③ 环境不存在 → ~/.zshenv 仅在封闭形态时接受（整个文件逐行分类后仅含空行、
# 注释行与恰好一条支持型简单赋值——行首 (export )?ZDOTDIR=，值为绝对路径或
# 未加引号的 ~/...；引号 ~/... 存字面值不展开；出现任何其他可执行语句——
# if / case / 函数 / source / eval / 命令替换 / 其他命令——一律解析失败：
# grep 无法证明任意 shell 文件的控制流与最终赋值，仅机械可验证的封闭形态可
# 接受）；.zshenv 无相关行（含文件不存在）→ $HOME。结果写入 cmpl_startup_root，
# 解析失败返回 1。
cmpl_resolve_zdotdir() {
  if printenv ZDOTDIR >/dev/null 2>&1; then
    local zd="${ZDOTDIR}"
    case "${zd}" in
      /*) cmpl_startup_root="${zd}"; return 0 ;;
      *) return 1 ;;
    esac
  fi
  local zshenv="${HOME}/.zshenv" line orig unq found=0
  if [ -e "${zshenv}" ] || [ -L "${zshenv}" ]; then
    if [ ! -f "${zshenv}" ] || [ ! -r "${zshenv}" ]; then
      # 文件存在但不是可读的普通文件：无法静态证明封闭形态
      return 1
    fi
    while IFS= read -r line || [ -n "${line}" ]; do
      case "${line}" in
        *[![:space:]]*) ;;
        *) continue ;;
      esac
      case "${line}" in
        \#*) continue ;;
        [[:space:]]*\#*) continue ;;
      esac
      orig=""
      case "${line}" in
        ZDOTDIR=*) orig="${line#ZDOTDIR=}" ;;
        export\ ZDOTDIR=*) orig="${line#export ZDOTDIR=}" ;;
        *) return 1 ;;
      esac
      [ "${found}" -eq 0 ] || return 1
      found=1
      unq="${orig}"
      if [ "${#orig}" -ge 2 ]; then
        case "${orig}" in
          \"*\") unq="${orig#\"}"; unq="${unq%\"}" ;;
          \'*\') unq="${orig#\'}"; unq="${unq%\'}" ;;
        esac
      fi
      case "${orig}" in
        '~/'*) cmpl_startup_root="${HOME}${orig#\~}" ;;
        *)
          case "${unq}" in
            /*) cmpl_startup_root="${unq}" ;;
            *) return 1 ;;
          esac
          ;;
      esac
    done < "${zshenv}"
  fi
  if [ "${found}" -eq 0 ]; then
    cmpl_startup_root="${HOME}"
  fi
  return 0
}

# cmpl_startup_files：marker 执行前的启动文件统一列表（P1 与 P3 的 brew 探测
# 共用同一份；按 zsh 启动顺序；/etc/zlogin 与 .zlogin 在 marker 之后执行、
# 不纳入；ZDOTDIR 场景下 HOME .zshenv 不在此链——zsh 读 $ZDOTDIR/.zshenv，
# 仅承担 P0 解析职责）。
cmpl_startup_files() {
  printf '%s\n' /etc/zshenv "${cmpl_startup_root}/.zshenv" /etc/zprofile "${cmpl_startup_root}/.zprofile" /etc/zshrc "${cmpl_startup_root}/.zshrc"
}

# cmpl_detect_compinit：P1 静态检测「可能已启用」——统一启动文件列表过滤注释
# 行后判调用形态（行含 compinit 且非纯 autoload 声明行；autoload -U compinit
# && compinit 同行第二个 compinit 会命中）。静态 grep 无法确认启用，完成提示
# 用「静态检测」措辞；不存在的候选文件视为正常跳过、存在的文件读取失败视同
# 未启用。判定「已初始化」的真判据是 compdef 是否存在（仅 autoload 不构成）。
cmpl_detect_compinit() {
  local f
  while IFS= read -r f; do
    [ -f "${f}" ] && [ -r "${f}" ] || continue
    if grep -v '^[[:space:]]*#' "${f}" 2>/dev/null | grep 'compinit' 2>/dev/null \
       | grep -Evq '^[[:space:]]*autoload([[:space:]]+-[[:alnum:]]+)*[[:space:]]+compinit[[:space:]]*$'; then
      return 0
    fi
  done < <(cmpl_startup_files)
  return 1
}

# cmpl_fpath_baseline：P2 静态 fpath 基线（zsh -f 编译默认条目，与用户环境的
# 基础部分一致且可信；探测不执行任何用户启动文件）。失败 → 空清单。结果写入
# cmpl_fpath_entries（换行分隔）。
cmpl_fpath_baseline() {
  cmpl_fpath_entries=""
  cmpl_run_with_timeout 5 zsh -f -c 'print TU_CMPL_FPATH:${(j.:.)fpath}'
  [ "${cmpl_probe_rc}" -eq 0 ] || return 0
  case "${cmpl_probe_out}" in
    TU_CMPL_FPATH:*) cmpl_fpath_entries="$(printf '%s' "${cmpl_probe_out#TU_CMPL_FPATH:}" | tr ':' '\n')" ;;
  esac
  return 0
}

# cmpl_brew_candidates：统一启动文件列表非注释行出现 brew shellenv 直接调用
# 形态时，纳入对应 brew 的 share/zsh/site-functions 目录（系统启动文件同样
# 生效——只扫用户启动根会漏检；无条件复刻 brew 的做法已删除：装了 brew 但
# 启动配置未启用时纳入会制造不在用户 fpath 的虚构目录）。结果写入
# cmpl_brew_entries（换行分隔，仅存在的目录、去重）。
cmpl_brew_candidates() {
  cmpl_brew_entries=""
  local f line token brew_bin site found=""
  while IFS= read -r f; do
    [ -f "${f}" ] && [ -r "${f}" ] || continue
    while IFS= read -r line; do
      token="$(printf '%s\n' "${line}" | grep -oE '[^()"$'"'"'[:space:]]*/brew|[[:<:]]brew' 2>/dev/null | head -n 1)" || true
      [ -n "${token}" ] || continue
      brew_bin="${token}"
      case "${brew_bin}" in
        */brew) ;;
        brew)
          brew_bin="$(command -v brew 2>/dev/null)" || brew_bin=""
          [ -n "${brew_bin}" ] || continue
          ;;
        *) continue ;;
      esac
      site="$(dirname "$(dirname "${brew_bin}")")/share/zsh/site-functions"
      if [ -d "${site}" ] && ! printf '%s\n' "${found}" | grep -qxF "${site}"; then
        found="${site}
${found}"
      fi
    done < <(grep -v '^[[:space:]]*#' "${f}" 2>/dev/null | grep 'brew shellenv' 2>/dev/null || true)
  done < <(cmpl_startup_files)
  cmpl_brew_entries="${found}"
}

# cmpl_run_compaudit：P3 权威检查。临时 fpath = P2 基线 ∪ brew 候选（启动配置
# 静态确认）∪ 落点目录本身（无论 mkdir 新建还是既有——只收既有落点时，
# .zsh 既有 777 而落点本次安全新建的场景会漏查父目录；父目录 .zsh 绝不入
# 临时 fpath——入则检查上推到 $HOME，comaudit 逐条目查「自身 + 直接父目录」）。
# compaudit 退出码 1 且输出目录是预期结果而非执行失败（按非零即失败处理会把
# 不安全目录全部吞成空清单）。输出经 chmod allowlist 过滤：允许集 = 每个临时
# fpath 候选目录及其直接父目录（compaudit 对每个条目报「自身 + 父」，allowlist
# 只收候选本身会丢弃父目录，选项 1 只修 site-functions 而漏 share/zsh）；
# allowlist 之外的任何输出一律忽略、绝不 chmod。结果三态写入 cmpl_p3_state
#（clean / insecure / unknown——超时、无法加载 compaudit、退出码其他值、输出
# 无法解析均归 unknown：无法证明安全即不写 marker / 文件 / autoload），
# insecure 清单（已过滤）写入 cmpl_p3_list（换行分隔）。
cmpl_run_compaudit() {
  cmpl_p3_state="unknown"
  cmpl_p3_list=""
  local candidates="${cmpl_fpath_entries}"
  [ -n "${cmpl_brew_entries}" ] && candidates="${candidates}
${cmpl_brew_entries}"
  candidates="${candidates}
${cmpl_zsh_landing_dir}"
  local fp="" c line hits="" saw_dir=0 allowlist=""
  while IFS= read -r c; do
    [ -n "${c}" ] || continue
    fp="${fp}$(cmpl_sh_quote "${c}") "
  done <<CMPL_CAND_EOF
${candidates}
CMPL_CAND_EOF
  cmpl_run_with_timeout 10 zsh -f -c "fpath=(${fp}\$fpath); autoload -U compaudit; compaudit"
  case "${cmpl_probe_rc}" in
    0) cmpl_p3_state="clean"; return 0 ;;
    1) ;;
    *) cmpl_p3_state="unknown"; return 0 ;;
  esac
  allowlist="${candidates}"
  while IFS= read -r c; do
    [ -n "${c}" ] || continue
    allowlist="${allowlist}
$(dirname "${c}")"
  done <<CMPL_ALLOW_EOF
${candidates}
CMPL_ALLOW_EOF
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    case "${line}" in
      /*) saw_dir=1 ;;
      *) cmpl_p3_state="unknown"; return 0 ;;
    esac
    if printf '%s\n' "${allowlist}" | grep -qxF "${line}"; then
      hits="${hits}${line}
"
    fi
  done <<CMPL_OUT_EOF
${cmpl_probe_out}
CMPL_OUT_EOF
  [ "${saw_dir}" -eq 1 ] || { cmpl_p3_state="unknown"; return 0; }
  cmpl_p3_state="insecure"
  cmpl_p3_list="${hits}"
}

# cmpl_marker_block：completion marker 的 canonical 形态（三变体仅 else 分支的
# compinit 后缀不同）。双分支自适应：已启用环境（compdef 存在）走 if 分支直接
# 注册；未启用环境走 else 分支初始化 compinit 后注册（补全脚本首行 #compdef
# 由 compinit 扫描 fpath 自动注册）。补全目录固定于 $HOME 而非 ZDOTDIR：marker
# 内 $HOME 在任何 ZDOTDIR 布局下都指向真实家目录，fpath 挂载不依赖 ZDOTDIR。
cmpl_marker_block() {
  local comp_call="  compinit"
  case "$1" in
    u) comp_call="  compinit -u" ;;
    i) comp_call="  compinit -i" ;;
  esac
  printf '# >>> token-usage completion >>>\nfpath=("$HOME/.zsh/completions" $fpath)\nif (( $+functions[compdef] )); then\n  autoload -Uz _token-usage\n  compdef _token-usage token-usage\nelse\n  autoload -U compinit\n%s\nfi\n# <<< token-usage completion <<<\n' "${comp_call}"
}

# cmpl_marker_state：检测 rc 中 completion marker 的状态，输出 none /
# canonical-<standard|u|i> / invalid。canonical 前置：恰好一处头行 + 恰好一处
# 尾行，且头尾之间内容与三种 canonical 之一逐字节匹配；仅凭单个头行或块内
# compinit 字样不得推断 mode（缺尾时整块替换范围失控会吞掉头行之后的用户内容，
# 重复块替换目标不明，被编辑块的头尾之间可能含用户添加行）。
cmpl_marker_state() {
  local rc_file="$1" h t loose mid mode
  h="$(grep -c '^# >>> token-usage completion >>>$' "${rc_file}" 2>/dev/null)" || true
  t="$(grep -c '^# <<< token-usage completion <<<$' "${rc_file}" 2>/dev/null)" || true
  loose="$(grep -cE 'token-usage completion (>>>|<<<)' "${rc_file}" 2>/dev/null)" || true
  [ -n "${h}" ] || h=0
  [ -n "${t}" ] || t=0
  [ -n "${loose}" ] || loose=0
  if [ "${h}" -eq 0 ] && [ "${t}" -eq 0 ] && [ "${loose}" -eq 0 ]; then
    echo none
    return 0
  fi
  if [ "${h}" -eq 1 ] && [ "${t}" -eq 1 ] && [ "${loose}" -eq 2 ]; then
    mid="$(sed -n '/^# >>> token-usage completion >>>$/,/^# <<< token-usage completion <<<$/p' "${rc_file}" | sed '1d;$d')"
    for mode in standard u i; do
      if [ "${mid}" = "$(cmpl_marker_block "${mode}" | sed '1d;$d')" ]; then
        echo "canonical-${mode}"
        return 0
      fi
    done
  fi
  echo invalid
}

# cmpl_rc_writable_check：rc 形态检查。FIFO/字符/块设备与断链 symlink 拒写
#（病态形态：读取阻塞或写入无效却报成功）；有效 symlink（指向已存在文件）与
# 硬链接（链接数 > 1）保守降级——rename 会分离硬链接两端 inode、把 symlink
# 替换成普通文件断开链接，不动链接与目标、给手动指引（自足指引，不叠加
# 指引①）。返回 0 = 可按普通文件写入。
cmpl_rc_writable_check() {
  local rc_file="$1" links
  if [ -p "${rc_file}" ] || [ -c "${rc_file}" ] || [ -b "${rc_file}" ]; then
    echo "提示：无法写入 ${rc_file}（非常规配置文件形态），completion 配置未写入。"
    return 1
  fi
  if [ -L "${rc_file}" ]; then
    if [ ! -e "${rc_file}" ]; then
      echo "提示：无法写入 ${rc_file}（断链符号链接），completion 配置未写入。"
      return 1
    fi
    echo "${rc_file} 为符号链接，安装脚本保守不改写；请手动编辑链接目标文件——无 marker 块则按 token-usage completion zsh --help 手动配置补全，已有旧块则手动编辑迁移；或解除链接后重跑安装脚本。"
    return 1
  fi
  if [ -f "${rc_file}" ]; then
    links="$(stat -f %l "${rc_file}" 2>/dev/null)" || links=""
    if [ -n "${links}" ] && [ "${links}" -gt 1 ]; then
      echo "${rc_file} 与其他文件为硬链接（链接数 ${links}），安装脚本保守不改写；请手动编辑其中一个文件——无 marker 块则按 token-usage completion zsh --help 手动配置补全，已有旧块则手动编辑迁移；或解除链接后重跑安装脚本。"
      return 1
    fi
  fi
  return 0
}

# cmpl_atomic_mv_rc：同目录临时文件 + mv 原子提交（不沿用多次 >> 追加——磁盘
# 满/进程中断会留下半个 marker，随后 canonical 规则判为非规范并拒绝修复）。
# 候选校验头尾行齐全；任一步失败 → 原 rc 字节不变，重跑安装可正常重试。
cmpl_atomic_mv_rc() {
  local rc_file="$1" cand="$2"
  local dir base tmpf
  dir="$(dirname "${rc_file}")"
  base="$(basename "${rc_file}")"
  if ! grep -q '^# >>> token-usage completion >>>$' "${cand}" 2>/dev/null \
     || ! grep -q '^# <<< token-usage completion <<<$' "${cand}" 2>/dev/null; then
    echo "提示：completion 配置写入失败（候选校验未通过），${rc_file} 保持不变。"
    return 1
  fi
  tmpf="${dir}/.${base}.token-usage.$$"
  if ! cp "${cand}" "${tmpf}" 2>/dev/null; then
    rm -f "${tmpf}" 2>/dev/null || true
    echo "提示：completion 配置写入失败，${rc_file} 保持不变。"
    return 1
  fi
  if ! mv -f "${tmpf}" "${rc_file}" 2>/dev/null; then
    rm -f "${tmpf}" 2>/dev/null || true
    echo "提示：completion 配置写入失败，${rc_file} 保持不变。"
    return 1
  fi
  return 0
}

# cmpl_write_marker_insert：首次插入。候选 = 原内容（末行无换行先补一个，避免
# marker 首行与用户末行粘连；空文件首行即 marker 首行、无多余空行）+ 完整
# marker 块。rc 不存在时同目录临时文件 + mv 原子创建。
cmpl_write_marker_insert() {
  local rc_file="$1" mode="$2"
  local cand="${tmp}/cmpl-rc-cand"
  if ! {
    if [ -s "${rc_file}" ]; then
      cat "${rc_file}"
      [ "$(tail -c 1 "${rc_file}" 2>/dev/null | od -An -tx1 | tr -d ' \n')" = "0a" ] || printf '\n'
    fi
    cmpl_marker_block "${mode}"
  } > "${cand}" 2>/dev/null; then
    echo "提示：completion 配置写入失败，${rc_file} 保持不变。"
    return 1
  fi
  cmpl_atomic_mv_rc "${rc_file}" "${cand}" || return 1
  echo "已把 completion 配置写入 ${rc_file}（新终端生效）。"
  return 0
}

# cmpl_write_marker_replace：canonical 块整块替换（按头尾行界定字节范围，不触碰
# 块外用户内容；块前内容末行无换行时补一个；块后内容原样衔接）。
cmpl_write_marker_replace() {
  local rc_file="$1" mode="$2"
  local start_line end_line head_bytes end_bytes cand="${tmp}/cmpl-rc-cand"
  start_line="$(grep -n '^# >>> token-usage completion >>>$' "${rc_file}" 2>/dev/null | head -n 1 | cut -d: -f1)"
  end_line="$(grep -n '^# <<< token-usage completion <<<$' "${rc_file}" 2>/dev/null | head -n 1 | cut -d: -f1)"
  if [ -z "${start_line}" ] || [ -z "${end_line}" ]; then
    echo "提示：completion 配置替换失败（旧块定位失败），${rc_file} 保持不变。"
    return 1
  fi
  head_bytes="$(head -n $(( start_line - 1 )) "${rc_file}" 2>/dev/null | wc -c | tr -d ' ')"
  end_bytes="$(head -n "${end_line}" "${rc_file}" 2>/dev/null | wc -c | tr -d ' ')"
  if ! {
    if [ "${head_bytes}" -gt 0 ]; then
      head -c "${head_bytes}" "${rc_file}"
      if [ "$(head -c "${head_bytes}" "${rc_file}" | tail -c 1 | od -An -tx1 | tr -d ' \n')" != "0a" ]; then
        printf '\n'
      fi
    fi
    cmpl_marker_block "${mode}"
    tail -c +$(( end_bytes + 1 )) "${rc_file}"
  } > "${cand}" 2>/dev/null; then
    echo "提示：completion 配置替换失败，${rc_file} 保持不变。"
    return 1
  fi
  cmpl_atomic_mv_rc "${rc_file}" "${cand}" || return 1
  echo "已把 completion 配置更新为 ${mode} 形态（${rc_file}，新终端生效）。"
  return 0
}

# cmpl_install_marker：把 canonical marker 写入 rc（安全写入协议 + mode 迁移
# 规则）。迁移表：同 mode 幂等跳过；standard → -i 升级替换（安全增强方向）；
# 本次用户显式选 -u（目标 u）时替换；现有 -u / -i 在用户本次无显式选择时保留
# 不推翻（-u 是用户显式的安全让步；-i 保留是因为静态检查无法覆盖全部 fpath，
# 曾因不安全目录写入的 -i 被自动放宽回 standard 后「误判 + 未覆盖目录」组合
# 会让新 shell 从原本可用的 compinit -i 退回确认/中止）。返回：0 = 写入或幂等
# 成功；2 = 非规范形态保留（原 rc 字节不变、提示手动处理——_token-usage 文件
# 与 rc 无关，由调用方照常覆盖写，但不宣称配置完成）；1 = 其余降级（提示已打印）。
cmpl_install_marker() {
  local rc_file="$1" target_mode="$2"
  local state existing
  state="$(cmpl_marker_state "${rc_file}")"
  case "${state}" in
    canonical-*)
      existing="${state#canonical-}"
      if [ "${existing}" = "${target_mode}" ]; then
        echo "提示：${rc_file} 已包含 completion 配置（${target_mode} 形态），无需重复写入。"
        return 0
      fi
      case "${existing}-${target_mode}" in
        standard-i|standard-u|i-u)
          cmpl_write_marker_replace "${rc_file}" "${target_mode}"
          return $?
          ;;
        u-standard|u-i)
          echo "检测到现有 compinit -u 配置（此前显式选择跳过安全检查），保持不变；如需切换请手动编辑 marker 块。"
          return 0
          ;;
        i-standard)
          echo "检测到现有 compinit -i 配置，保持不变（静态检查无法覆盖你的全部 fpath，故不自动放宽回标准 compinit）；如需切换请手动编辑 marker 块。"
          return 0
          ;;
      esac
      echo "提示：completion 配置写入被跳过（未定义的迁移组合 ${existing} → ${target_mode}）。"
      return 1
      ;;
    invalid)
      echo "检测到非规范 completion marker，请手动处理（删除旧块后重跑安装脚本，或按 token-usage completion zsh --help 手动配置）。"
      return 2
      ;;
    none)
      cmpl_rc_writable_check "${rc_file}" || return 1
      cmpl_write_marker_insert "${rc_file}" "${target_mode}"
      return $?
      ;;
  esac
  echo "提示：completion 配置写入被跳过（未知 marker 状态 ${state}）。"
  return 1
}

# cmpl_publish_file：补全文件安全发布（marker 写入动作的第一步，仅存在于判定
# 表的写入分支内部）。不直接 > 最终文件：以安全 umask（022 → 644）在目标目录
# 内写临时文件，校验生成成功（退出码 0 且非空）且临时文件无 ACL 威胁（继承 ACL
# 使 umask 022 的新建文件仍带 everyone allow write——644 承诺对 ACL 继承无效）
# 后 mv 原子替换；外层 umask 000 的新建与既有 666 文件的重跑经替换一并收紧为
# 644。返回 0 成功；1 失败（原文件不动，走指引①）；2 临时文件 ACL 命中（改走
#「目录 ACL 异常，chmod -a 后重跑」重跑导向提示，不叠加指引①——指引①的写入
# 步骤会引导向该目录手动写入，产出文件同样继承威胁 ACL）。
cmpl_publish_file() {
  local shell_kind="$1" dest="$2"
  local dir base tmpf acl_rc=0
  dir="$(dirname "${dest}")"
  base="$(basename "${dest}")"
  tmpf="${dir}/.${base}.token-usage.$$"
  rm -f "${tmpf}" 2>/dev/null || true
  if ! ( umask 022; "${BIN_PATH}" completion "${shell_kind}" > "${tmpf}" ) 2>/dev/null; then
    rm -f "${tmpf}" 2>/dev/null || true
    return 1
  fi
  if [ ! -s "${tmpf}" ]; then
    rm -f "${tmpf}" 2>/dev/null || true
    return 1
  fi
  cmpl_acl_threat "${tmpf}" file || acl_rc=$?
  if [ "${acl_rc}" -ne 0 ]; then
    rm -f "${tmpf}" 2>/dev/null || true
    [ "${acl_rc}" -eq 1 ] && return 2
    return 1
  fi
  if ! mv -f "${tmpf}" "${dest}" 2>/dev/null; then
    rm -f "${tmpf}" 2>/dev/null || true
    return 1
  fi
  return 0
}

# 指引①（手动配置指引，按需挂载：判定表与各降级条款的「跳过 + 指引①」共用）。
# zsh 场景必须给 fpath 挂载行（降级场景无 marker，~/.zsh/completions 不在
# fpath——缺挂载则文件写入后不被扫描、静默无效；compinit 行必须写入 rc，
# 一次性执行对后续会话无效）；fish 的 XDG 落点天然被 fish 搜索、无需挂载行。
cmpl_print_manual_guide() {
  local shell_kind="$1" landing="$2"
  echo '稍后可重跑官方安装脚本自动配置，或手动配置：'
  if [ "${shell_kind}" = "zsh" ]; then
    echo '  将下行加入 rc（如 ~/.zshrc）：fpath=("$HOME/.zsh/completions" $fpath); autoload -U compinit; compinit'
    echo '  在终端执行：token-usage completion zsh > ~/.zsh/completions/_token-usage'
  else
    # 落点路径经 shell 安全拼装（合法的绝对 XDG_CONFIG_HOME 可含空格/单引号，
    # 未引用路径经 shell 分词拆参会写到错误位置；fish 与 zsh 对 '\'' 转义结果
    # 一致，命令可直接复制执行）。
    echo "  在终端执行：token-usage completion fish > $(cmpl_sh_quote "${landing}/token-usage.fish")"
  fi
  echo "  详见 token-usage completion ${shell_kind} --help"
}

# cmpl_tty_available：交互终端可用性探测（curl | bash 管道形态 stdin 被管道
# 占用，交互须走 /dev/tty；无 /dev/tty 的环境自动降级非交互路径，不卡死）。
cmpl_tty_available() {
  if exec 3<>/dev/tty 2>/dev/null; then
    exec 3>&- 2>/dev/null || true
    return 0
  fi
  return 1
}

# cmpl_read_choice：从 /dev/tty 读取选项。回车（空输入/EOF）默认选项 1；非法
# 输入重问一次，再非法按最后一项（跳过方向）。结果写入 cmpl_choice；/dev/tty
# 打不开返回 2（由调用方按所在交互场景分派）。
cmpl_read_choice() {
  local n_options="$1" choice="" attempt
  exec 3<>/dev/tty 2>/dev/null || return 2
  for attempt in 1 2; do
    if ! IFS= read -r choice <&3; then
      choice=""
    fi
    case "${choice}" in
      "") cmpl_choice=1; exec 3>&- 2>/dev/null || true; return 0 ;;
      *[!0-9]*) ;;
      *)
        if [ "${choice}" -ge 1 ] && [ "${choice}" -le "${n_options}" ]; then
          cmpl_choice="${choice}"
          exec 3>&- 2>/dev/null || true
          return 0
        fi
        ;;
    esac
    if [ "${attempt}" -eq 1 ]; then
      echo "无效输入，请输入 1-${n_options} 的数字（直接回车 = 1）："
    fi
  done
  exec 3>&- 2>/dev/null || true
  cmpl_choice="${n_options}"
  return 0
}

# cmpl_print_chmod_cmds：清单内各目录的 chmod go-w 指引命令（路径按 shell 安全
# 拼装规则单引号包裹，可整行复制执行）。
cmpl_print_chmod_cmds() {
  local dir
  while IFS= read -r dir; do
    [ -n "${dir}" ] || continue
    echo "  chmod go-w $(cmpl_sh_quote "${dir}")"
  done <<CMPL_CHMOD_EOF
$1
CMPL_CHMOD_EOF
}

# cmpl_chmod_repair_set：对清单内目录逐个 chmod go-w（安全收紧方向，Homebrew
# 官方建议做法；目录归当前用户所有、无需 sudo——非属主目录上 chmod 必失败，
# 触发原子性中止）。全部成功才继续（任一失败即中止整链：不写 marker、不写
# 文件——残留不安全目录时标准 compinit 每次启动仍会弹确认或中止，「部分成功
# 仍写 marker」不成立）；返回 1 时已打印失败目录与后续建议。
cmpl_chmod_repair_set() {
  local list="$1" dir failed=""
  while IFS= read -r dir; do
    [ -n "${dir}" ] || continue
    if ! chmod go-w "${dir}" 2>/dev/null; then
      failed="${dir}"
      break
    fi
  done <<CMPL_REPAIR_EOF
${list}
CMPL_REPAIR_EOF
  if [ -n "${failed}" ]; then
    echo "提示：修复目录权限失败（${failed}），本次未写入任何补全配置；请手动修复后重跑安装脚本（root 属主目录需管理员），或按 token-usage completion zsh --help 手动配置。"
    return 1
  fi
  return 0
}

# cmpl_rc_preflight：rc 形态预检（在任何读取与发布动作之前；先于 marker 检测
# 与补全发布执行）。仅「不存在」或「单链接普通文件」通过；其余形态拒绝并置
# cmpl_rc_rejected=1（提示在此打印）。FIFO（命名管道）读取会一直等数据、设备
# 文件读取可能是无终止流——后续 marker 检测的 grep/sed 一旦读它们就持续阻塞；
# 目录 / 套接字等病态形态同拒；有效 symlink 与硬链接保守不改写（rename 会断开
# 链接 / 分离两端 inode）。判定：仅用文件类型原语（-p/-c/-b/-d/-L/-f/stat），
# 绝不读取内容。
cmpl_rc_preflight() {
  local rc_file="$1" links
  cmpl_rc_rejected=0
  if [ -p "${rc_file}" ] || [ -c "${rc_file}" ] || [ -b "${rc_file}" ]; then
    echo "提示：无法写入 ${rc_file}（非常规配置文件形态），completion marker 未写入。"
    cmpl_rc_rejected=1
    return 0
  fi
  if [ -d "${rc_file}" ]; then
    echo "提示：无法写入 ${rc_file}（目标为目录），completion marker 未写入。"
    cmpl_rc_rejected=1
    return 0
  fi
  if [ -L "${rc_file}" ]; then
    if [ ! -e "${rc_file}" ]; then
      echo "提示：无法写入 ${rc_file}（断链符号链接），completion marker 未写入。"
    else
      echo "${rc_file} 为符号链接，安装脚本保守不改写；请手动编辑链接目标文件——无 marker 块则按 token-usage completion zsh --help 手动配置补全，已有旧块则手动编辑迁移；或解除链接后重跑安装脚本。"
    fi
    cmpl_rc_rejected=1
    return 0
  fi
  if [ -e "${rc_file}" ] && [ ! -f "${rc_file}" ]; then
    echo "提示：无法写入 ${rc_file}（非常规配置文件形态），completion marker 未写入。"
    cmpl_rc_rejected=1
    return 0
  fi
  if [ -f "${rc_file}" ]; then
    links="$(stat -f %l "${rc_file}" 2>/dev/null)" || links=""
    if [ -n "${links}" ] && [ "${links}" -gt 1 ]; then
      echo "${rc_file} 与其他文件为硬链接（链接数 ${links}），安装脚本保守不改写；请手动编辑其中一个文件——无 marker 块则按 token-usage completion zsh --help 手动配置补全，已有旧块则手动编辑迁移；或解除链接后重跑安装脚本。"
      cmpl_rc_rejected=1
      return 0
    fi
  fi
  return 0
}

# cmpl_zsh_write_all：判定表写入分支的统一落盘动作——先安全发布 _token-usage
#（marker 写入动作的第一步；发布失败则不写 marker——否则补全文件以外层 umask
# 权限落地、经不审计文件权限的 compinit -u 加载，权限缺口逃过 compaudit 审计），
# 再写 marker（canonical 前置 + 迁移规则）。返回 0 = 成功；1 = 已打印降级提示
#（marker 与文件均未新增/替换，原文件字节与权限不动）。
cmpl_zsh_write_all() {
  local mode="$1" pub_rc=0
  cmpl_publish_file zsh "${cmpl_zsh_landing_dir}/_token-usage" || pub_rc=$?
  if [ "${pub_rc}" -ne 0 ]; then
    if [ "${pub_rc}" -eq 2 ]; then
      echo "检测到补全目录的 ACL 异常（发布文件继承了威胁 ACL 条目），本次未写入补全文件；请执行 chmod -a 移除该目录的威胁 ACL 条目后重跑安装脚本。"
    else
      echo "提示：补全文件发布失败，本次未写入 marker 与补全文件。"
      cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
    fi
    return 1
  fi
  if [ "${cmpl_rc_rejected:-0}" -eq 1 ]; then
    # rc 形态预检已拒绝（提示已在预检打印）：补全文件照常发布（文件与 rc 无关），
    # 但不读 rc、不写 marker、不宣称配置完成。
    return 0
  fi
  local mk_rc=0
  cmpl_install_marker "${cmpl_target_rc}" "${mode}" || mk_rc=$?
  if [ "${mk_rc}" -eq 0 ]; then
    echo "completion 配置完成：${cmpl_zsh_landing_dir}/_token-usage 与 rc marker 均在新终端加载生效（已开着的 shell 会话不受影响）。"
    return 0
  fi
  if [ "${mk_rc}" -eq 2 ]; then
    # 非规范形态：补全文件已发布、rc 保留原字节（提示已打印），不宣称完成。
    return 0
  fi
  return 1
}

# setup_zsh_completion：zsh 补全自动配置主流程（判定表按行序短路；探测全程
# 不执行任何用户启动文件）。
setup_zsh_completion() {
  cmpl_zsh_landing_dir="${HOME}/.zsh/completions"
  local entry mode repair_list has_non_landing=0

  # 完整替换链信任边界（第一步，先于 mkdir 与全部探测；命中即短路一切后续
  # 补全动作：任何开关不写 marker、不发布补全文件、不 chmod、不创建落点目录）。
  cmpl_chain_boundary "${HOME}" home || return 0

  # P0 ZDOTDIR 三态解析。
  if ! cmpl_resolve_zdotdir; then
    echo "检测到无法静态解析的 ZDOTDIR，补全自动配置已跳过。"
    cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
    return 0
  fi
  cmpl_target_rc="${cmpl_startup_root}/.zshrc"

  # ZDOTDIR 实际启动链信任边界：marker 写入并被 zsh 启动加载的是该 rc 根，
  # 其祖先可被他人替换（rename 整个 ZDOTDIR 连同 .zshrc）时，发布补全与写入
  # marker 都发生在不可信链上——只审 $HOME 不够。与 $HOME 相同时已由上方边界
  # 覆盖；缺失根降级（见 cmpl_chain_boundary 的 zdot 分支），绝不自动创建。
  if [ "${cmpl_startup_root}" != "${HOME}" ]; then
    cmpl_chain_boundary "${cmpl_startup_root}" zdot || return 0
  fi

  # rc 形态预检（在任何读取与发布动作之前）：仅「不存在」或「单链接普通文件」
  # 可继续。FIFO / 设备文件会被后续 grep/sed 读取阻塞（读取等数据、无终止流），
  # 目录与套接字等病态形态同样在此一次性拒绝；symlink（含断链）与硬链接走
  # 保守降级定稿指引。拒绝时置 cmpl_rc_rejected=1：判定表照常走环境检查，
  # 写入分支仅发布补全文件（文件与 rc 无关）、不读 rc、不写 marker。
  cmpl_rc_preflight "${cmpl_target_rc}"

  # 落点目录就绪（安全 umask 创建，新建目录恒 755、属主恒当前用户，不依赖
  # 外层 umask；降级场景下已创建的空目录保留无害）。
  if ! ( umask 022; mkdir -p "${cmpl_zsh_landing_dir}" ) 2>/dev/null; then
    echo "提示：无法创建补全目录 ${cmpl_zsh_landing_dir}，补全自动配置已跳过。"
    cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
    return 0
  fi

  # 落点链 ACL 复查（compaudit 对 ACL 完全盲区；无论新建既有——新建目录可
  # 继承父目录 ACL。命中 → 整体降级：任何开关不写 marker、不发布补全文件，
  # 不进修复/交互体系——chmod go-w 对 ACL 无效，自动改 ACL 的误伤面（继承链、
  # deny 保护条目、用户有意授权）大于 chmod go-w 的纯收紧）。
  for entry in "${HOME}/.zsh" "${cmpl_zsh_landing_dir}"; do
    local acl_rc=0
    cmpl_acl_threat "${entry}" dir || acl_rc=$?
    if [ "${acl_rc}" -eq 1 ]; then
      cmpl_print_acl_reject "${entry}"
      return 0
    elif [ "${acl_rc}" -ne 0 ]; then
      echo "检测到 ${entry} 存在无法判定安全的组件形态（symlink / 非目录实体 / 无法访问），补全自动配置已跳过；请人工核查该路径后重跑安装脚本。"
      return 0
    fi
  done

  # P1 compinit 静态检测（可能已启用）。
  local compinit_maybe=0
  if cmpl_detect_compinit; then
    compinit_maybe=1
  fi

  # P2 基线 + brew 静态确认 + P3 权威检查。
  cmpl_fpath_baseline
  cmpl_brew_candidates
  cmpl_run_compaudit

  # 清单成员分类：落点链条目 vs 其他系统目录条目。
  repair_list=""
  while IFS= read -r entry; do
    [ -n "${entry}" ] || continue
    if [ "${entry}" = "${HOME}/.zsh" ] || [ "${entry}" = "${cmpl_zsh_landing_dir}" ]; then
      repair_list="${repair_list}${entry}
"
    else
      has_non_landing=1
    fi
  done <<CMPL_CLASSIFY_EOF
${cmpl_p3_list}
CMPL_CLASSIFY_EOF

  # ---- 判定表（行序短路）----
  if [ "${cmpl_p3_state}" = "unknown" ]; then
    # 行 0：无法证明安全即不写 marker / 文件 / autoload（yes 亦同）。
    echo "无法完成补全目录安全检查，补全自动配置已跳过。"
    cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
    return 0
  fi

  if [ "${compinit_maybe}" -eq 1 ]; then
    if [ -z "${repair_list}" ]; then
      # 行 3：clean 或 insecure 且清单不含落点链 → 自动完成（if 分支自适应，
      # 不引入新的 compinit 调用）。insecure 子形态（仅系统目录）用 -i 变体：
      # grep 误命中而实际未初始化时 else 分支将运行 compinit，系统目录仍不
      # 安全会弹确认或中止——-i 静默忽略不安全目录、不信任也不打扰，安全
      # 落点目录下补全照常注册。
      mode="standard"
      [ "${cmpl_p3_state}" = "insecure" ] && mode="i"
      cmpl_zsh_write_all "${mode}"
      if [ "${has_non_landing}" -eq 1 ]; then
        # 其他既有系统目录仅提示级：用户环境既有状态、非本次安装引入。
        echo "检测到 group/other 可写的补全目录（$(cmpl_list_inline "${cmpl_p3_list}")），zsh 启动时的 compinit 安全确认由你自行处理（修复 / -u / 每次 y）。"
      fi
      # rc 形态被预检拒绝时 marker 未写入，「按该判定写入」的收尾措辞不成立。
      if [ "${cmpl_rc_rejected:-0}" -ne 1 ]; then
        echo "静态检测到补全系统可能已启用，以上 completion 配置按该判定写入。"
      fi
      return 0
    fi
    # 行 3b/3c/3d：insecure 且清单含落点链——落点链安全是写 marker 的硬前置
    #（direct-autoload marker 从不安全目录加载代码会绕过 compinit 审计）。
    if [ "${completion_auto}" -eq 1 ]; then
      # 行 3c：yes 原子修复（repair_set = 清单 ∩ 落点链）。
      if cmpl_chmod_repair_set "${repair_list}"; then
        mode="standard"
        [ "${has_non_landing}" -eq 1 ] && mode="i"
        if cmpl_zsh_write_all "${mode}"; then
          echo "已修复目录权限：$(cmpl_list_inline "${repair_list}")。"
          [ "${has_non_landing}" -eq 1 ] && echo "检测到 group/other 可写的补全目录（$(cmpl_list_inline "${cmpl_p3_list}")），zsh 启动时的 compinit 安全确认由你自行处理（修复 / -u / 每次 y）。"
        fi
      fi
      return 0
    fi
    if cmpl_tty_available; then
      # 行 3b：交互两选项（回车默认 1）。
      echo "检测到 zsh 补全系统可能已启用，但补全目录或其父目录 group/other 可写："
      printf '%s\n' "${repair_list}" | sed '/^$/d; s/^/  /'
      cat <<'CMPL_ASK2'
补全脚本将从此目录自动加载，不安全目录会使每次启动加载未审计的代码。请选择：
  1) 修复目录权限（chmod go-w）后安装补全
  2) 跳过补全（稍后可手动 chmod 后重跑安装脚本，或按 token-usage completion zsh --help 手动配置）
请选择 [1/2]（直接回车 = 1）：
CMPL_ASK2
      if cmpl_read_choice 2; then
        if [ "${cmpl_choice}" -eq 1 ]; then
          if cmpl_chmod_repair_set "${repair_list}"; then
            mode="standard"
            [ "${has_non_landing}" -eq 1 ] && mode="i"
            if cmpl_zsh_write_all "${mode}"; then
              echo "已修复目录权限：$(cmpl_list_inline "${repair_list}")。"
              [ "${has_non_landing}" -eq 1 ] && echo "检测到 group/other 可写的补全目录（$(cmpl_list_inline "${cmpl_p3_list}")），zsh 启动时的 compinit 安全确认由你自行处理（修复 / -u / 每次 y）。"
            fi
          fi
        else
          cmpl_print_chmod_cmds "${repair_list}"
          cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
        fi
      fi
      return 0
    fi
    # 行 3d：无交互终端 → 跳过 + 指引①（附 chmod 命令）。
    echo "未检测到交互终端，补全自动配置已跳过。"
    cmpl_print_chmod_cmds "${repair_list}"
    cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
    return 0
  fi

  # ---- 未启用场景 ----
  if [ "${completion_auto}" -eq 1 ]; then
    # 行 5：yes 修复全部 P3 清单后标准 marker（else 分支将运行标准 compinit，
    # 须全修以兑现「不再弹确认」；任一失败整体中止）。
    if cmpl_chmod_repair_set "${cmpl_p3_list}"; then
      if cmpl_zsh_write_all "standard"; then
        [ -n "${cmpl_p3_list}" ] && echo "已修复目录权限：$(cmpl_list_inline "${cmpl_p3_list}")。"
      fi
    fi
    return 0
  fi
  if cmpl_tty_available; then
    # 行 6：交互三选项（回车默认 1；文案含 P3 清单，清单为空时选项 1 用
    #「未检测到 group/other 可写目录」变体）。
    echo "检测到 zsh 补全系统（compinit）未启用，安装 Tab 补全需要初始化 compinit。请选择："
    if [ -n "${cmpl_p3_list}" ]; then
      cat <<'CMPL_ASK3_HEAD'
  1) 修复以下 group/other 可写目录的权限（chmod go-w，Homebrew 官方建议做法，
     目录归当前用户所有、无需 sudo）并启用标准 compinit：
CMPL_ASK3_HEAD
      printf '%s\n' "${cmpl_p3_list}" | sed '/^$/d; s/^/     /'
    else
      echo "  1) 未检测到 group/other 可写目录，直接启用标准 compinit："
    fi
    cat <<'CMPL_ASK3_TAIL'
  2) 不修改目录权限，启用 compinit -u（跳过补全目录安全检查，之后不再弹出确认）
  3) 跳过补全，稍后按 token-usage completion zsh --help 手动配置
请选择 [1/2/3]（直接回车 = 1）：
CMPL_ASK3_TAIL
    if cmpl_read_choice 3; then
      case "${cmpl_choice}" in
        1)
          if cmpl_chmod_repair_set "${cmpl_p3_list}"; then
            if cmpl_zsh_write_all "standard"; then
              [ -n "${cmpl_p3_list}" ] && echo "已修复目录权限：$(cmpl_list_inline "${cmpl_p3_list}")。"
            fi
          else
            echo "可改选跳过（重跑安装脚本后选 2 或 3），或手动修复后重跑。"
          fi
          ;;
        2) cmpl_zsh_write_all "u" ;;
        3) cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}" ;;
      esac
    fi
    return 0
  fi
  # 行 4：无交互终端 → 自动按选项 3 执行（跳过 + 指引①）。
  echo "未检测到交互终端，补全自动配置已跳过。"
  cmpl_print_manual_guide zsh "${cmpl_zsh_landing_dir}"
  return 0
}

# cmpl_fish_review_verdict：fish 三级链复查的单级判定——group/other 可写
#（无 sticky 豁免：内层目录在用户可自行收紧范围，与 compaudit「g+w / o+w
# 即报、sticky 不进入判定」语义一致）、属主 ∉ {当前用户, root}（第三方属主
# 目录对属主恒可写）、ACL 威胁。输出 ok / owner / acl / writable / error。
cmpl_fish_review_verdict() {
  local d="$1" perms rc=0
  if ! cmpl_dir_owner_trusted "${d}"; then
    echo owner
    return 0
  fi
  cmpl_acl_threat "${d}" dir || rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo acl
    return 0
  fi
  perms="$(stat -f %Lp "${d}" 2>/dev/null)" || { echo error; return 0; }
  if [ $(( 8#${perms} & 8#022 )) -ne 0 ]; then
    echo writable
    return 0
  fi
  echo ok
}

# setup_fish_completion：fish 补全自动配置（零前置全自动：fish 原生自动加载
# 完成目录，写入即生效；默认 ask 与 yes 同动作——无决策分歧）。
setup_fish_completion() {
  # 落点解析：XDG_CONFIG_HOME 非空且为绝对路径 → <该值>/fish/completions；
  # 未设置/空/相对路径 → $HOME/.config/fish/completions（XDG 规范规定无效值按
  # 未设置处理，回退与 fish 实际搜索路径一致——硬编码默认会写到 fish 不搜索
  # 的路径并误报配置成功）。
  local cfg_root="${HOME}/.config" xdg_note=""
  if [ -n "${XDG_CONFIG_HOME:-}" ]; then
    case "${XDG_CONFIG_HOME}" in
      /*) cfg_root="${XDG_CONFIG_HOME}"; xdg_note="，或改用默认落点（取消 XDG_CONFIG_HOME 后重跑）" ;;
    esac
  fi
  local landing="${cfg_root}/fish/completions"
  local d verdict pub_rc=0

  # 审计（先于 mkdir）：完整替换链信任边界，链顶 = <cfg>；<cfg> 可写即可
  # rename 整个 fish 子目录植入恶意目录树，须查至配置根及以上。
  cmpl_chain_boundary "${cfg_root}" cfg || return 0

  # 创建（仅 ENOENT 待建层可进；链全存在时 mkdir 幂等空跑）：安全 umask 创建，
  # 新建组件属主恒为当前用户、恒 755，不依赖外层 umask。
  if ! ( umask 022; mkdir -p "${landing}" ) 2>/dev/null; then
    echo "提示：无法创建补全目录 ${landing}，补全自动配置已跳过。"
    return 0
  fi

  # 复查（创建后、发布前对三级终判：新建组件的属主与权限由创建形态保证，
  # 复查防的是创建过程中的环境变异——含 ACL 继承竞态）。命中 → 不自动
  # chmod、不发布补全文件（默认与 yes 同：fish 的自动化前提是零安全权衡）。
  for d in "${cfg_root}" "${cfg_root}/fish" "${landing}"; do
    verdict="$(cmpl_fish_review_verdict "${d}")"
    case "${verdict}" in
      owner)
        echo "检测到 ${d} 属主为其他用户，补全自动配置已跳过；请人工核查该目录属主与用途（必要时以管理员修复属主${xdg_note}）后重跑。"
        return 0
        ;;
      acl)
        cmpl_print_acl_reject "${d}"
        return 0
        ;;
      writable)
        if [ "$(stat -f %u "${d}" 2>/dev/null)" = "${cmpl_uid}" ]; then
          echo "检测到 ${d} 对组/其他用户可写，补全自动配置已跳过；请手动执行 chmod go-w $(cmpl_sh_quote "${d}") 收紧后重跑${xdg_note}。"
        else
          echo "检测到 ${d} 属主为其他用户，补全自动配置已跳过；请人工核查该目录属主与用途（必要时以管理员修复属主${xdg_note}）后重跑。"
        fi
        return 0
        ;;
      error)
        echo "检测到 ${d} 存在无法判定安全的组件形态（symlink / 非目录实体 / 无法访问），补全自动配置已跳过；请人工核查该路径后重跑安装脚本。"
        return 0
        ;;
    esac
  done

  # 安全发布（同 zsh：安全 umask 临时文件 + 校验 + mv 原子替换，外层 umask 000
  # 与既有 666 文件一并收紧为 644）。
  cmpl_publish_file fish "${landing}/token-usage.fish" || pub_rc=$?
  if [ "${pub_rc}" -eq 0 ]; then
    echo "completion 配置完成：补全文件已写入 ${landing}/token-usage.fish，新开的 fish 会话自动生效。"
    return 0
  fi
  if [ "${pub_rc}" -eq 2 ]; then
    echo "检测到补全目录的 ACL 异常（发布文件继承了威胁 ACL 条目），本次未写入补全文件；请执行 chmod -a 移除该目录的威胁 ACL 条目后重跑安装脚本。"
    return 0
  fi
  cmpl_print_manual_guide fish "${landing}"
}

# 补全段分派：仅默认布局执行；按登录 shell 进入对应分支（其余/未知 shell 不
# 打印补全相关内容，避免噪音）。
if [ "${is_default_layout}" -eq 1 ]; then
  cmpl_shell_name=""
  if [ -n "${SHELL:-}" ]; then
    cmpl_shell_name="$(basename "${SHELL}")"
  fi
  case "${cmpl_shell_name}" in
    zsh) setup_zsh_completion ;;
    fish) setup_fish_completion ;;
    bash)
      # bash 不自动安装：瓶颈是 bash-completion 包缺失，交互无助，仅指引。
      echo "提示：bash 补全依赖 bash-completion 包（macOS 系统 bash 3.2 不满足，需 brew install bash-completion@2 与 bash 4+），详见 token-usage completion bash --help。"
      ;;
    *) ;;
  esac
fi

# ---------- 以下为安装后的 PATH 配置与旧布局迁移处理 ----------

# 判断 PATH 是否已包含指定目录：按冒号拆分后逐条目去尾斜杠再字面比较。
path_contains() {
  local target entry
  local globbing_was_off
  target="$(normalize_dir "$1")"
  # 临时关闭 globbing 防 PATH 条目含 * 等元字符被展开；退出时恢复进入前状态，
  # 不无条件开启（调用方可能本就处于 set -f 环境）。
  globbing_was_off=0
  case "$-" in *f*) globbing_was_off=1 ;; esac
  set -f
  local IFS=:
  for entry in ${PATH}; do
    if [ "$(normalize_dir "${entry}")" = "${target}" ]; then
      [ "${globbing_was_off}" -eq 1 ] || set +f
      return 0
    fi
  done
  [ "${globbing_was_off}" -eq 1 ] || set +f
  return 1
}

# 向登录 shell 配置文件写入 PATH marker 块（已含则跳过，保证幂等重跑不重复）；
# 读写失败返回非零，由调用方降级为人工指引，不影响已完成的安装。
write_rc_marker() {
  local rc_file="$1"
  # rc 为 FIFO（命名管道）时不能当常规配置文件用：读取等数据、无读者的追加写
  # 都会一直阻塞；rc 为设备文件（字符/块设备）时读取可能是无终止流（如 zero
  # 设备）、写入无实际效果却仍报成功，提示会误导。两者都按写入失败同路降级
  # 返回，由调用方给人工指引。
  if [ -p "${rc_file}" ] || [ -c "${rc_file}" ] || [ -b "${rc_file}" ]; then
    return 1
  fi
  if [ ! -e "${rc_file}" ]; then
    # rc 为断链 symlink（链接存在、目标不存在，-e 跟随链接误判为不存在）时不
    # 走创建路径：`>>` 会经链接在目标位置创建文件并返回成功，用户预期放仓库
    # 文件的路径被静默占用，与写入失败同路降级返回，由调用方给人工指引。
    # 活 symlink（指向已存在文件）不走此分支，仍进入下方既有内容追加路径。
    if [ -L "${rc_file}" ]; then
      return 1
    fi
    # 配置文件不存在：创建并写入。追加重定向失败（只读 rc、rc 为目录等）时
    # bash 的原生错误发生在命令自身 2>/dev/null 生效之前，包一层子 shell 再
    # 吞掉 stderr，保证降级路径只见人工指引、不泄漏裸错误行。
    if ( printf '# >>> token-usage path >>>\nexport PATH="$HOME/.token-usage/bin:$PATH"\n# <<< token-usage path <<<\n' >> "${rc_file}" ) 2>/dev/null; then
      echo "已创建 ${rc_file} 并写入 PATH 配置（新终端生效）。"
      return 0
    fi
    return 1
  fi
  # 区分 grep 退出码：1=未含 marker（正常继续追加）；>=2=读取失败（如权限不可读），
  # 无法确认幂等性时按 rc 写入失败降级处理，避免不可读但可写的病态组合下重跑重复追加。
  local grep_rc=0
  grep -qF '# >>> token-usage path >>>' "${rc_file}" 2>/dev/null || grep_rc=$?
  if [ "${grep_rc}" -eq 0 ]; then
    echo "提示：${rc_file} 已包含 PATH 配置，无需重复写入。"
    return 0
  fi
  if [ "${grep_rc}" -ge 2 ]; then
    return 1
  fi
  # 既有内容末行无换行时先补一个换行，避免 marker 首行与用户末行粘连；
  # marker 写入内容本身不带前导换行，空文件追加时首行即 marker 首行、无多余空行。
  # 末字节按十六进制字节值判定（0a 为换行）而非按替换结果是否为空：命令替换会
  # 丢弃 NUL 字节，末字节恰为 NUL 的 rc 会被误判为已换行而漏补。
  if [ -s "${rc_file}" ] && [ "$(tail -c 1 "${rc_file}" 2>/dev/null | od -An -tx1 | tr -d ' \n')" != "0a" ]; then
    ( printf '\n' >> "${rc_file}" ) 2>/dev/null || return 1
  fi
  if ( printf '# >>> token-usage path >>>\nexport PATH="$HOME/.token-usage/bin:$PATH"\n# <<< token-usage path <<<\n' >> "${rc_file}" ) 2>/dev/null; then
    echo "已把 PATH 配置追加到 ${rc_file}（新终端生效）。"
    return 0
  fi
  return 1
}

# PATH 配置：默认布局写登录 shell 配置文件；覆盖为其他目录时不写文件、仅人工指引。
manual_path_hint=0   # 1 = 登录 shell 无法自动配置（空 SHELL / 非 zsh、bash），省略 export 粘贴提示行
path_persisted=0     # 1 = PATH 配置已成功写入登录 shell 配置文件（新终端 PATH 已配置）
shell_is_bash=0
if [ "${is_default_layout}" -eq 1 ]; then
  login_shell="${SHELL:-}"
  target_rc=""
  if [ -z "${login_shell}" ]; then
    # SHELL 未设置时无法判定登录 shell，只给不针对特定 shell 的通用指引。
    echo '提示：未检测到登录 shell（SHELL 未设置），请手动将 $HOME/.token-usage/bin 加入所用 shell 的 PATH。'
    manual_path_hint=1
  else
    shell_name="$(basename "${login_shell}")"
    case "${shell_name}" in
      zsh)
        target_rc="$HOME/.zshrc"
        ;;
      bash)
        shell_is_bash=1
        # bash 登录 shell 只读取 ~/.bash_profile、~/.bash_login、~/.profile 中
        # 第一个存在的文件；三者皆无时才创建 ~/.bash_profile（避免遮蔽既有配置）。
        if [ -e "$HOME/.bash_profile" ]; then
          target_rc="$HOME/.bash_profile"
        elif [ -e "$HOME/.bash_login" ]; then
          target_rc="$HOME/.bash_login"
        elif [ -e "$HOME/.profile" ]; then
          target_rc="$HOME/.profile"
        else
          target_rc="$HOME/.bash_profile"
        fi
        ;;
      *)
        echo "提示：暂不支持自动配置 ${shell_name}，请手动将 \$HOME/.token-usage/bin 加入 PATH。"
        manual_path_hint=1
        ;;
    esac
  fi
  if [ -n "${target_rc}" ]; then
    if write_rc_marker "${target_rc}"; then
      path_persisted=1
    else
      # 写入失败只降级为持久化配置的人工指引；当前会话的 export 粘贴提示照常输出
      # （此分支 shell 已知为 zsh/bash，export 行语法有效）。
      echo "提示：无法写入 ${target_rc}，请手动将 \$HOME/.token-usage/bin 加入 PATH。"
    fi
  fi
else
  echo "提示：INSTALL_DIR 已覆盖为 ${INSTALL_DIR}，请手动将该目录加入 PATH。"
fi

# 旧布局副本检测与清理（仅默认布局；先判类型再判所在目录可写性，
# 符号链接只删链接本身；任何失败只提示、不影响安装）。
need_hash_hint=0
if [ "${is_default_layout}" -eq 1 ]; then
  if [ -e "${LEGACY_BIN}" ] || [ -L "${LEGACY_BIN}" ]; then
    if [ -L "${LEGACY_BIN}" ] || [ -f "${LEGACY_BIN}" ]; then
      if [ -w "$(dirname "${LEGACY_BIN}")" ]; then
        if rm -f "${LEGACY_BIN}" 2>/dev/null; then
          echo "已删除旧副本：\"${LEGACY_BIN}\""
          need_hash_hint=1
        else
          echo "提示：删除旧副本 \"${LEGACY_BIN}\" 失败，请手动删除后再使用。"
          need_hash_hint=1
        fi
      else
        echo "检测到旧布局副本 \"${LEGACY_BIN}\"（所在目录不可写），可执行 sudo rm \"${LEGACY_BIN}\" 移除。"
        need_hash_hint=1
      fi
    else
      echo "提示：检测到 \"${LEGACY_BIN}\" 为目录或其他非普通文件/符号链接类型，请自行确认处理。"
    fi
  fi
fi

# 当前会话提示：脚本内 export 无法影响父 shell，PATH 未含安装目录时提示用户自行执行。
if [ "${is_default_layout}" -eq 1 ] && [ "${manual_path_hint}" -eq 0 ]; then
  if ! path_contains "${normalized_install_dir}"; then
    echo "提示：在当前终端执行以下命令使 PATH 立即生效："
    echo 'export PATH="$HOME/.token-usage/bin:$PATH"'
  fi
fi

# 删除过旧副本或给出清理指引时，提示刷新命令路径缓存（bash/zsh 会缓存命令路径）。
if [ "${need_hash_hint}" -eq 1 ]; then
  echo "提示：若当前终端此前解析过旧命令路径，请执行 hash -r 清除缓存（适用于 bash/zsh）。"
fi

# 守护进程检测：经新装二进制查询运行状态；查询失败一律视为未运行（仅默认布局，
# 覆盖安装目录时命令指向由用户自行管理，不做运行态提示）。
if [ "${is_default_layout}" -eq 1 ]; then
  daemon_output="$("${BIN_PATH}" status 2>/dev/null || true)"
  if printf '%s\n' "${daemon_output}" | grep -qF '守护进程运行中'; then
    echo "提示：检测到守护进程正在运行（将保持旧版本直到重启），请执行 token-usage restart 切换到新版本（若上方打印了 export 命令、hash -r 提示或旧副本清理指引，请先完成后再执行）。"
  fi

  # 指向确认依赖新终端 PATH 已配置：仅在 rc 写入成功且 shell 已识别（zsh/bash）时提示，
  # rc 写入失败、未知 shell 或空 SHELL 场景下新终端 PATH 尚未配置，不引导用户依赖新终端。
  if [ "${manual_path_hint}" -eq 0 ] && [ "${path_persisted}" -eq 1 ]; then
    echo "请在新的终端窗口执行 command -v token-usage，输出应为 ${normalized_install_dir}/token-usage（确认命令解析到新位置）。"
  fi
fi

# 自启定义迁移提示：旧布局的登录自启定义指向旧二进制路径，需重建（默认布局下无条件提示）。
if [ "${is_default_layout}" -eq 1 ]; then
  echo "若此前开启过开机自启，请在新终端执行 token-usage config set daemon.autostart true 重建定义（若配置文件不存在，先执行 token-usage config init）。"
fi

# bash 附注与 rc 写入同口径：仅默认布局下才发生登录文件写入，覆盖安装目录时无 rc
# 动作，附注指向的补行对象不存在，不打印。
if [ "${is_default_layout}" -eq 1 ] && [ "${shell_is_bash}" -eq 1 ]; then
  echo "注：非登录交互 shell 环境（部分 IDE 集成终端读取 ~/.bashrc 而非登录文件）请自行补一行。"
fi

echo "运行 token-usage --help 查看可用命令。"
