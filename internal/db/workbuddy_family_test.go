package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// wbFamilyTestDB 构造内存用量库并写入初始 messages/sessions 行。
func wbFamilyTestDB(t *testing.T, msgs []model.Message, sess []model.Session) *DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := UpsertMessages(context.Background(), d, msgs); err != nil {
		t.Fatalf("UpsertMessages: %v", err)
	}
	if _, err := UpsertSessionMeta(context.Background(), d, sess); err != nil {
		t.Fatalf("UpsertSessionMeta: %v", err)
	}
	return d
}

func wbMsg(id, sessionID, client string, ts int64, total int64) model.Message {
	return model.Message{ID: id, SessionID: sessionID, Client: client, Date: "2026-10-08", TS: ts, TotalTokens: total, Directory: "/d", Project: "p"}
}

func wbSess(id, client, project, title string) model.Session {
	return model.Session{ID: id, Client: client, Directory: "/d", Project: project, Title: title, FirstTS: 1000, LastTS: 2000}
}

// 家族读：只命中精确 WorkBuddy 与严格合法 expert 编码；前缀粗筛命中的非法
// 编码（大写 hex）按错误返回，不静默纳入。
func TestWorkBuddyFamilyRead_Validation(t *testing.T) {
	expertA := model.WorkBuddyExpertClientKey("A")
	d := wbFamilyTestDB(t,
		[]model.Message{
			wbMsg("m1", "s1", model.ClientWorkBuddy, 1000, 10),
			wbMsg("m2", "s1", expertA, 2000, 20),
			wbMsg("m3", "s1", "WorkBuddy Expert:4G", 3000, 30), // 非法编码（G 非法）
		},
		nil)

	if _, err := WorkBuddyFamilyMessages(context.Background(), d, []string{"s1"}); err == nil {
		t.Fatal("非法家族编码应报错")
	}
	// 非 WorkBuddy 前缀的其他 client 不受家族影响
	msgs, err := WorkBuddyFamilyMessages(context.Background(), d, []string{"s1"})
	if err == nil && len(msgs) == 3 {
		t.Fatal("不应把非法编码行当作家族返回")
	}
}

func TestWorkBuddyFamilyMessages_ScopedToFamilyAndSessions(t *testing.T) {
	expertA := model.WorkBuddyExpertClientKey("A")
	d := wbFamilyTestDB(t,
		[]model.Message{
			wbMsg("m1", "s1", model.ClientWorkBuddy, 1000, 10),
			wbMsg("m2", "s1", expertA, 2000, 20),
			wbMsg("m3", "s2", model.ClientWorkBuddy, 1500, 30),
			// 其他客户端的同 session 行不读取
			wbMsg("m4", "s1", model.ClientClaudeCode, 5000, 99),
		},
		nil)

	msgs, err := WorkBuddyFamilyMessages(context.Background(), d, []string{"s1"})
	if err != nil {
		t.Fatalf("WorkBuddyFamilyMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("家族行 = %d, want 2（不含 claude 行）: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Client == model.ClientClaudeCode {
			t.Errorf("不应返回非家族行: %+v", m)
		}
	}
}

// 删除：只删家族粗筛行；非家族 client 的同 id/session 行保留。
func TestDeleteWorkBuddyFamilyRows_LeavesOtherClients(t *testing.T) {
	expertA := model.WorkBuddyExpertClientKey("A")
	d := wbFamilyTestDB(t,
		[]model.Message{
			wbMsg("m1", "s1", model.ClientWorkBuddy, 1000, 10),
			wbMsg("m2", "s1", expertA, 2000, 20),
			wbMsg("same-id", "s1", model.ClientClaudeCode, 3000, 30),
		},
		[]model.Session{
			wbSess("s1", model.ClientWorkBuddy, "p", "t"),
			wbSess("s1", expertA, "p", "t"),
			wbSess("s1", model.ClientClaudeCode, "p", "t"),
		})

	if err := DeleteWorkBuddyFamilyRows(context.Background(), d, []string{"s1"}); err != nil {
		t.Fatalf("DeleteWorkBuddyFamilyRows: %v", err)
	}
	var msgCount, sessCount int
	d.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id='s1'`).Scan(&msgCount)
	d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id='s1'`).Scan(&sessCount)
	if msgCount != 1 || sessCount != 1 {
		t.Errorf("删除后剩 messages=%d sessions=%d, want 各 1（仅 Claude 行）", msgCount, sessCount)
	}
	var remaining string
	d.QueryRow(`SELECT client FROM messages WHERE session_id='s1'`).Scan(&remaining)
	if remaining != model.ClientClaudeCode {
		t.Errorf("剩余行 client = %q, want Claude Code", remaining)
	}
}

