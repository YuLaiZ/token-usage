// internal/service/service_darwin_test.go
//go:build darwin

package service

import (
	"strings"
	"testing"
)

func TestBuildPlist_ContainsRunArgs(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/usr/local/bin/token-usage", DataDir: "/data", Args: []string{"_run"}}
	s := buildPlist(opts)
	// 必须含完整 ProgramArguments（BinPath + _run，而非 run --daemon）
	if !strings.Contains(s, "/usr/local/bin/token-usage</string>") {
		t.Error("plist 应含 BinPath")
	}
	if !strings.Contains(s, "_run</string>") {
		t.Error("plist 应含 _run 参数")
	}
	if strings.Contains(s, "run</string>") && strings.Contains(s, "--daemon") {
		t.Error("plist 不应含旧版 run --daemon 参数")
	}
	if !strings.Contains(s, "<key>Label</key>") || !strings.Contains(s, "com.yulaiz.token-usage") {
		t.Error("plist 应含 Label")
	}
	if !strings.Contains(s, "<key>KeepAlive</key>") || !strings.Contains(s, "<key>Crashed</key>") {
		t.Error("plist 应含 KeepAlive.Crashed")
	}
	if !strings.Contains(s, "SuccessfulExit") {
		t.Error("plist 应含 SuccessfulExit=false")
	}
	if !strings.Contains(s, "launchd.out.log") || !strings.Contains(s, "launchd.err.log") {
		t.Error("plist 应含日志路径")
	}
	if !strings.Contains(s, "ThrottleStartInterval") {
		t.Error("plist 应含 ThrottleStartInterval")
	}
}

func TestParsePlistArgs_NewRun(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	data := []byte(buildPlist(opts))
	args, stdout, stderr, err := parsePlistArgs(data)
	if err != nil {
		t.Fatalf("parsePlistArgs err=%v", err)
	}
	want := []string{"/bin/tu", "_run"}
	if len(args) != 2 || args[0] != want[0] || args[1] != want[1] {
		t.Errorf("args=%v want %v", args, want)
	}
	if stdout != "/data/launchd.out.log" {
		t.Errorf("stdout=%q want /data/launchd.out.log", stdout)
	}
	if stderr != "/data/launchd.err.log" {
		t.Errorf("stderr=%q want /data/launchd.err.log", stderr)
	}
}

// 模拟旧版 plist（ProgramArguments 含 run --daemon）→ SpecMatches 应 false
func TestSpecMatchesPlist_LegacyRunDaemon_Drift(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	legacyArgs := []string{"/bin/tu", "run", "--daemon"}
	if specMatchesPlist(opts, legacyArgs, "/data/launchd.out.log", "/data/launchd.err.log") {
		t.Error("旧版 run --daemon 参数应判 SpecMatches=false")
	}
}

func TestSpecMatchesPlist_BinPathDrift(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/new/tu", DataDir: "/data", Args: []string{"_run"}}
	installed := []string{"/old/tu", "_run"}
	if specMatchesPlist(opts, installed, "/data/launchd.out.log", "/data/launchd.err.log") {
		t.Error("BinPath 变更应判 SpecMatches=false")
	}
}

func TestSpecMatchesPlist_LogPathDrift(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/newdata", Args: []string{"_run"}}
	installed := []string{"/bin/tu", "_run"}
	if specMatchesPlist(opts, installed, "/old/launchd.out.log", "/data/launchd.err.log") {
		t.Error("日志路径变更应判 SpecMatches=false")
	}
}

func TestSpecMatchesPlist_ExactMatch(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	installed := []string{"/bin/tu", "_run"}
	if !specMatchesPlist(opts, installed, "/data/launchd.out.log", "/data/launchd.err.log") {
		t.Error("完全一致应判 SpecMatches=true")
	}
}

func TestSpecMatchesPlist_StderrPathDrift(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	if specMatchesPlist(opts, []string{"/bin/tu", "_run"},
		"/data/launchd.out.log", "/old/launchd.err.log") {
		t.Error("stderr 日志路径漂移应判 SpecMatches=false")
	}
}

func TestDecodeStringArray_TruncatedXMLReturnsError(t *testing.T) {
	_, _, _, err := parsePlistArgs([]byte(
		`<plist><dict><key>ProgramArguments</key><array><string>/bin/tu</string>`))
	if err == nil {
		t.Fatal("截断的 plist 不得被当成完整定义")
	}
}

