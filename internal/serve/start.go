// internal/serve/start.go
package serve

// start.go 实现后台启动编排（nginx 风格）：serve-start.lock 串行化 → 已运行
// 预检（状态文件 + /api/meta 探活；损坏文件与陈旧状态一样删除后放行）→ 截断
// 日志文件 → daemon.SpawnDetached 拉起 detached 的 _serve-run → 轮询 serve.json
// 等待子命令完成监听 → HTTP 探活确认。serve start 命令与 update 的 dashboard
// 自动恢复共用本编排：
//   - serve start 命令不传 BinPath（运行时探测 os.Executable，与既有行为一致），
//     随后由命令层渲染输出并处理 --open（浏览器打开只属于命令层，本编排绝不
//     打开浏览器）；
//   - update 的恢复路径显式传入新二进制路径与原监听地址（替换后的目标二进制
//     与恢复进程可能不同，不能探测 os.Executable——Windows helper 尤其如此，
//     与 daemon 侧 Session.StartWithExecutable 同理）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// 后台启动的等待参数（包级 var 供测试按需缩短）。
var (
	// StartPollTimeout 是轮询 serve.json 的上限：子命令监听成功才写
	// 状态文件，超时说明 spawn 失败或监听报错（原因看 serve.log）。
	StartPollTimeout  = 5 * time.Second
	StartPollInterval = 100 * time.Millisecond
	// StartProbeTimeout/StartProbeRetries 是状态文件出现后的存活确认。
	StartProbeTimeout = 1 * time.Second
	StartProbeRetries = 3
	StartConfirmGap   = 100 * time.Millisecond
	// StartLogTailLines 是启动失败错误附带的服务日志尾行数。
	StartLogTailLines = 10
)

// StartOptions 是后台启动编排的入参。
type StartOptions struct {
	// DataDir 状态文件/日志/锁所在的数据目录。
	DataDir string
	// Addr 请求的监听地址（":0" 端口由子命令实际监听结果决定，见 StartResult.Addr）。
	Addr string
	// BinPath 拉起 `_serve-run` 使用的二进制绝对路径；空串表示运行时探测
	// os.Executable()（serve start 命令的既有行为）。update 的恢复路径必须
	// 显式传入目标二进制，禁止留空。
	BinPath string
}

// StartResult 描述一次成功的后台启动：PID 为后台子进程 PID，Addr 为实际监听
// 地址（取自状态文件；":0" 端口下与请求地址不同）。AlreadyRunning=true 表示
// 预检发现已有存活实例、本次未拉起新进程（幂等避让），PID/Addr 描述的是现存
// 实例。
type StartResult struct {
	PID  int
	Addr string
	// AlreadyRunning 表示本次调用是「已有存活实例」的幂等避让而非全新启动。
	AlreadyRunning bool
}

// SpawnAndWait 是「spawn + 轮询等待」的注入 seam：生产实现拉起 detached 的
// `_serve-run --addr <addr>`（stdout/stderr 重定向到 logPath）并轮询 serve.json——
// 子命令 net.Listen 成功才写状态文件，出现后再以 /api/meta 探活确认服务真正
// 可达。返回后台进程 PID 与实际监听地址。测试注入假实现，不做真实 spawn。
// binPath 为空时生产实现运行时探测 os.Executable()。
var SpawnAndWait = func(dataDir, binPath, addr, logPath string) (pid int, realAddr string, err error) {
	if binPath == "" {
		bin, err := os.Executable()
		if err != nil {
			return 0, "", fmt.Errorf("%s: %w", ui.Bi("failed to locate current executable", "定位当前可执行文件失败"), err)
		}
		binPath = bin
	}
	// 截断重建日志文件：SpawnDetached 以 O_APPEND 追加打开，先截断保证每次
	// 后台启动的日志从空白开始，start 超时附带的日志尾才是本次的失败原因。
	if err := truncateLog(logPath); err != nil {
		return 0, "", err
	}
	// Lease=nil：serve 后台是一次性孤儿进程，不参与 daemon 的 control/lease 体系。
	if _, err := daemon.SpawnDetached(daemon.SpawnOptions{
		BinPath:    binPath,
		Args:       []string{"_serve-run", "--addr", addr},
		StdoutPath: logPath,
		StderrPath: logPath,
		Lease:      nil,
	}); err != nil {
		return 0, "", err
	}
	deadline := time.Now().Add(StartPollTimeout)
	for {
		if st, err := ReadState(dataDir); err == nil && st != nil {
			base := "http://" + st.Addr
			for i := 0; i < StartProbeRetries; i++ {
				if MetaAlive(base, StartProbeTimeout) {
					return st.PID, st.Addr, nil
				}
				time.Sleep(StartConfirmGap)
			}
			return 0, "", errors.New(ui.Bi(
				"background serve wrote its state file but /api/meta never responded",
				"后台服务已写出状态文件，但 /api/meta 始终无响应"))
		}
		if time.Now().After(deadline) {
			return 0, "", errors.New(ui.Bi(
				fmt.Sprintf("background serve did not become ready within %s", StartPollTimeout),
				fmt.Sprintf("后台服务未在 %s 内就绪", StartPollTimeout)))
		}
		time.Sleep(StartPollInterval)
	}
}

