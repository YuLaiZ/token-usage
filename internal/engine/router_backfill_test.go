package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// fakeBackfillRouter 实现 RouterAdapter，用于 RunRouterBackfill 测试。
// 返回预设的 RouterLog 列表（模拟 CC Switch 全表读）。
// nextCursor 用于 cursor 守护测试：返回非零值以放大"误写 cursor"的可观测性（I-1）。
//
// SyncSource 必须返回 collector.SyncSourceCCSwitchRouter（= "ccswitch_router"），
// 与 TestRunRouterBackfill_CursorUnchanged 断言查询的 key 一致（I-1，第七轮评审修订）。
// 若返回任意其他常量，断言查询的 source 与潜在误写 SetSyncCursors 的 source 不匹配，
// cursor 守护会恒通过、漏报回归。现有 collect_test.go:422 的 fakeRouter 即用此常量。
type fakeBackfillRouter struct {
	name       string
	logs       []model.RouterLog
	nextCursor model.SyncCursor
}

func (f *fakeBackfillRouter) Name() string { return f.name }
func (f *fakeBackfillRouter) Capabilities() collector.RouterCapabilities {
	return collector.RouterCapabilities{}
}
func (f *fakeBackfillRouter) SyncSource() string { return collector.SyncSourceCCSwitchRouter }
func (f *fakeBackfillRouter) CollectLogs(ctx context.Context, req collector.RouterCollectRequest, log *slog.Logger) (collector.RouterCollectResult, error) {
	return collector.RouterCollectResult{Logs: f.logs, NextCursor: f.nextCursor}, nil
}

// setupBackfillTestDB 建临时 DB + 插入若干 messages，返回 db 与 deps。
// router 可为 nil（TestRunRouterBackfill_RouterNil 用例）：此时 routers 表传 nil，
// 不能调用 router.Name()，否则 nil 解引用 panic（C2，第七轮评审修订）。
func setupBackfillTestDB(t *testing.T, cfg *config.Config, router collector.RouterAdapter) (*db.DB, *Deps) {
	t.Helper()
	// 用 :memory: 库（与现有 engine 测试风格一致，避免文件 IO）
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	t.Cleanup(func() { usageDB.Close() })
	var routers map[string]collector.RouterAdapter
	if router != nil {
		routers = map[string]collector.RouterAdapter{router.Name(): router}
	}
	deps := NewDepsWithCollectors(cfg, nil, routers)
	return usageDB, deps
}

// TestRunRouterBackfill_Normal 全表读 CC Switch + 回填 messages 的 router 字段。
func TestRunRouterBackfill_Normal(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
	}
	// fake router 返回 1 条 log，message_id 指向已插入的 message
	router := &fakeBackfillRouter{
		name: "cc_switch",
		logs: []model.RouterLog{
			{RequestID: "req1", MessageID: "cc1", RouterName: "cc_switch",
				AppType: "claude", Model: "claude-sonnet-4", ProviderName: "Anthropic", CreatedAt: 1000},
		},
	}
	usageDB, deps := setupBackfillTestDB(t, cfg, router)
	ctx := context.Background()

	// 插入 client="Claude Code" 的 message（id=cc1 与 router log 的 MessageID 对应）
	_, err := db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "cc1", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 500},
	})
	if err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	var out bytes.Buffer
	err = RunRouterBackfill(ctx, deps, usageDB, slog.Default(), &out, "claude")
	if err != nil {
		t.Fatalf("RunRouterBackfill failed: %v", err)
	}

	// 断言进度反馈
	if !strings.Contains(out.String(), "router 回填") {
		t.Errorf("期望输出含 'router 回填'，实际: %q", out.String())
	}

	// 断言 message 的 router_model 被回填（C2 修复有效性验证）
	// 直接查 messages 表
	var routerModel string
	err = usageDB.QueryRow(`SELECT router_model FROM messages WHERE id='cc1' AND client=?`, model.ClientClaudeCode).Scan(&routerModel)
	if err != nil {
		t.Fatalf("查询回填结果失败: %v", err)
	}
	if routerModel != "claude-sonnet-4" {
		t.Errorf("router_model 应被回填为 'claude-sonnet-4'，实际 %q（C2 修复可能无效）", routerModel)
	}
}

