// internal/cli/serve_status.go
package cli

// serve_status.go 实现 `token-usage serve status`：读 serve.json 并以
// /api/meta 探活分诊——无状态文件（或损坏）报未运行；有且响应报运行中；
// 有但无响应判定为陈旧状态；损坏文件与陈旧状态同样在报告前删除（与文档
// 承诺一致）。退出码恒 0。`--format json` 把同一判定输出为机器可读文档
//（state 取封闭值域 running / not_running / not_running_stale_removed /
// not_running_corrupt_removed，字段与 table 报告同源）。
//
// 「读状态 → 探活判定 → 陈旧/损坏删除」整段在 serve-state 状态迁移锁内进行，
// 删除一律走 removeServeStateIfSame 条件删除：锁内重读发现文件已被新实例改写
// 时不删，改用当前状态重新探活——响应则按运行中的新实例报告（不删任何东西），
// 无响应才删除并报未运行。这消除了「判定陈旧后、删除前新实例恰好写出新
// serve.json」的 TOCTOU 误删窗口。
//
// 实现为「收集-渲染」两段：serveStatusOutcome 是一次判定的结构化结果，
// table 与 json 两种渲染共用同一 outcome，保证两种格式对同一磁盘状态输出
// 同源结论（与 doctor --format json 的重构同风格）。

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// serveStatusProbeTimeout 是 status 对已记录地址的单次探活超时。
var serveStatusProbeTimeout = 2 * time.Second

// serveStatus 状态机值的封闭值域（--format json 的 state 字段）。四个值与
// collectServeStatus 的四个判定出口一一对应，也与 table 输出的四种文案一一
// 对应；新增判定分支必须同步扩充值域并更新本注释与 Long 帮助。
const (
	serveStatusRunning                  = "running"
	serveStatusNotRunning               = "not_running"
	serveStatusNotRunningStaleRemoved   = "not_running_stale_removed"
	serveStatusNotRunningCorruptRemoved = "not_running_corrupt_removed"
)

// serveStatusOutcome 是一次 status 判定的结构化结果：table 与 json 渲染共用。
// Running=true 时 PID/Addr/URL/StartedAt 描述运行实例；否则这些字段为零值
// （实例不存在），State 携带未运行的具体形态。
type serveStatusOutcome struct {
	Running   bool
	State     string
	PID       int
	Addr      string
	URL       string
	StartedAt string
}

