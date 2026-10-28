package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// setIsolatedHome 把 os.UserHomeDir 隔离到临时目录：本机 ~/.cc-switch 真实存在，
// 不隔离会让发现结果依赖本机状态。USERPROFILE 与 HOME 同时设，兼容 Windows。
func setIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// ccSwitchAccountProjects 返回隔离 home 下指定账号 id 的 projects 目录路径。
func ccSwitchAccountProjects(t *testing.T, home, accountID string) string {
	t.Helper()
	return filepath.Join(home, ".cc-switch", "claude-accounts", accountID, "config", "projects")
}

// writeClaudeMsgJSONL 在 path 写入一条最小可采的 assistant 消息行（带 usage）。
func writeClaudeMsgJSONL(t *testing.T, path, msgID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","sessionId":"s-%s","timestamp":"2026-07-08T10:00:00+08:00","cwd":"/tmp/p","message":{"id":"%s","role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", msgID, msgID)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- 纯函数级（discoverCCSwitchClaudeProjects）----

func TestDiscoverCCSwitchClaudeProjects_NoCCSwitchDir(t *testing.T) {
	home := t.TempDir()
	if got := discoverCCSwitchClaudeProjects(home); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

func TestDiscoverCCSwitchClaudeProjects_EmptyHome(t *testing.T) {
	if got := discoverCCSwitchClaudeProjects(""); len(got) != 0 {
		t.Fatalf("expected no matches for empty home, got %v", got)
	}
}

func TestDiscoverCCSwitchClaudeProjects_MultipleAccountsSorted(t *testing.T) {
	home := t.TempDir()
	for _, id := range []string{"b-acct", "a-acct"} {
		writeClaudeMsgJSONL(t, filepath.Join(ccSwitchAccountProjects(t, home, id), "s1.jsonl"), "m1")
	}
	got := discoverCCSwitchClaudeProjects(home)
	want := []string{
		ccSwitchAccountProjects(t, home, "a-acct"),
		ccSwitchAccountProjects(t, home, "b-acct"),
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("discover = %v, want %v", got, want)
	}
}

func TestDiscoverCCSwitchClaudeProjects_MissingProjectsSubdirNotMatched(t *testing.T) {
	home := t.TempDir()
	// 账号目录存在但缺 config/projects：glob 深度不匹配，不返回。
	if err := os.MkdirAll(filepath.Join(home, ".cc-switch", "claude-accounts", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverCCSwitchClaudeProjects(home); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

func TestDiscoverCCSwitchClaudeProjects_NonDirMatchSkipped(t *testing.T) {
	home := t.TempDir()
	// projects 是同名文件而非目录：跳过。
	f := ccSwitchAccountProjects(t, home, "a")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := discoverCCSwitchClaudeProjects(home); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

// home 是动态路径、可能含 Glob 元字符（如 macOS 临时目录 user[1]）：
// 字面枚举不受模式解释影响。
func TestDiscoverCCSwitchClaudeProjects_HomeWithGlobMetachars(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "user[1]")
	writeClaudeMsgJSONL(t, filepath.Join(ccSwitchAccountProjects(t, home, "a"), "s.jsonl"), "m")
	got := discoverCCSwitchClaudeProjects(home)
	want := ccSwitchAccountProjects(t, home, "a")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("discover = %v, want [%s]", got, want)
	}
}

// ---- Collect 集成级（HOME 隔离）----

func TestClaudeCollector_CollectsCCSwitchAccountProjects(t *testing.T) {
	home := setIsolatedHome(t)
	mainDir := t.TempDir()
	writeClaudeMsgJSONL(t, filepath.Join(mainDir, "main.jsonl"), "msg-main")
	writeClaudeMsgJSONL(t, filepath.Join(ccSwitchAccountProjects(t, home, "a"), "acct.jsonl"), "msg-acct")

	c := newClaudeCollectorCfg(t, mainDir)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || len(result.Sessions) != 2 {
		t.Fatalf("messages = %d, sessions = %d, statuses = %d", len(result.Messages), len(result.Sessions), len(result.FileStatuses))
	}
	if len(result.FileStatuses) != 2 {
		t.Fatalf("statuses = %+v", result.FileStatuses)
	}
	// 主根文件在前（D5 顺序合同）。
	if filepath.Base(result.FileStatuses[0].Path) != "main.jsonl" ||
		filepath.Base(result.FileStatuses[1].Path) != "acct.jsonl" {
		t.Fatalf("status order = %v then %v", result.FileStatuses[0].Path, result.FileStatuses[1].Path)
	}
	if result.PartialErr != nil {
		t.Fatalf("unexpected partial error: %v", result.PartialErr)
	}
}

func TestClaudeCollector_DedupesMainRootEqualToAccountDir(t *testing.T) {
	home := setIsolatedHome(t)
	// 主根恰好配成账号 projects 目录：同一文件两个发现面，须去重为一次。
	acctDir := ccSwitchAccountProjects(t, home, "a")
	writeClaudeMsgJSONL(t, filepath.Join(acctDir, "dup.jsonl"), "msg-dup")

	c := newClaudeCollectorCfg(t, acctDir)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FileStatuses) != 1 || len(result.Messages) != 1 {
		t.Fatalf("statuses = %d, messages = %d（应各为 1）", len(result.FileStatuses), len(result.Messages))
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-dup" {
		t.Fatalf("messages = %+v", result.Messages)
	}
}

func TestClaudeCollector_DedupesMainRootAncestorOfAccountDir(t *testing.T) {
	home := setIsolatedHome(t)
	// 主根是账号 projects 的祖先目录（如配成 <acct>/config）：主根递归扫描与
	// 自动发现都会命中同一文件，须去重为一次。
	acctDir := ccSwitchAccountProjects(t, home, "a")
	writeClaudeMsgJSONL(t, filepath.Join(acctDir, "anc.jsonl"), "msg-anc")

	ancestor := filepath.Dir(acctDir) // <home>/.cc-switch/claude-accounts/a/config
	c := newClaudeCollectorCfg(t, ancestor)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FileStatuses) != 1 || len(result.Messages) != 1 || result.Messages[0].ID != "msg-anc" {
		t.Fatalf("statuses = %d, messages = %+v（祖先主根应去重为一次）", len(result.FileStatuses), result.Messages)
	}
}

func TestFindClaudeJSONLFilesInDirs_CancelledContextSurfaces(t *testing.T) {
	// ctx 取消经 walk 返回并由 errors.Join 聚合携带：Collect 层的
	// errors.Is(scanErr, context.Canceled) 判定据此穿透 Join 树（stdlib 语义）。
	home := t.TempDir()
	dirs := []string{
		filepath.Join(home, "one"),
		filepath.Join(home, "two"),
	}
	for _, d := range dirs {
		writeClaudeMsgJSONL(t, filepath.Join(d, "f.jsonl"), "m")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := findClaudeJSONLFilesInDirs(ctx, dirs)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled 穿透 Join", err)
	}
}

func TestClaudeCollector_AccountProjectsSubagentsExcluded(t *testing.T) {
	home := setIsolatedHome(t)
	acctDir := ccSwitchAccountProjects(t, home, "a")
	writeClaudeMsgJSONL(t, filepath.Join(acctDir, "keep.jsonl"), "msg-keep")
	writeClaudeMsgJSONL(t, filepath.Join(acctDir, "proj", "subagents", "drop.jsonl"), "msg-drop")

	c := newClaudeCollectorCfg(t, t.TempDir())
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-keep" {
		t.Fatalf("messages = %+v", result.Messages)
	}
}

func TestClaudeCollector_NoCCSwitchDirBehaviorUnchanged(t *testing.T) {
	setIsolatedHome(t)
	mainDir := t.TempDir()
	writeClaudeMsgJSONL(t, filepath.Join(mainDir, "only.jsonl"), "msg-only")

	c := newClaudeCollectorCfg(t, mainDir)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-only" {
		t.Fatalf("messages = %+v", result.Messages)
	}
}

func TestClaudeCollector_EmptyAccountProjectsDirNoError(t *testing.T) {
	home := setIsolatedHome(t)
	if err := os.MkdirAll(ccSwitchAccountProjects(t, home, "a"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := newClaudeCollectorCfg(t, t.TempDir())
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 0 || result.PartialErr != nil {
		t.Fatalf("messages = %d, partialErr = %v（空账号目录应为零文件零错误）", len(result.Messages), result.PartialErr)
	}
}

func TestClaudeCollector_ChangedFileSkipsDiscovery(t *testing.T) {
	home := setIsolatedHome(t)
	mainDir := t.TempDir()
	mainFile := filepath.Join(mainDir, "changed.jsonl")
	writeClaudeMsgJSONL(t, mainFile, "msg-changed")
	writeClaudeMsgJSONL(t, filepath.Join(ccSwitchAccountProjects(t, home, "a"), "other.jsonl"), "msg-other")

	c := newClaudeCollectorCfg(t, mainDir)
	result, err := c.Collect(context.Background(), CollectRequest{ChangedFile: mainFile}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != "msg-changed" {
		t.Fatalf("messages = %+v（ChangedFile 形态不得触发目录发现）", result.Messages)
	}
}
