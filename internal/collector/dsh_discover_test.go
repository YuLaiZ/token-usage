package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dshHeaderJSON 生成一行的会话 header，供后续的 header/版本测试复用。
func dshHeaderJSON(version int, id string) string {
	return dshHeaderJSONCwd(version, id, "/tmp/dsh-test")
}

func dshHeaderJSONCwd(version int, id, cwd string) string {
	return fmt.Sprintf(
		`{"type":"session","version":%d,"id":%q,"createdAt":1,"cwd":%q,"isSeeded":false,"delegationDepth":0}`+"\n",
		version, id, cwd)
}

func TestParseDSHGenerationName(t *testing.T) {
	cases := []struct {
		name        string
		wantVersion int
		wantComp    dshCompression
		wantOK      bool
	}{
		{"session.jsonl", 0, dshCompressionNone, true},
		{"session.jsonl.zstd", 0, dshCompressionZstd, true},
		{"session.v1.jsonl", 1, dshCompressionNone, true},
		{"session.v4.jsonl.zstd", 4, dshCompressionZstd, true},
		{"session.v12.jsonl", 12, dshCompressionNone, true},
		{"session.lock", 0, "", false},
		{"session.tmp.jsonl", 0, "", false},
		{"session.v0.jsonl", 0, "", false},
		{"session.v01.jsonl", 0, "", false},
		{"notes.txt", 0, "", false},
	}
	for _, tc := range cases {
		version, comp, ok := parseDSHGenerationName(tc.name)
		if ok != tc.wantOK || version != tc.wantVersion || comp != tc.wantComp {
			t.Fatalf("%s: got (%d,%q,%v), want (%d,%q,%v)",
				tc.name, version, comp, ok, tc.wantVersion, tc.wantComp, tc.wantOK)
		}
	}
}

func TestDiscoverDSHPicksHighestGeneration(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshHeaderJSON(3, "session-a"), dshFixtureOpts{Version: 3})
	writeDSHSession(t, home, "session-a", "/tmp/a", dshHeaderJSON(4, "session-a"), dshFixtureOpts{})

	got := discoverDSHSessions(filepath.Join(home, "sessions"))
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (warnings=%v errors=%v)", len(got.Sessions), got.Warnings, got.Errors)
	}
	if !strings.HasSuffix(got.Sessions[0].Path, "session.v4.jsonl") {
		t.Fatalf("selected %s, want v4", got.Sessions[0].Path)
	}
	if got.Sessions[0].Version != 4 {
		t.Fatalf("version = %d, want 4", got.Sessions[0].Version)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", got.Errors)
	}
}

func TestDiscoverDSHRejectsMixedEncodings(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshHeaderJSON(4, "session-a"), dshFixtureOpts{
		Compression:       "zstd",
		SecondCompression: "none",
	})

	got := discoverDSHSessions(filepath.Join(home, "sessions"))
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions = %d, want 0", len(got.Sessions))
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "mixed encodings") {
		t.Fatalf("errors = %v, want mixed encodings", got.Errors)
	}
}

func TestDiscoverDSHIgnoresLockAndNoise(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshHeaderJSON(4, "session-a"), dshFixtureOpts{})
	dir := filepath.Join(home, "sessions", dshProjectKey("/tmp/a"), "session-a")
	if err := os.WriteFile(filepath.Join(dir, "session.lock"), nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("noise"), 0o600); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "README"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write root noise: %v", err)
	}

	got := discoverDSHSessions(filepath.Join(home, "sessions"))
	if len(got.Sessions) != 1 || len(got.Errors) != 0 {
		t.Fatalf("sessions=%d errors=%v, want 1/0", len(got.Sessions), got.Errors)
	}
}

func TestDiscoverDSHMissingRootIsEmpty(t *testing.T) {
	got := discoverDSHSessions(filepath.Join(t.TempDir(), "missing"))
	if len(got.Sessions) != 0 || len(got.Errors) != 0 {
		t.Fatalf("missing root must be empty: %+v", got)
	}
}
