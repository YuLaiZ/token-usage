// internal/cli/daemon.go
package cli

// daemon.go 定义 `token-usage daemon` 命令组：管理采集与分析守护进程的
// 后台生命周期（start/status/stop/restart）。daemon 本身只是命令组，
// 不含 RunE：裸执行只显示命令组帮助，不产生进程、端口或状态文件副作用。
// 守护进程本体由 Hidden 的 _run 承载（经 control.Manager spawn 或
// launchd/注册表自启定义拉起），与仪表板服务（serve 命令组）完全独立。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newDaemonCmd 构造 daemon 命令组：仅注册四个生命周期动作。RunE 显式打印
// 帮助并返回成功——cobra 对不可执行（无 RunE）的命令会在参数校验之前直接
// 打印帮助，`daemon <未知子命令>` 就会静默降级为帮助；显式 RunE 让 NoArgs
// 校验先生效，未知子命令按 unknown command 失败，而裸执行仍只显示帮助，
// 不产生进程、端口或状态文件副作用。
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the collection daemon (start/status/stop/restart) / 管理采集守护进程（start/status/stop/restart）",
		Long: ui.Bi("Manage the collection/analysis daemon that watches AI client session logs and keeps usage data up to date. The daemon runs in the background: use start/status/stop/restart to control the current process. Bare `token-usage daemon` only prints this help and starts nothing; the dashboard service is managed separately by the `serve` command group. The autostart definition (whether the daemon starts on next login/reboot) is configured via daemon.autostart in config and is never modified by these commands.",
			"管理采集/分析守护进程：它后台监控各 AI 客户端会话日志，保持用量数据持续更新。守护进程以后台方式运行：用 start/status/stop/restart 管理当前进程。裸执行 `token-usage daemon` 只显示本帮助，不启动任何进程；仪表板服务由 `serve` 命令组单独管理。开机自启定义（下次登录/重启是否自动启动）由 config 的 daemon.autostart 决定，这些命令不修改它。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newDaemonStartCmd(),
		newDaemonStatusCmd(),
		newDaemonStopCmd(),
		newDaemonRestartCmd(),
	)
	return cmd
}

// controlStartStopper 是 daemon 生命周期动作需要的 control.Manager 子集。
// 用接口而非 *control.Manager，使 CLI 测试可注入 stub 覆盖结果合同（已运行/未运行/失败）。
type controlStartStopper interface {
	Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error)
	Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error)
	Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error)
	Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error)
}

// realControlStartStopper 适配 *control.Manager 到 controlStartStopper（直接转发，类型一致）。
type realControlStartStopper struct{ m *control.Manager }

func (r realControlStartStopper) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	return r.m.Start(ctx, load)
}
func (r realControlStartStopper) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return r.m.Stop(ctx, load)
}
func (r realControlStartStopper) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return r.m.Restart(ctx, load)
}
func (r realControlStartStopper) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return r.m.Inspect(ctx, cfg)
}

// controlManagerFactory 是 controlStartStopper 的工厂（默认走 *control.Manager）。
// 测试覆盖以注入 stub，避免触碰真实 home/文件系统。
var controlManagerFactory = func() (controlStartStopper, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}
	return realControlStartStopper{m: mgr}, nil
}

// newDaemonStartCmd 后台启动守护进程（立即返回，nginx 风格）。
// 经 control.Manager.Start：control lock 内 load config → inspect daemon lock
// → 已运行返回 PID（不 spawn）→ 未运行 spawn _run → 等 PID+daemon lock 就绪。
func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background (nginx-style) / 后台启动守护进程（立即返回，nginx 风格）",
		Long: ui.Bi("Start the daemon in the background (returns immediately, nginx-style).\n\n"+
			"What gets started is the currently running daemon (the live collection/analysis monitor),\n"+
			"separate from the autostart definition: autostart is decided by daemon.autostart in config;\n"+
			"this command only starts the current process and does not touch the autostart definition.",
			"后台启动守护进程（立即返回，nginx 风格）。\n\n"+
				"启动的是当前运行的守护进程（采集/分析的实时监控进程），\n"+
				"与开机自启定义分离：开机自启由 config 的 daemon.autostart 决定，\n"+
				"本命令只启动当前进程，不修改自启定义。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd)
		},
	}
}

