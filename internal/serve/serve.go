// Package serve 实现本地仪表板（dashboard）的共用生命周期层：serve.json
// 状态文件、三把文件锁、/api/meta 探活、后台停止编排与后台启动编排。
//
// 本包由两个消费方共用：
//   - internal/cli 的 `serve start|status|stop|restart` 命令族与后台服务主体
//     （_serve-run 的单实例守卫、状态写出与自清理）；
//   - internal/update 的自更新编排（更新前探测并停止运行中的 dashboard，
//     替换成功后以原监听地址、用新二进制恢复后台实例）。
//
// 本包刻意不包含三类内容，保持能力层边界：
//   - Cobra 命令与用户可见的结果渲染（留在 cli 命令层；本包编排函数接受
//     io.Writer 仅用于在编排过程内部原样透出既有过程文案，传 nil 即静默）；
//   - 浏览器打开（--open 属 serve start 命令的展示行为，自动恢复路径绝不经过）；
//   - HTTP 服务主体（_serve-run 的监听与请求处理留在 cli/serve_dashboard.go）。
//
// 包级导出的 var（SpawnAndWait、RemoveStateIfSame、SignalProc、KillProc 与各
// 超时参数）是既有测试 seam 的平移：生产路径在 init 后即为固定实现，测试按需
// 注入 fake 或缩短窗口保持用例确定性。
package serve

// StateFile 与 LogFile 是 DataDir 下的状态文件与后台日志文件固定名。
const (
	StateFile = "serve.json"
	LogFile   = "serve.log"
)

// LifecycleLockFile、StartLockFile 与 StateLockFile 是 DataDir 下的三把文件锁
// 固定名（gofrs/flock 跨平台），共同支撑 serve 的单实例契约，角色分工如下：
//   - serve.lock 是生命周期锁：由后台服务主体（_serve-run 子进程）经
//     LifecycleGuard 的单实例守卫获取，在整个服务生命周期持有。
//     它挡住「另一个实例正在启动」的瞬时竞态；「已有实例在运行」的稳态由
//     serve.json + /api/meta 探活判定。
//   - serve-start.lock 是 start 串行化锁：仅 serve start 父进程在预检 → spawn →
//     确认期间持有，串行化并发 start；它不表达运行状态。
//   - serve-state.lock 是状态迁移锁：串行化 serve.json 的所有「读-判定-写/删」
//     迁移（见 state.go 的状态迁移不变量），持有时间为毫秒级（仅 status/stop 的
//     探活判定段可达探活超时的秒级）；它也不表达运行状态。
//
// 三把锁的文件均可残留（flock 随进程退出自动释放），残留文件本身无含义。
const (
	LifecycleLockFile = "serve.lock"
	StartLockFile     = "serve-start.lock"
	StateLockFile     = "serve-state.lock"
)
