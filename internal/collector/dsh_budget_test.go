package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// dshLegalContentOfSize 构造恰好 size 字节、全部为合法 JSON 行的会话日志。
func dshLegalContentOfSize(t *testing.T, size int) string {
	t.Helper()
	header := dshHeaderJSONCwd(4, "session-f37", "/tmp/f37")
	line := `{"type":"pad","data":"` + strings.Repeat("a", 1000) + `"}` + "\n"
	n := (size - len(header)) / len(line)
	rest := size - len(header) - n*len(line)
	tailPrefix := `{"type":"tail","pad":"`
	tailSuffix := `"}` + "\n"
	for rest < len(tailPrefix)+len(tailSuffix)+1 {
		n--
		rest += len(line)
	}
	tail := tailPrefix + strings.Repeat("b", rest-len(tailPrefix)-len(tailSuffix)) + tailSuffix
	return header + strings.Repeat(line, n) + tail
}

func writeDSHSmallWindowZstd(t *testing.T, home, sessionID, cwd, content string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", dshProjectKey(cwd), sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithWindowSize(1<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl.zstd"), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestFixtureF37ExactBudgetPasses(t *testing.T) {
	content := dshLegalContentOfSize(t, dshMaxFrameDecodedBytes)
	home := writeDSHHome(t, "session-f37", "/tmp/f37", content, dshFixtureOpts{Compression: "zstd"})
	path := filepath.Join(home, "sessions", dshProjectKey("/tmp/f37"), "session-f37", "session.v4.jsonl.zstd")

	decoded, err := decodeDSHSession(context.Background(), path, dshCompressionZstd)
	if err != nil {
		t.Fatalf("exact budget must pass: %v", err)
	}
	if decoded.BadLines != 0 {
		t.Fatalf("legal fixture produced %d bad lines", decoded.BadLines)
	}
}

func TestFixtureF37OverBudgetFails(t *testing.T) {
	content := dshLegalContentOfSize(t, dshMaxFrameDecodedBytes+1)
	home := writeDSHHome(t, "session-f37", "/tmp/f37", content, dshFixtureOpts{Compression: "zstd"})
	path := filepath.Join(home, "sessions", dshProjectKey("/tmp/f37"), "session-f37", "session.v4.jsonl.zstd")

	_, err := decodeDSHSession(context.Background(), path, dshCompressionZstd)
	if err == nil || !errors.Is(err, errDSHFrameBudget) {
		t.Fatalf("over budget err = %v, want ErrFrameBudget", err)
	}
}

func TestFixtureF37SmallWindowOverBudget(t *testing.T) {
	content := dshLegalContentOfSize(t, dshMaxFrameDecodedBytes+1)
	home := newDSHHome(t)
	writeDSHSmallWindowZstd(t, home, "session-f37", "/tmp/f37", content)
	path := filepath.Join(home, "sessions", dshProjectKey("/tmp/f37"), "session-f37", "session.v4.jsonl.zstd")

	_, err := decodeDSHSession(context.Background(), path, dshCompressionZstd)
	if err == nil || !errors.Is(err, errDSHFrameBudget) {
		t.Fatalf("small-window over budget err = %v, want ErrFrameBudget", err)
	}
}

func TestFixtureF37OversizedLineZstdKeepsNormalRow(t *testing.T) {
	pad := strings.Repeat("x", 33<<20)
	content := dshSessionWithMessage("session-oversized", "/tmp/o") +
		`{"type":"tool/result","seq":9,"time":3,"data":{"pad":"` + pad + `"}}` + "\n"
	home := writeDSHHome(t, "session-oversized", "/tmp/o", content, dshFixtureOpts{Compression: "zstd"})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("oversized line dropped the normal row: %d messages", len(result.Messages))
	}
	if result.FileStatuses[0].OversizedLines != 1 || result.FileStatuses[0].FullyParsed() {
		t.Fatalf("status = %+v", result.FileStatuses[0])
	}
}

func TestFixtureF37FailureDoesNotBlockHealthySession(t *testing.T) {
	over := dshLegalContentOfSize(t, dshMaxFrameDecodedBytes+1)
	home := newDSHHome(t)
	writeDSHHomeInto(t, home, "session-bad", "/tmp/bad", over, dshFixtureOpts{Compression: "zstd"})
	writeDSHSession(t, home, "session-good", "/tmp/good", dshSessionWithMessage("session-good", "/tmp/good"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatalf("over-budget session must set PartialErr")
	}
	if len(result.Messages) != 1 || result.Messages[0].SessionID != "session-good" {
		t.Fatalf("healthy session missing: %+v", result.Messages)
	}
}

// writeDSHHomeInto 在已有 home 下写一个会话（writeDSHHome 会新建 home）。
func writeDSHHomeInto(t *testing.T, home, sessionID, cwd, content string, opts dshFixtureOpts) {
	t.Helper()
	writeDSHSession(t, home, sessionID, cwd, content, opts)
}
