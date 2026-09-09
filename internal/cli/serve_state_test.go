package cli

// serve_state_test.go 覆盖 serve.json 状态文件的磁盘合同：写读往返与 JSON
// 字段名、缺失降级为「无后台状态」而损坏/字段无效返回哨兵 errServeStateCorrupt、
// 原子替换覆盖旧值、/api/meta 探活 helper 的存活判定，以及 status/start 对
// 损坏状态文件的统一清理承诺（stop 侧见 serve_stop_test.go）。
// 另覆盖状态迁移锁（带界重试与 busy 错误）与条件删除原语
// removeServeStateIfSame 的三态语义（一致才删、被改写不删、缺失视为已删、
// 损坏直删）。

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/YuLaiZ/token-usage/internal/config"
)

func TestServeState_WriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}
	if err := writeServeState(dir, want); err != nil {
		t.Fatalf("writeServeState: %v", err)
	}
	got, err := readServeState(dir)
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got == nil {
		t.Fatal("readServeState 不应返回 nil")
	}
	if got.PID != want.PID || got.Addr != want.Addr || got.StartedAt != want.StartedAt {
		t.Errorf("往返不一致: got %+v, want %+v", got, want)
	}

	// 磁盘形态使用合同约定的 JSON 字段名。
	data, err := os.ReadFile(serveStatePath(dir))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	for _, want := range []string{
		`"pid":4242`,
		`"addr":"127.0.0.1:8619"`,
		`"started_at":"2026-09-08T10:30:00+08:00"`,
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("serve.json 应含 %s,实际: %s", want, data)
		}
	}
}

func TestServeState_ReadMissingCorruptedOrInvalid(t *testing.T) {
	dir := t.TempDir()

	// 文件缺失：无后台状态（不报错）。
	st, err := readServeState(dir)
	if err != nil || st != nil {
		t.Errorf("缺失文件应 (nil, nil),实际 (%v, %v)", st, err)
	}

	// 存在但损坏/字段无效：无法定位实例的残留文件，返回哨兵 errServeStateCorrupt
	// 供 status/stop/start 删除后按未运行继续。
	cases := map[string]string{
		"JSON 损坏": "{not json",
		"pid 无效":  `{"pid":0,"addr":"127.0.0.1:8619","started_at":"2026-09-08T10:30:00+08:00"}`,
		"pid 为负":  `{"pid":-1,"addr":"127.0.0.1:8619","started_at":"2026-09-08T10:30:00+08:00"}`,
		"addr 为空": `{"pid":123,"addr":"","started_at":"2026-09-08T10:30:00+08:00"}`,
	}
	for name, content := range cases {
		if err := os.WriteFile(serveStatePath(dir), []byte(content), 0644); err != nil {
			t.Fatalf("%s: 写入失败: %v", name, err)
		}
		st, err := readServeState(dir)
		if st != nil || !errors.Is(err, errServeStateCorrupt) {
			t.Errorf("%s 应返回 (nil, errServeStateCorrupt),实际 (%v, %v)", name, st, err)
		}
	}
}

