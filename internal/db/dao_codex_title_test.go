package db

import (
	"context"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === Codex 会话标题来源优先级合同（upsertCodexSessionMetaSQL / ApplyCodexIndexTitles） ===

// codexTitleRow 读取单行 (title, title_source)。
func codexTitleRow(t *testing.T, d *DB, id, client string) (string, string) {
	t.Helper()
	title, src, _ := codexTitleRowTS(t, d, id, client)
	return title, src
}

// codexTitleRowTS 读取单行 (title, title_source, title_index_ts)。
func codexTitleRowTS(t *testing.T, d *DB, id, client string) (string, string, int64) {
	t.Helper()
	var title, src string
	var ts int64
	if err := d.QueryRow(`SELECT title,COALESCE(title_source,''),COALESCE(title_index_ts,0) FROM sessions WHERE id=? AND client=?`, id, client).Scan(&title, &src, &ts); err != nil {
		t.Fatalf("读取 sessions %s/%s: %v", id, client, err)
	}
	return title, src, ts
}

// upsertCodexSession 是 UpsertSessionMeta 的 Codex 单行便捷封装（ts 为索引
// 来源的 title_index_ts，非 index 来源传 0）。
func upsertCodexSession(t *testing.T, d *DB, id, title, source string, ts int64) {
	t.Helper()
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: id, Client: model.ClientCodexApp, Directory: "/d", Project: "p",
		Title: title, TitleSource: source, TitleIndexTS: ts, FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestUpsertSessionMetaCodexTitlePriority 锁定 Codex 行 title/title_source 的
// 来源优先级合并矩阵：index > native > fallback，空标题保留库值，历史空来源
// 等价 native 的可覆盖地位且不被兜底按前缀误判。
func TestUpsertSessionMetaCodexTitlePriority(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()

	cases := []struct {
		name                  string
		seedTitle, seedSource string
		seedTS                int64
		newTitle, newSource   string
		newTS                 int64
		wantTitle, wantSource string
		wantTS                int64 // 期望落库 title_index_ts（显式声明，不推导）
	}{
		{"索引覆盖历史未知来源", "prompt", "", 0, "app-title", model.TitleSourceIndex, 100, "app-title", model.TitleSourceIndex, 100},

		{"native 不回退 index", "app-title", model.TitleSourceIndex, 200, "prompt-v2", model.TitleSourceNative, 0, "app-title", model.TitleSourceIndex, 200},

		{"native 覆盖 native", "prompt", model.TitleSourceNative, 0, "prompt-v2", model.TitleSourceNative, 0, "prompt-v2", model.TitleSourceNative, 0},

		{"native 覆盖历史未知来源", "prompt", "", 0, "prompt-v2", model.TitleSourceNative, 0, "prompt-v2", model.TitleSourceNative, 0},

		// 种子标题恰好长得像兜底：错误实现（按 LIKE 'Codex 子线程 %' 前缀判兜底）
		// 会把它覆盖掉而被本用例抓住——判定只能依据 title_source 列。
		{"fallback 不覆盖长得像兜底的历史标题（不按前缀判兜底）", "Codex 子线程 · 用户自拟", "", 0, "Codex 子线程 · abc", model.TitleSourceFallback, 0, "Codex 子线程 · 用户自拟", "", 0},

		{"native 替换既有兜底", "Codex 子线程 · abc", model.TitleSourceFallback, 0, "任务描述", model.TitleSourceNative, 0, "任务描述", model.TitleSourceNative, 0},

		{"index 替换既有兜底", "Codex 子线程 · abc", model.TitleSourceFallback, 0, "app-title", model.TitleSourceIndex, 100, "app-title", model.TitleSourceIndex, 100},

		{"空标题兜底写入空会话", "", "", 0, "Codex 子线程 · abc", model.TitleSourceFallback, 0, "Codex 子线程 · abc", model.TitleSourceFallback, 0},

		{"兜底不覆盖 index", "app-title", model.TitleSourceIndex, 200, "Codex 子线程 · abc", model.TitleSourceFallback, 0, "app-title", model.TitleSourceIndex, 200},

		{"兜底不覆盖 native", "prompt", model.TitleSourceNative, 0, "Codex 子线程 · abc", model.TitleSourceFallback, 0, "prompt", model.TitleSourceNative, 0},

		{"兜底可替换兜底", "Codex 子线程 · old", model.TitleSourceFallback, 0, "Codex 子线程 · new", model.TitleSourceFallback, 0, "Codex 子线程 · new", model.TitleSourceFallback, 0},

		{"index 覆盖 index（改名推进）", "app-v1", model.TitleSourceIndex, 200, "app-v2", model.TitleSourceIndex, 300, "app-v2", model.TitleSourceIndex, 300},

		{"index 旧 ts 不倒退（索引重建旧快照）", "app-v2", model.TitleSourceIndex, 300, "app-v1", model.TitleSourceIndex, 200, "app-v2", model.TitleSourceIndex, 300},

		{"index 同名时间推进（ts 单独更新）", "app", model.TitleSourceIndex, 100, "app", model.TitleSourceIndex, 300, "app", model.TitleSourceIndex, 300},

		{"index 同 ts 不覆盖", "app-same", model.TitleSourceIndex, 200, "app-other", model.TitleSourceIndex, 200, "app-same", model.TitleSourceIndex, 200},

		{"空标题保留库值与来源", "app-title", model.TitleSourceIndex, 200, "", "", 0, "app-title", model.TitleSourceIndex, 200},

		{"无来源新值等价 native 覆盖", "prompt", "", 0, "prompt-v2", "", 0, "prompt-v2", "", 0},
	}
	for i, tc := range cases {
		id := "ses-prio"
		// 每个用例从干净库行开始：先删残留。
		if _, err := d.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if tc.seedTitle != "" || tc.seedSource != "" {
			upsertCodexSession(t, d, id, tc.seedTitle, tc.seedSource, tc.seedTS)
		}
		upsertCodexSession(t, d, id, tc.newTitle, tc.newSource, tc.newTS)
		gotTitle, gotSource, gotTS := codexTitleRowTS(t, d, id, model.ClientCodexApp)
		if gotTitle != tc.wantTitle || gotSource != tc.wantSource {
			t.Errorf("用例[%d %s]: got (%q,%q), want (%q,%q)", i, tc.name, gotTitle, gotSource, tc.wantTitle, tc.wantSource)
		}
		// title_index_ts 显式断言：防止实现在保留分支误把 ts 归 0——那会让
		// 下一次旧快照重放再次覆盖（倒退链）。
		if gotTS != tc.wantTS {
			t.Errorf("用例[%d %s]: title_index_ts = %d, want %d", i, tc.name, gotTS, tc.wantTS)
		}
	}
}

// TestUpsertSessionMetaCodexInsertPath：首次 INSERT 直落 excluded 值
// （无冲突合并参与），三种来源各自成立；Codex CLI 行与 Codex App 行同语义。
func TestUpsertSessionMetaCodexInsertPath(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()

	for _, tc := range []struct {
		id, client, title, source string
	}{
		{"ins-native", model.ClientCodexCLI, "prompt", model.TitleSourceNative},
		{"ins-index", model.ClientCodexApp, "app-title", model.TitleSourceIndex},
		{"ins-fallback", model.ClientCodexCLI, "Codex 子线程 · 019e1f37", model.TitleSourceFallback},
		{"ins-empty", model.ClientCodexApp, "", ""},
	} {
		if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
			ID: tc.id, Client: tc.client, Title: tc.title, TitleSource: tc.source, FirstTS: 1, LastTS: 2,
		}}); err != nil {
			t.Fatal(err)
		}
		gotTitle, gotSource := codexTitleRow(t, d, tc.id, tc.client)
		if gotTitle != tc.title || gotSource != tc.source {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.id, gotTitle, gotSource, tc.title, tc.source)
		}
	}
}

