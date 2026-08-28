package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// ErrDeferredToHelper 表示文件替换已延迟到后台 helper（Windows staged replacement）。
// Windows 不允许替换正在运行的可执行文件：Install 在此返回该 sentinel，表示它已
// 构造 plan、复制 helper.exe 并 spawn 后台 helper；实际 MoveFileEx 与 daemon 切换
// 由 helper 在父进程退出后完成。installUnderLock 检测到该 sentinel 后跳过
// Start/Commit/Rollback（这些全部由 helper 负责），并在 ApplyResult 中报告
// Installed=false、Deferred=true。
// POSIX 的 Install 永不返回此错误，故 POSIX 路径完全不受影响。
var ErrDeferredToHelper = errors.New(ui.Bi(
	"file replacement deferred to background helper",
	"文件替换已延迟到后台 helper",
))

// update.go 实现更新判定与编排骨架：Service.Check（只读判定）与 Service.Apply（带来源校验的执行）。
//
// 判定顺序（与 provenance.go 的来源校验解耦）：
//  1. 解析当前版本；dev / 非正式 tag → Check 与 Apply 均拒绝
//    （update --force 时 dev 例外：结构前置与目标资产校验照常执行后覆盖）；
//  2. 查询目标 Release；目标不严格更高 → 「无需更新」，Check 不写文件，Apply 不做来源校验；
//  3. （仅 Apply，且确有更新时）来源校验：当前二进制是否为官方 Release 资产；
//  4. （仅 Apply，来源可信时）准备下载安装——本任务定义结果类型与集成点，
//     实际下载/原子替换/daemon 切换由后续任务填充。
//
// Check 永不创建本地文件；Apply 仅在「确有更新」分支才做来源校验，
// 来源不可信时只返回目标版本与人工安装指引，绝不下载/覆盖。

// Service 是更新流程的编排器，所有外部依赖经字段注入，便于确定性测试。
// 零值 Service 不可用：调用前必须至少注入 ReleaseClient、CurrentVersion、ProvenanceDeps。
type Service struct {
	// CurrentVersion 当前安装版本字面量（注入，避免逻辑层直接读 buildinfo）。
	// dev 或非正式 tag 会被判定阶段拒绝。
	CurrentVersion string

	// ReleaseClient 查询目标 Release 元数据。生产用 githubReleaseClient。
	ReleaseClient ReleaseClient

	// ProvenanceDeps 来源校验依赖。Apply 在「确有更新」时使用。
	ProvenanceDeps ProvenanceDeps

	// Goos / Goarch 当前平台标识，决定选用哪个目标平台资产名。
	// 默认 runtime.GOOS / runtime.GOARCH；测试可注入任意组合。
	Goos   string
	Goarch string

	// DownloadBase 官方下载前缀。生产用 githubDownloadBase；后续下载任务使用。
	DownloadBase string

	// ControlManager 进程控制锁管理器。Apply 在来源校验通过后用它编排
	// 「锁内 Inspect → Stop → install → StartWithExecutable」。
	// 未注入（nil）时 Apply 只判定到 ReadyToInstall，不做锁内编排。
	ControlManager ControlManager

	// ConfigLoader 在 control lock 内加载有效配置。生产由 CLI 层用
	// runtimecfg.LoadEffectiveConfig 构造（复用默认路径/TOML 解析/配置校验，不在此处复制）。
	// Apply 锁内编排用它取得 *config.Config 喂给 ControlSession.Inspect/Stop/StartWithExecutable。
	// 未注入时即使 ControlManager 已注入也不做锁内编排（缺配置无法与 control.Session 交互）。
	ConfigLoader control.ConfigLoader

	// Installer 平台专属安装器（集成点：实际文件替换由后续任务填充）。
	// Apply 在锁内 Stop 之后、StartWithExecutable 之前调用 Installer.Install。
	// 未注入时锁内编排执行「占位安装」（不替换文件），仍会按原先运行状态决定是否
	// 用 binPath 重新启动 daemon——便于先打通锁内编排与回滚骨架。
	Installer Installer

	// VersionProbe 是生产 Apply 路径必注入的「stage --version」探针：在来源校验通过后，
	// 对 stage 文件运行 --version 并要求单行版本与目标 tag 一致，作为对发布工作流错误
	// 的额外防线。不替代 SHA256 provenance。nil 仅保留给隔离测试或未完成装配的嵌入方，
	// 此时 Service 会跳过该额外检查；默认 CLI 工厂绝不走此路径。
	VersionProbe VersionProbe

	// AssetDownloader 在来源校验通过后下载目标 Release 资产到 stage 文件（与 target 同目录，
	// 保证同卷 rename）。生产用 NewDownloader 返回的 *downloader，与 ProvenanceDeps.Manifest
	// 复用同一对象；测试可注入 httptest.Server 驱动的真实 downloader 或内存 fake。
	// 未注入时 Apply 保持向后兼容：只到 ReadyToInstall，不下载（stagePath 为空，
	// 由注入的 Installer 自行处理或测试注入 fake stagePath）。
	AssetDownloader AssetDownloader

	// LogSink 是升级步骤日志的写入目标（注入，nil=静默）。生产由 CLI 工厂打开
	// update-YYYY-MM-DD.log 注入；测试注入 buffer 可断言行内容。Apply 各关键步骤
	// 经 stepLogger 输出 [update] 行到此处。
	LogSink io.Writer

	// LogPath 是升级日志文件的完整路径（由 CLI 工厂注入为唯一来源，工厂打开文件时已知路径）。
	// Apply 将其原样放入 ApplyResult.LogPath 供 CLI 提示用户日志位置；retainUpdateLogs
	// 据其目录清理过期日志。
	LogPath string

	// downloaderInvoked 由 Apply 在「准备下载」分支置位，仅供测试断言「不可信时不下载」。
	// 生产代码不读该字段。
	downloaderInvoked bool

	// 以下两个字段仅供测试装配时记录当前二进制路径与内容，便于断言；生产代码不读。
	binPathForTest    string
	binContentForTest []byte
}

