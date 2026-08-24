// internal/analyzer/jsonl_watcher.go
package analyzer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// JSONLWatcher 监控 JSONL 文件目录变化
type JSONLWatcher struct {
	dirs          []string
	clientName    string
	debounce      *Debounce
	submit        MonitorSubmitFunc // 向 Analyzer 上报请求（不直接持有 ExecuteFunc）
	signalReady   func()            // Analyzer 预置的就绪回调（Walk+watch 注册后调用恰好一次）
	signalFailure func(error)       // 初始化失败回调；与 signalReady 共享一次性计数
	logger        *slog.Logger
	watcher       *fsnotify.Watcher
	watched       map[string]struct{} // 仅由 Run goroutine 访问
	readyOnce     sync.Once           // 保证 signalReady 恰好一次（重复 signal 不重复扣减）
	lifecycleMu   sync.Mutex
	started       bool
	stopOnce      sync.Once
	stopCh        chan struct{}
}

// NewJSONLWatcher 创建 JSONL 监控器。
// submit 是 Analyzer 注入的 MonitorSubmitFunc：watcher 只产生 (client, req) 并调用它，
// 不直接持有 ExecuteFunc（采集执行与 gate/mutex 由 Analyzer.Submit 统一负责）。
// debounce 回调固定把触发文件绝对路径作为 ChangedFile 传给 submit，不再将 mtime 转日期——
// 日期采集已由 collector 内部处理。
// signalReady 由 Analyzer 在注册时设置（每个 monitor 一个，sync.Once 保护），
// 本构造函数不接收测试专用 onReady 参数。
func NewJSONLWatcher(dirs []string, clientName string, debounceDuration time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) (*JSONLWatcher, error) {
	if len(dirs) == 0 {
		return nil, fmt.Errorf("监控目录不能为空")
	}
	if submit == nil {
		return nil, fmt.Errorf("monitor submit callback 不能为空")
	}
	normalized := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("监控目录不能为空")
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("解析监控目录 %q 失败: %w", dir, err)
		}
		abs = filepath.Clean(abs)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		normalized = append(normalized, abs)
	}

	w := &JSONLWatcher{
		dirs:       normalized,
		clientName: clientName,
		submit:     submit,
		logger:     logger,
		watched:    make(map[string]struct{}),
		stopCh:     make(chan struct{}),
	}

	if w.logger == nil {
		w.logger = slog.Default()
	}
	w.logger = w.logger.With("component", "jsonl_watcher", "client", w.clientName)

	// 构造期创建 fsnotify watcher：失败立即返回 error（而非 Run 内静默 return），
	// 使 setupFromConfig 能跳过该客户端，Analyzer.Run 在无任何存活监控时返回 error
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建 fsnotify watcher 失败: %w", err)
	}
	w.watcher = watcher

	// 创建 debounce，回调触发上报。
	// key 是变化的文件绝对路径，直接作为 ChangedFile 传递。
	w.debounce = NewDebounce(debounceDuration, func(key string) {
		w.logger.Info("debounce triggered", "file", key)
		w.submit(w.clientName, collector.CollectRequest{ChangedFile: key})
	})

	return w, nil
}

// Run 启动监控
func (w *JSONLWatcher) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.lifecycleMu.Lock()
	select {
	case <-w.stopCh:
		w.lifecycleMu.Unlock()
		w.signalReadyOnce()
		return nil
	default:
		w.started = true
	}
	w.lifecycleMu.Unlock()
	defer w.watcher.Close()

	// 递归注册现存目标目录。目标尚不存在时，注册最近存在的父目录，待客户端首次
	// 创建目标路径后动态接管；这兼容默认启用但尚未安装的客户端，同时不伪造监听能力。
	if err := w.registerRoots(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			w.signalReadyOnce()
			return nil
		}
		w.readyOnce.Do(func() {
			if w.signalFailure != nil {
				w.signalFailure(err)
			}
		})
		return err
	}

	// 就绪信号：Walk + watch 注册完成后恰好一次。readyOnce 保证重复调用不重复扣减
	// Analyzer 的 readyWg（不重复 close readyCh / panic）。
	w.signalReadyOnce()

	// 事件循环
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("watcher stopped")
			return nil
		case <-w.stopCh:
			w.logger.Info("watcher stopped")
			return nil
		case event, ok := <-w.watcher.Events:
			if !ok {
				if w.stopping(ctx) {
					return nil
				}
				return fmt.Errorf("fsnotify event channel 意外关闭")
			}
			// 新建相关目录时动态递归 Add（包括原先不存在的目标路径逐级创建）。
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if addErr := w.registerCreatedDir(ctx, event.Name); addErr != nil {
						if w.stopping(ctx) {
							return nil
						}
						return fmt.Errorf("动态注册目录 %s 失败: %w", event.Name, addErr)
					}
					// 运行期新目录接管是低频事件，单条保留排查线索。
					w.logger.Debug("watching new directory", "dir", event.Name)
					continue
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				w.removeWatchedTree(event.Name)
				if err := w.registerRoots(ctx); err != nil {
					if w.stopping(ctx) {
						return nil
					}
					return fmt.Errorf("监控目录变更后恢复监听失败: %w", err)
				}
			}
			// 只处理 Create 和 Write 事件
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			// 只处理 .jsonl 文件
			if filepath.Ext(event.Name) != ".jsonl" {
				continue
			}
			if !w.isWithinTarget(event.Name) {
				continue
			}
			w.logger.Debug("file event", "op", event.Op, "file", event.Name)
			w.debounce.Trigger(event.Name)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				if w.stopping(ctx) {
					return nil
				}
				return fmt.Errorf("fsnotify error channel 意外关闭")
			}
			return fmt.Errorf("fsnotify watcher error: %w", err)
		}
	}
}

