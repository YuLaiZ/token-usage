package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

func writeDSHEngineFixture(t *testing.T, sessions, content string) {
	t.Helper()
	dir := filepath.Join(sessions, "--tmp-engine--", "session-engine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func dshEngineMessage(ts int64, turn, step int, usage string) string {
	return `{"type":"assistant/message","seq":` + itoaEngine(step) + `,"time":` + itoa64Engine(ts) +
		`,"data":{"turn":` + itoaEngine(turn) + `,"step":` + itoaEngine(step) +
		`,"usage":` + usage + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
}

func itoaEngine(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func itoa64Engine(n int64) string {
	return itoaEngine(int(n))
}

// TestDSH_RunCollect_EndToEnd 走真实 RunCollect：collector 日期 → engine 落库 →
// 撤销计划 → DB 删除，验证完整链路。
func TestDSH_RunCollect_EndToEnd(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	day1 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local).UnixMilli()
	day2 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local).UnixMilli()

	content := `{"type":"session","version":4,"id":"session-engine","createdAt":1,"cwd":"/tmp/engine","isSeeded":false,"delegationDepth":0}` + "\n" +
		dshEngineMessage(day1, 1, 1, `{"inputTokens":100,"outputTokens":50,"totalTokens":150}`) +
		dshEngineMessage(day2, 1, 2, `{"inputTokens":200,"outputTokens":60,"totalTokens":260}`)
	writeDSHEngineFixture(t, sessions, content)

	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"dsh": {Enabled: true, Paths: map[string]string{"sessions_dir": sessions}},
	}}
	deps := NewDeps(cfg)
	ctx := context.Background()
	run := func(dates []string) {
		t.Helper()
		res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "dsh",
			collector.CollectRequest{Source: collector.CollectSourceClient, Dates: dates}, true, false)
		if err := ValidateResult("dsh", res); err != nil {
			t.Fatalf("RunCollect: %v", err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = ?`, model.ClientDSH).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	run(nil)
	if got := count(); got != 2 {
		t.Fatalf("first run messages = %d, want 2", got)
	}
	var firstDate string
	if err := usageDB.QueryRow(`SELECT date FROM messages WHERE client = ? AND id = 'session-engine:1:1:0'`,
		model.ClientDSH).Scan(&firstDate); err != nil {
		t.Fatalf("date: %v", err)
	}
	if firstDate != time.UnixMilli(day1).In(time.Local).Format("2006-01-02") {
		t.Fatalf("date = %q", firstDate)
	}

	run(nil)
	if got := count(); got != 2 {
		t.Fatalf("second run messages = %d, want 2 (idempotent)", got)
	}

	run([]string{time.UnixMilli(day1).In(time.Local).Format("2006-01-02")})
	if got := count(); got != 2 {
		t.Fatalf("date-filtered run must not delete rows: %d", got)
	}

	// 追加同 key 的最终 0（保留前面的 100）：RunCollect 应携带撤销计划并删除 day1 行。
	appendContent := dshEngineMessage(day1, 1, 1, `{"inputTokens":0,"outputTokens":0,"totalTokens":0}`)
	f, err := os.OpenFile(filepath.Join(sessions, "--tmp-engine--", "session-engine", "session.v4.jsonl"),
		os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := f.WriteString(appendContent); err != nil {
		f.Close()
		t.Fatalf("append fixture: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	run(nil)
	if got := count(); got != 1 {
		t.Fatalf("after obsolete run messages = %d, want 1", got)
	}
	var remainingID string
	if err := usageDB.QueryRow(`SELECT id FROM messages WHERE client = ?`, model.ClientDSH).Scan(&remainingID); err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if remainingID != "session-engine:1:2:0" {
		t.Fatalf("remaining id = %q, want day2 row", remainingID)
	}
}
