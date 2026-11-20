package collector

// 本文件集中实现 Claude Desktop Cowork（local-agent-mode-sessions）补采相关逻辑：
// 数据目录发现、受控布局谓词、逐层枚举、会话目录级映射关联、project/directory
// 取值，以及门命中时的会话元数据 UPDATE-only 通道构造。
// 元数据只来自会话目录同层的映射 JSON：映射缺失走空值兜底，映射损坏或缺
// cliSessionId 时整目录跳过（延迟而非丢失），不覆盖库中已有归属。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// claudeCoworkDirName 是 Cowork transcript 根目录名（Claude Desktop 数据目录下）。
const claudeCoworkDirName = "local-agent-mode-sessions"

// ClaudeDesktopDataDirs 返回 Claude Desktop 的数据目录（1P 与 3P 各一个）。
// 目录是否存在由调用方静默跳过（1P 用户可能没有 3P 目录，反之亦然）。
// getenv 为环境变量读取的注入点（Windows APPDATA 重定向 / 测试注入）。
// goos 为平台标识（runtime.GOOS 或测试注入）；非 darwin/windows 返回 nil
// （项目仅支持 macOS 与 Windows，Linux 不提供默认值）。
func ClaudeDesktopDataDirs(home, goos string, getenv func(string) string) []string {
	if home == "" {
		return nil
	}
	get := getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	switch goos {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Claude"),
			filepath.Join(base, "Claude-3p"),
		}
	case "windows":
		// 1P 路径取自 issue #84552 报告的 Windows 11 落盘位置，本地无 Windows
		// 样本可复核；-3p 后缀沿用 3P 的命名习惯，同样未在本机验证。APPDATA
		// 被重定向时按注入 env 取值，为空时回退 home/AppData/Roaming（按 home 推导）。
		base := get("APPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
		return []string{
			filepath.Join(base, "Claude"),
			filepath.Join(base, "Claude-3p"),
		}
	default:
		return nil
	}
}

// ClaudeCoworkRoots 返回各 Claude Desktop 数据目录下的 Cowork transcript 根
// （dataDir/local-agent-mode-sessions）。目录本身不预检，交由枚举/注册侧容错。
func ClaudeCoworkRoots(home, goos string, getenv func(string) string) []string {
	var roots []string
	for _, dir := range ClaudeDesktopDataDirs(home, goos, getenv) {
		roots = append(roots, filepath.Join(dir, claudeCoworkDirName))
	}
	return roots
}

// claudeCoworkRootsFromEnv 用当前运行环境构造 Cowork 根集合（生产装配入口）。
func claudeCoworkRootsFromEnv() []string {
	home, _ := os.UserHomeDir()
	return ClaudeCoworkRoots(home, runtime.GOOS, os.Getenv)
}

// claudeCoworkTranscriptRel 判定 path 相对某个 Cowork 采集根是否为受控布局的
// transcript，并返回其相对根的标准斜杠分段。受控布局：
//
//	root/account/profile/会话目录/.claude/projects/.../id.jsonl
//
// .claude 之前必须恰好三段（account/profile/会话目录），文件为 .jsonl 且不
// 位于 .claude/projects 下的 subagents 子树。两段布局（account/profile 直下
// 挂 projects）不放行——元数据定位会落到错误层级，宁可不采，避免以空值覆盖
// 已有归属。不以字符串包含 local-agent-mode-sessions 判定：cwd
// 编码目录名本身可能含该字样，必须以采集根为边界（filepath.Rel）。
func claudeCoworkTranscriptRel(root, path string) (parts []string, ok bool) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	absRoot = filepath.Clean(absRoot)
	absPath := path
	if ap, err := filepath.Abs(path); err == nil {
		absPath = filepath.Clean(ap)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return nil, false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, false
	}
	parts = strings.Split(rel, "/")
	if len(parts) < 5 {
		return nil, false
	}
	// .claude 之前恰好三段：account(0)/profile(1)/会话目录(2)。
	if parts[3] != ".claude" || parts[4] != "projects" {
		return nil, false
	}
	if !strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		return nil, false
	}
	// .claude/projects 下的 subagents 子树不采（一期不采 Cowork 子代理）。
	for _, seg := range parts[5:] {
		if seg == "subagents" {
			return nil, false
		}
	}
	return parts, true
}

