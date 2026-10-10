package engine

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// OpenCode 布局三态判定的 engine 级验收：真实 collector 走完整
// GetSyncCursors → Collect → persistClientBatch → SetSyncCursors 链路，
// 验证布局标记与消息、message 游标的同事务原子性（F8/F10/F11/F12）。
// 源库 fixture 见 opencode_layout_fixture_test.go（合成 V2 形态，真实 2.x
// 未知项未闭合，不据此登记 V2 兼容完成）。

// ocLayoutTestEnv 组装 engine 级 OpenCode 采集环境（真实 collector + 临时目标库）。
// HOME 重定向隔离 ~/.cache/opencode 的 provider 映射。
func ocLayoutTestEnv(t *testing.T, dbPath string) (*db.DB, *Deps) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usageDB.Close() })
	cfg := &config.Config{Clients: map[string]config.Client{
		"opencode": {Enabled: true, Paths: map[string]string{"db": dbPath}},
	}}
	return usageDB, testDeps(true, collector.NewOpenCodeCollector(cfg))
}

func ocLayoutRunIncremental(t *testing.T, usageDB *db.DB, deps *Deps) Result {
	t.Helper()
	return RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "opencode",
		collector.CollectRequest{Incremental: true}, false, false)
}

func ocLayoutRunDates(t *testing.T, usageDB *db.DB, deps *Deps, dates []string) Result {
	t.Helper()
	return RunCollect(context.Background(), deps, usageDB,
		collectTestLogger(), io.Discard, "opencode",
		collector.CollectRequest{Dates: dates}, false, false)
}

// ocLayoutMark 读取当前布局标记（0=未标记）。
func ocLayoutMark(t *testing.T, usageDB *db.DB) int64 {
	t.Helper()
	cursors, err := db.GetSyncCursors(context.Background(), usageDB, "opencode",
		[]string{collector.SyncSourceOpenCodeLayout})
	if err != nil {
		t.Fatal(err)
	}
	return cursors[collector.SyncSourceOpenCodeLayout].Value
}

func ocLayoutMessageCursor(t *testing.T, usageDB *db.DB) model.SyncCursor {
	t.Helper()
	cursors, err := db.GetSyncCursors(context.Background(), usageDB, "opencode",
		[]string{collector.SyncSourceOpenCodeMessage})
	if err != nil {
		t.Fatal(err)
	}
	return cursors[collector.SyncSourceOpenCodeMessage]
}

func ocLayoutTokenSum(t *testing.T, usageDB *db.DB, id string) int64 {
	t.Helper()
	var sum int64
	if err := usageDB.QueryRow(`SELECT COALESCE(SUM(total_tokens),0) FROM messages WHERE client=? AND id=?`,
		model.ClientOpenCode, id).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// ===== F8：空目标库 + 混合源库 + 无日期增量请求 =====

func TestOpenCodeLayout_EmptyTargetMixedSourceFullSweep(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)
	v2TS1 := ocLayoutMSAt(2026, 10, 4, 1, 0)
	v2TS2 := ocLayoutMSAt(2026, 10, 4, 2, 0)
	v1OldTS := ocLayoutMSAt(2026, 10, 3, 1, 0)
	// 混合源库：已迁移会话（V2 行 + V1 侧同 id 残留）与未迁移会话（仅 V1 行）。
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s-mig", "/mig", "MIG")
	ocFixtureInsertSession(t, dbPath, "s-mig", "/mig", "MIG")
	ocFixtureV2Message(t, dbPath, "v2-one", "s-mig", v2TS1, 50)
	ocFixtureV2Message(t, dbPath, "v2-two", "s-mig", v2TS2, 60)
	ocFixtureV1Message(t, dbPath, "v2-one", "s-mig", v2TS1, 99, 99, 0)
	ocFixtureInsertSession(t, dbPath, "s-old", "/old", "UNMIG")
	ocFixtureV1Message(t, dbPath, "v1-old", "s-old", v1OldTS, 40, 40, 0)

	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	result := ocLayoutRunIncremental(t, usageDB, deps)
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	// ""×V2 全量兜底：V2 全量 + V1 未迁移历史完整入账；同 id V2 优先不双计。
	for id, want := range map[string]int64{"v2-one": 50, "v2-two": 60, "v1-old": 40} {
		if got := ocLayoutTokenSum(t, usageDB, id); got != want {
			t.Fatalf("%s token sum = %d, want %d", id, got, want)
		}
	}
	if got := ocLayoutMark(t, usageDB); got != 2 {
		t.Fatalf("layout mark = %d, want 2（V2）", got)
	}
	cur := ocLayoutMessageCursor(t, usageDB)
	if cur.Value != v2TS2 || cur.ID != "v2-two" {
		t.Fatalf("message 游标应重建为 V2 扫描最大行: %+v", cur)
	}
	// 幂等重扫无重账。
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("第二轮 result = %+v", result)
	}
	for id, want := range map[string]int64{"v2-one": 50, "v2-two": 60, "v1-old": 40} {
		if got := ocLayoutTokenSum(t, usageDB, id); got != want {
			t.Fatalf("第二轮 %s token sum = %d, want %d（重扫不得双计）", id, got, want)
		}
	}
}

