package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
	_ "modernc.org/sqlite"
)

// CodexCollector 从 Codex state DB（会话元数据 + rollout_path）和 rollout JSONL
// （逐消息 last_token_usage）采集消息级 token。
//
// 四种请求模式：
//   - ChangedFile：只解析该 rollout；fallback metadata 为空，由 session_meta 提供
//     ID/client/cwd/ParentID/source/originator。
//   - ScanExistingJSONL：递归扫描 sessions_dir 与同级 archived_sessions，不依赖 state DB。
//   - CLI Dates：读取全部 state thread，按每个 rollout 的 Message.Date 过滤。
//   - Incremental：对每个 state DB 使用 (updated_at_ms,id) 条件查询变更 threads，
//     仅扫描返回的 rollout_path；NextCursor 为所有成功扫描 thread 的最大复合键。
type CodexCollector struct {
	cfg         *config.Config
	stateDir    string
	sessionsDir string
}

func NewCodexCollector(cfg *config.Config) *CodexCollector {
	stateDir := ""
	sessionsDir := ""

	if cfg != nil {
		if clientCfg, ok := cfg.ClientConfig("codex"); ok {
			stateDir = clientCfg.Paths["state_dir"]
			sessionsDir = clientCfg.Paths["sessions_dir"]
		}
	}
	// 默认路径由 runtimecfg.ResolveEffectiveConfig 在装配期回填（effective config），此处不再补 local 默认。
	return &CodexCollector{
		cfg:         cfg,
		stateDir:    stateDir,
		sessionsDir: sessionsDir,
	}
}

func (c *CodexCollector) Name() string {
	return "codex"
}

func (c *CodexCollector) SyncSources() []string { return []string{SyncSourceCodexState} }