// serveStatusReport 是 `serve status --format json` 的结构化载荷：字段与
// table 报告同源（同一次判定 outcome），state 取封闭值域。
type serveStatusReport struct {
	Running   bool   `json:"running"`
	State     string `json:"state"`
	PID       int    `json:"pid,omitempty"`
	Addr      string `json:"addr,omitempty"`
	URL       string `json:"url,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	DataDir   string `json:"data_dir"`
}

// newServeStatusCmd 构造 `serve status`。--addr/--open 继承自 serve 的
// persistent flags（status 自身不使用，仅保持命令族 flag 面一致）。
func newServeStatusCmd(load func() (*config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: ui.Bi("Show whether the dashboard server is running in the background", "查看仪表板后台服务运行状态"),
		Long: ui.Bi(
			"Show whether the dashboard server is running in the background. The check reads serve.json under the data directory (~/.token-usage/serve.json by default) and probes the recorded address on /api/meta: a response reports running with URL, PID and start time; no response means the state is stale (left by a crash or SIGKILL) and the file is removed before reporting not running, and a corrupt state file is removed the same way. Every state outcome exits 0; only unexpected I/O failures exit non-zero. With `--format json` the same outcome is emitted as a machine-readable document: `state` is a closed vocabulary (`running`, `not_running`, `not_running_stale_removed`, `not_running_corrupt_removed`), `running` is the boolean the state derives from, and `pid`, `addr`, `url` and `started_at` (RFC3339, verbatim from serve.json) appear only while running.\n\nExamples:\n  token-usage serve status\n  token-usage serve status --format json",
			"查看仪表板服务是否在后台运行。检查方式为读取数据目录下的 serve.json（默认 ~/.token-usage/serve.json）并对记录地址的 /api/meta 探活：有响应则报告运行中（含 URL、PID 与启动时间）；无响应说明是崩溃或 SIGKILL 遗留的陈旧状态，会在报告未运行前删除该文件；损坏的状态文件同样删除后再报告。所有状态结论均以退出码 0 返回；只有意外的 I/O 失败才非零。`--format json` 把同一判定输出为机器可读文档：`state` 取封闭值域（`running`、`not_running`、`not_running_stale_removed`、`not_running_corrupt_removed`），`running` 是 state 对应的布尔值，`pid`、`addr`、`url` 与 `started_at`（RFC3339，serve.json 原值）仅在运行中出现。\n\n示例：\n  token-usage serve status\n  token-usage serve status --format json",
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("format")
			switch format {
			case "", "table", "json":
			default:
				return fmt.Errorf("%s", ui.Bi(
					fmt.Sprintf("invalid --format %q (allowed: table, json)", format),
					fmt.Sprintf("无效的 --format %q（允许：table、json）", format),
				))
			}
			out := cmd.OutOrStdout()
			cfg, err := load()
			if err != nil {
				return fmt.Errorf("%s: %w", ui.Bi("failed to load config", "加载配置失败"), err)
			}
			outcome, err := collectServeStatus(cfg)
			if err != nil {
				return err
			}
			if format == "json" {
				return renderServeStatusJSON(out, cfg.DataDir, outcome)
			}
			renderServeStatusTable(out, outcome)
			return nil
		},
	}
	cmd.Flags().String("format", "table", ui.Bi(
		"Output format: table (human-readable report) or json (machine-readable status document)",
		"输出格式：table（人读报告）或 json（机器可读的状态文档）",
	))
	return cmd
}

// collectServeStatus 执行「读-判-删」判定并返回结构化结果：serve-state 锁内
// 完成整段迁移（与 table 文档承诺一致），判定分支与既有 table 输出一一对应。
func collectServeStatus(cfg *config.Config) (serveStatusOutcome, error) {
	// 状态迁移锁：「读-判-删」整段持锁（status 无信号等待段，全程毫秒
	// 级至探活超时的秒级），保证判定所据内容与被删内容一致。
	stateLock, err := serve.AcquireStateLock(cfg.DataDir)
	if err != nil {
		return serveStatusOutcome{}, err
	}
	defer serve.ReleaseStateLock(stateLock)

	st, err := serve.ReadState(cfg.DataDir)
	if err != nil {
		if errors.Is(err, serve.ErrStateCorrupt) {
			// 损坏的状态文件无法定位实例：持锁期间内容稳定，直接删除
			// 残留后按未运行如实报告。
			if rmErr := serve.RemoveState(cfg.DataDir); rmErr != nil {
				return serveStatusOutcome{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
			}
			return serveStatusOutcome{State: serveStatusNotRunningCorruptRemoved}, nil
		}
		return serveStatusOutcome{}, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	}
	if st == nil {
		return serveStatusOutcome{State: serveStatusNotRunning}, nil
	}
	if serve.MetaAlive("http://"+st.Addr, serveStatusProbeTimeout) {
		return runningOutcome(st), nil
	}
	// 状态文件在但探活无响应：疑似陈旧状态。条件删除——锁内重读与判定
	// 所据一致才删；不一致说明新实例已接管，不删。
	removed, current, err := serve.RemoveStateIfSame(cfg.DataDir, st)
	if err != nil {
		return serveStatusOutcome{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), err)
	}
	if removed {
		return serveStatusOutcome{State: serveStatusNotRunningStaleRemoved}, nil
	}
	// 新实例已写出自己的 serve.json：用新状态重新探活——响应则按运行中
	// 报告（不删任何东西）；无响应则当前状态才是真的陈旧，删除后报未运行。
	if current != nil && serve.MetaAlive("http://"+current.Addr, serveStatusProbeTimeout) {
		return runningOutcome(current), nil
	}
	if rmErr := serve.RemoveState(cfg.DataDir); rmErr != nil {
		return serveStatusOutcome{}, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
	}
	return serveStatusOutcome{State: serveStatusNotRunningStaleRemoved}, nil
}

// runningOutcome 把运行实例的 serve.json 状态映射为运行中 outcome。
func runningOutcome(st *serve.ServeState) serveStatusOutcome {
	return serveStatusOutcome{
		Running:   true,
		State:     serveStatusRunning,
		PID:       st.PID,
		Addr:      st.Addr,
		URL:       "http://" + st.Addr,
		StartedAt: st.StartedAt,
	}
}

// renderServeStatusTable 输出与既有行为逐字节一致的人读报告；分支与
// collectServeStatus 的判定出口一一对应。
func renderServeStatusTable(out io.Writer, o serveStatusOutcome) {
	switch {
	case o.State == serveStatusNotRunningCorruptRemoved:
		fmt.Fprintln(out, ui.Bi("serve is not running (corrupt state removed)", "仪表板未在后台运行（已清理损坏的状态文件）"))
	case o.State == serveStatusNotRunning:
		fmt.Fprintln(out, ui.Bi("serve is not running", "仪表板未在后台运行"))
	case o.Running:
		fmt.Fprintf(out, "%s\n", ui.Bi(
			fmt.Sprintf("serve is running: %s (pid %d, since %s)", o.URL, o.PID, serveLocalTime(o.StartedAt)),
			fmt.Sprintf("仪表板正在后台运行：%s（PID %d，自 %s 起）", o.URL, o.PID, serveLocalTime(o.StartedAt)),
		))
	default: // not_running_stale_removed
		fmt.Fprintln(out, ui.Bi("serve is not running (stale state removed)", "仪表板未在后台运行（已清理陈旧状态）"))
	}
}

// renderServeStatusJSON 输出机器可读状态文档（两空格缩进 + 尾随换行，
// 与 doctor/daemon status 的机器输出约定一致）。
func renderServeStatusJSON(out io.Writer, dataDir string, o serveStatusOutcome) error {
	report := serveStatusReport{
		Running:   o.Running,
		State:     o.State,
		PID:       o.PID,
		Addr:      o.Addr,
		URL:       o.URL,
		StartedAt: o.StartedAt,
		DataDir:   dataDir,
	}
	payload, err := marshalExportJSON(report)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to encode serve status as JSON", "serve 状态 JSON 编码失败"), err)
	}
	_, err = io.WriteString(out, payload)
	return err
}

// serveLocalTime 把 RFC3339 的 started_at 渲染为本地可读形态；解析失败时
// 原样返回，不因状态文件内容异常阻塞 status。
func serveLocalTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
