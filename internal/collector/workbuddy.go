package collector

import (
	"bufio"
	"context"
	"database/sql"
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
	_ "modernc.org/sqlite"
)

type WorkBuddyCollector struct {
	cfg *config.Config
}

func NewWorkBuddyCollector(cfg *config.Config) *WorkBuddyCollector {
	return &WorkBuddyCollector{cfg: cfg}
}

func (c *WorkBuddyCollector) Name() string {
	return "workbuddy"
}

func (c *WorkBuddyCollector) SyncSources() []string { return nil }

// Collect 逐消息采集：每个顶层 message.id 产出一条 model.Message，每个物理 JSONL 文件产出一条 Session 元数据。
// req.ChangedFile 非空时只扫描该文件；否则扫描 projects/*/*.jsonl。
// title 是装饰性字段，workbuddy.db 打开/查询失败时降级为空 title，不阻断 token 采集。
func (c *WorkBuddyCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
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
		return CollectResult{}, fmt.Errorf("WorkBuddy collector 配置为空")
	}
	clientCfg, ok := c.cfg.ClientConfig("workbuddy")
	if !ok || !clientCfg.Enabled {
		return CollectResult{}, nil
	}

	projectsDir := clientCfg.Paths["projects_dir"]

	// 1. 文件列表：ChangedFile 优先（daemon 增量），否则扫描 projects 目录
	var files []string
	if req.ChangedFile != "" {
		files = []string{req.ChangedFile}
	} else {
		if projectsDir == "" {
			return CollectResult{}, nil
		}
		var err error
		files, err = scanWorkBuddyJSONLContext(ctx, projectsDir)
		if err != nil {
			return CollectResult{}, fmt.Errorf("扫描 WorkBuddy JSONL 失败: %w", err)
		}
	}
	if len(files) == 0 {
		return CollectResult{}, nil
	}

	// 2. 加载 models.json（model短id -> vendor）
	workbuddyDir := filepath.Dir(projectsDir)
	modelMapping, err := loadWorkBuddyModelsMapping(workbuddyDir)
	if err != nil {
		return CollectResult{}, fmt.Errorf("加载 models.json 失败: %w", err)
	}

	// 3. 查询 workbuddy.db 补充 title（map[fileSessionID]title）
	// title 是装饰性字段：打开/查询失败时降级为空 title，不阻断 token 采集
	titleMap := make(map[string]string)
	if dbPath := clientCfg.Paths["db"]; dbPath != "" {
		if db, openErr := openSQLiteReadOnly(dbPath); openErr == nil {
			var queryErr error
			titleMap, queryErr = queryWorkBuddyTitles(ctx, db)
			if queryErr != nil {
				logger.Debug("WorkBuddy title query failed, degrading to empty title",
					"error", queryErr,
					"db_path", dbPath)
			}
			db.Close()
		} else {
			logger.Debug("WorkBuddy DB open failed, degrading to empty title",
				"error", openErr,
				"db_path", dbPath)
		}
	}

	// 4. 日期过滤集合（dates 为空时不过滤，即全量）
	dateSet := make(map[string]bool, len(req.Dates))
	for _, d := range req.Dates {
		dateSet[d] = true
	}

	// 5. 逐文件解析，按消息粒度过滤日期后转换
	var result CollectResult
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		messages, err := parseWorkBuddyJSONLContext(ctx, file, logger)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			logger.Warn("WorkBuddy JSONL file parse failed, skipped", "file", file, "error", err)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, err))
			continue
		}
		if len(messages) == 0 {
			continue
		}

		// fileSessionID 来自文件名（去 .jsonl），作为 SessionID 与 title 关联键
		fileSessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		if fileSessionID == "" {
			err := fmt.Errorf("%s: 文件名缺少 session ID", file)
			result.PartialErr = errors.Join(result.PartialErr, err)
			logger.Warn("WorkBuddy JSONL file name invalid, skipped", "file", file)
			continue
		}

		var firstTS, lastTS int64
		var cwd string
		var hitMessages []model.Message
		for _, pm := range messages {
			if pm.Timestamp > 0 {
				if firstTS == 0 {
					firstTS = pm.Timestamp
				}
				if pm.Timestamp > lastTS {
					lastTS = pm.Timestamp
				}
				if pm.Timestamp < firstTS {
					firstTS = pm.Timestamp
				}
			}
			if cwd == "" && pm.Cwd != "" {
				cwd = pm.Cwd
			}
			date := workbuddyTsMsToDate(pm.Timestamp)
			if len(req.Dates) > 0 && !dateSet[date] {
				continue
			}
			hitMessages = append(hitMessages, workBuddyMessage(fileSessionID, pm, modelMapping))
		}

		if len(hitMessages) == 0 {
			continue
		}

		result.Messages = append(result.Messages, hitMessages...)
		// 每个物理 JSONL 文件一条 Session 元数据
		result.Sessions = append(result.Sessions, model.Session{
			ID:        fileSessionID,
			Client:    model.ClientWorkBuddy,
			Directory: cwd,
			Project:   workbuddyInferProject(cwd),
			Title:     titleMap[fileSessionID],
			FirstTS:   firstTS,
			LastTS:    lastTS,
		})
	}

	return result, nil
}

