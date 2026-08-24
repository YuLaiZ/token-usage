package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/update"
)

// update.go 实现用户可见的 `token-usage update` 子命令。
//
// 命令只做参数解析与编排：把 --check/--version 翻译为对 update.Service 的调用，
// 并据 Service 返回的 CheckResult/ApplyResult 在 stdout/stderr 展示面向用户的结果。
// 所有外部依赖（GitHub Release 查询、来源校验、安装替换、进程控制）都封装在
// 经工厂注入的 UpdateService 之后，命令本身不直接触网、不读写目标二进制。
//
// 路径分派：
//   - --check：只读判定（Service.Check）。工厂以 checkOnly=true 调用，构造不含
//     control.Manager 的服务，绝不创建 ~/.token-usage 配置目录；
//   - 默认（无 --check）：执行完整更新（Service.Apply）。工厂构造含 control.Manager
//     + ConfigLoader + Installer 的服务，Apply 在来源校验通过后于 control lock 内
//     完成「替换二进制 + 按原运行态重启 daemon」。
//
// --version 在任何工厂/网络调用之前由 update.ParseVersion 严格校验（v 前缀、数字段、
// 可选 rc.N），非法值立即返回清晰错误并写 stderr，杜绝非法输入触发网络请求。

// UpdateService 是 update 命令对 update.Service 的窄依赖，仅暴露 Check 与 Apply 两个方法。
// 用接口而非 *update.Service，使 CLI 测试可注入 stub 覆盖所有结果合同，
// 不触及真实网络或文件系统；生产实现由默认 updateServiceFactory 装配真实 *update.Service。
type UpdateService interface {
	Check(ctx context.Context, opts update.CheckOptions) (update.CheckResult, error)
	Apply(ctx context.Context, opts update.ApplyOptions) (update.ApplyResult, error)
}

var (
	errRequestedUpdateVersionMissing = errors.New("指定的更新版本不存在")
	errUpdateSourceUntrusted         = errors.New("当前来源无法安全覆盖")
	errUpdateVerificationFailed      = errors.New("自动更新校验未通过")
	errUpdateIncomplete              = errors.New("自动更新未完成")
)

// updateServiceFactory 装配 update 命令所需的 UpdateService。
//
// checkOnly=true 时构造「只读判定」服务：注入 ReleaseClient + ProvenanceDeps，
// 但显式不注入 control.Manager（nil），保证 --check 路径永不获取 control lock、
// 永不创建 ~/.token-usage 配置目录。checkOnly=false 时构造「完整更新」服务，
// 在 ProvenanceDeps 之外再注入 control.Manager + ConfigLoader + Installer，
// 使 Apply 在来源校验通过后能在 control lock 内完成二进制替换与 daemon 切换。
//
// 测试覆盖以注入 stub，避免访问真实网络/HOME/文件系统。
var updateServiceFactory = defaultUpdateServiceFactory

