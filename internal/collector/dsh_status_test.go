package collector

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDSHFileStatusClean(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("session-a", "/tmp/a"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.FileStatuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(result.FileStatuses))
	}
	if !result.FileStatuses[0].FullyParsed() {
		t.Fatalf("clean file must be fully parsed: %+v", result.FileStatuses[0])
	}
}

func TestDSHFileStatusTorn(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-t", "/tmp/t", dshSessionWithMessage("session-t", "/tmp/t"), dshFixtureOpts{
		Compression: "zstd",
		Frames:      2,
		Truncate:    3,
	})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.FileStatuses) != 1 || !result.FileStatuses[0].TruncatedTail {
		t.Fatalf("status = %+v, want TruncatedTail", result.FileStatuses)
	}
	if result.FileStatuses[0].FullyParsed() {
		t.Fatalf("torn file must not be fully parsed")
	}
}

func TestDSHFileStatusBadLine(t *testing.T) {
	home := newDSHHome(t)
	content := dshSessionWithMessage("session-b", "/tmp/b") + "{not-json}\n"
	writeDSHSession(t, home, "session-b", "/tmp/b", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.FileStatuses) != 1 || result.FileStatuses[0].BadLines != 1 {
		t.Fatalf("status = %+v, want one bad line", result.FileStatuses)
	}
	if result.FileStatuses[0].FullyParsed() {
		t.Fatalf("file with bad line must not be fully parsed")
	}
}

func TestDSHFileStatusCorrupt(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-c", "/tmp/c", dshSessionWithMessage("session-c", "/tmp/c"), dshFixtureOpts{
		Compression: "zstd",
		CorruptAt:   5,
	})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.FileStatuses) != 1 || result.FileStatuses[0].Err == nil {
		t.Fatalf("status = %+v, want Err", result.FileStatuses)
	}
	if result.PartialErr == nil {
		t.Fatalf("corrupt file must set PartialErr")
	}
}

func TestDSHFileStatusPathIsSessionFile(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("session-a", "/tmp/a"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := filepath.Join(home, "sessions", dshProjectKey("/tmp/a"), "session-a", "session.v4.jsonl")
	if result.FileStatuses[0].Path != want {
		t.Fatalf("path = %s, want %s", result.FileStatuses[0].Path, want)
	}
}
