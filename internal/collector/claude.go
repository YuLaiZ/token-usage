package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	// coworkOutcomes 是参与组裁决的 Cowork transcript（含映射损坏而跳过的副本，
	// 不含主根内副本）。同一逻辑会话 ID 的多份现存文件必须一起核对：元数据来源
	// 在写入前确定，不依赖库中残留状态，也不受跳过门命中与否影响；组内任一
	// 副本映射损坏时整组冻结，健康副本不得接管归因。
	var coworkOutcomes []claudeCoworkTranscriptOutcome
	// 枚举不完整时，未读到的目录可能包含任意同 ID 副本，不能裁决 Cowork 归因。
	var coworkScanErr error
	coworkRoots := claudeCoworkRootsFromEnv()
	// projectsRoots 用作 Cowork 与主根的重叠边界：会话目录 .claude 为符号链接时，
	// Cowork 枚举会跟随到 projects 根下的 transcript。这类副本由主根全扫按其
	// entrypoint 正常判定，必须跳过 Cowork 强制 Desktop，避免同一 message.id
	// 跨 client 双计。
	agentRoots := ClaudeProjectsRoots(c.cfg)
	projectsRoots := slices.Clone(agentRoots)
	for i, root := range projectsRoots {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			projectsRoots[i] = real
		}
	}
	changed := req.ChangedFile
	if changed != "" {
		// 与全扫、watcher 事件统一为绝对 Clean 形态（受控判定根为绝对路径）。
		if abs, aerr := filepath.Abs(changed); aerr == nil {
			changed = filepath.Clean(abs)
		}
		if IsClaudeCoworkTranscriptInRoots(coworkRoots, changed) {
			// 通过 Cowork 别名补采主根文件时用真实路径，保留原生分类和子代理归属。
			// 在读取 Cowork 映射前判定，主根文件不受别名侧映射状态影响。
			if native := claudeCoworkProjectsPath(changed, projectsRoots); native != "" {
				files = []string{native}
			}
		}
		if len(files) == 0 {
			out, inCoworkRoot, controlled := claudeCoworkOutcomeForPath(coworkRoots, changed, logger)
			switch {
			case inCoworkRoot && !controlled:
				logger.Debug("Claude Cowork non-transcript ChangedFile skipped", "file", changed)
				return CollectResult{}, nil
			case controlled && out.Skip:
				coworkOutcomes = []claudeCoworkTranscriptOutcome{out}
			case controlled:
				coworkOutcomes, coworkScanErr = claudeCoworkGroupForChanged(ctx, coworkRoots, changed, out, projectsRoots, logger)
			default:
				files = []string{changed}
			}
		}
	} else {
		files, scanErr = findClaudeJSONLFiles(ctx, clientCfg.Paths["projects_dir"])
		home, _ := os.UserHomeDir()
		if extraDirs := discoverCCSwitchClaudeProjects(home); len(extraDirs) > 0 {
			extraFiles, extraErr := findClaudeJSONLFilesInDirs(ctx, extraDirs)
			files = dedupeCleanedFilePaths(files, extraFiles)
			scanErr = errors.Join(scanErr, extraErr)
		}
		// Cowork 补采：逐层 ReadDir 枚举受控布局 transcript，按会话目录关联
		// 映射。映射损坏的副本也保留在组内（Skip 标记），让组裁决按会话 ID
		// 整组冻结；失败原因统一在组裁决按会话目录 Warn 一次并计入部分失败。
		coworkOutcomes, coworkScanErr = claudeCoworkEnumerate(ctx, coworkRoots, logger)
		coworkOutcomes = slices.DeleteFunc(coworkOutcomes, func(out claudeCoworkTranscriptOutcome) bool {
			return claudeCoworkProjectsPath(out.Path, projectsRoots) != ""
		})
	}
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}

	dateSet := make(map[string]struct{}, len(req.Dates))
	for _, date := range req.Dates {
		dateSet[date] = struct{}{}
	}

	var result CollectResult
	if scanErr != nil {
		result.PartialErr = fmt.Errorf("查找 Claude JSONL 文件失败: %w", scanErr)
	}
	if coworkScanErr != nil {
		scanFailure := fmt.Errorf("查找 Claude Cowork 文件失败: %w", coworkScanErr)
		logger.Warn("Claude Cowork enumeration incomplete", "changed_file", changed, "error", coworkScanErr)
		result.PartialErr = errors.Join(result.PartialErr, scanFailure)
		if changed != "" {
			result.FileStatuses = append(result.FileStatuses, FileScanStatus{Path: changed, Err: scanFailure})
		}
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
		part, status, err := parseClaudeMessageFile(file, agentRoots, dateSet, logger)
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

	// Cowork 组裁决：同一逻辑会话 ID 的多份现存 transcript 一起核对，按路径
	// 字典序取第一份实际产出用量的文件提供会话元数据。全扫、启动补采与
	// ChangedFile 共用这条规则，避免同一组文件因跳过门状态不同而选出不同的
	// 标题或项目。组内只有一份时继续使用跳过门（未变文件不重读，只走
	// UPDATE-only 元数据通道）。组内任一副本映射损坏时整组冻结：健康但无映射
	// 的副本不得接管归因，否则会把已有 sessions/messages 的 directory/project
	// 写空；失败逐条计入部分失败，映射修复后重试。
	groups := groupClaudeCoworkOutcomes(coworkOutcomes)
	groupIDs := make([]string, 0, len(groups))
	for id := range groups {
		groupIDs = append(groupIDs, id)
	}
	sort.Strings(groupIDs)
	// Warn 按失败原因（会话目录 + 映射路径）去重：同一目录下的多个 transcript
	// 只打印一条，部分失败通道仍按文件逐条记录。
	warnedSessionDirs := make(map[string]struct{})
	for _, id := range groupIDs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if skips := claudeCoworkGroupSkips(groups[id]); len(skips) > 0 {
			for _, out := range skips {
				skipErr := out.SkipErr
				if skipErr == nil {
					skipErr = fmt.Errorf("Claude Cowork transcript skipped: %s", out.Path)
				}
				if _, dup := warnedSessionDirs[skipErr.Error()]; !dup {
					warnedSessionDirs[skipErr.Error()] = struct{}{}
					logger.Warn("Claude Cowork transcript skipped", "file", out.Path, "reason", skipErr)
				}
				result.FileStatuses = append(result.FileStatuses, FileScanStatus{Path: out.Path, Err: skipErr})
				result.PartialErr = errors.Join(result.PartialErr, fmt.Errorf("%s: %w", out.Path, skipErr))
			}
			continue
		}
		if coworkScanErr != nil {
			continue
		}
		msgs, sess, statuses, updates, groupErrs := collectClaudeCoworkGroup(ctx, groups[id], req.SkipGate, agentRoots, dateSet, logger)
		result.Messages = append(result.Messages, msgs...)
		result.Sessions = append(result.Sessions, sess...)
		result.FileStatuses = append(result.FileStatuses, statuses...)
		result.SessionMetaUpdates = append(result.SessionMetaUpdates, updates...)
		for _, gerr := range groupErrs {
			result.PartialErr = errors.Join(result.PartialErr, gerr)
		}
		// 组内解析中途取消时不能把结果当成功返回：与普通文件路径一致，取消
		// 是请求中止而不是部分失败。
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

// collectClaudeCoworkGroup 处理同一逻辑会话 ID 的一组 transcript（group 已按
// 绝对路径字典序排序，且调用方已保证组内没有映射损坏的副本）。组内只有一份时
// 沿用跳过门：门命中只提交 UPDATE-only 元数据候选，不重读文件。组内有多份现存
// 副本时整组强制重读——只有实际读取内容才能判断哪一份真正产出用量，按「第一份
// 实际产出用量」确定元数据来源，其余副本只贡献消息、不产出会话行。调用方负责
// 把返回值并入 CollectResult。
func collectClaudeCoworkGroup(
	ctx context.Context,
	group []claudeCoworkTranscriptOutcome,
	gate FileSkipGate,
	agentRoots []string,
	dateSet map[string]struct{},
	logger *slog.Logger,
) (messages []model.Message, sessions []model.Session, statuses []FileScanStatus, metaUpdates []model.Session, errs []error) {
	if len(group) == 1 {
		out := group[0]
		path := out.Path
		before := fsident.SnapshotOfFile(path)
		if skipGateHit(gate, path, before) {
			statuses = append(statuses, FileScanStatus{Path: path, Skipped: true, Before: before})
			// 门命中且映射可用：只更新已存在会话行的元数据（UPDATE-only），
			// 不读 transcript、不建行。title/directory/project 空值不覆盖库值；
			// 无映射 transcript 门命中无元数据可更新，不做任何更新（不清空历史值）。
			if id := strings.TrimSpace(out.Meta.CLISessionID); id != "" {
				directory, project := claudeCoworkDirectoryProject(out.Meta)
				metaUpdates = append(metaUpdates, model.Session{
					ID:        id,
					Client:    model.ClientClaudeDesktop,
					Title:     out.Meta.Title,
					Directory: directory,
					Project:   project,
				})
			}
			return
		}
		part, status, err := parseClaudeMessageFile(path, agentRoots, dateSet, logger)
		status.Before = before
		status.After = fsident.SnapshotOfFile(path)
		statuses = append(statuses, status)
		if err != nil {
			logger.Warn("Claude Cowork transcript parse failed, skipped", "file", path, "error", err)
			statuses[len(statuses)-1].Err = err
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		applyCoworkGroupMeta(&part, out, true)
		messages = part.Messages
		sessions = part.Sessions
		metaUpdates = coworkMetaUpdatesForSessions(out, part.Sessions)
		return
	}

	// 多副本：整组强制重读，结果不依赖跳过门状态。
	parts := make([]CollectResult, len(group))
	for i, out := range group {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			return
		}
		path := out.Path
		before := fsident.SnapshotOfFile(path)
		part, status, err := parseClaudeMessageFile(path, agentRoots, dateSet, logger)
		status.Before = before
		status.After = fsident.SnapshotOfFile(path)
		statuses = append(statuses, status)
		if err != nil {
			logger.Warn("Claude Cowork transcript parse failed, skipped", "file", path, "error", err)
			statuses[len(statuses)-1].Err = err
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		parts[i] = part
	}
	if len(errs) > 0 {
		// 部分读取不能证明归因来源。保留失败文件状态，未写入的成功副本不推进跳过门。
		statuses = slices.DeleteFunc(statuses, func(status FileScanStatus) bool { return status.Err == nil })
		return
	}
	// 元数据来源：排序后的第一份完整 transcript 产出用量的副本。判定基于
	// hasUsage（日期过滤之前记录），因此按日期采集不会改选另一份副本；空
	// transcript（只有用户消息、无 usage）不提供元数据，也不会挡住后面有
	// 消息的副本。
	source := -1
	for i := range group {
		if parts[i].hasUsage {
			source = i
			break
		}
	}
	if source < 0 {
		// 整组都没有用量：不产出会话行，也不提交元数据更新，避免空值覆盖库中
		// 既有归属（文件未变时单副本路径的门命中仍会走 UPDATE-only）。
		return
	}
	// 来源副本在当日没有消息（日期过滤后 Sessions 为空），但同组其他副本有
	// 消息等待入库：用来源副本的完整会话骨架生成会话行，消息仍按请求日期
	// 筛选。否则空库首次按日期采集会只写消息、不写会话行，会话排行查不到
	// 这批用量；而让次份生成会话行又会改写元数据来源。
	if len(parts[source].Sessions) == 0 && parts[source].sessionSkeleton != nil {
		for i := range group {
			if len(parts[i].Messages) > 0 {
				parts[source].Sessions = append(parts[source].Sessions, *parts[source].sessionSkeleton)
				break
			}
		}
	}
	for i := range group {
		applyCoworkGroupMeta(&parts[i], group[source], i == source)
		messages = append(messages, parts[i].Messages...)
		sessions = append(sessions, parts[i].Sessions...)
	}
	metaUpdates = coworkMetaUpdatesForSessions(group[source], sessions)
	return
}

// coworkMetaUpdatesForSessions 生成会话归因对齐候选：Cowork 的 directory/project
// 只由映射决定，且映射可能晚于 transcript 落盘（新增或删除）。消息 UPSERT 对
// 同一时间戳不会重写归因，因此会话写完后还要走一次 UPDATE-only 通道，把该会话
// 下消息的归因对齐到会话行，保证会话列表与按项目汇总口径一致。
func coworkMetaUpdatesForSessions(source claudeCoworkTranscriptOutcome, sessions []model.Session) []model.Session {
	if len(sessions) == 0 {
		return nil
	}
	directory, project := claudeCoworkDirectoryProject(source.Meta)
	updates := make([]model.Session, 0, len(sessions))
	for _, s := range sessions {
		updates = append(updates, model.Session{
			ID:        s.ID,
			Client:    s.Client,
			Title:     s.Title,
			Directory: directory,
			Project:   project,
		})
	}
	return updates
}

// findClaudeJSONLFiles 递归查找主根（projects_dir）下所有 .jsonl 文件；
// subagents 子树受控进入（agent transcript，见 walkClaudeJSONLDir）。
func findClaudeJSONLFiles(ctx context.Context, projectsDir string) ([]string, error) {
	if strings.TrimSpace(projectsDir) == "" {
		return nil, fmt.Errorf("Claude projects_dir 未配置")
	}
	// 统一为绝对 Clean 形态：watcher 事件与受控判定根均为绝对路径，相对配置
	// 必须在此规范化，否则两条入口的路径形态不一致（agent 判定与门记录分叉）。
	if abs, err := filepath.Abs(projectsDir); err == nil {
		projectsDir = filepath.Clean(abs)
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
	return walkClaudeJSONLDir(ctx, projectsDir)
}

// CCSwitchClaudeAccountsRoot 返回 cc-switch Claude 多账号功能的账号根目录
// （<home>/.cc-switch/claude-accounts）。目录不存在时由各调用方容错：采集侧
// glob 零匹配静默，watch 侧落到最近存在的祖先、待创建后由 Create 事件接管。
func CCSwitchClaudeAccountsRoot(home string) string {
	return filepath.Join(home, ".cc-switch", "claude-accounts")
}

// discoverCCSwitchClaudeProjects 发现 cc-switch Claude 多账号功能的各账号会话目录
// （~/.cc-switch/claude-accounts/<id>/config/projects）。cc-switch 4.0.21 起「Open Terminal」
// 隔离终端把 CLAUDE_CONFIG_DIR 指向各账号目录，其中的会话日志写在账号自己的 projects
// 下而非 ~/.claude/projects。账号目录随账号增删生灭，零匹配是常态；仅接受目录形态的
// 匹配项，非目录匹配（如同名文件）跳过。home 是动态路径、可能含 [ ] 等 Glob 元字符，
// 故按字面 ReadDir 枚举账号层，其余各层字面拼接，不经模式解释。
func discoverCCSwitchClaudeProjects(home string) []string {
	if home == "" {
		return nil
	}
	accountsDir := CCSwitchClaudeAccountsRoot(home)
	entries, err := os.ReadDir(accountsDir)
	if err != nil {
		return nil // 账号根不存在（未用多账号）或不可读：零匹配常态
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(accountsDir, e.Name(), "config", "projects")
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// findClaudeJSONLFilesInDirs 收集多个自动发现根下的文件；单根 walk 失败仅聚合错误、
// 不中断其余根（账号目录生灭是常态，I/O 异常经 Join 归 PartialErr 由调用方观察）。
func findClaudeJSONLFilesInDirs(ctx context.Context, dirs []string) ([]string, error) {
	var files []string
	var errs error
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		// 与主根和 ChangedFile 入口一致：统一为绝对 Clean 形态，避免相对目录
		// 让受控判定根与收集路径的形态分叉。
		if abs, err := filepath.Abs(dir); err == nil {
			dir = filepath.Clean(abs)
		}
		part, err := walkClaudeJSONLDir(ctx, dir)
		files = append(files, part...)
		errs = errors.Join(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return files, errs
}

// walkClaudeJSONLDir 收集单个根目录下的 .jsonl 文件。主 transcript 保持全树发现
// （含根级文件与任意深度目录），仅 subagents 子树受控进入：只收本层
// agent-*.jsonl 与 workflows/<runId>/ 下的 agent-*.jsonl，不深入其他子目录，
// journal.jsonl 等其他文件忽略；收集结果按采集根经
// claudeAgentSubagentsDirInRoot 复核，与 IsClaudeAgentTranscriptInRoots 同口径。
//
// 根为符号链接时先解析到真实目录（filepath.Walk 对根用 Lstat 不跟随符号链接，
// 不解析会让全扫静默返回零文件）。子目录读取失败（如不可读的 workflow 目录）
// 记入返回错误并跳过该子树、继续扫描健康兄弟，不终止整棵根；遍历期间被清理的
// 目录（ENOENT）属正常竞态，静默跳过不记错误；仅根自身的遍历失败作为错误返回。
// ctx 取消立即中止遍历（守护进程关闭时尽快退出，避免长采集阻塞关闭路径）。
func walkClaudeJSONLDir(ctx context.Context, dir string) ([]string, error) {
	// 根本身是符号链接时解析到真实目录：filepath.Walk 对根用 Lstat 不跟随
	// 符号链接，不解析会让全扫静默返回零文件（预检 os.Stat 跟随会通过）。
	// 普通目录不解析，保持调用方传入的路径形态（门记录键与既有语义不变）。
	if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
			dir = resolved
		}
	}
	var files []string
	var errs error
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == dir {
				return err
			}
			// 子项不可访问：目录树在遍历期间被清理（ENOENT）属正常竞态，静默
			// 跳过；其他错误记为部分失败并跳过该子树，继续兄弟——一棵坏子树
			// 不得拖累同根主会话与其他 agent transcript 的采集。
			if !errors.Is(err, fs.ErrNotExist) {
				errs = errors.Join(errs, err)
			}
			return nil
		}

		if info.IsDir() {
			if info.Name() == "subagents" {
				agentFiles, cerr := collectClaudeAgentFiles(ctx, path, &errs)
				// 受控复核：只收采集根下相对布局合法的 agent transcript（与
				// IsClaudeAgentTranscriptInRoots 同口径，防遍历结构与谓词漂移）。
				for _, f := range agentFiles {
					if claudeAgentSubagentsDirInRoot(dir, f) != "" {
						files = append(files, f)
					}
				}
				if cerr != nil {
					return cerr // ctx 取消：中止整棵遍历
				}
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}

		return nil
	})

	return files, errors.Join(walkErr, errs)
}

// maxClaudeAgentMetaSize 是 agent meta.json 的读取上限：文件只含描述等短字段，
// 上限防御异常损坏文件造成的内存尖峰；超限按不可解析处理（标题留空）。
const maxClaudeAgentMetaSize = 1 << 20

// isClaudeDesktopEntrypoint 报告 entrypoint 是否属于 Claude Desktop 家族。
// 只认四份落地证据覆盖的显式值，不做前缀匹配（claude-desktop-worktree /
// claude-desktop-ssh 等变体是否落盘无证据）。四值仅是判定输入，输出统一归
// Claude Desktop，不产生子类型、不记录入口来源。
func isClaudeDesktopEntrypoint(entrypoint string) bool {
	switch entrypoint {
	case "claude-desktop", "claude-desktop-3p", "local-agent", "local_agent":
		return true
	default:
		return false
	}
}

// isClaudeAgentJSONLName 报告文件名是否为 agent-<id>.jsonl（subagents 下的
// agent transcript 命名）。
func isClaudeAgentJSONLName(name string) bool {
	return strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") &&
		len(name) > len("agent-")+len(".jsonl")
}

// claudeAgentSubagentsDirInRoot 判定 path 是否位于采集根 root 下的受控 agent
// 布局，返回其 subagents 目录：
//
//	<root>/<编码cwd>/<父sessionId>/subagents/agent-*.jsonl
//	<root>/<编码cwd>/<父sessionId>/subagents/workflows/<runId>/agent-*.jsonl
//
// 判定以采集根为边界（只检查相对根的路径段）：根自身或其祖先目录名与受控段
// 同名（如 projects_dir 位于名为 subagents 的目录下）不会误判；相对路径中多出
// 的层级（subagents/x/subagents/ 等嵌套形态）同样不匹配。返回空串表示不属于
// 受控 agent transcript。
func claudeAgentSubagentsDirInRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 4 && len(parts) != 6 {
		return ""
	}
	if parts[2] != "subagents" || !isClaudeAgentJSONLName(parts[len(parts)-1]) {
		return ""
	}
	if len(parts) == 6 && parts[3] != "workflows" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(strings.Join(parts[:3], "/")))
}

