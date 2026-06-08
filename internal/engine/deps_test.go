package engine

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestNewDeps_Router_DisabledWhenNoRouter(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true}},
	}
	deps := NewDeps(cfg)
	if r := deps.RouterFor("claude"); r != nil {
		t.Error("未配置 router 时 RouterFor 应返回 nil")
	}
}

func TestNewDeps_Router_DisabledWhenClientOff(t *testing.T) {
	// client 关闭：装配时 NewDeps 仍按 routers.* 建 router（装配不检查 client enabled），
	// 由 RunCollect 的 clientCfg.Enabled 拦截。这里验证 routers 表装配本身正确。
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: false, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/tmp/cc.db"}},
	}
	deps := NewDeps(cfg)
	if r := deps.routers["cc_switch"]; r == nil {
		t.Error("cc_switch 配置了 dbPath 应被装配进 routers 表")
	}
}

func TestNewDeps_Router_Enabled(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/tmp/cc.db"}},
	}
	deps := NewDeps(cfg)
	r := deps.RouterFor("claude")
	if r == nil {
		t.Fatal("配置了 cc_switch 应返回非 nil")
	}
	if r.Name() != "cc_switch" {
		t.Errorf("Name = %q, want cc_switch", r.Name())
	}
}

func TestNewDeps_Router_ConfigMissing(t *testing.T) {
	// claude enabled + Router=cc_switch，但 routers.cc_switch 未配置 → RouterFor 返回 nil
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
	}
	deps := NewDeps(cfg)
	if deps.RouterFor("claude") != nil {
		t.Error("routers.cc_switch 缺失时 RouterFor 应返回 nil")
	}
}

func TestNewDeps_Router_EmptyDBPathNotAssembled(t *testing.T) {
	// routers.cc_switch 存在但 db_path 为空（漏填/误配）：NewDeps 不装配（与 analyzer 对齐），
	// 避免 CollectLogs 报「数据库路径未配置」刷屏错误记录
	cfg := &config.Config{
		Clients: map[string]config.Client{"claude": {Enabled: true, Router: "cc_switch"}},
		Routers: map[string]config.RouterConfig{"cc_switch": {}}, // DBPath 留空
	}
	deps := NewDeps(cfg)
	if deps.routers["cc_switch"] != nil {
		t.Errorf("expected nil router when cc_switch.db_path empty")
	}
}

// TestNewDeps_Router_NonClaudeClientHitsRouter router 泛化核心新行为：
// 非 claude client（如 codex）配置 Router=cc_switch 时也能命中 router。
// 通用化前 router 写死 claude，codex 无法触发 router 采集。
func TestNewDeps_Router_NonClaudeClientHitsRouter(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"codex": {Enabled: true, Router: "cc_switch"}, // 非 claude client
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: "/tmp/cc.db"}},
	}
	deps := NewDeps(cfg)
	if r := deps.RouterFor("codex"); r == nil {
		t.Fatal("codex 配置 Router=cc_switch 时应命中 router")
	}
}

// TestNewDeps_Router_NonStandardName 在路由名直接表示路由类型后已不适用：
// router 表名必须等于其实现类型名（如 cc_switch），非标准表名不会被装配。
// adapter.Name() 返回配置名的行为由 ccswitch_test.go 的
// TestCCSwitchAdapter_CollectLogs_RouterNameUsesConfiguredName 覆盖。

// TestNewDeps_RegistersZCodeCollector 验证 NewDeps collectors 切片含 zcode。
// 注册后 hasCollector("zcode") 返回 true，是 ValidateResult/RunRetryWithDeps 不把
// zcode 当未知 client 的前置条件（二者均靠 hasCollector 兜底）。
func TestNewDeps_RegistersZCodeCollector(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode": {Enabled: true, Paths: map[string]string{"db": "/tmp/zcode.db"}},
		},
	}
	deps := NewDeps(cfg)
	if !hasCollector(deps, "zcode") {
		t.Fatal("NewDeps 应注册 zcode collector")
	}
}

// TestValidateResult_ZCodeNotUnknown zcode 注册后，Matched=true 时 ValidateResult
// 不报「未知客户端」。文案同步更新由 grep 断言覆盖（见 task-4 report）。
func TestValidateResult_ZCodeNotUnknown(t *testing.T) {
	res := Result{Matched: true, Attempted: 1, Succeeded: 1}
	if err := ValidateResult("zcode", res); err != nil {
		t.Errorf("zcode 已注册不应报未知: %v", err)
	}
}

// TestNewDeps_RegistersAutoClawCollector 验证 NewDeps collectors 切片含 autoclaw。
func TestNewDeps_RegistersAutoClawCollector(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": "/tmp/autoclaw"}},
		},
	}
	deps := NewDeps(cfg)
	if !hasCollector(deps, "autoclaw") {
		t.Fatal("NewDeps 应注册 autoclaw collector")
	}
}

// TestValidateResult_AutoClawNotUnknown autoclaw 注册后，Matched=true 时 ValidateResult
// 不报「未知客户端」。
func TestValidateResult_AutoClawNotUnknown(t *testing.T) {
	res := Result{Matched: true, Attempted: 1, Succeeded: 1}
	if err := ValidateResult("autoclaw", res); err != nil {
		t.Errorf("autoclaw 已注册不应报未知: %v", err)
	}
}
