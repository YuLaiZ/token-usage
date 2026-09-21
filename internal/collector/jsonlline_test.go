package collector

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type iterStep struct {
	lineNo    int
	line      string
	oversized bool
}

func runIter(ctx context.Context, r io.Reader, maxLine int) ([]iterStep, error) {
	var steps []iterStep
	it := newJSONLLineIter(ctx, r, maxLine)
	for it.Next() {
		s := iterStep{lineNo: it.LineNo(), oversized: it.Oversized()}
		if !it.Oversized() {
			s.line = string(it.Line())
		}
		steps = append(steps, s)
	}
	return steps, it.Err()
}

func TestJSONLLineIter_ScanLinesSemantics(t *testing.T) {
	// 剥行尾单个 \r、空行照常产出、尾行无 \n 产出一次。
	steps, err := runIter(context.Background(), strings.NewReader("a\nb\r\n\nc"), 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []iterStep{
		{lineNo: 1, line: "a"},
		{lineNo: 2, line: "b"},
		{lineNo: 3, line: ""},
		{lineNo: 4, line: "c"},
	}
	if len(steps) != len(want) {
		t.Fatalf("steps = %+v, want %+v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("steps[%d] = %+v, want %+v", i, steps[i], w)
		}
	}
}

func TestJSONLLineIter_NoEmptyTailLineAfterFinalNewline(t *testing.T) {
	steps, err := runIter(context.Background(), strings.NewReader("a\nb\n"), 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].line != "a" || steps[1].line != "b" {
		t.Fatalf("steps = %+v, want exactly [a b]（以 \\n 结尾不产出空尾行）", steps)
	}
}

func TestJSONLLineIter_SingleTrailingCRStripped(t *testing.T) {
	// 仅剥一个 \r：双 \r 保留一个，整行 \r 变空行。
	steps, err := runIter(context.Background(), strings.NewReader("ab\r\r\n\r\n"), 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].line != "ab\r" || steps[1].line != "" {
		t.Fatalf("steps = %+v, want [ab\\r \"\"]", steps)
	}
}

func TestJSONLLineIter_EmptyInput(t *testing.T) {
	steps, err := runIter(context.Background(), strings.NewReader(""), 1<<20)
	if err != nil || len(steps) != 0 {
		t.Fatalf("steps = %+v err = %v, want 0 步且无错", steps, err)
	}
}

func TestJSONLLineIter_ExactMaxLineMultiChunkDelivered(t *testing.T) {
	// 恰等于 maxLine（>64KB 读缓冲，跨 chunk 累积）必须完整产出。
	maxLine := 100_000
	long := strings.Repeat("x", maxLine)
	steps, err := runIter(context.Background(), strings.NewReader(long+"\nnext\n"), maxLine)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].oversized || len(steps[0].line) != maxLine || steps[1].line != "next" {
		t.Fatalf("steps = [%d 项，首行 len=%d oversized=%v]，want 恰等于上限完整产出 + next", len(steps), len(steps[0].line), steps[0].oversized)
	}
}