func (w *JSONLWatcher) signalReadyOnce() {
	w.readyOnce.Do(func() {
		if w.signalReady != nil {
			w.signalReady()
		}
	})
}

func (w *JSONLWatcher) registerRoots(ctx context.Context) error {
	for _, root := range w.dirs {
		if err := w.checkRunning(ctx); err != nil {
			return err
		}
		if err := w.registerRoot(ctx, root); err != nil {
			return fmt.Errorf("注册监控根目录 %s: %w", root, err)
		}
	}
	// 注册结果是启动必经的批量预期路径，逐目录打印只会刷屏；数量才是有效信息。
	w.logger.Debug("watching directories", "count", len(w.watched))
	return nil
}

func (w *JSONLWatcher) registerRoot(ctx context.Context, root string) error {
	if err := w.checkRunning(ctx); err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("路径不是目录")
		}
		// 同时监控父目录，以便目标根目录被删除后可以观察其重建。
		if parent := filepath.Dir(root); parent != root {
			if err := w.addWatch(parent); err != nil {
				return err
			}
		}
		return w.addTree(ctx, root)
	}
	if !os.IsNotExist(err) {
		return err
	}

	ancestor, err := nearestExistingDir(root)
	if err != nil {
		return err
	}
	return w.addWatch(ancestor)
}

func nearestExistingDir(path string) (string, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("最近存在的父路径 %s 不是目录", current)
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("找不到可监听的已存在父目录: %s", path)
		}
	}
}

func (w *JSONLWatcher) addTree(ctx context.Context, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if err := w.checkRunning(ctx); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() && w.isRelevantDir(path) {
			if err := w.addWatch(path); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *JSONLWatcher) addWatch(path string) error {
	path = filepath.Clean(path)
	if _, ok := w.watched[path]; ok {
		return nil
	}
	if err := w.watcher.Add(path); err != nil {
		return fmt.Errorf("监听目录 %s: %w", path, err)
	}
	w.watched[path] = struct{}{}
	return nil
}

func (w *JSONLWatcher) registerCreatedDir(ctx context.Context, path string) error {
	path = filepath.Clean(path)
	var errs []error
	for _, root := range w.dirs {
		if err := w.checkRunning(ctx); err != nil {
			return err
		}
		switch {
		case pathWithin(root, path):
			if err := w.addTree(ctx, path); err != nil {
				errs = append(errs, err)
			}
		case pathWithin(path, root):
			if err := w.registerRoot(ctx, root); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (w *JSONLWatcher) checkRunning(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-w.stopCh:
		return context.Canceled
	default:
		return nil
	}
}

// removeWatchedTree 清除 path 及其所有子目录的本地 watch 记录。
// 目录被 rename/remove 后，fsnotify 可能一次性撤销整棵子树的内核 watch；
// 若只删除事件路径本身，重建目录时 addTree 会被陈旧 map 误判为已经注册。
func (w *JSONLWatcher) removeWatchedTree(path string) {
	path = filepath.Clean(path)
	for watched := range w.watched {
		if pathWithin(path, watched) {
			delete(w.watched, watched)
		}
	}
}

func (w *JSONLWatcher) isRelevantDir(path string) bool {
	for _, root := range w.dirs {
		if pathWithin(root, path) || pathWithin(path, root) {
			return true
		}
	}
	return false
}

func (w *JSONLWatcher) isWithinTarget(path string) bool {
	for _, root := range w.dirs {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

// pathWithin 判断 path 是否等于 root 或位于 root 之下，避免字符串前缀把
// /tmp/source-other 错判为 /tmp/source 的子路径。
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func (w *JSONLWatcher) stopping(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-w.stopCh:
		return true
	default:
		return false
	}
}

// Stop 停止监控（可安全多次调用）
// 顺序：先 close(stopCh) 停掉事件循环，再 debounce.Stop drain。
// 若先 debounce.Stop（清空 pending + 等回调），其等待期间事件循环仍在运行，新事件会 Trigger
// 重新 arm timer，产生 debounce.Stop 之后的 late fire——这些 fire 不被 debounce.wg 跟踪，
// 可能延迟到 Run 返回、usageDB 被外层 cleanup 关闭后才触发写库。先停事件源可杜绝 late fire。
func (w *JSONLWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.debounce.Stop()
		w.lifecycleMu.Lock()
		started := w.started
		w.lifecycleMu.Unlock()
		if !started {
			_ = w.watcher.Close()
		}
	})
}
