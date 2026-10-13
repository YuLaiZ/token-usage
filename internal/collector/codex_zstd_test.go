package collector

// Codex 压缩 rollout（.jsonl.zst）适配测试：发现层双形态识别与去重、解析
// 等价、行级语义与帧完整性合同、state threads 兄弟映射、损坏负向、压缩→
// 解压往返幂等。daemon startup 全扫与跳过门的 engine 级集成在
// internal/engine/scan_gate_zstd_test.go。

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/klauspost/compress/zstd"
)

// zstdCompressFixture 用单帧 zstd 压缩 fixture 内容（与 Codex 本地历史压缩产物
// 同形态：压缩后删除原 .jsonl、保留原 mtime 由文件系统语义承担，本 helper 只
// 产出字节形态）。
func zstdCompressFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

// writeCompressedRollout 把 rollout 内容压缩写到 path（调用方给 .jsonl.zst 路径）。
func writeCompressedRollout(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, zstdCompressFixture(t, []byte(content)), 0644); err != nil {
		t.Fatalf("write compressed rollout: %v", err)
	}
}

// rolloutFixture 是本文件的基准 rollout：两条消息（msg-c1/msg-c2），尾行带 \n。
const rolloutFixture = `{"timestamp":"2026-07-11T14:26:35Z","type":"session_meta","payload":{"id":"019f4fdb","originator":"Codex CLI","source":"cli","cwd":"/tmp/project"}}
{"timestamp":"2026-07-11T14:27:00Z","type":"turn_context","payload":{"model":"gpt-5"}}
{"timestamp":"2026-07-11T14:27:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-c1"}}
{"timestamp":"2026-07-11T14:27:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":30,"total_tokens":180}}}}
{"timestamp":"2026-07-11T14:28:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-c2"}}
{"timestamp":"2026-07-11T14:28:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":90,"cached_input_tokens":10,"output_tokens":15,"total_tokens":105}}}}
`

