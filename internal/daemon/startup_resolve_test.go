package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// 启动协调器结案判定：只有本轮补采完整完成且零失败、且故障来源仍在本轮
// 客户端源补采范围内，才按实例结案监控中断；失败、提前取消、来源未知/禁用
// 或无客户端源请求时保留缺口提示。
func TestCoordinatorResolveInterruptions(t *testing.T) {
	cases := []struct {
		name         string
		pending      []pendingInterruption
		failOn       map[int]error
		extraClients []string
		wantResolved []string
	}{
		{
			name:         "caught-up-and-covered",
			pending:      []pendingInterruption{{InstanceID: "prev", Source: "claude"}},
			wantResolved: []string{"prev"},
		},
		{
			name:    "catch-up-failed",
			pending: []pendingInterruption{{InstanceID: "prev", Source: "claude"}},
			failOn:  map[int]error{0: errors.New("boom")},
		},
		{
			name:    "cancelled-early",
			pending: []pendingInterruption{{InstanceID: "prev", Source: "claude"}},
			failOn:  map[int]error{0: context.Canceled},
		},
		{
			name:    "unknown-source",
			pending: []pendingInterruption{{InstanceID: "prev", Source: ""}},
		},
		{
			name:    "disabled-source",
			pending: []pendingInterruption{{InstanceID: "prev", Source: "codex"}},
		},
		{
			name:         "mixed-only-covered-resolved",
			pending:      []pendingInterruption{{InstanceID: "p1", Source: "claude"}, {InstanceID: "p2", Source: ""}},
			wantResolved: []string{"p1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfg := buildEnabledClients(t, tmpDir, append([]string{"claude"}, tc.extraClients...), nil)
			rec := &fakeSubmitRecorder{failOn: tc.failOn}
			coord := newStartupCoordinator(cfg, rec.submit, nil, 1, "inst-1", nil)
			coord.pendingInterruptions = tc.pending
			var resolved []string
			coord.resolveInterruption = func(ctx context.Context, it pendingInterruption) error {
				resolved = append(resolved, it.InstanceID)
				return nil
			}
			ready := make(chan struct{})
			close(ready)
			fatal := make(chan error, 1)
			coord.run(context.Background(), ready, fatal)

			if len(resolved) != len(tc.wantResolved) {
				t.Fatalf("resolved = %v, want %v", resolved, tc.wantResolved)
			}
			for i := range tc.wantResolved {
				if resolved[i] != tc.wantResolved[i] {
					t.Fatalf("resolved = %v, want %v", resolved, tc.wantResolved)
				}
			}
		})
	}
}

// 结案回调失败只记日志，不阻塞后续条目。
func TestCoordinatorResolveContinuesOnError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)
	rec := &fakeSubmitRecorder{}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 1, "inst-1", nil)
	coord.pendingInterruptions = []pendingInterruption{
		{InstanceID: "p1", Source: "claude"},
		{InstanceID: "p2", Source: "claude"},
	}
	var resolved []string
	coord.resolveInterruption = func(ctx context.Context, it pendingInterruption) error {
		resolved = append(resolved, it.InstanceID)
		if it.InstanceID == "p1" {
			return errors.New("db locked")
		}
		return nil
	}
	ready := make(chan struct{})
	close(ready)
	coord.run(context.Background(), ready, make(chan error, 1))
	if len(resolved) != 2 {
		t.Fatalf("单条结案失败不得跳过后续条目: %v", resolved)
	}
}