// 消息归属查询：同 id 的全部家族归属返回（含跨会话），供冲突检测。
func TestQueryWorkBuddyMessageOwners(t *testing.T) {
	expertA := model.WorkBuddyExpertClientKey("A")
	d := wbFamilyTestDB(t,
		[]model.Message{
			wbMsg("shared", "s1", model.ClientWorkBuddy, 1000, 10),
			wbMsg("shared", "s2", expertA, 2000, 20),
			wbMsg("solo", "s1", model.ClientWorkBuddy, 3000, 30),
			wbMsg("other-client", "s3", model.ClientCodexCLI, 4000, 40),
		},
		nil)

	owners, err := QueryWorkBuddyMessageOwners(context.Background(), d, []string{"shared", "solo", "other-client"})
	if err != nil {
		t.Fatalf("QueryWorkBuddyMessageOwners: %v", err)
	}
	byMsg := map[string][]string{}
	for _, o := range owners {
		byMsg[o.MessageID] = append(byMsg[o.MessageID], o.SessionID)
	}
	if len(byMsg["shared"]) != 2 {
		t.Errorf("shared 归属 = %v, want 两个会话", byMsg["shared"])
	}
	if len(byMsg["solo"]) != 1 || byMsg["solo"][0] != "s1" {
		t.Errorf("solo 归属 = %v", byMsg["solo"])
	}
	if _, ok := byMsg["other-client"]; ok {
		t.Error("非家族 client 的同 id 行不应出现在归属结果中")
	}
}

// 按片段解决：完整复核成功后解决历史 WorkBuddy 元数据/重归属错误，不受登记
// 日期限制；其他 source/其他消息不受影响。
func TestResolveWorkBuddyErrorsByMessagePattern(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// 模拟 RunCollect 记录形态：message = "source stage: cause"，片段在 cause 内
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-09-24"}, "workbuddy",
		"workbuddy 读取数据源失败: workbuddy metadata failed: 会话 x 在源库无有效元数据", ""); err != nil {
		t.Fatal(err)
	}
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-09-25"}, "workbuddy",
		"workbuddy 写入事务失败: workbuddy rekey failed: 消息冲突", ""); err != nil {
		t.Fatal(err)
	}
	// 不应被解决的对照行
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-09-25"}, "workbuddy",
		"workbuddy 读取数据源失败: 普通扫描失败", ""); err != nil {
		t.Fatal(err)
	}
	if err := RecordErrorsByDate(context.Background(), d, []string{"2026-09-25"}, "claude",
		"claude 读取数据源失败: workbuddy metadata failed: 无关客户端错误文本", ""); err != nil {
		t.Fatal(err)
	}

	n, err := ResolveWorkBuddyErrorsByMessagePattern(context.Background(), d)
	if err != nil {
		t.Fatalf("ResolveWorkBuddyErrorsByMessagePattern: %v", err)
	}
	if n != 2 {
		t.Errorf("解决行数 = %d, want 2（只匹配 workbuddy source 的两个片段）", n)
	}
	var unresolved int
	d.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE resolved=0`).Scan(&unresolved)
	if unresolved != 2 {
		t.Errorf("未解决行 = %d, want 2（普通失败 + 其他客户端）", unresolved)
	}
}

// 家族会话清单：messages 与 sessions 任一有家族行即列入。
func TestWorkBuddyFamilySessionIDsWithRows(t *testing.T) {
	expertA := model.WorkBuddyExpertClientKey("A")
	d := wbFamilyTestDB(t,
		[]model.Message{wbMsg("m1", "msg-only", model.ClientWorkBuddy, 1000, 10)},
		[]model.Session{wbSess("sess-only", expertA, "p", "t")})

	ids, err := WorkBuddyFamilySessionIDsWithRows(context.Background(), d)
	if err != nil {
		t.Fatalf("WorkBuddyFamilySessionIDsWithRows: %v", err)
	}
	if len(ids) != 2 || ids[0] != "msg-only" || ids[1] != "sess-only" {
		t.Errorf("家族会话清单 = %v, want [msg-only sess-only]", ids)
	}
}

// IN 分块：超过单块上限的会话集合分批读写，不遗漏。
func TestWorkBuddyFamilyChunking(t *testing.T) {
	if workBuddyFamilyChunk > 300 {
		t.Skip("块大小过大，跳过分块测试")
	}
	var msgs []model.Message
	var ids []string
	for i := 0; i < workBuddyFamilyChunk+37; i++ {
		id := fmtSessionID(i)
		msgs = append(msgs, wbMsg("m"+id, id, model.ClientWorkBuddy, 1000, 1))
		ids = append(ids, id)
	}
	d := wbFamilyTestDB(t, msgs, nil)

	got, err := WorkBuddyFamilyMessages(context.Background(), d, ids)
	if err != nil {
		t.Fatalf("WorkBuddyFamilyMessages: %v", err)
	}
	if len(got) != len(ids) {
		t.Errorf("跨块读取 = %d, want %d", len(got), len(ids))
	}
	if err := DeleteWorkBuddyFamilyRows(context.Background(), d, ids); err != nil {
		t.Fatalf("DeleteWorkBuddyFamilyRows: %v", err)
	}
	var remaining int
	d.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&remaining)
	if remaining != 0 {
		t.Errorf("跨块删除后剩 %d, want 0", remaining)
	}
}

func fmtSessionID(i int) string {
	return fmt.Sprintf("s%04d", i)
}