// TestUpsertSessionMetaNonCodexKeepsLegacySemantics：非 Codex 行保持通用
// upsert 语义（非空覆盖、空保留）且不触碰 title_source（列恒为空字符串，即使
// model.Session.TitleSource 带值——该字段对非 Codex client 无意义）。
func TestUpsertSessionMetaNonCodexKeepsLegacySemantics(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()

	write := func(id, title, source string) {
		t.Helper()
		if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
			ID: id, Client: model.ClientClaudeCode, Title: title, TitleSource: source, FirstTS: 1, LastTS: 2,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	write("s-claude", "t1", model.TitleSourceIndex)
	write("s-claude", "t2", model.TitleSourceIndex)
	if title, src := codexTitleRow(t, d, "s-claude", model.ClientClaudeCode); title != "t2" || src != "" {
		t.Fatalf("非 Codex 行应保持非空覆盖且 title_source 恒 '': got (%q,%q)", title, src)
	}
	write("s-claude", "", "")
	if title, src := codexTitleRow(t, d, "s-claude", model.ClientClaudeCode); title != "t2" || src != "" {
		t.Fatalf("非 Codex 行空标题应保留: got (%q,%q)", title, src)
	}
	// mimocode 行同语义（兼容 trigger 的列清单不含 title_source）。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "s-mimo", Client: model.ClientMiMoCode, Title: "m1", FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	if title, src := codexTitleRow(t, d, "s-mimo", model.ClientMiMoCode); title != "m1" || src != "" {
		t.Fatalf("mimocode 行 title_source 应为 '': got (%q,%q)", title, src)
	}
}

