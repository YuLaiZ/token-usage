package update

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// manifest.go 实现 SHA256SUMS 清单的严格解析与目标资产 hash 查询。
//
// SHA256SUMS 清单的格式契约：
//   - 无头 UTF-8 文本，恰好三行，每行按资产名 ASCII 升序排列；
//   - 每行格式：<64 位小写 hex SHA256> + 两个空格 + 精确资产名 + 换行（\n 或 \r\n）；
//   - 三行对应三个二进制资产名（SHA256SUMS 自身不出现在清单中）。
//
// 解析器必须先完成整份文档校验，再允许查询目标 hash：
// 不边扫描边容忍坏行。任何畸形输入返回错误与 nil Manifest。
// 本文件不涉及 I/O 与网络，仅做字节流解析。

// manifestBinaryAssets 是清单允许出现的三个二进制资产名，按 ASCII 升序排列。
// 顺序即权威排序，解析器据此校验输入行的次序。
var manifestBinaryAssets = []string{
	"token-usage-darwin-amd64",
	"token-usage-darwin-arm64",
	"token-usage-windows-amd64.exe",
}

// manifestLineCount 是合法清单的固定行数（每个二进制资产一行）。
const manifestLineCount = 3

// Manifest 是解析后的 SHA256SUMS 清单值对象。
// 通过 HashFor 查询目标资产名对应的 64 位小写 hash。
// 零值 Manifest 不可用（HashFor 返回 false）。
type Manifest struct {
	hashes map[string]string // 资产名 → 64 位小写 hex
}

// ParseManifest 严格解析 SHA256SUMS 清单字节流。
//
// 校验顺序：
//  1. 非空输入；
//  2. 行数恰好等于 manifestLineCount；
//  3. 每行严格符合 `<hash>  <name>` 格式（两空格分隔）；
//  4. hash 为 64 位小写 hex，资产名在冻结集合中且无路径分隔符/空白；
//  5. 资产名集合恰好等于冻结集合（无缺项、无重复、无多余）；
//  6. 行序与冻结集合的 ASCII 升序一致。
//
// 任一校验失败返回错误与 nil Manifest。
//
// 行结束符：接受 \n 与 \r\n；行内出现 \r（非行尾）视为异常。
// 整份输入必须以换行结尾（每行都有换行符），否则拒绝。
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return nil, errors.New("清单内容为空")
	}
	// 仅以 \n 切行；\r\n 中的 \r 留待逐行校验时剥离。
	// 不使用 bytes.Split(_, "\n") 后直接断言长度，因尾随换行会产生尾空段。
	lines, err := splitManifestLines(data)
	if err != nil {
		return nil, err
	}
	if len(lines) != manifestLineCount {
		return nil, fmt.Errorf("清单行数必须为 %d，实际 %d", manifestLineCount, len(lines))
	}

	hashes := make(map[string]string, manifestLineCount)
	seenNames := make([]string, 0, manifestLineCount)
	for i, raw := range lines {
		hash, name, perr := parseManifestLine(raw, i)
		if perr != nil {
			return nil, perr
		}
		if _, dup := hashes[name]; dup {
			return nil, fmt.Errorf("清单第 %d 行资产名 %q 重复", i+1, name)
		}
		hashes[name] = hash
		seenNames = append(seenNames, name)
	}

	// 集合精确匹配 + 顺序匹配（一次性比较）。
	for i, want := range manifestBinaryAssets {
		if seenNames[i] != want {
			// 区分「顺序错」与「集合错」给更清晰错误。
			gotSorted := append([]string(nil), seenNames...)
			sort.Strings(gotSorted)
			equalSet := true
			for j := range gotSorted {
				if gotSorted[j] != manifestBinaryAssets[j] {
					equalSet = false
					break
				}
			}
			if !equalSet {
				return nil, fmt.Errorf("清单资产集合与冻结集合不符：got %v", seenNames)
			}
			return nil, fmt.Errorf("清单第 %d 行资产名顺序错误：got %q want %q（须 ASCII 升序）", i+1, seenNames[i], want)
		}
	}

	return &Manifest{hashes: hashes}, nil
}

