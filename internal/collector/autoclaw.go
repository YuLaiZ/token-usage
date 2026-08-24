package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// AutoClawCollector 采集 Zhipu-AutoClaw 的逐次 LLM 调用 token 用量。
// 数据源是明文 JSONL：~/.openclaw-autoclaw/agents/{agentId}/sessions/{sessionId}.jsonl。
// 属 JSONL 型 collector（仿 workbuddy），非 SQLite 型。
type AutoClawCollector struct {
	cfg *config.Config

	// walkFn / parseFn 仅供包内测试注入确定性 seam（构造时为 nil，Collect 走生产路径）。
	// walkFn 注入目录遍历器（测试可模拟 Walk 错误/取消）；parseFn 注入文件解析器
	// （测试可模拟 parser 返回 ctx.Canceled 或部分结果 + error）。
	walkFn  autoClawWalker
	parseFn func(ctx context.Context, path string, logger *slog.Logger) ([]autoclawParsedMessage, error)
}

// NewAutoClawCollector 创建 AutoClaw 采集器。
func NewAutoClawCollector(cfg *config.Config) *AutoClawCollector {
	return &AutoClawCollector{cfg: cfg}
}

func (c *AutoClawCollector) Name() string { return "autoclaw" }

// SyncSources 返回 nil（JSONL 全扫型，无增量游标）。
func (c *AutoClawCollector) SyncSources() []string { return nil }

// Collect 按 CLI 日期过滤、ChangedFile 单文件或全量扫描采集消息级 token。
// req.ChangedFile 非空时只采集该文件（daemon 增量）；否则扫描 sessions_dir 下所有合法 messages JSONL。
func (c *AutoClawCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if c == nil || c.cfg == nil {
		return CollectResult{}, fmt.Errorf("AutoClaw collector 配置为空")
	}
	clientCfg, ok := c.cfg.ClientConfig("autoclaw")
	if !ok || !clientCfg.Enabled {
		return CollectResult{}, nil
	}

	sessionsDir := clientCfg.Paths["sessions_dir"]
	// 空白 sessions_dir 拒绝：filepath.Abs("") 返回当前工作目录（err=nil），
	// 会让 ChangedFile 校验和扫描误判。仅含空白同样不应被视为真实目录。
	if strings.TrimSpace(sessionsDir) == "" {
		return CollectResult{}, nil
	}

	// 1. 文件列表：ChangedFile 优先（daemon 增量），否则扫描 sessions 目录。
	var files []string
	var scanErr error
	if req.ChangedFile != "" {
		// 单文件：先按三层路径 + trajectory 拒绝校验，不通过则返回空结果。
		if !isAutoClawValidChangedFile(req.ChangedFile, sessionsDir) {
			return CollectResult{}, nil
		}
		files = []string{req.ChangedFile}
	} else {
		files, scanErr = c.scanSessions(ctx, sessionsDir)
		if scanErr != nil && (errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded)) {
			// ctx 取消直接返回，不降级为 PartialErr
			return CollectResult{}, scanErr
		}
	}
	if len(files) == 0 {
		var result CollectResult
		// files 为空但有 scanErr 时不能丢掉该 PartialErr
		if scanErr != nil {
			result.PartialErr = scanErr
		}
		return result, nil
	}

	// 2. 日期过滤集合（dates 为空时不过滤，即全量）
	dateSet := make(map[string]bool, len(req.Dates))
	for _, d := range req.Dates {
		dateSet[d] = true
	}

	// 3. 逐文件解析 + 部分结果保留。
	var result CollectResult
	if scanErr != nil {
		// 非 ctx 扫描错误先记入 PartialErr，但继续解析已发现的 files。
		result.PartialErr = scanErr
	}
	// providerMap 按 agent 缓存：同一 agent 的多个 session 文件共享一份 models.json 解析结果，
	// 避免重复 IO 与反序列化（仍按 agent 隔离，不跨 agent 合并）。
	providerCache := make(map[string]map[string]autoclawProviderInfo)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			// 逐文件循环开头检查 ctx；取消立即返回已收集结果 + ctx.Err。
			return result, err
		}

		// 解析 agentId（从路径 rel 到 sessions_dir）
		agentID := autoclawAgentIDFromFile(file, sessionsDir)

		// 加载该 agent 的 models.json（按 agent 隔离，命中缓存则复用）
		providerMap, ok := providerCache[agentID]
		if !ok {
			providerMap = loadAutoClawProviderMap(sessionsDir, agentID)
			providerCache[agentID] = providerMap
		}

		// 解析该文件全部有效行（ctx 透传到 scanner 循环）
		pmsgs, parseErr := c.parseFile(ctx, file, logger)
		if parseErr != nil {
			if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
				// ctx 取消：直接返回 ctx.Err，不降级为 PartialErr
				return result, parseErr
			}
			logger.Warn("AutoClaw JSONL file partially parsed, keeping parsed messages and recording error", "file", file, "error", parseErr)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, parseErr))
			// parseErr 时 parser 仍返回已解析的 pmsgs（部分结果保留），继续处理这些消息再记 PartialErr（不 continue 丢弃）。
		}
		if len(pmsgs) == 0 {
			continue
		}

		// 先从全部有效消息计算 FirstTS/LastTS（全文件范围，不被日期过滤缩窄）
		var firstTS, lastTS int64
		for _, pm := range pmsgs {
			if pm.Timestamp > 0 {
				if firstTS == 0 || pm.Timestamp < firstTS {
					firstTS = pm.Timestamp
				}
				if pm.Timestamp > lastTS {
					lastTS = pm.Timestamp
				}
			}
		}

		fileSessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		// 按 providerMap 转换 + 按日期筛出 hitMessages
		var hitMessages []model.Message
		for _, pm := range pmsgs {
			m := autoClawMessage(fileSessionID, pm, providerMap)
			if len(req.Dates) > 0 && !dateSet[m.Date] {
				continue
			}
			hitMessages = append(hitMessages, m)
		}
		if len(hitMessages) == 0 {
			continue
		}

		result.Messages = append(result.Messages, hitMessages...)
		// 仅当该文件至少有一条 Message 命中日期时产出 Session；
		// FirstTS/LastTS 取全文件有效消息范围，不受日期过滤影响。
		result.Sessions = append(result.Sessions, model.Session{
			ID:        fileSessionID,
			Client:    model.ClientZhipuAutoClaw,
			Directory: pmsgs[0].Cwd,
			Project:   autoclawInferProject(pmsgs[0].Cwd),
			FirstTS:   firstTS,
			LastTS:    lastTS,
		})
	}

	return result, nil
}

