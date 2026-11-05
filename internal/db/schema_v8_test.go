package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === schema v8 升级合同（messages/sessions.agent_parent_id） ===
//
// 合同：currentSchemaVersion=8；migrateV8 单事务为 messages 与 sessions 各加
// agent_parent_id TEXT NOT NULL DEFAULT 空字符串一列并建索引
// （idx_messages_agent_parent / idx_sessions_agent_parent），最后
// PRAGMA user_version=8。存量行保持空字符串（主会话/未知），不做数据回填
// （重采自然补齐）。任一步失败整体回滚保持 v7。v8 语句为 migration-local
// 冻结字面量，不随运行期 DAO 演进。

// messagesAgentParentColumn 查询 messages 表 agent_parent_id 列的默认值。
func messagesAgentParentColumn(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) (exists bool, dflt string) {
	t.Helper()
	var d string
	err := q.QueryRow(`SELECT COALESCE(dflt_value,'') FROM pragma_table_info('messages') WHERE name='agent_parent_id'`).Scan(&d)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return false, ""
		}
		t.Fatalf("query table_info(messages): %v", err)
	}
	return true, d
}

// messagesAgentParentIndexExists 查询 idx_messages_agent_parent 索引是否存在。
func messagesAgentParentIndexExists(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) bool {
	t.Helper()
	var name string
	err := q.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_messages_agent_parent'`).Scan(&name)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return false
		}
		t.Fatalf("query sqlite_master: %v", err)
	}
	return name == "idx_messages_agent_parent"
}

// sessionsAgentParentColumn 查询 sessions 表 agent_parent_id 列的默认值。
func sessionsAgentParentColumn(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) (exists bool, dflt string) {
	t.Helper()
	var d string
	err := q.QueryRow(`SELECT COALESCE(dflt_value,'') FROM pragma_table_info('sessions') WHERE name='agent_parent_id'`).Scan(&d)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return false, ""
		}
		t.Fatalf("query table_info(sessions): %v", err)
	}
	return true, d
}

// sessionsAgentParentIndexExists 查询 idx_sessions_agent_parent 索引是否存在。
func sessionsAgentParentIndexExists(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) bool {
	t.Helper()
	var name string
	err := q.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sessions_agent_parent'`).Scan(&name)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return false
		}
		t.Fatalf("query sqlite_master: %v", err)
	}
	return name == "idx_sessions_agent_parent"
}

// TestFreshDBReachesV8：全新库 Open 后 user_version=8、agent_parent_id 列就绪
// （messages 与 sessions 双侧；默认空字符串）、索引就绪；不写该列的新行取
// 空字符串。
func TestFreshDBReachesV8(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	if got := userVersion(t, d); got != currentSchemaVersion {
		t.Fatalf("fresh DB user_version = %d, want %d", got, currentSchemaVersion)
	}
	exists, dflt := messagesAgentParentColumn(t, d)
	if !exists {
		t.Fatal("fresh DB messages 应含 agent_parent_id 列")
	}
	if dflt != "''" {
		t.Fatalf("messages.agent_parent_id 默认值应为空字符串, 实际 %q", dflt)
	}
	if !messagesAgentParentIndexExists(t, d) {
		t.Fatal("fresh DB 应含 idx_messages_agent_parent 索引")
	}
	sExists, sDflt := sessionsAgentParentColumn(t, d)
	if !sExists {
		t.Fatal("fresh DB sessions 应含 agent_parent_id 列（家族关系权威来源）")
	}
	if sDflt != "''" {
		t.Fatalf("sessions.agent_parent_id 默认值应为空字符串, 实际 %q", sDflt)
	}
	if !sessionsAgentParentIndexExists(t, d) {
		t.Fatal("fresh DB 应含 idx_sessions_agent_parent 索引")
	}
}

// buildV7RawDB 造一个停在 v7 的原始库（模拟旧版二进制写入的存量库）。
func buildV7RawDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := buildV6RawDB(t)
	if err := migrateV7(raw); err != nil {
		t.Fatalf("seed migrateV7: %v", err)
	}
	return raw
}