// 发现层：混合目录只收 *.jsonl 与 *.jsonl.zst 两种合法形态；rollout-x.zst
// （不带 .jsonl 前缀）、.json.gz、无关后缀、子目录都不收；archived_sessions
// 同 root 同则。
func TestFindCodexRollout_MixedForms(t *testing.T) {
	sessions := t.TempDir()
	archived := filepath.Join(filepath.Dir(sessions), "archived_sessions")
	for _, sub := range []string{filepath.Join(sessions, "2026", "07"), archived} {
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
	}
	plain := filepath.Join(sessions, "rollout-a.jsonl")
	compressed := filepath.Join(sessions, "rollout-b.jsonl.zst")
	nested := filepath.Join(sessions, "2026", "07", "rollout-c.jsonl")
	archivedZst := filepath.Join(archived, "rollout-d.jsonl.zst")
	for _, p := range []string{plain, nested} {
		if err := os.WriteFile(p, []byte(rolloutFixture), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeCompressedRollout(t, compressed, rolloutFixture)
	writeCompressedRollout(t, archivedZst, rolloutFixture)
	for _, name := range []string{"rollout-e.zst", "rollout-f.json.gz", "notes.txt", "rollout-g.jsonl.zst.tmp"} {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := findExistingCodexJSONLContext(context.Background(), sessions)
	if err != nil {
		t.Fatalf("findExistingCodexJSONLContext: %v", err)
	}
	want := []string{plain, compressed, nested, archivedZst}
	sort.Strings(want)
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("发现结果不符:\ngot  %v\nwant %v", files, want)
	}
}

// 双形态去重：同逻辑名（完整路径剥 .zst）双形态并存只收 .jsonl；仅 .zst
// 照收；跨 root 同 basename 不误去重（逻辑名取完整路径）；损坏 .zst 与完好
// .jsonl 兄弟并存只收 .jsonl 且不产生 partialErr（去重先于解析）。
func TestFindCodexRollout_DedupeSiblings(t *testing.T) {
	sessions := t.TempDir()
	archived := filepath.Join(filepath.Dir(sessions), "archived_sessions")
	if err := os.MkdirAll(archived, 0755); err != nil {
		t.Fatal(err)
	}
	// 双形态并存：只收 .jsonl。
	dup := filepath.Join(sessions, "rollout-dup.jsonl")
	if err := os.WriteFile(dup, []byte(rolloutFixture), 0644); err != nil {
		t.Fatal(err)
	}
	writeCompressedRollout(t, dup+".zst", rolloutFixture)
	// 仅压缩形态：照收。
	solo := filepath.Join(sessions, "rollout-solo.jsonl.zst")
	writeCompressedRollout(t, solo, rolloutFixture)
	// 跨 root 同 basename：完整路径逻辑名不同，两个都收。
	crossPlain := filepath.Join(sessions, "rollout-cross.jsonl")
	crossZst := filepath.Join(archived, "rollout-cross.jsonl.zst")
	if err := os.WriteFile(crossPlain, []byte(rolloutFixture), 0644); err != nil {
		t.Fatal(err)
	}
	writeCompressedRollout(t, crossZst, rolloutFixture)

	files, err := findExistingCodexJSONLContext(context.Background(), sessions)
	if err != nil {
		t.Fatalf("findExistingCodexJSONLContext: %v", err)
	}
	want := []string{crossPlain, dup, crossZst, solo}
	sort.Strings(want)
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("去重结果不符:\ngot  %v\nwant %v", files, want)
	}
}

// testCodexConfig 构造指向临时目录的 Codex client 配置。
func testCodexConfig(stateDir, sessionsDir string) *config.Config {
	return &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
}

// 去重先于解析：损坏 .zst 与完好 .jsonl 兄弟并存时，去重先于解析——.zst 不被触达，
// 采集无 partialErr。
func TestCollectExisting_CorruptedSiblingDedupedBeforeParse(t *testing.T) {
	sessions := t.TempDir()
	plain := filepath.Join(sessions, "rollout-x.jsonl")
	if err := os.WriteFile(plain, []byte(rolloutFixture), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain+".zst", []byte("not a zstd stream"), 0644); err != nil {
		t.Fatal(err)
	}

	c := &CodexCollector{sessionsDir: sessions}
	result, err := c.collectExistingJSONL(context.Background(), nil, slog.Default())
	if err != nil {
		t.Fatalf("collectExistingJSONL: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("损坏兄弟被去重后不得产生 partialErr: %v", result.PartialErr)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2（完好 .jsonl 正常采集）", len(result.Messages))
	}
}

// 解析等价：同一 rollout 内容两形态（原始与单帧 zstd 压缩）产出完全一致的
// Messages 与 Sessions。
func TestParseCodexRollout_CompressedEquivalent(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "rollout.jsonl")
	compressed := filepath.Join(dir, "rollout.jsonl.zst")
	if err := os.WriteFile(plain, []byte(rolloutFixture), 0644); err != nil {
		t.Fatal(err)
	}
	writeCompressedRollout(t, compressed, rolloutFixture)

	plainResult, plainStatus, err := parseCodexRollout(plain, codexThread{}, nil)
	if err != nil {
		t.Fatalf("plain parse: %v", err)
	}
	zstResult, zstStatus, err := parseCodexRollout(compressed, codexThread{}, nil)
	if err != nil {
		t.Fatalf("compressed parse: %v", err)
	}
	if !reflect.DeepEqual(plainResult.Messages, zstResult.Messages) {
		t.Fatalf("两形态 Messages 不一致:\nplain %v\nzst   %v", plainResult.Messages, zstResult.Messages)
	}
	if !reflect.DeepEqual(plainResult.Sessions, zstResult.Sessions) {
		t.Fatalf("两形态 Sessions 不一致:\nplain %v\nzst   %v", plainResult.Sessions, zstResult.Sessions)
	}
	if plainStatus.BadLines != zstStatus.BadLines || plainStatus.TrailingNewline != zstStatus.TrailingNewline {
		t.Fatalf("两形态行级状态不一致: plain(%d,%v) zst(%d,%v)",
			plainStatus.BadLines, plainStatus.TrailingNewline, zstStatus.BadLines, zstStatus.TrailingNewline)
	}
}

