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
	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

func newDoctorCmd() *cobra.Command {
	return newDoctorCmdWithDeps(loadConfig, dbOpener)
}

// newDoctorCmdWithDeps 构造 doctor 命令;load/open 可注入供包内测试真实 RunE
// 接线(生产路径传入 loadConfig 与 dbOpener)。
func newDoctorCmdWithDeps(load func() (*config.Config, error), open func(string) (*db.DB, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks and report problems / 运行健康检查并报告问题",
		Long: ui.Bi(
			"Run read-only health checks and print one line per check (OK/WARN/FAIL) with a final summary. Checks: config, data directory (the writability probe creates exactly one temporary file and removes it immediately), database (SQLite quick_check plus message count), enabled clients, last successful collection, data freshness (WARN when the last collection is more than seven days old), date consistency (WARN when stored dates disagree with the local dates recomputed from message timestamps; report-only, no auto-fix), unresolved collection errors, query view definitions (subqueries/groups/default semantic validity, warnings only), a read-only dashboard probe (serve.json plus /api/meta liveness — INFO when not running, OK when answering, WARN for a stale or corrupt state file with cleanup pointed at `token-usage serve status`), and an informational pointer to `token-usage daemon status` for daemon state. No business data is written: opening the database (journal-mode setup and schema migration) behaves exactly as in every other read command, and doctor itself performs no writes of its own; it never starts, stops, or restarts the daemon, and never modifies configuration. With `--format json` the same checks and summary are emitted as a machine-readable JSON document (stable check ids, status in a closed set of ok/warn/fail/skipped/info, bilingual detail strings identical to the table lines). FAIL/WARN are report-only; the exit code is always 0 in v1.",
			"运行只读健康检查,逐项输出检查结果(OK/WARN/FAIL)并给出汇总。检查项:配置、数据目录(可写探针仅创建一个临时文件并立即删除)、数据库(SQLite quick_check 与消息行数)、已启用客户端、最近成功采集、数据新鲜度(最近采集距今超过七天告警)、日期一致性(date 列与按 ts 毫秒重算的本地日期不一致时告警,仅报告不自动修复)、未解决采集异常、查询视图定义(subqueries/groups/default 的语义合法性,仅警告)、只读仪表板探测(读 serve.json 并探活 /api/meta——未运行为 INFO、应答为 OK、陈旧或损坏状态为 WARN 并指向 `token-usage serve status` 清理),以及指向 `token-usage daemon status` 的守护进程状态提示。不写业务数据:打开数据库的行为(journal 模式设置与 schema 迁移)与其它读取类命令一致,doctor 自身不执行任何特有的写操作;绝不启动/停止/重启守护进程,绝不修改配置。`--format json` 把同一批检查项与汇总输出为机器可读的 JSON 文档（稳定的检查项 id、取值封闭的 status（ok/warn/fail/skipped/info）、与 table 行一致的双语 detail）。FAIL/WARN 仅体现在输出,v1 退出码恒为 0。",
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
	cmd.Flags().String("format", "table", ui.Bi(
		"Output format: table (human-readable report) or json (machine-readable checks plus summary)",
		"输出格式：table（人读报告）或 json（机器可读的检查项与汇总）",
	))
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
	format, _ := cmd.Flags().GetString("format")
	switch format {
	case "", "table", "json":
	default:
		return fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("invalid --format %q (allowed: table, json)", format),
			fmt.Sprintf("无效的 --format %q（允许：table、json）", format),
		))
	}
	// checks 收集全部检查项的结构化结果:table 模式逐行渲染(与既有输出逐字节
	// 一致),json 模式经 doctorReport 序列化(marshalExportJSON,两空格缩进)。
	// statusXxx 显示串仅用于 table 渲染;emit 传入的是同名的 doctorStatus 常量。
	var checks []doctorCheck
	emit := func(id, label string, status doctorStatus, detail string) {
		checks = append(checks, doctorCheck{ID: id, Label: label, Status: status, Detail: detail})
	}
	statusText := map[doctorStatus]string{
		statusOK:   ui.Bi("OK", "正常"),
		statusWarn: ui.Bi("WARN", "警告"),
		statusFail: ui.Bi("FAIL", "失败"),
		statusSkip: ui.Bi("SKIPPED", "跳过"),
		statusInfo: ui.Bi("INFO", "提示"),
	}

	out := cmd.OutOrStdout()
	ctx := cmdContext(cmd)

	// 标题与空行仅属于 table 报告;json 载荷必须是可直连解析的纯 JSON。
	if format != "json" {
		fmt.Fprintln(out, ui.Bi("Health check", "健康检查"))
		fmt.Fprintln(out)
	}

	var warnings, failures int

	// 1. Config / 配置
	cfg, cfgErr := load()
	if cfgErr != nil {
		failures++
		emit("config", ui.Bi("Config", "配置"), statusFail, cfgErr.Error())
	} else {
		// 路径展示复用 loadConfig 同一解析边界(runtimecfg.ConfigPath(home)),
		// 不在 doctor 内复制 ~ 展开逻辑;home 获取失败时仅缺省路径展示
		//(此时配置已加载成功,缺路径只是展示信息不完整,不算失败)。
		desc := ""
		if env, envErr := defaultResolveEnv(); envErr == nil {
			desc = runtimecfg.ConfigPath(env.Home)
		}
		emit("config", ui.Bi("Config", "配置"), statusOK, desc)
	}
	configFailed := cfgErr != nil

	// 2. Data directory / 数据目录
	if configFailed {
		emit("data_directory", ui.Bi("Data directory", "数据目录"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	} else {
		// cfg.DataDir 已经 runtimecfg.LoadEffectiveConfig 展开 ~ 前缀,直接使用。
		dataDir := cfg.DataDir
		info, statErr := os.Stat(dataDir)
		switch {
		case statErr != nil:
			failures++
			emit("data_directory", ui.Bi("Data directory", "数据目录"), statusFail, ui.Bi("directory does not exist", "目录不存在")+" "+dataDir)
		case !info.IsDir():
			failures++
			emit("data_directory", ui.Bi("Data directory", "数据目录"), statusFail, ui.Bi("not a directory", "不是目录")+" "+dataDir)
		default:
			if probeErr := probeDataDirWritable(dataDir); probeErr != nil {
				failures++
				emit("data_directory", ui.Bi("Data directory", "数据目录"), statusFail, ui.Bi("not writable", "不可写")+": "+probeErr.Error())
			} else {
				emit("data_directory", ui.Bi("Data directory", "数据目录"), statusOK, dataDir)
			}
		}
	}

	// 3. Database / 数据库
	var usageDB *db.DB
	dbState := dbStateSkipped
	// messages 提到外层声明:检查 7「日期一致性」的 OK 描述复用同一份消息
	// 计数,避免重复查询;仅在 dbState 达到 OK 时被赋值,后续检查读它前已按
	// dbState 分派,不会读到零值假象。
	var messages int64
	if configFailed {
		emit("database", ui.Bi("Database", "数据库"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	} else {
		dbPath := filepath.Join(cfg.DataDir, "usage.db")
		info, statErr := os.Stat(dbPath)
		switch {
		case statErr != nil:
			warnings++
			dbState = dbStateMissing
			emit("database", ui.Bi("Database", "数据库"), statusWarn, ui.Bi("not created yet (run collect to generate)", "尚未创建(运行 collect 后生成)"))
		case info.Size() == 0:
			// 0 字节文件是「文件已建但从未初始化 schema」的形态:与不存在同路径
			// 记 WARN 且不打开——db.Open 会对其做 journal 模式设置与 schema 迁移
			// (doctor 自身不特有的写操作),初始化留给 collect。
			warnings++
			dbState = dbStateMissing
			emit("database", ui.Bi("Database", "数据库"), statusWarn, ui.Bi("empty database file (uninitialized); run collect to initialize", "空数据库文件(未初始化);运行 collect 后初始化"))
		default:
			opened, openErr := open(dbPath)
			if openErr != nil {
				failures++
				dbState = dbStateBroken
				emit("database", ui.Bi("Database", "数据库"), statusFail, openErr.Error())
			} else {
				usageDB = opened
				defer usageDB.Close()
				quick, quickErr := sqliteQuickCheck(ctx, usageDB)
				switch {
				case quickErr != nil:
					failures++
					dbState = dbStateBroken
					emit("database", ui.Bi("Database", "数据库"), statusFail, quickErr.Error())
				case quick != "ok":
					failures++
					dbState = dbStateBroken
					emit("database", ui.Bi("Database", "数据库"), statusFail, ui.Bi("quick_check failed", "quick_check 未通过")+": "+quick)
				default:
					if countErr := usageDB.QueryRowContext(ctx,
						"SELECT COUNT(*) FROM messages").Scan(&messages); countErr != nil {
						failures++
						dbState = dbStateBroken
						emit("database", ui.Bi("Database", "数据库"), statusFail, countErr.Error())
					} else {
						dbState = dbStateOK
						emit("database", ui.Bi("Database", "数据库"), statusOK, fmt.Sprintf("%s (quick_check: ok, %s: %d)", dbPath, ui.Bi("messages", "消息数"), messages))
					}
				}
			}
		}
	}

	// 4. Clients / 客户端
	if configFailed {
		emit("clients", ui.Bi("Clients", "客户端"), statusSkip, ui.Bi("config failed", "配置加载失败"))
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
			emit("clients", ui.Bi("Clients", "客户端"), statusWarn, ui.Bi("no client enabled", "未启用任何客户端"))
		} else {
			emit("clients", ui.Bi("Clients", "客户端"), statusOK, fmt.Sprintf("%d (%s)", len(enabled), strings.Join(enabled, ", ")))
		}
	}

	// 5. Last collection / 最近采集
	// fresh 提到 switch 之外声明,供下一项「数据新鲜度」复用同一份查询结果,
	// 避免对 collection_log 重复查询;freshKnown 区分「查询失败」与「查询成功
	// 但无记录」两种零值形态,下一项按各自语义跳过(查询失败时上游已计 FAIL,
	// 不重复计数)。
	var fresh querier.Freshness
	freshKnown := false
	switch {
	case configFailed:
		emit("last_collection", ui.Bi("Last collection", "最近采集"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		// 数据库缺失或损坏时最近采集无从谈起,上游项已计 WARN/FAIL,此处跳过不重复计数。
		emit("last_collection", ui.Bi("Last collection", "最近采集"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	default:
		// 复用 querier.Freshness 的最近成功采集查询(dates 为空即只查
		// collection_log 全库口径),时区语义与 query 统计信息区一致。
		f, err := querier.New(usageDB).Freshness(ctx, nil)
		if err != nil {
			failures++
			emit("last_collection", ui.Bi("Last collection", "最近采集"), statusFail, err.Error())
		} else {
			fresh = f
			freshKnown = true
			if fresh.LastCollection.IsZero() {
				warnings++
				emit("last_collection", ui.Bi("Last collection", "最近采集"), statusWarn, ui.Bi("no successful collection recorded yet", "尚无成功采集记录"))
			} else {
				emit("last_collection", ui.Bi("Last collection", "最近采集"), statusOK, fresh.LastCollection.Format(time.DateTime))
			}
		}
	}

	// 6. Data freshness / 数据新鲜度:复用上一项取出的 fresh,判断最近采集
	// 是否超过陈旧阈值(阈值取舍见 doctorStaleThreshold)。
	switch {
	case configFailed:
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		// 上游数据库项已计 WARN/FAIL,此处跳过不重复计数。
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	case !freshKnown:
		// Freshness 查询失败:上一项 Last collection 已 FAIL,此处按
		// 「无法获取」跳过,不误报为无采集记录,也不重复计数。
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusSkip, ui.Bi("unavailable", "无法获取"))
	case fresh.LastCollection.IsZero():
		// 无采集记录:上一项 Last collection 已 WARN,此处跳过不重复计数。
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusSkip, ui.Bi("no collection recorded", "无采集记录"))
	case doctorFreshnessStale(time.Since(fresh.LastCollection)):
		warnings++
		days := int(time.Since(fresh.LastCollection).Hours() / 24)
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusWarn, ui.Bi(
			fmt.Sprintf("last collection %d d ago; run `token-usage collect` to refresh", days),
			fmt.Sprintf("最近采集距今 %d 天；运行 `token-usage collect` 刷新", days),
		))
	default:
		emit("data_freshness", ui.Bi("Data freshness", "数据新鲜度"), statusOK, doctorFreshnessDesc(time.Since(fresh.LastCollection)))
	}

	// 7. Date consistency / 日期一致性:messages.date 是采集时按本地时区归属
	// 写入的 YYYY-MM-DD,应与按 ts 毫秒重算的本地日期一致;不一致说明存在时区
	// 变更、时钟异常或数据被直接修改,按日统计的归属会失真。仅 WARN 不自动
	// 修复(修复需重写业务数据,违背 doctor 只读铁律);消息总数复用检查 3
	// 的查询结果,单行只读计数仿检查 3 的写法。
	switch {
	case configFailed:
		emit("date_consistency", ui.Bi("Date consistency", "日期一致性"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		// 上游数据库项已计 WARN/FAIL,此处跳过不重复计数。
		emit("date_consistency", ui.Bi("Date consistency", "日期一致性"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	default:
		var mismatched int64
		if scanErr := usageDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM messages WHERE date != strftime('%Y-%m-%d', ts/1000, 'unixepoch', 'localtime')",
		).Scan(&mismatched); scanErr != nil {
			failures++
			emit("date_consistency", ui.Bi("Date consistency", "日期一致性"), statusFail, scanErr.Error())
		} else if mismatched == 0 {
			// 总数分别内嵌进双语半句:若只在句首加 %d,中文半句会缺数字。
			emit("date_consistency", ui.Bi("Date consistency", "日期一致性"), statusOK, ui.Bi(
				fmt.Sprintf("%d messages consistent", messages),
				fmt.Sprintf("%d 条消息日期一致", messages),
			))
		} else {
			warnings++
			emit("date_consistency", ui.Bi("Date consistency", "日期一致性"), statusWarn, ui.Bi(
				fmt.Sprintf("%d messages with date inconsistent with timestamp; check whether the system timezone changed or data was modified directly", mismatched),
				fmt.Sprintf("%d 条消息日期与时间戳不一致；请检查系统时区是否变更或数据是否被直接修改", mismatched),
			))
		}
	}

	// 8. Unresolved errors / 未解决异常
	switch {
	case configFailed:
		emit("unresolved_errors", ui.Bi("Unresolved errors", "未解决异常"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	case dbState != dbStateOK:
		emit("unresolved_errors", ui.Bi("Unresolved errors", "未解决异常"), statusSkip, ui.Bi("database unavailable", "数据库不可用"))
	default:
		errs, err := db.GetErrorsContext(ctx, usageDB, db.ErrorFilter{Unresolved: true})
		if err != nil {
			failures++
			emit("unresolved_errors", ui.Bi("Unresolved errors", "未解决异常"), statusFail, err.Error())
		} else if len(errs) == 0 {
			emit("unresolved_errors", ui.Bi("Unresolved errors", "未解决异常"), statusOK, ui.Bi("none", "无"))
		} else {
			warnings++
			emit("unresolved_errors", ui.Bi("Unresolved errors", "未解决异常"), statusWarn, ui.Bi(
				fmt.Sprintf("%d unresolved; run `token-usage errors` for details, `token-usage collect retry` to retry", len(errs)),
				fmt.Sprintf("%d 条未解决;运行 `token-usage errors` 查看详情、`token-usage collect retry` 重试", len(errs)),
			))
		}
	}

	// 9. Query definitions / 查询视图:主动巡检配置的视图定义语义(default、
	// subqueries、groups),在使用路径报错之前提前发现坏定义。仅 WARN 不 FAIL:
	// 配置是纯展示态,坏定义不阻断采集与其他静态命令。
	switch {
	case configFailed:
		emit("query_definitions", ui.Bi("Query definitions", "查询视图"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	default:
		if _, qdErr := querydef.ParseViews(querydefInput(cfg)); qdErr != nil {
			warnings++
			var ve *querydef.ValidationError
			desc := qdErr.Error()
			if errors.As(qdErr, &ve) && len(ve.Issues) > 0 {
				desc = fmt.Sprintf("%d %s: %s", len(ve.Issues), ui.Bi("issue(s)", "项问题"), ve.Issues[0].Message)
			}
			emit("query_definitions", ui.Bi("Query definitions", "查询视图"), statusWarn, ui.Bi(
				fmt.Sprintf("%s; run `token-usage query list` for details", desc),
				fmt.Sprintf("%s;运行 `token-usage query list` 查看详情", desc),
			))
		} else {
			emit("query_definitions", ui.Bi("Query definitions", "查询视图"), statusOK, ui.Bi("definitions valid", "定义合法"))
		}
	}

	// 10. Daemon / 守护进程:固定输出提示行,不计入警告。
	// 取舍:现成的只读判活 helper 复用并不干净——control.NewManager 构造期即
	// MkdirAll 创建配置目录,daemon.IsDaemonRunning 经 flock TryLock 探测会在
	// 锁文件不存在时创建它、锁文件不可创建时又保守误判为运行中;两者均违背
	// doctor「绝不修改/写副作用」铁律或语义不清,故不探测,指向 status。
	// 本项不依赖配置,任何场景都输出。
	emit("daemon", ui.Bi("Daemon", "守护进程"), statusInfo, ui.Bi(
		"run `token-usage daemon status` for daemon state (this command never probes or controls the daemon)",
		"使用 `token-usage daemon status` 查看守护进程状态(本命令绝不探测或操作守护进程)",
	))

	// 11. Dashboard / 仪表板:只读探测后台仪表板,与第 10 项形成对照——daemon
	// 探测有写副作用(锁文件)故只指向 status;而读 serve.json 与 HTTP GET
	// /api/meta 均为纯读,doctor 可安全执行。本项绝不取 serve-state 锁、绝不
	// 删除状态文件:损坏/陈旧的清理指引交给 `token-usage serve status`(其
	// 陈旧/损坏清理会在锁内条件删除残留)。探测仅在 serve.json 存在时发生,
	// 缺失时零 HTTP 请求,常见路径不增加耗时。损坏与陈旧计 WARN(残留会误导
	// serve stop/status 的判定语义);未运行是 INFO(仪表板可选,不构成健康问题)。
	switch {
	case configFailed:
		emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusSkip, ui.Bi("config failed", "配置加载失败"))
	default:
		st, stErr := serve.ReadState(cfg.DataDir)
		switch {
		case errors.Is(stErr, serve.ErrStateCorrupt):
			warnings++
			emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusWarn, ui.Bi(
				"corrupt serve state file; run `token-usage serve status` to clean it up",
				"serve.json 状态文件损坏;运行 `token-usage serve status` 清理",
			))
		case stErr != nil:
			warnings++
			emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusWarn, ui.Bi(
				fmt.Sprintf("failed to read serve state: %v", stErr),
				fmt.Sprintf("读取服务状态失败:%v", stErr),
			))
		case st == nil:
			emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusInfo, ui.Bi(
				"dashboard not running; run `token-usage serve start` to start it",
				"仪表板未在后台运行;可用 `token-usage serve start` 启动",
			))
		default:
			url := "http://" + st.Addr
			if serve.MetaAlive(url, serve.StaleProbeTimeout) {
				emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusOK, ui.Bi(
					fmt.Sprintf("running at %s (PID %d)", url, st.PID),
					fmt.Sprintf("运行中 %s（PID %d）", url, st.PID),
				))
			} else {
				warnings++
				emit("dashboard", ui.Bi("Dashboard", "仪表板"), statusWarn, ui.Bi(
					fmt.Sprintf("stale state for %s (PID %d not answering); run `token-usage serve status` to clean it up", url, st.PID),
					fmt.Sprintf("%s（PID %d）无响应的陈旧状态;运行 `token-usage serve status` 清理", url, st.PID),
				))
			}
		}
	}

	// 汇总:FAIL 优先于 WARN;两者皆无才是一切正常。table 逐行渲染后输出汇总
	// 行;json 序列化 checks 与 summary(status 机器值与 table 同源)。
	summary := doctorSummary{Warnings: warnings, Problems: failures}
	switch {
	case failures > 0:
		summary.Result = "fail"
	case warnings > 0:
		summary.Result = "warn"
	default:
		summary.Result = "ok"
	}

	if format == "json" {
		payload, jsonErr := marshalExportJSON(doctorReport{Checks: checks, Summary: summary})
		if jsonErr != nil {
			return fmt.Errorf("%s: %w", ui.Bi("failed to encode doctor report as JSON", "doctor 报告 JSON 编码失败"), jsonErr)
		}
		_, jsonErr = io.WriteString(out, payload)
		return jsonErr
	}

	for _, c := range checks {
		doctorLine(out, c.Label, statusText[c.Status], c.Detail)
	}

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

// doctorStatus 是检查项结论的封闭值域(ok/warn/fail/skipped/info),作为
// 编译期常量供 emit 调用点与 --format json 序列化共用,避免裸字符串漂移。
type doctorStatus string

const (
	statusOK   doctorStatus = "ok"
	statusWarn doctorStatus = "warn"
	statusFail doctorStatus = "fail"
	statusSkip doctorStatus = "skipped"
	statusInfo doctorStatus = "info"
)

// doctorCheck 是单个检查项的结构化结果:ID 是 --format json 的稳定机器键,
// Label/Detail 与 table 输出同行同文(双语拼接串),Status 取封闭值域。
type doctorCheck struct {
	ID     string       `json:"id"`
	Label  string       `json:"label"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail"`
}

// doctorSummary 汇总全部检查项的结论:Warnings/Problems 与 table 的汇总行同
// 口径(SKIPPED/INFO 不计入),Result 是 fail/warn/ok 的机器结论。
type doctorSummary struct {
	Result   string `json:"result"`
	Warnings int    `json:"warnings"`
	Problems int    `json:"problems"`
}

// doctorReport 是 --format json 的顶层载荷。
type doctorReport struct {
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
}

// doctorStaleThreshold 是「数据新鲜度」检查项的陈旧阈值:最近成功采集距今
// 超过该时长即 WARN。取舍:7 天覆盖周末与短假,避免日常停用(如整周末开机的
// 设备)触发误报;WARN 仅提醒不 FAIL,是否补采由用户自行决定。
const doctorStaleThreshold = 7 * 24 * time.Hour

// doctorFreshnessStale 判断距最近采集的时长 age 是否陈旧(超过阈值)。
// 独立成纯函数,便于对边界(恰等于阈值不陈旧、阈值再多 1ns 即陈旧)直接单测;
// time.Since 在命令路径中不可注入,陈旧分支的集成测试改以回填旧采集记录实现。
func doctorFreshnessStale(age time.Duration) bool {
	return age > doctorStaleThreshold
}

// doctorFreshnessDesc 把距最近采集的时长渲染为人性化描述:
// 小于 1 小时为 "just now / 刚刚"(避免 "0 h ago" 的怪异观感);1 小时至不足
// 24 小时为 "X h ago / X 小时前";满 24 小时起为 "X d ago / X 天前"。
// X 一律向下取整(age 恒为正,int 截断即向下)。
func doctorFreshnessDesc(age time.Duration) string {
	switch {
	case age < time.Hour:
		return ui.Bi("just now", "刚刚")
	case age < 24*time.Hour:
		hours := int(age.Hours())
		return ui.Bi(fmt.Sprintf("%d h ago", hours), fmt.Sprintf("%d 小时前", hours))
	default:
		days := int(age.Hours() / 24)
		return ui.Bi(fmt.Sprintf("%d d ago", days), fmt.Sprintf("%d 天前", days))
	}
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