// claudeAgentSubagentsDirInRoots 在多个采集根（主根 + cc-switch 账号根）上
// 依次尝试，返回首个命中的受控 subagents 目录。
func claudeAgentSubagentsDirInRoots(roots []string, path string) string {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if sub := claudeAgentSubagentsDirInRoot(root, path); sub != "" {
			return sub
		}
	}
	return ""
}

// IsClaudeAgentTranscriptInRoots 报告路径是否为采集根下的受控 agent transcript。
// 全扫的受控入口、ChangedFile、watcher 事件判定与启动跳过门绕过共用本判定，
// 保证各入口对「哪些文件是 agent transcript」口径一致。
func IsClaudeAgentTranscriptInRoots(roots []string, path string) bool {
	return claudeAgentSubagentsDirInRoots(roots, path) != ""
}

// claudeRootWithResolved 追加根的绝对 Clean 形态；根本身是符号链接时同时追加
// 解析后的真实路径（全扫 walk 用解析后路径收集文件，watcher 事件用注册时的
// 绝对路径，两种形态都要能匹配采集根）。相对路径配置在此统一为绝对路径，
// 与 watcher 监控根的规范化保持一致。
func claudeRootWithResolved(roots []string, root string) []string {
	if root == "" {
		return roots
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	abs = filepath.Clean(abs)
	roots = append(roots, abs)
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil && resolved != abs {
			roots = append(roots, resolved)
		}
	}
	return roots
}