// defaultUpdateServiceFactory 用生产适配器装配真实 *update.Service 并适配为 UpdateService。
// 装配细节镜像 start/stop 等命令的 control 装配方式：
//   - ReleaseClient：内置 HTTPS-only HTTP 客户端的 GitHub Release 客户端；
//   - ProvenanceDeps：os.Executable / os.Lstat / os.ReadFile 与 runtime 平台标识；
//   - ManifestFetcher：复用内置下载器（与 ReleaseClient 共用 HTTPS-only HTTP 客户端）拉取 SHA256SUMS 清单；
//   - control.Manager：os.UserHomeDir → control.NewManager → update.NewControlManager
//     （仅 checkOnly=false 时构造）；
//   - ConfigLoader：runtimecfg.LoadEffectiveConfig 的函数值（仅 checkOnly=false 时注入）；
//   - Installer：非 Windows 平台用 POSIX 事务性安装器；Windows 用 staged replacement
//     安装器（父进程 spawn 后台 helper，父退出后 helper 完成 MoveFileEx 与 daemon 切换）。
//   - VersionProbe：仅 Apply 路径注入，运行已校验 stage 的 --version，作为 manifest
//     SHA256 之外的发布物版本交叉校验。
func defaultUpdateServiceFactory(info buildinfo.Info, checkOnly bool) (UpdateService, error) {
	releaseClient := update.NewGithubReleaseClient(nil)
	// 一个下载器对象两用：既是 ManifestFetcher（来源校验拉取当前版本 SHA256SUMS），
	// 又是 AssetDownloader（可信分支下载目标资产到 stage）。*downloader 同时实现两个接口，
	// 故保留同一对象分别赋给 ProvenanceDeps.Manifest 与 Service.AssetDownloader。
	downloader := update.NewDownloader(nil)

	svc := &update.Service{
		CurrentVersion: info.Version,
		ReleaseClient:  releaseClient,
		ProvenanceDeps: update.ProvenanceDeps{
			Executable: osExecutable{},
			Lstat:      osLstat{},
			FileReader: osFileReader{},
			Manifest:   downloader,
			Goos:       goruntime.GOOS,
			Goarch:     goruntime.GOARCH,
		},
	}

	// 仅 Apply 路径才注入 control 与下载依赖；--check 路径保持 ControlManager/ConfigLoader
	// 与 AssetDownloader 为 nil，保证只读判定不触碰 control lock、不创建配置目录、不下载。
	if !checkOnly {
		mgr, err := buildUpdateControlManager()
		if err != nil {
			return nil, err
		}
		svc.ControlManager = mgr
		svc.ConfigLoader = loadConfig
		svc.Installer = buildUpdateInstaller()
		svc.AssetDownloader = downloader
		svc.VersionProbe = update.NewExecVersionProbe()

		// 打开升级日志文件并注入 LogSink/LogPath + installer 的 writer/logDir。
		// 日志打开失败是 best-effort：Apply 不依赖日志也能工作。
		if home, herr := os.UserHomeDir(); herr == nil {
			logDir := update.ResolveUpdateLogDir(home)
			if f, logPath, oerr := update.OpenUpdateLogFile(logDir, time.Now()); oerr == nil {
				svc.LogSink = f
				svc.LogPath = logPath
				// POSIX 安装器接收 logWriter 输出 [install] 行；Windows 安装器接收 logDir
				// 重定向 helper stderr。经类型断言注入，使工厂无需知道安装器具体类型。
				if s, ok := svc.Installer.(update.StepLogWriter); ok {
					s.SetLogWriter(f)
				}
				if s, ok := svc.Installer.(update.HelperLogDirSetter); ok {
					s.SetLogDir(logDir)
				}
			}
		}
	}
	return svc, nil
}

// osExecutable 把 os.Executable 适配到 update.ExecutableResolver。
type osExecutable struct{}

func (osExecutable) Executable() (string, error) { return os.Executable() }

// osLstat 把 os.Lstat 适配到 update.Lstat。
type osLstat struct{}

func (osLstat) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

// osFileReader 把 os.ReadFile 适配到 update.FileReader。
type osFileReader struct{}

func (osFileReader) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// ---- 平台专属 Installer / control.Manager 装配（分平台编译）----

// buildUpdateControlManager 构造 control.Manager 并适配为 update.ControlManager。
// home 经 os.UserHomeDir 解析（与 start/stop 一致），失败时返回错误而非静默回退。
func buildUpdateControlManager() (update.ControlManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("获取用户主目录失败: %w", err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("创建进程控制管理器失败: %w", err)
	}
	return update.NewControlManager(mgr), nil
}

// newUpdateCmd 构造 update 子命令。info 由 newRootCmd 注入，保证 CurrentVersion 与
// `token-usage version` / `token-usage --version` 共享同一份构建信息快照。
func newUpdateCmd(info buildinfo.Info) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Update token-usage to the latest or a given version / 更新 token-usage 到最新或指定版本",
		SilenceUsage: true,
		Long: "检查并更新 token-usage 自身到最新稳定版或指定版本。\n\n" +
			"  token-usage update            更新到最新稳定版（来源校验通过后替换二进制并恢复 daemon）\n" +
			"  token-usage update --check    只检查是否有新版本，不做任何修改\n" +
			"  token-usage update --version vX.Y.Z   更新到指定版本\n" +
			"  token-usage update --check --version vX.Y.Z-rc.N   只检查指定候选版\n\n" +
			"--version 接受严格 Release tag（vMAJOR.MINOR.PATCH 或 vMAJOR.MINOR.PATCH-rc.N）。\n" +
			"来源不可信（如本地构建、go install）时不自动覆盖，改为输出人工安装指引。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd, info)
		},
	}
	cmd.Flags().Bool("check", false, "只检查是否有可更新版本，不执行更新")
	cmd.Flags().String("version", "", "指定目标版本 tag（如 vX.Y.Z 或 vX.Y.Z-rc.N）")
	return cmd
}

