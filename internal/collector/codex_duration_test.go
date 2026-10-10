package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ===== Codex 请求时长估算状态机（B 包 T5-T8）=====

// codexTimingTS 生成 UTC 时刻的 RFC3339 时间戳与毫秒值。
func codexTimingTS(sec int) (string, int64) {
	tt := time.Date(2026, 10, 8, 3, 0, sec, 0, time.UTC)
	return tt.Format(time.RFC3339), tt.UnixMilli()
}

func codexTimingWrite(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	content := `{"timestamp":"2026-10-08T02:59:00Z","type":"session_meta","payload":{"id":"dur-test","source":"cli","originator":"codex-tui","cwd":"/tmp"}}` + "\n"
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexTimingUser(sec int) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"user","content":"q"}}`
}

func codexTimingTurnContext(sec int) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"turn_context","payload":{"model":"gpt-5"}}`
}

func codexTimingAssistant(sec int) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1"}}`
}

func codexTimingTokenCount(sec int, input, cached, output int64) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":` + itoa(input) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(output) + `}}}}`
}

// codexTimingTokenCountFull 生成带 total_token_usage 的完整签名事件（重播
// 判定只在带 total 时启用）。
func codexTimingTokenCountFull(sec int, input, cached, output int64) string {
	ts, _ := codexTimingTS(sec)
	usage := `{"input_tokens":` + itoa(input) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(output) + `}`
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":` + usage + `,"total_token_usage":` + usage + `}}}`
}

func codexTimingToolOutputLine(sec int) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"function_call_output","output":"r"}}`
}

func codexTimingEvent(sec int, kind string) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"` + kind + `"}}`
}

