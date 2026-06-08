package logger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInit_CreatesLogFile(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := Init("info", tmpDir, 7)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, "token-usage-"+today+".log")

	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("log file should exist after Init")
	}

	logger.Info("test message")
}

func TestCleanup_RemovesOldLogs(t *testing.T) {
	tmpDir := t.TempDir()

	oldDate := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	oldPath := filepath.Join(tmpDir, "token-usage-"+oldDate+".log")
	os.WriteFile(oldPath, []byte("old log"), 0644)

	newDate := time.Now().Format("2006-01-02")
	newPath := filepath.Join(tmpDir, "token-usage-"+newDate+".log")
	os.WriteFile(newPath, []byte("new log"), 0644)

	cleanup(tmpDir, 7)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old log file should be removed")
	}
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Error("new log file should still exist")
	}
}

func TestGetLogger_ReturnsLogger(t *testing.T) {
	tmpDir := t.TempDir()
	Init("info", tmpDir, 7)
	defer Close()

	logger := GetLogger()
	if logger == nil {
		t.Error("GetLogger should not return nil")
	}
}

func TestInit_WritesToFile(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := Init("debug", tmpDir, 7)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	logger.Debug("debug message", "key", "value")
	logger.Info("info message")

	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(tmpDir, "token-usage-"+today+".log")

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	logStr := string(content)
	if !strings.Contains(logStr, "debug message") {
		t.Error("log should contain debug message")
	}
	if !strings.Contains(logStr, "info message") {
		t.Error("log should contain info message")
	}
}

func TestRotatingWriter_RotatesAtDateChange(t *testing.T) {
	dir := t.TempDir()
	current := time.Date(2026, 6, 22, 23, 59, 59, 0, time.Local)
	w := newRotatingWriter(dir, func() time.Time { return current })
	if _, err := w.Write([]byte("before-midnight\n")); err != nil {
		t.Fatal(err)
	}
	current = current.Add(2 * time.Second)
	if _, err := w.Write([]byte("after-midnight\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	first, err := os.ReadFile(filepath.Join(dir, "token-usage-2026-06-22.log"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "token-usage-2026-06-23.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "before-midnight") || strings.Contains(string(first), "after-midnight") {
		t.Fatalf("first file content: %q", first)
	}
	if !strings.Contains(string(second), "after-midnight") {
		t.Fatalf("second file content: %q", second)
	}
}

func TestRotatingWriter_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w := newRotatingWriter(dir, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := fmt.Fprintf(w, "line-%d\n", i); err != nil {
				t.Errorf("write: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "token-usage-"+time.Now().Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(content), "line-") != 20 {
		t.Fatalf("lost concurrent writes: %q", content)
	}
}

// TestRotatingWriter_WriteAfterCloseFails 边界修复：
// Close 后再 Write 必须返回 os.ErrClosed，且不得重新打开日志文件。
// 否则旧 slog.Logger 引用（持有已 Close 的 writer）仍会经 ensureFileLocked 重开文件，
// 导致关闭语义失效并泄漏句柄。
func TestRotatingWriter_WriteAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	w := newRotatingWriter(dir, time.Now)
	if _, err := w.Write([]byte("before-close\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("after-close\n")); err == nil {
		t.Fatal("Write after Close must fail")
	} else if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close must return os.ErrClosed, got %v", err)
	}
	today := time.Now().Format("2006-01-02")
	content, err := os.ReadFile(filepath.Join(dir, "token-usage-"+today+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "after-close") {
		t.Fatalf("Write after Close must not reopen file: %q", content)
	}
}
