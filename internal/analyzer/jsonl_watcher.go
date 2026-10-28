// internal/analyzer/jsonl_watcher.go
package analyzer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// JSONLWatcher 监控 JSONL 文件目录变化
type JSONLWatcher struct {
	dirs []string
	// optional 标记 dirs 中由调用方声明的可选根（Clean 后路径集合）：注册失败
	// 降级为锚定最近存在目录祖先并 Warn，不拖停强根监控；强根失败仍使 Run 返回
	// error。自动附加的监控面（如 cc-switch 账号根）属此类。
	optional      map[string]struct{}
	clientName    string
	debounce      *Debounce
	submit        MonitorSubmitFunc // 向 Analyzer 上报请求（不直接持有 ExecuteFunc）
	signalReady   func()            // Analyzer 预置的就绪回调（Walk+watch 注册后调用恰好一次）
	signalFailure func(error)       // 初始化失败回调；与 signalReady 共享一次性计数
	logger        *slog.Logger
	watcher       *fsnotify.Watcher
	watched       map[string]struct{} // 仅由 Run goroutine 访问
	// deferred 记录降级后待重试接管的目录（可选根或其内部子树，仅 Run goroutine
	// 访问）：注册/接管失败时进入，retryTicker 周期重试 takeoverDir，成功后移除
	// 并补采——覆盖不产生文件事件的恢复形态（如权限修复：父目录 watch 收不到
	// 未监听子树的属性变化，重建事件链路触达不了）。
	deferred       map[string]struct{}
	retryInterval  time.Duration    // deferred 非空时的重试周期；测试可注入短值
	afterWatchHook func(dir string) // 测试 seam：takeoverDir 注册监听后、枚举子项前回调；生产为 nil
	readyOnce      sync.Once        // 保证 signalReady 恰好一次（重复 signal 不重复扣减）
	lifecycleMu    sync.Mutex
	started        bool
	stopOnce       sync.Once
	stopCh         chan struct{}
}

