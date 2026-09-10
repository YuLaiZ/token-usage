package cli

// serve_status_test.go 用 httptest 假 /api/meta 端点驱动 serve status 的
// 三个分支：无状态文件、运行中、陈旧状态清理。不发真信号、不 spawn。

import (
	"bytes"
	"encoding/json"
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

// serveStatusCmdFixture 构造注入临时 DataDir 的 serve status 命令与输出 buffer。
func serveStatusCmdFixture(t *testing.T, dataDir string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newServeStatusCmd(func() (*config.Config, error) { return &config.Config{DataDir: dataDir}, nil })
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	return cmd, &buf
}

func TestServeStatusCmd_NotRunning(t *testing.T) {
	cmd, buf := serveStatusCmdFixture(t, t.TempDir())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("无状态文件应幂等成功,实际错误: %v", err)
	}
	for _, want := range []string{"serve is not running", "仪表板未在后台运行"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("输出应含 %q,实际: %q", want, buf.String())
		}
	}
}

func TestServeStatusCmd_Running(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{
		PID:       4242,
		Addr:      addr,
		StartedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStatusCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("运行中应成功: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"serve is running",
		"http://" + addr,
		"4242",
		"仪表板正在后台运行",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
	// 运行中不得删除状态文件。
	if _, err := os.Stat(serve.StatePath(dir)); err != nil {
		t.Errorf("运行中状态文件应保留: %v", err)
	}
}

func TestServeStatusCmd_StaleStateRemoved(t *testing.T) {
	// 预留一个确定空闲的回环端口后立即关闭:探活必然连接拒绝。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{
		PID:       999999,
		Addr:      addr,
		StartedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, buf := serveStatusCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("陈旧状态应清理成功且退出码 0,实际错误: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"serve is not running (stale state removed)", "已清理陈旧状态"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("陈旧状态文件应被删除,stat err = %v", err)
	}
}

// TestServeStatusCmd_ReportsNewInstanceWhenStateReplaced 锁定 TOCTOU 重评估：
// 旧状态探活无响应（判定陈旧），条件删除时发现文件已被存活的新实例 B 接管——
// status 按新实例报告运行中，不删除任何状态文件。
func TestServeStatusCmd_ReportsNewInstanceWhenStateReplaced(t *testing.T) {
	// 预留后关闭一个端口构成旧状态的陈旧地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	// 存活的假端点模拟新实例 B。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	liveAddr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	stale := &serve.ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}
	if err := serve.WriteState(dir, stale); err != nil {
		t.Fatalf("写陈旧状态: %v", err)
	}

	// 条件删除 seam：模拟磁盘状态已被改写为存活的新实例 B（真实写入），
	// 返回 (false, B) 触发重评估分支。
	next := &serve.ServeState{PID: 5555, Addr: liveAddr, StartedAt: time.Now().Format(time.RFC3339)}
	stubRemoveServeStateIfSame(t, func(dataDir string, judged *serve.ServeState) (bool, *serve.ServeState, error) {
		if err := serve.WriteState(dataDir, next); err != nil {
			return false, nil, err
		}
		return false, next, nil
	})

	cmd, buf := serveStatusCmdFixture(t, dir)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("按新实例报告应成功,实际错误: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"serve is running", "http://" + liveAddr, "5555", "仪表板正在后台运行"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际: %q", want, got)
		}
	}
	// 新实例 B 的状态文件原样保留。
	cur, err := serve.ReadState(dir)
	if err != nil || cur == nil || cur.PID != 5555 || cur.Addr != liveAddr {
		t.Errorf("新实例状态应原样保留,实际 (%+v, %v)", cur, err)
	}
}

// ---- serve status --format json ----

// jsonDecodeServeStatus 解析 status --format json 的输出为结构化载荷。
func jsonDecodeServeStatus(t *testing.T, raw string) serveStatusReport {
	t.Helper()
	var report serveStatusReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("解析 status json: %v\n原始输出: %s", err, raw)
	}
	return report
}

