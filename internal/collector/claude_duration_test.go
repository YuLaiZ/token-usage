package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// ===== 会话日志请求时长估算（B 包 T1-T4 / T9）=====

// claudeTimingLine 构造一行 Claude JSONL；format 由调用方给定 uuid/parentUuid/
// timestamp/message 载荷。便捷封装在下方按行类型展开。
func writeClaudeTimingFixture(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "proj.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudeTimingUser(uuid, parent, ts string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%q,"timestamp":%q,"message":{"id":"u-%s","role":"user"}}`, uuid, parent, ts, uuid)
}

// claudeTimingAssistant 构造 assistant 行：block 序号嵌入 id（同 id 多 block）。
func claudeTimingAssistant(uuid, parent, ts, msgID string, output int64, stopReason string) string {
	usage := ""
	if output >= 0 {
		usage = fmt.Sprintf(`,"usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, output)
	}
	stop := ""
	if stopReason != "" {
		stop = fmt.Sprintf(`,"stop_reason":%q`, stopReason)
	}
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":%q,"message":{"id":%q,"role":"assistant","model":"m1"%s%s}}`,
		uuid, parent, ts, msgID, usage, stop)
}

func claudeTimingAttachment(uuid, parent, ts string) string {
	return fmt.Sprintf(`{"type":"attachment","uuid":%q,"parentUuid":%q,"timestamp":%q}`, uuid, parent, ts)
}

func claudeDurationByID(t *testing.T, path string) map[string]int64 {
	t.Helper()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Dir(path)}},
	}}
	c := NewClaudeCollector(cfg)
	res, err := c.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, m := range res.Messages {
		out[m.ID] = m.DurationMS
	}
	return out
}

func claudeTimingMS(h, m, s int) (string, int64) {
	tt := time.Date(2026, 10, 8, h, m, s, 0, time.Local)
	return tt.Format(time.RFC3339), tt.UnixMilli()
}

// T1：完整回合（单 block）duration = 末 block ts − 链首起点。
func TestClaudeDuration_SingleBlock(t *testing.T) {
	start, startMS := claudeTimingMS(10, 0, 0)
	end, endMS := claudeTimingMS(10, 0, 30)
	path := writeClaudeTimingFixture(t,
		claudeTimingUser("u1", "", start),
		claudeTimingAssistant("a1", "u1", end, "msg_1", 500, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	if d := got["msg_1"]; d != endMS-startMS {
		t.Fatalf("duration = %d, want %d", d, endMS-startMS)
	}
}

// T2：同 message.id 多 block——token 取首块非零 usage（去重合同不变）+ 计时
// 取全 block max ts、起点用首块父链。
func TestClaudeDuration_MultiBlock(t *testing.T) {
	start, startMS := claudeTimingMS(10, 1, 0)
	mid, midMS := claudeTimingMS(10, 1, 10)
	end, endMS := claudeTimingMS(10, 1, 40)
	path := writeClaudeTimingFixture(t,
		claudeTimingUser("u1", "", start),
		// 首 block：带 usage（token 去重合同保留首条非零 usage）。
		claudeTimingAssistant("a1", "u1", mid, "msg_1", 500, ""),
		// 末 block：stop_reason + 更大 usage（token 仍取首块 500；计时取 max ts）。
		claudeTimingAssistant("a2", "a1", end, "msg_1", 800, "end_turn"),
	)
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Dir(path)}},
	}}
	c := NewClaudeCollector(cfg)
	res, err := c.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("期望 1 条消息（去重合同不变），实际 %d", len(res.Messages))
	}
	m := res.Messages[0]
	if m.OutputTokens != 500 {
		t.Fatalf("token 应取首块非零 usage: output=%d, want 500", m.OutputTokens)
	}
	if m.DurationMS != endMS-startMS {
		t.Fatalf("duration = %d, want %d（全 block max ts − 首块父链起点）", m.DurationMS, endMS-startMS)
	}
	_ = midMS
}