// 行级语义继承：解压后超过 32MiB 上限的行按坏行丢弃并继续（与 .jsonl 同
// 语义——上限约束的是解压后的单行 JSON 解析边界，与存储压缩无关）。
func TestParseCodexRollout_CompressedOversizedLineSkipped(t *testing.T) {
	dir := t.TempDir()
	before := `{"timestamp":"2026-09-21T02:30:00Z","type":"session_meta","payload":{"id":"oversize-zst","originator":"codex-tui","source":"cli"}}
{"timestamp":"2026-09-21T02:30:30Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-o1"}}
{"timestamp":"2026-09-21T02:31:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":2,"total_tokens":12}}}}`
	oversized := `{"timestamp":"2026-09-21T02:36:12.843Z","type":"compacted","payload":{"replacement_history":"` +
		strings.Repeat("x", maxJSONLLineSize+1) + `"}}`
	after := `{"timestamp":"2026-09-21T02:37:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-o2"}}
{"timestamp":"2026-09-21T02:37:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"total_tokens":120}}}}`
	writeCompressedRollout(t, filepath.Join(dir, "oversized.jsonl.zst"), before+"\n"+oversized+"\n"+after+"\n")

	result, status, err := parseCodexRollout(filepath.Join(dir, "oversized.jsonl.zst"), codexThread{}, nil)
	if err != nil {
		t.Fatalf("parseCodexRollout failed: %v（解压后超限行不得构成文件级失败）", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2（超限行前后各一条）", len(result.Messages))
	}
	if status.BadLines != 1 || status.FirstBadLine != 4 {
		t.Fatalf("BadLines = %d FirstBadLine = %d, want 1 和 4（第 4 行为解压后超限行）",
			status.BadLines, status.FirstBadLine)
	}
	if status.FullyParsed() {
		t.Error("FullyParsed = true, want false（坏行存在时跳过门不得推进）")
	}
}

// 帧完整性合同：压缩源尾行无 \n 的 .zst 必须判 TrailingNewline=true 且
// FullyParsed=true——「尾行可能仍在写」的语义只对 Codex 追加写入的 .jsonl 成立，
// 完整 zstd 帧不存在半写形态。本用例区分错误实现「检查解压流尾字节是否 \n」：
// 该实现在此场景恒 false，门永不推进。
func TestParseCodexRollout_CompressedNoTrailingNewlineStillFullyParsed(t *testing.T) {
	dir := t.TempDir()
	noTail := strings.TrimSuffix(rolloutFixture, "\n")
	zstPath := filepath.Join(dir, "no-tail.jsonl.zst")
	writeCompressedRollout(t, zstPath, noTail)

	zstResult, zstStatus, err := parseCodexRollout(zstPath, codexThread{}, nil)
	if err != nil {
		t.Fatalf("compressed parse: %v", err)
	}
	if len(zstResult.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2（无尾换行的尾行照常产出）", len(zstResult.Messages))
	}
	if !zstStatus.TrailingNewline || !zstStatus.FullyParsed() {
		t.Fatalf("压缩形态帧完整解码即 TrailingNewline=true/FullyParsed=true, got (%v,%v)",
			zstStatus.TrailingNewline, zstStatus.FullyParsed())
	}

	// 对照锚定：同内容 .jsonl 形态维持现状检测（TrailingNewline=false，不推进门）。
	plainPath := filepath.Join(dir, "no-tail.jsonl")
	if err := os.WriteFile(plainPath, []byte(noTail), 0644); err != nil {
		t.Fatal(err)
	}
	_, plainStatus, err := parseCodexRollout(plainPath, codexThread{}, nil)
	if err != nil {
		t.Fatalf("plain parse: %v", err)
	}
	if plainStatus.TrailingNewline {
		t.Fatal(".jsonl 形态尾行无 \\n 必须 TrailingNewline=false（现状合同不得被改动）")
	}
}

// state threads 兄弟映射：rollout_path 指向不存在的 .jsonl、磁盘有 .zst 兄弟
// 时经兄弟解析成功——无 partialErr、Incremental 游标正常推进；FileScanStatus.Path
// 记录实际读取的 .zst 路径。
func TestCodexCollector_SiblingResolutionForward(t *testing.T) {
	sessions := t.TempDir()
	zstPath := filepath.Join(sessions, "rollout-2026-07-11.jsonl.zst")
	writeCompressedRollout(t, zstPath, rolloutFixture)
	stateDir := filepath.Join(filepath.Dir(sessions), "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	createStateDBWithThreads(t, stateDir, []codexThread{{
		// rollout_path 指向已不存在的逻辑 .jsonl（Codex 压缩后删除原文件）。
		ID: "019f4fdb", RolloutPath: filepath.Join(sessions, "rollout-2026-07-11.jsonl"),
		Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 100,
	}})

	cfg := testCodexConfig(stateDir, sessions)
	c := NewCodexCollector(cfg)

	result, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("兄弟映射成功时不得有 partialErr: %v", result.PartialErr)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2（压缩兄弟正常采集）", len(result.Messages))
	}
	cur, ok := result.NextCursors[SyncSourceCodexState]
	if !ok || cur.Value != 100 || cur.ID != "019f4fdb" {
		t.Fatalf("游标未按成功采集推进: %+v (ok=%v)", cur, ok)
	}
	if len(result.FileStatuses) != 1 || result.FileStatuses[0].Path != zstPath {
		t.Fatalf("FileScanStatus.Path 应记录实际读取的 .zst 路径: %+v", result.FileStatuses)
	}
	if !result.FileStatuses[0].FullyParsed() {
		t.Fatalf("压缩兄弟 FullyParsed 应为 true: %+v", result.FileStatuses[0])
	}
}

