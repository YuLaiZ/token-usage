package configapp

import (
	"reflect"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// AnalyzeConfigEffects 是纯函数，比较两个已解析（effective）config。
// 测试直接构造 resolved 配置（路径已展开、默认值已补），不依赖 runtimecfg provider。
// 矩阵每一行 + 组合/去重/effective-diff 默认路径场景均独立用例。

// clientCfg 是构造测试用 client 的简写。
func clientCfg(enabled bool, router string, paths map[string]string) config.Client {
	return config.Client{Enabled: enabled, Router: router, Paths: paths}
}

// warningsContaining 报告 effects.Warnings 中是否所有 needle 都至少出现一次。
func warningsContaining(e ConfigEffects, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, w := range e.Warnings {
			if w == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ---- 矩阵每一行：表驱动 ----

func TestAnalyzeConfigEffects_Matrix(t *testing.T) {
	type cs struct {
		name               string
		prev               *config.Config
		curr               *config.Config
		wantRuntime        bool
		wantFull           []string
		wantRouter         []string
		wantWarnings       []string // 必须全部出现的 warning 子串集合
		wantDataDirMigrate bool
	}

	// 基线：一个已启用且配 router 的 claude，cc_switch router，data_dir=/d。
	base := func() *config.Config {
		return &config.Config{
			DataDir: "/d",
			Clients: map[string]config.Client{
				"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/old"}),
			},
			Routers: map[string]config.RouterConfig{
				"cc_switch": {DBPath: "/router.db"},
			},
		}
	}

	cases := []cs{
		{
			name:        "client disabled→enabled 全量采集该 client",
			prev:        &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "", map[string]string{"projects_dir": "/p"})}},
			curr:        &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/p"})}},
			wantRuntime: true,
			wantFull:    []string{"claude"},
		},
		{
			name:         "client disabled→enabled 且已配 router 仍只 full（router 被 full 去重）",
			prev:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "cc_switch", map[string]string{"projects_dir": "/p"})}},
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}}},
			wantRuntime:  true,
			wantFull:     []string{"claude"},
			wantRouter:   nil, // full 覆盖 router
			wantWarnings: nil,
		},
		{
			name:         "已启用 client path A→B 全量采集并警告旧路径历史",
			prev:         base(),
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/new"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/router.db"}}},
			wantRuntime:  true,
			wantFull:     []string{"claude"},
			wantWarnings: []string{warningOldPathHistoryNotDeleted("claude")},
		},
		{
			name:         "已禁用 client path A→B 无采集仅警告",
			prev:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "", map[string]string{"projects_dir": "/old"})}},
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "", map[string]string{"projects_dir": "/new"})}},
			wantRuntime:  true,
			wantWarnings: []string{warningDisabledClientPathChanged("claude")},
		},
		{
			name:         "client enabled→disabled 无采集警告已有历史",
			prev:         base(),
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "cc_switch", map[string]string{"projects_dir": "/old"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/router.db"}}},
			wantRuntime:  true,
			wantWarnings: []string{warningClientDisabledHistoryKept("claude")},
		},
		{
			name:        "已启用 client router 空→R router backfill 该 client",
			prev:        &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/p"})}},
			curr:        &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}}},
			wantRuntime: true,
			wantRouter:  []string{"claude"},
		},
		{
			name:         "已启用 client router R1→R2 router backfill 并警告旧关联",
			prev:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}}},
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch2", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch2": {DBPath: "/r2.db"}}},
			wantRuntime:  true,
			wantRouter:   []string{"claude"},
			wantWarnings: []string{warningRouterRebindOldAssoc("claude")},
		},
		{
			name:         "已禁用 client router 变化 无采集警告启用后再采",
			prev:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "cc_switch", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}}},
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(false, "cc_switch2", map[string]string{"projects_dir": "/p"})}, Routers: map[string]config.RouterConfig{"cc_switch2": {DBPath: "/r2.db"}}},
			wantRuntime:  true,
			wantWarnings: []string{warningDisabledClientRouterChanged("claude")},
		},
		{
			name:         "client router R→空 无采集警告已有关联不删",
			prev:         base(),
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/old"})}},
			wantRuntime:  true,
			wantWarnings: []string{warningRouterRemovedAssocKept("claude")},
		},
		{
			name:         "router db_path 变化 backfill 绑定该 router 的已启用 client",
			prev:         base(),
			curr:         &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/old"})}, Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/new-router.db"}}},
			wantRuntime:  true,
			wantRouter:   []string{"claude"},
			wantWarnings: []string{warningRouterDBPathAttribution("cc_switch")},
		},
		{
			name: "provider alias 新增 backfill 所有已启用且配 router 的 client",
			prev: &config.Config{
				DataDir: "/d",
				Clients: map[string]config.Client{
					"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"}),
					"codex":  clientCfg(true, "", map[string]string{"state_dir": "/s"}),
				},
				Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}},
			},
			curr: &config.Config{
				DataDir: "/d",
				Clients: map[string]config.Client{
					"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"}),
					"codex":  clientCfg(true, "", map[string]string{"state_dir": "/s"}),
				},
				Routers:         map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}},
				ProviderAliases: map[string]string{"anthropic": "Anthropic"},
			},
			wantRuntime:  true,
			wantRouter:   []string{"claude"}, // codex 无 router 不 backfill
			wantWarnings: []string{warningAliasAttribution},
		},
		{
			name:         "daemon poll_interval 变化 无采集 runtime changed",
			prev:         &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: 30}},
			curr:         &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: 60}},
			wantRuntime:  true,
			wantWarnings: nil,
		},
		{
			name:         "log level 变化 无采集 runtime changed",
			prev:         &config.Config{DataDir: "/d", Log: config.LogConfig{Level: "info"}},
			curr:         &config.Config{DataDir: "/d", Log: config.LogConfig{Level: "debug"}},
			wantRuntime:  true,
			wantWarnings: nil,
		},
		{
			name:         "log max_days 变化 无采集 runtime changed",
			prev:         &config.Config{DataDir: "/d", Log: config.LogConfig{MaxDays: 7}},
			curr:         &config.Config{DataDir: "/d", Log: config.LogConfig{MaxDays: 14}},
			wantRuntime:  true,
			wantWarnings: nil,
		},
		{
			name:         "log dir 变化 无采集 runtime changed 且警告日志不迁移",
			prev:         &config.Config{DataDir: "/d", Log: config.LogConfig{Dir: "/d/logs"}},
			curr:         &config.Config{DataDir: "/d", Log: config.LogConfig{Dir: "/new-logs"}},
			wantRuntime:  true,
			wantWarnings: []string{warningLogDirNotMigrated},
		},
		{
			name:         "仅 autostart 变化 无采集 runtime 不变",
			prev:         &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{AutoStart: false}},
			curr:         &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{AutoStart: true}},
			wantRuntime:  false,
			wantWarnings: nil,
		},
		{
			name:               "data_dir A→B 无采集 runtime changed 且生成迁移项",
			prev:               &config.Config{DataDir: "/old-data"},
			curr:               &config.Config{DataDir: "/new-data"},
			wantRuntime:        true,
			wantDataDirMigrate: true,
			wantWarnings:       []string{warningDataDirManualMigration},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AnalyzeConfigEffects(c.prev, c.curr)
			if got.RuntimeChanged != c.wantRuntime {
				t.Errorf("RuntimeChanged = %v, want %v", got.RuntimeChanged, c.wantRuntime)
			}
			if !sortedEqual(got.FullCollectClients, c.wantFull) {
				t.Errorf("FullCollectClients = %v, want %v", got.FullCollectClients, c.wantFull)
			}
			if !sortedEqual(got.RouterBackfillClients, c.wantRouter) {
				t.Errorf("RouterBackfillClients = %v, want %v", got.RouterBackfillClients, c.wantRouter)
			}
			if len(c.wantWarnings) > 0 && !warningsContaining(got, c.wantWarnings...) {
				t.Errorf("Warnings = %v, 缺少期望子集 %v", got.Warnings, c.wantWarnings)
			}
			if c.wantDataDirMigrate && got.DataDirMigration == nil {
				t.Errorf("期望 DataDirMigration 非 nil，got nil")
			}
			if !c.wantDataDirMigrate && got.DataDirMigration != nil {
				t.Errorf("期望 DataDirMigration nil，got %+v", got.DataDirMigration)
			}
		})
	}
}

