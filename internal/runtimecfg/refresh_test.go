package runtimecfg

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// refreshResolveEnv 返回固定测试环境(home 绝对路径、GOOS、空默认路径 provider)。
func refreshResolveEnv() ResolveEnv {
	return ResolveEnv{Home: "/tmp/fake-home", GOOS: "darwin", DefaultPaths: nil}
}

// refreshFakeProvider 是不做任何事的 DefaultPathProvider(测试只需核心默认值)。
type refreshFakeProvider struct{}

func (refreshFakeProvider) ApplyDefaults(*config.Config, string, string) error { return nil }

// TestRefreshEffectiveDefaults:用户层缺段(0)在有效层补为 30/30;显式值
// 原样保留;负值被读链校验拒绝。
func TestRefreshEffectiveDefaults(t *testing.T) {
	env := ResolveEnv{Home: "/tmp/fake-home", GOOS: "darwin", DefaultPaths: refreshFakeProvider{}}

	// 缺省:0 → 30/30。
	eff, err := ResolveEffectiveConfig(&config.Config{}, env)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Refresh.DashboardInterval != 30 || eff.Refresh.WatchInterval != 30 {
		t.Errorf("缺省有效值应 30/30,实际 %+v", eff.Refresh)
	}

	// 显式配置:20/45 原样保留;用户层对象不被修改(深拷贝边界)。
	user := &config.Config{Refresh: config.RefreshConfig{DashboardInterval: 20, WatchInterval: 45}}
	eff, err = ResolveEffectiveConfig(user, env)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Refresh.DashboardInterval != 20 || eff.Refresh.WatchInterval != 45 {
		t.Errorf("显式配置应原样进入有效层,实际 %+v", eff.Refresh)
	}
	if user.Refresh.DashboardInterval != 20 || user.Refresh.WatchInterval != 45 {
		t.Errorf("ResolveEffectiveConfig 不得修改入参用户层,实际 %+v", user.Refresh)
	}
}

// TestRefreshValidation:读取链拒绝负值、容忍超上限(手工编辑不 brick 整机);
// 写入链拒绝负值与 >3600(与网页/TUI 下拉口径一致),0 与 1..3600 合法。
func TestRefreshValidation(t *testing.T) {
	cases := []struct {
		name     string
		refresh  config.RefreshConfig
		readOK   bool
		writeOK  bool
		contains string // 拒绝时错误信息应含的片段
	}{
		{name: "零值合法", refresh: config.RefreshConfig{}, readOK: true, writeOK: true},
		{name: "合法区间", refresh: config.RefreshConfig{DashboardInterval: 1, WatchInterval: 3600}, readOK: true, writeOK: true},
		{name: "负值双拒", refresh: config.RefreshConfig{DashboardInterval: -1}, contains: "dashboard_interval"},
		{name: "watch 负值双拒", refresh: config.RefreshConfig{WatchInterval: -5}, contains: "watch_interval"},
		{name: "超上限读容忍写拒绝", refresh: config.RefreshConfig{WatchInterval: 3601}, readOK: true, contains: "watch_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := &config.Config{Refresh: tc.refresh}
			readErr := ValidateUserConfig(user)
			writeErr := ValidateUserConfigForWrite(user)
			if tc.readOK && readErr != nil {
				t.Errorf("读链应容忍: %v", readErr)
			}
			if !tc.readOK && readErr == nil {
				t.Errorf("读链应拒绝")
			}
			if tc.writeOK && writeErr != nil {
				t.Errorf("写链应接受: %v", writeErr)
			}
			if !tc.writeOK {
				if writeErr == nil {
					t.Fatalf("写链应拒绝")
				}
				if !strings.Contains(writeErr.Error(), tc.contains) {
					t.Errorf("写链错误应含字段名 %q,实际: %v", tc.contains, writeErr)
				}
			}
		})
	}
}
