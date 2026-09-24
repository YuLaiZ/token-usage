package collector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === Codex 会话标题四入口统一解析合同（索引 > state/rollout > 子线程兜底） ===
//
// 四入口 = Incremental、CLI Dates、ChangedFile、ScanExistingJSONL；全部在
// CollectResult 返回前经 resolveSessionTitles 应用同一优先级，TitleSource 随
// 标题一起产出，供 db 层做来源优先级合并（防重采回退）。

// writeCodexIndexFile 在 stateDir 下写标题索引 fixture。
func writeCodexIndexFile(t *testing.T, stateDir string, lines string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, codexSessionIndexFile), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

// codexTitleFixture 组装 Codex collector 测试环境：state DB 三个 thread——
// thread-main（state prompt 标题，user 主线程）、thread-sub（subagent 无标题）、
// thread-app（agent_created_thread 无标题）；索引命中 thread-main 与 thread-app。
func codexTitleFixture(t *testing.T) (*CodexCollector, string) {
	// 第二返回值是 sessions 目录（ChangedFile 用例定位 rollout 用）。
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	rolloutMain := filepath.Join(sessionsDir, "rollout-main.jsonl")
	rolloutSub := filepath.Join(sessionsDir, "rollout-sub.jsonl")
	rolloutApp := filepath.Join(sessionsDir, "rollout-app.jsonl")
	writeRollout(t, rolloutMain, "thread-main", "gpt-5.4", "msg-main")
	writeRollout(t, rolloutSub, "thread-sub", "gpt-5.4", "msg-sub")
	writeRollout(t, rolloutApp, "thread-app", "gpt-5.4", "msg-app")

	createStateDBWithThreads(t, stateDir, []codexThread{
		{ID: "thread-main", RolloutPath: rolloutMain, Cwd: "/tmp/a", Source: "cli",
			Title: "state prompt 标题", UpdatedAtMS: 1000, ThreadSource: "user"},
		{ID: "thread-sub", RolloutPath: rolloutSub, Cwd: "/tmp/a", Source: "cli",
			Title: "", UpdatedAtMS: 2000, ThreadSource: "subagent"},
		{ID: "thread-app", RolloutPath: rolloutApp, Cwd: "/tmp/a", Source: "vscode",
			Title: "", UpdatedAtMS: 3000, ThreadSource: "agent_created_thread"},
	})

	writeCodexIndexFile(t, stateDir,
		`{"id":"thread-main","thread_name":"App 改名标题","updated_at":"2026-09-20T00:00:00Z"}`+"\n"+
			`{"id":"thread-app","thread_name":"修复中外运电子合同与摘要文件链路","updated_at":"2026-09-23T11:30:00Z"}`+"\n")

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	_ = rolloutMain
	return NewCodexCollector(cfg), sessionsDir
}

// codexTitlesByID 把 Sessions 折成 id → (title, titleSource) 便于断言。
func codexTitlesByID(t *testing.T, sessions []model.Session) map[string][2]string {
	t.Helper()
	out := make(map[string][2]string, len(sessions))
	for _, s := range sessions {
		out[s.ID] = [2]string{s.Title, s.TitleSource}
	}
	return out
}

func assertCodexTitle(t *testing.T, got map[string][2]string, id, title, source string) {
	t.Helper()
	pair, ok := got[id]
	if !ok {
		t.Fatalf("session %s 缺失", id)
	}
	if pair[0] != title || pair[1] != source {
		t.Errorf("session %s: got (%q,%q), want (%q,%q)", id, pair[0], pair[1], title, source)
	}
}

// TestCodexCollector_IncrementalTitlePriority：state 增量入口的优先级——索引
// 命中覆盖 state prompt 标题（index）、未命中保留（无）、子线程全空兜底
// （subagent 与 agent_created_thread 两种 thread_source 都兜底）、索引命中的
// agent_created_thread 不再兜底。
func TestCodexCollector_IncrementalTitlePriority(t *testing.T) {
	c, _ := codexTitleFixture(t)
	result, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceCodexState: {}},
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := codexTitlesByID(t, result.Sessions)
	assertCodexTitle(t, got, "thread-main", "App 改名标题", model.TitleSourceIndex)
	assertCodexTitle(t, got, "thread-sub", "Codex 子线程 · thread-s", model.TitleSourceFallback)
	assertCodexTitle(t, got, "thread-app", "修复中外运电子合同与摘要文件链路", model.TitleSourceIndex)
	// 索引命中的 Session 携带 updated_at（TitleIndexTS>0，落库防倒退依赖）。
	for _, s := range result.Sessions {
		if s.TitleSource == model.TitleSourceIndex && s.TitleIndexTS <= 0 {
			t.Errorf("session %s 索引命中但 TitleIndexTS = %d, want >0", s.ID, s.TitleIndexTS)
		}
		if s.TitleSource != model.TitleSourceIndex && s.TitleIndexTS != 0 {
			t.Errorf("session %s 非索引来源但 TitleIndexTS = %d, want 0", s.ID, s.TitleIndexTS)
		}
	}
}

