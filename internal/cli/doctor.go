package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newDoctorCmd() *cobra.Command {
	return newDoctorCmdWithDeps(loadConfig, dbOpener)
}

// newDoctorCmdWithDeps 构造 doctor 命令;load/open 可注入供包内测试真实 RunE
// 接线(生产路径传入 loadConfig 与 dbOpener,与 query/export 命令一致)。
func newDoctorCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks and report problems / 运行健康检查并报告问题",
		Long: ui.Bi(
			"Run read-only health checks and print one line per check (OK/WARN/FAIL) with a final summary. Checks: config, data directory (the writability probe creates exactly one temporary file and removes it immediately), database (SQLite quick_check plus message count), enabled clients, last successful collection, unresolved collection errors, query view definitions (subqueries/groups/default semantic validity, warnings only), and an informational pointer to `token-usage status` for daemon state. No business data is written: opening the database (journal-mode setup and schema migration) behaves exactly as in every other read command, and doctor itself performs no writes of its own; it never starts, stops, or restarts the daemon, and never modifies configuration. FAIL/WARN are report-only; the exit code is always 0 in v1.",
			"运行只读健康检查,逐项输出检查结果(OK/WARN/FAIL)并给出汇总。检查项:配置、数据目录(可写探针仅创建一个临时文件并立即删除)、数据库(SQLite quick_check 与消息行数)、已启用客户端、最近成功采集、未解决采集异常、查询视图定义(subqueries/groups/default 的语义合法性,仅警告),以及指向 `token-usage status` 的守护进程状态提示。不写业务数据:打开数据库的行为(journal 模式设置与 schema 迁移)与其它读取类命令一致,doctor 自身不执行任何特有的写操作;绝不启动/停止/重启守护进程,绝不修改配置。FAIL/WARN 仅体现在输出,v1 退出码恒为 0。",
		),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("doctor accepts no positional args (got %d)", len(args)),
					fmt.Sprintf("doctor 不接受位置参数(当前 %d 个)", len(args)),
				))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, load, open)
		},
	}
	return cmd
}

// doctorDBState 是 doctor 中数据库检查项的结论,决定后续依赖数据库的检查项
// 是执行还是跳过。
type doctorDBState int

const (
	// dbStateSkipped 表示数据库检查因上游(配置)失败未执行。
	dbStateSkipped doctorDBState = iota
	// dbStateMissing 表示 usage.db 文件不存在(已按 WARN 计)。
	dbStateMissing
	// dbStateBroken 表示数据库打开失败或 quick_check 未通过(已按 FAIL 计)。
	dbStateBroken
	// dbStateOK 表示数据库可打开且完整性检查通过。
	dbStateOK
)

// doctorLine 按固定格式输出一行检查结果:「标签: 状态 描述」;描述为空时省略。
func doctorLine(out io.Writer, label, status, desc string) {
	if desc == "" {
		fmt.Fprintf(out, "%s: %s\n", label, status)
		return
	}
	fmt.Fprintf(out, "%s: %s %s\n", label, status, desc)
}

