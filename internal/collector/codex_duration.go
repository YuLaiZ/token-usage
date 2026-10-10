package collector

import (
	"encoding/json"
	"strings"
)

// Codex 请求时长估算状态机：逐行对齐上游 timing 状态机语义移植
// （事件分类 → observe → token_count 落定时结算）。
//
// Codex 会话日志没有请求级计时，按事件顺序估算：起点=本次请求第一个输出项
// 之前最近的起点行（上一次 token_count、工具结果、用户消息、turn_context），
// 终点优先匹配本次用量的 token_usage_record 记录；旧日志在工具完成后才写
// token_count 时改用最后一个模型输出时刻（模型输出 5s + 工具 60s 不得算成
// 65s）。被中断（turn_aborted/task_started 后无 token_count）的请求起点在
// 下一轮开始时作废，不得被下一次请求沿用。

// codexTimingKind 是参与时长估算的事件分类。
type codexTimingKind int

const (
	codexTimingBoundary    codexTimingKind = iota // 用户消息（非 assistant）、turn_context
	codexTimingToolOutput                         // 工具结果（*_output）
	codexTimingModelOutput                        // 模型输出项（assistant 消息、reasoning、*_call）
	codexTimingUsageRecord                        // token_usage_record（响应结束时的用量记录，顶层 type）
	codexTimingTaskStarted                        // task_started：新一轮开始（未等到 token_count 的请求作废）
	codexTimingTurnAborted                        // turn_aborted：请求被中断（同样作废，但语义是回到旧时段残余）
)

// noteReplayedTokenCount 记录被去重丢弃的重播 token_count 时刻：它仍是
// 「最新 token_count」这一事实，作为旧布局补写输出识别（trails-token-count）
// 的时间锚——否则锚点停留在上一条被接受的计数事件，较晚重播后紧跟的旧
// 输出补写会被误判为新请求首输出并提前锁定旧边界，污染下一请求的起点。
// 信号守卫：新请求开始信号（晚于锚的用户消息/turn_context/task_started）
// 出现后到达的重播属新请求时段，不更新锚（其同批输出是新请求首输出）；
// turn_aborted 后的重播属旧时段残余，仍更新锚。只推进锚，不产出时长
// （重播不是一次请求）。
func (t *codexRequestTimer) noteReplayedTokenCount(tsMS int64) {
	if t.newRequestSeen {
		return // 新请求时段内的重播不构成补写锚
	}
	v := tsMS
	t.lastTokenCountMS = &v
}

// codexSameFlushSlackMS 是「同一批写出」的毫秒容差：实测相差 0–1 毫秒，
// 一次真实请求不可能这么快。用于识别旧布局把上轮输出项补写在 token_count
// 之后（不作下一轮起点）与 token_count 紧跟工具结果（终点改用最后输出时刻）。
const codexSameFlushSlackMS int64 = 100

// codexRequestTimer 是时长估算的逐事件状态（Option 语义用指针表达）。
type codexRequestTimer struct {
	lastBoundaryMS *int64 // 最近起点行时刻
	// lastTokenCountMS 是上一条 token_count 时刻（旧布局补写输出的比对锚）。
	lastTokenCountMS *int64
	// newRequestSeen 报告自上次结算/中断以来是否出现过「新请求开始」信号
	//（晚于锚的用户消息、turn_context、task_started）。该信号之后到达的
	// 重播快照属新请求时段——其同批输出是新请求首输出，重播不得作补写锚
	//（否则吞掉首输出）；turn_aborted 作废当前请求、回到旧时段残余状态，
	// 清除该信号，其后到达的旧快照重播仍构成补写锚。
	newRequestSeen         bool
	requestStartMS         *int64 // 本次请求起点（首输出项锁定）
	lastModelOutputMS      *int64 // 最后一个模型输出项时刻
	toolOutputAfterModelMS *int64 // 最后输出项之后出现的工具结果里最晚的一个（再遇输出项清空）
	usageRecord            *codexUsage
	usageRecordMS          *int64
}

// codexTimingOptionMax 复刻 Option<i64>::max：None 或更大值时取新值。
func codexTimingOptionMax(a *int64, v int64) *int64 {
	if a == nil || v > *a {
		return &v
	}
	return a
}