// isAutoClawValidChangedFile 校验 ChangedFile 是否为合法的 messages JSONL。
// 合法 = 恰好 {agentId}/sessions/{sessionId}.jsonl 三层 + 非 trajectory + 非空 sessionId + 在 sessions_dir 内。
// 全程用 filepath（不用 path、不用通配符），正确处理 Windows 分隔符和相对配置路径。
func isAutoClawValidChangedFile(changedFile, sessionsDir string) bool {
	// 拒绝空白 sessions_dir（filepath.Abs("") 返回 cwd 且 err=nil，会导致误判）
	if strings.TrimSpace(sessionsDir) == "" {
		return false
	}
	abs, err := filepath.Abs(changedFile)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)

	// 拒绝 trajectory（精确后缀，避免双计）
	if strings.HasSuffix(filepath.Base(abs), ".trajectory.jsonl") {
		return false
	}

	sd, err := filepath.Abs(sessionsDir)
	if err != nil {
		return false
	}
	sd = filepath.Clean(sd)
	rel, err := filepath.Rel(sd, abs)
	if err != nil {
		return false
	}

	// 越界（sessions_dir 之外）：拒绝以 ".." + 路径分隔符开头的相对路径。
	// 注意区分两种 HasPrefix：
	//   ✗ strings.HasPrefix(rel, "..")       — 会误拒合法的 "..agent/sessions/x.jsonl"
	//   ✓ strings.HasPrefix(rel, ".."+sep)   — 只拒绝越界路径
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}

	// 三层验证
	base := filepath.Base(rel)
	if !strings.HasSuffix(base, ".jsonl") {
		return false
	}
	// sessionId 去后缀非空（拒绝 ".jsonl"，避免空 SessionID）
	if strings.TrimSuffix(base, ".jsonl") == "" {
		return false
	}
	if filepath.Base(filepath.Dir(rel)) != "sessions" {
		return false
	}
	agentID := filepath.Dir(filepath.Dir(rel))
	if agentID == "" || agentID == "." {
		return false // 拒绝缺 agentId（如 sessions/x.jsonl，Dir2 为 "."）
	}
	if filepath.Dir(agentID) != "." {
		return false // 恰好三层，agentId 之上不再有目录
	}
	return true
}

