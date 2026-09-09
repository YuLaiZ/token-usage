// internal/cli/serve_stop.go
package cli

// serve_stop.go 实现 `token-usage serve stop` 的平台无关编排：读 serve.json →
// 探活分诊（无文件幂等、无响应按陈旧条件清理、损坏按残留清理）→ 平台信号 →
// 以「记录地址的 /api/meta 不再响应」为停止判据 → 条件删除状态文件；强杀兜底
// 后服务仍在响应则报非零错误并保留状态文件。
//
// 状态迁移锁的覆盖范围（见 serve_state.go 锁序约定）：「读状态 → 探活判定 →
// 陈旧/损坏删除」整段在 serve-state 锁内，删除一律走 removeServeStateIfSame
// 条件删除（锁内重读比对一致才删）——判定陈旧后、删除前新实例恰好接管时不会
// 误删新状态，而是对新状态重探活：响应则重跑完整 stop 停掉新实例（保证停的
// 是探活响应的那个），无响应才删后按未运行报告。信号与等待段不持 state 锁
// （窗口秒级，不能阻塞其他状态迁移），探活确认下线后的删除由 serveStopFinalize
// 重新取锁条件删除，等待窗口内接管的新实例状态不会被误删。
//
// pid 复用风险的处置约定：信号发给状态文件记录的 PID，但停止成功的唯一判据
// 是记录地址的 /api/meta 不再响应——本命令是同一实例的权威停止方（状态文件
// 由 serve 体系写出）。信号发送成功不代表可以宣称停止：若 pid 已被无关进程
// 复用，信号可能落在别人头上而探活目标毫无变化，此时以探活为准继续兜底强杀；
// 反之探活不再响应即视为已停，即便信号路径上出现过个别错误。信号投递失败也
// 不短路——进程可能恰在探活与信号之间死亡，照常进入探活等待，由探活给出结论。
//
// 删除状态文件的唯一判据：探活确认下线，或信号前本就无响应（陈旧）/文件损坏。
// 强杀后 /api/meta 仍响应（无论强杀本身是否报错）说明存在本 CLI 管不住的实例
// （如 pid 已被无关进程复用且对方常驻、强杀被系统拒绝），此时报非零错误并
// 保留 serve.json 供人工排查进程/端口，绝不谎报停止成功。

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// stop 的等待参数（包级 var 供测试按需缩短）。
var (
	// serveStopProbeTimeout 是分诊与停止判据的单次探活超时。
	serveStopProbeTimeout = 1 * time.Second
	// serveStopTermWait 是优雅信号后等待 /api/meta 下线的窗口。
	serveStopTermWait = 3 * time.Second
	// serveStopKillWait 是强杀兜底后的等待窗口。
	serveStopKillWait = 1 * time.Second
	// serveStopProbeInterval 是判据轮询的间隔。
	serveStopProbeInterval = 200 * time.Millisecond
)

// serveSignalProc / serveKillProc 是平台停止信号的注入 seam：unix 实现为
// SIGTERM / SIGKILL（serve_stop_unix.go）；Windows 均为 taskkill /F
// （serve_stop_windows.go，Windows 控制台进程没有跨进程优雅停止通道）。
// 测试注入记录调用而不真杀进程。
var (
	serveSignalProc = serveSignalProcPlatform
	serveKillProc   = serveKillProcPlatform
)

