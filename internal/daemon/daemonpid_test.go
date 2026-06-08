// internal/daemon/daemonpid_test.go
package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDaemonPID_Valid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("12345"), 0644)
	pid, err := ReadDaemonPID(p)
	if err != nil || pid != 12345 {
		t.Fatalf("got pid=%d err=%v, want 12345 nil", pid, err)
	}
}

func TestReadDaemonPID_WhitespaceTrimmed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("  12345\n"), 0644)
	pid, err := ReadDaemonPID(p)
	if err != nil || pid != 12345 {
		t.Fatalf("应 TrimSpace,got pid=%d err=%v", pid, err)
	}
}

func TestReadDaemonPID_Missing(t *testing.T) {
	if _, err := ReadDaemonPID(filepath.Join(t.TempDir(), "no.pid")); err == nil {
		t.Fatal("pid 缺失应报错")
	}
}

func TestReadDaemonPID_Invalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("not-a-number"), 0644)
	if _, err := ReadDaemonPID(p); err == nil {
		t.Fatal("无效 pid 应报错")
	}
}
