// internal/serve/guard.go
package serve

// guard.go 实现后台服务主体的单实例守卫 LifecycleGuard：任意时刻至多一个
// 服务实例。由 cli 包的后台服务主体（_serve-run → serveDashboard）在监听之前
// 调用；update 的恢复路径不经本守卫——它通过 StartInBackground 的已运行预检
// 幂等避让，而非接管端口。

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// lifecycleLockWait/lifecycleLockRetry 是 LifecycleGuard 获取 serve.lock 的
// 带界重试参数(前一个实例退出收尾的瞬态窗口)。
var (
	lifecycleLockWait  = 3 * time.Second
	lifecycleLockRetry = 100 * time.Millisecond
)

// LifecycleGuard 是后台服务主体的单实例守卫，在监听之前执行单实例契约。
// 依次判定：
//
//  1. serve-state 状态迁移锁内的状态分诊（「读-判定-删」整段在锁内，杜绝判定
//     与删除之间新实例接管导致的误删）。损坏的 serve.json → 删除残留后继续
//     （与 status/stop/start 的统一承诺一致）；存在且 /api/meta 有响应 → 幂等
//     拒绝：向 out 打印现存实例的 URL 与 PID，返回 (nil, false, nil)，调用方以
//     退出码 0 返回——URL 对用户可用，exit 0 诚实；存在但无响应 → 陈旧
//     （SIGKILL/崩溃遗留），条件删除后继续。条件删除锁内重读发现已被新实例
//     改写则不删——此时不重评估：新实例能写出状态说明它已持有（或刚释放）
//     serve.lock，下方第 2 步的生命周期锁获取本身就是仲裁（新实例活着则取锁
//     失败报重试，已死则放行接管）。
//  2. 释放 state 锁后取 AcquireLock(serve.lock 生命周期锁)：失败说明另一个
//     实例正在启动（瞬时竞态），返回双语错误，调用方以非零退出码结束；成功则
//     锁随返回值交出，调用方必须在整个服务生命周期持有并在退出时释放。
//
// 锁序：判定段先取再释放 state.lock，之后才取 serve.lock，两锁从不同时持有；
// 放行后服务主体对状态的写/删是 serve.lock → state.lock 顺序（见 cli 包
// serveDashboard），全局无环。
//
// 返回 (lock, true, nil) 表示放行继续启动（lock 非 nil）；(nil, false, nil)
// 表示已有实例在运行、幂等拒绝；err 非 nil 表示意外失败。
func LifecycleGuard(dataDir string, out io.Writer, probeTimeout time.Duration) (*flock.Flock, bool, error) {
	stateLock, err := AcquireStateLock(dataDir)
	if err != nil {
		return nil, false, err
	}
	st, err := ReadState(dataDir)
	switch {
	case errors.Is(err, ErrStateCorrupt):
		// 损坏残留与陈旧同路：锁内直接删除（损坏文件无新实例语义），避免带着
		// 无法辨识的旧文件进入正常路径（后续写状态文件会覆盖它，但删除与 start
		// 的承诺保持一致）。
		if rmErr := RemoveState(dataDir); rmErr != nil {
			_ = ReleaseStateLock(stateLock)
			return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
		}
	case err != nil:
		_ = ReleaseStateLock(stateLock)
		return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	case st != nil:
		if MetaAlive("http://"+st.Addr, probeTimeout) {
			_ = ReleaseStateLock(stateLock)
			url := "http://" + st.Addr
			fmt.Fprintf(out, "%s\n", ui.Bi(
				fmt.Sprintf("dashboard is already running at %s (pid %d); stop it first with token-usage serve stop, or open that URL", url, st.PID),
				fmt.Sprintf("仪表板已在运行（%s，PID %d）；请先用 token-usage serve stop 停止，或直接打开该地址", url, st.PID)))
			return nil, false, nil
		}
		// 放行前条件删除陈旧状态，避免误判「已启动」；新实例已接管时不删：
		// 它存活会令下方 serve.lock 获取失败并报「正在启动」，已死则放行接管。
		if _, _, rmErr := RemoveStateIfSame(dataDir, st); rmErr != nil {
			_ = ReleaseStateLock(stateLock)
			return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
	}
	// 判定段结束，先释放 state 锁再取生命周期锁：两锁从不同时持有（锁序注释）。
	_ = ReleaseStateLock(stateLock)

	// 生命周期锁：挡住「另一个实例正在启动」的瞬时竞态（它已通过状态文件
	// 之前的检查但尚未写出 serve.json）。锁由调用方持有至服务退出。
	// 获取失败按带界重试处理:探活判定下线只说明端口已关闭,前一个实例可能
	// 尚未走完退出路径(释放 serve.lock 前的收尾),serve restart 的 start 段
	// 恰好落在这一瞬态窗口;窗口耗尽仍是真互斥失败,报错退出。
	deadline := time.Now().Add(lifecycleLockWait)
	for {
		lock, ok := daemon.AcquireLock(filepath.Join(dataDir, LifecycleLockFile))
		if ok {
			return lock, true, nil
		}
		if time.Now().After(deadline) {
			return nil, false, errors.New(ui.Bi(
				"another serve instance is starting; retry in a moment",
				"另一个 serve 实例正在启动，请稍后重试"))
		}
		time.Sleep(lifecycleLockRetry)
	}
}
