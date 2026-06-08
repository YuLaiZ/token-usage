package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCollectWithRetry_SuccessAfterRetries(t *testing.T) {
	var calls atomic.Int32
	collectFn := func(ctx context.Context) Result {
		n := calls.Add(1)
		if n < 3 { // 前 2 次失败，第 3 次成功
			return Result{Matched: true, Attempted: 1, Succeeded: 0, Err: fmt.Errorf("fail %d", n)}
		}
		return Result{Matched: true, Attempted: 1, Succeeded: 1}
	}
	// backoff 用 0 延迟（测试不真睡）
	res := RunCollectWithRetry(context.Background(), collectFn, 3,
		func(int) time.Duration { return 0 }, nil)
	if !res.Complete() {
		t.Fatalf("期望第 3 次成功，得到 %+v", res)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("期望调用 3 次，实际 %d", got)
	}
}

func TestRunCollectWithRetry_ExhaustsMaxRetries(t *testing.T) {
	var calls atomic.Int32
	collectFn := func(ctx context.Context) Result {
		calls.Add(1)
		return Result{Matched: true, Attempted: 1, Err: fmt.Errorf("always fail")}
	}
	res := RunCollectWithRetry(context.Background(), collectFn, 3,
		func(int) time.Duration { return 0 }, nil)
	if res.Complete() {
		t.Fatal("期望最终失败")
	}
	// 首次 + 3 次重试 = 4 次
	if got := calls.Load(); got != 4 {
		t.Fatalf("期望调用 4 次（首采+3 重试），实际 %d", got)
	}
}

func TestRunCollectWithRetry_CancelInterruptsBackoff(t *testing.T) {
	var calls atomic.Int32
	collectFn := func(ctx context.Context) Result {
		calls.Add(1)
		return Result{Matched: true, Attempted: 1, Err: fmt.Errorf("fail")}
	}
	ctx, cancel := context.WithCancel(context.Background())
	// 长退避 + 取消前不重试
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res := RunCollectWithRetry(ctx, collectFn, 3,
		func(int) time.Duration { return 10 * time.Second }, nil)
	if got := calls.Load(); got != 1 {
		t.Fatalf("ctx 取消应只采 1 次（退避被打断），实际 %d", got)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("取消原因必须保留在 Result.Err 中，err=%v", res.Err)
	}
}

func TestRunCollectWithRetry_PreCancelledContextDoesNotCollect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	res := RunCollectWithRetry(ctx, func(context.Context) Result {
		calls.Add(1)
		return Result{Matched: true, Attempted: 1, Succeeded: 1}
	}, 3, nil, nil)
	if calls.Load() != 0 {
		t.Fatalf("预先取消后仍调用采集 %d 次", calls.Load())
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", res.Err)
	}
}

func TestRunCollectWithRetry_NilCollectFuncReturnsError(t *testing.T) {
	res := RunCollectWithRetry(context.Background(), nil, 1, nil, nil)
	if res.Err == nil {
		t.Fatal("nil collectFn 应返回错误")
	}
}
