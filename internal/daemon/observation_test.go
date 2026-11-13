// internal/daemon/observation_test.go
package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/analyzer"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

func testObservationConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{DataDir: t.TempDir()}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// registerActiveObservation：首次登记 active；上一代残留无退出记录 → 记
// interrupted_unknown；已记录退出 → 不重复记录；损坏文件只降级。
func TestRegisterActiveObservation(t *testing.T) {
	cfg := testObservationConfig(t)
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)

	// 首次：无文件 → 直接登记 active。
	registerActiveObservation(cfg, log, nil, "inst-1", now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil || obs.Active == nil || obs.Active.InstanceID != "inst-1" {
		t.Fatalf("首次登记失败: %+v err=%v", obs, err)
	}

	// 上一代 active 残留且无 last_exit → 记录 interrupted_unknown 并换新 active。
	registerActiveObservation(cfg, log, nil, "inst-2", now.Add(time.Hour))
	obs, _ = runmeta.ReadObservation(path)
	if obs.Active == nil || obs.Active.InstanceID != "inst-2" {
		t.Fatalf("新 active 未登记: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-1" || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown {
		t.Fatalf("残留 active 应记为中断未知: %+v", obs.LastExit)
	}

	// 上一代（inst-2）同样无自身退出记录 → 覆盖为 inst-2 的中断记录。
	registerActiveObservation(cfg, log, nil, "inst-3", now.Add(2*time.Hour))
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-2" || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown {
		t.Fatalf("连续残留应逐代记录中断: %+v", obs.LastExit)
	}

	// 上一代已有同实例退出记录（写 stopped 后清 active 失败残留）→ 保留原记录。
	stale := runmeta.Observation{
		Version:  runmeta.ObservationVersion,
		Active:   &runmeta.ActiveRun{InstanceID: "inst-keep", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{InstanceID: "inst-keep", Kind: runmeta.ExitKindStopped, DetectedAt: now.Format(time.RFC3339), Summary: "采集已停止"},
	}
	if err := runmeta.WriteObservation(path, stale); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, log, nil, "inst-next", now.Add(4*time.Hour))
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-keep" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("已有同实例退出记录不得被改写: %+v", obs.LastExit)
	}

	// 损坏文件 → 降级为全新证据，仍写新 active。
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, log, nil, "inst-4", now.Add(3*time.Hour))
	obs, err = runmeta.ReadObservation(path)
	if err != nil || obs.Active == nil || obs.Active.InstanceID != "inst-4" {
		t.Fatalf("损坏文件应降级并写新 active: %+v err=%v", obs, err)
	}
	if obs.LastExit != nil {
		t.Fatalf("损坏证据不得构造故障记录: %+v", obs.LastExit)
	}
}

// finishDaemonExit：正常取消记 stopped 不写错误；监控致命错误记 failed、
// 结构化来源、写 ERROR 日志与一条幂等的 collection_errors 事件。
func TestFinishDaemonExit(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	var logBuf safeBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)

	// 正常取消 → stopped。
	registerActiveObservation(cfg, log, nil, "inst-ok", now)
	finishDaemonExit(cfg, log, usageDB, "inst-ok", context.Canceled, now.Add(time.Minute))
	obs, _ := runmeta.ReadObservation(path)
	if obs.Active != nil {
		t.Fatalf("退出后 active 必须清空: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("正常取消应记 stopped: %+v", obs.LastExit)
	}
	if n := countMonitorRows(t, usageDB); n != 0 {
		t.Fatalf("正常停止不得写错误记录, got %d", n)
	}

	// 监控致命错误 → failed + 来源 + 日志 + 幂等错误记录。
	registerActiveObservation(cfg, log, nil, "inst-fail", now.Add(2*time.Minute))
	fatal := &analyzer.MonitorFatalError{
		Client:     "claude",
		OccurredAt: now.Add(2*time.Minute + 30*time.Second),
		Err:        errors.New("fsnotify watcher error: boom"),
	}
	finishDaemonExit(cfg, log, usageDB, "inst-fail", fatal, now.Add(3*time.Minute))
	// 重复调用（模拟退出路径重入）必须幂等。
	finishDaemonExit(cfg, log, usageDB, "inst-fail", fatal, now.Add(3*time.Minute))
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("监控致命错误应记 failed: %+v", obs.LastExit)
	}
	if obs.LastExit.Source != "claude" || obs.LastExit.Summary == "" {
		t.Fatalf("退出摘要缺少来源/简述: %+v", obs.LastExit)
	}
	if obs.LastExit.OccurredAt != fatal.OccurredAt.Format(time.RFC3339) {
		t.Fatalf("occurred_at 应取故障发生时刻: %q", obs.LastExit.OccurredAt)
	}
	if n := countMonitorRows(t, usageDB); n != 1 {
		t.Fatalf("同一实例的监控中断记录必须幂等, 行数=%d", n)
	}
	var source, kind string
	if err := usageDB.QueryRow(`SELECT source, json_extract(detail,'$.kind') FROM collection_errors WHERE error_type='monitor'`).Scan(&source, &kind); err != nil {
		t.Fatal(err)
	}
	if source != "claude" || kind != "failed" {
		t.Fatalf("错误记录 source=%q kind=%q", source, kind)
	}
	if !logBuf.contains("fatal monitor error") {
		t.Fatalf("缺少守护进程级 ERROR 日志: %s", logBuf.String())
	}
	// 安全简述：不得透出原始错误、路径或堆栈。
	if strings.Contains(obs.LastExit.Summary, "boom") || strings.Contains(obs.LastExit.Summary, "fsnotify") {
		t.Fatalf("summary 不得包含原始错误: %q", obs.LastExit.Summary)
	}
}

