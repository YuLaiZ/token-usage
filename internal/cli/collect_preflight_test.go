package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// TestCollect_PreflightBlocksDBOpenOnDaemonConflict 守护进程运行时，collect / all /
// router / retry 四条路径的 DB opener 与 collector factory 调用次数均为 0。
// 通过覆盖 dbOpener / newDepsFactory 包级变量，断言它们在 daemon conflict 时从未被调用。
func TestCollect_PreflightBlocksDBOpenOnDaemonConflict(t *testing.T) {
	// 准备一个"守护进程正运行"的临时目录作为 config 的 data_dir。
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(cfgDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	// config.toml：claude enabled，log 段让 logger.Init 可用
	cfgContent := fmt.Sprintf(`data_dir = "%s"

[clients.claude]
enabled = true

[log]
level = "info"
dir = "%s/logs"
max_days = 7
`, dataDir, dataDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	// 在 data_dir 下抢锁（模拟守护进程运行）。
	// collectPreflight 检查的路径：cfg.DataDir + "/token-usage.lock"
	lockPath := filepath.Join(dataDir, "token-usage.lock")
	f, ok := daemon.AcquireLock(lockPath)
	if !ok {
		t.Fatal("无法抢占守护进程锁")
	}
	defer daemon.ReleaseLock(f)

	// 覆盖 DB opener / deps factory，用原子计数器记录调用次数。
	var dbOpenCalls int32
	var newDepsCalls int32
	resetInjections(t, &dbOpenCalls, &newDepsCalls)

	for _, sub := range []string{"collect", "all", "router", "retry"} {
		t.Run(sub, func(t *testing.T) {
			atomic.StoreInt32(&dbOpenCalls, 0)
			atomic.StoreInt32(&newDepsCalls, 0)
			root := NewRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			args := []string{"collect"}
			if sub != "collect" {
				args = append(args, sub)
			}
			// router 必须带 --client 才能走完整校验链路（但 preflight 在 client 校验之前，
			// 所以即使不带 client 也会先被 daemon 拦截）。
			if sub == "router" {
				args = append(args, "--client", "claude")
			}
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("%s 期望返回 error（daemon 冲突），实际 nil", sub)
			}
			if !strings.Contains(err.Error(), "守护进程正在运行") {
				t.Errorf("%s error 应含 '守护进程正在运行'，实际 %q", sub, err.Error())
			}
			if got := atomic.LoadInt32(&dbOpenCalls); got != 0 {
				t.Errorf("%s: DB opener 不应被调用，实际 %d 次", sub, got)
			}
			if got := atomic.LoadInt32(&newDepsCalls); got != 0 {
				t.Errorf("%s: collector factory 不应被调用，实际 %d 次", sub, got)
			}
		})
	}
}

func TestCollect_InvalidTargetDoesNotOpenDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".token-usage")
	dataDir := filepath.Join(cfgDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgContent := fmt.Sprintf(`data_dir = %q

[clients.claude]
enabled = true
router = "cc_switch"

[routers.cc_switch]
db_path = %q

[log]
level = "info"
dir = %q
max_days = 7
`, dataDir, filepath.Join(dataDir, "router.db"), filepath.Join(dataDir, "logs"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var dbOpenCalls int32
	var newDepsCalls int32
	resetInjections(t, &dbOpenCalls, &newDepsCalls)

	cases := [][]string{
		{"collect", "--client", "ghost"},
		{"collect", "all", "--client", "ghost"},
		{"collect", "router"},
		{"collect", "retry", "--client", "ghost"},
	}
	for _, args := range cases {
		atomic.StoreInt32(&dbOpenCalls, 0)
		atomic.StoreInt32(&newDepsCalls, 0)
		root := NewRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("%v 应因目标参数无效而报错", args)
		}
		if got := atomic.LoadInt32(&dbOpenCalls); got != 0 {
			t.Errorf("%v: 目标参数错误必须先于 DB 打开，实际 %d 次", args, got)
		}
		if got := atomic.LoadInt32(&newDepsCalls); got != 0 {
			t.Errorf("%v: 目标参数错误不得装配 collector，实际 %d 次", args, got)
		}
	}
}

// resetInjections 覆盖包级 dbOpener / newDepsFactory 为计数桩，
// 测试结束后由 t.Cleanup 恢复（恢复到标准 db.Open / engine.NewDeps）。
func resetInjections(t *testing.T, dbOpenCalls, newDepsCalls *int32) {
	t.Helper()
	origDBOpener := dbOpener
	origNewDeps := newDepsFactory
	dbOpener = func(path string) (*db.DB, error) {
		atomic.AddInt32(dbOpenCalls, 1)
		return origDBOpener(path)
	}
	newDepsFactory = func(cfg *config.Config) *engine.Deps {
		atomic.AddInt32(newDepsCalls, 1)
		return origNewDeps(cfg)
	}
	t.Cleanup(func() {
		dbOpener = origDBOpener
		newDepsFactory = origNewDeps
	})
}