// 已启用但没有任何客户端源补采请求的来源不得被全局成功冒充补齐：守卫必须
// 命中 catchUpRequestsFor 为空处（该 client 不在受支持分类内，配置加载层
// 不会产生这种条目，直接用 Config 结构模拟防御路径）。
func TestCoordinatorResolveSkipsSourceWithoutClientRequests(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true},
		"custom": {Enabled: true},
	}}
	rec := &fakeSubmitRecorder{}
	coord := newStartupCoordinator(cfg, rec.submit, nil, 1, "inst-1", nil)
	coord.pendingInterruptions = []pendingInterruption{{InstanceID: "prev", Source: "custom"}}
	var resolved []string
	coord.resolveInterruption = func(ctx context.Context, it pendingInterruption) error {
		resolved = append(resolved, it.InstanceID)
		return nil
	}
	ready := make(chan struct{})
	close(ready)
	coord.run(context.Background(), ready, make(chan error, 1))
	if len(resolved) != 0 {
		t.Fatalf("无客户端源补采请求的来源不得结案: %v", resolved)
	}
}

// 观察文件中来源已知、尚未结案的 failed 退出摘要也纳入待结案集合（退出时
// 写库失败时它是唯一证据）；interrupted_unknown 来源未知不纳结案；库中已
// 结案的同实例不再纳入；同一实例库证据优先。
func TestLoadPendingInterruptionsMergesObservation(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, usageDB, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-db", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	writeExit := func(kind, source string) {
		t.Helper()
		obs := runmeta.Observation{Version: runmeta.ObservationVersion, LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-obs", Kind: kind, OccurredAt: "2026-11-11T16:00:00+08:00",
			DetectedAt: "2026-11-11T16:00:00+08:00", Source: source, Summary: "Claude 目录监听失败",
		}}
		if err := runmeta.WriteObservation(runmeta.ObservationPath(cfg.DataDir), obs); err != nil {
			t.Fatal(err)
		}
	}

	writeExit(runmeta.ExitKindFailed, "claude")
	got := loadPendingInterruptions(usageDB, cfg.DataDir)
	if len(got) != 2 {
		t.Fatalf("pending = %+v, want 2（库 + 观察文件）", got)
	}
	var obsPending *pendingInterruption
	for i := range got {
		if got[i].InstanceID == "inst-obs" {
			obsPending = &got[i]
		}
	}
	if obsPending == nil || obsPending.Source != "claude" || obsPending.Kind != "failed" || obsPending.OccurredAt == "" || obsPending.Summary == "" {
		t.Fatalf("观察文件证据字段不完整: %+v", obsPending)
	}

	// 已结案的同实例不再纳入。
	if err := db.ConfirmMonitorInterruption(ctx, usageDB, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-obs", Kind: "failed", OccurredAt: "2026-11-11T16:00:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got := loadPendingInterruptions(usageDB, cfg.DataDir); len(got) != 1 || got[0].InstanceID != "inst-db" {
		t.Fatalf("已结案实例不应再纳入: %+v", got)
	}

	// interrupted_unknown（来源未知）不纳入。
	writeExit(runmeta.ExitKindInterruptedUnknown, "claude")
	if got := loadPendingInterruptions(usageDB, cfg.DataDir); len(got) != 1 {
		t.Fatalf("来源未知的中断不应纳入结案集合: %+v", got)
	}
}