// ===== F10：旧版游标已存在 + 无标记 + V2 源库 + 写失败回滚后重试 =====

func TestOpenCodeLayout_UnmarkedV2WriteFailureRollsBackAndRetries(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)
	v2TS := ocLayoutMSAt(2026, 10, 6, 1, 0)
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s-mig", "/mig", "MIG")
	ocFixtureV2Message(t, dbPath, "v2-one", "s-mig", v2TS, 50)
	ocFixtureInsertSession(t, dbPath, "s-old", "/old", "UNMIG")
	ocFixtureV1Message(t, dbPath, "v1-old", "s-old", ocLayoutMSAt(2026, 10, 5, 1, 0), 40, 40, 0)

	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	// 旧版游标（V1 时代值）已存在、无布局标记。
	if err := db.SetSyncCursors(context.Background(), usageDB, "opencode", map[string]model.SyncCursor{
		collector.SyncSourceOpenCodeMessage: {Value: ocLayoutMSAt(2026, 10, 5, 12, 0), ID: "v1-era"},
	}); err != nil {
		t.Fatal(err)
	}
	// 注入游标写入失败：布局标记与重建游标必须连同消息一起回滚。
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_cursor BEFORE INSERT ON sync_state
		BEGIN SELECT RAISE(ABORT, 'cursor fail'); END`); err != nil {
		t.Fatal(err)
	}
	result := ocLayoutRunIncremental(t, usageDB, deps)
	if result.Complete() || result.Err == nil {
		t.Fatalf("游标写入失败必须失败: %+v", result)
	}
	var msgs int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientOpenCode).Scan(&msgs); err != nil || msgs != 0 {
		t.Fatalf("失败轮消息必须整轮回滚: count=%d err=%v", msgs, err)
	}
	if got := ocLayoutMark(t, usageDB); got != 0 {
		t.Fatalf("失败轮不得写入布局标记: %d", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.ID != "v1-era" {
		t.Fatalf("失败轮不得推进 message 游标: %+v", cur)
	}
	// 移除故障后下一轮自然重试成功（""×V2 兜底重新执行）。
	if _, err := usageDB.Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("重试轮 result = %+v", result)
	}
	if got := ocLayoutMark(t, usageDB); got != 2 {
		t.Fatalf("重试轮 layout mark = %d, want 2", got)
	}
	if got := ocLayoutTokenSum(t, usageDB, "v2-one"); got != 50 {
		t.Fatalf("重试轮 token sum = %d, want 50", got)
	}
}

// ===== F11：显式单日 CLI 请求与初始化解耦 =====

func TestOpenCodeLayout_ExplicitDatesDoNotInitialize(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)
	ocFixtureInsertSession(t, dbPath, "s1", "/p", "T")
	dayD := ocLayoutMSAt(2026, 10, 8, 10, 0)
	dayNext := ocLayoutMSAt(2026, 10, 9, 10, 0)
	ocFixtureV1Message(t, dbPath, "on-d", "s1", dayD, 100, 60, 40)
	ocFixtureV1Message(t, dbPath, "next-d", "s1", dayNext, 200, 120, 80)

	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	result := ocLayoutRunDates(t, usageDB, deps, []string{"2026-10-08"})
	if !result.Complete() {
		t.Fatalf("result = %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "on-d"); got != 100 {
		t.Fatalf("D 日行应入账: %d", got)
	}
	var nextRows int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id='next-d'`,
		model.ClientOpenCode).Scan(&nextRows); err != nil || nextRows != 0 {
		t.Fatalf("非 D 日不得入账: %d", nextRows)
	}
	if got := ocLayoutMark(t, usageDB); got != 0 {
		t.Fatalf("CLI Dates 不得写布局标记: %d", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != 0 || cur.ID != "" {
		t.Fatalf("CLI Dates 不得推进初始化游标: %+v", cur)
	}
	// 后续无日期增量轮正常走 ""×V1 分支：全量重扫、标记 V1、无重账。
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("增量轮 result = %+v", result)
	}
	if got := ocLayoutMark(t, usageDB); got != 1 {
		t.Fatalf("增量轮 layout mark = %d, want 1（V1）", got)
	}
	if got := ocLayoutTokenSum(t, usageDB, "on-d"); got != 100 {
		t.Fatalf("重扫不得双计: %d", got)
	}
	if got := ocLayoutTokenSum(t, usageDB, "next-d"); got != 200 {
		t.Fatalf("增量全量重扫应补齐 next-d: %d", got)
	}
}

