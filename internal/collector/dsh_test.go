package collector

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// dshFixtureOpts 控制写入 ~/.dsh 形态临时 home 的落盘形态。
// 所有字段零值都代表「不启用该变形」，避免调用方漏设时意外截断/损坏。
type dshFixtureOpts struct {
	Compression       string // "zstd" | "none"；空按 "none"
	SecondCompression string // 非空时在同目录再写一份另一编码（混合编码夹具）
	Frames            int    // zstd 拆帧数；<=1 表示单帧
	Truncate          int    // >0 时截掉末帧 N 字节
	CorruptAt         int    // >0 时翻转该字节（1-based）
	Window            int    // 预留：大窗口帧（尚未生效）
	Version           int    // 会话格式版本；0 表示默认 v4
}

// dshProjectKey 复刻 DSH 的可读项目目录键：分隔符折叠为 '-'，前后包 '--'。
// 仅用于夹具布局；采集实现不依赖目录名（真实 cwd 取自 header）。
func dshProjectKey(cwd string) string {
	slug := strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-")
	slug = strings.ReplaceAll(slug, "\\", "-")
	if slug == "" {
		slug = "root"
	}
	return "--" + slug + "--"
}

// writeDSHHome 在 t.TempDir() 下构造一份 ~/.dsh 形态的临时 home，返回 home 路径。
// 会话落在 sessions/<projectKey>/<sessionID>/session.v4.jsonl[.zstd]。
func newDSHHome(t testing.TB) string {
	t.Helper()
	return t.TempDir()
}

func writeDSHHome(t testing.TB, sessionID, cwd, content string, opts dshFixtureOpts) string {
	t.Helper()
	home := newDSHHome(t)
	writeDSHSession(t, home, sessionID, cwd, content, opts)
	return home
}

func writeDSHSession(t testing.TB, home, sessionID, cwd, content string, opts dshFixtureOpts) {
	t.Helper()

	dir := filepath.Join(home, "sessions", dshProjectKey(cwd), sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create session dir: %v", err)
	}

	primary := opts.Compression
	if primary == "" {
		primary = "none"
	}

	version := opts.Version
	if version == 0 {
		version = 4
	}

	write := func(encoding string) {
		name := fmt.Sprintf("session.v%d.jsonl", version)
		data := []byte(content)
		if encoding == "zstd" {
			name += ".zstd"
			data = encodeDSHFrames(t, content, opts.Frames)
		}
		if encoding == primary {
			if opts.Truncate > 0 {
				if opts.Truncate >= len(data) {
					t.Fatalf("truncate %d exceeds file size %d", opts.Truncate, len(data))
				}
				data = data[:len(data)-opts.Truncate]
			}
			if opts.CorruptAt > 0 {
				pos := opts.CorruptAt - 1
				if pos >= len(data) {
					t.Fatalf("corrupt offset %d exceeds file size %d", pos, len(data))
				}
				data[pos] ^= 0xFF
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write(primary)
	if opts.SecondCompression != "" && opts.SecondCompression != primary {
		write(opts.SecondCompression)
	}
}

// encodeDSHFrames 把内容按 Frames 拆成多个 zstd 帧（模拟 DSH 的「一批一帧」）。
func encodeDSHFrames(t testing.TB, content string, frames int) []byte {
	t.Helper()

	encode := func(part string) []byte {
		var buf bytes.Buffer
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatalf("zstd write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}
		return buf.Bytes()
	}

	if frames <= 1 {
		return encode(content)
	}

	lines := strings.SplitAfter(content, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return encode(content)
	}
	per := (len(lines) + frames - 1) / frames
	var out bytes.Buffer
	for i := 0; i < len(lines); i += per {
		end := i + per
		if end > len(lines) {
			end = len(lines)
		}
		out.Write(encode(strings.Join(lines[i:end], "")))
	}
	return out.Bytes()
}

func readDSHFile(t testing.TB, home, sessionID, cwd, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "sessions", dshProjectKey(cwd), sessionID, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func decodeDSHZstd(t testing.TB, data []byte) string {
	t.Helper()
	r, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	return string(out)
}

const dshFixtureContent = `{"type":"session","version":4,"id":"session-test","createdAt":1,"cwd":"/tmp/dsh-test","isSeeded":false,"delegationDepth":0}
{"type":"user/message","seq":0,"time":1,"data":{"content":[]}}
{"type":"assistant/message","seq":1,"time":2,"data":{"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0,"totalTokens":15}}}
`

func TestDSHWriteHomePlain(t *testing.T) {
	home := writeDSHHome(t, "session-test", "/tmp/dsh-test", dshFixtureContent, dshFixtureOpts{})
	got := readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl")
	if string(got) != dshFixtureContent {
		t.Fatalf("plain content mismatch: %q", got)
	}
}

func TestDSHWriteHomeZstdFrames(t *testing.T) {
	home := writeDSHHome(t, "session-test", "/tmp/dsh-test", dshFixtureContent, dshFixtureOpts{
		Compression: "zstd",
		Frames:      3,
	})
	data := readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl.zstd")
	if got := decodeDSHZstd(t, data); got != dshFixtureContent {
		t.Fatalf("decoded content mismatch: %q", got)
	}
}

func TestDSHWriteHomeTruncated(t *testing.T) {
	opts := dshFixtureOpts{Compression: "zstd", Truncate: 3}
	home := writeDSHHome(t, "session-test", "/tmp/dsh-test", dshFixtureContent, opts)
	full := encodeDSHFrames(t, dshFixtureContent, opts.Frames)
	got := readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl.zstd")
	if len(got) != len(full)-opts.Truncate {
		t.Fatalf("truncated size = %d, want %d", len(got), len(full)-opts.Truncate)
	}
}

func TestDSHWriteHomeCorrupt(t *testing.T) {
	home := writeDSHHome(t, "session-test", "/tmp/dsh-test", dshFixtureContent, dshFixtureOpts{
		Compression: "zstd",
		CorruptAt:   5,
	})
	got := readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl.zstd")
	want := encodeDSHFrames(t, dshFixtureContent, 0)
	if len(got) != len(want) || got[4] == want[4] {
		t.Fatalf("corrupt fixture did not change byte 5")
	}
}

func TestDSHWriteHomeDualEncoding(t *testing.T) {
	home := writeDSHHome(t, "session-test", "/tmp/dsh-test", dshFixtureContent, dshFixtureOpts{
		Compression:       "zstd",
		SecondCompression: "none",
	})
	_ = readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl")
	_ = readDSHFile(t, home, "session-test", "/tmp/dsh-test", "session.v4.jsonl.zstd")
}

func TestDSHCollectSkeleton(t *testing.T) {
	c := NewDSHCollector(nil)
	if got := c.Name(); got != "dsh" {
		t.Fatalf("Name() = %q, want dsh", got)
	}
	if got := c.SyncSources(); got != nil {
		t.Fatalf("SyncSources() = %v, want nil", got)
	}
	res, err := c.Collect(nil, CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(res.Messages) != 0 || len(res.Sessions) != 0 {
		t.Fatalf("skeleton Collect() must be empty, got %d messages / %d sessions",
			len(res.Messages), len(res.Sessions))
	}
}
