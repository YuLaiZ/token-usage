package update

import (
	"context"
	"errors"
	"fmt"
)

// process_identity.go 实现「按显式进程身份（PID + 创建时间）等待进程退出」的平台无关决策。
//
// 背景：Windows 不允许替换运行中的 .exe。自更新 staged replacement 由父进程 spawn
// 后台 helper，helper 必须等父进程退出后才能替换旧 .exe；替换成功后 helper 再 spawn
// cleanup，cleanup 等_helper_退出后清理临时文件。两条等待路径都不能只靠裸 PID——
// 操作系统会回收 PID，若等待期间原进程退出、PID 被复用给无关进程，裸 PID 等待会误等
// 新进程（TOCTOU），可能导致永不退出或等待错误对象。
//
// 解决：等待方持有一份显式身份（PID + 进程创建时刻），打开句柄后比对创建时刻：
//   - 进程不存在 → 原进程已退出，安全继续；
//   - 创建时刻不匹配 → PID 已被复用，原进程已退出，安全继续；
//   - 创建时刻匹配 → 句柄绑定原进程实例，等其 signaled，不受 PID 回收影响；
//   - 无法确认身份（access denied / 查询失败 / 等待失败 / 超时 / 取消）→ 失败，绝不继续。
//
// 本文件只定义决策逻辑与 seam 接口；真实 Windows API 实现在 helper_seams_windows.go
// （build tag windows）。决策逻辑经 fake probe + fake handle 在 macOS 单元测试覆盖。

// ProcessIdentity 唯一标识一个进程实例：PID + 创建时刻。
// CreationTime 在 Windows 上是 GetProcessTimes 返回的 creation FILETIME 原始 64 位值，
// 两个值相等当且仅当指向同一进程实例（同 PID 不同实例创建时刻必然不同）。
type ProcessIdentity struct {
	PID          uint32 `json:"pid"`
	CreationTime uint64 `json:"creation_time"`
}

// Valid 报告身份是否为合法非零值（PID>0 且 CreationTime>0）。
// 零值身份无法可靠等待，必须在校验链中拒绝（防降级或缺失）。
func (i ProcessIdentity) Valid() bool {
	return i.PID > 0 && i.CreationTime > 0
}

// errProcessGone 是 OpenForWait 在「目标进程已不存在」时返回的哨兵错误。
// 调用方用 IsProcessGone 判定，区别于 access denied 等无法确认身份的错误。
var errProcessGone = errors.New("process gone")

// IsProcessGone 报告 err 是否表示「目标进程已不存在」（含包装）。
func IsProcessGone(err error) bool {
	return err != nil && errors.Is(err, errProcessGone)
}

// ProcessWaitHandle 抽象「一个已打开的进程句柄」。
// 生产实现（windows tag）封装 windows.Handle；测试用 fake 记录调用。
type ProcessWaitHandle interface {
	// CreationTime 返回进程创建时刻（Windows FILETIME 原始 uint64）。
	CreationTime() (uint64, error)
	// Wait 阻塞至句柄 signaled（进程退出）；ctx 取消/超时/等待错误返回非 nil。
	Wait(ctx context.Context) error
	// Close 释放句柄。幂等。
	Close()
}

// ProcessProbe 抽象「按 PID 打开可等待且可查询创建时间的句柄」。
// 生产实现（windows tag）调用 windows.OpenProcess；测试用 fake 注入。
type ProcessProbe interface {
	// OpenForWait 以 SYNCHRONIZE + 查询创建时间所需权限打开 pid。
	// 进程不存在 → 返回 (nil, errProcessGone)（哨兵，可被 IsProcessGone 判定）。
	// 其它任何错误（含 access denied）→ 返回 (nil, 非 gone 错误)。
	OpenForWait(pid uint32) (ProcessWaitHandle, error)
}

// WaitProcessIdentity 等待 identity 指定的进程实例退出，按以下顺序决策：
//  1. probe.OpenForWait(identity.PID)；
//  2. gone（进程不存在）→ nil（视为已退出，继续后续操作）；
//  3. 非 gone 的 OpenForWait 错误 → 失败（无法确认身份，绝不当作已退出）；
//  4. handle.CreationTime 错误 → 关闭句柄并失败；
//  5. 创建时间与 identity.CreationTime 不等（PID 复用）→ 关闭句柄，返回 nil（原进程已退出）；
//  6. 创建时间匹配 → handle.Wait(ctx)；其错误（含 ctx 取消/超时）→ 失败。
//
// 本函数同时服务 helper 等待父进程（identity = plan.Parent）和 cleanup 等待 helper
// （identity = helper 自身），一处实现两处复用。
func WaitProcessIdentity(ctx context.Context, probe ProcessProbe, identity ProcessIdentity) error {
	handle, err := probe.OpenForWait(identity.PID)
	if err != nil {
		if IsProcessGone(err) {
			return nil // 步骤2：进程已不存在，安全继续。
		}
		// 步骤3：access denied 等，无法确认身份 → 失败。
		return fmt.Errorf("打开进程 %d 失败: %w", identity.PID, err)
	}
	defer handle.Close()

	ct, err := handle.CreationTime()
	if err != nil {
		// 步骤4：查询创建时间失败 → 失败（无法确认身份）。
		return fmt.Errorf("查询进程 %d 创建时间失败: %w", identity.PID, err)
	}
	if ct != identity.CreationTime {
		// 步骤5：PID 已被复用，原进程已退出 → 安全继续。
		return nil
	}

	// 步骤6：身份匹配，等句柄 signaled。
	if err := handle.Wait(ctx); err != nil {
		return fmt.Errorf("等待进程 %d 退出失败: %w", identity.PID, err)
	}
	return nil
}
