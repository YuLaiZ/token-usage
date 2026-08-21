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

# 布局判定：是否为默认安装布局（去尾斜杠后字面比较，尾斜杠变体视同默认布局）。
normalized_install_dir="$(normalize_dir "${INSTALL_DIR}")"
normalized_default_dir="$(normalize_dir "${DEFAULT_INSTALL_DIR}")"
if [ "${normalized_install_dir}" = "${normalized_default_dir}" ]; then
  is_default_layout=1
else
  is_default_layout=0
fi

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
