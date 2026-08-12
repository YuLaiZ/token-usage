package update

import (
	"errors"
	"fmt"
	"sort"
)

// assets.go 实现平台 → 资产名映射、Release/Asset 值对象与 Release 元数据校验。
//
// 自更新发布物的资产契约（平台 → 二进制资产名固定）：
//
//	平台            二进制资产名
//	darwin/arm64    token-usage-darwin-arm64
//	darwin/amd64    token-usage-darwin-amd64
//	windows/amd64   token-usage-windows-amd64.exe
//	所有平台        SHA256SUMS
//
// 一个可用 Release 必须恰好包含上述全部四项资产（不多不少）。
// 本文件不涉及 I/O 与网络，仅做纯映射与集合校验。

// 受支持的二进制资产名集合，键为 "GOOS/GOARCH"。
// 任何新增平台必须在此显式登记；不支持的平台稳定返回 ("", false)。
var platformAssetNames = map[string]string{
	"darwin/arm64":  "token-usage-darwin-arm64",
	"darwin/amd64":  "token-usage-darwin-amd64",
	"windows/amd64": "token-usage-windows-amd64.exe",
}

// SumsAssetName 是 SHA256SUMS 清单文件资产名，所有平台共用。
const SumsAssetName = "SHA256SUMS"

// platformAssetNamesSorted 是按 ASCII 升序排列的全部资产名（三二进制 + SHA256SUMS），
// 供 ValidateRelease 做集合精确匹配。
var platformAssetNamesSorted = func() []string {
	seen := map[string]struct{}{}
	for _, n := range platformAssetNames {
		seen[n] = struct{}{}
	}
	seen[SumsAssetName] = struct{}{}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}()

// AssetName 把 (GOOS, GOARCH) 映射到二进制资产名。
// 受支持组合返回 (name, true)；其它一律返回 ("", false)。
// 映射大小写敏感：仅接受规范小写 GOOS/GOARCH。
func AssetName(goos, goarch string) (string, bool) {
	name, ok := platformAssetNames[goos+"/"+goarch]
	if !ok {
		return "", false
	}
	return name, true
}

// NeedsUnixExecMode 报告该 GOOS 的二进制是否需要 Unix 可执行位（0755）。
// Windows PE 文件不依赖可执行位；其它 POSIX 系统（含 darwin/linux/freebsd）需要。
// 该判断与「平台是否受支持」解耦：Linux 虽不受支持但落地后仍需可执行位语义。
func NeedsUnixExecMode(goos string) bool {
	return goos != "windows"
}

// Asset 表示 Release 中一个资产的值对象。仅携带资产名；
// 下载 URL 始终由固定 GitHub 下载前缀 + 校验过的 tag + 资产名重构，
// 不信任 Release JSON 中任意 browser_download_url 字段，故 Asset 不保存 URL。
type Asset struct {
	Name string // 精确资产名，如 token-usage-darwin-arm64
}

// Release 表示一个 GitHub Release 的元数据快照，是自更新流程的核心值对象。
// Tag 与 Version 必须一致（Version 由 Tag 解析得到）。
// Assets 键为资产名，便于 O(1) 查找与集合校验。
type Release struct {
	Tag        string           // Release tag 字面量，如 v0.1.0-rc.1
	Version    Version          // 由 Tag 解析得到的结构化版本
	Draft      bool             // 是否草稿（草稿一律拒绝）
	Prerelease bool             // Release 元数据中的 prerelease 标记
	Assets     map[string]Asset // 资产集合，键为资产名
}

// ValidateRelease 校验 Release 元数据与资产集合的合法性：
//   - r 非 nil；
//   - r.Tag 与 wantTag 一致（防止 latest 别名误用）；
//   - 非 Draft；
//   - 版本与 prerelease 元数据一致：
//     rc 版本必须 Prerelease=true；稳定版必须 Prerelease=false；
//   - allowPrerelease=false 时，rc 版本被拒；
//   - Assets 恰好包含全部四项冻结资产名（不多不少）。
//
// 任一条件不满足返回错误；通过则返回 nil。该校验不触碰网络与文件系统。
func ValidateRelease(r *Release, wantTag string, allowPrerelease bool) error {
	if r == nil {
		return errors.New("release 不能为空")
	}
	if r.Tag != wantTag {
		return fmt.Errorf("release tag 不匹配：got %q want %q", r.Tag, wantTag)
	}
	if r.Draft {
		return fmt.Errorf("release %q 是草稿，拒绝更新", r.Tag)
	}
	// 版本与 prerelease 元数据的一致性：rc 版本必须标记 prerelease，稳定版反之。
	if r.Version.IsPrerelease() {
		if !r.Prerelease {
			return fmt.Errorf("版本 %q 是候选版但 Prerelease=false", r.Tag)
		}
		if !allowPrerelease {
			return fmt.Errorf("版本 %q 是候选版，当前未允许安装预发布版本", r.Tag)
		}
	} else if r.Prerelease {
		return fmt.Errorf("版本 %q 是稳定版但 Prerelease=true", r.Tag)
	}
	// 资产集合精确匹配：键集合必须等于冻结的 {三二进制, SHA256SUMS}。
	if err := validateAssetSet(r.Assets); err != nil {
		return fmt.Errorf("release %q 资产集合非法: %w", r.Tag, err)
	}
	return nil
}

// validateAssetSet 要求资产名键集合恰好等于平台资产名冻结集合（含 SHA256SUMS）。
// 多一项、少一项、错名、空集合均视为非法。
func validateAssetSet(assets map[string]Asset) error {
	if len(assets) != len(platformAssetNamesSorted) {
		return fmt.Errorf("资产数量不匹配：got %d want %d", len(assets), len(platformAssetNamesSorted))
	}
	for _, name := range platformAssetNamesSorted {
		if a, ok := assets[name]; !ok {
			return fmt.Errorf("缺少资产 %q", name)
		} else if a.Name != name {
			return fmt.Errorf("资产键 %q 的 Name 字段不一致: %q", name, a.Name)
		}
	}
	return nil
}
