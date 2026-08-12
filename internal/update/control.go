package update

import (
	"context"
	"fmt"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// control.go 定义 update 流程对进程控制层的窄依赖：
//   - ControlSession：control lock 持有期内需要的操作（Inspect/Stop/StartWithExecutable）；
//   - ControlManager：control lock 的获取与回调执行；
//   - controlAdapter：生产适配器，把 *control.Manager 适配到 ControlManager。
//
// update 包通过这些接口而非具体 *control.Session 类型，原因：
//   - 避免把对 control.Session 全部方法的依赖带入 update；
//   - 测试可用 fake 实现，无需真实 control.Manager / 文件锁 / daemon。
//
// 锁内编排约定：update 在 ControlManager.WithLock 的同一个回调内执行
// Inspect →（如运行中）Stop → install →（如原先运行）StartWithExecutable，
// 绝不在该回调内调用 control.Manager.Start/Stop/Restart——它们各自 WithLock 会二次加锁死锁。

// ControlSession 是 update 流程在 control lock 持有期内需要操作的窄接口。
//
// 生产实现是 *control.Session（结构化匹配：它具备下列三方法）。语义与 control.Session 公开方法一致：
//   - Inspect：只读快照，不加 control lock（本 Session 已持锁）；
//   - Stop：锁内停止当前 daemon，等 daemon lock 释放；
//   - StartWithExecutable：锁内用指定 binPath 启动新 daemon
//     （区别于 control.Manager.Start 自动探测 os.Executable，update 替换后
//     必须显式指定新二进制路径，避免探测到旧路径）。
type ControlSession interface {
	Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error)
	Stop(ctx context.Context, cfg *config.Config) error
	StartWithExecutable(ctx context.Context, cfg *config.Config, binPath string) error
}

// ControlManager 抽象 update 流程对 control lock 的获取与回调执行。
//
// 语义对齐 control.Manager.WithLock：超时返回 control.ErrControlLockTimeout，
// 主动取消返回 ctx.Err()，fn 返回值透传，无论 fn 是否出错都释放锁。
// 生产实现是 controlAdapter（包装 *control.Manager）；测试注入 fakeControlManager。
type ControlManager interface {
	// WithLock 获取 control lock，在锁内以 ControlSession 形式执行 fn，结束时释放锁。
	WithLock(ctx context.Context, fn func(ControlSession) error) error
}

// NewControlManager 用 *control.Manager 构造生产用 ControlManager。
// 返回的适配器在 WithLock 回调边界把 *control.Session 作为 ControlSession 传给 update 逻辑。
// 供 CLI 装配层（update.Service 的 control 依赖注入点）使用。
func NewControlManager(mgr *control.Manager) ControlManager {
	return &controlAdapter{mgr: mgr}
}

// controlAdapter 把 control.Manager.WithLock（签名 func(*control.Session) error）
// 适配到 ControlManager.WithLock（签名 func(ControlSession) error）。
//
// 适配器编译的前提是 *control.Session 满足 ControlSession（具备 Inspect/Stop/StartWithExecutable）。
// 在 control 层补齐 Stop/StartWithExecutable 后，*control.Session 结构化匹配本接口，
// 回调内直接把 *control.Session 当 ControlSession 传给 fn（无需包装）。
type controlAdapter struct {
	mgr *control.Manager
}

// WithLock 委托 control.Manager.WithLock：在回调边界把 *control.Session 适配为 ControlSession。
// control.Manager.WithLock 的超时/取消/fn 透传/失败仍释放锁语义原样保留。
func (a *controlAdapter) WithLock(ctx context.Context, fn func(ControlSession) error) error {
	if a == nil || a.mgr == nil {
		return fmt.Errorf("controlAdapter 未装配 control.Manager")
	}
	if fn == nil {
		return fmt.Errorf("进程控制锁回调不能为空")
	}
	return a.mgr.WithLock(ctx, func(s *control.Session) error {
		// *control.Session 此时已满足 ControlSession，无需包装直接传入。
		return fn(s)
	})
}

// Compile-time assertions: 生产实现满足接口。
var (
	_ ControlManager = (*controlAdapter)(nil)
	_ ControlSession = (*control.Session)(nil)
)