// StartInBackground 执行一次完整的后台启动编排：--addr 非空校验 → serve-start.lock
// 串行化 → 已运行预检（幂等避让/陈旧清理）→ spawn + 探活确认。由 serve start
// 命令与 update 的 dashboard 自动恢复共用。
//
// 已有存活实例时返回幂等成功：StartResult 描述现存实例（PID/Addr 为其值），
// 与 daemon start 的 AlreadyRunning 同语义（由调用方决定如何呈现）。陈旧/损坏
// 状态按条件删除语义清理后照常启动。
func StartInBackground(opts StartOptions) (StartResult, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return StartResult{}, fmt.Errorf("%s: %s",
			ui.Bi("missing required --addr", "缺少必填的 --addr"),
			ui.Bi("example: token-usage serve start --addr 127.0.0.1:8619", "示例：token-usage serve start --addr 127.0.0.1:8619"))
	}
	// BinPath 为空/空白时由 SpawnAndWait 内部运行时探测 os.Executable()（serve
	// start 命令的既有行为）；update 的恢复路径必须显式传入目标二进制。

	// 启动互斥（serve-start.lock）：串行化并发 start 的竞态窗口（两个
	// start 同时通过「未运行」预检会各拉起一个实例抢同一端口）。锁持有
	// 覆盖预检 → spawn → 探活确认的全过程；已运行避让逻辑仍由
	// serve.json 与探活决定，锁只保证同一时刻至多一个 start 在执行。
	// 注意与 serve.lock 的分工：后者是服务主体经 LifecycleGuard 持有的
	// 生命周期锁，本锁不表达运行状态。
	lock, ok := daemon.AcquireLock(filepath.Join(opts.DataDir, StartLockFile))
	if !ok {
		return StartResult{}, errors.New(ui.Bi(
			"another serve start is in progress; retry in a moment",
			"另一个 serve start 正在执行，请稍后重试"))
	}
	defer daemon.ReleaseLock(lock)

	// 已运行检查（startPreflight，state 锁内）：状态文件存在且
	// /api/meta 有响应 → 幂等返回；存在但无响应 → 视为陈旧（SIGKILL/
	// 崩溃遗留），条件删除后继续；条件删除发现新实例接管且响应 → 同样
	// 幂等返回（返回的是新实例）。返回时 state 锁已释放，spawn 之前
	// 不得再持锁（子进程写状态需取同一把锁）。
	running, err := startPreflight(opts.DataDir)
	if err != nil {
		return StartResult{}, err
	}
	if running != nil {
		return StartResult{PID: running.PID, Addr: running.Addr, AlreadyRunning: true}, nil
	}

	pid, realAddr, err := SpawnAndWait(opts.DataDir, opts.BinPath, opts.Addr, LogPath(opts.DataDir))
	if err != nil {
		// 失败时把 serve.log 末尾 ≤10 行附在错误里，调用方无需另开日志。
		return StartResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to start dashboard in background", "后台启动仪表板失败"),
			errors.Join(err, logTailError(LogPath(opts.DataDir), StartLogTailLines)))
	}
	return StartResult{PID: pid, Addr: realAddr}, nil
}

// startPreflight 是启动的已运行预检，在 serve-state 状态迁移锁内执行
// 「读状态 → 探活判定 → 陈旧/损坏条件删除」整段：返回 (running, err)——
// running 非 nil 表示已有存活实例（陈旧判定的原状态响应，或条件删除发现新
// 实例已接管且响应），调用方按其内容幂等避让；running 为 nil 表示可继续启动，
// 陈旧/损坏状态已按条件删除语义清理完毕。返回时 state 锁必然已释放：spawn
// 之前必须无 state 锁——子进程写状态需取同一把锁（POSIX flock 不同 fd 互斥），
// 锁内 spawn 会自死锁；serve-start.lock 与 state 锁亦无嵌套（见 state.go
// 锁序约定）。
func startPreflight(dataDir string) (*ServeState, error) {
	stateLock, err := AcquireStateLock(dataDir)
	if err != nil {
		return nil, err
	}
	defer ReleaseStateLock(stateLock)

	st, err := ReadState(dataDir)
	switch {
	case errors.Is(err, ErrStateCorrupt):
		// 损坏状态与陈旧同路：锁内删除残留文件，避免 spawn 轮询读到
		// 无法辨识的旧文件误判就绪。
		if rmErr := RemoveState(dataDir); rmErr != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
		}
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	case st != nil:
		if MetaAlive("http://"+st.Addr, StaleProbeTimeout) {
			return st, nil
		}
		// 放行前条件删除陈旧状态，避免 spawn 轮询读到旧文件误判就绪；
		// 锁内重读不一致说明新实例已接管，不删。
		removed, current, rmErr := RemoveStateIfSame(dataDir, st)
		if rmErr != nil {
			return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
		if !removed {
			// 新实例已写出自己的 serve.json：对它重新探活——响应则按新实例
			// 幂等避让；无响应则当前状态才是真的陈旧，删除后放行。
			if current != nil && MetaAlive("http://"+current.Addr, StaleProbeTimeout) {
				return current, nil
			}
			if rmErr := RemoveState(dataDir); rmErr != nil {
				return nil, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
			}
		}
	}
	return nil, nil
}

// truncateLog 截断重建后台日志文件（O_TRUNC|O_CREATE 0644）。
func truncateLog(path string) error {
	f, err := os.OpenFile(path, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create serve log file", "创建后台日志文件失败"), err)
	}
	return f.Close()
}

// logTailError 读取日志文件末尾至多 maxLines 行并包装为可合并进错误的
// 提示；日志缺失/为空/读取失败时返回 nil（静默省略，保留主错误）。
func logTailError(logPath string, maxLines int) error {
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