// IsClaudeCoworkDirInRoots 报告 dir 是否为任一 Cowork 采集根下受控布局的合法
// 祖先（可能成为 transcript 的目录）。受控布局：
//
//	root/account/profile/会话目录/.claude/projects/.../id.jsonl
//
// 合法祖先包括 account/profile/会话目录（根内前三段）、.claude 本身、
// .claude/projects 及其后代；subagents 子树除外。watcher 在新建目录事件里用它
// 选择合法入口：刚创建的 .claude 下面还没有 projects/，只靠合成 probe.jsonl
// 会把它误判为不可采集，从而回退到 uploads/ 等非采集入口。
func IsClaudeCoworkDirInRoots(roots []string, dir string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if claudeCoworkDirAncestorRel(root, dir) {
			return true
		}
	}
	return false
}

// claudeCoworkDirAncestorRel 判定目录相对采集根是否落在受控布局的合法前缀上。
// 目录可能是采集根本身（相对路径 "."）、.claude 之前的任意一层、.claude 本身
// 或 .claude/projects 之下；根内第四段不是 .claude 的目录（如 uploads/）不是
// 合法祖先。
func claudeCoworkDirAncestorRel(root, dir string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	absRoot = filepath.Clean(absRoot)
	absDir := dir
	if ap, err := filepath.Abs(dir); err == nil {
		absDir = filepath.Clean(ap)
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	parts := strings.Split(rel, "/")
	// account/profile/会话目录及更浅的祖先都可以通向受控 transcript。
	if len(parts) <= 3 {
		return true
	}
	if parts[3] != ".claude" {
		return false
	}
	if len(parts) == 4 {
		// .claude 本身：projects/ 可能尚未创建。
		return true
	}
	if parts[4] != "projects" {
		return false
	}
	for _, seg := range parts[5:] {
		if seg == "subagents" {
			return false
		}
	}
	return true
}

// IsClaudeCoworkTranscriptInRoots 报告 path 是否为任一 Cowork 采集根下受控
// 布局的 transcript。全扫、ChangedFile、watcher 事件三处共用本谓词。
func IsClaudeCoworkTranscriptInRoots(roots []string, path string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if _, ok := claudeCoworkTranscriptRel(root, path); ok {
			return true
		}
	}
	return false
}

// ClaudeCoworkDirHoldsTranscript 报告 dir 是否可能成为某个 Cowork transcript
// 的上级目录：dir 位于任一 Cowork 根内，或 dir 解析符号链接后的真实路径位于
// 任一 Cowork 根内。watcher 用它把「跟随目录符号链接」限制在 Cowork 覆盖范围
// 内——会话目录的 .claude 指向采集面之外是 Cowork 的落盘形态，而普通客户端的
// 目录链接不应扩大监听范围。
func ClaudeCoworkDirHoldsTranscript(roots []string, dir string) bool {
	if len(roots) == 0 || dir == "" {
		return false
	}
	forms := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		forms = append(forms, resolved)
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			absRoot = root
		}
		absRoot = filepath.Clean(absRoot)
		for _, form := range forms {
			abs, aerr := filepath.Abs(form)
			if aerr != nil {
				abs = form
			}
			rel, rerr := filepath.Rel(absRoot, filepath.Clean(abs))
			if rerr != nil {
				continue
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return true
			}
		}
	}
	return false
}

