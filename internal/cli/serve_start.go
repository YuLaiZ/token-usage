// internal/cli/serve_start.go
package cli

// serve_start.go 实现 `token-usage serve start`：nginx 风格的后台启动。
// 父进程负责：获取跨进程启动锁 serve-start.lock（串行化并发 start 的竞态
// 窗口；与服务主体的 serve.lock 生命周期锁分工见 serve_state.go）→ 已运行
// 检查（状态文件 + /api/meta 探活；损坏文件与陈旧状态一样删除后放行）→
// 截断日志文件 → daemon.SpawnDetached 拉起 detached 的 _serve-run → 轮询
// serve.json 等待子命令完成监听 → HTTP 探活确认 → 输出结果；--open 由父进程
// 在确认启动成功后执行浏览器打开（子命令 _serve-run 不传 --open）。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// 后台启动的等待参数（包级 var 供测试按需缩短）。
var (
	// serveStaleProbeTimeout 是判定已记录实例是否仍存活的单次探活超时，
	// serveDashboard 的单实例守卫（serveLifecycleGuard）与 serve start 的
	// 已运行预检共用。
	serveStaleProbeTimeout = 1500 * time.Millisecond
	// serveStartPollTimeout 是轮询 serve.json 的上限：子命令监听成功才写
	// 状态文件，超时说明 spawn 失败或监听报错（原因看 serve.log）。
	serveStartPollTimeout  = 5 * time.Second
	serveStartPollInterval = 100 * time.Millisecond
	// serveStartProbeTimeout/serveStartProbeRetries 是状态文件出现后的存活确认。
	serveStartProbeTimeout = 1 * time.Second
	serveStartProbeRetries = 3
	serveStartConfirmGap   = 100 * time.Millisecond
	serveStartLogTailLines = 10
	// serveLifecycleLockWait/serveLifecycleLockRetry 是 serveLifecycleGuard
	// 获取 serve.lock 的带界重试参数(前一个实例退出收尾的瞬态窗口)。
	serveLifecycleLockWait  = 3 * time.Second
	serveLifecycleLockRetry = 100 * time.Millisecond
)

// serveSpawnAndWait 是「spawn + 轮询等待」的注入 seam：生产实现拉起 detached
// 的 `_serve-run --addr <addr>`（stdout/stderr 重定向到 logPath）并轮询
// serve.json——子命令 net.Listen 成功才写状态文件，出现后再以 /api/meta 探活
// 确认服务真正可达。返回后台进程 PID 与实际监听地址（取自状态文件，:0 端口
// 下与请求地址不同）。测试注入假实现，不做真实 spawn。
var serveSpawnAndWait = func(cfg *config.Config, addr, logPath string) (pid int, realAddr string, err error) {
	bin, err := os.Executable()
	if err != nil {
		return 0, "", fmt.Errorf("%s: %w", ui.Bi("failed to locate current executable", "定位当前可执行文件失败"), err)
	}
	// 截断重建日志文件：SpawnDetached 以 O_APPEND 追加打开，先截断保证每次
	// 后台启动的日志从空白开始，start 超时附带的日志尾才是本次的失败原因。
	if err := truncateServeLog(logPath); err != nil {
		return 0, "", err
	}
	// Lease=nil：serve 后台是一次性孤儿进程，不参与 daemon 的 control/lease 体系。
	if _, err := daemon.SpawnDetached(daemon.SpawnOptions{
		BinPath:    bin,
		Args:       []string{"_serve-run", "--addr", addr},
		StdoutPath: logPath,
		StderrPath: logPath,
		Lease:      nil,
	}); err != nil {
		return 0, "", err
	}
	deadline := time.Now().Add(serveStartPollTimeout)
	for {
		if st, err := readServeState(cfg.DataDir); err == nil && st != nil {
			base := "http://" + st.Addr
			for i := 0; i < serveStartProbeRetries; i++ {
				if serveMetaAlive(base, serveStartProbeTimeout) {
					return st.PID, st.Addr, nil
				}
				time.Sleep(serveStartConfirmGap)
			}
			return 0, "", errors.New(ui.Bi(
				"background serve wrote its state file but /api/meta never responded",
				"后台服务已写出状态文件，但 /api/meta 始终无响应"))
		}
		if time.Now().After(deadline) {
			return 0, "", errors.New(ui.Bi(
				fmt.Sprintf("background serve did not become ready within %s", serveStartPollTimeout),
				fmt.Sprintf("后台服务未在 %s 内就绪", serveStartPollTimeout)))
		}
		time.Sleep(serveStartPollInterval)
	}
}

// serveOpenBrowser 是 start --open 的浏览器 seam：默认转发 openBrowser；
// 测试注入以避免拉起真实浏览器。
var serveOpenBrowser = openBrowser

