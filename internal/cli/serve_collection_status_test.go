package cli

// serve 采集状态来源：三态判活 + PID/runtime-state 匹配的阶段读取，与
// daemon status 共用 daemonstatus.Load；只读，不写库、不改元数据。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

func TestServeCollectionStatus(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	d, err := db.Open(filepath.Join(cfg.DataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()

	// 无锁文件：未运行、无中断证据。
	col := serveCollectionStatus(ctx, cfg, d)
	if col.State != "stopped" || col.CatchUp != nil || col.Interruption != nil {
		t.Fatalf("无锁文件时 collection = %+v", col)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "token-usage.lock")); err == nil {
		t.Fatal("只读状态查询不得创建锁文件")
	}

	// 持有 daemon lock + PID/runtime-state 全匹配 → running + catch_up 同源。
	lockPath := filepath.Join(cfg.DataDir, "token-usage.lock")
	fl, ok := daemon.AcquireLock(lockPath)
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	defer daemon.ReleaseLock(fl)
	if err := runmeta.WritePIDFile(runmeta.PIDPath(cfg.DataDir), os.Getpid(), "inst-serve"); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteRuntimeState(runmeta.StatePath(cfg.DataDir), runmeta.RuntimeState{
		PID: os.Getpid(), InstanceID: "inst-serve", MonitorReady: true, CatchUp: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	col = serveCollectionStatus(ctx, cfg, d)
	if col.State != "running" || col.CatchUp == nil || *col.CatchUp != "succeeded" {
		t.Fatalf("运行中 collection = %+v", col)
	}

	// 持锁但 PID 元数据不匹配（旧代残留）：阶段不可信 → unknown。
	if err := runmeta.WriteRuntimeState(runmeta.StatePath(cfg.DataDir), runmeta.RuntimeState{
		PID: os.Getpid() + 1, InstanceID: "inst-stale", MonitorReady: true, CatchUp: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	col = serveCollectionStatus(ctx, cfg, d)
	if col.State != "unknown" {
		t.Fatalf("PID 不匹配时 state = %q, want unknown", col.State)
	}

	// 未运行 + 已有未结案监控中断：中断提示可见（DB 证据优先）。
	if err := daemon.ReleaseLock(fl); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-dead", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	col = serveCollectionStatus(ctx, cfg, d)
	if col.State != "interrupted" || col.Interruption == nil || col.Interruption.Source == nil || *col.Interruption.Source != "claude" {
		t.Fatalf("未运行+中断 collection = %+v", col)
	}
}
