package engine

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// wbPlan 构造目标归属计划的便捷 helper（playground 决定 project 语义：true 恒
// 空串；false 为 projectBase(directory)，project 参数仅为与计划自洽的冗余值）。
func wbPlan(sessionID, client, project, title, directory string, playground bool) collector.WorkBuddySessionPlan {
	return collector.WorkBuddySessionPlan{
		SessionID: sessionID, Client: client, Project: project, Title: title, Directory: directory,
		Playground: playground,
	}
}

// wbRow 构造存量家族消息行（token 值用 total 简化断言）。
func wbRow(id, sessionID, client string, ts int64, total int64) model.Message {
	return model.Message{
		ID: id, SessionID: sessionID, Client: client, Date: "2026-10-08", TS: ts,
		Directory: "/old", Project: "old-project", TotalTokens: total,
	}
}

// 普通→expert：同事务把普通 WorkBuddy 历史行整体迁移到 expert client，token
// 不双计、不残留普通副本、project 同步校正为计划值。
func TestWorkBuddyPersist_RekeyToExpert(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	expert := model.WorkBuddyExpertClientKey("MeituanLivingAssistant")
	// 存量：普通 client 两条消息（时间戳目录 project）
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10),
		wbRow("m2", "s1", model.ClientWorkBuddy, 2000, 20),
	})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/d", Project: "2026-06-17-18-25-24", Title: "旧标题", FirstTS: 1000, LastTS: 2000},
	})

	plan := wbPlan("s1", expert, "", "新标题", "/d", true)
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{plan},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if !result.Complete() {
		t.Fatalf("采集应成功: %+v", result)
	}

	var msgs []model.Message
	rows, _ := usageDB.QueryContext(context.Background(), `SELECT id, client, project, total_tokens FROM messages WHERE session_id='s1' ORDER BY id`)
	for rows.Next() {
		var m model.Message
		rows.Scan(&m.ID, &m.Client, &m.Project, &m.TotalTokens)
		msgs = append(msgs, m)
	}
	rows.Close()
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d, want 2（迁移不增不减）", len(msgs))
	}
	for _, m := range msgs {
		if m.Client != expert {
			t.Errorf("消息 %s client = %q, want expert 键", m.ID, m.Client)
		}
		if m.Project != "" {
			t.Errorf("消息 %s project = %q, want 空串（计划 project 校正）", m.ID, m.Project)
		}
	}
	if msgs[0].TotalTokens != 10 || msgs[1].TotalTokens != 20 {
		t.Errorf("token 应保持原值: %+v", msgs)
	}

	var sessClient, sessProject, sessTitle string
	var firstTS, lastTS int64
	usageDB.QueryRowContext(context.Background(),
		`SELECT client, project, title, first_ts, last_ts FROM sessions WHERE id='s1'`).
		Scan(&sessClient, &sessProject, &sessTitle, &firstTS, &lastTS)
	if sessClient != expert || sessProject != "" || sessTitle != "新标题" {
		t.Errorf("会话行 = (%q,%q,%q), want expert 键/空串/新标题", sessClient, sessProject, sessTitle)
	}
	if firstTS != 1000 || lastTS != 2000 {
		t.Errorf("时间区间 = %d..%d, want 1000..2000（历史区间保留）", firstTS, lastTS)
	}

	// 无残留普通副本
	var stray int
	usageDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE session_id='s1' AND client=?`, model.ClientWorkBuddy).Scan(&stray)
	if stray != 0 {
		t.Errorf("普通副本残留 %d 行", stray)
	}
}

// expert→普通 与 expertA→expertB 同样整会话迁移；重复执行幂等（不增加行、不叠加 token）。
func TestWorkBuddyPersist_RekeyBackAndForthIdempotent(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	expertA := model.WorkBuddyExpertClientKey("A")
	expertB := model.WorkBuddyExpertClientKey("B")
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10),
	})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/d", Project: "p", Title: "t", FirstTS: 1000, LastTS: 1000},
	})

	run := func(plan collector.WorkBuddySessionPlan) {
		c := fixedResultCollector("workbuddy", collector.CollectResult{WorkBuddyPlans: []collector.WorkBuddySessionPlan{plan}})
		result := RunCollect(context.Background(), testDeps(true, c), usageDB,
			collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
		if !result.Complete() {
			t.Fatalf("迁移轮失败: %+v", result)
		}
	}

	run(wbPlan("s1", expertA, "p", "t", "/d", false))               // 普通→A
	run(wbPlan("s1", expertB, "p", "t", "/d", false))               // A→B
	run(wbPlan("s1", model.ClientWorkBuddy, "p", "t", "/d", false)) // B→普通
	run(wbPlan("s1", model.ClientWorkBuddy, "p", "t", "/d", false)) // 幂等重跑

	var msgCount int
	var clients []string
	_ = clients
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE session_id='s1'`).Scan(&msgCount)
	if msgCount != 1 {
		t.Fatalf("三向迁移+幂等后消息数 = %d, want 1", msgCount)
	}
	var sessCount int
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions WHERE id='s1'`).Scan(&sessCount)
	if sessCount != 1 {
		t.Fatalf("会话行 = %d, want 1（仅一个最终 client，无孤儿）", sessCount)
	}
	var total int64
	usageDB.QueryRowContext(context.Background(), `SELECT total_tokens FROM messages WHERE session_id='s1'`).Scan(&total)
	if total != 10 {
		t.Errorf("token = %d, want 10（不叠加）", total)
	}
}

// 本轮消息优先于迁移旧副本：同 ID 同 ts 时本轮解析值（token/model）胜出。
func TestWorkBuddyPersist_CurrentRoundWinsOverMigratedCopy(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	expert := model.WorkBuddyExpertClientKey("A")
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 111),
	})
	current := model.Message{
		ID: "m1", SessionID: "s1", Client: expert, Date: "2026-10-08", TS: 1000,
		Model: "new-model", Provider: "P", Directory: "/d", Project: "", TotalTokens: 999,
	}
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		Messages:       []model.Message{current},
		Sessions:       []model.Session{{ID: "s1", Client: expert, Directory: "/d", Project: "", Title: "t", FirstTS: 1000, LastTS: 1000}},
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", expert, "", "t", "/d", true)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if !result.Complete() {
		t.Fatalf("采集失败: %+v", result)
	}
	var total int64
	var mdl string
	var msgCount int
	usageDB.QueryRowContext(context.Background(),
		`SELECT total_tokens, model FROM messages WHERE session_id='s1'`).Scan(&total, &mdl)
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE session_id='s1'`).Scan(&msgCount)
	if msgCount != 1 {
		t.Fatalf("消息数 = %d, want 1（同 ID 不双行）", msgCount)
	}
	if total != 999 || mdl != "new-model" {
		t.Errorf("本轮值应胜出: total=%d model=%q, want 999/new-model", total, mdl)
	}
}

