package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === codex router 归因合同（session + 时间窗） ===
//
// 合同：QueryCodexRouterLogsBySessions 按 session 集合查 raw_router_logs 的
// codex proxy 行（session_id 双形态展开、data_source='proxy'、app_type='codex'、
// router_name 隔离、500 分块）；QueryCodexMessagesBySessions 按 session 集合查
// Codex App / Codex CLI 两显示名的全量 messages；MatchCodexRouterAttributions
// 纯函数按「剥 codex_ 前缀同 session + 300s 毫秒归一时间窗最近邻」配对，
// proxy 行按 (created_at, request_id) 升序迭代、每 message 至多消费一次、
// Δt 相同按 message id 字典序。

// seedCodexRouterFixture 在新库写入归因 DAO 测试行。
func seedCodexRouterFixture(t *testing.T, d *DB) {
	t.Helper()
	rows := []model.RouterLog{
		// 前缀形态（cc-switch codex proxy 源 session_id = codex_{uuid}）。
		{RequestID: "session:codex:p1:resp_1", RouterName: "cc_switch", SessionID: "codex_uuid-1",
			AppType: "codex", Model: "gpt-5.6-terra", ProviderName: "Zhipu GLM", DataSource: "proxy", CreatedAt: 1781092800},
		// 裸形态（上游直接给 uuid）。
		{RequestID: "session:codex:p1:resp_2", RouterName: "cc_switch", SessionID: "uuid-2",
			AppType: "codex", Model: "gpt-5.6-sol", ProviderName: "Zhipu GLM", DataSource: "proxy", CreatedAt: 1781092900},
		// codex_session 行：不得进入候选。
		{RequestID: "codex_session:thread-v1:uuid-3:1", RouterName: "cc_switch", SessionID: "codex_uuid-3",
			AppType: "codex", Model: "gpt-5.6-terra", DataSource: "codex_session", CreatedAt: 1781093000},
		// claude 行：不得进入候选（即使 session_id 同形态）。
		{RequestID: "session:msg_claude", RouterName: "cc_switch", SessionID: "codex_uuid-1",
			AppType: "claude", Model: "glm-5.3", DataSource: "proxy", CreatedAt: 1781093100},
		// 其他 router 的行：router_name 隔离。
		{RequestID: "session:codex:p1:resp_5", RouterName: "other_router", SessionID: "codex_uuid-1",
			AppType: "codex", Model: "gpt-5.6-terra", DataSource: "proxy", CreatedAt: 1781093200},
		// 空 session_id 的行：不参与匹配。
		{RequestID: "session:codex:p1:resp_6", RouterName: "cc_switch", SessionID: "",
			AppType: "codex", Model: "gpt-5.6-terra", DataSource: "proxy", CreatedAt: 1781093300},
	}
	if _, err := UpsertRawRouterLogs(context.Background(), d, rows); err != nil {
		t.Fatalf("seed raw_router_logs failed: %v", err)
	}
}

func TestQueryCodexRouterLogsBySessions(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	seedCodexRouterFixture(t, d)

	logs, err := QueryCodexRouterLogsBySessions(context.Background(), d, "cc_switch",
		[]string{"uuid-1", "uuid-2", "uuid-3", ""})
	if err != nil {
		t.Fatalf("QueryCodexRouterLogsBySessions failed: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs (前缀形态+裸形态), got %d: %+v", len(logs), logs)
	}
	got := map[string]bool{}
	for _, l := range logs {
		got[l.RequestID] = true
	}
	if !got["session:codex:p1:resp_1"] || !got["session:codex:p1:resp_2"] {
		t.Fatalf("命中集合错误: %+v", got)
	}

	// 空集合不报错、返回空。
	empty, err := QueryCodexRouterLogsBySessions(context.Background(), d, "cc_switch", nil)
	if err != nil {
		t.Fatalf("空 session 集合不应报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空集合期望 0 行, got %d", len(empty))
	}
}

// TestQueryCodexRouterLogsBySessions_Chunking：501 个 session 超出单块 500 上限，
// 分块查询合并后全部命中。
func TestQueryCodexRouterLogsBySessions_Chunking(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	var want []string
	var logs []model.RouterLog
	for i := 0; i < 501; i++ {
		sid := fmt.Sprintf("uuid-chunk-%03d", i)
		want = append(want, sid)
		logs = append(logs, model.RouterLog{
			RequestID: fmt.Sprintf("session:codex:p:resp_%03d", i), RouterName: "cc_switch",
			SessionID: "codex_" + sid, AppType: "codex", Model: "m", DataSource: "proxy",
			CreatedAt: int64(1781090000 + i),
		})
	}
	if _, err := UpsertRawRouterLogs(context.Background(), d, logs); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	got, err := QueryCodexRouterLogsBySessions(context.Background(), d, "cc_switch", want)
	if err != nil {
		t.Fatalf("QueryCodexRouterLogsBySessions failed: %v", err)
	}
	if len(got) != 501 {
		t.Fatalf("分块查询应命中全部 501 行, got %d", len(got))
	}
}

