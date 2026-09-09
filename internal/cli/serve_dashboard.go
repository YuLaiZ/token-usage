// internal/cli/serve_dashboard.go
package cli

// serve_dashboard.go 持有前台 `serve` 与后台 `_serve-run` 共用的仪表板服务
// 生命周期：单实例守卫（serve.json + 探活拒绝、serve.lock 生命周期锁）→
// 监听 → 写 serve.json → 宣告启动 →（可选开浏览器）→ Serve → 信号优雅关停 →
// 自清理 serve.json。两条路径只差输出 writer 与 autoOpen：
// OSC 8 链接随 writerIsTerminal 自动降级（后台日志 writer 非 TTY 输出纯文本），
// 无需按前台/后台特判。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/YuLaiZ/token-usage/internal/web"
)

// serveLifecycleGuard 是 serveDashboard 的单实例守卫，在监听之前执行单实例
// 契约：任意时刻至多一个服务实例（前台或后台）。依次判定：
//
//  1. serve-state 状态迁移锁内的状态分诊（「读-判定-删」整段在锁内，杜绝判定
//     与删除之间新实例接管导致的误删）。损坏的 serve.json → 删除残留后继续
//     （与 status/stop/start 的统一承诺一致）；存在且 /api/meta 有响应 → 幂等
//     拒绝：向 out 打印现存实例的 URL 与 PID，返回 (nil, false, nil)，调用方以
//     退出码 0 返回——URL 对用户可用，exit 0 诚实；存在但无响应 → 陈旧
//     （SIGKILL/崩溃遗留），条件删除后继续。条件删除锁内重读发现已被新实例
//     改写则不删——此时不重评估：新实例能写出状态说明它已持有（或刚释放）
//     serve.lock，下方第 2 步的生命周期锁获取本身就是仲裁（新实例活着则取锁
//     失败报重试，已死则放行接管）。
//  2. 释放 state 锁后取 AcquireLock(serve.lock 生命周期锁)：失败说明另一个
//     实例正在启动（瞬时竞态），返回双语错误，调用方以非零退出码结束；成功则
//     锁随返回值交出，调用方必须在整个服务生命周期持有并在退出时释放。
//
// 锁序：判定段先取再释放 state.lock，之后才取 serve.lock，两锁从不同时持有；
// 放行后服务主体对状态的写/删是 serve.lock → state.lock 顺序（见 serveDashboard），
// 全局无环。
//
// 返回 (lock, true, nil) 表示放行继续启动（lock 非 nil）；(nil, false, nil)
// 表示已有实例在运行、幂等拒绝；err 非 nil 表示意外失败。
func serveLifecycleGuard(dataDir string, out io.Writer, probeTimeout time.Duration) (*flock.Flock, bool, error) {
	stateLock, err := acquireServeStateLock(dataDir)
	if err != nil {
		return nil, false, err
	}
	st, err := readServeState(dataDir)
	switch {
	case errors.Is(err, errServeStateCorrupt):
		// 损坏残留与陈旧同路：锁内直接删除（损坏文件无新实例语义），避免带着
		// 无法辨识的旧文件进入正常路径（后续写状态文件会覆盖它，但删除与 start
		// 的承诺保持一致）。
		if rmErr := removeServeState(dataDir); rmErr != nil {
			_ = releaseServeStateLock(stateLock)
			return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to remove corrupt serve state", "清理损坏的服务状态失败"), rmErr)
		}
	case err != nil:
		_ = releaseServeStateLock(stateLock)
		return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to read serve state", "读取服务状态失败"), err)
	case st != nil:
		if serveMetaAlive("http://"+st.Addr, probeTimeout) {
			_ = releaseServeStateLock(stateLock)
			url := "http://" + st.Addr
			fmt.Fprintf(out, "%s\n", ui.Bi(
				fmt.Sprintf("dashboard is already running at %s (pid %d); stop it first with token-usage serve stop, or open that URL", url, st.PID),
				fmt.Sprintf("仪表板已在运行（%s，PID %d）；请先用 token-usage serve stop 停止，或直接打开该地址", url, st.PID)))
			return nil, false, nil
		}
		// 放行前条件删除陈旧状态，避免误判「已启动」；新实例已接管时不删：
		// 它存活会令下方 serve.lock 获取失败并报「正在启动」，已死则放行接管。
		if _, _, rmErr := removeServeStateIfSame(dataDir, st); rmErr != nil {
			_ = releaseServeStateLock(stateLock)
			return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to remove stale serve state", "清理陈旧服务状态失败"), rmErr)
		}
	}
	// 判定段结束，先释放 state 锁再取生命周期锁：两锁从不同时持有（锁序注释）。
	_ = releaseServeStateLock(stateLock)

	// 生命周期锁：挡住「另一个实例正在启动」的瞬时竞态（它已通过状态文件
	// 之前的检查但尚未写出 serve.json）。锁由调用方持有至服务退出。
	// 获取失败按带界重试处理:探活判定下线只说明端口已关闭,前一个实例可能
	// 尚未走完退出路径(释放 serve.lock 前的收尾),serve restart 的 start 段
	// 恰好落在这一瞬态窗口;窗口耗尽仍是真互斥失败,报错退出。
	deadline := time.Now().Add(serveLifecycleLockWait)
	for {
		lock, ok := daemon.AcquireLock(filepath.Join(dataDir, serveLifecycleLockFile))
		if ok {
			return lock, true, nil
		}
		if time.Now().After(deadline) {
			return nil, false, errors.New(ui.Bi(
				"another serve instance is starting; retry in a moment",
				"另一个 serve 实例正在启动，请稍后重试"))
		}
		time.Sleep(serveLifecycleLockRetry)
	}
}