// Collect 按请求模式采集 Codex 消息级 token。
//
// ChangedFile 模式：只解析 req.ChangedFile 指向的 rollout，不需要 state DB。
// CLI Dates 模式：读全部 state DB，逐 rollout 解析后按 Message.Date 过滤。
// Incremental 模式：按 (updated_at_ms,id) 复合游标增量查 threads，仅扫描返回 rollout_path，
// 返回 NextCursor（所有成功扫描 thread 的最大复合键）。
//
// state DB 全部读取失败 → 终止性 error；单个 DB 失败 → 降级（有任一成功即继续）。
// ctx 取消 → 返回 context error，不把不完整批次落库。
func (c *CodexCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if c == nil {
		return CollectResult{}, fmt.Errorf("Codex collector 不能为空")
	}
	// ChangedFile 模式：只解析该 rollout，无需 state DB。
	if req.ChangedFile != "" {
		return c.collectChangedFile(ctx, req.ChangedFile, logger)
	}
	// startup catch-up 的 rollout 全扫必须显式走 sessions_dir，不能退化成“读取
	// state threads 再扫其 rollout_path”，否则尚未入 state DB 的现存 JSONL 会永久漏采。
	if req.ScanExistingJSONL {
		return c.collectExistingJSONL(ctx, logger)
	}

	stateInfo, statErr := os.Stat(c.stateDir)
	if os.IsNotExist(statErr) {
		// state 目录不存在：无数据可采，返回空。
		return CollectResult{}, nil
	}
	if statErr != nil {
		return CollectResult{}, fmt.Errorf("访问 Codex state_dir %s 失败: %w", c.stateDir, statErr)
	}
	if !stateInfo.IsDir() {
		return CollectResult{}, fmt.Errorf("Codex state_dir 不是目录: %s", c.stateDir)
	}

	stateDBs, err := findStateDBs(c.stateDir)
	if err != nil {
		return CollectResult{}, err
	}
	if len(stateDBs) == 0 {
		return CollectResult{}, nil
	}

	dateSet := make(map[string]struct{}, len(req.Dates))
	for _, d := range req.Dates {
		dateSet[d] = struct{}{}
	}

	var (
		result     CollectResult
		inCursor   = req.Cursors[SyncSourceCodexState]
		nextCur    = inCursor
		hasNext    bool
		dbSuccess  int
		partialErr error
	)
	for _, dbPath := range stateDBs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var threads []codexThread
		var err error
		if req.Incremental {
			// 多 state DB 都从同一输入 cursor 查询，合并后取全局最大值。
			threads, err = readThreadsIncremental(ctx, dbPath, inCursor)
		} else {
			threads, err = readThreads(ctx, dbPath)
		}
		if err != nil {
			logger.Warn("Codex state DB 查询失败，跳过", "db_path", dbPath, "error", err)
			partialErr = errors.Join(partialErr, fmt.Errorf("%s: %w", dbPath, err))
			continue
		}
		dbSuccess++
		for _, th := range threads {
			if th.RolloutPath == "" {
				if req.Incremental {
					nextCur, hasNext = advanceCodexCursor(nextCur, hasNext, th)
				}
				continue
			}
			info, err := os.Stat(th.RolloutPath)
			if err != nil {
				wrapped := fmt.Errorf("%s: %w", th.RolloutPath, err)
				partialErr = errors.Join(partialErr, wrapped)
				logger.Warn("Codex rollout_path 访问失败，跳过", "rollout_path", th.RolloutPath, "error", err)
				continue
			}
			if !info.Mode().IsRegular() {
				wrapped := fmt.Errorf("%s 不是普通文件", th.RolloutPath)
				partialErr = errors.Join(partialErr, wrapped)
				logger.Warn("Codex rollout_path 不是普通文件，跳过", "rollout_path", th.RolloutPath)
				continue
			}
			part, perr := parseCodexRolloutContext(ctx, th.RolloutPath, th, dateSet)
			if perr != nil {
				if errors.Is(perr, context.Canceled) || errors.Is(perr, context.DeadlineExceeded) {
					return result, perr
				}
				partialErr = errors.Join(partialErr, fmt.Errorf("%s: %w", th.RolloutPath, perr))
				logger.Warn("Codex rollout 解析失败，跳过", "rollout_path", th.RolloutPath, "error", perr)
				continue
			}
			result.Messages = append(result.Messages, part.Messages...)
			// 仅在命中消息时携带 Session：parseCodexRollout 即使无命中消息也会构造
			// Session（FirstTS/LastTS 回退到 state 字段），在 CLI Dates 模式下会用空数据
			// 经 INSERT OR REPLACE 覆盖已有会话，故这里必须丢弃。
			if len(part.Messages) > 0 {
				result.Sessions = append(result.Sessions, part.Sessions...)
			}
			if req.Incremental {
				nextCur, hasNext = advanceCodexCursor(nextCur, hasNext, th)
			}
		}
	}
	// 存在 state DB 但全部读取失败时，必须返回终止性错误。
	if dbSuccess == 0 {
		return CollectResult{}, errors.Join(
			fmt.Errorf("全部 %d 个 Codex state DB 读取失败，无法采集", len(stateDBs)),
			partialErr,
		)
	}

	result.PartialErr = partialErr
	if req.Incremental && partialErr == nil {
		// 无新 thread 时 hasNext=false，nextCur 保持输入 cursor，避免回退。
		result.NextCursors = map[string]model.SyncCursor{SyncSourceCodexState: nextCur}
	}
	return result, nil
}

func advanceCodexCursor(current model.SyncCursor, hasCurrent bool, th codexThread) (model.SyncCursor, bool) {
	if !hasCurrent || th.UpdatedAtMS > current.Value ||
		(th.UpdatedAtMS == current.Value && th.ID > current.ID) {
		return model.SyncCursor{Value: th.UpdatedAtMS, ID: th.ID}, true
	}
	return current, hasCurrent
}

// collectChangedFile 只解析单个 rollout 文件，fallback metadata 为空，
// 由 rollout 内的 session_meta 提供 ID/client/cwd/ParentID/source/originator。
func (c *CodexCollector) collectChangedFile(ctx context.Context, path string, logger *slog.Logger) (CollectResult, error) {
	// fallback 为空 codexThread：所有字段零值，session_meta 将回填。
	part, err := parseCodexRolloutContext(ctx, path, codexThread{}, map[string]struct{}{})
	if err != nil {
		return CollectResult{}, err
	}
	return part, nil
}

