package collector

import (
	"encoding/binary"
	"strconv"
)

// zstd 帧结构常量（RFC 8878）。
const (
	dshZstdMagic         uint32 = 0xFD2FB528
	dshZstdSkippableMask uint32 = 0xFFFFFFF0
	dshZstdSkippableBase uint32 = 0x184D2A50
	dshZstdMaxFrames            = 100_000
)

// dshFrameRange 是一个完整 zstd 帧的字节区间，[Start,End)。
type dshFrameRange struct {
	Start int
	End   int
}

// dshFrameScan 是一次结构扫描的结果。
type dshFrameScan struct {
	Frames    []dshFrameRange
	TornStart int  // 末尾不完整帧的起点；-1 表示没有
	Truncated bool // 达到帧数上限，后面的帧未扫描
}

// errDSHZstdCorrupt 表示数据不是可识别的 zstd 帧序列。
type dshZstdError struct {
	Offset int
	Reason string
}

func (e *dshZstdError) Error() string {
	return "zstd frame scan: offset " + strconv.Itoa(e.Offset) + ": " + e.Reason
}

// scanDSHZstdFrames 只做帧结构扫描，不解压：
//   - 返回完整帧区间；末尾不完整帧记入 TornStart；
//   - skippable frame 整体跳过；
//   - 字典帧（DictID != 0）不支持，直接报错；
//   - 超过 maxFrames 时返回已扫描帧并置 Truncated。
func scanDSHZstdFrames(data []byte, maxFrames int) (dshFrameScan, error) {
	scan := dshFrameScan{TornStart: -1}
	if maxFrames <= 0 {
		maxFrames = dshZstdMaxFrames
	}

	offset := 0
	physicalFrames := 0
	for offset < len(data) {
		if physicalFrames >= maxFrames {
			scan.Truncated = true
			return scan, nil
		}
		if len(data)-offset < 4 {
			scan.TornStart = offset
			return scan, nil
		}
		magic := binary.LittleEndian.Uint32(data[offset:])
		if magic == dshZstdMagic {
			end, err := dshZstdFrameEnd(data, offset)
			if err != nil {
				return scan, err
			}
			if end < 0 {
				scan.TornStart = offset
				return scan, nil
			}
			scan.Frames = append(scan.Frames, dshFrameRange{Start: offset, End: end})
			physicalFrames++
			offset = end
			continue
		}
		if magic&dshZstdSkippableMask == dshZstdSkippableBase {
			if len(data)-offset < 8 {
				scan.TornStart = offset
				return scan, nil
			}
			size := int(binary.LittleEndian.Uint32(data[offset+4:]))
			end := offset + 8 + size
			if size < 0 || end > len(data) || end < offset {
				scan.TornStart = offset
				return scan, nil
			}
			physicalFrames++
			offset = end
			continue
		}
		return scan, &dshZstdError{Offset: offset, Reason: "invalid frame magic"}
	}
	return scan, nil
}

// dshZstdFrameEnd 解析一个帧头与块链，返回帧结束偏移；数据不足返回 -1。
func dshZstdFrameEnd(data []byte, start int) (int, error) {
	pos := start + 4 // magic
	if pos >= len(data) {
		return -1, nil
	}
	descriptor := data[pos]
	pos++

	if descriptor&0x08 != 0 {
		return 0, &dshZstdError{Offset: start, Reason: "reserved frame header bit is set"}
	}
	fcsFlag := descriptor >> 6
	singleSegment := descriptor&0x20 != 0
	hasChecksum := descriptor&0x04 != 0
	dictIDFlag := descriptor & 0x03

	if !singleSegment {
		if pos >= len(data) {
			return -1, nil
		}
		pos++ // window descriptor
	}

	dictIDSize := [4]int{0, 1, 2, 4}[dictIDFlag]
	if dictIDSize > 0 {
		if len(data)-pos < dictIDSize {
			return -1, nil
		}
		dictID := 0
		for i := 0; i < dictIDSize; i++ {
			dictID = dictID<<8 | int(data[pos+i])
		}
		if dictID != 0 {
			return 0, &dshZstdError{Offset: start, Reason: "dictionary frames are not supported"}
		}
		pos += dictIDSize
	}

	fcsSize := [4]int{0, 2, 4, 8}[fcsFlag]
	if singleSegment && fcsFlag == 0 {
		fcsSize = 1
	}
	if len(data)-pos < fcsSize {
		return -1, nil
	}
	pos += fcsSize

	for {
		if len(data)-pos < 3 {
			return -1, nil
		}
		header := uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16
		pos += 3
		lastBlock := header&0x01 != 0
		blockType := (header >> 1) & 0x03
		blockSize := int(header >> 3)
		switch blockType {
		case 0, 2: // raw / compressed
			if blockSize < 0 || len(data)-pos < blockSize {
				return -1, nil
			}
			pos += blockSize
		case 1: // RLE：1 字节
			if len(data)-pos < 1 {
				return -1, nil
			}
			pos++
		default:
			return 0, &dshZstdError{Offset: pos - 3, Reason: "reserved block type"}
		}
		if lastBlock {
			break
		}
	}

	if hasChecksum {
		if len(data)-pos < 4 {
			return -1, nil
		}
		pos += 4
	}
	return pos, nil
}
