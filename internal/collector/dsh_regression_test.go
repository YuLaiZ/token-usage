package collector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 只有失败 attempt、没有最终 message 的会话也必须计入。
func TestDSHOnlyFailedAttemptProducesMessage(t *testing.T) {
	content := dshHeaderJSONCwd(4, "session-attempt-only", "/tmp/a") +
		`{"type":"request/context","seq":0,"time":1,"data":{"provider":"p","model":"m"}}` + "\n" +
		`{"type":"assistant/attempt","seq":1,"time":2,"data":{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":` + dshUsage100 + `}}]}}` + "\n"
	home := writeDSHHome(t, "session-attempt-only", "/tmp/a", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || result.PartialErr != nil {
		t.Fatalf("collect: %v / %v", err, result.PartialErr)
	}
	if len(result.Messages) != 1 || len(result.Sessions) != 1 {
		t.Fatalf("attempt-only = %d messages / %d sessions", len(result.Messages), len(result.Sessions))
	}
}

// 明文尾行没有换行：保留前面的完整行，但不视为完整文件、不写缓存。
func TestDSHPlainMissingTrailingNewlineNotFullyParsed(t *testing.T) {
	content := strings.TrimSuffix(dshSessionWithMessage("session-tail", "/tmp/t"), "\n")
	home := writeDSHHome(t, "session-tail", "/tmp/t", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.FileStatuses) != 1 {
		t.Fatalf("statuses = %d", len(result.FileStatuses))
	}
	status := result.FileStatuses[0]
	if status.TrailingNewline || status.FullyParsed() {
		t.Fatalf("status = %+v, want incomplete tail", status)
	}
}

// 超长行被跳过时，文件不得视为完整、不得写缓存。
func TestDSHOversizedLineNotFullyParsed(t *testing.T) {
	pad := strings.Repeat("x", 33<<20)
	content := dshSessionWithMessage("session-oversized", "/tmp/o") +
		`{"type":"tool/result","seq":9,"time":3,"data":{"pad":"` + pad + `"}}` + "\n"
	home := writeDSHHome(t, "session-oversized", "/tmp/o", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	status := result.FileStatuses[0]
	if status.OversizedLines != 1 || status.FullyParsed() {
		t.Fatalf("status = %+v, want one oversized line and not fully parsed", status)
	}
}

// 同一 header session id 出现在两个候选文件时，本轮必须报冲突且只保留一个。
func TestDSHDuplicateSessionIDRejected(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("shared-id", "/tmp/a"), dshFixtureOpts{})
	writeDSHSession(t, home, "session-b", "/tmp/b", dshSessionWithMessage("shared-id", "/tmp/b"), dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	if result.PartialErr == nil || !strings.Contains(result.PartialErr.Error(), "duplicate session id") {
		t.Fatalf("PartialErr = %v, want duplicate id", result.PartialErr)
	}
}

// 未换行的非零尾记录不得入库。
func TestDSHPlainUnterminatedNonzeroTailExcluded(t *testing.T) {
	content := dshHeaderJSONCwd(4, "session-tail2", "/tmp/t2") +
		`{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}`
	home := writeDSHHome(t, "session-tail2", "/tmp/t2", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Messages) != 0 || len(result.Sessions) != 0 {
		t.Fatalf("unterminated tail leaked: %d messages / %d sessions", len(result.Messages), len(result.Sessions))
	}
}

// 未换行的零值尾记录不得产生撤销计划。
func TestDSHPlainUnterminatedZeroTailExcluded(t *testing.T) {
	content := dshHeaderJSONCwd(4, "session-tail3", "/tmp/t3") +
		`{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n" +
		`{"type":"assistant/message","seq":2,"time":2,"data":{"turn":1,"step":1,"usage":` + dshUsageZero + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}`
	home := writeDSHHome(t, "session-tail3", "/tmp/t3", content, dshFixtureOpts{})

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 100 {
		t.Fatalf("messages = %+v, want the complete 100 row", result.Messages)
	}
	if len(result.DSHObsolete) != 0 {
		t.Fatalf("unterminated zero tail produced obsolete: %+v", result.DSHObsolete)
	}
}

// 只有继承消息、没有切点后用量事件的 fork 不产会话。
func TestDSHSeededOnlyInheritedHasNoSession(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1, dshMessageJSON(1, 1, dshUsage100, "p", "m")),
		dshTestEvent(t, "session/end-seed", 2, 2, `{"inherited":true}`),
	)
	decoded.Cut = 2
	decoded.Header.IsSeeded = true
	result := buildDSHResult(decoded)
	if len(result.Sessions) != 0 || len(result.Messages) != 0 {
		t.Fatalf("inherited-only fork = %d sessions / %d messages", len(result.Sessions), len(result.Messages))
	}
}

// skippable frame 也计入物理帧上限。
func TestScanDSHZstdSkippableCountsTowardLimit(t *testing.T) {
	data := append(makeDSHSkippable(t, []byte("a")), makeDSHSkippable(t, []byte("b"))...)
	data = append(data, makeDSHSkippable(t, []byte("c"))...)
	scan, err := scanDSHZstdFrames(data, 1)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !scan.Truncated {
		t.Fatalf("skippable frames must count toward the frame limit")
	}
}

// 只有全零 attempt 的会话不产空 Session（first/last 无意义）。
func TestDSHZeroOnlyAttemptHasNoSession(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/attempt", 1, 1,
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsageZero+`}}]}`),
	)
	result := buildDSHResult(decoded)
	if len(result.Sessions) != 0 || len(result.Messages) != 0 || len(result.Obsolete) != 0 {
		t.Fatalf("zero-only attempt = %+v", result)
	}
}