// downloaderUsed 是测试辅助：报告 Apply 是否进入了「准备下载」分支。
func (s *Service) downloaderUsed() bool { return s.downloaderInvoked }

// VersionProbe 抽象「对 stage 文件运行 --version 取单行版本」。
// 生产实现用 exec.CommandContext 启动 `<stagePath> --version` 并严格解析单行输出；
// 测试注入 fake 返回固定版本或错误。该检查是 SHA256 provenance 的补充防线，
// 不参与来源是否可信的判定，仅影响是否进入「准备安装」状态。
type VersionProbe interface {
	// ProbeVersion 运行 stagePath 的 --version 并返回规范化的版本字面量
	//（应等于目标 Release tag，如 v0.2.0）。错误表示无法执行或无法解析。
	ProbeVersion(ctx context.Context, stagePath string) (string, error)
}

// Installer 抽象「平台专属的二进制落地替换」步骤。
//
// 生产实现按 GOOS 分派：POSIX 做事务性备份+原子 rename（见 install_unix.go）；
// Windows 经 helper 等待父进程退出后 MoveFileEx（后续任务填充）。
// 本接口是 Apply 锁内编排与实际文件替换之间的集成点：Apply 在 control lock 内、
// Stop 之后、StartWithExecutable 之前调用 Install。
//
// Install 把已验证的 stage 文件（DownloadAsset 下载并校验过 SHA256 的新版本二进制）
// 事务性地落地到 targetBinPath 位置（覆盖当前二进制）。stagePath 是已验证 stage 文件的
// 绝对路径，oldBinPath 是当前二进制路径（供回滚诊断，通常等于 targetBinPath），
// targetBinPath 是被覆盖的目标路径。
//
// Install 返回的 newBinPath 是替换后应启动的新二进制绝对路径（通常等于 targetBinPath，
// 但允许平台实现返回实际落地的路径，如 Windows helper 路径）。
// 失败时调用方据此回滚：用 oldBinPath（当前二进制路径，即 Provenance 校验得到的 BinaryPath）
// 重新启动 daemon，保持替换前运行状态。
type Installer interface {
	// Install 在 control lock 持有期内把已验证的 stage 文件事务性替换到 targetBinPath。
	// stagePath 是 DownloadAsset 产出并校验过 SHA256 的新版本二进制绝对路径；
	// oldBinPath 是当前二进制路径（供回滚诊断）；targetBinPath 是被覆盖的目标路径。
	// wasRunning 是替换前 daemon 运行态，写入 journal 供中断恢复时按原运行态重启 daemon。
	// 成功返回实际落地的新二进制路径。任一失败须保证 target 处于可恢复状态（旧版本或已回滚）。
	Install(ctx context.Context, stagePath, oldBinPath, targetBinPath string, wasRunning bool) (newBinPath string, err error)

	// Platform 返回当前安装器对应的 GOOS（"darwin"/"linux"/"windows"）。
	Platform() string
}

// CheckOptions 是 Service.Check 的入参。
type CheckOptions struct {
	// TargetTag 目标 Release tag；空字符串表示查询 latest 稳定版。
	TargetTag string
	// Force 允许 dev 当前版本通过判定（dev 无版本语义，可被任何合法目标替换，
	// 不做版本序比较）。它只由 Apply 按 ApplyOptions.Force 内部传入；
	// --check 命令分支恒为 false 且 --check --force 组合已在 CLI 层被拒绝。
	Force bool
}

// CheckResult 是 Service.Check 的判定结果，携带供 CLI 展示的全部信息。
// Check 不创建任何本地文件，不下载，不做来源校验。
type CheckResult struct {
	// CurrentTag 当前安装版本 tag（解析成功时填充）。
	CurrentTag string
	// TargetTag 目标 Release tag（查询成功时填充）。
	TargetTag string
	// UpdateAvailable 目标是否严格高于当前。
	UpdateAvailable bool
	// NoStableRelease latest 端点无稳定 Release（ErrNoStableRelease）。
	NoStableRelease bool
	// VersionNotFound 显式 tag 不存在（ErrVersionNotFound）。
	VersionNotFound bool
}

// ApplyOptions 是 Service.Apply 的入参。
type ApplyOptions struct {
	// TargetTag 目标 Release tag；空字符串表示查询 latest 稳定版。
	TargetTag string
	// Force 允许在来源具备豁免资格（hash 失配 / dev 本地构建，见
	// ProvenanceExemption.ForceEligible）时跳过来源信任要求继续安装：
	// 结构前置与目标资产 SHA256 校验全部照常执行，软链 / 非官方 tag 等
	// 白名单外来源不可豁免。
	Force bool
}

// ApplyResult 是 Service.Apply 的执行结果，携带供 CLI 展示的状态字段。
type ApplyResult struct {
	// 内嵌 CheckResult，复用当前/目标版本与更新判定。
	CheckResult
	// ProvenanceChecked 是否执行了来源校验（仅在确有更新时为 true）。
	ProvenanceChecked bool
	// ProvenanceTrusted 来源是否可信。
	ProvenanceTrusted bool
	// ProvenanceForced 本次在 --force 下跳过来源信任要求继续安装。
	// 仅在 force 生效（来源具备豁免资格且申请携带 Force）时置位；
	// ProvenanceTrusted 保持 false，不谎报 trusted。
	ProvenanceForced bool
	// ForceEligible 当前来源具备 force 豁免资格（ProvenanceExemption.ForceEligible）。
	// 供渲染层区分「可 force 而未 force」与「不可 force」两类拒绝文案。
	ForceEligible bool
	// Reason 不可信或拒绝安装的原因（Trusted=true 且 ReadyToInstall=true 时为空）。
	Reason string
	// BinaryPath 当前可执行文件路径（来源校验执行时填充）。
	BinaryPath string
	// TargetAsset 目标平台二进制资产名（确有更新时填充）。
	TargetAsset string
	// StageVersion stage --version 探针返回的版本（执行了探针时填充）。
	StageVersion string
	// ReadyToInstall 是否到达「来源已验证、准备下载安装」状态。
	// true 表示后续下载/安装可继续；false 表示被来源或 stage 探针拒绝。
	ReadyToInstall bool
	// Installed 是否已完成安装替换。
	Installed bool
	// Deferred 表示 Windows 后台 helper 已接管替换与 daemon 恢复。此时命令可
	// 成功返回，但替换尚未完成；调用方应提示用户稍后确认最终版本。
	Deferred bool
	// Recovered 表示本次未开始新的下载或替换，而是已安全处理上次中断留下的
	// 本地事务。RecoveryState 说明最终落在新版本还是恢复到旧版本。
	Recovered bool
	// RecoveryState 仅在 Recovered=true 时有效。
	RecoveryState RecoveryState
	// LogPath 升级日志文件路径（注入 LogSink 时填充），供 CLI 提示用户日志位置。
	LogPath string
}