// collectExistingJSONL 递归扫描 Codex 当前 sessions_dir 与同级 archived_sessions。
// 单文件损坏时保留其他成功文件，并通过 PartialErr 交给 engine 在落库后记录失败。
func (c *CodexCollector) collectExistingJSONL(ctx context.Context, logger *slog.Logger) (CollectResult, error) {
	files, scanErr := findExistingCodexJSONLContext(ctx, c.sessionsDir)
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}
	result := CollectResult{PartialErr: scanErr}
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		part, err := parseCodexRolloutContext(ctx, path, codexThread{}, map[string]struct{}{})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			wrapped := fmt.Errorf("%s: %w", path, err)
			result.PartialErr = errors.Join(result.PartialErr, wrapped)
			logger.Warn("Codex 现存 rollout 解析失败，跳过", "rollout_path", path, "error", err)
			continue
		}
		result.Messages = append(result.Messages, part.Messages...)
		result.Sessions = append(result.Sessions, part.Sessions...)
	}
	return result, nil
}

func findExistingCodexJSONL(sessionsDir string) ([]string, error) {
	return findExistingCodexJSONLContext(context.Background(), sessionsDir)
}

func findExistingCodexJSONLContext(ctx context.Context, sessionsDir string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionsDir) == "" {
		return nil, fmt.Errorf("Codex sessions_dir 未配置")
	}
	sessionsDir = filepath.Clean(sessionsDir)
	roots := []string{
		sessionsDir,
		filepath.Join(filepath.Dir(sessionsDir), "archived_sessions"),
	}
	seenRoot := make(map[string]struct{}, len(roots))
	seenFile := make(map[string]struct{})
	var (
		files   []string
		scanErr error
	)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return files, err
		}
		root = filepath.Clean(root)
		if _, ok := seenRoot[root]; ok {
			continue
		}
		seenRoot[root] = struct{}{}

		info, err := os.Stat(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("访问 Codex JSONL 目录 %s: %w", root, err))
			continue
		}
		if !info.IsDir() {
			scanErr = errors.Join(scanErr, fmt.Errorf("Codex JSONL 路径不是目录: %s", root))
			continue
		}

		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("扫描 %s: %w", path, walkErr))
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			path = filepath.Clean(path)
			if _, ok := seenFile[path]; ok {
				return nil
			}
			seenFile[path] = struct{}{}
			files = append(files, path)
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return files, err
			}
			scanErr = errors.Join(scanErr, fmt.Errorf("扫描 Codex JSONL 目录 %s: %w", root, err))
		}
	}
	sort.Strings(files)
	return files, scanErr
}

// ===== rollout payload 类型 =====

// codexUsage 对应 event_msg.token_count.info.last_token_usage 结构。
// 同时也是 total_token_usage 的结构（两者字段完全一致）。
// 作为纯值类型，可直接用 == 比较，用作重播识别的 token 签名。
type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// 已知的 token 字段名（用于区分空对象 {} 与有效对象）。
// codex 偶发输出空对象 {} 的 total/last（仅含 rate_limits 的事件），
// 空对象不应参与去重；要求对象至少含一个已知 key 才视为有效。
var codexUsageKnownKeys = []string{
	"input_tokens", "cached_input_tokens", "cache_read_input_tokens",
	"output_tokens", "reasoning_output_tokens", "total_tokens",
}

