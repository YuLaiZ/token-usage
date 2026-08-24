// internal/configapp/apply.go
// Package configapp 的 ApplyConfig 编排：在 control lock 内原子应用用户配置。
//
// 锁内应用顺序：
//  1. WithLock 获取 control lock。
//  2. 清理 .token-usage/ 下 .config.toml.tmp-* 残留 temp。
//  3. 重新读 raw 算 revision；不存在用 sentinel；与 expectedRevision 不匹配 → ErrConfigChangedExternally。
//  4. 加载 previous、分别 ResolveEffectiveConfig(previous/current)。
//  5. ValidateUserConfig(currentUser)。
//  6. Session.Inspect 读运行状态。
//  7. MarshalUserConfig(currentUser) 一次 → writeBytes；与 previousRaw 相等 → Saved=false 不写；
//     否则 ReplaceCompleteFile，NewRevision=Revision(writeBytes)。
//  8. service.SyncWith：ErrPlatformUnsupported 不致命；其他错误进 PartialErrors；不回滚 config。
//  9. data_dir 变化：按 previous effective oldDataDir Inspect.Running；运行中即使 confirm 也拒绝；
//     已停 → CleanupStaleMetadata(oldDataDir)；不移动 DB。
//  10. AnalyzeConfigEffects 生成 effects + SuggestedSteps + ExplanatoryNotes + SuccessMessage；释放锁。
package configapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// ErrConfigChangedExternally 表示 expectedRevision 与重新读取的磁盘 revision 不一致，
// 本次未写入。调用方应据此提示用户「配置已被其他进程修改，请重读后再试」。
var ErrConfigChangedExternally = errors.New(ui.Bi(
	"config was modified by another process; nothing was written this time",
	"配置已被其他进程修改，本次未写入",
))

// errDataDirMigrationNotConfirmed data_dir 变化但未传确认参数。
var errDataDirMigrationNotConfirmed = errors.New(ui.Bi(
	"data_dir change requires explicit migration confirmation (pass confirmDataDirMigration=true)",
	"data_dir 变化需显式确认迁移（传 confirmDataDirMigration=true）",
))

// errDataDirMigrationRunning data_dir 变化但旧 daemon 仍在运行。
var errDataDirMigrationRunning = errors.New(ui.Bi(
	"data_dir changed but the old daemon is still running; run token-usage stop first",
	"data_dir 变化但旧 daemon 仍在运行，请先 token-usage stop",
))

// missingFileSentinel 是「配置文件不存在」的固定 revision（区别于空文件的 sha256）。
// 文件不存在与空文件语义不同：前者是合法的「首次写入」，后者是「损坏」。
// 用一个固定字符串的 sha256，保证与空文件 hash 永远不同。
var missingFileSentinel = sha256Sum([]byte("token-usage:missing-config-file-sentinel"))

// controlPort 是 configapp 内部对 control.Manager 锁相关能力的抽象。
// 生产实现 managerAdapter 包装 *control.Manager：WithLock 捕获 *control.Session，
// Inspect/CleanupStaleMetadata 委托给锁内 Session。
// 测试用 fakeControlPort 直接实现，无真实锁/进程。
//
// ApplyConfig 是单 goroutine 顺序执行，adapter 在 WithLock 回调内写入 capturedSession，
// 同一 control lock 持有期内 Inspect/CleanupStaleMetadata 复用该 Session，安全无竞争。
type controlPort interface {
	// WithLock 获取 control lock 后执行 fn（锁内），fn 返回后释放。
	// fn 不接收 Session——Inspect/CleanupStaleMetadata 通过接口方法调用，复用同一锁内 Session。
	WithLock(ctx context.Context, fn func() error) error
	// Inspect 在锁内读取运行状态（不加 control lock，只读 daemon lock 快照）。
	Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error)
	// CleanupStaleMetadata 在锁内清理旧 data_dir 的 stale PID/runtime-state。
	CleanupStaleMetadata(ctx context.Context, dataDir string) error
}