// 同 ID 双副本确定性合并：归因列取较早 ts、token 列取较大 ts 副本整组、
// router 空值补齐非空、project 总由计划校正；请求计数与 token 不相加。
func TestMergeWorkBuddySession_DeterministicRules(t *testing.T) {
	expert := model.WorkBuddyExpertClientKey("A")
	rows := []model.Message{
		// 普通：早 ts、旧 router 值
		{ID: "m1", SessionID: "s1", Client: model.ClientWorkBuddy, Date: "2026-10-07", TS: 1000, Model: "old", Provider: "oldp", RouterName: "r-old", Directory: "/old-dir", Project: "old", TotalTokens: 10},
		// expert：晚 ts、新 token/router
		{ID: "m1", SessionID: "s1", Client: expert, Date: "2026-10-08", TS: 2000, Model: "new", Provider: "newp", RouterName: "", Directory: "/new-dir", Project: "old", TotalTokens: 20},
	}
	merge := mergeWorkBuddySession(wbPlan("s1", expert, "", "t", "/d", true), rows, nil)
	if len(merge.messages) != 1 {
		t.Fatalf("合并后 = %d 行, want 1", len(merge.messages))
	}
	m := merge.messages[0]
	if m.TS != 1000 || m.Date != "2026-10-07" || m.Directory != "/old-dir" {
		t.Errorf("归因列应取较早 ts 副本: %+v", m)
	}
	if m.Model != "new" || m.Provider != "newp" || m.TotalTokens != 20 {
		t.Errorf("token/model 应取较大 ts 副本整组: %+v", m)
	}
	if m.RouterName != "r-old" {
		t.Errorf("router 空值应从其他副本补齐非空, got %q", m.RouterName)
	}
	if m.Project != "" {
		t.Errorf("project 总由计划校正, got %q", m.Project)
	}
	if m.Client != expert {
		t.Errorf("client = %q, want expert", m.Client)
	}
}

// 同 ts 双副本：目标侧优先（归因与 token 都保留目标侧值）。
func TestMergeWorkBuddySession_SameTSTargetWins(t *testing.T) {
	expert := model.WorkBuddyExpertClientKey("A")
	rows := []model.Message{
		{ID: "m1", SessionID: "s1", Client: expert, Date: "d", TS: 1000, Model: "target", Provider: "tp", Directory: "/tgt", Project: "x", TotalTokens: 7},
		{ID: "m1", SessionID: "s1", Client: model.ClientWorkBuddy, Date: "d", TS: 1000, Model: "source", Provider: "sp", Directory: "/src", Project: "x", TotalTokens: 9},
	}
	merge := mergeWorkBuddySession(wbPlan("s1", expert, "p", "t", "/d", false), rows, nil)
	m := merge.messages[0]
	if m.Model != "target" || m.TotalTokens != 7 || m.Directory != "/tgt" {
		t.Errorf("同 ts 应目标侧优先: %+v", m)
	}
}