func TestJSONLLineIter_OversizedSingleChunkSkipped(t *testing.T) {
	// 单 chunk 即完整的超限行（len ≤ 64KB 读缓冲）走 emit 判超路径。
	steps, err := runIter(context.Background(), strings.NewReader("0123456789A\nnext\n"), 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || !steps[0].oversized || steps[0].lineNo != 1 || steps[1].line != "next" || steps[1].lineNo != 2 {
		t.Fatalf("steps = %+v, want 第 1 行超限跳过、第 2 行 next", steps)
	}
}

func TestJSONLLineIter_OversizedMultiChunkDrainedAndContinue(t *testing.T) {
	// 超限行跨越多个 64KB chunk：丢弃全部数据后继续，后续行行号连续。
	maxLine := 100_000
	huge := strings.Repeat("y", 200_000)
	steps, err := runIter(context.Background(), strings.NewReader("before\n"+huge+"\nafter\n"), maxLine)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 3 || steps[0].line != "before" || !steps[1].oversized || steps[1].lineNo != 2 || steps[2].line != "after" || steps[2].lineNo != 3 {
		t.Fatalf("steps = %+v, want before / 第2行超限 / 第3行 after", steps)
	}
}

func TestJSONLLineIter_OversizedFinalLineWithoutNewline(t *testing.T) {
	// 超限行即尾行（无 \n）：产出一次 Oversized 后正常结束。
	steps, err := runIter(context.Background(), strings.NewReader("good\n"+strings.Repeat("z", 20)), 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].line != "good" || !steps[1].oversized || steps[1].lineNo != 2 {
		t.Fatalf("steps = %+v, want good + 第2行超限", steps)
	}
}

func TestJSONLLineIter_LineNoContinuityAcrossMultipleOversized(t *testing.T) {
	maxLine := 100_000
	huge := strings.Repeat("q", 150_000)
	steps, err := runIter(context.Background(), strings.NewReader("a\n"+huge+"\nb\n"+huge+"\nc\n"), maxLine)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []iterStep{
		{lineNo: 1, line: "a"},
		{lineNo: 2, oversized: true},
		{lineNo: 3, line: "b"},
		{lineNo: 4, oversized: true},
		{lineNo: 5, line: "c"},
	}
	if len(steps) != len(want) {
		t.Fatalf("steps = %+v, want %+v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Errorf("steps[%d] = %+v, want %+v", i, steps[i], w)
		}
	}
}

// failingReader 供完首块后返回固定错误。
type failingReader struct {
	data string
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestJSONLLineIter_IOErrorAfterGoodLine(t *testing.T) {
	boom := errors.New("boom")
	r := &failingReader{data: "line1\n", err: boom}
	steps, err := runIter(context.Background(), r, 1<<20)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(steps) != 1 || steps[0].line != "line1" {
		t.Fatalf("steps = %+v, want 错误前的完整行已产出", steps)
	}
}

func TestJSONLLineIter_IOErrorPartialLineNotEmitted(t *testing.T) {
	// 非_EOF_读错误带回的半行数据不得产出。
	boom := errors.New("boom")
	r := &failingReader{data: "partial-without-newline", err: boom}
	steps, err := runIter(context.Background(), r, 1<<20)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want 0 步（不产出半行）", steps)
	}
}

func TestJSONLLineIter_CtxCancelBeforeFirstNext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	steps, err := runIter(ctx, strings.NewReader("a\n"), 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want 0 步", steps)
	}
}

func TestJSONLLineIter_CtxCancelBetweenLines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	it := newJSONLLineIter(ctx, strings.NewReader("a\nb\n"), 1<<20)
	if !it.Next() || string(it.Line()) != "a" {
		t.Fatalf("首行应正常产出")
	}
	cancel()
	if it.Next() {
		t.Fatalf("取消后 Next 应返回 false")
	}
	if !errors.Is(it.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", it.Err())
	}
}

// cancelDuringDrainReader 每次填满 p 且永不含换行；第 2 次读时取消 ctx，
// 用于验证 drain 循环内的取消检查。
type cancelDuringDrainReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelDuringDrainReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 2 {
		r.cancel()
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestJSONLLineIter_CtxCancelDuringDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps, err := runIter(ctx, &cancelDuringDrainReader{cancel: cancel}, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled（drain 期间取消必须终止迭代）", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want 0 步", steps)
	}
}

// 尾行无 \n 且长度恰为读缓冲（64KiB）整数倍：数据全部经 ErrBufferFull 走完
// 累积、EOF 到来时 chunk 为空，累积中的尾行仍须产出一次（对齐 ScanLines）。
func TestJSONLLineIter_TailLineExactBufferMultipleNoNewline(t *testing.T) {
	for _, size := range []int{64 * 1024, 128 * 1024} {
		input := "good\n" + strings.Repeat("x", size)
		steps, err := runIter(context.Background(), strings.NewReader(input), 1<<20)
		if err != nil {
			t.Fatalf("size=%d: err = %v, want nil", size, err)
		}
		if len(steps) != 2 || steps[0].line != "good" || len(steps[1].line) != size {
			t.Errorf("size=%d: steps = [%d 项，尾行 len=%d]，want good + 完整尾行（不得静默丢弃）",
				size, len(steps), len(steps[len(steps)-1].line))
		}
	}
}

