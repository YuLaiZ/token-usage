package cli

// update_render_serve_test.go 校验 update 结果渲染的 dashboard 维度：标题按
// daemon 与 dashboard 的原运行态组合分流（各自恢复才称「恢复」），安装完成
// 出口输出 dashboard 恢复明细行（含原监听地址与「未打开浏览器」明示），
// Windows 后台替换出口告知 dashboard 已停止并将被 helper 自动恢复。
// 另以生产装配链（updateServeLifecycle 适配器 + internal/serve 编排 seam）
// 断言自动恢复绝不携带浏览器打开行为。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/serve"
	"github.com/YuLaiZ/token-usage/internal/update"
)

func TestInstalledTitleServeCombinations(t *testing.T) {
	cases := []struct {
		name    string
		res     update.ApplyResult
		wantEn  string
		notWant []string
		forceEn string
		forceZh string
	}{
		{"both restored", update.ApplyResult{DaemonWasRunning: true, ServeWasRunning: true}, "Updated; daemon and dashboard restored", nil, "", ""},
		{"daemon only", update.ApplyResult{DaemonWasRunning: true}, "Updated and daemon restored", []string{"dashboard"}, "", ""},
		{"serve only", update.ApplyResult{ServeWasRunning: true}, "Updated; dashboard restored", []string{"daemon"}, "", ""},
		{"none", update.ApplyResult{}, "Updated", []string{"restored", "恢复"}, "", ""},
		{"both with force", update.ApplyResult{DaemonWasRunning: true, ServeWasRunning: true}, "Updated; daemon and dashboard restored (--force overwrite)", nil, " (--force overwrite)", "（--force 强制覆盖）"},
	}
	for _, tc := range cases {
		got := installedTitle(tc.res, tc.forceEn, tc.forceZh)
		if !strings.Contains(got, tc.wantEn) {
			t.Errorf("%s: 标题应含 %q,实际 %q", tc.name, tc.wantEn, got)
		}
		for _, no := range tc.notWant {
			if strings.Contains(got, no) {
				t.Errorf("%s: 标题不应含 %q,实际 %q", tc.name, no, got)
			}
		}
	}
}

func TestRenderApplyResult_ServeRestoredLine(t *testing.T) {
	res := update.ApplyResult{
		CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
		ProvenanceTrusted: true,
		ReadyToInstall:    true,
		Installed:         true,
		DaemonWasRunning:  true,
		ServeWasRunning:   true,
		ServeAddr:         "127.0.0.1:8619",
	}
	var out, errOut bytes.Buffer
	if err := renderApplyResult(&out, &errOut, "darwin", res); err != nil {
		t.Fatalf("渲染 err=%v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Updated; daemon and dashboard restored",
		"已更新，daemon 与 dashboard 已恢复",
		"http://127.0.0.1:8619",
		"the browser was not opened",
		"未打开浏览器",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际:\n%s", want, got)
		}
	}
}

func TestRenderApplyResult_ServeNotRunningNoLine(t *testing.T) {
	res := update.ApplyResult{
		CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
		ProvenanceTrusted: true,
		ReadyToInstall:    true,
		Installed:         true,
		DaemonWasRunning:  true,
	}
	var out, errOut bytes.Buffer
	if err := renderApplyResult(&out, &errOut, "darwin", res); err != nil {
		t.Fatalf("渲染 err=%v", err)
	}
	if got := out.String(); strings.Contains(got, "dashboard has been restored") || strings.Contains(got, "已按原监听地址后台恢复") {
		t.Errorf("dashboard 未运行时不得输出恢复明细行,实际:\n%s", got)
	}
}

func TestRenderApplyResult_ServeDeferredLine(t *testing.T) {
	res := update.ApplyResult{
		CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
		ProvenanceTrusted: true,
		ReadyToInstall:    true,
		Deferred:          true,
		DaemonWasRunning:  true,
		ServeWasRunning:   true,
		ServeAddr:         "127.0.0.1:8619",
	}
	var out, errOut bytes.Buffer
	if err := renderApplyResult(&out, &errOut, "windows", res); err != nil {
		t.Fatalf("渲染 err=%v", err)
	}
	got := out.String()
	for _, want := range []string{
		"restore it automatically at http://127.0.0.1:8619",
		"以原监听地址自动恢复",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出应含 %q,实际:\n%s", want, got)
		}
	}
}