// ===== F12：完整往返三段（V2 → V1 → V2）=====

func TestOpenCodeLayout_RoundTripV2V1V2(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)

	ts100 := ocLayoutMSAt(2026, 10, 1, 1, 0)
	ts150 := ocLayoutMSAt(2026, 10, 1, 1, 30)
	ts200 := ocLayoutMSAt(2026, 10, 1, 2, 0)
	ts250 := ocLayoutMSAt(2026, 10, 1, 2, 30)
	ts300 := ocLayoutMSAt(2026, 10, 1, 3, 0)

	// 段 1：纯 V2 源库；V2 侧行 time_updated 至 200。
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s2", "/p", "RT")
	ocFixtureV2Message(t, dbPath, "rt-100", "s2", ts100, 10)
	ocFixtureV2Message(t, dbPath, "rt-200", "s2", ts200, 20)

	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("段 1 result = %+v", result)
	}
	if got := ocLayoutMark(t, usageDB); got != 2 {
		t.Fatalf("段 1 后 mark = %d, want 2", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts200 || cur.ID != "rt-200" {
		t.Fatalf("段 1 后游标 = %+v, want ts200/rt-200", cur)
	}

	// 段 2：源库回退 V1；V1 侧存在未入账行 150（< 旧游标 200）与新行 300。
	ocFixtureDropV2(t, dbPath)
	ocFixtureInsertSession(t, dbPath, "s2", "/p", "RT")
	ocFixtureV1Message(t, dbPath, "rt-150", "s2", ts150, 15, 15, 0)
	ocFixtureV1Message(t, dbPath, "rt-300", "s2", ts300, 30, 30, 0)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("段 2 result = %+v", result)
	}
	if got := ocLayoutMark(t, usageDB); got != 1 {
		t.Fatalf("段 2 后 mark = %d, want 1（降级）", got)
	}
	for id, want := range map[string]int64{"rt-150": 15, "rt-300": 30} {
		if got := ocLayoutTokenSum(t, usageDB, id); got != want {
			t.Fatalf("段 2 %s token sum = %d, want %d（150 与 300 都必须在降级轮入账）", id, got, want)
		}
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts300 || cur.ID != "rt-300" {
		t.Fatalf("段 2 后游标应重建为 V1 扫描最大行: %+v", cur)
	}

	// 段 3：源库恢复 V2（上游迁移把 V1 行搬回 session_message 并带来新行 250；
	// V1 表残留 150/300 行，s2 已有 session_v2 行 → NOT EXISTS 排除，不重复）。
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s2", "/p", "RT")
	ocFixtureV2Message(t, dbPath, "rt-100", "s2", ts100, 10)
	ocFixtureV2Message(t, dbPath, "rt-150", "s2", ts150, 15)
	ocFixtureV2Message(t, dbPath, "rt-200", "s2", ts200, 20)
	ocFixtureV2Message(t, dbPath, "rt-250", "s2", ts250, 25)
	ocFixtureV2Message(t, dbPath, "rt-300", "s2", ts300, 30)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("段 3 result = %+v", result)
	}
	if got := ocLayoutMark(t, usageDB); got != 2 {
		t.Fatalf("段 3 后 mark = %d, want 2（恢复）", got)
	}
	if got := ocLayoutTokenSum(t, usageDB, "rt-250"); got != 25 {
		t.Fatalf("段 3 rt-250 token sum = %d, want 25（恢复轮兜底必须捕获 250 行）", got)
	}
	// 往返终态：无漏账（每 id 恰一行）且无重账（token 守恒）。
	for id, want := range map[string]int64{
		"rt-100": 10, "rt-150": 15, "rt-200": 20, "rt-250": 25, "rt-300": 30,
	} {
		var rows int
		if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=? AND id=?`,
			model.ClientOpenCode, id).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("%s rows = %d, want 1", id, rows)
		}
		if got := ocLayoutTokenSum(t, usageDB, id); got != want {
			t.Fatalf("%s token sum = %d, want %d（往返不得双计）", id, got, want)
		}
	}
}

// ===== V2 total 缺失行：既有正确账目不得被覆盖，PartialErr 轮不推游标/标记，
// 语义闭合后（补上 total）重采可正确落账。 =====

func TestOpenCodeLayout_MissingTotalDoesNotOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s2", "/p", "T")
	ts := ocLayoutMSAt(2026, 10, 8, 8, 0)
	// 先以正常 total 行完成一轮（目标已有正确账目 total=300）。
	ocFixtureV2Message(t, dbPath, "keep-1", "s2", ts, 300)
	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	if result := ocLayoutRunIncremental(t, usageDB, deps); result.Complete() {
		// 首轮 PartialErr 应为零（total 正常）。
	} else {
		t.Fatalf("首轮 result = %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 300 {
		t.Fatalf("首轮 total = %d, want 300", got)
	}

	// 同 ID 行以「五分项非零、total 缺失」形态更新（time_updated 更大）：
	// 不得把既有 300 覆盖为 0。
	noTotal := fmt.Sprintf(`{"id":"keep-1","sessionID":"s2","time":{"created":%d,"completed":%d},"tokens":{"input":50,"output":60}}`, ts+1000, ts+1000)
	ocFixtureExec(t, dbPath,
		`INSERT OR REPLACE INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?, 'assistant', 1)`,
		"keep-1", "s2", noTotal, ts+1000, ts+1000)

	result := ocLayoutRunIncremental(t, usageDB, deps)
	if result.Complete() || result.Err == nil {
		t.Fatalf("total 缺失行应使本轮以 PartialErr 失败: %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 300 {
		t.Fatalf("既有正确账目被覆盖: total=%d, want 300（未知构成不得按 0 落账）", got)
	}
	// PartialErr 轮不推进 message 游标（下一轮重试同一区间）。
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts || cur.ID != "keep-1" {
		t.Fatalf("PartialErr 轮不得推进 message 游标: %+v", cur)
	}

	// 语义闭合（行补上 total=110）后重采：正确落账，游标推进。
	fixed := fmt.Sprintf(`{"id":"keep-1","sessionID":"s2","time":{"created":%d,"completed":%d},"tokens":{"total":110,"input":50,"output":60}}`, ts+2000, ts+2000)
	ocFixtureExec(t, dbPath,
		`INSERT OR REPLACE INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?, 'assistant', 2)`,
		"keep-1", "s2", fixed, ts+2000, ts+2000)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("修复后重采 result = %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 110 {
		t.Fatalf("修复后 total = %d, want 110", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts+2000 || cur.ID != "keep-1" {
		t.Fatalf("修复后游标应推进: %+v", cur)
	}
}

// 受保护 ID 的完整链路回归（外审场景）：既有 total=300 → V2 主源改 total
// 缺失 + event 存在同 ID 旧终态 total=100 → PartialErr 轮后既有账目保持 300
// （event 不得绕过保护代落）；补齐 total 后重采恢复。
func TestOpenCodeLayout_MissingTotalEventDoesNotOverwrite(t *testing.T) {
	for _, form := range []struct {
		name   string
		tokens string
	}{
		{"key-absent", `"tokens":{"input":50,"output":60}`},
		{"null-value", `"tokens":{"total":null,"input":50,"output":60}`},
	} {
		t.Run(form.name, func(t *testing.T) {
			testMissingTotalEventDoesNotOverwrite(t, form.tokens)
		})
	}
}

func testMissingTotalEventDoesNotOverwrite(t *testing.T, missingTokens string) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	ocFixtureCreateV1(t, dbPath)
	ocFixtureCreateV2(t, dbPath)
	ocFixtureInsertSessionV2(t, dbPath, "s2", "/p", "T")
	ts := ocLayoutMSAt(2026, 10, 8, 8, 0)
	ocFixtureV2Message(t, dbPath, "keep-1", "s2", ts, 300)
	usageDB, deps := ocLayoutTestEnv(t, dbPath)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("首轮 result = %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 300 {
		t.Fatalf("首轮 total = %d, want 300", got)
	}

	// 主源更新为 total 缺失/null 形态；event 插入同 ID 旧终态（total=100）。
	noTotal := fmt.Sprintf(`{"id":"keep-1","sessionID":"s2","time":{"created":%d,"completed":%d},%s}`, ts+1000, ts+1000, missingTokens)
	ocFixtureExec(t, dbPath,
		`INSERT OR REPLACE INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?, 'assistant', 1)`,
		"keep-1", "s2", noTotal, ts+1000, ts+1000)
	ocFixtureEvent(t, dbPath, "keep-1", "s2", ts+500, 100, 40, 60)

	result := ocLayoutRunIncremental(t, usageDB, deps)
	if result.Complete() || result.Err == nil {
		t.Fatalf("total 缺失行应使本轮以 PartialErr 失败: %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 300 {
		t.Fatalf("受保护 ID 的既有账目被 event 旧终态覆盖: total=%d, want 300", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts || cur.ID != "keep-1" {
		t.Fatalf("PartialErr 轮不得推进 message 游标: %+v", cur)
	}

	// 补齐 total 后重采恢复（event 旧终态存在也不得干扰主源覆盖）。
	fixed := fmt.Sprintf(`{"id":"keep-1","sessionID":"s2","time":{"created":%d,"completed":%d},"tokens":{"total":110,"input":50,"output":60}}`, ts+2000, ts+2000)
	ocFixtureExec(t, dbPath,
		`INSERT OR REPLACE INTO session_message (id,session_id,data,time_created,time_updated,type,seq) VALUES (?,?,?,?,?, 'assistant', 2)`,
		"keep-1", "s2", fixed, ts+2000, ts+2000)
	if result := ocLayoutRunIncremental(t, usageDB, deps); !result.Complete() {
		t.Fatalf("修复后重采 result = %+v", result)
	}
	if got := ocLayoutTokenSum(t, usageDB, "keep-1"); got != 110 {
		t.Fatalf("修复后 total = %d, want 110", got)
	}
	if cur := ocLayoutMessageCursor(t, usageDB); cur.Value != ts+2000 || cur.ID != "keep-1" {
		t.Fatalf("修复后游标应推进: %+v", cur)
	}
}
