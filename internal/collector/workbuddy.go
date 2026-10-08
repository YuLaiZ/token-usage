package collector

import (
	"context"
	"database/sql"
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
	"github.com/YuLaiZ/token-usage/internal/fsident"
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

// workBuddyAssistantTitle 是本地助理会话的固定落库标题：助理页在源库的 title
// 是旧话题标题（如「卫生间下雨渗水原因排查」），不能透传。
const workBuddyAssistantTitle = "本地助理"

// WorkBuddyMetadataErrPrefix 是 WorkBuddy 元数据/暂缓错误的固定可识别片段：
// 进入 collection_errors.message 后，完整无日期复核成功时按该片段解决
// （db.ResolveWorkBuddyErrorsByMessagePattern），不受登记日期限制。engine 侧
// 的复核完整性检查复用同一片段。
const WorkBuddyMetadataErrPrefix = "workbuddy metadata failed:"

// Collect 逐消息采集：每个顶层 message.id 产出一条 model.Message，每个物理
// JSONL 文件产出一条 Session 元数据。
//
// 元数据强依赖合同：workbuddy.db 是会话归属（标题/project/client）的唯一判据。
// db 路径未配置、打开、读取或必要列缺失时本轮整体失败（不沿用历史「title
// 装饰字段失败仍采 token」的降级）；单会话元数据未命中或无效时该会话暂缓
// （消息与计划都不产出、经 PartialErr 报告保留重试），其他成功会话正常落库。
// req.ChangedFile 非空时只扫描该文件；无日期、无 ChangedFile 请求的计划覆盖
// 全部有效元数据会话（含仅历史库数据的会话，供 engine 刷新历史归属）。
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

	home, err := os.UserHomeDir()
	if err != nil {
		return CollectResult{}, fmt.Errorf("%s获取用户主目录失败: %w", WorkBuddyMetadataErrPrefix, err)
	}

	// 1. 元数据快照（强依赖；db 未配置属配置不完整，须报告失败——不能在检查
	// db 前因 projects_dir 为空或文件列表为空而静默成功返回）
	dbPath := clientCfg.Paths["db"]
	if dbPath == "" {
		return CollectResult{}, fmt.Errorf(
			"%sdb 路径未配置（clients.workbuddy.paths.db），无法判定会话归属，不猜测写入",
			WorkBuddyMetadataErrPrefix)
	}
	metaDB, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return CollectResult{}, fmt.Errorf("%s打开源库失败: %w", WorkBuddyMetadataErrPrefix, err)
	}
	metaMap, err := queryWorkBuddyMetadata(ctx, metaDB)
	metaDB.Close()
	if err != nil {
		return CollectResult{}, fmt.Errorf("%s%w", WorkBuddyMetadataErrPrefix, err)
	}

	// 2. 文件列表：ChangedFile 优先（daemon 增量），否则扫描 projects 目录。
	// projects_dir 为空且无 ChangedFile 时 files 为空：这是合法的「仅历史刷新」
	// 形态——计划仍按元数据与用量库家族行构造，不在 db 检查前静默返回。
	var files []string
	if req.ChangedFile != "" {
		files = []string{req.ChangedFile}
	} else if projectsDir := clientCfg.Paths["projects_dir"]; projectsDir != "" {
		files, err = scanWorkBuddyJSONLContext(ctx, projectsDir)
		if err != nil {
			return CollectResult{}, fmt.Errorf("扫描 WorkBuddy JSONL 失败: %w", err)
		}
	}

	// 3. 加载 models.json（model 短 id -> vendor）；仅在确有文件要解析时读取。
	var modelMapping map[string]string
	if len(files) > 0 {
		workbuddyDir := filepath.Dir(clientCfg.Paths["projects_dir"])
		modelMapping, err = loadWorkBuddyModelsMapping(workbuddyDir)
		if err != nil {
			return CollectResult{}, fmt.Errorf("加载 models.json 失败: %w", err)
		}
	}

	// 4. 日期过滤集合（dates 为空时不过滤，即全量）
	dateSet := make(map[string]bool, len(req.Dates))
	for _, d := range req.Dates {
		dateSet[d] = true
	}

	// 5. 逐文件解析，按会话汇总后统一产出。同一 session ID 的多份物理文件按
	// 会话汇总决策：任一文件发生终止性读取失败（或文件名无效）→ 该会话整会话
	// 暂缓——全部 messages、sessions 与计划都不产出，成功文件的内容也不得
	// 让失败会话的历史归属被刷新；全部文件成功且元数据有效才进入结果。
	var result CollectResult
	type sessionOutcome struct {
		plan       WorkBuddySessionPlan // 会话级目标计划（跨文件一致性已校验；区间在产出阶段汇总回填）
		firstTS    int64                // 跨全部成功文件的最小正时间戳
		lastTS     int64                // 跨全部成功文件的最大时间戳
		messages   []model.Message
		hasPlan    bool // 是否有成功文件贡献过计划
		hasSession bool // 是否有命中日期的消息（产出 Session 行）
		deferred   bool // 任一文件失败或跨文件目标矛盾：整会话不产出
		deferErr   error
	}
	sessions := make(map[string]*sessionOutcome)
	sessionOrder := []string{}
	getOutcome := func(id string) *sessionOutcome {
		if o, ok := sessions[id]; ok {
			return o
		}
		o := &sessionOutcome{}
		sessions[id] = o
		sessionOrder = append(sessionOrder, id)
		return o
	}

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// fileSessionID 来自文件名（去 .jsonl），作为 SessionID 与元数据关联键。
		// 在解析前提取：文件级失败（打开/读取终止性错误）也要按会话粒度暂缓。
		fileSessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")

		before := fsident.SnapshotOfFile(file)
		if skipGateHit(req.SkipGate, file, before) {
			result.FileStatuses = append(result.FileStatuses, FileScanStatus{Path: file, Skipped: true, Before: before})
			continue
		}
		messages, fileCwd, status, err := parseWorkBuddyJSONLContext(ctx, file, logger)
		status.Before = before
		status.After = fsident.SnapshotOfFile(file)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			logger.Warn("WorkBuddy JSONL file parse failed, skipped", "file", file, "error", err)
			status.Err = err
			result.FileStatuses = append(result.FileStatuses, status)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, err))
			if fileSessionID != "" {
				o := getOutcome(fileSessionID)
				o.deferred = true
				o.deferErr = err
			}
			continue
		}

		if fileSessionID == "" {
			err := fmt.Errorf("%s: 文件名缺少 session ID", file)
			result.PartialErr = errors.Join(result.PartialErr, err)
			logger.Warn("WorkBuddy JSONL file name invalid, skipped", "file", file)
			// 文件级失败：该文件不得推进跳过门（消息已读但未完整产出）。
			status.Err = err
			result.FileStatuses = append(result.FileStatuses, status)
			continue
		}

		// 会话计划：元数据未命中或无效时该会话整体暂缓——本轮不产出消息、
		// 会话与计划（不区分已有/新增消息做猜测写入），经文件失败状态与
		// PartialErr 报告，保留重试机会。
		plan, planErr := workBuddyPlanFor(metaMap, fileSessionID, fileCwd, home)
		if planErr != nil {
			status.Err = planErr
			result.FileStatuses = append(result.FileStatuses, status)
			result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", file, planErr))
			o := getOutcome(fileSessionID)
			o.deferred = true
			if o.deferErr == nil {
				o.deferErr = planErr
			}
			continue
		}
		result.FileStatuses = append(result.FileStatuses, status)

		var firstTS, lastTS int64
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
			date := workbuddyTsMsToDate(pm.Timestamp)
			if len(req.Dates) > 0 && !dateSet[date] {
				continue
			}
			hitMessages = append(hitMessages, workBuddyMessage(fileSessionID, pm, modelMapping, plan))
		}

		plan.FirstTS = firstTS
		plan.LastTS = lastTS
		plan.HasJSONL = true
		o := getOutcome(fileSessionID)
		if o.hasPlan {
			// 同一 session 的多份成功文件：目标（client/directory/project/title）
			// 必须一致——源库 cwd 为空时 directory 依赖各文件的 fileCwd，可能
			// 产出相互矛盾的目标。矛盾即整会话暂缓（方案：同批相互矛盾的
			// 目标不进入成功计划），其他有效会话不受影响。
			if o.plan.Client != plan.Client || o.plan.Directory != plan.Directory ||
				o.plan.Project != plan.Project || o.plan.Title != plan.Title {
				conflict := fmt.Errorf("%s会话 %s 的多份 JSONL 文件产出相互矛盾的目标（directory/project 不一致），本轮暂缓",
					WorkBuddyMetadataErrPrefix, fileSessionID)
				o.deferred = true
				if o.deferErr == nil {
					o.deferErr = conflict
				}
				result.PartialErr = errors.Join(result.PartialErr, conflict)
				continue
			}
		}
		o.hasPlan = true
		o.plan = plan
		// 时间区间跨文件汇总（min 正 ts / max ts）：最终计划与 session 行携带
		// 整个会话本轮的最早/最晚时间，不只剩最后一个文件的区间。
		if firstTS > 0 && (o.firstTS == 0 || firstTS < o.firstTS) {
			o.firstTS = firstTS
		}
		if lastTS > o.lastTS {
			o.lastTS = lastTS
		}
		o.messages = append(o.messages, hitMessages...)
		if len(hitMessages) > 0 {
			o.hasSession = true
		}
	}

	// 按会话统一产出：deferred 会话丢弃全部产出（整会话暂缓，PartialErr 已
	// 报告）；成功会话产出消息与会话行，计划与会话行携带跨文件汇总的时间
	// 区间。
	touched := make(map[string]bool)
	for _, id := range sessionOrder {
		o := sessions[id]
		if o.deferred {
			continue
		}
		if !o.hasPlan {
			continue // 只有失败文件贡献过 outcome：无计划可产出
		}
		plan := o.plan
		plan.FirstTS = o.firstTS
		plan.LastTS = o.lastTS
		o.plan = plan
		result.Messages = append(result.Messages, o.messages...)
		if o.hasSession {
			result.Sessions = append(result.Sessions, model.Session{
				ID:        id,
				Client:    plan.Client,
				Directory: plan.Directory,
				Project:   plan.Project,
				Title:     plan.Title,
				FirstTS:   o.firstTS,
				LastTS:    o.lastTS,
			})
		}
		touched[id] = true
	}

	// 6. 计划构造：无日期全量请求（含启动 catch-up 与周期复核）覆盖全部有效
	// 元数据会话；日期/ChangedFile 请求只覆盖本轮触达的会话。计划的会话顺序
	// 按 sessionID 升序，保证 engine 执行确定性。文件级读取失败的会话整会话
	// 暂缓，从全量计划排除（其 PartialErr 已报告）；「元数据存在但无效」的
	// 会话记入 Excluded，由 engine 核对用量库历史行后决定复核完整性。
	fullPlanScope := len(req.Dates) == 0 && req.ChangedFile == ""
	var plans []WorkBuddySessionPlan
	if fullPlanScope {
		ids := make([]string, 0, len(metaMap))
		for id := range metaMap {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if touched[id] {
				plans = append(plans, sessions[id].plan)
				continue
			}
			if sessions[id] != nil && sessions[id].deferred {
				continue // 文件级失败：整会话暂缓（PartialErr 已报告），不刷新历史
			}
			// 无 JSONL 触达的仅历史会话：jsonlCwd 为空（不凭文件夹转写反推
			// 路径），计划有效即纳入——是否真有历史行由 engine 按用量库判定，
			// 完全无数据的源库行在 engine 侧忽略。
			p, perr := workBuddyPlanFor(metaMap, id, "", home)
			if perr == nil {
				plans = append(plans, p)
				continue
			}
			// 元数据存在但无效（未命中的已登记会话在此列，源库无行的则不进
			// metaMap）：collector 无法判断其是否已有用量库历史行，记入
			// Excluded 交 engine 核对；完全无历史行的由 engine 忽略。
			result.WorkBuddyExcluded = append(result.WorkBuddyExcluded,
				WorkBuddyExcludedSession{SessionID: id, Reason: perr})
		}
	} else {
		touchedIDs := make([]string, 0, len(touched))
		for id := range touched {
			touchedIDs = append(touchedIDs, id)
		}
		sort.Strings(touchedIDs)
		for _, id := range touchedIDs {
			plans = append(plans, sessions[id].plan)
		}
	}
	result.WorkBuddyPlans = plans

	return result, nil
}

