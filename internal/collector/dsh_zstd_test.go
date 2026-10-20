package collector

import (
	"encoding/binary"
	"strings"
	"testing"
)

func makeDSHSkippable(t *testing.T, payload []byte) []byte {
	t.Helper()
	buf := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(buf[0:], dshZstdSkippableBase)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

func makeDSHDictionaryFrame() []byte {
	// magic + descriptor(dictIDFlag=1, singleSegment=0) + window + dictID(1) +
	// 空 last block。扫描应在 dictID 非零时报「不支持」。
	return []byte{
		0x28, 0xB5, 0x2F, 0xFD,
		0x01,
		0x00,
		0x2A,
		0x01, 0x00, 0x00,
	}
}

func TestScanDSHZstdFramesSingle(t *testing.T) {
	data := encodeDSHFrames(t, dshFixtureContent, 1)
	scan, err := scanDSHZstdFrames(data, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 1 || scan.Frames[0].Start != 0 || scan.Frames[0].End != len(data) {
		t.Fatalf("frames = %+v, want one full frame", scan.Frames)
	}
	if scan.TornStart != -1 || scan.Truncated {
		t.Fatalf("torn=%d truncated=%v, want none", scan.TornStart, scan.Truncated)
	}
}

func TestScanDSHZstdFramesMulti(t *testing.T) {
	data := encodeDSHFrames(t, dshFixtureContent, 3)
	scan, err := scanDSHZstdFrames(data, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(scan.Frames))
	}
	for i := 1; i < len(scan.Frames); i++ {
		if scan.Frames[i].Start != scan.Frames[i-1].End {
			t.Fatalf("frames not contiguous: %+v", scan.Frames)
		}
	}
	if scan.Frames[len(scan.Frames)-1].End != len(data) {
		t.Fatalf("last frame end = %d, want %d", scan.Frames[len(scan.Frames)-1].End, len(data))
	}
}

func TestScanDSHZstdFramesSkippable(t *testing.T) {
	first := encodeDSHFrames(t, dshFixtureContent, 1)
	second := encodeDSHFrames(t, dshFixtureContent, 1)
	skippable := makeDSHSkippable(t, []byte("metadata"))

	data := append(append(append([]byte{}, first...), skippable...), second...)
	scan, err := scanDSHZstdFrames(data, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 2 {
		t.Fatalf("frames = %d, want 2 (skippable skipped)", len(scan.Frames))
	}
	if scan.Frames[1].Start != len(first)+len(skippable) {
		t.Fatalf("second frame start = %d, want %d", scan.Frames[1].Start, len(first)+len(skippable))
	}
}

func TestScanDSHZstdFramesTornTail(t *testing.T) {
	data := encodeDSHFrames(t, dshFixtureContent, 3)
	torn := data[:len(data)-2]
	scan, err := scanDSHZstdFrames(torn, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 2 {
		t.Fatalf("frames = %d, want 2 complete frames", len(scan.Frames))
	}
	if scan.TornStart != scan.Frames[1].End {
		t.Fatalf("torn start = %d, want %d", scan.TornStart, scan.Frames[1].End)
	}
}

func TestScanDSHZstdFramesDictionaryRejected(t *testing.T) {
	_, err := scanDSHZstdFrames(makeDSHDictionaryFrame(), 0)
	if err == nil || !strings.Contains(err.Error(), "dictionary frames are not supported") {
		t.Fatalf("err = %v, want dictionary rejection", err)
	}
}

func TestScanDSHZstdFramesLimit(t *testing.T) {
	data := encodeDSHFrames(t, dshFixtureContent, 3)
	scan, err := scanDSHZstdFrames(data, 1)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 1 || !scan.Truncated {
		t.Fatalf("frames=%d truncated=%v, want 1/true", len(scan.Frames), scan.Truncated)
	}
}

func TestScanDSHZstdFramesInvalidMagic(t *testing.T) {
	_, err := scanDSHZstdFrames([]byte("not a zstd stream"), 0)
	if err == nil || !strings.Contains(err.Error(), "invalid frame magic") {
		t.Fatalf("err = %v, want invalid magic", err)
	}
}

func TestScanDSHZstdFramesEmpty(t *testing.T) {
	scan, err := scanDSHZstdFrames(nil, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Frames) != 0 || scan.TornStart != -1 {
		t.Fatalf("empty scan = %+v", scan)
	}
}
