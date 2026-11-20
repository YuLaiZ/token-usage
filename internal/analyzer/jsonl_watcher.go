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
	// coworkRoots 是 Claude Desktop Cowork 采集根集合（local-agent-mode-sessions）。
	// 与 agentRoots 判定语义不同，单列平行字段；未设置时 Cowork 事件不特殊放行。
	// 运行期动态接管后经 coworkRootsRefresh 刷新。
	coworkRoots        []string
	coworkRootsRefresh func() []string
	debounce           *Debounce
	submit             MonitorSubmitFunc // 向 Analyzer 上报请求（不直接持有 ExecuteFunc）
	signalReady        func()            // Analyzer 预置的就绪回调（Walk+watch 注册后调用恰好一次）
	signalFailure      func(error)       // 初始化失败回调；与 signalReady 共享一次性计数
	logger             *slog.Logger
	watcher            *fsnotify.Watcher
	// errCh 是 watcher 级致命错误的来源通道：构造时绑定后端真实 Errors；测试可在
	// Run 前替换为只读注入通道，从 Run 入口验证错误传播与退出路径，不写入真实后端。
	errCh <-chan error
	// dirsResolved 是 dirs 各自解析符号链接后的真实路径集合（与 dirs 一一对应，
	// 解析失败时取字面路径）。事件可能按真实路径产生（符号链接目录按真实路径
	// 登记监听），根归属判定需要同时接受两种形态。
	dirsResolved []string
	// linkMap 记录「登记监听的真实路径 → 采集根之下的逻辑路径集合」。子目录是
	// 符号链接且目标在采集面之外时，fsnotify 只在真实路径下给事件；采集侧需要
	// 的是根内的逻辑路径（受控布局与 Cowork 会话目录关联都以采集根为边界）。
	// 同一真实目录可能被多个链接同时指向（例如一个 Cowork 会话的合法
	// projects/session/ 入口与另一个会话的 uploads/ 入口共享目标），必须保留
	// 全部入口：事件映射时按采集资格选择，不能只留最后登记的那一个，否则合法
	// 入口会被非采集路径覆盖后静默漏采。仅由 Run goroutine 访问。
	linkMap map[string]map[string]struct{}
	// staleLinkTargets 记录已撤销别名的旧真实路径：这些子树里残留的监听仍会
	// 送来事件，收到后必须丢弃（链接删除或重指后，旧目标不再代表任何逻辑路径）。
	// 仅由 Run goroutine 访问。
	staleLinkTargets map[string]struct{}
	// watchOwners 记录每个已登记监听被谁持有：空串表示采集根自身的接管，非空为
	// 跟随该逻辑路径的链接别名。同一真实目录被主根与 Cowork 链接同时需要时，
	// 撤销链接只解除自己那一份持有，绝不释放主根仍在用的监听。仅 Run goroutine 访问。
	watchOwners         map[string]map[string]struct{}
	watchOwnerRealPaths map[string]string   // 注册时的真实路径，目录消失后也能清理。
	watched             map[string]struct{} // 仅由 Run goroutine 访问
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

	resolvedDirs := make([]string, 0, len(normalized))
	for _, dir := range normalized {
		resolvedDirs = append(resolvedDirs, resolveSymlinkPath(dir))
	}

	w := &JSONLWatcher{
		dirs:                normalized,
		dirsResolved:        resolvedDirs,
		optional:            optional,
		clientName:          clientName,
		submit:              submit,
		logger:              logger,
		watched:             make(map[string]struct{}),
		unconfirmed:         make(map[string]struct{}),
		linkMap:             make(map[string]map[string]struct{}),
		staleLinkTargets:    make(map[string]struct{}),
		watchOwners:         make(map[string]map[string]struct{}),
		watchOwnerRealPaths: make(map[string]string),
		deferred:            make(map[string]struct{}),
		retryInterval:       10 * time.Second,
		stopCh:              make(chan struct{}),
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
			// 事件名可能是被跟随链接之下的真实路径，范围判定与登记都用逻辑形态。
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					// 目录事件按「逻辑路径」接管：目录本身不是 .jsonl，不能用 transcript
					// 资格判定去改写它，否则通往合法 transcript 的祖先目录会被判成
					// 根外路径，新目录永远接不上。
					if addErr := w.registerCreatedDir(ctx, w.logicalDirPath(event.Name)); addErr != nil {
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
				// 链接被删除或重指：watched 以真实路径为键，链接路径上来的事件
				// 找不到条目，必须先撤销别名并释放旧目标的监听。
				w.invalidateLinksUnder(event.Name)
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
			// 目录自身的 Write 事件（kqueue 对"已监听目录内的条目被替换"常只给
			// 父目录一个 Write）同样要复核链接：链接被删除或改指另一目录时，
			// watched 里的旧真实路径仍然有效，只能靠重新解析逻辑路径发现不一致。
			// 按全量复核而不是按事件路径筛选——重指后事件仍落在旧目标，与逻辑
			// 路径没有字面关系，筛选会漏检。
			if event.Op&fsnotify.Write != 0 {
				w.revalidateAllLinkAliases()
			}
			// 只处理 Create 和 Write 事件
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			// 只处理 .jsonl 文件；Claude 子代理的 agent-<id>.meta.json 事件映射
			// 到同目录对应 JSONL（meta 晚于 JSONL 落盘时据此补采标题）。
			// 被跟随的符号链接目录按真实路径登记监听，事件名也在真实路径下产生；
			// 先还原成采集根之内的逻辑路径，后续受控判定与上报都用它。
			// 还原前先就地校验别名是否仍成立：链接被删除或重指后，旧目标残留的
			// 监听仍可能送来事件，而撤销别名的那次目录事件可能晚于本次写入。
			// 每个文件事件都复核全部别名：删除或重指链接本身在 macOS 上可能完全
			// 不产生事件（监听挂在真实目标上，删除链接只是父目录的条目变更），
			// 只复核「相关路径」会让失效别名一直留到下一次写入才被发现。
			w.revalidateAllLinkAliases()
			target := w.logicalEventPath(event.Name)
			if target == event.Name && w.linkAliasStale(event.Name) {
				// 事件落在已失效的链接目标下：别名刚被撤销，本次事件不应再被采集。
				w.logger.Debug("stale symlink target event dropped", "file", event.Name)
				continue
			}
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
			// 事件路径可能是符号链接形态（根经链接访问，或 macOS 的 /var 与
			// /private/var 两种写法），isWithinTarget 内部同时比对字面与解析后的形态。
			if !w.isWithinTarget(target) {
				continue
			}
			// 归属兜底：事件路径若既不在采集根内直立存在（不经任何链接），也没有
			// 命中当前仍有效的链接别名，就说明它已经失去归属（链接被删除或重指后，
			// 旧目标下残留的监听仍会送来事件），必须丢弃。
			if !w.eventHasAttribution(event.Name, target) {
				w.logger.Debug("event without collectable attribution dropped", "file", event.Name)
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

// SetClaudeCoworkRoots 设置 Claude Desktop Cowork 采集根集合与刷新函数。
// Cowork 根事件只放行受控布局 transcript（与全扫/ChangedFile 的
// collector.IsClaudeCoworkTranscriptInRoots 同口径），audit/ledger/uploads/
// subagents 等一律丢弃。未设置时行为不变（仅供测试直接构造 watcher）。
// 须在 Run 前调用。
func (w *JSONLWatcher) SetClaudeCoworkRoots(roots []string, refresh func() []string) {
	w.coworkRoots = roots
	w.coworkRootsRefresh = refresh
}

// refreshRoots 刷新 agent 与 Cowork 两套采集根（动态接管后调用）。
func (w *JSONLWatcher) refreshRoots() {
	w.refreshAgentRoots()
	if w.coworkRootsRefresh != nil {
		w.coworkRoots = w.coworkRootsRefresh()
	}
}

// collectableJSONLPath 报告 .jsonl 事件路径是否属于该 client 的采集范围。
// 非 claude client 维持原有「根范围内全部 .jsonl」语义；claude 的 subagents
// 子树只放行受控布局下的 agent transcript（与全扫和 ChangedFile 的
// collector.IsClaudeAgentTranscriptInRoots 同口径），journal.jsonl 等非受控
// 文件不再触发增量采集；主 transcript（采集根内非 subagents 子树）恒放行。
func (w *JSONLWatcher) collectableJSONLPath(path string) bool {
	if w.clientName != "claude" {
		return true
	}
	// Cowork 根优先：落在 Cowork 根内必须是受控布局 transcript 才放行
	// （audit.jsonl / usage-ledger / uploads / subagents 等丢弃）。
	if len(w.coworkRoots) > 0 && claudeCoworkPathInRoots(w.coworkRoots, path) {
		return collector.IsClaudeCoworkTranscriptInRoots(w.coworkRoots, path)
	}
	if len(w.agentRoots) == 0 {
		return true
	}
	if !collector.ClaudePathInAgentSubtree(w.agentRoots, path) {
		return true
	}
	return collector.IsClaudeAgentTranscriptInRoots(w.agentRoots, path)
}

// claudeCoworkPathInRoots 报告 path 是否落在任一 Cowork 采集根内（含根本身）。
func claudeCoworkPathInRoots(roots []string, path string) bool {
	absP, err := filepath.Abs(path)
	if err != nil {
		absP = path
	}
	absP = filepath.Clean(absP)
	for _, r := range roots {
		if r == "" {
			continue
		}
		absR, err := filepath.Abs(r)
		if err != nil {
			absR = r
		}
		rel, err := filepath.Rel(filepath.Clean(absR), absP)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
		existing, failed, err := w.takeoverDeferredDir(ctx, d)
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
		w.refreshRoots()
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
	return w.takeoverDirRecursive(ctx, dir, dir, collectExisting, tolerance, nil)
}

// watchDir 用于注册监听，dir 用于逻辑路径判定与枚举。只阻断当前递归链上的
// 真实目录回边，平行入口仍可各自登记别名和监听所有者。
func (w *JSONLWatcher) takeoverDirRecursive(ctx context.Context, watchDir, dir string, collectExisting bool, tolerance vanishPolicy, ancestors map[string]struct{}) ([]string, []string, error) {
	if err := w.checkRunning(ctx); err != nil {
		return nil, nil, err
	}
	if !w.isRelevantDir(dir) {
		return nil, nil, nil
	}
	if ancestors == nil {
		ancestors = make(map[string]struct{}, 8)
		// 运行期可能直接从新建或重指的链接开始接管，父目录未经过这次递归。
		for child := filepath.Clean(dir); ; {
			parent := filepath.Dir(child)
			if parent == child {
				break
			}
			ancestors[resolveSymlinkPath(parent)] = struct{}{}
			child = parent
		}
	}
	key := resolveSymlinkPath(dir)
	if _, seen := ancestors[key]; seen {
		return nil, nil, nil
	}
	ancestors[key] = struct{}{}
	defer delete(ancestors, key)
	// 先登记本层链接再计算 owner：ownerForSubtree 沿 linkMap 找链路起点，
	// 若放在登记之后计算，链接目标第一层会拿到空串（原生所有者），递归进入
	// 的普通后代也会跟着误记为原生，撤销链接时这些监听无法被释放。
	if watchDir != dir {
		w.addLinkAlias(watchDir, dir)
	}
	owners := w.ownerForSubtree(dir)
	if len(owners) == 0 {
		// 不在任何链接之下：采集根自身持有该监听（空串 owner）。
		owners = []string{""}
	}
	if err := w.addWatchOwned(watchDir, owners...); err != nil {
		if tolerance == tolerateVanishedDirs && w.vanishedError(watchDir, err) {
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
			sub, subFailed, err := w.takeoverDirRecursive(ctx, path, path, collectExisting, tolerance, ancestors)
			existing = append(existing, sub...)
			failed = append(failed, subFailed...)
			errs = errors.Join(errs, err)
			continue
		}
		// 符号链接目录：只有落在 Cowork 覆盖范围内才跟随。文件监听必须挂在真实
		// 路径上（fsnotify 只监听链接本身时，目标子树里的写入不产生任何事件），
		// 枚举与范围判定仍走采集根之下的逻辑路径；linkMap 记下两者的对应关系，
		// 事件到达时才能还原成采集侧认识的逻辑路径。普通客户端的目录链接不
		// 跟随，避免在配置根之外扩大监听与采集范围。
		if realDir, isLink := w.symlinkDirTarget(path, e); isLink {
			if !w.shouldFollowDirLink(path, realDir) {
				continue
			}
			sub, subFailed, err := w.takeoverDirRecursive(ctx, realDir, path, collectExisting, tolerance, ancestors)
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

// logicalEventPath 把事件路径还原为采集根之下的逻辑路径：命中被跟随的目录
// 链接时，把真实路径前缀换成登记时的逻辑路径（更深的子目录继续拼接）；
// 未命中则原样返回。
//
// 映射只在「该路径原本没有采集资格」时生效：链接目标若本身就是某个监控根内
// 的正常目录（典型是 Cowork 会话里的 uploads/ 链到 Claude 的 projects_dir），
// 它产生的事件属于那个根，改写会让真实采集路径被顶替成链接的逻辑路径，随后
// 被 Cowork 的受控过滤丢弃——主根的事件因此静默消失。同理，映射结果自身不
// 具备采集资格时也保留原路径，由上游的原有判定决定去留。
//
// 同一真实目录可能有多个逻辑入口（合法 projects/session/ 与另一个会话的
// uploads/ 共享目标）：按逻辑路径排序逐个尝试，返回第一个具备采集资格的
// 入口；更深一层前缀没有可采集入口时继续回退到较浅的匹配前缀，避免 uploads/
// 挂在目标子目录时把较浅层的合法 .claude/ 入口一起遮蔽。
func (w *JSONLWatcher) logicalEventPath(path string) string {
	if len(w.linkMap) == 0 {
		return path
	}
	levels := w.linkLevelsFor(path)
	if len(levels) == 0 {
		return path
	}
	// 真实路径本身已在某个监控根内、且已通过该 client 的受控判定时保留原归属：
	// 典型是 Cowork 会话里的 uploads/ 链到 Claude 的 projects_dir，那条事件属于
	// 主根，改写成 Cowork 逻辑路径会被受控过滤丢弃，主根事件因此静默消失。
	// 与下方「映射结果必须具备采集资格」互为纵深：任一处单独失效时另一处仍挡。
	if w.hasNativeCollectability(levels[0].form) {
		return levels[0].form
	}
	// 从最具体的真实路径前缀开始逐层回退：更深一层的入口可能全部不可采集
	// （uploads/ 链到目标子目录），此时较浅一层的合法入口（.claude/）仍是
	// 同一事件的合法归属，不能因为选中了最深前缀就整份漏采。
	for _, lv := range levels {
		for _, logical := range w.linkLogics(lv.real) {
			mapped, ok := mappedLinkPath(lv.real, lv.form, logical)
			if !ok {
				continue
			}
			// 映射结果必须具备采集资格，否则说明这条入口到达的不是合法 transcript，
			// 尝试下一个入口/下一层；全部不合格时保留原路径，让上游按原有规则
			// 决定去留（宁可漏掉链接副本，也不改写真实归属）。
			if w.hasNativeCollectability(mapped) {
				return mapped
			}
		}
	}
	return path
}

// logicalDirPath 把目录事件的真实路径还原为采集根之下的逻辑路径。与
// logicalEventPath 的区别是不要求路径本身具备 transcript 采集资格：目录只是
// 容器。按真实路径前缀从深到浅逐层回退，优先选择能成为受控 transcript 祖先的
// 逻辑入口（合成子路径后走采集资格判定）——较深的 uploads/ 入口不能遮蔽较浅的
// 合法 .claude/ 入口，否则新目录接管时的存量文件补采会被受控过滤丢掉。
func (w *JSONLWatcher) logicalDirPath(path string) string {
	if len(w.linkMap) == 0 {
		return path
	}
	// 目录事件本身具备原生目录接管资格时保留原生入口：典型是 Cowork 会话的
	// uploads/ 链接到 Claude projects_dir，主根里移入的新目录必须按主根接管，
	// 才能补采移入前已存在的 transcript。仅「在某个监控根内」不足够：
	// uploads/ 等受控排除位置虽然位于 Cowork 根内，却不具备采集资格，
	// 必须继续查找合法别名（例如另一个会话的 .claude/projects/session/）。
	if w.logicalDirCandidateCollectable(path) {
		return path
	}
	levels := w.linkLevelsFor(path)
	if len(levels) == 0 {
		return path
	}
	fallback := ""
	for _, lv := range levels {
		for _, logical := range w.linkLogics(lv.real) {
			mapped, ok := mappedLinkPath(lv.real, lv.form, logical)
			if !ok {
				continue
			}
			if fallback == "" {
				fallback = mapped
			}
			if w.logicalDirCandidateCollectable(mapped) {
				return mapped
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return path
}

// linkLevel 是事件路径命中的一层链接映射：real 是登记监听的真实路径前缀，
// form 是事件路径在该层匹配到的形态（绝对或符号链接解析形态）。
type linkLevel struct {
	real string
	form string
}

// linkLevelsFor 返回事件路径命中的所有链接层，按真实路径长度降序（更具体的
// 前缀优先），同长度按真实路径字典序，保证选择结果确定。
func (w *JSONLWatcher) linkLevelsFor(path string) []linkLevel {
	forms := pathForms(path)
	levels := make([]linkLevel, 0, len(w.linkMap))
	for realPath := range w.linkMap {
		for _, form := range forms {
			if !pathWithin(realPath, form) {
				continue
			}
			levels = append(levels, linkLevel{real: filepath.Clean(realPath), form: form})
			break
		}
	}
	sort.Slice(levels, func(i, j int) bool {
		if len(levels[i].real) != len(levels[j].real) {
			return len(levels[i].real) > len(levels[j].real)
		}
		return levels[i].real < levels[j].real
	})
	return levels
}

// linkLogics 返回某真实路径的全部逻辑入口（排序后的 Clean 形态）。
func (w *JSONLWatcher) linkLogics(realPath string) []string {
	logics := make([]string, 0, len(w.linkMap[realPath]))
	for logical := range w.linkMap[realPath] {
		logics = append(logics, filepath.Clean(logical))
	}
	sort.Strings(logics)
	return logics
}

// mappedLinkPath 把事件路径在某层映射为逻辑路径。
func mappedLinkPath(realPath, form, logical string) (string, bool) {
	rel, err := filepath.Rel(realPath, form)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return logical, true
	}
	return filepath.Join(logical, rel), true
}

// logicalDirCandidateCollectable 报告目录逻辑路径是否可能成为受控 transcript
// 的祖先：合成一个 .jsonl 子路径后走与文件事件相同的采集资格判定。目录本身
// 不是 transcript，不能直接判定，但选错入口（uploads/）会让接管时的存量文件
// 补采被受控过滤丢弃。
func (w *JSONLWatcher) logicalDirCandidateCollectable(dir string) bool {
	if dir == "" {
		return false
	}
	if !w.isWithinTarget(dir) {
		return false
	}
	// Cowork 目录按受控布局的合法祖先判定：新建的 .claude 还没有 projects/，
	// 合成 probe.jsonl 会把它误判为不可采集，目录接管就会回退到更深的 uploads/
	// 入口，后续子目录与文件事件都沿着 uploads 逻辑路径传播并被受控过滤丢弃。
	if len(w.coworkRoots) > 0 && collector.IsClaudeCoworkDirInRoots(w.coworkRoots, dir) {
		return true
	}
	// 原生 Claude 子代理布局：<proj>/subagents[/workflows/<runId>] 只放行
	// agent-*.jsonl，合成 probe.jsonl 会误判；保留原生入口，接管时仍按文件
	// 过滤。否则 subagents 目录会被 Cowork 别名接管，父会话与标题随之丢失。
	if collector.IsClaudeAgentDirInRoots(w.agentRoots, dir) {
		return true
	}
	return w.collectableJSONLPath(filepath.Join(dir, "probe.jsonl"))
}

// eventHasAttribution 报告一个事件是否仍然归属于某个采集入口：
//   - 事件路径本身就是采集根之下的正常路径（不经链接、或链接仍在有效映射中
//     且改写后仍在根内）；
//   - 或它是当前 linkMap 中某个有效别名映射出来的逻辑路径。
//
// 链接被删除或重指后，旧目标下残留的监听仍会送来事件：此时既没有根内位置，
// 也没有有效别名，必须丢弃，否则会以原逻辑路径提交一个已失效的采集请求。
func (w *JSONLWatcher) eventHasAttribution(eventPath, target string) bool {
	// 事件路径直接在根内（普通目录）：归属成立。
	for _, root := range w.rootForms() {
		for _, form := range pathForms(eventPath) {
			if pathWithin(root, form) && w.isWithinTarget(form) {
				return true
			}
		}
	}
	// 事件被改写成根内逻辑路径：要求该逻辑路径现在仍能解析回同一个真实目标
	// （链接仍在、未重指），否则改写只是残留别名的产物。
	if filepath.Clean(target) != filepath.Clean(eventPath) && w.isWithinTarget(target) {
		if resolved, err := filepath.EvalSymlinks(target); err == nil {
			if filepath.Clean(resolved) == filepath.Clean(eventPath) {
				return true
			}
		}
	}
	return false
}

// linkAliasStale 报告 path 是否落在「曾经被跟随、但当前已不再指向该逻辑路径」
// 的链接目标下。这类路径既没有采集资格（不在监控根内的正常位置），也不再有
// 有效的别名可以映射回去，必须直接丢弃——否则链接删除或重指后，旧目标里残留
// 的监听会把写入继续当成原逻辑路径的采集请求。
func (w *JSONLWatcher) linkAliasStale(path string) bool {
	if len(w.staleLinkTargets) == 0 {
		return false
	}
	// 该路径若本身仍是某个采集根下的正常采集路径（典型：别名指向的正是主
	// projects_dir），它就不能算「失效目标」——主根的写入必须继续采集。
	if w.hasNativeCollectability(path) {
		return false
	}
	for realPath := range w.staleLinkTargets {
		if pathWithin(realPath, path) {
			return true
		}
	}
	return false
}

// revalidateAllLinkAliases 复核全部链接别名是否仍然成立。文件事件远少于目录
// 枚举，代价可以接受；这样即使链接的删除/重指没有产生任何目录事件（macOS
// kqueue 常见），失效别名也会在下一次写入时被清理。
func (w *JSONLWatcher) revalidateAllLinkAliases() {
	if len(w.linkMap) == 0 {
		return
	}
	type alias struct{ real, logical string }
	stale := make([]alias, 0, len(w.linkMap))
	for realPath, logics := range w.linkMap {
		for logical := range logics {
			stale = append(stale, alias{realPath, logical})
		}
	}
	for _, a := range stale {
		// 不能只 EvalSymlinks 整条逻辑路径：链路中间的符号链接被删除后，
		// 某些平台仍可能返回旧解析结果。逻辑路径自身若已不是符号链接即失效。
		if fi, lerr := os.Lstat(a.logical); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
			w.dropLinkAlias(a.real, a.logical)
			continue
		}
		resolved, err := filepath.EvalSymlinks(a.logical)
		if err != nil {
			w.dropLinkAlias(a.real, a.logical)
			continue
		}
		target := resolved
		if abs, absErr := filepath.Abs(target); absErr == nil {
			target = abs
		}
		if filepath.Clean(target) == filepath.Clean(a.real) {
			continue
		}
		w.logger.Debug("symlink retargeted, releasing old alias",
			"old_real", a.real, "new_real", target, "logical", a.logical)
		w.dropLinkAlias(a.real, a.logical)
		// 新目标必须立刻接管：重指后旧目标的监听已撤销，若不同时登记新目标，
		// 新目标里的写入既没有监听也没有映射，会整体漏采。
		w.retargetLink(a.logical, target)
	}
}

// addLinkAlias 登记一条「真实路径 → 逻辑路径」别名。同一真实目录可能被多个
// 逻辑入口同时指向（Cowork 一个会话的合法 projects/session/ 与另一个会话的
// uploads/ 共享目标），集合保留全部入口；事件映射时再按采集资格选择可用入口。
// 重新登记同时清除该真实路径的失效标记（链接重指回来时旧目标恢复可用）。
func (w *JSONLWatcher) addLinkAlias(realPath, logical string) {
	realPath = filepath.Clean(realPath)
	logical = filepath.Clean(logical)
	set := w.linkMap[realPath]
	if set == nil {
		set = make(map[string]struct{}, 2)
		w.linkMap[realPath] = set
	}
	set[logical] = struct{}{}
	delete(w.staleLinkTargets, realPath)
}

// ownerForSubtree 返回逻辑路径 dir 所属的全部链路起点：沿 linkMap 找到所有
// 覆盖它的逻辑路径。不在任何链接之下时返回空切片（采集根自身接管）。链接
// 之下的所有后代共享这些归属，撤销任一链接只解除它自己的持有，最后一个入口
// 撤销时才真正释放监听。
func (w *JSONLWatcher) ownerForSubtree(dir string) []string {
	dir = filepath.Clean(dir)
	resolvedDir := resolveSymlinkPath(dir)
	owners := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for realPath, logics := range w.linkMap {
		realPath = filepath.Clean(realPath)
		for logical := range logics {
			logical = filepath.Clean(logical)
			// 逻辑路径与真实路径都要比：递归进入的普通后代用逻辑路径，
			// 而 linkMap 的键是解析后的真实路径，两者在 macOS 上完全不同。
			if !pathWithin(logical, dir) && !pathWithin(realPath, resolvedDir) {
				continue
			}
			if _, dup := seen[logical]; dup {
				continue
			}
			seen[logical] = struct{}{}
			owners = append(owners, logical)
		}
	}
	sort.Strings(owners)
	return owners
}

// takeoverDeferredDir 重试接管一个 deferred 子树。路径位于被跟随的链接之下
// 时，按「真实目标监听 + 逻辑路径枚举」重试（与首次链接接管一致）；否则走
// 普通 takeoverDir。真实目标注册失败后若仍用逻辑路径重试，真实目标的 Add
// 永远不会再执行，监听也无法恢复。
func (w *JSONLWatcher) takeoverDeferredDir(ctx context.Context, dir string) ([]string, []string, error) {
	if realDir, ok := w.realDirForLogical(dir); ok {
		return w.takeoverDirRecursive(ctx, realDir, dir, true, strictVanishedDirs, nil)
	}
	return w.takeoverDir(ctx, dir, true, strictVanishedDirs)
}

// realDirForLogical 返回逻辑路径所属链接的真实目标：路径位于某个已登记别名
// 的逻辑路径之下时，解析符号链接得到真实目录；否则 ok=false（普通目录）。
func (w *JSONLWatcher) realDirForLogical(path string) (string, bool) {
	path = filepath.Clean(path)
	best := ""
	for _, logics := range w.linkMap {
		for logical := range logics {
			logical = filepath.Clean(logical)
			if pathWithin(logical, path) && len(logical) > len(best) {
				best = logical
			}
		}
	}
	if best == "" {
		return "", false
	}
	resolved := resolveSymlinkPath(path)
	if resolved == "" {
		return "", false
	}
	return resolved, true
}

// retargetLink 在链接重指后接管新的目标目录：按逻辑路径重新登记别名与监听，
// 并补采目标树内已存在的 transcript。失败子树进入 deferred：逻辑链接此刻已
// 指向登记的新目标，后续复核会跳过接管，只有 ticker 重试能把真实目标补上。
func (w *JSONLWatcher) retargetLink(logical, realPath string) {
	ctx := context.Background()
	existing, failed, err := w.takeoverDirRecursive(ctx, realPath, logical, true, tolerateVanishedDirs, nil)
	if err != nil {
		w.logger.Debug("retargeted symlink takeover failed", "logical", logical, "real", realPath, "error", err)
		if len(failed) > 0 {
			for _, f := range failed {
				w.deferred[f] = struct{}{}
			}
		} else {
			w.deferred[logical] = struct{}{}
		}
	}
	// 接管只登记监听；切换前已经落盘、之后不再变化的文件不会再有事件，必须
	// 显式补采——否则重指会漏掉这些存量 transcript。
	for _, f := range existing {
		w.debounce.Trigger(f)
	}
}

// dropLinkAlias 撤销一条链接别名并释放它独占的监听。同一真实目录可能被多个
// 逻辑入口共用（合法 projects/session/ 与另一个会话的 uploads/）：只删除当前
// 逻辑路径并解除它的持有，其他入口仍在映射中时保留监听；最后一个入口撤销后
// 才把真实路径记为失效目标，旧目标里的残留事件不再被采集。
// 监听以真实路径登记，而事件与逻辑路径可能以 /var 形态出现，释放必须按真实
// 路径遍历该子树的 watched 记录；释放失败的条目留在 unconfirmed，由重建接管
// 路径先确认再重新 Add。
func (w *JSONLWatcher) dropLinkAlias(realPath, logical string) {
	realPath = filepath.Clean(realPath)
	logical = filepath.Clean(logical)
	set, ok := w.linkMap[realPath]
	if !ok {
		return
	}
	if _, held := set[logical]; !held {
		return
	}
	delete(set, logical)
	w.logger.Debug("released symlink alias", "real", realPath, "logical", logical)
	// 只释放本链接独占的监听：主项目根可能同时监听同一棵真实目录（会话里的
	// uploads/ 链到 projects_dir 正是这种形态），无条件释放会把主根的监听一起
	// 摘掉，之后主根完全收不到事件。
	w.releaseLinkOwnedWatches(realPath, logical)
	if len(set) == 0 {
		delete(w.linkMap, realPath)
		w.staleLinkTargets[realPath] = struct{}{}
	}
}

// releaseLinkOwnedWatches 释放某条链接别名在 realPath 子树内独占的监听：
// 逐一解除该逻辑路径的持有，只有已无人使用的条目才 Remove。仍有其他持有者
// （采集根本身或另一条链接）的目录保持监听不动。
func (w *JSONLWatcher) releaseLinkOwnedWatches(realPath, logical string) {
	logical = filepath.Clean(logical)
	// 两处都要扫描：链接目标的真实子树（按真实路径登记的监听）以及链接
	// 之下的逻辑子树（递归进入普通子目录时按逻辑路径登记的监听）。
	paths := make(map[string]struct{})
	for _, p := range w.watchedSubtreePaths(realPath) {
		paths[p] = struct{}{}
	}
	for _, p := range w.watchedSubtreePaths(logical) {
		paths[p] = struct{}{}
	}
	for path := range paths {
		if !w.releaseOwner(path, logical) {
			continue
		}
		if err := w.removePathWatch(path); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			w.unconfirmed[path] = struct{}{}
			w.logger.Debug("stale link watch release failed, marked unconfirmed", "path", path, "error", err)
			continue
		}
		w.forgetWatch(path)
		delete(w.unconfirmed, path)
	}
}

// hasNativeCollectability 报告该路径本身（不经链接改写）是否就是一条可采集
// 的采集路径：落在某个监控根内且通过该 client 的受控判定。链接映射两处都要求
// 它——真实路径已具备资格时保留原归属，映射结果不具备资格时不做改写，避免
// 链接把另一个根的真实事件顶替成自己的逻辑路径。
func (w *JSONLWatcher) hasNativeCollectability(path string) bool {
	return w.isWithinTarget(path) && w.collectableJSONLPath(path)
}

// symlinkDirTarget 报告目录项是否为指向目录的符号链接，返回目标的真实路径。
// 普通目录与非链接项返回 ("", false)；断链或指向普通文件同样返回 false
// （跳过不报错）。是否跟随由 shouldFollowDirLink 决定。
func (w *JSONLWatcher) symlinkDirTarget(path string, e os.DirEntry) (string, bool) {
	if e.IsDir() || e.Type()&os.ModeSymlink == 0 {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		return resolved, true
	}
	return path, true
}

// shouldFollowDirLink 报告是否跟随该目录符号链接。只有 Cowork 覆盖范围内的
// 链接才跟随：会话目录的 .claude 指向采集面之外是 Cowork 的落盘形态，全扫按
// os.Stat 能采到，watcher 不跟随就会漏掉运行期写入；其他客户端（Codex 等）
// 的目录链接不扩大监听范围，保持跟随前的行为。
func (w *JSONLWatcher) shouldFollowDirLink(linkPath, realPath string) bool {
	if w.clientName != "claude" {
		return false
	}
	if collector.ClaudeCoworkDirHoldsTranscript(w.coworkRoots, linkPath) {
		return true
	}
	return collector.ClaudeCoworkDirHoldsTranscript(w.coworkRoots, realPath)
}

// resolveSymlinkPath 返回 path 解析符号链接后的绝对 Clean 形态；解析失败时
// 退回原路径。用于递归判环和采集根归属匹配：符号链接在 macOS 上会产生
// /private/var 与 /var 两种形态，不归一会让同一采集根的两条入口判定分叉。
func resolveSymlinkPath(path string) string {
	if path == "" {
		return path
	}
	resolved := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		resolved = real
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return filepath.Clean(resolved)
}

func (w *JSONLWatcher) addWatch(path string) error {
	return w.addWatchOwned(path, "")
}

// addWatchOwned 登记监听并记录全部所有者（空串表示采集根自身的接管）。
// 同一真实目录可能被多处同时监听：主项目根、指向它的 Cowork 链接、以及多个
// 逻辑入口共享同一目标时，watched 里只有一条记录，必须先记下谁需要它，撤销
// 其中一方时才不会把另一方仍在使用的监听一起释放。
func (w *JSONLWatcher) addWatchOwned(path string, owners ...string) error {
	// 所有者按真实形态归一：macOS 上同一目录会以 /var 与 /private/var 两种写法
	// 出现，主根接管与链接跟随各用一种就会变成两条互不相认的记录，撤销链接时
	// 无法判断主根是否还在使用同一目录。监听路径本身保持不变，避免改变既有
	// 根注册与事件路径的语义。
	path = filepath.Clean(path)
	if _, unconfirmed := w.unconfirmed[path]; unconfirmed {
		// 旧监听是否已释放未确认：绕过 watched 快速返回，先重试释放再重新
		// 登记——否则同名目录重建时会把可能仍存活于后端的旧监听当成已恢复。
		// 仍无法确认释放则按真实错误上抛，交由强根失败/可选根降级策略处置。
		if err := w.removePathWatch(path); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("release stale watch for %s", path), fmt.Sprintf("释放旧监听 %s", path)), err)
		}
		delete(w.unconfirmed, path)
		w.forgetWatch(path)
	} else if _, ok := w.watched[path]; ok {
		w.retainOwners(path, owners)
		return nil
	}
	if err := w.addPathWatch(path); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi(fmt.Sprintf("watch directory %s", path), fmt.Sprintf("监听目录 %s", path)), err)
	}
	w.watched[path] = struct{}{}
	w.retainOwners(path, owners)
	return nil
}

// retainOwners 为监听路径及其真实形态登记全部所有者：macOS 上同一目录会以
// /var 与 /private/var 两种写法出现，两种形态共享一份持有关系，撤销时才能
// 看全谁还在使用同一目录。
func (w *JSONLWatcher) retainOwners(path string, owners []string) {
	realPath := resolveSymlinkPath(path)
	if realPath != path {
		w.watchOwnerRealPaths[path] = realPath
	}
	for _, owner := range owners {
		w.retainOwner(path, owner)
		if realPath != path {
			w.retainOwner(realPath, owner)
		}
	}
}

// forgetWatch 清理监听和登记时的所有者键。真实路径仍被其它监听共享时保留该键。
func (w *JSONLWatcher) forgetWatch(path string) {
	delete(w.watched, path)
	realPath := w.watchOwnerRealPaths[path]
	delete(w.watchOwnerRealPaths, path)
	for _, key := range []string{path, realPath} {
		if key == "" {
			continue
		}
		if _, watched := w.watched[key]; watched {
			continue
		}
		shared := false
		for _, other := range w.watchOwnerRealPaths {
			if key == other {
				shared = true
				break
			}
		}
		if !shared {
			delete(w.watchOwners, key)
		}
	}
}

// retainOwner 记下一个监听持有的所有者（根接管用空串表示）。
func (w *JSONLWatcher) retainOwner(path, owner string) {
	if w.watchOwners == nil {
		w.watchOwners = make(map[string]map[string]struct{})
	}
	// 不能解析路径：逻辑路径在链接存活时会解析成真实目标，链接删除后解析
	// 结果变化，释放时按登记时的原始路径才能找到持有记录。
	path = filepath.Clean(path)
	set, ok := w.watchOwners[path]
	if !ok {
		set = make(map[string]struct{}, 2)
		w.watchOwners[path] = set
	}
	set[owner] = struct{}{}
	w.logger.Debug("retained watch owner", "path", path, "owner", owner)
}

// releaseOwner 解除 owner 对该路径的持有，返回监听是否已无人使用。owner 不在
// 持有集合里（例如该目录只被别的入口需要）时返回 false——绝不能把它当成
// 「无人使用」，否则会把其他入口仍在使用的监听一并释放。
func (w *JSONLWatcher) releaseOwner(path, owner string) bool {
	path = filepath.Clean(path)
	set, ok := w.watchOwners[path]
	if !ok {
		return false
	}
	if _, held := set[owner]; !held {
		return false
	}
	delete(set, owner)
	if len(set) > 0 {
		return false
	}
	delete(w.watchOwners, path)
	return true
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
		w.forgetWatch(path)
		delete(w.unconfirmed, path)
	}
	w.dropLinkAliases(root)
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
		w.forgetWatch(p)
	}
	// 失效子树同时撤销链接别名：链接被删除或重指后，旧目标下残留的监听还会
	// 继续给事件，若别名仍在，事件会被改写成已失效的逻辑路径而被采集成新文件。
	w.dropLinkAliases(path)
	return true
}