// workbuddySessionMeta 是源库 sessions 表的有效元组（deleted_at IS NULL）。
// NULL 文本列按空字符串读取；is_playground 的 NULL 由 playgroundOK 区分。
type workbuddySessionMeta struct {
	id           string
	customTitle  string
	title        string
	cwd          string
	sourceMode   string
	mode         string
	playground   int64
	playgroundOK bool
	expertID     string
}

// queryWorkBuddyMetadata 在源库只读读取全部有效会话元组（同一快照供所有判定
// 与校正使用）。必要列缺失（旧 schema）由查询错误自然覆盖，调用方整轮失败。
// 重复 id 元数据按异常报告，不进入计划。
func queryWorkBuddyMetadata(ctx context.Context, db *sql.DB) (map[string]workbuddySessionMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id,
		       COALESCE(custom_title, ''),
		       COALESCE(title, ''),
		       COALESCE(cwd, ''),
		       COALESCE(source_mode, ''),
		       COALESCE(mode, ''),
		       is_playground,
		       COALESCE(expert_id, '')
		FROM sessions
		WHERE deleted_at IS NULL
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("查询 workbuddy sessions 元数据失败: %w", err)
	}
	defer rows.Close()

	metas := make(map[string]workbuddySessionMeta)
	for rows.Next() {
		var m workbuddySessionMeta
		var playground sql.NullInt64
		if err := rows.Scan(&m.id, &m.customTitle, &m.title, &m.cwd,
			&m.sourceMode, &m.mode, &playground, &m.expertID); err != nil {
			return nil, fmt.Errorf("扫描 workbuddy sessions 元数据失败: %w", err)
		}
		if m.id == "" {
			return nil, fmt.Errorf("workbuddy sessions 元数据存在空 id 行")
		}
		if _, dup := metas[m.id]; dup {
			return nil, fmt.Errorf("workbuddy sessions 元数据重复 id: %s", m.id)
		}
		m.playground, m.playgroundOK = playground.Int64, playground.Valid
		metas[m.id] = m
	}
	return metas, rows.Err()
}