// workBuddyMessage 将单条解析消息转换为 model.Message。
// total: usage.TotalTokens 原样保留，缺失时回退 input+output。
// fresh = max(0, input - cache_read)（无 cache_create、无 reasoning）。
func workBuddyMessage(fileSessionID string, in workbuddyParsedMessage, modelMapping map[string]string) model.Message {
	total := in.TotalTokens
	if total == 0 {
		total = in.InputTokens + in.OutputTokens
	}
	provider := strings.TrimSpace(modelMapping[in.Model])
	if provider == "" {
		// WorkBuddy 的 models.json 可能不包含当前模型；客户端来源仍可确定。
		provider = model.ClientWorkBuddy
	}
	return model.Message{
		ID:                in.ID,
		SessionID:         fileSessionID,
		Client:            model.ClientWorkBuddy,
		Date:              workbuddyTsMsToDate(in.Timestamp),
		TS:                in.Timestamp,
		Model:             in.Model,
		Provider:          provider,
		Directory:         in.Cwd,
		Project:           workbuddyInferProject(in.Cwd),
		InputTokens:       in.InputTokens,
		FreshInputTokens:  model.SubtractCache(in.InputTokens, in.CacheReadTokens, 0),
		OutputTokens:      in.OutputTokens,
		CacheReadTokens:   in.CacheReadTokens,
		CacheCreateTokens: 0,
		ReasoningTokens:   0,
		TotalTokens:       total,
	}
}

// scanWorkBuddyJSONL 三层路径扫描 projects/*/*.jsonl
// 跳过 */subagents/*.jsonl（子代理日志，首版不采集）
func scanWorkBuddyJSONL(projectsDir string) ([]string, error) {
	return scanWorkBuddyJSONLContext(context.Background(), projectsDir)
}

func scanWorkBuddyJSONLContext(ctx context.Context, projectsDir string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectsDir) == "" {
		return nil, fmt.Errorf("projects_dir 未配置")
	}
	info, err := os.Stat(projectsDir)
	if os.IsNotExist(err) {
		return nil, nil // 目录不存在视为无数据，不报错
	}
	if err != nil {
		return nil, fmt.Errorf("访问 projects_dir 失败: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("projects_dir 不是目录: %s", projectsDir)
	}
	projectEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, fmt.Errorf("读取 projects_dir 失败: %w", err)
	}
	files := make([]string, 0)
	for _, projectEntry := range projectEntries {
		if err := ctx.Err(); err != nil {
			return files, err
		}
		projectPath := filepath.Join(projectsDir, projectEntry.Name())
		projectInfo, statErr := os.Stat(projectPath)
		if statErr != nil {
			return nil, fmt.Errorf("访问 WorkBuddy 项目目录 %s 失败: %w", projectPath, statErr)
		}
		if !projectInfo.IsDir() || projectEntry.Name() == "subagents" {
			continue
		}
		entries, readErr := os.ReadDir(projectPath)
		if readErr != nil {
			return nil, fmt.Errorf("读取 WorkBuddy 项目目录 %s 失败: %w", projectPath, readErr)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return files, err
			}
			if filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			path := filepath.Join(projectPath, entry.Name())
			fileInfo, statErr := os.Stat(path)
			if statErr != nil {
				return nil, fmt.Errorf("访问 WorkBuddy JSONL %s 失败: %w", path, statErr)
			}
			if fileInfo.Mode().IsRegular() {
				files = append(files, path)
			}
		}
	}
	return files, nil
}

// workbuddyMessage 对应 JSONL 一行（只声明采集需要的字段，其余忽略）
type workbuddyMessage struct {
	ID           string                 `json:"id"`
	Timestamp    int64                  `json:"timestamp"`
	Role         string                 `json:"role"`
	SessionID    string                 `json:"sessionId"`
	Cwd          string                 `json:"cwd"`
	ProviderData *workbuddyProviderData `json:"providerData"`
}

// workbuddyProviderData 只解析 model 和 usage，其余字段忽略
type workbuddyProviderData struct {
	Model            string          `json:"model"`
	RequestModelName string          `json:"requestModelName"`
	Usage            *workbuddyUsage `json:"usage"`
}

// workbuddyUsage 对应 providerData.usage
type workbuddyUsage struct {
	InputTokens         int64                  `json:"inputTokens"`
	OutputTokens        int64                  `json:"outputTokens"`
	TotalTokens         int64                  `json:"totalTokens"`
	InputTokensDetails  []workbuddyTokenDetail `json:"inputTokensDetails"`
	OutputTokensDetails []workbuddyTokenDetail `json:"outputTokensDetails"`
}

