// internal/control/lease.go
//
// 跨平台父子 control lease 的平台无关部分。
//
// start spawn 的 _run 与 start 持锁存在死锁边界：start 持 control
// lock 约 5s 等 ready，spawn 的 _run 无法获取 control lock 只能超时 exitEarly。本文件
// 通过「父子 control lease」解决：父进程 spawn 时持有 control lock 并通过 pipe lease
// 授权 child，child 不需要自己获取 control lock，而是在 daemon lock commit 后通过
// OnDaemonLockCommit 上报。
//
// lease 机制：
//   - 父进程（start/restart）在持有 control lock 时生成一次性 instanceID，创建匿名单向
//     pipe；父持有 write end，child 继承 read end；pipe 不传业务数据，read end 的 EOF 只
//     表示「父级 control lease 已消失」。
//   - instanceID + child 侧 pipe fd/handle 通过三个内部环境变量传递：
//     TOKEN_USAGE_START_INSTANCE（instanceID）
//     POSIX:  TOKEN_USAGE_LEASE_FD（read end 的 fd，值为 3+i）
//     Windows:TOKEN_USAGE_LEASE_HANDLE（read end 的 handle 数值）
//   - child 启动 lease watcher 阻塞读 read end；watcher 与 daemon-lock 获取路径通过
//     同一互斥状态机提交 daemonLockAcquired：
//   - EOF 先发生 + daemon lock 未获得 → child 取消启动，不写 PID/runtime-state，退出。
//   - child 先获取 daemon lock（commit）→ 之后 EOF 只表示父命令结束，不再停止 daemon。
//   - 没有合法父 lease 的独立 _run（launchd/注册表直接拉起）忽略零散内部环境变量，自行
//     获取 control lock。
//
// 平台专属部分（pipe 创建、read end 传递、env 写值）在 lease_unix.go / lease_windows.go。
package control

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 内部环境变量名 ----
//
// 这三项是父进程 spawn child 时传递 lease 上下文的「私有协议」，绝不能被用户设置或
// 被无条件继承（残留会导致 child 误判有父 lease）。BuildChildEnv 总是先过滤这三项
// 再追加本次值。

const (
	// envInstance 传递一次性 instanceID。POSIX/Windows 共用。
	envInstance = "TOKEN_USAGE_START_INSTANCE"
	// envLeaseFD POSIX 专用：read end 在 child 中的 fd（值为 3+i，i 是 ExtraFiles 实际索引）。
	envLeaseFD = "TOKEN_USAGE_LEASE_FD"
	// envLeaseHandle Windows 专用：read end 的 handle 数值。
	envLeaseHandle = "TOKEN_USAGE_LEASE_HANDLE"
)

// leaseInternalEnvVars 是需要从 child env 中过滤的内部变量集合。
// BuildChildEnv/FilterLeaseEnvVars 据此无条件清除残留，避免 child 误判有父 lease。
var leaseInternalEnvVars = []string{envInstance, envLeaseFD, envLeaseHandle}

// ---- instanceID ----

// GenerateInstanceID 生成一次性守护进程实例标识（16 字节随机 → 32 字符 hex）。
// 父进程在持有 control lock 时生成并通过 envInstance 传给 child；child 在 daemon
// lock commit 后写入 PID 文件/runtime-state，供 start 握手确认是本次启动的进程。
// 用 crypto/rand 保证不可预测与全局唯一（不依赖时间戳）。
var instanceIDFallbackSequence atomic.Uint64

func GenerateInstanceID() string {
	return generateInstanceID(rand.Read, time.Now(), os.Getpid(), instanceIDFallbackSequence.Add(1))
}

func generateInstanceID(readRandom func([]byte) (int, error), now time.Time, pid int, sequence uint64) string {
	var b [16]byte
	if n, err := readRandom(b[:]); err == nil && n == len(b) {
		return hex.EncodeToString(b[:])
	}
	// 随机源异常时不能返回全零常量，否则连续 start 会复用同一代次。instanceID 不是
	// 安全凭证；用纳秒时间、PID 与进程内单调序列组成同样长度的唯一 fallback。
	return fmt.Sprintf("%016x%08x%08x", uint64(now.UnixNano()), uint32(pid), uint32(sequence))
}

// ---- env 过滤与构造 ----

