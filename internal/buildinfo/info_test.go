// internal/buildinfo/info_test.go
package buildinfo

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

// 版本/平台常量集中管理，便于一致性；progName 复用 info.go 中的常量。
const (
	testGoVersion = "go1.26.4"
	testGOOS      = "darwin"
	testGOARCH    = "arm64"
)

// setting 快捷构造器。
func setting(k, v string) debug.BuildSetting {
	return debug.BuildSetting{Key: k, Value: v}
}

// fullRevision 是一个超过 12 位的完整 revision 样例。
const fullRevision = "59a8d55a1b2c3d4e5f60718293a4b5c6d7e8f90"
const shortRevision = "59a8d55a1b2c"

// 拆分 Detail 输出，校验严格五行（含末尾换行拆出的尾部空串被去掉）。
func detailLines(t *testing.T, s string) []string {
	t.Helper()
	// 必须以换行结尾，去掉它再按行切。
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("Detail 输出缺少末尾换行, got %q", s)
	}
	trimmed := strings.TrimSuffix(s, "\n")
	return strings.Split(trimmed, "\n")
}

// ---- 用例 1: 注入的 Version/Commit/BuildTime 全部生效 ----

func TestResolve_注入的版本提交构建时间全部生效(t *testing.T) {
	in := versionVars{Version: "v0.1.0", Commit: "abcdef123456", BuildTime: "2026-07-30T10:00:00Z"}
	got := resolve(in, nil, testGoVersion, testGOOS, testGOARCH)

	if got.Version != "v0.1.0" {
		t.Errorf("Version = %q, want v0.1.0", got.Version)
	}
	if got.Commit != "abcdef123456" {
		t.Errorf("Commit = %q, want abcdef123456", got.Commit)
	}
	if got.BuildTime != "2026-07-30T10:00:00Z" {
		t.Errorf("BuildTime = %q, want 2026-07-30T10:00:00Z", got.BuildTime)
	}
	if got.GoVersion != testGoVersion {
		t.Errorf("GoVersion = %q, want %s", got.GoVersion, testGoVersion)
	}
	if got.GOOS != testGOOS {
		t.Errorf("GOOS = %q, want %s", got.GOOS, testGOOS)
	}
	if got.GOARCH != testGOARCH {
		t.Errorf("GOARCH = %q, want %s", got.GOARCH, testGOARCH)
	}
	if got.Modified {
		t.Errorf("Modified = true, want false（无 build info 也无注入）")
	}
}

// ---- 用例 2: 注入 Release version 优先于 debug.BuildInfo.Main.Version ----

func TestResolve_注入的ReleaseVersion优先于MainVersion(t *testing.T) {
	in := versionVars{Version: "v9.9.9"}
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.0"},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if got.Version != "v9.9.9" {
		t.Errorf("Version = %q, want v9.9.9（注入优先）", got.Version)
	}
}

// ---- 用例 3: 注入版本为默认 dev 时，合法 Main.Version=v0.1.0 可回填 Version ----

func TestResolve_默认dev时用MainVersion回填(t *testing.T) {
	// 注入默认 dev（即未真正注入 release 版本号）。
	in := versionVars{Version: "dev"}
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.0"},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if got.Version != "v0.1.0" {
		t.Errorf("Version = %q, want v0.1.0（由 Main.Version 回填）", got.Version)
	}
}

// ---- 用例 4: Main.Version 为空值或 (devel) 时回退 dev ----

func TestResolve_MainVersion为空或Devel时回退dev(t *testing.T) {
	cases := []struct {
		name    string
		mainVer string
	}{
		{"空值", ""},
		{"devel标记", "(devel)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := versionVars{Version: "dev"}
			bi := &debug.BuildInfo{
				Main: debug.Module{Version: c.mainVer},
			}
			got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
			if got.Version != "dev" {
				t.Errorf("Version = %q, want dev", got.Version)
			}
		})
	}
}

// ---- 用例 4b: Main.Version 为本地构建伪版本号时回退 dev ----
//
// 本地直接 go build / make build 时，Go 工具链会把 Main.Version 填成
// 伪版本号（形如 v0.0.0-YYYYMMDDHHMMSS-hash[+dirty]）。它既非空值也非
// "(devel)"，但不是真实的 SemVer 模块版本，必须排除并回退到 "dev"。
// 只有 go install pkg@v0.1.0 这类经模块代理下载的真实 SemVer 才允许回填。

