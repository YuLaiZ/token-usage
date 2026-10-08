package querier

import (
	"context"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// WorkBuddy expert 身份分组：存储键不折叠（仅大小写不同的 expert ID 独立
// 计数），显示为可读名；普通 client 的大小写变体仍按显示键折叠归并。
func TestByClient_WorkBuddyExpertFamilyGrouping(t *testing.T) {
	q := newEmptyQuerier(t)
	expertA := model.WorkBuddyExpertClientKey("A")
	expertA2 := model.WorkBuddyExpertClientKey("a") // 仅大小写不同
	msgs := []model.Message{
		{ID: "e1", SessionID: "s1", Client: expertA, Date: "2026-10-08", TS: wbQTS(10, 0), TotalTokens: 100},
		{ID: "e2", SessionID: "s2", Client: expertA2, Date: "2026-10-08", TS: wbQTS(11, 0), TotalTokens: 50},
		{ID: "e3", SessionID: "s3", Client: model.ClientWorkBuddy, Date: "2026-10-08", TS: wbQTS(12, 0), TotalTokens: 30},
		// 普通 client 大小写变体：与 WorkBuddy 折叠归并为一组
		{ID: "e4", SessionID: "s4", Client: "workbuddy", Date: "2026-10-08", TS: wbQTS(13, 0), TotalTokens: 20},
		// 另一个普通客户端对照
		{ID: "e5", SessionID: "s5", Client: model.ClientClaudeCode, Date: "2026-10-08", TS: wbQTS(14, 0), TotalTokens: 10},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}

	out, err := q.ByClient(context.Background(), []string{"2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	// 分组数：expertA、expertA2 独立两组 + WorkBuddy（含 workbuddy 变体折叠）+ Claude Code = 4 组
	if !strings.Contains(out, "WorkBuddy [A]") {
		t.Errorf("输出应含可读显示名 WorkBuddy [A]:\n%s", out)
	}
	if !strings.Contains(out, "WorkBuddy [a]") {
		t.Errorf("输出应含独立分组 WorkBuddy [a]（仅大小写不同不折叠）:\n%s", out)
	}
	if strings.Contains(out, "WorkBuddy Expert:") {
		t.Errorf("不得向用户展示十六进制存储键:\n%s", out)
	}

	// distinctClientCount 与分组同口径：4 个活跃客户端
	n, err := q.distinctClientCount(context.Background(), "(?)", []interface{}{"2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("客户端数 = %d, want 4（expert 大小写独立、普通变体折叠）", n)
	}
}

// Summary 的客户端数与 client 维度分组同键（expert 不折叠、普通变体折叠）。
func TestSummary_ClientCountWorkBuddyExpert(t *testing.T) {
	q := newEmptyQuerier(t)
	msgs := []model.Message{
		{ID: "e1", SessionID: "s1", Client: model.WorkBuddyExpertClientKey("A"), Date: "2026-10-08", TS: wbQTS(10, 0), TotalTokens: 100},
		{ID: "e2", SessionID: "s2", Client: model.WorkBuddyExpertClientKey("a"), Date: "2026-10-08", TS: wbQTS(11, 0), TotalTokens: 50},
	}
	if _, err := db.UpsertMessages(context.Background(), q.db, msgs); err != nil {
		t.Fatal(err)
	}
	out, err := q.Summary(context.Background(), []string{"2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	// 客户端数行：en/zh 形态（"Clients: 2" / 「客户端数: 2」任一）
	if !(strings.Contains(out, "Clients: 2") || strings.Contains(out, "客户端数: 2")) {
		t.Errorf("Summary 客户端数应计 2（expert 大小写独立）:\n%s", out)
	}
}

func wbQTS(hour, min int) int64 {
	return int64(2026*31536000+283*86400)*1000 + int64(hour*3600+min*60)*1000
}
