package update

// serve_lifecycle.go 定义 update 流程对 dashboard 生命周期的窄依赖。
//
// 自更新必须保持 dashboard（serve）的原运行态：更新前探测并停止运行中的
// 实例（释放被占用的目标二进制——Windows 上运行中的 .exe 无法被替换），
// 替换成功后以原监听地址、用新二进制恢复后台实例；更新前未运行则保持未
// 运行。serve 的状态文件、锁、探活、停止与后台启动编排由 internal/serve
// 包提供，cli 装配层用它的导出函数适配本接口注入 Service.ServeLifecycle；
// update 包绝不 import cli 包，也绝不通过 Cobra、CLI 子进程或 shell 调用
// `serve stop`/`serve start`。
//
// ServeLifecycle 为 nil 时（历史调用方与隔离测试）Apply 完全跳过 serve
// 编排，行为与「dashboard 未运行」一致，不影响既有合同。

// ServeLifecycle 是 update 流程需要的 dashboard 生命周期操作窄集。
type ServeLifecycle interface {
	// DetectRunning 返回更新前 dashboard 是否在运行及其实例监听地址。
	// serve.json 缺失、损坏或陈旧（记录地址对 /api/meta 无响应）一律视为
	// 未运行（running=false, addr=""）——陈旧状态不能误判为运行实例。
	// 实现为纯读：绝不修改、删除任何状态文件，也绝不停止实例。
	DetectRunning(dataDir string) (running bool, addr string, err error)

	// Stop 停止运行中的 dashboard，并以「记录地址的 /api/meta 不再响应」
	// 为停止判据。actualStopped=true 表示探活确认停止了一个运行实例；
	// false 表示本就无运行实例（无状态文件、损坏或陈旧残留——探测与停止
	// 之间实例自行退出的场景由调用方据此放弃恢复）。停止未确认成功时返回
	// 非 nil error，调用方必须中止更新且不得替换二进制。
	Stop(dataDir string) (actualStopped bool, err error)

	// Start 用指定二进制以原监听地址把 dashboard 拉起为后台实例（detached
	// spawn `_serve-run --addr`，等待 serve.json 出现并以 /api/meta 探活
	// 确认）。实现绝不允许打开浏览器、绝不继承手工 `serve start --open`
	// 行为；失败返回非 nil error（实例未就绪，状态文件由编排清理）。
	Start(dataDir, binPath, addr string) error
}
