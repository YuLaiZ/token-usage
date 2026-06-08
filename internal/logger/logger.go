package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	globalLogger *slog.Logger
	globalWriter *rotatingWriter
)

type rotatingWriter struct {
	mu     sync.Mutex
	dir    string
	now    func() time.Time
	date   string
	file   *os.File
	closed bool
}

func newRotatingWriter(dir string, now func() time.Time) *rotatingWriter {
	return &rotatingWriter{dir: dir, now: now}
}

func (w *rotatingWriter) ensureFileLocked() error {
	if w.closed {
		return os.ErrClosed
	}
	date := w.now().Format("2006-01-02")
	if w.file != nil && w.date == date {
		return nil
	}
	if w.file != nil {
		old := w.file
		w.file = nil
		w.date = ""
		if err := old.Close(); err != nil {
			return fmt.Errorf("关闭旧日志文件失败: %w", err)
		}
	}
	path := filepath.Join(w.dir, "token-usage-"+date+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	w.file = f
	w.date = date
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureFileLocked(); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func Init(level, dir string, maxDays int) (*slog.Logger, error) {
	if globalWriter != nil {
		_ = globalWriter.Close()
		globalWriter = nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}

	cleanup(dir, maxDays)

	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	rw := newRotatingWriter(dir, time.Now)
	rw.mu.Lock()
	err := rw.ensureFileLocked()
	rw.mu.Unlock()
	if err != nil {
		return nil, err
	}
	globalWriter = rw

	handler := slog.NewTextHandler(rw, &slog.HandlerOptions{Level: slogLevel})
	globalLogger = slog.New(handler)
	return globalLogger, nil
}

func Close() {
	if globalWriter != nil {
		_ = globalWriter.Close()
		globalWriter = nil
	}
	globalLogger = nil
}

func GetLogger() *slog.Logger {
	if globalLogger == nil {
		return slog.Default()
	}
	return globalLogger
}

func cleanup(dir string, maxDays int) {
	if maxDays <= 0 {
		return
	}

	pattern := filepath.Join(dir, "token-usage-*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	cutoff := time.Now().AddDate(0, 0, -maxDays)

	for _, path := range matches {
		base := filepath.Base(path)
		dateStr := strings.TrimPrefix(base, "token-usage-")
		dateStr = strings.TrimSuffix(dateStr, ".log")

		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		if fileDate.Before(cutoff) {
			os.Remove(path)
		}
	}
}

func GetLogFiles(dir string) ([]string, error) {
	pattern := filepath.Join(dir, "token-usage-*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i] > matches[j]
	})
	return matches, nil
}