// expandWorkBuddyHomePath 把 "~" 与 "~/"（Windows 上也接受 "~\"）展开到主目录；
// 其他值（含 "~otheruser"、环境变量形态）原样返回，不做文件系统探测。
func expandWorkBuddyHomePath(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// isWorkBuddyAssistantPath 按「主目录下的 WorkBuddy/Claw 完整路径」判定助理
// 工作区：两侧 filepath.Clean 后严格字符串相等；不做大小写折叠、不解析符号
// 链接，相对路径不能命中（Clean 不改变相对性，目标路径恒为绝对路径）。
func isWorkBuddyAssistantPath(cwd, home string) bool {
	target := filepath.Clean(filepath.Join(home, "WorkBuddy", "Claw"))
	return filepath.Clean(expandWorkBuddyHomePath(cwd, home)) == target
}

// workBuddyPlanFor 由单个会话的有效元数据构造目标归属计划。元数据未命中、
// is_playground 取值非 0/1（含 NULL）、expert_id 非空但非有效 UTF-8 时返回
// 错误：调用方将该会话整体暂缓并报告，不猜归属。
func workBuddyPlanFor(metaMap map[string]workbuddySessionMeta, sessionID, jsonlCwd, home string) (WorkBuddySessionPlan, error) {
	plan := WorkBuddySessionPlan{SessionID: sessionID}
	meta, ok := metaMap[sessionID]
	if !ok {
		return plan, fmt.Errorf("%s会话 %s 在源库无有效元数据（未登记或已删除），本轮暂缓", WorkBuddyMetadataErrPrefix, sessionID)
	}

	if !meta.playgroundOK || (meta.playground != 0 && meta.playground != 1) {
		return plan, fmt.Errorf("%s会话 %s 的 is_playground 取值无效（NULL 或非 0/1），本轮暂缓", WorkBuddyMetadataErrPrefix, sessionID)
	}

	client := model.ClientWorkBuddy
	if meta.expertID != "" {
		key := model.WorkBuddyExpertClientKey(meta.expertID)
		if key == "" {
			return plan, fmt.Errorf("%s会话 %s 的 expert_id 非有效 UTF-8，本轮暂缓", WorkBuddyMetadataErrPrefix, sessionID)
		}
		client = key
	}

	title := meta.customTitle
	if title == "" {
		title = meta.title
	}
	directory := meta.cwd
	// 助理识别只用源库 cwd（不使用 JSONL 首条 cwd 兜底猜入口类型）
	if isWorkBuddyAssistantPath(directory, home) && meta.sourceMode == "" && meta.mode == "" {
		title = workBuddyAssistantTitle
	}
	if directory == "" {
		directory = jsonlCwd
	}

	plan.Client = client
	plan.Title = title
	plan.Directory = directory
	plan.Playground = meta.playground == 1
	if !plan.Playground {
		plan.Project = workbuddyInferProject(directory)
	}
	return plan, nil
}

// workBuddyMessage 将单条解析消息转换为目标归属（client/project 来自会话计划）
// 下的 model.Message。消息 directory 保留原始 JSONL cwd（消息级事实，不随会话
// 计划改写）；provider 回退保持普通 client 名，不随 expert client 改名。
// total: usage.TotalTokens 原样保留，缺失时回退 input+output。
// fresh = max(0, input - cache_read)（无 cache_create、无 reasoning）。
func workBuddyMessage(fileSessionID string, in workbuddyParsedMessage, modelMapping map[string]string, plan WorkBuddySessionPlan) model.Message {
	total := in.TotalTokens
	if total == 0 {
		total = in.InputTokens + in.OutputTokens
	}
	provider := strings.TrimSpace(modelMapping[in.Model])
	if provider == "" {
		// 会话日志中的模型 id 被客户端小写化时，用小写键兜底匹配 models.json 条目。
		provider = strings.TrimSpace(modelMapping[strings.ToLower(in.Model)])
	}
	if provider == "" {
		// WorkBuddy 的 models.json 可能不包含当前模型；客户端来源仍可确定。
		provider = model.ClientWorkBuddy
	}
	return model.Message{
		ID:                in.ID,
		SessionID:         fileSessionID,
		Client:            plan.Client,
		Date:              workbuddyTsMsToDate(in.Timestamp),
		TS:                in.Timestamp,
		Model:             in.Model,
		Provider:          provider,
		Directory:         in.Cwd,
		Project:           plan.Project,
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
func parseWorkBuddyJSONL(path string, logger *slog.Logger) ([]workbuddyParsedMessage, string, FileScanStatus, error) {
	return parseWorkBuddyJSONLContext(context.Background(), path, logger)
}

// parseWorkBuddyJSONLContext 解析 WorkBuddy JSONL，提取「带 usage 的 assistant
// 消息」。fileCwd 是该文件按物理顺序第一条非空 cwd——收集发生在结构性过滤
// （非 assistant、无 usage）之前，任何原生 JSON 行的 cwd 都参与；消息自身的
// directory 仍保留各自原值。返回的 FileScanStatus 已填 Path/BadLines/FirstBad*/
// TrailingNewline（Before/After 快照与 Err 由 Collect 层补充）。
func parseWorkBuddyJSONLContext(ctx context.Context, path string, logger *slog.Logger) ([]workbuddyParsedMessage, string, FileScanStatus, error) {
	var status FileScanStatus
	status.Path = path
	fileCwd := ""
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fileCwd, status, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fileCwd, status, fmt.Errorf("打开 WorkBuddy JSONL 失败: %w", err)
	}
	defer f.Close()

	var outcome parseFileOutcome
	var messages []workbuddyParsedMessage
	it := newJSONLLineIter(ctx, f, maxJSONLLineSize)
	seen := make(map[string]struct{})
	for it.Next() {
		// 迭代器已按行检查 ctx：取消时终止迭代并经 Err() 返回。
		if it.Oversized() {
			outcome.addBad(it.LineNo(), errJSONLLineOversized)
			continue
		}
		lineNum := it.LineNo()
		line := it.Line()
		if len(line) == 0 {
			continue
		}

		var msg workbuddyMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			outcome.addBad(lineNum, err)
			continue
		}

		// 文件级首条非空 cwd：任何可解析的原生 JSON 行都参与（含 user 消息、
		// 无 usage 的 assistant 等），与消息过滤无关。
		if fileCwd == "" && msg.Cwd != "" {
			fileCwd = msg.Cwd
		}

		// 只处理 assistant 且能提取到 usage 的消息（结构性过滤，不计坏行）
		if msg.ID == "" || msg.Role != "assistant" || msg.ProviderData == nil || msg.ProviderData.Usage == nil {
			continue
		}
		if msg.Timestamp <= 0 {
			// assistant 消息已带 usage 却因 timestamp 无效被丢弃：数据异常，计入坏行。
			outcome.addBad(lineNum, fmt.Errorf("assistant 消息 timestamp 无效: %d", msg.Timestamp))
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

	if err := it.Err(); err != nil {
		// ctx 取消丢弃已解析部分（原语义）；IO 错误保留部分结果。按 err 本身
		// 判来源（迭代器取消时返回 ctx.Err()，IO 错误返回原始错误），避免取消
		// 与 IO 错误并发时误丢 partial。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fileCwd, status, err
		}
		return messages, fileCwd, status, err
	}
	// 尾行未以 \n 终结：可能仍在写，不得视为完整采集。
	if fi, serr := f.Stat(); serr == nil {
		outcome.trailingNewline = tailHasNewline(f, fi.Size())
	}
	if err := ctx.Err(); err != nil {
		return nil, fileCwd, status, err
	}
	if outcome.badLines > 0 {
		logger.Debug("WorkBuddy JSONL line parse failed, skipped",
			"file", path, "count", outcome.badLines, "first_line", outcome.firstBadLine, "error", outcome.firstBadErr)
	}
	outcome.fillStatus(&status)
	return messages, fileCwd, status, nil
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
// 返回 model短id -> vendor 映射（vendor 作为 provider）；
// 混合大小写条目额外补一条小写兜底键，应对会话日志中模型 id 被客户端小写化
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

	mapping := make(map[string]string, len(models)*2)
	// 小写兜底键的来源条目 id：同一原始 id 的重复条目在其仍持有兜底键时
	// 跟随精确键更新 vendor（全小写条目接管同名键后属主被撤销，不再跟随）；
	// 不同大小写变体之间按条目顺序先到优先。
	fallbackOwner := make(map[string]string, len(models))
	for _, m := range models {
		if m.ID == "" || m.Vendor == "" {
			continue
		}
		mapping[m.ID] = m.Vendor
		// WorkBuddy 会话日志中的模型短 id 被客户端小写化，与条目 id 的大小写
		// 可能不一致；补一条小写兜底键。该键已被其他条目（含全小写精确键、
		// 其他大小写变体）占用时不覆盖；由相同原始 id 持有时跟随更新。
		// 全小写条目的精确写入接管同名兜底键后撤销原属主更新权，防止后续
		// 原混合 id 的重复条目反覆盖该精确值。
		if lower := strings.ToLower(m.ID); lower != m.ID {
			if _, exists := mapping[lower]; !exists || fallbackOwner[lower] == m.ID {
				mapping[lower] = m.Vendor
				fallbackOwner[lower] = m.ID
			}
		} else {
			delete(fallbackOwner, m.ID)
		}
	}
	return mapping, nil
}
