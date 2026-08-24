package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
	"github.com/YuLaiZ/token-usage/internal/logger"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// newCollectCmd 构造 collect 命令及其 all/router/retry 子命令。
//
// 命令树：
//
//	collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]   # 今天或指定日期，所有 enabled client（含 router）
//	├── all                                # 两阶段全采：messages 全历史 + router 全量回填
//	├── router --client X                  # 仅 router 全量回填（不动 messages）
//	└── retry                              # 重试未解决失败组
//
// flag 边界：
//   - --client：collect 的 PersistentFlag，被三个子命令继承。
//   - --force：collect 的 LocalFlag，子命令不继承、不接受（unknown flag）。
func newCollectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]",
		Short: "Collect token usage data (router included by default) / 采集 token 使用数据（默认含 router）",
		Long: ui.Bi(`Collect token usage data.

Without a subcommand it runs one incremental collection for the given date (today or the positional date arg) across all enabled clients, reading router logs and backfilling attribution along the way; --client X limits to one client, --force recollects.

Subcommands:
  all    Scan full history and backfill router attribution (two phases: messages → router)
  router Backfill router attribution for one client (does not invoke client collectors)
  retry  Retry unresolved failed groups in collection_errors

collect all already includes the router backfill; no need to run collect router separately.
`, `采集 token 使用数据。

不带子命令时按日期（今天或位置参数指定日期）对所有已启用客户端做一次增量采集，
采集过程中会同步读取 router 日志并回填归因；--client X 限定单客户端，--force 强制覆盖。

子命令：
  all    全量扫描历史消息并回填 router 归因（两阶段：messages → router）
  router 为指定客户端全量回填 router 归因（不调用 client collector）
  retry  重试 collection_errors 中未解决的失败组

collect all 已隐含包含 router backfill，无需再单独执行 collect router。
`),
		Args: cobra.MaximumNArgs(1),
		RunE: runCollectDefault,
	}

	// --client 为 PersistentFlag，三个子命令继承。
	cmd.PersistentFlags().String("client", "",
		ui.Bi("Limit to one client (claude/opencode/codex/workbuddy/zcode/autoclaw), inherited by subcommands", "指定客户端 (claude/opencode/codex/workbuddy/zcode/autoclaw)，子命令继承"))
	// --force 仅 collect 本身的 LocalFlag，子命令不继承。
	cmd.Flags().Bool("force", false, ui.Bi("Force recollection (ignore collection_log dedup)", "强制重新采集（忽略 collection_log 去重）"))

	cmd.AddCommand(newCollectAllCmd(), newCollectRouterCmd(), newCollectRetryCmd())
	return cmd
}

// runCollectDefault 裸 collect 路径：今天或指定日期采集，所有 enabled client（含 router）。
// --client X 限定单 client；--force 强制覆盖。**取消旧 collect --client X 无日期全采语义**：
// 无日期参数时只采今天，全采请使用 `collect all --client X`。
func runCollectDefault(cmd *cobra.Command, args []string) error {
	// 1. 早期解析：DB 打开前完成格式校验与默认日期计算。
	dates, err := parseDateArgs(args, true, "collect")
	if err != nil {
		return err
	}

	// 2. 加载配置 → daemon 预检（DB 打开之前）。
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	if handled, perr := collectPreflight(cfg, false, false); handled {
		return perr
	}

	force, _ := cmd.Flags().GetBool("force")
	client, _ := cmd.Flags().GetString("client")

	// 3. --client 存在性校验仍在运行时资源打开前完成。
	if client != "" {
		if err := validateClientExists(cfg, client); err != nil {
			return err
		}
	}

	// 4. 参数与冲突预检通过后才初始化 logger 与打开 DB。
	log, usageDB, cleanup, err := openCollectRuntime(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	deps := newDepsFactory(cfg)
	ctx := cmdContext(cmd)
	result := engine.RunCollect(ctx, deps, usageDB, log,
		cmd.OutOrStdout(), client, collector.CollectRequest{Dates: dates}, true, !force)
	return engine.ValidateResult(client, result)
}

// loadCollectConfig 完成所有 collect 变体共用的配置与 daemon 冲突预检。
func loadCollectConfig(force bool) (*config.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	if handled, perr := collectPreflight(cfg, false, force); handled {
		return nil, perr
	}
	return cfg, nil
}

// openCollectRuntime 只负责在参数与目标校验全部通过后初始化 logger 和 DB。
func openCollectRuntime(cfg *config.Config) (log *slog.Logger, usageDB *db.DB, cleanup func(), err error) {
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("%s", ui.Bi("valid config must not be empty", "有效配置不能为空"))
	}
	log, err = logger.Init(cfg.Log.Level, cfg.Log.Dir, cfg.Log.MaxDays)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", ui.Bi("failed to init logger", "初始化日志失败"), err)
	}
	usageDB, err = dbOpener(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		logger.Close()
		return nil, nil, nil, fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
	}
	cleanup = func() {
		usageDB.Close()
		logger.Close()
	}
	return log, usageDB, cleanup, nil
}