// 身份冲突：同 ID 已属于其他会话（含目标侧）→ 整轮失败回滚，不吞并不删数据。
// 注：messages 主键为 (client,id)，同 client 同 id 会被 upsert 归并，「同 ID 跨
// 会话」只能以跨家族 client 副本形态存在——正是身份冲突检测的对象。
func TestWorkBuddyPersist_IdentityConflictRollsBack(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	expert := model.WorkBuddyExpertClientKey("A")
	// m1 在普通 client 下属于 s1，同时在 expert client 下属于 s2（历史双份行）
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10),
		wbRow("m1", "s2", expert, 2000, 20),
	})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/d", Project: "p", Title: "t", FirstTS: 1000, LastTS: 1000},
	})

	// 计划把 s1 收敛回普通 client：m1 的 expert 副本归属 s2，冲突。
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", model.ClientWorkBuddy, "p", "t", "/d", false)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if result.Complete() {
		t.Fatal("身份冲突必须令本轮失败")
	}
	// 回滚后双方数据原样保留
	var count int
	var s1Client, s2Client string
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE id='m1'`).Scan(&count)
	usageDB.QueryRowContext(context.Background(), `SELECT client FROM messages WHERE id='m1' AND session_id='s1'`).Scan(&s1Client)
	usageDB.QueryRowContext(context.Background(), `SELECT client FROM messages WHERE id='m1' AND session_id='s2'`).Scan(&s2Client)
	if count != 2 || s1Client != model.ClientWorkBuddy || s2Client != expert {
		t.Errorf("冲突回滚后应保留双方原行: count=%d s1=%q s2=%q", count, s1Client, s2Client)
	}
}

// 完全无数据的源库行：无家族行且本轮无消息/会话 → 不创建空会话、不产生写入。
func TestWorkBuddyPersist_NoDataRowsIgnored(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	c := fixedResultCollector("workbuddy", collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("ghost", model.ClientWorkBuddy, "p", "t", "/d", false)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if !result.Complete() {
		t.Fatalf("无数据计划会话应被忽略而非失败: %+v", result)
	}
	var count int
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&count)
	if count != 0 {
		t.Errorf("不应创建空会话, got %d", count)
	}
}

// 完整无日期复核成功后按片段解决历史元数据错误（不受登记日期限制）；
// 日期请求成功不触发片段解决（只按现有日期/source 规则）。
func TestWorkBuddyPersist_FullReviewResolvesPatternErrors(t *testing.T) {
	setup := func(t *testing.T) *db.DB {
		usageDB, _ := db.Open(":memory:")
		t.Cleanup(func() { usageDB.Close() })
		db.RecordErrorsByDate(context.Background(), usageDB, []string{"2026-09-24"}, "workbuddy",
			"workbuddy 读取数据源失败: workbuddy metadata failed: 会话 x 无有效元数据", "")
		return usageDB
	}
	req := collector.CollectRequest{}
	datedReq := collector.CollectRequest{Dates: []string{"2026-10-08"}}

	usageDB := setup(t)
	c := fixedResultCollector("workbuddy", collector.CollectResult{})
	if result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", req, true, false); !result.Complete() {
		t.Fatalf("完整复核应成功: %+v", result)
	}
	var unresolved int
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM collection_errors WHERE resolved=0`).Scan(&unresolved)
	if unresolved != 0 {
		t.Errorf("完整复核成功应解决历史片段错误, 剩 %d", unresolved)
	}

	usageDB2 := setup(t)
	c2 := fixedResultCollector("workbuddy", collector.CollectResult{})
	if result := RunCollect(context.Background(), testDeps(true, c2), usageDB2,
		collectTestLogger(), io.Discard, "workbuddy", datedReq, true, false); !result.Complete() {
		t.Fatalf("日期请求应成功: %+v", result)
	}
	var unresolved2 int
	usageDB2.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM collection_errors WHERE resolved=0`).Scan(&unresolved2)
	if unresolved2 != 1 {
		t.Errorf("日期请求不应触发片段解决, 剩 %d", unresolved2)
	}
}

// 仅历史库数据、JSONL 已不存在的会话：计划仍刷新历史行的 title/client/project。
func TestWorkBuddyPersist_HistoryOnlyRefresh(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "hist", model.ClientWorkBuddy, 1000, 10),
	})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "hist", Client: model.ClientWorkBuddy, Directory: "/Users/x/WorkBuddy/2026-06-17-18-25-24", Project: "2026-06-17-18-25-24", Title: "", FirstTS: 1000, LastTS: 1000},
	})
	// 计划：playground=1（project 清空）+ 新标题；无本轮消息/会话
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{
			{SessionID: "hist", Client: model.ClientWorkBuddy, Project: "", Title: "每日A股收盘复盘", Directory: "/Users/x/WorkBuddy/2026-06-17-18-25-24", Playground: true},
		},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if !result.Complete() {
		t.Fatalf("仅历史刷新应成功: %+v", result)
	}
	var project, title string
	var msgProject string
	usageDB.QueryRowContext(context.Background(), `SELECT project, title FROM sessions WHERE id='hist'`).Scan(&project, &title)
	usageDB.QueryRowContext(context.Background(), `SELECT project FROM messages WHERE session_id='hist'`).Scan(&msgProject)
	if project != "" || title != "每日A股收盘复盘" || msgProject != "" {
		t.Errorf("仅历史刷新结果: session=(%q,%q) msg project=%q, want 空串/新标题/空串", project, title, msgProject)
	}
}

// 事务任一步失败不留下半迁移：verify 失败（构造非法残留）整轮回滚。
func TestWorkBuddyPersist_VerifyFailureRollsBack(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()

	// 普通侧有历史行。构造非法计划 client（防御路径）：buildWorkBuddyPersistPlan
	// 拒绝，整轮失败回滚，原行保留。
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10),
	})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/d", Project: "p", Title: "t", FirstTS: 1000, LastTS: 1000},
	})

	// 非法计划 client（防御路径）：buildWorkBuddyPersistPlan 拒绝，整轮失败。
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", "WorkBuddy Expert:ZZ", "p", "t", "/d", false)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if result.Complete() {
		t.Fatal("非法计划 client 必须失败")
	}
	var client string
	usageDB.QueryRowContext(context.Background(), `SELECT client FROM messages WHERE session_id='s1'`).Scan(&client)
	if client != model.ClientWorkBuddy {
		t.Errorf("失败回滚后应保留原行, got client=%q", client)
	}
}

// ---------- GPT 评审复现场景（修复回归锚点） ----------

// 本轮新消息与历史行同 ID 不同会话：身份冲突必须令整轮失败回滚，历史消息
// 的归属与 token 不被本轮覆盖（此前冲突检测只看历史行，通用 UPSERT 会把
// 另一会话的 token 写到历史消息上）。
func TestWorkBuddyPersist_IncomingIdentityConflict(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("shared", "s1", model.ClientWorkBuddy, 1000, 10),
	})
	incoming := wbRow("shared", "s2", model.ClientWorkBuddy, 2000, 999)
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		Messages:       []model.Message{incoming},
		Sessions:       []model.Session{{ID: "s2", Client: model.ClientWorkBuddy, Project: "old-project", FirstTS: 2000, LastTS: 2000}},
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s2", model.ClientWorkBuddy, "old-project", "t", "/old", false)},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	var sid string
	var total int64
	usageDB.QueryRowContext(context.Background(), "SELECT session_id, total_tokens FROM messages WHERE id='shared'").Scan(&sid, &total)
	if result.Complete() || sid != "s1" || total != 10 {
		t.Fatalf("应冲突回滚: complete=%v owner=%s tokens=%d, want false/s1/10", result.Complete(), sid, total)
	}
}

// 本轮两个新会话互相冲突（同 ID 不同会话、库内无历史）：build 阶段拒绝。
func TestWorkBuddyPersist_IncomingCrossSessionConflict(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	c := fixedResultCollector("workbuddy", collector.CollectResult{
		Messages: []model.Message{
			wbRow("dup", "s1", model.ClientWorkBuddy, 1000, 1),
			wbRow("dup", "s2", model.ClientWorkBuddy, 2000, 2),
		},
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{
			wbPlan("s1", model.ClientWorkBuddy, "p", "t", "/d", false),
			wbPlan("s2", model.ClientWorkBuddy, "p", "t", "/d", false),
		},
	})
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if result.Complete() {
		t.Fatal("本轮两个新会话同 ID 不同归属必须失败")
	}
	var count int
	usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages`).Scan(&count)
	if count != 0 {
		t.Errorf("失败回滚后不应有消息行, got %d", count)
	}
}

