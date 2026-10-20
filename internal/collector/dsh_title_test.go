package collector

import "testing"

func TestBuildDSHTitleAfterCut(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "session/title", 5, 1000, `{"title":"inherited"}`),
		dshTestEvent(t, "session/title", 11, 2000, `{"title":"own"}`),
		dshTestEvent(t, "assistant/message", 12, 3000, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
	)
	decoded.Cut = 10
	result := buildDSHResult(decoded)
	if len(result.Sessions) != 1 || result.Sessions[0].Title != "own" {
		t.Fatalf("title = %+v, want own", result.Sessions)
	}
}

func TestBuildDSHTitleLastWins(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "session/title", 1, 1000, `{"title":"first"}`),
		dshTestEvent(t, "session/title", 2, 2000, `{"title":"second"}`),
		dshTestEvent(t, "assistant/message", 3, 3000, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Sessions) != 1 || result.Sessions[0].Title != "second" {
		t.Fatalf("title = %+v, want second", result.Sessions)
	}
}

func TestBuildDSHParentID(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1000, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
	)
	decoded.Header.ParentSession = "parent-session"
	result := buildDSHResult(decoded)
	if len(result.Sessions) != 1 || result.Sessions[0].ParentID != "parent-session" {
		t.Fatalf("parent = %+v", result.Sessions)
	}
}

func TestBuildDSHIgnoresCompactionSummary(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "compaction/summary", 1, 1000,
			`{"compactionId":"c1","summary":"x","provider":"p","model":"m","usage":`+dshUsage100+`}`),
		dshTestEvent(t, "assistant/message", 2, 2000, dshMessageJSON(1, 1, dshUsage250, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 250 {
		t.Fatalf("messages = %+v, want only the assistant message", result.Messages)
	}
}

func TestComputeDSHCutChain(t *testing.T) {
	events := []dshEvent{
		{Type: "session/end-seed", Seq: 15, Data: []byte(`{}`)},
		{Type: "session/end-seed", Seq: 593, Data: []byte(`{}`)},
		{Type: "session/end-seed", Seq: 993, Data: []byte(`{"inherited":true}`)},
		{Type: "session/end-seed", Seq: 1047, Data: []byte(`{"inherited":true}`)},
	}
	cut, all := computeDSHCut(events, true)
	if all || cut != 1047 {
		t.Fatalf("cut = %d all = %v, want 1047/false", cut, all)
	}
}