// splitManifestLines 把原始字节按 \n 切成「行内容」切片（去除每行结尾的 \n 与可选 \r）。
// 校验：必须以 \n 结尾；不允许出现单独 \r（非 \r\n 的孤立 CR）或空行；
// 不允许出现 \0 等控制字符以外的异常字节（这里只校验行结构与空行）。
func splitManifestLines(data []byte) ([]string, error) {
	// 必须以 \n 结尾（每行含尾随换行）。
	if data[len(data)-1] != '\n' {
		return nil, errors.New("清单必须以换行符结尾")
	}
	// 切分前先拒绝裸 \r（单独出现的回车，非 \r\n 一部分）：扫描 \r\n 配对。
	if err := validateNewlines(data); err != nil {
		return nil, err
	}
	// 以 \n 切分；因尾部有 \n，最后一个元素为空串，需丢弃。
	parts := strings.Split(string(data), "\n")
	// 末尾空串是尾部换行产生的，不是真实行。
	parts = parts[:len(parts)-1]
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		// 剥离 \r\n 的 \r。
		if len(p) > 0 && p[len(p)-1] == '\r' {
			p = p[:len(p)-1]
		}
		if p == "" {
			return nil, fmt.Errorf("清单第 %d 行为空行（不允许）", i+1)
		}
		out = append(out, p)
	}
	return out, nil
}

// validateNewlines 拒绝裸 \r（非 \r\n 一部分的回车）。
// 清单行尾接受 CRLF 与 LF，但拒绝 CRLF 之外的异常行（如孤立 CR 或 CR+其它）。
func validateNewlines(data []byte) error {
	for i := 0; i < len(data); i++ {
		if data[i] != '\r' {
			continue
		}
		// 当前是 \r，必须是 \r\n。
		if i+1 < len(data) && data[i+1] == '\n' {
			i++ // 跳过 \n
			continue
		}
		return fmt.Errorf("清单含非法回车（非 CRLF）：偏移 %d", i)
	}
	return nil
}

// parseManifestLine 解析单行为 (hash, name)。严格校验：
//   - 形如 <64hex> + "  "（恰好两空格）+ <name>；
//   - hash 为 64 位小写十六进制；
//   - name 非空、不含路径分隔符（/ 与 \）与空白、且在冻结资产名集合内。
func parseManifestLine(line string, idx int) (hash, name string, err error) {
	// 恰好两空格分隔：找到 "  "，且其前其后不再有连续空格。
	sepi := strings.Index(line, "  ")
	if sepi < 0 {
		return "", "", fmt.Errorf("清单第 %d 行缺少两空格分隔符", idx+1)
	}
	hash = line[:sepi]
	rest := line[sepi+2:]
	// rest 必须不再含空格/制表符（资产名内无空白）；rest 也不能以空格开头（已 +2，仍校验）。
	if rest == "" {
		return "", "", fmt.Errorf("清单第 %d 行资产名缺失", idx+1)
	}
	if strings.ContainsAny(rest, " \t") {
		return "", "", fmt.Errorf("清单第 %d 行资产名含空白", idx+1)
	}
	name = rest

	// hash 校验：64 位小写 hex。
	if !isLowerHex64(hash) {
		return "", "", fmt.Errorf("清单第 %d 行 hash %q 必须为 64 位小写十六进制", idx+1, hash)
	}

	// name 校验：禁止路径分隔符与空白（防御文件名注入）。
	if strings.ContainsAny(name, "/\\") {
		return "", "", fmt.Errorf("清单第 %d 行资产名 %q 含路径分隔符", idx+1, name)
	}
	// name 必须在冻结集合内（拒绝多余/未知资产名）。
	if !isKnownManifestAsset(name) {
		return "", "", fmt.Errorf("清单第 %d 行出现未知资产名 %q", idx+1, name)
	}
	return hash, name, nil
}

// isKnownManifestAsset 报告 name 是否在冻结的二进制资产名集合内。
func isKnownManifestAsset(name string) bool {
	for _, n := range manifestBinaryAssets {
		if n == name {
			return true
		}
	}
	return false
}

// isLowerHex64 报告 s 是否恰好 64 个字符且全部为 0-9 / a-f。
// 拒绝大写、非 hex、长度 != 64。
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// HashFor 查询资产名对应的 64 位小写 hash。
// 命中返回 (hash, true)；未命中或 Manifest 未初始化返回 ("", false)。
func (m *Manifest) HashFor(assetName string) (string, bool) {
	if m == nil || m.hashes == nil {
		return "", false
	}
	h, ok := m.hashes[assetName]
	return h, ok
}