func TestResolve_MainVersion为伪版本号时回退dev(t *testing.T) {
	cases := []struct {
		name    string
		mainVer string
		want    string
	}{
		{"伪版本号含dirty后缀", "v0.0.0-20260730061846-59a8d5538012+dirty", "dev"},
		{"伪版本号不含dirty后缀", "v0.0.0-20260730061846-59a8d5538012", "dev"},
		{"真实SemVer不误伤", "v0.1.0", "v0.1.0"},
		{"devel标记仍回退", "(devel)", "dev"},
		{"空值仍回退", "", "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := versionVars{Version: "dev"}
			bi := &debug.BuildInfo{
				Main: debug.Module{Version: c.mainVer},
			}
			got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
			if got.Version != c.want {
				t.Errorf("Version = %q, want %q", got.Version, c.want)
			}
		})
	}
}

// ---- 用例 5: commit 从 vcs.revision 回填并截断到 12 位 ----

func TestResolve_commit从VcsRevision回填并截断到12位(t *testing.T) {
	in := versionVars{Version: "dev"} // 无注入 commit
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			setting("vcs.revision", fullRevision),
		},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	// 内部保留完整 revision。
	if got.Commit != fullRevision {
		t.Errorf("Commit = %q, want 完整 %s", got.Commit, fullRevision)
	}
	// 展示层截断 12 位。
	if d := got.Detail(); !strings.Contains(d, "commit: "+shortRevision+"\n") {
		t.Errorf("Detail 缺少截断 commit 行 %q, got %q", shortRevision, d)
	}
}

// ---- 用例 6: 注入 commit 优先于 VCS revision ----

func TestResolve_注入commit优先于VcsRevision(t *testing.T) {
	in := versionVars{Version: "dev", Commit: "injected123456"}
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			setting("vcs.revision", fullRevision),
		},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if got.Commit != "injected123456" {
		t.Errorf("Commit = %q, want injected123456（注入优先）", got.Commit)
	}
}

// ---- 用例 7: vcs.modified=true 记录 dirty，展示追加 -dirty ----

func TestResolve_vcsModified为true时记录dirty(t *testing.T) {
	in := versionVars{Version: "dev"}
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			setting("vcs.revision", fullRevision),
			setting("vcs.modified", "true"),
		},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if !got.Modified {
		t.Errorf("Modified = false, want true")
	}
	// Detail 中 commit 行应为 <12位>-dirty。
	lines := detailLines(t, got.Detail())
	if len(lines) != 5 {
		t.Fatalf("Detail 行数 = %d, want 5", len(lines))
	}
	wantCommitLine := "commit: " + shortRevision + "-dirty"
	if lines[1] != wantCommitLine {
		t.Errorf("commit 行 = %q, want %q", lines[1], wantCommitLine)
	}
}

// ---- 用例 8: 即使存在 vcs.time 或 SOURCE_DATE_EPOCH，无注入 BuildTime 仍为 unknown ----

func TestResolve_无注入BuildTime时忽略VcsTime和环境变量(t *testing.T) {
	// 显式设置环境变量，确认不被读取。
	t.Setenv("SOURCE_DATE_EPOCH", "1753879200")

	in := versionVars{Version: "dev"} // 无注入 BuildTime
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			setting("vcs.time", "2026-07-30T10:00:00Z"),
			setting("vcs.revision", fullRevision),
		},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if got.BuildTime != "unknown" {
		t.Errorf("BuildTime = %q, want unknown（不读取 vcs.time/环境变量）", got.BuildTime)
	}
	// 二次确认环境变量确实存在但未被消费。
	if v, ok := os.LookupEnv("SOURCE_DATE_EPOCH"); !ok || v == "" {
		t.Fatalf("测试前置失败：SOURCE_DATE_EPOCH 未被设置")
	}
}

// ---- 用例 9: GoVersion 缺失（无 BuildInfo）时回退传入的 runtime version ----

