package cli

import (
	"bytes"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// TestServeCmd_BareShowsHelpNoSideEffect 裸 `serve` 只显示命令组帮助并以
// 退出码 0 返回：不监听端口、不 spawn、不创建 serve.json/serve.lock 等运行态
// 文件（副作用边界在临时 DataDir 上断言）。
func TestServeCmd_BareShowsHelpNoSideEffect(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: dataDir}, nil },
		"test-version",
	)
	cmd.SetArgs(nil)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("裸 serve 应退出 0，实际: %v", err)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("帮助应列出动作 %q:\n%s", want, out.String())
		}
	}
	// 零副作用：DataDir 下不得出现任何运行态文件。
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("裸 serve 不得产生运行态文件，实际: %v", entries)
	}
}

// TestServeCmd_UnknownSubcommandFails serve 带未知子命令按 unknown command
// 失败（非零退出），不静默当作帮助或转发。
func TestServeCmd_UnknownSubcommandFails(t *testing.T) {
	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		"test-version",
	)
	cmd.SetArgs([]string{"frobnicate"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("未知子命令应报错")
	}
	if combined := out.String() + errOut.String(); !strings.Contains(combined, "unknown command") {
		t.Errorf("serve 未知子命令应报 unknown command，实际:\n%s", combined)
	}
}

// TestServeDashboard_ListenFailure 前台路径删除后，监听失败路径仍由
// serveDashboard 承载（_serve-run 使用）：端口被占用时返回双语错误，不写
// serve.json（监听成功才写状态文件）。
func TestServeDashboard_ListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	defer func() { _ = ln.Close() }()

	dataDir := t.TempDir()
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageDB.Close() }()

	cfg := &config.Config{DataDir: dataDir}
	err = serveDashboard(cfg, usageDB, "test-version", ln.Addr().String(), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("端口被占用应报错")
	}
	if msg := err.Error(); !strings.Contains(msg, "failed to listen") && !strings.Contains(msg, "监听") {
		t.Errorf("错误文案应含监听失败双语前缀,实际 %q", msg)
	}
	// 监听失败不得写出 serve.json。
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "serve.json" {
			t.Errorf("监听失败不应写出 serve.json，实际目录: %v", entries)
		}
	}
}

// TestHyperlinkURL 校验 OSC 8 超链接渲染合同：TTY 下精确形态为
// "\x1b]8;;URL\x07URL\x1b]8;;\x07" 且可见文本恰为 URL 本身；非 TTY 或空
// URL 原样返回，保证重定向/管道输出不带任何转义序列。
func TestHyperlinkURL(t *testing.T) {
	const url = "http://127.0.0.1:8619"
	const esc = "\x1b]"
	const bel = "\x07"

	linked := hyperlinkURL(url, true)
	if want := esc + "8;;" + url + bel + url + esc + "8;;" + bel; linked != want {
		t.Errorf("TTY 精确形态不符:\n got %q\nwant %q", linked, want)
	}
	for _, want := range []string{
		esc + "8;;" + url + bel, // 开启序列,链接目标为 URL
		url,                     // 可见文本
		esc + "8;;" + bel,       // 关闭序列
	} {
		if !strings.Contains(linked, want) {
			t.Errorf("TTY 输出应含 %q,实际 %q", want, linked)
		}
	}
	// 可见文本恰为 URL 本身:去掉两段转义后剩余内容等于 url。
	visible := strings.ReplaceAll(linked, esc+"8;;"+url+bel, "")
	visible = strings.ReplaceAll(visible, esc+"8;;"+bel, "")
	if visible != url {
		t.Errorf("可见文本应为 URL 本身,实际 %q", visible)
	}

	for name, tc := range map[string]struct {
		url string
		tty bool
	}{
		"非 TTY 原样返回":   {url, false},
		"空串 TTY 原样返回":  {"", true},
		"空串非 TTY 原样返回": {"", false},
	} {
		got := hyperlinkURL(tc.url, tc.tty)
		if got != tc.url {
			t.Errorf("%s: 应原样返回 %q,实际 %q", name, tc.url, got)
		}
		if !tc.tty && strings.Contains(got, esc) {
			t.Errorf("%s: 非 TTY 输出不应含转义序列,实际 %q", name, got)
		}
	}
}

// lookupCommand 按名在命令树第一层查找子命令。
func lookupCommand(root *cobra.Command, name string) *cobra.Command {
	for _, sub := range root.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}

// TestServeCmd_RegisteredAndFlags:serve 在 root 注册、Short/Long 双语合同
// (Long 含可照抄示例、0.0.0.0 安全说明与「裸执行只显示帮助」边界)、
// flag 缺省值正确,且 root --help 列出 serve。
func TestServeCmd_RegisteredAndFlags(t *testing.T) {
	root := NewRootCmd()
	serveFound := lookupCommand(root, "serve")
	if serveFound == nil {
		t.Fatal("root 应注册 serve 子命令")
	}
	if got := serveFound.Use; got != "serve" {
		t.Errorf("Use 应为 serve,实际 %q", got)
	}
	for _, want := range []string{"Manage the local dashboard server", "管理本地仪表板服务"} {
		if !strings.Contains(serveFound.Short, want) {
			t.Errorf("Short 应含 %q,实际 %q", want, serveFound.Short)
		}
	}
	// Long 含可照抄示例、安全说明与裸执行零副作用边界。
	for _, want := range []string{
		"token-usage serve start",
		"token-usage serve start --addr 127.0.0.1:9000",
		"token-usage serve status",
		"token-usage serve stop",
		"token-usage serve restart",
		"0.0.0.0",
		"only prints this help",
	} {
		if !strings.Contains(serveFound.Long, want) {
			t.Errorf("Long 应含 %q", want)
		}
	}
	// serve 是命令组：--addr/--open 挂在 persistent flags 供子命令继承。
	// 裸执行只打印帮助的零副作用边界由 TestServeCmd_BareShowsHelpNoSideEffect
	// 以行为断言锁定。
	// flag 缺省值:--addr 仅回环,--open 关闭。两 flag 在 persistent flags
	// 上供 start/restart 子命令继承。
	addr := serveFound.PersistentFlags().Lookup("addr")
	if addr == nil || addr.DefValue != "127.0.0.1:8619" {
		t.Errorf("--addr 缺省应为 127.0.0.1:8619,实际 %+v", addr)
	}
	openFlag := serveFound.PersistentFlags().Lookup("open")
	if openFlag == nil || openFlag.DefValue != "false" {
		t.Errorf("--open 缺省应为 false,实际 %+v", openFlag)
	}

	// root --help 列出 serve。
	var buf bytes.Buffer
	helpRoot := NewRootCmd()
	helpRoot.SetOut(&buf)
	helpRoot.SetErr(&buf)
	helpRoot.SetArgs([]string{"--help"})
	if err := helpRoot.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\n  serve ") {
		t.Errorf("root --help 应列出 serve:\n%s", buf.String())
	}
}