// FilterLeaseEnvVars 从 env 中删除三项内部 lease 变量，保留其余（顺序不变）。
// 用于构造 child env 时先清掉可能残留的父进程内部启动上下文，避免 child 误判有父 lease。
func FilterLeaseEnvVars(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if isLeaseEnvVar(kv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isLeaseEnvVar 判断 kv 是否是三项内部 lease 变量之一（按 "KEY=" 前缀精确匹配）。
func isLeaseEnvVar(kv string) bool {
	for _, name := range leaseInternalEnvVars {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// BuildChildEnv 构造 child 进程的环境变量切片：
//  1. 先用 FilterLeaseEnvVars 清除 parentEnv 中的三项残留内部变量；
//  2. 追加 instanceID（envInstance）；
//  3. 调用 appendPlatformLeaseEnv 追加平台专属 lease 标识（POSIX fd / Windows handle）。
//
// appendPlatformLeaseEnv 由 lease_unix.go / lease_windows.go 提供，负责把 read end 的
// 实际 fd/handle 数值写入对应环境变量（POSIX 计算 3+i，Windows 写 handle 数值）。
// 返回的切片可直接赋给 cmd.Env。
func BuildChildEnv(parentEnv []string, instanceID string, appendPlatformLeaseEnv func(env []string) []string) []string {
	filtered := FilterLeaseEnvVars(parentEnv)
	filtered = append(filtered, envInstance+"="+instanceID)
	if appendPlatformLeaseEnv != nil {
		filtered = appendPlatformLeaseEnv(filtered)
	}
	return filtered
}

// ---- lease descriptor（child 侧解析）----

// leaseReader 抽象父 lease 的 read end：WaitForEOF 阻塞到 EOF（父 lease 消失），
// Close 释放底层资源。平台实现：POSIX=*os.File，Windows=handle 包装。
type leaseReader interface {
	WaitForEOF()
	Close()
}

// ParentLeaseDescriptor 描述从 child 环境变量解析出的父 lease 上下文。
type ParentLeaseDescriptor struct {
	// InstanceID 父进程生成的一次性标识（envInstance）。
	InstanceID string
	// reader 父 lease read end（已绑定平台 fd/handle），由 leaseReaderFromEnv 构造。
	reader leaseReader
}

// leasePlatformResolver 负责平台专属解析：从 env 中找出平台匹配的 lease 标识
// （POSIX fd / Windows handle）并构造 leaseReader。
//
// 抽象出来使 parseParentLease 可被平台无关测试（注入 fake resolver）覆盖；
// 生产实现 leaseReaderFromEnv 在 lease_unix.go / lease_windows.go。
type leasePlatformResolver interface {
	resolve(env []string) (leaseReader, bool)
}

// parseParentLease 从 env 解析父 lease 上下文。
//   - 必须同时存在 envInstance 与平台匹配的 lease 标识（POSIX fd / Windows handle）；
//   - 只有 instanceID、只有 lease 标识、或平台不匹配（如 POSIX 平台只出现 handle）→ ok=false，
//     调用方据此走「独立获取 control lock」路径（launchd/注册表直接拉起场景）。
//
// 零散/非法/平台不匹配的内部环境变量全部忽略（不部分信任）。
func parseParentLease(env []string) (ParentLeaseDescriptor, bool) {
	return parseParentLeaseWith(env, platformResolverAdapter{})
}

// ParseParentLease 是 parseParentLease 的导出版本，供 cli 包（_run）解析父 lease 上下文。
// 返回 desc.reader 用于启动 lease watcher；desc.InstanceID 用于 daemon.Run 的 InstanceID。
func ParseParentLease(env []string) (ParentLeaseDescriptor, bool) {
	return parseParentLease(env)
}

// LeaseReader 是 leaseReader 的导出接口，供 cli 包调用 waitForEOF/close。
type LeaseReader = leaseReader

// Reader 返回 desc 的 lease reader（child 侧 lease watcher 用）。
// 无父 lease 时返回 nil。
func (d ParentLeaseDescriptor) Reader() LeaseReader {
	return d.reader
}

// parseParentLeaseWith 注入平台 resolver 的 parseParentLease，便于平台无关测试。
func parseParentLeaseWith(env []string, resolver leasePlatformResolver) (ParentLeaseDescriptor, bool) {
	instanceID := lookupEnvValue(env, envInstance)
	if instanceID == "" {
		// 没有 instanceID：不存在合法父 lease 协议（即使有 fd/handle 也视为零散）。
		return ParentLeaseDescriptor{}, false
	}
	reader, ok := resolver.resolve(env)
	if !ok {
		// 有 instanceID 但平台 lease 标识缺失/不匹配：视为零散变量，忽略。
		return ParentLeaseDescriptor{}, false
	}
	return ParentLeaseDescriptor{InstanceID: instanceID, reader: reader}, true
}

// lookupEnvValue 在 env（"KEY=VALUE" 切片）中查找 key 对应的值，不存在返回空串。
func lookupEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// platformResolverAdapter 把包级函数 leaseReaderFromEnv 适配为 leasePlatformResolver。
// 生产用；测试直接实现 leasePlatformResolver 注入 fake。
type platformResolverAdapter struct{}

func (platformResolverAdapter) resolve(env []string) (leaseReader, bool) {
	return leaseReaderFromEnv(env)
}

// ---- leaseStateMachine（单一互斥状态机）----
//
// 核心不变量：EOF 与 daemon-lock-commit 必须由同一个互斥状态机
// 处理，回调恰好一次。禁止用两个无同步的 bool 形成竞态。
//
// 状态：
//   - 初始：既未 EOF 也未 commit。
//   - notifyEOF()：标记父 lease 消失。若尚未 commit → 关闭 leaseLost（触发取消）；
//     若已 commit → 无操作（child 已接管，EOF 只表示父命令结束）。
//   - markDaemonLockCommitted(onCommit)：标记 daemon lock 已获得。若尚未 EOF → 调用
//     onCommit 恰好一次（child 继续启动）；若已 EOF → 不调用 onCommit（取消优先）。
//
// leaseLost 一旦关闭不再重开（幂等）；onCommit 至多调用一次。

// leaseStateMachine 互斥处理 EOF 与 daemon-lock-commit 的状态机。
// 导出为 LeaseStateMachine（方法导出），供 cli 包（_run）使用。
type leaseStateMachine struct {
	mu          sync.Mutex
	eof         bool // EOF 是否已发生（父 lease 消失）
	committed   bool // daemon lock 是否已 commit
	leaseLostCh chan struct{}
}

// LeaseStateMachine 是 leaseStateMachine 的导出版本（同一类型）。
type LeaseStateMachine = leaseStateMachine

// newLeaseStateMachine 创建初始状态机（未 EOF、未 commit）。
func newLeaseStateMachine() *leaseStateMachine {
	return &leaseStateMachine{leaseLostCh: make(chan struct{})}
}

// NewLeaseStateMachine 是 newLeaseStateMachine 的导出版本，供 cli 包构造。
func NewLeaseStateMachine() *LeaseStateMachine {
	return newLeaseStateMachine()
}

// leaseLost 返回「父 lease 已消失且 daemon lock 未获得」的关闭信号。
// daemon.Run 通过 select 监听此 channel 决定是否取消启动；commit 后 EOF 不再关闭它。
func (s *leaseStateMachine) leaseLost() <-chan struct{} {
	return s.leaseLostCh
}

// LeaseLost 是 leaseLost 的导出版本，供 cli 包传递给 daemon.RunOptions.ParentLeaseLost。
func (s *LeaseStateMachine) LeaseLost() <-chan struct{} {
	return s.leaseLost()
}

// notifyEOF 标记父 lease 消失（read end EOF）。幂等：多次调用安全。
// 若 daemon lock 尚未 commit → 关闭 leaseLostCh（触发 child 取消启动）；
// 若已 commit → 无操作（child 已接管生命周期）。
func (s *leaseStateMachine) notifyEOF() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eof = true
	if !s.committed {
		// 取消优先：关闭 leaseLost（幂等——用 select-if-not-closed 模式）。
		s.closeLeaseLostLocked()
	}
}

// NotifyEOF 是 notifyEOF 的导出版本，供 cli 包的 lease watcher 调用。
func (s *LeaseStateMachine) NotifyEOF() {
	s.notifyEOF()
}

// markDaemonLockCommitted 标记 daemon lock 已成功获得。
// onCommit 在「尚未 EOF」时恰好调用一次（child 继续启动）；已 EOF 时不调用且不推进
// committed（取消优先）。onCommit 可为 nil（仅状态推进，不执行回调）。幂等：重复调用
// 在未 EOF 时只 commit 一次，在已 EOF 时永远返回 false 且不副作用。
//
// 返回值 committed：true=本次 commit 生效（child 继续启动）；false=EOF 已先到，取消优先
// （daemon.Run 据此释放刚获取的 daemon lock 并返回取消错误，不写 PID/runtime-state）。
//
// 注意：EOF 已先到时**不设置** committed=true。这让 isCancelled() 的语义「eof && !committed」
// 始终与 leaseLostCh 是否已关闭保持一致（EOF 先到时 notifyEOF 已关闭 leaseLostCh）。
// daemon.Run 实际用 ParentLeaseLost channel 检测取消，故此语义修正不影响其行为，
// 但 isCancelled 作为公开语义保持正确。
func (s *leaseStateMachine) markDaemonLockCommitted(onCommit func()) (committed bool) {
	s.mu.Lock()
	alreadyCommitted := s.committed
	alreadyEOF := s.eof
	if !alreadyEOF {
		// 仅在未 EOF 时推进 committed（取消优先：EOF 已先到则保持未 commit）。
		s.committed = true
	}
	s.mu.Unlock()
	if alreadyCommitted {
		return true // 幂等：之前已 commit（onCommit 在首次调用时执行过）。
	}
	if alreadyEOF {
		return false // 取消优先：EOF 已先到，不 commit。
	}
	if onCommit != nil {
		onCommit()
	}
	return true
}

// MarkDaemonLockCommitted 是 markDaemonLockCommitted 的导出版本，供 cli 包的
// OnDaemonLockCommit 回调调用。
func (s *LeaseStateMachine) MarkDaemonLockCommitted(onCommit func()) bool {
	return s.markDaemonLockCommitted(onCommit)
}

// isCancelled 返回是否因 EOF 先到而取消（daemon.Run 在获取 lock 前检查）。
func (s *leaseStateMachine) isCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eof && !s.committed
}

// closeLeaseLostLocked 关闭 leaseLostCh（调用方持锁）。幂等：已关闭则无操作。
func (s *leaseStateMachine) closeLeaseLostLocked() {
	select {
	case <-s.leaseLostCh:
		// 已关闭。
	default:
		close(s.leaseLostCh)
	}
}

// ---- 父进程侧 lease 上下文 ----
//
// leaseContext 由父进程（start/restart）在持有 control lock 时创建，封装：
//   - instanceID（一次性标识，传给 child）；
//   - leaseHandle：平台 pipe 持有者（POSIX leasePipeHolder / Windows leasePipeHolderWindows），
//     暴露 read end（传给 spawner）、closeWrite（ready 成功后释放）、cleanup（失败时清理）。
//
// spawner（productionSpawner）从 leaseContext.readerForDaemon() 取出 read end 载体，
// 构造 daemon.LeaseSpawnInput 传给 daemon.SpawnDetached。

// leaseHandle 抽象平台 pipe 持有者（POSIX/Windows），让 lease.go 平台无关。
// 实现在 lease_unix.go / lease_windows.go。
type leaseHandle interface {
	// reader 返回传给 daemon.SpawnDetached 的 read end 载体（POSIX *os.File / Windows syscall.Handle）。
	reader() interface{}
	// closeRead 在 child 成功继承后关闭父侧 read end；只由 control 层持有并调用，
	// 避免 spawn helper 与失败清理双方都关闭同一已可能复用的 fd/handle。
	closeRead()
	// closeWrite 关闭父侧 write end（ready 成功后释放 lease，触发 child EOF）。
	closeWrite()
	// cleanup 关闭两端（spawn 失败/ready 失败清理）。
	cleanup()
}

// leaseContext 父进程侧 lease 上下文（spawn 时传给 spawner，ready 后释放）。
type leaseContext struct {
	instanceID string
	handle     leaseHandle
}

// newLeaseContext 创建父进程 lease 上下文：用传入的 instanceID + 新建平台 pipe。
// 在持有 control lock 时调用：父进程 spawn 前生成 instanceID + pipe。
// instanceID 由调用方生成（startLocked 经 managerDependencies.instanceIDGen 注入，
// 默认 GenerateInstanceID；测试可注入确定性值供 ready 握手匹配）。
func newLeaseContext(instanceID string) (*leaseContext, error) {
	h, err := newLeasePipe()
	if err != nil {
		return nil, err
	}
	return &leaseContext{instanceID: instanceID, handle: h}, nil
}

// readerForDaemon 返回传给 daemon.LeaseSpawnInput.Reader 的 read end 载体。
func (c *leaseContext) readerForDaemon() interface{} {
	return c.handle.reader()
}

// closeWrite 关闭父侧 write end（ready 成功后释放 lease）。
func (c *leaseContext) closeWrite() {
	if c != nil && c.handle != nil {
		c.handle.closeWrite()
	}
}

func (c *leaseContext) closeRead() {
	if c != nil && c.handle != nil {
		c.handle.closeRead()
	}
}

// cleanup 关闭两端（spawn 失败/ready 失败清理）。
func (c *leaseContext) cleanup() {
	if c != nil && c.handle != nil {
		c.handle.cleanup()
	}
}