func TestQueryCodexMessagesBySessions(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	msgs := []model.Message{
		{ID: "msg_app#1", SessionID: "uuid-a", Client: "Codex App", Date: "2026-06-10", TS: 1781092800000},
		{ID: "msg_cli#1", SessionID: "uuid-b", Client: "Codex CLI", Date: "2026-06-10", TS: 1781092900000},
		{ID: "msg_claude#1", SessionID: "uuid-c", Client: "Claude Code", Date: "2026-06-10", TS: 1781093000000},
	}
	for _, m := range msgs {
		if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
			t.Fatalf("seed message failed: %v", err)
		}
	}
	got, err := QueryCodexMessagesBySessions(context.Background(), d, []string{"uuid-a", "uuid-b", "uuid-c"})
	if err != nil {
		t.Fatalf("QueryCodexMessagesBySessions failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 codex messages (App+CLI, claude 排除), got %d", len(got))
	}
	clients := map[string]bool{}
	for _, m := range got {
		clients[m.Client] = true
	}
	if !clients["Codex App"] || !clients["Codex CLI"] {
		t.Fatalf("两显示名应各自命中: %+v", clients)
	}

	empty, err := QueryCodexMessagesBySessions(context.Background(), d, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空集合期望 0 行无错, got %d err=%v", len(empty), err)
	}
}

// TestQueryCodexMessagesBySessions_EmptySessionSkipped：入参集合中的空串不得
// 拼入 IN 占位符——预置 session_id 为空串的 codex message，传 []string{""} 须 0 行；
// 空串混入合法 session 时只命中合法行（与 router 侧 DAO 空串跳过同一口径）。
func TestQueryCodexMessagesBySessions_EmptySessionSkipped(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	msgs := []model.Message{
		{ID: "msg_empty", SessionID: "", Client: "Codex App", Date: "2026-06-10", TS: 1781092800000},
		{ID: "msg_a", SessionID: "uuid-a", Client: "Codex App", Date: "2026-06-10", TS: 1781092900000},
	}
	for _, m := range msgs {
		if _, err := UpsertMessages(context.Background(), d, []model.Message{m}); err != nil {
			t.Fatalf("seed message failed: %v", err)
		}
	}
	onlyEmpty, err := QueryCodexMessagesBySessions(context.Background(), d, []string{""})
	if err != nil {
		t.Fatalf("QueryCodexMessagesBySessions failed: %v", err)
	}
	if len(onlyEmpty) != 0 {
		t.Fatalf("空 session 入参须跳过、期望 0 行, got %d", len(onlyEmpty))
	}
	mixed, err := QueryCodexMessagesBySessions(context.Background(), d, []string{"", "uuid-a"})
	if err != nil {
		t.Fatalf("QueryCodexMessagesBySessions failed: %v", err)
	}
	if len(mixed) != 1 || mixed[0].ID != "msg_a" {
		t.Fatalf("空串混入不得影响合法命中, got %+v", mixed)
	}
}

// TestQueryCodexMessagesBySessions_Chunking：501 个 session 超出单块 500 上限，
// 分块查询合并后全部命中（与 router 侧分块对称）。
func TestQueryCodexMessagesBySessions_Chunking(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	var sessions []string
	for i := 0; i < 501; i++ {
		sid := fmt.Sprintf("uuid-msg-%03d", i)
		sessions = append(sessions, sid)
		if _, err := UpsertMessages(context.Background(), d, []model.Message{{
			ID: fmt.Sprintf("msg_chunk_%03d#1", i), SessionID: sid,
			Client: model.ClientCodexCLI, Date: "2026-06-10", TS: 1781092800000,
		}}); err != nil {
			t.Fatalf("seed message failed: %v", err)
		}
	}
	got, err := QueryCodexMessagesBySessions(context.Background(), d, sessions)
	if err != nil {
		t.Fatalf("QueryCodexMessagesBySessions failed: %v", err)
	}
	if len(got) != 501 {
		t.Fatalf("分块查询应命中全部 501 行, got %d", len(got))
	}
}