// runUpdate 抽出便于测试：解析 flag → 校验 --version → 装配服务 → 调用 Check/Apply → 展示结果。
func runUpdate(cmd *cobra.Command, info buildinfo.Info) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	checkOnly, _ := cmd.Flags().GetBool("check")
	versionFlag, _ := cmd.Flags().GetString("version")

	// --version 在任何网络/工厂调用之前严格校验，非法值立即清晰报错。
	if versionFlag != "" {
		if _, err := update.ParseVersion(versionFlag); err != nil {
			fmt.Fprintf(errOut, "无效的 --version %q: %v\n", versionFlag, err)
			return fmt.Errorf("无效的 --version %q: %w", versionFlag, err)
		}
	}

	svc, err := updateServiceFactory(info, checkOnly)
	if err != nil {
		fmt.Fprintf(errOut, "装配更新服务失败: %v\n", err)
		return err
	}

	// 升级日志文件在工厂内打开，命令返回前关闭句柄（仅 *update.Service 有此方法）。
	if rs, ok := svc.(*update.Service); ok {
		defer rs.CloseLogSink()
	}

	ctx := cmdContext(cmd)

	if checkOnly {
		return runUpdateCheck(ctx, cmd, svc, versionFlag, out, errOut)
	}
	return runUpdateApply(ctx, cmd, svc, versionFlag, out, errOut)
}

// runUpdateCheck 执行只读判定并展示结果。
// NoStableRelease 是正常结果；指定 tag 不存在则返回错误，避免脚本把拼写错误当作成功。
// 面向用户的提示写 stdout；服务调用错误返回非 0 退出码并写 stderr。
func runUpdateCheck(ctx context.Context, cmd *cobra.Command, svc UpdateService, versionFlag string, out, errOut io.Writer) error {
	res, err := svc.Check(ctx, update.CheckOptions{TargetTag: versionFlag})
	if err != nil {
		fmt.Fprintf(errOut, "检查更新失败: %v\n", err)
		return err
	}
	return renderCheckResult(out, res)
}

