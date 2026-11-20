package collector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

func silentTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// .claude 符号链接指向 projects 根时，Cowork 枚举跟随链接遍历到的文件真实
// 路径落在 projects 根内，必须被识别为副本，交由 (client,id) 主键兜底。
func TestClaudeCoworkPathInProjectsRoots(t *testing.T) {
	base := t.TempDir()
	projectsDir := filepath.Join(base, "projects")
	coworkProj := filepath.Join(base, "cowork", "acct", "profile", "sess")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectsDir, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(coworkProj, 0o755); err != nil {
		t.Fatal(err)
	}
	// 真实 transcript 在 projects 根内。
	realFile := filepath.Join(projectsDir, "proj", "code-msg.jsonl")
	if err := os.WriteFile(realFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Cowork 会话目录通过 projects 符号链接指向同一文件。
	link := filepath.Join(coworkProj, "code-msg-link.jsonl")
	if err := os.Symlink(realFile, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	realProjects, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		t.Fatal(err)
	}
	if claudeCoworkProjectsPath(link, []string{realProjects}) == "" {
		t.Fatal("symlink into projects root should be detected as in-projects-root")
	}
	// 真实 Cowork transcript（不在 projects 根内）不应被误判。
	local := filepath.Join(coworkProj, "local-id.jsonl")
	if err := os.WriteFile(local, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claudeCoworkProjectsPath(local, []string{realProjects}) != "" {
		t.Fatal("local cowork transcript must not be treated as in-projects-root")
	}
	// 无 projects 根时恒 false（默认行为不变）。
	if claudeCoworkProjectsPath(link, nil) != "" {
		t.Fatal("no projects roots: expect false")
	}
}

// Collect 级：Cowork 会话目录 .claude 符号链接指向 projects 根时，跟随链接
// 遍历到的 transcript 是 projects 副本，不得强制归 Desktop。
func TestClaudeCoworkSymlinkIntoProjectsNotForcedDesktop(t *testing.T) {
	home := setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	// projects 根：放一条 cli entrypoint 的消息。
	projectsDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s1","timestamp":"2026-11-16T10:00:00+08:00","entrypoint":"cli","cwd":"/tmp/p","message":{"id":"code-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectsDir, "code-msg.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	// Cowork 会话目录的 .claude 符号链接指向 projects 的父 .claude。
	sessionDir := filepath.Join(roots[0], "acct", "profile", "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(sessionDir, ".claude")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	c := newClaudeCollectorCfg(t, projectsDir)
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	var sawCode, sawDesktop bool
	for _, m := range res.Messages {
		if m.ID != "code-msg" {
			continue
		}
		switch m.Client {
		case model.ClientClaudeDesktop:
			sawDesktop = true
		case model.ClientClaudeCode:
			sawCode = true
		}
	}
	if !sawCode {
		t.Fatalf("code-msg not collected as Claude Code: %+v", res.Messages)
	}
	if sawDesktop {
		t.Fatal("symlinked projects copy must not be forced to Claude Desktop")
	}
}

func TestIsClaudeDesktopEntrypoint(t *testing.T) {
	for _, ep := range []string{"claude-desktop", "claude-desktop-3p", "local-agent", "local_agent"} {
		if !isClaudeDesktopEntrypoint(ep) {
			t.Errorf("%q should be Desktop entrypoint", ep)
		}
	}
	for _, ep := range []string{"", "cli", "claude-vscode", "claude-desktop-worktree", "remote_cowork", "claude-coworker"} {
		if isClaudeDesktopEntrypoint(ep) {
			t.Errorf("%q should NOT be Desktop entrypoint", ep)
		}
	}
}

func TestClaudeDesktopDataDirsAndCoworkRoots(t *testing.T) {
	home := "/home/user"
	got := ClaudeDesktopDataDirs(home, "darwin", nil)
	want := []string{
		filepath.Join(home, "Library", "Application Support", "Claude"),
		filepath.Join(home, "Library", "Application Support", "Claude-3p"),
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("darwin dirs = %v, want %v", got, want)
	}
	// Windows 通过注入 env 取 APPDATA；为空回退 home/AppData/Roaming。
	w := ClaudeDesktopDataDirs(home, "windows", func(k string) string {
		if k == "APPDATA" {
			return "D:\\Custom AppData"
		}
		return ""
	})
	if len(w) != 2 || w[0] != filepath.Join("D:\\Custom AppData", "Claude") {
		t.Fatalf("windows APPDATA dirs = %v", w)
	}
	w2 := ClaudeDesktopDataDirs(home, "windows", func(string) string { return "" })
	if len(w2) != 2 || w2[0] != filepath.Join(home, "AppData", "Roaming", "Claude") {
		t.Fatalf("windows fallback dirs = %v", w2)
	}
	if got := ClaudeDesktopDataDirs(home, "linux", nil); len(got) != 0 {
		t.Fatalf("linux dirs = %v, want none", got)
	}
	roots := ClaudeCoworkRoots(home, "darwin", nil)
	if len(roots) != 2 || roots[0] != filepath.Join(want[0], claudeCoworkDirName) {
		t.Fatalf("cowork roots = %v", roots)
	}
}

func TestIsClaudeCoworkTranscriptInRoots(t *testing.T) {
	root := filepath.Join("/data", claudeCoworkDirName)
	three := filepath.Join(root, "acct", "profile", "007393ce", ".claude", "projects", "session", "abc.jsonl")
	if !IsClaudeCoworkTranscriptInRoots([]string{root}, three) {
		t.Fatal("three-segment layout should be allowed")
	}
	cwdEncoded := filepath.Join(root, "acct", "profile", "007393ce", ".claude", "projects", "-Users-demo-proj", "abc.jsonl")
	if !IsClaudeCoworkTranscriptInRoots([]string{root}, cwdEncoded) {
		t.Fatal("cwd-encoded layout should be allowed")
	}
	// 两段布局（account/profile 直下挂 projects）不放行。
	two := filepath.Join(root, "acct", "profile", ".claude", "projects", "session", "abc.jsonl")
	if IsClaudeCoworkTranscriptInRoots([]string{root}, two) {
		t.Fatal("two-segment layout should NOT be allowed")
	}
	// subagents 子树不放行。
	sub := filepath.Join(root, "acct", "profile", "007393ce", ".claude", "projects", "session", "subagents", "agent-x.jsonl")
	if IsClaudeCoworkTranscriptInRoots([]string{root}, sub) {
		t.Fatal("subagents subtree should NOT be allowed")
	}
	// 非 .jsonl 与根外路径不放行。
	audit := filepath.Join(root, "acct", "profile", "007393ce", ".claude", "projects", "session", "audit.jsonl")
	if !IsClaudeCoworkTranscriptInRoots([]string{root}, audit) {
		t.Fatal("audit.jsonl matches controlled layout (filtering happens at event/ChangedFile layer via non-transcript skip)")
	}
	outside := filepath.Join("/other", claudeCoworkDirName, "acct", "profile", "d", ".claude", "projects", "s", "abc.jsonl")
	if IsClaudeCoworkTranscriptInRoots([]string{root}, outside) {
		t.Fatal("path outside root should NOT be allowed")
	}
}

func TestClaudeCoworkDirectoryProject(t *testing.T) {
	meta := claudeCoworkSessionMeta{
		UserSelectedFolder: "/Users/demo/proj",
		FolderMountNames:   map[string]string{"/Users/demo/proj": "token-usage"},
	}
	d, p := claudeCoworkDirectoryProject(meta)
	if d != "/Users/demo/proj" || p != "token-usage" {
		t.Fatalf("primary mount: dir=%q project=%q", d, p)
	}
	// 首选目录无对应挂载 → 字典序第一。
	meta.FolderMountNames = map[string]string{"/z": "z-proj", "/a": "a-proj"}
	_, p = claudeCoworkDirectoryProject(meta)
	if p != "a-proj" {
		t.Fatalf("lexicographic first = %q, want a-proj", p)
	}
	// 无挂载 → 空 project，但 directory 保留。
	meta.FolderMountNames = nil
	d, p = claudeCoworkDirectoryProject(meta)
	if d != "/Users/demo/proj" || p != "" {
		t.Fatalf("no mount: dir=%q project=%q", d, p)
	}
	// 无选择目录 → 两者为空。
	_, p = claudeCoworkDirectoryProject(claudeCoworkSessionMeta{})
	if p != "" {
		t.Fatalf("no folder: project=%q", p)
	}
}

// 写一段映射到 profile 目录，返回映射路径。
func writeCoworkMapping(t *testing.T, profile, name, content string) string {
	t.Helper()
	p := filepath.Join(profile, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeCoworkMappingForSession(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "acct", "profile")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	// short 形态：local_<short>-<uuid>.json，且同名目录必须被跳过。
	shortDir := filepath.Join(profile, "007393ce")
	if err := os.MkdirAll(shortDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCoworkMapping(t, profile, "local_007393ceextra-1234.json", `{"cliSessionId":"other"}`)
	if path, err := claudeCoworkMappingForSession(profile, shortDir, silentTestLogger()); err != nil || path != "" {
		t.Fatalf("相邻会话的映射不应命中：%q，%v", path, err)
	}
	writeCoworkMapping(t, profile, "local_007393ce-1234.json", `{"cliSessionId":"bc202522"}`)
	p, err := claudeCoworkMappingForSession(profile, shortDir, silentTestLogger())
	if err != nil || filepath.Base(p) != "local_007393ce-1234.json" {
		t.Fatalf("short mapping = %q, %v", p, err)
	}
	// local_ 形态：目录名本身已含 local_，映射文件名=<目录名>.json。
	uuidDir := filepath.Join(profile, "local_72ae64f7-abcd")
	if err := os.MkdirAll(uuidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCoworkMapping(t, profile, "local_72ae64f7-abcd.json", `{"cliSessionId":"7962911b"}`)
	p2, err := claudeCoworkMappingForSession(profile, uuidDir, silentTestLogger())
	if err != nil || filepath.Base(p2) != "local_72ae64f7-abcd.json" {
		t.Fatalf("local_ mapping = %q, %v", p2, err)
	}
	// 损坏 / 缺 cliSessionId 按失败处理。
	bad := writeCoworkMapping(t, profile, "bad.json", "not-json")
	if _, ok, corrupt := loadClaudeCoworkSessionMeta(bad); ok || !corrupt {
		t.Fatal("corrupt mapping should fail")
	}
	missing := writeCoworkMapping(t, profile, "missing.json", `{"title":"x"}`)
	if _, ok, corrupt := loadClaudeCoworkSessionMeta(missing); ok || !corrupt {
		t.Fatal("mapping without cliSessionId should fail")
	}
}

// Cowork 布局内零 entrypoint 的 transcript 也归 Claude Desktop。
func TestClaudeCoworkCollectorZeroEntrypointGoesDesktop(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	// 用当前平台的数据目录构造 fixture：Linux 下 Cowork 根为空（不提供 Linux
	// 默认值），该 Collect 级用例在 Linux 跳过，由平台无关的单测覆盖逻辑。
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	root := roots[0]
	sessionDir := filepath.Join(root, "acct", "profile", "007393ce")
	projDir := filepath.Join(sessionDir, ".claude", "projects", "session")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"cowork-s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"cowork-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "cowork-id.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	projectsDir := t.TempDir()
	c := newClaudeCollectorCfg(t, projectsDir)
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %d, want 1: %+v", len(res.Messages), res.Messages)
	}
	if res.Messages[0].Client != model.ClientClaudeDesktop {
		t.Errorf("cowork message client = %q, want %q", res.Messages[0].Client, model.ClientClaudeDesktop)
	}
	// 无映射 → 以文件名兜底会话 id。
	if len(res.Sessions) != 1 || res.Sessions[0].ID != "cowork-id" || res.Sessions[0].Client != model.ClientClaudeDesktop {
		t.Errorf("cowork session = %+v", res.Sessions)
	}
}

// 损坏映射：整目录跳过，但必须进入部分失败通道（文件级 Err + PartialErr），
// 同批其他会话目录照常产出——否则该轮会被当作完整成功，映射修复后普通重跑
// 可能因日期完成记录而跳过。
func TestClaudeCoworkCorruptMappingGoesPartialFailure(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	badSession := filepath.Join(profile, "badsess")
	badProj := filepath.Join(badSession, ".claude", "projects", "session")
	goodProj := filepath.Join(profile, "goodsess", ".claude", "projects", "session")
	for _, dir := range []string{badProj, goodProj} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCoworkMapping(t, profile, "local_badsess.json", "not-json")
	writeCoworkMapping(t, profile, "local_goodsess.json", `{"cliSessionId":"good-id"}`)
	if err := os.WriteFile(filepath.Join(badProj, "bad-id.jsonl"), []byte(coworkTestLine("bad-msg")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodProj, "good-id.jsonl"), []byte(coworkTestLine("good-msg")), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.PartialErr == nil {
		t.Fatal("corrupt mapping must surface as partial failure, got nil")
	}
	var sawBadErr bool
	for _, st := range res.FileStatuses {
		if filepath.Base(st.Path) == "bad-id.jsonl" {
			sawBadErr = st.Err != nil
		}
	}
	if !sawBadErr {
		t.Fatalf("skipped transcript must carry a file-level error: %+v", res.FileStatuses)
	}
	// 同批健康目录照常入库。
	var sawGood bool
	for _, msg := range res.Messages {
		if msg.ID == "good-msg" && msg.Client == model.ClientClaudeDesktop {
			sawGood = true
		}
	}
	if !sawGood {
		t.Fatalf("healthy session must still be collected: %+v", res.Messages)
	}
	for _, msg := range res.Messages {
		if msg.ID == "bad-msg" {
			t.Fatalf("corrupt session must not produce messages: %+v", msg)
		}
	}
}

// ChangedFile 入口的损坏映射同样必须返回部分失败（而不是空结果 + nil）。
func TestClaudeCoworkCorruptMappingChangedFilePartialFailure(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	sessionDir := filepath.Join(profile, "badsess")
	proj := filepath.Join(sessionDir, ".claude", "projects", "session")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCoworkMapping(t, profile, "local_badsess.json", "not-json")
	file := filepath.Join(proj, "bad-id.jsonl")
	if err := os.WriteFile(file, []byte(coworkTestLine("bad-msg")), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{ChangedFile: file}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.PartialErr == nil {
		t.Fatal("ChangedFile with corrupt mapping must report partial failure")
	}
	if len(res.Messages) != 0 {
		t.Fatalf("skipped transcript must not produce messages: %+v", res.Messages)
	}
}

// 同一 cliSessionId 被两个 profile 重复发现时按逻辑会话 ID 分组裁决：消息仍按
// 各自文件采到，会话元数据只由字典序第一份实际产出用量的副本提供，不被后一份
// 覆盖。
func TestClaudeCoworkDuplicateSessionKeepsFirstDiscovery(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	root := roots[0]
	writeSession := func(profile, sessionDir, title, folder, project, msgID string) {
		t.Helper()
		profileDir := filepath.Join(root, "acct", profile)
		proj := filepath.Join(profileDir, sessionDir, ".claude", "projects", "session")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"cliSessionId":"shared-id","title":"` + title + `"`
		if folder != "" {
			body += `,"userSelectedFolders":["` + folder + `"],"folderMountNames":{"` + folder + `":"` + project + `"}`
		}
		body += "}"
		if err := os.WriteFile(filepath.Join(profileDir, "local_"+sessionDir+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, "shared-id.jsonl"), []byte(coworkTestLine(msgID)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSession("profile-a", "sess-a", "first-title", "/work/first", "first-proj", "msg-a")
	writeSession("profile-b", "sess-b", "second-title", "/work/second", "second-proj", "msg-b")

	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	var session *model.Session
	for i := range res.Sessions {
		if res.Sessions[i].ID != "shared-id" {
			continue
		}
		if session != nil {
			t.Fatalf("duplicate cliSessionId must yield one session row: %+v", res.Sessions)
		}
		session = &res.Sessions[i]
	}
	if session == nil {
		t.Fatalf("session for shared-id not collected: %+v", res.Sessions)
	}
	if session.Title != "first-title" || session.Project != "first-proj" {
		t.Fatalf("later discovery overwrote first: title=%q project=%q", session.Title, session.Project)
	}
	// 两份 transcript 的消息都按 Cowork 口径入库，不做半途丢弃。
	seen := map[string]bool{}
	for _, msg := range res.Messages {
		seen[msg.ID] = true
	}
	if !seen["msg-a"] || !seen["msg-b"] {
		t.Fatalf("messages from both copies should be collected: %+v", res.Messages)
	}
}

// coworkTestLine 构造一条最小可采的 Cowork transcript 行（无 entrypoint，
// 受控布局内一律归 Claude Desktop）。
func coworkTestLine(msgID string) string {
	return `{"type":"assistant","sessionId":"cowork-s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"` + msgID + `","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
}

// 首份副本为空、次份有消息时，会话行必须随消息一起建立（否则用量成为
// 无会话的孤儿，会话排行按内连接查不到）。
func TestClaudeCoworkEmptyFirstCopyDoesNotBlockSession(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	root := roots[0]
	write := func(profile, sessionDir, title, msgID string) {
		t.Helper()
		profileDir := filepath.Join(root, "acct", profile)
		proj := filepath.Join(profileDir, sessionDir, ".claude", "projects", "session")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"cliSessionId":"shared-empty","title":"` + title + `"}`
		if err := os.WriteFile(filepath.Join(profileDir, "local_"+sessionDir+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		content := ""
		if msgID != "" {
			content = coworkTestLine(msgID)
		}
		if err := os.WriteFile(filepath.Join(proj, "shared-empty.jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// profile-a 的 transcript 为空（无任何带 usage 的行），profile-b 有一条有效消息。
	write("profile-a", "sess-a", "empty-first", "")
	write("profile-b", "sess-b", "has-message", "only-msg")

	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	var msgs int
	for _, m := range res.Messages {
		if m.ID == "only-msg" {
			msgs++
		}
	}
	if msgs != 1 {
		t.Fatalf("第二份副本的消息应被采集: %+v", res.Messages)
	}
	var sessions int
	for _, s := range res.Sessions {
		if s.ID == "shared-empty" {
			sessions++
		}
	}
	if sessions != 1 {
		t.Fatalf("有消息的副本必须建立会话行（否则用量无会话可关联）: sessions=%+v", res.Sessions)
	}
}

// 门命中时，同一 cliSessionId 的重复副本整组重读，按路径字典序取第一份
// 实际产出用量的副本提供元数据；第二次启动补采不得让第二份覆盖首份，
// 也不得因门状态不同选出与全扫不同的标题。
func TestClaudeCoworkGateHitKeepsFirstDiscovery(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	root := roots[0]
	write := func(profile, sessionDir, title, projectName, msgID string) {
		t.Helper()
		profileDir := filepath.Join(root, "acct", profile)
		proj := filepath.Join(profileDir, sessionDir, ".claude", "projects", "session")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		folder := "/work/" + projectName
		body := `{"cliSessionId":"shared-gate","title":"` + title + `","userSelectedFolders":["` + folder + `"],"folderMountNames":{"` + folder + `":"` + projectName + `"}}`
		if err := os.WriteFile(filepath.Join(profileDir, "local_"+sessionDir+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, "shared-gate.jsonl"), []byte(coworkTestLine(msgID)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("profile-a", "sess-a", "first-title", "first-proj", "msg-a")
	write("profile-b", "sess-b", "second-title", "second-proj", "msg-b")

	c := newClaudeCollectorCfg(t, t.TempDir())
	// 第一轮：无门，全读并产出元数据。
	if _, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger()); err != nil {
		t.Fatal(err)
	}
	// 第二轮：模拟全部门命中。重复组必须整组重读，按同一确定顺序选来源。
	gate := func(string, model.FileSnapshot) bool { return true }
	res, err := c.Collect(context.Background(), CollectRequest{SkipGate: gate}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, s := range res.Sessions {
		if s.ID == "shared-gate" {
			titles = append(titles, s.Title)
		}
	}
	if len(titles) != 1 || titles[0] != "first-title" {
		t.Fatalf("重复组必须由排序第一份有消息副本提供元数据，got %v", titles)
	}
	for _, u := range res.SessionMetaUpdates {
		if u.ID == "shared-gate" && u.Title != "first-title" {
			t.Fatalf("门命中路径的元数据应与全扫一致: %+v", u)
		}
	}
}

// 同一逻辑会话 ID 的某份副本映射暂时损坏时，组内健康但无映射的副本
// 不得接管归因。整组本轮冻结（不产出消息/会话、不提交元数据更新），同批其他
// 会话照常采集，并走部分失败通道。
func TestClaudeCoworkCorruptCopyFreezesGroup(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	firstProj := filepath.Join(profile, "sess-a", ".claude", "projects", "session")
	secondProj := filepath.Join(profile, "sess-b", ".claude", "projects", "session")
	otherProj := filepath.Join(profile, "sess-other", ".claude", "projects", "session")
	for _, dir := range []string{firstProj, secondProj, otherProj} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mapBody := "{\"cliSessionId\":\"shared-corrupt\",\"title\":\"首份\",\"userSelectedFolders\":[\"/work/a\"],\"folderMountNames\":{\"/work/a\":\"proj-a\"}}"
	writeCoworkMapping(t, profile, "local_sess-a.json", mapBody)
	if err := os.WriteFile(filepath.Join(firstProj, "shared-corrupt.jsonl"), []byte(coworkTestLine("m-a")), 0o600); err != nil {
		t.Fatal(err)
	}
	// 次份同 ID、无映射：正常情况下它就是映射缺失的兜底副本。
	if err := os.WriteFile(filepath.Join(secondProj, "shared-corrupt.jsonl"), []byte(coworkTestLine("m-b")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherProj, "other-id.jsonl"), []byte(coworkTestLine("m-other")), 0o600); err != nil {
		t.Fatal(err)
	}
	// 首份映射损坏：同 ID 组必须整组冻结，不能被次份的空映射接管。
	writeCoworkMapping(t, profile, "local_sess-a.json", "not-json")

	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.PartialErr == nil {
		t.Fatal("同 ID 损坏副本必须让本轮走部分失败")
	}
	for _, msg := range res.Messages {
		if msg.ID == "m-a" || msg.ID == "m-b" {
			t.Fatalf("同 ID 组必须整组冻结，不得产出消息: %+v", msg)
		}
	}
	for _, s := range res.Sessions {
		if s.ID == "shared-corrupt" {
			t.Fatalf("同 ID 组不得产出会话行: %+v", s)
		}
	}
	for _, u := range res.SessionMetaUpdates {
		if u.ID == "shared-corrupt" {
			t.Fatalf("同 ID 组不得提交元数据更新: %+v", u)
		}
	}
	var sawOther bool
	for _, msg := range res.Messages {
		if msg.ID == "m-other" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatalf("同批其他会话必须照常采集: %+v", res.Messages)
	}
	var sawSkipErr bool
	for _, st := range res.FileStatuses {
		if filepath.Base(st.Path) == "shared-corrupt.jsonl" && st.Err != nil {
			sawSkipErr = true
		}
	}
	if !sawSkipErr {
		t.Fatalf("损坏副本必须带文件级错误: %+v", res.FileStatuses)
	}
}

// ChangedFile 指向同 ID 的健康无映射副本时，也不能丢掉另一份损坏
// 副本的状态而返回成功；整组冻结并走部分失败通道。
func TestClaudeCoworkCorruptCopyChangedFilePartialFailure(t *testing.T) {
	setIsolatedClaudeDesktopHome(t)
	roots := currentTestCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	firstProj := filepath.Join(profile, "sess-a", ".claude", "projects", "session")
	secondProj := filepath.Join(profile, "sess-b", ".claude", "projects", "session")
	for _, dir := range []string{firstProj, secondProj} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mapBody := "{\"cliSessionId\":\"shared-corrupt\",\"userSelectedFolders\":[\"/work/a\"],\"folderMountNames\":{\"/work/a\":\"proj-a\"}}"
	writeCoworkMapping(t, profile, "local_sess-a.json", mapBody)
	if err := os.WriteFile(filepath.Join(firstProj, "shared-corrupt.jsonl"), []byte(coworkTestLine("m-a")), 0o600); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(secondProj, "shared-corrupt.jsonl")
	if err := os.WriteFile(secondPath, []byte(coworkTestLine("m-b")), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCoworkMapping(t, profile, "local_sess-a.json", "not-json")

	c := newClaudeCollectorCfg(t, t.TempDir())
	res, err := c.Collect(context.Background(), CollectRequest{ChangedFile: secondPath}, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.PartialErr == nil {
		t.Fatal("ChangedFile 必须保留同 ID 损坏副本的失败，不能返回成功")
	}
	if len(res.Messages) != 0 || len(res.Sessions) != 0 || len(res.SessionMetaUpdates) != 0 {
		t.Fatalf("组内存在损坏副本时必须整组冻结: messages=%+v sessions=%+v updates=%+v",
			res.Messages, res.Sessions, res.SessionMetaUpdates)
	}
}

func TestClaudeCoworkEnumerateSkipsDirectoryCycle(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "account", "profile", "session", ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projects, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(projects, "self")); err != nil {
		t.Skipf("无法创建目录链接: %v", err)
	}
	otherSession := filepath.Join(root, "account", "profile", "other-session")
	if err := os.MkdirAll(otherSession, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(otherSession, ".claude")
	if err := os.Symlink(filepath.Dir(projects), alias); err != nil {
		t.Skipf("无法创建共享目标链接: %v", err)
	}

	outcomes, err := claudeCoworkEnumerate(t.Context(), []string{root}, nil)
	if err != nil || len(outcomes) != 2 {
		t.Fatalf("原路径与共享入口应各枚举一次，得到 %d 条路径，错误：%v", len(outcomes), err)
	}
	want := map[string]bool{transcript: true, filepath.Join(alias, "projects", "transcript.jsonl"): true}
	for _, outcome := range outcomes {
		if !want[outcome.Path] {
			t.Fatalf("意外或重复的 transcript 路径：%s", outcome.Path)
		}
		delete(want, outcome.Path)
	}
}