// ClaudeProjectsRoots 返回 Claude 采集的全部 projects 根（主根 + cc-switch
// 账号根，含符号链接根的解析形态），供全扫之外的入口（ChangedFile、watcher、
// 启动跳过门）复用同一根集合做受控判定。projects_dir 必须配置为 projects
// 目录本身（默认 ~/.claude/projects）；配成其祖先目录时 agent transcript 的
// 相对层级不再是受控布局，将不被采集。
func ClaudeProjectsRoots(cfg *config.Config) []string {
	clientCfg, ok := cfg.ClientConfig("claude")
	if !ok || clientCfg.Paths == nil {
		return nil
	}
	var roots []string
	roots = claudeRootWithResolved(roots, clientCfg.Paths["projects_dir"])
	home, _ := os.UserHomeDir()
	for _, dir := range discoverCCSwitchClaudeProjects(home) {
		roots = claudeRootWithResolved(roots, dir)
	}
	return roots
}

// ClaudePathInAgentSubtree 报告路径是否位于采集根的 subagents 子树内（相对根
// 的路径段含 subagents）。watcher 用它区分主 transcript 事件与 subagents 子树
// 事件：主 transcript 恒放行，subagents 子树才要求受控判定。
func ClaudePathInAgentSubtree(roots []string, path string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == "subagents" {
				return true
			}
		}
	}
	return false
}