// renderCheckResult 把 CheckResult 翻译为面向用户的中文提示。
func renderCheckResult(out io.Writer, res update.CheckResult) error {
	switch {
	case res.NoStableRelease:
		fmt.Fprintln(out, "当前还没有可用的稳定 Release，请稍后再试或访问项目主页确认。")
		return nil
	case res.VersionNotFound:
		fmt.Fprintln(out, "指定的版本不存在，请用有效的 Release tag 重试。")
		return errRequestedUpdateVersionMissing
	case res.UpdateAvailable:
		fmt.Fprintf(out, "发现可更新版本：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "运行 `token-usage update` 执行更新。")
		return nil
	default:
		// UpdateAvailable=false 且无领域标记：已是最新（含 CurrentTag/TargetTag 相等的显式情况）。
		current := res.CurrentTag
		if current == "" {
			current = "（未知）"
		}
		target := res.TargetTag
		if target == "" {
			target = current
		}
		fmt.Fprintf(out, "已是最新版本（%s）\n", target)
		return nil
	}
}

// runUpdateApply 执行完整更新并展示结果。
// NoStableRelease 与无更新是正常结果；指定 tag 不存在、来源不可信、校验拒绝或安装未完成
// 均返回错误，避免自动化调用把未完成的更新当作成功。
func runUpdateApply(ctx context.Context, cmd *cobra.Command, svc UpdateService, versionFlag string, out, errOut io.Writer) error {
	res, err := svc.Apply(ctx, update.ApplyOptions{TargetTag: versionFlag})
	if err != nil {
		fmt.Fprintf(errOut, "更新失败: %v\n", err)
		if res.LogPath != "" {
			fmt.Fprintf(out, "升级日志: %s\n", res.LogPath)
		}
		return err
	}
	renderErr := renderApplyResult(out, errOut, res)
	if res.LogPath != "" {
		fmt.Fprintf(out, "升级日志: %s\n", res.LogPath)
	}
	return renderErr
}

// renderApplyResult 把 ApplyResult 翻译为面向用户的中文提示。
//
// 结果分支：
//   - NoStableRelease：给出明确提示，退出 0；VersionNotFound 返回非 0；
//   - 无更新：提示已是最新；
//   - ProvenanceChecked=true 且 Trusted=false：来源不可信，输出人工安装指引并返回非 0；
//   - Installed=true：POSIX 已同步完成替换与 daemon 恢复，提示已更新；
//   - Deferred=true：Windows helper 已排队，提示用户稍后验证并退出 0；
//   - Recovered=true：上次中断事务已恢复；新版本已落地时退出 0，恢复旧版本时非 0；
//   - ReadyToInstall=true 但未 Installed/Deferred：更新未完成，返回非 0。
func renderApplyResult(out, errOut io.Writer, res update.ApplyResult) error {
	switch {
	case res.Recovered:
		return renderRecoveredApplyResult(out, res)
	case res.NoStableRelease:
		fmt.Fprintln(out, "当前还没有可用的稳定 Release，请稍后再试或访问项目主页确认。")
		return nil
	case res.VersionNotFound:
		fmt.Fprintln(out, "指定的版本不存在，请用有效的 Release tag 重试。")
		return errRequestedUpdateVersionMissing
	case res.ProvenanceChecked && !res.ProvenanceTrusted:
		// 来源不可信：绝不自动覆盖。给出目标版本与人工安装指引。
		fmt.Fprintf(out, "发现可更新版本：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "当前来源无法安全覆盖，请手动安装：")
		printManualInstallGuide(out, res.TargetTag)
		if res.Reason != "" {
			fmt.Fprintf(errOut, "原因：%s\n", res.Reason)
		}
		return errUpdateSourceUntrusted
	case res.Installed:
		fmt.Fprintf(out, "已更新并恢复 daemon：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "可用 `token-usage version` 确认当前版本。")
		return nil
	case res.Deferred:
		fmt.Fprintf(out, "后台替换已排队：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "请稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本。")
		return nil
	case res.ReadyToInstall:
		// 已通过来源校验但没有完成安装，也没有确认 helper 已接管；不能把它归类为成功。
		fmt.Fprintf(out, "已确认可更新：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "自动更新尚未完成，请检查错误信息或手动安装。")
		return errUpdateIncomplete
	case res.UpdateAvailable:
		// 确有更新但未到达 ReadyToInstall（被 stage 探针拒绝等）：保守提示人工安装。
		fmt.Fprintf(out, "发现可更新版本：%s → %s\n", res.CurrentTag, res.TargetTag)
		fmt.Fprintln(out, "本次未能完成自动更新，请手动安装：")
		printManualInstallGuide(out, res.TargetTag)
		if res.Reason != "" {
			fmt.Fprintf(errOut, "原因：%s\n", res.Reason)
		}
		return errUpdateVerificationFailed
	default:
		// 无更新：已是最新。
		target := res.TargetTag
		if target == "" {
			target = res.CurrentTag
		}
		fmt.Fprintf(out, "已是最新版本（%s）\n", target)
		return nil
	}
}

// renderRecoveredApplyResult 输出上次更新中断后的恢复结果。NewInstalled 表示新版本
// 已经落地，只是上次命令未能完成收尾；旧版本恢复则保证系统一致但本轮目标尚未安装。
func renderRecoveredApplyResult(out io.Writer, res update.ApplyResult) error {
	switch res.RecoveryState {
	case update.RecoveryStateNewInstalled:
		fmt.Fprintln(out, "检测到上次更新中断，已恢复完成：新版本已落地，并已按原运行态恢复 daemon。")
		fmt.Fprintln(out, "请运行 token-usage version 确认当前版本。")
		return nil
	case update.RecoveryStateOldIntact, update.RecoveryStateOldRestored:
		fmt.Fprintln(out, "检测到上次更新中断，已恢复到旧版本，并已按原运行态恢复 daemon。")
		fmt.Fprintln(out, "本次未安装目标版本，可重新运行 token-usage update 重试。")
		return errUpdateIncomplete
	default:
		// 当前恢复流程只会将上述三种状态标记为 Recovered。保留非零回退，避免未来
		// 新增状态被误报为成功。
		fmt.Fprintln(out, "遗留更新事务已处理，但无法确定恢复结果；请检查当前版本后重试。")
		return errUpdateIncomplete
	}
}

// printManualInstallGuide 输出面向用户的人工安装指引。
// 仅使用冻结的官方下载前缀与精确 tag + 平台资产名，绝不采用运行时探测到的任意 URL。
func printManualInstallGuide(out io.Writer, targetTag string) {
	if targetTag == "" {
		targetTag = "<version>"
	}
	fmt.Fprintf(out, "  1. 访问 https://github.com/YuLaiZ/token-usage/releases/tag/%s\n", targetTag)
	fmt.Fprintln(out, "  2. 下载对应平台的二进制资产，替换当前 token-usage 可执行文件")
	fmt.Fprintln(out, "  3. 运行 `token-usage version` 确认新版本")
}