// serveDashboard 完成一次前台或后台的仪表板服务生命周期：单实例守卫（serve.json
// + 探活幂等拒绝第二实例；serve.lock 生命周期锁交由本函数持有至退出）→ 接收
// 已打开的只读数据库（调用方负责打开与 Close）→ 监听 addr → 写 serve.json →
// 宣告启动 → （autoOpen 时开浏览器）→ Serve → SIGINT/SIGTERM 优雅关停（3s
// Shutdown 宽限）→ 自清理 serve.json。
//
// 状态文件由前台与后台共用：监听成功即写出（PID 为本进程），任何退出路径
// 都在 defer 中删除；SIGKILL/崩溃留下的陈旧文件由 serve status/stop 与下一次
// 启动的守卫探活陈旧清理兜底。本进程对状态文件的全部写/删都在 serve-state
// 状态迁移锁内、以持有的 serve.lock 为先（锁序 serve.lock → state.lock）。
// 启动行的 OSC 8 链接与 autoOpen 语义与抽取前的
// 前台 RunE 逐字节一致。
func serveDashboard(cfg *config.Config, usageDB *db.DB, version, addr string, out, errOut io.Writer, autoOpen bool) error {
	// 单实例守卫先于监听：第二个实例无论请求哪个端口、以哪种形态启动都会在
	// 这里被拒绝或报错，serve.json 永远只描述唯一实例。守卫放行时交出的
	// 生命周期锁持有至本函数退出（defer 顺序：先删状态文件，再释放锁；
	// 两者互不依赖，均必达）。
	lock, proceed, err := serveLifecycleGuard(cfg.DataDir, out, serveStaleProbeTimeout)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	defer daemon.ReleaseLock(lock)

	// 先建立监听再宣告启动:端口被占用等错误走常规 RunE 错误路径,
	// 不产生「已启动」的误导输出。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: %w",
			ui.Bi(fmt.Sprintf("failed to listen on %s", addr), fmt.Sprintf("监听 %s 失败", addr)), err)
	}
	url := fmt.Sprintf("http://%s", ln.Addr())

	// 监听成功即写状态文件：serve start 的父进程靠它判定后台启动就绪，
	// serve status/stop 靠它探活。写失败则放弃启动（后台路径静默容忍会造成
	// 父进程轮询假超时），此时关闭已建立的监听。
	// 写入在 serve-state 状态迁移锁内进行（状态迁移不变量）：本进程已持
	// serve.lock，理论上无竞争者，取锁只为让所有状态迁移共享同一不变量——
	// 锁序 serve.lock → state.lock，与 status/stop/start（仅 state.lock）无环。
	ownState := &ServeState{
		PID:       os.Getpid(),
		Addr:      ln.Addr().String(),
		StartedAt: time.Now().Format(time.RFC3339),
	}
	stateLock, err := acquireServeStateLock(cfg.DataDir)
	if err != nil {
		_ = ln.Close()
		return err
	}
	writeErr := writeServeState(cfg.DataDir, ownState)
	_ = releaseServeStateLock(stateLock)
	if writeErr != nil {
		_ = ln.Close()
		return fmt.Errorf("%s: %w", ui.Bi("failed to write serve state file", "写入服务状态文件失败"), writeErr)
	}
	// 退出时自清理状态文件（defer LIFO：先于 serve.lock 释放执行）。自删同样
	// 在 state 锁内走条件删除 removeServeStateIfSame：自己写出的状态必然一致
	// → 删除；若锁内重读不一致说明文件被改写——持 serve.lock 时理论上不可达，
	// 防御性报告到 errOut 且不删。取锁失败或 I/O 失败静默容忍（文件系统只读等
	// 罕见场景下，后续 status/stop 的陈旧探活清理仍会兜底删除）。
	defer func() {
		slock, err := acquireServeStateLock(cfg.DataDir)
		if err != nil {
			return
		}
		removed, cur, err := removeServeStateIfSame(cfg.DataDir, ownState)
		_ = releaseServeStateLock(slock)
		if err == nil && !removed && cur != nil {
			fmt.Fprintf(errOut, "%s\n", ui.Bi(
				fmt.Sprintf("warning: serve state was overwritten by pid %d at %s during shutdown; left untouched", cur.PID, cur.Addr),
				fmt.Sprintf("警告：关停期间服务状态已被改写（PID %d，%s），未予删除", cur.PID, cur.Addr)))
		}
	}()

	// 信号监听先于服务启动注册,避免启动即收到信号时走默认终止路径。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	// 输出列布局:与 query 静态命令同一解析(query.output 配置 → 布局),
	// 让仪表板的指标条对齐用户配置的可见列;布局非法时启动失败并把诊断
	// 写入 serve.log(start 会附带日志尾),与静态命令的开库前错误同语义。
	layout, err := staticTableOutputLayout(cfg)
	if err != nil {
		return err
	}
	q, err := newLayoutQuerier(usageDB, layout)
	if err != nil {
		return err
	}

	handler := web.NewServer(q, version, web.WithProviderAliases(cfg.ProviderAliases))
	srv := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// 交互终端下用 OSC 8 超链接包裹 URL,支持单击打开浏览器;
	// 非 TTY 或不支持 OSC 8 的终端安全降级为纯文本（后台 serve.log 即此形态）。
	tty := writerIsTerminal(out)
	linked := hyperlinkURL(url, tty)
	fmt.Fprintf(out, "%s\n", ui.Bi(
		fmt.Sprintf("dashboard served at %s (Ctrl+C to stop)", linked),
		fmt.Sprintf("仪表板已启动 %s（Ctrl+C 停止）", linked),
	))
	if autoOpen {
		// 打开浏览器失败不致命:打印警告后继续服务,URL 已在上方输出。
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(errOut, "%s\n", ui.Bi(
				fmt.Sprintf("failed to open browser: %v", err),
				fmt.Sprintf("打开浏览器失败：%v", err),
			))
		}
	}

	select {
	case err := <-serveErr:
		// Shutdown 主动关闭监听器时 Serve 返回 ErrServerClosed,不视为故障。
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("%s: %w", ui.Bi("dashboard server failed", "仪表板服务异常退出"), err)
	case <-sig:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to shut down dashboard server", "关闭仪表板服务失败"), err)
	}
	fmt.Fprintln(out, ui.Bi("dashboard stopped", "仪表板已停止"))
	return nil
}
