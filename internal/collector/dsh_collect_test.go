package collector

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func newDSHTestCollector(t testing.TB, home string) *DSHCollector {
	t.Helper()
	cfg := &config.Config{Clients: map[string]config.Client{
		"dsh": {Paths: map[string]string{"sessions_dir": filepath.Join(home, "sessions")}},
	}}
	return NewDSHCollector(cfg)
}

func dshSessionWithMessage(id, cwd string) string {
	return dshHeaderJSONCwd(4, id, cwd) +
		`{"type":"user/message","seq":0,"time":1,"data":{"content":[]}}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}}` + "\n"
}

func TestDSHCollectSessionMetadata(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/project-a", dshSessionWithMessage("session-a", "/tmp/project-a"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("PartialErr: %v", result.PartialErr)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	got := result.Sessions[0]
	if got.ID != "session-a" || got.Client != "DeepSeek Harness" {
		t.Fatalf("session = %+v", got)
	}
	if got.Directory != "/tmp/project-a" || got.Project != "project-a" {
		t.Fatalf("directory/project = %q/%q", got.Directory, got.Project)
	}
}

func TestDSHCollectSkipsEmptySession(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-empty", "/tmp/a", dshHeaderJSON(4, "session-empty"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("Collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("empty session must not produce a session row: %+v", result.Sessions)
	}
}

func TestDSHCollectNoCwd(t *testing.T) {
	home := newDSHHome(t)
	content := `{"type":"session","version":4,"id":"session-nocwd","createdAt":1,"isSeeded":false,"delegationDepth":0}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n"
	writeDSHSession(t, home, "session-nocwd", "/tmp/a", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].Directory != "" || result.Sessions[0].Project != "" {
		t.Fatalf("no-cwd session = %+v", result.Sessions)
	}
}

func TestDSHCollectKeepsHeaderCwd(t *testing.T) {
	home := newDSHHome(t)
	content := dshHeaderJSONCwd(4, "session-move", "/tmp/original") +
		`{"type":"working-directory/change","seq":0,"time":1,"data":{"cwd":"/tmp/other"}}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n"
	writeDSHSession(t, home, "session-move", "/tmp/original", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].Directory != "/tmp/original" {
		t.Fatalf("directory = %+v, want header cwd", result.Sessions)
	}
}

func TestDSHCollectVersionMismatch(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("session-a", "/tmp/a"), dshFixtureOpts{Version: 3})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("mismatched version must not produce a session: %+v", result.Sessions)
	}
	if result.PartialErr == nil || !strings.Contains(result.PartialErr.Error(), "header version") {
		t.Fatalf("PartialErr = %v, want version mismatch", result.PartialErr)
	}
}

func TestDSHCollectCorruptFrame(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("session-a", "/tmp/a"), dshFixtureOpts{
		Compression: "zstd",
		CorruptAt:   5,
	})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Sessions) != 0 || result.PartialErr == nil {
		t.Fatalf("corrupt frame: sessions=%d err=%v", len(result.Sessions), result.PartialErr)
	}
}

func TestDSHCollectZstdSession(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-z", "/tmp/z", dshSessionWithMessage("session-z", "/tmp/z"), dshFixtureOpts{
		Compression: "zstd",
		Frames:      2,
	})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("Collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
}

func TestDSHDecodeTornFrame(t *testing.T) {
	content := dshHeaderJSON(4, "session-t") +
		`{"type":"user/message","seq":0,"time":1,"data":{}}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n"
	home := writeDSHHome(t, "session-t", "/tmp/t", content, dshFixtureOpts{
		Compression: "zstd",
		Frames:      2,
		Truncate:    3,
	})
	path := filepath.Join(home, "sessions", dshProjectKey("/tmp/t"), "session-t", "session.v4.jsonl.zstd")

	decoded, err := decodeDSHSession(context.Background(), path, dshCompressionZstd)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	if decoded.HasUsageEvents {
		t.Fatalf("torn second frame must not contribute messages")
	}
}

func TestDSHComputeCut(t *testing.T) {
	events := []dshEvent{
		{Type: "session/end-seed", Seq: 10, Data: []byte(`{}`)},
		{Type: "session/end-seed", Seq: 20, Data: []byte(`{"inherited":true}`)},
		{Type: "session/end-seed", Seq: 30, Data: []byte(`{"inherited":true}`)},
	}
	if cut, all := computeDSHCut(events, false); cut != -1 || all {
		t.Fatalf("not seeded: cut=%d all=%v", cut, all)
	}
	if cut, all := computeDSHCut(events, true); cut != 30 || all {
		t.Fatalf("seeded: cut=%d all=%v, want 30/false", cut, all)
	}
	if cut, all := computeDSHCut([]dshEvent{{Type: "session", Seq: 0}}, true); cut != -1 || !all {
		t.Fatalf("seeded without marker: cut=%d all=%v, want -1/true", cut, all)
	}
}

func TestDSHFrameBudget(t *testing.T) {
	r := &frameBudgetReader{r: strings.NewReader(strings.Repeat("a", 10)), limit: 5}
	buf := make([]byte, 20)
	if _, err := r.Read(buf); !errors.Is(err, errDSHFrameBudget) {
		t.Fatalf("Read err = %v, want budget exceeded", err)
	}
}