// ---- 组合 / 去重 / 顺序 ----

// TestAnalyzeConfigEffects_FullAbsorbsRouterWhenSameClient 同一 client 同时命中 full 与 router：
// 只保留 full（router backfill 不含该 client）。
func TestAnalyzeConfigEffects_FullAbsorbsRouterWhenSameClient(t *testing.T) {
	prev := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{"claude": clientCfg(false, "cc_switch", map[string]string{"projects_dir": "/p"})},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}},
	}
	// disabled→enabled（full）+ 同时 router R1→R2（router backfill）+ path 变化（full）
	curr := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch2", map[string]string{"projects_dir": "/p2"})},
		Routers: map[string]config.RouterConfig{"cc_switch2": {DBPath: "/r2.db"}},
	}
	got := AnalyzeConfigEffects(prev, curr)
	if !sortedEqual(got.FullCollectClients, []string{"claude"}) {
		t.Errorf("FullCollectClients = %v, want [claude]", got.FullCollectClients)
	}
	if len(got.RouterBackfillClients) != 0 {
		t.Errorf("同一 client 已在 full，不应再进 RouterBackfillClients: %v", got.RouterBackfillClients)
	}
}

// TestAnalyzeConfigEffects_ClientsSortedAndDeduped 多 client 输出按稳定顺序排序并去重。
func TestAnalyzeConfigEffects_ClientsSortedAndDeduped(t *testing.T) {
	prev := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"zcode":    clientCfg(false, "", map[string]string{"db": "/z"}),
			"claude":   clientCfg(false, "", map[string]string{"projects_dir": "/c"}),
			"opencode": clientCfg(false, "", map[string]string{"db": "/o"}),
		},
	}
	curr := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"zcode":    clientCfg(true, "", map[string]string{"db": "/z"}),
			"claude":   clientCfg(true, "", map[string]string{"projects_dir": "/c"}),
			"opencode": clientCfg(true, "", map[string]string{"db": "/o"}),
		},
	}
	got := AnalyzeConfigEffects(prev, curr)
	want := []string{"claude", "opencode", "zcode"}
	if !sortedEqual(got.FullCollectClients, want) {
		t.Errorf("FullCollectClients = %v, want %v（稳定排序）", got.FullCollectClients, want)
	}
	// 严格校验顺序就是字典序
	if !reflect.DeepEqual(got.FullCollectClients, want) {
		t.Errorf("FullCollectClients 顺序错误: %v, want %v", got.FullCollectClients, want)
	}
}