// TestCodexCollector_DatesTitlePriority：CLI Dates 入口（非增量全读 state）同一优先级。
func TestCodexCollector_DatesTitlePriority(t *testing.T) {
	c, _ := codexTitleFixture(t)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := codexTitlesByID(t, result.Sessions)
	assertCodexTitle(t, got, "thread-main", "App 改名标题", model.TitleSourceIndex)
	assertCodexTitle(t, got, "thread-sub", "Codex 子线程 · thread-s", model.TitleSourceFallback)
	assertCodexTitle(t, got, "thread-app", "修复中外运电子合同与摘要文件链路", model.TitleSourceIndex)
}

// TestCodexCollector_ChangedFileTitlePriority：ChangedFile 入口——解析侧无
// state fallback，state 回填后同样被索引覆盖；子线程经 state DB 判定兜底。
func TestCodexCollector_ChangedFileTitlePriority(t *testing.T) {
	c, sessionsDir := codexTitleFixture(t)
	rolloutSub := filepath.Join(sessionsDir, "rollout-sub.jsonl")
	result, err := c.Collect(context.Background(), CollectRequest{ChangedFile: rolloutSub}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := codexTitlesByID(t, result.Sessions)
	assertCodexTitle(t, got, "thread-sub", "Codex 子线程 · thread-s", model.TitleSourceFallback)
}

// TestCodexCollector_ExistingJSONLTitlePriority：rollout 全扫入口（startup
// catch-up）同一优先级，含索引命中与子线程兜底。
func TestCodexCollector_ExistingJSONLTitlePriority(t *testing.T) {
	c, _ := codexTitleFixture(t)
	result, err := c.Collect(context.Background(), CollectRequest{ScanExistingJSONL: true}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := codexTitlesByID(t, result.Sessions)
	assertCodexTitle(t, got, "thread-main", "App 改名标题", model.TitleSourceIndex)
	assertCodexTitle(t, got, "thread-sub", "Codex 子线程 · thread-s", model.TitleSourceFallback)
	assertCodexTitle(t, got, "thread-app", "修复中外运电子合同与摘要文件链路", model.TitleSourceIndex)
}

// TestCodexCollector_TitleKeepsStateWhenIndexMissing：索引缺文件（老版本
// Codex / 尚未写入）时保留 state 标题并标记 native，子线程仍兜底。
func TestCodexCollector_TitleKeepsStateWhenIndexMissing(t *testing.T) {
	c, _ := codexTitleFixture(t)
	if err := os.Remove(filepath.Join(c.stateDir, codexSessionIndexFile)); err != nil {
		t.Fatal(err)
	}
	result, err := c.Collect(context.Background(), CollectRequest{
		Incremental: true,
		Cursors:     map[string]model.SyncCursor{SyncSourceCodexState: {}},
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got := codexTitlesByID(t, result.Sessions)
	assertCodexTitle(t, got, "thread-main", "state prompt 标题", model.TitleSourceNative)
	assertCodexTitle(t, got, "thread-sub", "Codex 子线程 · thread-s", model.TitleSourceFallback)
	assertCodexTitle(t, got, "thread-app", "Codex 子线程 · thread-a", model.TitleSourceFallback)
}

// TestCodexCollector_ChildThreadKeepsStateTitle：子线程在 state DB 已有非空
// 标题（subagent 的任务描述形态）且索引未命中时保留该标题并标 native——
// 即方案 §5.2 的「无索引且已有标题」形态，不误兜底。
func TestCodexCollector_ChildThreadKeepsStateTitle(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)
	rollout := filepath.Join(sessionsDir, "rollout-sub-titled.jsonl")
	writeRollout(t, rollout, "thread-sub-titled", "gpt-5.4", "msg-sub-titled")
	createStateDBWithThreads(t, stateDir, []codexThread{
		{ID: "thread-sub-titled", RolloutPath: rollout, Cwd: "/tmp/a", Source: "cli",
			Title: "实现合同选择与供应商选择", UpdatedAtMS: 1000, ThreadSource: "subagent"},
	})
	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	result, err := NewCodexCollector(cfg).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	assertCodexTitle(t, codexTitlesByID(t, result.Sessions), "thread-sub-titled",
		"实现合同选择与供应商选择", model.TitleSourceNative)
}

// TestCodexCollector_NonChildEmptyTitleStaysEmpty：非子线程（user /
// realtime_voice / 未知）空标题不兜底、来源为空，等待更好的来源。
func TestCodexCollector_NonChildEmptyTitleStaysEmpty(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)
	rollout := filepath.Join(sessionsDir, "rollout-voice.jsonl")
	writeRollout(t, rollout, "thread-voice", "gpt-5.4", "msg-voice")
	createStateDBWithThreads(t, stateDir, []codexThread{
		{ID: "thread-voice", RolloutPath: rollout, Cwd: "/tmp/a", Source: "cli",
			Title: "", UpdatedAtMS: 1000, ThreadSource: "realtime_voice"},
	})
	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	result, err := NewCodexCollector(cfg).Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	assertCodexTitle(t, codexTitlesByID(t, result.Sessions), "thread-voice", "", "")
}

// TestCodexChildThreadFallbackTitle：兜底格式=固定前缀 + ID 前 8 位；短 ID
// 用整个 ID。
func TestCodexChildThreadFallbackTitleFormat(t *testing.T) {
	if got := CodexChildThreadFallbackTitle("019e1f37-ca44-7e51"); got != "Codex 子线程 · 019e1f37" {
		t.Fatalf("got %q", got)
	}
	if got := CodexChildThreadFallbackTitle("short"); got != "Codex 子线程 · short" {
		t.Fatalf("got %q", got)
	}
}
