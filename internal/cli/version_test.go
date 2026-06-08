package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
)

// fixedInfo 是 version 子命令与 root --version 共享的固定构建信息快照，
// 两者必须来自同一份 Info（经构造器注入），保证输出一致。
var fixedInfo = buildinfo.Info{
	Version:   "v0.1.0",
	Commit:    "59a8d55a1b2c",
	BuildTime: "2026-07-30T10:00:00Z",
	GoVersion: "go1.26.4",
	GOOS:      "darwin",
	GOARCH:    "arm64",
	Modified:  false,
}

// ---- 用例 1/2: version 子命令挂载，且为用户可见（非 Hidden）----

func TestVersionCmd_MountedAndVisible(t *testing.T) {
	root := newRootCmd(fixedInfo)
	for _, sub := range root.Commands() {
		if sub.Name() == "version" {
			if sub.Hidden {
				t.Error("version 子命令应为用户可见（非 Hidden）")
			}
			return
		}
	}
	t.Error("root 应挂载 version 子命令")
}

// ---- 用例 3: --version 输出严格等于 "token-usage v0.1.0\n" ----
// 测试隔离：独立 root + 独立 buffer，未与其它执行场景共享。

func TestVersionFlag_LongVersionOutput(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--version 执行失败: %v", err)
	}
	want := "token-usage v0.1.0\n"
	if got := out.String(); got != want {
		t.Errorf("--version 输出不匹配:\ngot:  %q\nwant: %q", got, want)
	}
}

// ---- 用例 4: -v 输出与 --version 严格相同（各自独立 root + buffer）----

func TestVersionFlag_ShortVersionOutput(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"-v"})

	if err := root.Execute(); err != nil {
		t.Fatalf("-v 执行失败: %v", err)
	}
	want := "token-usage v0.1.0\n"
	if got := out.String(); got != want {
		t.Errorf("-v 输出不匹配:\ngot:  %q\nwant: %q", got, want)
	}
}

// ---- 用例 5: version 子命令输出严格等于 fixedInfo.Detail() ----

func TestVersionCmd_DetailOutput(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version 执行失败: %v", err)
	}
	want := fixedInfo.Detail()
	if got := out.String(); got != want {
		t.Errorf("version 输出不等于 fixedInfo.Detail():\ngot:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasSuffix(want, "\n") {
		t.Errorf("fixedInfo.Detail() 缺少末尾换行（buildinfo 契约回归）")
	}
}

// ---- 用例 6: version 多余参数返回 error（NoArgs 拒绝）----

func TestVersionCmd_RejectsUnexpectedArgs(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version", "unexpected"})

	err := root.Execute()
	if err == nil {
		t.Fatal("version 多余参数应返回 error（NoArgs 拒绝）")
	}
}

// ---- 用例 7: --version 与 version 使用同一 fixed Info snapshot ----
// 经构造器注入保证两者来自同一份 info：断言 --version 的版本号与 version 详情首行一致，
// 且均为 fixedInfo.Version，不依赖指针比较。

func TestVersionFlagAndCommand_ShareSameInfoSnapshot(t *testing.T) {
	// 独立 root 执行 --version
	longRoot := newRootCmd(fixedInfo)
	var longOut bytes.Buffer
	longRoot.SetOut(&longOut)
	longRoot.SetErr(&longOut)
	longRoot.SetArgs([]string{"--version"})
	if err := longRoot.Execute(); err != nil {
		t.Fatalf("--version 执行失败: %v", err)
	}

	// 独立 root 执行 version
	subRoot := newRootCmd(fixedInfo)
	var subOut bytes.Buffer
	subRoot.SetOut(&subOut)
	subRoot.SetErr(&subOut)
	subRoot.SetArgs([]string{"version"})
	if err := subRoot.Execute(); err != nil {
		t.Fatalf("version 执行失败: %v", err)
	}

	// --version 输出应为 "token-usage <fixedVersion>\n"。
	wantLong := "token-usage " + fixedInfo.Version + "\n"
	if got := longOut.String(); got != wantLong {
		t.Errorf("--version 与 fixedInfo 版本号不一致:\ngot:  %q\nwant: %q", got, wantLong)
	}

	// version 详情首行也应含相同版本号。
	firstLine := strings.Split(strings.TrimSuffix(subOut.String(), "\n"), "\n")[0]
	if firstLine != "token-usage "+fixedInfo.Version {
		t.Errorf("version 详情首行版本号不一致:\ngot:  %q\nwant: %q", firstLine, "token-usage "+fixedInfo.Version)
	}
}

// ---- 用例 8: --help 输出同时包含 version 子命令与可见 -v, --version flag ----
// 测试隔离：独立 root 执行 --help，对输出字符串做 Contains 断言，
// 不遍历执行后 root 的 Commands()（Cobra 会延迟注入 completion/help）。

func TestVersionCmd_HelpListsVersionSubcommandAndFlag(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--help 执行失败: %v", err)
	}
	got := out.String()

	// 必须同时命中子命令文本与 flag 文本，不能只命中子命令就通过。
	if !strings.Contains(got, "version") {
		t.Errorf("--help 输出应包含 version 子命令，实际:\n%s", got)
	}
	if !strings.Contains(got, "-v, --version") {
		t.Errorf("--help 输出应包含可见的 -v, --version flag，实际:\n%s", got)
	}
}