// Check 只做判定第 1/2 步：解析当前版本 + 查询目标 Release + 比较。
// 不创建任何本地文件，不做来源校验，不下载。
//
// 当前版本为 dev 或非正式 tag 时返回 error（非法当前版本，无法判定更新）；
// opts.Force=true 时 dev 放行——dev 无版本语义，查询到合法目标即视为可更新。
// 目标 Release 查询返回 ErrNoStableRelease / ErrVersionNotFound 时翻译为结果标记，不返回 error；
// 其它查询错误（含瞬时网络错误）原样透传。
func (s *Service) Check(ctx context.Context, opts CheckOptions) (CheckResult, error) {
	if err := s.validateForCheck(); err != nil {
		return CheckResult{}, err
	}
	// 第 1 步：解析当前版本。dev / 非正式 tag 直接拒绝（force 的 dev 除外）。
	current, err := s.parseCurrent(opts.Force)
	if err != nil {
		return CheckResult{}, err
	}

	// 第 2 步：查询目标 Release。
	target, ferr := s.ReleaseClient.FetchRelease(ctx, opts.TargetTag)
	if ferr != nil {
		// 领域结果翻译为标记，不返回 error。
		result := CheckResult{CurrentTag: current.String()}
		if errors.Is(ferr, ErrNoStableRelease) {
			result.NoStableRelease = true
			return result, nil
		}
		if errors.Is(ferr, ErrVersionNotFound) {
			result.VersionNotFound = true
			return result, nil
		}
		// 瞬时错误透传。
		return CheckResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to fetch target release", "查询目标 Release 失败"), ferr)
	}

	// dev + force：dev 无版本语义，可被任何合法目标替换，不做版本序比较。
	if s.CurrentVersion == "dev" {
		return CheckResult{
			CurrentTag:      "dev",
			TargetTag:       target.Tag,
			UpdateAvailable: true,
		}, nil
	}

	// 比较：目标严格高于当前 → updateAvailable。
	cmp := target.Version.Compare(current)
	return CheckResult{
		CurrentTag:      current.String(),
		TargetTag:       target.Tag,
		UpdateAvailable: cmp > 0,
	}, nil
}

