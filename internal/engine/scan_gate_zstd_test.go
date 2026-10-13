package engine

// Codex 压缩 rollout（.jsonl.zst）的 startup 全扫与跳过门集成测试：
// ScanExistingJSONL 在含 .zst 的 sessions 目录双形态采齐；同一 .zst 连续两轮
// 全扫第二轮门命中跳过（不重复解压）——直接验证「帧完整解码即
// TrailingNewline=true」合同：若 trailingNewline 误检 zstd 帧尾字节，门永不
// 推进、二轮全量重读，本测试失败。

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// zstdCompressGateFixture 用单帧 zstd 压缩 fixture（与 Codex 本地历史压缩产物同形态）。
func zstdCompressGateFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

// TestScanGate_CodexCompressedRolloutRoundTwoSkips：混合目录（.jsonl +
// .jsonl.zst）首轮 catch-up 采齐并写门（门记录 key 含 .zst 路径），第二轮门
// 命中跳过（Debug 日志 + DB 消息不变）。
func TestScanGate_CodexCompressedRolloutRoundTwoSkips(t *testing.T) {
	requireGateIdentityEnv(t)
	root := t.TempDir()
	rollouts := filepath.Join(root, "rollouts")
	if err := os.MkdirAll(rollouts, 0755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(rollouts, "r1.jsonl")
	if err := os.WriteFile(plain, []byte(codexGateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	// 会话 id 与消息 id 都替换：messages 的 UPSERT 冲突键是 (client,id)，
	// 两文件消息 id 相同会互相顶掉 session 归属，无法证明双形态都采到。
	compressedFixture := strings.Replace(codexGateFixture, "019f4fdb", "019f4fdc", 1)
	compressedFixture = strings.Replace(compressedFixture, "msg-c1", "msg-z1", 1)
	compressed := filepath.Join(rollouts, "r2.jsonl.zst")
	if err := os.WriteFile(compressed, zstdCompressGateFixture(t, []byte(compressedFixture)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{"sessions_dir": rollouts}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewCodexCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	runCatchUp := func(handler *levelCaptureHandler) {
		result := RunCollect(context.Background(), deps, usageDB,
			slog.New(handler), io.Discard, "codex",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true},
			false, false)
		if !result.Complete() {
			t.Fatalf("catch-up failed: %+v", result)
		}
	}

	runCatchUp(&levelCaptureHandler{})
	msgQuery := `SELECT id, session_id FROM messages ORDER BY id`
	first := dumpRows(t, usageDB, msgQuery)
	if !strings.Contains(first, "019f4fdb") || !strings.Contains(first, "019f4fdc") {
		t.Fatalf("首轮应采齐双形态两个会话:\n%s", first)
	}

	// 门记录 key 须含 .zst 实际路径（第二轮按同 key 命中）。
	logs, err := db.GetFileScanLogs(context.Background(), usageDB, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := logs[compressed]; !ok {
		t.Fatalf("压缩形态首轮应写门记录（key=实际 .zst 路径）: %+v", logs)
	}

	handler := &levelCaptureHandler{}
	runCatchUp(handler)
	if !handler.hasRecordAt(slog.LevelDebug, "scan gate skipped files") {
		t.Fatal("第二轮 catch-up 压缩文件应门命中跳过（帧完整性合同失败的信号）")
	}
	// skipped 计数须为 2：r1.jsonl 与 r2.jsonl.zst 都命中门。若压缩文件门失效
	//（trailingNewline 误检 zstd 帧尾、二轮全量重读），计数退化为 1——hasRecordAt
	// 单独存在会被 r1 的正常命中稀释。
	if n := handler.gateSkippedCount(); n != 2 {
		t.Fatalf("第二轮门命中跳过数 = %d, want 2（.zst 门未生效则只剩 r1 命中）", n)
	}
	if second := dumpRows(t, usageDB, msgQuery); second != first {
		t.Fatalf("门命中跳过后 DB 消息应不变:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// gateSkippedCount 提取 "scan gate skipped files" 日志的 skipped 数量 attr；
// 未记录时返回 -1（供与合法计数区分）。
func (h *levelCaptureHandler) gateSkippedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != slog.LevelDebug || r.Message != "scan gate skipped files" {
			continue
		}
		found := -1
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "skipped" {
				// slog 把 int 参数归一为 KindInt64，Any() 取出为 int64。
				switch n := a.Value.Any().(type) {
				case int:
					found = n
					return false
				case int64:
					found = int(n)
					return false
				}
			}
			return true
		})
		if found >= 0 {
			return found
		}
	}
	return -1
}