// specMatchesFromPlistBytes 是 Status「读 plist 字节算 SpecMatches」的纯函数封装，
// 供「job 已加载」与「job 未加载但 plist 存在」两个分支共用。
// 「plist 存在但 job 未加载」是用户 stop 后的良性停止状态——定义本身可能完全一致，
// 此时应返回 SpecMatches=true，让 syncWith 走 noop 分支，不视为漂移重启进程。

// 定义与 opts 完全一致 → SpecMatches=true（即便 job 未加载也不视为漂移）
func TestSpecMatchesFromPlistBytes_ExactMatch(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	data := []byte(buildPlist(opts))
	ok, err := specMatchesFromPlistBytes(opts, data)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !ok {
		t.Error("plist 内容与 opts 完全一致应判 SpecMatches=true（job 未加载不等于漂移）")
	}
}

// 定义漂移（BinPath 变了）→ SpecMatches=false
func TestSpecMatchesFromPlistBytes_BinPathDrift(t *testing.T) {
	installed := Options{Label: Label, BinPath: "/old/tu", DataDir: "/data", Args: []string{"_run"}}
	want := Options{Label: Label, BinPath: "/new/tu", DataDir: "/data", Args: []string{"_run"}}
	data := []byte(buildPlist(installed))
	ok, err := specMatchesFromPlistBytes(want, data)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ok {
		t.Error("BinPath 漂移应判 SpecMatches=false")
	}
}

// 损坏的 plist 字节（非 XML）→ parsePlistArgs 容错返回空 args，specMatchesPlist 判 false（长度不匹配）。
// 不报 err，调用方按 SpecMatches=false 处理（等同漂移）。
func TestSpecMatchesFromPlistBytes_InvalidXML(t *testing.T) {
	opts := Options{Label: Label, BinPath: "/bin/tu", DataDir: "/data", Args: []string{"_run"}}
	ok, _ := specMatchesFromPlistBytes(opts, []byte("not a plist"))
	if ok {
		t.Error("损坏的非 plist 文本应判 SpecMatches=false")
	}
}

// launchdManager_EnableDisableStatus_RoundTrip 验证纯 definition 契约：
// Enable/Disable/Status 只操作 plist 文件，不调用任何 launchctl（不 bootstrap/bootout）。
// 隔离 HOME 到 TempDir，确保不触碰真实 ~/Library/LaunchAgents。
func launchdRoundTripOpts(bin, data string) Options {
	return Options{Label: Label, BinPath: bin, DataDir: data, Args: []string{"_run"}}
}

// TestLaunchdManager_PureDefinitionRoundTrip 覆盖 false→true install、true→false remove、漂移修复。
func TestLaunchdManager_PureDefinitionRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := launchdManager{}

	// 初始：定义不存在
	st, err := m.Status(launchdRoundTripOpts("/bin/tu", "/data"))
	if err != nil {
		t.Fatalf("初始 Status err=%v", err)
	}
	if st.Exists {
		t.Error("初始 Exists 应为 false")
	}

	// Enable：写 plist（纯 definition，不 bootstrap）
	opts := launchdRoundTripOpts("/bin/tu", "/data")
	if err := m.Enable(opts); err != nil {
		t.Fatalf("Enable err=%v", err)
	}
	st, _ = m.Status(opts)
	if !st.Exists || !st.SpecMatches {
		t.Errorf("Enable 后应 Exists && SpecMatches，实际 %+v", st)
	}

	// 漂移：DataDir 变了 → SpecMatches=false（定义内容不一致）
	drifted := launchdRoundTripOpts("/bin/tu", "/newdata")
	st, _ = m.Status(drifted)
	if !st.Exists {
		t.Error("Exists 应仍为 true（plist 还在）")
	}
	if st.SpecMatches {
		t.Error("DataDir 漂移后 SpecMatches 应为 false")
	}

	// Disable：删 plist（纯 definition，不 bootout）
	if err := m.Disable(opts); err != nil {
		t.Fatalf("Disable err=%v", err)
	}
	st, _ = m.Status(opts)
	if st.Exists {
		t.Error("Disable 后 Exists 应为 false")
	}
}

// TestLaunchdManager_DisableIdempotent 删除不存在的 plist 不报错（幂等）。
func TestLaunchdManager_DisableIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := launchdManager{}
	if err := m.Disable(launchdRoundTripOpts("/bin/tu", "/data")); err != nil {
		t.Errorf("Disable 不存在的 plist 应幂等返回 nil，实际 err=%v", err)
	}
}