// TestApplyCodexIndexTitles：写入门是单一时间门（记录时间严格晚于已应用时间
// 即写入，含同名时间推进——另见 SameNameAdvancesTimestamp）；未命中、非 Codex
// 行、无对应 session 的 ID 不受影响；messages 不被触碰；同 ts 重放幂等。
func TestApplyCodexIndexTitles(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	ctx := context.Background()

	upsertCodexSession(t, d, "codex-1", "prompt 标题", model.TitleSourceNative, 0)
	upsertCodexSession(t, d, "codex-2", "", "", 0)
	if _, err := UpsertSessionMeta(ctx, d, []model.Session{{
		ID: "claude-1", Client: model.ClientClaudeCode, Title: "claude 标题", FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertMessages(ctx, d, []model.Message{{
		ID: "m1", SessionID: "codex-1", Client: model.ClientCodexApp,
		Date: "2026-09-24", TS: 1000, TotalTokens: 42,
	}}); err != nil {
		t.Fatal(err)
	}

	updated, err := ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-1":    {Name: "App 改名标题", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
		"codex-2":    {Name: "空标题会话的 App 标题", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
		"claude-1":   {Name: "不应命中", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
		"codex-none": {Name: "无 session 行", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
		"":           {Name: "空 ID 跳过", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 2 {
		t.Fatalf("更新行数 = %d, want 2", updated)
	}
	if title, src := codexTitleRow(t, d, "codex-1", model.ClientCodexApp); title != "App 改名标题" || src != model.TitleSourceIndex {
		t.Fatalf("codex-1: got (%q,%q)", title, src)
	}
	if title, src := codexTitleRow(t, d, "codex-2", model.ClientCodexApp); title != "空标题会话的 App 标题" || src != model.TitleSourceIndex {
		t.Fatalf("codex-2: got (%q,%q)", title, src)
	}
	if title, src := codexTitleRow(t, d, "claude-1", model.ClientClaudeCode); title != "claude 标题" || src != "" {
		t.Fatalf("claude-1 不应被同步: got (%q,%q)", title, src)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id='codex-none'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("同步不得插入新 session 行")
	}
	var total int64
	if err := d.QueryRow(`SELECT total_tokens FROM messages WHERE id='m1'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 42 {
		t.Fatalf("messages 不应被触碰: total=%d", total)
	}

	// 幂等：同 ts 同值再同步零写入。
	updated, err = ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-1": {Name: "App 改名标题", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
		"codex-2": {Name: "空标题会话的 App 标题", UpdatedAtUnixNano: tsNano("2026-09-24T10:00:00Z")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Fatalf("幂等重放更新行数 = %d, want 0", updated)
	}

	// 标题相同但来源不同：仍写（来源标识推进）。
	upsertCodexSession(t, d, "codex-3", "同名标题", model.TitleSourceNative, 0)
	updated, err = ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-3": {Name: "同名标题", UpdatedAtUnixNano: tsNano("2026-09-24T11:00:00Z")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("来源推进更新行数 = %d, want 1", updated)
	}
	if _, src := codexTitleRow(t, d, "codex-3", model.ClientCodexApp); src != model.TitleSourceIndex {
		t.Fatalf("codex-3 来源应推进为 index,实际 %q", src)
	}
}

// tsNano 把 RFC3339 时间字符串转为 UnixNano（测试构造记录用）。
func tsNano(rfc3339 string) int64 {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(err)
	}
	return t.UnixNano()
}

// TestApplyCodexIndexTitlesStaleSnapshotDoesNotRegress：索引截断/重建回旧快照
// （记录 updated_at 早于或等于已应用时间）不得把已同步的新标题倒退回旧标题；
// 更新的记录时间照常推进。
func TestApplyCodexIndexTitlesStaleSnapshotDoesNotRegress(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	ctx := context.Background()
	upsertCodexSession(t, d, "codex-r", "prompt", model.TitleSourceNative, 0)

	// 首次同步：ts=200 写入「新名字」。
	if _, err := ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-r": {Name: "新名字", UpdatedAtUnixNano: 200},
	}); err != nil {
		t.Fatal(err)
	}
	// 索引重建回旧快照：同一 id 只有 ts=100 的「旧名字」→ 不倒退。
	updated, err := ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-r": {Name: "旧名字", UpdatedAtUnixNano: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Fatalf("旧快照更新行数 = %d, want 0", updated)
	}
	if title, src := codexTitleRow(t, d, "codex-r", model.ClientCodexApp); title != "新名字" || src != model.TitleSourceIndex {
		t.Fatalf("旧快照后不应倒退: got (%q,%q)", title, src)
	}
	// 同 ts 的重放（含标题被改写的极端形态）同样不覆盖——这也是同时间戳跨轮
	// 「后行胜出」不支持 的显式决策锁定：仅凭时间戳无法区分「文件内后追加」
	// 与「重建快照里的旧行」，后者覆盖会破坏防倒退；该缝隙中下一次任何时间
	// 更新的改名仍会正常覆盖（见下一断言）。
	updated, err = ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-r": {Name: "同时间不同标题", UpdatedAtUnixNano: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 || mustTitle(t, d, "codex-r") != "新名字" {
		t.Fatalf("同 ts 重放不应覆盖: updated=%d title=%q", updated, mustTitle(t, d, "codex-r"))
	}
	// 更新的记录时间照常推进（ts=300 的「再改名」）。
	updated, err = ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-r": {Name: "再改名", UpdatedAtUnixNano: 300},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 || mustTitle(t, d, "codex-r") != "再改名" {
		t.Fatalf("新记录应推进: updated=%d title=%q", updated, mustTitle(t, d, "codex-r"))
	}
}

// TestApplyCodexIndexTitlesSameNameAdvancesTimestamp：同名、时间更新的记录必须
// 推进 title_index_ts——否则随后的旧快照能凭时间门把标题倒退（A@100 → A@300 →
// B@200 回归：B@200 不得通过 200>100 的假门）。
func TestApplyCodexIndexTitlesSameNameAdvancesTimestamp(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	ctx := context.Background()
	upsertCodexSession(t, d, "codex-same", "prompt", model.TitleSourceNative, 0)

	// A@100 首次同步。
	if _, err := ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-same": {Name: "A", UpdatedAtUnixNano: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// A@300：标题与来源都没变、只有时间更新 → 必须写入（ts 推进到 300）。
	updated, err := ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-same": {Name: "A", UpdatedAtUnixNano: 300},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("同名时间推进更新行数 = %d, want 1", updated)
	}
	if _, _, ts := codexTitleRowTS(t, d, "codex-same", model.ClientCodexApp); ts != 300 {
		t.Fatalf("title_index_ts = %d, want 300（同名新记录必须推进已应用时间）", ts)
	}
	// B@200：若上一步漏推进（ts 停在 100），200>100 会通过并倒退——必须被 300 挡住。
	updated, err = ApplyCodexIndexTitles(ctx, d, map[string]model.CodexTitleIndexRecord{
		"codex-same": {Name: "B", UpdatedAtUnixNano: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 || mustTitle(t, d, "codex-same") != "A" {
		t.Fatalf("A@300 后的 B@200 不得倒退: updated=%d title=%q", updated, mustTitle(t, d, "codex-same"))
	}
}

// mustTitle 读取 Codex App 行的标题。
func mustTitle(t *testing.T, d *DB, id string) string {
	t.Helper()
	title, _ := codexTitleRow(t, d, id, model.ClientCodexApp)
	return title
}

// TestApplyCodexFallbackTitles：兜底只写空标题 Codex 行；非空（含兜底标题自身
// 与用户标题）、非 Codex 行、无行 ID 不动；幂等。
func TestApplyCodexFallbackTitles(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	ctx := context.Background()
	upsertCodexSession(t, d, "empty-child", "", "", 0)
	upsertCodexSession(t, d, "native-titled", "用户标题", model.TitleSourceNative, 0)
	upsertCodexSession(t, d, "old-fallback", "Codex 子线程 · old", model.TitleSourceFallback, 0)
	if _, err := UpsertSessionMeta(ctx, d, []model.Session{{
		ID: "claude-empty", Client: model.ClientClaudeCode, Title: "", FirstTS: 1, LastTS: 2,
	}}); err != nil {
		t.Fatal(err)
	}

	updated, err := ApplyCodexFallbackTitles(ctx, d, map[string]string{
		"empty-child":   "Codex 子线程 · empty-ch",
		"native-titled": "Codex 子线程 · native-",
		"old-fallback":  "Codex 子线程 · new",
		"claude-empty":  "Codex 子线程 · claude-e",
		"no-row":        "Codex 子线程 · no-row-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("更新行数 = %d, want 1（只有空标题 Codex 行）", updated)
	}
	if title, src := codexTitleRow(t, d, "empty-child", model.ClientCodexApp); title != "Codex 子线程 · empty-ch" || src != model.TitleSourceFallback {
		t.Fatalf("empty-child: got (%q,%q)", title, src)
	}
	if title, _ := codexTitleRow(t, d, "native-titled", model.ClientCodexApp); title != "用户标题" {
		t.Fatalf("非空用户标题不应被兜底覆盖: %q", title)
	}
	if title, _ := codexTitleRow(t, d, "old-fallback", model.ClientCodexApp); title != "Codex 子线程 · old" {
		t.Fatalf("既有兜底标题也不应被改写: %q", title)
	}
	if title, _ := codexTitleRow(t, d, "claude-empty", model.ClientClaudeCode); title != "" {
		t.Fatalf("非 Codex 行不应被写: %q", title)
	}
	// 幂等：空行已非空，重放零写入。
	updated, err = ApplyCodexFallbackTitles(ctx, d, map[string]string{
		"empty-child": "Codex 子线程 · empty-ch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Fatalf("幂等重放更新行数 = %d, want 0", updated)
	}
}