// newServeStopCmd 构造 `serve stop`。--addr/--open 继承自 serve 的
// persistent flags（stop 自身不使用，仅保持命令族 flag 面一致）。
func newServeStopCmd(load func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: ui.Bi("Stop the dashboard server running in the background", "停止后台运行的仪表板服务"),
		Long: ui.Bi(
			"Stop the dashboard server running in the background. The command reads serve.json under the data directory (~/.token-usage/serve.json by default), sends the stop signal to the recorded PID (SIGTERM on Unix with a 3s graceful window, then SIGKILL as a fallback; taskkill /F on Windows — Windows console processes have no cross-process graceful-stop channel, which is acceptable for a strictly read-only service), and confirms the stop by probing /api/meta until it stops responding. Success is judged solely by the probe — the recorded PID may have been reused by an unrelated process, so the probe wins over the signal result, and a failed signal delivery does not short-circuit the probe wait. The state file is removed only after the probe confirms shutdown; stale state (no response before signalling) and a corrupt state file are cleaned up too. If the server still responds after the SIGKILL fallback (whether or not the kill itself reported an error), the command exits non-zero with the recorded URL and PID and keeps serve.json in place for manual inspection. Stopping an already-stopped server is a no-op that still exits 0.\n\nExamples:\n  token-usage serve stop",
			"停止后台运行的仪表板服务。命令读取数据目录下的 serve.json（默认 ~/.token-usage/serve.json），向记录的 PID 发送停止信号（Unix 为 SIGTERM 并给 3s 优雅窗口，超时以 SIGKILL 兜底；Windows 为 taskkill /F——Windows 控制台进程没有跨进程的优雅停止通道，对严格只读的服务可接受），并以探活 /api/meta 直至不再响应来确认停止。是否停止成功仅以探活为准：记录的 PID 可能已被无关进程复用，探活的结论优先于信号发送结果，信号投递失败也不会短路探活等待。只有探活确认下线后才删除状态文件；陈旧状态（信号前即无响应）与损坏的状态文件同样会被清理。若强杀兜底后服务仍在响应（无论强杀本身是否报错），命令以非零退出码报错并列出记录的 URL 与 PID，保留 serve.json 供人工检查。对已停止的服务重复执行是幂等空操作，退出码仍为 0。\n\n示例：\n  token-usage serve stop",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			return serveStopRun(cfg, cmd.OutOrStdout())
		},
	}
}