// 非 playground 仅历史会话、源库 cwd 为空且无 JSONL：directory 由历史兜底后
// project 按 projectBase(有效目录) 重算，不得以空目录清空历史分类。
func TestWorkBuddyPersist_HistoryDirectoryFallback(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	m := wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10)
	m.Project = "repo"
	db.UpsertMessages(context.Background(), usageDB, []model.Message{m})
	db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/history/repo", Project: "repo", FirstTS: 1000, LastTS: 1000},
	})
	runWBCollect(t, usageDB, collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", model.ClientWorkBuddy, "", "t", "", false)},
	})
	var p, dir, msgProject string
	usageDB.QueryRowContext(context.Background(), "SELECT project, directory FROM sessions WHERE id='s1'").Scan(&p, &dir)
	usageDB.QueryRowContext(context.Background(), "SELECT project FROM messages WHERE session_id='s1'").Scan(&msgProject)
	if p != "repo" || dir != "/history/repo" || msgProject != "repo" {
		t.Fatalf("非 playground 仅历史会话应目录兜底重算: session=(%q,%q) msg=%q", p, dir, msgProject)
	}
}

// 只有 messages、没有 sessions 的历史数据：合并必须补出目标会话行（修复关联）。
func TestWorkBuddyPersist_OrphanSessionCreated(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.UpsertMessages(context.Background(), usageDB, []model.Message{wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10)})
	runWBCollect(t, usageDB, collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", model.ClientWorkBuddy, "old-project", "t", "/old", false)},
	})
	var n int
	usageDB.QueryRowContext(context.Background(), "SELECT count(*) FROM sessions WHERE id='s1'").Scan(&n)
	if n != 1 {
		t.Fatalf("已有消息应补会话, got sessions=%d", n)
	}
}