// managerAdapter 把 *control.Manager 适配到 controlPort（生产）。
// capturedSession 在 WithLock 回调内被设置，供 Inspect/CleanupStaleMetadata 复用。
// ApplyConfig 单 goroutine 顺序调用，无并发写入 capturedSession。
type managerAdapter struct {
	mu              sync.Mutex
	mgr             *control.Manager
	capturedSession *control.Session
}

func (a *managerAdapter) WithLock(ctx context.Context, fn func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mgr.WithLock(ctx, func(s *control.Session) error {
		a.capturedSession = s
		defer func() {
			a.capturedSession = nil
		}()
		return fn()
	})
}

func (a *managerAdapter) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	if a.capturedSession == nil {
		return control.RuntimeState{}, errors.New(ui.Bi("Inspect must be called while holding the control lock", "Inspect 必须在 control lock 内调用"))
	}
	return a.capturedSession.Inspect(ctx, cfg)
}

func (a *managerAdapter) CleanupStaleMetadata(ctx context.Context, dataDir string) error {
	if a.capturedSession == nil {
		return errors.New(ui.Bi("CleanupStaleMetadata must be called while holding the control lock", "CleanupStaleMetadata 必须在 control lock 内调用"))
	}
	return a.capturedSession.CleanupStaleMetadata(ctx, dataDir)
}

// 编译期保证 managerAdapter 实现 controlPort。
var _ controlPort = (*managerAdapter)(nil)

// AutoStartOutcome 描述一次 ApplyConfig 中自启定义同步的结果。
type AutoStartOutcome struct {
	Requested     bool // currentUser.Daemon.AutoStart 目标态
	DefinitionWas bool // 同步前定义是否存在（仅平台支持时可靠）
	DefinitionNow bool // 同步后定义是否存在
	DriftRepaired bool // 本次是否修复了漂移（triggered 且未出错）
	Err           error
}

// NeedsRetry 只对真实的定义同步失败返回 true。平台不支持是已知能力边界，
// 配置意图已经保存，重复保存不会改变结果，不能进入 TUI 的重试状态。
func (o AutoStartOutcome) NeedsRetry() bool {
	return o.Err != nil && !errors.Is(o.Err, service.ErrPlatformUnsupported)
}

// ApplyConfigResult 是 ApplyConfig 的结构化返回。
type ApplyConfigResult struct {
	Changed          bool
	Saved            bool
	ConfigApplied    bool
	NewRevision      []byte
	DaemonState      control.RuntimeState
	Effects          ConfigEffects
	AutoStart        AutoStartOutcome
	SuccessMessage   string
	SuggestedSteps   []string
	ExplanatoryNotes []string
	PartialErrors    []error
}

// Application 是 ApplyConfig 的编排器（configapp 的核心类型）。
type Application struct {
	home       string
	resolveEnv runtimecfg.ResolveEnv
	ctrl       controlPort
	autoStart  service.AutoStartManager
}

// NewApplication 创建生产用 Application。
// 校验：manager/autoStart 非 nil；home == env.Home 且 manager.ConfigHome() == filepath.Join(home, ".token-usage")，
// 阻止 config path、resolver 与 control lock 使用不同 home（签名不变，内部转 adapter）。
func NewApplication(
	home string,
	env runtimecfg.ResolveEnv,
	manager *control.Manager,
	autoStart service.AutoStartManager,
) (*Application, error) {
	if manager == nil {
		return nil, errors.New(ui.Bi("control manager must not be nil", "control manager 不能为 nil"))
	}
	if got := manager.ConfigHome(); got != filepath.Join(home, ".token-usage") {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("manager.ConfigHome() (%q) does not match filepath.Join(home,\".token-usage\") (%q)", got, filepath.Join(home, ".token-usage")),
			fmt.Sprintf("manager.ConfigHome() (%q) 与 filepath.Join(home,\".token-usage\") (%q) 不一致", got, filepath.Join(home, ".token-usage")),
		))
	}
	return newApplicationWithDeps(home, env, &managerAdapter{mgr: manager}, autoStart)
}

