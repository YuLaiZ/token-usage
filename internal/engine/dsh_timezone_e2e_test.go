package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

const dshF38TS = "2026-10-19T00:30:00Z"

func dshF38Millis() int64 {
	return time.Date(2026, 10, 19, 0, 30, 0, 0, time.UTC).UnixMilli()
}

func writeF38Fixture(t *testing.T, sessions string, extraZero bool) {
	t.Helper()
	dir := filepath.Join(sessions, "--tmp-f38--", "session-f38")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ts := dshF38Millis()
	content := `{"type":"session","version":4,"id":"session-f38","createdAt":1,"cwd":"/tmp/f38","isSeeded":false,"delegationDepth":0}` + "\n" +
		`{"type":"assistant/message","seq":1,"time":` + itoa64Engine(ts) + `,"data":{"turn":1,"step":1,"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150},"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	if extraZero {
		content += `{"type":"assistant/message","seq":2,"time":` + itoa64Engine(ts) + `,"data":{"turn":1,"step":1,"usage":{"inputTokens":0,"outputTokens":0,"totalTokens":0},"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	}
	path := filepath.Join(dir, "session.v4.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func runF38Child(t *testing.T) {
	t.Helper()
	sessions := os.Getenv("DSH_F38_SESSIONS")
	dates := strings.Split(os.Getenv("DSH_F38_DATES"), ",")
	if len(dates) == 1 && dates[0] == "" {
		dates = nil
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"dsh": {Enabled: true, Paths: map[string]string{"sessions_dir": sessions}},
	}}
	usageDB, err := db.Open(os.Getenv("DSH_F38_DB"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	res := RunCollect(context.Background(), NewDeps(cfg), usageDB, slog.Default(), io.Discard, "dsh",
		collector.CollectRequest{Source: collector.CollectSourceClient, Dates: dates}, true, false)
	if err := ValidateResult("dsh", res); err != nil {
		t.Fatalf("RunCollect: %v", err)
	}
}

func runF38ChildProcess(t *testing.T, tz, dbPath, sessions string, dates []string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestF38RunCollectAcrossTimezone")
	var env []string
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "TZ=") || strings.HasPrefix(item, "DSH_F38_") {
			continue
		}
		env = append(env, item)
	}
	env = append(env,
		"TZ="+tz,
		"DSH_F38_CHILD=1",
		"DSH_F38_DB="+dbPath,
		"DSH_F38_SESSIONS="+sessions,
		"DSH_F38_DATES="+strings.Join(dates, ","),
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %s: %v\n%s", tz, err, out)
	}
	return string(out)
}

func dshF38DBDates(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer usageDB.Close()
	rows, err := db.FindMessageIDsByClient(context.Background(), usageDB, model.ClientDSH, []string{"session-f38:1:1:0"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	return rows
}

// TestF38RunCollectAcrossTimezone 走真实 RunCollect + 子进程 TZ：
// 上海落库 → 洛杉矶重采同 TS → 日期保留 → 洛杉矶撤销告警 → 上海撤销成功 → 幂等 → 重建后撤销。
func TestF38RunCollectAcrossTimezone(t *testing.T) {
	if os.Getenv("DSH_F38_CHILD") == "1" {
		runF38Child(t)
		return
	}

	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	dbPath := filepath.Join(home, "usage.db")
	writeF38Fixture(t, sessions, false)

	runF38ChildProcess(t, "Asia/Shanghai", dbPath, sessions, nil)
	if got := dshF38DBDates(t, dbPath)["session-f38:1:1:0"]; got != "2026-10-19" {
		t.Fatalf("shanghai date = %q, want 2026-10-19", got)
	}

	// 追加同 key 的零值样本，洛杉矶重采同 TS。
	writeF38Fixture(t, sessions, true)
	laLog := runF38ChildProcess(t, "America/Los_Angeles", dbPath, sessions, []string{"2026-10-18"})
	if !strings.Contains(laLog, "db_date=2026-10-19") || !strings.Contains(laLog, "request_dates=[2026-10-18]") {
		t.Fatalf("LA obsolete warning missing: %s", laLog)
	}
	if got := dshF38DBDates(t, dbPath)["session-f38:1:1:0"]; got != "2026-10-19" {
		t.Fatalf("LA recollect changed date: %q", got)
	}

	// 上海按旧日期撤销：删除成功。
	runF38ChildProcess(t, "Asia/Shanghai", dbPath, sessions, []string{"2026-10-19"})
	if _, ok := dshF38DBDates(t, dbPath)["session-f38:1:1:0"]; ok {
		t.Fatalf("shanghai obsolete did not delete")
	}

	// 再跑一次：删 0 行且无残留，不告警。
	again := runF38ChildProcess(t, "Asia/Shanghai", dbPath, sessions, []string{"2026-10-19"})
	if strings.Contains(again, "still present") {
		t.Fatalf("idempotent obsolete warned: %s", again)
	}

	// 重建数据库：先用只有 100 的 fixture 按洛杉矶落库 10-18，再改出零值撤销。
	rebuildDB := filepath.Join(home, "usage2.db")
	writeF38Fixture(t, sessions, false)
	runF38ChildProcess(t, "America/Los_Angeles", rebuildDB, sessions, nil)
	if got := dshF38DBDates(t, rebuildDB)["session-f38:1:1:0"]; got != "2026-10-18" {
		t.Fatalf("rebuild LA date = %q, want 2026-10-18", got)
	}
	writeF38Fixture(t, sessions, true)
	runF38ChildProcess(t, "America/Los_Angeles", rebuildDB, sessions, []string{"2026-10-18"})
	if _, ok := dshF38DBDates(t, rebuildDB)["session-f38:1:1:0"]; ok {
		t.Fatalf("rebuild LA obsolete did not delete")
	}
}
