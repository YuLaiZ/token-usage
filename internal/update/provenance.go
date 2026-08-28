package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// provenance.go 实现「当前安装来源验证」安全门。
//
// 在任何替换前必须确认：当前被覆盖的二进制确实是官方 Release 资产（其 SHA256 等于
// 当前版本 Release 的 SHA256SUMS 中对应平台的 hash）。否则一律拒绝自动覆盖，
// 改为给出目标版本与人工安装指引。这是安全门，不是版本展示逻辑。
//
// 本文件实现判定顺序的第 1/3/4/5 步（第 2 步目标版本比较由 update.go 负责）：
//  1. 解析当前版本；dev 或非正式 tag → untrusted；
//  3. 解析当前可执行文件路径；Lstat 确认是普通文件且非 symlink，路径须绝对；
//  4. 读取当前版本 Release 的 SHA256SUMS（经 ManifestFetcher），计算当前二进制 SHA256；
//  5. 仅当本地 hash == 该平台官方资产 hash 才视为 trusted。
//
// 本文件不下载目标资产，只回答「当前二进制是否为已知官方 Release 资产」。

// ManifestFetcher 抽象「按 tag 拉取 SHA256SUMS 清单」。
// 生产实现经 downloader.FetchManifest(tag) 从固定 GitHub 下载前缀获取并解析；
// 测试注入内存 fake，杜绝真实网络与磁盘 IO。
// 该 seam 与 ReleaseClient 解耦：ReleaseClient 负责获取 Release 元数据，
// ManifestFetcher 负责获取该 Release 的 SHA256SUMS 清单（两者 URL 前缀不同）。
type ManifestFetcher interface {
	// FetchManifest 获取并解析指定 tag 的 SHA256SUMS，返回 Manifest。
	// tag 为当前安装版本对应的 Release tag（非目标版本）。
	FetchManifest(ctx context.Context, tag string) (*Manifest, error)
}

// ProvenanceDeps 承载 VerifyProvenance 的全部可注入依赖。
// 所有外部副作用（路径解析、文件元信息、文件内容、网络清单、平台信息）均经此结构注入，
// 使来源校验可在无真实 OS / 无真实网络下做确定性测试。
type ProvenanceDeps struct {
	// Executable 解析「当前可执行文件路径」。生产包装 os.Executable。
	Executable ExecutableResolver
	// Lstat 不跟随 symlink 地查询路径元信息。生产包装 os.Lstat。
	Lstat Lstat
	// FileReader 读取当前二进制完整内容用于 SHA256 计算。生产包装 os.ReadFile。
	FileReader FileReader
	// Manifest 经 tag 拉取当前版本的 SHA256SUMS。生产用 downloader.FetchManifest。
	Manifest ManifestFetcher
	// Goos / Goarch 是当前平台标识，决定选用哪个平台资产名。
	// 生产用 runtime.GOOS / runtime.GOARCH；测试可注入任意组合。
	Goos   string
	Goarch string
	// SignatureProbe 对当前二进制做签名元信息探测（仅 darwin 生产实现）。
	// 只在 hash 不匹配分支被调用以细化提示文案；nil 或探测失败降级通用文案。
	SignatureProbe SignatureProbe
}

// SignatureProbeResult 是签名探测的结论。
type SignatureProbeResult int

const (
	// SignatureUnknown 表示探测不可用或失败（非 darwin 平台、codesign 缺失、
	// 超时等），调用方应降级为通用文案。
	SignatureUnknown SignatureProbeResult = iota
	// SignatureAdhoc 表示检测到 ad-hoc 签名标记。
	SignatureAdhoc
)

// SignatureProbe 抽象「对二进制做签名元信息探测」。
// 生产 darwin 实现执行 codesign -dv——它是签名元信息展示命令，不做完整性校验；
// 仅以其 stderr 的 flags 字段含 adhoc 判定「带 ad-hoc 签名标记」。
// 探测结论不证明二进制未被改动，只用于细化提示文案，不参与可信判定。
type SignatureProbe interface {
	ProbeSignature(ctx context.Context, binPath string) SignatureProbeResult
}