func TestResolve_无BuildInfo时GoVersion回退到传入值(t *testing.T) {
	in := versionVars{Version: "dev"}
	// bi 为 nil 表示无 build info。
	got := resolve(in, nil, testGoVersion, testGOOS, testGOARCH)
	if got.GoVersion != testGoVersion {
		t.Errorf("GoVersion = %q, want %s（回退 runtime version）", got.GoVersion, testGoVersion)
	}
}

// ---- 用例 10: commit 超过 12 位时只在展示层截断（内部保留完整值） ----

func TestResolve_长Commit内部保留展示截断(t *testing.T) {
	in := versionVars{Version: "dev", Commit: fullRevision}
	got := resolve(in, nil, testGoVersion, testGOOS, testGOARCH)
	if got.Commit != fullRevision {
		t.Errorf("内部 Commit = %q, want 完整 %s", got.Commit, fullRevision)
	}
	d := got.Detail()
	if !strings.Contains(d, "commit: "+shortRevision+"\n") {
		t.Errorf("Detail 展示未截断为 12 位 %q, got %q", shortRevision, d)
	}
	if strings.Contains(d, fullRevision) {
		t.Errorf("Detail 不应出现完整 revision, got %q", d)
	}
}

// ---- 用例 11: commit 少于 12 位时不切片越界（直接原样） ----

func TestResolve_短Commit不切片越界(t *testing.T) {
	short := "abc123"
	in := versionVars{Version: "dev", Commit: short}
	got := resolve(in, nil, testGoVersion, testGOOS, testGOARCH)
	if got.Commit != short {
		t.Errorf("Commit = %q, want %s", got.Commit, short)
	}
	lines := detailLines(t, got.Detail())
	if len(lines) != 5 {
		t.Fatalf("Detail 行数 = %d, want 5", len(lines))
	}
	if lines[1] != "commit: "+short {
		t.Errorf("commit 行 = %q, want commit: %s", lines[1], short)
	}
}

// ---- 用例 12: 全部元数据缺失（无注入、无 BuildInfo）时全降级 ----

func TestResolve_全部元数据缺失时降级(t *testing.T) {
	in := versionVars{Version: "dev"}
	got := resolve(in, nil, testGoVersion, testGOOS, testGOARCH)
	if got.Version != "dev" {
		t.Errorf("Version = %q, want dev", got.Version)
	}
	if got.Commit != "unknown" {
		t.Errorf("Commit = %q, want unknown", got.Commit)
	}
	if got.BuildTime != "unknown" {
		t.Errorf("BuildTime = %q, want unknown", got.BuildTime)
	}
}

// ---- 用例 13: Short() 严格一行并带末尾换行 ----

func TestShort_严格一行带末尾换行(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want string
	}{
		{"release版本", Info{Version: "v0.1.0"}, progName + " v0.1.0\n"},
		{"dev版本", Info{Version: "dev"}, progName + " dev\n"},
		{"空版本回退", Info{Version: ""}, progName + " dev\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.info.Short()
			if got != c.want {
				t.Errorf("Short = %q, want %q", got, c.want)
			}
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("Short 缺少末尾换行")
			}
			if strings.Count(got, "\n") != 1 {
				t.Errorf("Short 应严格一行，含 %d 个换行", strings.Count(got, "\n"))
			}
		})
	}
}

// ---- 用例 14: Detail() 严格五行并带末尾换行 ----

func TestDetail_严格五行带末尾换行(t *testing.T) {
	info := Info{
		Version:   "v0.1.0",
		Commit:    "59a8d55a1b2c",
		BuildTime: "2026-07-30T10:00:00Z",
		GoVersion: testGoVersion,
		GOOS:      testGOOS,
		GOARCH:    testGOARCH,
		Modified:  false,
	}
	got := info.Detail()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("Detail 缺少末尾换行")
	}
	lines := detailLines(t, got)
	if len(lines) != 5 {
		t.Fatalf("Detail 行数 = %d, want 5, content=%q", len(lines), got)
	}
	wantLines := []string{
		progName + " v0.1.0",
		"commit: 59a8d55a1b2c",
		"build_time: 2026-07-30T10:00:00Z",
		"go: " + testGoVersion,
		"platform: " + testGOOS + "/" + testGOARCH,
	}
	for i, want := range wantLines {
		if lines[i] != want {
			t.Errorf("第 %d 行 = %q, want %q", i+1, lines[i], want)
		}
	}
}

