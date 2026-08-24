// Package engine 封装采集编排：依赖装配、采集主循环、重试主循环、结果校验。
// cli 表示层（cobra 命令）与守护进程共同复用本包，避免业务编排逻辑错位进表示层。
package engine

import (
	"errors"
	"fmt"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// Deps 封装采集所需的无状态依赖（cfg + collectors + routers 表）。
// collect 命令与守护进程在装配期各建一次复用，避免每次触发都重建
// 6 个 collector + N 个 router（它们仅持有 cfg、无连接/句柄，Collect/CollectLogs 每次现读现开）。
type Deps struct {
	cfg        *config.Config
	collectors []collector.Collector
	routers    map[string]collector.RouterAdapter // key = router 配置名
}

// NewDeps 装配采集依赖。按 router 表名（配置 key）实例化对应 adapter——
// 未来加路由中间件类型时，约定其表名并在此 switch 增 case。
func NewDeps(cfg *config.Config) *Deps {
	if cfg == nil {
		return &Deps{}
	}
	routers := make(map[string]collector.RouterAdapter)
	for name, rc := range cfg.Routers {
		switch name {
		case "cc_switch":
			// db_path 非空判定与 analyzer.setupFromConfig 对齐：避免装配一个 dbPath 为空、
			// 每次 CollectLogs 都报「数据库路径未配置」的 adapter（会导致 RecordError 刷屏）。
			if rc.DBPath != "" {
				routers[name] = collector.NewCCSwitchAdapter(name, rc, cfg)
			}
		}
	}
	return &Deps{
		cfg: cfg,
		collectors: []collector.Collector{
			collector.NewClaudeCollector(cfg),
			collector.NewOpenCodeCollector(cfg),
			collector.NewCodexCollector(cfg),
			collector.NewWorkBuddyCollector(cfg),
			collector.NewZCodeCollector(cfg),
			collector.NewAutoClawCollector(cfg),
		},
		routers: routers,
	}
}

// RouterFor 返回该 client 配置声明的 router；未配置或未装配返回 nil。
// 按 client.Router 配置驱动，不再写死 client 名。
// client 启用与否由 RunCollect 的 clientCfg.Enabled 判定拦截，本方法只查 routers 表。
func (d *Deps) RouterFor(clientName string) collector.RouterAdapter {
	if d == nil || d.cfg == nil {
		return nil
	}
	cc, ok := d.cfg.ClientConfig(clientName)
	if !ok || cc.Router == "" {
		return nil
	}
	return d.routers[cc.Router]
}

// Result 采集结果：匹配/尝试/成功计数与聚合错误
type Result struct {
	Matched   bool
	Attempted int
	Succeeded int
	Err       error
}

func (r Result) Complete() bool {
	return r.Attempted > 0 && r.Succeeded == r.Attempted && r.Err == nil
}

// unknownClientError 统一构造「未知客户端」双语错误（ValidateResult 与
// RunRetryWithDepsContext 共用，避免支持列表两处文案漂移）。
func unknownClientError(client string) error {
	return errors.New(ui.Bi(
		fmt.Sprintf("unknown client: %s (supported: claude, opencode, codex, workbuddy, zcode, autoclaw)", client),
		fmt.Sprintf("未知客户端: %s（支持: claude, opencode, codex, workbuddy, zcode, autoclaw）", client)))
}

// ValidateResult 校验采集结果语义
func ValidateResult(client string, result Result) error {
	if client != "" && !result.Matched {
		return unknownClientError(client)
	}
	if result.Attempted == 0 {
		if client != "" {
			return errors.New(ui.Bi(
				fmt.Sprintf("client %s is not enabled, check the enabled config", client),
				fmt.Sprintf("客户端 %s 未启用，请检查 enabled 配置", client)))
		}
		return errors.New(ui.Bi("no enabled clients, collection not executed", "没有已启用的客户端，未执行采集"))
	}
	if result.Err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("collection incomplete", "采集未完全成功"), result.Err)
	}
	if !result.Complete() {
		return errors.New(ui.Bi(
			fmt.Sprintf("collection incomplete: %d/%d sources succeeded", result.Succeeded, result.Attempted),
			fmt.Sprintf("采集未完全成功: %d/%d 个数据源成功", result.Succeeded, result.Attempted)))
	}
	return nil
}

func hasCollector(deps *Deps, name string) bool {
	if deps == nil {
		return false
	}
	for _, c := range deps.collectors {
		if c.Name() == name {
			return true
		}
	}
	return false
}

func collectorEnabled(deps *Deps, name string) bool {
	if deps == nil || deps.cfg == nil {
		return false
	}
	clientCfg, ok := deps.cfg.ClientConfig(name)
	return ok && clientCfg.Enabled
}

// NewDepsWithCollectors 供集成测试注入 collector/router 桩（Deps 字段是小写，
// 包外无法直接构造）。生产路径用 NewDeps（从配置装配真实 collector）。
func NewDepsWithCollectors(cfg *config.Config, collectors []collector.Collector, routers map[string]collector.RouterAdapter) *Deps {
	return &Deps{cfg: cfg, collectors: collectors, routers: routers}
}
