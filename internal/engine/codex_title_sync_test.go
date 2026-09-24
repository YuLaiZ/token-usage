package engine

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === Codex 标题同步合同（RunCollect 轮末同步 + SyncTitles 纯请求） ===
//
// 合同：codex 的每次采集尝试（CLI 手动、daemon catch-up、运行期触发）在采集
// 后追加一次标题索引同步；索引文件轮询器经 SyncTitles 纯请求独立触发（覆盖
// App 改名只写索引、state/rollout 均不变）。upsert 优先级合并保证重采不把
// App 标题打回 prompt 标题；索引删除/损坏不清空、不降级。

// codexTitleSyncEnv 组装真实 collector + 内存库的 codex 标题同步测试环境。
func codexTitleSyncEnv(t *testing.T) (*Deps, *db.DB, string) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	sessionsDir := filepath.Join(tmp, "sessions")
	os.MkdirAll(stateDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	rollout := filepath.Join(sessionsDir, "rollout-title-sync.jsonl")
	writeCodexEngineRollout(t, rollout, "thread-title-sync", "msg-ts-1")
	createCodexEngineStateDB(t, stateDir, []codexEngineThread{
		{ID: "thread-title-sync", RolloutPath: rollout, Source: "cli",
			Title: "state prompt 标题", ThreadSource: "user", UpdatedAtMS: 1000},
		// 存量子线程：state 有行（subagent、空标题）但无 rollout——增量游标不
		// 会重触达、全扫无文件可扫，只能由独立同步步骤兜底。
		{ID: "thread-sub-stale", Source: "cli", ThreadSource: "subagent", UpdatedAtMS: 500},
		// thread_source 为空的存量线程：不判子线程，保持空标题（数据边界）。
		{ID: "thread-unknown-src", Source: "cli", ThreadSource: "", UpdatedAtMS: 600},
	})

	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: true, Paths: map[string]string{
			"state_dir": stateDir, "sessions_dir": sessionsDir,
		}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usageDB.Close() })
	return NewDeps(cfg), usageDB, stateDir
}

// codexEngineThread / writeCodexEngineRollout / createCodexEngineStateDB 是
// engine 侧测试的 codex fixture（collector 包的 helper 不导出，此处独立最小实现）。
type codexEngineThread struct {
	ID, RolloutPath, Source, Title, ThreadSource string
	UpdatedAtMS                                  int64
}

func writeCodexEngineRollout(t *testing.T, path, sessionID, msgID string) {
	t.Helper()
	content := `{"timestamp":"2026-09-24T03:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","source":"cli","originator":"codex-tui","cwd":"/tmp"}}
{"timestamp":"2026-09-24T03:01:00Z","type":"turn_context","payload":{"model":"gpt-5.4"}}
{"timestamp":"2026-09-24T03:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"` + msgID + `"}}
{"timestamp":"2026-09-24T03:03:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func createCodexEngineStateDB(t *testing.T, stateDir string, threads []codexEngineThread) {
	t.Helper()
	dbPath := filepath.Join(stateDir, "state_5.sqlite")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open state DB: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS threads (
	id TEXT PRIMARY KEY,
	rollout_path TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT '',
	model_provider TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	tokens_used INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	first_user_message TEXT NOT NULL DEFAULT '',
	model TEXT,
	thread_source TEXT,
	agent_role TEXT,
	created_at_ms INTEGER,
	updated_at_ms INTEGER
)`); err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if _, err := conn.Exec(`INSERT OR REPLACE INTO threads
			(id, rollout_path, source, cwd, title, thread_source, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?)`,
			th.ID, th.RolloutPath, th.Source, "/tmp", th.Title, th.ThreadSource, th.UpdatedAtMS, th.UpdatedAtMS); err != nil {
			t.Fatal(err)
		}
	}
}