// TestV7UpgradePreservesData：v7 库升级到 v8 后存量行零数据变化
// （agent_parent_id 全空串），token 列原样保留。
func TestV7UpgradePreservesData(t *testing.T) {
	raw := buildV7RawDB(t)
	defer raw.Close()
	// 存量行按 v7 布局（无 agent_parent_id 列）预置：messages 与 sessions 各一行。
	if _, err := raw.Exec(`INSERT INTO messages (id,session_id,client,date,ts,model,provider,
input_tokens,fresh_input_tokens,output_tokens,total_tokens,duration_ms)
VALUES ('m1','s1',?,'2026-11-05',1000,'ccs-opencode/deepseek-v4.1-flash','OpenAI',10,10,20,30,4200)`,
		model.ClientCodexCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO sessions (id,client,directory,project,title,parent_id,first_ts,last_ts)
VALUES ('s1',?,'/d','p','t','',1000,2000)`, model.ClientCodexCLI); err != nil {
		t.Fatal(err)
	}
	if err := migrateV8(raw); err != nil {
		t.Fatalf("migrateV8: %v", err)
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 8 {
		t.Fatalf("user_version = %d, want 8", v)
	}
	var parent, modelName string
	var duration, total int64
	if err := raw.QueryRow(`SELECT agent_parent_id, model, duration_ms, total_tokens FROM messages WHERE client=? AND id=?`,
		model.ClientCodexCLI, "m1").Scan(&parent, &modelName, &duration, &total); err != nil {
		t.Fatal(err)
	}
	if parent != "" {
		t.Fatalf("存量行 agent_parent_id = %q, want ''（不回填）", parent)
	}
	if modelName != "ccs-opencode/deepseek-v4.1-flash" || duration != 4200 || total != 30 {
		t.Fatalf("升级后 model=%q duration=%d total=%d，数据应零变化", modelName, duration, total)
	}
	var sessParent string
	if err := raw.QueryRow(`SELECT agent_parent_id FROM sessions WHERE client=? AND id=?`,
		model.ClientCodexCLI, "s1").Scan(&sessParent); err != nil {
		t.Fatal(err)
	}
	if sessParent != "" {
		t.Fatalf("sessions 存量行 agent_parent_id = %q, want 空字符串（不回填）", sessParent)
	}
}

// TestMigrateV8FailureKeepsV7：中段注入失败整体回滚——库保持 v7：列与索引
// 均不残留、版本号不前进；清除注入后重试成功。
func TestMigrateV8FailureKeepsV7(t *testing.T) {
	raw := buildV7RawDB(t)
	defer raw.Close()
	migrateV8PostAlterHook = func() error { return errors.New("injected mid-migration failure") }
	t.Cleanup(func() { migrateV8PostAlterHook = nil })
	if err := migrateV8(raw); err == nil {
		t.Fatal("migrateV8 应因中段注入失败")
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 7 {
		t.Fatalf("失败后 user_version = %d, want 7", v)
	}
	if exists, _ := messagesAgentParentColumn(t, raw); exists {
		t.Fatal("失败后 agent_parent_id 列不应残留（ALTER 随事务回滚）")
	}
	if messagesAgentParentIndexExists(t, raw) {
		t.Fatal("失败后 idx_messages_agent_parent 索引不应残留（CREATE 随事务回滚）")
	}
	if exists, _ := sessionsAgentParentColumn(t, raw); exists {
		t.Fatal("失败后 sessions.agent_parent_id 列不应残留（ALTER 随事务回滚）")
	}
	if sessionsAgentParentIndexExists(t, raw) {
		t.Fatal("失败后 idx_sessions_agent_parent 索引不应残留（CREATE 随事务回滚）")
	}
	migrateV8PostAlterHook = nil
	if err := migrateV8(raw); err != nil {
		t.Fatalf("重试 migrateV8 失败: %v", err)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 8 {
		t.Fatalf("重试后 user_version = %d, want 8", v)
	}
}

// TestV8PersistsAgentParentID：UpsertMessages 落库 agent_parent_id；空值重放
// 不清除既有父关系（与 router_* 空不清除同口径）。
func TestV8PersistsAgentParentID(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	m := model.Message{
		ID: "cm1#1", SessionID: "child-1", Client: model.ClientCodexCLI,
		Date: "2026-11-05", TS: 1000, TotalTokens: 5, OutputTokens: 5,
		AgentParentID: "parent-1",
	}
	if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.QueryRow(`SELECT agent_parent_id FROM messages WHERE client=? AND id=?`,
		model.ClientCodexCLI, "cm1#1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "parent-1" {
		t.Fatalf("agent_parent_id = %q, want parent-1", got)
	}
	// 空值重放：不清除既有父关系。
	m.AgentParentID = ""
	if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT agent_parent_id FROM messages WHERE client=? AND id=?`,
		model.ClientCodexCLI, "cm1#1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "parent-1" {
		t.Fatalf("空值重放后 agent_parent_id = %q, want parent-1（空不清除）", got)
	}
	// sessions 侧同合同：落库 + 空值重放不清除（家族关系权威来源）。
	s := model.Session{ID: "child-1", Client: model.ClientCodexCLI, AgentParentID: "parent-1", FirstTS: 1, LastTS: 2}
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{s}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT agent_parent_id FROM sessions WHERE client=? AND id=?`,
		model.ClientCodexCLI, "child-1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "parent-1" {
		t.Fatalf("sessions.agent_parent_id = %q, want parent-1", got)
	}
	s.AgentParentID = ""
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{s}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT agent_parent_id FROM sessions WHERE client=? AND id=?`,
		model.ClientCodexCLI, "child-1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "parent-1" {
		t.Fatalf("空值重放后 sessions.agent_parent_id = %q, want parent-1（空不清除）", got)
	}
}

