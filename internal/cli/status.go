// internal/cli/status.go
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/service"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "查看守护进程运行状态与配置摘要",
		Long: "查看守护进程运行状态与配置摘要。\n\n" +
			"「运行状态」反映当前守护进程（采集/分析的实时监控进程）是否在运行，\n" +
			"与开机自启定义分离：开机自启反映「下次登录/重启是否自动启动」，\n" +
			"由 config 的 daemon.autostart 决定，与当前是否运行相互独立。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd)
		},
	}
}

// runStatus 抽出便于测试。
func runStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
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

	if st.Running {
		if st.PID > 0 {
			fmt.Fprintf(out, "● 守护进程运行中（PID %d）\n", st.PID)
		} else {
			fmt.Fprintln(out, "● 守护进程运行中")
		}
		// 启动阶段只在 daemon lock 判 Running 后解释 runtime-state，
		// 不参与 autostart 漂移判断。catch_up=succeeded 不打印额外行（仅运行中即足）。
		printStartupPhase(out, st)
	} else {
		fmt.Fprintln(out, "○ 守护进程未运行")
	}

	// 配置摘要
	fmt.Fprintf(out, "数据目录: %s\n", cfg.DataDir)
	fmt.Fprintf(out, "轮询间隔: %ds\n", cfg.Daemon.PollInterval)

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
		fmt.Fprintf(out, "开机自启: %s（检测失败: 获取当前可执行文件路径: %v）\n",
			boolText(cfg.Daemon.AutoStart), err)
		return
	}
	opts := service.Options{Label: service.Label, BinPath: bin, DataDir: cfg.DataDir, Args: []string{"_run"}}

	st, err := mgr.Status(opts)
	if err != nil {
		// 平台不支持或检测失败：打印 autostart 配置值，不报错
		fmt.Fprintf(out, "开机自启: %s（检测失败: %v）\n", boolText(cfg.Daemon.AutoStart), err)
		return
	}

	switch {
	case cfg.Daemon.AutoStart && st.Exists && st.SpecMatches:
		// 状态 1：定义存在且完全一致
		fmt.Fprintln(out, "开机自启: 已启用")

	case cfg.Daemon.AutoStart && !st.Exists:
		// 状态 2：用户开自启但定义缺失 → 漂移
		fmt.Fprintln(out, "⚠ 配置与实际状态不一致：autostart=开 但自启定义缺失，建议重新保存配置")

	case cfg.Daemon.AutoStart && st.Exists && !st.SpecMatches:
		// 状态 3：定义存在但内容不一致（漂移）
		fmt.Fprintln(out, "⚠ 配置与实际状态不一致：autostart=开 但自启定义内容不一致，建议重新保存配置")

	case !cfg.Daemon.AutoStart && st.Exists:
		// 状态 4：用户关自启但定义仍存在（残留）
		fmt.Fprintln(out, "⚠ 配置与实际状态不一致：autostart=关 但自启定义仍存在，建议重新保存配置")

	default:
		// 状态 5：!AutoStart && !Exists → 已收敛
		fmt.Fprintln(out, "开机自启: 未启用")
	}
}

var executableForStatus = os.Executable

func boolText(b bool) string {
	if b {
		return "开"
	}
	return "关"
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
			fmt.Fprintln(out, "启动阶段: 未知")
		} else {
			fmt.Fprintln(out, "PID 元数据不可用")
		}
		return
	}
	// monitor_ready 未就绪：监听初始化中（先于补采，无论 CatchUp 取值）。
	if !st.MonitorReady {
		fmt.Fprintln(out, "启动阶段: 监听初始化中")
		return
	}
	// monitor_ready 已就绪：按补采阶段展示。
	switch st.CatchUp {
	case "pending", "running":
		fmt.Fprintln(out, "启动阶段: 监听已就绪，正在补采")
	case "succeeded":
		// 补采成功：无额外阶段行（运行中即足）。
	case "failed":
		fmt.Fprintf(out, "启动阶段: 补采部分失败（%d），请执行 `token-usage errors`\n", st.CatchUpFailures)
	default:
		// 未知 CatchUp 值：降级为阶段未知，不猜测新阶段。
		fmt.Fprintln(out, "启动阶段: 未知")
	}
}
