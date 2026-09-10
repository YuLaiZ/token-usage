package cli

// command_surface_test.go 锁定命令面收敛后的公开 CLI 契约：
//
//  1. 根帮助仅展示收敛后的命令集合（daemon/serve 命令组，无顶层生命周期命令
//     与六个分析命令）。
//  2. 裸 daemon / 裸 serve 只显示命令组帮助：退出码 0，无 spawn、无监听、
//     无运行态文件写入。
//  3. 已删除命令按 unknown command 失败，不是 hidden/deprecated 转发。
//  4. Shell 补全与 __complete 不暴露已删除命令和内部入口。
//  5. internal/web 不 import internal/cli（HTTP 数据面对 CLI 的依赖边界）。
//  6. daemon 与 serve 两个服务生命周期互不影响（状态文件互不触碰）。

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// removedCommands 是命令面收敛删除的十个公开命令：四个旧顶层生命周期命令
// 与六个分析命令。任何形态（顶层、hidden 别名、转发）重新出现都应失败。
var removedCommands = []string{
	"start", "status", "stop", "restart",
	"chart", "compare", "export", "forecast", "report", "top",
}

// executeRoot 以隔离 HOME 构造根命令并执行 args，返回退出错误与输出。
func executeRoot(t *testing.T, args ...string) (error, string, string) {
	t.Helper()
	return executeRootWithHome(t, t.TempDir(), args...)
}

// executeRootWithHome 在指定 HOME（通常由 setupHomeConfig 预置配置）下执行 args。
func executeRootWithHome(t *testing.T, home string, args ...string) (error, string, string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	root := NewRootCmd()
	root.SetArgs(args)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	return err, out.String(), errOut.String()
}

// TestCommandSurface_RootHelp 根 --help 展示 daemon/serve 命令组，
// 不展示已删除的顶层生命周期命令与六个分析命令。
func TestCommandSurface_RootHelp(t *testing.T) {
	err, out, _ := executeRoot(t, "--help")
	if err != nil {
		t.Fatalf("root --help 应成功: %v", err)
	}
	for _, want := range []string{"daemon", "serve", "query", "collect", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("root --help 应展示 %q:\n%s", want, out)
		}
	}
	for _, name := range removedCommands {
		// 帮助正文按「Available Commands」逐行列出命令；已删除命令不得作为
		// 独立条目出现（匹配 "  <name> " 行首形态，避免误伤子串）。
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "  "+name+" ") {
				t.Errorf("root --help 不应列出已删除命令 %q（行: %q）", name, line)
			}
		}
	}
}

// TestCommandSurface_GroupHelps daemon --help 与 serve --help 均展示相同的
// 四个动作；daemon 描述采集守护进程、serve 描述仪表板服务。
func TestCommandSurface_GroupHelps(t *testing.T) {
	err, out, _ := executeRoot(t, "daemon", "--help")
	if err != nil {
		t.Fatalf("daemon --help 应成功: %v", err)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon --help 应展示动作 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "collection") && !strings.Contains(out, "采集") {
		t.Errorf("daemon --help 应说明管理对象是采集守护进程:\n%s", out)
	}

	err, out, _ = executeRoot(t, "serve", "--help")
	if err != nil {
		t.Fatalf("serve --help 应成功: %v", err)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help 应展示动作 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "dashboard") && !strings.Contains(out, "仪表板") {
		t.Errorf("serve --help 应说明管理对象是仪表板服务:\n%s", out)
	}
}

// TestCommandSurface_BareDaemonNoSideEffect 裸 `token-usage daemon` 只显示
// 帮助：退出 0，且不产生进程/端口/状态文件副作用——隔离 HOME 下连
// ~/.token-usage 目录都不得创建。
func TestCommandSurface_BareDaemonNoSideEffect(t *testing.T) {
	err, out, errOut := executeRoot(t, "daemon")
	if err != nil {
		t.Fatalf("裸 daemon 应退出 0: %v（stderr: %s）", err, errOut)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("裸 daemon 应显示命令组帮助（缺 %q）:\n%s", want, out)
		}
	}
	assertNoTokenUsageDir(t)
}

// TestCommandSurface_BareServeNoSideEffect 裸 `token-usage serve` 只显示
// 帮助：退出 0，不监听端口、不 spawn、不创建 serve.json/锁/日志等运行态文件。
func TestCommandSurface_BareServeNoSideEffect(t *testing.T) {
	err, out, errOut := executeRoot(t, "serve")
	if err != nil {
		t.Fatalf("裸 serve 应退出 0: %v（stderr: %s）", err, errOut)
	}
	for _, want := range []string{"start", "status", "stop", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("裸 serve 应显示命令组帮助（缺 %q）:\n%s", want, out)
		}
	}
	assertNoTokenUsageDir(t)
}

// assertNoTokenUsageDir 断言隔离 HOME 下未创建 ~/.token-usage（零副作用的
// 目录级证据：命令组帮助路径不触碰文件系统）。
func assertNoTokenUsageDir(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".token-usage")); !os.IsNotExist(err) {
		t.Errorf("命令组帮助不得创建 ~/.token-usage（无 spawn/监听/状态文件写入），stat err=%v", err)
	}
}

// TestCommandSurface_RemovedCommandsAreUnknown 已删除命令必须按 unknown
// command 失败：非零退出、报 unknown command、stdout 无数据输出（不是
// hidden/deprecated 转发，也没有可用入口）。
func TestCommandSurface_RemovedCommandsAreUnknown(t *testing.T) {
	for _, name := range removedCommands {
		t.Run(name, func(t *testing.T) {
			err, out, errOut := executeRoot(t, name)
			if err == nil {
				t.Fatalf("已删除命令 %q 应以非零退出失败", name)
			}
			combined := out + errOut
			if !strings.Contains(combined, "unknown command") {
				t.Errorf("%q 应报 unknown command，实际输出:\n%s", name, combined)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("已删除命令 %q 不得产生 stdout 数据输出，实际:\n%s", name, out)
			}
		})
	}
}