// Apply 执行恢复、完整判定与来源校验编排：
//  1. 若有遗留 journal，先恢复已存在的本地事务，不接受新来源；
//  2. Check：解析当前版本 + 查询目标 Release + 比较；
//  3. VerifyProvenance（提前到 Check 后，无论 UpdateAvailable）：为 consume/sweep 提供可信门——
//     可信时在 control lock 内消费上次 helper result + POSIX sweep；不可信/网络失败则跳过
//     （consume 是 best-effort，不阻塞用户）；
//  4. 若无更新 → 直接返回（可信时已消费了上次 result）；
//  5. 确有更新且 provenance 失败 → 返回 error（编程错误不可被 force 豁免）；
//  6. 不可信且未携带 force，或来源不具备豁免资格 → 返回人工安装指引（绝不下载）；
//     force 且来源具备豁免资格（hash 失配 / dev 本地构建）→ 置 ProvenanceForced 继续
//     安装（ProvenanceTrusted 保持 false，consume/sweep 仍不执行，目标资产校验照常）；
//  7. 可信或 force 生效 → VersionProbe（默认 CLI 工厂必注入）：版本不一致 → 拒绝安装；
//  8. 全部通过 → ReadyToInstall=true；
//  9. 若注入 ControlManager + ConfigLoader：进入锁内编排
//     Inspect →（运行中）Stop → Install →（原先运行）StartWithExecutable，
//     任何步骤失败均回滚至替换前运行状态（用旧二进制重启）。
//
// 来源不可信时绝不下载、绝不覆盖、绝不创建目标文件。
// 锁内编排只调用 ControlSession 的 Inspect/Stop/StartWithExecutable——
// 禁止调用 control.Manager.Start/Stop/Restart（它们各自 WithLock 会二次加锁死锁）。
func (s *Service) Apply(ctx context.Context, opts ApplyOptions) (ApplyResult, error) {
	if s == nil {
		return ApplyResult{}, errors.New(ui.Bi("Service must not be nil", "Service 不能为空"))
	}
	// 在入口初始化带 LogPath 的 result 与 stepLogger，使所有失败路径（包括早期
	// recoverPendingJournal / Check / provenance error）都能返回带日志路径的结果，
	// 并在返回前写入 [update] 失败日志，保证失败可诊断。
	result := ApplyResult{LogPath: s.LogPath}
	ul := newStepLogger(s.LogSink, "update", nil)

	// 先处理已存在的遗留事务。它只使用 journal 中受限的同目录路径和已记录的
	// hash 恢复本地一致性，不下载或执行新的来源；因此不能被当前进程的版本比较
	// 或来源验证挡住。没有 journal 时该路径只做本地只读探测，不获取 control lock。
	if outcome, handled, err := s.recoverPendingJournal(ctx); err != nil {
		ul.step("recovery error: %v", err)
		return result, err
	} else if handled {
		result.Recovered = outcome.Recovered
		result.RecoveryState = outcome.RecoveryState
		return result, nil
	}

	// 先做 Check（含当前版本解析与目标查询）。Force 由 Apply 内部传入：
	// dev + force 不在 Check 处拒绝；--check 命令分支不传 force 且组合已被 CLI 拒绝。
	checked, err := s.Check(ctx, CheckOptions{TargetTag: opts.TargetTag, Force: opts.Force})
	if err != nil {
		ul.step("check error: %v", err)
		return result, err
	}
	result.CheckResult = checked

	// 来源校验提前到 Check 后（无论 UpdateAvailable），为 consume/sweep 提供可信门。
	// 这样"升级成功后再次 update"（已是最新）也能消费上次的 result。
	// 无更新 + provenance 网络失败或不可信 → 跳过 consume，仍返回"已是最新"。
	prov, perr := VerifyProvenance(ctx, s.ProvenanceDeps, s.CurrentVersion, s.ReleaseClient, ProvenanceOptions{Force: opts.Force})
	if perr == nil && prov.Trusted {
		ul.step("provenance verified: trusted")
		// 可信来源：在 control lock 内消费上次 helper result + POSIX sweep（best-effort）。
		// 不可信来源不被新增的 consume/sweep 触碰（现有 recoverPendingJournal 属既有机制，
		// 在此前已执行）。
		s.consumeAndSweep(ctx, prov.BinaryPath, ul)
		// 日志保留仅在来源可信后执行——不可信或 provenance 出错时不触碰旧日志。
		if s.LogSink != nil && s.LogPath != "" {
			retainUpdateLogs(filepath.Dir(s.LogPath), updateLogRetentionDays, time.Now())
		}
	} else if perr == nil {
		ul.step("provenance untrusted: %s", prov.Reason)
	}

	// 无更新：直接返回。若来源可信，已在上面消费了上次 result。
	// provenance 查询失败在无更新时不阻塞用户返回"已是最新"，但记录为 best-effort failure，
	// 便于诊断 result 未被消费或清理被跳过的原因（有更新路径在下方按致命错误单独记录）。
	if !checked.UpdateAvailable {
		if perr != nil {
			ul.step("provenance check failed (best-effort): %v", perr)
		}
		return result, nil
	}

	// 确有更新：provenance 失败（nil deps 等编程错误）返回 error。
	if perr != nil {
		ul.step("provenance error: %v", perr)
		return result, perr
	}

	// 解析目标平台资产名（用于结果展示与后续下载）。
	goos, goarch := s.platform()
	assetName, ok := AssetName(goos, goarch)
	result.TargetAsset = assetName
	if !ok {
		// 平台不受支持：无法给出目标资产，视为不可信来源（保守拒绝）。
		result.ProvenanceChecked = true
		result.Reason = ui.Bi(
			fmt.Sprintf("no official asset for target platform %s/%s; please install manually", goos, goarch),
			fmt.Sprintf("目标平台 %s/%s 无官方资产，请手动安装", goos, goarch),
		)
		ul.step("platform unsupported: %s/%s", goos, goarch)
		return result, nil
	}

	result.ProvenanceChecked = true
	result.ProvenanceTrusted = prov.Trusted
	result.BinaryPath = prov.BinaryPath
	result.Reason = prov.Reason
	result.ForceEligible = prov.Exemption.ForceEligible()

	// 不可信：仅当申请携带 force 且来源具备豁免资格（显式白名单）才继续安装；
	// 白名单外来源（symlink、非官方 tag、清单缺失等）force 不可救，
	// 只返回目标版本与人工安装指引，绝不下载。
	if !prov.Trusted {
		if !opts.Force || !result.ForceEligible {
			return result, nil
		}
		// force 生效：按豁免类别记录日志（dev 无 hash 比较事实，不得误报 hash 语义），
		// ProvenanceTrusted 保持 false，不谎报 trusted。consume/sweep 仍不执行。
		if prov.Exemption == ExemptionDevBuild {
			ul.step("provenance source unverifiable: dev build (forced by --force); continuing install")
		} else {
			ul.step("provenance hash mismatch (forced by --force); continuing install")
		}
		result.ProvenanceForced = true
	}

	// 可信：标记「准备下载」。
	s.downloaderInvoked = true
	ul.step("started: %s → %s", checked.CurrentTag, checked.TargetTag)

	// 下载目标资产到 stage 文件（若注入 AssetDownloader）。
	// expectedHash 取自目标 Release 的 SHA256SUMS（ManifestFetcher 是 tag 参数化的，
	// 目标版本同样适用）。未注入 AssetDownloader 时保持向后兼容：stagePath 为空，
	// 仅依赖后续注入的 Installer 自行处理（生产 POSIX installer 要求非空；测试可注入 fake）。
	stagePath, derr := s.downloadStage(ctx, checked.TargetTag, result.TargetAsset, result.BinaryPath)
	if derr != nil {
		// 下载或清单查询失败：保守拒绝安装，写明原因。ReadyToInstall 保持 false。
		result.Reason = ui.Bi(
			fmt.Sprintf("failed to download target asset, please install manually: %v", derr),
			fmt.Sprintf("下载目标资产失败，请手动安装: %v", derr),
		)
		ul.step("download failed: %v", derr)
		return result, nil
	}
	if stagePath != "" {
		ul.step("stage downloaded: %s", filepath.Base(stagePath))
	}

	// 默认 CLI 工厂注入的 stage --version 探针：用真实 stage 路径运行 --version，作为
	// 发布工作流错误的额外防线。未注入仅发生在隔离测试或未完成装配的嵌入方。
	if s.VersionProbe != nil {
		stageVer, verr := s.VersionProbe.ProbeVersion(ctx, stagePath)
		result.StageVersion = stageVer
		if verr != nil {
			// 探针失败即拒绝安装：已下载的 stage 不再有用，best-effort 删除，
			// 避免失败路径在目标目录残留大文件（实测 Windows 失败后残留 ~24MB）。
			_ = os.Remove(stagePath)
			result.Reason = ui.Bi(
				fmt.Sprintf("stage version probe failed: %v; please install manually", verr),
				fmt.Sprintf("stage 版本探针失败: %v；请手动安装", verr),
			)
			ul.step("stage probe failed: %v", verr)
			return result, nil
		}
		if stageVer != checked.TargetTag {
			_ = os.Remove(stagePath)
			result.Reason = ui.Bi(
				fmt.Sprintf("stage version %q does not match target tag %q; refusing to install, please install manually", stageVer, checked.TargetTag),
				fmt.Sprintf("stage 版本 %q 与目标 tag %q 不一致，拒绝安装；请手动安装", stageVer, checked.TargetTag),
			)
			ul.step("stage version mismatch: %s != %s", stageVer, checked.TargetTag)
			return result, nil
		}
		ul.step("stage version probe: %s", stageVer)
	}

	// 全部通过：到达「准备安装」状态。
	result.ReadyToInstall = true

	// 锁内编排集成点：注入 ControlManager + ConfigLoader 后，在 control lock 内完成
	// Inspect → Stop → Install → StartWithExecutable，并据替换前运行状态回滚。
	// 未注入任一依赖时只到 ReadyToInstall=true，不做锁内操作（保持向后兼容，便于分阶段接入）。
	if s.ControlManager != nil && s.ConfigLoader != nil {
		outcome, ierr := s.installUnderLockOutcome(ctx, stagePath, result.BinaryPath)
		// Install 已把外部 stage 复制为内部 nonce 副本（POSIX copyStageWithMode /
		// Windows copyFileWithMode），此后外部 stagePath 冗余——成功/失败路径都 best-effort 删除。
		// Deferred 时 helper 用内部副本 paths.Stage，不引用外部 stagePath，删除亦安全。
		if stagePath != "" {
			_ = removeRegularFile(stagePath)
		}
		if ierr != nil {
			// 锁内编排失败：保留 ReadyToInstall=true（来源校验已通过），返回错误供上层处理。
			// Installed 与 Deferred 均保持 false。
			ul.step("install failed: %v", ierr)
			return result, ierr
		}
		result.Installed = outcome.Installed
		result.Deferred = outcome.Deferred
		result.Recovered = outcome.Recovered
		result.RecoveryState = outcome.RecoveryState
		if result.Installed {
			ul.step("installed: %s", checked.TargetTag)
		} else if result.Deferred {
			ul.step("deferred to background helper")
		}
	} else if stagePath != "" {
		// 未注入 control（向后兼容路径）：无 install 消费 stage，best-effort 清理。
		_ = removeRegularFile(stagePath)
	}
	return result, nil
}