// startConfigLoader 是 start 前置检查用的配置加载函数（默认 loadConfig），
// 测试注入替身以覆盖「全关/仅 router/有 enabled」三态，不依赖开发机配置。
var startConfigLoader = loadConfig

// runStart 抽出便于测试。
func runStart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	// 前置拦截：配置上无任何监控目标时，daemon 启动后必然「无存活监控」失败，
	// 等待就绪只会白耗 5 秒超时。提前提示引导先启用客户端；已在运行的实例不
	// 拦截（走原 AlreadyRunning 输出）；load/inspect 失败降级放行，由 Start
	// 内部权威路径兜底。
	if cfg, cfgErr := startConfigLoader(); cfgErr == nil {
		if st, insErr := mgr.Inspect(cmdContext(cmd), cfg); insErr == nil && !st.Running {
			if !analyzer.HasMonitorTargets(cfg) {
				fmt.Fprintln(out, "⚠ "+ui.Bi("No enabled clients; the daemon would have nothing to monitor",
					"没有任何已启用的客户端，守护进程将无事可做"))
				fmt.Fprintln(out, "  "+ui.Bi("Enable at least one client before starting",
					"请先启用客户端再启动")+":")
				fmt.Fprintln(out, "  token-usage config set clients.claude.enabled true")
				return errors.New(ui.Bi("no enabled clients, start aborted", "没有已启用的客户端，已取消启动"))
			}
		}
	}

	res, err := mgr.Start(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …），命令只补充上下文返回，不再手写 stderr。
		return fmt.Errorf("%s: %w", ui.Bi("failed to start daemon", "启动守护进程失败"), err)
	}

	if res.AlreadyRunning {
		// 已运行：stdout 显示当前 PID，退出码 0
		if res.PID > 0 {
			fmt.Fprintf(out, "%s（PID %d）\n", ui.Bi("daemon already running", "守护进程已在运行"), res.PID)
		} else {
			fmt.Fprintln(out, ui.Bi("daemon already running", "守护进程已在运行"))
		}
		return nil
	}

	fmt.Fprintf(out, "✓ %s（PID %d）\n", ui.Bi("daemon started", "守护进程已启动"), res.PID)
	return nil
}

// newDaemonStatusCmd 查看守护进程运行状态与配置摘要。
func newDaemonStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status and config summary / 查看守护进程运行状态与配置摘要",
		Long: ui.Bi("Show daemon status and a config summary.\n\n"+
			"\"Running status\" reflects whether the current daemon (the live collection/analysis monitor) is running,\n"+
			"separate from the autostart definition: autostart reflects \"whether it auto-starts on next login/reboot\",\n"+
			"decided by daemon.autostart in config, independent of whether it is currently running. With `--format json` the same state is emitted as a machine-readable document (stable fields, closed status vocabularies).",
			"查看守护进程运行状态与配置摘要。\n\n"+
				"「运行状态」反映当前守护进程（采集/分析的实时监控进程）是否在运行，\n"+
				"与开机自启定义分离：开机自启反映「下次登录/重启是否自动启动」，\n"+
				"由 config 的 daemon.autostart 决定，与当前是否运行相互独立。`--format json` 把同一份状态输出为机器可读的文档（字段稳定、状态取封闭值域）。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd)
		},
	}
	cmd.Flags().String("format", "table", ui.Bi(
		"Output format: table (human-readable report) or json (machine-readable status document)",
		"输出格式：table（人读报告）或 json（机器可读的状态文档）",
	))
	return cmd
}