// runDoctor 是 doctor 的执行入口:按固定顺序逐项检查并输出,最后汇总。
// 铁律:不写业务数据——打开数据库的行为(journal 模式设置与 schema 迁移)与
// 其它读取类命令一致,doctor 自身不执行任何特有的写操作;绝不启动/停止/重启
// 守护进程;绝不修改配置(数据目录可写性探针的临时文件即建即删)。
//
// 计数与退出码取舍:SKIPPED/INFO 不计入 warnings/failures——SKIPPED 表示检查
// 因上游失败无法执行(上游 FAIL 已计数),INFO 是纯提示;v1 退出码恒为 0,
// FAIL/WARN 只体现在报告里(RunE 返回 error 会被 cobra 冠以 Error: 前缀打到
// stderr,语义是命令失败而非体检结论;os.Exit 又会跳过 defer 的 DB 关闭)。
func runDoctor(cmd *cobra.Command, load func() (*config.Config, error), open func(string) (*db.DB, error)) error {
	out := cmd.OutOrStdout()
	ctx := cmdContext(cmd)

	statusOK := ui.Bi("OK", "正常")
	statusWarn := ui.Bi("WARN", "警告")
	statusFail := ui.Bi("FAIL", "失败")
	statusSkip := ui.Bi("SKIPPED", "跳过")
	statusInfo := ui.Bi("INFO", "提示")

	fmt.Fprintln(out, ui.Bi("Health check", "健康检查"))
	fmt.Fprintln(out)

	var warnings, failures int

	// 1. Config / 配置
	cfg, cfgErr := load()
	if cfgErr != nil {
		failures++
		doctorLine(out, ui.Bi("Config", "配置"), statusFail, cfgErr.Error())
	} else {
		// 路径展示复用 loadConfig 同一解析边界(runtimecfg.ConfigPath(home)),
		// 不在 doctor 内复制 ~ 展开逻辑;home 获取失败时仅缺省路径展示
		//(此时配置已加载成功,缺路径只是展示信息不完整,不算失败)。
		desc := ""
		if env, envErr := defaultResolveEnv(); envErr == nil {
			desc = runtimecfg.ConfigPath(env.Home)
		}
		doctorLine(out, ui.Bi("Config", "配置"), statusOK, desc)
	}
	configFailed := cfgErr != nil

	// 2. Data directory / 数据目录
	if configFailed {
		doctorLine(out, ui.Bi("Data directory", "数据目录"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	} else {
		// cfg.DataDir 已经 runtimecfg.LoadEffectiveConfig 展开 ~ 前缀,直接使用。
		dataDir := cfg.DataDir
		info, statErr := os.Stat(dataDir)
		switch {
		case statErr != nil:
			failures++
			doctorLine(out, ui.Bi("Data directory", "数据目录"), statusFail,
				ui.Bi("directory does not exist", "目录不存在")+" "+dataDir)
		case !info.IsDir():
			failures++
			doctorLine(out, ui.Bi("Data directory", "数据目录"), statusFail,
				ui.Bi("not a directory", "不是目录")+" "+dataDir)
		default:
			if probeErr := probeDataDirWritable(dataDir); probeErr != nil {
				failures++
				doctorLine(out, ui.Bi("Data directory", "数据目录"), statusFail,
					ui.Bi("not writable", "不可写")+": "+probeErr.Error())
			} else {
				doctorLine(out, ui.Bi("Data directory", "数据目录"), statusOK, dataDir)
			}
		}
	}

	// 3. Database / 数据库
	var usageDB *db.DB
	dbState := dbStateSkipped
	if configFailed {
		doctorLine(out, ui.Bi("Database", "数据库"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	} else {
		dbPath := filepath.Join(cfg.DataDir, "usage.db")
		info, statErr := os.Stat(dbPath)
		switch {
		case statErr != nil:
			warnings++
			dbState = dbStateMissing
			doctorLine(out, ui.Bi("Database", "数据库"), statusWarn,
				ui.Bi("not created yet (run collect to generate)", "尚未创建(运行 collect 后生成)"))
		case info.Size() == 0:
			// 0 字节文件是「文件已建但从未初始化 schema」的形态:与不存在同路径
			// 记 WARN 且不打开——db.Open 会对其做 journal 模式设置与 schema 迁移
			// (doctor 自身不特有的写操作),初始化留给 collect。
			warnings++
			dbState = dbStateMissing
			doctorLine(out, ui.Bi("Database", "数据库"), statusWarn,
				ui.Bi("empty database file (uninitialized); run collect to initialize", "空数据库文件(未初始化);运行 collect 后初始化"))
		default:
			opened, openErr := open(dbPath)
			if openErr != nil {
				failures++
				dbState = dbStateBroken
				doctorLine(out, ui.Bi("Database", "数据库"), statusFail, openErr.Error())
			} else {
				usageDB = opened
				defer usageDB.Close()
				quick, quickErr := sqliteQuickCheck(ctx, usageDB)
				switch {
				case quickErr != nil:
					failures++
					dbState = dbStateBroken
					doctorLine(out, ui.Bi("Database", "数据库"), statusFail, quickErr.Error())
				case quick != "ok":
					failures++
					dbState = dbStateBroken
					doctorLine(out, ui.Bi("Database", "数据库"), statusFail,
						ui.Bi("quick_check failed", "quick_check 未通过")+": "+quick)
				default:
					var messages int64
					if countErr := usageDB.QueryRowContext(ctx,
						"SELECT COUNT(*) FROM messages").Scan(&messages); countErr != nil {
						failures++
						dbState = dbStateBroken
						doctorLine(out, ui.Bi("Database", "数据库"), statusFail, countErr.Error())
					} else {
						dbState = dbStateOK
						doctorLine(out, ui.Bi("Database", "数据库"), statusOK,
							fmt.Sprintf("%s (quick_check: ok, %s: %d)", dbPath, ui.Bi("messages", "消息数"), messages))
					}
				}
			}
		}
	}

	// 4. Clients / 客户端
	if configFailed {
		doctorLine(out, ui.Bi("Clients", "客户端"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	} else {
		var enabled []string
		for name, client := range cfg.Clients {
			if client.Enabled {
				enabled = append(enabled, name)
			}
		}
		// map 遍历无序,排序保证输出确定。
		sort.Strings(enabled)
		if len(enabled) == 0 {
			warnings++
			doctorLine(out, ui.Bi("Clients", "客户端"), statusWarn,
				ui.Bi("no client enabled", "未启用任何客户端"))
		} else {
			doctorLine(out, ui.Bi("Clients", "客户端"), statusOK,
				fmt.Sprintf("%d (%s)", len(enabled), strings.Join(enabled, ", ")))
		}
	}

	// 5. Last collection / 最近采集
	switch {
	case configFailed:
		doctorLine(out, ui.Bi("Last collection", "最近采集"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		// 数据库缺失或损坏时最近采集无从谈起,上游项已计 WARN/FAIL,此处跳过不重复计数。
		doctorLine(out, ui.Bi("Last collection", "最近采集"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	default:
		// 复用 querier.Freshness 的最近成功采集查询(dates 为空即只查
		// collection_log 全库口径),时区语义与 query 统计信息区一致。
		fresh, err := querier.New(usageDB).Freshness(ctx, nil)
		if err != nil {
			failures++
			doctorLine(out, ui.Bi("Last collection", "最近采集"), statusFail, err.Error())
		} else if fresh.LastCollection.IsZero() {
			warnings++
			doctorLine(out, ui.Bi("Last collection", "最近采集"), statusWarn,
				ui.Bi("no successful collection recorded yet", "尚无成功采集记录"))
		} else {
			doctorLine(out, ui.Bi("Last collection", "最近采集"), statusOK,
				fresh.LastCollection.Format(time.DateTime))
		}
	}

	// 6. Unresolved errors / 未解决异常
	switch {
	case configFailed:
		doctorLine(out, ui.Bi("Unresolved errors", "未解决异常"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		doctorLine(out, ui.Bi("Unresolved errors", "未解决异常"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	default:
		errs, err := db.GetErrorsContext(ctx, usageDB, db.ErrorFilter{Unresolved: true})
		if err != nil {
			failures++
			doctorLine(out, ui.Bi("Unresolved errors", "未解决异常"), statusFail, err.Error())
		} else if len(errs) == 0 {
			doctorLine(out, ui.Bi("Unresolved errors", "未解决异常"), statusOK, ui.Bi("none", "无"))
		} else {
			warnings++
			doctorLine(out, ui.Bi("Unresolved errors", "未解决异常"), statusWarn, ui.Bi(
				fmt.Sprintf("%d unresolved; run `token-usage errors` for details, `token-usage collect retry` to retry", len(errs)),
				fmt.Sprintf("%d 条未解决;运行 `token-usage errors` 查看详情、`token-usage collect retry` 重试", len(errs)),
			))
		}
	}

	// 7. Query definitions / 查询视图:主动巡检配置的视图定义语义(default、
	// subqueries、groups),在使用路径报错之前提前发现坏定义。仅 WARN 不 FAIL:
	// 配置是纯展示态,坏定义不阻断采集与其他静态命令。
	switch {
	case configFailed:
		doctorLine(out, ui.Bi("Query definitions", "查询视图"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	default:
		if _, qdErr := querydef.ParseViews(querydef.Input{RawQuery: cfg.RawQuery}); qdErr != nil {
			warnings++
			var ve *querydef.ValidationError
			desc := qdErr.Error()
			if errors.As(qdErr, &ve) && len(ve.Issues) > 0 {
				desc = fmt.Sprintf("%d %s: %s", len(ve.Issues), ui.Bi("issue(s)", "项问题"), ve.Issues[0].Message)
			}
			doctorLine(out, ui.Bi("Query definitions", "查询视图"), statusWarn, ui.Bi(
				fmt.Sprintf("%s; run `token-usage query list` for details", desc),
				fmt.Sprintf("%s;运行 `token-usage query list` 查看详情", desc),
			))
		} else {
			doctorLine(out, ui.Bi("Query definitions", "查询视图"), statusOK, ui.Bi("definitions valid", "定义合法"))
		}
	}

	// 8. Daemon / 守护进程:固定输出提示行,不计入警告。
	// 取舍:现成的只读判活 helper 复用并不干净——control.NewManager 构造期即
	// MkdirAll 创建配置目录,daemon.IsDaemonRunning 经 flock TryLock 探测会在
	// 锁文件不存在时创建它、锁文件不可创建时又保守误判为运行中;两者均违背
	// doctor「绝不修改/写副作用」铁律或语义不清,故不探测,指向 status。
	// 本项不依赖配置,任何场景都输出。
	doctorLine(out, ui.Bi("Daemon", "守护进程"), statusInfo, ui.Bi(
		"run `token-usage status` for daemon state (this command never probes or controls the daemon)",
		"使用 `token-usage status` 查看守护进程状态(本命令绝不探测或操作守护进程)",
	))

	// 汇总行:FAIL 优先于 WARN;两者皆无才是一切正常。
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s: ", ui.Bi("Result", "结果"))
	switch {
	case failures > 0:
		fmt.Fprintln(out, ui.Bi(fmt.Sprintf("%d problems", failures), fmt.Sprintf("%d 项失败", failures)))
	case warnings > 0:
		fmt.Fprintln(out, ui.Bi(fmt.Sprintf("%d warnings", warnings), fmt.Sprintf("%d 项警告", warnings)))
	default:
		fmt.Fprintln(out, ui.Bi("OK", "一切正常"))
	}
	return nil
}

// probeDataDirWritable 对数据目录做可写探针:创建随机名临时文件即建即删。
// doctor 承诺绝不修改数据目录内容,探针在创建/关闭/删除任一步失败即判不可写;
// 各失败分支均经 removeProbe 尽力清理,不留下探针垃圾。
func probeDataDirWritable(dir string) error {
	probe := filepath.Join(dir, fmt.Sprintf(".token-usage-doctor-%d-%d", os.Getpid(), time.Now().UnixNano()))
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		removeProbe(probe)
		return err
	}
	if err := os.Remove(probe); err != nil {
		removeProbe(probe)
		return err
	}
	return nil
}

// removeProbe 尽力删除探针文件:失败重试一次(规避 Windows 句柄延迟释放等瞬态),
// 两次仍失败则放弃(调用方已把原始错误上报,残留经报告可见)。
func removeProbe(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path)
}

// sqliteQuickCheck 执行 PRAGMA quick_check 并拼接全部结果行:健康库返回 "ok"。
// quick_check 是纯读完整性检查,不修复不写入,符合 doctor 只读铁律。
func sqliteQuickCheck(ctx context.Context, usageDB *db.DB) (string, error) {
	rows, err := usageDB.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(line)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return sb.String(), nil
}