// downloadStage 在可信分支下载目标资产到 stage 文件，返回 stage 绝对路径。
// expectedHash 取自目标 Release 的 SHA256SUMS（经 ManifestFetcher 按 targetTag 拉取）。
// 未注入 AssetDownloader 时返回 ("", nil)，保持向后兼容（不下载，stagePath 为空，
// 由注入的 Installer 自行处理或测试注入 fake stagePath）。
// 清单查询或下载失败返回 error，调用方据此拒绝安装（ReadyToInstall=false）。
func (s *Service) downloadStage(ctx context.Context, targetTag, assetName, binPath string) (string, error) {
	if s.AssetDownloader == nil {
		return "", nil
	}
	if s.ProvenanceDeps.Manifest == nil {
		return "", errors.New(ui.Bi("no official manifest fetcher configured; cannot get the expected hash of the target asset", "未配置官方清单获取方式，无法取得目标资产预期 hash"))
	}
	targetManifest, merr := s.ProvenanceDeps.Manifest.FetchManifest(ctx, targetTag)
	if merr != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("failed to fetch manifest for target version %s", targetTag),
			fmt.Sprintf("获取目标版本 %s 清单失败", targetTag),
		), merr)
	}
	if targetManifest == nil {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("manifest for target version %s is empty", targetTag),
			fmt.Sprintf("目标版本 %s 清单为空", targetTag),
		))
	}
	expectedHash, ok := targetManifest.HashFor(assetName)
	if !ok {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("manifest for target version %s is missing the hash of asset %s", targetTag, assetName),
			fmt.Sprintf("目标版本 %s 清单缺少资产 %s 的 hash", targetTag, assetName),
		))
	}
	// stage 落在 target 同目录，保证后续 rename 同卷原子。
	targetDir := filepath.Dir(binPath)
	stagePath, derr := s.AssetDownloader.DownloadAsset(ctx, targetTag, assetName, expectedHash, targetDir, "")
	if derr != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("failed to download asset %s", assetName),
			fmt.Sprintf("下载资产 %s 失败", assetName),
		), derr)
	}
	return stagePath, nil
}

// recoverPendingJournal 在新一轮版本检查和来源验证之前，处理当前二进制同目录中
// 已存在的遗留 POSIX journal。恢复不引入任何新来源：journal 的路径均由当前
// executable basename 和 nonce 推导，RecoverJournal 还会重新比对已记录的 hash。
//
// 无 journal 时不加载配置、不获取 control lock，保持常规 update 的副作用边界。
// 当前路径不安全或无法确认时则交由后续 provenance 安全门给出常规人工安装指引。
func (s *Service) recoverPendingJournal(ctx context.Context) (installOutcome, bool, error) {
	if s == nil || s.ControlManager == nil || s.ConfigLoader == nil || s.Installer == nil {
		return installOutcome{}, false, nil
	}
	if _, ok := s.Installer.(JournalRecoverer); !ok {
		return installOutcome{}, false, nil
	}

	target, ok := s.recoveryTargetPath()
	if !ok {
		return installOutcome{}, false, nil
	}
	_, found, err := findLeftoverJournal(target)
	if err != nil {
		return installOutcome{}, false, fmt.Errorf("%s: %w", ui.Bi("failed to check leftover journal", "检查遗留 journal 失败"), err)
	}
	if !found {
		return installOutcome{}, false, nil
	}
	return s.recoverJournalUnderLock(ctx, target)
}