// ProvenanceExemption 表达「结构前置全部通过、但官方信任未建立」的豁免资格。
// 它描述当前来源的客观状态，与本次请求是否携带 force 无关：
// 走到对应判定步即按事实赋值，force 只决定 Apply 是否据此继续安装。
type ProvenanceExemption string

const (
	// ExemptionHashMismatch：存在可比的官方资产但内容不一致
	//（按安装指引重签修复 Gatekeeper 拦截的官方资产、go install pkg@vX.Y.Z 场景）。
	ExemptionHashMismatch ProvenanceExemption = "hash-mismatch"
	// ExemptionDevBuild：当前版本为 dev（本地构建）——不存在可比的官方 Release
	// 与 manifest，从未发生 hash 比较；该场景禁止使用「hash 不匹配」表述，
	// 诊断、日志与帮助不得误报原因。
	ExemptionDevBuild ProvenanceExemption = "dev-build"
)

// ForceEligible 报告该豁免资格是否允许 update --force 覆盖当前来源。
// 显式白名单：只有此处逐一列出的豁免值可被 force 豁免，未来新增枚举值必须
// 显式修改本函数才可能获得资格，权限边界不随枚举扩值隐式放宽。
// 空值表示无豁免资格（信任已建立，或结构/清单任一检查失败——force 不可救）。
func (e ProvenanceExemption) ForceEligible() bool {
	return e == ExemptionHashMismatch || e == ExemptionDevBuild
}

// ProvenanceOptions 是 VerifyProvenance 的行为选项。
type ProvenanceOptions struct {
	// Force 允许为具备豁免资格的来源继续安装前的判定。对豁免枚举赋值的影响按分支区分：
	//   - dev 分支：非 force 维持第 1 步短路（Exemption 为空）；force 完成结构前置
	//     检查后置 dev-build——该分支的枚举赋值随 Force 变化；
	//   - hash 失配分支：赋值与 Force 无关，走到第 9 步失配即置 hash-mismatch。
	// 任何情况下 Force 都不把不可信结果变为可信（Trusted=false 语义不变）。
	Force bool
}

// ProvenanceResult 是 VerifyProvenance 的判定结果。
// Trusted=true 表示当前二进制是已知官方 Release 资产，可安全覆盖；
// Trusted=false 时 Reason 必填，供上层展示人工安装指引。
// 其余字段在对应阶段填充，便于诊断与日志。
type ProvenanceResult struct {
	// Trusted 是否通过全部来源校验。
	Trusted bool
	// Reason 不可信原因（Trusted=false 必填；Trusted=true 为空）。
	Reason string
	// CurrentTag 当前安装版本对应的 Release tag（解析成功时填充）。
	CurrentTag string
	// BinaryPath 当前可执行文件绝对路径（解析成功时填充）。
	BinaryPath string
	// Asset 当前平台对应的二进制资产名（平台受支持时填充）。
	Asset string
	// LocalHash 当前二进制实际 SHA256（成功读取并计算时填充，64 位小写 hex）。
	LocalHash string
	// OfficialHash 当前版本官方资产 SHA256（manifest 命中时填充，64 位小写 hex）。
	OfficialHash string
	// Exemption 豁免资格（描述来源状态；信任已建立或结构/清单检查失败时为空）。
	Exemption ProvenanceExemption
}

