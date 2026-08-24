package update

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// updatelog.go 实现升级日志的路径解析、append 打开、7 天保留与 nil 安全的 stepLogger。
//
// 升级日志记录每次 update 的关键步骤（来源校验、下载、安装、helper 替换等），
// 供用户在升级失败后诊断。与 daemon 日志（token-usage-*.log）同目录但用独立前缀
// update- 区别，日期命名（update-YYYY-MM-DD.log），append 写入，权限 0600。
//
// 设计原则：
//   - 日志 sink 经字段注入（nil=静默），保持单测可注入 buffer 断言行内容；
//   - 不引入结构化日志框架，用 fmt.Fprintln 带时间戳足够；
//   - 保留策略镜像 logger.cleanup 的日期解析删除模式，前缀独立不影响 daemon 日志。

// updateLogRetentionDays 是升级日志的保留天数。超过此天数的 update-*.log 文件
// 在 Apply 期被 best-effort 清理。
const updateLogRetentionDays = 7

// updateLogFilePrefix 是升级日志文件名前缀，区别于 daemon 的 token-usage- 前缀。
const updateLogFilePrefix = "update-"

// ResolveUpdateLogDir 解析升级日志目录：home/.token-usage/logs（与 daemon 日志同目录）。
func ResolveUpdateLogDir(home string) string {
	return filepath.Join(home, ".token-usage", "logs")
}

// resolveUpdateLogPath 解析指定日期的升级日志文件路径。
// 用于 CLI 工厂打开文件与 Windows installer 重定向 helper stderr。
func resolveUpdateLogPath(dir string, now time.Time) string {
	return filepath.Join(dir, updateLogFilePrefix+now.Format("2006-01-02")+".log")
}

// OpenUpdateLogFile 以 append 方式打开（或创建）升级日志文件，权限 0600。
// 目录不存在时先创建（0755）。返回打开的文件、完整路径与可能的错误。
// 供 CLI 工厂（打开 LogSink）和 Windows spawnUpdateHelper（重定向 helper stderr）复用。
func OpenUpdateLogFile(dir string, now time.Time) (*os.File, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("%s: %w", ui.Bi("failed to create update log directory", "创建升级日志目录失败"), err)
	}
	path := resolveUpdateLogPath(dir, now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", ui.Bi("failed to open update log file", "打开升级日志文件失败"), err)
	}
	return f, path, nil
}

// retainUpdateLogs 清理 dir 下超过 maxDays 天的 update-*.log 文件（best-effort）。
// 镜像 logger.cleanup 的日期解析模式：glob → 解析 2006-01-02 → Before(cutoff) 删。
// 前缀独立为 update-，不影响 daemon 的 token-usage-*.log。
// 目录不存在或清理失败不阻塞升级。
func retainUpdateLogs(dir string, maxDays int, now time.Time) {
	if maxDays <= 0 {
		return
	}
	pattern := filepath.Join(dir, updateLogFilePrefix+"*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -maxDays)
	for _, path := range matches {
		base := filepath.Base(path)
		dateStr := strings.TrimPrefix(base, updateLogFilePrefix)
		dateStr = strings.TrimSuffix(dateStr, ".log")
		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if fileDate.Before(cutoff) {
			_ = removeRegularFile(path)
		}
	}
}

// stepLogger 是 nil 安全的步骤日志器：每行以 RFC3339 时间戳 + [prefix] 开头。
// writer 为 nil 时所有方法静默 no-op。便于生产注入文件 writer、测试注入 buffer。
type stepLogger struct {
	writer io.Writer
	prefix string
	now    func() time.Time
}

// newStepLogger 构造步骤日志器。writer 为 nil 表示静默。now 为 nil 时用 time.Now。
func newStepLogger(writer io.Writer, prefix string, now func() time.Time) *stepLogger {
	if now == nil {
		now = time.Now
	}
	return &stepLogger{writer: writer, prefix: prefix, now: now}
}

// step 写入一行日志：RFC3339 时间戳 + [prefix] + message。
// writer 为 nil 时静默 no-op。
func (l *stepLogger) step(format string, args ...any) {
	if l == nil || l.writer == nil {
		return
	}
	ts := l.now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.writer, "%s [%s] %s\n", ts, l.prefix, msg)
}