// IsClaudeAgentDirInRoots 报告 dir 是否为任一采集根下受控 Claude 子代理布局的
// 目录容器：<proj>/subagents、<proj>/subagents/workflows、
// <proj>/subagents/workflows/<runId>。这些目录只放行 agent-*.jsonl，合成普通
// probe.jsonl 的采集资格判定会误判；watcher 目录事件用本谓词保留原生
// subagents 入口，避免被 Cowork 别名改写后丢失父会话与标题。
func IsClaudeAgentDirInRoots(roots []string, dir string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if claudeAgentDirInRoot(root, dir) {
			return true
		}
	}
	return false
}

// claudeAgentDirInRoot 判定目录相对单个采集根是否位于受控子代理容器层级。
// 受控布局：<proj>/subagents/agent-*.jsonl 与
// <proj>/subagents/workflows/<runId>/agent-*.jsonl。
func claudeAgentDirInRoot(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	parts := strings.Split(rel, "/")
	// 受控 agent transcript 的相对布局是 <a>/<b>/subagents[/workflows/<runId>]，
	// subagents 固定在第 3 段（索引 2）。
	if len(parts) < 3 || parts[2] != "subagents" {
		return false
	}
	switch len(parts) {
	case 3: // <a>/<b>/subagents
		return true
	case 4: // <a>/<b>/subagents/workflows
		return parts[3] == "workflows"
	case 5: // <a>/<b>/subagents/workflows/<runId>
		return parts[3] == "workflows"
	default:
		return false
	}
}

