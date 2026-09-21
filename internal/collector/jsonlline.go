package collector

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// jsonlLineIter 逐行迭代 JSONL，行语义对齐 bufio.Scanner/ScanLines：产出不含
// \n 的行并剥离行尾单个 \r；空行照常产出；文件末尾无 \n 的尾行产出一次；文件
// 以 \n 结尾时不产出空尾行。
//
// 超过 maxLine 的行以 Oversized()==true 产出一次（Line() 返回 nil，行数据丢弃，
// 迭代继续，由调用方计入坏行）；IO 错误与 ctx 取消终止迭代并由 Err() 返回。
// Line() 返回的切片仅在当前迭代步内有效，跨 Next() 调用不得持有。
type jsonlLineIter struct {
	ctx     context.Context
	r       *bufio.Reader
	maxLine int

	lineNo    int
	line      []byte
	oversized bool
	err       error
	done      bool
	acc       []byte // 跨 chunk 行的累积缓冲（容量复用）；len==0 表示当前行未跨 chunk
}

func newJSONLLineIter(ctx context.Context, r io.Reader, maxLine int) *jsonlLineIter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &jsonlLineIter{
		ctx:     ctx,
		r:       bufio.NewReaderSize(r, 64*1024),
		maxLine: maxLine,
	}
}

// Next 推进到下一行；返回 false 时迭代结束，结束原因经 Err() 获取（nil 为正常 EOF）。
func (it *jsonlLineIter) Next() bool {
	if it.done {
		return false
	}
	it.acc = it.acc[:0]
	for {
		chunk, rerr := it.r.ReadSlice('\n')
		switch {
		case rerr == nil:
			// 完整行：chunk 以 \n 结尾。
			it.lineNo++
			line := chunk[:len(chunk)-1]
			if len(it.acc) > 0 {
				it.acc = append(it.acc, line...)
				line = it.acc
			}
			return it.deliver(line)
		case errors.Is(rerr, bufio.ErrBufferFull):
			it.acc = append(it.acc, chunk...)
			// 累积长度一超上限即判超限并转入丢弃（内存边界不随输入膨胀）。
			if len(it.acc) > it.maxLine {
				return it.skipOversized()
			}
		case errors.Is(rerr, io.EOF):
			it.done = true
			// 尾行未以 \n 终结：产出一次。长度恰为读缓冲整数倍的尾行会以
			// ErrBufferFull 走完积累、EOF 时 chunk 为空，此时 acc 里就是尾行。
			if len(chunk) > 0 || len(it.acc) > 0 {
				it.lineNo++
				line := chunk
				if len(it.acc) > 0 {
					it.acc = append(it.acc, chunk...)
					line = it.acc
				}
				return it.deliver(line)
			}
			return false
		default:
			// 读错误：不产出半行。
			it.fail(rerr)
			return false
		}
	}
}

// deliver 在「行已读出、尚未交给调用方」之间检查 ctx 取消，与旧 Scanner 循环
// 「Scan 之后、处理之前」的检查点对齐：与取消并发读出的行不得再被处理。
func (it *jsonlLineIter) deliver(line []byte) bool {
	if err := it.ctx.Err(); err != nil {
		it.fail(err)
		return false
	}
	it.emit(line)
	return true
}

// emit 产出一行。单 chunk 即完整的超限行走此路径（无需 drain）。
func (it *jsonlLineIter) emit(line []byte) {
	if len(line) > it.maxLine {
		it.oversized = true
		it.line = nil
		return
	}
	it.oversized = false
	it.line = dropCR(line)
}

// skipOversized 处理累积超限的行：丢弃已累积数据，drain 到行尾（\n 或 EOF）
// 后产出一次 Oversized；drain 每轮检查 ctx 取消（病态超长行不得绕过取消）。
func (it *jsonlLineIter) skipOversized() bool {
	it.acc = it.acc[:0]
	it.lineNo++
	for {
		if err := it.ctx.Err(); err != nil {
			it.fail(err)
			return false
		}
		_, rerr := it.r.ReadSlice('\n')
		switch {
		case rerr == nil:
			if err := it.ctx.Err(); err != nil {
				it.fail(err)
				return false
			}
			it.oversized = true
			it.line = nil
			return true
		case errors.Is(rerr, bufio.ErrBufferFull):
			// 仍在丢弃超限行剩余数据。
		case errors.Is(rerr, io.EOF):
			// 超限行即尾行（无 \n）：产出 Oversized 后结束。
			if err := it.ctx.Err(); err != nil {
				it.fail(err)
				return false
			}
			it.done = true
			it.oversized = true
			it.line = nil
			return true
		default:
			it.fail(rerr)
			return false
		}
	}
}

func (it *jsonlLineIter) fail(err error) {
	it.err = err
	it.done = true
}

// LineNo 返回当前行号（1 起，与文件真实行号一致，含超限行）。
func (it *jsonlLineIter) LineNo() int { return it.lineNo }

// Line 返回当前行内容（不含 \n 与行尾单个 \r）；超限行返回 nil。
func (it *jsonlLineIter) Line() []byte { return it.line }

// Oversized 报告当前行是否超过 maxLine（该行数据已丢弃）。
func (it *jsonlLineIter) Oversized() bool { return it.oversized }

// Err 返回终止迭代的错误（IO 错误或 ctx 取消）；正常 EOF 时为 nil。
// 注意：取消若发生在最后一行已交付后的干净 EOF 读窗口，迭代器按正常 EOF 结束、
// Err 为 nil——该竞态由调用方循环后的 ctx.Err() 终检兜底（codex/workbuddy/
// autoclaw 均保留），不得移除调用方终检。
func (it *jsonlLineIter) Err() error { return it.err }

// dropCR 剥离行尾单个 \r（对齐 bufio.ScanLines）。
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[:len(data)-1]
	}
	return data
}