// TestServeStatusJSON_Running 运行中：state=running，pid/addr/url/started_at
// 齐备，data_dir 携带配置值；输出为两空格缩进 + 尾随换行。
func TestServeStatusJSON_Running(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	dir := t.TempDir()
	startedAt := "2026-09-10T14:51:01+08:00"
	if err := serve.WriteState(dir, &serve.ServeState{PID: 4242, Addr: addr, StartedAt: startedAt}); err != nil {
		t.Fatalf("写状态文件: %v", err)
	}

	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"status", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --format json 应成功, err=%v", err)
	}
	raw := out.String()
	if !strings.HasSuffix(raw, "\n") || !strings.Contains(raw, "\n  \"running\": true") {
		t.Errorf("输出应为两空格缩进 JSON 且以换行收尾,实际: %q", raw)
	}
	report := jsonDecodeServeStatus(t, raw)
	if !report.Running || report.State != "running" {
		t.Errorf("应报告运行中, got %+v", report)
	}
	if report.PID != 4242 || report.Addr != addr || report.URL != "http://"+addr || report.StartedAt != startedAt {
		t.Errorf("运行实例字段应与 serve.json 同源, got %+v", report)
	}
	if report.DataDir != dir {
		t.Errorf("data_dir 应为配置的数据目录, got %q", report.DataDir)
	}
}

// TestServeStatusJSON_NotRunningMissing 无状态文件：state=not_running，
// 运行实例字段不出现（omitempty）。
func TestServeStatusJSON_NotRunningMissing(t *testing.T) {
	cmd, out, _ := serveFamilyFixture(t, t.TempDir())
	cmd.SetArgs([]string{"status", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --format json 应成功, err=%v", err)
	}
	report := jsonDecodeServeStatus(t, out.String())
	if report.Running || report.State != "not_running" {
		t.Errorf("应报告未运行, got %+v", report)
	}
	// 运行实例字段承诺「仅在运行中出现」：结构体断言无法区分字段缺失与零值，
	// 以原始键集合强断言这些键在未运行时不存在。
	var rawKeys map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rawKeys); err != nil {
		t.Fatalf("解析 status json: %v", err)
	}
	for _, key := range []string{"pid", "addr", "url", "started_at"} {
		if _, present := rawKeys[key]; present {
			t.Errorf("未运行时 JSON 不得含 %q 键,实际: %v", key, rawKeys)
		}
	}
}

// TestServeStatusJSON_CorruptRemoved 损坏状态：state=not_running_corrupt_removed
// 且文件被清理（与 table 模式同一判定出口）。
func TestServeStatusJSON_CorruptRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serve.StatePath(dir), []byte("{not json"), 0644); err != nil {
		t.Fatalf("写损坏状态文件: %v", err)
	}
	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"status", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --format json 应成功, err=%v", err)
	}
	report := jsonDecodeServeStatus(t, out.String())
	if report.Running || report.State != "not_running_corrupt_removed" {
		t.Errorf("应报告损坏清理, got %+v", report)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("损坏状态文件应被删除, stat err = %v", err)
	}
}

// TestServeStatusJSON_StaleRemoved 陈旧状态（探活无响应）：
// state=not_running_stale_removed 且文件被条件删除。
func TestServeStatusJSON_StaleRemoved(t *testing.T) {
	// 预留后关闭一个端口构成陈旧记录。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	staleAddr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	if err := serve.WriteState(dir, &serve.ServeState{PID: 999999, Addr: staleAddr, StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("写陈旧状态: %v", err)
	}
	cmd, out, _ := serveFamilyFixture(t, dir)
	cmd.SetArgs([]string{"status", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --format json 应成功, err=%v", err)
	}
	report := jsonDecodeServeStatus(t, out.String())
	if report.Running || report.State != "not_running_stale_removed" {
		t.Errorf("应报告陈旧清理, got %+v", report)
	}
	if _, err := os.Stat(serve.StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("陈旧状态文件应被删除, stat err = %v", err)
	}
}

// TestServeStatusJSON_InvalidFormat 非法 --format：双语错误（与 daemon status
// 的非法值错误文案同型）。
func TestServeStatusJSON_InvalidFormat(t *testing.T) {
	cmd, _, _ := serveFamilyFixture(t, t.TempDir())
	cmd.SetArgs([]string{"status", "--format", "yaml"})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("非法 --format 应报错")
	}
	for _, want := range []string{`invalid --format "yaml"`, `无效的 --format "yaml"`, "table, json"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("错误信息应含 %q,实际: %q", want, execErr.Error())
		}
	}
}