// observeTiming 按事件分类推进状态。usage 仅 UsageRecord 分类提供。
func (t *codexRequestTimer) observeTiming(tsMS int64, kind codexTimingKind, usage *codexUsage) {
	switch kind {
	case codexTimingBoundary:
		// 晚于锚的用户消息/turn_context 开启新请求：其后的重播不作补写锚。
		if t.lastTokenCountMS == nil || tsMS > *t.lastTokenCountMS {
			t.newRequestSeen = true
		}
		t.lastBoundaryMS = codexTimingOptionMax(t.lastBoundaryMS, tsMS)
	case codexTimingToolOutput:
		t.lastBoundaryMS = codexTimingOptionMax(t.lastBoundaryMS, tsMS)
		if t.lastModelOutputMS != nil {
			ts := tsMS
			t.toolOutputAfterModelMS = &ts
		}
	case codexTimingModelOutput:
		// 旧布局补写判定：请求尚未开始（无首输出项）且本输出项与上一条
		// token_count 同批写出——属上一轮响应的补写，不作本轮起点锚。
		if t.requestStartMS == nil && t.lastTokenCountMS != nil &&
			tsMS-*t.lastTokenCountMS <= codexSameFlushSlackMS {
			return
		}
		if t.requestStartMS == nil {
			t.requestStartMS = t.lastBoundaryMS
		}
		ts := tsMS
		t.lastModelOutputMS = &ts
		t.toolOutputAfterModelMS = nil
	case codexTimingUsageRecord:
		// usage 解析失败置空（无记录可用），不中断采集主流程。
		t.usageRecord = usage
		if usage != nil {
			ts := tsMS
			t.usageRecordMS = &ts
		} else {
			t.usageRecordMS = nil
		}
	case codexTimingTaskStarted:
		// 新一轮开始：作废未结算请求，且其后的重播属新请求时段（不作
		// 补写锚）。
		lb := codexTimingOptionMax(t.lastBoundaryMS, tsMS)
		ltc := t.lastTokenCountMS
		*t = codexRequestTimer{lastBoundaryMS: lb, lastTokenCountMS: ltc, newRequestSeen: true}
	case codexTimingTurnAborted:
		// 请求被中断：同样作废未结算请求，但回到旧时段残余状态——其后、
		// 下一用户消息之前到达的旧快照重播及其同批补写仍属旧时段。
		lb := codexTimingOptionMax(t.lastBoundaryMS, tsMS)
		ltc := t.lastTokenCountMS
		*t = codexRequestTimer{lastBoundaryMS: lb, lastTokenCountMS: ltc}
	}
}

// finishRequest 在一条带用量的 token_count 落定时结算本次请求的估算时长
// （毫秒；起点或终点缺失、非正返回 0），并把该 token_count 记为下一次请求
// 的起点锚。last 是本次请求自己的用量（token_count 的 last_token_usage），
// 用于确认 usage_record 说的是同一次请求（input/cached_input/output 三值比对）。
func (t *codexRequestTimer) finishRequest(tokenCountMS int64, last *codexUsage) int64 {
	startMS := t.requestStartMS
	if startMS == nil {
		startMS = t.lastBoundaryMS
	}
	var recordMS *int64
	if t.usageRecord != nil && recordMatchesUsage(t.usageRecord, last) {
		recordMS = t.usageRecordMS
	}
	waitedForTools := false
	if t.toolOutputAfterModelMS != nil &&
		tokenCountMS-*t.toolOutputAfterModelMS <= codexSameFlushSlackMS {
		waitedForTools = true
	}
	endMS := recordMS
	if endMS == nil {
		if waitedForTools {
			endMS = t.lastModelOutputMS
		} else {
			v := tokenCountMS
			endMS = &v
		}
	}
	lb := codexTimingOptionMax(t.lastBoundaryMS, tokenCountMS)
	tc := tokenCountMS
	*t = codexRequestTimer{lastBoundaryMS: lb, lastTokenCountMS: &tc}
	if startMS == nil || endMS == nil {
		return 0
	}
	d := *endMS - *startMS
	if d <= 0 {
		return 0
	}
	return d
}

// recordMatchesUsage 判定 usage_record 与本次请求用量是否同一次请求。
func recordMatchesUsage(record, last *codexUsage) bool {
	if last == nil {
		return true // 无本轮用量可比对时按上游语义接受记录
	}
	return record.InputTokens == last.InputTokens &&
		record.CachedInputTokens == last.CachedInputTokens &&
		record.OutputTokens == last.OutputTokens
}

// codexTokenUsageRecordPayload 对应顶层 type=token_usage_record 的 payload。
type codexTokenUsageRecordPayload struct {
	Usage *codexUsage `json:"usage"`
}

// classifyResponseItemTiming 对 response_item 载荷做时长事件分类；
// 未识别的类型不参与时长估算。
func classifyResponseItemTiming(payload responseItemPayload) (codexTimingKind, bool) {
	switch {
	case payload.Type == "message" && payload.Role == "assistant":
		return codexTimingModelOutput, true
	case payload.Type == "message":
		return codexTimingBoundary, true
	case payload.Type == "reasoning":
		return codexTimingModelOutput, true
	case strings.HasSuffix(payload.Type, "_output"):
		return codexTimingToolOutput, true
	case strings.HasSuffix(payload.Type, "_call"):
		return codexTimingModelOutput, true
	}
	return 0, false
}

// parseCodexTimingTimestamp 解析行时间戳为毫秒；失败返回 false（该行不
// 参与时长估算，不影响采集主流程）。
func parseCodexTimingTimestamp(timestamp string) (int64, bool) {
	ts, err := parseCodexTimestamp(timestamp)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// parseCodexUsageRecordUsage 解析 token_usage_record 行的 payload.usage；
// 行级解析失败返回 nil（无记录可用）。
func parseCodexUsageRecordUsage(payload json.RawMessage) *codexUsage {
	var p codexTokenUsageRecordPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	return p.Usage
}