// newApplicationWithDeps 用可注入的 controlPort 构造 Application（白盒测试用）。
// home/env/ctrl/autoStart 一致性在此统一校验。
func newApplicationWithDeps(
	home string,
	env runtimecfg.ResolveEnv,
	ctrl controlPort,
	autoStart service.AutoStartManager,
) (*Application, error) {
	if ctrl == nil {
		return nil, errors.New(ui.Bi("controlPort must not be nil", "controlPort 不能为 nil"))
	}
	if autoStart == nil {
		return nil, errors.New(ui.Bi("autoStart manager must not be nil", "autoStart manager 不能为 nil"))
	}
	if home == "" {
		return nil, errors.New(ui.Bi("home must not be empty", "home 不能为空"))
	}
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("home must be an absolute path, got %q", home),
			fmt.Sprintf("home 必须是绝对路径，当前 %q", home),
		))
	}
	if env.Home != home {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("env.Home (%q) does not match home (%q)", env.Home, home),
			fmt.Sprintf("env.Home (%q) 与 home (%q) 不一致", env.Home, home),
		))
	}
	if env.GOOS == "" {
		return nil, errors.New(ui.Bi("ResolveEnv.GOOS must not be empty", "ResolveEnv.GOOS 不能为空"))
	}
	if env.DefaultPaths == nil {
		return nil, errors.New(ui.Bi("ResolveEnv.DefaultPaths must not be nil", "ResolveEnv.DefaultPaths 不能为 nil"))
	}
	// 生产路径：manager.ConfigHome() == filepath.Join(home,".token-usage")。
	// controlPort 接口不暴露 ConfigHome（fake 无此概念），故只在 NewApplication（生产入口）校验，
	// 此处通过 home 一致性 + DefaultPaths 非 nil 保证 resolver 边界。
	return &Application{
		home:       home,
		resolveEnv: env,
		ctrl:       ctrl,
		autoStart:  autoStart,
	}, nil
}

// Revision 计算原始完整 bytes 的 SHA-256。
// 不使用 mtime（不可靠）；文件不存在用固定 sentinel（见 ApplyConfig）。
func Revision(raw []byte) []byte {
	return sha256Sum(raw)
}

