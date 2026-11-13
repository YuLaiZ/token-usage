//go:build !windows

// internal/daemon/fatal_exit_e2e_test.go
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// skipIfRootOrNoChmod：root 绕过目录读权限（chmod 000 仍可 ReadDir），权限
// 注入夹具在 root 下无区分度，按既有规则跳过。
func skipIfRootOrNoChmod(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 权限注入无区分度")
	}
}

// T2 + T6 完整链路：真实权限错误触发 watcher 致命退出（非 ENOENT、非目录
// 消失竞态），经 reportMonitorError 进入 daemon 退出路径；日志/错误记录/
// observation 三处可追溯，读端能读到中断；修复权限后手动重启，startup
// catch-up 成功并由协调器结案，读端恢复为运行中且无中断提示。
func TestFatalMonitorErrorExitAndRecoveryVisibility(t *testing.T) {
	skipIfRootOrNoChmod(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectsDir := filepath.Join(home, "claude", "projects")
	projDir := filepath.Join(projectsDir, "-tmp-e2e")
	denied := filepath.Join(projDir, "denied")
	victim := filepath.Join(projDir, "victim")
	for _, d := range []string{denied, victim} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sess := filepath.Join(projDir, "sess.jsonl")
	line := `{"type":"assistant","sessionId":"sess-fatal-e2e","timestamp":"2026-06-10T12:00:00Z","entrypoint":"claude-desktop-3p","cwd":"/tmp/e2e","message":{"id":"fatal-e2e-msg","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(sess, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		DataDir: filepath.Join(home, "data"),
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
		},
		Daemon: config.DaemonConfig{PollInterval: 1},
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	var logs safeBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	openLog := func(c *config.Config) (RuntimeResources, error) {
		usageDB, err := db.Open(filepath.Join(c.DataDir, "usage.db"))
		if err != nil {
			return RuntimeResources{}, err
		}
		return RuntimeResources{DB: usageDB, Log: log, Close: func() error { return usageDB.Close() }}, nil
	}
	runOnce := func(instanceID string) (context.CancelFunc, <-chan error, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			done <- Run(ctx, cfg, RunOptions{
				InstanceID:         instanceID,
				OnDaemonLockCommit: func() error { close(ready); return nil },
				OpenResources:      openLog,
			})
		}()
		return cancel, done, ready
	}

	// === 第一次运行：就绪后把 denied 变不可读，删除兄弟目录触发恢复遍历，
	// 遍历遇到 ReadDir EACCES（真实非 ENOENT 错误）→ 守护进程退出。 ===
	cancel1, done1, ready1 := runOnce("inst-fatal")
	<-ready1
	statePath := runmeta.StatePath(cfg.DataDir)
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 10*time.Second); st == nil {
		t.Fatal("首次运行 catch-up 未完成")
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(victim); err != nil {
		t.Fatal(err)
	}
	var runErr error
	select {
	case runErr = <-done1:
	case <-time.After(10 * time.Second):
		cancel1()
		t.Fatal("watcher 致命错误后 daemon.Run 未返回")
	}
	if runErr == nil || errors.Is(runErr, context.Canceled) {
		t.Fatalf("致命监控错误必须以非取消错误退出: %v", runErr)
	}

	// 日志：守护进程级 ERROR 含影响与根因。
	if !logs.contains("fatal monitor error") || !logs.contains("claude") {
		t.Fatalf("日志缺少守护进程级 ERROR: %s", logs.String())
	}

	// 错误记录：恰好一条 monitor 事件，来源 claude。
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	var count int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor' AND resolved=0`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("监控中断错误记录 = %d 条, want 1", count)
	}
	var errSource, errDetail string
	if err := usageDB.QueryRow(`SELECT source, detail FROM collection_errors WHERE error_type='monitor'`).Scan(&errSource, &errDetail); err != nil {
		t.Fatal(err)
	}
	if errSource != "claude" {
		t.Fatalf("错误记录来源 = %q, want claude（结构化来源，不从文案猜）", errSource)
	}
	detail, derr := db.ParseMonitorErrorDetail(errDetail)
	if derr != nil || detail.Kind != "failed" || detail.OccurredAt == "" {
		t.Fatalf("错误明细不完整: %+v err=%v", detail, derr)
	}

	// observation：active 结束、last_exit=failed 且来源 claude。
	obs, oerr := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if oerr != nil || obs.Active != nil {
		t.Fatalf("退出后观察文件状态错误: %+v err=%v", obs, oerr)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "claude" || obs.LastExit.InstanceID != "inst-fatal" {
		t.Fatalf("退出摘要 = %+v", obs.LastExit)
	}
	fatalInstance := obs.LastExit.InstanceID

	// 读端（daemon status / serve 共用判定）：确认未运行 + 中断可见。
	col := daemonstatus.Load(context.Background(), cfg.DataDir,
		daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, usageDB, time.Now())
	if col.State != daemonstatus.StateInterrupted || col.Interruption == nil || col.Interruption.Kind != "failed" {
		t.Fatalf("退出后读端状态 = %+v", col)
	}
	if col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("中断来源 = %v, want claude", col.Interruption.Source)
	}

	// === 修复权限后手动重启：新实例补采成功 → 结案，读端恢复。 ===
	if err := os.Chmod(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	cancel2, done2, ready2 := runOnce("inst-recover")
	<-ready2
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 10*time.Second); st == nil {
		cancel2()
		t.Fatal("恢复运行 catch-up 未完成")
	}
	// 结案在 final state 之后同步执行：轮询到已结案。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if resolved, rerr := db.IsMonitorInstanceResolved(context.Background(), usageDB, fatalInstance); rerr == nil && resolved {
			break
		}
		if time.Now().After(deadline) {
			cancel2()
			t.Fatal("补采成功后监控中断事件未被结案")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 运行中读端：state=running，无当前中断提示（历史记录仍可按条件查询）。
	col = daemonstatus.Load(context.Background(), cfg.DataDir,
		daemonstatus.Snapshot{Probe: daemonstatus.ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded"}, usageDB, time.Now())
	if col.State != daemonstatus.StateRunning || col.Interruption != nil {
		t.Fatalf("恢复后的读端状态 = %+v", col)
	}
	var resolvedCount int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor' AND resolved=1`).Scan(&resolvedCount); err != nil {
		t.Fatal(err)
	}
	if resolvedCount != 1 {
		t.Fatalf("历史中断应保留为已结案可查, resolved=%d", resolvedCount)
	}

	// 正常停止恢复实例：last_exit=stopped，不写新的错误记录。
	cancel2()
	if err := <-done2; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("恢复实例停止返回错误: %v", err)
	}
	obs, _ = runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped || obs.LastExit.InstanceID != "inst-recover" {
		t.Fatalf("正常停止应记 stopped: %+v", obs.LastExit)
	}
	if n := countMonitorRows(t, usageDB); n != 1 {
		t.Fatalf("正常停止不得新增错误记录, 总行数=%d", n)
	}
	_ = model.ClientClaudeDesktop
}

