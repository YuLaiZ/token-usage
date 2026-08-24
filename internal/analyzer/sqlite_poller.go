// internal/analyzer/sqlite_poller.go
package analyzer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// SQLitePoller 定时轮询 SQLite 数据库变化。
// request 是固定采集请求，由构造期确定（client 源 Incremental；router 源 Source=router），
// mtime 仅做变化检测——检测到变化后原样上报 request，不再将 mtime 转日期。
type SQLitePoller struct {
	dbPath          string
	clientName      string
	request         collector.CollectRequest
	interval        time.Duration
	submit          MonitorSubmitFunc // 向 Analyzer 上报请求（不直接持有 ExecuteFunc）
	logger          *slog.Logger
	lastFingerprint string
	expandGlob      bool
	globDir         string
	globPattern     string
	signalReady     func() // Analyzer 预置的就绪回调（记录初始 mtime 后调用恰好一次）
	readyOnce       sync.Once
	stopOnce        sync.Once
	stopCh          chan struct{}
}

// NewSQLitePoller 创建 SQLite 轮询器。
// submit 是 Analyzer 注入的 MonitorSubmitFunc：poller 只产生 (client, req) 并调用它，
// 不直接持有 ExecuteFunc（采集执行与 gate/mutex 由 Analyzer.Submit 统一负责）。
// request 携带固定采集语义（Incremental / Source=router），运行期原样透传给 submit。
// signalReady 由 Analyzer 在注册时设置（每个 monitor 一个，sync.Once 保护），
// 本构造函数不接收测试专用 onReady 参数。
func NewSQLitePoller(dbPath, clientName string, request collector.CollectRequest,
	interval time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) *SQLitePoller {
	return newSQLitePoller(dbPath, clientName, request, interval, submit, logger, false)
}

func newSQLiteGlobPoller(dbPattern, clientName string, request collector.CollectRequest,
	interval time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) *SQLitePoller {
	return newSQLitePoller(dbPattern, clientName, request, interval, submit, logger, true)
}

func newSQLiteDirGlobPoller(dir, pattern, clientName string, request collector.CollectRequest,
	interval time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) *SQLitePoller {
	p := newSQLitePoller(dir, clientName, request, interval, submit, logger, false)
	p.globDir = dir
	p.globPattern = pattern
	return p
}

func newSQLitePoller(dbPath, clientName string, request collector.CollectRequest,
	interval time.Duration, submit MonitorSubmitFunc, logger *slog.Logger, expandGlob bool) *SQLitePoller {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p := &SQLitePoller{
		dbPath:     dbPath,
		clientName: clientName,
		request:    request,
		interval:   interval,
		submit:     submit,
		logger:     logger.With("component", "sqlite_poller", "client", clientName),
		expandGlob: expandGlob,
		stopCh:     make(chan struct{}),
	}
	return p
}

// Run 启动轮询
func (p *SQLitePoller) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// 初始记录完整文件集合指纹。dbPath 可以是精确路径，也可以是 Codex 的
	// state_*.sqlite glob；后者每次 tick 都重新展开，覆盖运行后新增的 state DB。
	p.lastFingerprint, _ = p.fingerprint()
	p.logger.Info("starting poller", "db", p.dbPath, "interval", p.interval)

	// 就绪信号：记录初始 mtime 后恰好一次。readyOnce 保证重复调用不重复扣减
	// Analyzer 的 readyWg（不重复 close readyCh / panic）。
	p.readyOnce.Do(func() {
		if p.signalReady != nil {
			p.signalReady()
		}
	})

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("poller stopped")
			return
		case <-p.stopCh:
			p.logger.Info("poller stopped")
			return
		case <-ticker.C:
			fingerprint, present := p.fingerprint()
			if fingerprint != p.lastFingerprint {
				p.lastFingerprint = fingerprint
				if !present {
					// 文件删除只更新基线；等同一路径重新出现时再触发采集。
					continue
				}
				// 文件集合指纹只做变化检测：检测到变化后原样上报构造期 request，
				// 增量/路由语义由 request 携带，sync cursor 由 collector 内部维护。
				// 变化触发是活跃源的每个 tick 必然事件，属预期心跳，降 Debug 保留排查轨迹。
				p.logger.Debug("database changed, triggering collection", "request", p.request)
				p.submit(p.clientName, p.request)
			}
		}
	}
}