// TestCommandSurface_CompletionExcludesRemovedAndInternal Shell 补全面
// 不含已删除命令与内部入口：completion 脚本生成成功且不引用它们；
// __complete（cobra 运行时补全协议，zsh/bash 脚本最终都调用它）只候选
// 可见命令。
func TestCommandSurface_CompletionExcludesRemovedAndInternal(t *testing.T) {
	// completion 脚本：直调 cobra 生成 API 到自有 buffer（completion 命令
	// 的 RunE 在 InitDefaultCompletionCmd 时即绑定 stdout，无法注入 writer）。
	var script bytes.Buffer
	root := NewRootCmd()
	if err := root.GenZshCompletion(&script); err != nil {
		t.Fatalf("GenZshCompletion 应成功: %v", err)
	}
	if len(strings.TrimSpace(script.String())) == 0 {
		t.Fatal("zsh completion 脚本不应为空")
	}
	var bashScript bytes.Buffer
	root2 := NewRootCmd()
	if err := root2.GenBashCompletionV2(&bashScript, true); err != nil {
		t.Fatalf("GenBashCompletionV2 应成功: %v", err)
	}
	generated := script.String() + bashScript.String()
	for _, name := range removedCommands {
		if strings.Contains(generated, name+"--") || strings.Contains(generated, `"`+name+`"`) {
			t.Errorf("completion 脚本不应引用已删除命令 %q", name)
		}
	}

	// __complete：cobra 运行时补全协议对空前缀的候选即用户可 Tab 到的命令面。
	err, out, _ := executeRoot(t, "__complete", "")
	if err != nil {
		t.Fatalf("__complete 应成功: %v", err)
	}
	for _, want := range []string{"daemon", "serve"} {
		if !strings.Contains(out, want) {
			t.Errorf("__complete 应候选 %q:\n%s", want, out)
		}
	}
	for _, name := range append(removedCommands, "_run", "_serve-run") {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, name) {
				t.Errorf("__complete 不应候选 %q（行: %q）", name, line)
			}
		}
	}
}

// TestCommandSurface_WebDoesNotImportCli HTTP 数据面对 CLI 的依赖边界：
// internal/web 的全部源文件不得 import internal/cli。web 只依赖
// charts/querier 等无 UI 内部能力；若此边界被打破，仪表板将反向依赖
// Cobra/进程语义。
func TestCommandSurface_WebDoesNotImportCli(t *testing.T) {
	dir := filepath.Join("..", "web")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 internal/web 目录失败: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, src, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", src, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "github.com/YuLaiZ/token-usage/internal/cli" {
				t.Errorf("internal/web 不得 import internal/cli（HTTP 层禁止依赖 CLI）: %s", e.Name())
			}
		}
	}
}

// TestCommandSurface_DaemonStopLeavesServeStateAlone 两个服务生命周期互不影响
// （daemon → serve 方向）：`daemon stop` 只管理采集守护进程，对数据目录中的
// serve.json 只字不读、原样保留。
func TestCommandSurface_DaemonStopLeavesServeStateAlone(t *testing.T) {
	dataDir := t.TempDir()
	home := setupHomeConfig(t, `data_dir = "`+filepath.ToSlash(dataDir)+`"
[daemon]
poll_interval = 30
`)
	serveState := filepath.Join(dataDir, "serve.json")
	marker := []byte(`{"pid": 999999, "addr": "127.0.0.1:1", "started_at": "2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(serveState, marker, 0644); err != nil {
		t.Fatal(err)
	}

	err, out, _ := executeRootWithHome(t, home, "daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop（无守护进程）应幂等成功: %v", err)
	}
	if !strings.Contains(out, "未运行") {
		t.Errorf("daemon stop 应输出未运行，实际: %q", out)
	}
	got, err := os.ReadFile(serveState)
	if err != nil {
		t.Fatalf("daemon stop 不得删除 serve.json: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Errorf("daemon stop 不得改写 serve.json:\n got %q\nwant %q", got, marker)
	}
}

// TestCommandSurface_ServeStopLeavesDaemonStateAlone 两个服务生命周期互不影响
// （serve → daemon 方向）：`serve stop` 只作用于仪表板状态，对守护进程的
// daemon lock 文件（token-usage.lock）不创建不改写。
func TestCommandSurface_ServeStopLeavesDaemonStateAlone(t *testing.T) {
	dataDir := t.TempDir()
	home := setupHomeConfig(t, `data_dir = "`+filepath.ToSlash(dataDir)+`"
[daemon]
poll_interval = 30
`)
	daemonLock := filepath.Join(dataDir, "token-usage.lock")
	marker := []byte("daemon-lock-marker")
	if err := os.WriteFile(daemonLock, marker, 0644); err != nil {
		t.Fatal(err)
	}

	err, out, _ := executeRootWithHome(t, home, "serve", "stop")
	if err != nil {
		t.Fatalf("serve stop（无运行实例）应幂等成功: %v", err)
	}
	if strings.Contains(out, ui.Bi("dashboard stopped", "仪表板已停止")) {
		t.Errorf("无实例时 serve stop 不应报告已停止: %q", out)
	}
	got, err := os.ReadFile(daemonLock)
	if err != nil {
		t.Fatalf("serve stop 不得删除 daemon lock: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Errorf("serve stop 不得改写 daemon lock:\n got %q\nwant %q", got, marker)
	}
}
