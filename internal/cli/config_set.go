// internal/cli/config_set.go
//
// config set 把「写盘 + 自启同步 + 运行状态验证 + 动作建议」整体委托给
// configapp.Application.ApplyConfig。CLI 层只负责：
//   - 读取用户层 snapshot（raw bytes + Config 同源），取 revision；
//   - 应用 dotted-key 草稿修改（内存，config.Set）；
//   - 调 ApplyConfig（control lock 内原子编排：revision 冲突保护、写盘、自启同步、
//     stale metadata 清理、动作建议生成）；
//   - 按 stdout/stderr 合同输出：稳定成功行写 stdout，动作建议/说明/warning 写 stderr。
//
// 不在 CLI 复制 effect 判断或自启判断（全交给 ApplyConfig）。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// configSetApplyFunc 是 config set 应用配置的可注入抽象。
// 生产包装 configapp.Application.ApplyConfig；测试注入 fake 锁定 stdout/stderr/exit 合同。
// 暴露为包级类型以便测试构造 fake。
type configSetApplyFunc func(
	ctx context.Context,
	expectedRevision []byte,
	currentUser *config.Config,
	confirmDataDirMigration bool,
) (configapp.ApplyConfigResult, error)

// configSetApplyFactory 是 configSetApplyFunc 的工厂（默认走真实 ApplyConfig）。
// 抽成包级变量使 newConfigSetCmd 的 RunE 可在生产装配真实 Application，测试可替换为 fake。
var configSetApplyFactory = func() (configSetApplyFunc, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         goruntime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}
	app, err := configapp.NewApplication(home, env, mgr, service.NewAutoStartManager())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create config application layer", "创建配置应用层失败"), err)
	}
	return func(ctx context.Context, expectedRevision []byte, currentUser *config.Config, confirm bool) (configapp.ApplyConfigResult, error) {
		return app.ApplyConfig(ctx, expectedRevision, currentUser, confirm)
	}, nil
}

func newConfigSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set one config value (dotted key, script-friendly) / 设置单项配置(dotted key,脚本友好)",
		Long: ui.Bi("Set one config value (dotted key, script-friendly).\n\n"+
			"The write is done atomically by configapp.ApplyConfig inside the process control lock: read the latest config, verify revision,\n"+
			"write to disk, sync autostart definitions, clean up stale data_dir leftovers, and generate structured suggested actions.\n\n"+
			"Output contract (script-friendly):\n"+
			"  - Stable success line `✓ <key> = <value>` goes to stdout;\n"+
			"  - Suggested actions (restart/collect), notes and warnings go to stderr.\n\n"+
			"revision protection: the config revision read at command start must match the on-disk revision re-read inside the lock,\n"+
			"otherwise it is treated as \"config modified by another process; nothing written\": no success line on stdout, non-zero exit.\n"+
			"After a conflict simply re-run the command — it re-reads the latest config and recomputes the revision automatically.\n\n"+
			"Partial failure: if the config is saved but autostart sync or leftover cleanup fails, stdout still gets the stable success line,\n"+
			"stderr gets the specific failure and the exit is non-zero (a saved result is never reported as a total failure).\n\n"+
			"data_dir changes require --confirm-migrate (usage.db/logs must be migrated manually and the daemon stopped first).\n\n"+
			"Collection commands in suggested actions follow the new syntax:\n"+
			"  token-usage collect all --client <name>     (full collection incl. the router phase)\n"+
			"  token-usage collect router --client <name>  (router attribution layer only)\n"+
			"See `token-usage collect --help` for more collection usage.",
			"设置单项配置（dotted key，脚本友好）。\n\n"+
				"写入由 configapp.ApplyConfig 在进程控制锁内原子完成：读取最新配置、校验 revision、\n"+
				"写盘、同步自启定义、清理旧 data_dir 残留，并生成结构化动作建议。\n\n"+
				"输出合同（便于脚本解析）：\n"+
				"  - 成功稳定行 `✓ <key> = <value>` 写 stdout；\n"+
				"  - 动作建议（restart/collect）、说明与 warning 写 stderr。\n\n"+
				"revision 保护：命令开始时读取的配置 revision 与锁内重读的磁盘 revision 必须一致，\n"+
				"否则判定「配置已被其他进程修改，本次未写入」，stdout 不写成功行、退出非零。\n"+
				"冲突后请直接重新执行命令——会自动重新读取最新配置并重算 revision，无需手动干预。\n\n"+
				"部分失败：配置已落盘但自启同步或残留清理失败时，stdout 仍写稳定成功行，\n"+
				"stderr 写具体失败并退出非零（已落盘结果不会被描述为完全失败）。\n\n"+
				"data_dir 变化需 --confirm-migrate（usage.db/logs 需手动迁移，且须先停守护进程）。\n\n"+
				"动作建议中的采集命令遵循新语法：\n"+
				"  token-usage collect all --client <name>     （含 router 阶段的全量采集）\n"+
				"  token-usage collect router --client <name>  （仅 router 归因层）\n"+
				"更多采集用法见 `token-usage collect --help`。"),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			confirmMigrate, _ := cmd.Flags().GetBool("confirm-migrate")
			applyFn, err := configSetApplyFactory()
			if err != nil {
				return err
			}
			return runConfigSet(cmdContext(cmd), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], args[1], confirmMigrate, applyFn)
		},
	}
	cmd.Flags().Bool("confirm-migrate", false, ui.Bi("Confirm data_dir migration (usage.db/logs must be migrated manually and the daemon stopped first)", "确认迁移 data_dir(需手动迁移 usage.db/logs 并先停守护进程)"))
	return cmd
}