// Stop 停止轮询（可安全多次调用）
func (p *SQLitePoller) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

func (p *SQLitePoller) fingerprint() (string, bool) {
	if p.globDir != "" {
		return getSQLiteDirectoryFingerprint(p.globDir, p.globPattern)
	}
	return getSQLiteFingerprintMode(p.dbPath, p.expandGlob)
}

// GetSQLiteMtime 获取 SQLite 文件的有效 mtime
// WAL 模式下需要同时检查 -wal 文件
func GetSQLiteMtime(dbPath string) int64 {
	var latest int64
	for _, candidate := range []string{dbPath, dbPath + "-wal"} {
		if mtime := getFileMtime(candidate); mtime > latest {
			latest = mtime
		}
	}
	return latest
}

// getSQLiteFingerprint 返回当前匹配文件集合（含各自 -wal）的稳定元数据指纹。
// 与单纯比较 “mtime 变大” 相比，它能识别：
//   - 文件删除后重新创建；
//   - 原子替换成 mtime 更早的文件；
//   - glob 下运行期新增/删除 state DB。
//
// 不读取数据库内容，避免轮询大型 DB 的额外 IO。精确路径额外纳入父目录 mtime，
// 从而识别相同大小和 mtime 的原子替换，同时避免 FileInfo.Sys() 中访问时间变化
// 反过来让采集读取持续触发轮询。
func getSQLiteFingerprint(dbPath string) (fingerprint string, present bool) {
	return getSQLiteFingerprintMode(dbPath, strings.ContainsAny(dbPath, "*?["))
}

func getSQLiteFingerprintMode(dbPath string, expandGlob bool) (fingerprint string, present bool) {
	paths := []string{dbPath}
	if expandGlob {
		paths = expandSQLitePaths(dbPath)
	}
	return getSQLitePathsFingerprint(paths, nil)
}

func getSQLiteDirectoryFingerprint(dir, pattern string) (fingerprint string, present bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		matched, matchErr := filepath.Match(pattern, entry.Name())
		if matchErr != nil {
			return "", false
		}
		if matched && !entry.IsDir() {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return getSQLitePathsFingerprint(paths, []string{dir})
}

func getSQLitePathsFingerprint(paths, extraDirs []string) (fingerprint string, present bool) {
	var b strings.Builder
	parentDirs := make(map[string]struct{})
	for _, dir := range extraDirs {
		parentDirs[dir] = struct{}{}
	}
	for _, path := range paths {
		parentDirs[filepath.Dir(path)] = struct{}{}
		for _, candidate := range []string{path, path + "-wal"} {
			info, err := os.Stat(candidate)
			if err != nil {
				continue
			}
			present = true
			_, _ = fmt.Fprintf(
				&b,
				"%q:%d:%d:%s\n",
				candidate,
				info.ModTime().UnixNano(),
				info.Size(),
				info.Mode(),
			)
		}
	}
	dirs := make([]string, 0, len(parentDirs))
	for dir := range parentDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil {
			_, _ = fmt.Fprintf(&b, "dir:%q:%d:%s\n", dir, info.ModTime().UnixNano(), info.Mode())
		}
	}
	return b.String(), present
}

func expandSQLitePaths(dbPath string) []string {
	if !strings.ContainsAny(dbPath, "*?[") {
		return []string{dbPath}
	}
	paths, err := filepath.Glob(dbPath)
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	return paths
}

// getFileMtime 获取文件修改时间
func getFileMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}