func TestRenderApplyResult_ServeRecoveredLine(t *testing.T) {
	res := update.ApplyResult{
		Recovered:       true,
		RecoveryState:   update.RecoveryStateNewInstalled,
		ServeWasRunning: true,
		ServeAddr:       "127.0.0.1:8630",
	}
	var out, _errOut bytes.Buffer
	if err := renderApplyResult(&out, &_errOut, "darwin", res); err != nil {
		t.Fatalf("渲染 err=%v", err)
	}
	if got := out.String(); !strings.Contains(got, "http://127.0.0.1:8630") {
		t.Errorf("中断恢复成功出口应携带 dashboard 恢复地址,实际:\n%s", got)
	}
}

// TestUpdateServeAdapter_StartNeverOpensBrowser 用生产装配链断言「自动恢复
// 不打开浏览器」：适配器 Start → internal/serve.StartInBackground → SpawnAndWait
// seam 收到的调用只有 `_serve-run --addr` 语义，serveOpenBrowser seam 全程零调用。
func TestUpdateServeAdapter_StartNeverOpensBrowser(t *testing.T) {
	// 存活的假端点供预检与探活确认不误判（未运行路径放行由注入的 SpawnAndWait
	// 模拟——本测试关注编排传参而非真实 spawn）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	var captured struct {
		dataDir, binPath, addr, logPath string
	}
	origSpawn := serve.SpawnAndWait
	serve.SpawnAndWait = func(dataDir, binPath, addr, logPath string) (int, string, error) {
		captured.dataDir, captured.binPath, captured.addr, captured.logPath = dataDir, binPath, addr, logPath
		return 4242, "127.0.0.1:8677", nil
	}
	t.Cleanup(func() { serve.SpawnAndWait = origSpawn })

	browserCalls := 0
	origOpen := serveOpenBrowser
	serveOpenBrowser = func(string) error { browserCalls++; return nil }
	t.Cleanup(func() { serveOpenBrowser = origOpen })

	adapter := newUpdateServeLifecycle()
	if err := adapter.Start(t.TempDir(), "/opt/new/token-usage", "127.0.0.1:8677"); err != nil {
		t.Fatalf("Start err=%v", err)
	}
	if browserCalls != 0 {
		t.Errorf("自动恢复不得打开浏览器, browser calls=%d", browserCalls)
	}
	if captured.binPath != "/opt/new/token-usage" {
		t.Errorf("恢复必须用显式新二进制路径, got %q", captured.binPath)
	}
	if captured.addr != "127.0.0.1:8677" {
		t.Errorf("恢复必须携带原监听地址, got %q", captured.addr)
	}
	if strings.Contains(captured.addr, "open") || captured.addr == "" {
		t.Errorf("spawn 参数不得携带浏览器语义, got %q", captured.addr)
	}
}

// TestUpdateServeAdapter_DetectRunningIgnoresCorruptState 损坏 serve.json：
// 适配器判为未运行且不修改状态文件（清理职责留在 serve status/stop）。
func TestUpdateServeAdapter_DetectRunningIgnoresCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := writeServeStateFile(dir, "{not-json"); err != nil {
		t.Fatalf("写损坏状态: %v", err)
	}
	seeded := "{not-json"

	adapter := newUpdateServeLifecycle()
	running, addr, err := adapter.DetectRunning(dir)
	if err != nil || running || addr != "" {
		t.Errorf("损坏状态应判为未运行, got (%v, %q, %v)", running, addr, err)
	}
	if got := readServeStateFile(dir); got != seeded {
		t.Errorf("损坏状态文件不得被修改, got %q", got)
	}
}

func writeServeStateFile(dir, content string) error {
	return os.WriteFile(filepath.Join(dir, "serve.json"), []byte(content), 0644)
}

func readServeStateFile(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "serve.json"))
	if err != nil {
		return ""
	}
	return string(data)
}