// NewJSONLWatcher 创建 JSONL 监控器。
// dirs 是强根：任一注册失败使 Run 返回 error。optionalRoots 是可选根（自动附加
// 的监控面）：注册失败降级锚定最近存在的目录祖先并 Warn，保留强根监控与后续
// 经 Create/Remove 事件恢复接管的途径；与强根重复时按强根语义对待。
// submit 是 Analyzer 注入的 MonitorSubmitFunc：watcher 只产生 (client, req) 并调用它，
// 不直接持有 ExecuteFunc（采集执行与 gate/mutex 由 Analyzer.Submit 统一负责）。
// debounce 回调固定把触发文件绝对路径作为 ChangedFile 传给 submit，不再将 mtime 转日期——
// 日期采集已由 collector 内部处理。
// signalReady 由 Analyzer 在注册时设置（每个 monitor 一个，sync.Once 保护），
// 本构造函数不接收测试专用 onReady 参数。
func NewJSONLWatcher(dirs []string, optionalRoots []string, clientName string, debounceDuration time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) (*JSONLWatcher, error) {
	if len(dirs) == 0 {
		return nil, errors.New(ui.Bi("watch directory must not be empty", "监控目录不能为空"))
	}
	if submit == nil {
		return nil, errors.New(ui.Bi("monitor submit callback must not be nil", "monitor submit callback 不能为 nil"))
	}
	normalized := make([]string, 0, len(dirs)+len(optionalRoots))
	seen := make(map[string]struct{}, len(dirs)+len(optionalRoots))
	normalize := func(dir string) (string, error) {
		if strings.TrimSpace(dir) == "" {
			return "", errors.New(ui.Bi("watch directory must not be empty", "监控目录不能为空"))
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("failed to resolve watch directory %q", dir), fmt.Sprintf("解析监控目录 %q 失败", dir)), err)
		}
		return filepath.Clean(abs), nil
	}
	for _, dir := range dirs {
		abs, err := normalize(dir)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		normalized = append(normalized, abs)
	}
	// 可选根与强根重复时按强根语义对待（不进 optional 降级集合）。
	optional := make(map[string]struct{}, len(optionalRoots))
	for _, dir := range optionalRoots {
		abs, err := normalize(dir)
		if err != nil {
			return nil, err
		}
		if _, isStrong := seen[abs]; isStrong {
			continue
		}
		seen[abs] = struct{}{}
		normalized = append(normalized, abs)
		optional[abs] = struct{}{}
	}

	w := &JSONLWatcher{
		dirs:          normalized,
		optional:      optional,
		clientName:    clientName,
		submit:        submit,
		logger:        logger,
		watched:       make(map[string]struct{}),
		deferred:      make(map[string]struct{}),
		retryInterval: 10 * time.Second,
		stopCh:        make(chan struct{}),
	}

	if w.logger == nil {
		w.logger = slog.Default()
	}
	w.logger = w.logger.With("component", "jsonl_watcher", "client", w.clientName)
	if w.retryInterval <= 0 {
		// NewTicker 对非正值 panic；非正值一律回落默认节拍。
		w.retryInterval = 10 * time.Second
	}

	// 构造期创建 fsnotify watcher：失败立即返回 error（而非 Run 内静默 return），
	// 使 setupFromConfig 能跳过该客户端，Analyzer.Run 在无任何存活监控时返回 error
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create fsnotify watcher", "创建 fsnotify watcher 失败"), err)
	}
	w.watcher = watcher

	// 创建 debounce，回调触发上报。
	// key 是变化的文件绝对路径，直接作为 ChangedFile 传递。
	w.debounce = NewDebounce(debounceDuration, func(key string) {
		// debounce 触发是活跃会话的常态心跳，属预期行为，降 Debug 保留排查轨迹。
		w.logger.Debug("debounce triggered", "file", key)
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
	// 首扫不补采存量文件（startup catch-up 负责现存 JSONL 的采集）。
	if err := w.registerRoots(ctx, false); err != nil {
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

	// 降级根重试节拍：deferred 为空时 tick 空转（一次 select 分支的开销）。
	retryTicker := time.NewTicker(w.retryInterval)
	defer retryTicker.Stop()

	// 事件循环
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("watcher stopped")
			return nil
		case <-w.stopCh:
			w.logger.Info("watcher stopped")
			return nil
		case <-retryTicker.C:
			w.retryDeferred(ctx)
		case event, ok := <-w.watcher.Events:
			if !ok {
				if w.stopping(ctx) {
					return nil
				}
				return errors.New(ui.Bi("fsnotify event channel closed unexpectedly", "fsnotify event channel 意外关闭"))
			}
			// 新建相关目录时动态递归 Add（包括原先不存在的目标路径逐级创建）。
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if addErr := w.registerCreatedDir(ctx, event.Name); addErr != nil {
						if w.stopping(ctx) {
							return nil
						}
						return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("failed to register directory %s dynamically", event.Name), fmt.Sprintf("动态注册目录 %s 失败", event.Name)), addErr)
					}
					// 运行期新目录接管是低频事件，单条保留排查线索。
					w.logger.Debug("watching new directory", "dir", event.Name)
					continue
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// 只有被 watch 的目录树确有条目被移除才需要恢复监听；普通
				// 文件删除与 watch 无关（watch 挂在目录上），跳过全量重注册
				// ——会话文件删除/轮转是高频事件，逐次重注册加汇总日志会刷屏。
				if w.removeWatchedTree(event.Name) {
					// 运行期恢复接管：目录树删除后重建的场景，重建树内已写入
					// 的文件不产生新事件，恢复时补采（幂等由采集链保证）。
					if err := w.registerRoots(ctx, true); err != nil {
						if w.stopping(ctx) {
							return nil
						}
						return fmt.Errorf("%s: %w", ui.Bi("failed to restore watching after monitored directory changed", "监控目录变更后恢复监听失败"), err)
					}
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
				return errors.New(ui.Bi("fsnotify error channel closed unexpectedly", "fsnotify error channel 意外关闭"))
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

func (w *JSONLWatcher) registerRoots(ctx context.Context, collectExisting bool) error {
	for _, root := range w.dirs {
		if err := w.checkRunning(ctx); err != nil {
			return err
		}
		if _, optional := w.optional[root]; optional {
			if err := w.registerOptionalRoot(ctx, root, collectExisting); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("register watch root %s", root), fmt.Sprintf("注册监控根目录 %s", root)), err)
			}
			continue
		}
		if _, err := w.registerRoot(ctx, root, collectExisting); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("register watch root %s", root), fmt.Sprintf("注册监控根目录 %s", root)), err)
		}
	}
	// 注册结果是启动必经的批量预期路径，逐目录打印只会刷屏；数量才是有效信息。
	w.logger.Debug("watching directories", "count", len(w.watched))
	return nil
}

