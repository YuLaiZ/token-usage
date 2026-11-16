// internal/analyzer/jsonl_watcher.go
package analyzer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// vanishPolicy 控制接管遍历对"目录在枚举后、注册前消失"竞态的处理策略。
// 运行期接管入口（Remove/Rename 恢复、Create/接管、祖先路径逐级创建触发的
// 根接管）使用 tolerateVanishedDirs：跳过已消失的失效子树、继续健康兄弟；
// 首次启动与可选根 deferred 重试使用 strictVanishedDirs，保持强根失败即致命、
// 可选根最小失败子树持续重试的既有语义（本次不把运行期容错扩成新的启动期
// 或重试期策略）。
type vanishPolicy bool

const (
	strictVanishedDirs   vanishPolicy = false
	tolerateVanishedDirs vanishPolicy = true
)

// JSONLWatcher 监控 JSONL 文件目录变化
type JSONLWatcher struct {
	dirs []string
	// optional 标记 dirs 中由调用方声明的可选根（Clean 后路径集合）：注册失败
	// 降级为锚定最近存在目录祖先并 Warn，不拖停强根监控；强根失败仍使 Run 返回
	// error。自动附加的监控面（如 cc-switch 账号根）属此类。
	optional   map[string]struct{}
	clientName string
	// agentRoots 是 Claude 采集根集合（主根 + cc-switch 账号根），用于 .jsonl
	// 事件与 meta 映射的受控判定（以采集根为边界，根祖先目录名不影响结果）。
	// 未设置时全部 .jsonl 放行且 meta 映射不做受控判定（仅供直接构造 watcher
	// 的测试使用；生产装配总会设置）。运行期动态接管新目录后经 agentRootsRefresh
	// 刷新（如新增 cc-switch 账号）。
	agentRoots        []string
	agentRootsRefresh func() []string
	debounce          *Debounce
	submit            MonitorSubmitFunc // 向 Analyzer 上报请求（不直接持有 ExecuteFunc）
	signalReady       func()            // Analyzer 预置的就绪回调（Walk+watch 注册后调用恰好一次）
	signalFailure     func(error)       // 初始化失败回调；与 signalReady 共享一次性计数
	logger            *slog.Logger
	watcher           *fsnotify.Watcher
	// errCh 是 watcher 级致命错误的来源通道：构造时绑定后端真实 Errors；测试可在
	// Run 前替换为只读注入通道，从 Run 入口验证错误传播与退出路径，不写入真实后端。
	errCh   <-chan error
	watched map[string]struct{} // 仅由 Run goroutine 访问
	// unconfirmed 记录"已失效、但旧监听是否已释放尚未确认"的已登记路径（仅 Run
	// goroutine 访问，与 watched 分开维护）：释放失败时后端可能仍持有旧监听，
	// 同名目录重建前必须先确认释放，不能仅凭 watched 快速返回或事件清理当作已恢复。
	unconfirmed map[string]struct{}
	// addPathWatch/removePathWatch 绑定 fsnotify 后端最底层的 Add/Remove：构造时
	// 绑定真实方法，测试可在 Run 前替换为替身，稳定覆盖释放监听的各类结果分支。
	// 路径规范化、watched/unconfirmed 维护和清理排序始终走生产逻辑。
	addPathWatch    func(path string) error
	removePathWatch func(path string) error
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
		unconfirmed:   make(map[string]struct{}),
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
	w.errCh = watcher.Errors
	w.addPathWatch = watcher.Add
	w.removePathWatch = watcher.Remove

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
	// 首扫用 strict：启动期强根失败仍是致命错误，本次容错只覆盖运行期入口。
	if err := w.registerRoots(ctx, false, strictVanishedDirs); err != nil {
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
				// 事件清理前先释放受影响子树的后端监听：kqueue 对整树 rename-away
				// 只清理事件路径自身，子孙 watch 会以旧路径残留，同名重建时
				// 后端 Add 命中陈旧条目（假恢复/EBADF）；释放失败进 unconfirmed，
				// 由重建接管路径先确认再登记。
				if w.releaseInvalidatedTree(event.Name) {
					// 运行期恢复接管：目录树删除后重建的场景，重建树内已写入
					// 的文件不产生新事件，恢复时补采（幂等由采集链保证）。
					// tolerate：恢复遍历期间其他目录被删除是常态竞态，跳过失效
					// 子树而不是让整个 watcher 退出。
					if err := w.registerRoots(ctx, true, tolerateVanishedDirs); err != nil {
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
			// 只处理 .jsonl 文件；Claude 子代理的 agent-<id>.meta.json 事件映射
			// 到同目录对应 JSONL（meta 晚于 JSONL 落盘时据此补采标题）。
			target := event.Name
			if filepath.Ext(target) != ".jsonl" {
				if w.clientName != "claude" {
					continue
				}
				target = w.claudeAgentMetaJSONL(target)
				if target == "" {
					continue
				}
			} else if !w.collectableJSONLPath(target) {
				continue
			}
			if !w.isWithinTarget(target) {
				continue
			}
			w.logger.Debug("file event", "op", event.Op, "event_file", event.Name, "target", target)
			w.debounce.Trigger(target)
		case err, ok := <-w.errCh:
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

// SetClaudeAgentRoots 设置 Claude 采集根集合与刷新函数：受控判定以采集根为
// 边界；refresh 在动态接管新目录（如运行期新增 cc-switch 账号）后调用以更新
// 根集合，为 nil 时只在启动时快照。仅对 claude watcher 有意义；须在 Run 前调用。
// 未设置时全部 .jsonl 放行且 meta 映射不做受控判定（仅供测试直接构造）。
func (w *JSONLWatcher) SetClaudeAgentRoots(roots []string, refresh func() []string) {
	w.agentRoots = roots
	w.agentRootsRefresh = refresh
}

// refreshAgentRoots 在动态接管新目录后刷新受控判定根集合，保持 watcher 判定
// 与采集根发现一致（如运行期新增 cc-switch 账号）。仅 Run goroutine 调用。
func (w *JSONLWatcher) refreshAgentRoots() {
	if w.agentRootsRefresh == nil {
		return
	}
	w.agentRoots = w.agentRootsRefresh()
}

// collectableJSONLPath 报告 .jsonl 事件路径是否属于该 client 的采集范围。
// 非 claude client 维持原有「根范围内全部 .jsonl」语义；claude 的 subagents
// 子树只放行受控布局下的 agent transcript（与全扫和 ChangedFile 的
// collector.IsClaudeAgentTranscriptInRoots 同口径），journal.jsonl 等非受控
// 文件不再触发增量采集；主 transcript（采集根内非 subagents 子树）恒放行。
func (w *JSONLWatcher) collectableJSONLPath(path string) bool {
	if w.clientName != "claude" || len(w.agentRoots) == 0 {
		return true
	}
	if !collector.ClaudePathInAgentSubtree(w.agentRoots, path) {
		return true
	}
	return collector.IsClaudeAgentTranscriptInRoots(w.agentRoots, path)
}

// claudeAgentMetaJSONL 把 agent-<id>.meta.json 事件的路径映射到同目录对应的
// agent-<id>.jsonl。非受控 Claude 子代理 meta 形态（subagents 直下或
// workflows/<runId> 下的 agent-*.meta.json 之外的路径）、或目标 JSONL 尚不存在时
// 返回空串——JSONL 自身的创建/写入事件会独立触发采集，不依赖本映射，也避免
// 提交不存在的文件形成采集失败记录。设置了采集根时复用
// collector.IsClaudeAgentTranscriptInRoots，与全扫和 ChangedFile 保持同一范围口径。
func (w *JSONLWatcher) claudeAgentMetaJSONL(metaPath string) string {
	const metaSuffix = ".meta.json"
	if !strings.HasSuffix(metaPath, metaSuffix) {
		return ""
	}
	jsonlPath := strings.TrimSuffix(metaPath, metaSuffix) + ".jsonl"
	if len(w.agentRoots) > 0 && !collector.IsClaudeAgentTranscriptInRoots(w.agentRoots, jsonlPath) {
		return ""
	}
	info, err := os.Stat(jsonlPath)
	if err != nil || info.IsDir() {
		return ""
	}
	return jsonlPath
}

func (w *JSONLWatcher) registerRoots(ctx context.Context, collectExisting bool, tolerance vanishPolicy) error {
	for _, root := range w.dirs {
		if err := w.checkRunning(ctx); err != nil {
			return err
		}
		if _, optional := w.optional[root]; optional {
			if err := w.registerOptionalRoot(ctx, root, collectExisting, tolerance); err != nil {
				return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("register watch root %s", root), fmt.Sprintf("注册监控根目录 %s", root)), err)
			}
			continue
		}
		if _, err := w.registerRoot(ctx, root, collectExisting, tolerance); err != nil {
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
func (w *JSONLWatcher) registerRoot(ctx context.Context, root string, collectExisting bool, tolerance vanishPolicy) ([]string, error) {
	if err := w.checkRunning(ctx); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return nil, errors.New(ui.Bi("path is not a directory", "路径不是目录"))
		}
		// 同时监控父目录，以便目标根目录被删除后可以观察其重建。
		// 运行期容错：父目录可能在 Stat 与监听之间随整树消失（ENOENT 或已
		// 证实的后端竞态错误），此时回到缺根锚定语义重新寻找仍存活的祖先；
		// 真实错误（不可读、非目录等）保持既有失败语义。
		if parent := filepath.Dir(root); parent != root {
			if err := w.addWatch(parent); err != nil {
				if tolerance == tolerateVanishedDirs && w.vanishedError(parent, err) {
					return w.anchorMissingRoot(ctx, root, tolerance)
				}
				return nil, err
			}
		}
		existing, failed, err := w.takeoverDir(ctx, root, collectExisting, tolerance)
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
	return w.anchorMissingRoot(ctx, root, tolerance)
}

// anchorMissingRoot 在配置根缺失（或运行期随父目录一起消失）时锚定最近存在
// 的目录祖先，由其 Create 事件负责逐级重建接管。若竞态窗口内根已快速重建
// （nearestExistingDir 返回根自身），按完整接管处理：递归监听、补采存量——
// 只挂顶层监听会静默漏掉重建树内已存在的子目录与存量文件。同一竞态也存在于
// addWatch(ancestor)：祖先到根之间的路径可能在监听真正生效之前就已出现各级
// 目录，其 Create 事件早于监听而永久丢失——挂上祖先后必须沿目标路径逐段
// 回查：已出现的中间目录逐个登记监听，根也已出现就继续完整接管；第一个
// 尚未出现的目录由上一段监听覆盖，其创建事件会触发后续接管。
// 结果显式区分：已接管（根有有效监听）与已锚定（祖先/中间目录已登记）才
// 返回成功；根在接管中再次消失（tolerate 跳过，含旧登记未确认释放、watched
// 中的记录不可信）时回到锚定流程。all attempts 都是"目录消失待恢复"时绝不
// 报告成功。
func (w *JSONLWatcher) anchorMissingRoot(ctx context.Context, root string, tolerance vanishPolicy) ([]string, error) {
	attempts := 1
	if tolerance == tolerateVanishedDirs {
		attempts = 2
	}
	var lastErr error
	anchored := false
	cleanRoot := filepath.Clean(root)
	// takeoverRebuiltRoot 对已出现的根执行完整接管（递归监听 + 补采存量）；
	// effective=false 表示根当前没有有效监听：接管中再次消失，或旧登记释放
	// 未确认（unconfirmed），调用方应回到锚定流程重试。
	takeoverRebuiltRoot := func(policy vanishPolicy) ([]string, bool, error) {
		existing, failed, terr := w.takeoverDir(ctx, cleanRoot, true, policy)
		for _, f := range existing {
			w.debounce.Trigger(f)
		}
		if terr != nil {
			return failed, false, terr
		}
		return failed, w.rootWatchEffective(cleanRoot), nil
	}
	for i := 0; i < attempts; i++ {
		ancestor, err := nearestExistingDir(root)
		if err != nil {
			return nil, err
		}
		if ancestor == cleanRoot {
			failed, effective, terr := takeoverRebuiltRoot(tolerance)
			if terr != nil {
				return failed, terr
			}
			if effective {
				return failed, nil
			}
			// 根在接管中再次消失（tolerate 跳过），或旧登记仍在 unconfirmed
			// （监听释放未确认、watched 记录不可信）：回到锚定流程重试。
			continue
		}
		if err := w.addWatch(ancestor); err != nil {
			// 目录消失是运行期常态：tolerate 下已有有效锚点时它不构成失败
			// 原因（耗尽判定依赖 lastErr 是否为空），直接回到锚定流程。
			if tolerance == tolerateVanishedDirs && w.vanishedError(ancestor, err) {
				continue
			}
			return nil, err
		}
		anchored = true
		// 挂上祖先监听后沿目标路径逐段回查：中间目录可能已在监听生效之前
		// 出现（创建事件丢失），只查最终根会让根在未被监听的中间目录下创建，
		// 祖先收不到该事件——存量与后续新写入都会静默漏采。
		rootPresent, cerr := w.watchPresentChain(ctx, ancestor, cleanRoot)
		if cerr != nil {
			return nil, cerr
		}
		if !rootPresent {
			return nil, nil
		}
		// 根已完整出现：按完整接管处理（递归监听 + 补采存量）。回查接管是
		// 竞态窗口内的补救：固定使用 tolerate 语义，只过滤明确消失的目录，
		// 权限等真实错误照旧上抛——不能用事后 Stat 把整轮覆盖成锚定成功。
		// strict 下根自身在接管中消失是明确的消失结果，按锚定成功回退，
		// 重建经已登记的祖先/中间目录监听触发。
		failed, effective, terr := takeoverRebuiltRoot(tolerateVanishedDirs)
		if terr != nil {
			return failed, terr
		}
		if effective {
			w.logger.Debug("watch root recovered during anchor recheck",
				"root", cleanRoot, "failed_subtrees", failed)
			return failed, nil
		}
		// tolerate 回到锚定流程重试；strict 只有确认根已消失才回退锚定成功
		// （重建经已登记的祖先/中间目录监听触发）。strict 没有第二次尝试：
		// 根仍存在（旧监听释放未确认）或 Stat 出错时由循环末尾统一报错，
		// Stat 的真实错误保留为失败原因。
		if tolerance == tolerateVanishedDirs {
			continue
		}
		_, statErr := os.Stat(cleanRoot)
		if os.IsNotExist(statErr) {
			w.logger.Debug("root vanished again during recheck takeover, anchored on ancestor",
				"root", cleanRoot, "failed_subtrees", failed)
			return nil, nil
		}
		if statErr != nil {
			lastErr = statErr
		}
		continue
	}
	// 重试耗尽：区分"仍有有效锚点"与"确实没有可用监听"。至少一次尝试已
	// 登记有效父监听且没有真实接管错误时，锚定状态仍然成立——根或下一级
	// 目录的创建事件会再次触发接管，不能把"等待重建"误报为致命失败。
	if anchored && lastErr == nil {
		return nil, nil
	}
	// 一次监听也没挂上：不能报告成功。
	if lastErr == nil {
		lastErr = errors.New(ui.Bi(
			fmt.Sprintf("watch root %s kept vanishing before takeover", root),
			fmt.Sprintf("监控根 %s 在接管前持续消失", root)))
	}
	return nil, lastErr
}

// watchPresentChain 从已监听的祖先出发，沿目标根的路径逐段挂上仍然存在的
// 目录，直到配置根（返回 true，调用方按完整接管处理）或第一个尚未出现的
// 目录（返回 false——它的创建事件必然落在上一段已生效的监听里）。
// 目录在 Stat 与 Add 之间消失是竞态常态：明确的消失结果回退为锚定成功
// （重建事件同样由上一段监听捕获），权限等其他错误保留失败语义。
func (w *JSONLWatcher) watchPresentChain(ctx context.Context, ancestor, root string) (bool, error) {
	rel, err := filepath.Rel(ancestor, root)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	current := ancestor
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if err := w.checkRunning(ctx); err != nil {
			return false, err
		}
		current = filepath.Join(current, seg)
		info, serr := os.Stat(current)
		switch {
		case serr == nil:
			if !info.IsDir() {
				return false, errors.New(ui.Bi("path is not a directory", "路径不是目录"))
			}
		case os.IsNotExist(serr):
			return false, nil
		default:
			return false, serr
		}
		if err := w.addWatch(current); err != nil {
			if w.vanishedError(current, err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// rootWatchEffective 判定根当前是否有有效监听：已登记且未被标记为释放未确认
// （unconfirmed 表示旧监听可能仍在后端，watched 里的记录不能当作已恢复）。
func (w *JSONLWatcher) rootWatchEffective(path string) bool {
	if _, watched := w.watched[path]; !watched {
		return false
	}
	_, unconfirmed := w.unconfirmed[path]
	return !unconfirmed
}

// registerOptionalRoot 注册可选根：注册失败（同名文件占用、树不可读等）降级为
// Warn + 失败子树记入 deferred（retryTicker 周期重试，成功后补采——覆盖权限修复
// 等不产生文件事件的恢复形态；**只记最小失败子树**，已成功接管的健康部分完成一次
// 补采后即退出重试范围，故障持续期间不得周期性重复提交健康兄弟，R8 实证）+
// 尽力锚定最近存在的目录祖先（锚点上的 Create/Remove 事件经 registerCreatedDir /
// releaseInvalidatedTree 链路即时恢复，ticker 是兜底），不拖停强根监控。
func (w *JSONLWatcher) registerOptionalRoot(ctx context.Context, root string, collectExisting bool, tolerance vanishPolicy) error {
	failed, err := w.registerRoot(ctx, root, collectExisting, tolerance)
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
		// 可选根重试保持既有 strict 语义（方案：可选根对真实错误的最小失败
		// 子树重试、锚定和补采保持原样）：目录暂缺时继续留在 deferred 重试，
		// 不能把"本次未接管成功"当成已恢复——锚点注册失败时 ticker 是唯一
		// 恢复途径，提前移出会让同名目录重建后失去兜底。
		existing, failed, err := w.takeoverDir(ctx, d, true, strictVanishedDirs)
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
		// 延迟接管成功同样可能引入新采集根（如账号目录首次创建失败后恢复）。
		w.refreshAgentRoots()
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
//
// tolerance 为 tolerateVanishedDirs 时（运行期入口）：已被枚举或已登记的子目录
// 随后消失（fs.ErrNotExist）按竞态跳过——不记失败子树、不向上传播，继续接管
// 健康兄弟；Add 成功后发现消失的，释放该子树已失效的监听再跳过。只在单个
// Add/ReadDir 失败处分类，聚合后的错误含 ENOENT 不代表整批可吞。真实错误
// （非 ENOENT）语义不变。
func (w *JSONLWatcher) takeoverDir(ctx context.Context, dir string, collectExisting bool, tolerance vanishPolicy) ([]string, []string, error) {
	if err := w.checkRunning(ctx); err != nil {
		return nil, nil, err
	}
	if !w.isRelevantDir(dir) {
		return nil, nil, nil
	}
	if err := w.addWatch(dir); err != nil {
		if tolerance == tolerateVanishedDirs && w.vanishedError(dir, err) {
			w.logger.Debug("directory vanished before watch registration, skipping subtree", "dir", dir)
			return nil, nil, nil
		}
		return nil, []string{dir}, err
	}
	if w.afterWatchHook != nil {
		w.afterWatchHook(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if tolerance == tolerateVanishedDirs && w.vanishedError(dir, err) {
			// Add 已成功但目录在枚举前消失：后端可能仍持有该子树的监听，
			// 释放后再跳过，避免同名目录重建时被旧监听状态误导。
			w.releaseVanishedSubtree(dir)
			w.logger.Debug("directory vanished after watch registration, released subtree", "dir", dir)
			return nil, nil, nil
		}
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
			sub, subFailed, err := w.takeoverDir(ctx, path, collectExisting, tolerance)
			existing = append(existing, sub...)
			failed = append(failed, subFailed...)
			errs = errors.Join(errs, err)
			continue
		}
		if collectExisting && filepath.Ext(path) == ".jsonl" && w.isWithinTarget(path) && w.collectableJSONLPath(path) {
			existing = append(existing, path)
		}
	}
	return existing, failed, errs
}

func (w *JSONLWatcher) addWatch(path string) error {
	path = filepath.Clean(path)
	if _, unconfirmed := w.unconfirmed[path]; unconfirmed {
		// 旧监听是否已释放未确认：绕过 watched 快速返回，先重试释放再重新
		// 登记——否则同名目录重建时会把可能仍存活于后端的旧监听当成已恢复。
		// 仍无法确认释放则按真实错误上抛，交由强根失败/可选根降级策略处置。
		if err := w.removePathWatch(path); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("release stale watch for %s", path), fmt.Sprintf("释放旧监听 %s", path)), err)
		}
		delete(w.unconfirmed, path)
		delete(w.watched, path)
	} else if _, ok := w.watched[path]; ok {
		return nil
	}
	if err := w.addPathWatch(path); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("watch directory %s", path), fmt.Sprintf("监听目录 %s", path)), err)
	}
	w.watched[path] = struct{}{}
	return nil
}

// vanishedError 判定 Add/ReadDir 失败是否属于"目录已消失"的运行期竞态。
// 直接按 fs.ErrNotExist 分类（方案要求的判定口径）；后端在删除竞态下还可能
// 返回已证实的实现相关错误（kqueue 在 readEvents 清理与 Add 并发时用陈旧 fd
// 注册得到 EBADF），只有这类错误才做"目录当前是否仍存在"的二次确认——权限
// 等其他真实错误即使目录事后消失也保持失败语义，不能被事后的 ENOENT 兜底
// 吞掉。
func (w *JSONLWatcher) vanishedError(path string, err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if !backendVanishErr(err) {
		return false
	}
	_, statErr := os.Stat(filepath.Clean(path))
	return errors.Is(statErr, fs.ErrNotExist)
}

// backendVanishErr 是 fsnotify 后端在目录删除竞态下可能返回的实现相关错误
// 集合（当前仅已证实的 kqueue 陈旧 fd EBADF）；新增成员须有原生复现依据。
func backendVanishErr(err error) bool {
	return errors.Is(err, syscall.EBADF)
}

// releaseVanishedSubtree 释放已消失目录树在本地登记的监听：先快照 watched 中
// 该子树（含根）的路径，按相对失效子树根的组件数降序（子路径先于父路径）、
// 同深度按路径名升序逐个 Remove。Remove 成功或 ErrNonExistentWatch 表示旧监听
// 已释放，清除本地记录与未确认标记；其他错误说明后端可能仍持有旧监听，把路径
// 记入 unconfirmed，等待重建或后续遍历触达时先重试释放再重新 Add。Remove 不
// 递归，也不得清理仍存活的父目录或其他根。
func (w *JSONLWatcher) releaseVanishedSubtree(root string) {
	for _, path := range w.watchedSubtreePaths(root) {
		if err := w.removePathWatch(path); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			w.unconfirmed[path] = struct{}{}
			w.logger.Debug("stale watch release failed, marked unconfirmed", "path", path, "error", err)
			continue
		}
		delete(w.watched, path)
		delete(w.unconfirmed, path)
	}
}

// releaseInvalidatedTree 处理 Remove/Rename 事件驱动的失效清理：目录被删除或
// 移走后，本地记录随事件清除，但后端监听必须先逐个释放确认——kqueue 对整树
// rename-away 只清理事件路径自身，子孙 watch 会以旧路径残留在后端表中，同名
// 目录重建时 Add 会命中陈旧条目并被误当已恢复（写入事件静默丢失）。Remove
// 成功或 ErrNonExistentWatch 视为已释放；其他错误仍清本地记录（事件已证明
// 路径失效，不能靠陈旧记录快速返回），并记入 unconfirmed，由重建接管路径
// 先重试释放再登记。返回是否确有条目被移除（仅目录树被移除时调用方才需要
// 恢复监听）。普通文件删除在 watched 中没有记录，直接返回 false，不触发
// 重注册（会话文件删除/轮转是高频事件）。
func (w *JSONLWatcher) releaseInvalidatedTree(path string) bool {
	paths := w.watchedSubtreePaths(path)
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if err := w.removePathWatch(p); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			w.unconfirmed[p] = struct{}{}
			w.logger.Debug("invalidated watch release failed, marked unconfirmed", "path", p, "error", err)
		} else {
			delete(w.unconfirmed, p)
		}
		delete(w.watched, p)
	}
	return true
}