// TestMatchCodexRouterAttributions：匹配纯函数表驱动。fixture 用真实量级：
// message TS 为 Unix 毫秒、log CreatedAt 为 Unix 秒——单位错误的实现必挂。
func TestMatchCodexRouterAttributions(t *testing.T) {
	const baseSec = int64(1781092800) // 2026-06-10 12:00:00 UTC
	proxyLog := func(reqID, session string, createdAt int64) model.RouterLog {
		return model.RouterLog{RequestID: reqID, RouterName: "cc_switch", SessionID: session,
			AppType: "codex", Model: "gpt-5.6-terra", ProviderName: "Zhipu GLM",
			DataSource: "proxy", CreatedAt: createdAt}
	}
	message := func(id, session string, tsMs int64) model.Message {
		return model.Message{ID: id, SessionID: session, Client: "Codex App", TS: tsMs}
	}

	cases := []struct {
		name     string
		logs     []model.RouterLog
		messages []model.Message
		want     []string // "MessageID<-RequestID" 有序对（按 proxy 迭代顺序）
	}{
		{
			name:     "窗口内最近邻命中（秒/毫秒真实量级）",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec+5)*1000)},
			want:     []string{"m1<-r1"},
		},
		{
			name:     "边界 +300s 含边界命中",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec+300)*1000)},
			want:     []string{"m1<-r1"},
		},
		{
			name:     "边界 -300s 含边界命中",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec-300)*1000)},
			want:     []string{"m1<-r1"},
		},
		{
			name:     "窗口外 +301s 跳过",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec+301)*1000)},
			want:     nil,
		},
		{
			name:     "窗口外 -301s 跳过",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec-301)*1000)},
			want:     nil,
		},
		{
			name:     "最近邻：两候选取距离小者",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m_far", "s1", (baseSec+10)*1000), message("m_near", "s1", (baseSec+2)*1000)},
			want:     []string{"m_near<-r1"},
		},
		{
			name: "每 message 单次消费：后到 proxy 行不抢已消费 message",
			logs: []model.RouterLog{
				proxyLog("r1", "codex_s1", baseSec),
				proxyLog("r2", "codex_s1", baseSec+10),
			},
			messages: []model.Message{message("m1", "s1", (baseSec+1)*1000)},
			want:     []string{"m1<-r1"},
		},
		{
			name:     "Δt 相同按 message id 字典序",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m_b", "s1", (baseSec-5)*1000), message("m_a", "s1", (baseSec+5)*1000)},
			want:     []string{"m_a<-r1"},
		},
		{
			name: "双 proxy × 双 message 交叉距离：(created_at,request_id) 升序迭代下的唯一结果",
			logs: []model.RouterLog{
				proxyLog("rb", "codex_s1", baseSec+100), // 后迭代：只剩 m1
				proxyLog("ra", "codex_s1", baseSec),     // 先迭代：m2(Δ40) 优于 m1(Δ60)
			},
			messages: []model.Message{
				message("m1", "s1", (baseSec+60)*1000),
				message("m2", "s1", (baseSec+40)*1000),
			},
			want: []string{"m2<-ra", "m1<-rb"},
		},
		{
			name:     "session 不同不配对（时间再近也不跨 session）",
			logs:     []model.RouterLog{proxyLog("r1", "codex_s1", baseSec)},
			messages: []model.Message{message("m1", "s9", (baseSec+1)*1000)},
			want:     nil,
		},
		{
			name:     "裸形态 session_id（无 codex_ 前缀）同样可配对",
			logs:     []model.RouterLog{proxyLog("r1", "s1", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec+1)*1000)},
			want:     []string{"m1<-r1"},
		},
		{
			name:     "空 proxy session 跳过",
			logs:     []model.RouterLog{proxyLog("r1", "", baseSec)},
			messages: []model.Message{message("m1", "s1", (baseSec+1)*1000)},
			want:     nil,
		},
		{
			name:     "空输入",
			logs:     nil,
			messages: nil,
			want:     nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MatchCodexRouterAttributions(c.logs, c.messages, CodexRouterMatchWindowSec)
			if len(got) != len(c.want) {
				t.Fatalf("归因数 = %d, want %d: %+v", len(got), len(c.want), got)
			}
			for i, w := range c.want {
				// w 形如 "m1<-r1"：命中 message <- proxy 行。
				msgID, reqID, ok := splitWantPair(w)
				if !ok {
					t.Fatalf("测试用例 want 对 %q 格式错误", w)
				}
				a := got[i]
				if a.MessageID != msgID || a.RequestID != reqID {
					t.Errorf("第 %d 对 = (%q <- %q), want (%q <- %q)", i, a.MessageID, a.RequestID, msgID, reqID)
				}
				// Client 取自 message 行；Provider/Model/RouterName 取自 proxy 行。
				if a.Client != "Codex App" || a.Provider != "Zhipu GLM" || a.Model != "gpt-5.6-terra" || a.RouterName != "cc_switch" {
					t.Errorf("第 %d 对字段传递错误: %+v", i, a)
				}
			}
		})
	}
}

// splitWantPair 解析 "messageID<-requestID" 形态。
func splitWantPair(w string) (msgID, reqID string, ok bool) {
	for i := 1; i < len(w)-1; i++ {
		if w[i] == '<' && w[i+1] == '-' {
			return w[:i], w[i+2:], true
		}
	}
	return "", "", false
}