// dropLinkAliases 撤销 root 子树相关的链接别名（真实路径或逻辑路径落在 root
// 内的都算），使后续事件不再被改写到失效或已重指的逻辑位置。返回被撤销的
// 别名个数。
func (w *JSONLWatcher) dropLinkAliases(root string) int {
	if len(w.linkMap) == 0 {
		return 0
	}
	type alias struct{ real, logical string }
	stale := make([]alias, 0, len(w.linkMap))
	for realPath, logics := range w.linkMap {
		for logical := range logics {
			if pathWithin(root, realPath) || pathWithin(root, logical) {
				stale = append(stale, alias{realPath, logical})
			}
		}
	}
	for _, a := range stale {
		w.dropLinkAlias(a.real, a.logical)
	}
	return len(stale)
}

// invalidateLinksUnder 处理链接自身被删除或重指的事件：watched 以真实路径为
// 键，链接路径上来的 Remove/Rename 找不到对应条目，必须单独按逻辑路径撤销
// 别名并释放目标子树的监听，否则旧目标仍在监听表里，后续写入会继续产生事件
// 并被改写成已失效（或已重指）的逻辑路径。
func (w *JSONLWatcher) invalidateLinksUnder(path string) {
	if len(w.linkMap) == 0 {
		return
	}
	path = filepath.Clean(path)
	type alias struct{ real, logical string }
	var stale []alias
	for realPath, logics := range w.linkMap {
		for logical := range logics {
			if pathWithin(path, logical) {
				stale = append(stale, alias{realPath, logical})
			}
		}
	}
	for _, a := range stale {
		// 目标子树可能还有其它别名共享同一真实路径（多个链接指向同一目标），
		// dropLinkAlias 只在最后一个别名消失后才释放监听。
		w.dropLinkAlias(a.real, a.logical)
	}
}

