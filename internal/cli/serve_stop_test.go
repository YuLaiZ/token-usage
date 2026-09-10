package cli

// serve_stop_test.go 驱动 serve stop 的全部分支，信号一律经注入 seam 记录
// 调用而不真杀进程：「服务下线」用关闭 httptest 假端点模拟（探活随之失败），
// 等待窗口经包级 var 缩短保持测试快速。unix 真实信号的编译与行为覆盖在
// serve_stop_unix_test.go。

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/serve"
)

// withServeStopSeams 替换信号 seam 并缩短等待窗口，测试结束恢复。
// 返回记录信号调用的指针。
func withServeStopSeams(t *testing.T, sig func(int) error, kill func(int) error) (sigs, kills *[]int) {
	t.Helper()
	origSig, origKill := serve.SignalProc, serve.KillProc
	sigCalls, killCalls := []int{}, []int{}
	serve.SignalProc = func(pid int) error {
		sigCalls = append(sigCalls, pid)
		return sig(pid)
	}
	serve.KillProc = func(pid int) error {
		killCalls = append(killCalls, pid)
		return kill(pid)
	}
	origTerm, origKillWait, origInterval, origProbe := serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout
	serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout =
		80*time.Millisecond, 80*time.Millisecond, 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		serve.SignalProc, serve.KillProc = origSig, origKill
		serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout =
			origTerm, origKillWait, origInterval, origProbe
	})
	return &sigCalls, &killCalls
}

// serveStopCmdFixture 构造注入临时 DataDir 的 serve stop 命令与输出 buffer。
func serveStopCmdFixture(t *testing.T, dataDir string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newServeStopCmd(func() (*config.Config, error) { return &config.Config{DataDir: dataDir}, nil })
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	return cmd, &buf
}

func TestServeStopCmd_NotRunningIdempotent(t *testing.T) {
	sigCalls, killCalls := withServeStopSeams(t, func(int) error { return nil }, func(int) error { return nil })
	cmd, buf := serveStopCmdFixture(t, t.TempDir())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("无状态文件应幂等成功,实际错误: %v", err)
	}
	for _, want := range []string{"serve is not running", "仪表板未在后台运行"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 0 || len(*killCalls) != 0 {
		t.Errorf("无状态文件不应发信号,signal=%v kill=%v", *sigCalls, *killCalls)
	}
}

func TestServeStopCmd_StaleBeforeSignal(t *testing.T) {
	// 预留后关闭一个端口:探活必然失败,构成「信号前即陈旧」场景。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	sigCalls, killCalls := withServeStopSeams(t, func(int) error { return nil }, func(int) error { return nil })
	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 999999, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("陈旧状态应清理成功,实际错误: %v", err)
	}
	for _, want := range []string{"stale state removed", "已清理陈旧状态"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 0 || len(*killCalls) != 0 {
		t.Errorf("陈旧状态不应发信号,signal=%v kill=%v", *sigCalls, *killCalls)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("陈旧状态文件应被删除,stat err = %v", err)
	}
}