// serveStopRun 执行一次完整的 stop 编排：serve-state 锁内「读状态 → 探活判定
// → 陈旧/损坏条件删除」→ 锁外信号与探活等待 → serveStopFinalize 条件删除。
// 当陈旧判定发现状态已被新实例改写且新实例探活响应时，重跑一次本函数——保证
// 停的是探活响应的那个实例（重跑后磁盘状态即新实例所写，走存活 → 信号路径，
// 递归在实际中至多一层；每层递归都需要一次恰好的接管竞态，不设深度上限）。
func serveStopRun(cfg *config.Config, out io.Writer) error {
	// 判定段：serve-state 锁内「读-判-删」。
	stateLock, err := acquireServeStateLock(cfg.DataDir)
	if err != nil {
		return err
	}
	st, err := readServeState(cfg.DataDir)
	if err != nil {
		if errors.Is(err, errServeStateCorrupt) {
			// 损坏的状态文件无法定位实例：删除必须在 state 锁释放之前完成——
			// 先释放再删会重开「新实例写入后被误删」的 TOCTOU 窗口。
			if rmErr := removeServeState(cfg.DataDir); rmErr != nil {
				_ = releaseServeStateLock(stateLock)
				return fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
			}
			_ = releaseServeStateLock(stateLock)
			fmt.Fprintln(out, ui.Bi("serve is not running (corrupt state removed)", "仪表板未在后台运行（已清理损坏的状态文件）"))
			return nil
		}
		_ = releaseServeStateLock(stateLock)
		return fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	}
	if st == nil {
		// 无状态文件：幂等成功，退出码 0。
		_ = releaseServeStateLock(stateLock)
		fmt.Fprintln(out, ui.Bi("serve is not running", "仪表板未在后台运行"))
		return nil
	}

	url := "http://" + st.Addr
	if !serveMetaAlive(url, serveStopProbeTimeout) {
		// 信号前就无响应：疑似陈旧状态。条件删除——锁内重读与判定所据一致
		// 才删；不一致说明新实例已接管，不删。
		removed, current, err := removeServeStateIfSame(cfg.DataDir, st)
		if err != nil {
			_ = releaseServeStateLock(stateLock)
			return fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), err)
		}
		if removed {
			_ = releaseServeStateLock(stateLock)
			fmt.Fprintln(out, ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
			return nil
		}
		// 新实例已写出自己的 serve.json：对它重新探活——响应则重跑完整 stop
		// 逻辑停掉新实例（绝不向旧 PID 发信号）；无响应则当前状态才是真的
		// 陈旧，删除后按未运行报告。
		if current != nil && serveMetaAlive("http://"+current.Addr, serveStopProbeTimeout) {
			_ = releaseServeStateLock(stateLock)
			return serveStopRun(cfg, out)
		}
		if rmErr := removeServeState(cfg.DataDir); rmErr != nil {
			_ = releaseServeStateLock(stateLock)
			return fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
		_ = releaseServeStateLock(stateLock)
		fmt.Fprintln(out, ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
		return nil
	}
	// 判定段结束：信号与等待段不持 state 锁（秒级窗口不阻塞其他状态迁移）。
	_ = releaseServeStateLock(stateLock)

	// 优雅信号：投递失败（进程已死、PID 已被复用又退出等）不短路——
	// 进程可能恰在探活与信号之间死亡，照常进入探活等待，由探活下结论。
	_ = serveSignalProc(st.PID)

	// 停止判据：探活直至不再响应（3s 优雅窗口）。
	if waitMetaDown(url, serveStopTermWait) {
		return serveStopFinalize(cfg, st, out)
	}

	// 宽限窗口内仍响应：兜底强杀后再等一小段。唯一判据仍是探活——
	// 确认下线才删文件并报告停止；仍响应（无论 kill 是否报错）说明
	// 存在本 CLI 管不住的实例，报非零错误并保留状态文件供人工排查，
	// 绝不谎报停止成功。
	killErr := serveKillProc(st.PID)
	if waitMetaDown(url, serveStopKillWait) {
		return serveStopFinalize(cfg, st, out)
	}
	msg := ui.Bi(
		fmt.Sprintf("failed to stop dashboard: server still responding at %s (pid %d); check the process or port manually", url, st.PID),
		fmt.Sprintf("停止仪表板失败：服务仍在响应 %s（PID %d），请检查该进程或端口", url, st.PID),
	)
	if killErr != nil {
		msg += "\n" + ui.Bi(fmt.Sprintf("kill: %v", killErr), fmt.Sprintf("强杀失败：%v", killErr))
	}
	return errors.New(msg)
}

// serveStopFinalize 在探活确认下线后取 serve-state 锁做条件删除并报告停止。
// 重新取锁：信号等待段不持锁，等待窗口内可能有新实例接管（文件被改写）——
// 条件删除发现接管且新实例仍在响应时，与陈旧分支同契约：打印接管说明并对
// 新实例重路由完整 stop 逻辑（stop 只发探活响应的 PID，见 serveStopRun）。
// 新实例接管后自身已退出的，其状态即陈旧，下一轮循环条件删除；每轮循环都
// 需要一次恰好的接管竞态，实际至多一层。
func serveStopFinalize(cfg *config.Config, judged *ServeState, out io.Writer) error {
	for {
		stateLock, err := acquireServeStateLock(cfg.DataDir)
		if err != nil {
			return err
		}
		removed, current, err := removeServeStateIfSame(cfg.DataDir, judged)
		_ = releaseServeStateLock(stateLock)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to remove serve state", "删除服务状态文件失败"), err)
		}
		if removed || current == nil {
			fmt.Fprintln(out, ui.Bi("dashboard stopped", "仪表板已停止"))
			return nil
		}
		// 状态在停止期间被改写：current 是接管实例。仍响应 → 重路由停它；
		// 无响应 → 接管实例自身也已退出，其状态即陈旧，以它为新的 judged
		// 进入下一轮条件删除。
		if !serveMetaAlive("http://"+current.Addr, serveStopProbeTimeout) {
			judged = current
			continue
		}
		fmt.Fprintln(out, ui.Bi(
			fmt.Sprintf("the stopped instance has been replaced by a new one at http://%s; stopping it instead", current.Addr),
			fmt.Sprintf("已停止的实例已被新实例接管（http://%s），转而停止新实例", current.Addr)))
		return serveStopRun(cfg, out)
	}
}

// waitMetaDown 轮询探活直到 /api/meta 不再响应或窗口耗尽；返回 true 表示
// 已确认下线（停止判据，见文件头的 pid 复用处置约定）。
func waitMetaDown(baseURL string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		if !serveMetaAlive(baseURL, serveStopProbeTimeout) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(serveStopProbeInterval)
	}
}