// 无法定位来源的错误：source 空、summary 通用；DB 归一 daemon。
func TestFinishDaemonExitUnknownSource(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	log := quietLogger()
	registerActiveObservation(cfg, log, nil, "inst-u", time.Now())
	finishDaemonExit(cfg, log, usageDB, "inst-u", errors.New("boom"), time.Now())
	obs, _ := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "" {
		t.Fatalf("未知来源应留空: %+v", obs.LastExit)
	}
	var source string
	if err := usageDB.QueryRow(`SELECT source FROM collection_errors WHERE error_type='monitor'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "daemon" {
		t.Fatalf("未知来源的错误记录 source=%q, want daemon", source)
	}
}

// 退出边界：DB 不可用（nil）不影响 observation 写入；observation 写失败不
// 影响退出（不 panic、不阻塞）。
func TestFinishDaemonExitDegraded(t *testing.T) {
	cfg := testObservationConfig(t)
	log := quietLogger()
	registerActiveObservation(cfg, log, nil, "inst-d", time.Now())
	finishDaemonExit(cfg, log, nil, "inst-d", &analyzer.MonitorFatalError{Client: "codex", OccurredAt: time.Now(), Err: errors.New("x")}, time.Now())
	obs, err := runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if err != nil || obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("DB 不可用时 observation 仍应写入: %+v err=%v", obs, err)
	}

	// 观察文件路径不可写：不 panic、退出流程照常返回。
	cfgRO := &config.Config{DataDir: filepath.Join(t.TempDir(), "missing", "data")}
	finishDaemonExit(cfgRO, log, nil, "inst-e", errors.New("boom"), time.Now())
}

// 首次 UpdateObservation 锁超时后，有界重试必须复用完整职责：上一代残留
// active 的 interrupted_unknown 推导与证据迁移不能因重试而丢失。
func TestRegisterActiveObservation_RetriesAfterLockTimeout(t *testing.T) {
	cfg := testObservationConfig(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-old", StartedAt: occurred.Format(time.RFC3339)},
	}
	obs.AddPendingExit(runmeta.ExitRecord{
		InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
		OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
		Source: "claude", Summary: "Claude 目录监听失败",
	})
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	// 持有观察文件写锁 2.1 秒：首次 Update 等满 2 秒超时，重试（1 秒上限）
	// 在锁释放后成功。
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	go func() {
		time.Sleep(2100 * time.Millisecond)
		_ = lock.Unlock()
	}()

	registerActiveObservation(cfg, quietLogger(), nil, "inst-new", now)
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active == nil || obs.Active.InstanceID != "inst-new" {
		t.Fatalf("重试成功后本代 active 必须落定: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-old" || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown {
		t.Fatalf("重试必须复用完整 mutate：上一代残留应推导为中断未知: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("既有 pending 证据必须保留: %+v", obs.PendingExits)
	}
}

// UpdateObservation 失败但观察文件仍可解析（锁超时/写入 IO）时，不得用 fresh
// 覆盖已有证据：last_exit 与 pending_exits 原样保留，本代 active 不登记。
func TestRegisterActiveObservation_KeepsEvidenceWhenUpdateFails(t *testing.T) {
	// 目录只读在 Windows 不阻止写、root 会绕过权限位：这两种环境跳过。
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("目录只读无法制造写入失败")
	}
	cfg := testObservationConfig(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
		PendingExits: []runmeta.ExitRecord{{
			InstanceID: "inst-P", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "codex", Summary: "Codex 目录监听失败",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// 只读数据目录：观察文件可读，但 UpdateObservation 的锁文件/写回失败。
	if err := os.Chmod(cfg.DataDir, 0o555); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, quietLogger(), nil, "inst-B", now)
	if err := os.Chmod(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-A" || obs.LastExit.Kind != runmeta.ExitKindFailed {
		t.Fatalf("锁/写失败不得覆盖 last_exit: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-P" {
		t.Fatalf("锁/写失败不得覆盖 pending_exits: %+v", obs.PendingExits)
	}
	if obs.Active != nil {
		t.Fatalf("写入失败时本代 active 不应被伪造: %+v", obs.Active)
	}
}

// holdSQLiteWriteLock 在 usage.db 上持有一个独立连接的写事务，模拟数据库
// 长时间不可写；返回释放函数。
func holdSQLiteWriteLock(t *testing.T, path string) func() {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// 单连接：BEGIN 与 ROLLBACK 必须落在同一连接上。
	raw.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		_ = conn.Close()
		_ = raw.Close()
	}
}

// 历史缺口整批补写有统一预算，且不阻塞本代运行证据：数据库写锁被长期持有
// 时，registerActiveObservation 应在预算内返回；补写进行期间本代 active 已
// 落定。用条目上限构造，使「移除预算」与「补写放到 active 之前」两种回退
// 都会转红。
func TestRegisterActiveObservation_PendingPersistBudget(t *testing.T) {
	cfg := testObservationConfig(t)
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	obs := runmeta.Observation{Version: runmeta.ObservationVersion}
	for i := 0; i < runmeta.MaxPendingExits; i++ {
		obs.AddPendingExit(runmeta.ExitRecord{
			InstanceID: fmt.Sprintf("inst-%02d", i), Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		})
	}
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	writer, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	release := holdSQLiteWriteLock(t, dbPath)
	defer release()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		registerActiveObservation(cfg, log, writer, "inst-new", now)
	}()
	// 顺序合同：历史补写进行期间本代 active 必须已落定。
	activeSeen := false
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		cur, rerr := runmeta.ReadObservation(path)
		if rerr == nil && cur.Active != nil && cur.Active.InstanceID == "inst-new" {
			activeSeen = true
			break
		}
	}
	if !activeSeen {
		t.Fatal("历史补写进行期间本代 active 必须已落定")
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("历史补写必须有整批预算（未在预算内返回）")
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("历史补写必须有整批预算，耗时 %v", elapsed)
	}
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.PendingExits) != runmeta.MaxPendingExits {
		t.Fatalf("预算耗尽时 pending 必须保留: got %d", len(obs.PendingExits))
	}

	// 写锁释放后恢复补写：全部入库并移除文件条目。
	release()
	registerActiveObservation(cfg, log, writer, "inst-new2", now.Add(time.Second))
	if n := countMonitorRows(t, writer); n != runmeta.MaxPendingExits {
		t.Fatalf("写库恢复后应补写全部缺口, monitor 行数=%d", n)
	}
	obs, _ = runmeta.ReadObservation(path)
	if len(obs.PendingExits) != 0 {
		t.Fatalf("补写成功后 pending 条目应移除: %+v", obs.PendingExits)
	}
}

// 退出路径同样先落定本次退出证据：数据库写锁被长期持有时 finishDaemonExit
// 仍应在预算内返回，last_exit 已写入、pending 保留。用条目上限构造以同时
// 守护预算与顺序。
func TestFinishDaemonExit_PendingPersistBudget(t *testing.T) {
	cfg := testObservationConfig(t)
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
	}
	for i := 0; i < runmeta.MaxPendingExits; i++ {
		obs.AddPendingExit(runmeta.ExitRecord{
			InstanceID: fmt.Sprintf("inst-p%02d", i), Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		})
	}
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	writer, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	release := holdSQLiteWriteLock(t, dbPath)
	defer release()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		finishDaemonExit(cfg, log, writer, "inst-B", nil, now.Add(time.Minute))
	}()
	// 顺序合同：历史补写进行期间本次退出证据必须已落定。
	exitSeen := false
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		cur, rerr := runmeta.ReadObservation(path)
		if rerr == nil && cur.LastExit != nil && cur.LastExit.InstanceID == "inst-B" && cur.LastExit.Kind == runmeta.ExitKindStopped {
			exitSeen = true
			break
		}
	}
	if !exitSeen {
		t.Fatal("历史补写进行期间本次退出证据必须已落定")
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("历史补写必须有整批预算（未在预算内返回）")
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("历史补写必须有整批预算，耗时 %v", elapsed)
	}
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("退出后 active 应清空: %+v", obs.Active)
	}
	if len(obs.PendingExits) != runmeta.MaxPendingExits {
		t.Fatalf("预算耗尽时 pending 必须保留: got %d", len(obs.PendingExits))
	}

	release()
	// 重入退出（模拟退出路径重跑）时补写恢复。
	finishDaemonExit(cfg, log, writer, "inst-B", nil, now.Add(2*time.Minute))
	if n := countMonitorRows(t, writer); n != runmeta.MaxPendingExits {
		t.Fatalf("写库恢复后应补写全部缺口, monitor 行数=%d", n)
	}
	obs, _ = runmeta.ReadObservation(path)
	if len(obs.PendingExits) != 0 {
		t.Fatalf("补写成功后 pending 条目应移除: %+v", obs.PendingExits)
	}
}

// 来源未知的旧 failed 证据在下一次退出时不得被覆盖丢失：库中无该实例记录
// 时移入 pending_exits 保留（读端继续提示、补写端按来源规则跳过）。
func TestFinishDaemonExitKeepsUnknownSourcePreviousFailure(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Summary: "daemon exited unexpectedly",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishDaemonExit(cfg, log, usageDB, "inst-B", nil, now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("本次退出应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("来源未知的旧 failed 证据必须移入 pending: %+v", obs.PendingExits)
	}
}

// 更早一代的 active 残留不得覆盖更晚的 last_exit（本代登记写失败后的降级
// 组合）：过期 active 直接清除，更晚的退出证据保留。
func TestRegisterActiveObservationDropsStaleActive(t *testing.T) {
	cfg := testObservationConfig(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-old", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-newer", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "采集已停止",
		},
	}); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, quietLogger(), nil, "inst-current", now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-newer" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("更晚的退出证据不得被过期 active 覆盖: %+v", obs.LastExit)
	}
	if obs.Active == nil || obs.Active.InstanceID != "inst-current" {
		t.Fatalf("本代 active 应登记: %+v", obs.Active)
	}
}

// finishDaemonExit 清除其他代 active 残留：daemon lock 由本代持有，其必然
// 已退出，转为历史 unknown 移入 pending，避免残留 active 压过最近退出。
func TestFinishDaemonExitMigratesStaleActive(t *testing.T) {
	cfg := testObservationConfig(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-old", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	finishDaemonExit(cfg, quietLogger(), nil, "inst-current", nil, now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("残留 active 应清除: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-current" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("本代退出应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-old" || obs.PendingExits[0].Kind != runmeta.ExitKindInterruptedUnknown {
		t.Fatalf("其他代 active 应转为历史 unknown 移入 pending: %+v", obs.PendingExits)
	}
}

// 上一代 active 残留且其证据已在 pending_exits 时，不得再覆盖 last_exit 为
// 推导的 interrupted_unknown（避免同实例重复/降级提示）。
func TestRegisterActiveObservationPendingSupersedesUnknownDerivation(t *testing.T) {
	cfg := testObservationConfig(t)
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-prev", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-earlier", Kind: runmeta.ExitKindStopped,
			DetectedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), Summary: "采集已停止",
		},
	}
	obs.AddPendingExit(runmeta.ExitRecord{
		InstanceID: "inst-prev", Kind: runmeta.ExitKindFailed,
		OccurredAt: now.Add(-30 * time.Minute).Format(time.RFC3339), DetectedAt: now.Add(-30 * time.Minute).Format(time.RFC3339),
		Source: "claude", Summary: "Claude 目录监听失败",
	})
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, quietLogger(), nil, "inst-current", now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-earlier" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("pending 已承载上一代证据时不得改写 last_exit: %+v", obs.LastExit)
	}
}

func countMonitorRows(t *testing.T, usageDB *db.DB) int {
	t.Helper()
	var n int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// safeBuffer 是并发安全的日志缓冲（daemon goroutine 与测试断言可能并发）。
type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *safeBuffer) contains(sub string) bool {
	return strings.Contains(b.String(), sub)
}

// 锁未取得（已有实例运行）时不得写观察文件：本代没有运行证据。
func TestDaemonRun_LockFailureDoesNotWriteObservation(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	lockPath := filepath.Join(cfg.DataDir, "token-usage.lock")
	fl, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	defer ReleaseLock(fl)

	err := Run(context.Background(), cfg, RunOptions{InstanceID: "inst-second"})
	if err == nil {
		t.Fatal("锁被持有时 Run 应返回 already running 错误")
	}
	if _, serr := os.Stat(runmeta.ObservationPath(cfg.DataDir)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("锁未取得时不得写观察文件: %v", serr)
	}
}

// 数据目录完全缺失（直启拿锁失败）：启动失败、不创建替代目录、不伪造退出
// 证据（T5 的目录不可用路径；spawn 准备/父进程预检为既有启动路径，行为保持）。
func TestDaemonRun_MissingDataDirFailsWithoutObservation(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "missing", "data")
	cfg := &config.Config{DataDir: dataDir}
	if err := Run(context.Background(), cfg, RunOptions{InstanceID: "inst-missing"}); err == nil {
		t.Fatal("缺失数据目录应使启动失败")
	}
	if _, serr := os.Stat(dataDir); !os.IsNotExist(serr) {
		t.Fatalf("不得创建替代目录: %v", serr)
	}
	if _, serr := os.Stat(runmeta.ObservationPath(dataDir)); !os.IsNotExist(serr) {
		t.Fatalf("启动失败不得伪造观察文件: %v", serr)
	}
}

// 不可捕获退出（panic/强杀）不会执行 finishDaemonExit：本代 active 必须保留，
// 由下一次启动检测推导为中断未知。active 的清理只由 finishDaemonExit 完成，
// 不得有 defer 兜底把崩溃证据擦掉。
func TestActiveEvidenceSurvivesWithoutFinish(t *testing.T) {
	cfg := testObservationConfig(t)
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.Local)

	registerActiveObservation(cfg, log, nil, "inst-crash", now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil || obs.Active == nil || obs.Active.InstanceID != "inst-crash" {
		t.Fatalf("登记后 active 应存在: %+v err=%v", obs, err)
	}
	// 模拟进程未写退出摘要即消失；下一次启动核对残留证据。
	registerActiveObservation(cfg, log, nil, "inst-next", now.Add(time.Minute))
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.Kind != runmeta.ExitKindInterruptedUnknown || obs.LastExit.InstanceID != "inst-crash" {
		t.Fatalf("无退出记录的残留 active 应被推导为中断未知: %+v", obs.LastExit)
	}
	if obs.Active == nil || obs.Active.InstanceID != "inst-next" {
		t.Fatalf("新实例 active = %+v", obs.Active)
	}
}

// 监控中断的 date 分桶取实际发生时刻的本地日期：故障发生在 23:59、退出处理
// 跨过午夜时，记录不得落到次日（否则 Date 列与 Occurred 列自相矛盾，按发生日
// 查 errors 与区间警告都会漏掉该条）。幂等键不含 date，改分桶不影响查重。
func TestFinishDaemonExit_DateBucketedByOccurredAt(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	log := quietLogger()
	now := time.Date(2026, 11, 12, 0, 0, 10, 0, time.Local) // 退出处理在次日凌晨
	registerActiveObservation(cfg, log, nil, "inst-midnight", now.Add(-time.Minute))
	fatal := &analyzer.MonitorFatalError{
		Client:     "claude",
		OccurredAt: time.Date(2026, 11, 11, 23, 59, 50, 0, time.Local),
		Err:        errors.New("boom"),
	}
	finishDaemonExit(cfg, log, usageDB, "inst-midnight", fatal, now)

	var date string
	if err := usageDB.QueryRow(`SELECT date FROM collection_errors WHERE error_type='monitor'`).Scan(&date); err != nil {
		t.Fatal(err)
	}
	if date != "2026-11-11" {
		t.Fatalf("date = %q, want 2026-11-11（按发生时刻而非检测时刻分桶）", date)
	}
}

// 后续实例正常停止不得抹掉未补齐的缺口：A 代故障只写进观察文件（库写失败）
// 时，B 代启动即把该证据持久化到库；B 正常停止覆盖 last_exit 后，缺口提示
// 与历史记录仍由库承载（state 反映最近一代的 stopped，缺口作为历史提示保留），
// 直到来源补采成功才结案。
func TestFinishDaemonExit_PersistsUnresolvedEvidenceBeforeOverwrite(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	// 用真实时钟：库内 created_at 是 SQLite 的真实 UTC 时刻，与虚构时间比较
	// 会让"pending 是否晚于 last_exit"的判定失真。
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)

	// A 代故障只存在于观察文件（模拟退出时写库失败）。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// B 启动：覆盖前把 A 的故障证据持久化到库。
	registerActiveObservation(cfg, log, usageDB, "inst-B", now)
	if resolved, _ := db.IsMonitorInstanceResolved(context.Background(), usageDB, "inst-A"); resolved {
		t.Fatal("A 的证据应保持未结案")
	}
	if n := countMonitorRows(t, usageDB); n != 1 {
		t.Fatalf("启动时应把观察文件中的 A 证据持久化到库, monitor 行数=%d", n)
	}

	// B 正常停止：覆盖 last_exit，缺口由库承载。
	finishDaemonExit(cfg, log, usageDB, "inst-B", nil, now.Add(time.Minute))
	obs, err := runmeta.ReadObservation(path)
	if err != nil || obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("B 正常停止应覆盖 last_exit: %+v err=%v", obs.LastExit, err)
	}
	if n := countMonitorRows(t, usageDB); n != 1 {
		t.Fatalf("覆盖 last_exit 不得丢失缺口证据, monitor 行数=%d", n)
	}

	// 读端：最近一代为 stopped，缺口作为历史提示保留。
	col := daemonstatus.Load(context.Background(), cfg.DataDir,
		daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, usageDB, now.Add(2*time.Minute))
	if col.State != daemonstatus.StateStopped {
		t.Fatalf("state = %q, want stopped（最近一代正常停止）", col.State)
	}
	if col.Interruption == nil || col.Interruption.Kind != "failed" {
		t.Fatalf("历史缺口应保留提示: %+v", col.Interruption)
	}
}

// finishDaemonExit 的防御分支：观察文件里其他代的 failed 尚未持久化（不经过
// 启动清理路径）时，本次退出仍要落定，旧证据移入 pending_exits。
func TestFinishDaemonExit_MovesUnpersistedPreviousEvidenceToPending(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_monitor BEFORE INSERT ON collection_errors
		WHEN NEW.error_type = 'monitor' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)
	// 直接构造：last_exit=A failed（未持久化）+ active=B，不经过 register 的迁移。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishDaemonExit(cfg, log, usageDB, "inst-B", nil, now.Add(time.Minute))
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Active != nil {
		t.Fatalf("本代 active 应结束: %+v", obs.Active)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("本次退出应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("未持久化的旧证据应移入 pending_exits: %+v", obs.PendingExits)
	}
}

// 补写持续失败（trigger 拒绝插入）时，历史缺口移入 pending_exits 保留、最近
// 一次退出照常落定：B 正常停止写 last_exit=stopped，A 的 failed 摘要留在
// pending_exits；数据库恢复后下一次启动把 A 落库并移除 pending 条目。
func TestFinishDaemonExit_PersistFailureKeepsOldEvidence(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_monitor BEFORE INSERT ON collection_errors
		WHEN NEW.error_type = 'monitor' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)

	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// B 启动：持久化失败 → A 的已知故障移入 pending_exits，不被覆盖丢失。
	registerActiveObservation(cfg, log, usageDB, "inst-B", now)
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit != nil {
		t.Fatalf("持久化失败的旧证据应移出 last_exit: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" || obs.PendingExits[0].Kind != runmeta.ExitKindFailed {
		t.Fatalf("旧故障应保留在 pending_exits: %+v", obs.PendingExits)
	}
	// B 正常停止：持久化仍失败 → pending 保留 A，本次 stopped 照常落定。
	finishDaemonExit(cfg, log, usageDB, "inst-B", nil, now.Add(time.Minute))
	obs, _ = runmeta.ReadObservation(path)
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("最近一次退出应落定 stopped: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("历史缺口应保留在 pending_exits: %+v", obs.PendingExits)
	}
	if obs.Active != nil {
		t.Fatalf("本代 active 仍应结束: %+v", obs.Active)
	}
	if n := countMonitorRows(t, usageDB); n != 0 {
		t.Fatalf("trigger 生效时不应有 monitor 行, got %d", n)
	}

	// 数据库恢复后下一次启动把 A 落库并移除 pending 条目。
	if _, err := usageDB.Exec(`DROP TRIGGER fail_monitor`); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, log, usageDB, "inst-C", now.Add(2*time.Minute))
	if n := countMonitorRows(t, usageDB); n != 1 {
		t.Fatalf("数据库恢复后应把 A 的证据落库, monitor 行数=%d", n)
	}
	obs, _ = runmeta.ReadObservation(path)
	if len(obs.PendingExits) != 0 {
		t.Fatalf("补写成功后 pending 条目应移除: %+v", obs.PendingExits)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindStopped {
		t.Fatalf("C 启动不得覆盖最近一次停止摘要: %+v", obs.LastExit)
	}
}

// 数据库持续写入失败时连续两代已知故障都要保留：A（Claude）写库失败留在
// pending_exits，B 随后因 Codex 失败退出仍要写进 last_exit——最近一次退出
// 与历史缺口分开保存，读端显示最近一次（B），缺口可继续补写。
func TestFinishDaemonExit_PersistFailureKeepsNewFailure(t *testing.T) {
	cfg := testObservationConfig(t)
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	if _, err := usageDB.Exec(`CREATE TRIGGER fail_monitor BEFORE INSERT ON collection_errors
		WHEN NEW.error_type = 'monitor' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	log := quietLogger()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	occurred := now.Add(-10 * time.Minute)

	// A 代 Claude 故障只存在于观察文件（写库失败）。
	if err := runmeta.WriteObservation(path, runmeta.Observation{
		Version: runmeta.ObservationVersion,
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: occurred.Format(time.RFC3339), DetectedAt: occurred.Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// B 启动（写库仍失败）：A 移入 pending_exits。
	registerActiveObservation(cfg, log, usageDB, "inst-B", now.Add(time.Minute))
	// B 因 Codex 故障退出：本次失败必须写进 last_exit。
	fatal := &analyzer.MonitorFatalError{
		Client:     "codex",
		OccurredAt: now.Add(2 * time.Minute),
		Err:        errors.New("fsnotify watcher error: boom"),
	}
	finishDaemonExit(cfg, log, usageDB, "inst-B", fatal, now.Add(3*time.Minute))
	obs, err := runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Kind != runmeta.ExitKindFailed || obs.LastExit.Source != "codex" {
		t.Fatalf("本次已知故障必须写入 last_exit: %+v", obs.LastExit)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" || obs.PendingExits[0].Source != "claude" {
		t.Fatalf("历史缺口应保留在 pending_exits: %+v", obs.PendingExits)
	}

	// 读端显示最近一次退出（B 的 Codex 故障），而不是被 A 挡住。
	col := daemonstatus.Load(context.Background(), cfg.DataDir,
		daemonstatus.Snapshot{Probe: daemonstatus.ProbeStopped}, usageDB, now.Add(4*time.Minute))
	if col.State != daemonstatus.StateInterrupted {
		t.Fatalf("state = %q, want interrupted", col.State)
	}
	if col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "codex" {
		t.Fatalf("应提示最近一次 Codex 故障: %+v", col.Interruption)
	}

	// 数据库恢复后补写两代证据：A 从 pending 移除，B 由启动路径补写。
	if _, err := usageDB.Exec(`DROP TRIGGER fail_monitor`); err != nil {
		t.Fatal(err)
	}
	registerActiveObservation(cfg, log, usageDB, "inst-C", now.Add(5*time.Minute))
	if n := countMonitorRows(t, usageDB); n != 2 {
		t.Fatalf("数据库恢复后两代证据都应落库, monitor 行数=%d", n)
	}
	obs, _ = runmeta.ReadObservation(path)
	if len(obs.PendingExits) != 0 {
		t.Fatalf("补写成功后 pending 条目应移除: %+v", obs.PendingExits)
	}
	if obs.LastExit == nil || obs.LastExit.InstanceID != "inst-B" || obs.LastExit.Source != "codex" {
		t.Fatalf("last_exit 应保持最近一次退出: %+v", obs.LastExit)
	}
}