// runStatus 抽出便于测试。
func runStatus(cmd *cobra.Command) error {
	format, _ := cmd.Flags().GetString("format")
	switch format {
	case "", "table", "json":
	default:
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid --format %q (allowed: table, json)", format),
			fmt.Sprintf("无效的 --format %q（允许：table、json）", format),
		))
	}

	out := cmd.OutOrStdout()

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}
	// status 只读：Inspect 不抢 control lock（仅以 daemon lock 判活），返回一致快照。
	st, err := mgr.Inspect(cmdContext(cmd), cfg)
	if err != nil {
		return err
	}

	if format == "json" {
		// 结构化状态仅 json 模式构造（resolveAutostart 会做一次只读定义检测，
		// table 路径由 printAutoStartStatus 自行完成，避免重复执行）。
		report := daemonStatusReport{
			Running:             st.Running,
			PID:                 st.PID,
			StartupPhase:        resolveStartupPhase(st),
			DataDir:             cfg.DataDir,
			PollIntervalSeconds: cfg.Daemon.PollInterval,
			Autostart:           resolveAutostart(cfg, service.NewAutoStartManager()),
		}
		payload, jsonErr := marshalExportJSON(report)
		if jsonErr != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to encode daemon status as JSON", "daemon 状态 JSON 编码失败"), jsonErr)
		}
		_, jsonErr = io.WriteString(out, payload)
		return jsonErr
	}

	if st.Running {
		if st.PID > 0 {
			fmt.Fprintf(out, "● %s（PID %d）\n", ui.Bi("daemon running", "守护进程运行中"), st.PID)
		} else {
			fmt.Fprintln(out, "● "+ui.Bi("daemon running", "守护进程运行中"))
		}
		// 启动阶段只在 daemon lock 判 Running 后解释 runtime-state，
		// 不参与 autostart 漂移判断。catch_up=succeeded 不打印额外行（仅运行中即足）。
		printStartupPhase(out, st)
	} else {
		fmt.Fprintln(out, "○ "+ui.Bi("daemon not running", "守护进程未运行"))
	}

	// 配置摘要
	fmt.Fprintf(out, "%s: %s\n", ui.Bi("Data directory", "数据目录"), cfg.DataDir)
	fmt.Fprintf(out, "%s: %ds\n", ui.Bi("Poll interval", "轮询间隔"), cfg.Daemon.PollInterval)

	// 开机自启状态（只读漂移检测，不触发 Sync）
	printAutoStartStatus(out, cfg, service.NewAutoStartManager())
	return nil
}

// printAutoStartStatus 打印开机自启状态，只读检测定义漂移（不触发 Sync）。
// 第三参 mgr 显式注入 service.AutoStartManager（纯 definition），使单测可传 fake 覆盖各组合。
//
// 漂移判定使用 AutoStartStatus.Exists/SpecMatches：
// autostart 只表达「下次登录/重启是否自动启动」，与当前 daemon 是否运行相互独立。
// 当前 daemon 状态由上方「守护进程运行中/未运行」单独展示，两者不互相推断。
//
// 状态分类：
//  1. autostart=true  && Exists && SpecMatches      → 已启用
//  2. autostart=true  && !Exists                     → 定义丢失，建议重新保存配置
//  3. autostart=true  && Exists && !SpecMatches      → 内容不一致，建议重新保存配置
//  4. autostart=false && Exists                      → 残留，建议重新保存配置
//  5. autostart=false && !Exists                     → 未启用（已收敛）
func printAutoStartStatus(out io.Writer, cfg *config.Config, mgr service.AutoStartManager) {
	bin, err := executableForStatus()
	if err != nil {
		fmt.Fprintf(out, "%s: %s（%s: %s: %v）\n",
			ui.Bi("Autostart", "开机自启"), boolText(cfg.Daemon.AutoStart), ui.Bi("detection failed", "检测失败"), ui.Bi("failed to get current executable path", "获取当前可执行文件路径"), err)
		return
	}
	opts := service.Options{Label: service.Label, BinPath: bin, DataDir: cfg.DataDir,
		LogDir: service.EffectiveLogDir(cfg), Args: []string{"_run"}}

	st, err := mgr.Status(opts)
	if err != nil {
		// 平台不支持或检测失败：打印 autostart 配置值，不报错
		fmt.Fprintf(out, "%s: %s（%s: %v）\n", ui.Bi("Autostart", "开机自启"), boolText(cfg.Daemon.AutoStart), ui.Bi("detection failed", "检测失败"), err)
		return
	}

	switch {
	case cfg.Daemon.AutoStart && st.Exists && st.SpecMatches:
		// 状态 1：定义存在且完全一致
		fmt.Fprintln(out, ui.Bi("Autostart: enabled", "开机自启: 已启用"))

	case cfg.Daemon.AutoStart && !st.Exists:
		// 状态 2：用户开自启但定义缺失 → 漂移
		fmt.Fprintln(out, "⚠ "+ui.Bi("config and actual state mismatch: autostart=on but the autostart definition is missing; re-saving the config is recommended", "配置与实际状态不一致：autostart=开 但自启定义缺失，建议重新保存配置"))

	case cfg.Daemon.AutoStart && st.Exists && !st.SpecMatches:
		// 状态 3：定义存在但内容不一致（漂移）
		fmt.Fprintln(out, "⚠ "+ui.Bi("config and actual state mismatch: autostart=on but the autostart definition differs; re-saving the config is recommended", "配置与实际状态不一致：autostart=开 但自启定义内容不一致，建议重新保存配置"))

	case !cfg.Daemon.AutoStart && st.Exists:
		// 状态 4：用户关自启但定义仍存在（残留）
		fmt.Fprintln(out, "⚠ "+ui.Bi("config and actual state mismatch: autostart=off but an autostart definition still exists; re-saving the config is recommended", "配置与实际状态不一致：autostart=关 但自启定义仍存在，建议重新保存配置"))

	default:
		// 状态 5：!AutoStart && !Exists → 已收敛
		fmt.Fprintln(out, ui.Bi("Autostart: disabled", "开机自启: 未启用"))
	}
}