// autoclawAgentIDFromFile 从文件路径解析 agentId（rel 到 sessions_dir 的第一段）。
// 调用方已保证 file 是经过 isAutoClawValidChangedFile 校验的合法三层路径。
func autoclawAgentIDFromFile(file, sessionsDir string) string {
	abs, err := filepath.Abs(file)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	sd, err := filepath.Abs(sessionsDir)
	if err != nil {
		return ""
	}
	sd = filepath.Clean(sd)
	rel, err := filepath.Rel(sd, abs)
	if err != nil {
		return ""
	}
	// rel 形如 {agentId}/sessions/{sessionId}.jsonl，去 sessions/x.jsonl 两层剩 agentId
	return filepath.Dir(filepath.Dir(rel))
}

// autoClawWalker 是可注入的目录遍历器（生产用 filepath.Walk，测试用受控 walker）。
type autoClawWalker func(root string, fn filepath.WalkFunc) error

// scanSessions 返回扫描结果；walkFn 非 nil 时用注入的 walker（测试 seam），否则走生产 filepath.Walk。
func (c *AutoClawCollector) scanSessions(ctx context.Context, sessionsDir string) ([]string, error) {
	return scanAutoClawSessionsWith(ctx, sessionsDir, c.walkFn)
}

// parseFile 解析单文件；parseFn 非 nil 时用注入的 parser（测试 seam），否则走生产路径。
func (c *AutoClawCollector) parseFile(ctx context.Context, path string, logger *slog.Logger) ([]autoclawParsedMessage, error) {
	if c.parseFn != nil {
		return c.parseFn(ctx, path, logger)
	}
	return parseAutoClawJSONLContext(ctx, path, logger)
}

// scanAutoClawSessionsWith 是注入 walker 的扫描 seam（仅包内可见），便于测试构造确定性取消/错误。
// walk 为 nil 时回退到 filepath.Walk。对每个 .jsonl 候选文件复用 isAutoClawValidChangedFile
// 做三层路径校验 + trajectory 拒绝，保持扫描路径与 ChangedFile 路径校验完全对称。
// Walk 回调最开始检查 ctx.Err()。
func scanAutoClawSessionsWith(ctx context.Context, sessionsDir string, walk autoClawWalker) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionsDir) == "" {
		return nil, nil
	}
	info, err := os.Stat(sessionsDir)
	if os.IsNotExist(err) {
		return nil, nil // 目录不存在视为无数据，不报错
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	if walk == nil {
		walk = filepath.Walk
	}

	var files []string
	walkErr := walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		// ctx 取消：中止遍历，回调最开始检查（先于 walkErr 与候选处理）
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		// 复用 ChangedFile 校验契约，保持扫描与增量路径对称
		if isAutoClawValidChangedFile(path, sessionsDir) {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			// ctx 取消：返回已发现文件 + ctx 错误（Collect 层据此返回 ctx.Err，不降级为 PartialErr）
			return files, walkErr
		}
		// 非 ctx 错误：返回已发现文件 + error（Collect 层保留已发现消息并记 PartialErr）
		return files, walkErr
	}
	return files, nil
}

// ---- JSONL 行解析 ----

// autoclawMessage 对应 JSONL 一行（只声明采集需要的字段，其余忽略）。
type autoclawMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id"`        // 行顶层 id，幂等去重键（实测非空且唯一）
	Timestamp string `json:"timestamp"` // 顶层 ISO8601/RFC3339，回退用
	Cwd       string `json:"cwd"`       // type=session 行的工作目录
	Message   *struct {
		Role      string         `json:"role"`
		Model     string         `json:"model"`
		Provider  string         `json:"provider"`
		Usage     *autoclawUsage `json:"usage"`
		Timestamp int64          `json:"timestamp"` // 毫秒，主时间
	} `json:"message"`
}

