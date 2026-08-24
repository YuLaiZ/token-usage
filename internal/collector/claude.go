package collector

import (
	"bufio"
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
		part, err := parseClaudeMessageFile(file, dateSet, logger)
		if err != nil {
			logger.Warn("Claude JSONL 文件解析失败，跳过", "file", file, "error", err)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, err))
			continue
		}
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

// jsonlEntry JSONL 单行结构
type jsonlEntry struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	Entrypoint  string `json:"entrypoint"`
	Cwd         string `json:"cwd"`
	CustomTitle string `json:"custom-title"`
	Message     *struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *tokenUsage `json:"usage"`
	} `json:"message"`
}

type tokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_input_tokens"`
	CacheCreateTokens int64 `json:"cache_creation_input_tokens"`
}

// parseClaudeMessageFile 单次全量扫描 JSONL 文件，产出消息级结果。
// dates 非空时只保留命中日期的 Message；Session 的 first/last 始终来自完整文件，
// 但仅当存在命中消息时才返回 Session。
func parseClaudeMessageFile(filePath string, dates map[string]struct{}, logger *slog.Logger) (CollectResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	file, err := os.Open(filePath)
	if err != nil {
		return CollectResult{}, err
	}
	defer file.Close()

	var entries []jsonlEntry
	// 行解析失败按文件聚合为一条汇总（首行号+首个错误保留定位线索）：
	// 上游合法数据形态变化（如 user 行 content 为字符串）会让失败在全量扫描中
	// 必然重复出现，逐行打印只产生噪音。
	var (
		badLines     int
		firstBadLine int
		firstBadErr  error
	)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineSize)
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry jsonlEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			if badLines == 0 {
				firstBadLine = line
				firstBadErr = err
			}
			badLines++
			continue
		}
		entries = append(entries, entry)
	}
	if badLines > 0 {
		logger.Debug("Claude JSONL 行解析失败，跳过",
			"file", filePath, "count", badLines, "first_line", firstBadLine, "error", firstBadErr)
	}
	if err := scanner.Err(); err != nil {
		return CollectResult{}, fmt.Errorf("读取文件失败: %w", err)
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
		if entry.CustomTitle != "" {
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
			logger.Debug("Claude assistant 消息时间戳无效，跳过",
				"file", filePath, "message_id", entry.Message.ID, "timestamp", entry.Timestamp, "error", perr)
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
			ID:                entry.Message.ID,
			SessionID:         fileSessionID,
			Client:            model.RawClientToClient[client],
			Date:              date,
			TS:                ts,
			Model:             entry.Message.Model,
			Directory:         cwd,
			Project:           project,
			InputTokens:       usage.InputTokens,
			FreshInputTokens:  usage.InputTokens,
			OutputTokens:      usage.OutputTokens,
			CacheReadTokens:   usage.CacheReadTokens,
			CacheCreateTokens: usage.CacheCreateTokens,
			TotalTokens:       usage.InputTokens + usage.CacheReadTokens + usage.CacheCreateTokens + usage.OutputTokens,
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
	return result, nil
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
