// internal/cli/run_internal.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newInternalRunCmd 创建 Hidden 内部命令 _run：被 start/launchd 拉起的守护进程主体。
// 用户不直接接触（Hidden=true，--help 不可见）。
//
// 资源打开顺序：_run 不预开 DB/logger（不调 loadRuntime）。
// control lock（独立路径）仅用于序列化 config 加载；DB/logger 由 daemon.Run 在 daemon lock
// commit 后通过 OpenResources 打开。
//
// 父子 lease 有两条路径：
//   - 父 lease 路径（start spawn 的 _run）：control.ParseParentLease 解析出合法 instanceID +
//     lease read end → 启动 lease watcher（阻塞读 read end，EOF 触发状态机 NotifyEOF）→
//     在 lease 生效后 load config → daemon.Run(ParentLeaseLost=sm.LeaseLost(),
//     InstanceID=desc.InstanceID, OnDaemonLockCommit=sm.MarkDaemonLockCommitted(nil))。
//     child 不抢 control lock（父进程持有授权）；EOF 先于 daemon lock commit → 取消启动。
//   - 独立路径（launchd/注册表直接拉起）：无合法父 lease → 自行获取 control lock（有界 15s）→
//     load config → daemon.Run(OnDaemonLockCommit=释放 control lock, InstanceID=自行生成,
//     ParentLeaseLost=nil)。
//
// 两条路径都满足不变量「从读取 effective config 到获取 daemon lock 期间始终存在 control lease」：
//   - 父 lease 路径：父进程持 control lock 授权 child（lease），child 在 lease 下读 config + 获 daemon lock；
//   - 独立路径：child 自己获 control lock。
//
// launchd 防护：独立路径获取 control lock 发生 ErrControlLockTimeout →
// 记录 + 成功退出（退出码 0，不进 daemon.Run）。macOS 避免 KeepAlive 循环。
func newInternalRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_run",
		Short:  ui.Bi("Internal command (daemon body, spawned by start/launchd; do not invoke directly)", "内部命令（守护进程主体，由 start/launchd 拉起，不直接调用）"),
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon(cmd.Context())
		},
	}
}

// runDaemon 装配并运行守护进程。抽出便于测试（注入 ctx + 覆盖 home/manager 构造）。
//
// 顺序：
//  1. 解析 home → control.NewManager。
//  2. 解析父 lease：control.ParseParentLease(os.Environ())。
//  3. 父 lease 路径：启动 watcher + 状态机 → 在 lease 生效后 load config → daemon.Run。
//     独立路径：AcquireLock（超时 → exitEarly）→ load config → daemon.Run。
//  4. signal ctx（SIGINT/SIGTERM）。
//
// launchd 防护：独立路径 control lock 超时 → 返回 nil（退出码 0）。
// config 解析、daemon lock、DB 或 watcher 错误仍非零退出（由 cobra RunE 自动转成退出码）。
func runDaemon(parentCtx context.Context) (retErr error) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// signal context 必须覆盖 control lock 等待、配置加载和 daemon.Run 全流程，
	// 不能等到启动准备完成后才开始响应 SIGINT/SIGTERM。
	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}

	desc, hasParentLease := control.ParseParentLease(os.Environ())

	var cfg *config.Config
	var opts daemon.RunOptions
	var leaseReader control.LeaseReader
	if hasParentLease {
		// 父 lease 路径：父进程持 control lock 授权 child，child 不抢锁。
		leaseReader = desc.Reader()
		// lease watcher + 状态机在 prepareParentLeaseOptions 内启动（与 config 加载解耦）。
		_, opts = prepareParentLeaseOptions(desc, leaseReader)
		// 父 lease 已生效（父进程持 control lock + write end 授权 child），可在 watcher 启动后加载 config。
		cfg, err = configLoaderForRun()
		if err != nil {
			if leaseReader != nil {
				leaseReader.Close()
			}
			return err
		}
	} else {
		// 独立路径：launchd/注册表直接拉起，自行获取 control lock。
		var exitEarly bool
		cfg, opts, exitEarly, err = prepareIndependentRun(ctx, mgr)
		if err != nil {
			return err
		}
		if exitEarly {
			// control lock 超时：launchd 防护分支——成功退出（退出码 0）不进 daemon.Run。
			return nil
		}
		// daemon lock 提交时会提前释放；若 daemon.Run 在提交前失败，defer 兜底释放。
		// prepareIndependentRun 用 sync.Once 保证两条路径合并后只关闭一次 Session。
		defer func() {
			if releaseErr := opts.OnDaemonLockCommit(); releaseErr != nil &&
				!errors.Is(retErr, releaseErr) {
				retErr = errors.Join(retErr, fmt.Errorf("%s: %w", ui.Bi("failed to release process control lock", "释放进程控制锁失败"), releaseErr))
			}
		}()
	}

	runErr := daemon.Run(ctx, cfg, opts)
	// lease reader 资源清理：watcher goroutine 已退出（EOF 或 daemon 退出），关闭 read end。
	if leaseReader != nil {
		leaseReader.Close()
	}
	// 父 lease 路径下，daemon.Run 因 ErrParentLeaseLost（EOF 先到）取消是预期行为：
	// child 不写 PID/runtime-state、退出码 0（不视为 daemon 启动失败）。
	if hasParentLease && errors.Is(runErr, daemon.ErrParentLeaseLost) {
		slog.Info("_run startup cancelled due to lost parent control lease (daemon.Run not entered)", "err", runErr)
		return nil
	}
	return runErr
}

