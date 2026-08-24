//go:build !windows

package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTimer 受控定时器：记录调度、手动触发回调，使日界测试可确定执行。
type fakeTimer struct {
	stopped bool
	fire    func()
}

func (t *fakeTimer) Stop() bool {
	t.stopped = true
	return true
}

// newMirrorTestWriter 构造注入受控 clock 与 afterFunc 的 rotatingWriter。
func newMirrorTestWriter(t *testing.T, dir string) (*rotatingWriter, *[]*fakeTimer, *time.Time) {
	t.Helper()
	cur := time.Date(2026, 8, 24, 23, 59, 30, 0, time.Local)
	var timers []*fakeTimer
	w := newRotatingWriter(dir, func() time.Time { return cur })
	w.afterFunc = func(d time.Duration, f func()) afterFuncTimer {
		ft := &fakeTimer{fire: f}
		timers = append(timers, ft)
		return ft
	}
	return w, &timers, &cur
}

func readLogFile(t *testing.T, dir, date string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "token-usage-"+date+".log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(b)
}

// 接管后 stderr 输出并入当日日志文件；restore 完全撤销（fd 恢复 + 停 timer + 幂等），
// 恢复后 stderr 输出与 timer 触发均不再触碰日志文件。
func TestMirrorStd_TakeOverRestoreAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, timers, _ := newMirrorTestWriter(t, dir)

	restore := w.enableMirrorStd()
	fmt.Fprint(os.Stderr, "marker-during-mirror")

	if got := readLogFile(t, dir, "2026-08-24"); !strings.Contains(got, "marker-during-mirror") {
		t.Errorf("接管期间 stderr 输出应落入当日日志文件，实际内容: %q", got)
	}
	if len(*timers) != 1 {
		t.Fatalf("接管后应注册 1 个日界 timer，实际 %d", len(*timers))
	}

	restore()
	restore() // 幂等：重复调用无副作用。
	if !(*timers)[0].stopped {
		t.Error("restore 后日界 timer 应被停止")
	}

	fmt.Fprint(os.Stderr, "marker-after-restore")
	if got := readLogFile(t, dir, "2026-08-24"); strings.Contains(got, "marker-after-restore") {
		t.Error("restore 后 stderr 输出不应再进入日志文件")
	}

	// restore 后触发旧 timer 回调：mirrorStd 已禁用，不应重新注册或产生接管副作用。
	(*timers)[0].fire()
	if n := len(*timers); n != 1 {
		t.Errorf("restore 后 timer 回调不应重新注册日界 timer，实际 %d 个", n)
	}

	// restore 后继续写结构化日志正常（内容落文件，且不重做接管）。
	if _, err := w.Write([]byte("post-restore-entry\n")); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	if got := readLogFile(t, dir, "2026-08-24"); !strings.Contains(got, "post-restore-entry") {
		t.Errorf("restore 后结构化日志写入应正常，实际内容: %q", got)
	}
}

// 日界 timer 连续重排：第一个午夜触发后切换到新当日文件并接管到新文件、
// 同时注册第二个午夜；第二个午夜再次触发后注册第三个（长期运行每午夜必达）。
func TestMirrorStd_DayBoundaryReschedulesContinuously(t *testing.T) {
	dir := t.TempDir()
	w, timers, cur := newMirrorTestWriter(t, dir)

	restore := w.enableMirrorStd()
	defer restore()

	// 第一个午夜：23:59:30 → 次日 00:00:01。
	*cur = cur.Add(31 * time.Second)
	(*timers)[0].fire()

	fmt.Fprint(os.Stderr, "marker-day2")
	if got := readLogFile(t, dir, "2026-08-25"); !strings.Contains(got, "marker-day2") {
		t.Errorf("跨午夜后 stderr 输出应落入新当日文件，实际内容: %q", got)
	}
	if old := readLogFile(t, dir, "2026-08-24"); strings.Contains(old, "marker-day2") {
		t.Error("跨午夜后输出不应再写入前一日文件")
	}
	if len(*timers) < 2 {
		t.Fatalf("第一个日界触发后应注册第二个日界 timer，实际 %d 个", len(*timers))
	}

	// 第二个午夜。
	*cur = cur.Add(24 * time.Hour)
	(*timers)[1].fire()
	fmt.Fprint(os.Stderr, "marker-day3")
	if got := readLogFile(t, dir, "2026-08-26"); !strings.Contains(got, "marker-day3") {
		t.Errorf("第二个日界触发后输出应落入 08-26 文件，实际: %q", got)
	}
	if len(*timers) < 3 {
		t.Fatalf("第二个日界触发后应注册第三个日界 timer，实际 %d 个", len(*timers))
	}
}

// 经 globalWriter 的公开入口 MirrorStdOutput：Init 后接管生效、Close 不因
// mirror 状态误报（timer 停止路径）。
func TestMirrorStdOutput_PublicEntryWithGlobalWriter(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init("debug", dir, 0); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	restore := MirrorStdOutput()
	fmt.Fprint(os.Stderr, "marker-global-entry")
	if got := readLogFile(t, dir, time.Now().Format("2006-01-02")); !strings.Contains(got, "marker-global-entry") {
		t.Errorf("MirrorStdOutput 后 stderr 应落入当日日志文件，实际: %q", got)
	}
	restore()
}