// VerifyProvenance 校验当前安装来源是否为官方 Release 资产。
//
// 该函数只做当前来源验证，不做目标版本比较，也不下载目标资产。
// 任一阶段失败均返回 (ProvenanceResult{Trusted:false, Reason:...}, nil)：
//   - 来源不可信是领域结果，不是错误；调用方据此走「人工安装」分支。
//   - 仅当注入不完整（deps 字段为 nil）这类编程错误返回 error。
//
// 判定顺序（任一步失败即短路为 untrusted，不再执行后续步骤）：
//  1. 解析 currentVersion；dev 或非正式 tag → untrusted（force 的 dev 例外，
//     跳过版本解析与 Release/manifest 查询，仅做结构前置检查后置 dev-build 豁免）；
//  2. 校验 deps 必填字段齐全；
//  3. Executable 解析路径；非绝对路径 → untrusted；
//  4. Lstat 确认是普通文件且非 symlink；目录/symlink/不存在 → untrusted；
//  5. FileReader 读取二进制并计算 SHA256；
//  6. ManifestFetcher 拉取当前版本 manifest；失败或解析失败 → untrusted；
//  7. AssetName(goos, goarch) 命中平台资产；不受支持 → untrusted；
//  8. manifest.HashFor(asset) 命中官方 hash；未命中 → untrusted；
//  9. 本地 hash == 官方 hash → trusted；否则 untrusted 并按事实置 hash-mismatch
//     豁免（赋值与 force 无关），darwin 上先做签名探测以细化提示文案。
func VerifyProvenance(ctx context.Context, deps ProvenanceDeps, currentVersion string, rc ReleaseClient, opts ProvenanceOptions) (ProvenanceResult, error) {
	// 第 1 步：解析当前版本。dev 或非法 tag 直接判不可信（短路，不触网/不读盘）。
	if currentVersion == "dev" {
		if !opts.Force {
			return untrusted(ui.Bi(
				"current version is dev (local build); official provenance cannot be verified, please install manually",
				"当前版本为 dev（本地构建），无法验证官方来源，请手动安装",
			)), nil
		}
		// force 的 dev：无可查询的官方 Release 与 manifest，
		// 仅完成结构前置检查后置 dev-build 豁免。
		return verifyDevBuildForce(deps)
	}
	ver, err := ParseVersion(currentVersion)
	if err != nil {
		return untrusted(ui.Bi(
			fmt.Sprintf("current version %q is not an official release tag; official provenance cannot be verified, please install manually", currentVersion),
			fmt.Sprintf("当前版本 %q 非正式 Release tag，无法验证官方来源，请手动安装", currentVersion),
		)), nil
	}
	result := ProvenanceResult{CurrentTag: currentVersion}
	_ = ver // 已确认合法；tag 字面量即后续查询键

	// 第 2-4 步：deps 校验、路径解析、Lstat 普通文件检查。
	result, failed, serr := resolveBinaryStructure(deps, result)
	if serr != nil {
		return ProvenanceResult{}, serr
	}
	if failed {
		return result, nil
	}

	// 第 5 步：读取当前二进制并计算 SHA256。
	content, err := deps.FileReader.ReadFile(result.BinaryPath)
	if err != nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("cannot read the current binary: %v; please install manually", err),
			fmt.Sprintf("无法读取当前二进制内容: %v；请手动安装", err),
		)), nil
	}
	sum := sha256.Sum256(content)
	result.LocalHash = hex.EncodeToString(sum[:])

	// 第 6 步：先确认当前版本存在合法的官方 Release（经 ReleaseClient 查询），
	// 再拉取该 Release 的 SHA256SUMS manifest。任一环节失败均视为来源不可信。
	// 这一步同时把 currentVersion 当作精确 tag 查询，确认它确实是已发布的官方版本。
	if rc == nil {
		return untrustedWith(result, ui.Bi(
			"no release query client configured; provenance cannot be verified, please install manually",
			"未配置 Release 查询客户端，无法验证来源；请手动安装",
		)), nil
	}
	curRelease, err := rc.FetchRelease(ctx, currentVersion)
	if err != nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("failed to fetch the official release for current version %s: %v; please install manually", currentVersion, err),
			fmt.Sprintf("当前版本 %s 的官方 Release 查询失败: %v；请手动安装", currentVersion, err),
		)), nil
	}
	if curRelease == nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("current version %s has no matching official release; please install manually", currentVersion),
			fmt.Sprintf("当前版本 %s 无对应官方 Release；请手动安装", currentVersion),
		)), nil
	}
	if curRelease.Tag != currentVersion {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("current version %s does not match official release tag %q; please install manually", currentVersion, curRelease.Tag),
			fmt.Sprintf("当前版本 %s 与官方 Release tag %q 不一致；请手动安装", currentVersion, curRelease.Tag),
		)), nil
	}

	// 拉取当前版本的官方 manifest。未注入 ManifestFetcher 视为不可信。
	if deps.Manifest == nil {
		return untrustedWith(result, ui.Bi(
			"no official manifest fetcher configured; provenance cannot be verified, please install manually",
			"未配置官方清单获取方式，无法验证来源；请手动安装",
		)), nil
	}
	manifest, err := deps.Manifest.FetchManifest(ctx, currentVersion)
	if err != nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("cannot fetch the official manifest for current version %s: %v; please install manually", currentVersion, err),
			fmt.Sprintf("无法获取当前版本 %s 的官方清单: %v；请手动安装", currentVersion, err),
		)), nil
	}
	if manifest == nil {
		return untrustedWith(result, ui.Bi(
			"official manifest for the current version is empty; provenance cannot be compared, please install manually",
			"当前版本官方清单为空，无法比对来源；请手动安装",
		)), nil
	}

	// 第 7 步：当前平台须受支持。
	assetName, ok := AssetName(deps.Goos, deps.Goarch)
	if !ok {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("no official asset for current platform %s/%s, please install manually", deps.Goos, deps.Goarch),
			fmt.Sprintf("当前平台 %s/%s 无官方资产，请手动安装", deps.Goos, deps.Goarch),
		)), nil
	}
	result.Asset = assetName

	// 第 8 步：manifest 须含当前平台资产 hash。
	official, ok := manifest.HashFor(assetName)
	if !ok {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("manifest for current version %s is missing the hash of platform asset %s, please install manually", currentVersion, assetName),
			fmt.Sprintf("当前版本 %s 清单缺少平台资产 %s 的 hash，请手动安装", currentVersion, assetName),
		)), nil
	}
	result.OfficialHash = official

	// 第 9 步：本地 hash 须严格等于官方 hash。
	if result.LocalHash != official {
		// 豁免资格按事实赋值，与本次请求是否 force 无关：
		// 走到本步即存在可比官方资产且内容不一致。
		result.Exemption = ExemptionHashMismatch
		return untrustedWith(result, hashMismatchReason(ctx, deps, result.BinaryPath)), nil
	}

	result.Trusted = true
	return result, nil
}

