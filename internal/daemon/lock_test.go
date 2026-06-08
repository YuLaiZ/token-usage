// internal/daemon/lock_test.go
package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireLock_Success(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.lock")

	f, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("expected to acquire lock")
	}
	defer ReleaseLock(f)

	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Error("lock file should exist")
	}
}

func TestAcquireLock_AlreadyLocked(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.lock")

	f1, ok1 := AcquireLock(lockPath)
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	defer ReleaseLock(f1)

	_, ok2 := AcquireLock(lockPath)
	if ok2 {
		t.Error("second acquire should fail when lock is held")
	}
}

func TestReleaseLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.lock")

	f, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("expected to acquire lock")
	}

	ReleaseLock(f)

	f2, ok2 := AcquireLock(lockPath)
	if !ok2 {
		t.Error("should be able to acquire lock after release")
	}
	if f2 != nil {
		ReleaseLock(f2)
	}
}

func TestIsDaemonRunning(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "daemon.lock")

	if IsDaemonRunning(lockPath) {
		t.Error("daemon should not be running initially")
	}

	f, ok := AcquireLock(lockPath)
	if !ok {
		t.Fatal("failed to acquire lock")
	}

	if !IsDaemonRunning(lockPath) {
		t.Error("daemon should be running after acquiring lock")
	}

	ReleaseLock(f)

	if IsDaemonRunning(lockPath) {
		t.Error("daemon should not be running after releasing lock")
	}
}

func TestWritePID(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	if err := WritePID(pidPath); err != nil {
		t.Fatalf("WritePID failed: %v", err)
	}

	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		t.Error("PID file should exist")
	}

	// WritePID 现经 runmeta.WritePIDFile 写新格式 "<pid> <instanceID>"（instanceID 空）。
	// 用 ReadDaemonPID 校验而非裸 string 比较，兼容格式演进。
	pid, err := ReadDaemonPID(pidPath)
	if err != nil {
		t.Fatalf("read PID file failed: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("expected PID %s, got %d", strconv.Itoa(os.Getpid()), pid)
	}
}