// recoveryTargetPath 返回可用于恢复的当前 executable 路径。状态 3 恢复时 target
// 已不存在是预期情况，仍允许继续；symlink、目录或其他无法确认的路径不进入恢复路径。
func (s *Service) recoveryTargetPath() (string, bool) {
	deps := s.ProvenanceDeps
	if deps.Executable == nil || deps.Lstat == nil {
		return "", false
	}
	target, err := deps.Executable.Executable()
	if err != nil || !filepath.IsAbs(target) {
		return "", false
	}
	info, err := deps.Lstat.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return target, true
		}
		return "", false
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false
	}
	return target, true
}

// recoverJournalUnderLock 把已确认存在的 journal 放到 control lock 内处理，确保
// daemon 恢复和正常安装路径不会并发交错。
func (s *Service) recoverJournalUnderLock(ctx context.Context, oldBinPath string) (installOutcome, bool, error) {
	cfg, err := s.ConfigLoader()
	if err != nil {
		return installOutcome{}, false, fmt.Errorf("%s: %w", ui.Bi("failed to load effective config under lock", "锁内加载有效配置失败"), err)
	}
	if cfg == nil {
		return installOutcome{}, false, errors.New(ui.Bi("loading effective config under lock returned nil", "锁内加载有效配置返回 nil"))
	}

	var recovered installOutcome
	var handled bool
	if err := s.ControlManager.WithLock(ctx, func(sess ControlSession) error {
		var rerr error
		recovered, handled, rerr = s.recoverJournalWithSession(ctx, sess, cfg, oldBinPath)
		return rerr
	}); err != nil {
		return installOutcome{}, false, err
	}
	return recovered, handled, nil
}

// recoverJournalWithSession 处理同一 control lock 中的 journal 恢复逻辑。handled
// 为 false 表示没有待恢复事务，或 OldIntact 且 daemon 原本未运行，调用方可继续本轮安装。
func (s *Service) recoverJournalWithSession(ctx context.Context, sess ControlSession, cfg *config.Config, oldBinPath string) (installOutcome, bool, error) {
	recoverer, ok := s.Installer.(JournalRecoverer)
	if !ok {
		return installOutcome{}, false, nil
	}

	journalOutcome, cleanupErr := recoverer.RecoverJournal(oldBinPath)
	if cleanupErr != nil && journalOutcome.State != RecoveryStateCleanupPending {
		return installOutcome{}, false, fmt.Errorf("%s: %w", ui.Bi("failed to handle leftover journal", "处理遗留 journal 失败"), cleanupErr)
	}

	switch journalOutcome.State {
	case RecoveryStateManual:
		return installOutcome{}, false, errors.New(ui.Bi(
			"leftover journal state unrecognized; files kept for manual handling",
			"遗留 journal 状态无法识别，保留文件要求人工处理",
		))
	case RecoveryStateNewInstalled, RecoveryStateOldRestored, RecoveryStateCleanupPending, RecoveryStateOldIntact:
		// 状态 2 且原 daemon 未运行时，旧 target 完好，可继续本轮 Install。
		if journalOutcome.State == RecoveryStateOldIntact && !journalOutcome.RestartDaemon {
			return installOutcome{}, false, nil
		}

		// daemon 的处置必须依据 journal 的原运行态，而非此刻的 Inspect：中断可能发生在
		// Stop 后、Start 前，此时 Inspect 会把原先运行的 daemon 误判为未运行。
		restartDaemon := journalOutcome.RestartDaemon ||
			journalOutcome.State == RecoveryStateNewInstalled ||
			journalOutcome.State == RecoveryStateOldRestored
		if restartDaemon && journalOutcome.WasRunning {
			if journalOutcome.NewBinPath == "" {
				return installOutcome{}, false, errors.New(ui.Bi(
					"leftover journal is missing the daemon recovery target path",
					"遗留 journal 缺少 daemon 恢复目标路径",
				))
			}
			if startErr := sess.StartWithExecutable(ctx, cfg, journalOutcome.NewBinPath); startErr != nil {
				if cleanupErr != nil {
					return installOutcome{}, false, errors.Join(
						fmt.Errorf("%s: %w", ui.Bi("failed to restart daemon per original running state after recovering the leftover transaction", "恢复遗留事务后按原运行态重启 daemon 失败"), startErr),
						fmt.Errorf("%s: %w", ui.Bi("leftover transaction file cleanup pending", "遗留事务文件清理待处理"), cleanupErr),
					)
				}
				return installOutcome{}, false, fmt.Errorf("%s: %w", ui.Bi("failed to restart daemon per original running state after recovering the leftover transaction", "恢复遗留事务后按原运行态重启 daemon 失败"), startErr)
			}
		}
		if cleanupErr != nil {
			return installOutcome{}, false, fmt.Errorf("%s: %w", ui.Bi("leftover transaction recovered, but cleanup is pending", "遗留事务已恢复，但清理待处理"), cleanupErr)
		}
		return installOutcome{Recovered: true, RecoveryState: journalOutcome.State}, true, nil
	case RecoveryStateClean:
		return installOutcome{}, false, nil
	default:
		return installOutcome{}, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover journal returned unknown state %q; files kept for manual handling", journalOutcome.State),
			fmt.Sprintf("遗留 journal 返回未知状态 %q，保留文件要求人工处理", journalOutcome.State),
		))
	}
}

// installUnderLock 保留给直接测试与旧调用方，返回是否已同步完成安装。
// 需要区分 Windows 后台替换时，调用 installUnderLockOutcome。
func (s *Service) installUnderLock(ctx context.Context, stagePath, oldBinPath string) (bool, error) {
	outcome, err := s.installUnderLockOutcome(ctx, stagePath, oldBinPath)
	return outcome.Installed, err
}

type installOutcome struct {
	Installed     bool
	Deferred      bool
	Recovered     bool
	RecoveryState RecoveryState
}