// registerRoot 注册单个强根，返回 (失败子树列表, error)。失败子树是渐进接管中
// addWatch/ReadDir 失败的最小目录（可选根据此收敛 deferred 范围）；根不存在走
// 锚定路径时无失败子树。强根调用方只消费 error。
func (w *JSONLWatcher) registerRoot(ctx context.Context, root string, collectExisting bool) ([]string, error) {
	if err := w.checkRunning(ctx); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return nil, errors.New(ui.Bi("path is not a directory", "路径不是目录"))
		}
		// 同时监控父目录，以便目标根目录被删除后可以观察其重建。
		if parent := filepath.Dir(root); parent != root {
			if err := w.addWatch(parent); err != nil {
				return nil, err
			}
		}
		existing, failed, err := w.takeoverDir(ctx, root, collectExisting)
		if collectExisting {
			for _, f := range existing {
				w.debounce.Trigger(f)
			}
		}
		return failed, err
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	ancestor, err := nearestExistingDir(root)
	if err != nil {
		return nil, err
	}
	return nil, w.addWatch(ancestor)
}

// registerOptionalRoot 注册可选根：注册失败（同名文件占用、树不可读等）降级为
// Warn + 失败子树记入 deferred（retryTicker 周期重试，成功后补采——覆盖权限修复
// 等不产生文件事件的恢复形态；**只记最小失败子树**，已成功接管的健康部分完成一次
// 补采后即退出重试范围，故障持续期间不得周期性重复提交健康兄弟，R8 实证）+
// 尽力锚定最近存在的目录祖先（锚点上的 Create/Remove 事件经 registerCreatedDir /
// removeWatchedTree 链路即时恢复，ticker 是兜底），不拖停强根监控。
func (w *JSONLWatcher) registerOptionalRoot(ctx context.Context, root string, collectExisting bool) error {
	failed, err := w.registerRoot(ctx, root, collectExisting)
	if err == nil {
		delete(w.deferred, root)
		return nil
	}
	w.logger.Warn("optional watch root unavailable, anchored and deferred for retry",
		"root", root, "error", err)
	if len(failed) > 0 {
		for _, f := range failed {
			w.deferred[f] = struct{}{}
		}
	} else {
		// 失败点不在树内（根为同名文件等）：整根入 deferred。
		w.deferred[root] = struct{}{}
	}
	// loose：非目录条目（如占用路径的同名文件）也向上跳过，找到最近的目录祖先。
	ancestor, aerr := nearestExistingDirLoose(root)
	if aerr != nil {
		w.logger.Warn("optional watch root has no directory ancestor to anchor", "root", root, "error", aerr)
		return nil
	}
	if cerr := w.addWatch(ancestor); cerr != nil {
		w.logger.Warn("optional watch root ancestor anchor failed", "root", root, "ancestor", ancestor, "error", cerr)
	}
	return nil
}

// retryDeferred 重试降级子树的接管（仅监听注册与树内补采，不做周期全量读取）：
// 每个 deferred 项只覆盖其自身子树——健康兄弟不在范围内，不会周期性重复提交。
// 成功即移除并补采本次树内已有文件；仍失败则以本次的失败子树替换该项（范围只
// 收敛不扩大）。排序遍历保证日志与行为确定性。
func (w *JSONLWatcher) retryDeferred(ctx context.Context) {
	if len(w.deferred) == 0 {
		return
	}
	dirs := make([]string, 0, len(w.deferred))
	for d := range w.deferred {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		existing, failed, err := w.takeoverDir(ctx, d, true)
		for _, f := range existing {
			w.debounce.Trigger(f)
		}
		delete(w.deferred, d)
		if err != nil {
			if len(failed) > 0 {
				for _, f := range failed {
					w.deferred[f] = struct{}{}
				}
			} else {
				w.deferred[d] = struct{}{}
			}
			w.logger.Debug("deferred watch subtree retry failed", "root", d, "error", err)
			continue
		}
		w.logger.Info("deferred watch subtree recovered", "root", d)
	}
}

func nearestExistingDir(path string) (string, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", errors.New(ui.Bi(fmt.Sprintf("nearest existing parent path %s is not a directory", current), fmt.Sprintf("最近存在的父路径 %s 不是目录", current)))
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New(ui.Bi(fmt.Sprintf("no existing parent directory available to watch: %s", path), fmt.Sprintf("找不到可监听的已存在父目录: %s", path)))
		}
	}
}

