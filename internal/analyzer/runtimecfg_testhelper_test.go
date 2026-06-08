package analyzer

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// loadTestEffectiveConfig 加载 effective config 用于测试。
// 旧 config.LoadFrom（含默认值+路径展开）已删除，统一走 runtimecfg.LoadEffectiveConfig。
// home 用 tmpDir（测试配置均为绝对路径，不依赖 ~ 展开；provider 默认路径被显式路径覆盖）。
func loadTestEffectiveConfig(t *testing.T, cfgPath, home string) *config.Config {
	t.Helper()
	eff, err := runtimecfg.LoadEffectiveConfig(cfgPath, runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         "linux",
		DefaultPaths: runtimecfg.NewStandardProvider(),
	})
	if err != nil {
		t.Fatalf("load effective config: %v", err)
	}
	return eff
}