var executableForStatus = os.Executable

func boolText(b bool) string {
	if b {
		return ui.Bi("on", "开")
	}
	return ui.Bi("off", "关")
}

// printStartupPhase 在 daemon 运行中时打印启动阶段（一行，紧随运行行）。
// 阶段信息来自 control.RuntimeState（control.Inspect 已在 PID/state 的 PID+instanceID 全匹配时填充）：
//
//	state 缺失/非法/不匹配（!PhaseAvailable，且 PID 可读） → 启动阶段未知
//	PID 元数据不可用（!PhaseAvailable，且 PID=0）           → PID 元数据不可用
//	monitor_ready=false                                       → 监听初始化中
//	catch_up=pending/running                                  → 监听已就绪，正在补采
//	catch_up=succeeded                                        → 无额外行（仅运行中即足）
//	catch_up=failed                                           → 补采部分失败（N），执行 token-usage errors
//
// 任何 catch_up 的未知值（既非 pending/running/succeeded/failed）按「阶段未知」降级，
// 不擅自猜测新阶段。阶段信息只用于展示，不参与 autostart 漂移判断（printAutoStartStatus 独立判定）。
// 未运行时调用方不调用本函数（保持无输出）。
func printStartupPhase(out io.Writer, st control.RuntimeState) {
	if !st.Running {
		return
	}
	// PhaseAvailable=false：阶段不可信，按 PID 元数据是否可用分别降级。
	if !st.PhaseAvailable {
		if st.PID > 0 {
			fmt.Fprintln(out, ui.Bi("Startup phase: unknown", "启动阶段: 未知"))
		} else {
			fmt.Fprintln(out, ui.Bi("PID metadata unavailable", "PID 元数据不可用"))
		}
		return
	}
	// monitor_ready 未就绪：监听初始化中（先于补采，无论 CatchUp 取值）。
	if !st.MonitorReady {
		fmt.Fprintln(out, ui.Bi("Startup phase: monitors initializing", "启动阶段: 监听初始化中"))
		return
	}
	// monitor_ready 已就绪：按补采阶段展示。
	switch st.CatchUp {
	case "pending", "running":
		fmt.Fprintln(out, ui.Bi("Startup phase: monitors ready, catching up", "启动阶段: 监听已就绪，正在补采"))
	case "succeeded":
		// 补采成功：无额外阶段行（运行中即足）。
	case "failed":
		fmt.Fprintf(out, "%s（%d），%s\n", ui.Bi("Startup phase: catch-up partially failed", "启动阶段: 补采部分失败"), st.CatchUpFailures, ui.Bi("run `token-usage errors`", "请执行 `token-usage errors`"))
	default:
		// 未知 CatchUp 值：降级为阶段未知，不猜测新阶段。
		fmt.Fprintln(out, ui.Bi("Startup phase: unknown", "启动阶段: 未知"))
	}
}