func TestServeStopCmd_SignalThenProbeDown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := strings.TrimPrefix(ts.URL, "http://")

	// 优雅信号模拟:记录 PID 并关闭假端点,下一次探活即失败 → 判定已停。
	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { ts.Close(); return nil },
		func(int) error { t.Error("不应走到强杀兜底"); return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("正常停止应成功,实际错误: %v", err)
	}
	for _, want := range []string{"dashboard stopped", "仪表板已停止"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 4242 {
		t.Errorf("应恰向记录 PID 4242 发一次信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 0 {
		t.Errorf("探活下线后不应强杀,kill=%v", *killCalls)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("停止后状态文件应被删除,stat err = %v", err)
	}
}

func TestServeStopCmd_KillFallbackWhenStillAlive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	// 优雅信号后服务仍响应（不关端点），宽限窗口耗尽 → 强杀兜底关闭端点。
	// 强杀真实生效的正例：探活确认下线后才删状态文件并报告停止。
	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { return nil },
		func(int) error { ts.Close(); return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("强杀兜底路径应成功,实际错误: %v", err)
	}
	for _, want := range []string{"dashboard stopped", "仪表板已停止"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 4242 {
		t.Errorf("应先发一次优雅信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 1 || (*killCalls)[0] != 4242 {
		t.Errorf("宽限超时应恰强杀一次,实际 %v", *killCalls)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("停止后状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStopCmd_SignalErrorButServerGoneStops 覆盖「信号失败不短路」：探活
// 通过后、信号投递时进程已死（ESRCH），此时不应按陈旧清理谎报，而应照常进入
// 探活等待——探活确认下线即报告停止（探活为准）。
func TestServeStopCmd_SignalErrorButServerGoneStops(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := strings.TrimPrefix(ts.URL, "http://")

	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { ts.Close(); return errors.New("no such process") },
		func(int) error { t.Error("探活已下线不应强杀"); return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("信号失败但服务已死应正常停止,实际错误: %v", err)
	}
	for _, want := range []string{"dashboard stopped", "仪表板已停止"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 4242 {
		t.Errorf("应尝试一次信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 0 {
		t.Errorf("探活下线后不应强杀,kill=%v", *killCalls)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("停止后状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStopCmd_SignalAndKillErrorsStillResponding 覆盖最坏路径：优雅信号
// 与强杀兜底都返回错误、/api/meta 持续响应。此时绝不能删状态文件谎报停止：
// 应返回非零错误（含记录 URL、PID 与 kill 错误原文）并保留 serve.json。
func TestServeStopCmd_SignalAndKillErrorsStillResponding(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { return errors.New("no such process") },
		func(int) error { return errors.New("access denied") })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("信号与强杀都失败且服务仍响应时应报非零错误")
	}
	msg := execErr.Error()
	for _, want := range []string{
		"http://" + addr,
		"4242",
		"still responding",
		"仍在响应",
		"access denied",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
	if len(*sigCalls) != 1 || len(*killCalls) != 1 {
		t.Errorf("信号与强杀应各恰一次,sig=%v kill=%v", *sigCalls, *killCalls)
	}
	if out := buf.String(); strings.Contains(out, "dashboard stopped") || strings.Contains(out, "仪表板已停止") {
		t.Errorf("不得输出停止成功文案,实际: %q", out)
	}
	if _, err := os.Stat(serve.StatePath(dir)); err != nil {
		t.Errorf("服务仍响应时状态文件必须保留,stat err = %v", err)
	}
}

// TestServeStopCmd_KillIneffectiveStillResponding 模拟「杀不死」：信号成功、
// 强杀也成功返回，但服务仍在响应（如 pid 被无关进程复用且对方不受信号影响）。
// 停止成功的唯一判据是探活下线——此处必须报非零并保留状态文件。
func TestServeStopCmd_KillIneffectiveStillResponding(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { return nil },
		func(int) error { return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("强杀无效且服务仍响应时应报非零错误")
	}
	msg := execErr.Error()
	for _, want := range []string{
		"http://" + addr,
		"4242",
		"still responding",
		"仍在响应",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 4242 {
		t.Errorf("应先发一次优雅信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 1 || (*killCalls)[0] != 4242 {
		t.Errorf("宽限超时应恰强杀一次,实际 %v", *killCalls)
	}
	if out := buf.String(); strings.Contains(out, "dashboard stopped") || strings.Contains(out, "仪表板已停止") {
		t.Errorf("不得输出停止成功文案,实际: %q", out)
	}
	if _, err := os.Stat(serve.StatePath(dir)); err != nil {
		t.Errorf("服务仍响应时状态文件必须保留,stat err = %v", err)
	}
}

func TestServeStopCmd_CorruptStateRemoved(t *testing.T) {
	// 损坏的状态文件无法定位实例：删除残留后按未运行报告（exit 0），不发信号。
	sigCalls, killCalls := withServeStopSeams(t, func(int) error { return nil }, func(int) error { return nil })
	dir := t.TempDir()
	if err := os.WriteFile(serve.StatePath(dir), []byte("{not json"), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("损坏状态应清理成功,实际错误: %v", err)
	}
	for _, want := range []string{"corrupt state removed", "已清理损坏的状态文件"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if len(*sigCalls) != 0 || len(*killCalls) != 0 {
		t.Errorf("损坏状态不应发信号,signal=%v kill=%v", *sigCalls, *killCalls)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStopCmd_ReroutesToNewInstanceWhenStateReplaced 锁定 TOCTOU 重评估：
// 旧状态探活无响应（判定陈旧），条件删除时发现文件已被存活的新实例 B 接管——
// stop 绝不向旧 PID 发信号，而是重跑完整 stop 逻辑停掉探活响应的 B：信号发往
// B.PID、B 下线后条件删除其状态文件并报告停止。
func TestServeStopCmd_ReroutesToNewInstanceWhenStateReplaced(t *testing.T) {
	// 预留后关闭一个端口构成旧状态的陈旧地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	// 存活的假端点模拟新实例 B，信号送达 B 时关闭（下一次探活失败 → 判定已停）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { ts.Close(); return nil },
		func(int) error { t.Error("新实例探活确认下线后不应强杀"); return nil })

	dir := t.TempDir()
	stale := &serve.ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}
	if err := serve.WriteState(dir, stale); err != nil {
		t.Fatalf("写陈旧状态: %v", err)
	}

	// 条件删除 seam：第一次调用（陈旧判定段）模拟新实例已接管——磁盘状态被
	// 改写为 B 并返回 (false, B)；后续调用（B 确认下线后的 finalize）转发原始实现。
	orig := serve.RemoveStateIfSame
	calls := 0
	serve.RemoveStateIfSame = func(dataDir string, judged *serve.ServeState) (bool, *serve.ServeState, error) {
		calls++
		if calls == 1 {
			next := &serve.ServeState{PID: 5555, Addr: liveAddr, StartedAt: time.Now().Format(time.RFC3339)}
			if err := serve.WriteState(dataDir, next); err != nil {
				t.Fatalf("模拟新实例接管写入失败: %v", err)
			}
			return false, next, nil
		}
		return orig(dataDir, judged)
	}
	t.Cleanup(func() { serve.RemoveStateIfSame = orig })

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("重路由停止新实例应成功,实际错误: %v", err)
	}
	for _, want := range []string{"dashboard stopped", "仪表板已停止"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	// 信号必须只发给新实例 B（5555），旧 PID 999999 不得收到任何信号。
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 5555 {
		t.Errorf("应恰向新实例 PID 5555 发一次信号,旧 PID 不得收到信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 0 {
		t.Errorf("不应走到强杀兜底,kill=%v", *killCalls)
	}
	if calls < 2 {
		t.Errorf("陈旧判定段与 finalize 应各调用一次条件删除,实际 %d 次", calls)
	}
	// B 确认下线后其状态文件被条件删除。
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("新实例停止后状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStopCmd_FinalizeReroutesWhenStateReplaced 覆盖最终清理窗口的接管
// 竞态：旧实例确认下线后、条件删除前，新实例已写出自己的 serve.json——
// finalize 的条件删除正确保留新状态，且 stop 重路由停掉探活响应的新实例
// （旧 PID 零信号），最终报告停止。
func TestServeStopCmd_FinalizeReroutesWhenStateReplaced(t *testing.T) {
	tsA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tsA.Close()
	addrA := strings.TrimPrefix(tsA.URL, "http://")

	tsB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tsB.Close()
	addrB := strings.TrimPrefix(tsB.URL, "http://")

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addrA, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	// 优雅信号命中旧实例：关闭 A 并模拟等待窗口内的接管——新实例 B 写出
	// 自己的 serve.json。命中新实例：关闭 B 让重路由后的探活确认下线。
	var sigCalls []int
	origSig := serve.SignalProc
	serve.SignalProc = func(pid int) error {
		sigCalls = append(sigCalls, pid)
		switch pid {
		case 4242:
			tsA.Close()
			return serve.WriteState(dir, &serve.ServeState{PID: 5151, Addr: addrB, StartedAt: time.Now().Format(time.RFC3339)})
		case 5151:
			tsB.Close()
			return nil
		}
		return nil
	}
	origTerm, origKillWait, origInterval, origProbe := serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout
	serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout =
		80*time.Millisecond, 80*time.Millisecond, 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		serve.SignalProc = origSig
		serve.StopTermWait, serve.StopKillWait, serve.StopProbeInterval, serve.StopProbeTimeout =
			origTerm, origKillWait, origInterval, origProbe
	})
	origFinalize := serve.RemoveStateIfSame
	serve.RemoveStateIfSame = func(dataDir string, judged *serve.ServeState) (bool, *serve.ServeState, error) {
		// finalize 阶段（judged=旧实例 A）遇到文件已是 B：模拟接管，返回
		// (false, B) 走重路由；其余场景（judged=B）走真实条件删除。
		if judged.PID == 4242 {
			cur, err := serve.ReadState(dataDir)
			if err != nil {
				return false, nil, err
			}
			if cur != nil && cur.PID == 5151 {
				return false, cur, nil
			}
			return true, nil, nil
		}
		return origFinalize(dataDir, judged)
	}
	t.Cleanup(func() { serve.RemoveStateIfSame = origFinalize })

	cmd, buf := serveStopCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("接管重路由路径应成功,实际错误: %v", err)
	}
	if len(sigCalls) != 2 || sigCalls[0] != 4242 || sigCalls[1] != 5151 {
		t.Errorf("应先后向旧/新实例各发一次信号,实际 %v", sigCalls)
	}
	for _, want := range []string{
		"已被新实例接管", "has been replaced by a new one",
		"dashboard stopped", "仪表板已停止",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("最终状态文件应被删除: %v", err)
	}
}
