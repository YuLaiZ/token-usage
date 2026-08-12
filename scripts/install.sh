#!/usr/bin/env bash
# token-usage 官方安装脚本（macOS）。
#
# 自动检测 CPU 架构、下载官方 Release 二进制（默认取最新发布，含预发布）、
# 用官方 SHA256SUMS 校验、安装到 /usr/local/bin 并验证版本。
# 用法（README 一句话安装）：
#   curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
# 可选环境变量：
#   TAG=v0.1.0-rc.1    指定版本（默认自动取最新发布）
#   INSTALL_DIR=/path  安装目录（默认 /usr/local/bin；测试用）
set -euo pipefail

REPO="YuLaiZ/token-usage"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
TAG="${TAG:-}"

# 平台检测：仅官方资产覆盖的 macOS（Apple Silicon / Intel）。
os="$(uname -s)"
arch="$(uname -m)"
case "${os}-${arch}" in
  Darwin-arm64)      asset="token-usage-darwin-arm64" ;;
  Darwin-x86_64|Darwin-amd64) asset="token-usage-darwin-amd64" ;;
  *)
    echo "错误：官方资产仅支持 macOS（Apple Silicon / Intel）；当前为 ${os}-${arch}。" >&2
    echo "Windows 用户请从 Releases 页面下载 token-usage-windows-amd64.exe；其他平台请从源码构建。" >&2
    exit 1
    ;;
esac

# 未显式指定版本时，从 GitHub API 取最新 Release tag（列表按发布时间倒序，含预发布）。
if [ -z "${TAG}" ]; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=1" \
    | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  if [ -z "${TAG}" ]; then
    echo "错误：无法获取最新 Release tag。" >&2
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

chmod +x "${tmp}/${asset}"
mkdir -p "${INSTALL_DIR}"
if [ -w "${INSTALL_DIR}" ]; then
  mv "${tmp}/${asset}" "${INSTALL_DIR}/token-usage"
else
  sudo mv "${tmp}/${asset}" "${INSTALL_DIR}/token-usage"
fi

"${INSTALL_DIR}/token-usage" version
echo "完成：token-usage 已安装到 ${INSTALL_DIR}/token-usage"
echo "运行 token-usage --help 查看可用命令。"