// UnmarshalJSON 拒绝空对象 {}：要求至少含一个已知 token 字段。
// 解析失败时 *codexUsage 保持 nil，使上层把空对象当作「无 total/last」处理，
// 不参与去重判定（避免空对象误启用去重，吞掉后续合法事件）。
func (u *codexUsage) UnmarshalJSON(data []byte) error {
	type alias codexUsage // 避免递归
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	known := false
	for _, k := range codexUsageKnownKeys {
		if _, ok := raw[k]; ok {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("codexUsage 对象缺少已知 token 字段（空对象不启用去重）")
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*u = codexUsage(a)
	return nil
}

// isZero 判断是否全零（compaction 重置点识别）。
func (u codexUsage) isZero() bool {
	return u.InputTokens == 0 && u.CachedInputTokens == 0 &&
		u.OutputTokens == 0 && u.ReasoningOutputTokens == 0 &&
		u.TotalTokens == 0
}

// tokenCountPayload 对应 type=event_msg 的 token_count 子类型 payload。
// RateLimits.LimitID 标识限流桶：codex 切换限流桶时会重播同一份完整 token 快照，
// 按限流桶分通道比对签名才能识别这类重播。
//
// 注意：LastTokenUsage/TotalTokenUsage 用 *codexUsage 且依赖 codexUsage.UnmarshalJSON
// 拒绝空对象。json.Unmarshal 对结构体字段（非指针）遇到子解码失败会整体失败，
// 因此 Info 用命名结构体（而非匿名），并为 Info 实现 UnmarshalJSON 容错：任一字段
// 是空对象时该字段保持 nil、另一字段仍正常解析（不因一个空对象丢失整个事件）。
type tokenCountPayload struct {
	Type       string               `json:"type"`
	Info       tokenCountInfo       `json:"info"`
	RateLimits tokenCountRateLimits `json:"rate_limits"`
}

type tokenCountInfo struct {
	LastTokenUsage  *codexUsage `json:"last_token_usage"`
	TotalTokenUsage *codexUsage `json:"total_token_usage"`
}

// UnmarshalJSON 容错解析：last/total 任一为空对象 {} 时该字段保持 nil，
// 另一字段正常解析。逐字段独立判定，避免一个空对象导致整个事件被丢弃。
func (i *tokenCountInfo) UnmarshalJSON(data []byte) error {
	type alias tokenCountInfo
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var a alias
	if v, ok := raw["last_token_usage"]; ok {
		var lu codexUsage
		if json.Unmarshal(v, &lu) == nil {
			a.LastTokenUsage = &lu
		} // 空对象或解析失败：保持 nil
	}
	if v, ok := raw["total_token_usage"]; ok {
		var tu codexUsage
		if json.Unmarshal(v, &tu) == nil {
			a.TotalTokenUsage = &tu
		}
	}
	*i = tokenCountInfo(a)
	return nil
}

type tokenCountRateLimits struct {
	LimitID string `json:"limit_id"`
}

// tokenUsageSignature 是 (total,last) 的完整复合签名，用于重播识别。
// 同源与紧邻判定都基于完整签名（total+last），仅比 total 会误杀
// 「同 limit_id 下 total 相同、last 不同」的合法事件。
type tokenUsageSignature struct {
	total *codexUsage
	last  *codexUsage
}

// equal 判断两条复合签名是否相同（同时非 nil 且值相等）。
func (s tokenUsageSignature) equal(other tokenUsageSignature) bool {
	return ptrUsageEqual(s.total, other.total) && ptrUsageEqual(s.last, other.last)
}

func ptrUsageEqual(a, b *codexUsage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// computeDelta 用 total 累计高水位计算本轮增量。
// total-only 事件（无 last）走高水位差减：高水位为 nil 时 delta=current，否则 saturating_sub。
// total/reasoning 不参与差减（累计口径只追踪 input/cached/output 三字段）。
func computeDelta(highWater *codexUsage, current *codexUsage) codexUsage {
	if highWater == nil {
		return codexUsage{
			InputTokens:       current.InputTokens,
			CachedInputTokens: current.CachedInputTokens,
			OutputTokens:      current.OutputTokens,
		}
	}
	return codexUsage{
		InputTokens:       saturatingSub(current.InputTokens, highWater.InputTokens),
		CachedInputTokens: saturatingSub(current.CachedInputTokens, highWater.CachedInputTokens),
		OutputTokens:      saturatingSub(current.OutputTokens, highWater.OutputTokens),
	}
}

// updateHighWater 就地取 max 更新 total 累计高水位（input/cached/output 三字段）。
func updateHighWater(highWater *codexUsage, current *codexUsage) {
	if current.InputTokens > highWater.InputTokens {
		highWater.InputTokens = current.InputTokens
	}
	if current.CachedInputTokens > highWater.CachedInputTokens {
		highWater.CachedInputTokens = current.CachedInputTokens
	}
	if current.OutputTokens > highWater.OutputTokens {
		highWater.OutputTokens = current.OutputTokens
	}
}

func saturatingSub(a, b int64) int64 {
	if a < b {
		return 0
	}
	return a - b
}

// responseItemPayload 对应 type=response_item 的 payload。
type responseItemPayload struct {
	Type string `json:"type"`
	Role string `json:"role"`
	ID   string `json:"id"`
}

// turnContextPayload 对应 type=turn_context 的 payload。
type turnContextPayload struct {
	Model string `json:"model"`
}

// codexThread 对应 state DB threads 表结构。
// state DB 只提供会话元数据和 rollout_path 定位；token 来自 rollout JSONL。
type codexThread struct {
	ID           string `db:"id"`
	RolloutPath  string `db:"rollout_path"`  // 完整绝对路径，可指向 archived_sessions
	CreatedAt    int64  `db:"created_at"`    // 秒时间戳
	UpdatedAt    int64  `db:"updated_at"`    // 秒时间戳
	CreatedAtMS  int64  `db:"created_at_ms"` // 毫秒时间戳（可能为 0）
	UpdatedAtMS  int64  `db:"updated_at_ms"` // 毫秒时间戳（可能为 0）
	Source       string `db:"source"`        // cli / vscode
	Cwd          string `db:"cwd"`
	Title        string `db:"title"`
	Archived     int64  `db:"archived"`
	FirstUserMsg string `db:"first_user_message"`
	Model        string `db:"model"`
	ThreadSource string `db:"thread_source"` // subagent → 子代理
}

// rolloutEntry 对应 rollout JSONL 每行结构
type rolloutEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMetaPayload 对应 session_meta 的 payload 结构（平铺，非嵌套）。
type sessionMetaPayload struct {
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	Originator    string `json:"originator"`
	CLIVersion    string `json:"cli_version"`
	Source        string `json:"source"`
	ModelProvider string `json:"model_provider"`
	ForkedFromID  string `json:"forked_from_id"`
}

// ===== rollout 状态机 =====

// parseCodexRollout 解析单个 rollout JSONL，产出消息级 token 的 Messages 与 Session 元数据。
//
// 先解析 session_meta，与 fallback 合并出 sessionID/displayClient/cwd/ParentID；
// ChangedFile 模式下 fallback 为空，由 session_meta 提供。
//
// 状态机逐 entry 推进：
//   - turn_context 更新当前 model；
//   - response_item(message,assistant) 更新 lastAssistantID；
//   - event_msg(token_count) 关联到 lastAssistantID，跳过全零 usage（compaction 重置点），
//     同一 msg 多次非零 token_count 用 #序号 派生 ID。
//
// dates 非空时按 Message.Date 过滤；sessionID 缺失（fallback 与 session_meta 都无 id）时返回错误。
func parseCodexRollout(path string, fallback codexThread, dates map[string]struct{}) (CollectResult, error) {
	return parseCodexRolloutContext(context.Background(), path, fallback, dates)
}

func parseCodexRolloutContext(ctx context.Context, path string, fallback codexThread, dates map[string]struct{}) (CollectResult, error) {
	entries, err := parseRolloutJSONLContext(ctx, path)
	if err != nil {
		return CollectResult{}, err
	}

	// 合并 session_meta 与 fallback。
	sessionMeta := sessionMetaPayload{
		ID:     fallback.ID,
		Cwd:    fallback.Cwd,
		Source: fallback.Source,
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return CollectResult{}, err
		}
		if entry.Type != "session_meta" {
			continue
		}
		meta, merr := extractSessionMeta(entry)
		if merr == nil {
			sessionMeta = *meta
		}
	}

	sessionID := fallback.ID
	if sessionID == "" {
		sessionID = sessionMeta.ID
	}
	cwd := fallback.Cwd
	if cwd == "" {
		cwd = sessionMeta.Cwd
	}
	source := fallback.Source
	if source == "" {
		source = sessionMeta.Source
	}
	rawClient := inferClient(source, sessionMeta.Originator, fallback.ThreadSource)
	displayClient := model.RawClientToClient[rawClient]

	if sessionID == "" {
		return CollectResult{}, fmt.Errorf("rollout %s 缺少 session ID（state fallback 与 session_meta.id 均为空）", path)
	}

	currentModel := fallback.Model
	lastAssistantID := ""
	sequence := map[string]int{}

	// 重播去重状态。
	// codex 切换 rate_limits.limit_id 限流桶时会重播同一份完整 token 快照，
	// 逐条计入会导致用量虚高；用窄比对去重——只比对「同限流桶最近一条」和「紧邻上一条」，
	// 不做全表比对（否则会吞掉合法的计数器 reset 后再现旧值）。
	lastSigBySource := map[string]tokenUsageSignature{} // limit_id -> 上一条完整 (total,last) 签名
	prevTokenSig := tokenUsageSignature{}               // 紧邻上一条 (total,last) 复合签名
	var totalHighWater *codexUsage                      // total 累计高水位（跨 model/limit_id 全局）

	var (
		result   CollectResult
		firstTS  int64
		lastTS   int64
		parentID = sessionMeta.ForkedFromID
	)
	// fallback 可提供 ParentID（理论上 threads 表不存 forked_from_id，但保持扩展性）。

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return CollectResult{}, err
		}
		switch entry.Type {
		case "turn_context":
			var payload turnContextPayload
			if json.Unmarshal(entry.Payload, &payload) == nil && payload.Model != "" {
				currentModel = payload.Model
			}
		case "response_item":
			var payload responseItemPayload
			if json.Unmarshal(entry.Payload, &payload) == nil &&
				payload.Type == "message" && payload.Role == "assistant" {
				lastAssistantID = payload.ID
			}
		case "event_msg":
			var payload tokenCountPayload
			if json.Unmarshal(entry.Payload, &payload) != nil ||
				payload.Type != "token_count" {
				continue
			}
			last := payload.Info.LastTokenUsage
			total := payload.Info.TotalTokenUsage
			// 两者都无：非 token 计数事件（仅含 rate_limits），跳过。
			if last == nil && total == nil {
				continue
			}
			// 全零 last 且无 total：compaction 重置点，跳过且不污染游标。
			if last != nil && last.isZero() && total == nil {
				continue
			}
			// === 重播判定 ===
			// 仅在带有效 total 时启用（空对象 {} 已被 UnmarshalJSON 过滤为 nil）；
			// 纯 last 重复不去重——仅凭「本轮用量相同」不足以证明是重播。
			// 同源与紧邻判定都基于完整 (total,last) 签名，仅比 total 会误杀
			// 「同 limit_id 下 total 相同、last 不同」的合法事件。
			hasTotal := total != nil
			curSig := tokenUsageSignature{total: total, last: last}
			duplicate := hasTotal && (prevTokenSig.equal(curSig) || // 紧邻重播（含跨 limit_id）
				lastSigBySource[payload.RateLimits.LimitID].equal(curSig)) // 同源重播
			if hasTotal {
				lastSigBySource[payload.RateLimits.LimitID] = curSig
			}
			prevTokenSig = curSig
			if duplicate {
				continue // 重播：不计序号、不生成 Message、不推进高水位
			}
			// === delta 选取 ===
			// last 存在即优先用 last（哪怕全零——全零表示本轮无用量，由 zero-delta 抑制）；
			// 仅 last 缺失时才回退到 total-only 高水位差减。
			var usage codexUsage
			if last != nil {
				usage = *last
			} else if total != nil {
				usage = computeDelta(totalHighWater, total)
				// total-only 差减可能产生 input=0,cached>0 的脏组合（累计 cached 涨而 input 未涨），
				// clamp 到 cached<=input，避免落库 cache>input 的消息。
				// 仅对 total-only 差减结果 clamp：last 是 codex 显式给出的本轮用量，信任原值。
				if usage.CachedInputTokens > usage.InputTokens {
					usage.CachedInputTokens = usage.InputTokens
				}
			} else {
				continue
			}
			// === 高水位推进 ===
			// 只要带有效 total 就推进，不论本条走 last 还是 total 差减路径。
			// 否则「last=100,total=100」后接「空last,total=150」会算成 100+150，
			// 正确应为 100+(150-100)=100+50。
			if total != nil {
				if totalHighWater == nil {
					totalHighWater = &codexUsage{}
				}
				updateHighWater(totalHighWater, total)
			}
			// === zero delta 抑制序号 ===
			// 全零 delta（显式零 last，或 total-only 高水位未涨）不分配序号、不生成 Message，
			// 避免 zero token 消息污染库。
			if usage.isZero() {
				continue
			}
			if lastAssistantID == "" {
				continue
			}
			index := sequence[lastAssistantID]
			sequence[lastAssistantID] = index + 1

			ts, err := parseCodexTimestamp(entry.Timestamp)
			if err != nil {
				return CollectResult{}, err
			}
			message := codexMessageFromUsage(ts, lastAssistantID, index,
				sessionID, displayClient, currentModel, cwd, usage)
			// 追踪 rollout 时间范围（用于 Session FirstTS/LastTS）。
			if firstTS == 0 || ts < firstTS {
				firstTS = ts
			}
			if ts > lastTS {
				lastTS = ts
			}
			if len(dates) > 0 {
				if _, ok := dates[message.Date]; !ok {
					continue
				}
			}
			result.Messages = append(result.Messages, message)
		}
	}

	// 构造 Session 元数据。
	// FirstTS/LastTS 优先 state 字段，缺失时取 rollout 全部事件时间范围。
	sessFirst := threadEffectiveTS(fallback.CreatedAt, fallback.CreatedAtMS)
	sessLast := threadEffectiveTS(fallback.UpdatedAt, fallback.UpdatedAtMS)
	if sessFirst == 0 {
		sessFirst = firstTS
	}
	if sessLast == 0 {
		sessLast = lastTS
	}
	// 合并 ParentID：fallback.ThreadSource 不影响；session_meta.forked_from_id 优先。
	mergedParent := parentID
	result.Sessions = []model.Session{{
		ID:        sessionID,
		Client:    displayClient,
		Directory: cwd,
		Project:   codexProjectName(cwd),
		Title:     fallback.Title,
		FirstTS:   sessFirst,
		LastTS:    sessLast,
		ParentID:  mergedParent,
	}}

	return result, nil
}