// serveStartPreflight 是 start 的已运行预检，在 serve-state 状态迁移锁内执行
// 「读状态 → 探活判定 → 陈旧/损坏条件删除」整段：返回 (running, err)——
// running 非 nil 表示已有存活实例（陈旧判定的原状态响应，或条件删除发现新
// 实例已接管且响应），调用方按其内容幂等拒绝；running 为 nil 表示可继续启动，
// 陈旧/损坏状态已按条件删除语义清理完毕。返回时 state 锁必然已释放：spawn
// 之前必须无 state 锁——子进程 serveDashboard 写状态需取同一把锁（POSIX
// flock 不同 fd 互斥），锁内 spawn 会自死锁；serve-start.lock 与 state 锁
// 亦无嵌套（见 serve_state.go 锁序约定）。
func serveStartPreflight(dataDir string) (*ServeState, error) {
	stateLock, err := acquireServeStateLock(dataDir)
	if err != nil {
		return nil, err
	}
	defer releaseServeStateLock(stateLock)

	st, err := readServeState(dataDir)
	switch {
	case errors.Is(err, errServeStateCorrupt):
		// 损坏状态与陈旧同路：锁内删除残留文件，避免 spawn 轮询读到
		// 无法辨识的旧文件误判就绪。
		if rmErr := removeServeState(dataDir); rmErr != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
		}
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	case st != nil:
		if serveMetaAlive("http://"+st.Addr, serveStaleProbeTimeout) {
			return st, nil
		}
		// 放行前条件删除陈旧状态，避免 spawn 轮询读到旧文件误判就绪；
		// 锁内重读不一致说明新实例已接管，不删。
		removed, current, rmErr := removeServeStateIfSame(dataDir, st)
		if rmErr != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
		if !removed {
			// 新实例已写出自己的 serve.json：对它重新探活——响应则按新实例
			// 幂等拒绝；无响应则当前状态才是真的陈旧，删除后放行。
			if current != nil && serveMetaAlive("http://"+current.Addr, serveStaleProbeTimeout) {
				return current, nil
			}
			if rmErr := removeServeState(dataDir); rmErr != nil {
				return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
			}
		}
	}
	return nil, nil
}

