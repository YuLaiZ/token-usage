package cli

// serve_restart_test.go 驱动 `serve restart` 的编排组合：stop 段经信号 seam
// 与 httptest 假端点驱动（与 serve_stop_test 同法），start 段经
// serveSpawnAndWait 假实现驱动（与 serve_start_test 同法）。核心断言：
// 未运行（含陈旧/损坏清理后）报错指引 serve start 且绝不 spawn；运行中先停
// （信号只发旧 PID）后起（新 PID/地址）；stop 失败时中止重启且绝不 spawn。

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/serve"
)

// TestServeRestartCmd_NotRunningErrors 覆盖未运行分支：stop 段幂等成功
// （输出未运行说明），restart 以非零错误退出并指引 serve start，绝不 spawn。
func TestServeRestartCmd_NotRunningErrors(t *testing.T) {
	// 未运行：stop 段不应发出任何信号。
	_, _ = withServeStopSeams(t,
		func(int) error { t.Error("未运行时 stop 段不应发信号"); return nil },
		func(int) error { t.Error("未运行时不应强杀"); return nil })

	stubServeSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("未运行时 restart 不得 spawn")
		return 0, "", nil
	})

	cmd, out, _ := serveFamilyFixture(t, t.TempDir())
	cmd.SetArgs([]string{"restart"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("未运行时 restart 应报非零错误")
	}
	if !errors.Is(execErr, errServeRestartNotRunning) {
		t.Errorf("错误应为 errServeRestartNotRunning,实际: %v", execErr)
	}
	for _, want := range []string{
		"serve is not running, run token-usage serve start",
		"仪表板未在后台运行，请使用 token-usage serve start",
	} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, execErr.Error())
		}
	}
	for _, want := range []string{"serve is not running", "仪表板未在后台运行"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout 过程行应含 %q,实际: %q", want, out.String())
		}
	}
}

// TestServeRestartCmd_StaleStateCleanedThenErrors 锁定陈旧形态=未运行：
// 合法 JSON 状态指向无响应地址（信号前即陈旧）被 stop 段清理后，restart
// 报错指引 serve start，绝不 spawn（造法与 serve_stop_test 的陈旧用例同款）。
func TestServeRestartCmd_StaleStateCleanedThenErrors(t *testing.T) {
	// 预留后关闭一个端口:探活必然失败,构成「信号前即陈旧」场景。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	sigCalls, killCalls := withServeStopSeams(t, func(int) error { return nil }, func(int) error { return nil })

	stubServeSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("陈旧清理后 restart 不得 spawn")
		return 0, "", nil
	})

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 999999, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"restart"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("陈旧清理后 restart 应报非零错误")
	}
	if !errors.Is(execErr, errServeRestartNotRunning) {
		t.Errorf("错误应为 errServeRestartNotRunning,实际: %v", execErr)
	}
	for _, want := range []string{"stale state removed", "已清理陈旧状态"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout 清理说明应含 %q,实际: %q", want, out.String())
		}
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("陈旧状态文件应被删除,stat err = %v", err)
	}
	if len(*sigCalls) != 0 || len(*killCalls) != 0 {
		t.Errorf("陈旧状态不应发信号,signal=%v kill=%v", *sigCalls, *killCalls)
	}
}

// TestServeRestartCmd_CorruptStateCleanedThenErrors 锁定损坏形态=未运行：
// 非法 JSON 状态文件被 stop 段清理后，restart 报错指引 serve start，绝不
// spawn（损坏与陈旧是 stop 编排的不同分支，restart 层分别锁定）。
func TestServeRestartCmd_CorruptStateCleanedThenErrors(t *testing.T) {
	sigCalls, killCalls := withServeStopSeams(t, func(int) error { return nil }, func(int) error { return nil })

	stubServeSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("损坏清理后 restart 不得 spawn")
		return 0, "", nil
	})

	dir := t.TempDir()
	if err := os.WriteFile(serve.StatePath(dir), []byte("{not json"), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"restart"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("损坏清理后 restart 应报非零错误")
	}
	if !errors.Is(execErr, errServeRestartNotRunning) {
		t.Errorf("错误应为 errServeRestartNotRunning,实际: %v", execErr)
	}
	for _, want := range []string{"corrupt state removed", "已清理损坏的状态文件"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout 清理说明应含 %q,实际: %q", want, out.String())
		}
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除,stat err = %v", err)
	}
	if len(*sigCalls) != 0 || len(*killCalls) != 0 {
		t.Errorf("损坏状态不应发信号,signal=%v kill=%v", *sigCalls, *killCalls)
	}
}

