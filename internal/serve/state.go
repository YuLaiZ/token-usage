// internal/serve/state.go
package serve

// state.go 实现 serve.json 运行状态文件：记录当前仪表板服务进程的 PID、监听
// 地址与启动时间，供 serve start/status/stop、后台服务主体与 update 的运行态
// 探测共用（服务主体写/删同一状态文件，status 与 update 据此判定）。
// 本文件同时定义状态迁移锁的获取/释放 helper 与条件删除原语 RemoveStateIfSame。
//
// 文件位于配置的 DataDir 下，固定名 serve.json（单实例语义，不掺日期）；写入
// 经 fileutil.ReplaceCompleteFile 原子替换。读取对缺失返回「无后台状态」
// （nil, nil）；存在但 JSON 损坏或字段无效则返回哨兵 ErrStateCorrupt——
// 这类文件无法定位实例但确是残留，由调用方按统一承诺清理：status/stop 删除后
// 报告未运行（exit 0），start 删除后照常启动。update 的运行态探测对损坏状态
// 一律判为未运行，且绝不修改状态文件。其余 I/O 错误（如权限拒绝）仍作为错误
// 返回。
//
// 状态迁移不变量：serve.json 的所有「读-判定-写/删」迁移都必须在 serve-state
// 状态迁移锁内进行，且删除一律走 RemoveStateIfSame 条件删除（锁内重读比对
// 一致才删）。否则「读旧状态→探活判定陈旧→删除」与「新实例取得 serve.lock、
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

// ServeState 是 serve.json 的磁盘形态。StartedAt 为 RFC3339 本机时区字符串。
type ServeState struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt string `json:"started_at"`
}

// StatePath 返回状态文件完整路径。
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, StateFile)
}

// LogPath 返回后台日志文件完整路径。
func LogPath(dataDir string) string {
	return filepath.Join(dataDir, LogFile)
}

// WriteState 原子写出状态文件（0644）。
func WriteState(dataDir string, st *ServeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal serve state: %w", err)
	}
	return fileutil.ReplaceCompleteFile(StatePath(dataDir), data, 0644)
}

// ErrStateCorrupt 表示 serve.json 存在但内容损坏或字段无效：与缺失不同，
// 它是确实留下的残留文件，只是无法辨识出实例信息。调用方（status/stop/start）
// 对它的统一处置是删除残留后按「未运行」继续，与陈旧清理同一承诺。
var ErrStateCorrupt = errors.New("corrupt serve state")

// ReadState 读取状态文件。文件缺失返回 (nil, nil)；存在但 JSON 损坏或
// 字段无效（PID 非正、Addr 为空、StartedAt 非 RFC3339）返回
// (nil, ErrStateCorrupt)，由调用方清理残留后按未运行处理；其余 I/O 错误
// （如权限拒绝）仍作为错误返回。StartedAt 的格式校验保证 serve status
// --format json 对 started_at 的 RFC3339 承诺：无法解析的时间戳与损坏的
// JSON 同属「无法辨识的残留」，走统一的损坏清理出口。
func ReadState(dataDir string) (*ServeState, error) {
	data, err := os.ReadFile(StatePath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read serve state %q: %w", StatePath(dataDir), err)
	}
	var st ServeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, ErrStateCorrupt
	}
	if st.PID <= 0 || st.Addr == "" {
		return nil, ErrStateCorrupt
	}
	if _, err := time.Parse(time.RFC3339, st.StartedAt); err != nil {
		return nil, ErrStateCorrupt
	}
	return &st, nil
}

// RemoveState 删除状态文件，容忍文件已不存在。
func RemoveState(dataDir string) error {
	err := os.Remove(StatePath(dataDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove serve state %q: %w", StatePath(dataDir), err)
	}
	return nil
}

// stateLockRetries / stateLockRetryGap 是状态迁移锁带界重试的参数
// （包级 var 供测试按需缩短，保持用例确定性无长睡眠）。
var (
	stateLockRetries  = 5
	stateLockRetryGap = 20 * time.Millisecond
)

// ErrStateBusy 表示 serve-state 状态迁移锁在带界重试耗尽后仍被他人持有。
// 普通写/删持锁为毫秒级；status/stop 的探活判定段合法持锁可达探活超时的秒级，
// 而重试窗口（5 × 20ms）远小于它——并发状态操作互遇时以 busy 非零失败属预期
// 行为，调用方（含脚本）应容忍并重试。
var ErrStateBusy = errors.New(ui.Bi("serve state file is busy; retry", "状态文件忙，请重试"))

// AcquireStateLock 获取 dataDir 下的 serve-state 状态迁移锁：TryLock 失败
// 后按 stateLockRetries × stateLockRetryGap 退避重试（带界阻塞获取，
// 不无限等待），耗尽后返回 ErrStateBusy。锁经 gofrs/flock 实现，跨平台可用。
func AcquireStateLock(dataDir string) (*flock.Flock, error) {
	fl := flock.New(filepath.Join(dataDir, StateLockFile))
	var lastErr error
	for i := 0; i < stateLockRetries; i++ {
		locked, err := fl.TryLock()
		if err == nil && locked {
			return fl, nil
		}
		lastErr = err
		if i < stateLockRetries-1 {
			time.Sleep(stateLockRetryGap)
		}
	}
	if lastErr != nil {
		// TryLock 本身报错（如权限拒绝）：带上底层原因，busy 语义不变。
		return nil, fmt.Errorf("%s: %w", ErrStateBusy, lastErr)
	}
	return nil, ErrStateBusy
}

// ReleaseStateLock 释放状态迁移锁（AcquireStateLock 的成对操作）。
func ReleaseStateLock(fl *flock.Flock) error {
	if fl == nil {
		return nil
	}
	return fl.Unlock()
}

// RemoveStateIfSame 是「锁内重读比对、一致才删」的条件删除原语：在
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
var RemoveStateIfSame = func(dataDir string, judged *ServeState) (removed bool, current *ServeState, err error) {
	cur, err := ReadState(dataDir)
	switch {
	case errors.Is(err, ErrStateCorrupt):
		// 损坏文件无法辨识出任何实例，不构成「新实例已接管」：直接删除。
		if rmErr := RemoveState(dataDir); rmErr != nil {
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
		if rmErr := RemoveState(dataDir); rmErr != nil {
			return false, nil, rmErr
		}
		return true, nil, nil
	}
	// 磁盘已是另一份有效状态（新实例已接管）：不删除，交调用方对新状态重评估。
	return false, cur, nil
}

// MetaAlive 探活仪表板 /api/meta：HTTP 2xx 视为存活，连接失败、超时或
// 其他状态码均视为不存活。baseURL 形如 "http://127.0.0.1:8619"。
func MetaAlive(baseURL string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimSuffix(baseURL, "/") + "/api/meta")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// StaleProbeTimeout 是判定已记录实例是否仍存活的单次探活超时，单实例守卫
// （LifecycleGuard）、serve start 的已运行预检、doctor 的仪表板检查与 update
// 的更新前探测共用。
var StaleProbeTimeout = 1500 * time.Millisecond