// codexMessageFromUsage 将单个非零 last_token_usage 转为 Message。
// total：使用源值；源为 0 但有 input/output 时回退 input+output。
// fresh = input - cached（cacheCreate 为 0）。
func codexMessageFromUsage(ts int64, messageID string, index int,
	sessionID, client, currentModel, cwd string, usage codexUsage) model.Message {
	total := usage.TotalTokens
	if total == 0 && (usage.InputTokens != 0 || usage.OutputTokens != 0) {
		total = usage.InputTokens + usage.OutputTokens
	}
	return model.Message{
		ID:               fmt.Sprintf("%s#%d", messageID, index),
		SessionID:        sessionID,
		Client:           client,
		Date:             codexTsMsToDate(ts),
		TS:               ts,
		Model:            currentModel,
		Directory:        cwd,
		Project:          codexProjectName(cwd),
		InputTokens:      usage.InputTokens,
		FreshInputTokens: model.SubtractCache(usage.InputTokens, usage.CachedInputTokens, 0),
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CachedInputTokens,
		ReasoningTokens:  usage.ReasoningOutputTokens,
		TotalTokens:      total,
	}
}

// parseCodexTimestamp 解析 RFC3339Nano 时间戳为毫秒。
func parseCodexTimestamp(timestamp string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("解析 Codex token_count 时间失败: %w", err)
	}
	return parsed.UnixMilli(), nil
}

