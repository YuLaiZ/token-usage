// Package daemonstatus 提供采集守护状态与当前中断提示的纯判定逻辑：
// daemon status（table/JSON）与 serve 的 GET /api/daemon-status 共用同一份
// collection 对象与状态映射，保证两个读端同源。
//
// 判定输入是调用方已经采集好的证据快照（判活三态、当前实例阶段、退出观测
// 与结案证据）；本包不读文件、不查库、不做任何写操作。
package daemonstatus

import (
	"time"

	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// collection.state 的封闭值域。
const (
	StateStarting    = "starting"
	StateRunning     = "running"
	StateStopped     = "stopped"
	StateInterrupted = "interrupted"
	StateUnknown     = "unknown"
)

// CatchUp 值域（与 runmeta.RuntimeState.CatchUp 一致），外加 unknown。
const (
	CatchUpPending   = "pending"
	CatchUpRunning   = "running"
	CatchUpSucceeded = "succeeded"
	CatchUpFailed    = "failed"
	CatchUpUnknown   = "unknown"
)

// Probe 是 daemon lock 判活的三态结果：锁错误等探测失败返回 ProbeUnknown，
// 不与"确认未运行"混淆（现有布尔判活把锁错误保守视为运行中，不能直接用于
// 本判定的确定性展示）。
type Probe int

const (
	ProbeRunning Probe = iota
	ProbeStopped
	ProbeUnknown
)

// StopIntentWindow 是受控停止意图抑制读端中断误报的时间窗口：与写入端的
// 约定一致，超过窗口的残留意图不再掩盖中断。
const StopIntentWindow = time.Minute

// Interruption 是当前仍需提示的中断摘要（面向用户的安全字段：不含原始错误、
// 路径、堆栈或日志）。时刻为 RFC 3339，不可得为 null。Source 保持机器可读的
// 配置键契约，SourceDisplay 是同一来源的展示名（与守护进程摘要共用
// model.ClientKeyDisplayName 单一映射，读端直接展示即可）。
type Interruption struct {
	Kind          string  `json:"kind"`
	OccurredAt    *string `json:"occurred_at"`
	DetectedAt    *string `json:"detected_at"`
	Source        *string `json:"source"`
	SourceDisplay *string `json:"source_display"`
	Summary       string  `json:"summary"`
}

// Collection 是 Web 与 daemon status --format json 共用的 collection 对象。
type Collection struct {
	State        string        `json:"state"`
	CatchUp      *string       `json:"catch_up"`
	Interruption *Interruption `json:"interruption"`
}

// ExitEvidence 是 observation 中最近一条退出摘要的判定投影。
type ExitEvidence struct {
	InstanceID string
	Kind       string
	OccurredAt string
	DetectedAt string
	Source     string
	Summary    string
}

// Inputs 是判定所需的证据快照。
type Inputs struct {
	Probe Probe
	// 当前实例阶段：仅 ProbeRunning 且 PhaseAvailable 时可信（与 control.Inspect
	// 的 PID+instanceID 全匹配语义一致）。
	PhaseAvailable bool
	MonitorReady   bool
	CatchUp        string
	// 退出观测：ObservationReadable=false 表示缺失/损坏/不可读（不构造故障记录）。
	ObservationReadable bool
	HasActive           bool
	ActiveInstanceID    string
	// ActiveStartedUnixNano 是残留 active 的启动时刻（解析失败为 0）：与
	// last_exit 比较判断残留是否属于更早一代。
	ActiveStartedUnixNano int64
	// ActiveStopRequestedUnixNano 非零表示存在受控停止意图。
	ActiveStopRequestedUnixNano int64
	LastExit                    *ExitEvidence
	// LatestPending 是参与最近退出判定的本代库证据：有 active 残留时只取
	// 同实例候选，无同实例候选时为 nil（历史缺口改由 HistoricalPending
	// 提示）；DB 不可读时也为 nil（此时只按 LastExit 证据提示，写失败场景
	// 保守提示）。
	LatestPending *PendingInterruption
	// HistoricalPending 是不属于最近一次退出、但尚未结案的更早缺口（有 active
	// 且本代无库证据时由 selectPending 提供）：不参与 state 判定，只作为
	// interruption 继续提示，直到对应来源补采确认结案。
	HistoricalPending *PendingInterruption
	// PendingExit 是观察文件中尚未持久化到库的历史退出证据（最近一条）：
	// 数据库写入失败时它是历史缺口在文件里的唯一留存，作为 interruption
	// 的最后兜底提示，直到启动补写成功。
	PendingExit *ExitEvidence
	// LastExitKnownPending 是 last_exit 推导为 interrupted_unknown 时，库中
	// 同实例的持久故障证据：已知来源与发生时间优先于推导的"原因未知"。
	LastExitKnownPending *PendingInterruption
	// LastExitResolved 表示 LastExit 对应实例已有明确结案记录（DB 可读时才有值）。
	LastExitResolved bool
	// NowUnixNano 是判定时刻（入口统一取一次时钟）。
	NowUnixNano int64
}

// PendingInterruption 是 DB 未结案监控事件的最小投影。
type PendingInterruption struct {
	InstanceID string
	Kind       string
	OccurredAt string
	DetectedAt string
	Source     string
	Summary    string
}

// Evaluate 计算 collection 对象：state 先看当前实例，不能被旧退出摘要覆盖；
// 只在确认未运行时读可信退出证据。interruption 独立于 state——新实例运行/
// 补采期间，上一代尚未确认补齐的中断仍要提示。
func Evaluate(in Inputs) Collection {
	out := Collection{State: StateUnknown}
	switch in.Probe {
	case ProbeRunning:
		if in.PhaseAvailable {
			if in.MonitorReady {
				out.State = StateRunning
			} else {
				out.State = StateStarting
			}
			out.CatchUp = normalizeCatchUp(in.CatchUp)
		} else {
			// 实例阶段不可判定（PID 元数据缺失/不匹配）。
			out.CatchUp = ptr(CatchUpUnknown)
		}
	case ProbeUnknown:
		// 判活或阶段无法确认：不猜测，也不读退出证据把不确定判活判成中断。
		out.CatchUp = ptr(CatchUpUnknown)
	case ProbeStopped:
		// 确认未运行：读可信退出证据。
		out.State = stoppedState(in)
	}
	out.Interruption = currentInterruption(in)
	return out
}

// stoppedState 在确认未运行时把退出证据映射为 stopped/interrupted，按"最近
// 一次退出"归属证据：库中属于最近退出的事件（本代同实例或晚于 last_exit）
// 优先；否则同代 active 残留 > last_exit 的 kind 与结案证据；库中更早的
// 未结案事件只是历史缺口，不覆盖本次退出结论。
func stoppedState(in Inputs) string {
	if pendingIsLatest(in) {
		// 库中有属于最近一次退出的已知故障（观察文件写失败/被覆盖时它是
		// 唯一证据）：已知故障优先于推导出的原因未知与旧摘要。
		return StateInterrupted
	}
	if in.ObservationReadable && in.HasActive && activeIsLatest(in) &&
		(in.LastExit == nil || in.LastExit.InstanceID != in.ActiveInstanceID) {
		if stopIntentFresh(in) {
			return StateUnknown
		}
		return StateInterrupted
	}
	if in.LastExit != nil {
		if isInterruptedKind(in.LastExit.Kind) && !in.LastExitResolved {
			return StateInterrupted
		}
		return StateStopped
	}
	return StateStopped
}

// pendingIsLatest 判定库中最新未结案事件是否属于"最近一次退出"：
//   - 有同代 active 残留时按实例匹配（本代库证据优先于推导的原因未知）；
//   - 无 active 时与 last_exit 比较：同实例视为同代证据；不同实例时按
//     **原始故障发生时刻**与该代退出时刻排序——绝不用入库顺序（回填的旧
//     故障可能在停止后才入库，会被误当成本次退出）；
//   - 无 last_exit 时是唯一线索；发生时刻缺失、无法解析或与参照严格相等
//     （同一秒的两次退出）时不足以确定顺序，按历史缺口处理（不冒充）。
func pendingIsLatest(in Inputs) bool {
	p := in.LatestPending
	if p == nil {
		return false
	}
	if in.ObservationReadable && in.HasActive && in.ActiveInstanceID != "" && activeIsLatest(in) {
		return p.InstanceID == in.ActiveInstanceID
	}
	if in.LastExit == nil {
		return true
	}
	if p.InstanceID == in.LastExit.InstanceID {
		return true
	}
	if p.OccurredAt == "" {
		return false
	}
	pt, perr := time.Parse(time.RFC3339, p.OccurredAt)
	ref := in.LastExit.OccurredAt
	if ref == "" {
		ref = in.LastExit.DetectedAt
	}
	lt, lerr := time.Parse(time.RFC3339, ref)
	if perr != nil || lerr != nil {
		return false
	}
	return pt.After(lt)
}

// currentInterruption 计算"当前仍需提示"的中断，最近一次退出的证据优先：
//  1. 库中属于最近一次退出的已知故障（观察文件写失败/被覆盖时的唯一证据，
//     含来源与时间）；
//  2. 确认未运行时上一代 active 残留且无本代退出记录（只读推导，原因未知）；
//  3. last_exit 为未结案中断（含已知来源与时间）；
//  4. 更早的历史缺口——保留提示（补采确认前不撤），不冒充最近一次的退出原因。
func currentInterruption(in Inputs) *Interruption {
	if pendingIsLatest(in) {
		return &Interruption{
			Kind:          kindOrDefault(in.LatestPending.Kind),
			OccurredAt:    optionalString(in.LatestPending.OccurredAt),
			DetectedAt:    optionalString(in.LatestPending.DetectedAt),
			Source:        optionalString(in.LatestPending.Source),
			SourceDisplay: displayOf(in.LatestPending.Source),
			Summary:       in.LatestPending.Summary,
		}
	}
	if in.Probe == ProbeStopped && in.ObservationReadable && in.HasActive && activeIsLatest(in) &&
		(in.LastExit == nil || in.LastExit.InstanceID != in.ActiveInstanceID) && !stopIntentFresh(in) {
		return &Interruption{
			Kind:    "interrupted_unknown",
			Summary: ui.Bi("collection interrupted, exit reason unknown", "采集已中断，退出原因未知"),
		}
	}
	// 启动时上一代 active 残留会被推导为 interrupted_unknown；若库中已有同
	// 实例的持久故障证据，已知来源与发生时间优先，不显示推导出的"原因未知"。
	if p := in.LastExitKnownPending; p != nil {
		return &Interruption{
			Kind:          kindOrDefault(p.Kind),
			OccurredAt:    optionalString(p.OccurredAt),
			DetectedAt:    optionalString(p.DetectedAt),
			Source:        optionalString(p.Source),
			SourceDisplay: displayOf(p.Source),
			Summary:       p.Summary,
		}
	}
	if in.LastExit != nil && isInterruptedKind(in.LastExit.Kind) && !in.LastExitResolved {
		return &Interruption{
			Kind:          in.LastExit.Kind,
			OccurredAt:    optionalString(in.LastExit.OccurredAt),
			DetectedAt:    optionalString(in.LastExit.DetectedAt),
			Source:        optionalString(in.LastExit.Source),
			SourceDisplay: displayOf(in.LastExit.Source),
			Summary:       in.LastExit.Summary,
		}
	}
	if p := historicalPending(in); p != nil {
		return &Interruption{
			Kind:          kindOrDefault(p.Kind),
			OccurredAt:    optionalString(p.OccurredAt),
			DetectedAt:    optionalString(p.DetectedAt),
			Source:        optionalString(p.Source),
			SourceDisplay: displayOf(p.Source),
			Summary:       p.Summary,
		}
	}
	// 陈旧 active 残留（属于更早一代）不覆盖最近一次退出，但它的未知中断
	// 仍作为历史提示保留。
	if in.ObservationReadable && in.HasActive && in.ActiveInstanceID != "" && !activeIsLatest(in) && !stopIntentFresh(in) {
		return &Interruption{
			Kind:    "interrupted_unknown",
			Summary: ui.Bi("earlier collection interruption, exit reason unknown", "更早的采集曾中断，退出原因未知"),
		}
	}
	return nil
}

// activeIsLatest 判定残留 active 是否属于最近一次退出：有 last_exit 时，只有
// active 的启动时刻严格早于 last_exit 的参照时刻（新代登记必然晚于上一代退出
// 写）才视为更早一代的陈旧残留；同一秒无法区分顺序，维持既有优先（把 active
// 当最近退出）。任一时刻缺失或不可解析时同样维持既有优先。
// 时间比较依赖墙上时钟：系统时钟大幅回拨时，新代 StartedAt 可能早于上一代
// 退出时刻而被误判为陈旧（本次中断只剩历史提示）；这是与写端 staleActiveBefore
// 一致的低概率取舍，无法比的时刻一律按最新处理。
func activeIsLatest(in Inputs) bool {
	if !in.ObservationReadable || !in.HasActive || in.ActiveInstanceID == "" {
		return false
	}
	if in.LastExit == nil || in.ActiveStartedUnixNano == 0 {
		return true
	}
	ref := in.LastExit.OccurredAt
	if ref == "" {
		ref = in.LastExit.DetectedAt
	}
	lt, err := time.Parse(time.RFC3339, ref)
	if err != nil {
		return true
	}
	return in.ActiveStartedUnixNano >= lt.UnixNano()
}

// historicalPending 返回不属于最近一次退出、但仍需提示的历史缺口：库候选
// （HistoricalPending，或没有 active 时按时刻选出的 LatestPending）与观察
// 文件中尚未持久化的待补写证据按发生时刻取较新者——库中较旧的缺口不能
// 长期遮住文件里更新的缺口；时刻缺失或不可解析时维持库证据优先。
func historicalPending(in Inputs) *PendingInterruption {
	dbPending := in.HistoricalPending
	if dbPending == nil {
		dbPending = in.LatestPending
	}
	pe := in.PendingExit
	if pe == nil || !isInterruptedKind(pe.Kind) {
		return dbPending
	}
	filePending := &PendingInterruption{
		InstanceID: pe.InstanceID,
		Kind:       pe.Kind,
		OccurredAt: pe.OccurredAt,
		DetectedAt: pe.DetectedAt,
		Source:     pe.Source,
		Summary:    pe.Summary,
	}
	if dbPending == nil {
		return filePending
	}
	if occurredAfter(pe.OccurredAt, dbPending.OccurredAt) {
		return filePending
	}
	return dbPending
}

// occurredAfter 比较两个 RFC 3339 时刻：a 严格晚于 b 时返回 true；任一缺失
// 或不可解析返回 false（不可比较时维持既有优先级，不按入库顺序猜测）。
func occurredAfter(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	at, aerr := time.Parse(time.RFC3339, a)
	bt, berr := time.Parse(time.RFC3339, b)
	if aerr != nil || berr != nil {
		return false
	}
	return at.After(bt)
}

func isInterruptedKind(kind string) bool {
	return kind == "failed" || kind == "interrupted_unknown"
}

func kindOrDefault(kind string) string {
	if isInterruptedKind(kind) {
		return kind
	}
	return "interrupted_unknown"
}

func stopIntentFresh(in Inputs) bool {
	if in.ActiveStopRequestedUnixNano == 0 || in.NowUnixNano == 0 {
		return false
	}
	delta := time.Duration(in.NowUnixNano - in.ActiveStopRequestedUnixNano)
	return delta >= 0 && delta <= StopIntentWindow
}

func normalizeCatchUp(v string) *string {
	switch v {
	case CatchUpPending, CatchUpRunning, CatchUpSucceeded, CatchUpFailed:
		return ptr(v)
	default:
		return ptr(CatchUpUnknown)
	}
}

func ptr(v string) *string { return &v }

func optionalString(v string) *string {
	if v == "" {
		return nil
	}
	return ptr(v)
}

// displayOf 返回来源配置键的展示名（与守护进程退出摘要共用同一映射）；
// 来源缺失时为 nil，不编造。
func displayOf(source string) *string {
	if source == "" {
		return nil
	}
	return ptr(model.ClientKeyDisplayName(source))
}