// workbuddyTokenDetail 对应 inputTokensDetails/outputTokensDetails 的数组元素
type workbuddyTokenDetail struct {
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// workbuddyParsedMessage 解析后的单条有效消息
type workbuddyParsedMessage struct {
	ID              string
	SessionID       string
	Cwd             string
	Model           string
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CacheReadTokens int64
	Timestamp       int64
}

// parseWorkBuddyJSONL 解析 WorkBuddy JSONL，提取「带 usage 的 assistant 消息」
func parseWorkBuddyJSONL(path string, logger *slog.Logger) ([]workbuddyParsedMessage, error) {
	return parseWorkBuddyJSONLContext(context.Background(), path, logger)
}

func parseWorkBuddyJSONLContext(ctx context.Context, path string, logger *slog.Logger) ([]workbuddyParsedMessage, error) {
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
		return nil, fmt.Errorf("打开 WorkBuddy JSONL 失败: %w", err)
	}
	defer f.Close()

	var messages []workbuddyParsedMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineSize)
	lineNum := 0
	seen := make(map[string]struct{})
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lineNum++
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg workbuddyMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			logger.Debug("WorkBuddy JSONL line parse failed, skipped",
				"file", path,
				"line", lineNum,
				"error", err)
			continue
		}

		// 只处理 assistant 且能提取到 usage 的消息
		if msg.ID == "" || msg.Role != "assistant" || msg.ProviderData == nil || msg.ProviderData.Usage == nil {
			continue
		}
		if msg.Timestamp <= 0 {
			logger.Debug("WorkBuddy assistant message timestamp invalid, skipped",
				"file", path, "line", lineNum, "message_id", msg.ID, "timestamp", msg.Timestamp)
			continue
		}
		if _, ok := seen[msg.ID]; ok {
			continue
		}
		seen[msg.ID] = struct{}{}

		usage := msg.ProviderData.Usage
		parsed := workbuddyParsedMessage{
			ID:           msg.ID,
			SessionID:    msg.SessionID,
			Cwd:          msg.Cwd,
			Model:        extractWorkBuddyModel(msg.ProviderData),
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
			Timestamp:    msg.Timestamp,
		}
		// cached_tokens：取 inputTokensDetails 第一个元素的 cached_tokens
		// 实测 100% 存在；缺失时为 0
		if len(usage.InputTokensDetails) > 0 {
			parsed.CacheReadTokens = usage.InputTokensDetails[0].CachedTokens
		}

		messages = append(messages, parsed)
	}

	if err := scanner.Err(); err != nil {
		return messages, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// extractWorkBuddyModel 提取模型名
// 优先级：providerData.model（短 id，能匹配 models.json）> requestModelName（显示名，回退）
func extractWorkBuddyModel(pd *workbuddyProviderData) string {
	if pd.Model != "" {
		return pd.Model
	}
	return pd.RequestModelName
}

// workbuddyTsMsToDate 毫秒时间戳转日期字符串
// 独立定义：遵循客户端文件独立性原则，不复用 claude.go 的 tsMsToDate
func workbuddyTsMsToDate(tsMs int64) string {
	if tsMs <= 0 {
		return ""
	}
	return time.UnixMilli(tsMs).Format("2006-01-02")
}

// workbuddyInferProject 从工作目录路径提取项目名（末段）
// 用 filepath.Base 正确处理尾斜杠；独立定义遵循客户端文件独立性原则
func workbuddyInferProject(directory string) string {
	return projectBase(directory)
}

// loadWorkBuddyModelsMapping 加载 ~/.workbuddy/models.json
// 返回 model短id -> vendor 映射（vendor 作为 provider）
// 文件不存在视为空映射（用户未配置自定义模型）
func loadWorkBuddyModelsMapping(workbuddyDir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(workbuddyDir, "models.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("读取 models.json 失败: %w", err)
	}

	var models []struct {
		ID     string `json:"id"`
		Vendor string `json:"vendor"`
	}
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("解析 models.json 失败: %w", err)
	}

	mapping := make(map[string]string, len(models))
	for _, m := range models {
		if m.ID != "" && m.Vendor != "" {
			mapping[m.ID] = m.Vendor
		}
	}
	return mapping, nil
}

// queryWorkBuddyTitles 从 workbuddy.db sessions 表查询标题
// 返回 map[sessionID]title（custom_title 优先于 title，排除已删除）
func queryWorkBuddyTitles(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(NULLIF(custom_title, ''), title) AS title
		FROM sessions
		WHERE deleted_at IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("查询 workbuddy sessions 失败: %w", err)
	}
	defer rows.Close()

	titles := make(map[string]string)
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			continue // title 为可选装饰信息，单行扫描失败跳过，不阻断采集
		}
		if id != "" && title != "" {
			titles[id] = title
		}
	}
	return titles, rows.Err()
}