// loadCollectRuntime 保留为共用装配 helper；需要校验命令参数的调用方应先用
// loadCollectConfig，校验后再调用 openCollectRuntime。
// 任一阶段失败立即返回；preflight 通过后才打开 DB / 初始化 logger。
// 返回 (cfg, log, usageDB, cleanup)，调用方须 defer cleanup()。
func loadCollectRuntime(force bool) (cfg *config.Config, log *slog.Logger, usageDB *db.DB, cleanup func(), err error) {
	cfg, err = loadCollectConfig(force)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	log, usageDB, cleanup, err = openCollectRuntime(cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, log, usageDB, cleanup, nil
}

// validateClientExists 校验 --client 指定的客户端在配置中存在且 enabled。
// 不存在 → "未知客户端"；存在但 disabled → "已禁用"。
func validateClientExists(cfg *config.Config, client string) error {
	if cfg == nil {
		return fmt.Errorf("%s", ui.Bi("valid config must not be empty", "有效配置不能为空"))
	}
	cc, ok := cfg.ClientConfig(client)
	if !ok {
		return fmt.Errorf("%s: %s", ui.Bi("unknown client", "未知客户端"), client)
	}
	if !cc.Enabled {
		return fmt.Errorf("%s %s %s", ui.Bi("client", "客户端"), client, ui.Bi("is disabled; enable it in config first", "已禁用，请先在 config 中启用"))
	}
	return nil
}

// runOneFullCollect 单客户端全量消息采集（不采 router）。
// Dates=nil 触发 collector 全扫；skipCollected=false 走全量 upsert 覆盖。
func runOneFullCollect(ctx context.Context, deps *engine.Deps, usageDB *db.DB, log *slog.Logger, out io.Writer, client string) error {
	req := collector.CollectRequest{Dates: nil}
	result := engine.RunCollect(ctx, deps, usageDB, log, out, client, req, true, false)
	return engine.ValidateResult(client, result)
}

// checkDaemonConflict 检查守护进程是否冲突。
func checkDaemonConflict(lockPath string) error {
	if daemon.IsDaemonRunning(lockPath) {
		return fmt.Errorf("%s", ui.Bi("daemon is running; data is maintained by the daemon", "守护进程正在运行，数据由守护进程维护"))
	}
	return nil
}

// collectPreflight collect 命令前置检查：守护进程冲突检测先于任何 collect 变体
// （含 --force），确保不与守护进程并发写库。返回 handled=true 时调用方应直接返回 err。
func collectPreflight(cfg *config.Config, retry, force bool) (handled bool, err error) {
	if cfg == nil {
		return true, fmt.Errorf("%s", ui.Bi("valid config must not be empty", "有效配置不能为空"))
	}
	if err := checkDaemonConflict(filepath.Join(cfg.DataDir, "token-usage.lock")); err != nil {
		return true, err
	}
	return false, nil
}