// sha256Sum 返回 SHA-256 的切片副本（长度 32）。
func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// ApplyConfig 在 control lock 内原子应用 currentUser 配置。
//
// 详细语义见文件头注释。ConfigApplied 表示「返回时磁盘 config 与 currentUser
// 对应」：no-op 或写入成功为 true；写入前失败为 false；写入后部分失败仍为 true（含准确
// NewRevision + 非空 PartialErrors + errors.Join 非 nil error）。
func (a *Application) ApplyConfig(
	ctx context.Context,
	expectedRevision []byte,
	currentUser *config.Config,
	confirmDataDirMigration bool,
) (ApplyConfigResult, error) {
	var result ApplyConfigResult
	var partialErrs []error

	lockErr := a.ctrl.WithLock(ctx, func() error {
		// ---- 步骤 2：清理 .config.toml.tmp-* temp（锁内，避免与并发写竞争）----
		configHome := filepath.Join(a.home, ".token-usage")
		if err := fileutil.CleanupKnownTempFiles(configHome, []string{fileutil.TempPrefix(runtimecfg.ConfigPath(a.home))}); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to clean up config temp files", "清理 config temp 失败"), err)
		}

		// ---- 步骤 3：重新读 raw 算 revision，校验 expectedRevision ----
		configPath := runtimecfg.ConfigPath(a.home)
		snap, err := runtimecfg.LoadUserConfigSnapshot(configPath)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to read config file", "读取配置文件失败"), err)
		}
		var diskRevision []byte
		var previousRaw []byte
		var previous *config.Config
		if !snap.Exists {
			// 文件不存在：用固定 sentinel（区别空文件 hash）。
			diskRevision = missingFileSentinel
			previous = nil
		} else {
			// 文件存在（含空文件）：revision 基于实际 raw bytes。
			// 注意：空文件/损坏文件 Exists=true 但 Config 可能为 nil，revision 仍基于 raw。
			diskRevision = Revision(snap.Raw)
			previousRaw = snap.Raw
			previous = snap.Config
		}
		if !bytes.Equal(diskRevision, expectedRevision) {
			return ErrConfigChangedExternally
		}

		// ---- 步骤 4：校验 current，再 ResolveEffectiveConfig(previous/current) ----
		if err := runtimecfg.ValidateUserConfigForWrite(currentUser); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("config validation failed", "配置校验失败"), err)
		}
		// previous 为 nil（首次写入）时，以合法空用户配置解析默认 effective。
		previousForResolve := previous
		if previousForResolve == nil {
			previousForResolve = &config.Config{}
		}
		prevEff, err := runtimecfg.ResolveEffectiveConfig(previousForResolve, a.resolveEnv)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to resolve previous effective config", "解析 previous effective config 失败"), err)
		}
		currEff, err := runtimecfg.ResolveEffectiveConfig(currentUser, a.resolveEnv)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to resolve current effective config", "解析 current effective config 失败"), err)
		}
		pathWarnings := changedResolvedPathWarnings(prevEff, currEff)

		// ---- 步骤 5：data_dir 迁移前置条件 ----
		// data_dir 迁移前置条件（写入前校验，避免「写了 config 但迁移被拒」的中间态）：
		// 仅当存在 previous 配置且 previous 有实际 data_dir 时才视为迁移。
		dataDirChanged := previous != nil && prevEff.DataDir != "" && prevEff.DataDir != currEff.DataDir
		if dataDirChanged {
			if !confirmDataDirMigration {
				return errDataDirMigrationNotConfirmed
			}
			// 按 previous effective oldDataDir Inspect.Running；运行中即使确认也拒绝（写入前）。
			oldState, inspErr := a.ctrl.Inspect(ctx, prevEff)
			if inspErr != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to check old daemon state", "检查旧 daemon 状态失败"), inspErr)
			}
			if oldState.Running {
				return errDataDirMigrationRunning
			}
		}

		// ---- 步骤 6：Session.Inspect 读运行状态（当前 effective config）----
		daemonState, err := a.ctrl.Inspect(ctx, currEff)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to read daemon state", "读取 daemon 状态失败"), err)
		}
		result.DaemonState = daemonState

		// ---- 步骤 7：MarshalUserConfig 一次 → writeBytes；与 previousRaw 比较；写入 ----
		writeBytes, err := config.MarshalUserConfig(currentUser)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to marshal config", "序列化配置失败"), err)
		}

		rawUnchanged := previous != nil && bytes.Equal(writeBytes, previousRaw)
		if rawUnchanged {
			// 无 raw 变化：Saved=false，NewRevision 仍为磁盘 revision。
			result.Saved = false
			result.NewRevision = diskRevision
		} else {
			// 写入磁盘（完整替换）。
			if err := fileutil.ReplaceCompleteFile(configPath, writeBytes, 0o644); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to write config file", "写入配置文件失败"), err)
			}
			result.Saved = true
			result.NewRevision = Revision(writeBytes)
		}

		// ConfigApplied 在此刻为 true：磁盘已确认与 currentUser 对应（写入或 no-op 一致）。
		result.ConfigApplied = true

		// ---- 步骤 8：service.SyncWith（不回滚 config）----
		// 先读同步前 definition 状态（仅平台支持时可靠）。
		autoStartRequested := currEff.Daemon.AutoStart
		outcome := AutoStartOutcome{Requested: autoStartRequested}
		syncReport, syncErr := service.SyncWithReport(currEff, a.autoStart)
		outcome.Err = syncErr
		outcome.DefinitionWas = syncReport.Before.Exists
		outcome.DefinitionNow = syncReport.After.Exists
		outcome.DriftRepaired = syncReport.DriftRepaired && syncErr == nil
		if syncErr != nil {
			if !errors.Is(syncErr, service.ErrPlatformUnsupported) {
				// 真实同步失败 → PartialErrors（不回滚）。
				partialErrs = append(partialErrs, fmt.Errorf("%s: %w", ui.Bi("failed to sync autostart definition", "同步自启定义失败"), syncErr))
			}
			// ErrPlatformUnsupported → 非致命，进 ExplanatoryNotes（步骤10）。
		}
		result.AutoStart = outcome

		// ---- 步骤 9：data_dir 迁移清理（前置条件已在步骤5校验通过）----
		if dataDirChanged {
			// 已停 → CleanupStaleMetadata(oldDataDir)（不移动 DB）。
			if err := a.ctrl.CleanupStaleMetadata(ctx, prevEff.DataDir); err != nil {
				partialErrs = append(partialErrs, fmt.Errorf("%s: %w", ui.Bi("failed to clean up stale metadata in the old data_dir", "清理旧 data_dir stale metadata 失败"), err))
			}
		}

		// ---- 步骤 10：AnalyzeConfigEffects + SuggestedSteps + ExplanatoryNotes ----
		effects := AnalyzeConfigEffects(prevEff, currEff)
		// 首次写入（文件原本不存在）：无「previous data_dir」可迁移，抑制 DataDirMigration。
		if previous == nil {
			effects.DataDirMigration = nil
			// 同样抑制由空 previous 产生的 data_dir 迁移 warning。
			effects.Warnings = filterWarning(effects.Warnings, warningDataDirManualMigration)
		}
		effects.Warnings = mergeWarnings(effects.Warnings, pathWarnings)
		result.Effects = effects

		result.SuggestedSteps, result.ExplanatoryNotes = a.buildActionsAndNotes(
			effects, daemonState.Running, currEff.Daemon.AutoStart, currentUser, autoStartRequested, outcome,
			rawUnchanged, prevEff, currEff,
		)
		result.SuccessMessage, result.Changed = a.buildSuccessMessage(effects, rawUnchanged, prevEff, currEff)

		return nil
	})

	// lock 失败（含 ErrConfigChangedExternally、timeout、validation、写入前失败）：ConfigApplied 保持 false。
	if lockErr != nil {
		// ErrConfigChangedExternally 是预期的「不写盘」分支，ConfigApplied=false，返回零值结果。
		result.PartialErrors = partialErrs
		return result, lockErr
	}

	// 成功路径：汇总 PartialErrors。
	result.PartialErrors = partialErrs
	if len(partialErrs) > 0 {
		return result, errors.Join(partialErrs...)
	}
	return result, nil
}