// TestAnalyzeConfigEffects_RouterBackfillSortedAndDeduped router db_path 变化导致多 client backfill，
// 输出按稳定顺序排序。
func TestAnalyzeConfigEffects_RouterBackfillSortedAndDeduped(t *testing.T) {
	prev := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"zcode":  clientCfg(true, "cc_switch", map[string]string{"db": "/z"}),
			"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/c"}),
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}},
	}
	curr := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"zcode":  clientCfg(true, "cc_switch", map[string]string{"db": "/z"}),
			"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/c"}),
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r2.db"}},
	}
	got := AnalyzeConfigEffects(prev, curr)
	want := []string{"claude", "zcode"}
	if !reflect.DeepEqual(got.RouterBackfillClients, want) {
		t.Errorf("RouterBackfillClients = %v, want %v（稳定排序）", got.RouterBackfillClients, want)
	}
}

// ---- effective diff：写法变化但有效值不变 → 无动作 ----

// TestAnalyzeConfigEffects_NoActionWhenEffectiveUnchanged 同一 effective 配置（即使两个对象不同实例）
// 不产生任何 collect/runtime 变化。
func TestAnalyzeConfigEffects_NoActionWhenEffectiveUnchanged(t *testing.T) {
	mk := func() *config.Config {
		return &config.Config{
			DataDir: "/d",
			Daemon:  config.DaemonConfig{PollInterval: 30, AutoStart: true},
			Log:     config.LogConfig{Level: "info", Dir: "/d/logs", MaxDays: 7},
			Clients: map[string]config.Client{"claude": clientCfg(true, "cc_switch", map[string]string{"projects_dir": "/p"})},
			Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/r.db"}},
		}
	}
	got := AnalyzeConfigEffects(mk(), mk())
	if got.RuntimeChanged {
		t.Errorf("effective 未变，RuntimeChanged 应为 false")
	}
	if len(got.FullCollectClients) != 0 || len(got.RouterBackfillClients) != 0 {
		t.Errorf("effective 未变，不应有 collect 动作: full=%v router=%v", got.FullCollectClients, got.RouterBackfillClients)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("effective 未变，不应有 warning: %v", got.Warnings)
	}
	if got.DataDirMigration != nil {
		t.Errorf("effective 未变，不应有 DataDirMigration")
	}
}

