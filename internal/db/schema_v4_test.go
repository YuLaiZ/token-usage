package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v4 升级合同（mimocode 落库名改名 + 旧名回写兼容 trigger） ===
//
// 合同：currentSchemaVersion=4；migrateV4 单事务执行 v4MigrationStatements
//（migration-local 冻结字面量，冲突合并语义冻结自 v0.1.10 发布时的 DAO
// upsert 合同，不随当前 DAO 演进）——① messages/sessions 的 legacy 名行
//（"Xiaomi MiMo / MiMo Code"，v0.1.10 及之前写入）按该语义折叠改名为
// "MiMo Code"（同 id 新旧行并存时确定性合并为一条，不报主键冲突、token 不
// 双计）；② 删除 legacy 行；③ 创建 BEFORE INSERT 兼容 trigger（旧版二进制
// 回滚后仍写旧名时改写为新名 upsert，RAISE(IGNORE) 放弃原语句）；④ 最后
// PRAGMA user_version=4。任一步失败整体回滚：库保持 v3、数据原样、trigger
// 不残留、版本号不前进。

const (
	v4TriggerMessages = "messages_legacy_mimo_client_rewrite"
	v4TriggerSessions = "sessions_legacy_mimo_client_rewrite"
)

// v4RowsByClient 返回 table 中 client 精确等于 client 的行数。
func v4RowsByClient(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}, table, client string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE client=?", table), client).Scan(&n); err != nil {
		t.Fatalf("count %s client=%q: %v", table, client, err)
	}
	return n
}

// v4TriggerExists 判断指定 trigger 是否已创建。
func v4TriggerExists(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}, name string) bool {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?", name).Scan(&n); err != nil {
		t.Fatalf("query trigger %s: %v", name, err)
	}
	return n > 0
}

// TestFreshDBReachesV4：全新库 Open 后 user_version=4 且两张兼容 trigger 已
// 创建（从第一笔写入起旧名回写即被改写；无 legacy 行，折叠为空集）。
func TestFreshDBReachesV4(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != 4 {
		t.Fatalf("fresh DB user_version = %d, want 4", got)
	}
	if !v4TriggerExists(t, d, v4TriggerMessages) {
		t.Fatal("fresh DB 应含 messages 兼容 trigger")
	}
	if !v4TriggerExists(t, d, v4TriggerSessions) {
		t.Fatal("fresh DB 应含 sessions 兼容 trigger")
	}
}