// resolveBinaryStructure 执行结构前置检查：deps 校验 → 解析可执行文件路径（须绝对）
// → Lstat 确认普通文件且非 symlink。通过时返回 (携带 BinaryPath 的 result, false, nil)；
// 任一检查失败返回 (不可信结果, true, nil)；deps 不完整属编程错误，返回 (零值, false, err)。
func resolveBinaryStructure(deps ProvenanceDeps, result ProvenanceResult) (ProvenanceResult, bool, error) {
	// nil 属于编程错误，返回 error 而非领域结果。
	if err := deps.validate(); err != nil {
		return ProvenanceResult{}, false, err
	}
	binPath, err := deps.Executable.Executable()
	if err != nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("cannot resolve the current executable path: %v; please install manually", err),
			fmt.Sprintf("无法解析当前可执行文件路径: %v；请手动安装", err),
		)), true, nil
	}
	if !filepath.IsAbs(binPath) {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("current executable path %q is not absolute; it may come from go install / a local build, please install manually", binPath),
			fmt.Sprintf("当前可执行文件路径 %q 非绝对路径，可能为 go install / 本地构建，请手动安装", binPath),
		)), true, nil
	}
	result.BinaryPath = binPath

	info, err := deps.Lstat.Lstat(binPath)
	if err != nil {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("cannot stat the current executable: %v; please install manually", err),
			fmt.Sprintf("无法读取当前可执行文件元信息: %v；请手动安装", err),
		)), true, nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return untrustedWith(result, ui.Bi(
			"current executable is a symlink; cannot be replaced safely, please install manually",
			"当前可执行文件是符号链接，无法安全覆盖，请手动安装",
		)), true, nil
	}
	if !info.Mode().IsRegular() {
		return untrustedWith(result, ui.Bi(
			fmt.Sprintf("current executable is not a regular file (mode %s), please install manually", info.Mode()),
			fmt.Sprintf("当前可执行文件不是普通文件（mode %s），请手动安装", info.Mode()),
		)), true, nil
	}
	return result, false, nil
}

