package collector

import (
	"encoding/json"
	"fmt"
	"testing"
)

func dshTestEvent(t *testing.T, typ string, seq, time int64, data string) dshEvent {
	t.Helper()
	raw := fmt.Sprintf(`{"type":%q,"seq":%d,"time":%d,"data":%s}`, typ, seq, time, data)
	var event dshEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("decode test event: %v", err)
	}
	event.Raw = []byte(raw)
	return event
}

func dshTestDecoded(events ...dshEvent) *dshDecoded {
	return &dshDecoded{
		Header:         dshHeader{ID: "session-s", Cwd: "/tmp/p"},
		Events:         events,
		Cut:            -1,
		HasUsageEvents: true,
	}
}

func dshMessageJSON(turn, step int, usage string, provider, model string) string {
	return fmt.Sprintf(`{"turn":%d,"step":%d,"usage":%s,"message":{"id":"m","source":{"provider":%q,"model":%q}}}`,
		turn, step, usage, provider, model)
}

const dshUsage100 = `{"inputTokens":100,"outputTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"totalTokens":100}`
const dshUsage250 = `{"inputTokens":250,"outputTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"totalTokens":250}`
const dshUsageZero = `{"inputTokens":0,"outputTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"totalTokens":0}`

func TestBuildDSHBaseline(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1700000000000,
			dshMessageJSON(1, 1, `{"inputTokens":10,"outputTokens":5,"cacheReadTokens":3,"totalTokens":18}`, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(result.Messages))
	}
	got := toModelMessage(result.Messages[0])
	if got.ID != "session-s:1:1:0" || got.InputTokens != 10 || got.FreshInputTokens != 10 {
		t.Fatalf("message = %+v", got)
	}
	if got.CacheReadTokens != 3 || got.TotalTokens != 18 || got.Provider != "p" || got.Model != "m" {
		t.Fatalf("tokens/route = %+v", got)
	}
	if got.Date == "" || got.SessionID != "session-s" {
		t.Fatalf("date/session = %+v", got)
	}
}

func TestBuildDSHRetryAccumulates(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/attempt", 1, 1000,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsage100+`}}]}`),
		dshTestEvent(t, "llm/retry-started", 2, 2000, `{"turn":1,"step":1,"retry":1}`),
		dshTestEvent(t, "assistant/message", 3, 3000, dshMessageJSON(1, 1, dshUsage250, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(result.Messages))
	}
	var total int64
	for _, message := range result.Messages {
		total += message.TotalTokens
	}
	if total != 350 {
		t.Fatalf("total = %d, want 350", total)
	}
}

func TestBuildDSHSameAttemptReplaces(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/attempt", 1, 1000,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsage100+`}}]}`),
		dshTestEvent(t, "assistant/message", 2, 2000, dshMessageJSON(1, 1, dshUsage250, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 250 {
		t.Fatalf("messages = %+v, want one 250", result.Messages)
	}
}

func TestBuildDSHZeroAttemptsProduceNothing(t *testing.T) {
	var events []dshEvent
	for i := 0; i < 6; i++ {
		events = append(events, dshTestEvent(t, "assistant/attempt", int64(i), int64(1000+i),
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsageZero+`}}]}`))
	}
	result := buildDSHResult(dshTestDecoded(events...))
	if len(result.Messages) != 0 || len(result.Obsolete) != 0 {
		t.Fatalf("zero attempts: messages=%d obsolete=%d", len(result.Messages), len(result.Obsolete))
	}
}

func TestBuildDSHZeroReplacementProducesObsolete(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1000, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
		dshTestEvent(t, "assistant/message", 2, 2000, dshMessageJSON(1, 1, dshUsageZero, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 0 {
		t.Fatalf("messages = %d, want 0", len(result.Messages))
	}
	if len(result.Obsolete) != 1 || result.Obsolete[0].ID != "session-s:1:1:0" {
		t.Fatalf("obsolete = %+v", result.Obsolete)
	}
}

func TestBuildDSHUsageFallbackToStream(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1000,
			`{"turn":1,"step":1,"message":{"id":"m","source":{"provider":"p","model":"m"}},"stream":[{"chunk":{"type":"usage","usage":{"inputTokens":175,"outputTokens":0,"totalTokens":175}}}]}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 175 {
		t.Fatalf("messages = %+v, want 175", result.Messages)
	}
}

func TestBuildDSHMissingUsageDoesNotOverride(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/attempt", 1, 1000,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsage100+`}}]}`),
		dshTestEvent(t, "assistant/message", 2, 2000,
			`{"turn":1,"step":1,"message":{"id":"m","source":{"provider":"p","model":"m"}}}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 100 {
		t.Fatalf("messages = %+v, want 100", result.Messages)
	}
}

func TestBuildDSHAttemptUsesInheritedRoute(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "request/context", 2, 500, `{"provider":"route-a","model":"model-a"}`),
		dshTestEvent(t, "assistant/attempt", 6, 600,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsage100+`}}]}`),
	)
	decoded.Cut = 5
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(result.Messages))
	}
	if result.Messages[0].Provider != "route-a" || result.Messages[0].Model != "model-a" {
		t.Fatalf("route = %s/%s, want route-a/model-a", result.Messages[0].Provider, result.Messages[0].Model)
	}
}

func TestBuildDSHCutFiltersInheritedMessages(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 5, 1000, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
		dshTestEvent(t, "assistant/message", 11, 2000, dshMessageJSON(1, 2, dshUsage250, "p", "m")),
	)
	decoded.Cut = 10
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 250 {
		t.Fatalf("messages = %+v, want only post-cut 250", result.Messages)
	}
}

func TestBuildDSHAllInheritedIsEmpty(t *testing.T) {
	decoded := dshTestDecoded(dshTestEvent(t, "assistant/message", 1, 1000, dshMessageJSON(1, 1, dshUsage100, "p", "m")))
	decoded.AllInherited = true
	result := buildDSHResult(decoded)
	if len(result.Messages) != 0 || len(result.Sessions) != 0 || len(result.Obsolete) != 0 {
		t.Fatalf("all-inherited result = %+v", result)
	}
}
