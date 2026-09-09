// internal/cli/serve_state.go
package cli

// serve_state.go 实现 `token-usage serve` 的运行状态文件 serve.json：记录当前
// 仪表板服务进程的 PID、监听地址与启动时间，供 serve start/status/stop 与前台
// serve 共用（前台 serve 也写/删同一状态文件，status 对两者一视同仁）。
// 本文件同时定义 serve 家族在 DataDir 下的三把文件锁固定名（角色分工见常量
// 注释）与状态迁移锁的获取/释放 helper、条件删除原语 removeServeStateIfSame。
//
// 文件位于配置的 DataDir 下，固定名 serve.json（单实例语义，不掺日期）；写入
// 经 fileutil.ReplaceCompleteFile 原子替换。读取对缺失返回「无后台状态」
// （nil, nil）；存在但 JSON 损坏或字段无效则返回哨兵 errServeStateCorrupt——
// 这类文件无法定位实例但确是残留，由调用方按统一承诺清理：status/stop 删除后
// 报告未运行（exit 0），start 删除后照常启动。其余 I/O 错误（如权限拒绝）
// 仍作为错误返回。
//
// 状态迁移不变量：serve.json 的所有「读-判定-写/删」迁移都必须在 serve-state
// 状态迁移锁内进行，且删除一律走 removeServeStateIfSame 条件删除（锁内重读比
// 对一致才删）。否则「读旧状态→探活判定陈旧→删除」与「新实例取得 serve.lock、
// 完成监听、写出新 serve.json」交错时，会误删新实例的状态，把唯一运行实例变成
// 持锁却无法被 status/stop 定位的孤儿。
//
// 锁序全局约定（无环）：
//   - 服务主体 serveDashboard：serve.lock（整生命周期）→ state.lock（毫秒级，
//     写状态/关停自删时短暂持有）；守卫判定段先取再释放 state.lock，之后才取
//     serve.lock，两锁从不同时持有。
//   - status/stop/start：只取 state.lock，不取 serve.lock；start 父进程的
//     serve-start.lock 与 state.lock 无嵌套（预检段取 state.lock，spawn 前
//     必须已释放——子进程写状态需取同一把锁）。
//   - 任何路径都不存在 state.lock → serve.lock 的持锁顺序。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// serveStateFile 是 DataDir 下的状态文件固定名。
const serveStateFile = "serve.json"

// serveLogFile 是后台运行日志文件固定名：serve start 每次启动截断重建，
// 后台 _serve-run 的 stdout/stderr 都重定向到这里。
const serveLogFile = "serve.log"

// serveLifecycleLockFile、serveStartLockFile 与 serveStateLockFile 是 DataDir
// 下的三把文件锁固定名（gofrs/flock 跨平台），共同支撑 serve 的单实例契约，
// 角色分工如下：
//   - serve.lock 是生命周期锁：由服务主体（前台 serve 进程或后台 _serve-run
//     子进程）经 serveDashboard 的单实例守卫获取，在整个服务生命周期持有。
//     它挡住「另一个实例正在启动」的瞬时竞态；「已有实例在运行」的稳态由
//     serve.json + /api/meta 探活判定。
//   - serve-start.lock 是 start 串行化锁：仅 serve start 父进程在预检 → spawn →
//     确认期间持有，串行化并发 start；它不表达运行状态。
//   - serve-state.lock 是状态迁移锁：串行化 serve.json 的所有「读-判定-写/删」
//     迁移（见文件头「状态迁移不变量」），持有时间为毫秒级（仅 status/stop 的
//     探活判定段可达探活超时的秒级）；它也不表达运行状态。
//
// 三把锁的文件均可残留（flock 随进程退出自动释放），残留文件本身无含义。
const (
	serveLifecycleLockFile = "serve.lock"
	serveStartLockFile     = "serve-start.lock"
	serveStateLockFile     = "serve-state.lock"
)

// ServeState 是 serve.json 的磁盘形态。StartedAt 为 RFC3339 本机时区字符串。
type ServeState struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt string `json:"started_at"`
}

// serveStatePath 返回状态文件完整路径。
func serveStatePath(dataDir string) string {
	return filepath.Join(dataDir, serveStateFile)
}

// serveLogPath 返回后台日志文件完整路径。
func serveLogPath(dataDir string) string {
	return filepath.Join(dataDir, serveLogFile)
}

// writeServeState 原子写出状态文件（0644）。
func writeServeState(dataDir string, st *ServeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal serve state: %w", err)
	}
	return fileutil.ReplaceCompleteFile(serveStatePath(dataDir), data, 0644)
}

// errServeStateCorrupt 表示 serve.json 存在但内容损坏或字段无效：与缺失不同，
// 它是确实留下的残留文件，只是无法辨识出实例信息。调用方（status/stop/start）
// 对它的统一处置是删除残留后按「未运行」继续，与陈旧清理同一承诺。
var errServeStateCorrupt = errors.New("corrupt serve state")