// prepareParentLeaseOptions 构造父 lease 路径的 daemon.RunOptions（不含 config 加载）。
// config 由 runDaemon 在调用本函数后加载（保证「在 lease 生效后加载 config」）。
//
//   - 启动 lease watcher goroutine：阻塞读 read end，EOF/错误 → sm.NotifyEOF()。
//   - OnDaemonLockCommit = sm.MarkDaemonLockCommitted(nil)：daemon lock 获取后推进状态机；
//     若 EOF 已先到则不 commit（daemon.Run 会因 LeaseLost 检测到取消并返回 ErrParentLeaseLost）。
//   - ParentLeaseLost = sm.LeaseLost()：daemon.Run 在 AcquireLock 前后检查它。
//   - InstanceID = desc.InstanceID（父进程生成的一次性标识）。
//
// lease watcher 与状态机通过同一互斥状态机提交 daemonLockAcquired。
// 返回 (sm, opts)：sm 供 runDaemon 持有引用（虽然 watcher 通过 sm 交互，但 runDaemon 无需直接用）。
func prepareParentLeaseOptions(desc control.ParentLeaseDescriptor, reader control.LeaseReader) (*control.LeaseStateMachine, daemon.RunOptions) {
	sm := control.NewLeaseStateMachine()
	if reader != nil {
		go func() {
			reader.WaitForEOF()
			sm.NotifyEOF()
		}()
	}
	opts := daemon.RunOptions{
		InstanceID:      desc.InstanceID,
		ParentLeaseLost: sm.LeaseLost(),
		OnDaemonLockCommit: func() error {
			sm.MarkDaemonLockCommitted(nil)
			return nil
		},
	}
	return sm, opts
}

// prepareIndependentRun 构造独立路径（无父 lease）的 daemon.RunOptions。
//
// 顺序：AcquireLock（超时 → exitEarly）→ load config → OnDaemonLockCommit=释放 control lock。
//   - ErrControlLockTimeout：记录 + exitEarly=true（退出码 0，launchd 防护）。
//   - config 加载错误：返回错误（调用方非零退出）。
//   - OnDaemonLockCommit 在 daemon lock commit 后释放 control lock。
//
// 返回：
//   - cfg：effective config（exitEarly=true 时为 nil）。
//   - opts：daemon.RunOptions（exitEarly=true 时为零值，调用方不进入 Run）。
//   - exitEarly：true 表示「不进入 daemon.Run，成功退出」。
//   - err：config 解析错误或非超时 control lock 错误。
func prepareIndependentRun(ctx context.Context, mgr *control.Manager) (*config.Config, daemon.RunOptions, bool, error) {
	sess, err := mgr.AcquireLock(ctx)
	if err != nil {
		if errors.Is(err, control.ErrControlLockTimeout) {
			// launchd 防护分支：没有父 lease 的 _run（launchd/注册表直接拉起）
			// 获取不到 control lock 时，成功退出且不进入 daemon.Run，避免与正在进行的控制操作冲突，
			// 并在 macOS 上避免 launchd KeepAlive 立即重拉。
			slog.Info("_run timed out waiting for control lock, exiting to avoid conflicting with an in-flight control operation (daemon.Run not entered)",
				"err", err)
			return nil, daemon.RunOptions{}, true, nil
		}
		return nil, daemon.RunOptions{}, false, fmt.Errorf("%s: %w", ui.Bi("failed to acquire process control lock", "获取进程控制锁失败"), err)
	}

	// 锁内加载 raw config + resolve effective。
	cfg, err := configLoaderForRun()
	if err != nil {
		return nil, daemon.RunOptions{}, false, errors.Join(err, sess.Close())
	}

	// OnDaemonLockCommit 在 daemon lock commit 后恰好调用一次：释放 control lock。
	var releaseOnce sync.Once
	var releaseErr error
	release := func() error {
		releaseOnce.Do(func() {
			releaseErr = sess.Close()
		})
		return releaseErr
	}
	opts := daemon.RunOptions{
		InstanceID:         control.GenerateInstanceID(),
		OnDaemonLockCommit: release,
	}
	return cfg, opts, false, nil
}

// configLoaderForRun 加载 effective config 的可注入函数。
// _run 专用：不走 loadRuntime（不预开 DB/logger）。测试可临时替换以注入 fake config，
// 避免依赖 os.UserHomeDir() 下的真实 config.toml（生产路径与 loadConfigForRun 一致）。
var configLoaderForRun = loadConfigForRun

// loadConfigForRun 加载 effective config（runtimecfg.LoadEffectiveConfig，唯一解析边界）。
// _run 专用：不走 loadRuntime（不预开 DB/logger）。
func loadConfigForRun() (*config.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	return cfg, nil
}
