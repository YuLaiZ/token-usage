package runmeta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

// observation 文件协议：原子读写、版本与字段校验、缺失/损坏只降级。
func TestObservationRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	obs := Observation{
		Version: ObservationVersion,
		Active:  &ActiveRun{InstanceID: "inst-1", StartedAt: "2026-11-11T10:00:00+08:00"},
		LastExit: &ExitRecord{
			InstanceID: "inst-0",
			Kind:       ExitKindFailed,
			OccurredAt: "2026-11-11T09:59:00+08:00",
			DetectedAt: "2026-11-11T09:59:01+08:00",
			Source:     "claude",
			Summary:    "Claude 目录监听失败",
		},
	}
	if err := WriteObservation(path, obs); err != nil {
		t.Fatalf("WriteObservation: %v", err)
	}
	got, err := ReadObservation(path)
	if err != nil {
		t.Fatalf("ReadObservation: %v", err)
	}
	if got.Active == nil || got.Active.InstanceID != "inst-1" {
		t.Fatalf("active round-trip 失败: %+v", got)
	}
	if got.LastExit == nil || got.LastExit.Kind != ExitKindFailed || got.LastExit.Source != "claude" {
		t.Fatalf("last_exit round-trip 失败: %+v", got)
	}
	if got.Version != ObservationVersion {
		t.Fatalf("version = %d, want %d", got.Version, ObservationVersion)
	}
}

