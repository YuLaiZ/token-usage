//go:build perf

package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// TestScanGatePerformanceSynthetic（性能验收，非正确性替代）：
// 合成 ~400MB claude 目录（400 文件 × ~1MB，每文件 40 条带 usage 的 assistant 消息），
// 对比 cold 全量 catch-up（门空）与门命中 catch-up（第二轮，全部跳过）的耗时。
// 数字经 t.Logf 输出；断言只做量级方向（命中轮显著快于 cold 轮），
// 不作为正确性证据。
//
// 运行：go test -tags perf ./internal/engine -run TestScanGatePerformanceSynthetic -v -timeout 20m
func TestScanGatePerformanceSynthetic(t *testing.T) {
	requireGateIdentityEnv(t)
	dir := t.TempDir()
	// 单文件 ~1MB：1 行 10KB assistant（带 usage）× 100 行，重复 1MB / 10KB ≈ 100 块。
	block := strings.Repeat("x", 10*1024)
	var oneFile strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&oneFile,
			`{"type":"assistant","sessionId":"s","timestamp":"2026-07-08T10:%02d:00+08:00","cwd":"/tmp/project","message":{"id":"msg-%d","role":"assistant","model":"m","content":[{"type":"text","text":"%s"}],"usage":{"input_tokens":100,"output_tokens":20}}}`+"\n",
			i%60, i, block)
	}
	content := oneFile.String()
	files := 400
	for i := 0; i < files; i++ {
		p := filepath.Join(dir, fmt.Sprintf("s%03d.jsonl", i))
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	totalMB := int64(files) * int64(len(content)) / (1024 * 1024)

	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": dir}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewClaudeCollector(cfg)}, nil)
	usageDB, err := db.Open(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	catchUp := func() time.Duration {
		start := time.Now()
		result := RunCollect(context.Background(), deps, usageDB,
			slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
			false, false)
		if !result.Complete() {
			t.Fatalf("catch-up failed: %+v", result)
		}
		return time.Since(start)
	}

	cold := catchUp() // 门空：全量解析 + upsert
	warm := catchUp() // 门全命中：仅 stat 快照

	var msgs int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic corpus: %d files / %dMB / %d messages", files, totalMB, msgs)
	t.Logf("cold catch-up (full parse + upsert): %v", cold)
	t.Logf("warm catch-up (gate hit, stat only): %v", warm)
	t.Logf("speedup: %.1fx", float64(cold)/float64(max(warm, time.Millisecond)))

	// 量级方向断言：门命中轮至少比 cold 轮快一个数量级（400 次 stat 的量级）。
	if warm > cold/10 {
		t.Errorf("门命中轮耗时 %v 未达 cold 轮 1/10（%v），收益不成立", warm, cold/10)
	}
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// TestScanGateContentConsistencyTiered（分级一致性之 12MB/50MB 级，perf 标签下运行）：
// 大文件路径（Scanner 缓冲增长、多消息块解析）在有门/无门两库下 messages 行集零差异。
// 运行：go test -tags perf ./internal/engine -run TestScanGateContentConsistencyTiered -v -timeout 20m
func TestScanGateContentConsistencyTiered(t *testing.T) {
	requireGateIdentityEnv(t)
	for _, mb := range []int{12, 50} {
		t.Run(fmt.Sprintf("%dMB", mb), func(t *testing.T) {
			dir := t.TempDir()
			var b strings.Builder
			b.WriteString(gateFixtureJSONL)
			for i := 0; b.Len() < mb*1024*1024; i++ {
				b.WriteString(strings.Replace(gateFixtureJSONL, "msg-1", fmt.Sprintf("filler-%06d", i), 1))
			}
			writeFileT(t, filepath.Join(dir, "PROJECTS", "big.jsonl"), b.String())

			cfg := &config.Config{Clients: map[string]config.Client{
				"claude": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Join(dir, "PROJECTS")}},
			}}
			newDeps := func() *Deps {
				return NewDepsWithCollectors(cfg, []collector.Collector{collector.NewClaudeCollector(cfg)}, nil)
			}
			runCatchUp := func(deps *Deps, usageDB *db.DB) {
				result := RunCollect(context.Background(), deps, usageDB,
					slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, "claude",
					collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
					false, false)
				if !result.Complete() {
					t.Fatalf("catch-up failed: %+v", result)
				}
			}
			gatedDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer gatedDB.Close()
			gatedDeps := newDeps()
			runCatchUp(gatedDeps, gatedDB)
			runCatchUp(gatedDeps, gatedDB)
			ungatedDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer ungatedDB.Close()
			ungatedDeps := newDeps()
			runCatchUp(ungatedDeps, ungatedDB)
			if _, err := ungatedDB.Exec("DELETE FROM file_scan_log"); err != nil {
				t.Fatal(err)
			}
			runCatchUp(ungatedDeps, ungatedDB)

			q := `SELECT id, session_id, client, date, ts, model, provider, input_tokens, fresh_input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, total_tokens FROM messages ORDER BY id`
			if got, want := dumpRows(t, gatedDB, q), dumpRows(t, ungatedDB, q); got != want {
				t.Errorf("%dMB fixture messages 行集不一致（有门 vs 无门）", mb)
			}
			var n int
			if err := gatedDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
				t.Fatal(err)
			}
			t.Logf("%dMB tier: %d messages, rows identical", mb, n)
		})
	}
}