// TestServeStatusCmd_CorruptStateRemoved 锁定 status 对损坏状态文件的清理承诺：
// 删除残留文件、双语报告未运行、退出码 0。
func TestServeStatusCmd_CorruptStateRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serveStatePath(dir), []byte("{not json"), 0644); err != nil {
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
	if _, err := os.Stat(serveStatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStartCmd_CorruptStateRemovedBeforeSpawn 锁定 start 对损坏状态文件
// 的处置：与陈旧同路——spawn 前删除残留，随后照常启动。
func TestServeStartCmd_CorruptStateRemovedBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serveStatePath(dir), []byte(`{"pid":0,"addr":"","started_at":"bogus"}`), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	stubServeSpawnAndWait(t, func(cfg *config.Config, addr, logPath string) (int, string, error) {
		// spawn 前损坏状态必须已被清理,否则轮询会把残留当就绪信号。
		if st, err := readServeState(cfg.DataDir); err != nil || st != nil {
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

func TestServeState_AtomicReplaceOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	if err := writeServeState(dir, &ServeState{PID: 1, Addr: "127.0.0.1:1111", StartedAt: "s1"}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 覆盖写：原子替换后读到的是新值而非残留旧值。
	if err := writeServeState(dir, &ServeState{PID: 2, Addr: "127.0.0.1:2222", StartedAt: "s2"}); err != nil {
		t.Fatalf("覆盖写入: %v", err)
	}
	got, err := readServeState(dir)
	if err != nil {
		t.Fatalf("readServeState: %v", err)
	}
	if got == nil || got.PID != 2 || got.Addr != "127.0.0.1:2222" {
		t.Errorf("覆盖写后应读到新值,实际 %+v", got)
	}
}

func TestServeMetaAlive_ProbeSemantics(t *testing.T) {
	// 正常 /api/meta（2xx）→ 存活。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/meta" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if !serveMetaAlive(ts.URL, 2*time.Second) {
		t.Error("2xx /api/meta 应判定存活")
	}

	// 404 → 不存活。
	if serveMetaAlive(ts.URL+"/nope", 2*time.Second) {
		t.Error("404 应判定不存活")
	}

	// 连接拒绝 → 不存活。
	if serveMetaAlive("http://127.0.0.1:1", 500*time.Millisecond) {
		t.Error("连接拒绝应判定不存活")
	}

	// 尾部斜杠容忍：base 以 / 结尾时不产生 //api/meta。
	if !serveMetaAlive(ts.URL+"/", 2*time.Second) {
		t.Error("base 带尾斜杠仍应探活成功")
	}
	if !strings.HasPrefix(ts.URL, "http://") {
		t.Fatalf("httptest URL 前提变化: %s", ts.URL)
	}
}

// holdServeStateLock 测试占住 dir 的 serve-state 状态迁移锁，测试结束自动释放，
// 用于驱动「锁忙」路径。
func holdServeStateLock(t *testing.T, dir string) {
	t.Helper()
	fl := flock.New(filepath.Join(dir, serveStateLockFile))
	locked, err := fl.TryLock()
	if err != nil || !locked {
		t.Fatalf("测试前置: 占用 serve-state 锁失败 (locked=%v, err=%v)", locked, err)
	}
	t.Cleanup(func() { _ = fl.Unlock() })
}

// stubRemoveServeStateIfSame 注入条件删除 seam 并在测试结束恢复，返回调用
// 计数指针。fn 收到每次调用的参数，可按需转发原始实现。
func stubRemoveServeStateIfSame(t *testing.T, fn func(dataDir string, judged *ServeState) (bool, *ServeState, error)) *int {
	t.Helper()
	orig := removeServeStateIfSame
	calls := 0
	removeServeStateIfSame = func(dataDir string, judged *ServeState) (bool, *ServeState, error) {
		calls++
		return fn(dataDir, judged)
	}
	t.Cleanup(func() { removeServeStateIfSame = orig })
	return &calls
}

// TestRemoveServeStateIfSame 直测条件删除原语的三态语义。前置条件合同是
// 调用方已持 serve-state 锁，各子用例先取锁再调用。
func TestRemoveServeStateIfSame(t *testing.T) {
	judged := &ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}

	t.Run("judged在位则删除", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := writeServeState(dir, judged); err != nil {
			t.Fatalf("写入 judged 状态: %v", err)
		}
		removed, current, err := removeServeStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("removeServeStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("judged 在位应 (true, nil),实际 (%v, %v)", removed, current)
		}
		if _, err := os.Stat(serveStatePath(dir)); !os.IsNotExist(err) {
			t.Errorf("一致状态应被删除,stat err = %v", err)
		}
	})

	t.Run("已被新实例改写则不删", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := writeServeState(dir, judged); err != nil {
			t.Fatalf("写入 judged 状态: %v", err)
		}
		// 模拟新实例接管：磁盘被改写为另一份有效状态 B。
		next := &ServeState{PID: 5555, Addr: "127.0.0.1:9999", StartedAt: "2026-09-08T11:00:00+08:00"}
		if err := writeServeState(dir, next); err != nil {
			t.Fatalf("改写为新实例状态: %v", err)
		}
		removed, current, err := removeServeStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("removeServeStateIfSame: %v", err)
		}
		if removed {
			t.Error("文件已被新实例改写时不得删除")
		}
		if current == nil || *current != *next {
			t.Errorf("应返回当前新状态 B,实际 %+v", current)
		}
		got, err := readServeState(dir)
		if err != nil || got == nil || *got != *next {
			t.Errorf("新实例状态 B 必须原样保留,实际 (%+v, %v)", got, err)
		}
	})

	t.Run("文件缺失视为已删", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		removed, current, err := removeServeStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("removeServeStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("文件缺失应 (true, nil),实际 (%v, %v)", removed, current)
		}
	})

	t.Run("损坏文件直接删除", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := os.WriteFile(serveStatePath(dir), []byte("{not json"), 0644); err != nil {
			t.Fatalf("写损坏状态文件: %v", err)
		}
		// 损坏文件没有新实例语义：无论 judged 为何都直接删除（与 corrupt
		// 清理契约一致）。
		removed, current, err := removeServeStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("removeServeStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("损坏文件应 (true, nil),实际 (%v, %v)", removed, current)
		}
		if _, err := os.Stat(serveStatePath(dir)); !os.IsNotExist(err) {
			t.Errorf("损坏文件应被删除,stat err = %v", err)
		}
	})
}

// TestServeStateLockBusy 锁定状态迁移锁的忙语义：测试占住 serve-state.lock 后，
// status/stop/start 都应在带界重试耗尽后返回双语 busy 错误，且不触碰状态文件。
// 重试参数经包级 seam 缩短保持用例快速确定。
func TestServeStateLockBusy(t *testing.T) {
	origRetries, origGap := serveStateLockRetries, serveStateLockRetryGap
	serveStateLockRetries, serveStateLockRetryGap = 2, time.Millisecond
	t.Cleanup(func() { serveStateLockRetries, serveStateLockRetryGap = origRetries, origGap })

	dir := t.TempDir()
	holdServeStateLock(t, dir)

	// 种子一份有效状态文件，断言整轮命令不触碰它。
	seed := &ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}
	if err := writeServeState(dir, seed); err != nil {
		t.Fatalf("写种子状态: %v", err)
	}
	seeded, err := os.ReadFile(serveStatePath(dir))
	if err != nil {
		t.Fatalf("读种子状态: %v", err)
	}

	stubServeSpawnAndWait(t, func(*config.Config, string, string) (int, string, error) {
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
		for _, want := range []string{"serve state file is busy; retry", "状态文件忙，请重试"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: 错误信息应含 %q,实际: %q", tc.name, want, msg)
			}
		}
		got, err := os.ReadFile(serveStatePath(dir))
		if err != nil || string(got) != string(seeded) {
			t.Errorf("%s: 锁忙时状态文件不得被触碰,实际 (%q, %v)", tc.name, got, err)
		}
	}
}
