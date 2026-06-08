// internal/analyzer/sqlite_poller_test.go
package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

func TestGetSQLiteMtime_MainOnly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// 创建 .db 文件
	os.WriteFile(dbPath, []byte("test"), 0644)
	time.Sleep(20 * time.Millisecond) // 增加 sleep 确保 mtime 可区分

	mtime := GetSQLiteMtime(dbPath)
	if mtime == 0 {
		t.Error("expected non-zero mtime")
	}
}

func TestGetSQLiteMtime_WithWAL(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	walPath := dbPath + "-wal"

	// 创建 .db 文件
	os.WriteFile(dbPath, []byte("test"), 0644)
	time.Sleep(50 * time.Millisecond) // 增加 sleep 确保 mtime 可区分

	// 创建 .db-wal 文件（mtime 更晚）
	os.WriteFile(walPath, []byte("wal data"), 0644)

	mtime := GetSQLiteMtime(dbPath)
	walMtime := getFileMtime(walPath)

	if mtime < walMtime {
		t.Errorf("GetSQLiteMtime should return max(db, wal), got db=%d wal=%d", mtime, walMtime)
	}
}

func TestSQLitePoller_DetectsChange(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// 创建初始文件
	os.WriteFile(dbPath, []byte("initial"), 0644)

	var triggered int32
	poller := NewSQLitePoller(
		dbPath,
		"test_client",
		collector.CollectRequest{Incremental: true},
		50*time.Millisecond, // 快速轮询用于测试
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil, // logger
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go poller.Run(ctx)

	// 等待初始轮询
	time.Sleep(80 * time.Millisecond)

	// 修改文件
	os.WriteFile(dbPath, []byte("updated"), 0644)

	// 等待检测到变化
	time.Sleep(150 * time.Millisecond)

	cancel()
	poller.Stop()

	if atomic.LoadInt32(&triggered) == 0 {
		t.Error("expected at least one trigger after file change")
	}
}

func TestSQLitePoller_StopTwice(t *testing.T) {
	poller := NewSQLitePoller("/tmp/test.db", "test", collector.CollectRequest{Incremental: true}, time.Second, func(string, collector.CollectRequest) {}, nil)

	// Stop 两次不应 panic
	poller.Stop()
	poller.Stop()
}

func TestGetSQLiteMtime_FileNotExist(t *testing.T) {
	// 不存在的文件应返回 0
	mtime := GetSQLiteMtime("/nonexistent/test.db")
	if mtime != 0 {
		t.Errorf("expected 0 for nonexistent file, got %d", mtime)
	}
}

func TestSQLitePoller_FileCreatedLater(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// 初始不创建文件

	var triggered int32
	poller := NewSQLitePoller(
		dbPath,
		"test_client",
		collector.CollectRequest{Incremental: true},
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			atomic.AddInt32(&triggered, 1)
		},
		nil, // logger
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go poller.Run(ctx)

	// 等待初始轮询（文件不存在，mtime=0）
	time.Sleep(80 * time.Millisecond)

	// 后续创建文件
	os.WriteFile(dbPath, []byte("created"), 0644)

	// 等待检测到变化
	time.Sleep(150 * time.Millisecond)

	cancel()
	poller.Stop()

	if atomic.LoadInt32(&triggered) == 0 {
		t.Error("expected trigger after file created")
	}
}

// TestSQLitePoller_PassesIncrementalRequest 行为：SQLite client poller 检测到 mtime 变化后，
// 应原样回调构造期传入的 request（Incremental=true, Source=""），不再将 mtime 转日期。
func TestSQLitePoller_PassesIncrementalRequest(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	os.WriteFile(dbPath, []byte("initial"), 0644)

	type capturedReq struct {
		client string
		req    collector.CollectRequest
	}
	requests := make(chan capturedReq, 4)
	poller := NewSQLitePoller(
		dbPath,
		"zcode",
		collector.CollectRequest{Incremental: true},
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			requests <- capturedReq{client: client, req: req}
		},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go poller.Run(ctx)
	time.Sleep(80 * time.Millisecond)

	os.WriteFile(dbPath, []byte("updated"), 0644)

	select {
	case got := <-requests:
		if got.client != "zcode" {
			t.Fatalf("client = %q, want zcode", got.client)
		}
		if !got.req.Incremental {
			t.Fatalf("Incremental = false, want true")
		}
		if got.req.Source != "" {
			t.Fatalf("Source = %q, want empty", got.req.Source)
		}
		if got.req.ChangedFile != "" {
			t.Fatalf("ChangedFile = %q, want empty", got.req.ChangedFile)
		}
		if len(got.req.Dates) != 0 {
			t.Fatalf("Dates = %v, want empty", got.req.Dates)
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("timeout waiting for poller callback")
	}

	cancel()
	poller.Stop()
}

// TestSQLitePoller_PassesRouterRequest 行为：CC Switch router poller 检测到 mtime 变化后，
// 应原样回调构造期传入的 request（Source=router, Incremental=true）。
func TestSQLitePoller_PassesRouterRequest(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cc-switch.db")
	os.WriteFile(dbPath, []byte("initial"), 0644)

	type capturedReq struct {
		client string
		req    collector.CollectRequest
	}
	requests := make(chan capturedReq, 4)
	poller := NewSQLitePoller(
		dbPath,
		"claude",
		collector.CollectRequest{Source: collector.CollectSourceRouter, Incremental: true},
		50*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			requests <- capturedReq{client: client, req: req}
		},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go poller.Run(ctx)
	time.Sleep(80 * time.Millisecond)

	os.WriteFile(dbPath, []byte("updated"), 0644)

	select {
	case got := <-requests:
		if got.client != "claude" {
			t.Fatalf("client = %q, want claude", got.client)
		}
		if got.req.Source != collector.CollectSourceRouter {
			t.Fatalf("Source = %q, want %q", got.req.Source, collector.CollectSourceRouter)
		}
		if !got.req.Incremental {
			t.Fatalf("Incremental = false, want true")
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("timeout waiting for router poller callback")
	}

	cancel()
	poller.Stop()
}
