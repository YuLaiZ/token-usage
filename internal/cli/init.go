package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// newInitCmd 构造 `config init` 子命令。
//
// 路径契约：
//   - 配置文件固定创建在 ~/.token-usage/config.toml（config.ConfigPath(home)），
//     不随 data_dir 变化——所有加载入口都读这个固定路径，在 data_dir 下另写一份
//     只会产生实际不生效的配置。
//   - 自定义 data_dir 只影响数据文件：usage.db（以及未来日志/PID/runtime-state）。
//   - 配置写入复用 fileutil.ReplaceCompleteFile，遵循统一完整文件替换契约。
//   - cobra.NoArgs：拒绝任何位置参数。
//   - 幂等：已存在的固定 config.toml 不覆盖；但缺失的数据文件（usage.db）仍会初始化。
func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "初始化配置和数据库",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("获取用户主目录失败: %w", err)
			}
			return runInit(cmdContext(cmd), cmd.OutOrStdout(), home)
		},
	}
}

func runInit(ctx context.Context, out io.Writer, home string) error {
	mgr, err := control.NewManager(home)
	if err != nil {
		return fmt.Errorf("创建进程控制管理器失败: %w", err)
	}
	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         goruntime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	cfgPath := config.ConfigPath(home)
	cfgDir := filepath.Dir(cfgPath)

	return mgr.WithLock(ctx, func(*control.Session) error {
		snap, err := runtimecfg.LoadUserConfigSnapshot(cfgPath)
		if err != nil {
			return fmt.Errorf("读取已有配置失败: %w", err)
		}
		created := false
		if !snap.Exists {
			if err := config.WriteDefaultConfig(cfgPath); err != nil {
				return fmt.Errorf("生成配置文件失败: %w", err)
			}
			created = true
		}

		// 无论新建还是已有配置，都必须经过同一校验与 effective 解析链。
		// 已有配置损坏时明确报错，不能静默改用默认 data_dir 初始化错误的数据库。
		effective, err := runtimecfg.LoadEffectiveConfig(cfgPath, env)
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}
		dataDir := effective.DataDir
		if dataDir == "" {
			return fmt.Errorf("配置中的 data_dir 不能为空")
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return fmt.Errorf("创建数据目录失败 (%s): %w", dataDir, err)
		}

		dbPath := filepath.Join(dataDir, "usage.db")
		usageDB, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("初始化数据库失败: %w", err)
		}
		if err := usageDB.Close(); err != nil {
			return fmt.Errorf("关闭初始化数据库失败: %w", err)
		}

		fmt.Fprintf(out, "✓ 配置目录: %s\n", cfgDir)
		if created {
			fmt.Fprintf(out, "✓ 生成配置: %s\n", cfgPath)
		} else {
			fmt.Fprintf(out, "- 配置已存在: %s\n", cfgPath)
		}
		fmt.Fprintf(out, "✓ 数据目录: %s\n", dataDir)
		fmt.Fprintf(out, "✓ 初始化数据库: %s\n", dbPath)
		if created {
			fmt.Fprintln(out, "\n初始化完成！默认配置未启用任何客户端，按需开启后再采集，例如：")
			fmt.Fprintln(out, "  token-usage config set clients.claude.enabled true")
		} else {
			fmt.Fprintln(out, "\n初始化完成！")
		}
		return nil
	})
}
