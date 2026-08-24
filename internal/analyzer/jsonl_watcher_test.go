// internal/analyzer/jsonl_watcher_test.go
package analyzer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// captureLogHandler 收集全部日志记录，供断言日志聚合行为。
type captureLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(name string) slog.Handler       { return h }

func (h *captureLogHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := make([]string, 0, len(h.records))
	for _, r := range h.records {
		msgs = append(msgs, r.Message)
	}
	return msgs
}

// 目录注册只输出一条 count 汇总，不再逐目录打印。
func TestJSONLWatcher_RegistrationSummarySingleLog(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"proj-a", "proj-b", "proj-c"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	handler := &captureLogHandler{}
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir}, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, slog.New(handler))
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	watcher.Stop()

	var summaries, perDir int
	for _, msg := range handler.messages() {
		if msg == "watching directories" {
			summaries++
		}
		if msg == "watching directory" {
			perDir++
		}
	}
	if summaries != 1 {
		t.Errorf("期望恰好 1 条 watching directories 汇总，实际 %d 条: %v", summaries, handler.messages())
	}
	if perDir != 0 {
		t.Errorf("不应再逐目录打印 watching directory，实际 %d 条", perDir)
	}
}

func TestJSONLWatcher_DetectsNewFile(t *testing.T) {
	tmpDir := t.TempDir()

	// 模拟真实目录结构：projects_dir/<proj>/session.jsonl
	projDir := filepath.Join(tmpDir, "my-project")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var triggered int32
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir},
		"claude",
		50*time.Millisecond, // debounce
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil, // logger: 用 nil，Run 内会 fallback 到 slog.Default()
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go watcher.Run(ctx)

	// 等待 watcher 启动 + Walk 递归 Add
	time.Sleep(100 * time.Millisecond)

	// 在子目录中创建新 JSONL 文件
	newFile := filepath.Join(projDir, "session-123.jsonl")
	os.WriteFile(newFile, []byte(`{"type":"message"}`), 0644)

	// 等待检测和 debounce
	time.Sleep(300 * time.Millisecond)

	cancel()
	watcher.Stop()

	if atomic.LoadInt32(&triggered) == 0 {
		t.Error("expected at least one trigger after new file in subdirectory")
	}
}

// TestJSONLWatcher_PassesChangedFile 行为：JSONL watcher debounce 回调应收到 ChangedFile
// 原始绝对路径，而非 mtime 推导的日期。日期采集已由 collector 内部处理。
func TestJSONLWatcher_PassesChangedFile(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "my-project")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	type capturedReq struct {
		client string
		req    collector.CollectRequest
	}
	requests := make(chan capturedReq, 4)
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir},
		"claude",
		50*time.Millisecond, // debounce
		func(client string, req collector.CollectRequest) {
			requests <- capturedReq{client: client, req: req}
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	filePath := filepath.Join(projDir, "session-abc.jsonl")
	if err := os.WriteFile(filePath, []byte(`{"type":"message"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-requests:
		if got.client != "claude" {
			t.Fatalf("client = %q, want claude", got.client)
		}
		if got.req.ChangedFile != filePath {
			t.Fatalf("ChangedFile = %q, want %q", got.req.ChangedFile, filePath)
		}
		if len(got.req.Dates) != 0 {
			t.Fatalf("Dates = %v, want empty", got.req.Dates)
		}
		if got.req.Incremental {
			t.Fatalf("Incremental = true, want false")
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("timeout waiting for debounce callback")
	}

	cancel()
	watcher.Stop()
}

func TestJSONLWatcher_DetectsNewSubdir(t *testing.T) {
	tmpDir := t.TempDir()

	var triggered int32
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir},
		"claude",
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	go watcher.Run(ctx)

	// 等待 watcher 启动
	time.Sleep(100 * time.Millisecond)

	// 运行中新建子目录（模拟 claude 动态创建项目目录）
	newProjDir := filepath.Join(tmpDir, "new-project")
	os.MkdirAll(newProjDir, 0755)

	// 等待 Create+IsDir 事件触发动态 Add
	time.Sleep(100 * time.Millisecond)

	// 在新子目录中创建 JSONL 文件
	newFile := filepath.Join(newProjDir, "session-456.jsonl")
	os.WriteFile(newFile, []byte(`{"type":"message"}`), 0644)

	// 等待检测和 debounce
	time.Sleep(300 * time.Millisecond)

	cancel()
	watcher.Stop()

	if atomic.LoadInt32(&triggered) == 0 {
		t.Error("expected trigger after creating file in newly created subdirectory")
	}
}

func TestJSONLWatcher_DetectsFileChange(t *testing.T) {
	tmpDir := t.TempDir()

	// 模拟子目录结构
	projDir := filepath.Join(tmpDir, "my-project")
	os.MkdirAll(projDir, 0755)

	// 创建初始文件
	existingFile := filepath.Join(projDir, "session-456.jsonl")
	os.WriteFile(existingFile, []byte(`{"type":"message"}`), 0644)

	var triggered int32
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir},
		"claude",
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go watcher.Run(ctx)

	// 等待 watcher 启动
	time.Sleep(100 * time.Millisecond)

	// 修改现有文件
	os.WriteFile(existingFile, []byte(`{"type":"message"}\n{"type":"assistant"}`), 0644)

	// 等待检测和 debounce
	time.Sleep(300 * time.Millisecond)

	cancel()
	watcher.Stop()

	if atomic.LoadInt32(&triggered) == 0 {
		t.Error("expected at least one trigger after file change")
	}
}

func TestJSONLWatcher_IgnoresNonJSONL(t *testing.T) {
	tmpDir := t.TempDir()

	var triggered int32
	watcher, err := NewJSONLWatcher(
		[]string{tmpDir},
		"claude",
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go watcher.Run(ctx)

	time.Sleep(100 * time.Millisecond)

	// 创建非 .jsonl 文件
	os.WriteFile(filepath.Join(tmpDir, "session.txt"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "session.json"), []byte("{}"), 0644)

	time.Sleep(200 * time.Millisecond)

	cancel()
	watcher.Stop()

	if atomic.LoadInt32(&triggered) != 0 {
		t.Errorf("expected 0 triggers for non-jsonl files, got %d", atomic.LoadInt32(&triggered))
	}
}

func TestJSONLWatcher_StopTwice(t *testing.T) {
	watcher, err := NewJSONLWatcher([]string{"/tmp"}, "test", time.Second, func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}

	// Stop 两次不应 panic
	watcher.Stop()
	watcher.Stop()
}
