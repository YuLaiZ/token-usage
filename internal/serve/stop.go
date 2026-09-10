// internal/serve/stop.go
package serve

// stop.go 实现后台停止编排：读 serve.json → 探活分诊（无文件幂等、无响应按
// 陈旧条件清理、损坏按残留清理）→ 平台信号 → 以「记录地址的 /api/meta 不再
// 响应」为停止判据 → 条件删除状态文件；强杀兜底后服务仍在响应则报非零错误并
// 保留状态文件。
//
// serve stop 命令与 update 的「更新前停止 dashboard」共用本编排：
//   - 命令层传 out（io.Writer）获得与既有行为逐字节一致的过程/结果文案，
//     忽略返回结构；
//   - update 传 out=nil（静默）并消费 StopResult.Stopped 判定是否需要在
//     更新后恢复实例。
//
// 状态迁移锁的覆盖范围（见 state.go 锁序约定）：「读状态 → 探活判定 →
// 陈旧/损坏删除」整段在 serve-state 锁内，删除一律走 RemoveStateIfSame
// 条件删除（锁内重读比对一致才删）——判定陈旧后、删除前新实例恰好接管时不会
// 误删新状态，而是对新状态重探活：响应则重跑完整 stop 停掉新实例（保证停的
// 是探活响应的那个），无响应才删后按未运行报告。信号与等待段不持 state 锁
// （窗口秒级，不能阻塞其他状态迁移），探活确认下线后的删除由 stopFinalize
// 重新取锁条件删除，等待窗口内接管的新实例状态不会被误删。
//
// pid 复用风险的处置约定：信号发给状态文件记录的 PID，但停止成功的唯一判据
// 是记录地址的 /api/meta 不再响应——本编排是同一实例的权威停止方（状态文件
// 由 serve 体系写出）。信号发送成功不代表可以宣称停止：若 pid 已被无关进程
// 复用，信号可能落在别人头上而探活目标毫无变化，此时以探活为准继续兜底强杀；
// 反之探活不再响应即视为已停，即便信号路径上出现过个别错误。信号投递失败也
// 不短路——进程可能恰在探活与信号之间死亡，照常进入探活等待，由探活给出结论。
//
// 删除状态文件的唯一判据：探活确认下线，或信号前本就无响应（陈旧）/文件损坏。
// 强杀后 /api/meta 仍响应（无论强杀本身是否报错）说明存在本体系管不住的实例
// （如 pid 已被无关进程复用且对方常驻、强杀被系统拒绝），此时报非零错误并
// 保留 serve.json 供人工排查进程/端口，绝不谎报停止成功。

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// 停止的等待参数（包级 var 供测试按需缩短）。
var (
	// StopProbeTimeout 是分诊与停止判据的单次探活超时。
	StopProbeTimeout = 1 * time.Second
	// StopTermWait 是优雅信号后等待 /api/meta 下线的窗口。
	StopTermWait = 3 * time.Second
	// StopKillWait 是强杀兜底后的等待窗口。
	StopKillWait = 1 * time.Second
	// StopProbeInterval 是判据轮询的间隔。
	StopProbeInterval = 200 * time.Millisecond
)

// SignalProc / KillProc 是平台停止信号的注入 seam：unix 实现为
// SIGTERM / SIGKILL（stop_unix.go）；Windows 均为 taskkill /F
// （stop_windows.go，Windows 控制台进程没有跨进程优雅停止通道）。
// 测试注入记录调用而不真杀进程。
var (
	SignalProc = signalProcPlatform
	KillProc   = killProcPlatform
)

// StopResult 描述一次停止编排的结局：
//   - Stopped=true：本次调用探活确认停止了一个运行实例（PID/Addr 为其实例信息）；
//   - Stopped=false：本就无运行实例（无状态文件、损坏残留清理或陈旧状态清理），
//     PID/Addr 为零值。
type StopResult struct {
	Stopped bool
	PID     int
	Addr    string
}

// printf 在 out 非 nil 时向其写一行（编排内的过程/结果文案；nil=静默，
// 供 update 等只消费结构化结果的调用方使用）。
func printf(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, format, a...)
}