// TestRunRouterBackfill_RouterNil client 未配 router 返回 error。
func TestRunRouterBackfill_RouterNil(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"opencode": {Enabled: true}, // 未配 router
		},
	}
	usageDB, deps := setupBackfillTestDB(t, cfg, nil)
	err := RunRouterBackfill(context.Background(), deps, usageDB, slog.Default(), nil, "opencode")
	if err == nil {
		t.Fatal("期望返回 error（client 无 router），实际 nil")
	}
}

// TestRunRouterBackfill_UnknownClientConfigKey I-v4-1：
// 配置 key 不在 ClientToDisplayNames 时返回 error，不静默返回空。
func TestRunRouterBackfill_UnknownClientConfigKey(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"trae": {Enabled: true, Router: "cc_switch"},
		},
	}
	router := &fakeBackfillRouter{name: "cc_switch"}
	usageDB, deps := setupBackfillTestDB(t, cfg, router)
	err := RunRouterBackfill(context.Background(), deps, usageDB, slog.Default(), nil, "trae")
	if err == nil {
		t.Fatal("期望返回 error（未识别 client 配置 key），实际 nil（I-v4-1 修复可能无效）")
	}
}

// TestRunRouterBackfill_CursorUnchanged 不更新 cursor（不影响 daemon 增量）。
func TestRunRouterBackfill_CursorUnchanged(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
	}
	// I-1：让 fake router 返回非零 NextCursor（Value=9999）。
	// 若 RunRouterBackfill 错误调用了 SetSyncCursors，cursor 会变成 9999，断言才能捕捉回归；
	// 若实现正确不调 SetSyncCursors，cursor 保持零值。
	router := &fakeBackfillRouter{
		name: "cc_switch",
		logs: []model.RouterLog{
			{RequestID: "req1", MessageID: "cc1", RouterName: "cc_switch", AppType: "claude", CreatedAt: 1000},
		},
		nextCursor: model.SyncCursor{Value: 9999, ID: "reqX"},
	}
	usageDB, deps := setupBackfillTestDB(t, cfg, router)
	ctx := context.Background()

	_, _ = db.UpsertMessages(ctx, usageDB, []model.Message{
		{ID: "cc1", Client: model.ClientClaudeCode, Date: "2026-07-01", TS: 500},
	})

	// 记录调用前的 cursor（应为零值）
	cursorsBefore, _ := db.GetSyncCursors(ctx, usageDB, "claude", []string{"ccswitch_router"})

	_ = RunRouterBackfill(ctx, deps, usageDB, slog.Default(), nil, "claude")

	cursorsAfter, _ := db.GetSyncCursors(ctx, usageDB, "claude", []string{"ccswitch_router"})
	// cursor 应保持零值（未被更新）。fake router 返回了非零 NextCursor{9999}，
	// 若实现误写 cursor，cursorsAfter 会变成 9999，与零值 cursorsBefore 不相等，断言失败。
	if cursorsAfter["ccswitch_router"] != cursorsBefore["ccswitch_router"] {
		t.Errorf("RunRouterBackfill 不应更新 cursor（影响 daemon 增量），before=%v after=%v",
			cursorsBefore, cursorsAfter)
	}
}

// TestRunRouterBackfill_CtxCancelled ctx 取消时返回 ctx 错误。
func TestRunRouterBackfill_CtxCancelled(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
	}
	router := &fakeBackfillRouter{name: "cc_switch"}
	usageDB, deps := setupBackfillTestDB(t, cfg, router)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	err := RunRouterBackfill(ctx, deps, usageDB, slog.Default(), nil, "claude")
	if err == nil {
		t.Fatal("期望返回 error（ctx 取消），实际 nil")
	}
}