// 防御形态：rollout_path 直接为存在的 .zst（将来 Codex 改存压缩路径）时读取
// 层正常解析；rollout_path 为 .zst 且缺失、磁盘有 .jsonl 兄弟时反向映射成功。
func TestCodexCollector_SiblingResolutionReverse(t *testing.T) {
	sessions := t.TempDir()

	t.Run("rollout_path 直接为存在的 .zst", func(t *testing.T) {
		stateDir := t.TempDir()
		zstPath := filepath.Join(sessions, "rollout-direct.jsonl.zst")
		writeCompressedRollout(t, zstPath, rolloutFixture)
		createStateDBWithThreads(t, stateDir, []codexThread{{
			ID: "direct", RolloutPath: zstPath, Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 100,
		}})
		result, err := NewCodexCollector(testCodexConfig(stateDir, sessions)).Collect(
			context.Background(), CollectRequest{}, slog.Default())
		if err != nil {
			t.Fatalf("Collect failed: %v", err)
		}
		if result.PartialErr != nil || len(result.Messages) != 2 {
			t.Fatalf("direct .zst rollout 解析异常: partialErr=%v messages=%d", result.PartialErr, len(result.Messages))
		}
	})

	t.Run("rollout_path 为缺失的 .zst、磁盘有 .jsonl 兄弟（解压回写瞬间）", func(t *testing.T) {
		stateDir := t.TempDir()
		plain := filepath.Join(sessions, "rollout-back.jsonl")
		if err := os.WriteFile(plain, []byte(rolloutFixture), 0644); err != nil {
			t.Fatal(err)
		}
		createStateDBWithThreads(t, stateDir, []codexThread{{
			ID: "back", RolloutPath: plain + ".zst", Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 100,
		}})
		result, err := NewCodexCollector(testCodexConfig(stateDir, sessions)).Collect(
			context.Background(), CollectRequest{}, slog.Default())
		if err != nil {
			t.Fatalf("Collect failed: %v", err)
		}
		if result.PartialErr != nil || len(result.Messages) != 2 {
			t.Fatalf("反向映射解析异常: partialErr=%v messages=%d", result.PartialErr, len(result.Messages))
		}
		if len(result.FileStatuses) != 1 || result.FileStatuses[0].Path != plain {
			t.Fatalf("FileScanStatus.Path 应记录实际读取的 .jsonl 兄弟: %+v", result.FileStatuses)
		}
	})
}

// 负向：两形态皆缺（真实文件丢失）维持现状 warn + partialErr；Incremental 下
// partialErr 抑制游标推进（不得静默、不得死循环豁免）。
func TestCodexCollector_SiblingBothMissingPartialErr(t *testing.T) {
	sessions := t.TempDir()
	stateDir := filepath.Join(filepath.Dir(sessions), "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	createStateDBWithThreads(t, stateDir, []codexThread{{
		ID: "gone", RolloutPath: filepath.Join(sessions, "rollout-gone.jsonl"),
		Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 100,
	}})

	result, err := NewCodexCollector(testCodexConfig(stateDir, sessions)).Collect(
		context.Background(), CollectRequest{Incremental: true}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v（部分失败不得终止）", err)
	}
	if result.PartialErr == nil {
		t.Fatal("两形态皆缺必须报 partialErr（真实文件丢失不得静默）")
	}
	if result.NextCursors != nil {
		t.Fatalf("partialErr 非空时游标不得推进: %+v", result.NextCursors)
	}
}