func codexTimingUsageRecordLine(sec int, input, cached, output int64) string {
	ts, _ := codexTimingTS(sec)
	return `{"timestamp":"` + ts + `","type":"token_usage_record","payload":{"usage":{"input_tokens":` + itoa(input) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(output) + `}}}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// codexDurationOf 解析 rollout 并返回消息时长列表（单消息场景便捷断言）。
func codexDurationOf(t *testing.T, path string) []int64 {
	t.Helper()
	result, _, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout: %v", err)
	}
	durations := make([]int64, 0, len(result.Messages))
	for _, m := range result.Messages {
		durations = append(durations, m.DurationMS)
	}
	return durations
}

// T5：正常回合——duration = token_count ts − 边界 ts（用户消息边界）。
func TestCodexDuration_NormalTurn(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(15),
		codexTimingTokenCount(20, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 {
		t.Fatalf("消息数 = %d, want 1", len(got))
	}
	if got[0] != 20000 {
		t.Fatalf("duration = %d, want 20000（20s − 0s）", got[0])
	}
}

// T5 变体：终点优先匹配本次用量的 token_usage_record。
func TestCodexDuration_UsageRecordEndpoint(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingTurnContext(0),
		codexTimingAssistant(10),
		codexTimingUsageRecordLine(25, 10, 0, 500), // 与 last 用量匹配
		codexTimingTokenCount(30, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 25000 {
		t.Fatalf("duration = %v, want [25000]（终点=usage_record 25s）", got)
	}
}

// T6：慢工具——模型输出 5s + 工具 60s + token_count 后置（同批写入容差内），
// duration ≈ 5s（终点=最后模型输出时刻），不得 ≈65s。
func TestCodexDuration_SlowToolExcluded(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5),               // 模型输出完成于 5s
		codexTimingToolOutputLine(60),         // 工具运行 55s
		codexTimingTokenCount(60, 10, 0, 500), // token_count 紧跟工具结果（同批）
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 {
		t.Fatalf("消息数 = %d, want 1", len(got))
	}
	if got[0] != 5000 {
		t.Fatalf("duration = %d, want 5000（工具时间不得计入模型速度）", got[0])
	}
}

// T7：token_usage_record 匹配/不匹配——不匹配（用量不同）回退常规终点。
func TestCodexDuration_UsageRecordMismatch(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(10),
		codexTimingUsageRecordLine(15, 99, 0, 99), // 用量不匹配本次 last
		codexTimingTokenCount(30, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 30000 {
		t.Fatalf("duration = %v, want [30000]（不匹配时终点=token_count ts）", got)
	}
}

// T8：turn_aborted 清起点（中断请求的起点不得借给下一轮）。
func TestCodexDuration_AbortedTurnResetsStart(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5),
		codexTimingEvent(6, "turn_aborted"), // 中断：起点作废
		codexTimingUser(60),
		codexTimingAssistant(70),
		codexTimingTokenCount(80, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 20000 {
		t.Fatalf("duration = %v, want [20000]（起点=中断后的用户消息 60s）", got)
	}
}

// T8：旧版 output 后置——token_count 之后同批写入的补写输出项不作下一轮起点。
func TestCodexDuration_TrailingOutputsIgnored(t *testing.T) {
	// 第一轮：token_count 于 10s 结束；旧布局把该轮输出项补写在 token_count
	// 之后（同批，11s）——不得成为第二轮起点。
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCount(10, 10, 0, 500),
		codexTimingAssistant(10), // 同批补写（10s−10s=0 ≤ 100ms 容差）
		// 用户隔很久才发下一条消息。
		codexTimingUser(1000),
		codexTimingAssistant(1010),
		codexTimingTokenCount(1020, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 2 {
		t.Fatalf("消息数 = %d, want 2", len(got))
	}
	if got[1] != 20000 {
		t.Fatalf("第二轮 duration = %d, want 20000（起点=1000s 的用户消息，不得钉在补写输出）", got[1])
	}
}

// T8：重播快照不产出 duration（与重播去重共存）。
func TestCodexDuration_ReplayedSnapshotNoDuration(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5),
		// 完整签名=total+last；紧邻重播（完全相同签名）不产出消息与时长。
		codexTimingTokenCountFull(10, 10, 0, 500),
		codexTimingTokenCountFull(11, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 {
		t.Fatalf("消息数 = %d, want 1（重播去重不变）", len(got))
	}
	if got[0] != 10000 {
		t.Fatalf("duration = %d, want 10000", got[0])
	}
}

// T8：容差边界——补写输出距 token_count 超 100ms 属于新一轮首个输出（可作起点锚）。
func TestCodexDuration_SlackBoundary(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCount(10, 10, 0, 500),
		codexTimingAssistant(11), // 距 token_count 1s > 100ms 容差：新请求首输出
		codexTimingTokenCount(21, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 2 {
		t.Fatalf("消息数 = %d, want 2", len(got))
	}
	// 第二轮起点=上一 token_count（10s）边界（首输出前最近起点），终点 21s。
	if got[1] != 11000 {
		t.Fatalf("第二轮 duration = %d, want 11000", got[1])
	}
}

// 过滤门端到端：Codex 短回复（output<200）不落时长。
func TestCodexDuration_GateEndToEnd(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5),
		codexTimingTokenCount(30, 10, 0, 150),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("duration = %v, want [0]（过滤门内短回复）", got)
	}
}

// T8 加固：task_started 与 turn_aborted 同为 TurnReset（起点作废）。
func TestCodexDuration_TaskStartedResetsStart(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5),
		codexTimingEvent(6, "task_started"),
		codexTimingUser(60),
		codexTimingAssistant(70),
		codexTimingTokenCount(80, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 20000 {
		t.Fatalf("duration = %v, want [20000]（task_started 同样清起点）", got)
	}
}

// T5 加固：回合内多段输出夹工具结果（assistant→tool→assistant→token_count），
// 起点锁定首个输出前的边界，中途工具结果不推迟起点。
func TestCodexDuration_ToolLoopKeepsFirstStart(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(5), // 首输出：锁定起点=0s
		codexTimingToolOutputLine(30),
		codexTimingAssistant(40), // 工具后的第二段输出（不重锁起点）
		codexTimingTokenCount(50, 10, 0, 500),
	)
	got := codexDurationOf(t, path)
	if len(got) != 1 || got[0] != 50000 {
		t.Fatalf("duration = %v, want [50000]（起点=首个输出前的边界）", got)
	}
}

// 重播快照+旧版输出补写组合：重播 token_count 虽被去重丢弃，但计时锚点须
// 推进——否则较晚重播后同批的旧输出被误判为新请求首输出，下一请求起点被
// 污染（正确 [10000, 20000]，污染实现会得到第二个 1010000）。
func TestCodexDuration_ReplayThenTrailingOutputThenNextRequest(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCountFull(10, 10, 0, 500),
		// 90 秒后重播同一快照（紧邻重播，同 (total,last) 签名）。
		codexTimingTokenCountFull(100, 10, 0, 500),
		// 重播同批（0ms 差）的旧布局补写输出：不作下一请求起点。
		codexTimingAssistant(100),
		// 下一请求：用户消息在 1000s，结算于 1020s → 时长 20s（用量不同，
		// 不与重播签名冲突）。
		codexTimingUser(1000),
		codexTimingAssistant(1010),
		codexTimingTokenCountFull(1020, 20, 0, 600),
	)
	got := codexDurationOf(t, path)
	if len(got) != 2 {
		t.Fatalf("消息数 = %d, want 2（重播去重不变）", len(got))
	}
	if got[0] != 10000 {
		t.Fatalf("首轮 duration = %d, want 10000", got[0])
	}
	if got[1] != 20000 {
		t.Fatalf("次轮 duration = %d, want 20000（起点=1000s 用户消息，不得被补写输出污染）", got[1])
	}
}

// 反向顺序组合（外审场景）：新请求边界（user+task_started@1000）之后到达的
// 重播快照（@1001）不构成补写锚——其同批的本轮首输出不得被当补写忽略，
// 起点须为 1000 的边界（正确 [10000, 20000]；锚点误更新会得 [10000, 10000]）。
func TestCodexDuration_ReplayAfterNewBoundaryKeepsFirstOutput(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCountFull(10, 10, 0, 500), // 首轮结算
		codexTimingUser(1000),                     // 新请求边界
		codexTimingEvent(1000, "task_started"),
		codexTimingTokenCountFull(1001, 10, 0, 500), // 旧快照重播（同签名）
		codexTimingAssistant(1001),                  // 本轮首输出（与重播同批）
		codexTimingToolOutputLine(1010),
		codexTimingAssistant(1015),
		codexTimingTokenCountFull(1020, 20, 0, 600), // 本轮有效结算
	)
	got := codexDurationOf(t, path)
	if len(got) != 2 {
		t.Fatalf("消息数 = %d, want 2（重播去重不变）", len(got))
	}
	if got[0] != 10000 {
		t.Fatalf("首轮 duration = %d, want 10000", got[0])
	}
	if got[1] != 20000 {
		t.Fatalf("次轮 duration = %d, want 20000（起点=1000 边界，首输出不得被重播锚吞掉）", got[1])
	}
	// token 面不受计时状态影响：第二轮 output=600、重播不产出。
	result, _, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("消息数 = %d, want 2", len(result.Messages))
	}
	outputs := make([]int64, 0, len(result.Messages))
	for _, m := range result.Messages {
		outputs = append(outputs, m.OutputTokens)
	}
	if len(outputs) == 2 && outputs[1] != 600 {
		t.Fatalf("次轮 output = %d, want 600（token 计量不受重播锚影响）", outputs[1])
	}
}

// 三向组合（外审三轮场景）：已结算 → 新请求中断（turn_aborted）→ 旧快照重播
// 与同批旧输出补写 → 下一用户请求。中断后的重播属旧时段残余，仍构成补写锚
// ——旧输出被忽略、下一请求起点取其用户消息（正确 [10000, 20000]；锚被
// 误拦时会得 [10000, 970000]，起点锁在中断时刻）。
func TestCodexDuration_AbortedThenReplayTrailingOutputThenNextRequest(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCountFull(10, 10, 0, 500), // 首轮结算
		codexTimingUser(40),                       // 第二次请求开始（后被中断）
		codexTimingAssistant(50),
		codexTimingEvent(50, "turn_aborted"),       // 中断：该请求作废，回到旧时段残余
		codexTimingTokenCountFull(100, 10, 0, 500), // 旧快照重播（同签名→去重）
		codexTimingAssistant(100),                  // 同批旧输出补写：不作下一轮起点
		codexTimingUser(1000),                      // 下一请求
		codexTimingAssistant(1010),
		codexTimingTokenCountFull(1020, 20, 0, 600),
	)
	result, _, err := parseCodexRollout(path, codexThread{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("消息数 = %d, want 2（中断轮与重播均不产出）", len(result.Messages))
	}
	durations := make([]int64, 0, len(result.Messages))
	outputs := make([]int64, 0, len(result.Messages))
	for _, m := range result.Messages {
		durations = append(durations, m.DurationMS)
		outputs = append(outputs, m.OutputTokens)
	}
	if durations[0] != 10000 {
		t.Fatalf("首轮 duration = %d, want 10000", durations[0])
	}
	if durations[1] != 20000 {
		t.Fatalf("次轮 duration = %d, want 20000（起点=1000 用户消息，不得锁在中断时刻 50s）", durations[1])
	}
	if outputs[0] != 500 || outputs[1] != 600 {
		t.Fatalf("token 面 = %v, want [500 600]（计量不受计时状态影响）", outputs)
	}
}

// Boundary 置信号路径的独立保护（无 task_started 伴随）：仅 user 边界后的
// 重播不得吞掉本轮首输出——掩盖该分支时（只推边界不置信号），重播会误推
// 锚点，且 lastModelOutputMS 丢失使慢工具终点回退场景把工具时间计入时长。
func TestCodexDuration_ReplayAfterUserBoundaryOnlyKeepsFirstOutput(t *testing.T) {
	path := codexTimingWrite(t,
		codexTimingUser(0),
		codexTimingAssistant(9),
		codexTimingTokenCountFull(10, 10, 0, 500),   // 首轮结算
		codexTimingUser(1000),                       // 新请求边界（仅 user，无 task_started）
		codexTimingTokenCountFull(1001, 10, 0, 500), // 旧快照重播（同签名→去重）
		codexTimingAssistant(1001),                  // 本轮首输出（与重播同批）
		codexTimingToolOutputLine(1010),
		codexTimingTokenCountFull(1010, 20, 0, 600), // 结算紧跟工具（同批→终点回退最后输出）
	)
	got := codexDurationOf(t, path)
	if len(got) != 2 || got[0] != 10000 || got[1] != 1000 {
		t.Fatalf("durations = %v, want [10000 1000]（user 边界独立置信号：重播不吞首输出，终点回退首输出时刻）", got)
	}
}