// buildActionsAndNotes 根据 effects + daemon 运行态 + 自启结果生成 SuggestedSteps 与 ExplanatoryNotes。
func (a *Application) buildActionsAndNotes(
	effects ConfigEffects,
	daemonRunning bool,
	currentAutoStart bool,
	currentUser *config.Config,
	requested bool,
	outcome AutoStartOutcome,
	rawUnchanged bool,
	prevEff, currEff *config.Config,
) ([]string, []string) {
	var steps []string
	var notes []string

	// ---- 动作建议合并 ----
	hasCollect := len(effects.FullCollectClients) > 0 || len(effects.RouterBackfillClients) > 0

	if daemonRunning {
		if hasCollect {
			// stop → 全部 collect → start。
			steps = append(steps, "token-usage stop")
		} else if effects.RuntimeChanged {
			// 仅运行时配置变化（poll/log 等）→ restart。
			steps = append(steps, "token-usage restart")
		}
	}
	// 收集动作（稳定顺序：full 先，再未被 full 去重的 router；已排序）。
	for _, c := range effects.FullCollectClients {
		steps = append(steps, fmt.Sprintf("token-usage collect all --client %s", c))
	}
	for _, c := range effects.RouterBackfillClients {
		steps = append(steps, fmt.Sprintf("token-usage collect router --client %s", c))
	}
	if daemonRunning && hasCollect {
		steps = append(steps, "token-usage start")
	} else if !daemonRunning && effects.DataDirMigration != nil {
		// data_dir 迁移要求旧 daemon 已停止；用户完成持久数据搬运后需要显式启动，
		// 因此把完整命令作为最后一步返回，而不是只在说明文字中隐含提及。
		steps = append(steps, "token-usage start")
	}

	// ---- raw 变化但 effective 相同（纯写法规范化）→ 明确说明，不生成 restart/collect ----
	if !rawUnchanged && effectiveEqual(prevEff, currEff) {
		notes = append(notes, ui.Bi("effective config unchanged (only formatting was normalized)", "有效配置未变化（仅写法变化已规范化）"))
	}

	// ---- 自启结构化说明 ----
	a.appendAutoStartNotes(&notes, requested, outcome, daemonRunning, currentAutoStart)

	// ---- Effects warning 复用为 ExplanatoryNotes ----
	for _, w := range effects.Warnings {
		notes = append(notes, w)
	}

	// ---- data_dir 迁移说明 ----
	if effects.DataDirMigration != nil {
		notes = append(notes, ui.Bi(
			fmt.Sprintf("data_dir moved from %q to %q: %s must be migrated manually; old PID/lock/runtime-state cleaned up, the database is not moved automatically",
				effects.DataDirMigration.From, effects.DataDirMigration.To,
				strings.Join(effects.DataDirMigration.Items, ", ")),
			fmt.Sprintf("data_dir 从 %q 迁移到 %q：需手工搬运 %s；旧 PID/lock/runtime-state 已清理，不自动移动数据库",
				effects.DataDirMigration.From, effects.DataDirMigration.To,
				strings.Join(effects.DataDirMigration.Items, "、")),
		))
	}

	// ---- collect 末尾 start 提示（daemon 未运行但有采集）----
	if !daemonRunning && hasCollect {
		if currentAutoStart {
			notes = append(notes, ui.Bi(
				"run token-usage start after collection; autostart is enabled and takes effect at next login, but it will not start implicitly this time",
				"采集后可执行 token-usage start 启动；自启已开启，下次登录会自动启动，但本次不会隐式启动",
			))
		} else {
			notes = append(notes, ui.Bi(
				"run token-usage start after collection to launch the daemon",
				"采集后可执行 token-usage start 启动守护进程",
			))
		}
	}

	return steps, notes
}

