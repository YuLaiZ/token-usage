package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// TestValidateRouterTargetClient 错误矩阵：空/未知/禁用/未配 router/合法。
func TestValidateRouterTargetClient(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true, Router: "cc_switch"},
			"opencode": {Enabled: true},                       // 未配 router
			"codex":    {Enabled: false, Router: "cc_switch"}, // disabled
		},
	}
	tests := []struct {
		name       string
		client     string
		expectErr  bool
		errContain string
	}{
		{"empty client", "", true, "未指定客户端"},
		{"unknown client", "unknown", true, "未知客户端"},
		{"disabled client with router", "codex", true, "已禁用"},
		{"client without router", "opencode", true, "未配置 router"},
		{"valid claude", "claude", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRouterTargetClient(cfg, tt.client)
			if tt.expectErr {
				if err == nil {
					t.Fatal("期望返回 error，实际 nil")
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error 应含 %q，实际 %q", tt.errContain, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("期望无 error，实际 %v", err)
				}
			}
		})
	}
}

// TestRunCollectRouter_NoClient 纯逻辑函数：空 client 委托给 RunRouterBackfill 报错。
// （CLI 路径在 RunE 中由 validateRouterTargetClient 先拦截，给出更明确的"未指定客户端"错误。）
func TestRunCollectRouter_NoClient(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{}}
	deps := engine.NewDepsWithCollectors(cfg, nil, nil)
	usageDB := openTestDB(t)
	err := runCollectRouter(context.Background(), deps, usageDB, slog.Default(), io.Discard, "")
	if err == nil {
		t.Fatal("期望空 client 返回 error")
	}
	if !strings.Contains(err.Error(), "router") {
		t.Errorf("error 应含 'router'，实际 %q", err.Error())
	}
}

// TestRunCollectRouter_NoRouterDoesBackfill 通过 RunRouterBackfill 失败：
// 合法 client 但未配 router，应为该 client 的 router 阶段失败（错误含 client 名 + router 字样）。
func TestRunCollectRouter_NoRouterDoesBackfill(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{"opencode": {Enabled: true}}}
	deps := engine.NewDepsWithCollectors(cfg, nil, nil)
	usageDB := openTestDB(t)
	err := runCollectRouter(context.Background(), deps, usageDB, slog.Default(), io.Discard, "opencode")
	if err == nil {
		t.Fatal("未配 router 应返回 error")
	}
	if !strings.Contains(err.Error(), "opencode") || !strings.Contains(err.Error(), "router") {
		t.Errorf("error 应含 opencode+router，实际 %q", err.Error())
	}
}

// TestRunCollectRouter_DoesNotWriteCollectionLogErrorsCursor backfill 不写 collection_log/
// collection_errors/router cursor。用真实内存 DB + 注入 router，断言三张表写入次数均为 0。
func TestRunCollectRouter_DoesNotWriteCollectionLogErrorsCursor(t *testing.T) {
	// 插入一条 messages 记录，让 backfill 路径有机会写 attribution（但它仍不应改 log/errors/cursor）
	usageDB := openTestDB(t)
	ctx := context.Background()
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []model.Message{
		{ID: "m1", Client: model.ClientClaudeCode, Date: "2026-07-01"},
	}
	if _, err := db.UpsertMessages(ctx, tx, msgs); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 收集 baseline 计数
	beforeLog, err := countRows(usageDB, "collection_log")
	if err != nil {
		t.Fatal(err)
	}
	beforeErrors, err := countRows(usageDB, "collection_errors")
	if err != nil {
		t.Fatal(err)
	}
	beforeCursor, err := countRows(usageDB, "sync_state")
	if err != nil {
		t.Fatal(err)
	}

	// router adapter 返回一条带 messageID 的 RouterLog（与 m1 关联），
	// 让 RunRouterBackfill 走完整归因回填路径
	ra := &staticLogsRouter{name: "cc_switch", logs: []model.RouterLog{
		{MessageID: "m1", ProviderName: "anthropic", Model: "claude-sonnet"},
	}}
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "x"}},
	}
	deps := engine.NewDepsWithCollectors(cfg, nil,
		map[string]collector.RouterAdapter{"cc_switch": ra})

	err = runCollectRouter(ctx, deps, usageDB, slog.Default(), io.Discard, "claude")
	if err != nil {
		t.Fatalf("runCollectRouter: %v", err)
	}

	afterLog, _ := countRows(usageDB, "collection_log")
	afterErrors, _ := countRows(usageDB, "collection_errors")
	afterCursor, _ := countRows(usageDB, "sync_state")
	if afterLog != beforeLog {
		t.Errorf("collection_log 不应被写入：before=%d after=%d", beforeLog, afterLog)
	}
	if afterErrors != beforeErrors {
		t.Errorf("collection_errors 不应被写入：before=%d after=%d", beforeErrors, afterErrors)
	}
	if afterCursor != beforeCursor {
		t.Errorf("sync_state 不应被写入：before=%d after=%d", beforeCursor, afterCursor)
	}
}

// TestRunCollectRouter_AdapterFailure adapter backfill 失败时返回 error。
func TestRunCollectRouter_AdapterFailure(t *testing.T) {
	ra := &callProbeRouter{name: "cc_switch", err: fmt.Errorf("adapter 装配失败")}
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "x"}},
	}
	deps := engine.NewDepsWithCollectors(cfg, nil,
		map[string]collector.RouterAdapter{"cc_switch": ra})
	usageDB := openTestDB(t)

	err := runCollectRouter(context.Background(), deps, usageDB, slog.Default(), io.Discard, "claude")
	if err == nil {
		t.Fatal("期望返回 error")
	}
	if !strings.Contains(err.Error(), "router") {
		t.Errorf("error 应含 'router'，实际 %q", err.Error())
	}
}

// staticLogsRouter 返回固定 logs。
type staticLogsRouter struct {
	name string
	logs []model.RouterLog
}

func (s *staticLogsRouter) Name() string { return s.name }
func (s *staticLogsRouter) Capabilities() collector.RouterCapabilities {
	return collector.RouterCapabilities{Provider: true, Model: true}
}
func (s *staticLogsRouter) SyncSource() string { return "static_logs_router_" + s.name }
func (s *staticLogsRouter) CollectLogs(ctx context.Context, req collector.RouterCollectRequest, log *slog.Logger) (collector.RouterCollectResult, error) {
	return collector.RouterCollectResult{Logs: s.logs}, nil
}

func countRows(d *db.DB, table string) (int, error) {
	row := d.QueryRow("SELECT COUNT(*) FROM " + table)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