// TestV3UpgradeRenamesLegacyMimoRows：v3 存量库升级后 legacy 行整体改名，全部
// 业务字段（token/时间/目录/项目/标题）逐字段保留；近似拼写不被误改；其他
// client 不受影响；sessions↔messages JOIN 配对不因改名失配；全库无 legacy 名。
func TestV3UpgradeRenamesLegacyMimoRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer upgraded.Close()
	if got := userVersion(t, upgraded); got != 4 {
		t.Fatalf("upgraded user_version = %d, want 4", got)
	}

	// messages：两条 legacy 行改名且字段逐项保留。
	var (
		id, sessionID, client, date, directory, project, mModel, provider  string
		ts, input, fresh, output, cacheRead, cacheCreate, reasoning, total int64
	)
	if err := upgraded.QueryRow(
		`SELECT id,session_id,client,date,ts,directory,project,model,provider,
		        input_tokens,fresh_input_tokens,output_tokens,cache_read_tokens,
		        cache_create_tokens,reasoning_tokens,total_tokens
		 FROM messages WHERE id='m-old-1'`).Scan(
		&id, &sessionID, &client, &date, &ts, &directory, &project, &mModel, &provider,
		&input, &fresh, &output, &cacheRead, &cacheCreate, &reasoning, &total,
	); err != nil {
		t.Fatalf("query m-old-1: %v", err)
	}
	if client != model.ClientMiMoCode {
		t.Errorf("m-old-1 client = %q, want %q", client, model.ClientMiMoCode)
	}
	if sessionID != "ses_mc" || date != "2026-09-18" || ts != 1000 ||
		directory != "/m" || project != "proj-M" || mModel != "mimo-pro" || provider != "Xiaomi" ||
		input != 10 || fresh != 10 || output != 5 || cacheRead != 2 || cacheCreate != 0 || reasoning != 1 || total != 17 {
		t.Errorf("m-old-1 字段应逐项保留: sid=%q date=%q ts=%d dir=%q proj=%q model=%q prov=%q in=%d fresh=%d out=%d cr=%d cc=%d r=%d total=%d",
			sessionID, date, ts, directory, project, mModel, provider, input, fresh, output, cacheRead, cacheCreate, reasoning, total)
	}

	// sessions：legacy 行改名且元数据保留。
	var sTitle string
	var sFirst, sLast int64
	if err := upgraded.QueryRow(
		`SELECT client,title,first_ts,last_ts FROM sessions WHERE id='ses_mc'`).Scan(&client, &sTitle, &sFirst, &sLast); err != nil {
		t.Fatalf("query ses_mc: %v", err)
	}
	if client != model.ClientMiMoCode || sTitle != "mc-title" || sFirst != 1000 || sLast != 2000 {
		t.Errorf("ses_mc 应改名且元数据保留: client=%q title=%q first=%d last=%d", client, sTitle, sFirst, sLast)
	}

	// 全库无 legacy 名；近似拼写与其他 client 不受影响。
	for _, table := range []string{"messages", "sessions"} {
		if n := v4RowsByClient(t, upgraded, table, model.LegacyClientXiaomiMiMoCode); n != 0 {
			t.Fatalf("%s 升级后残留 legacy 名 %d 行", table, n)
		}
		if n := v4RowsByClient(t, upgraded, table, "Xiaomi MiMo"); n != 1 {
			t.Fatalf("%s 近似拼写 'Xiaomi MiMo' 行数 = %d, want 1（精确匹配不误伤）", table, n)
		}
		if n := v4RowsByClient(t, upgraded, table, "Claude Code"); n != 1 {
			t.Fatalf("%s Claude Code 行数 = %d, want 1（不受迁移影响）", table, n)
		}
	}

	// JOIN 配对不因改名失配。
	var joined int
	if err := upgraded.QueryRow(
		`SELECT COUNT(*) FROM sessions s JOIN messages m ON m.session_id=s.id AND m.client=s.client`,
	).Scan(&joined); err != nil {
		t.Fatal(err)
	}
	// 配对行：ses_mc(2 条 legacy 改名) + ses_cl(1) + ses_mc2(1 新名) + ses_xm(1)。
	if joined != 5 {
		t.Fatalf("sessions↔messages JOIN 行数 = %d, want 5", joined)
	}
}