// installUnderLockOutcome 在 control lock 内完成 daemon 切换与（占位）安装编排。
//
// 编排顺序（全部在 ControlManager.WithLock 的同一个回调内，不二次加锁）：
//  1. ConfigLoader 加载有效配置；
//  2. （若 Installer 实现 JournalRecoverer）检测并处理上次中断遗留的 journal：
//     按 3 种可恢复状态恢复，模糊状态返回 error 不继续；命中 NewInstalled、OldRestored，
//     或 journal 记录曾运行的 OldIntact 时，按 journal 记录的原运行态重启 daemon
//     （不能依赖 Inspect——Stop 已执行后 Inspect 报 not-running，会丢失原运行态），
//     随后本轮结束，不再尝试新的 Install；
//  3. ControlSession.Inspect 判定替换前 daemon 是否运行（决定是否需要 Stop / 之后是否 Start）；
//  4. 若运行中：ControlSession.Stop 停掉 daemon，等 daemon lock 释放；
//  5. Installer.Install 做实际文件替换（集成点；未注入时占位：不替换，newBinPath=oldBinPath），
//     wasRunning 传入以便写入 journal 供中断恢复；
//  6. 若替换前运行：ControlSession.StartWithExecutable(newBinPath) 启动新 daemon。
//     install/Start 失败时尽力用 oldBinPath 回滚重启，保持替换前运行状态；主失败与
//     restart/rollback 失败用 errors.Join 聚合保留。
//
// oldBinPath 是当前二进制路径（Provenance.BinaryPath），也是被覆盖的目标位置。
// stagePath 是 DownloadAsset 产出并校验过 SHA256 的新版本二进制路径；
// 未注入 AssetDownloader 时 stagePath 为空（向后兼容），此时 Install 依赖注入的
// Installer 自行处理（生产 POSIX installer 要求非空 stagePath；测试可注入 fake stagePath
// 指向真实临时文件以驱动事务）。
func (s *Service) installUnderLockOutcome(ctx context.Context, stagePath, oldBinPath string) (installOutcome, error) {
	cfg, err := s.ConfigLoader()
	if err != nil {
		return installOutcome{}, fmt.Errorf("%s: %w", ui.Bi("failed to load effective config under lock", "锁内加载有效配置失败"), err)
	}
	if cfg == nil {
		return installOutcome{}, errors.New(ui.Bi("loading effective config under lock returned nil", "锁内加载有效配置返回 nil"))
	}

	var installErr error
	var newBinPath string
	var wasRunning bool
	var deferred bool // Windows helper 已接管后续替换与 daemon 切换，本轮 installed=false
	var recovery installOutcome

	lockErr := s.ControlManager.WithLock(ctx, func(sess ControlSession) error {
		// 0. 检测并处理上次中断遗留的 journal。正常 Apply 开始前已经做过一次
		// 无副作用探测；这里保留同锁检查，处理探测和持锁安装之间出现的 journal。
		// 若命中，必须先恢复并结束本轮安装。stagePath 是调用方提供的已验证输入，
		// 恢复逻辑只删除其 nonce 派生的事务文件，不能在这里删除外部 stage。
		if outcome, handled, rerr := s.recoverJournalWithSession(ctx, sess, cfg, oldBinPath); rerr != nil {
			return rerr
		} else if handled {
			recovery = outcome
			return nil
		}

		// 1. Inspect 判定替换前运行状态。
		st, ierr := sess.Inspect(ctx, cfg)
		if ierr != nil {
			return fmt.Errorf("%s: %w", ui.Bi("Inspect failed under lock", "锁内 Inspect 失败"), ierr)
		}
		wasRunning = st.Running

		// 2. 运行中先 Stop（等 daemon lock 释放），为文件替换腾出干净状态。
		if wasRunning {
			if serr := sess.Stop(ctx, cfg); serr != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to stop daemon before replacement", "替换前停止 daemon 失败"), serr)
			}
		}

		// 3. 实际文件替换（集成点）。未注入 Installer 时占位：不替换文件，
		// newBinPath 沿用 oldBinPath，保证 StartWithExecutable 仍有合法目标路径。
		// Install 成功后事务文件（backup/journal）暂不删除——若 Installer 实现
		// TransactionHandler，由步骤 4 在 daemon Start 成功后 Commit、失败时 Rollback。
		// wasRunning 传入 Install 以便写入 journal，供中断恢复时按原运行态重启 daemon。
		if s.Installer != nil {
			nb, ierr := s.Installer.Install(ctx, stagePath, oldBinPath, oldBinPath, wasRunning)
			if ierr != nil {
				// Windows staged replacement：Install 已构造 plan、复制 helper.exe 并 spawn
				// 后台 helper，文件替换与 daemon 切换由 helper 在父进程退出后完成。
				// 立即结束锁内编排（installed=false），跳过 Start/Commit/Rollback——
				// 这些全部由 helper 负责。POSIX 的 Install 永不返回该 sentinel。
				if errors.Is(ierr, ErrDeferredToHelper) {
					newBinPath = nb
					deferred = true
					return nil
				}
				installErr = ierr
			} else {
				newBinPath = nb
			}
		} else {
			newBinPath = oldBinPath
		}
		if installErr != nil {
			// 安装失败：若替换前 daemon 在运行，用旧二进制重启恢复运行态。
			// POSIX Install 失败时 target 已是旧版本（或已内部 rollback），故 oldBinPath 是旧版本。
			// restart 失败与主失败用 errors.Join 聚合保留，供上层诊断。
			var restartErr error
			if wasRunning {
				restartErr = sess.StartWithExecutable(ctx, cfg, oldBinPath)
			}
			if restartErr != nil {
				return errors.Join(
					fmt.Errorf("%s: %w", ui.Bi("failed to install new version", "安装新版本失败"), installErr),
					fmt.Errorf("%s: %w", ui.Bi("rollback restart with the old binary also failed", "回滚重启旧二进制也失败"), restartErr),
				)
			}
			if wasRunning {
				return fmt.Errorf("%s: %w", ui.Bi("failed to install new version (restarted with the old binary)", "安装新版本失败（已用旧二进制重启）"), installErr)
			}
			return fmt.Errorf("%s: %w", ui.Bi("failed to install new version", "安装新版本失败"), installErr)
		}

		// 4. 替换前运行 → 用新二进制重启 daemon，完成「热切换」。
		// Start 成功后调 Commit 清理事务文件；Start 失败调 Rollback 恢复旧版本再重启。
		if wasRunning {
			if serr := sess.StartWithExecutable(ctx, cfg, newBinPath); serr != nil {
				// 启动失败：若 Installer 支持 TransactionHandler，先 Rollback（恢复旧版本）。
				var rollbackErr error
				if th, ok := s.Installer.(TransactionHandler); ok && th != nil {
					rollbackErr = th.Rollback()
				}
				// Rollback 后 target 已是旧版本（或 POSIX 中 Install 已 rollback），
				// 用 oldBinPath（= target）重启恢复旧版本运行。
				if rerr := sess.StartWithExecutable(ctx, cfg, oldBinPath); rerr != nil {
					if rollbackErr != nil {
						return errors.Join(
							fmt.Errorf("%s: %w", ui.Bi("failed to start new binary", "新二进制启动失败"), serr),
							fmt.Errorf("%s: %w", ui.Bi("rollback restore failed", "回滚恢复失败"), rollbackErr),
							fmt.Errorf("%s: %w", ui.Bi("rollback restart with the old binary also failed", "回滚重启旧二进制也失败"), rerr),
						)
					}
					return errors.Join(
						fmt.Errorf("%s: %w", ui.Bi("failed to start new binary", "新二进制启动失败"), serr),
						fmt.Errorf("%s: %w", ui.Bi("rollback restart with the old binary also failed", "回滚重启旧二进制也失败"), rerr),
					)
				}
				if rollbackErr != nil {
					return errors.Join(
						fmt.Errorf("%s: %w", ui.Bi("failed to start new binary", "新二进制启动失败"), serr),
						fmt.Errorf("%s: %w", ui.Bi("rollback restore failed", "回滚恢复失败"), rollbackErr),
					)
				}
				return fmt.Errorf("%s: %w", ui.Bi("failed to start new binary; rolled back and restarted with the old binary", "新二进制启动失败，已用旧二进制回滚重启"), serr)
			}
		}
		// daemon Start 成功（或无需启动）：提交事务（清理 backup/journal）。
		// Commit 失败不回滚已成功的新版本，返回「清理待处理」可诊断错误。
		if th, ok := s.Installer.(TransactionHandler); ok && th != nil {
			if cerr := th.Commit(); cerr != nil {
				return fmt.Errorf("%s: %w", ui.Bi("update completed, cleanup pending", "更新完成，清理待处理"), cerr)
			}
		}
		return nil
	})

	if lockErr != nil {
		return installOutcome{}, lockErr
	}
	if recovery.Recovered {
		return recovery, nil
	}
	// deferred=true 表示 Windows helper 已接管替换（Install 返回 sentinel），installed=false。
	// 否则表示执行了完整 Install 流程，installed=true。
	return installOutcome{Installed: !deferred, Deferred: deferred}, nil
}