// newServeStartCmd 构造 `serve start`。--addr/--open 继承自 serve 的
// persistent flags。
func newServeStartCmd(load func() (*config.Config, error), version string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: ui.Bi("Start the dashboard server in the background", "后台启动仪表板服务"),
		Long: ui.Bi(
			"Start the read-only dashboard server in the background (nginx-style): this command spawns a detached child that keeps running after it exits, then waits until the child reports ready. State is recorded in serve.json under the data directory (~/.token-usage/serve.json by default) and logs go to serve.log in the same directory; each start truncates the log. If the recorded state still answers on /api/meta the command reports it and returns idempotently with exit code 0 (stop it first with `token-usage serve stop` to restart); stale state left by a crash or SIGKILL — and a corrupt state file — is removed and start proceeds. Concurrent starts are serialized by a serve-start.lock file lock in the data directory (start coordination only — a running instance is described by serve.json and held via the serve.lock lifecycle lock): a second start while another is still working fails with a retry hint. Use `token-usage serve status` to check and `token-usage serve stop` to stop. With --open the browser is opened by this command after the background server is confirmed up.\n\nExamples:\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve start --open",
			"以后台方式（nginx 风格）启动只读仪表板服务：本命令拉起一个 detached 子进程并在退出后继续运行，然后等待子进程报告就绪。状态记录在数据目录下的 serve.json（默认 ~/.token-usage/serve.json），日志写入同目录的 serve.log；每次启动都会截断日志。若已记录的状态在 /api/meta 上仍有响应则报告已在运行并以退出码 0 幂等返回（要重启请先用 `token-usage serve stop` 停止）；崩溃或 SIGKILL 遗留的陈旧状态与损坏的状态文件会被清理并照常启动。并发启动由数据目录下的 serve-start.lock 文件锁串行化（仅用于启动协调——运行中的实例由 serve.json 描述、以 serve.lock 生命周期锁持有）：另一个 serve start 尚在执行时报错并提示稍后重试。用 `token-usage serve status` 查看状态、`token-usage serve stop` 停止。带 --open 时由本命令在确认后台启动成功后打开浏览器。\n\n示例：\n  token-usage serve start\n  token-usage serve start --addr 127.0.0.1:9000\n  token-usage serve start --open",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			autoOpen, _ := cmd.Flags().GetBool("open")
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			return serveStartRun(cfg, addr, autoOpen, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

// serveStartRun 执行一次完整的后台启动编排：--addr 非空校验 → serve-start.lock
// 串行化 → 已运行预检（幂等拒绝/陈旧清理）→ spawn + 探活确认 → 输出 →
// （--open 时开浏览器）。由 `serve start` 与 `serve restart` 共用：restart 在
// 调用本函数前先完成 stop 编排。返回值语义与 start 的 RunE 一致（已运行为
// 幂等成功 exit 0）。
func serveStartRun(cfg *config.Config, addr string, autoOpen bool, out, errOut io.Writer) error {
	if strings.TrimSpace(addr) == "" {
		return fmt.Errorf("%s: %s",
			ui.Bi("missing required --addr", "缺少必填的 --addr"),
			ui.Bi("example: token-usage serve start --addr 127.0.0.1:8619", "示例：token-usage serve start --addr 127.0.0.1:8619"))
	}

	// 启动互斥（serve-start.lock）：串行化并发 start 的竞态窗口（两个
	// start 同时通过「未运行」预检会各拉起一个实例抢同一端口）。锁持有
	// 覆盖预检 → spawn → 探活确认 → 输出的全过程；已运行拒绝逻辑仍由
	// serve.json 与探活决定，锁只保证同一时刻至多一个 start 在执行。
	// 注意与 serve.lock 的分工：后者是服务主体经 serveDashboard 持有的
	// 生命周期锁（见 serveLifecycleGuard），本锁不表达运行状态。
	lock, ok := daemon.AcquireLock(filepath.Join(cfg.DataDir, serveStartLockFile))
	if !ok {
		return errors.New(ui.Bi(
			"another serve start is in progress; retry in a moment",
			"另一个 serve start 正在执行，请稍后重试"))
	}
	defer daemon.ReleaseLock(lock)

	// 已运行检查（serveStartPreflight，state 锁内）：状态文件存在且
	// /api/meta 有响应 → 幂等返回（与 daemon start 的 AlreadyRunning
	// 同语义：informational 输出、退出码 0、不倒 Usage）；存在但无响应
	// → 视为陈旧（SIGKILL/崩溃遗留），条件删除后继续；条件删除发现新
	// 实例接管且响应 → 同样幂等拒绝（拒绝的是新实例）。返回时 state 锁
	// 已释放，spawn 之前不得再持锁（子进程写状态需取同一把锁）。
	if running, err := serveStartPreflight(cfg.DataDir); err != nil {
		return err
	} else if running != nil {
		fmt.Fprintf(out, "%s\n", ui.Bi(
			fmt.Sprintf("serve is already running (pid %d, http://%s); stop it first with token-usage serve stop", running.PID, running.Addr),
			fmt.Sprintf("仪表板已在后台运行（PID %d，http://%s）；请先用 token-usage serve stop 停止", running.PID, running.Addr)))
		return nil
	}

	logPath := serveLogPath(cfg.DataDir)
	pid, realAddr, err := serveSpawnAndWait(cfg, addr, logPath)
	if err != nil {
		// 失败时把 serve.log 末尾 ≤10 行附在错误里，用户无需另开日志。
		return fmt.Errorf("%s: %w", ui.Bi("failed to start dashboard in background", "后台启动仪表板失败"),
			errors.Join(err, serveLogTailError(logPath, serveStartLogTailLines)))
	}

	url := "http://" + realAddr
	// 交互终端下用 OSC 8 链接包裹 URL，支持单击打开（非 TTY 自动降级纯文本）。
	linked := hyperlinkURL(url, writerIsTerminal(out))
	fmt.Fprintf(out, "%s\n", ui.Bi(
		fmt.Sprintf("dashboard started in background at %s (pid %d)", linked, pid),
		fmt.Sprintf("仪表板已后台启动 %s（PID %d）", linked, pid),
	))
	fmt.Fprintf(out, "%s\n", ui.Bi(
		fmt.Sprintf("log: %s · stop: token-usage serve stop", logPath),
		fmt.Sprintf("日志 %s · 停止 token-usage serve stop", logPath),
	))

	if autoOpen {
		// 打开浏览器失败不致命：打印警告，后台服务已就绪。
		if err := serveOpenBrowser(url); err != nil {
			fmt.Fprintf(errOut, "%s\n", ui.Bi(
				fmt.Sprintf("failed to open browser: %v", err),
				fmt.Sprintf("打开浏览器失败：%v", err),
			))
		}
	}
	return nil
}

// truncateServeLog 截断重建后台日志文件（O_TRUNC|O_CREATE 0644）。
func truncateServeLog(path string) error {
	f, err := os.OpenFile(path, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create serve log file", "创建后台日志文件失败"), err)
	}
	return f.Close()
}

// serveLogTailError 读取日志文件末尾至多 maxLines 行并包装为可合并进错误的
// 提示；日志缺失/为空/读取失败时返回 nil（静默省略，保留主错误）。
func serveLogTailError(logPath string, maxLines int) error {
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return fmt.Errorf("%s\n%s", ui.Bi("log tail:", "日志末尾："), strings.Join(lines, "\n"))
}