// newDaemonStopCmd 停止守护进程。
// 经 control.Manager.Stop：control lock 内 load config → inspect daemon lock
// → 未运行幂等返回 → 运行中按平台停止（macOS bootout→查 lock→必要时 SIGTERM；Windows taskkill 准确 PID）
// → 以 daemon lock 释放为成功条件；超时返回错误，不删 PID 伪装成功。
func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon / 停止守护进程",
		Long: ui.Bi("Stop the currently running daemon.\n\n"+
			"Only stops the current process and does not modify the autostart definition: whether it autostarts on next login/reboot is still decided by\n"+
			"daemon.autostart in config. To disable autostart, use the config command to set daemon.autostart=false.",
			"停止当前运行的守护进程。\n\n"+
				"仅停止当前进程，不修改开机自启定义：下次登录/重启是否自启仍由 config 的\n"+
				"daemon.autostart 决定。如需关闭自启，请使用 config 命令设置 daemon.autostart=false。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(cmd)
		},
	}
}

// runStop 抽出便于测试。
func runStop(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	res, err := mgr.Stop(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …），命令只补充上下文返回，不再手写 stderr。
		return fmt.Errorf("%s: %w", ui.Bi("failed to stop daemon", "停止守护进程失败"), err)
	}

	if !res.WasRunning {
		// 未运行：stdout 显示未运行，退出码 0
		fmt.Fprintln(out, ui.Bi("daemon not running", "守护进程未运行"))
		return nil
	}

	fmt.Fprintf(out, "✓ %s（PID %d）\n", ui.Bi("daemon stopped", "守护进程已停止"), res.PID)
	return nil
}

// newDaemonRestartCmd 重启当前守护进程：在单次 control lock 内停旧起新。
// 经 control.Manager.Restart：Acquire control lock → load config → inspect daemon lock
// → 未运行返回 ErrRestartNotRunning → 运行中 stopLocked 等旧 daemon lock 释放
// → startLocked spawn 新 child 等 PID+daemon lock 就绪。
//
// 全流程不触碰 config、plist 或注册表：stop 是 bootout/SIGTERM（保留定义），
// start 是 detached spawn。macOS 若旧进程由 launchd 启动，stop 会 bootout 当前 job，
// 随后以 detached 方式 start；plist 定义保留，但本次会话失去 KeepAlive（已接受取舍，
// 不增加 kickstart 或隐式 bootstrap）。如需恢复 KeepAlive 托管，请用 config 重存自启定义。
func newDaemonRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon / 重启当前守护进程",
		Long: ui.Bi("Restart the currently running daemon (stop the old and start the new within a single process control lock).\n\n"+
			"Only restarts the currently running daemon and does not modify autostart definitions in config, plist or the registry:\n"+
			"whether it autostarts on next login/reboot is still decided by daemon.autostart in config.\n\n"+
			"macOS trade-off: if the old process was started by launchd, stop boots out the current job and then\n"+
			"starts detached; the plist definition is kept but this session loses KeepAlive.\n"+
			"To restore KeepAlive management, re-save the autostart definition via the config command.",
			"重启当前守护进程（在单次进程控制锁内停旧起新）。\n\n"+
				"仅重启当前运行的守护进程，不修改 config、plist 或注册表等自启定义：\n"+
				"下次登录/重启是否自启仍由 config 的 daemon.autostart 决定。\n\n"+
				"macOS 取舍：若旧进程由 launchd 启动，stop 会 bootout 当前 job，随后以\n"+
				"detached 方式 start；plist 定义保留但本次会话失去 KeepAlive。\n"+
				"如需恢复 KeepAlive 托管，请使用 config 命令重新保存自启定义。"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestart(cmd)
		},
	}
}

// runRestart 抽出便于测试。
func runRestart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	mgr, err := controlManagerFactory()
	if err != nil {
		return err
	}

	res, err := mgr.Restart(cmdContext(cmd), func() (*config.Config, error) {
		return loadConfig()
	})
	if err != nil {
		// 失败文本由 cobra 统一输出（Error: …）。未运行的文案已含「请使用
		// token-usage daemon start」指引，原样返回；其它失败补充上下文。
		if errors.Is(err, control.ErrRestartNotRunning) {
			return err
		}
		return fmt.Errorf("%s: %w", ui.Bi("failed to restart daemon", "重启守护进程失败"), err)
	}

	fmt.Fprintf(out, "✓ %s（PID %d → %d）\n", ui.Bi("daemon restarted", "守护进程已重启"), res.OldPID, res.NewPID)
	return nil
}

