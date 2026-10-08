package analyzer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// monitorSubmit 对 Submit 失败只打 Debug 心跳：采集失败的根因已由
// engine.RunCollect 以 ERROR 记录（"collection stage failed"），本侧是
// ValidateResult 对同一失败的二次包装——周期提交源在持续性部分失败期间
// 每 tick 重试，ERROR 级会按轮询间隔刷屏。
func TestMonitorSubmit_FailureLogsDebugNotError(t *testing.T) {
	var mu sync.Mutex
	var levels []slog.Level
	var msgs []string
	handler := func(_ context.Context, r slog.Record) error {
		mu.Lock()
		defer mu.Unlock()
		levels = append(levels, r.Level)
		msgs = append(msgs, r.Message)
		return nil
	}
	a := New(func(context.Context, string, collector.CollectRequest) error {
		return errors.New("collection incomplete")
	}, slog.New(&captureHandler{handle: handler}))
	// Run 安装 runCtx 后 monitorSubmit 才能提交；直接预置 accepting 形态：
	// 用一次真实 Run + 立即取消会引入时序，改为直接装配 runCtx。
	a.gateMu.Lock()
	runCtx, cancel := context.WithCancel(context.Background())
	a.runCtx = runCtx
	a.runCancel = cancel
	a.accepting = true
	a.gateMu.Unlock()
	defer cancel()

	a.monitorSubmit("workbuddy", collector.CollectRequest{})
	a.Stop()

	mu.Lock()
	defer mu.Unlock()
	// 目标日志必须恰好出现一次且为 DEBUG：零命中（日志被删/改名/未进入分支）
	// 或重复输出都判失败，级别回退（ERROR 刷屏）同样判失败。
	hits := 0
	for i, msg := range msgs {
		if msg == "monitor collection submit failed" {
			hits++
			if levels[i] != slog.LevelDebug {
				t.Errorf("monitor submit 失败应为 Debug 心跳, got %v（防止周期重试刷屏回退）", levels[i])
			}
		}
	}
	if hits != 1 {
		t.Errorf("目标日志应恰好命中 1 次, got %d（0=日志丢失或未进入分支, >1=重复输出）: %v", hits, msgs)
	}
}

type captureHandler struct {
	handle func(context.Context, slog.Record) error
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.handle(ctx, r)
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(name string) slog.Handler       { return h }
