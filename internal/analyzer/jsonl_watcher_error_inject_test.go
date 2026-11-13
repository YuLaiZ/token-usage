package analyzer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// 包内错误通道注入点：watcher 从注入的只读通道收到致命错误即返回（默认
// 绑定真实后端 Errors，测试不向真实后端通道写入）。
func TestJSONLWatcher_ErrorChannelInjection(t *testing.T) {
	tmpDir := t.TempDir()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	inject := make(chan error, 1)
	watcher.errCh = inject

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	inject <- errors.New("boom")

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "fsnotify watcher error: boom") {
			t.Fatalf("注入错误应从 Run 返回: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("注入致命错误后 Run 未返回")
	}
}

// monitor 级致命错误携带结构化来源：Analyzer.Run 返回的错误可被 errors.As
// 提取 Client 与 OccurredAt（daemon 退出边界据此记录来源，不从文案猜）。
func TestAnalyzer_MonitorFatalErrorCarriesSource(t *testing.T) {
	tmpDir := t.TempDir()
	projects := filepath.Join(tmpDir, "claude")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmpDir, "config.toml")
	cfgContent := fmt.Sprintf(`data_dir = %q
[clients.claude]
enabled = true
[clients.claude.paths]
projects_dir = %q
`, tmpDir, projects)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)
	a := NewFromConfig(cfg, noopExecute, nil, 50*time.Millisecond)
	inject := make(chan error, 1)
	replaced := false
	for _, w := range a.jsonlWatchers {
		if w.clientName == "claude" {
			w.errCh = inject
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("claude watcher 未装配")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-a.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("ready barrier 超时")
	}
	inject <- errors.New("boom")

	select {
	case err := <-done:
		var mfe *MonitorFatalError
		if !errors.As(err, &mfe) {
			t.Fatalf("错误链应含 MonitorFatalError: %v", err)
		}
		if mfe.Client != "claude" {
			t.Fatalf("Client = %q, want claude", mfe.Client)
		}
		if mfe.OccurredAt.IsZero() {
			t.Fatal("OccurredAt 未记录")
		}
		if !strings.Contains(mfe.Error(), "claude JSONL watcher failed") || !strings.Contains(mfe.Error(), "boom") {
			t.Fatalf("包装后错误原文必须保留: %v", mfe.Error())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("致命 monitor 错误后 Analyzer.Run 未返回")
	}
}
