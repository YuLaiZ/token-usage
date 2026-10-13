package engine

// Codex 压缩 rollout 真实样本的跳过门验收：真实 ~/.codex sessions 下连续
// 两轮 ScanExistingJSONL，第二轮有门文件（含全部零坏行 .jsonl.zst）门命中
// 跳过——验证「帧完整解码即 TrailingNewline」合同在真实压缩数据上的效果
//（门失效则每轮全量重解压）。全程只读 ~/.codex、只写 :memory: 库，不触达
// ~/.token-usage。
//
// 显式开关（CODEX_ZST_REAL_ACCEPTANCE=1）+ 样本探测双守卫。

import (
	"context"
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

// realCodexGateEnv 探测真实样本环境；未显式开启或无样本时 Skip。
func realCodexGateEnv(t *testing.T) (sessionsDir, stateDir string) {
	t.Helper()
	if os.Getenv("CODEX_ZST_REAL_ACCEPTANCE") != "1" {
		t.Skip("未设置 CODEX_ZST_REAL_ACCEPTANCE=1（真实样本验收按需显式执行）")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sessionsDir = filepath.Join(home, ".codex", "sessions")
	stateDir = filepath.Join(home, ".codex")
	if _, err := os.Stat(filepath.Join(stateDir, "state_5.sqlite")); err != nil {
		t.Skipf("无 Codex state DB: %v", err)
	}
	return sessionsDir, stateDir
}

// countRealRollouts 统计双 root 下双形态 rollout 文件总数与 .zst 数。
func countRealRollouts(t *testing.T, sessionsDir string) (total, zst int) {
	t.Helper()
	roots := []string{
		sessionsDir,
		filepath.Join(filepath.Dir(sessionsDir), "archived_sessions"),
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				name := d.Name()
				if strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.zst") {
					total++
					if strings.HasSuffix(name, ".jsonl.zst") {
						zst++
					}
				}
			}
			return err
		})
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return total, zst
}

// TestCodexRealGate_SecondRoundSkipsAll：首轮全扫后门记录必须覆盖全部「门资格」
// .zst（零坏行且帧完整；帧完整性合同的直接验证——门失效则 .zst 门记录缺失）；
// 第二轮 skipped 计数 == 门记录数（有门文件全部命中跳过，无一重解压）；消息行数
// 两轮一致。存量坏行 .zst 与尾行未终结的活跃 .jsonl 合法不写门，不参与核算。
func TestCodexRealGate_SecondRoundSkipsAll(t *testing.T) {
	sessionsDir, stateDir := realCodexGateEnv(t)
	total, zst := countRealRollouts(t, sessionsDir)
	if zst == 0 {
		t.Skip("无压缩样本")
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewCodexCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	// 门资格探测：先经 collector 直跑一轮（只读源文件，不落库），数出零坏行
	// 且帧完整的 .zst 数——真实历史数据存在压缩前即有的存量坏行（双侧同坏，
	// 由 collector 包诊断锚定），含坏行文件不写门与 .jsonl 形态同语义。
	preRes, err := collector.NewCodexCollector(cfg).Collect(context.Background(),
		collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("门资格探测轮失败: %v", err)
	}
	var zstGateEligible int
	for _, fs := range preRes.FileStatuses {
		if strings.HasSuffix(fs.Path, ".zst") && fs.FullyParsed() {
			zstGateEligible++
		}
	}
	if zstGateEligible == 0 {
		t.Skip("无门资格压缩文件")
	}

	run := func(handler *levelCaptureHandler) {
		t.Helper()
		start := time.Now()
		result := RunCollect(context.Background(), deps, usageDB,
			slog.New(handler), io.Discard, "codex",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
			false, false)
		if !result.Complete() {
			t.Fatalf("catch-up failed: %+v", result)
		}
		t.Logf("catch-up 轮完成, 耗时 %s", time.Since(start))
	}

	run(&levelCaptureHandler{})
	logs, err := db.GetFileScanLogs(context.Background(), usageDB, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var gated, zstGated int
	for path := range logs {
		if strings.HasSuffix(path, ".zst") {
			zstGated++
		}
		gated++
	}
	if zstGated != zstGateEligible {
		t.Fatalf("首轮门记录覆盖 .zst %d/%d（门资格=零坏行且帧完整；缺失即压缩形态跳过门合同失效）", zstGated, zstGateEligible)
	}
	var first int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&first); err != nil {
		t.Fatal(err)
	}

	handler := &levelCaptureHandler{}
	run(handler)
	if n := handler.gateSkippedCount(); n != gated {
		t.Fatalf("第二轮门命中 %d, 门记录数 %d（有门文件必须全部命中跳过）", n, gated)
	}
	var second int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("第二轮消息数 %d != 首轮 %d（重读不得改变行集）", second, first)
	}
	t.Logf("门二轮: 文件 %d（.zst %d, 门资格 %d）, 门记录 %d（含 .zst %d 全部入档）, 首轮消息 %d, 第二轮全部命中跳过",
		total, zst, zstGateEligible, gated, zstGated, first)
}
