package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// 只读三态探测：锁文件缺失不创建；探测不改写既有锁文件内容；
// 持锁时返回运行中。
func TestProbeDaemonLockReadOnly(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "token-usage.lock")

	if got := ProbeDaemonLock(lockPath); got != ProbeStopped {
		t.Fatalf("缺失锁文件 = %v, want ProbeStopped", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("探测不得创建锁文件: %v", err)
	}

	fl, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	defer ReleaseLock(fl)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProbeDaemonLock(lockPath); got != ProbeRunning {
		t.Fatalf("持锁时 = %v, want ProbeRunning", got)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("探测不得修改锁文件内容: %q -> %q", before, after)
	}
}

// 并发只读探测互不排斥：无 daemon 持锁时并发探测不得把彼此的共享锁误判为
// "运行中"（排他锁探测在此场景会大量误报）。
func TestProbeDaemonLock_ConcurrentReadersNeverMisreport(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "token-usage.lock")
	// 预建锁文件（无持有者）。
	fl, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	ReleaseLock(fl)

	const workers = 8
	const rounds = 200
	var misreported int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if ProbeDaemonLock(lockPath) != ProbeStopped {
					atomic.AddInt64(&misreported, 1)
				}
			}
		}()
	}
	wg.Wait()
	if misreported != 0 {
		t.Fatalf("并发只读探测误报运行中 %d 次（共享锁应互不排斥）", misreported)
	}
}

// 只读状态探测（共享锁）不得干扰控制路径判活：无 daemon 持锁时并发混合调用
// ProbeDaemonLock 与 IsDaemonRunning，IsDaemonRunning 必须恒为 false（排他
// 探测会被读者的共享锁误判成"运行中"，进而让 daemon start 误报已在运行）。
func TestIsDaemonRunning_NotDisturbedByReaders(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "token-usage.lock")
	fl, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	ReleaseLock(fl)

	var misreported int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				ProbeDaemonLock(lockPath)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if IsDaemonRunning(lockPath) {
					atomic.AddInt64(&misreported, 1)
				}
			}
		}()
	}
	wg.Wait()
	if misreported != 0 {
		t.Fatalf("并发状态探测把读者误判为守护进程 %d 次", misreported)
	}
}

// 短命的共享读锁（只读状态探测）不得让启动拿锁失败：AcquireLock 在读锁存在
// 期间重试，读锁释放后成功；真正的排他持有者会让重试在窗口耗尽后返回 false。
func TestAcquireLockToleratesReadProbes(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "token-usage.lock")
	reader := flock.New(lockPath)
	if locked, err := reader.TryRLock(); err != nil || !locked {
		t.Fatalf("获取读锁失败: %v", err)
	}

	type result struct {
		fl *flock.Flock
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		fl, ok := AcquireLock(lockPath)
		done <- result{fl: fl, ok: ok}
	}()
	time.Sleep(100 * time.Millisecond) // 让 AcquireLock 进入重试
	if err := reader.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if !res.ok {
			t.Fatal("读探测释放后 AcquireLock 应成功")
		}
		ReleaseLock(res.fl)
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireLock 未在读锁释放后完成")
	}

	// 排他持有者：第二次获取在重试窗口后返回 false。
	holder, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("首个 AcquireLock 应成功")
	}
	defer ReleaseLock(holder)
	start := time.Now()
	if _, ok := AcquireLock(lockPath); ok {
		t.Fatal("排他持有时 AcquireLock 必须失败")
	}
	if elapsed := time.Since(start); elapsed < acquireLockRetryWindow/2 {
		t.Fatalf("排他冲突应经过重试窗口再失败, elapsed=%v", elapsed)
	}
}