type resolvedPath struct {
	label string
	value string
}

// changedResolvedPathWarnings 检查新增或修改后的有效路径。路径暂时不存在或不可访问
// 只产生提示，不阻止保存，允许用户先写配置、后安装对应客户端或创建目录。
func changedResolvedPathWarnings(previous, current *config.Config) []string {
	if previous == nil || current == nil {
		return nil
	}

	var changed []resolvedPath
	add := func(label, before, after string) {
		if before != after && strings.TrimSpace(after) != "" {
			changed = append(changed, resolvedPath{label: label, value: after})
		}
	}

	add("data_dir", previous.DataDir, current.DataDir)
	add("log.dir", previous.Log.Dir, current.Log.Dir)
	for name, client := range current.Clients {
		previousClient := previous.Clients[name]
		for key, path := range client.Paths {
			add(fmt.Sprintf("clients.%s.paths.%s", name, key), previousClient.Paths[key], path)
		}
	}
	for name, router := range current.Routers {
		add(fmt.Sprintf("routers.%s.db_path", name), previous.Routers[name].DBPath, router.DBPath)
	}

	sort.Slice(changed, func(i, j int) bool {
		if changed[i].label == changed[j].label {
			return changed[i].value < changed[j].value
		}
		return changed[i].label < changed[j].label
	})

	warnings := make([]string, 0, len(changed))
	for _, path := range changed {
		if _, err := os.Stat(path.value); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				warnings = append(warnings, ui.Bi(
					fmt.Sprintf("changed path %s=%q does not exist yet; config saved, create the path or install the corresponding client before use", path.label, path.value),
					fmt.Sprintf("变更后的路径 %s=%q 当前不存在；配置已保存，请在使用前创建路径或安装对应客户端", path.label, path.value),
				))
			} else {
				warnings = append(warnings, ui.Bi(
					fmt.Sprintf("changed path %s=%q is not accessible (%v); config saved, please verify before use", path.label, path.value, err),
					fmt.Sprintf("变更后的路径 %s=%q 当前无法访问（%v）；配置已保存，请在使用前确认", path.label, path.value, err),
				))
			}
		}
	}
	return warnings
}

