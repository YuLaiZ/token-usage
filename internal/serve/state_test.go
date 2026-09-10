package serve

// state_test.go 覆盖 serve.json 状态文件的磁盘合同：写读往返与 JSON
// 字段名、缺失降级为「无后台状态」而损坏/字段无效返回哨兵 ErrStateCorrupt、
// 原子替换覆盖旧值、/api/meta 探活 helper 的存活判定，以及状态迁移锁的
// 带界重试与条件删除原语 RemoveStateIfSame 的三态语义（一致才删、被改写
// 不删、缺失视为已删、损坏直删）。命令级的损坏清理承诺测试在 cli 包
// （serve stop/status/start 走真实命令链）。

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
)

func TestServeState_WriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got == nil {
		t.Fatal("ReadState 不应返回 nil")
	}
	if got.PID != want.PID || got.Addr != want.Addr || got.StartedAt != want.StartedAt {
		t.Errorf("往返不一致: got %+v, want %+v", got, want)
	}

	// 磁盘形态使用合同约定的 JSON 字段名。
	data, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	for _, want := range []string{
		`"pid":4242`,
		`"addr":"127.0.0.1:8619"`,
		`"started_at":"2026-09-08T10:30:00+08:00"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("serve.json 应含 %s,实际: %s", want, data)
		}
	}
}

func TestServeState_ReadMissingCorruptedOrInvalid(t *testing.T) {
	dir := t.TempDir()

	// 文件缺失：无后台状态（不报错）。
	st, err := ReadState(dir)
	if err != nil || st != nil {
		t.Errorf("缺失文件应 (nil, nil),实际 (%v, %v)", st, err)
	}

	// 存在但损坏/字段无效：无法定位实例的残留文件，返回哨兵 ErrStateCorrupt
	// 供 status/stop/start 删除后按未运行继续。
	cases := map[string]string{
		"JSON 损坏": "{not json",
		"pid 无效":  `{"pid":0,"addr":"127.0.0.1:8619","started_at":"2026-09-08T10:30:00+08:00"}`,
		"pid 为负":  `{"pid":-1,"addr":"127.0.0.1:8619","started_at":"2026-09-08T10:30:00+08:00"}`,
		"addr 为空": `{"pid":123,"addr":"","started_at":"2026-09-08T10:30:00+08:00"}`,
		"时间戳非法":   `{"pid":123,"addr":"127.0.0.1:8619","started_at":"yesterday"}`,
		"时间戳为空":   `{"pid":123,"addr":"127.0.0.1:8619","started_at":""}`,
	}
	for name, content := range cases {
		if err := os.WriteFile(StatePath(dir), []byte(content), 0644); err != nil {
			t.Fatalf("%s: 写入失败: %v", name, err)
		}
		st, err := ReadState(dir)
		if st != nil || !errors.Is(err, ErrStateCorrupt) {
			t.Errorf("%s 应返回 (nil, ErrStateCorrupt),实际 (%v, %v)", name, st, err)
		}
	}
}

func TestServeState_AtomicReplaceOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, &ServeState{PID: 1, Addr: "127.0.0.1:1111", StartedAt: "2026-09-10T10:00:00+08:00"}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 覆盖写：原子替换后读到的是新值而非残留旧值。
	if err := WriteState(dir, &ServeState{PID: 2, Addr: "127.0.0.1:2222", StartedAt: "2026-09-10T11:00:00+08:00"}); err != nil {
		t.Fatalf("覆盖写入: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got == nil || got.PID != 2 || got.Addr != "127.0.0.1:2222" {
		t.Errorf("覆盖写后应读到新值,实际 %+v", got)
	}
}

func TestMetaAlive_ProbeSemantics(t *testing.T) {
	// 正常 /api/meta（2xx）→ 存活。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/meta" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if !MetaAlive(ts.URL, 2*time.Second) {
		t.Error("2xx /api/meta 应判定存活")
	}

	// 404 → 不存活。
	if MetaAlive(ts.URL+"/nope", 2*time.Second) {
		t.Error("404 应判定不存活")
	}

	// 连接拒绝 → 不存活。
	if MetaAlive("http://127.0.0.1:1", 500*time.Millisecond) {
		t.Error("连接拒绝应判定不存活")
	}

	// 尾部斜杠容忍：base 以 / 结尾时不产生 //api/meta。
	if !MetaAlive(ts.URL+"/", 2*time.Second) {
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
	fl := flock.New(filepath.Join(dir, StateLockFile))
	locked, err := fl.TryLock()
	if err != nil || !locked {
		t.Fatalf("测试前置: 占用 serve-state 锁失败 (locked=%v, err=%v)", locked, err)
	}
	t.Cleanup(func() { _ = fl.Unlock() })
}

// stubRemoveStateIfSame 注入条件删除 seam 并在测试结束恢复，返回调用
// 计数指针。fn 收到每次调用的参数，可按需转发原始实现。
func stubRemoveStateIfSame(t *testing.T, fn func(dataDir string, judged *ServeState) (bool, *ServeState, error)) *int {
	t.Helper()
	orig := RemoveStateIfSame
	calls := 0
	RemoveStateIfSame = func(dataDir string, judged *ServeState) (bool, *ServeState, error) {
		calls++
		return fn(dataDir, judged)
	}
	t.Cleanup(func() { RemoveStateIfSame = orig })
	return &calls
}

// TestRemoveStateIfSame 直测条件删除原语的三态语义。前置条件合同是
// 调用方已持 serve-state 锁，各子用例先取锁再调用。
func TestRemoveStateIfSame(t *testing.T) {
	judged := &ServeState{PID: 4242, Addr: "127.0.0.1:8619", StartedAt: "2026-09-08T10:30:00+08:00"}

	t.Run("judged在位则删除", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := WriteState(dir, judged); err != nil {
			t.Fatalf("写入 judged 状态: %v", err)
		}
		removed, current, err := RemoveStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("RemoveStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("judged 在位应 (true, nil),实际 (%v, %v)", removed, current)
		}
		if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
			t.Errorf("一致状态应被删除,stat err = %v", err)
		}
	})

	t.Run("已被新实例改写则不删", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := WriteState(dir, judged); err != nil {
			t.Fatalf("写入 judged 状态: %v", err)
		}
		// 模拟新实例接管：磁盘被改写为另一份有效状态 B。
		next := &ServeState{PID: 5555, Addr: "127.0.0.1:9999", StartedAt: "2026-09-08T11:00:00+08:00"}
		if err := WriteState(dir, next); err != nil {
			t.Fatalf("改写为新实例状态: %v", err)
		}
		removed, current, err := RemoveStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("RemoveStateIfSame: %v", err)
		}
		if removed {
			t.Error("文件已被新实例改写时不得删除")
		}
		if current == nil || *current != *next {
			t.Errorf("应返回当前新状态 B,实际 %+v", current)
		}
		got, err := ReadState(dir)
		if err != nil || got == nil || *got != *next {
			t.Errorf("新实例状态 B 必须原样保留,实际 (%+v, %v)", got, err)
		}
	})

	t.Run("文件缺失视为已删", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		removed, current, err := RemoveStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("RemoveStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("文件缺失应 (true, nil),实际 (%v, %v)", removed, current)
		}
	})

	t.Run("损坏文件直接删除", func(t *testing.T) {
		dir := t.TempDir()
		holdServeStateLock(t, dir)
		if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0644); err != nil {
			t.Fatalf("写损坏状态文件: %v", err)
		}
		// 损坏文件没有新实例语义：无论 judged 为何都直接删除（与 corrupt
		// 清理契约一致）。
		removed, current, err := RemoveStateIfSame(dir, judged)
		if err != nil {
			t.Fatalf("RemoveStateIfSame: %v", err)
		}
		if !removed || current != nil {
			t.Errorf("损坏文件应 (true, nil),实际 (%v, %v)", removed, current)
		}
		if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
			t.Errorf("损坏文件应被删除,stat err = %v", err)
		}
	})
}

// TestServeStateLockBusy 直测状态迁移锁的忙语义：测试占住 serve-state.lock 后，
// AcquireStateLock 应在带界重试耗尽后返回双语 busy 错误。
func TestServeStateLockBusy(t *testing.T) {
	origRetries, origGap := stateLockRetries, stateLockRetryGap
	stateLockRetries, stateLockRetryGap = 2, time.Millisecond
	t.Cleanup(func() { stateLockRetries, stateLockRetryGap = origRetries, origGap })

	dir := t.TempDir()
	holdServeStateLock(t, dir)
	_, err := AcquireStateLock(dir)
	if err == nil {
		t.Fatal("状态迁移锁被占时应报错")
	}
	msg := err.Error()
	for _, want := range []string{"serve state file is busy; retry", "状态文件忙，请重试"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
}