func writeCodexEngineIndex(t *testing.T, stateDir, lines string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, "session_index.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

func engineSessionTitle(t *testing.T, usageDB *db.DB, id string) (string, string) {
	t.Helper()
	var title, src string
	if err := usageDB.QueryRow(
		`SELECT title,COALESCE(title_source,'') FROM sessions WHERE id=? AND client=?`,
		id, model.ClientCodexCLI).Scan(&title, &src); err != nil {
		t.Fatalf("读取 sessions %s: %v", id, err)
	}
	return title, src
}

func engineMessageTokens(t *testing.T, usageDB *db.DB, id string) int64 {
	t.Helper()
	var total int64
	if err := usageDB.QueryRow(
		`SELECT total_tokens FROM messages WHERE id=? AND client=?`, id, model.ClientCodexCLI).Scan(&total); err != nil {
		t.Fatalf("读取 messages %s: %v", id, err)
	}
	return total
}

// TestRunCollectCodexTitleSyncLifecycle：完整生命周期——
//  1. 无索引采集：落 state prompt 标题（native）；
//  2. 索引出现 App 标题：普通采集轮末自动同步（state 无变化也同步）；
//  3. 重跑 ChangedFile（rollout 重采）：upsert 优先级合并防回退，App 标题保持；
//  4. App 再改名（只写索引）：SyncTitles 纯请求同步新名；
//  5. 索引删除：SyncTitles 无写入、标题保持；
//     全程 messages token 不变。
func TestRunCollectCodexTitleSyncLifecycle(t *testing.T) {
	deps, usageDB, stateDir := codexTitleSyncEnv(t)
	log := slog.Default()

	// 1. 无索引首轮采集：prompt 标题落库（native）。
	res := RunCollect(context.Background(), deps, usageDB, log, nil, "codex", collector.CollectRequest{}, true, false)
	if !res.Complete() {
		t.Fatalf("首轮采集失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "state prompt 标题" || src != model.TitleSourceNative {
		t.Fatalf("首轮: got (%q,%q), want (state prompt 标题,native)", title, src)
	}

	// 2. 索引出现 App 标题：state/rollout 均不变，普通采集轮末同步覆盖。
	writeCodexEngineIndex(t, stateDir,
		`{"id":"thread-title-sync","thread_name":"App 改名标题","updated_at":"2026-09-24T10:00:00Z"}`+"\n")
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex", collector.CollectRequest{}, true, false)
	if !res.Complete() {
		t.Fatalf("第二轮采集失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 改名标题" || src != model.TitleSourceIndex {
		t.Fatalf("轮末同步: got (%q,%q), want (App 改名标题,index)", title, src)
	}

	// 3. 索引短暂缺失 + 重跑 ChangedFile（rollout 重采，解析侧只能拿 state
	// prompt 标题）：upsert 优先级合并（存量 index 压过 excluded native）防
	// 回退，App 标题保持。先删索引使 excluded 侧确实是 native——索引若还在，
	// 解析产出的已是 index，断言走不到防回退分支。
	if err := os.Remove(filepath.Join(stateDir, "session_index.jsonl")); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(filepath.Dir(stateDir), "sessions", "rollout-title-sync.jsonl")
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex",
		collector.CollectRequest{ChangedFile: rollout}, true, false)
	if !res.Complete() {
		t.Fatalf("ChangedFile 采集失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 改名标题" || src != model.TitleSourceIndex {
		t.Fatalf("索引缺失期重采防回退: got (%q,%q), want (App 改名标题,index)", title, src)
	}

	// 4. App 再改名：只写索引（更大的 updated_at），SyncTitles 纯请求同步。
	writeCodexEngineIndex(t, stateDir,
		`{"id":"thread-title-sync","thread_name":"App 再改名","updated_at":"2026-09-24T12:00:00Z"}`+"\n")
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if !res.Complete() {
		t.Fatalf("SyncTitles 请求失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 再改名" || src != model.TitleSourceIndex {
		t.Fatalf("改名同步: got (%q,%q), want (App 再改名,index)", title, src)
	}

	// 4.5 索引重建回旧快照（同一 id 只有更早 updated_at 的旧标题）：SyncTitles
	// 不把已同步的新标题倒退回旧标题。
	writeCodexEngineIndex(t, stateDir,
		`{"id":"thread-title-sync","thread_name":"旧快照标题","updated_at":"2026-09-24T11:00:00Z"}`+"\n")
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if !res.Complete() {
		t.Fatalf("旧快照 SyncTitles 失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 再改名" || src != model.TitleSourceIndex {
		t.Fatalf("旧快照防倒退: got (%q,%q), want 保持 (App 再改名,index)", title, src)
	}

	// 4.6 采集轮读到同一份旧快照索引：excluded 侧带旧 ts 的 index 标题经
	// upsert 的 ts 门同样不倒退（端到端覆盖 Apply 与 upsert 两条写入路径）。
	rollout4 := filepath.Join(filepath.Dir(stateDir), "sessions", "rollout-title-sync.jsonl")
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex",
		collector.CollectRequest{ChangedFile: rollout4}, true, false)
	if !res.Complete() {
		t.Fatalf("旧快照 ChangedFile 采集失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 再改名" || src != model.TitleSourceIndex {
		t.Fatalf("旧快照采集轮防倒退: got (%q,%q), want 保持 (App 再改名,index)", title, src)
	}

	// 5. 索引删除：SyncTitles 无写入（不清空、不降级），标题与 token 保持。
	if err := os.Remove(filepath.Join(stateDir, "session_index.jsonl")); err != nil {
		t.Fatal(err)
	}
	res = RunCollect(context.Background(), deps, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if !res.Complete() {
		t.Fatalf("索引删除后 SyncTitles 失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "App 再改名" || src != model.TitleSourceIndex {
		t.Fatalf("索引删除后: got (%q,%q), want 保持 (App 再改名,index)", title, src)
	}
	if total := engineMessageTokens(t, usageDB, "msg-ts-1#0"); total != 120 {
		t.Fatalf("token 不应变化: %d", total)
	}
	var msgCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		t.Fatal(err)
	}
	if msgCount != 1 {
		t.Fatalf("messages 行数 = %d, want 1（不得重放）", msgCount)
	}
}

// TestRunCollectSyncTitlesDispatch：纯同步请求的分派——非 codex client
// 静默零值结果；未启用 codex 同样零值（ValidateResult 层报未启用）。
func TestRunCollectSyncTitlesDispatch(t *testing.T) {
	deps, usageDB, _ := codexTitleSyncEnv(t)
	log := slog.Default()

	res := RunCollect(context.Background(), deps, usageDB, log, nil, "claude",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if res.Matched || res.Attempted != 0 || res.Err != nil {
		t.Fatalf("非 codex client 应零值返回: %+v", res)
	}

	// codex disabled：同步同样不执行。
	cfg := &config.Config{Clients: map[string]config.Client{
		"codex": {Enabled: false},
	}}
	disabled := NewDeps(cfg)
	res = RunCollect(context.Background(), disabled, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if res.Matched || res.Attempted != 0 || res.Err != nil {
		t.Fatalf("disabled codex 应零值返回: %+v", res)
	}
}

// TestRunCollectTitleSyncFailureCounted：索引存在但不可读（路径被目录占据）
// → 轮末同步失败计入 result.Err，recordError=true 时落 collection_errors；
// 采集本体（messages）不受影响仍成功落库。
func TestRunCollectTitleSyncFailureCounted(t *testing.T) {
	deps, usageDB, stateDir := codexTitleSyncEnv(t)
	log := slog.Default()

	// 索引路径放目录：Stat 成功但非普通文件 → 读取器报错。
	if err := os.Mkdir(filepath.Join(stateDir, "session_index.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := RunCollect(context.Background(), deps, usageDB, log, nil, "codex", collector.CollectRequest{}, true, false)
	if res.Err == nil {
		t.Fatal("同步失败应计入 result.Err")
	}
	if res.Succeeded != 1 {
		t.Fatalf("采集本体应仍成功: Succeeded=%d", res.Succeeded)
	}
	// messages 落库不受同步失败影响。
	if total := engineMessageTokens(t, usageDB, "msg-ts-1#0"); total != 120 {
		t.Fatalf("messages 应已落库: %d", total)
	}
	var n int
	if err := usageDB.QueryRow(
		`SELECT COUNT(*) FROM collection_errors WHERE source='codex' AND resolved=0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("同步失败应记录 collection_errors 供 retry 恢复")
	}
}

// TestRunCollectCodexTitleSyncFallbackBackfill：存量空标题子线程经独立同步
// 步骤获得兜底——不依赖增量游标重触达或 rollout 重扫（跳过门）。thread_source
// 为空的空标题行不判子线程、保持空（数据边界）；state 有标题的空库行留给采集
// 路径回填 native；幂等。
func TestRunCollectCodexTitleSyncFallbackBackfill(t *testing.T) {
	deps, usageDB, stateDir := codexTitleSyncEnv(t)
	log := slog.Default()
	ctx := context.Background()

	// 手工构造存量库行（模拟历史采集落下的空标题会话，本轮不经采集产生）。
	insertEmpty := func(id, client string) {
		t.Helper()
		if _, err := usageDB.Exec(`INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts)
VALUES (?,?,'/d','p','','',1,2)`, id, client); err != nil {
			t.Fatal(err)
		}
	}
	insertEmpty("thread-sub-stale", model.ClientCodexCLI)
	insertEmpty("thread-unknown-src", model.ClientCodexCLI)
	// state 有非空标题的空库行：不兜底（留给采集路径回填 native）。
	insertEmpty("thread-title-sync", model.ClientCodexCLI)
	// 非 Codex 空标题行：不兜底。
	insertEmpty("claude-empty", model.ClientClaudeCode)

	res := RunCollect(ctx, deps, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if !res.Complete() {
		t.Fatalf("SyncTitles 失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-sub-stale"); title != "Codex 子线程 · thread-s" || src != model.TitleSourceFallback {
		t.Fatalf("存量子线程应获兜底: got (%q,%q)", title, src)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-unknown-src"); title != "" || src != "" {
		t.Fatalf("thread_source 空的行不应兜底: got (%q,%q)", title, src)
	}
	// state 有标题的空库行不被兜底覆盖（等采集轮回填 native）。
	if title, _ := engineSessionTitle(t, usageDB, "thread-title-sync"); title != "" {
		t.Fatalf("state 有标题的空库行不应被兜底抢先: %q", title)
	}
	var claudeTitle string
	if err := usageDB.QueryRow(`SELECT COALESCE(title,'') FROM sessions WHERE id='claude-empty'`).Scan(&claudeTitle); err != nil {
		t.Fatal(err)
	}
	if claudeTitle != "" {
		t.Fatalf("非 Codex 行不应被兜底: %q", claudeTitle)
	}

	// 幂等：重放后兜底行不再变化（空行已非空）。
	res = RunCollect(ctx, deps, usageDB, log, nil, "codex",
		collector.CollectRequest{SyncTitles: true}, true, false)
	if !res.Complete() {
		t.Fatalf("幂等重放失败: %+v", res)
	}
	if title, src := engineSessionTitle(t, usageDB, "thread-sub-stale"); title != "Codex 子线程 · thread-s" || src != model.TitleSourceFallback {
		t.Fatalf("幂等重放后兜底行应不变: got (%q,%q)", title, src)
	}
	_ = stateDir
}