// readServeState 读取状态文件。文件缺失返回 (nil, nil)；存在但 JSON 损坏或
// 字段无效（PID 非正、Addr 为空）返回 (nil, errServeStateCorrupt)，由调用方
// 清理残留后按未运行处理；其余 I/O 错误（如权限拒绝）仍作为错误返回。
func readServeState(dataDir string) (*ServeState, error) {
	data, err := os.ReadFile(serveStatePath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read serve state %q: %w", serveStatePath(dataDir), err)
	}
	var st ServeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, errServeStateCorrupt
	}
	if st.PID <= 0 || st.Addr == "" {
		return nil, errServeStateCorrupt
	}
	return &st, nil
}

// removeServeState 删除状态文件，容忍文件已不存在。
func removeServeState(dataDir string) error {
	err := os.Remove(serveStatePath(dataDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove serve state %q: %w", serveStatePath(dataDir), err)
	}
	return nil
}

// serveStateLockRetries / serveStateLockRetryGap 是状态迁移锁带界重试的参数
// （包级 var 供测试按需缩短，保持用例确定性无长睡眠）。
var (
	serveStateLockRetries  = 5
	serveStateLockRetryGap = 20 * time.Millisecond
)

// errServeStateBusy 表示 serve-state 状态迁移锁在带界重试耗尽后仍被他人持有。
// 普通写/删持锁为毫秒级；status/stop 的探活判定段合法持锁可达探活超时的秒级，
// 而重试窗口（5 × 20ms）远小于它——并发状态操作互遇时以 busy 非零失败属预期
// 行为，调用方（含脚本）应容忍并重试。
var errServeStateBusy = errors.New(ui.Bi("serve state file is busy; retry", "状态文件忙，请重试"))

// acquireServeStateLock 获取 dataDir 下的 serve-state 状态迁移锁：TryLock 失败
// 后按 serveStateLockRetries × serveStateLockRetryGap 退避重试（带界阻塞获取，
// 不无限等待），耗尽后返回 errServeStateBusy。锁经 daemon.AcquireLock 同源的
// gofrs/flock 实现，跨平台可用。
func acquireServeStateLock(dataDir string) (*flock.Flock, error) {
	fl := flock.New(filepath.Join(dataDir, serveStateLockFile))
	var lastErr error
	for i := 0; i < serveStateLockRetries; i++ {
		locked, err := fl.TryLock()
		if err == nil && locked {
			return fl, nil
		}
		lastErr = err
		if i < serveStateLockRetries-1 {
			time.Sleep(serveStateLockRetryGap)
		}
	}
	if lastErr != nil {
		// TryLock 本身报错（如权限拒绝）：带上底层原因，busy 语义不变。
		return nil, fmt.Errorf("%s: %w", errServeStateBusy, lastErr)
	}
	return nil, errServeStateBusy
}

// releaseServeStateLock 释放状态迁移锁（acquireServeStateLock 的成对操作）。
func releaseServeStateLock(fl *flock.Flock) error {
	if fl == nil {
		return nil
	}
	return fl.Unlock()
}

// removeServeStateIfSame 是「锁内重读比对、一致才删」的条件删除原语：在
// serve-state 状态迁移锁内重读状态文件并与 judged 比较，内容结构化等价
// （PID/Addr/StartedAt 三字段一致）才删除，返回 (true, nil, nil)；文件已被
// 新实例改写为其他有效状态时不删除，返回 (false, 当前状态, nil) 交调用方重新
// 评估；文件缺失视为已删，返回 (true, nil, nil)（无副作用）；I/O 错误原样上抛；
// 损坏文件没有新实例语义，直接删除并返回 (true, nil, nil)（与既有 corrupt
// 清理契约一致）。
//
// 前置条件：调用方必须已持有 dataDir 的 serve-state 锁——原语自身不取锁，
// 避免与外层持锁段嵌套获取（POSIX flock 不同 fd 互斥，嵌套会自死锁）。原语
// 是包级 var 仅为测试注入 seam（模拟「判定与删除之间新实例已接管」的时序），
// 生产实现即函数体。
var removeServeStateIfSame = func(dataDir string, judged *ServeState) (removed bool, current *ServeState, err error) {
	cur, err := readServeState(dataDir)
	switch {
	case errors.Is(err, errServeStateCorrupt):
		// 损坏文件无法辨识出任何实例，不构成「新实例已接管」：直接删除。
		if rmErr := removeServeState(dataDir); rmErr != nil {
			return false, nil, rmErr
		}
		return true, nil, nil
	case err != nil:
		return false, nil, err
	case cur == nil:
		// 文件已缺失：等价于目标状态已被删除，无副作用。
		return true, nil, nil
	}
	if judged != nil && *cur == *judged {
		// 磁盘内容仍与判定所据一致：确实是这条陈旧状态，删除。
		if rmErr := removeServeState(dataDir); rmErr != nil {
			return false, nil, rmErr
		}
		return true, nil, nil
	}
	// 磁盘已是另一份有效状态（新实例已接管）：不删除，交调用方对新状态重评估。
	return false, cur, nil
}

// serveMetaAlive 探活仪表板 /api/meta：HTTP 2xx 视为存活，连接失败、超时或
// 其他状态码均视为不存活。baseURL 形如 "http://127.0.0.1:8619"。
func serveMetaAlive(baseURL string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimSuffix(baseURL, "/") + "/api/meta")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
