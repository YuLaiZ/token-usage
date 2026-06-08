// internal/db/dao_ccswitch_test.go
package db

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// 注：setupTestDB 复用 dao_test.go 中已有的定义（Open 内部已调 ensureSchema，
// raw_router_logs 表随之建好）。本计划原拟在此重新定义 setupTestDB，
// 但 Go 不允许同包重定义，故复用现有 helper，测试体与计划完全一致。

func TestUpsertRawRouterLogs_Insert(t *testing.T) {
	db := setupTestDB(t)
	logs := []model.RouterLog{
		{
			RequestID:    "session:abc",
			MessageID:    "abc",
			RouterName:   "cc_switch",
			Model:        "glm-5.2",
			ProviderName: "Zhipu GLM 宇来",
			InputTokens:  1234,
			OutputTokens: 567,
			CreatedAt:    1781092800,
			RawData:      `{"latency_ms":1200}`,
		},
	}
	count, err := UpsertRawRouterLogs(context.Background(), db, logs)
	if err != nil {
		t.Fatalf("UpsertRawRouterLogs failed: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	var msgID, pName string
	if err := db.QueryRow(`SELECT message_id, provider_name FROM raw_router_logs WHERE request_id = ?`, "session:abc").Scan(&msgID, &pName); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if msgID != "abc" {
		t.Errorf("message_id = %q, want abc", msgID)
	}
	if pName != "Zhipu GLM 宇来" {
		t.Errorf("provider_name = %q, want Zhipu GLM 宇来", pName)
	}
}

func TestUpsertRawRouterLogs_Dedup(t *testing.T) {
	db := setupTestDB(t)
	logs := []model.RouterLog{
		{RequestID: "session:abc", MessageID: "abc", RouterName: "cc_switch", InputTokens: 1234},
	}
	if _, err := UpsertRawRouterLogs(context.Background(), db, logs); err != nil {
		t.Fatalf("首次插入失败: %v", err)
	}

	// 重复插入（相同 request_id + router_name）应覆盖而非新增
	logs[0].InputTokens = 9999
	count, err := UpsertRawRouterLogs(context.Background(), db, logs)
	if err != nil {
		t.Fatalf("重复插入失败: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM raw_router_logs WHERE router_name = 'cc_switch'`).Scan(&total); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 1 {
		t.Errorf("记录数 = %d, want 1（INSERT OR REPLACE 幂等）", total)
	}
	var in int64
	if err := db.QueryRow(`SELECT input_tokens FROM raw_router_logs WHERE request_id = 'session:abc'`).Scan(&in); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if in != 9999 {
		t.Errorf("input_tokens = %d, want 9999（应被覆盖）", in)
	}
}