// 只报 totalTokens 的适配器不能被当成零值。
func TestDSHTotalOnlyUsageProducesMessage(t *testing.T) {
	decoded := dshTestDecoded(
		dshTestEvent(t, "assistant/message", 1, 1000,
			dshMessageJSON(1, 1, `{"totalTokens":175}`, "p", "m")),
	)
	result := buildDSHResult(decoded)
	if len(result.Messages) != 1 || result.Messages[0].TotalTokens != 175 {
		t.Fatalf("total-only usage = %+v", result.Messages)
	}
}

// 空文件与首帧撕裂都不算错误（撕裂由 TruncatedTail 表达）。
func TestDSHEmptyOrTornFirstFrameNoError(t *testing.T) {
	emptyHome := writeDSHHome(t, "session-empty-file", "/tmp/e", "", dshFixtureOpts{})
	empty, err := newDSHTestCollector(t, emptyHome).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || empty.PartialErr != nil || len(empty.Sessions) != 0 {
		t.Fatalf("empty file: %v / %v / %d sessions", err, empty.PartialErr, len(empty.Sessions))
	}

	tornHome := newDSHHome(t)
	content := dshSessionWithMessage("session-torn-first", "/tmp/tf")
	writeDSHSession(t, tornHome, "session-torn-first", "/tmp/tf", content, dshFixtureOpts{Compression: "zstd"})
	// 截断整帧到只剩 magic：没有完整帧，但也不应报错。
	path := filepath.Join(tornHome, "sessions", dshProjectKey("/tmp/tf"), "session-torn-first", "session.v4.jsonl.zstd")
	data := readDSHFile(t, tornHome, "session-torn-first", "/tmp/tf", "session.v4.jsonl.zstd")
	if err := os.WriteFile(path, data[:4], 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	torn, err := newDSHTestCollector(t, tornHome).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || torn.PartialErr != nil {
		t.Fatalf("torn first frame: %v / %v", err, torn.PartialErr)
	}
	if len(torn.Sessions) != 0 {
		t.Fatalf("torn first frame produced sessions: %+v", torn.Sessions)
	}
}

// F13②③：只有旧代/未来代时跳过且不设 PartialErr。
func TestDSHCollectLegacyAndFutureVersionsSkipped(t *testing.T) {
	for _, version := range []int{3, 5} {
		home := newDSHHome(t)
		content := dshHeaderJSONCwd(version, "session-v", "/tmp/v") +
			`{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n"
		writeDSHSession(t, home, "session-v", "/tmp/v", content, dshFixtureOpts{Version: version})

		result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
		if err != nil {
			t.Fatalf("v%d collect: %v", version, err)
		}
		if len(result.Sessions) != 0 || len(result.Messages) != 0 {
			t.Fatalf("v%d produced data: %d/%d", version, len(result.Sessions), len(result.Messages))
		}
		if result.PartialErr != nil {
			t.Fatalf("v%d must not set PartialErr: %v", version, result.PartialErr)
		}
	}
}

// F08 变体：不同截断位置都只丢弃末尾帧、保留完整帧。
func TestDSHFileStatusTornVariants(t *testing.T) {
	for _, truncate := range []int{1, 8, 20} {
		home := newDSHHome(t)
		content := dshHeaderJSONCwd(4, "session-t", "/tmp/t") +
			`{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n" +
			`{"type":"assistant/message","seq":2,"time":2,"data":{"turn":1,"step":2,"usage":` + dshUsage250 + `}}` + "\n"
		writeDSHSession(t, home, "session-t", "/tmp/t", content, dshFixtureOpts{
			Compression: "zstd",
			Frames:      2,
			Truncate:    truncate,
		})
		result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
		if err != nil {
			t.Fatalf("truncate %d: %v", truncate, err)
		}
		if len(result.FileStatuses) != 1 || !result.FileStatuses[0].TruncatedTail {
			t.Fatalf("truncate %d status = %+v", truncate, result.FileStatuses)
		}
	}
}

// F26：attempt 全 0 + turn/end error 不产消息、不产会话。
func TestFixtureF26ExhaustedAttemptsWithTurnError(t *testing.T) {
	var events []dshEvent
	for i := 0; i < 6; i++ {
		events = append(events, dshTestEvent(t, "assistant/attempt", int64(i), int64(i+1),
			`{"turn":1,"step":1,"stream":[{"chunk":{"type":"usage","usage":`+dshUsageZero+`}}]}`))
	}
	events = append(events, dshTestEvent(t, "turn/end", 7, 8, `{"turn":1,"status":"error","reason":"TRANSPORT"}`))
	result := buildDSHResult(dshTestDecoded(events...))
	if len(result.Messages) != 0 || len(result.Sessions) != 0 || len(result.Obsolete) != 0 {
		t.Fatalf("F26 = %+v", result)
	}
}

// 明文首行尚未换行：不报错、不产会话/消息（不完整由 TrailingNewline 表达）。
func TestDSHPlainUnterminatedFirstHeaderNoError(t *testing.T) {
	cases := []string{
		`{"type":"session","version":4,"id":"s","createdAt":1,"cwd":"/tmp/x","isSeeded":false}`, // 完整 JSON 只缺换行
		`{"type":"session","version":4,"id":"s","createdAt":1,"cwd":"/tmp/x"`,                   // 截断 JSON
	}
	for _, content := range cases {
		home := writeDSHHome(t, "session-first", "/tmp/x", content, dshFixtureOpts{})
		result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
		if err != nil || result.PartialErr != nil {
			t.Fatalf("unterminated first header: %v / %v", err, result.PartialErr)
		}
		if len(result.Sessions) != 0 || len(result.Messages) != 0 {
			t.Fatalf("unterminated first header produced data")
		}
	}
}

// F09：帧结构完整、checksum 损坏时仍要拒绝坏会话并保留健康会话。
func TestDSHChecksumCorruptionRejected(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-bad", "/tmp/bad", dshSessionWithMessage("session-bad", "/tmp/bad"), dshFixtureOpts{Compression: "zstd"})
	writeDSHSession(t, home, "session-good", "/tmp/good", dshSessionWithMessage("session-good", "/tmp/good"), dshFixtureOpts{})

	badPath := filepath.Join(home, "sessions", dshProjectKey("/tmp/bad"), "session-bad", "session.v4.jsonl.zstd")
	data := readDSHFile(t, home, "session-bad", "/tmp/bad", "session.v4.jsonl.zstd")
	data[len(data)-1] ^= 0xFF // 只破坏末尾 checksum，帧结构仍完整
	if err := os.WriteFile(badPath, data, 0o600); err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatalf("checksum corruption must set PartialErr")
	}
	if len(result.Messages) != 1 || result.Messages[0].SessionID != "session-good" {
		t.Fatalf("healthy session missing: %+v", result.Messages)
	}
}

// F30：多块大帧末尾截断 1 字节：坏会话不产消息/会话/撤销，健康会话不受影响。
func TestDSHMultiBlockTornFrameNoState(t *testing.T) {
	home := newDSHHome(t)
	big := dshHeaderJSONCwd(4, "session-big", "/tmp/big")
	big += `{"type":"assistant/message","seq":1,"time":1,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n"
	big += `{"type":"tool/result","seq":2,"time":2,"data":{"pad":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n"
	big += `{"type":"session/title","seq":3,"time":3,"data":{"title":"big"}}` + "\n"
	writeDSHSession(t, home, "session-big", "/tmp/big", big, dshFixtureOpts{Compression: "zstd"})
	writeDSHSession(t, home, "session-good", "/tmp/good", dshSessionWithMessage("session-good", "/tmp/good"), dshFixtureOpts{})

	bigPath := filepath.Join(home, "sessions", dshProjectKey("/tmp/big"), "session-big", "session.v4.jsonl.zstd")
	data := readDSHFile(t, home, "session-big", "/tmp/big", "session.v4.jsonl.zstd")
	if err := os.WriteFile(bigPath, data[:len(data)-1], 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].SessionID != "session-good" {
		t.Fatalf("torn big frame leaked state: %+v", result.Messages)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].ID != "session-good" {
		t.Fatalf("torn big frame leaked session/title: %+v", result.Sessions)
	}
	if len(result.DSHObsolete) != 0 {
		t.Fatalf("torn big frame leaked obsolete: %+v", result.DSHObsolete)
	}
}

// F35：固定源文件，依次请求 A、B、全量，并与全新实例逐字段比较。
func TestF35RequestScopeCombination(t *testing.T) {
	home := newDSHHome(t)
	day1 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local).UnixMilli()
	day2 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local).UnixMilli()
	content := dshHeaderJSONCwd(4, "session-scope", "/tmp/s") +
		`{"type":"assistant/message","seq":1,"time":` + itoa64(day1) + `,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `}}` + "\n" +
		`{"type":"assistant/message","seq":2,"time":` + itoa64(day1) + `,"data":{"turn":1,"step":1,"usage":` + dshUsageZero + `}}` + "\n" +
		`{"type":"assistant/message","seq":3,"time":` + itoa64(day2) + `,"data":{"turn":1,"step":2,"usage":` + dshUsage250 + `}}` + "\n"
	writeDSHSession(t, home, "session-scope", "/tmp/s", content, dshFixtureOpts{})

	c := newDSHTestCollector(t, home)
	d1 := tsMsToDate(day1)
	d2 := tsMsToDate(day2)

	a, _ := c.Collect(context.Background(), CollectRequest{Dates: []string{d1}}, nil)
	b, _ := c.Collect(context.Background(), CollectRequest{Dates: []string{d2}}, nil)
	all, _ := c.Collect(context.Background(), CollectRequest{}, nil)
	fresh, _ := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)

	if len(a.Messages) != 0 || len(a.DSHObsolete) != 1 {
		t.Fatalf("scope A = %d messages / %d obsolete, want 0/1", len(a.Messages), len(a.DSHObsolete))
	}
	if len(b.Messages) != 1 || len(b.DSHObsolete) != 0 {
		t.Fatalf("scope B = %d messages / %d obsolete, want 1/0", len(b.Messages), len(b.DSHObsolete))
	}
	if len(all.Messages) != 1 || len(all.DSHObsolete) != 1 {
		t.Fatalf("full = %d messages / %d obsolete, want 1/1", len(all.Messages), len(all.DSHObsolete))
	}
	if len(fresh.Messages) != len(all.Messages) || len(fresh.DSHObsolete) != len(all.DSHObsolete) {
		t.Fatalf("fresh instance differs: %+v vs %+v", fresh, all)
	}
	if fresh.Messages[0] != all.Messages[0] || fresh.DSHObsolete[0] != all.DSHObsolete[0] {
		t.Fatalf("fresh instance field mismatch")
	}
}

// 完整坏行 + 未换行尾片段：仍需拒绝，不能当作「首行尚未写完」。
func TestDSHPlainBadCompleteLineThenUnterminatedTail(t *testing.T) {
	content := "{not-json}\n" + `{"type":"session","version":4`
	home := writeDSHHome(t, "session-bad-tail", "/tmp/bt", content, dshFixtureOpts{})
	result, err := newDSHTestCollector(t, home).Collect(context.Background(), CollectRequest{}, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.PartialErr == nil {
		t.Fatalf("bad complete line must not be exempted as unfinished first line")
	}
}
