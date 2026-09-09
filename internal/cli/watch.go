package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// watchLoopSentinel 是循环模式测试的终止哨兵:sleep 注入在计数达标后抛出,
// 外层 recover 断言帧数,避免测试真实死循环。
var watchLoopSentinel = fmt.Errorf("watch loop sentinel")

// watchFrameExecutor 把一帧的日期窗口渲染为 query 输出体。
// watch 不自建帧内容:帧体与对应 query 执行逐字节一致(统计信息区、视图表、
// 输出列布局、provider 别名与采集异常警告),watch 只负责壳与刷新。
type watchFrameExecutor func(ctx context.Context, out io.Writer, dates []string) error

func newWatchCmd() *cobra.Command {
	return newWatchCmdWithDeps(loadConfig, db.Open, time.Now, time.Sleep)
}

func newWatchCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error),
	now func() time.Time, sleep func(time.Duration)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch [DATE|DATE-DATE]",
		Short: "Refresh the query output at a fixed interval / 以固定间隔刷新 query 输出",
		Long: ui.Bi(
			"Refresh the query output for a window at a fixed interval. View selection matches `token-usage query`: with no --by, the default view runs (query.default, built-in fallback client); --by accepts a built-in view (client, model, provider, project, day, month, hour, weekday, heatmap, session, summary) or a configured view name from query.subqueries/query.groups. The frame body is exactly the query output: statistics header, view tables with the configured output columns and provider aliases, plus collection error warnings. DATE is a day (YYYYMMDD), month (YYYYMM), or year (YYYY; single arg only); DATE-DATE is an inclusive range whose endpoints are days or months; with no date the frame tracks today, recomputed every refresh so it rolls over midnight automatically.",
			"以固定间隔刷新某个窗口的 query 输出。视图选择与 `token-usage query` 一致：不带 --by 时执行默认视图（query.default，内置回退 client）；--by 接受内置视图（client、model、provider、project、day、month、hour、weekday、heatmap、session、summary）或 query.subqueries/query.groups 中已配置的视图名。帧体与 query 输出完全一致：统计信息区、应用输出列布局与 provider 别名的视图表，以及采集异常警告。DATE 为日 YYYYMMDD、月 YYYYMM 或年 YYYY（年仅单独使用）；DATE-DATE 为闭区间，端点为日或月；不带日期时帧跟随今天，每次刷新重算，跨午夜自动切换。"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 缺省日期与刷新时间戳同源,都取注入时钟:watch 的 now 是完整
			// 语义注入,缺省"今天"若绕开它读真实时钟,测试注入即失效。
			// 缺省日期逐帧重算(跨午夜后帧自动切到新的一天);显式指定的
			// 日期区间保持固定——监视历史区间是合法用法。
			defaultToday := len(args) == 0
			var fixedDates []string
			if !defaultToday {
				var err error
				fixedDates, err = parseDateArgs(args, false, "watch")
				if err != nil {
					return err
				}
			}
			interval, err := cmd.Flags().GetDuration("interval")
			if err != nil {
				return err
			}
			if interval < time.Second {
				return fmt.Errorf("%s", ui.Bi(
					"--interval must be at least 1s",
					"--interval 至少为 1 秒",
				))
			}
			once, _ := cmd.Flags().GetBool("once")

			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}

			// 视图分派与 query 命令族同一解析合同:
			//   - 未传 --by:完整解析,执行 query.default(内置回退 client),
			//     视图定义错误在此路径拒绝;
			//   - 显式内置视图名:静态路径,只解析 query.output,无关的视图
			//     定义错误不阻断(与 query client 等静态子命令一致);
			//   - 其他值:完整解析后按配置视图名解析;两者皆非在开库前拒绝。
			by, err := cmd.Flags().GetString("by")
			if err != nil {
				return err
			}
			bySet := cmd.Flags().Changed("by")
			var usageDB *db.DB
			var exec watchFrameExecutor
			switch {
			case !bySet:
				defs, err := parseQueryDefinitions(cfg)
				if err != nil {
					return err
				}
				exec = func(ctx context.Context, out io.Writer, dates []string) error {
					return executeTargetQuery(ctx, out, usageDB, dates, defs, defs.Default, cfg.ProviderAliases)
				}
			default:
				if view, ok := queryBuiltinView(by); ok {
					// --by summary 与 `query summary` 同一边界:完全不解析
					// query.output,坏布局不阻断(Summary 渲染不消费布局);
					// 其余内置视图与静态表格命令一致,只解析输出布局,
					// 无关的视图定义错误不阻断。
					var layout []string
					if view != viewSummary {
						var err error
						layout, err = staticTableOutputLayout(cfg)
						if err != nil {
							return err
						}
					}
					exec = func(ctx context.Context, out io.Writer, dates []string) error {
						return executeQueryDatesWithAliases(ctx, out, usageDB, dates, view, cfg.ProviderAliases, layout)
					}
				} else {
					defs, err := parseQueryDefinitions(cfg)
					if err != nil {
						return err
					}
					target, ok := resolveTarget(defs, by)
					if !ok {
						return fmt.Errorf("%s", ui.Bi(
							fmt.Sprintf("unknown --by view %q (allowed: %s)", by, watchAllowedViews(defs)),
							fmt.Sprintf("未知 --by 视图 %q(允许：%s)", by, watchAllowedViews(defs)),
						))
					}
					exec = func(ctx context.Context, out io.Writer, dates []string) error {
						return executeTargetQuery(ctx, out, usageDB, dates, defs, target, cfg.ProviderAliases)
					}
				}
			}

			usageDB, err = open(filepath.Join(cfg.DataDir, "usage.db"))
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to open database", "打开数据库失败"), err)
			}
			defer usageDB.Close()

			// 交互式循环前启用 ANSI 转义(Windows conhost 需显式开启)。
			if !once {
				enableVT(os.Stdout)
			}

			ctx := cmdContext(cmd)
			out := cmd.OutOrStdout()
			// 单帧渲染:错误即返回(数据库损坏等)而非静默闪烁。缺省模式的
			// 帧日期在此重算,保证跨午夜切日。
			render := func() error {
				dates := fixedDates
				if defaultToday {
					dates = []string{now().Format("2006-01-02")}
				}
				stamp := now().Format("15:04:05")
				var frame bytes.Buffer
				frame.WriteString(ui.Bi(
					fmt.Sprintf("Live watch - refreshed at %s (Ctrl+C to exit)", stamp),
					fmt.Sprintf("实时监视 - 刷新于 %s(Ctrl+C 退出)", stamp)))
				frame.WriteString("\n\n")
				if err := exec(ctx, &frame, dates); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(out, strings.TrimRight(frame.String(), "\n")); err != nil {
					return err
				}
				return nil
			}

			if once {
				return render()
			}

			for {
				// 清屏并归位光标:每帧从左上角重绘,避免残影。
				fmt.Fprint(out, "\x1b[2J\x1b[H")
				if err := render(); err != nil {
					return err
				}
				sleep(interval)
			}
		},
	}

	cmd.Flags().Duration("interval", 5*time.Second, ui.Bi("Refresh interval (minimum 1s)", "刷新间隔(至少 1 秒)"))
	cmd.Flags().Bool("once", false, ui.Bi("Render a single frame and exit", "只渲染一帧后退出"))
	cmd.Flags().String("by", "", ui.Bi("Frame view: built-in view (client/model/provider/project/day/month/hour/weekday/heatmap/session/summary) or a configured view name; defaults to query.default", "帧内视图：内置视图（client/model/provider/project/day/month/hour/weekday/heatmap/session/summary）或已配置视图名；缺省跟随 query.default"))
	return cmd
}