// autoclawUsage 对应 message.usage（camelCase 字段名，与其他客户端不同）。
type autoclawUsage struct {
	Input           int64 `json:"input"`
	Output          int64 `json:"output"`
	CacheRead       int64 `json:"cacheRead"`
	CacheWrite      int64 `json:"cacheWrite"`
	ReasoningTokens int64 `json:"reasoningTokens"`
	TotalTokens     int64 `json:"totalTokens"`
}

// autoclawParsedMessage 解析后的单条有效消息。
type autoclawParsedMessage struct {
	ID                string
	Cwd               string
	Model             string
	Provider          string
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	ReasoningTokens   int64
	TotalTokens       int64
	Timestamp         int64
}

// parseAutoClawJSONLContext 解析 AutoClaw JSONL，提取「带 usage 的 assistant 消息」。
// 仿 workbuddy parseWorkBuddyJSONLContext，scanner 循环检查 ctx。
func parseAutoClawJSONLContext(ctx context.Context, path string, logger *slog.Logger) ([]autoclawParsedMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 AutoClaw JSONL 失败: %w", err)
	}
	defer f.Close()
	return parseAutoClawJSONLReader(ctx, f, path, logger)
}

// parseAutoClawJSONLReader 从 reader 逐行解析（仅包内可见的 reader seam，供受控 reader 测试取消点）。
// scanner 循环每行开头检查 ctx.Err()——取消时立即返回 (已解析 messages, ctx.Err)，
// 由 Collect 层返回外层 ctx.Err（不降级为 PartialErr）。
func parseAutoClawJSONLReader(ctx context.Context, r io.Reader, path string, logger *slog.Logger) ([]autoclawParsedMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	var messages []autoclawParsedMessage
	var cwd string
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineSize)
	lineNum := 0
	for scanner.Scan() {
		// 每行开头检查 ctx（取消时返回已解析 messages + ctx.Err，不丢弃部分结果）
		if err := ctx.Err(); err != nil {
			return messages, err
		}
		lineNum++
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg autoclawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			logger.Debug("AutoClaw JSONL line parse failed, skipped",
				"file", path, "line", lineNum, "error", err)
			continue
		}

		// session 行记录 cwd，供该文件所有 message 行复用
		if msg.Type == "session" {
			if cwd == "" {
				cwd = msg.Cwd
			}
			continue
		}

		// 只处理 type=message + role=assistant + usage 非空
		if msg.Type != "message" {
			continue
		}
		if msg.Message == nil || msg.Message.Role != "assistant" || msg.Message.Usage == nil {
			continue
		}

		// 顶层 id 为空跳过（防空 id 写入 (client,"") 主键互相覆盖）
		if msg.ID == "" {
			logger.Debug("AutoClaw message top-level id empty, skipped",
				"file", path, "line", lineNum)
			continue
		}
		// 按顶层 id 去重（同一顶层 id 的重复行只保留首条 usage 非空且时间有效的记录）
		if _, ok := seen[msg.ID]; ok {
			continue
		}

		// timestamp 主用 message.timestamp（毫秒）；缺失/0 回退顶层 RFC3339
		ts := msg.Message.Timestamp
		if ts <= 0 {
			if msg.Timestamp != "" {
				if t, perr := time.Parse(time.RFC3339, msg.Timestamp); perr == nil {
					ts = t.UnixMilli()
				}
			}
		}
		if ts <= 0 {
			// message.timestamp=0 且顶层 RFC3339 失败 → 跳过（不产 TS=0 脏消息）
			logger.Debug("AutoClaw assistant message timestamp invalid, skipped",
				"file", path, "line", lineNum, "id", msg.ID)
			continue
		}

		seen[msg.ID] = struct{}{}
		usage := msg.Message.Usage
		messages = append(messages, autoclawParsedMessage{
			ID:                msg.ID,
			Cwd:               cwd,
			Model:             msg.Message.Model,
			Provider:          msg.Message.Provider,
			InputTokens:       usage.Input,
			OutputTokens:      usage.Output,
			CacheReadTokens:   usage.CacheRead,
			CacheCreateTokens: usage.CacheWrite,
			ReasoningTokens:   usage.ReasoningTokens,
			TotalTokens:       usage.TotalTokens,
			Timestamp:         ts,
		})
	}

	// scanner 错误（IO 错误、行超 maxJSONLLineSize）：返回已解析部分 + err，
	// 让 Collect 层 append + PartialErr（不 continue 丢弃）。
	if err := scanner.Err(); err != nil {
		return messages, err
	}
	if err := ctx.Err(); err != nil {
		return messages, err
	}
	return messages, nil
}

