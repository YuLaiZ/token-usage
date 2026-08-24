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

// FallbackLogFileName 是 daemon 兜底输出文件名（logs/ 目录内固定 bootstrap 文件，
// 不随日期变化，见 control/service 侧的使用注释）。service 与 control 侧引用本常量
// 保持单一真相源。
const FallbackLogFileName = "daemon-fallback.log"

// afterFuncTimer 是日界定时器的最小抽象（time.AfterFunc 返回 *time.Timer 满足之），
// 包内可注入假实现使「跨午夜 + timer 触发」的单测可确定执行。
type afterFuncTimer interface{ Stop() bool }

type rotatingWriter struct {
	mu     sync.Mutex
	dir    string
	now    func() time.Time
	date   string
	file   *os.File
	closed bool

	// mirror 状态（全部由 mu 保护）：daemon 启动后把 fd 1/2 接管到当日日志文件，
	// 使 panic 等 runtime 直接写 stderr 的输出并入结构化日志。
	mirrorStd bool
	dayTimer  afterFuncTimer
	// afterFunc 为 nil 时不调度日界 timer（测试可注入受控实现）。
	afterFunc func(d time.Duration, f func()) afterFuncTimer
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
	// 文件切换后接管目标同步更新（仅 mirror 生效时；平台 no-op 实现见 stdfd_*.go）。
	w.remirrorStdFDsLocked()
	return nil
}

// scheduleNextDayBoundaryLocked 计算并注册下一个本地午夜。使用 time.Date 的
// day+1（溢出自动进位、DST 安全），禁止按 24h 偏移。timer 回调无论切换成败
// 都会重新调度（见 onDayBoundary），保证长期运行每个午夜都触发，兑现
// 「空闲跨午夜后 panic 仍落当天文件」。
func (w *rotatingWriter) scheduleNextDayBoundaryLocked() {
	if w.afterFunc == nil || !w.mirrorStd {
		return
	}
	now := w.now()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	d := next.Sub(now)
	if d <= 0 {
		d = time.Second
	}
	if w.dayTimer != nil {
		w.dayTimer.Stop()
	}
	w.dayTimer = w.afterFunc(d, w.onDayBoundary)
}

func (w *rotatingWriter) onDayBoundary() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.mirrorStd {
		return
	}
	// 日期已跨午夜：ensureFileLocked 切换到新当日文件并重做接管；
	// 失败也继续重排下一个午夜，下一个周期再次尝试。
	_ = w.ensureFileLocked()
	w.scheduleNextDayBoundaryLocked()
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
	if w.dayTimer != nil {
		w.dayTimer.Stop()
		w.dayTimer = nil
	}
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
	rw.afterFunc = func(d time.Duration, f func()) afterFuncTimer { return time.AfterFunc(d, f) }
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

	// fallback 文件无日期文件名，按 mtime 判断超龄删除。容量治理分层：
	//   - 本处 mtime 清理只覆盖「连续超过 max_days 未写入」的闲置遗留文件；
	//     活跃写入（mtime 持续更新）不会被本处删除。
	//   - 活跃增长的容量上限由 control 在每次 spawn 前按大小轮转兜住
	//     （start/update 启动时无进程持有该文件句柄，rename 跨平台可靠）；
	//     macOS 运行期崩溃输出经 fd 接管进当日结构化文件（受 max_days 治理），
	//     不经过 fallback。
	// unix 上若本进程 fd 1/2 仍指向上次运行遗留的超旧文件，删除后后续写入进入
	// 已 unlink 文件——仅丢失极早期兜底输出，MirrorStdOutput 接管后不再经过该
	// fd，可接受。
	fallback := filepath.Join(dir, FallbackLogFileName)
	if info, err := os.Stat(fallback); err == nil && info.ModTime().Before(cutoff) {
		os.Remove(fallback)
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
