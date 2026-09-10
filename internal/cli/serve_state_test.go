package cli

// serve_state_test.go 覆盖 serve 家族命令对损坏状态文件与状态迁移锁忙的统一
// 承诺（走真实命令链；状态文件磁盘合同与条件删除原语的纯测试在 internal/serve
// 包 state_test.go）：status/start 对损坏状态文件的删除后放行，锁忙时三类命令
// 的带界重试耗尽报错且不触碰状态文件。本文件同时保留 serve 家族测试共用的
// 状态锁与条件删除 seam 注入 helper。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/serve"
)

// TestServeStatusCmd_CorruptStateRemoved 锁定 status 对损坏状态文件的清理承诺：
// 删除残留文件、双语报告未运行、退出码 0。
func TestServeStatusCmd_CorruptStateRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serve.StatePath(dir), []byte("{not json"), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("损坏状态应由 status 清理并返回 0,实际错误: %v", err)
	}
	for _, want := range []string{"corrupt state removed", "已清理损坏的状态文件"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, out.String())
		}
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStartCmd_CorruptStateRemovedBeforeSpawn 锁定 start 对损坏状态文件
// 的处置：与陈旧同路——spawn 前删除残留，随后照常启动。
func TestServeStartCmd_CorruptStateRemovedBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serve.StatePath(dir), []byte(`{"pid":0,"addr":"","started_at":"bogus"}`), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	stubServeSpawnAndWait(t, func(dataDir, binPath, addr, logPath string) (int, string, error) {
		// spawn 前损坏状态必须已被清理,否则轮询会把残留当就绪信号。
		if st, err := serve.ReadState(dataDir); err != nil || st != nil {
			t.Errorf("spawn 时损坏状态应已被删除,实际 (%v, %v)", st, err)
		}
		return 4242, liveAddr, nil
	})

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"start"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("损坏状态清理后 start 应成功,实际错误: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "dashboard started in background") {
		t.Errorf("损坏清理后应走正常启动分支,实际: %q", got)
	}
}

// TestServeStateLockBusy 锁定状态迁移锁的忙语义：测试占住 serve-state.lock 后，
// status/stop/start 都应在带界重试耗尽后返回双语 busy 错误，且不触碰状态文件。
// （锁重试参数的缩短 seam 属 internal/serve 包内私有，命令级用例直接以生产
// 参数运行——5×20ms 的重试窗口对测试耗时无感。）
func TestServeStateLockBusy(t *testing.T) {
	dir := t.TempDir()
	holdServeStateLock(t, dir)

	// 种子一份有效状态文件，断言整轮命令不触碰它。
	seed := &serve.ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}
	if err := serve.WriteState(dir, seed); err != nil {
		t.Fatalf("写种子状态: %v", err)
	}
	seeded, err := os.ReadFile(serve.StatePath(dir))
	if err != nil {
		t.Fatalf("读种子状态: %v", err)
	}

	stubServeSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("状态锁忙时 start 不应走到 spawn")
		return 0, "", nil
	})

	cases := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"stop", []string{"stop"}},
		{"start", []string{"start", "--addr", "127.0.0.1:8619"}},
	}
	for _, tc := range cases {
		cmd, _, _ := serveFamilyFixture(t, dir)
		cmd.SetArgs(tc.args)
		execErr := cmd.Execute()
		if execErr == nil {
			t.Fatalf("%s: 状态迁移锁被占时应报错", tc.name)
		}
		msg := execErr.Error()
		if !errors.Is(execErr, serve.ErrStateBusy) && !strings.Contains(msg, "状态文件忙") {
			t.Errorf("%s: 错误信息应含 busy 语义,实际: %q", tc.name, msg)
		}
		got, err := os.ReadFile(serve.StatePath(dir))
		if err != nil || string(got) != string(seeded) {
			t.Errorf("%s: 锁忙时状态文件不得被触碰,实际 (%q, %v)", tc.name, got, err)
		}
	}
}

// holdServeStateLock 测试占住 dir 的 serve-state 状态迁移锁，测试结束自动释放，
// 用于驱动「锁忙」路径。
func holdServeStateLock(t *testing.T, dir string) {
	t.Helper()
	fl := flock.New(filepath.Join(dir, serve.StateLockFile))
	locked, err := fl.TryLock()
	if err != nil || !locked {
		t.Fatalf("测试前置: 占用 serve-state 锁失败 (locked=%v, err=%v)", locked, err)
	}
	t.Cleanup(func() { _ = fl.Unlock() })
}

// stubRemoveServeStateIfSame 注入条件删除 seam（internal/serve 包的
// RemoveStateIfSame var）并在测试结束恢复，返回调用计数指针。fn 收到每次
// 调用的参数，可按需转发原始实现。
func stubRemoveServeStateIfSame(t *testing.T, fn func(dataDir string, judged *serve.ServeState) (bool, *serve.ServeState, error)) *int {
	t.Helper()
	orig := serve.RemoveStateIfSame
	calls := 0
	serve.RemoveStateIfSame = func(dataDir string, judged *serve.ServeState) (bool, *serve.ServeState, error) {
		calls++
		return fn(dataDir, judged)
	}
	t.Cleanup(func() { serve.RemoveStateIfSame = orig })
	return &calls
}

// 时间导入防御位：TestServeStateLockBusy 的陈旧种子使用 RFC3339 字面量，
// time 包为后续用例保留。
var _ = time.Now