// autoClawMessage 将单条解析消息转换为 model.Message。
// FreshInput 直接赋值 usage.input（AutoClaw 的 input 是纯新输入，不含 cacheRead）。
// TotalTokens 取 JSON 原值；为 0 时回退 input + output + cacheRead（含 cacheRead）。
func autoClawMessage(fileSessionID string, in autoclawParsedMessage, providerMap map[string]autoclawProviderInfo) model.Message {
	total := in.TotalTokens
	if total == 0 {
		// AutoClaw 的 input 不含 cacheRead，回退必须含 cacheRead（否则少记）
		total = in.InputTokens + in.OutputTokens + in.CacheReadTokens
	}
	return model.Message{
		ID:                in.ID,
		SessionID:         fileSessionID,
		Client:            model.ClientZhipuAutoClaw,
		Date:              autoclawTsMsToDate(in.Timestamp),
		TS:                in.Timestamp,
		Model:             in.Model,
		Provider:          resolveAutoClawProvider(in.Provider, providerMap),
		Directory:         in.Cwd,
		Project:           autoclawInferProject(in.Cwd),
		InputTokens:       in.InputTokens,
		FreshInputTokens:  in.InputTokens, // 直接赋值，不调用 SubtractCache
		OutputTokens:      in.OutputTokens,
		CacheReadTokens:   in.CacheReadTokens,
		CacheCreateTokens: in.CacheCreateTokens,
		ReasoningTokens:   in.ReasoningTokens,
		TotalTokens:       total,
	}
}

// ---- provider 映射（按 agent 隔离）----

// autoclawProviderInfo 保留 provider 对象的 name（实测常为空）。
type autoclawProviderInfo struct {
	Name string
}

type autoclawModelsFile struct {
	Providers map[string]autoclawProviderEntry `json:"providers"`
}

type autoclawProviderEntry struct {
	Name string `json:"name"`
}

// loadAutoClawProviderMap 加载 agents/{agentId}/agent/models.json，返回 providerKey → info。
// 不跨 agent 合并（多 Agent 隔离）。models.json 缺失/损坏 → 返回空 map（不阻断）。
func loadAutoClawProviderMap(sessionsDir, agentID string) map[string]autoclawProviderInfo {
	m := make(map[string]autoclawProviderInfo)
	if strings.TrimSpace(sessionsDir) == "" || agentID == "" {
		return m
	}
	// sessionsDir 是 agents/ 根；models.json 在 agents/{agentId}/agent/models.json
	modelsPath := filepath.Join(sessionsDir, agentID, "agent", "models.json")
	data, err := os.ReadFile(modelsPath)
	if err != nil {
		// 缺失/不可读 → 空 map（不阻断采集）
		return m
	}
	var mf autoclawModelsFile
	if err := json.Unmarshal(data, &mf); err != nil {
		// 损坏 → 空 map（不阻断采集）
		return m
	}
	for key, entry := range mf.Providers {
		m[key] = autoclawProviderInfo{Name: entry.Name}
	}
	return m
}

// resolveAutoClawProvider 取 provider 值：
// message.provider 命中映射且 Name 非空 → 取 Name；
// 其余（命中但 Name 为空 / 未命中 / map 异常）→ 回退 message.provider 原值。
func resolveAutoClawProvider(messageProvider string, providerMap map[string]autoclawProviderInfo) string {
	if info, ok := providerMap[messageProvider]; ok && info.Name != "" {
		return info.Name
	}
	return messageProvider
}

// ---- 私有 helper（客户端文件独立性原则，不复用其他 collector）----

// autoclawTsMsToDate 毫秒时间戳转日期字符串。
func autoclawTsMsToDate(tsMs int64) string {
	if tsMs <= 0 {
		return ""
	}
	return time.UnixMilli(tsMs).Format("2006-01-02")
}

// autoclawInferProject 从工作目录路径提取项目名（末段）。
func autoclawInferProject(directory string) string {
	return projectBase(directory)
}
