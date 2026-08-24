package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
	"github.com/YuLaiZ/token-usage/internal/logger"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// cmdContext 返回 cobra 命令的 context；若命令未挂到根（如单测直接 newStartCmd().RunE），
// cmd.Context() 返回 nil，此处回退到 context.Background()，避免 nil 解引用。
func cmdContext(cmd interface{ Context() context.Context }) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}

// runtime 聚合 CLI 命令共享的运行时依赖（配置 + 日志 + DB）。
type runtime struct {
	cfg     *config.Config
	log     *slog.Logger
	usageDB *db.DB
}

// dbOpener 打开 usage DB 的可注入函数（测试覆盖以断言调用次数）。
// 默认为 db.Open；生产路径直接复用。
var dbOpener = db.Open

// newDepsFactory 装配 engine.Deps 的可注入函数（测试覆盖以断言调用次数）。
// 默认为 engine.NewDeps；collect 子命令与裸 collect 共用此工厂。
var newDepsFactory = engine.NewDeps

// defaultResolveEnv 构造生产用 ResolveEnv：home 取真实 os.UserHomeDir，goos 取 runtime.GOOS，
// 默认路径 provider 为 runtimecfg 标准实现。daemon/collect/analyzer 共用同一 resolver，
// 测试注入 fake provider 时不依赖开发机。
func defaultResolveEnv() (runtimecfg.ResolveEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return runtimecfg.ResolveEnv{}, fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	return runtimecfg.ResolveEnv{Home: home, GOOS: goruntime.GOOS, DefaultPaths: runtimecfg.NewStandardProvider()}, nil
}

// loadConfig 加载 effective config：runtimecfg.LoadEffectiveConfig 是 raw 与 effective 之间唯一解析边界，
// 内部固定执行 LoadUserConfigSnapshot → ValidateUserConfig → ResolveEffectiveConfig。
// 所有需要「有效配置」的命令路径（loadRuntime、config TUI 运行时层）都用此函数，
// collector 第二套默认值逻辑已删除，默认 paths/router db_path 由 runtimecfg 标准 provider 回填。
func loadConfig() (*config.Config, error) {
	env, err := defaultResolveEnv()
	if err != nil {
		return nil, err
	}
	return runtimecfg.LoadEffectiveConfig(runtimecfg.ConfigPath(env.Home), env)
}

// loadRuntime 装配 CLI 命令共用的运行时依赖：加载配置 → 初始化日志 → 打开 DB。
// 返回 cleanup，调用方须 defer cleanup() 以关闭 DB 与日志。
// 消除 collect/run 命令 RunE 中逐字重复的三段装配代码（加载配置/初始化日志/打开数据库）。
//
// 注意：本函数会直接打开 DB；凡需在打开 DB 前做 daemon 预检的 collect 路径
// 应使用 loadCollectRuntime（preflight 在 DB 打开之前）。
func loadRuntime() (*runtime, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	log, err := logger.Init(cfg.Log.Level, cfg.Log.Dir, cfg.Log.MaxDays)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", ui.Bi("failed to init logger", "初始化日志失败"), err)
	}
	usageDB, err := dbOpener(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		logger.Close()
		return nil, nil, fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	cleanup := func() {
		usageDB.Close()
		logger.Close()
	}
	return &runtime{cfg: cfg, log: log, usageDB: usageDB}, cleanup, nil
}
