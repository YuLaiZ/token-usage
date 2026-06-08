// internal/cli/config_tui.go
//
// config tui 把保存依赖收敛为单一 configapp.Application.ApplyConfig 入口。
// CLI 层只负责:
//   - 装配 Application(与 config set 同一工厂);
//   - 用 runtimecfg.LoadUserConfigSnapshot 做一次读取,同时初始化 TUI 的
//     draft(用户层)、diskRevision(expectedRevision 基准);
//   - 用 loadConfig() 加载 display(运行时层,显示参考);
//   - 把 ApplyConfig 包装成 tui.ApplyFunc(固定 confirmDataDirMigration=false,
//     data_dir 在 TUI 只读)。
//
// 不再注入独立的 write/sync/checker 回调;自启同步、revision 冲突保护、动作建议
// 全部由 ApplyConfig 在 control lock 内原子编排。TUI 不直接 import control/service
// (通过 ApplyFunc 解耦)。
package cli

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/tui"
)

// runConfigTUI 启动配置 TUI。
// 保存统一走 ApplyConfig:data_dir 只读(固定 confirm=false),revision 冲突、
// 自启同步、动作建议均由 ApplyConfig 编排并通过 ApplyConfigResult 回流 TUI。
func runConfigTUI() error {
	return runConfigTUIContext(context.Background())
}

func runConfigTUIContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("获取用户主目录失败: %w", err)
	}
	path := runtimecfg.ConfigPath(home)

	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         goruntime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return fmt.Errorf("创建进程控制管理器失败: %w", err)
	}

	created, err := ensureDefaultConfig(ctx, mgr, path)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("已生成默认配置:", path)
	}

	draft, display, diskRevision, err := loadTUIConfigState(path, env)
	if err != nil {
		return err
	}

	apply, err := newTUIApplyFuncWithManager(home, env, mgr)
	if err != nil {
		return err
	}
	return tui.Run(draft, display, diskRevision, apply)
}

func ensureDefaultConfig(ctx context.Context, mgr *control.Manager, path string) (bool, error) {
	created := false
	err := mgr.WithLock(ctx, func(*control.Session) error {
		_, statErr := os.Stat(path)
		switch {
		case statErr == nil:
			return nil
		case !os.IsNotExist(statErr):
			return fmt.Errorf("检查配置文件失败: %w", statErr)
		}
		if err := config.WriteDefaultConfig(path); err != nil {
			return fmt.Errorf("生成默认配置失败: %w", err)
		}
		created = true
		return nil
	})
	return created, err
}

// loadTUIConfigState 只读取一次 raw snapshot；draft、revision 与 display 都从这份
// snapshot 派生，避免两次读取拼出混合版本。
func loadTUIConfigState(
	path string,
	env runtimecfg.ResolveEnv,
) (draft, display *config.Config, revision []byte, err error) {
	snap, err := runtimecfg.LoadUserConfigSnapshot(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("加载配置失败: %w", err)
	}
	if !snap.Exists {
		return nil, nil, nil, fmt.Errorf("配置文件 %s 不存在，请先执行 `token-usage config init`", path)
	}
	if err := runtimecfg.ValidateUserConfig(snap.Config); err != nil {
		return nil, nil, nil, fmt.Errorf("配置校验失败: %w", err)
	}
	display, err = runtimecfg.ResolveEffectiveConfig(snap.Config, env)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("加载运行时配置失败: %w", err)
	}
	return snap.Config, display, configapp.Revision(snap.Raw), nil
}

// newTUIApplyFunc 装配 Application 并返回 tui.ApplyFunc。
// 与 configSetApplyFactory 同一装配模式(home/env/manager/autoStart);
// ApplyFunc 固定 confirmDataDirMigration=false(data_dir 在 TUI 只读)。
//
// ctx 用 context.Background: bubbletea 的 tea.Cmd 在后台 goroutine 求值,
// 无请求级 context;TUI 保存应在程序退出时中止,而 tea.Quit 已负责关闭 UI。
// ApplyConfig 内部的 control lock 有自己的超时,不依赖此处 ctx 的生命周期。
func newTUIApplyFunc(home string) (tui.ApplyFunc, error) {
	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         goruntime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("创建进程控制管理器失败: %w", err)
	}
	return newTUIApplyFuncWithManager(home, env, mgr)
}

func newTUIApplyFuncWithManager(
	home string,
	env runtimecfg.ResolveEnv,
	mgr *control.Manager,
) (tui.ApplyFunc, error) {
	app, err := configapp.NewApplication(home, env, mgr, service.NewAutoStartManager())
	if err != nil {
		return nil, fmt.Errorf("创建配置应用层失败: %w", err)
	}
	return func(expectedRevision []byte, currentUser *config.Config) (configapp.ApplyConfigResult, error) {
		return app.ApplyConfig(context.Background(), expectedRevision, currentUser, false)
	}, nil
}