// nearestExistingDirLoose 向上寻找最近存在的目录祖先，途经的非目录条目
// （如占用路径的同名文件）直接跳过；到根仍无目录可用时返回错误。
// 供可选根降级锚定使用。
func nearestExistingDirLoose(path string) (string, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New(ui.Bi(fmt.Sprintf("no existing directory ancestor available to anchor: %s", path), fmt.Sprintf("找不到可锚定的已存在目录祖先: %s", path)))
		}
	}
}

// takeoverDir 递归接管 root 子树，返回 (树内已有会话文件, 失败子树列表, error)。
// 顺序合同：每个目录**先注册监听、后枚举子项**——枚举后到达的写入有事件兜底、
// 枚举前的已有文件被 existing 补采覆盖，两者并集无窗口。不得换回 filepath.Walk：
// Walk 对目录是先读子项列表再调回调，回调里的 addWatch 晚于枚举，「枚举完成后、
// addWatch 前」的写入两头都漏（R6 实证）。单个子目录失败不阻断兄弟子树（渐进
// 接管），失败目录以最小子树形态记入 failed（调用方据此收敛重试范围），错误
// 聚合返回由调用方按强根/可选根语义处置；ctx 取消不带失败子树（非故障）。
func (w *JSONLWatcher) takeoverDir(ctx context.Context, dir string, collectExisting bool) ([]string, []string, error) {
	if err := w.checkRunning(ctx); err != nil {
		return nil, nil, err
	}
	if !w.isRelevantDir(dir) {
		return nil, nil, nil
	}
	if err := w.addWatch(dir); err != nil {
		return nil, []string{dir}, err
	}
	if w.afterWatchHook != nil {
		w.afterWatchHook(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{dir}, err
	}
	var existing []string
	var failed []string
	var errs error
	for _, e := range entries {
		if err := w.checkRunning(ctx); err != nil {
			return existing, nil, err
		}
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			sub, subFailed, err := w.takeoverDir(ctx, path, collectExisting)
			existing = append(existing, sub...)
			failed = append(failed, subFailed...)
			errs = errors.Join(errs, err)
			continue
		}
		if collectExisting && filepath.Ext(path) == ".jsonl" && w.isWithinTarget(path) {
			existing = append(existing, path)
		}
	}
	return existing, failed, errs
}

func (w *JSONLWatcher) addWatch(path string) error {
	path = filepath.Clean(path)
	if _, ok := w.watched[path]; ok {
		return nil
	}
	if err := w.watcher.Add(path); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("watch directory %s", path), fmt.Sprintf("监听目录 %s", path)), err)
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
		_, optional := w.optional[root]
		switch {
		case pathWithin(root, path):
			// 运行期接管的新目录树：注册前已写入的文件不会再产生事件，
			// 接管时补采（幂等由采集链保证）。可选根树内的接管失败不拖停
			// 强根：最小失败子树降级为 deferred（健康部分一次补采后退出
			// 重试范围，R8）。
			existing, failed, err := w.takeoverDir(ctx, path, true)
			for _, f := range existing {
				w.debounce.Trigger(f)
			}
			if err != nil {
				if optional {
					w.logger.Warn("optional root takeover failed, deferred for retry",
						"root", root, "path", path, "error", err)
					if len(failed) > 0 {
						for _, f := range failed {
							w.deferred[f] = struct{}{}
						}
					} else {
						w.deferred[path] = struct{}{}
					}
					continue
				}
				errs = append(errs, err)
			}
		case pathWithin(path, root):
			// path 是 root 的祖先被逐级创建：接管时机树内可能已有文件，补采。
			var err error
			if optional {
				err = w.registerOptionalRoot(ctx, root, true)
			} else {
				_, err = w.registerRoot(ctx, root, true)
			}
			if err != nil {
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
// 若只删除事件路径本身，重建目录时 takeoverDir 会被陈旧 map 误判为已经注册。
// 返回是否确有条目被移除（仅目录树被移除时调用方才需要恢复监听）。
func (w *JSONLWatcher) removeWatchedTree(path string) bool {
	path = filepath.Clean(path)
	removed := false
	for watched := range w.watched {
		if pathWithin(path, watched) {
			delete(w.watched, watched)
			removed = true
		}
	}
	return removed
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