// StopRun 执行一次完整的停止编排：serve-state 锁内「读状态 → 探活判定
// → 陈旧/损坏条件删除」→ 锁外信号与探活等待 → stopFinalize 条件删除。
// 当陈旧判定发现状态已被新实例改写且新实例探活响应时，重跑一次本函数——保证
// 停的是探活响应的那个实例（重跑后磁盘状态即新实例所写，走存活 → 信号路径，
// 递归在实际中至多一层；每层递归都需要一次恰好的接管竞态，不设深度上限）。
func StopRun(dataDir string, out io.Writer) (StopResult, error) {
	// 判定段：serve-state 锁内「读-判-删」。
	stateLock, err := AcquireStateLock(dataDir)
	if err != nil {
		return StopResult{}, err
	}
	st, err := ReadState(dataDir)
	if err != nil {
		if errors.Is(err, ErrStateCorrupt) {
			// 损坏的状态文件无法定位实例：删除必须在 state 锁释放之前完成——
			// 先释放再删会重开「新实例写入后被误删」的 TOCTOU 窗口。
			if rmErr := RemoveState(dataDir); rmErr != nil {
				_ = ReleaseStateLock(stateLock)
				return StopResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
			}
			_ = ReleaseStateLock(stateLock)
			printf(out, "%s\n", ui.Bi("serve is not running (corrupt state removed)", "仪表板未在后台运行（已清理损坏的状态文件）"))
			return StopResult{}, nil
		}
		_ = ReleaseStateLock(stateLock)
		return StopResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	}
	if st == nil {
		// 无状态文件：幂等成功，退出码 0。
		_ = ReleaseStateLock(stateLock)
		printf(out, "%s\n", ui.Bi("serve is not running", "仪表板未在后台运行"))
		return StopResult{}, nil
	}

	url := "http://" + st.Addr
	if !MetaAlive(url, StopProbeTimeout) {
		// 信号前就无响应：疑似陈旧状态。条件删除——锁内重读与判定所据一致
		// 才删；不一致说明新实例已接管，不删。
		removed, current, err := RemoveStateIfSame(dataDir, st)
		if err != nil {
			_ = ReleaseStateLock(stateLock)
			return StopResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), err)
		}
		if removed {
			_ = ReleaseStateLock(stateLock)
			printf(out, "%s\n", ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
			return StopResult{}, nil
		}
		// 新实例已写出自己的 serve.json：对它重新探活——响应则重跑完整 stop
		// 逻辑停掉新实例（绝不向旧 PID 发信号）；无响应则当前状态才是真的
		// 陈旧，删除后按未运行报告。
		if current != nil && MetaAlive("http://"+current.Addr, StopProbeTimeout) {
			_ = ReleaseStateLock(stateLock)
			return StopRun(dataDir, out)
		}
		if rmErr := RemoveState(dataDir); rmErr != nil {
			_ = ReleaseStateLock(stateLock)
			return StopResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
		_ = ReleaseStateLock(stateLock)
		printf(out, "%s\n", ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
		return StopResult{}, nil
	}
	// 判定段结束：信号与等待段不持 state 锁（秒级窗口不阻塞其他状态迁移）。
	_ = ReleaseStateLock(stateLock)

	// 优雅信号：投递失败（进程已死、PID 已被复用又退出等）不短路——
	// 进程可能恰在探活与信号之间死亡，照常进入探活等待，由探活下结论。
	_ = SignalProc(st.PID)

	// 停止判据：探活直至不再响应（3s 优雅窗口）。
	if waitMetaDown(url, StopTermWait) {
		return stopFinalize(dataDir, st, out)
	}

	// 宽限窗口内仍响应：兜底强杀后再等一小段。唯一判据仍是探活——
	// 确认下线才删文件并报告停止；仍响应（无论 kill 是否报错）说明
	// 存在管不住的实例，报非零错误并保留状态文件供人工排查，
	// 绝不谎报停止成功。
	killErr := KillProc(st.PID)
	if waitMetaDown(url, StopKillWait) {
		return stopFinalize(dataDir, st, out)
	}
	msg := ui.Bi(
		fmt.Sprintf("failed to stop dashboard: server still responding at %s (pid %d); check the process or port manually", url, st.PID),
		fmt.Sprintf("停止仪表板失败：服务仍在响应 %s（PID %d），请检查该进程或端口", url, st.PID),
	)
	if killErr != nil {
		msg += "\n" + ui.Bi(fmt.Sprintf("kill: %v", killErr), fmt.Sprintf("强杀失败：%v", killErr))
	}
	return StopResult{}, errors.New(msg)
}

// stopFinalize 在探活确认下线后取 serve-state 锁做条件删除并报告停止。
// 重新取锁：信号等待段不持锁，等待窗口内可能有新实例接管（文件被改写）——
// 条件删除发现接管且新实例仍在响应时，与陈旧分支同契约：打印接管说明并对
// 新实例重路由完整 stop 逻辑（stop 只发探活响应的 PID，见 StopRun）。
// 新实例接管后自身已退出的，其状态即陈旧，下一轮循环条件删除；每轮循环都
// 需要一次恰好的接管竞态，实际至多一层。
func stopFinalize(dataDir string, judged *ServeState, out io.Writer) (StopResult, error) {
	for {
		stateLock, err := AcquireStateLock(dataDir)
		if err != nil {
			return StopResult{}, err
		}
		removed, current, err := RemoveStateIfSame(dataDir, judged)
		_ = ReleaseStateLock(stateLock)
		if err != nil {
			return StopResult{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove serve state", "删除服务状态文件失败"), err)
		}
		if removed || current == nil {
			printf(out, "%s\n", ui.Bi("dashboard stopped", "仪表板已停止"))
			return StopResult{Stopped: true, PID: judged.PID, Addr: judged.Addr}, nil
		}
		// 状态在停止期间被改写：current 是接管实例。仍响应 → 重路由停它；
		// 无响应 → 接管实例自身也已退出，其状态即陈旧，以它为新的 judged
		// 进入下一轮条件删除。
		if !MetaAlive("http://"+current.Addr, StopProbeTimeout) {
			judged = current
			continue
		}
		printf(out, "%s\n", ui.Bi(
			fmt.Sprintf("the stopped instance has been replaced by a new one at http://%s; stopping it instead", current.Addr),
			fmt.Sprintf("已停止的实例已被新实例接管（http://%s），转而停止新实例", current.Addr)))
		return StopRun(dataDir, out)
	}
}

// waitMetaDown 轮询探活直到 /api/meta 不再响应或窗口耗尽；返回 true 表示
// 已确认下线（停止判据，见文件头的 pid 复用处置约定）。
func waitMetaDown(baseURL string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		if !MetaAlive(baseURL, StopProbeTimeout) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(StopProbeInterval)
	}
}