// watchedSubtreePaths 快照 watched 中位于 root 子树（含根）内的路径，按相对
// root 的组件数降序（子路径先于父路径）、同深度按路径名升序排序，供失效
// 监听逐个释放；Remove 不递归，子路径必须先于父路径释放。
func (w *JSONLWatcher) watchedSubtreePaths(root string) []string {
	root = filepath.Clean(root)
	resolvedRoot := resolveSymlinkPath(root)
	paths := make([]string, 0, 8)
	depths := make(map[string]int)
	for watched := range w.watched {
		realPath := w.watchOwnerRealPaths[watched]
		switch {
		case pathWithin(root, watched):
			depths[watched] = pathDepthWithin(root, watched)
		case pathWithin(resolvedRoot, watched):
			depths[watched] = pathDepthWithin(resolvedRoot, watched)
		case realPath != "" && pathWithin(resolvedRoot, realPath):
			depths[watched] = pathDepthWithin(resolvedRoot, realPath)
		default:
			continue
		}
		paths = append(paths, watched)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := depths[paths[i]], depths[paths[j]]
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

// takeoverCreatedDir 接管运行期新建的目录树。新路径是符号链接且位于 Cowork
// 跟随范围时，复用启动递归的链接规则：监听挂在解析后的真实目标上、枚举与
// 上报仍走采集根之下的逻辑路径，并登记别名。否则目标在根外，后续目录/文件
// 事件无法还原逻辑路径，目标里新建的 transcript 会整份漏采。
func (w *JSONLWatcher) takeoverCreatedDir(ctx context.Context, path string) ([]string, []string, error) {
	if realDir, isLink := w.symlinkDirTargetPath(path); isLink && w.shouldFollowDirLink(path, realDir) {
		return w.takeoverDirRecursive(ctx, realDir, path, true, tolerateVanishedDirs, nil)
	}
	return w.takeoverDir(ctx, path, true, tolerateVanishedDirs)
}

// symlinkDirTargetPath 报告 path 本身是否为指向目录的符号链接，返回目标的
// 真实路径。与 symlinkDirTarget 的区别是不依赖父目录枚举的 DirEntry，供
// 运行期目录事件（事件路径本身可能是刚创建的链接）使用。
func (w *JSONLWatcher) symlinkDirTargetPath(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	}
	return path, true
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
			// 竞态，跳过而不是把整个 watcher 拖停。新建路径是符号链接时
			// 复用启动递归的链接规则（真实路径监听 + 逻辑路径枚举 + 别名）。
			existing, failed, err := w.takeoverCreatedDir(ctx, path)
			// 动态接管可能带来新的采集根（如运行期新增 cc-switch 账号）：
			// 刷新受控判定根集合，后续 meta 映射与 .jsonl 事件按新根判定。
			w.refreshRoots()
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

// isRelevantDir 报告目录是否与某个监控根有包含关系（接管递归的进入判定）。
// 目录可能是符号链接（会话目录的 .claude 指向采集面之外），也可能采集根本身
// 经链接访问；全扫按 os.Stat 跟随链接能采到这类 transcript，watcher 若不跟随
// 就会漏掉运行期写入。因此字面路径与解析链接后的形态各比一次。
func (w *JSONLWatcher) isRelevantDir(path string) bool {
	roots := w.rootForms()
	for _, path := range pathForms(path) {
		for _, root := range roots {
			if pathWithin(root, path) || pathWithin(path, root) {
				return true
			}
		}
	}
	return false
}

// isWithinTarget 报告路径是否落在某个监控根之内（字面与解析形态都比对）。
func (w *JSONLWatcher) isWithinTarget(path string) bool {
	for _, root := range w.rootForms() {
		for _, form := range pathForms(path) {
			if pathWithin(root, form) {
				return true
			}
		}
	}
	return false
}

// rootForms 返回监控根的两种形态：登记的绝对路径 + 解析符号链接后的真实路径
// （同一目录的两种写法，符号链接根与 macOS 的 /var、/private/var 都属此类）。
func (w *JSONLWatcher) rootForms() []string {
	if len(w.dirsResolved) == 0 {
		return w.dirs
	}
	forms := make([]string, 0, len(w.dirs)+len(w.dirsResolved))
	forms = append(forms, w.dirs...)
	return append(forms, w.dirsResolved...)
}

// pathForms 返回路径的两种形态（原始绝对形态 + 解析符号链接后的形态），供
// 根归属匹配同时覆盖「事件按真实路径产生、根按链接路径登记」及其反向情形。
func pathForms(path string) []string {
	abs := path
	if a, err := filepath.Abs(path); err == nil {
		abs = a
	}
	abs = filepath.Clean(abs)
	resolved := resolveSymlinkPath(path)
	if resolved == abs {
		return []string{abs}
	}
	return []string{abs, resolved}
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