// TestUpsertSessionAgentParents_PreservesExistingMetadata：关系专用写入只更新
// agent_parent_id——不覆盖已有会话的 directory/project/parent_id/title（按日期
// 采集或空数据不得改动历史会话元数据）；新会话行以最小列创建。
func TestUpsertSessionAgentParents_PreservesExistingMetadata(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	// 既有完整会话行（含 fork parent_id 与标题）。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{{
		ID: "mid", Client: model.ClientCodexCLI, Directory: "/keep/dir", Project: "keep-proj",
		Title: "keep-title", ParentID: "fork-parent", FirstTS: 100, LastTS: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertSessionAgentParents(context.Background(), d, []model.Session{
		{ID: "mid", Client: model.ClientCodexCLI, AgentParentID: "root"},
		{ID: "new", Client: model.ClientCodexCLI, AgentParentID: "root2"},
	}); err != nil {
		t.Fatal(err)
	}
	var dir, proj, title, forkParent, agentParent string
	if err := d.QueryRow(`SELECT directory, project, title, parent_id, agent_parent_id FROM sessions WHERE id='mid' AND client=?`,
		model.ClientCodexCLI).Scan(&dir, &proj, &title, &forkParent, &agentParent); err != nil {
		t.Fatal(err)
	}
	if dir != "/keep/dir" || proj != "keep-proj" || title != "keep-title" || forkParent != "fork-parent" {
		t.Fatalf("关系写入不得覆盖完整元数据: dir=%q proj=%q title=%q fork=%q", dir, proj, title, forkParent)
	}
	if agentParent != "root" {
		t.Fatalf("agent_parent_id = %q, want root", agentParent)
	}
	// 新行：只带主键与父关系，其余列取默认值。
	var newDir, newAgent string
	if err := d.QueryRow(`SELECT directory, agent_parent_id FROM sessions WHERE id='new' AND client=?`,
		model.ClientCodexCLI).Scan(&newDir, &newAgent); err != nil {
		t.Fatal(err)
	}
	if newDir != "" || newAgent != "root2" {
		t.Fatalf("新会话行 dir=%q agent_parent=%q, want 空/root2", newDir, newAgent)
	}
}
