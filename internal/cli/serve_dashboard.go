// internal/cli/serve_dashboard.go
package cli

// serve_dashboard.go 持有后台 `_serve-run` 的仪表板服务生命周期：单实例
// 守卫（serve.json + 探活拒绝、serve.lock 生命周期锁）→ 监听 → 写 serve.json
// → 宣告启动 → Serve → 信号优雅关停 → 自清理 serve.json。本函数只由
// _serve-run 调用（`serve start` 拉起）；serve.log 的 writer 非 TTY，
// OSC 8 链接随 writerIsTerminal 自动降级为纯文本。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/YuLaiZ/token-usage/internal/web"
)

// serveShutdownTimeout 是收到 SIGINT/SIGTERM 后等待在途请求完成的宽限期。
const serveShutdownTimeout = 3 * time.Second

// serveDashboard 完成一次后台仪表板服务生命周期：单实例守卫（serve.json
// + 探活幂等拒绝第二实例；serve.lock 生命周期锁交由本函数持有至退出）→ 接收
// 已打开的只读数据库（调用方负责打开与 Close）→ 监听 addr → 写 serve.json
// → 宣告启动 → Serve → SIGINT/SIGTERM 优雅关停（3s Shutdown 宽限）→ 自清理
// serve.json。
//
// 状态文件由 serve start/status/stop 与服务主体共用：监听成功即写出（PID 为
// 本进程），任何退出路径都在 defer 中删除；SIGKILL/崩溃留下的陈旧文件由
// serve status/stop 与下一次启动的守卫探活陈旧清理兜底。本进程对状态文件的
// 全部写/删都在 serve-state 状态迁移锁内、以持有的 serve.lock 为先（锁序
// serve.lock → state.lock）。单实例守卫与状态/锁原语由 internal/serve 提供，
// 与 serve start/status/stop 命令及 update 的运行态探测共享同一实现。
func serveDashboard(cfg *config.Config, usageDB *db.DB, version, addr string, out, errOut io.Writer) error {
	// 单实例守卫先于监听：第二个实例无论请求哪个端口、以哪种形态启动都会在
	// 这里被拒绝或报错，serve.json 永远只描述唯一实例。守卫放行时交出的
	// 生命周期锁持有至本函数退出（defer 顺序：先删状态文件，再释放锁；
	// 两者互不依赖，均必达）。
	lock, proceed, err := serve.LifecycleGuard(cfg.DataDir, out, serve.StaleProbeTimeout)
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
	ownState := &serve.ServeState{
		PID:       os.Getpid(),
		Addr:      ln.Addr().String(),
		StartedAt: time.Now().Format(time.RFC3339),
	}
	stateLock, err := serve.AcquireStateLock(cfg.DataDir)
	if err != nil {
		_ = ln.Close()
		return err
	}
	writeErr := serve.WriteState(cfg.DataDir, ownState)
	_ = serve.ReleaseStateLock(stateLock)
	if writeErr != nil {
		_ = ln.Close()
		return fmt.Errorf("%s: %w", ui.Bi("failed to write serve state file", "写入服务状态文件失败"), writeErr)
	}
	// 退出时自清理状态文件（defer LIFO：先于 serve.lock 释放执行）。自删同样
	// 在 state 锁内走条件删除 serve.RemoveStateIfSame：自己写出的状态必然一致
	// → 删除；若锁内重读不一致说明文件被改写——持 serve.lock 时理论上不可达，
	// 防御性报告到 errOut 且不删。取锁失败或 I/O 失败静默容忍（文件系统只读等
	// 罕见场景下，后续 status/stop 的陈旧探活清理仍会兜底删除）。
	defer func() {
		slock, err := serve.AcquireStateLock(cfg.DataDir)
		if err != nil {
			return
		}
		removed, cur, err := serve.RemoveStateIfSame(cfg.DataDir, ownState)
		_ = serve.ReleaseStateLock(slock)
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
