package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// writeDSHZstdFrame 写一个只含 header + 指定条数消息的 zstd 帧；
// appendMode 为 true 时追加到文件末尾（模拟 DSH 的只追加帧）。
func writeDSHZstdFrame(t *testing.T, sessions string, messages int, appendMode bool) {
	t.Helper()
	dir := filepath.Join(sessions, "--tmp-daemon--", "session-e2e")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var payload bytes.Buffer
	payload.WriteString(`{"type":"session","version":4,"id":"session-e2e","createdAt":1,"cwd":"/tmp/daemon","isSeeded":false,"delegationDepth":0}` + "\n")
	for i := 1; i <= messages; i++ {
		payload.WriteString(`{"type":"assistant/message","seq":` + strconv.Itoa(i) + `,"time":` + strconv.FormatInt(1700000000000+int64(i), 10) +
			`,"data":{"turn":1,"step":` + strconv.Itoa(i) + `,"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150},"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n")
	}
	var frame bytes.Buffer
	w, err := zstd.NewWriter(&frame)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := w.Write(payload.Bytes()); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(filepath.Join(dir, "session.v4.jsonl.zstd"), flags, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(frame.Bytes()); err != nil {
		t.Fatalf("append frame: %v", err)
	}
}

func countDSHMessages(t *testing.T, dataDir string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(dataDir, "usage.db"))
	if err != nil {
		return -1
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = 'DeepSeek Harness'`).Scan(&n); err != nil {
		return -1
	}
	return n
}

func waitDSHMessages(t *testing.T, dataDir string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countDSHMessages(t, dataDir) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d dsh messages, got %d", want, countDSHMessages(t, dataDir))
}

func TestDSHDaemonEndToEnd(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	writeDSHZstdFrame(t, sessions, 1, false)

	cfg := &config.Config{
		DataDir: filepath.Join(home, "data"),
		Clients: map[string]config.Client{
			"dsh": {Enabled: true, Paths: map[string]string{"sessions_dir": sessions}},
		},
		Daemon: config.DaemonConfig{PollInterval: 1},
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}

	runOnce := func(instanceID string) (context.CancelFunc, <-chan error, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			done <- Run(ctx, cfg, RunOptions{
				InstanceID: instanceID,
				OnDaemonLockCommit: func() error {
					close(ready)
					return nil
				},
				OpenResources: openTestResources(t, nil),
			})
		}()
		return cancel, done, ready
	}

	cancel, done, ready := runOnce("e2e-1")
	<-ready
	waitDSHMessages(t, cfg.DataDir, 1)

	writeDSHZstdFrame(t, sessions, 2, true)
	waitDSHMessages(t, cfg.DataDir, 2)

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("first run: %v", err)
	}

	// 重启：新实例必须真正完成启动；文件不变时不重复写。
	cancel2, done2, ready2 := runOnce("e2e-2")
	<-ready2
	time.Sleep(1500 * time.Millisecond)
	if got := countDSHMessages(t, cfg.DataDir); got != 2 {
		t.Fatalf("restart duplicated rows: %d", got)
	}

	writeDSHZstdFrame(t, sessions, 3, true)
	waitDSHMessages(t, cfg.DataDir, 3)
	cancel2()
	if err := <-done2; err != nil && err != context.Canceled {
		t.Fatalf("second run: %v", err)
	}
}