// claudeAgentParentInRoots 从 agent transcript 路径推导父会话 id：受控布局中
// subagents 目录的父目录名（行内 sessionId 缺失时的兜底）。非受控路径返回空串。
func claudeAgentParentInRoots(roots []string, path string) string {
	subagentsDir := claudeAgentSubagentsDirInRoots(roots, path)
	if subagentsDir == "" {
		return ""
	}
	return filepath.Base(filepath.Dir(subagentsDir))
}

// collectClaudeAgentFiles 受控枚举一个 subagents 目录：只收本层 agent-*.jsonl
// 与 workflows/<runId>/ 下的 agent-*.jsonl，不深入其他子目录。目录在枚举期间被
// 清理（ENOENT）属正常竞态、静默跳过；其他读取失败记入 errs 并跳过该子树继续。
// 返回的非 nil 错误仅表示 ctx 取消（调用方须立即中止整棵遍历）。
func collectClaudeAgentFiles(ctx context.Context, subagentsDir string, errs *error) ([]string, error) {
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		// os.ReadDir 的错误是含路径的 *PathError，直接聚合即可定位；ENOENT 是
		// 目录树被清理的正常竞态，不记部分失败。
		if !errors.Is(err, fs.ErrNotExist) {
			*errs = errors.Join(*errs, err)
		}
		return nil, nil
	}
	var files []string
	for _, e := range entries {
		if ctx.Err() != nil {
			return files, ctx.Err()
		}
		if !e.IsDir() {
			if isClaudeAgentJSONLName(e.Name()) {
				files = append(files, filepath.Join(subagentsDir, e.Name()))
			}
			continue
		}
		if e.Name() != "workflows" {
			continue
		}
		workflowsDir := filepath.Join(subagentsDir, "workflows")
		runs, err := os.ReadDir(workflowsDir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				*errs = errors.Join(*errs, err)
			}
			continue
		}
		for _, run := range runs {
			if ctx.Err() != nil {
				return files, ctx.Err()
			}
			if !run.IsDir() {
				continue
			}
			runDir := filepath.Join(workflowsDir, run.Name())
			runEntries, err := os.ReadDir(runDir)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					*errs = errors.Join(*errs, err)
				}
				continue
			}
			for _, f := range runEntries {
				if ctx.Err() != nil {
					return files, ctx.Err()
				}
				if !f.IsDir() && isClaudeAgentJSONLName(f.Name()) {
					files = append(files, filepath.Join(runDir, f.Name()))
				}
			}
		}
	}
	// 谓词复核由调用方（walk 回调）按采集根完成：遍历结构与
	// IsClaudeAgentTranscriptInRoots 必须同口径，防止两套实现随布局演进漂移。
	return files, nil
}