// P2-3 场景：退出时 monitor 行写库失败（SQLite trigger 拒绝插入）→ 观察文件
// 是唯一证据；恢复实例补采成功后必须按观察文件中的实例/来源结案（库中无行
// 时补记已结案确认行），读端提示消失且历史可查。
func TestMonitorInterruptionConfirmedFromObservationAfterDBWriteFailure(t *testing.T) {
	skipIfRootOrNoChmod(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectsDir := filepath.Join(home, "claude", "projects")
	projDir := filepath.Join(projectsDir, "-tmp-e2e-db-fail")
	denied := filepath.Join(projDir, "denied")
	victim := filepath.Join(projDir, "victim")
	for _, d := range []string{denied, victim} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		DataDir: filepath.Join(home, "data"),
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
		},
		Daemon: config.DaemonConfig{PollInterval: 1},
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	// 第一次运行前安装拒绝 monitor 插入的 trigger（模拟退出时写库失败）。
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_monitor BEFORE INSERT ON collection_errors
		WHEN NEW.error_type = 'monitor' BEGIN SELECT RAISE(ABORT, 'monitor write blocked'); END`); err != nil {
		t.Fatal(err)
	}
	usageDB.Close()

	openLog := func(c *config.Config) (RuntimeResources, error) {
		d, oerr := db.Open(filepath.Join(c.DataDir, "usage.db"))
		if oerr != nil {
			return RuntimeResources{}, oerr
		}
		return RuntimeResources{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Close: func() error { return d.Close() }}, nil
	}
	runOnce := func(instanceID string) (context.CancelFunc, <-chan error, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			done <- Run(ctx, cfg, RunOptions{
				InstanceID:         instanceID,
				OnDaemonLockCommit: func() error { close(ready); return nil },
				OpenResources:      openLog,
			})
		}()
		return cancel, done, ready
	}

	cancel1, done1, ready1 := runOnce("inst-dbfail")
	<-ready1
	statePath := runmeta.StatePath(cfg.DataDir)
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 10*time.Second); st == nil {
		t.Fatal("首次运行 catch-up 未完成")
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(victim); err != nil {
		t.Fatal(err)
	}
	var runErr error
	select {
	case runErr = <-done1:
	case <-time.After(10 * time.Second):
		cancel1()
		t.Fatal("watcher 致命错误后 daemon.Run 未返回")
	}
	if runErr == nil || errors.Is(runErr, context.Canceled) {
		t.Fatalf("致命监控错误必须以非取消错误退出: %v", runErr)
	}

	// 写库失败：库中无 monitor 行，观察文件保留 failed 退出摘要。
	usageDB, err = db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	var monitorRows int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor'`).Scan(&monitorRows); err != nil {
		t.Fatal(err)
	}
	if monitorRows != 0 {
		t.Fatalf("trigger 生效时不应有 monitor 行, got %d", monitorRows)
	}
	obs, oerr := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if oerr != nil || obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "claude" {
		t.Fatalf("观察文件应保留可信故障证据: %+v err=%v", obs.LastExit, oerr)
	}
	fatalInstance := obs.LastExit.InstanceID

	// 移除 trigger、修复权限后重启：补采成功 → 从观察文件证据结案。
	if _, err := usageDB.Exec(`DROP TRIGGER fail_monitor`); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	cancel2, done2, ready2 := runOnce("inst-dbfail-recover")
	<-ready2
	defer func() { cancel2(); <-done2 }()
	if st := waitForRuntimeStateCatchUp(t, statePath, phaseSucceeded, 10*time.Second); st == nil {
		t.Fatal("恢复运行 catch-up 未完成")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if resolved, rerr := db.IsMonitorInstanceResolved(context.Background(), usageDB, fatalInstance); rerr == nil && resolved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("补采成功后应按观察文件证据结案（补记已结案确认行）")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 确认行历史可查。
	var confirmed int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor' AND resolved=1`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 {
		t.Fatalf("确认行数 = %d, want 1", confirmed)
	}
	// 读端不再提示。
	col := daemonstatus.Load(context.Background(), cfg.DataDir,
		daemonstatus.Snapshot{Probe: daemonstatus.ProbeRunning, PhaseAvailable: true, MonitorReady: true, CatchUp: "succeeded"}, usageDB, time.Now())
	if col.State != daemonstatus.StateRunning || col.Interruption != nil {
		t.Fatalf("结案后读端不应再提示中断: %+v", col)
	}
}
