package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// TestRunCollectRetry_ClientFilterLimitsGroups --client X 只处理 X 的未解决失败组。
// 通过手工注入两条 collection_errors（claude + opencode），加 --client opencode，
// 断言 opencode 的失败组被处理、claude 未被触碰。
func TestRunCollectRetry_ClientFilterLimitsGroups(t *testing.T) {
	usageDB := openTestDB(t)
	ctx := context.Background()

	// 注入两条未解决错误：claude 2026-07-01、opencode 2026-07-01
	for _, c := range []string{"claude", "opencode"} {
		if err := db.RecordErrorsByDate(ctx, usageDB, []string{"2026-07-01"}, c, "fail", ""); err != nil {
			t.Fatal(err)
		}
	}

	// 用 fake collector，opencode 成功、claude（不会被调用）
	fcOpen := &callProbeCollector{name: "opencode"}
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true},
			"opencode": {Enabled: true},
		},
	}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fcOpen}, nil)

	var out bytes.Buffer
	err := runCollectRetry(ctx, deps, usageDB, slog.Default(), &out, "opencode")
	if err != nil {
		t.Fatalf("runCollectRetry: %v\n%s", err, out.String())
	}
	if fcOpen.calls != 1 {
		t.Errorf("opencode 应被调用 1 次，实际 %d", fcOpen.calls)
	}
	// claude 的错误仍未解决（未被触碰）
	errs, err := db.GetErrors(usageDB, db.ErrorFilter{Source: "claude", Unresolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 {
		t.Errorf("claude 错误应保留 1 条，实际 %d", len(errs))
	}
}

// TestRunCollectRetry_NoErrors 报"暂无需要重试的失败记录"且无 error。
func TestRunCollectRetry_NoErrors(t *testing.T) {
	usageDB := openTestDB(t)
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	deps := engine.NewDepsWithCollectors(cfg, nil, nil)
	var out bytes.Buffer
	err := runCollectRetry(context.Background(), deps, usageDB, slog.Default(), &out, "")
	if err != nil {
		t.Fatalf("期望无 error，实际 %v", err)
	}
	if !strings.Contains(out.String(), "暂无需要重试的失败记录") {
		t.Errorf("输出应含 '暂无需要重试的失败记录'，实际 %q", out.String())
	}
}

// TestRunCollectRetry_UnknownClient 未知 client 应返回 error。
func TestRunCollectRetry_UnknownClient(t *testing.T) {
	usageDB := openTestDB(t)
	cfg := &config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}}
	// 不注入 unknown client collector
	deps := engine.NewDepsWithCollectors(cfg, nil, nil)
	err := runCollectRetry(context.Background(), deps, usageDB, slog.Default(), &bytes.Buffer{}, "unknown")
	if err == nil {
		t.Fatal("未知 client 应返回 error")
	}
	if !strings.Contains(err.Error(), "未知客户端") {
		t.Errorf("error 应含 '未知客户端'，实际 %q", err.Error())
	}
}

// TestRunCollectRetry_DisabledClient 失败组里的 source 已禁用时，记 retryFailed 但不调用 collector。
func TestRunCollectRetry_DisabledClient(t *testing.T) {
	usageDB := openTestDB(t)
	ctx := context.Background()
	// 注入一条 codex 的失败错误
	if err := db.RecordErrorsByDate(ctx, usageDB, []string{"2026-07-01"}, "codex", "fail", ""); err != nil {
		t.Fatal(err)
	}
	// codex 在配置里 disabled
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"codex": {Enabled: false},
		},
	}
	// 即便注入了 codex collector（含真实 collector），但因 disabled，不应被实际采集
	fc := &callProbeCollector{name: "codex"}
	deps := engine.NewDepsWithCollectors(cfg, []collector.Collector{fc}, nil)

	err := runCollectRetry(ctx, deps, usageDB, slog.Default(), &bytes.Buffer{}, "")
	if err == nil {
		t.Fatal("期望返回 error（部分重试失败）")
	}
	if !strings.Contains(err.Error(), "部分重试失败") {
		t.Errorf("error 应含 '部分重试失败'，实际 %q", err.Error())
	}
	if fc.calls != 0 {
		t.Errorf("disabled client 不应调用 collector，实际 %d", fc.calls)
	}
}

// TestRunCollectRetryCmd_CLIRejectsUnknownClient CLI 层 validateClientExists 拦截：
// --client unknown（不在配置中）应在 loadCollectRuntime 之后、runCollectRetry 之前
// 被 CLI 层校验拦截，返回"未知客户端"。沿用 runCollectDefault / runCollectAllCmd 的校验时机。
func TestRunCollectRetryCmd_CLIRejectsUnknownClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(cfgDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	// config.toml：只启用 claude，retry --client unknown 应被拦截。
	cfgContent := fmt.Sprintf(`data_dir = "%s"

[clients.claude]
enabled = true

[log]
level = "info"
dir = "%s/logs"
max_days = 7
`, dataDir, dataDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"collect", "retry", "--client", "unknown"})

	err := root.Execute()
	if err == nil {
		t.Fatal("期望 --client unknown 被 CLI 层拦截返回 error")
	}
	if !strings.Contains(err.Error(), "未知客户端") {
		t.Errorf("error 应含 '未知客户端'（沿用兄弟子命令文案），实际 %q", err.Error())
	}
}
