package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// TestServeCmd_InvalidAddr:--addr 缺端口等非法监听地址必须在启动前报错
// (常规 RunE 错误路径,不起真端口)。
func TestServeCmd_InvalidAddr(t *testing.T) {
	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return db.Open(":memory:") },
		"test-version",
	)
	cmd.SetArgs([]string{"--addr", "127.0.0.1"}) // 缺端口
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("非法 --addr(缺端口)应报错")
	}
}

// TestServeCmd_PortInUse:监听失败走 RunE 常规错误路径,错误文案含双语前缀。
func TestServeCmd_PortInUse(t *testing.T) {
	// 占用一个真实回环端口,再让 serve 绑定同一地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法预留回环端口: %v", err)
	}
	defer func() { _ = ln.Close() }()

	cmd := newServeCmdWithDeps(
		func() (*config.Config, error) { return &config.Config{DataDir: t.TempDir()}, nil },
		func(string) (*db.DB, error) { return db.Open(":memory:") },
		"test-version",
	)
	cmd.SetArgs([]string{"--addr", ln.Addr().String()})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatal("端口被占用应报错")
	}
	if msg := execErr.Error(); !strings.Contains(msg, "failed to listen") && !strings.Contains(msg, "监听") {
		t.Errorf("错误文案应含监听失败双语前缀,实际 %q", msg)
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
// (Long 含可照抄示例与 0.0.0.0 安全说明)、flag 缺省值正确,且 root --help
// 列出 serve。
func TestServeCmd_RegisteredAndFlags(t *testing.T) {
	root := NewRootCmd()
	serveFound := lookupCommand(root, "serve")
	if serveFound == nil {
		t.Fatal("root 应注册 serve 子命令")
	}
	if got := serveFound.Use; got != "serve" {
		t.Errorf("Use 应为 serve,实际 %q", got)
	}
	for _, want := range []string{"Start the local dashboard server", "启动本地仪表板服务"} {
		if !strings.Contains(serveFound.Short, want) {
			t.Errorf("Short 应含 %q,实际 %q", want, serveFound.Short)
		}
	}
	// Long 含三个可照抄示例与安全说明。
	for _, want := range []string{
		"token-usage serve",
		"token-usage serve --open",
		"token-usage serve --addr 127.0.0.1:9000",
		"0.0.0.0",
		"Ctrl+C",
	} {
		if !strings.Contains(serveFound.Long, want) {
			t.Errorf("Long 应含 %q", want)
		}
	}
	// flag 缺省值:--addr 仅回环,--open 关闭。两 flag 已移至 persistent flags
	// 供 start/status/stop 子命令继承,故查找需回退到 PersistentFlags。
	addr := serveFound.Flags().Lookup("addr")
	if addr == nil {
		addr = serveFound.PersistentFlags().Lookup("addr")
	}
	if addr == nil || addr.DefValue != "127.0.0.1:8619" {
		t.Errorf("--addr 缺省应为 127.0.0.1:8619,实际 %+v", addr)
	}
	openFlag := serveFound.Flags().Lookup("open")
	if openFlag == nil {
		openFlag = serveFound.PersistentFlags().Lookup("open")
	}
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