// verifyDevBuildForce 执行 dev + force 的来源判定：不存在可比的官方 Release 与
// manifest，跳过版本解析与 Release/manifest 查询，仅完成结构前置检查。
// 全部通过后置 dev-build 豁免（从未发生 hash 比较，Reason 不得使用 hash 表述）。
func verifyDevBuildForce(deps ProvenanceDeps) (ProvenanceResult, error) {
	result, failed, err := resolveBinaryStructure(deps, ProvenanceResult{})
	if err != nil {
		return ProvenanceResult{}, err
	}
	if failed {
		// 结构失败（symlink、非普通文件、路径不合法等）不具豁免资格，force 不可救。
		return result, nil
	}
	result.Exemption = ExemptionDevBuild
	result.Reason = ui.Bi(
		"current version is dev (local build), so the source cannot be verified; --force will overwrite it with an official release asset, whose SHA256 is still verified against SHA256SUMS",
		"当前版本为 dev（本地构建），来源无法验证；--force 将用官方资产覆盖，目标资产 SHA256 校验仍会执行",
	)
	return result, nil
}

// hashMismatchReason 构造 hash 失配的不可信原因。darwin 平台先做一次签名探测：
// 探测到 ad-hoc 签名标记时使用细化文案（重签官方资产 / go install / 本地构建并列）；
// 其余情况（非 darwin、未注入探测、探测失败）降级通用文案——同样列出重签可能项
// 与 --force 出口。探测只影响文案，不影响判定；仅在本分支触发，其他失败路径零开销。
func hashMismatchReason(ctx context.Context, deps ProvenanceDeps, binPath string) string {
	if deps.Goos == "darwin" && deps.SignatureProbe != nil &&
		deps.SignatureProbe.ProbeSignature(ctx, binPath) == SignatureAdhoc {
		return ui.Bi(
			"current binary carries a local ad-hoc signature: it may be the official asset re-signed per the install guide to fix a Gatekeeper block, or a go install / local build; install manually, or run the update again with --force to overwrite",
			"当前二进制带本地 ad-hoc 签名：可能是按安装指引重签修复 Gatekeeper 拦截的官方资产，也可能是 go install / 本地构建产物；请手动安装，或使用 --force 再次执行以强制覆盖",
		)
	}
	return ui.Bi(
		"current binary hash does not match the official asset (possibly re-signed per the install guide, or go install / local build / tampered); install manually, or run the update again with --force to overwrite",
		"当前二进制 hash 与官方资产不一致（可能为按安装指引重签、go install / 本地构建 / 已被篡改）；请手动安装，或使用 --force 再次执行以强制覆盖",
	)
}

// validate 校验 ProvenanceDeps 必填字段齐全。nil 属于编程错误。
func (d ProvenanceDeps) validate() error {
	if d.Executable == nil {
		return errors.New(ui.Bi("ProvenanceDeps.Executable must not be nil", "ProvenanceDeps.Executable 不能为空"))
	}
	if d.Lstat == nil {
		return errors.New(ui.Bi("ProvenanceDeps.Lstat must not be nil", "ProvenanceDeps.Lstat 不能为空"))
	}
	if d.FileReader == nil {
		return errors.New(ui.Bi("ProvenanceDeps.FileReader must not be nil", "ProvenanceDeps.FileReader 不能为空"))
	}
	// Manifest 允许在 Check 阶段为 nil（Check 不做来源校验），
	// 但 VerifyProvenance 调用前必须注入；此处不强制，留给 VerifyProvenance 的 manifest 阶段处理。
	return nil
}

// untrusted 构造一个仅含 Reason 的不可信结果（用于解析阶段尚未填充其它字段时）。
func untrusted(reason string) ProvenanceResult {
	return ProvenanceResult{Trusted: false, Reason: reason}
}

// untrustedWith 在已有 result 基础上追加 reason 并标记不可信，
// 保留已填充的 CurrentTag / BinaryPath 等诊断字段。
func untrustedWith(r ProvenanceResult, reason string) ProvenanceResult {
	r.Trusted = false
	r.Reason = reason
	return r
}