// ---- 用例 15: Modified=true 只改变 commit 行后缀，不增加第六行 ----

func TestDetail_Modified只改变Commit后缀不增加行数(t *testing.T) {
	info := Info{
		Version:   "v0.1.0",
		Commit:    "59a8d55a1b2c",
		BuildTime: "2026-07-30T10:00:00Z",
		GoVersion: testGoVersion,
		GOOS:      testGOOS,
		GOARCH:    testGOARCH,
		Modified:  true,
	}
	got := info.Detail()
	lines := detailLines(t, got)
	if len(lines) != 5 {
		t.Fatalf("Detail 行数 = %d, want 5（dirty 不应新增行）", len(lines))
	}
	if lines[1] != "commit: 59a8d55a1b2c-dirty" {
		t.Errorf("commit 行 = %q, want commit: 59a8d55a1b2c-dirty", lines[1])
	}
	// 其余行不应受 dirty 影响。
	if lines[0] != progName+" v0.1.0" {
		t.Errorf("标题行被改动 = %q", lines[0])
	}
}

// ---- 端到端契约：release 示例逐字一致 ----

func TestDetail_Release示例逐字一致(t *testing.T) {
	info := Info{
		Version:   "v0.1.0",
		Commit:    "59a8d55a1b2c",
		BuildTime: "2026-07-30T10:00:00Z",
		GoVersion: testGoVersion,
		GOOS:      testGOOS,
		GOARCH:    testGOARCH,
	}
	want := "token-usage v0.1.0\n" +
		"commit: 59a8d55a1b2c\n" +
		"build_time: 2026-07-30T10:00:00Z\n" +
		"go: " + testGoVersion + "\n" +
		"platform: " + testGOOS + "/" + testGOARCH + "\n"
	if got := info.Detail(); got != want {
		t.Errorf("Detail 与 release 契约不一致:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// ---- 端到端契约：降级示例逐字一致 ----

func TestDetail_降级示例逐字一致(t *testing.T) {
	info := Info{
		Version:   "dev",
		Commit:    "unknown",
		BuildTime: "unknown",
		GoVersion: testGoVersion,
		GOOS:      testGOOS,
		GOARCH:    testGOARCH,
	}
	want := "token-usage dev\n" +
		"commit: unknown\n" +
		"build_time: unknown\n" +
		"go: " + testGoVersion + "\n" +
		"platform: " + testGOOS + "/" + testGOARCH + "\n"
	if got := info.Detail(); got != want {
		t.Errorf("Detail 与降级契约不一致:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// ---- Short/Detail 的 commit 为 unknown 时不被截断也不追加 dirty ----

func TestDetail_UnknownCommit不被截断不追加Dirty(t *testing.T) {
	info := Info{
		Version:   "dev",
		Commit:    "unknown",
		BuildTime: "unknown",
		GoVersion: testGoVersion,
		GOOS:      testGOOS,
		GOARCH:    testGOARCH,
		Modified:  true, // 即使标记 dirty，unknown commit 不追加 -dirty
	}
	got := info.Detail()
	lines := detailLines(t, got)
	if len(lines) != 5 {
		t.Fatalf("Detail 行数 = %d, want 5", len(lines))
	}
	if lines[1] != "commit: unknown" {
		t.Errorf("unknown commit 行 = %q, want commit: unknown", lines[1])
	}
}

// ---- 未知 commit (unknown) 时 Modified 不应产生 -dirty ----

func TestResolve_无Revision但标记Modified不追加Dirty(t *testing.T) {
	in := versionVars{Version: "dev"}
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			setting("vcs.modified", "true"),
			// 注意：故意不提供 vcs.revision。
		},
	}
	got := resolve(in, bi, testGoVersion, testGOOS, testGOARCH)
	if got.Commit != "unknown" {
		t.Errorf("Commit = %q, want unknown", got.Commit)
	}
	if got.Modified {
		t.Errorf("无 revision 时 Modified 应回退为 false, got true")
	}
	d := got.Detail()
	if strings.Contains(d, "-dirty") {
		t.Errorf("unknown commit 不应出现 -dirty, got %q", d)
	}
}
