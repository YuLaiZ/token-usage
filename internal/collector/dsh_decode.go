package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// dshHeader 是会话日志首行（type=session）的字段子集。
type dshHeader struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd"`
	ParentSession   string `json:"parentSession"`
	Origin          string `json:"origin"`
	IsSeeded        bool   `json:"isSeeded"`
	DelegationDepth int    `json:"delegationDepth"`
	AgentPreset     string `json:"agentPreset"`
}

// dshEvent 是一条会话事件；Data 延迟到具体事件类型再解析。
type dshEvent struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// Raw 保留整行，供 header 等「没有 data 字段」的事件使用。
	Raw json.RawMessage `json:"-"`
}

// dshDecoded 是一份会话日志的完整解码结果（单会话暂存）。
type dshDecoded struct {
	Header          dshHeader
	Events          []dshEvent
	BadLines        int
	Oversized       int
	Truncated       bool
	TrailingNewline bool
	Cut             int64
	AllInherited    bool
	HasUsageEvents  bool
}

// decodeDSHSession 读取并解码一份会话日志。
// zstd 先扫描帧边界，只解码完整帧；任一帧失败则整会话失败（不返回部分结果）。
func decodeDSHSession(ctx context.Context, path string, compression dshCompression) (*dshDecoded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var decoded dshDecoded

	if compression == dshCompressionZstd {
		scan, err := scanDSHZstdFrames(data, 0)
		if err != nil {
			return nil, err
		}
		if scan.Truncated {
			return nil, fmt.Errorf("session log has more than %d zstd frames", dshZstdMaxFrames)
		}
		decoded.Truncated = scan.TornStart >= 0
		decoded.TrailingNewline = !decoded.Truncated
		for _, frame := range scan.Frames {
			if err := decodeDSHFrame(ctx, data[frame.Start:frame.End], dshCompressionZstd, &decoded); err != nil {
				return nil, err
			}
		}
	} else {
		payload := data
		trailing := len(data) == 0 || data[len(data)-1] == '\n'
		if !trailing {
			// 最后一个未换行片段是半写尾记录，不能进入归约；保留此前的完整行。
			if idx := bytes.LastIndexByte(payload, '\n'); idx >= 0 {
				payload = payload[:idx+1]
			} else {
				payload = nil
			}
		}
		if err := decodeDSHFrame(ctx, payload, dshCompressionNone, &decoded); err != nil {
			return nil, err
		}
		decoded.TrailingNewline = trailing
	}

	if len(decoded.Events) == 0 {
		// 空文件、压缩首帧未写完（撕裂）、明文首行尚未换行都不是错误：
		// 由 TruncatedTail/TrailingNewline 表达不完整，交给上层决定是否缓存。
		// 完整坏行或缺失 header 仍走下面的拒绝路径。
		incompletePlain := compression == dshCompressionNone && !decoded.TrailingNewline &&
			!bytes.Contains(data, []byte("\n"))
		if decoded.Truncated || len(data) == 0 || incompletePlain {
			return &decoded, nil
		}
		return nil, errors.New("empty session log")
	}
	header, err := parseDSHHeader(decoded.Events[0])
	if err != nil {
		return nil, err
	}
	decoded.Header = header
	decoded.Cut, decoded.AllInherited = computeDSHCut(decoded.Events, header.IsSeeded)
	decoded.HasUsageEvents = countDSHUsageEvents(decoded.Events) > 0
	return &decoded, nil
}

// decodeDSHFrame 解码一个完整帧到临时事件列表，成功后合并到 decoded。
// 帧内坏行只计数；预算超限或 IO 错误使整帧失败。
func decodeDSHFrame(ctx context.Context, frame []byte, compression dshCompression, out *dshDecoded) error {
	var reader io.Reader = bytes.NewReader(frame)
	if compression == dshCompressionZstd {
		zr, err := zstd.NewReader(bytes.NewReader(frame), zstd.WithDecoderMaxMemory(dshMaxZstdWindowBytes))
		if err != nil {
			return fmt.Errorf("zstd reader: %w", err)
		}
		defer zr.Close()
		reader = &frameBudgetReader{r: zr, limit: dshMaxFrameDecodedBytes}
	}

	iter := newJSONLLineIter(ctx, reader, maxJSONLLineSize)

	var events []dshEvent
	badLines := 0
	oversized := 0
	for iter.Next() {
		if iter.Oversized() {
			oversized++
			continue
		}
		line := iter.Line()
		var event dshEvent
		if err := json.Unmarshal(line, &event); err != nil {
			badLines++
			continue
		}
		event.Raw = append(json.RawMessage(nil), line...)
		events = append(events, event)
	}
	if err := iter.Err(); err != nil {
		if errors.Is(err, errDSHFrameBudget) {
			return err
		}
		return fmt.Errorf("decode frame: %w", err)
	}

	out.Events = append(out.Events, events...)
	out.BadLines += badLines
	out.Oversized += oversized
	return nil
}

// parseDSHHeader 解析首行 header，并做最低限度的形态校验。
func parseDSHHeader(event dshEvent) (dshHeader, error) {
	var header dshHeader
	if event.Type != "session" {
		return header, fmt.Errorf("first record is not a session header (type=%q)", event.Type)
	}
	if err := json.Unmarshal(event.Raw, &header); err != nil {
		return header, fmt.Errorf("decode session header: %w", err)
	}
	if header.ID == "" {
		return header, errors.New("session header has empty id")
	}
	return header, nil
}

// computeDSHCut 计算继承切点：
//   - 非 seeded → (-1, false)；
//   - seeded 且有 inherited end-seed → (最后一条 seq, false)；
//   - seeded 但无 marker → (-1, true)，表示整份继承。
func computeDSHCut(events []dshEvent, isSeeded bool) (int64, bool) {
	if !isSeeded {
		return -1, false
	}
	cut := int64(-1)
	for _, event := range events {
		if event.Type != "session/end-seed" {
			continue
		}
		var data struct {
			Inherited bool `json:"inherited"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			continue
		}
		if data.Inherited && event.Seq > cut {
			cut = event.Seq
		}
	}
	if cut < 0 {
		return -1, true
	}
	return cut, false
}

// countDSHUsageEvents 统计可能产出用量的槽：成功 message 与失败 attempt。
// 只有 attempt、没有最终 message 的会话同样要入库。
func countDSHUsageEvents(events []dshEvent) int {
	count := 0
	for _, event := range events {
		if event.Type == "assistant/message" || event.Type == "assistant/attempt" {
			count++
		}
	}
	return count
}
