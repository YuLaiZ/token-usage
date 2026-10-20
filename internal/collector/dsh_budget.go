package collector

import (
	"errors"
	"io"
)

// dshMaxFrameDecodedBytes 是单帧累计解压上限：超过即判该会话失败。
// 64 MiB 是初始值，实施阶段用基准校准。
const dshMaxFrameDecodedBytes = 64 << 20

// dshMaxZstdWindowBytes 只用于限制解码窗口内存（1 GiB），与累计预算无关。
// 不能把累计预算传给 WithDecoderMaxMemory：流式模式下它限制的是窗口。
const dshMaxZstdWindowBytes = 1 << 30

// errDSHFrameBudget 表示单帧累计解压超过预算。
var errDSHFrameBudget = errors.New("dsh: frame decode budget exceeded")

// frameBudgetReader 在 zstd reader 与行迭代器之间累计解压字节。
// 超过 limit 时返回 errDSHFrameBudget；不能用 io.LimitReader，
// 那会把「达到上限」表现为正常 EOF。
type frameBudgetReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (r *frameBudgetReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.read += int64(n)
	if r.read > r.limit {
		return n, errDSHFrameBudget
	}
	return n, err
}