// 补出的会话时间区间必须涵盖全部历史消息（不只历史 session 行）。
func TestWorkBuddyPersist_OrphanRekeyTimes(t *testing.T) {
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.UpsertMessages(context.Background(), usageDB, []model.Message{
		wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10),
		wbRow("m2", "s1", model.ClientWorkBuddy, 2000, 20),
	})
	runWBCollect(t, usageDB, collector.CollectResult{
		WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", model.WorkBuddyExpertClientKey("A"), "old-project", "t", "/old", false)},
	})
	var first, last int64
	usageDB.QueryRowContext(context.Background(), "SELECT first_ts, last_ts FROM sessions WHERE id='s1'").Scan(&first, &last)
	if first != 1000 || last != 2000 {
		t.Fatalf("会话时间应涵盖历史消息, got %d..%d", first, last)
	}
}

// 最大 ts 并列且目标侧不存在：token/model 整组取 client 键最小副本；当前标题
// 为空且无目标侧时，历史标题按 client 键字节序选择（非标题文本序）。
func TestMergeWorkBuddySession_TieWithoutTargetUsesSmallestClientKey(t *testing.T) {
	a, b, c := model.WorkBuddyExpertClientKey("A"), model.WorkBuddyExpertClientKey("B"), model.WorkBuddyExpertClientKey("C")
	merge := mergeWorkBuddySession(wbPlan("s1", c, "p", "", "/d", false),
		[]model.Message{wbRow("m1", "s1", a, 1000, 10), wbRow("m1", "s1", b, 1000, 20)},
		[]model.Session{{ID: "s1", Client: a, Title: "Z"}, {ID: "s1", Client: b, Title: "A"}})
	if merge.messages[0].TotalTokens != 10 {
		t.Errorf("无目标侧时应按 client 键最小选值, tokens=%d, want 10（expert A）", merge.messages[0].TotalTokens)
	}
	if merge.session.Title != "Z" {
		t.Errorf("历史标题应按 client 键最小选择, got %q, want Z（expert A 的标题）", merge.session.Title)
	}
}

// 完整无日期复核：仅历史会话的无效元数据（is_playground=2）且用量库已有历史
// 行——复核不得报告成功，历史错误不得被清除；下一轮源库修复后自然收敛。
func TestWorkBuddyPersist_InvalidHistoryMetadataDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "source.db")
	s, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, custom_title TEXT, cwd TEXT, source_mode TEXT, mode TEXT, is_playground INTEGER, expert_id TEXT, deleted_at INTEGER);
		INSERT INTO sessions VALUES('s1','t','','/repo','','',2,'',NULL)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": srcPath}},
	}}
	c := collector.NewWorkBuddyCollector(cfg)
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.UpsertMessages(ctx, usageDB, []model.Message{wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10)})
	db.RecordErrorsByDate(ctx, usageDB, []string{"2026-09-01"}, "workbuddy", "workbuddy metadata failed: invalid playground", "")

	deps := &Deps{cfg: cfg, collectors: []collector.Collector{c}}
	result := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	var unresolved int
	usageDB.QueryRowContext(ctx, "SELECT count(*) FROM collection_errors WHERE resolved=0").Scan(&unresolved)
	// 历史 metadata 错误保留（不被复核清除），且本轮失败按合同新增一条可观察
	// 记录（同片段、不同 message 文本）。
	if result.Complete() || unresolved != 2 {
		t.Fatalf("非法历史元数据应暂缓并保留错误: complete=%v unresolved=%d, want false/2", result.Complete(), unresolved)
	}

	// 源库修复（is_playground=0）后复核成功并解决该错误。
	s2, _ := sql.Open("sqlite", srcPath)
	s2.Exec(`UPDATE sessions SET is_playground=0 WHERE id='s1'`)
	s2.Close()
	result2 := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	var unresolved2 int
	usageDB.QueryRowContext(ctx, "SELECT count(*) FROM collection_errors WHERE resolved=0").Scan(&unresolved2)
	if !result2.Complete() || unresolved2 != 0 {
		t.Fatalf("源库修复后复核应成功并解决错误: complete=%v unresolved=%d", result2.Complete(), unresolved2)
	}
}

