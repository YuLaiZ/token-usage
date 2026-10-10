package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

type ClaudeCollector struct {
	cfg *config.Config
}

func NewClaudeCollector(cfg *config.Config) *ClaudeCollector {
	return &ClaudeCollector{cfg: cfg}
}

func (c *ClaudeCollector) Name() string {
	return "claude"
}

func (c *ClaudeCollector) SyncSources() []string { return nil }

// Collect 按 CLI 日期过滤或 ChangedFile 单文件模式采集消息级 token。
// 同一文件内按消息粒度过滤日期：每条 Message 归入其自身 timestamp 所在日。
func (c *ClaudeCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
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
		return CollectResult{}, fmt.Errorf("Claude collector 配置为空")
	}
	clientCfg, ok := c.cfg.ClientConfig("claude")
	if !ok || !clientCfg.Enabled {
		if !ok {
			return CollectResult{}, fmt.Errorf("claude 配置不存在")
		}
		return CollectResult{}, nil
	}
	if clientCfg.Paths == nil {
		return CollectResult{}, fmt.Errorf("claude 配置不存在")
	}

	files := []string(nil)
	var scanErr error
	if req.ChangedFile != "" {
		files = []string{req.ChangedFile}
	} else {
		files, scanErr = findClaudeJSONLFiles(ctx, clientCfg.Paths["projects_dir"])
		if scanErr != nil && (errors.Is(scanErr, context.Canceled) ||
			errors.Is(scanErr, context.DeadlineExceeded)) {
			return CollectResult{}, scanErr
		}
	}

	dateSet := make(map[string]struct{}, len(req.Dates))
	for _, date := range req.Dates {
		dateSet[date] = struct{}{}
	}

	var result CollectResult
	if scanErr != nil {
		result.PartialErr = fmt.Errorf("查找 Claude JSONL 文件失败: %w", scanErr)
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		before := fsident.SnapshotOfFile(file)
		if skipGateHit(req.SkipGate, file, before) {
			result.FileStatuses = append(result.FileStatuses, FileScanStatus{Path: file, Skipped: true, Before: before})
			continue
		}
		part, status, err := parseClaudeMessageFile(file, dateSet, logger)
		status.Before = before
		status.After = fsident.SnapshotOfFile(file)
		if err != nil {
			logger.Warn("Claude JSONL file parse failed, skipped", "file", file, "error", err)
			status.Err = err
			result.FileStatuses = append(result.FileStatuses, status)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, err))
			continue
		}
		result.FileStatuses = append(result.FileStatuses, status)
		result.Messages = append(result.Messages, part.Messages...)
		result.Sessions = append(result.Sessions, part.Sessions...)
	}
	return result, nil
}

// findClaudeJSONLFiles 递归查找所有 .jsonl 文件（排除 /subagents/ 目录）
func findClaudeJSONLFiles(ctx context.Context, projectsDir string) ([]string, error) {
	var files []string
	if strings.TrimSpace(projectsDir) == "" {
		return nil, fmt.Errorf("Claude projects_dir 未配置")
	}
	info, err := os.Stat(projectsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("访问 Claude projects_dir 失败: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Claude projects_dir 不是目录: %s", projectsDir)
	}

	err = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// ctx 取消：中止遍历，返回已找到的文件（守护进程关闭时尽快退出，避免长采集阻塞关闭路径）
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if info.IsDir() && info.Name() == "subagents" {
			return filepath.SkipDir
		}

		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}

		return nil
	})

	return files, err
}

// jsonlEntry JSONL 单行结构。lineNo 记录源行号（非 JSON 字段），供坏行汇总定位。
type jsonlEntry struct {
	Type       string `json:"type"`
	SessionID  string `json:"sessionId"`
	Timestamp  string `json:"timestamp"`
	Entrypoint string `json:"entrypoint"`
	Cwd        string `json:"cwd"`
	// UUID/ParentUUID 构成会话内的消息链，是请求时长估算的起点推导依据。
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	// APIBlockIndex 是该行所属内容块的序号（0 起，顶层字段）；nil 为旧日志
	// 无该字段。时长起点推导只认回复首块（block 0）的父链——文件中首见的
	// 块不一定是首块，缺失 block 0 的回复起点会取晚（速度偏高），宁可不估。
	APIBlockIndex *int64 `json:"apiBlockIndex"`
	// 标题行 {"type":"custom-title"} 的载荷字段双形态：现行版本为驼峰 customTitle，
	// kebab custom-title 为历史兼容；同行两形态同现时驼峰优先。
	CustomTitleCamel string `json:"customTitle"`
	CustomTitle      string `json:"custom-title"`
	lineNo           int
	Message          *struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *tokenUsage     `json:"usage"`
		// StopReason 非空表示完整回合（末 block 才出现）；时长估算只计入
		// 完整回合。
		StopReason *string `json:"stop_reason"`
	} `json:"message"`
}