// T3：链断（首块缺失父引用）/ stop_reason 空 / 终点≤起点 → duration = 0。
func TestClaudeDuration_InvalidPathsZero(t *testing.T) {
	start, _ := claudeTimingMS(10, 2, 0)
	end, _ := claudeTimingMS(10, 2, 5)
	early, _ := claudeTimingMS(10, 1, 0)
	path := writeClaudeTimingFixture(t,
		// 链断：首块 parentUuid 指向不存在的节点。
		claudeTimingAssistant("a1", "missing", end, "chain_broken", 500, "end_turn"),
		// stop_reason 空：不完整回合。
		claudeTimingUser("u2", "", start),
		claudeTimingAssistant("a2", "u2", end, "no_stop", 500, ""),
		// 终点早于起点（数据乱序）：非正时长。
		claudeTimingUser("u3", "", start),
		claudeTimingAssistant("a3", "u3", early, "negative", 500, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	for _, id := range []string{"chain_broken", "no_stop", "negative"} {
		if d := got[id]; d != 0 {
			t.Fatalf("%s duration = %d, want 0", id, d)
		}
	}
}

// T4：attachment 链跳过（取真请求起点）；sidechain 按自身链。
func TestClaudeDuration_AttachmentAndSidechain(t *testing.T) {
	userTS, userMS := claudeTimingMS(10, 3, 0)
	attTS, _ := claudeTimingMS(10, 3, 1)
	end1TS, end1MS := claudeTimingMS(10, 3, 20)
	scUserTS, scUserMS := claudeTimingMS(10, 3, 2)
	scEndTS, scEndMS := claudeTimingMS(10, 3, 25)
	path := writeClaudeTimingFixture(t,
		claudeTimingUser("u1", "", userTS),
		// attachment 行挂在用户消息下；assistant 的父是 attachment——
		// 起点须跳过 attachment 取用户消息 ts。
		claudeTimingAttachment("att1", "u1", attTS),
		claudeTimingAssistant("a1", "att1", end1TS, "with_attachment", 500, "end_turn"),
		// sidechain：独立链（自己的 user 节点）。
		claudeTimingUser("su1", "u1", scUserTS),
		claudeTimingAssistant("sa1", "su1", scEndTS, "sidechain", 500, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	if d := got["with_attachment"]; d != end1MS-userMS {
		t.Fatalf("attachment 链 duration = %d, want %d（跳过 attachment 取用户消息 ts）", d, end1MS-userMS)
	}
	if d := got["sidechain"]; d != scEndMS-scUserMS {
		t.Fatalf("sidechain duration = %d, want %d（按自身链）", d, scEndMS-scUserMS)
	}
}

// 环状链防御：parentUuid 成环不得死循环（跳数上限截断，duration=0）。
func TestClaudeDuration_CyclicChainTerminates(t *testing.T) {
	ts1, _ := claudeTimingMS(10, 4, 0)
	ts2, _ := claudeTimingMS(10, 4, 10)
	path := writeClaudeTimingFixture(t,
		claudeTimingAttachment("x1", "x2", ts1),
		claudeTimingAttachment("x2", "x1", ts1), // x1↔x2 成环
		claudeTimingAssistant("a1", "x1", ts2, "cyclic", 500, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	if d := got["cyclic"]; d != 0 {
		t.Fatalf("环状链 duration = %d, want 0", d)
	}
}

// T9：过滤门边界（199/200 tokens、999/1000ms）红测锁定阈值。
func TestEstimateDurationGate(t *testing.T) {
	cases := []struct {
		output int64
		ms     int64
		want   int64
	}{
		{output: 199, ms: 5000, want: 0},
		{output: 200, ms: 5000, want: 5000},
		{output: 500, ms: 999, want: 0},
		{output: 500, ms: 1000, want: 1000},
		{output: 500, ms: 0, want: 0},
		{output: 500, ms: -3, want: 0},
	}
	for _, tc := range cases {
		if got := estimateDurationMS(tc.output, tc.ms); got != tc.want {
			t.Fatalf("estimateDurationMS(%d,%d) = %d, want %d", tc.output, tc.ms, got, tc.want)
		}
	}
}

// Claude 过滤门经 Collect 端到端生效（短回复不落时长）。
func TestClaudeDuration_GateEndToEnd(t *testing.T) {
	start, _ := claudeTimingMS(10, 5, 0)
	end, _ := claudeTimingMS(10, 5, 30)
	path := writeClaudeTimingFixture(t,
		claudeTimingUser("u1", "", start),
		// 30 秒但 output=199 < 200：过滤门拦截。
		claudeTimingAssistant("a1", "u1", end, "gated", 199, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	if d := got["gated"]; d != 0 {
		t.Fatalf("过滤门内短回复 duration = %d, want 0", d)
	}
	_ = model.Message{}
}

// 起点节点时间戳缺失（不可解析）：该节点不能作为起点；缺时间戳的
// attachment 仍可穿越。
// 时间戳取 epoch 早期（分钟级毫秒）：若实现误把缺 ts 起点当作 0ms 时刻，
// duration=endMS−0 会恰好落在 [1s,1h] 过滤门内被落库——本测试因此对
// 「缺 ts 起点不作为起点」有独立区分度（不依赖 1h 上限兜底拦截）。
func TestClaudeDuration_StartNodeMissingTimestamp(t *testing.T) {
	start := "1970-01-01T00:00:00Z"
	startMS := int64(0 + 3600000) // 1970-01-01T01:00:00Z 基准用毫秒换算见下
	startMS = 0                   // start 本身 0ms；end 在 5 分钟处
	end := "1970-01-01T00:05:00Z"
	endMS := int64(5 * 60 * 1000)
	noTS := `{"type":"user","uuid":"u2","parentUuid":"","timestamp":"not-a-time","message":{"id":"u-u2","role":"user"}}`
	path := writeClaudeTimingFixture(t,
		noTS, // 起点候选但时间戳不可解析
		claudeTimingUser("u1", "", start),
		claudeTimingAssistant("a1", "u2", end, "broken_start", 500, "end_turn"),
		// attachment 缺时间戳：可穿越，起点仍是它上面的用户消息。
		`{"type":"attachment","uuid":"att-nts","parentUuid":"u1","timestamp":"bad"}`,
		claudeTimingAssistant("a2", "att-nts", end, "via_attachment", 500, "end_turn"),
	)
	got := claudeDurationByID(t, path)
	if d := got["broken_start"]; d != 0 {
		t.Fatalf("起点时间戳缺失应不产出时长: %d", d)
	}
	if d := got["via_attachment"]; d != endMS-startMS {
		t.Fatalf("缺时间戳 attachment 应可穿越: %d, want %d", d, endMS-startMS)
	}
}

// 超过 1 小时合理上限的估算不计入（跨段 resume/用户长间隔不属单次请求）。
func TestEstimateDurationUpperBound(t *testing.T) {
	if got := estimateDurationMS(500, 60*60*1000); got != 60*60*1000 {
		t.Fatalf("恰好 1h 应计入: %d", got)
	}
	if got := estimateDurationMS(500, 60*60*1000+1); got != 0 {
		t.Fatalf("超过 1h 应拦截: %d", got)
	}
}

// 字符串 content 形态 user 行：不参与元数据与消息产出（既有合同），但其
// 链节点仍参与起点推导（直接父级为该类行的回合可估时长）。
func TestClaudeDuration_StringFormUserInChain(t *testing.T) {
	start, startMS := claudeTimingMS(10, 7, 0)
	end, endMS := claudeTimingMS(10, 7, 30)
	strUser := `{"type":"user","uuid":"su","parentUuid":"","timestamp":"` + start + `","cwd":"/hidden","message":{"id":"u-su","role":"user","content":"plain text"}}`
	path := writeClaudeTimingFixture(t,
		strUser,
		claudeTimingAssistant("a1", "su", end, "str_parent", 500, "end_turn"),
	)
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Dir(path)}},
	}}
	c := NewClaudeCollector(cfg)
	res, err := c.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got *model.Message
	for i := range res.Messages {
		if res.Messages[i].ID == "str_parent" {
			got = &res.Messages[i]
		}
	}
	if got == nil {
		t.Fatal("str_parent 消息缺失")
	}
	if got.DurationMS != endMS-startMS {
		t.Fatalf("duration = %d, want %d（字符串 content 父行应入链）", got.DurationMS, endMS-startMS)
	}
}

// 首块缺失：回复只有 apiBlockIndex=1 的行（block 0 不在文件中）——父链可解、
// stop_reason 完整，但起点会取晚，按合同不估（duration=0）。
func TestClaudeDuration_MissingFirstBlockZero(t *testing.T) {
	start, _ := claudeTimingMS(10, 8, 0)
	end, _ := claudeTimingMS(10, 8, 30)
	withBlockIdx := func(uuid, parent, ts, msgID string, output int64, stop string, blockIdx int64) string {
		return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":%q,"apiBlockIndex":%d,"message":{"id":%q,"role":"assistant","model":"m1","usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"stop_reason":%q}}`,
			uuid, parent, ts, blockIdx, msgID, output, stop)
	}
	path := writeClaudeTimingFixture(t,
		claudeTimingUser("u1", "", start),
		withBlockIdx("a1", "u1", end, "no_block0", 500, "end_turn", 1),
	)
	got := claudeDurationByID(t, path)
	if d := got["no_block0"]; d != 0 {
		t.Fatalf("首块缺失 duration = %d, want 0（宁可不估）", d)
	}

	// 对照：带 apiBlockIndex=0 的完整形态同 fixture 正常估算。
	pathOK := writeClaudeTimingFixture(t,
		claudeTimingUser("u2", "", start),
		withBlockIdx("a1", "u2", end, "with_block0", 500, "end_turn", 0),
	)
	gotOK := claudeDurationByID(t, pathOK)
	if d := gotOK["with_block0"]; d == 0 {
		t.Fatalf("block 0 起步 duration = 0, want 正常估算")
	}

	// 多 block（0 与 1）：起点用首块父链、终点取全 block max ts。
	mid, _ := claudeTimingMS(10, 8, 10)
	pathMulti := writeClaudeTimingFixture(t,
		claudeTimingUser("u3", "", start),
		withBlockIdx("b1", "u3", mid, "multi", 500, "", 0),
		withBlockIdx("b2", "b1", end, "multi", 500, "end_turn", 1),
	)
	gotMulti := claudeDurationByID(t, pathMulti)
	if d := gotMulti["multi"]; d != 30000 {
		t.Fatalf("多 block duration = %d, want 30000（首块父链起点+max ts 终点）", d)
	}
}