// TestServeRestartCmd_StopsRunningThenStartsNew 锁定核心编排顺序：运行中的
// 旧实例先被优雅停止（信号只发给旧 PID、探活确认下线、状态文件条件删除），
// 随后 start 段拉起全新后台实例（新 PID/新地址）。
func TestServeRestartCmd_StopsRunningThenStartsNew(t *testing.T) {
	// 旧实例：存活假端点,信号送达时关闭（下一次探活失败 → 判定已停）。
	tsOld := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	oldAddr := strings.TrimPrefix(tsOld.URL, "http://")

	// 新实例：假 spawn 的返回值。
	tsNew := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tsNew.Close()
	newAddr := strings.TrimPrefix(tsNew.URL, "http://")

	sigCalls, killCalls := withServeStopSeams(t,
		func(int) error { tsOld.Close(); return nil },
		func(int) error { t.Error("优雅信号已确认下线,不应强杀"); return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: oldAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写运行中状态: %v", err)
	}

	var spawnedAddr string
	stubServeSpawnAndWait(t, func(dataDir, binPath, addr, logPath string) (int, string, error) {
		spawnedAddr = addr
		// spawn 前旧状态必须已被 stop 段条件删除,否则轮询会把旧文件当就绪信号。
		if st, err := serve.ReadState(dataDir); err != nil || st != nil {
			t.Errorf("spawn 时旧状态应已被删除,实际 (%v, %v)", st, err)
		}
		return 5151, newAddr, nil
	})

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"restart"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("重启应先停后起,实际错误: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"dashboard stopped",
		"仪表板已停止",
		"dashboard started in background",
		"仪表板已后台启动",
		"http://" + newAddr,
		"5151",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
	if len(*sigCalls) != 1 || (*sigCalls)[0] != 4242 {
		t.Errorf("stop 段应恰向旧 PID 4242 发一次信号,实际 %v", *sigCalls)
	}
	if len(*killCalls) != 0 {
		t.Errorf("不应走到强杀兜底,kill=%v", *killCalls)
	}
	if spawnedAddr != "127.0.0.1:8619" {
		t.Errorf("spawn 应收到默认地址,实际 %q", spawnedAddr)
	}
}

// TestServeRestartCmd_StopFailureAborts 锁定中止契约：旧实例管不住（探活
// 判据下停止失败）时 restart 以非零错误中止,绝不进入 start 段 spawn 一个
// 注定抢不到端口的新实例;旧状态文件保留供排查,输出不得含启动成功文案。
func TestServeRestartCmd_StopFailureAborts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	// 信号与强杀都无效,端点持续响应——stop 段必然失败。
	_, _ = withServeStopSeams(t,
		func(int) error { return nil },
		func(int) error { return nil })

	stubServeSpawnAndWait(t, func(string, string, string, string) (int, string, error) {
		t.Error("stop 失败后不应进入 start 段 spawn")
		return 0, "", nil
	})

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写运行中状态: %v", err)
	}

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"restart"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("stop 失败时 restart 应报非零错误")
	}
	msg := execErr.Error()
	for _, want := range []string{
		"restart aborted",
		"重启中止",
		"still responding",
		"仍在响应",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, msg)
		}
	}
	if out := out.String(); strings.Contains(out, "dashboard started in background") || strings.Contains(out, "仪表板已后台启动") {
		t.Errorf("中止后不得输出启动成功文案,实际: %q", out)
	}
	if _, err := os.Stat(serve.StatePath(dir)); err != nil {
		t.Errorf("停止失败时状态文件必须保留,stat err = %v", err)
	}
}

// TestServeRestartCmd_CustomAddrFlowsToSpawn 锁定 flag 透传：restart 继承的
// persistent --addr 必须原样送达 spawn（与 start 同一通道）。setup 造一个
// 运行中旧实例（探活响应、信号后下线），restart 才会进入 start 段。
func TestServeRestartCmd_CustomAddrFlowsToSpawn(t *testing.T) {
	// 旧实例：存活假端点,信号送达时关闭（下一次探活失败 → 判定已停）。
	tsOld := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tsOld.Close()
	oldAddr := strings.TrimPrefix(tsOld.URL, "http://")

	withServeStopSeams(t,
		func(int) error { tsOld.Close(); return nil },
		func(int) error { return nil })

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: oldAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写运行中状态: %v", err)
	}

	stubServeSpawnAndWait(t, func(dataDir, binPath, addr, logPath string) (int, string, error) {
		if addr != "127.0.0.1:9443" {
			t.Errorf("spawn 应收到自定义 --addr,实际 %q", addr)
		}
		return 4343, "127.0.0.1:9443", nil
	})

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"restart", "--addr", "127.0.0.1:9443"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("restart --addr 应成功,实际错误: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "http://127.0.0.1:9443") {
		t.Errorf("输出应含新地址,实际: %q", got)
	}
}