// queryBuiltinView 把内置视图名映射为 queryView;名单即 query 内置子命令集合,
// watch --by 的内置取值空间与其完全一致。
func queryBuiltinView(name string) (queryView, bool) {
	for _, meta := range queryBuiltinCmds {
		if meta.name == name {
			return meta.view, true
		}
	}
	return 0, false
}

// configuredViewNames 渲染「内置视图名 + 已配置视图名(subqueries 与 groups,
// 各自字节序)」的动态允许集合;builtinNames 由调用方传入(export 与 watch
// 的内置集合不同)。
func configuredViewNames(builtinNames []string, defs *querydef.QueryDefinitions) string {
	names := make([]string, 0, len(builtinNames)+len(defs.Subqueries)+len(defs.Groups))
	names = append(names, builtinNames...)
	for _, s := range defs.Subqueries {
		names = append(names, s.Name)
	}
	for _, g := range defs.Groups {
		names = append(names, g.Name)
	}
	return strings.Join(names, ", ")
}

// watchAllowedViews 渲染 watch --by 错误信息中的动态允许集合:
// query 内置视图名 + 已配置视图名。
func watchAllowedViews(defs *querydef.QueryDefinitions) string {
	builtin := make([]string, 0, len(queryBuiltinCmds))
	for _, meta := range queryBuiltinCmds {
		builtin = append(builtin, meta.name)
	}
	return configuredViewNames(builtin, defs)
}