// watchedSubtreePaths 快照 watched 中位于 root 子树（含根）内的路径，按相对
// root 的组件数降序（子路径先于父路径）、同深度按路径名升序排序，供失效
// 监听逐个释放；Remove 不递归，子路径必须先于父路径释放。
func (w *JSONLWatcher) watchedSubtreePaths(root string) []string {
	root = filepath.Clean(root)
	paths := make([]string, 0, 8)
	for watched := range w.watched {
		if pathWithin(root, watched) {
			paths = append(paths, watched)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := pathDepthWithin(root, paths[i]), pathDepthWithin(root, paths[j])
		if di != dj {
			return di > dj
		}
		return paths[i] < paths[j]
	})
	return paths
}

// pathDepthWithin 返回 path 相对 root 的路径组件数（root 自身为 0），
// 供失效子树清理排序使用。
func pathDepthWithin(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(os.PathSeparator)))
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
			// 重试范围，R8）。tolerate：接管遍历期间兄弟目录被删除是常态
			// 竞态，跳过而不是把整个 watcher 拖停。
			existing, failed, err := w.takeoverDir(ctx, path, true, tolerateVanishedDirs)
			// 动态接管可能带来新的采集根（如运行期新增 cc-switch 账号）：
			// 刷新受控判定根集合，后续 meta 映射与 .jsonl 事件按新根判定。
			w.refreshAgentRoots()
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
				err = w.registerOptionalRoot(ctx, root, true, tolerateVanishedDirs)
			} else {
				_, err = w.registerRoot(ctx, root, true, tolerateVanishedDirs)
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