// ---- daemon status --format json ----

// daemonStatusReport 是 `daemon status --format json` 的结构化载荷:字段与
// table 报告同源(同一 Inspect 快照与 autostart 漂移判定),startup_phase 仅在
// running=true 时非 null。
type daemonStatusReport struct {
	Running             bool                `json:"running"`
	PID                 int                 `json:"pid"`
	StartupPhase        *daemonStartupPhase `json:"startup_phase"`
	DataDir             string              `json:"data_dir"`
	PollIntervalSeconds int                 `json:"poll_interval_seconds"`
	Autostart           daemonAutostart     `json:"autostart"`
}

// daemonStartupPhase 归纳 runtime-state 的启动阶段:Available=false 表示阶段
// 不可信(runtime-state 缺失/非法/instanceID 不匹配);CatchUp 为 runtime-state
// 的原值,未知值降级为 unknown(与 table 的「启动阶段: 未知」同一保守策略)。
type daemonStartupPhase struct {
	Available       bool   `json:"available"`
	MonitorReady    bool   `json:"monitor_ready"`
	CatchUp         string `json:"catch_up"`
	CatchUpFailures int    `json:"catch_up_failures"`
}

// daemonAutostart 是 autostart 定义层与配置的对照结果:Status 取封闭值域
// enabled/missing/drift/residual/disabled(五态分类与 printAutoStartStatus 的
// 展示分支一一对应),检测不可行时为 unknown 并置 DetectFailed。
type daemonAutostart struct {
	Configured       bool   `json:"configured"`
	DefinitionExists bool   `json:"definition_exists"`
	SpecMatches      bool   `json:"spec_matches"`
	DetectFailed     bool   `json:"detect_failed"`
	Status           string `json:"status"`
	DetectErr        string `json:"detect_error,omitempty"`
}

// resolveAutostart 只读判定 autostart 五态(不触发 service.Sync,与
// printAutoStartStatus 同一漂移分类语义)。executableForStatus 与 mgr.Status
// 的失败统一按 detect_failed/unknown 报告,不视为配置错误。
func resolveAutostart(cfg *config.Config, mgr service.AutoStartManager) daemonAutostart {
	out := daemonAutostart{Configured: cfg.Daemon.AutoStart}
	bin, err := executableForStatus()
	if err != nil {
		out.DetectFailed = true
		out.Status = "unknown"
		out.DetectErr = ui.Bi("failed to get current executable path", "获取当前可执行文件路径") + ": " + err.Error()
		return out
	}
	opts := service.Options{Label: service.Label, BinPath: bin, DataDir: cfg.DataDir,
		LogDir: service.EffectiveLogDir(cfg), Args: []string{"_run"}}
	st, err := mgr.Status(opts)
	if err != nil {
		out.DetectFailed = true
		out.Status = "unknown"
		out.DetectErr = err.Error()
		return out
	}
	out.DefinitionExists = st.Exists
	out.SpecMatches = st.SpecMatches
	switch {
	case cfg.Daemon.AutoStart && st.Exists && st.SpecMatches:
		out.Status = "enabled"
	case cfg.Daemon.AutoStart && !st.Exists:
		out.Status = "missing"
	case cfg.Daemon.AutoStart && st.Exists && !st.SpecMatches:
		out.Status = "drift"
	case !cfg.Daemon.AutoStart && st.Exists:
		out.Status = "residual"
	default:
		out.Status = "disabled"
	}
	return out
}

// resolveStartupPhase 把 RuntimeState 归纳为结构化启动阶段;未运行返回 nil
// (运行态字段已在顶层)。未知 CatchUp 值降级 unknown,不猜测新阶段。
func resolveStartupPhase(st control.RuntimeState) *daemonStartupPhase {
	if !st.Running {
		return nil
	}
	phase := &daemonStartupPhase{
		Available:       st.PhaseAvailable,
		MonitorReady:    st.MonitorReady,
		CatchUp:         st.CatchUp,
		CatchUpFailures: st.CatchUpFailures,
	}
	if !st.PhaseAvailable {
		phase.CatchUp = "unknown"
	}
	switch phase.CatchUp {
	case "pending", "running", "succeeded", "failed":
	default:
		phase.CatchUp = "unknown"
	}
	return phase
}