// 仅存在于观察文件 pending_exits 的缺口也纳入待结案集合；补采确认时写入
// 结案记录并移除文件条目——即使数据库只拒绝未结案 monitor 行（结案行可写）。
func TestPendingExitsParticipateInStartupResolution(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	now := time.Now()
	obs := runmeta.Observation{Version: runmeta.ObservationVersion}
	obs.AddPendingExit(runmeta.ExitRecord{
		InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
		OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
		Source: "claude", Summary: "Claude 目录监听失败",
	})
	if err := runmeta.WriteObservation(runmeta.ObservationPath(cfg.DataDir), obs); err != nil {
		t.Fatal(err)
	}

	got := loadPendingInterruptions(usageDB, cfg.DataDir)
	if len(got) != 1 || got[0].InstanceID != "inst-A" || got[0].Source != "claude" || got[0].Kind != "failed" {
		t.Fatalf("pending_exits 中的缺口必须纳入待结案集合: %+v", got)
	}

	// 数据库只拒绝未结案 monitor 行，结案行可写。
	if _, err := usageDB.Exec(`CREATE TRIGGER block_unresolved BEFORE INSERT ON collection_errors
		WHEN NEW.error_type = 'monitor' AND NEW.resolved = 0 BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := resolvePendingInterruption(context.Background(), usageDB, cfg.DataDir, quietLogger(), got[0]); err != nil {
		t.Fatalf("仅存在于 pending_exits 的缺口应能完成补采确认: %v", err)
	}
	if resolved, rerr := db.IsMonitorInstanceResolved(context.Background(), usageDB, "inst-A"); rerr != nil || !resolved {
		t.Fatalf("应写入结案确认行: resolved=%v err=%v", resolved, rerr)
	}
	obs, err = runmeta.ReadObservation(runmeta.ObservationPath(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.PendingExits) != 0 {
		t.Fatalf("结案后应移除文件条目: %+v", obs.PendingExits)
	}
	if got := loadPendingInterruptions(usageDB, cfg.DataDir); len(got) != 0 {
		t.Fatalf("已结案实例不应再纳入: %+v", got)
	}
}

// 结案后的文件清理只使用批次剩余预算：观察文件锁被占用时不得再等默认上限
// （会推迟 daemon 退出），拿不到锁就保留文件条目。
func TestResolvePendingInterruptionUsesRemainingBudget(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	obs := runmeta.Observation{Version: runmeta.ObservationVersion}
	obs.AddPendingExit(runmeta.ExitRecord{
		InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
		OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:35:00+08:00",
		Source: "claude", Summary: "Claude 目录监听失败",
	})
	path := runmeta.ObservationPath(cfg.DataDir)
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	// 占用观察文件写锁：清理只能在剩余预算内等待。
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	defer func() { _ = lock.Unlock() }()

	it := pendingInterruption{
		InstanceID: "inst-A", Source: "claude", Kind: "failed",
		OccurredAt: "2026-11-11T15:35:00+08:00", Summary: "Claude 目录监听失败",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := resolvePendingInterruption(ctx, usageDB, cfg.DataDir, quietLogger(), it); err != nil {
		t.Fatalf("结案应在预算内完成: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("文件清理必须使用剩余预算，耗时 %v", elapsed)
	}
	if resolved, rerr := db.IsMonitorInstanceResolved(context.Background(), usageDB, "inst-A"); rerr != nil || !resolved {
		t.Fatalf("库应已结案: resolved=%v err=%v", resolved, rerr)
	}
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.PendingExits) != 1 {
		t.Fatalf("未拿到文件锁时应保留条目: %+v", obs.PendingExits)
	}
}

// 停止路径的观察文件锁等待不得串联超时：结案清理在等待期间取消应尽快返回，
// 退出收尾的可选清理只尝试一次——整体不超过 daemon stop 的等待窗口。
func TestStopPathLockWaitsStayWithinStopWindow(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	usageDB, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
	}
	obs.AddPendingExit(runmeta.ExitRecord{
		InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
		OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
		Source: "claude", Summary: "Claude 目录监听失败",
	})
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	// 观察文件锁被另一写者持有（模拟停止时并行的观察文件写）。
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	defer func() { _ = lock.Unlock() }()

	// 结案清理等待期间停止信号到达：批次 deadline 充足（5s），显式取消在
	// 100ms 后触发——不响应取消的实现会继续等满约 5s。
	it := pendingInterruption{
		InstanceID: "inst-A", Source: "claude", Kind: "failed",
		OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), Summary: "Claude 目录监听失败",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := resolvePendingInterruption(ctx, usageDB, cfg.DataDir, quietLogger(), it); err != nil {
		t.Fatalf("取消时的结案清理不应报错: %v", err)
	}
	// 协调器返回后退出收尾：写退出摘要等锁最多 2s，可选清理只尝试一次。
	finishDaemonExit(cfg, quietLogger(), usageDB, "inst-B", nil, now.Add(time.Minute))
	elapsed := time.Since(start)
	if elapsed > 3500*time.Millisecond {
		t.Fatalf("停止路径锁等待串联超出窗口，耗时 %v", elapsed)
	}
	// 结案已入库；未拿到文件锁时条目必须保留（读端按库证据抑制）。
	if resolved, rerr := db.IsMonitorInstanceResolved(context.Background(), usageDB, "inst-A"); rerr != nil || !resolved {
		t.Fatalf("库应已结案: resolved=%v err=%v", resolved, rerr)
	}
	obs, err = runmeta.ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.PendingExits) != 1 || obs.PendingExits[0].InstanceID != "inst-A" {
		t.Fatalf("未拿到文件锁时应保留待清理条目: %+v", obs.PendingExits)
	}
}

// 文件锁与数据库写锁同时被长期占用时，退出收尾的各段等待必须共享入口总
// 预算：注入短预算后总耗时紧贴预算（而非各段固定上限串联）。
func TestFinishExitBudgetCapsCombinedWaits(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	path := runmeta.ObservationPath(cfg.DataDir)
	now := time.Now()
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-B", StartedAt: now.Format(time.RFC3339)},
		LastExit: &runmeta.ExitRecord{
			InstanceID: "inst-A", Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		},
	}
	for i := 0; i < 4; i++ {
		obs.AddPendingExit(runmeta.ExitRecord{
			InstanceID: fmt.Sprintf("inst-p%d", i), Kind: runmeta.ExitKindFailed,
			OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), DetectedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Source: "claude", Summary: "Claude 目录监听失败",
		})
	}
	if err := runmeta.WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	fileLock := flock.New(path + ".lock")
	locked, err := fileLock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	defer func() { _ = fileLock.Unlock() }()
	releaseDB := holdSQLiteWriteLock(t, dbPath)
	defer releaseDB()

	// 200ms 入口预算：无入口预算时固定串联约 4s（旧证据写库 0.5s + 摘要 2s +
	// 中断记录 0.5s + 补写 1s），注入后各段共享剩余预算，总耗时紧贴预算。
	start := time.Now()
	finishDaemonExitWithBudget(cfg, quietLogger(), usageDB, "inst-B", errors.New("boom"), now.Add(time.Minute), 200*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 900*time.Millisecond {
		t.Fatalf("退出收尾各段等待必须共享入口预算，耗时 %v", elapsed)
	}
}

// 整批补采结案有时间预算：数据库写锁被长期持有时 resolveInterruptions 在
// 预算内返回，剩余条目保留待下次启动重试，不拖住 daemon 退出。
func TestCoordinatorResolveBudgetUnderWriteLock(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	release := holdSQLiteWriteLock(t, dbPath)
	defer release()

	coord := newStartupCoordinator(cfg, (&fakeSubmitRecorder{}).submit, nil, 1, "inst-1", nil)
	for i := 0; i < runmeta.MaxPendingExits; i++ {
		coord.pendingInterruptions = append(coord.pendingInterruptions, pendingInterruption{
			InstanceID: fmt.Sprintf("inst-%02d", i), Source: "claude", Kind: "failed",
			OccurredAt: time.Now().Add(-time.Hour).Format(time.RFC3339), Summary: "Claude 目录监听失败",
		})
	}
	coord.resolveInterruption = func(ctx context.Context, it pendingInterruption) error {
		return resolvePendingInterruption(ctx, usageDB, cfg.DataDir, quietLogger(), it)
	}
	start := time.Now()
	coord.resolveInterruptions(context.Background(), true)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("整批结案必须有预算，耗时 %v", elapsed)
	}
	if n := countMonitorRows(t, usageDB); n != 0 {
		t.Fatalf("持锁期间不应写入结案行, got %d", n)
	}
}

// loadPendingInterruptions 的保守分支与去重：库不可查时文件缺口必须保留；
// 同实例库证据优先；pending_exits 中的 interrupted_unknown/空来源不纳入。
func TestLoadPendingInterruptionsConservativeBranches(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	dbPath := filepath.Join(cfg.DataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	ctx := context.Background()
	if err := db.RecordMonitorError(ctx, usageDB, "2026-11-11", "claude", "库内摘要", db.MonitorErrorDetail{
		InstanceID: "inst-dup", Kind: "failed", OccurredAt: "2026-11-11T15:00:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	obs := runmeta.Observation{Version: runmeta.ObservationVersion}
	obs.AddPendingExit(runmeta.ExitRecord{InstanceID: "inst-dup", Kind: runmeta.ExitKindFailed, OccurredAt: "2026-11-11T16:00:00+08:00", DetectedAt: "2026-11-11T16:00:00+08:00", Source: "claude", Summary: "文件摘要"})
	obs.AddPendingExit(runmeta.ExitRecord{InstanceID: "inst-unknown", Kind: runmeta.ExitKindInterruptedUnknown, OccurredAt: "2026-11-11T16:10:00+08:00", DetectedAt: "2026-11-11T16:10:00+08:00", Source: "claude", Summary: "原因未知"})
	obs.AddPendingExit(runmeta.ExitRecord{InstanceID: "inst-nosrc", Kind: runmeta.ExitKindFailed, OccurredAt: "2026-11-11T16:20:00+08:00", DetectedAt: "2026-11-11T16:20:00+08:00", Summary: "无来源"})
	if err := runmeta.WriteObservation(runmeta.ObservationPath(cfg.DataDir), obs); err != nil {
		t.Fatal(err)
	}
	got := loadPendingInterruptions(usageDB, cfg.DataDir)
	if len(got) != 1 || got[0].InstanceID != "inst-dup" || got[0].Summary != "库内摘要" {
		t.Fatalf("同实例应只保留库证据且不纳入 unknown/无来源: %+v", got)
	}

	// 库不可查：文件中的 failed 缺口保守保留。
	closed, err := db.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	obs = runmeta.Observation{Version: runmeta.ObservationVersion}
	obs.AddPendingExit(runmeta.ExitRecord{InstanceID: "inst-A", Kind: runmeta.ExitKindFailed, OccurredAt: "2026-11-11T15:35:00+08:00", DetectedAt: "2026-11-11T15:35:00+08:00", Source: "claude", Summary: "Claude 目录监听失败"})
	if err := runmeta.WriteObservation(runmeta.ObservationPath(cfg.DataDir), obs); err != nil {
		t.Fatal(err)
	}
	got = loadPendingInterruptions(closed, cfg.DataDir)
	if len(got) != 1 || got[0].InstanceID != "inst-A" {
		t.Fatalf("库查询失败时文件缺口必须保守保留: %+v", got)
	}
}

// 提前终止（单项 Submit 超时但父 ctx 仍有效）：最终阶段不得发布 succeeded——
// 零失败不等于整轮完成；failures 至少 1 以满足 failed 状态契约。
func TestCoordinatorEarlyStopPublishesFailed(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := buildEnabledClients(t, tmpDir, []string{"claude"}, nil)
	rec := &fakeSubmitRecorder{failOn: map[int]error{0: context.DeadlineExceeded}}
	var final runmetaState
	write := func(st runmetaState) error { final = st; return nil }
	coord := newStartupCoordinator(cfg, rec.submit, write, 1, "inst-1", nil)
	ready := make(chan struct{})
	close(ready)
	coord.run(context.Background(), ready, make(chan error, 1))
	if final.catchUp != phaseFailed || final.catchUpFailures < 1 {
		t.Fatalf("final = %+v, want failed 且 failures>=1（提前终止不得发布 succeeded）", final)
	}
}