// 行长 maxLine+1 且跨 chunk：前段累积不超限、最终 chunk 补完时才越过上限，
// 走 deliver→emit 的判超路径（与 ErrBufferFull 中途判超的 drain 路径互补）。
func TestJSONLLineIter_OversizedBoundaryMaxLinePlusOneMultiChunk(t *testing.T) {
	maxLine := 100_000
	line := strings.Repeat("x", maxLine+1) // 65536 + 34465：次 chunk 含 \n 补完整行
	steps, err := runIter(context.Background(), strings.NewReader(line+"\nafter\n"), maxLine)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || !steps[0].oversized || steps[0].lineNo != 1 || steps[1].line != "after" {
		t.Fatalf("steps = %+v, want 第 1 行超限（恰超 1 字节）+ 第 2 行 after", steps)
	}
}

// 单 chunk 行恰等于 maxLine：emit 判定须为严格大于，不得误判超限。
func TestJSONLLineIter_ExactMaxLineSingleChunk(t *testing.T) {
	steps, err := runIter(context.Background(), strings.NewReader("0123456789A\nnext\n"), 11)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].oversized || steps[0].line != "0123456789A" || steps[1].line != "next" {
		t.Fatalf("steps = %+v, want 恰等上限（11 字节）完整产出 + next", steps)
	}
}

// cancelOnFirstReadReader 首次 Read 返回 payload 且同刻取消 ctx：行数据完整
// 返回、交付前已取消——锁定 deliver 检查点（与取消并发读出的行不得交付）。
type cancelOnFirstReadReader struct {
	payload string
	cancel  context.CancelFunc
	served  bool
}

func (r *cancelOnFirstReadReader) Read(p []byte) (int, error) {
	if r.served {
		return 0, io.EOF
	}
	r.served = true
	r.cancel()
	return copy(p, r.payload), nil
}

func TestJSONLLineIter_CtxCancelDuringReadOfLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps, err := runIter(ctx, &cancelOnFirstReadReader{payload: "a\n", cancel: cancel}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want 0 步（读出与取消并发的行不得交付）", steps)
	}
}

// ErrBufferFull 已累积部分数据后 IO 错误：不产出半行，Err 返回该错误。
func TestJSONLLineIter_IOErrorAfterBufferFullAccumulated(t *testing.T) {
	boom := errors.New("boom")
	r := &failingReader{data: strings.Repeat("x", 64*1024), err: boom}
	steps, err := runIter(context.Background(), r, 1<<20)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want 0 步（累积中的半行不得产出）", steps)
	}
}

// cancelOnEOFReader 首次 Read 供出唯一一行，EOF 读窗口内取消：最后一行已交付、
// 迭代按干净 EOF 结束且 Err==nil——该竞态由调用方循环后的 ctx.Err() 终检兜底
// （合同见 jsonlLineIter.Err 注释）。
type cancelOnEOFReader struct {
	cancel context.CancelFunc
	served bool
}

func (r *cancelOnEOFReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		return copy(p, "a\n"), nil
	}
	r.cancel()
	return 0, io.EOF
}

func TestJSONLLineIter_CtxCancelDuringCleanEOFReadErrNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps, err := runIter(ctx, &cancelOnEOFReader{cancel: cancel}, 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil（干净 EOF 竞态取消：Err 为 nil，调用方终检兜底）", err)
	}
	if len(steps) != 1 || steps[0].line != "a" {
		t.Fatalf("steps = %+v, want 仅 a 已交付", steps)
	}
}

// 跨 chunk 累积判超的超限行恰为无 \n 尾行：skipOversized 的 drain 遇 io.EOF
// 仍须产出一次 Oversized 并正常结束（回归该分支将漏记坏行）。
func TestJSONLLineIter_OversizedMultiChunkTailNoNewline(t *testing.T) {
	maxLine := 100_000
	input := "good\n" + strings.Repeat("x", 150_000)
	steps, err := runIter(context.Background(), strings.NewReader(input), maxLine)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(steps) != 2 || steps[0].line != "good" || !steps[1].oversized || steps[1].lineNo != 2 {
		t.Fatalf("steps = %+v, want good + 第 2 行超限（drain 至 EOF 仍须产出 Oversized）", steps)
	}
}