// claudeContentBlock 是 message.content 数组元素中采集关心的字段。
type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// normalizeClaudeContent 归一 message.content 的双形态，返回 (blocks, stringForm, err)：
// 数组形态（assistant 消息）原样解析为 block 列表；字符串形态（user 消息直接
// 携带纯文本）等价转换为单文本块并置 stringForm；其它形态返回错误，由调用方
// 按行失败汇总处理，保留对未来上游形态漂移的兜底能力。
func normalizeClaudeContent(raw json.RawMessage) (blocks []claudeContentBlock, stringForm bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var parsed []claudeContentBlock
	if uerr := json.Unmarshal(raw, &parsed); uerr == nil {
		return parsed, false, nil
	}
	var text string
	if uerr := json.Unmarshal(raw, &text); uerr == nil {
		return []claudeContentBlock{{Type: "text", Text: text}}, true, nil
	}
	return nil, false, fmt.Errorf("无法解析的 message.content 形态: %.80s", raw)
}

type tokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_input_tokens"`
	CacheCreateTokens int64 `json:"cache_creation_input_tokens"`
}

// claudeChainNode 是消息链上一行的计时线索。hasTS=false（timestamp 缺失或
// 不可解析）的行仍在链中供跳转，但不能作为起点（对齐上游 Option 语义）。
type claudeChainNode struct {
	tsMS         int64
	hasTS        bool
	parentUUID   string
	isAttachment bool
}

// claudeMessageTiming 聚合同一 message.id（一条回复的多个内容块）的计时线索：
// 首块的父引用定请求起点，全部块的最晚时间戳定结束，末块的 stop_reason 定
// 回合完整性。与 token 去重合同并行：token 取首条非零 usage，计时独立汇总。
type claudeMessageTiming struct {
	firstParent string
	// startsAtFirstBlock 报告文件中首见的块是否为回复首块（apiBlockIndex
	// 为 0 或旧日志无该字段）。非首块起步的回复（block 0 在回看范围外）
	// 起点会取晚、速度偏高，宁可不估。
	startsAtFirstBlock bool
	endMS              int64
	hasStopReason      bool
}

// claudeMaxChainHops 限制请求起点沿父链向上的跳数：防御损坏数据成环导致
// 死循环；正常会话链深度远小于该值。
const claudeMaxChainHops = 32

// buildClaudeTiming 从全部行构建链缓存与逐回复计时聚合。
// chain 覆盖所有带 uuid 的行（含 user/attachment/sidechain），attachment 行
// 的时间戳不可靠（回复开始后才补记），起点推导跳过。
func buildClaudeTiming(entries []jsonlEntry) (chain map[string]claudeChainNode, timings map[string]*claudeMessageTiming) {
	chain = make(map[string]claudeChainNode, len(entries))
	for _, e := range entries {
		if e.UUID == "" {
			continue
		}
		tsMS := int64(0)
		hasTS := false
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			tsMS = t.UnixMilli()
			hasTS = true
		}
		chain[e.UUID] = claudeChainNode{tsMS: tsMS, hasTS: hasTS, parentUUID: e.ParentUUID, isAttachment: e.Type == "attachment"}
	}
	timings = make(map[string]*claudeMessageTiming)
	for _, e := range entries {
		if e.Type != "assistant" || e.Message == nil || e.Message.ID == "" {
			continue
		}
		tsMS := int64(0)
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			tsMS = t.UnixMilli()
		}
		timing, ok := timings[e.Message.ID]
		if !ok {
			starts := e.APIBlockIndex == nil || *e.APIBlockIndex == 0
			timing = &claudeMessageTiming{firstParent: e.ParentUUID, startsAtFirstBlock: starts}
			timings[e.Message.ID] = timing
		}
		if tsMS > timing.endMS {
			timing.endMS = tsMS
		}
		if e.Message.StopReason != nil && *e.Message.StopReason != "" {
			timing.hasStopReason = true
		}
	}
	return chain, timings
}

// resolveClaudeStartMS 从回复首块的父引用沿链向上取请求起点：跳过
// attachment 类节点，遇第一个其他节点取其时间戳；链断（节点缺失）或超过
// 跳数上限（损坏数据成环）返回 false。
func resolveClaudeStartMS(chain map[string]claudeChainNode, firstParent string) (int64, bool) {
	cursor := firstParent
	for i := 0; i < claudeMaxChainHops; i++ {
		node, ok := chain[cursor]
		if !ok {
			return 0, false
		}
		if !node.isAttachment {
			if !node.hasTS {
				return 0, false
			}
			return node.tsMS, true
		}
		cursor = node.parentUUID
	}
	return 0, false
}