// ===== state DB 查询 =====

// findStateDBs 查找 state 目录下所有 state_*.sqlite 文件
func findStateDBs(stateDir string) ([]string, error) {
	info, err := os.Stat(stateDir)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("state 目录不存在: %s", stateDir)
	}
	if err != nil {
		return nil, fmt.Errorf("访问 state 目录失败: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state 路径不是目录: %s", stateDir)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("读取 state 目录失败: %w", err)
	}
	matches := make([]string, 0)
	for _, entry := range entries {
		matched, matchErr := filepath.Match("state_*.sqlite", entry.Name())
		if matchErr != nil {
			return nil, fmt.Errorf("匹配 state DB 文件名失败: %w", matchErr)
		}
		if !matched {
			continue
		}
		path := filepath.Join(stateDir, entry.Name())
		fileInfo, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("访问 state DB %s 失败: %w", path, statErr)
		}
		if fileInfo.Mode().IsRegular() {
			matches = append(matches, path)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// readThreads 从 state DB 读取所有 threads（只读元数据 + rollout_path，不再读 tokens_used）。
// 使用 COALESCE 处理可空列，按 (updated_at_ms,id) 排序便于增量游标推进。
func readThreads(ctx context.Context, dbPath string) ([]codexThread, error) {
	return queryThreads(ctx, dbPath, `SELECT COALESCE(id,''),COALESCE(rollout_path,''),COALESCE(cwd,''),COALESCE(title,''),
       COALESCE(created_at,0),COALESCE(updated_at,0),
       COALESCE(created_at_ms,0),COALESCE(updated_at_ms,0),
       COALESCE(source,''),COALESCE(archived,0),COALESCE(first_user_message,''),
       COALESCE(model,''),COALESCE(thread_source,'')
FROM threads
ORDER BY updated_at_ms,id`, nil)
}

// readThreadsIncremental 按 (updated_at_ms,id) 复合游标增量查询 threads。
func readThreadsIncremental(ctx context.Context, dbPath string, cursor model.SyncCursor) ([]codexThread, error) {
	query := `SELECT COALESCE(id,''),COALESCE(rollout_path,''),COALESCE(cwd,''),COALESCE(title,''),
       COALESCE(created_at,0),COALESCE(updated_at,0),
       COALESCE(created_at_ms,0),COALESCE(updated_at_ms,0),
       COALESCE(source,''),COALESCE(archived,0),COALESCE(first_user_message,''),
       COALESCE(model,''),COALESCE(thread_source,'')
FROM threads
WHERE updated_at_ms>? OR (updated_at_ms=? AND id>?)
ORDER BY updated_at_ms,id`
	args := []interface{}{cursor.Value, cursor.Value, cursor.ID}
	return queryThreads(ctx, dbPath, query, args)
}

// queryThreads 执行查询并扫描 codexThread。
func queryThreads(ctx context.Context, dbPath, query string, args []interface{}) ([]codexThread, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 state DB 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 threads 失败: %w", err)
	}
	defer rows.Close()

	var threads []codexThread
	for rows.Next() {
		var t codexThread
		if err := rows.Scan(
			&t.ID, &t.RolloutPath, &t.Cwd, &t.Title,
			&t.CreatedAt, &t.UpdatedAt,
			&t.CreatedAtMS, &t.UpdatedAtMS,
			&t.Source, &t.Archived, &t.FirstUserMsg,
			&t.Model, &t.ThreadSource,
		); err != nil {
			return nil, fmt.Errorf("扫描 thread 失败: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// ===== rollout JSONL 解析 =====

// parseRolloutJSONL 解析 rollout JSONL 文件
func parseRolloutJSONL(path string) ([]rolloutEntry, error) {
	return parseRolloutJSONLContext(context.Background(), path)
}

func parseRolloutJSONLContext(ctx context.Context, path string) ([]rolloutEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 rollout JSONL 失败: %w", err)
	}
	defer file.Close()

	var entries []rolloutEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineSize)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Text()
		if line == "" {
			continue
		}

		var entry rolloutEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // 跳过解析失败的行
		}
		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// extractSessionMeta 从 rolloutEntry 提取 session_meta payload
func extractSessionMeta(entry rolloutEntry) (*sessionMetaPayload, error) {
	if entry.Type != "session_meta" {
		return nil, fmt.Errorf("entry type is %q, not session_meta", entry.Type)
	}
	var meta sessionMetaPayload
	if err := json.Unmarshal(entry.Payload, &meta); err != nil {
		return nil, fmt.Errorf("解析 session_meta payload 失败: %w", err)
	}
	return &meta, nil
}

// ===== 辅助函数 =====

func codexProjectName(directory string) string {
	return projectBase(directory)
}

// inferClient 推断客户端类型。
// 优先级：originator > source > 默认 CLI。
// threadSource 用于区分子代理（subagent），不影响 CLI/App 判断。
func inferClient(source, originator, threadSource string) string {
	originatorLower := strings.ToLower(originator)

	if strings.Contains(originatorLower, "desktop") {
		return model.RawClientCodexApp
	}
	if strings.Contains(originatorLower, "cli") || strings.Contains(originatorLower, "tui") {
		return model.RawClientCodexCLI
	}

	switch source {
	case "vscode":
		return model.RawClientCodexApp
	case "cli":
		return model.RawClientCodexCLI
	}

	return model.RawClientCodexCLI
}

// threadEffectiveTS 返回线程的有效毫秒时间戳。
// 优先使用毫秒字段（如果非零），否则秒字段 × 1000
func threadEffectiveTS(sec, ms int64) int64 {
	if ms > 0 {
		return ms
	}
	if sec > 0 {
		return sec * 1000
	}
	return 0
}

// codexTsMsToDate 毫秒时间戳转日期字符串。
// 独立定义：遵循客户端文件独立性原则，不复用其他 collector 的 helper。
func codexTsMsToDate(tsMs int64) string {
	if tsMs <= 0 {
		return ""
	}
	t := time.UnixMilli(tsMs)
	return t.Format("2006-01-02")
}