// claudeCoworkProjectsPath 返回落在任一 Claude projects 根内的真实路径，
// 其余路径返回空值。projectsRoots 使用预解析的绝对形态。会话目录的 .claude 为
// 符号链接指向 ~/.claude 时，Cowork 枚举经 os.Stat 跟随链接会遍历到 projects
// 根下的 transcript，该文件同时被主根全扫采集。若此时仍走 Cowork 分支强制置
// Claude Desktop 并改写 SessionID，同一 message.id 就会以 Claude Code 与
// Claude Desktop 各入库一条（(client,id) 主键不冲突，跨 client 双计）。本函数
// 识别这种副本，使调用方跳过 Cowork 覆盖，让两路都按 entrypoint 判定、由
// (client,id) 主键兜底。
func claudeCoworkProjectsPath(path string, projectsRoots []string) string {
	if len(projectsRoots) == 0 || path == "" {
		return ""
	}
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	if abs, err := filepath.Abs(real); err == nil {
		real = abs
	}
	real = filepath.Clean(real)
	sep := string(filepath.Separator)
	for _, root := range projectsRoots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, real)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+sep)) {
			return real
		}
	}
	return ""
}

// claudeCoworkProfileFromPath 取相对 Cowork 根的第 2 段（profile 目录绝对路径）。
// 受控布局 .claude 之前有三段（account/profile/会话目录），profile 为第 2 段。
func claudeCoworkProfileFromPath(root, path string) string {
	parts, ok := claudeCoworkTranscriptRel(root, path)
	if !ok || len(parts) < 3 {
		return ""
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	return filepath.Join(absRoot, filepath.FromSlash(parts[0]), filepath.FromSlash(parts[1]))
}

// claudeCoworkSessionDirFromPath 返回 transcript 所在的会话目录绝对路径
// （root/account/profile/会话目录）。非受控路径返回空串。
func claudeCoworkSessionDirFromPath(root, path string) string {
	parts, ok := claudeCoworkTranscriptRel(root, path)
	if !ok || len(parts) < 3 {
		return ""
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	return filepath.Join(absRoot, filepath.FromSlash(parts[0]),
		filepath.FromSlash(parts[1]), filepath.FromSlash(parts[2]))
}

// claudeCoworkMappingForSession 在会话目录同层 profile 下按文件名前缀匹配
// 关联映射文件（关联粒度是会话目录，不是单个 transcript）：
//   - 会话目录以 local_ 开头：profile/目录名.json（目录名本身已含 local_，不再加前缀）；
//   - 其他（short 形态）：以 local_目录名 为前缀的普通 .json 文件。
//
// 用 os.ReadDir + 字面前缀比较，不用 filepath.Glob（home 可能含 Glob 元字符，
// 沿用 discoverCCSwitchClaudeProjects 先例）；只取 .json 结尾且非目录的项
// （现场 local_72ae64f7* 同时存在目录与同名文件，须跳过目录）。多个匹配按
// 文件名字典序先见保留并记 Debug。
//
// 空路径且无错误表示映射不存在。读取目录失败和映射解析失败都由调用方跳过会话目录。
func claudeCoworkMappingForSession(profile, sessionDir string, logger *slog.Logger) (string, error) {
	entries, err := os.ReadDir(profile)
	if err != nil {
		return "", err
	}
	return findClaudeCoworkMapping(profile, sessionDir, entries, logger), nil
}

// entries 沿用 os.ReadDir 的文件名排序，多个匹配时取字典序第一项。
func findClaudeCoworkMapping(profile, sessionDir string, entries []os.DirEntry, logger *slog.Logger) string {
	name := filepath.Base(sessionDir)
	prefix := "local_" + name
	candidates := make([]string, 0, 1)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(name, "local_") {
			if e.Name() == name+".json" {
				return filepath.Join(profile, e.Name())
			}
			continue
		}
		if e.Name() == prefix+".json" || strings.HasPrefix(e.Name(), prefix+"-") {
			candidates = append(candidates, e.Name())
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) > 1 && logger != nil {
		logger.Debug("Claude Cowork session mapping prefix collision, using first",
			"session_dir", sessionDir, "candidates", candidates)
	}
	return filepath.Join(profile, candidates[0])
}

// claudeCoworkSessionMeta 是从映射文件解析出的会话元数据（会话 id、标题、
// 用户选择的目录及其挂载名）。
type claudeCoworkSessionMeta struct {
	CLISessionID       string
	Title              string
	UserSelectedFolder string
	FolderMountNames   map[string]string
}

// loadClaudeCoworkSessionMeta 读取并解析映射文件。JSON 损坏、缺/空 cliSessionId
// 均视为解析失败（ok=false, corrupt=true），调用方据此整目录跳过。
// cliSessionId 非空但其余字段缺省时按空值兜底。
func loadClaudeCoworkSessionMeta(mappingPath string) (meta claudeCoworkSessionMeta, ok bool, corrupt bool) {
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		return meta, false, true
	}
	var raw struct {
		CLISessionID        string            `json:"cliSessionId"`
		Title               string            `json:"title"`
		UserSelectedFolders []string          `json:"userSelectedFolders"`
		FolderMountNames    map[string]string `json:"folderMountNames"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return meta, false, true
	}
	if strings.TrimSpace(raw.CLISessionID) == "" {
		return meta, false, true
	}
	meta.CLISessionID = raw.CLISessionID
	meta.Title = raw.Title
	meta.FolderMountNames = raw.FolderMountNames
	if len(raw.UserSelectedFolders) > 0 {
		meta.UserSelectedFolder = raw.UserSelectedFolders[0]
	}
	return meta, true, false
}

// claudeCoworkDirectoryProject 计算 Cowork 的 directory/project：
//   - directory = userSelectedFolders[0]，无选择时为空；
//   - project = folderMountNames[首选目录]；键不存在时按挂载路径字典序第一；
//   - 无挂载留空。
//
// Go map 遍历无序，必须显式排序取字典序第一。禁止对 Cowork 走 inferProject(cwd)。
func claudeCoworkDirectoryProject(meta claudeCoworkSessionMeta) (directory, project string) {
	directory = meta.UserSelectedFolder
	if directory == "" {
		return "", ""
	}
	if p, ok := meta.FolderMountNames[directory]; ok {
		return directory, p
	}
	keys := make([]string, 0, len(meta.FolderMountNames))
	for k := range meta.FolderMountNames {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return directory, ""
	}
	sort.Strings(keys)
	return directory, meta.FolderMountNames[keys[0]]
}

// claudeCoworkMappingCorruptErr 构造「会话目录映射损坏」的失败原因：JSON 无法
// 解析或缺/空 cliSessionId 时整目录跳过。原因进 Collected 的部分失败通道，
// 使该轮不被记为完整成功、映射修复后仍会重试，而不是被静默当作采集完成。
func claudeCoworkMappingCorruptErr(sessionDir, mappingPath string) error {
	return fmt.Errorf("Claude Cowork session mapping corrupt or missing cliSessionId, session dir skipped:"+
		" session_dir=%s mapping=%s", sessionDir, mappingPath)
}

// claudeCoworkTranscriptOutcome 是 Cowork 单个 transcript 在枚举/关联阶段的判定。
type claudeCoworkTranscriptOutcome struct {
	Path string
	// Skip 表示会话目录的映射损坏或缺 cliSessionId，整目录跳过（不产出
	// messages/Sessions，也不覆盖库中已有的 directory/project）。
	Skip bool
	// SkipErr 是跳过原因（含会话目录与映射路径）。调用方必须把它计入部分失败：
	// 映射修复后仍要能重试该目录，不能被当作采集成败已完整的证据。
	SkipErr error
	Meta    claudeCoworkSessionMeta
}

// claudeCoworkEnumerate 逐层 os.ReadDir 枚举 Cowork 根下受控布局的 transcript。
// 不用 filepath.Walk：DirEntry.IsDir() 基于 Lstat 不跟随符号链接，必须对每项
// 显式 os.Stat 判断目录（跟随），否则 .claude 为符号链接时漏采。
// 返回 (transcript 列表, 错误)。
//
// 关联以会话目录为单位：对每个含 .claude/projects 的会话目录定位映射文件，
// 解析失败则整目录跳过（连带冻结无映射 transcript，数据延迟非丢失）；无候选
// 则空值兜底。失败原因写在每条 outcome 的 SkipErr 上，由调用方按会话目录
// 去重后统一打印一次并计入部分失败，避免同一目录重复记录。
func claudeCoworkEnumerate(ctx context.Context, roots []string, logger *slog.Logger) ([]claudeCoworkTranscriptOutcome, error) {
	var outcomes []claudeCoworkTranscriptOutcome
	var errs error
	type dirResult struct {
		meta    claudeCoworkSessionMeta
		hasMap  bool
		corrupt bool
		skipErr error
	}
	cache := make(map[string]dirResult)
	type profileListing struct {
		entries []os.DirEntry
		err     error
	}
	profiles := make(map[string]profileListing)

	ancestors := make(map[string]struct{})
	var walk func(absRoot, dir, realDir string) error
	walk = func(absRoot, dir, realDir string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, seen := ancestors[realDir]; seen {
			return nil
		}
		ancestors[realDir] = struct{}{}
		defer delete(ancestors, realDir)

		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = errors.Join(errs, err)
			}
			return nil
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p := filepath.Join(dir, e.Name())
			if !claudeCoworkDirAncestorRel(absRoot, p) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = errors.Join(errs, err)
				}
				continue
			}
			if info.IsDir() {
				realChild := filepath.Join(realDir, e.Name())
				if e.Type()&os.ModeSymlink != 0 {
					resolved, err := filepath.EvalSymlinks(p)
					if err != nil {
						if !errors.Is(err, fs.ErrNotExist) {
							errs = errors.Join(errs, err)
						}
						continue
					}
					realChild = resolved
				}
				if err := walk(absRoot, p, realChild); err != nil {
					return err
				}
				continue
			}
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if _, ok := claudeCoworkTranscriptRel(absRoot, p); !ok {
				continue
			}
			sessionDir := claudeCoworkSessionDirFromPath(absRoot, p)
			res, cached := cache[sessionDir]
			if !cached {
				profile := claudeCoworkProfileFromPath(absRoot, p)
				listing, cached := profiles[profile]
				if !cached {
					listing.entries, listing.err = os.ReadDir(profile)
					profiles[profile] = listing
				}
				if listing.err != nil {
					res.corrupt = true
					res.skipErr = fmt.Errorf("read Claude Cowork mappings for session dir %s: %w", sessionDir, listing.err)
				} else if mappingPath := findClaudeCoworkMapping(profile, sessionDir, listing.entries, logger); mappingPath != "" {
					meta, ok, corrupt := loadClaudeCoworkSessionMeta(mappingPath)
					if !ok || corrupt {
						res.corrupt = true
						res.skipErr = claudeCoworkMappingCorruptErr(sessionDir, mappingPath)
					} else {
						res.hasMap = true
						res.meta = meta
					}
				}
				cache[sessionDir] = res
			}
			if res.corrupt {
				outcomes = append(outcomes, claudeCoworkTranscriptOutcome{Path: p, Skip: true, SkipErr: res.skipErr})
				continue
			}
			out := claudeCoworkTranscriptOutcome{Path: p}
			if res.hasMap && strings.TrimSuffix(e.Name(), ".jsonl") == res.meta.CLISessionID {
				out.Meta = res.meta
			}
			outcomes = append(outcomes, out)
		}
		return nil
	}

	for _, root := range roots {
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			absRoot = root
		}
		absRoot = filepath.Clean(absRoot)
		info, err := os.Stat(absRoot)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = errors.Join(errs, err)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = errors.Join(errs, err)
			}
			continue
		}
		if err := walk(absRoot, absRoot, realRoot); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
		}
	}
	return outcomes, errors.Join(errs)
}

// claudeCoworkSessionID 返回 transcript 的逻辑会话 ID（文件名去掉 .jsonl）。
// 受控布局下映射的 cliSessionId 只在与会话文件名一致时才关联，因此会话 ID
// 恒为文件名；映射缺失、按文件名兜底的副本也落到同一个键，便于按会话核对
// 整组现存副本。
func claudeCoworkSessionID(out claudeCoworkTranscriptOutcome) string {
	return strings.TrimSuffix(filepath.Base(out.Path), ".jsonl")
}

// groupClaudeCoworkOutcomes 按逻辑会话 ID 聚合现存 transcript，组内按绝对路径
// 字典序排序。这个顺序是「同一会话多份副本」的确定核对顺序：全扫、启动补采
// 与 ChangedFile 必须得到同一个组、同一份元数据来源，结果不能随枚举顺序或
// 跳过门状态变化。映射损坏的副本也保留在组内（Skip 标记），让组裁决看到同一
// 会话 ID 的完整状态，而不是把它剔掉后由健康副本接管归因。
func groupClaudeCoworkOutcomes(outcomes []claudeCoworkTranscriptOutcome) map[string][]claudeCoworkTranscriptOutcome {
	groups := make(map[string][]claudeCoworkTranscriptOutcome, len(outcomes))
	for _, out := range outcomes {
		id := claudeCoworkSessionID(out)
		groups[id] = append(groups[id], out)
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			return filepath.Clean(group[i].Path) < filepath.Clean(group[j].Path)
		})
	}
	return groups
}

// claudeCoworkGroupSkips 返回组内因映射损坏或缺 cliSessionId 而整目录跳过的
// 副本。同一逻辑会话 ID 只要有一份副本的映射状态不可用，就不能确定会话元数据
// 来源：健康但无映射的副本不得接管归因，否则会把已有 sessions/messages 的
// directory/project 写空。全扫与 ChangedFile 共用这条判定，组内命中时整组冻结
// （不产出 messages/Sessions、不提交元数据更新），并按部分失败报告。
func claudeCoworkGroupSkips(group []claudeCoworkTranscriptOutcome) []claudeCoworkTranscriptOutcome {
	var skips []claudeCoworkTranscriptOutcome
	for _, out := range group {
		if out.Skip {
			skips = append(skips, out)
		}
	}
	return skips
}

// claudeCoworkGroupForChanged 返回 ChangedFile 所在逻辑会话 ID 的全部现存
// transcript（含自身），让单文件入口与全扫共用同一条组裁决规则。映射损坏的
// 副本同样保留在组内（Skip 标记），由调用方整组冻结，避免健康副本改写归因。
// 枚举失败时返回已枚举到的组与错误：同 ID 其他副本的映射状态可能未知，调用方
// 不能静默把它当作只采这一份，需把错误计入部分失败。主根内的副本不参与（由
// 主根入口按 entrypoint 判定）。
func claudeCoworkGroupForChanged(ctx context.Context, roots []string, path string, own claudeCoworkTranscriptOutcome, projectsRoots []string, logger *slog.Logger) ([]claudeCoworkTranscriptOutcome, error) {
	id := claudeCoworkSessionID(own)
	group := []claudeCoworkTranscriptOutcome{own}
	seen := map[string]struct{}{filepath.Clean(path): {}}
	outcomes, err := claudeCoworkEnumerate(ctx, roots, logger)
	for _, out := range outcomes {
		clean := filepath.Clean(out.Path)
		if _, dup := seen[clean]; dup {
			continue
		}
		if claudeCoworkSessionID(out) != id {
			continue
		}
		if claudeCoworkProjectsPath(out.Path, projectsRoots) != "" {
			continue
		}
		seen[clean] = struct{}{}
		group = append(group, out)
	}
	sort.Slice(group, func(i, j int) bool {
		return filepath.Clean(group[i].Path) < filepath.Clean(group[j].Path)
	})
	return group, err
}

// applyCoworkGroupMeta 把组裁决结果应用到一份 transcript 的解析结果。元数据
// （title/directory/project 与会话 ID）只由 source 决定：非来源副本不产出
// Sessions，避免后读到的一份改写会话归属；但所有副本的 messages 都按同一口径
// 归 Claude Desktop 并使用 source 的 directory/project，保证会话列表与按项目
// 汇总的用量一致。
func applyCoworkGroupMeta(part *CollectResult, source claudeCoworkTranscriptOutcome, isSource bool) {
	directory, project := claudeCoworkDirectoryProject(source.Meta)
	if !isSource {
		part.Sessions = nil
	}
	id := strings.TrimSpace(source.Meta.CLISessionID)
	for i := range part.Sessions {
		s := &part.Sessions[i]
		s.Client = model.ClientClaudeDesktop
		s.Directory = directory
		s.Project = project
		if id != "" {
			s.ID = id
			s.Title = source.Meta.Title
		}
	}
	for i := range part.Messages {
		m := &part.Messages[i]
		m.Client = model.ClientClaudeDesktop
		m.Directory = directory
		m.Project = project
		if id != "" {
			m.SessionID = id
		}
	}
}

// claudeCoworkOutcomeForPath 为 ChangedFile 单文件入口构造 Cowork 判定：
// path 不在任何 Cowork 根内返回 (zero, false, false)；在 Cowork 根内但不满足受控谓词
// 返回 (zero, true, false)（需跳过）；满足谓词时按会话目录关联映射返回结果。
func claudeCoworkOutcomeForPath(roots []string, path string, logger *slog.Logger) (out claudeCoworkTranscriptOutcome, inCoworkRoot bool, controlled bool) {
	var root string
	for _, r := range roots {
		if r == "" {
			continue
		}
		absR, err := filepath.Abs(r)
		if err != nil {
			absR = r
		}
		absP := path
		if ap, err := filepath.Abs(path); err == nil {
			absP = ap
		}
		rel, err := filepath.Rel(filepath.Clean(absR), filepath.Clean(absP))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			root = r
			break
		}
	}
	if root == "" {
		return out, false, false
	}
	if _, ok := claudeCoworkTranscriptRel(root, path); !ok {
		return out, true, false
	}
	sessionDir := claudeCoworkSessionDirFromPath(root, path)
	profile := claudeCoworkProfileFromPath(root, path)
	mappingPath, err := claudeCoworkMappingForSession(profile, sessionDir, logger)
	if err != nil {
		skipErr := fmt.Errorf("read Claude Cowork mappings for session dir %s: %w", sessionDir, err)
		return claudeCoworkTranscriptOutcome{Path: path, Skip: true, SkipErr: skipErr}, true, true
	}
	if mappingPath == "" {
		if logger != nil {
			logger.Debug("Claude Cowork session mapping not found, using fallback", "session_dir", sessionDir)
		}
		return claudeCoworkTranscriptOutcome{Path: path}, true, true
	}
	meta, ok, corrupt := loadClaudeCoworkSessionMeta(mappingPath)
	if !ok || corrupt {
		skipErr := claudeCoworkMappingCorruptErr(sessionDir, mappingPath)
		// 日志只在调用方（Collect）打印一次，这里不重复记录。
		return claudeCoworkTranscriptOutcome{Path: path, Skip: true, SkipErr: skipErr}, true, true
	}
	if strings.TrimSuffix(filepath.Base(path), ".jsonl") != meta.CLISessionID {
		return claudeCoworkTranscriptOutcome{Path: path}, true, true
	}
	return claudeCoworkTranscriptOutcome{Path: path, Meta: meta}, true, true
}