// 负向：兄弟形态解析到目录等非普通文件时按现状 IsRegular 分支跳过。
func TestCodexCollector_SiblingNotRegularFile(t *testing.T) {
	sessions := t.TempDir()
	dirSibling := filepath.Join(sessions, "rollout-dir.jsonl.zst")
	if err := os.MkdirAll(dirSibling, 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.Dir(sessions), "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	createStateDBWithThreads(t, stateDir, []codexThread{{
		ID: "dir", RolloutPath: filepath.Join(sessions, "rollout-dir.jsonl"),
		Source: "cli", CreatedAtMS: 1, UpdatedAtMS: 100,
	}})

	result, err := NewCodexCollector(testCodexConfig(stateDir, sessions)).Collect(
		context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if result.PartialErr == nil || len(result.Messages) != 0 {
		t.Fatalf("目录兄弟须按非普通文件跳过: partialErr=%v messages=%d", result.PartialErr, len(result.Messages))
	}
	// 锚定 IsRegular 分支本身：错误文案必须是「不是普通文件」，而非
	// os.Open(目录) 的 EISDIR 读失败或兄弟探测缺失的 ENOENT。
	if !strings.Contains(result.PartialErr.Error(), "不是普通文件") {
		t.Fatalf("应走 IsRegular 分支（不是普通文件）, got: %v", result.PartialErr)
	}
}

// 损坏负向：.zst 内容非 zstd 流（magic 错误）→ 文件级失败计入 partialErr，
// 不中断同轮其他文件。
func TestCollectExisting_CorruptedZSTFileLevelFailure(t *testing.T) {
	sessions := t.TempDir()
	good := filepath.Join(sessions, "rollout-good.jsonl")
	if err := os.WriteFile(good, []byte(rolloutFixture), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "rollout-bad.jsonl.zst"),
		[]byte("definitely not a zstd stream"), 0644); err != nil {
		t.Fatal(err)
	}

	c := &CodexCollector{sessionsDir: sessions}
	result, err := c.collectExistingJSONL(context.Background(), nil, slog.Default())
	if err != nil {
		t.Fatalf("collectExistingJSONL: %v（单文件损坏不得终止）", err)
	}
	if result.PartialErr == nil {
		t.Fatal("损坏 .zst 必须计入 partialErr")
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2（同轮好文件不受影响）", len(result.Messages))
	}
	var badStatus *FileScanStatus
	for i := range result.FileStatuses {
		if result.FileStatuses[i].Err != nil {
			badStatus = &result.FileStatuses[i]
		}
	}
	if badStatus == nil {
		t.Fatalf("损坏文件须有 Err 状态: %+v", result.FileStatuses)
	}
	if badStatus.FullyParsed() {
		t.Error("损坏文件不得 FullyParsed")
	}
}

// 压缩→解压往返：采 .zst 后模拟 Codex 继续对话——解压回 .jsonl 并追加新行
// （.zst 保留的双形态并存形态，最保守场景）——再采不重不漏。
func TestCodexCollector_CompressDecompressRoundtrip(t *testing.T) {
	sessions := t.TempDir()
	zstPath := filepath.Join(sessions, "rollout-r.jsonl.zst")
	writeCompressedRollout(t, zstPath, rolloutFixture)
	plainPath := filepath.Join(sessions, "rollout-r.jsonl")

	c := &CodexCollector{sessionsDir: sessions}
	first, err := c.collectExistingJSONL(context.Background(), nil, slog.Default())
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if len(first.Messages) != 2 || first.PartialErr != nil {
		t.Fatalf("首轮采集异常: messages=%d partialErr=%v", len(first.Messages), first.PartialErr)
	}

	// 解压回 .jsonl（内容 = 原 fixture）并追加一条新消息；.zst 保留。
	appended := `{"timestamp":"2026-07-11T14:29:10Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-c3"}}
{"timestamp":"2026-07-11T14:29:20Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":8,"total_tokens":58}}}}
`
	if err := os.WriteFile(plainPath, []byte(rolloutFixture+appended), 0644); err != nil {
		t.Fatal(err)
	}

	second, err := c.collectExistingJSONL(context.Background(), nil, slog.Default())
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if second.PartialErr != nil {
		t.Fatalf("双形态并存轮不得有 partialErr: %v", second.PartialErr)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3（.jsonl 兄弟被采集、含新增消息）", len(second.Messages))
	}
	seen := map[string]int{}
	for _, m := range second.Messages {
		seen[m.ID]++
	}
	for _, id := range []string{"msg-c1#0", "msg-c2#0", "msg-c3#0"} {
		if seen[id] != 1 {
			t.Fatalf("消息 %s 出现 %d 次, want 1（不重不漏）: %v", id, seen[id], seen)
		}
	}
	// 第二轮只解析 .jsonl（去重生效）：FileStatuses 不含 .zst 路径。
	for _, fs := range second.FileStatuses {
		if strings.HasSuffix(fs.Path, ".zst") {
			t.Fatalf("双形态并存时 .zst 不得被解析: %+v", fs)
		}
	}
}
