package collector

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFixtureF01Baseline(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			dshMessageJSON(1, 1, `{"inputTokens":10,"outputTokens":5,"cacheReadTokens":3,"totalTokens":18}`, "p", "m")),
		dshTestEvent(t, "assistant/message", 2, 1700000001000,
			dshMessageJSON(1, 2, `{"inputTokens":20,"outputTokens":6,"cacheReadTokens":4,"totalTokens":30}`, "p", "m")),
		dshTestEvent(t, "session/title", 3, 1700000002000, `{"title":"baseline"}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 2 {
		t.Fatalf("F01 messages = %d, want 2", len(result.Messages))
	}
	first := dshFindStep(t, result.Messages, 1)
	if first.InputTokens != 10 || first.OutputTokens != 5 || first.CacheReadTokens != 3 || first.TotalTokens != 18 {
		t.Fatalf("F01 first tokens = %+v", first)
	}
	if result.Sessions[0].Title != "baseline" {
		t.Fatalf("F01 title = %q", result.Sessions[0].Title)
	}
	if result.Sessions[0].FirstTS != 1700000000000 || result.Sessions[0].LastTS != 1700000001000 {
		t.Fatalf("F01 session range = %d/%d", result.Sessions[0].FirstTS, result.Sessions[0].LastTS)
	}
}

func TestFixtureF02CacheReadDoesNotSubtractInput(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			dshMessageJSON(1, 1, `{"inputTokens":100,"outputTokens":5,"cacheReadTokens":0,"totalTokens":105}`, "p", "m")),
		dshTestEvent(t, "assistant/message", 2, 1700000001000,
			dshMessageJSON(1, 2, `{"inputTokens":100,"outputTokens":5,"cacheReadTokens":9000,"totalTokens":9105}`, "p", "m")),
	)
	result := buildDSHResult(decoded)
	for _, message := range result.Messages {
		modelMessage := toModelMessage(message)
		if modelMessage.FreshInputTokens != modelMessage.InputTokens {
			t.Fatalf("F02 FreshInput = %d, Input = %d", modelMessage.FreshInputTokens, modelMessage.InputTokens)
		}
	}
	if got := dshFindStep(t, result.Messages, 2); got.TotalTokens != 9105 {
		t.Fatalf("F02 total = %d", got.TotalTokens)
	}
}

func TestFixtureF03ModelAttribution(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000, dshMessageJSON(1, 1, dshUsage100, "zhipu", "glm")),
		dshTestEvent(t, "assistant/message", 2, 1700000001000, dshMessageJSON(1, 2, dshUsage100, "deepseek-account", "deepseek-flash")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 2 {
		t.Fatalf("F03 messages = %d", len(result.Messages))
	}
	if result.Messages[0].Provider != "zhipu" || result.Messages[0].Model != "glm" {
		t.Fatalf("F03 first = %+v", result.Messages[0])
	}
	second := toModelMessage(result.Messages[1])
	if second.Provider != "DeepSeek Account" || second.Model != "deepseek-flash" {
		t.Fatalf("F03 second = %+v", second)
	}
}

func TestFixtureF07PlainAndZstdAgree(t *testing.T) {
	content := dshHeaderJSONCwd(4, "session-f07", "/tmp/f07") +
		`{"type":"assistant/message","seq":1,"time":1700000000000,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	plainHome := writeDSHHome(t, "session-f07", "/tmp/f07", content, dshFixtureOpts{})
	zstdHome := writeDSHHome(t, "session-f07", "/tmp/f07", content, dshFixtureOpts{Compression: "zstd", Frames: 2})

	plainPath := plainHome + "/sessions/" + dshProjectKey("/tmp/f07") + "/session-f07/session.v4.jsonl"
	zstdPath := zstdHome + "/sessions/" + dshProjectKey("/tmp/f07") + "/session-f07/session.v4.jsonl.zstd"
	plain, err := decodeDSHSession(context.Background(), plainPath, dshCompressionNone)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	zstd, err := decodeDSHSession(context.Background(), zstdPath, dshCompressionZstd)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	plainResult := buildDSHResult(plain)
	zstdResult := buildDSHResult(zstd)
	if len(plainResult.Messages) != len(zstdResult.Messages) || len(plainResult.Messages) != 1 {
		t.Fatalf("F07 messages = %d/%d", len(plainResult.Messages), len(zstdResult.Messages))
	}
	if plainResult.Messages[0] != zstdResult.Messages[0] {
		t.Fatalf("F07 message mismatch: %+v vs %+v", plainResult.Messages[0], zstdResult.Messages[0])
	}
}

// dshMidnightPair 按进程固定时区构造某日 23:59 与次日 00:01 的毫秒 TS。
// 用 time.Date 而不是「固定 UTC 加小时」，避免测试依赖运行机器时区。
func dshMidnightPair() (int64, int64) {
	late := time.Date(2026, 10, 19, 23, 59, 0, 0, time.Local)
	early := time.Date(2026, 10, 20, 0, 1, 0, 0, time.Local)
	return late.UnixMilli(), early.UnixMilli()
}

func TestFixtureF11CrossMidnight(t *testing.T) {
	late, early := dshMidnightPair()
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, late, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
		dshTestEvent(t, "assistant/message", 2, early, dshMessageJSON(1, 2, dshUsage250, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if result.Messages[0].FirstSeenTS == result.Messages[1].FirstSeenTS {
		t.Fatalf("F11 timestamps must differ")
	}
	if tsMsToDate(result.Messages[0].FirstSeenTS) == tsMsToDate(result.Messages[1].FirstSeenTS) {
		t.Fatalf("F11 dates must differ")
	}
	if result.Sessions[0].FirstTS != late || result.Sessions[0].LastTS != early {
		t.Fatalf("F11 session range = %d/%d", result.Sessions[0].FirstTS, result.Sessions[0].LastTS)
	}
}

func TestFixtureF12AbortedAndError(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			`{"turn":1,"step":1,"interrupted":true,"message":{"id":"m","source":{"provider":"p","model":"m"}}}`),
		dshTestEvent(t, "assistant/message", 2, 1700000001000,
			`{"turn":1,"step":2,"interrupted":true,"usage":`+dshUsage100+`,"message":{"id":"m2","source":{"provider":"p","model":"m"}}}`),
		dshTestEvent(t, "turn/end", 3, 1700000002000, `{"turn":1,"status":"error"}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 100 {
		t.Fatalf("F12 messages = %+v, want only the defended usage row", result.Messages)
	}
}

func TestFixtureF16LongLine(t *testing.T) {
	pad := strings.Repeat("x", 16<<20)
	content := dshHeaderJSONCwd(4, "session-f16", "/tmp/f16") +
		`{"type":"tool/result","seq":0,"time":1,"data":{"pad":"` + pad + `"}}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	home := writeDSHHome(t, "session-f16", "/tmp/f16", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("F16 collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Messages) != 1 || len(result.FileStatuses) != 1 || result.FileStatuses[0].BadLines != 0 {
		t.Fatalf("F16 = %+v / %+v", result.Messages, result.FileStatuses)
	}
}

func TestFixtureF18RealShape(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			`{"turn":1,"step":1,"usage":{"inputTokens":10,"outputTokens":5,"reasoningTokens":2,"totalTokens":17},"message":{"id":"m","source":{"provider":"deepseek-account","model":"deepseek-flash"}},"request":{"header":{"tools":[]}}}`),
		dshTestEvent(t, "session-log-deepseek/delivery-accepted", 2, 1700000001000, `{}`),
		dshTestEvent(t, "plugin:future/thing", 3, 1700000002000, `{"ignorable":true}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].ReasoningTokens != 2 {
		t.Fatalf("F18 messages = %+v", result.Messages)
	}
}

func TestFixtureF21UsageVariants(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			dshMessageJSON(1, 1, `{"inputTokens":10,"outputTokens":5,"totalTokens":15}`, "p", "m")),
		dshTestEvent(t, "assistant/message", 2, 1700000001000,
			dshMessageJSON(1, 2, `{"inputTokens":10,"outputTokens":5,"cacheReadTokens":7}`, "p", "m")),
		dshTestEvent(t, "assistant/message", 3, 1700000002000,
			dshMessageJSON(1, 3, `{"inputTokens":10,"outputTokens":5,"cacheWriteTokens":9}`, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 3 {
		t.Fatalf("F21 messages = %d", len(result.Messages))
	}
	if got := dshFindStep(t, result.Messages, 2); got.TotalTokens != 22 {
		t.Fatalf("F21 derived total = %d, want 22", got.TotalTokens)
	}
	if got := dshFindStep(t, result.Messages, 3); got.TotalTokens != 24 {
		t.Fatalf("F21 derived total = %d, want 24", got.TotalTokens)
	}
}

func TestFixtureF23WorkflowChildren(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "parent", "/tmp/w", dshHeaderJSONCwd(4, "parent", "/tmp/w")+
		`{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":`+dshUsage100+`,"message":{"id":"m","source":{"provider":"deepseek-account","model":"deepseek-flash"}}}}`+"\n", dshFixtureOpts{})
	child := func(id, provider string) string {
		return `{"type":"session","version":4,"id":"` + id + `","createdAt":1,"cwd":"/tmp/w","parentSession":"parent","origin":"subagent","isSeeded":false,"delegationDepth":1}` + "\n" +
			`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"` + provider + `","model":"m"}}}}` + "\n"
	}
	writeDSHSession(t, home, "child-a", "/tmp/w", child("child-a", "deepseek-account"), dshFixtureOpts{})
	writeDSHSession(t, home, "child-b", "/tmp/w", child("child-b", "zhipu"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("F23 collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Messages) != 3 || len(result.Sessions) != 3 {
		t.Fatalf("F23 = %d messages / %d sessions", len(result.Messages), len(result.Sessions))
	}
	for _, session := range result.Sessions {
		if session.ID != "parent" && session.ParentID != "parent" {
			t.Fatalf("F23 parent = %+v", session)
		}
	}
}

func TestFixtureF25ImageInput(t *testing.T) {
	content := dshHeaderJSONCwd(4, "session-f25", "/tmp/f25") +
		`{"type":"user/message","seq":0,"time":1,"data":{"content":[{"type":"image","attachment":{"id":"a1"}}]}}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	home := writeDSHHome(t, "session-f25", "/tmp/f25", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("F25 collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("F25 messages = %d", len(result.Messages))
	}
}

func TestFixtureF32MidnightConsistency(t *testing.T) {
	late, early := dshMidnightPair()
	if tsMsToDate(late) == tsMsToDate(early) {
		t.Fatalf("F32 fixture must straddle midnight: %q", tsMsToDate(late))
	}
	events := []dshEvent{
		dshTestEvent(t, "assistant/attempt", 1, late,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsage100+`}}]}`),
		dshTestEvent(t, "assistant/message", 2, early, dshMessageJSON(1, 1, dshUsage250, "p", "m")),
	}
	full := buildDSHResult(dshTestDecoded(events...))
	split := buildDSHResult(dshTestDecoded(events[:1]...))
	split2 := buildDSHResult(dshTestDecoded(events...))
	if len(full.Messages) != 1 || len(split2.Messages) != 1 {
		t.Fatalf("F32 counts = %d/%d", len(full.Messages), len(split2.Messages))
	}
	if full.Messages[0].FirstSeenTS != late || split2.Messages[0].FirstSeenTS != late {
		t.Fatalf("F32 first-seen TS = %d/%d, want %d", full.Messages[0].FirstSeenTS, split2.Messages[0].FirstSeenTS, late)
	}
	if len(split.Messages) != 1 || split.Messages[0].TotalTokens != 100 {
		t.Fatalf("F32 first round = %+v, want one 100", split.Messages)
	}
	if len(split2.Messages) != 1 || split2.Messages[0].TotalTokens != 250 {
		t.Fatalf("F32 second round = %+v, want one 250", split2.Messages)
	}
	if split2.Messages[0].FirstSeenTS != late {
		t.Fatalf("F32 second round first-seen = %d, want %d", split2.Messages[0].FirstSeenTS, late)
	}
}

func TestFixtureF38TimezoneStable(t *testing.T) {
	const ts = int64(1700000000000)
	first := tsMsToDate(ts)
	for i := 0; i < 3; i++ {
		if got := tsMsToDate(ts); got != first {
			t.Fatalf("F38 date drifted: %q vs %q", got, first)
		}
	}
}

func dshFindStep(t *testing.T, messages []parsedMessage, step int) parsedMessage {
	t.Helper()
	for _, message := range messages {
		if message.Step == step {
			return message
		}
	}
	t.Fatalf("step %d not found in %+v", step, messages)
	return parsedMessage{}
}