// claudeDurationMS 计算一条回复的估算时长：完整回合（stop_reason 非空）+
// 链式起点可解 + 过滤门（output 与时长双阈值）通过才返回非零。
func claudeDurationMS(timing *claudeMessageTiming, chain map[string]claudeChainNode, outputTokens int64) int64 {
	if timing == nil || !timing.hasStopReason || !timing.startsAtFirstBlock {
		return 0
	}
	startMS, ok := resolveClaudeStartMS(chain, timing.firstParent)
	if !ok {
		return 0
	}
	return estimateDurationMS(outputTokens, timing.endMS-startMS)
}

// parseClaudeMessageFile 单次全量扫描 JSONL 文件，产出消息级结果。
// dates 非空时只保留命中日期的 Message；Session 的 first/last 始终来自完整文件，
// 但仅当存在命中消息时才返回 Session。
// 返回的 FileScanStatus 已填 Path/BadLines/FirstBad*/TrailingNewline（Before/After
// 快照与 Err 由 Collect 层补充）。
func parseClaudeMessageFile(filePath string, dates map[string]struct{}, logger *slog.Logger) (CollectResult, FileScanStatus, error) {
	var status FileScanStatus
	status.Path = filePath
	if logger == nil {
		logger = slog.Default()
	}
	file, err := os.Open(filePath)
	if err != nil {
		return CollectResult{}, status, err
	}
	defer file.Close()

	var entries []jsonlEntry
	// chainOnly 承载字符串 content 形态 user 行的链节点线索：这类行按既有
	// 归一合同不进 entries（元数据归类与消息产出不可见），但其
	// uuid/parentUuid/timestamp/type 仍参与请求起点链推导（顶层字段与
	// content 形态无关），否则直接父级为该类行的回合时长必然落 0。
	var chainOnly []jsonlEntry
	// 行解析失败按文件聚合为一条汇总（首行号+首个错误保留定位线索）：
	// 上游合法数据形态变化（如 user 行 content 为字符串）会让失败在全量扫描中
	// 必然重复出现，逐行打印只产生噪音。
	var outcome parseFileOutcome
	it := newJSONLLineIter(context.Background(), file, maxJSONLLineSize)
	for it.Next() {
		if it.Oversized() {
			outcome.addBad(it.LineNo(), errJSONLLineOversized)
			continue
		}
		raw := it.Line()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry jsonlEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			outcome.addBad(it.LineNo(), err)
			continue
		}
		// content 双形态归一校验：数字、对象等未知形态与行 Unmarshal 失败同等
		// 对待，维持按文件汇总的兜底路径。字符串形态行此前整体 Unmarshal 失败、
		// 从未参与元数据推断或消息产出，识别后必须保持同等不可见：其携带的
		// entrypoint/cwd/timestamp 一旦参与推断会改变 client/directory/project
		// 归类与时间戳边界，在 (client,id) 主键下对既有库形成重复行、聚合翻倍。
		if entry.Message != nil {
			_, stringForm, cerr := normalizeClaudeContent(entry.Message.Content)
			if cerr != nil {
				outcome.addBad(it.LineNo(), cerr)
				continue
			}
			if stringForm {
				chainOnly = append(chainOnly, entry)
				continue
			}
		}
		entry.lineNo = it.LineNo()
		entries = append(entries, entry)
	}
	if err := it.Err(); err != nil {
		return CollectResult{}, status, fmt.Errorf("读取文件失败: %w", err)
	}
	// 尾行未以 \n 终结：可能仍在写，即使恰好可解析也不得视为完整采集。
	if fi, serr := file.Stat(); serr == nil {
		outcome.trailingNewline = tailHasNewline(file, fi.Size())
	}

	// 文件级元信息：entrypoint、cwd、最后非空 title、first/last ts。
	entrypoint := ""
	cwd := ""
	customTitle := ""
	var firstTS, lastTS int64
	for _, entry := range entries {
		if entry.Entrypoint != "" && entrypoint == "" {
			entrypoint = entry.Entrypoint
		}
		if entry.Cwd != "" && cwd == "" {
			cwd = entry.Cwd
		}
		// 标题双形态归一：行内驼峰优先、kebab 兜底；跨行保持"行序最后非空"语义
		// （单遍循环内逐行归一，不得拆成两遍独立扫描导致顺序失真）。
		if title := entry.CustomTitleCamel; title != "" {
			customTitle = title
		} else if entry.CustomTitle != "" {
			customTitle = entry.CustomTitle
		}
		if entry.Timestamp != "" {
			if t, perr := time.Parse(time.RFC3339, entry.Timestamp); perr == nil {
				ts := t.UnixMilli()
				if firstTS == 0 {
					firstTS = ts
				}
				if ts < firstTS {
					firstTS = ts
				}
				if ts > lastTS {
					lastTS = ts
				}
			}
		}
	}

	client := model.RawClientClaudeCode
	if entrypoint == "claude-desktop-3p" {
		client = model.RawClientClaudeDesktop
	}

	fileSessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	project := inferProject(cwd)

	// 按首条非零 usage 记录去重 assistant 消息（同一 message.id 的 thinking/text/tool 片段合并）。
	// 请求时长估算：链缓存 + 逐回复计时聚合（与下方 token 去重循环并行）。
	// 字符串 content 行只贡献链节点。
	chain, timings := buildClaudeTiming(append(append([]jsonlEntry(nil), entries...), chainOnly...))

	seen := make(map[string]bool)
	var messages []model.Message
	for _, entry := range entries {
		if entry.Type != "assistant" || entry.Message == nil || entry.Message.ID == "" || entry.Message.Usage == nil {
			continue
		}
		usage := *entry.Message.Usage
		// 跳过无任何 token 的空片段（如纯 thinking 续片），且不占用 message.id；
		// 同一消息稍后的首条非零 usage 才是应保留的记录。
		if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreateTokens == 0 {
			continue
		}
		timestamp, perr := time.Parse(time.RFC3339, entry.Timestamp)
		if perr != nil {
			// assistant 消息已带 usage 却因 timestamp 非法被丢弃：数据异常，
			// 计入坏行（该文件不得推进跳过门），随文件汇总日志输出。
			outcome.addBad(entry.lineNo, fmt.Errorf("assistant 消息 timestamp 非法 %q: %w", entry.Timestamp, perr))
			continue
		}
		if seen[entry.Message.ID] {
			continue
		}
		seen[entry.Message.ID] = true

		ts := timestamp.UnixMilli()
		date := tsMsToDate(ts)
		if len(dates) > 0 {
			if _, ok := dates[date]; !ok {
				continue
			}
		}
		messages = append(messages, model.Message{
			ID:        entry.Message.ID,
			SessionID: fileSessionID,
			Client:    model.RawClientToClient[client],
			Date:      date,
			TS:        ts,
			Model:     entry.Message.Model,
			// Claude Code/Desktop 的本地会话不记录 provider；其官方来源可确定。
			Provider:          "Anthropic",
			Directory:         cwd,
			Project:           project,
			InputTokens:       usage.InputTokens,
			FreshInputTokens:  usage.InputTokens,
			OutputTokens:      usage.OutputTokens,
			CacheReadTokens:   usage.CacheReadTokens,
			CacheCreateTokens: usage.CacheCreateTokens,
			TotalTokens:       usage.InputTokens + usage.CacheReadTokens + usage.CacheCreateTokens + usage.OutputTokens,
			DurationMS:        claudeDurationMS(timings[entry.Message.ID], chain, usage.OutputTokens),
		})
	}

	var result CollectResult
	result.Messages = messages
	// 仅当存在命中消息时返回 Session（避免无消息的空会话污染结果）。
	if len(messages) > 0 {
		result.Sessions = append(result.Sessions, model.Session{
			ID:        fileSessionID,
			Client:    model.RawClientToClient[client],
			Directory: cwd,
			Project:   project,
			Title:     customTitle,
			FirstTS:   firstTS,
			LastTS:    lastTS,
		})
	}
	// 坏行汇总在两个解析循环之后打（行解析与消息产出都可能计入坏行）。
	if outcome.badLines > 0 {
		logger.Debug("Claude JSONL line parse failed, skipped",
			"file", filePath, "count", outcome.badLines, "first_line", outcome.firstBadLine, "error", outcome.firstBadErr)
	}
	outcome.fillStatus(&status)
	return result, status, nil
}

// inferProject 从工作目录路径提取项目名
func inferProject(directory string) string {
	if directory == "" {
		return ""
	}

	directory = strings.TrimRight(directory, `/\`)

	if strings.HasPrefix(directory, "-") {
		knownPrefixes := []string{
			"IdeaProjects", "Projects", "workspace", "repos", "src",
			"go/src", "code", "dev",
		}
		for _, prefix := range knownPrefixes {
			idx := strings.Index(directory, "-"+prefix+"-")
			if idx >= 0 {
				rest := directory[idx+len(prefix)+2:]
				if rest != "" {
					return rest
				}
			}
		}
		parts := strings.Split(directory, "-")
		if len(parts) >= 3 {
			return parts[len(parts)-2] + "-" + parts[len(parts)-1]
		}
		return parts[len(parts)-1]
	}

	return projectBase(directory)
}

// tsMsToDate 毫秒时间戳转日期字符串
func tsMsToDate(tsMs int64) string {
	if tsMs <= 0 {
		return ""
	}
	t := time.UnixMilli(tsMs)
	return t.Format("2006-01-02")
}