// 事务写回阶段故障（删除后/写回消息后注入失败）：整轮回滚，不留下半迁移
// （旧行保留、无双侧并存）。覆盖真实执行到删除与写回之后的失败路径。
func TestWorkBuddyPersist_StepFailureRollsBack(t *testing.T) {
	for _, step := range []string{"delete", "insert-messages", "insert-sessions"} {
		t.Run(step, func(t *testing.T) {
			usageDB, _ := db.Open(":memory:")
			defer usageDB.Close()
			expert := model.WorkBuddyExpertClientKey("A")
			db.UpsertMessages(context.Background(), usageDB, []model.Message{wbRow("m1", "s1", model.ClientWorkBuddy, 1000, 10)})
			db.UpsertSessionMeta(context.Background(), usageDB, []model.Session{
				{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/d", Project: "p", Title: "t", FirstTS: 1000, LastTS: 1000},
			})
			workBuddyStepHook = func(s string) error {
				if s == step {
					return fmt.Errorf("injected failure at %s", s)
				}
				return nil
			}
			t.Cleanup(func() { workBuddyStepHook = nil })

			c := fixedResultCollector("workbuddy", collector.CollectResult{
				WorkBuddyPlans: []collector.WorkBuddySessionPlan{wbPlan("s1", expert, "p", "t", "/d", false)},
			})
			result := RunCollect(context.Background(), testDeps(true, c), usageDB,
				collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
			if result.Complete() {
				t.Fatalf("注入 %s 失败必须令整轮失败", step)
			}
			var msgCount int
			var client string
			usageDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE session_id='s1'`).Scan(&msgCount)
			usageDB.QueryRowContext(context.Background(), `SELECT client FROM messages WHERE session_id='s1'`).Scan(&client)
			if msgCount != 1 || client != model.ClientWorkBuddy {
				t.Errorf("回滚后应保留原行: count=%d client=%q, want 1/WorkBuddy", msgCount, client)
			}
		})
	}
}

// runWBCollect 是单计划复核的便捷封装。
func runWBCollect(t *testing.T, usageDB *db.DB, r collector.CollectResult) {
	t.Helper()
	c := fixedResultCollector("workbuddy", r)
	result := RunCollect(context.Background(), testDeps(true, c), usageDB,
		collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if !result.Complete() {
		t.Fatalf("复核应成功: %+v", result)
	}
}

// ---------- GPT 复评（第三轮）复现场景 ----------

// 有 JSONL 触达但源库与文件 cwd 均为空：directory/project 允许为空（不做历史
// 兜底），历史合并、本轮消息、最终 session 与 verify 使用同一目标值，周期
// 复核不反复失败（此前历史兜底被误用于有 JSONL 的会话，合并结果被后续
// session UPSERT 覆盖，verify 每轮失败）。
func TestWorkBuddyPersist_JSONLSessionWithEmptyCwdConverges(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "source.db")
	s, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// 源库有效、cwd 空、非 playground；历史会话目录 /history/repo。
	if _, err := s.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, custom_title TEXT, cwd TEXT, source_mode TEXT, mode TEXT, is_playground INTEGER, expert_id TEXT, deleted_at INTEGER);
		INSERT INTO sessions VALUES('s1','new title','','',NULL,NULL,0,'',NULL)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// JSONL：一条历史消息（无 cwd）+ 一条本轮新消息（无 cwd）。
	emptyCwdLine := func(id string, ts int64) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":""}`,
			id, ts)
	}
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects", "dir")
	os.MkdirAll(projectsDir, 0755)
	os.WriteFile(filepath.Join(projectsDir, "s1.jsonl"),
		[]byte(emptyCwdLine("old", 1000)+"\n"+emptyCwdLine("fresh", 3000)+"\n"), 0644)

	// 用量库既有历史：目录 /history/repo、project=repo。
	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	oldMsg := wbRow("old", "s1", model.ClientWorkBuddy, 1000, 10)
	oldMsg.Project = "repo"
	db.UpsertMessages(ctx, usageDB, []model.Message{oldMsg})
	db.UpsertSessionMeta(ctx, usageDB, []model.Session{
		{ID: "s1", Client: model.ClientWorkBuddy, Directory: "/history/repo", Project: "repo", Title: "old", FirstTS: 1000, LastTS: 1000},
	})

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"projects_dir": filepath.Join(root, "projects"), "db": srcPath}},
	}}
	c := collector.NewWorkBuddyCollector(cfg)
	deps := &Deps{cfg: cfg, collectors: []collector.Collector{c}}
	for round := 0; round < 2; round++ { // 首轮 + 幂等重跑都必须成功
		result := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
		if !result.Complete() {
			t.Fatalf("round %d 应成功收敛（目录空是合法目标）: %+v", round, result)
		}
	}
	var msgCount int
	var dir, proj, title string
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id='s1'`).Scan(&msgCount)
	usageDB.QueryRowContext(ctx, `SELECT directory, project, title FROM sessions WHERE id='s1'`).Scan(&dir, &proj, &title)
	if msgCount != 2 {
		t.Errorf("消息数 = %d, want 2（历史+新消息）", msgCount)
	}
	if dir != "" || proj != "" || title != "new title" {
		t.Errorf("session = (%q,%q,%q), want 空目录/空 project/新标题", dir, proj, title)
	}
}

// 有历史行的无效元数据会话转为部分失败：其余有效会话（如需 expert 迁移与
// 标题刷新）仍正常落库，请求返回失败、不写完成标记、历史错误不被清除，
// 无效会话原数据保留（此前实现整批回滚，阻断了其他会话的周期收敛）。
func TestWorkBuddyPersist_ExcludedHistoryKeepsValidSessions(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "source.db")
	s, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// good：有效、expert=A；bad：is_playground=2（无效）。两会话均无 JSONL。
	if _, err := s.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, custom_title TEXT, cwd TEXT, source_mode TEXT, mode TEXT, is_playground INTEGER, expert_id TEXT, deleted_at INTEGER);
		INSERT INTO sessions VALUES('good','old','','/repo',NULL,NULL,0,'A',NULL);
		INSERT INTO sessions VALUES('bad','t','','/repo',NULL,NULL,2,'',NULL)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	goodOld := wbRow("g1", "good", model.ClientWorkBuddy, 1000, 10)
	db.UpsertMessages(ctx, usageDB, []model.Message{goodOld})
	db.UpsertSessionMeta(ctx, usageDB, []model.Session{
		{ID: "good", Client: model.ClientWorkBuddy, Directory: "/repo", Project: "repo", Title: "old", FirstTS: 1000, LastTS: 1000},
	})
	badOld := wbRow("b1", "bad", model.ClientWorkBuddy, 1000, 20)
	db.UpsertMessages(ctx, usageDB, []model.Message{badOld})
	db.UpsertSessionMeta(ctx, usageDB, []model.Session{
		{ID: "bad", Client: model.ClientWorkBuddy, Directory: "/repo", Project: "repo", Title: "t", FirstTS: 1000, LastTS: 1000},
	})
	db.RecordErrorsByDate(ctx, usageDB, []string{"2026-09-01"}, "workbuddy", "workbuddy metadata failed: previous failure", "")

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": srcPath}},
	}}
	c := collector.NewWorkBuddyCollector(cfg)
	deps := &Deps{cfg: cfg, collectors: []collector.Collector{c}}
	result := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)

	if result.Complete() {
		t.Fatal("存在有历史的无效元数据会话时请求必须失败（不写完成标记）")
	}
	// good 的合法迁移与标题刷新必须已落库（部分失败不回滚成功部分）。
	expertA := model.WorkBuddyExpertClientKey("A")
	var goodClient, goodTitle string
	usageDB.QueryRowContext(ctx, `SELECT client, title FROM sessions WHERE id='good'`).Scan(&goodClient, &goodTitle)
	if goodClient != expertA || goodTitle != "old" {
		t.Errorf("good = (%q,%q), want expert 键/old（迁移+标题保留）", goodClient, goodTitle)
	}
	var goodMsgClient string
	usageDB.QueryRowContext(ctx, `SELECT client FROM messages WHERE session_id='good'`).Scan(&goodMsgClient)
	if goodMsgClient != expertA {
		t.Errorf("good 消息 client = %q, want expert 键", goodMsgClient)
	}
	// bad 原数据保留（client/project 不动），历史错误未被解决。
	var badClient string
	usageDB.QueryRowContext(ctx, `SELECT client FROM sessions WHERE id='bad'`).Scan(&badClient)
	if badClient != model.ClientWorkBuddy {
		t.Errorf("bad 会话 client = %q, want 原值 WorkBuddy（暂缓不动）", badClient)
	}
	var unresolved int
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_errors WHERE resolved=0`).Scan(&unresolved)
	if unresolved < 1 {
		t.Errorf("历史错误不得被清除, unresolved=%d", unresolved)
	}

	// 源库修复 bad 后复核成功，全部错误按片段解决。
	s2, _ := sql.Open("sqlite", srcPath)
	s2.Exec(`UPDATE sessions SET is_playground=0 WHERE id='bad'`)
	s2.Close()
	result2 := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	var unresolved2 int
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_errors WHERE resolved=0`).Scan(&unresolved2)
	if !result2.Complete() || unresolved2 != 0 {
		t.Fatalf("修复后复核应成功并解决错误: complete=%v unresolved=%d", result2.Complete(), unresolved2)
	}
}

// 同一 session ID 双文件、一份失败的端到端：真 collector 采集 + 事务落库后，
// 该会话的历史归属、标题与消息数全部保持原状（整会话暂缓，engine 不刷新）。
func TestWorkBuddyPersist_DuplicateSessionFilesDefersEndToEnd(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("windows/root 下 chmod 000 不产生打开失败")
	}
	ctx := context.Background()
	root := t.TempDir()
	// writeWBGateMetaDB 返回相对文件名，必须与 root 拼接成绝对路径再使用。
	srcPath := filepath.Join(root, writeWBGateMetaDB(t, root))
	// 源库把 sess-001 标记为 expert=A（若被错误刷新，client 会变化）。
	s, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`UPDATE sessions SET expert_id='A', title='new title' WHERE id='sess-001'`); err != nil {
		t.Fatalf("更新源库元数据失败: %v", err)
	}
	s.Close()

	line := func(id string, ts int64) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":"/repo"}`, id, ts)
	}
	for _, dir := range []string{"bad", "dir"} {
		os.MkdirAll(filepath.Join(root, "projects", dir), 0755)
		os.WriteFile(filepath.Join(root, "projects", dir, "sess-001.jsonl"),
			[]byte(line("old1", 1000)+"\n"+line("old2", 2000)+"\n"), 0644)
	}
	os.Chmod(filepath.Join(root, "projects", "bad", "sess-001.jsonl"), 0)

	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	db.UpsertMessages(ctx, usageDB, []model.Message{wbRow("old1", "sess-001", model.ClientWorkBuddy, 1000, 10)})

	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": srcPath, "projects_dir": filepath.Join(root, "projects")}},
	}}
	c := collector.NewWorkBuddyCollector(cfg)
	deps := &Deps{cfg: cfg, collectors: []collector.Collector{c}}
	result := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)
	if result.Complete() {
		t.Fatal("部分失败必须令请求失败")
	}
	// 失败必须来自 bad JSONL 文件的部分读取（证明元数据读取成功、双文件
	// 处理分支真正执行），而不是源库问题。
	if result.Err == nil || !strings.Contains(result.Err.Error(), filepath.Join("bad", "sess-001.jsonl")) {
		t.Fatalf("失败应来自 bad/sess-001.jsonl 的文件级读取: %+v", result.Err)
	}
	var msgCount, sessCount int
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id='sess-001'`).Scan(&msgCount)
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id='sess-001'`).Scan(&sessCount)
	if msgCount != 1 || sessCount != 0 {
		t.Errorf("整会话暂缓后库应保持原状: msgs=%d sessions=%d, want 1/0（不创建 session 行、不新增消息）", msgCount, sessCount)
	}
}

