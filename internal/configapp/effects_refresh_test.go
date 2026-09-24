package configapp

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// refreshBase 构造两份比较用的基线配置。
func refreshBase() *config.Config {
	return &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{PollInterval: 30},
		Log:     config.LogConfig{Level: "info", Dir: "/d/logs", MaxDays: 7},
	}
}

// TestAnalyzeConfigEffects_RefreshIsWriteOnly:只改查询刷新间隔 → Changed 视角
// 的有效配置已变(effectiveEqual 感知),但不触发 daemon 重启(RuntimeChanged
// false)、不触发采集、不产生迁移/警告——刷新值只被网页仪表盘与 watch 消费。
func TestAnalyzeConfigEffects_RefreshIsWriteOnly(t *testing.T) {
	prev := refreshBase()
	curr := refreshBase()
	curr.Refresh = config.RefreshConfig{DashboardInterval: 20, WatchInterval: 60}

	effects := AnalyzeConfigEffects(prev, curr)
	if effects.RuntimeChanged {
		t.Error("查询刷新变化不得触发 daemon 重启(RuntimeChanged 应为 false)")
	}
	if len(effects.FullCollectClients) != 0 || len(effects.RouterBackfillClients) != 0 {
		t.Errorf("查询刷新变化不得触发采集: full=%v router=%v", effects.FullCollectClients, effects.RouterBackfillClients)
	}
	if effects.DataDirMigration != nil || len(effects.Warnings) != 0 {
		t.Errorf("查询刷新变化不得产生迁移/警告: %+v", effects)
	}
	if effectiveEqual(prev, curr) {
		t.Error("查询刷新变化必须被有效配置等价比较感知(否则保存被误报「未变化」)")
	}
	if effectiveEqual(prev, prev) != true {
		t.Error("同配置应等价")
	}
	// 反向:poll_interval 变化仍触发重启(守卫不因 refresh 加入而误伤既有矩阵)。
	curr2 := refreshBase()
	curr2.Daemon.PollInterval = 60
	if !AnalyzeConfigEffects(refreshBase(), curr2).RuntimeChanged {
		t.Error("poll_interval 变化应仍触发 RuntimeChanged")
	}
}
