// Package releasenotes 组装 GitHub Release 的定制 body 文本。
//
// 它把发布说明拆成「纯逻辑（本文件：BuildBody/Nature/SplitNotes，无文件与网络 IO）」
// 与「IO（credits.go：调 gh 提取贡献者）」两层。纯逻辑层可被表驱动测试充分覆盖，
// 所有动态输入（tag、手写中英内容、致谢）经 Options 显式传入。
//
// body 结构为：英文段（版本标题 + 版本性质 + 内容 + 致谢 + 资产 + 校验）在前，
// 独占一行的 --- 在中，中文段在后。致谢段在外部贡献为空时整体省略（含标题）。
package releasenotes

import (
	"errors"
	"fmt"
	"strings"
)

// Contributor 表示一位外部贡献者及其对应的 PR 或 issue 编号。
type Contributor struct {
	Login  string
	Number int
}

// ThanksData 汇总本版本要致谢的 PR 与 issue 贡献者，两类均可为空。
type ThanksData struct {
	PRs    []Contributor
	Issues []Contributor
}

// Options 是 BuildBody 的全部输入。所有字段不可省略地由调用方提供。
type Options struct {
	Tag       string     // 发布版本 tag，如 v0.1.0-rc.1
	ContentEN string     // 手写英文版本内容（New/Changed/Fixed 等小节）
	ContentZH string     // 手写中文版本内容（新功能/变更/修复 等小节）
	Thanks    ThanksData // 自动提取的外部贡献致谢
}

// enMarker / zhMarker 是手写内容文件里分隔中英文的 HTML 注释标记。
const (
	enMarker = "<!-- en -->"
	zhMarker = "<!-- zh -->"
)

// assetsSectionEN 是英文资产列表段（二进制资产名与正式分发产物逐字一致）。
const assetsSectionEN = "### Assets\n" +
	"- `token-usage-darwin-arm64` — macOS (Apple Silicon)\n" +
	"- `token-usage-darwin-amd64` — macOS (Intel)\n" +
	"- `token-usage-windows-amd64.exe` — Windows"

// verifySectionEN 是英文校验与安装指引段（macOS/Linux 用 shasum，Windows 用 certutil）。
const verifySectionEN = "### Verify & install\n" +
	"1. Download the asset for your platform and `SHA256SUMS`.\n" +
	"2. Verify: `shasum -a 256 token-usage-darwin-arm64` (Windows: `certutil -hashfile token-usage-windows-amd64.exe SHA256`).\n" +
	"3. Make it executable (macOS/Linux) and run `./token-usage version`."

// assetsSectionZH 是中文资产列表段。
const assetsSectionZH = "### 资产\n" +
	"- `token-usage-darwin-arm64` — macOS（Apple 芯片）\n" +
	"- `token-usage-darwin-amd64` — macOS（Intel）\n" +
	"- `token-usage-windows-amd64.exe` — Windows"

// verifySectionZH 是中文校验与安装指引段。
const verifySectionZH = "### 校验与安装\n" +
	"1. 下载与你平台匹配的资产以及 `SHA256SUMS`。\n" +
	"2. 校验：`shasum -a 256 token-usage-darwin-arm64`（Windows：`certutil -hashfile token-usage-windows-amd64.exe SHA256`）。\n" +
	"3. 赋予执行权限（macOS/Linux）并运行 `./token-usage version`。"

// BuildBody 按既定结构拼接完整 release body。它是纯函数：无文件与网络 IO，
// 全部输入经 opts 传入。英文段在前、--- 独占一行、中文段在后。
func BuildBody(opts Options) string {
	en := strings.Join(enBlockParts(opts), "\n\n")
	zh := strings.Join(zhBlockParts(opts), "\n\n")
	return en + "\n\n---\n\n" + zh + "\n"
}

// enBlockParts 组装英文段的所有子块，顺序：标题、性质、内容、致谢、资产、校验。
func enBlockParts(opts Options) []string {
	parts := []string{
		fmt.Sprintf("## token-usage %s", opts.Tag),
		NatureEN(opts.Tag),
	}
	parts = appendNonEmpty(parts, opts.ContentEN)
	parts = appendNonEmpty(parts, acknowledgementsEN(opts.Thanks))
	parts = append(parts, assetsSectionEN, verifySectionEN)
	return parts
}