// TestAnalyzeConfigEffects_DefaultPathChangeCaughtByEffectiveDiff 默认路径规则变化被 effective diff 捕获。
// 用户层两份配置写法不同，但 resolved 后 path 不同（这里直接传 resolved 值体现 effective diff）：
// claude.projects_dir 从 /old 变成 /new，视为已启用 client path 变化 → full collect。
func TestAnalyzeConfigEffects_DefaultPathChangeCaughtByEffectiveDiff(t *testing.T) {
	prev := &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/old"})}}
	curr := &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/new"})}}
	got := AnalyzeConfigEffects(prev, curr)
	if !sortedEqual(got.FullCollectClients, []string{"claude"}) {
		t.Errorf("默认路径变化应触发 full collect claude: %v", got.FullCollectClients)
	}
	if !got.RuntimeChanged {
		t.Errorf("默认路径变化应 RuntimeChanged=true")
	}
}

// TestAnalyzeConfigEffects_NilConfigsAreSafe prev/curr 为 nil 不 panic，按零值处理。
func TestAnalyzeConfigEffects_NilConfigsAreSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil config 不应 panic: %v", r)
		}
	}()
	// curr nil（视为空）与一个有 content 的 prev 比较：client enabled→disabled。
	prev := &config.Config{DataDir: "/d", Clients: map[string]config.Client{"claude": clientCfg(true, "", map[string]string{"projects_dir": "/p"})}}
	got := AnalyzeConfigEffects(prev, nil)
	if !got.RuntimeChanged {
		t.Errorf("client 消失应 RuntimeChanged=true")
	}
	if !warningsContaining(got, warningClientDisabledHistoryKept("claude")) {
		t.Errorf("应包含 client disabled 历史 warning: %v", got.Warnings)
	}
	// prev nil（视为空）与有 content 的 curr：client disabled→enabled。
	got2 := AnalyzeConfigEffects(nil, prev)
	if !sortedEqual(got2.FullCollectClients, []string{"claude"}) {
		t.Errorf("prev nil 时 client 出现应 full collect: %v", got2.FullCollectClients)
	}
}

// ---- DataDirMigration 内容 ----

// TestAnalyzeConfigEffects_DataDirMigrationContent data_dir 变化时 Items 说明手工迁移
// usage.db/logs，不迁移 PID/lock/runtime-state。
func TestAnalyzeConfigEffects_DataDirMigrationContent(t *testing.T) {
	prev := &config.Config{DataDir: "/old-data"}
	curr := &config.Config{DataDir: "/new-data"}
	got := AnalyzeConfigEffects(prev, curr)
	if got.DataDirMigration == nil {
		t.Fatalf("期望 DataDirMigration 非 nil")
	}
	if got.DataDirMigration.From != "/old-data" || got.DataDirMigration.To != "/new-data" {
		t.Errorf("DataDirMigration From/To = %q/%q, want /old-data//new-data", got.DataDirMigration.From, got.DataDirMigration.To)
	}
	if len(got.DataDirMigration.Items) == 0 {
		t.Errorf("DataDirMigration.Items 不应为空")
	}
	joined := strings.Join(got.DataDirMigration.Items, "|")
	for _, must := range []string{"usage.db", "logs"} {
		if !strings.Contains(joined, must) {
			t.Errorf("DataDirMigration.Items 应提及 %s: %v", must, got.DataDirMigration.Items)
		}
	}
	for _, mustNot := range []string{"PID", "lock", "runtime-state"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("DataDirMigration.Items 不应迁移 %s: %v", mustNot, got.DataDirMigration.Items)
		}
	}
}

// ---- helper ----

func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sortStrings(ac)
	sortStrings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