// 同 session 双文件矛盾目标的端到端：真 collector 在采集层暂缓矛盾会话，
// 同批 healthy 会话的消息正常落库（此前矛盾计划进入 verify 令整批回滚，
// healthy 也被阻断）。
func TestWorkBuddyPersist_ConflictingTargetsDoNotBlockHealthy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	srcPath := filepath.Join(root, writeWBGateMetaDB(t, root))
	s, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// sess-001 源库 cwd 为空（制造 fileCwd 依赖）；healthy 正常。
	if _, err := s.Exec(`UPDATE sessions SET cwd='' WHERE id='sess-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`INSERT INTO sessions (id, cwd, user_id, is_playground, created_at, updated_at)
		VALUES ('healthy', '/repo', 'u', 0, 1749312000, 1749312000)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	line := func(id string, ts int64, cwd string) string {
		return fmt.Sprintf(`{"id":"%s","timestamp":%d,"role":"assistant","providerData":{"model":"m","usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150,"inputTokensDetails":[{"cached_tokens":0}]}},"sessionId":"s","cwd":%q}`, id, ts, cwd)
	}
	for dir, content := range map[string]string{
		"a": line("m0", 1000, "/a") + "\n",
		"b": line("m1", 2000, "/b") + "\n",
		"c": line("h0", 1500, "/repo") + "\n",
	} {
		os.MkdirAll(filepath.Join(root, "projects", dir), 0755)
		os.WriteFile(filepath.Join(root, "projects", dir, map[string]string{"a": "sess-001", "b": "sess-001", "c": "healthy"}[dir]+".jsonl"), []byte(content), 0644)
	}

	usageDB, _ := db.Open(":memory:")
	defer usageDB.Close()
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": srcPath, "projects_dir": filepath.Join(root, "projects")}},
	}}
	c := collector.NewWorkBuddyCollector(cfg)
	deps := &Deps{cfg: cfg, collectors: []collector.Collector{c}}
	result := RunCollect(ctx, deps, usageDB, collectTestLogger(), io.Discard, "workbuddy", collector.CollectRequest{}, true, false)

	if result.Complete() {
		t.Fatal("矛盾会话暂缓应令请求部分失败")
	}
	var healthyMsgs, conflictMsgs int
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id='healthy'`).Scan(&healthyMsgs)
	usageDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id='sess-001'`).Scan(&conflictMsgs)
	if healthyMsgs != 1 {
		t.Errorf("healthy 消息应正常落库, got %d", healthyMsgs)
	}
	if conflictMsgs != 0 {
		t.Errorf("矛盾会话不应写入, got %d", conflictMsgs)
	}
}