// parseCurrent 解析当前版本。dev / 非正式 tag 返回 error；force=true 时 dev 放行
// （返回零值 Version，由调用方按 CurrentVersion=="dev" 走专用分支）。
// dev 非 force 的错误文本携带 --force 出口：这是 dev 用户唯一能看到 force 提示的
// 落点（该分支在渲染分流之前就中断），提示用完整命令避免与 --check 组合歧义。
func (s *Service) parseCurrent(force bool) (Version, error) {
	if s.CurrentVersion == "dev" {
		if force {
			return Version{}, nil
		}
		return Version{}, errors.New(ui.Bi(
			"current version is dev (local build); run `token-usage update --force` to replace it with an official release asset, or install manually",
			"当前版本为 dev（本地构建）；运行 `token-usage update --force` 可切换为官方 Release 资产，或手动安装正式版",
		))
	}
	ver, err := ParseVersion(s.CurrentVersion)
	if err != nil {
		return Version{}, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("current version %q is not an official release tag; cannot determine updates", s.CurrentVersion),
			fmt.Sprintf("当前版本 %q 非正式 Release tag，无法判定更新", s.CurrentVersion),
		), err)
	}
	return ver, nil
}

// validateForCheck 校验 Check 所需的最小依赖。
func (s *Service) validateForCheck() error {
	if s == nil {
		return errors.New(ui.Bi("Service must not be nil", "Service 不能为空"))
	}
	if s.ReleaseClient == nil {
		return errors.New(ui.Bi("Service.ReleaseClient must not be nil", "Service.ReleaseClient 不能为空"))
	}
	return nil
}

// platform 返回 (goos, goarch)，未注入时回退 runtime 值。
func (s *Service) platform() (string, string) {
	goos, goarch := s.Goos, s.Goarch
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// consumeAndSweep 在可信来源确认后，消费上次 helper result 并清理 POSIX nonce 事务残留。
// ControlManager != nil 时在 control lock 内执行（与 installUnderLockOutcome 的 WithLock
// 顺序独立、非嵌套）；== nil 时直接执行。consume 经原子文件操作、POSIX sweep 无后台 helper
// 竞态，无锁亦安全。best-effort：锁获取失败或清理失败不阻塞升级。
//
// 平台边界：consume 双平台执行（仅 Windows 产生 result，POSIX 无命中自动 no-op）；
// sweep 仅 POSIX（s.platform() goos != windows，Windows 放弃跨事务 sweep）。
func (s *Service) consumeAndSweep(ctx context.Context, target string, ul *stepLogger) {
	run := func() {
		consumePendingResult(target, ul)
		goos, _ := s.platform()
		if goos != "windows" {
			if err := SweepStaleTempFiles(target); err != nil {
				ul.step("sweep error: %v", err)
			}
		}
	}
	if s.ControlManager != nil {
		if err := s.ControlManager.WithLock(ctx, func(sess ControlSession) error {
			run()
			return nil
		}); err != nil {
			ul.step("consume/sweep lock error: %v", err)
		}
	} else {
		run()
	}
}

// CloseLogSink 关闭升级日志 sink（若它实现了 io.Closer）。供 CLI 命令在返回前 defer 调用。
// LogSink 未注入或不是 closer 时为 no-op。
func (s *Service) CloseLogSink() {
	if c, ok := s.LogSink.(io.Closer); ok {
		_ = c.Close()
	}
}