// TestV3UpgradeMergesIDCollision：同 id 的 legacy 行与新名行并存时，折叠改名
// 不报主键冲突，按 DAO upsert 语义确定性合并为一条新名行：归因字段取早 ts
// 版本、token/model/provider 以 legacy 行（excluded）覆盖、router_* 非空才
// 覆盖；最终该 id 只有一条 MiMo Code 行，token 不重复计数。
func TestV3UpgradeMergesIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3-collide.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	// 追加同 id 冲突对：新名行 ts 更晚/目录不同，legacy 行 ts 更早。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO messages (id,session_id,client,date,ts,model,provider,directory,project,total_tokens) VALUES
			('m-dup','ses_mc','MiMo Code','2026-09-20',9000,'mimo-pro','Xiaomi','/new','P-new',100),
			('m-dup','ses_mc','` + model.LegacyClientXiaomiMiMoCode + `','2026-09-19',8000,'mimo-pro-2','Xiaomi','/old','P-old',77)`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed collision rows: %v", err)
		}
	}
	raw.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 collide db: %v", err)
	}
	defer upgraded.Close()

	var n int
	if err := upgraded.QueryRow("SELECT COUNT(*) FROM messages WHERE id='m-dup'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("冲突 id 合并后应恰 1 行,实际 %d", n)
	}
	var client, directory, project, mModel string
	var ts, total int64
	if err := upgraded.QueryRow(
		`SELECT client,ts,directory,project,model,total_tokens FROM messages WHERE id='m-dup'`).Scan(
		&client, &ts, &directory, &project, &mModel, &total); err != nil {
		t.Fatal(err)
	}
	// 合并规则=DAO upsert：legacy 行作为 excluded 参与冲突更新——早 ts 归因字段
	// 取 legacy 行（ts=8000<9000），token/model 以 legacy 行值覆盖。
	if client != model.ClientMiMoCode || ts != 8000 || directory != "/old" || project != "P-old" || mModel != "mimo-pro-2" || total != 77 {
		t.Errorf("冲突合并结果应符合 DAO upsert 语义: client=%q ts=%d dir=%q proj=%q model=%q total=%d", client, ts, directory, project, mModel, total)
	}
	// token 不双计：m-dup 只计 77，不是 100+77。
	if n := v4RowsByClient(t, upgraded, "messages", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("冲突合并后 legacy 残留 %d 行", n)
	}
}

// TestV3UpgradeMergesSessionIDCollision：sessions 的直接迁移碰撞用例——同一
// session id 的 legacy 行与 canonical 行并存时，折叠改名按 v0.1.10 session
// upsert 语义确定性合并：directory/project/parent_id 按 excluded（legacy 行）
// 覆盖、legacy 空 title 保留 canonical 非空 title、first_ts 取最早非零、
// last_ts 取最大；最终一条 MiMo Code 会话，legacy 行删除，且与迁移后的
// messages 仍按 session_id+client JOIN 配对。
func TestV3UpgradeMergesSessionIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3-sess-collide.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		// canonical 行 title 非空、first/last 区间更晚；legacy 行 title 空、
		// 区间更早且 last_ts 更小（验证两侧收窄方向）。
		`INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts) VALUES
			('ses-dup','MiMo Code','/new','P-new','canonical-title','p1',5000,6000),
			('ses-dup','` + model.LegacyClientXiaomiMiMoCode + `','/old','P-old','','p2',3000,4000)`,
		// 碰撞会话的一条 legacy 消息（迁移后 client=MiMo Code，供 JOIN 断言）。
		`INSERT INTO messages (id,session_id,client,date,ts,total_tokens) VALUES
			('m-sd','ses-dup','` + model.LegacyClientXiaomiMiMoCode + `','2026-09-20',7000,80)`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed session collision rows: %v", err)
		}
	}
	raw.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 session collide db: %v", err)
	}
	defer upgraded.Close()

	var n int
	if err := upgraded.QueryRow("SELECT COUNT(*) FROM sessions WHERE id='ses-dup'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("碰撞 session 合并后应恰 1 行,实际 %d", n)
	}
	var client, directory, project, title, parentID string
	var first, last int64
	if err := upgraded.QueryRow(
		`SELECT client,directory,project,title,parent_id,first_ts,last_ts FROM sessions WHERE id='ses-dup'`).Scan(
		&client, &directory, &project, &title, &parentID, &first, &last); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoCode {
		t.Errorf("合并行 client = %q, want %q", client, model.ClientMiMoCode)
	}
	// directory/project/parent_id 直接以 excluded（legacy 行）覆盖。
	if directory != "/old" || project != "P-old" || parentID != "p2" {
		t.Errorf("directory/project/parent_id 应按 legacy 行覆盖: dir=%q proj=%q parent=%q", directory, project, parentID)
	}
	// legacy 空 title 不清掉 canonical 非空 title。
	if title != "canonical-title" {
		t.Errorf("title 应保留 canonical 非空值: %q", title)
	}
	// first_ts 收窄到最早非零（3000），last_ts 取最大（6000）。
	if first != 3000 || last != 6000 {
		t.Errorf("first/last 应区间收窄: first=%d last=%d, want 3000/6000", first, last)
	}
	if n := v4RowsByClient(t, upgraded, "sessions", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("sessions 合并后 legacy 残留 %d 行", n)
	}
	// 碰撞会话的 legacy 消息已随迁移改名，与合并后会话按 session_id+client 配对。
	var joined int
	if err := upgraded.QueryRow(
		`SELECT COUNT(*) FROM sessions s JOIN messages m ON m.session_id=s.id AND m.client=s.client WHERE s.id='ses-dup'`).Scan(&joined); err != nil {
		t.Fatal(err)
	}
	if joined != 1 {
		t.Fatalf("ses-dup 迁移后 JOIN 配对 = %d, want 1", joined)
	}
}

// TestMigrateV4FailureKeepsV3：迁移中段（数据折叠与 trigger 创建完成之后、
// user_version 之前）注入失败，整个事务回滚——库保持 v3：数据原样、两张
// trigger 均不残留（半套 trigger 不存在）、版本号不前进；清除注入后重试成功。
func TestMigrateV4FailureKeepsV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail4.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	migrateV4PostTriggerHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV4PostTriggerHook = nil })

	if err := migrateV4(raw); err == nil {
		t.Fatal("migrateV4 应因中段注入失败")
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Fatalf("失败后 user_version = %d, want 3", v)
	}
	for _, name := range []string{v4TriggerMessages, v4TriggerSessions} {
		if v4TriggerExists(t, raw, name) {
			t.Fatalf("失败后 trigger %s 不应残留（随事务回滚）", name)
		}
	}
	if n := v4RowsByClient(t, raw, "messages", model.LegacyClientXiaomiMiMoCode); n != 2 {
		t.Fatalf("失败后 messages legacy 行应原样 %d 行,实际 %d", 2, n)
	}
	if n := v4RowsByClient(t, raw, "sessions", model.LegacyClientXiaomiMiMoCode); n != 1 {
		t.Fatalf("失败后 sessions legacy 行应原样 %d 行,实际 %d", 1, n)
	}

	// 清除注入后重试成功，trigger 与改名全部到位。
	migrateV4PostTriggerHook = nil
	if err := migrateV4(raw); err != nil {
		t.Fatalf("重试 migrateV4 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Fatalf("重试后 user_version = %d, want 4", v)
	}
	if !v4TriggerExists(t, raw, v4TriggerMessages) || !v4TriggerExists(t, raw, v4TriggerSessions) {
		t.Fatal("重试后两张兼容 trigger 应存在")
	}
	if n := v4RowsByClient(t, raw, "messages", model.ClientMiMoCode); n != 3 {
		t.Fatalf("重试后 messages MiMo Code 行数 = %d, want 3（2 legacy 改名 + 1 原新名）", n)
	}
}

// TestLegacyWriteAfterMigrationRewrittenByTrigger：最重要的回滚兼容回归——
// migration 完成后，模拟旧版二进制用 legacy client 走生产 DAO（UpsertMessages/
// UpsertSessionMeta 的 SQL 与旧版一致，仅 client 值为 legacy）写入与重复
// upsert：库内只出现新名行、同 id 不形成两份、messages/sessions 的冲突更新
// 语义未被 trigger 改坏、重复执行幂等。
func TestLegacyWriteAfterMigrationRewrittenByTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	if err := sqlDumpV3ForMimoRename(t, path); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer d.Close()
	if got := userVersion(t, d); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}

	// 场景 A：旧版写入全新消息（legacy client 值）→ trigger 改写为新名入库。
	legacyMsg := model.Message{
		ID: "m-legacy-new", SessionID: "ses_mc", Client: model.LegacyClientXiaomiMiMoCode,
		Date: "2026-09-21", TS: 6000, Model: "mimo-pro", Provider: "Xiaomi",
		Directory: "/m", Project: "proj-M",
		InputTokens: 11, FreshInputTokens: 11, OutputTokens: 6,
		CacheReadTokens: 3, TotalTokens: 20,
	}
	if _, err := UpsertMessages(context.Background(), d, []model.Message{legacyMsg}); err != nil {
		t.Fatalf("旧名 upsert 不应报错: %v", err)
	}
	if n := v4RowsByClient(t, d, "messages", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("trigger 后 messages 不应出现 legacy 行,实际 %d", n)
	}
	if n := v4RowsByClient(t, d, "messages", model.ClientMiMoCode); n != 4 {
		t.Fatalf("新名行应含被改写的 m-legacy-new,实际 %d 行", n)
	}

	// 场景 B：旧版对既有 canonical 行重复 upsert（不同 token、更早 ts、非空
	// router_*）→ 冲突更新语义与 DAO 一致：早 ts 归因、token 覆盖、router_*
	// 非空才覆盖；行数不增。
	upsertAgain := legacyMsg
	upsertAgain.TS = 500 // 早于 m-legacy-new 的 6000
	upsertAgain.Directory = "/earlier"
	upsertAgain.TotalTokens = 99
	upsertAgain.RouterProvider = "cc_switch"
	if _, err := UpsertMessages(context.Background(), d, []model.Message{upsertAgain}); err != nil {
		t.Fatalf("旧名重复 upsert 不应报错: %v", err)
	}
	var client, directory, routerProvider string
	var ts, total int64
	if err := d.QueryRow(
		`SELECT client,ts,directory,router_provider,total_tokens FROM messages WHERE id='m-legacy-new'`).Scan(
		&client, &ts, &directory, &routerProvider, &total); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoCode {
		t.Errorf("canonical 行 client = %q, want %q", client, model.ClientMiMoCode)
	}
	if ts != 500 || directory != "/earlier" {
		t.Errorf("早 ts 归因字段应取 excluded 版本: ts=%d directory=%q", ts, directory)
	}
	if total != 99 {
		t.Errorf("token 应以旧名 upsert 值覆盖: total=%d, want 99", total)
	}
	if routerProvider != "cc_switch" {
		t.Errorf("非空 router_provider 应覆盖: %q", routerProvider)
	}
	if n := v4RowsByClient(t, d, "messages", model.ClientMiMoCode); n != 4 {
		t.Fatalf("重复 upsert 行数不应增加,实际 %d", n)
	}

	// 场景 C：空 router_* 不清除既有值（DAO 语义保留）。
	upsertThird := legacyMsg
	upsertThird.TS = 400
	upsertThird.RouterProvider = ""
	if _, err := UpsertMessages(context.Background(), d, []model.Message{upsertThird}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(
		`SELECT router_provider FROM messages WHERE id='m-legacy-new'`).Scan(&routerProvider); err != nil {
		t.Fatal(err)
	}
	if routerProvider != "cc_switch" {
		t.Errorf("空 router_provider 不应清除既有值: %q", routerProvider)
	}

	// 场景 D：sessions 的 legacy 写入同样被 trigger 改写，且 title 非空保留、
	// first_ts/last_ts 区间收窄语义未被改坏。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "ses-legacy-new", Client: model.LegacyClientXiaomiMiMoCode,
		Directory: "/m", Project: "proj-M", Title: "legacy-title", FirstTS: 100, LastTS: 900,
	}}); err != nil {
		t.Fatalf("旧名 session upsert 不应报错: %v", err)
	}
	if n := v4RowsByClient(t, d, "sessions", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("trigger 后 sessions 不应出现 legacy 行,实际 %d", n)
	}
	// 旧名重复 upsert：title 空（保留既有）、first_ts 更晚（不前移）、last_ts 更早（不回缩）。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "ses-legacy-new", Client: model.LegacyClientXiaomiMiMoCode,
		Directory: "/m2", Project: "proj-M2", Title: "", FirstTS: 300, LastTS: 500,
	}}); err != nil {
		t.Fatal(err)
	}
	var title string
	var first, last int64
	if err := d.QueryRow(
		`SELECT client,title,first_ts,last_ts FROM sessions WHERE id='ses-legacy-new'`).Scan(
		&client, &title, &first, &last); err != nil {
		t.Fatal(err)
	}
	if client != model.ClientMiMoCode {
		t.Errorf("session canonical 行 client = %q, want %q", client, model.ClientMiMoCode)
	}
	if title != "legacy-title" || first != 100 || last != 900 {
		t.Errorf("session 冲突语义应保留 title/区间: title=%q first=%d last=%d", title, first, last)
	}

	// 场景 E：同一批旧名写入重复执行（collect 全量重放的幂等形态）——行数不变。
	before := v4RowsByClient(t, d, "messages", model.ClientMiMoCode)
	for i := 0; i < 2; i++ {
		if _, err := UpsertMessages(context.Background(), d, []model.Message{legacyMsg}); err != nil {
			t.Fatal(err)
		}
		if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
			ID: "ses-legacy-new", Client: model.LegacyClientXiaomiMiMoCode,
			Directory: "/m", Project: "proj-M", Title: "legacy-title", FirstTS: 100, LastTS: 900,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if after := v4RowsByClient(t, d, "messages", model.ClientMiMoCode); after != before {
		t.Fatalf("重复旧名写入应幂等: before=%d after=%d", before, after)
	}
	if n := v4RowsByClient(t, d, "messages", model.LegacyClientXiaomiMiMoCode); n != 0 {
		t.Fatalf("幂等重放后 legacy 行 = %d, want 0", n)
	}
}

// TestV4MigrationStatementsFrozenStructure：锁定 v4 迁移语句数组的冻结形态——
// 历史迁移不可变（合同冻结自 v0.1.10 DAO upsert），不得回退为运行时解析当前
// DAO 常量生成；本测试断言其结构不变量（语句数、顺序、折叠/删除/trigger
// 形态、WHEN/RAISE 特征），行为正确性由迁移/冲突/回滚兼容测试锁定。
func TestV4MigrationStatementsFrozenStructure(t *testing.T) {
	if len(v4MigrationStatements) != 6 {
		t.Fatalf("v4MigrationStatements 条数 = %d, want 6（messages 折叠/删除 + sessions 折叠/删除 + 双 trigger）", len(v4MigrationStatements))
	}
	// 顺序：两表各「折叠 INSERT..SELECT..ON CONFLICT → DELETE」→ 双 trigger。
	for i, want := range []struct {
		prefix, contains string
	}{
		{"INSERT INTO messages", "SELECT id, session_id, 'MiMo Code'"},
		{"DELETE FROM messages", "WHERE client = 'Xiaomi MiMo / MiMo Code'"},
		{"INSERT INTO sessions", "SELECT id, 'MiMo Code'"},
		{"DELETE FROM sessions", "WHERE client = 'Xiaomi MiMo / MiMo Code'"},
		{"CREATE TRIGGER IF NOT EXISTS messages_legacy_mimo_client_rewrite", "SELECT RAISE(IGNORE)"},
		{"CREATE TRIGGER IF NOT EXISTS sessions_legacy_mimo_client_rewrite", "SELECT RAISE(IGNORE)"},
	} {
		stmt := v4MigrationStatements[i]
		if !strings.HasPrefix(stmt, want.prefix) {
			t.Errorf("v4 语句[%d] 应以 %q 开头, 实际以 %q 开头", i, want.prefix, stmt[:min(len(stmt), len(want.prefix))])
		}
		if !strings.Contains(stmt, want.contains) {
			t.Errorf("v4 语句[%d] 应含 %q", i, want.contains)
		}
	}
	// 冻结合同特征：v0.1.10 DAO 的冲突合并语义关键片段必须原样存在。
	for i, frag := range []string{
		"excluded.ts < messages.ts THEN excluded.ts ELSE messages.ts", // 早 ts 归因
		"excluded.router_provider != ''",                              // router_* 非空才覆盖
		"excluded.title<>'' THEN excluded.title ELSE sessions.title",  // title 非空保留
		"excluded.first_ts>0 AND excluded.first_ts<sessions.first_ts", // first_ts 收窄
		"WHEN NEW.client = 'Xiaomi MiMo / MiMo Code'",                 // trigger 触发条件
	} {
		var found bool
		for _, stmt := range v4MigrationStatements {
			if strings.Contains(stmt, frag) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("v4 冻结语句应含 v0.1.10 合同片段[%d]: %q", i, frag)
		}
	}
	// trigger NEW 引用数（含 WHEN 条件的 NEW.client）：messages 19 列
	//（INSERT VALUES 18 个 NEW + client 位字面量 + WHEN 1 个）、sessions 8 列
	//（7 + WHEN 1 个）。
	if got := strings.Count(v4MigrationStatements[4], "NEW."); got != 19 {
		t.Errorf("messages trigger NEW 引用数 = %d, want 19", got)
	}
	if got := strings.Count(v4MigrationStatements[5], "NEW."); got != 8 {
		t.Errorf("sessions trigger NEW 引用数 = %d, want 8", got)
	}
}

// sqlDumpV3ForMimoRename 生成 v3 门控存量库：全列 messages/sessions（migrateV4
// 折叠与 trigger 引用全部列）+ legacy 名行（2 消息 + 1 会话）、既有新名行、
// Claude 行、近似拼写行，user_version=3（Open 只会再跑 migrateV4）。
func sqlDumpV3ForMimoRename(t *testing.T, path string) error {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer raw.Close()
	stmts := []string{
		`CREATE TABLE messages (
			id                  TEXT NOT NULL,
			session_id          TEXT NOT NULL,
			client              TEXT NOT NULL,
			date                TEXT NOT NULL,
			ts                  INTEGER NOT NULL,
			model               TEXT NOT NULL DEFAULT '',
			provider            TEXT NOT NULL DEFAULT '',
			router_provider     TEXT NOT NULL DEFAULT '',
			router_model        TEXT NOT NULL DEFAULT '',
			router_name         TEXT NOT NULL DEFAULT '',
			directory           TEXT NOT NULL DEFAULT '',
			project             TEXT NOT NULL DEFAULT '',
			input_tokens        INTEGER NOT NULL DEFAULT 0,
			fresh_input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens       INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_create_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
			total_tokens        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (client, id)
		)`,
		`CREATE TABLE sessions (
			id        TEXT NOT NULL,
			client    TEXT NOT NULL,
			directory TEXT NOT NULL DEFAULT '',
			project   TEXT NOT NULL DEFAULT '',
			title     TEXT NOT NULL DEFAULT '',
			parent_id TEXT NOT NULL DEFAULT '',
			first_ts  INTEGER NOT NULL DEFAULT 0,
			last_ts   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (id, client)
		)`,
		`INSERT INTO messages (id,session_id,client,date,ts,model,provider,directory,project,
			input_tokens,fresh_input_tokens,output_tokens,cache_read_tokens,cache_create_tokens,reasoning_tokens,total_tokens) VALUES
			('m-old-1','ses_mc','` + model.LegacyClientXiaomiMiMoCode + `','2026-09-18',1000,'mimo-pro','Xiaomi','/m','proj-M',10,10,5,2,0,1,17),
			('m-old-2','ses_mc','` + model.LegacyClientXiaomiMiMoCode + `','2026-09-19',2000,'mimo-pro','Xiaomi','/m','proj-M',20,20,6,3,0,1,30),
			('m-cl-1','ses_cl','Claude Code','2026-09-18',3000,'claude-sonnet-4','Anthropic','/c','proj-C',0,0,0,0,0,0,40),
			('m-nm-1','ses_mc2','MiMo Code','2026-09-19',4000,'mimo-pro','Xiaomi','/m','proj-M',0,0,0,0,0,0,60),
			('m-xm-1','ses_xm','Xiaomi MiMo','2026-09-19',5000,'mimo-pro','Xiaomi','/x','proj-X',0,0,0,0,0,0,70)`,
		`INSERT INTO sessions (id,client,directory,project,title,first_ts,last_ts) VALUES
			('ses_mc','` + model.LegacyClientXiaomiMiMoCode + `','/m','proj-M','mc-title',1000,2000),
			('ses_cl','Claude Code','/c','proj-C','cl-title',3000,3000),
			('ses_mc2','MiMo Code','/m','proj-M','nm-title',4000,4000),
			('ses_xm','Xiaomi MiMo','/x','proj-X','xm-title',5000,5000)`,
		`PRAGMA user_version = 3`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
