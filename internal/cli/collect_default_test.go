package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// dateProbeCollector 记录最后一次 Collect 的 req.Dates。
type dateProbeCollector struct {
	name      string
	lastDates []string
}

func (d *dateProbeCollector) Name() string          { return d.name }
func (d *dateProbeCollector) SyncSources() []string { return nil }
func (d *dateProbeCollector) Collect(ctx context.Context, req collector.CollectRequest, log *slog.Logger) (collector.CollectResult, error) {
	d.lastDates = append([]string(nil), req.Dates...)
	return collector.CollectResult{}, nil
}

// TestCollectDefault_ClientNoDateCollectsToday 取消旧 collect --client X 无日期全采语义：
// 现在 collect --client X 无日期参数应只采今天（Dates=[today]，不是 nil 全扫）。
func TestCollectDefault_ClientNoDateCollectsToday(t *testing.T) {
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

	// 注入 dateProbe collector 到 newDepsFactory
	probe := &dateProbeCollector{name: "claude"}
	origNewDeps := newDepsFactory
	newDepsFactory = func(cfg *config.Config) *engine.Deps {
		return engine.NewDepsWithCollectors(cfg, []collector.Collector{probe}, nil)
	}
	t.Cleanup(func() { newDepsFactory = origNewDeps })

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"collect", "--client", "claude"})
	if err := root.Execute(); err != nil {
		t.Fatalf("collect --client claude 失败: %v", err)
	}
	if len(probe.lastDates) != 1 {
		t.Fatalf("期望 Dates 长度 1（今天），实际 %d: %v", len(probe.lastDates), probe.lastDates)
	}
	today := time.Now().Format("2006-01-02")
	if probe.lastDates[0] != today {
		t.Errorf("期望 Dates[0]=%q（今天），实际 %q", today, probe.lastDates[0])
	}
}

// TestCollectDefault_NoClientCollectsTodayAllEnabledClients 无 --client 时对所有 enabled client
// 只采今天（不是全采）。简化断言：用 fake collector 计数，确保走 default 分支而非 all。
func TestCollectDefault_NoClientCollectsTodayAllEnabledClients(t *testing.T) {
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
	cfgContent := fmt.Sprintf(`data_dir = "%s"

[clients.claude]
enabled = true

[clients.opencode]
enabled = true

[log]
level = "info"
dir = "%s/logs"
max_days = 7
`, dataDir, dataDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	probeClaude := &dateProbeCollector{name: "claude"}
	probeOpen := &dateProbeCollector{name: "opencode"}
	origNewDeps := newDepsFactory
	newDepsFactory = func(cfg *config.Config) *engine.Deps {
		return engine.NewDepsWithCollectors(cfg,
			[]collector.Collector{probeClaude, probeOpen}, nil)
	}
	t.Cleanup(func() { newDepsFactory = origNewDeps })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"collect"})
	if err := root.Execute(); err != nil {
		t.Fatalf("collect 失败: %v\n%s", err, out.String())
	}
	// 两个 client 都应被调用，且都是今天的单日期（非全采）
	for _, p := range []*dateProbeCollector{probeClaude, probeOpen} {
		if len(p.lastDates) != 1 {
			t.Errorf("%s 期望 Dates 长度 1（今天），实际 %d: %v", p.name, len(p.lastDates), p.lastDates)
		}
	}
}

// TestCollectDefault_MonthArgExpandsToDailyDates collect 202608 经完整执行链
// 展开为 31 天逐日列表并完整传入 collector（月粒度对 collect 语义为多日批量补采）。
func TestCollectDefault_MonthArgExpandsToDailyDates(t *testing.T) {
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

	probe := &dateProbeCollector{name: "claude"}
	origNewDeps := newDepsFactory
	newDepsFactory = func(cfg *config.Config) *engine.Deps {
		return engine.NewDepsWithCollectors(cfg, []collector.Collector{probe}, nil)
	}
	t.Cleanup(func() { newDepsFactory = origNewDeps })

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"collect", "--client", "claude", "202608"})
	if err := root.Execute(); err != nil {
		t.Fatalf("collect --client claude 202608 失败: %v", err)
	}
	if len(probe.lastDates) != 31 {
		t.Fatalf("期望 Dates 长度 31（2026 年 8 月逐日），实际 %d: %v", len(probe.lastDates), probe.lastDates)
	}
	if probe.lastDates[0] != "2026-08-01" || probe.lastDates[30] != "2026-08-31" {
		t.Errorf("期望首末日 2026-08-01/2026-08-31，实际 %q/%q", probe.lastDates[0], probe.lastDates[30])
	}
}