// PendingExits 协议：按实例键幂等去重、超上限丢弃最旧、补写后移除；序列化
// 往返与字段校验（非法条目拒绝读写）。
func TestObservationPendingExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	obs := Observation{Version: ObservationVersion}
	rec := ExitRecord{
		InstanceID: "inst-a", Kind: ExitKindFailed,
		OccurredAt: "2026-11-11T09:59:00+08:00", DetectedAt: "2026-11-11T09:59:01+08:00",
		Source: "claude", Summary: "Claude 目录监听失败",
	}
	obs.AddPendingExit(rec)
	obs.AddPendingExit(rec) // 同实例幂等
	if len(obs.PendingExits) != 1 {
		t.Fatalf("同实例重复追加应幂等: %+v", obs.PendingExits)
	}
	obs.AddPendingExit(ExitRecord{
		InstanceID: "inst-b", Kind: ExitKindFailed,
		OccurredAt: "2026-11-11T10:00:00+08:00", DetectedAt: "2026-11-11T10:00:01+08:00",
		Source: "codex", Summary: "Codex 目录监听失败",
	})
	if !obs.HasPendingExit("inst-a") || !obs.HasPendingExit("inst-b") || obs.HasPendingExit("inst-x") {
		t.Fatalf("存在性判断错误: %+v", obs.PendingExits)
	}
	obs.RemovePendingExit("inst-a")
	if obs.HasPendingExit("inst-a") || !obs.HasPendingExit("inst-b") || len(obs.PendingExits) != 1 {
		t.Fatalf("按实例移除错误: %+v", obs.PendingExits)
	}

	// 超上限丢弃最旧，保留最近的条目。
	for i := 0; i < MaxPendingExits+5; i++ {
		obs.AddPendingExit(ExitRecord{
			InstanceID: fmt.Sprintf("inst-%03d", i), Kind: ExitKindFailed,
			DetectedAt: "2026-11-11T10:00:00+08:00", Source: "claude", Summary: "x",
		})
	}
	if len(obs.PendingExits) != MaxPendingExits {
		t.Fatalf("pending 条数 = %d, want %d", len(obs.PendingExits), MaxPendingExits)
	}
	if last := obs.PendingExits[len(obs.PendingExits)-1].InstanceID; last != fmt.Sprintf("inst-%03d", MaxPendingExits+4) {
		t.Fatalf("应保留最近的条目，最后一条 = %s", last)
	}

	// 序列化往返。
	if err := WriteObservation(path, obs); err != nil {
		t.Fatalf("WriteObservation: %v", err)
	}
	got, err := ReadObservation(path)
	if err != nil {
		t.Fatalf("ReadObservation: %v", err)
	}
	if len(got.PendingExits) != len(obs.PendingExits) || got.PendingExits[0].InstanceID != obs.PendingExits[0].InstanceID {
		t.Fatalf("pending round-trip 失败: %+v", got.PendingExits)
	}

	// 非法条目拒绝写入与读取。
	invalid := Observation{Version: ObservationVersion, PendingExits: []ExitRecord{{
		InstanceID: "", Kind: ExitKindFailed, DetectedAt: "2026-11-11T10:00:00Z", Summary: "x",
	}}}
	if err := WriteObservation(path, invalid); err == nil {
		t.Fatal("缺 instance_id 的 pending 条目必须拒绝写入")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"pending_exits":[{"instance_id":"i","kind":"boom","detected_at":"2026-11-11T10:00:00Z","summary":"s"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadObservation(path); err == nil {
		t.Fatal("非法 kind 的 pending 条目必须拒绝读取")
	}
}

// UpdateObservationWithLockTimeout 以调用方给定的上限等待文件锁：锁被占用时
// 在约 lockTimeout 内返回超时错误；释放后读-改-写成功。
func TestUpdateObservationWithLockTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	if err := WriteObservation(path, Observation{
		Version:  ObservationVersion,
		LastExit: &ExitRecord{InstanceID: "i", Kind: ExitKindStopped, DetectedAt: "2026-11-11T10:00:00Z", Summary: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	start := time.Now()
	err = UpdateObservationWithLockTimeout(path, 150*time.Millisecond, func(obs *Observation) error {
		obs.LastExit.Summary = "blocked"
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("锁被占用时应返回超时错误")
	}
	if elapsed > time.Second {
		t.Fatalf("应按给定上限返回，耗时 %v", elapsed)
	}
	if uerr := lock.Unlock(); uerr != nil {
		t.Fatal(uerr)
	}
	if err := UpdateObservationWithLockTimeout(path, 150*time.Millisecond, func(obs *Observation) error {
		obs.LastExit.Summary = "changed"
		return nil
	}); err != nil {
		t.Fatalf("锁释放后应成功: %v", err)
	}
	obs, err := ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if obs.LastExit.Summary != "changed" {
		t.Fatalf("summary = %q, want changed", obs.LastExit.Summary)
	}
}

// UpdateObservationContext 在锁等待期间响应取消：取消后立即返回，不等满
// 调用方给定的上限（停止路径不能把已无意义的等待拖到超时）。
func TestUpdateObservationContextCancelsWhileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	if err := WriteObservation(path, Observation{
		Version:  ObservationVersion,
		LastExit: &ExitRecord{InstanceID: "i", Kind: ExitKindStopped, DetectedAt: "2026-11-11T10:00:00Z", Summary: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("占用观察文件锁失败: locked=%v err=%v", locked, err)
	}
	defer func() { _ = lock.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = UpdateObservationContext(ctx, path, 5*time.Second, func(obs *Observation) error {
		obs.LastExit.Summary = "changed"
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回取消错误: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("锁等待必须响应取消，耗时 %v", elapsed)
	}
	// 取消后不得写回。
	obs, rerr := ReadObservation(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if obs.LastExit.Summary != "s" {
		t.Fatalf("取消后不得写回 mutate 结果: summary=%q", obs.LastExit.Summary)
	}
}

// 锁空闲但调用前 ctx 已取消：不执行写回、不泄漏锁（取消边界覆盖等待以外）。
func TestUpdateObservationContextCancelledBeforeLockFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	if err := WriteObservation(path, Observation{
		Version:  ObservationVersion,
		LastExit: &ExitRecord{InstanceID: "i", Kind: ExitKindStopped, DetectedAt: "2026-11-11T10:00:00Z", Summary: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := UpdateObservationContext(ctx, path, time.Second, func(obs *Observation) error {
		obs.LastExit.Summary = "changed"
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应返回取消错误: %v", err)
	}
	obs, rerr := ReadObservation(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if obs.LastExit.Summary != "s" {
		t.Fatalf("取消后不得写回 mutate 结果: summary=%q", obs.LastExit.Summary)
	}
	// 取消路径必须释放锁：紧接着的正常写入应成功。
	if err := UpdateObservation(path, func(obs *Observation) error {
		obs.LastExit.Summary = "ok"
		return nil
	}); err != nil {
		t.Fatalf("取消路径必须释放锁: %v", err)
	}
}

// pending 达到容量上限时驱逐最旧条目不静默：必须留下包含被驱逐实例的告警。
func TestObservationPendingExitEvictionLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	obs := Observation{Version: ObservationVersion}
	for i := 0; i < MaxPendingExits; i++ {
		obs.AddPendingExit(ExitRecord{
			InstanceID: fmt.Sprintf("inst-%03d", i), Kind: ExitKindFailed,
			DetectedAt: "2026-11-11T10:00:00+08:00", Source: "claude", Summary: "x",
		})
	}
	buf.Reset()
	obs.AddPendingExit(ExitRecord{
		InstanceID: "inst-overflow", Kind: ExitKindFailed,
		DetectedAt: "2026-11-11T10:00:00+08:00", Source: "claude", Summary: "x",
	})
	got := buf.String()
	if !strings.Contains(got, "evicted") || !strings.Contains(got, "inst-000") {
		t.Fatalf("驱逐最旧条目必须告警并包含被驱逐实例: %q", got)
	}
	if len(obs.PendingExits) != MaxPendingExits {
		t.Fatalf("pending 条数 = %d, want %d", len(obs.PendingExits), MaxPendingExits)
	}
}

func TestObservationInvalidPayloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.json")
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown version", `{"version":2}`},
		{"bad kind", `{"version":1,"last_exit":{"instance_id":"i","kind":"boom","detected_at":"2026-11-11T10:00:00Z","summary":"s"}}`},
		{"empty summary", `{"version":1,"last_exit":{"instance_id":"i","kind":"failed","detected_at":"2026-11-11T10:00:00Z","summary":""}}`},
		{"bad time", `{"version":1,"active":{"instance_id":"i","started_at":"yesterday"}}`},
		{"empty active id", `{"version":1,"active":{"instance_id":"","started_at":"2026-11-11T10:00:00Z"}}`},
		{"not json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadObservation(path); err == nil {
				t.Fatalf("期望校验失败: %s", tc.raw)
			}
		})
	}
	if _, err := ReadObservation(filepath.Join(dir, "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("缺失文件应返回 fs.ErrNotExist: %v", err)
	}
}

// observation 文件不随旧代元数据清理被删除，只清精确 temp 残留。
func TestCleanupMetadataKeepsObservation(t *testing.T) {
	dir := t.TempDir()
	path := ObservationPath(dir)
	if err := WriteObservation(path, Observation{Version: ObservationVersion}); err != nil {
		t.Fatal(err)
	}
	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("CleanupStaleMetadata: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("observation 不应被旧代清理删除: %v", err)
	}
}

// observation 的精确 temp 残留随旧代元数据清理一并回收（文件本体保留）。
func TestCleanupMetadataRemovesObservationTempFiles(t *testing.T) {
	dir := t.TempDir()
	tempPath := filepath.Join(dir, fileutil.TempPrefix(observationFileName)+"12345")
	if err := os.WriteFile(tempPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("CleanupStaleMetadata: %v", err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("observation temp 残留应被清理: %v", err)
	}
}

// UpdateObservation 串行化并发读-改-写：多个写者并发追加同一字段不得丢更新
// （无锁实现会因整文件覆盖丢失并发写）。
func TestUpdateObservationSerializesWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-observation.json")
	if err := WriteObservation(path, Observation{
		Version:  ObservationVersion,
		LastExit: &ExitRecord{InstanceID: "i", Kind: ExitKindStopped, DetectedAt: "2026-11-11T10:00:00Z", Summary: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	const writers = 20
	var wg sync.WaitGroup
	var failures int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := UpdateObservation(path, func(obs *Observation) error {
				obs.LastExit.Summary += "x"
				return nil
			}); err != nil {
				atomic.AddInt64(&failures, 1)
			}
		}()
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("写入失败 %d 次", failures)
	}
	obs, err := ReadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(obs.LastExit.Summary); got != writers+1 {
		t.Fatalf("并发更新丢失：summary 长度 = %d, want %d（读-改-写必须串行化）", got, writers+1)
	}
}