func mergeWarnings(groups ...[]string) []string {
	unique := make(map[string]struct{})
	for _, group := range groups {
		for _, warning := range group {
			unique[warning] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return nil
	}
	merged := make([]string, 0, len(unique))
	for warning := range unique {
		merged = append(merged, warning)
	}
	sort.Strings(merged)
	return merged
}

// appendAutoStartNotes 根据自启同步结果生成对应 ExplanatoryNotes。
func (a *Application) appendAutoStartNotes(
	notes *[]string,
	requested bool,
	outcome AutoStartOutcome,
	daemonRunning bool,
	currentAutoStart bool,
) {
	switch {
	case outcome.Err != nil && errors.Is(outcome.Err, service.ErrPlatformUnsupported):
		// 平台不支持：配置意图已保存，不伪装已安装，不作为可重试同步失败。
		if requested {
			*notes = append(*notes, ui.Bi(
				"autostart definitions are not supported on this platform; the config intent is saved but the definition cannot be installed",
				"当前平台不支持开机自启定义；配置意图已保存，但无法安装自启定义",
			))
		} else {
			*notes = append(*notes, ui.Bi(
				"autostart definitions are not supported on this platform; sync skipped",
				"当前平台不支持开机自启定义，已跳过同步",
			))
		}
	case outcome.Err != nil:
		// 真实同步失败：配置已保存，但不声称 status 会自动修复。
		*notes = append(*notes, ui.Bi(
			fmt.Sprintf("config saved, but autostart definition sync failed: %v (please check the autostart definition manually)", outcome.Err),
			fmt.Sprintf("配置已保存，但自启定义同步失败: %v（请手动检查自启定义）", outcome.Err),
		))
	case requested && outcome.DefinitionNow:
		if daemonRunning {
			*notes = append(*notes, ui.Bi(
				"autostart definition enabled; the running daemon keeps running",
				"自启定义已启用；当前 daemon 保持运行",
			))
		} else {
			*notes = append(*notes, ui.Bi(
				"autostart definition enabled, taking effect at next login/boot; not running now, run token-usage start if you need it now",
				"自启定义已启用，下次登录/开机生效；当前未运行，如需现在运行可执行 token-usage start",
			))
		}
		if outcome.DriftRepaired {
			*notes = append(*notes, ui.Bi(
				"autostart definition repaired (drift detected and re-converged)",
				"自启定义已修复（检测到漂移并重新收敛）",
			))
		}
	case !requested:
		*notes = append(*notes, ui.Bi(
			"autostart definition disabled, no longer started at next login/boot; current daemon state unchanged",
			"自启定义已关闭，下次登录/开机不再启动；当前 daemon 状态不变",
		))
	default:
		// requested=true 但 DefinitionNow=false（Enable 失败已被 outcome.Err 捕获）。
	}
	_ = currentAutoStart
}

// buildSuccessMessage 生成 SuccessMessage 与 Changed 标志。
func (a *Application) buildSuccessMessage(
	effects ConfigEffects,
	rawUnchanged bool,
	prevEff, currEff *config.Config,
) (string, bool) {
	_ = effects
	effectiveChanged := !effectiveEqual(prevEff, currEff)
	if rawUnchanged {
		if !effectiveChanged {
			return ui.Bi("effective config unchanged", "有效配置未变化"), false
		}
		return ui.Bi("config saved", "配置已保存"), true
	}
	if !effectiveChanged {
		// raw 变化但 effective 相同。
		return ui.Bi("effective config unchanged (only formatting was normalized)", "有效配置未变化（仅写法变化已规范化）"), true
	}
	return ui.Bi("config saved", "配置已保存"), true
}

// effectiveEqual 比较两份 effective config 的全部字段。
// AnalyzeConfigEffects 有意忽略纯 autostart 变化，不能用于完整等价判断。
func effectiveEqual(prev, curr *config.Config) bool {
	return reflect.DeepEqual(normalize(prev), normalize(curr))
}

// filterWarning 从 warnings 移除等于 needle 的项，返回新切片。
func filterWarning(warnings []string, needle string) []string {
	out := warnings[:0:0]
	for _, w := range warnings {
		if w != needle {
			out = append(out, w)
		}
	}
	return out
}