// zhBlockParts 组装中文段的所有子块，顺序与英文段对应。
func zhBlockParts(opts Options) []string {
	parts := []string{
		fmt.Sprintf("## token-usage %s（中文说明）", opts.Tag),
		NatureZH(opts.Tag),
	}
	parts = appendNonEmpty(parts, opts.ContentZH)
	parts = appendNonEmpty(parts, acknowledgementsZH(opts.Thanks))
	parts = append(parts, assetsSectionZH, verifySectionZH)
	return parts
}

// appendNonEmpty 仅在 s 去除首尾空白后非空时追加，避免出现连续空行。
func appendNonEmpty(parts []string, s string) []string {
	if strings.TrimSpace(s) == "" {
		return parts
	}
	return append(parts, s)
}

// NatureEN 返回英文版本性质说明。tag 含 -rc. 视为候选版，返回预发布提示；
// 否则返回稳定版说明。
func NatureEN(tag string) string {
	if isRCTag(tag) {
		return fmt.Sprintf("> **Note:** This is a pre-release (%s) for testing; behavior may change before the stable release.", tag)
	}
	return fmt.Sprintf("This is the stable release of %s.", tag)
}

// NatureZH 返回中文版本性质说明，与 NatureEN 的 rc/稳定判定一致。
func NatureZH(tag string) string {
	if isRCTag(tag) {
		return fmt.Sprintf("> **提示：** 本版本为预发布（%s），仅供测试；稳定版发布前行为可能调整。", tag)
	}
	return fmt.Sprintf("本版本为 %s 稳定版发布。", tag)
}

// isRCTag 判定 tag 是否为候选版（含 -rc. 字样）。
func isRCTag(tag string) bool {
	return strings.Contains(tag, "-rc.")
}

// acknowledgementsEN 拼接英文致谢段。PR 与 issue 均为空时返回空串，
// 使整段（含标题）被省略。
func acknowledgementsEN(t ThanksData) string {
	if len(t.PRs) == 0 && len(t.Issues) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "### Acknowledgements")
	for _, c := range t.PRs {
		lines = append(lines, fmt.Sprintf("- PRs: @%s (#%d)", c.Login, c.Number))
	}
	for _, c := range t.Issues {
		lines = append(lines, fmt.Sprintf("- Issues: @%s (#%d)", c.Login, c.Number))
	}
	return strings.Join(lines, "\n")
}

// acknowledgementsZH 拼接中文致谢段，省略规则与英文版一致。
func acknowledgementsZH(t ThanksData) string {
	if len(t.PRs) == 0 && len(t.Issues) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "### 致谢")
	for _, c := range t.PRs {
		lines = append(lines, fmt.Sprintf("- 代码贡献：@%s (#%d)", c.Login, c.Number))
	}
	for _, c := range t.Issues {
		lines = append(lines, fmt.Sprintf("- 问题反馈：@%s (#%d)", c.Login, c.Number))
	}
	return strings.Join(lines, "\n")
}

// SplitNotes 按 <!-- en --> / <!-- zh --> 标记把手写内容文件拆为中英两段。
// 英文标记必须出现在中文标记之前；任一标记缺失或对应段去空白后为空均报错。
func SplitNotes(raw string) (en, zh string, err error) {
	enStart := strings.Index(raw, enMarker)
	zhStart := strings.Index(raw, zhMarker)
	if enStart < 0 {
		return "", "", errors.New("缺少英文内容标记 " + enMarker)
	}
	if zhStart < 0 {
		return "", "", errors.New("缺少中文内容标记 " + zhMarker)
	}
	if enStart > zhStart {
		return "", "", errors.New("英文内容标记应出现在中文标记之前")
	}

	enContent := strings.TrimSpace(raw[enStart+len(enMarker) : zhStart])
	zhContent := strings.TrimSpace(raw[zhStart+len(zhMarker):])
	if enContent == "" {
		return "", "", errors.New("英文内容段为空")
	}
	if zhContent == "" {
		return "", "", errors.New("中文内容段为空")
	}
	return enContent, zhContent, nil
}