// readClaudeAgentDescription 读取 agent transcript 同目录 meta.json 的
// description 原文作为会话标题。meta 缺失、超限或不可解析时返回空串（标题
// 留空，非致命）；每次解析 agent 文件都重读，保证 meta 晚于 JSONL 落盘后经
// 重启补采或 watcher meta 事件补齐。
func readClaudeAgentDescription(jsonlPath string) string {
	metaPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".meta.json"
	f, err := os.Open(metaPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxClaudeAgentMetaSize))
	if err != nil {
		return ""
	}
	var meta struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	if strings.TrimSpace(meta.Description) == "" {
		return ""
	}
	return meta.Description
}

// dedupeCleanedFilePaths 按 Clean 归一后的路径去重（各组序拼接、首见保留，主根在前）。
// 不做符号链接归一：仅防同一文件的字面重复（如 projects_dir 恰好配成自动发现目录或其祖先）。
func dedupeCleanedFilePaths(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, p := range group {
			key := filepath.Clean(p)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, p)
		}
	}
	return out
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
// agent transcript（subagents 下的 agent-*.jsonl）按行序首个有效非空 sessionId
// 解析父会话（空或等于文件名 agent id 的行跳过；缺失时以路径兜底），并重读同目录
// meta.json 的 description 作标题。
// 返回的 FileScanStatus 已填 Path/BadLines/FirstBad*/TrailingNewline（Before/After
// 快照与 Err 由 Collect 层补充）。
func parseClaudeMessageFile(filePath string, agentRoots []string, dates map[string]struct{}, logger *slog.Logger) (CollectResult, FileScanStatus, error) {
	var status FileScanStatus
	status.Path = filePath
	if logger == nil {
		logger = slog.Default()
	}
	agent := IsClaudeAgentTranscriptInRoots(agentRoots, filePath)
	agentID := ""
	if agent {
		agentID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(filePath), "agent-"), ".jsonl")
	}
	rowParentID := ""
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
	// hasDesktopEntrypoint 累积整份文件是否出现过 Desktop entrypoint。累积点放在
	// content 归一之前（父归属判定之后），使字符串 content 的 user 行（被归入
	// chainOnly 后 continue）也参与判定——官方 issue #110287 实测同一 transcript
	// 可混有 cli 与 claude-desktop 行，任一 Desktop 行即整份归 Claude Desktop。
	hasDesktopEntrypoint := false
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
		// Desktop entrypoint 家族累积：白名单内四个显式值（不做前缀匹配）。
		if isClaudeDesktopEntrypoint(entry.Entrypoint) {
			hasDesktopEntrypoint = true
		}
		// agent transcript 父归属：行序首个有效非空 sessionId（空值或等于文件名
		// agent id 的行跳过继续找）。放在 content 归一之前，字符串 content 行也
		// 参与父归属判定（其 sessionId 为顶层字段，与 content 形态无关）。
		if agent && rowParentID == "" && entry.SessionID != "" && entry.SessionID != agentID {
			rowParentID = entry.SessionID
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

	// 文件级元信息：cwd、最后非空 title、first/last ts。
	// entrypoint 已改为在行解析循环内累积为 hasDesktopEntrypoint（见上）。
	cwd := ""
	customTitle := ""
	var firstTS, lastTS int64
	for _, entry := range entries {
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
	if hasDesktopEntrypoint {
		client = model.RawClientClaudeDesktop
	}

	fileSessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	project := inferProject(cwd)

	// agent transcript 的父会话与标题：行内 sessionId 优先、路径兜底（两者不一致
	// 说明是跨父目录复制副本，以行内为准并记 Warn）；标题取同目录 meta.json 的
	// description 原文，meta 缺失时留空（不覆盖库中已有非空标题）。
	agentParentID := ""
	agentTitle := ""
	if agent {
		pathParent := claudeAgentParentInRoots(agentRoots, filePath)
		agentParentID = rowParentID
		if agentParentID == "" {
			agentParentID = pathParent
		} else if pathParent != "" && pathParent != agentParentID {
			logger.Warn("Claude agent transcript parent mismatch, using row sessionId",
				"file", filePath, "row_session_id", agentParentID, "path_parent", pathParent)
		}
		agentTitle = readClaudeAgentDescription(filePath)
	}

	// 按首条非零 usage 记录去重 assistant 消息（同一 message.id 的 thinking/text/tool 片段合并）。
	// 请求时长估算：链缓存 + 逐回复计时聚合（与下方 token 去重循环并行）。
	// 字符串 content 行只贡献链节点。
	chain, timings := buildClaudeTiming(append(append([]jsonlEntry(nil), entries...), chainOnly...))

	seen := make(map[string]bool)
	var messages []model.Message
	// hasUsage 记录完整 transcript 是否产出过量（与请求日期无关）：同一会话
	// 多副本的来源裁决必须以整份文件为依据，否则日期采集会改选另一份副本，
	// 把整场会话的历史归属改写成过滤后仍有消息的那份。
	hasUsage := false
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
		hasUsage = true

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
			// agent transcript 的每条消息携带父会话 id（主 transcript 恒空）。
			AgentParentID: agentParentID,
			Client:        model.RawClientToClient[client],
			Date:          date,
			TS:            ts,
			Model:         entry.Message.Model,
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
	result.hasUsage = hasUsage
	// 会话骨架：仅当完整文件产出了用量时构造，供 Cowork 组裁决在「来源副本
	// 当日无消息、其他副本有消息待入库」时生成会话行。首末时间取完整文件
	// 范围，不受请求日期影响。
	if hasUsage {
		skeleton := model.Session{
			ID:            fileSessionID,
			Client:        model.RawClientToClient[client],
			Directory:     cwd,
			Project:       project,
			Title:         claudeSessionTitle(customTitle, agentTitle),
			AgentParentID: agentParentID,
			FirstTS:       firstTS,
			LastTS:        lastTS,
		}
		result.sessionSkeleton = &skeleton
		// 有命中请求日期的消息时才返回会话。其他 Cowork 副本可用骨架补齐。
		if len(messages) > 0 {
			result.Sessions = append(result.Sessions, skeleton)
		}
	}
	// 坏行汇总在两个解析循环之后打（行解析与消息产出都可能计入坏行）。
	if outcome.badLines > 0 {
		logger.Debug("Claude JSONL line parse failed, skipped",
			"file", filePath, "count", outcome.badLines, "first_line", outcome.firstBadLine, "error", outcome.firstBadErr)
	}
	outcome.fillStatus(&status)
	return result, status, nil
}

// claudeSessionTitle 返回会话标题：agent transcript 的 meta description 优先，
// 否则使用 transcript 行内自定义标题。普通 transcript 与 Cowork 会话骨架共用。
func claudeSessionTitle(customTitle, agentTitle string) string {
	if agentTitle != "" {
		return agentTitle
	}
	return customTitle
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