// runConfigSet 是 config set 的核心逻辑（可注入 applyFn，便于测试锁定输出合同）。
//
// 步骤：
//  1. 读取用户层 snapshot（raw + Config 同源）；文件缺失提示先 config init。
//  2. expectedRevision = configapp.Revision(snapshot.Raw)。
//  3. 应用 dotted-key 草稿修改到 snapshot.Config（内存）；data_dir 需 confirm。
//  4. 调 applyFn（=ApplyConfig）做原子写盘 + 同步 + 动作建议。
//  5. 按合同写 stdout/stderr，映射错误。
func runConfigSet(
	ctx context.Context,
	out io.Writer,
	errOut io.Writer,
	key, value string,
	confirmMigrate bool,
	applyFn configSetApplyFunc,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if applyFn == nil {
		return errors.New(ui.Bi("config apply callback must not be empty", "配置应用回调不能为空"))
	}
	if out == nil || errOut == nil {
		return errors.New(ui.Bi("config command output must not be empty", "配置命令输出不能为空"))
	}
	// 1. 读取 snapshot（raw + Config 同源）。
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to get user home directory", "获取用户主目录失败"), err)
	}
	path := runtimecfg.ConfigPath(home)
	snap, err := runtimecfg.LoadUserConfigSnapshot(path)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
	}
	if !snap.Exists {
		return fmt.Errorf("%s %s %s", ui.Bi("config file", "配置文件"), path, ui.Bi("does not exist; run `token-usage config init` first", "不存在，请先执行 `token-usage config init`"))
	}

	// 2. expectedRevision 来自同一次读取的 raw bytes。
	expectedRevision := configapp.Revision(snap.Raw)

	// 3. 应用 dotted-key 草稿修改（内存）。data_dir 由 ApplyConfig 校验迁移前置条件，
	//    CLI 只需在 confirm 时把目标值写入内存 cfg。
	//    router 赋值前先做能力拦截：非空 router 只允许配在支持归因回填的客户端上
	//    （设为空字符串表示清除，始终放行）。key 用与 config.Set 相同的规则解析，
	//    使 clients."name".router 引号段写法与裸写法判定一致；解析失败的 key
	//    不在此拦，交由 config.Set 报错。
	if segs, err := config.ParseDottedKey(key); err == nil &&
		len(segs) == 3 && segs[0] == "clients" && segs[2] == "router" && value != "" {
		if !runtimecfg.ClientSupportsRouter(segs[1]) {
			return fmt.Errorf("client %q %s（%s %v %s）", segs[1], ui.Bi("does not support router attribution", "不支持 router 归因"), ui.Bi("currently only", "当前仅"), runtimecfg.RouterCapableClients(), ui.Bi("support it", "支持"))
		}
	}
	if err := config.Set(snap.Config, key, value); err != nil {
		if errors.Is(err, config.ErrDataDirNeedsConfirm) {
			if !confirmMigrate {
				return errors.New(ui.Bi("changing data_dir requires --confirm-migrate (usage.db/logs must be migrated manually and the daemon stopped first)", "修改 data_dir 需 --confirm-migrate 确认(usage.db/logs 需手动迁移,且须先停守护进程)"))
			}
			snap.Config.DataDir = value
		} else {
			return err
		}
	}

	// 4. 调 ApplyConfig（原子写盘 + 同步 + 动作建议）。
	result, applyErr := applyFn(ctx, expectedRevision, snap.Config, confirmMigrate)

	// 5. 错误映射 + 输出合同。
	// revision 冲突：stdout 不写成功行，stderr 给冲突与重试提示，退出非零。
	if errors.Is(applyErr, configapp.ErrConfigChangedExternally) {
		fmt.Fprintln(errOut, ui.Bi("Config modified by another process; nothing written. Re-run the command (it re-reads the latest config automatically).", "配置已被其他进程修改，本次未写入。请重新执行命令（会自动重新读取最新配置）。"))
		return applyErr
	}
	if applyErr != nil {
		// 区分「配置已保存后的部分失败」与「写入前/校验失败」。
		if result.ConfigApplied {
			// 部分失败：配置已落盘。stdout 写稳定成功行 + 「配置已保存」提示，
			// stderr 写具体失败，退出非零。
			writeSuccessStdout(out, key, value)
			if result.SuccessMessage != "" {
				fmt.Fprintln(out, result.SuccessMessage)
			}
			writeApplySuggestionsAndNotes(errOut, result)
			fmt.Fprintf(errOut, "%s: %v\n", ui.Bi("partial failure", "部分失败"), applyErr)
			return applyErr
		}
		// 写入前/校验失败：stdout 无成功行，错误向上传播（cobra 写 stderr）。
		return applyErr
	}

	// 成功路径：stdout 写稳定成功行。
	writeSuccessStdout(out, key, value)
	writeApplySuggestionsAndNotes(errOut, result)
	return nil
}

// writeSuccessStdout 写稳定成功行 `✓ <key> = <value>` 到 stdout。
func writeSuccessStdout(out io.Writer, key, value string) {
	fmt.Fprintf(out, "✓ %s = %s\n", key, value)
}

// writeApplySuggestionsAndNotes 把 ApplyConfig 返回的动作建议、说明、warning 写到 stderr。
// 稳定顺序：SuggestedSteps（动作建议）→ ExplanatoryNotes（说明，含自启/warning/迁移）。
func writeApplySuggestionsAndNotes(errOut io.Writer, result configapp.ApplyConfigResult) {
	if len(result.SuggestedSteps) > 0 {
		fmt.Fprintln(errOut, ui.Bi("Suggested next steps:", "建议后续步骤:"))
		for _, s := range result.SuggestedSteps {
			fmt.Fprintf(errOut, "  %s\n", s)
		}
	}
	for _, n := range result.ExplanatoryNotes {
		fmt.Fprintln(errOut, n)
	}
}
