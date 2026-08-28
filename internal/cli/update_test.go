package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/update"
)

// stubUpdateService 实现 UpdateService，供 update 命令注入确定性结果，
// 避免 Cobra 测试触及真实网络或文件系统。所有方法记录调用次数与入参。
type stubUpdateService struct {
	checkResult update.CheckResult
	checkErr    error
	applyResult update.ApplyResult
	applyErr    error
	checkCalls  int
	applyCalls  int
	lastCheck   update.CheckOptions
	lastApply   update.ApplyOptions
}

func (s *stubUpdateService) Check(ctx context.Context, opts update.CheckOptions) (update.CheckResult, error) {
	s.checkCalls++
	s.lastCheck = opts
	return s.checkResult, s.checkErr
}

func (s *stubUpdateService) Apply(ctx context.Context, opts update.ApplyOptions) (update.ApplyResult, error) {
	s.applyCalls++
	s.lastApply = opts
	return s.applyResult, s.applyErr
}

// withStubUpdateService 替换 updateServiceFactory 返回固定 stub，并在测试结束恢复原工厂。
// 返回 stub 以便用例断言调用次数与入参。
func withStubUpdateService(t *testing.T, stub *stubUpdateService) {
	t.Helper()
	orig := updateServiceFactory
	updateServiceFactory = func(info buildinfo.Info, checkOnly bool) (UpdateService, error) {
		return stub, nil
	}
	t.Cleanup(func() { updateServiceFactory = orig })
}

// findUpdateCmd 在 root 子命令集合中查找 update 命令，便于用例直接断言 flag 声明。
// 未找到时返回 nil，由调用方决定是否 Fatal。
func findUpdateCmd(root *cobra.Command) *cobra.Command {
	for _, sub := range root.Commands() {
		if sub.Name() == "update" {
			return sub
		}
	}
	return nil
}

// ---- 命令结构 ----

// TestUpdateCmd_MountedAndVisible update 子命令须挂载到 root 且用户可见（非 Hidden）。
func TestUpdateCmd_MountedAndVisible(t *testing.T) {
	root := newRootCmd(fixedInfo)
	sub := findUpdateCmd(root)
	if sub == nil {
		t.Fatal("root 应挂载 update 子命令")
	}
	if sub.Hidden {
		t.Error("update 子命令应为用户可见（非 Hidden）")
	}
}

// TestUpdateCmd_NoArgs update 必须声明 cobra.NoArgs，拒绝多余位置参数。
func TestUpdateCmd_NoArgs(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "extra-arg"})

	if err := root.Execute(); err == nil {
		t.Fatal("多余参数应被 NoArgs 拒绝并返回错误")
	}
}

// TestUpdateCmd_FlagsDefined update 必须定义 --check 与 --version 两个 flag，类型正确。
func TestUpdateCmd_FlagsDefined(t *testing.T) {
	root := newRootCmd(fixedInfo)
	sub := findUpdateCmd(root)
	if sub == nil {
		t.Fatal("未找到 update 子命令")
	}
	checkFlag := sub.Flag("check")
	if checkFlag == nil {
		t.Fatal("update 必须定义 --check flag")
	}
	if checkFlag.Value.Type() != "bool" {
		t.Errorf("--check 应为 bool 类型，实际 %q", checkFlag.Value.Type())
	}
	versionFlag := sub.Flag("version")
	if versionFlag == nil {
		t.Fatal("update 必须定义 --version flag")
	}
	if versionFlag.Value.Type() != "string" {
		t.Errorf("--version 应为 string 类型，实际 %q", versionFlag.Value.Type())
	}
}

// ---- --version 校验 ----

// TestUpdateCmd_InvalidVersionFlagRejectedBeforeNetwork 非法 --version 必须在任何
// 工厂/网络调用之前被拒绝：工厂不被调用，错误信息清晰，写 stderr。
func TestUpdateCmd_InvalidVersionFlagRejectedBeforeNetwork(t *testing.T) {
	// 工厂注入一个一旦被调用即失败的 stub，断言命令解析层不进入工厂。
	stub := &stubUpdateService{checkErr: errors.New("factory 不应被调用")}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--version", "not-a-tag"})

	err := root.Execute()
	if err == nil {
		t.Fatal("非法 --version 应返回错误")
	}
	if stub.checkCalls != 0 || stub.applyCalls != 0 {
		t.Errorf("非法 --version 不应触发工厂调用，check=%d apply=%d", stub.checkCalls, stub.applyCalls)
	}
	if !strings.Contains(errOut.String(), "version") && !strings.Contains(strings.ToLower(err.Error()), "version") {
		t.Errorf("错误信息应明确指向 version 校验失败，got err=%v stderr=%q", err, errOut.String())
	}
}

// TestUpdateCmd_InvalidVersionNoLeadingZeros 前导零的版本号应被拒绝。
func TestUpdateCmd_InvalidVersionNoLeadingZeros(t *testing.T) {
	stub := &stubUpdateService{}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--version", "v01.2.3"})

	if err := root.Execute(); err == nil {
		t.Fatal("含前导零的版本号应被拒绝")
	}
	if stub.checkCalls != 0 || stub.applyCalls != 0 {
		t.Errorf("非法版本号不应触发工厂调用，check=%d apply=%d", stub.checkCalls, stub.applyCalls)
	}
}

// TestUpdateCmd_ValidVersionFlagAccepted 合法 --version 通过解析并传给服务。
func TestUpdateCmd_ValidVersionFlagAccepted(t *testing.T) {
	stub := &stubUpdateService{
		checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check", "--version", "v0.2.0"})

	if err := root.Execute(); err != nil {
		t.Fatalf("合法 --version 应通过，got %v", err)
	}
	if stub.checkCalls != 1 {
		t.Errorf("check 应被调用 1 次，实际 %d", stub.checkCalls)
	}
	if stub.lastCheck.TargetTag != "v0.2.0" {
		t.Errorf("TargetTag 应为 v0.2.0，实际 %q", stub.lastCheck.TargetTag)
	}
}

// ---- --check 路径 ----

// TestUpdateCmd_CheckOnlyAlreadyLatest --check 且无更新：stdout 提示已是最新，退出 0。
func TestUpdateCmd_CheckOnlyAlreadyLatest(t *testing.T) {
	stub := &stubUpdateService{
		checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0", UpdateAvailable: false},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--check 无更新应退出 0，got %v", err)
	}
	if stub.applyCalls != 0 {
		t.Errorf("--check 路径不应调用 Apply，实际 %d", stub.applyCalls)
	}
	if !strings.Contains(out.String(), "已是最新") {
		t.Errorf("stdout 应提示已是最新，实际 %q", out.String())
	}
}

// TestUpdateCmd_CheckOnlyUpdateAvailable --check 且发现新版本：stdout 提示发现可更新版本。
func TestUpdateCmd_CheckOnlyUpdateAvailable(t *testing.T) {
	stub := &stubUpdateService{
		checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--check 发现更新应退出 0，got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "可更新") {
		t.Errorf("stdout 应提示发现可更新版本，实际 %q", got)
	}
	if !strings.Contains(got, "v0.1.0") || !strings.Contains(got, "v0.2.0") {
		t.Errorf("stdout 应同时含当前与目标版本号，实际 %q", got)
	}
}

// TestUpdateCmd_CheckOnlyErrorToStderr --check 查询失败：错误写 stderr 并返回非 0。
func TestUpdateCmd_CheckOnlyErrorToStderr(t *testing.T) {
	stub := &stubUpdateService{
		checkErr: errors.New("查询失败 boom"),
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check"})

	if err := root.Execute(); err == nil {
		t.Fatal("查询失败应返回错误")
	}
	if !strings.Contains(errOut.String(), "查询失败") {
		t.Errorf("错误应写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_CheckVersionNotFoundFails 显式指定不存在的 tag 不能被脚本当作成功。
func TestUpdateCmd_CheckVersionNotFoundFails(t *testing.T) {
	stub := &stubUpdateService{
		checkResult: update.CheckResult{VersionNotFound: true},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check", "--version", "v0.2.0"})

	err := root.Execute()
	if err == nil {
		t.Fatal("指定不存在版本应返回错误")
	}
	if !errors.Is(err, errRequestedUpdateVersionMissing) {
		t.Errorf("err=%v，应保留 VersionNotFound 根因", err)
	}
	if !strings.Contains(out.String(), "不存在") {
		t.Errorf("stdout 应说明指定版本不存在，实际 %q", out.String())
	}
}

// TestUpdateCmd_ResultErrorsSuppressUsage 确认四类领域结果错误都不输出 Cobra usage。
// Cobra 会把 usage 写到 SetOut 指定的 writer，因此 stdout 和 stderr 必须同时断言。
func TestUpdateCmd_ResultErrorsSuppressUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		stub *stubUpdateService
		want error
	}{
		{
			name: "指定版本不存在",
			args: []string{"update", "--check", "--version", "v0.2.0"},
			stub: &stubUpdateService{checkResult: update.CheckResult{VersionNotFound: true}},
			want: errRequestedUpdateVersionMissing,
		},
		{
			name: "来源不可信",
			args: []string{"update"},
			stub: &stubUpdateService{applyResult: update.ApplyResult{
				CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
				ProvenanceChecked: true,
			}},
			want: errUpdateSourceUntrusted,
		},
		{
			name: "校验被拒绝",
			args: []string{"update"},
			stub: &stubUpdateService{applyResult: update.ApplyResult{
				CheckResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
				Reason:      "checksum 不匹配",
			}},
			want: errUpdateVerificationFailed,
		},
		{
			name: "安装未完成",
			args: []string{"update"},
			stub: &stubUpdateService{applyResult: update.ApplyResult{
				CheckResult:    update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
				ReadyToInstall: true,
			}},
			want: errUpdateIncomplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withStubUpdateService(t, tt.stub)
			root := newRootCmd(fixedInfo)
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs(tt.args)

			err := root.Execute()
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v，应保留 %v", err, tt.want)
			}
			if strings.Contains(out.String(), "Usage:") || strings.Contains(errOut.String(), "Usage:") {
				t.Errorf("领域结果错误不应输出完整 usage，stdout=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

// ---- 非 check 路径（Apply）----

// TestUpdateCmd_ApplyAlreadyLatest 默认（无 --check）无更新：提示已是最新，退出 0。
func TestUpdateCmd_ApplyAlreadyLatest(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0", UpdateAvailable: false},
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("无更新应退出 0，got %v", err)
	}
	if stub.checkCalls != 0 {
		t.Errorf("非 --check 路径不应调用 Check，实际 %d", stub.checkCalls)
	}
	if !strings.Contains(out.String(), "已是最新") {
		t.Errorf("stdout 应提示已是最新，实际 %q", out.String())
	}
}

// TestUpdateCmd_ApplyInstalledSuccessfully 安装成功：提示已更新并恢复 daemon。
func TestUpdateCmd_ApplyInstalledSuccessfully(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: true,
			ReadyToInstall:    true,
			Installed:         true,
			BinaryPath:        "/usr/local/bin/token-usage",
			TargetAsset:       "token-usage-darwin-arm64",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("安装成功应退出 0，got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "已更新") {
		t.Errorf("stdout 应提示已更新，实际 %q", got)
	}
}

// TestUpdateCmd_ApplyRecoveredNewInstalledSucceeds 状态 1 恢复表示此前替换已经完成，
// CLI 应明确说明恢复完成并以成功退出，不能误报自动更新尚未完成。
func TestUpdateCmd_ApplyRecoveredNewInstalledSucceeds(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			Recovered:     true,
			RecoveryState: update.RecoveryStateNewInstalled,
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("新版本恢复完成应退出 0，got %v", err)
	}
	if !strings.Contains(out.String(), "已恢复完成") {
		t.Errorf("stdout 应说明恢复完成，实际 %q", out.String())
	}
	if strings.Contains(out.String(), "尚未完成") {
		t.Errorf("恢复完成不应误报未完成，stdout=%q", out.String())
	}
}

// TestUpdateCmd_ApplyRecoveredOldVersionNeedsRetry 状态 2/3 的恢复让系统回到旧版本；
// 命令须说明可重试并返回非 0，避免脚本误判目标版本已经安装。
func TestUpdateCmd_ApplyRecoveredOldVersionNeedsRetry(t *testing.T) {
	for _, state := range []update.RecoveryState{
		update.RecoveryStateOldIntact,
		update.RecoveryStateOldRestored,
	} {
		t.Run(string(state), func(t *testing.T) {
			stub := &stubUpdateService{
				applyResult: update.ApplyResult{Recovered: true, RecoveryState: state},
			}
			withStubUpdateService(t, stub)

			root := newRootCmd(fixedInfo)
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs([]string{"update"})

			err := root.Execute()
			if !errors.Is(err, errUpdateIncomplete) {
				t.Fatalf("err=%v，应保留未完成更新根因", err)
			}
			if !strings.Contains(out.String(), "恢复到旧版本") || !strings.Contains(out.String(), "重新运行") {
				t.Errorf("stdout 应说明已恢复且可重试，实际 %q", out.String())
			}
		})
	}
}

// TestUpdateCmd_ApplyUntrustedSource 来源不可信：提示无法安全覆盖，给人工安装指引。
func TestUpdateCmd_ApplyUntrustedSource(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			Reason:            "当前二进制 hash 与官方资产不一致",
			BinaryPath:        "/usr/local/bin/token-usage",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("来源不可信应返回错误，避免自动化调用误判成功")
	}
	if !errors.Is(err, errUpdateSourceUntrusted) {
		t.Errorf("err=%v，应保留来源不可信根因", err)
	}
	got := out.String()
	if !strings.Contains(got, "无法安全覆盖") && !strings.Contains(got, "手动安装") {
		t.Errorf("stdout 应提示无法安全覆盖并给人工安装指引，实际 %q", got)
	}
	if strings.Contains(got, "当前二进制 hash") {
		t.Errorf("来源校验详情应写 stderr，stdout=%q", got)
	}
	if !strings.Contains(errOut.String(), "当前二进制 hash") {
		t.Errorf("来源校验详情应写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_ApplyNoStableReleaseSucceeds 没有稳定版是可预期的正常结果。
func TestUpdateCmd_ApplyNoStableReleaseSucceeds(t *testing.T) {
	stub := &stubUpdateService{applyResult: update.ApplyResult{CheckResult: update.CheckResult{NoStableRelease: true}}}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("无稳定 Release 应退出 0，got %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("正常结果不应写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_ApplyVersionNotFoundFails 显式 tag 缺失必须返回非 0。
func TestUpdateCmd_ApplyVersionNotFoundFails(t *testing.T) {
	stub := &stubUpdateService{applyResult: update.ApplyResult{CheckResult: update.CheckResult{VersionNotFound: true}}}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("指定不存在版本应返回错误")
	}
	if !errors.Is(err, errRequestedUpdateVersionMissing) {
		t.Errorf("err=%v，应保留 VersionNotFound 根因", err)
	}
	if !strings.Contains(out.String(), "不存在") {
		t.Errorf("stdout 应说明指定版本不存在，实际 %q", out.String())
	}
}

// TestUpdateCmd_ApplyErrorToStderr Apply 真实失败：错误写 stderr 并返回非 0。
func TestUpdateCmd_ApplyErrorToStderr(t *testing.T) {
	stub := &stubUpdateService{
		applyErr: errors.New("下载失败 boom"),
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err == nil {
		t.Fatal("真实失败应返回错误")
	}
	if !strings.Contains(errOut.String(), "下载失败") {
		t.Errorf("错误应写 stderr，实际 %q", errOut.String())
	}
}

// ---- --check 与 --version 组合 ----

// TestUpdateCmd_CheckAndVersionCombinable --check 与 --version 可组合，TargetTag 正确传递。
func TestUpdateCmd_CheckAndVersionCombinable(t *testing.T) {
	stub := &stubUpdateService{
		checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0-rc.1", UpdateAvailable: true},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check", "--version", "v0.2.0-rc.1"})

	if err := root.Execute(); err != nil {
		t.Fatalf("组合应成功，got %v", err)
	}
	if stub.checkCalls != 1 || stub.applyCalls != 0 {
		t.Errorf("应只调用 Check，check=%d apply=%d", stub.checkCalls, stub.applyCalls)
	}
	if stub.lastCheck.TargetTag != "v0.2.0-rc.1" {
		t.Errorf("TargetTag 应为 v0.2.0-rc.1，实际 %q", stub.lastCheck.TargetTag)
	}
}

// ---- root --version 与 update --version 不冲突 ----

// TestUpdateCmd_RootVersionFlagNotConflicting root 的 --version flag 与 update 的 --version flag
// 各自独立工作：root --version 输出根版本，update --version=<tag> 把 tag 传给服务。
func TestUpdateCmd_RootVersionFlagNotConflicting(t *testing.T) {
	// root --version 仍输出固定版本号。
	rootA := newRootCmd(fixedInfo)
	var outA bytes.Buffer
	rootA.SetOut(&outA)
	rootA.SetErr(&outA)
	rootA.SetArgs([]string{"--version"})
	if err := rootA.Execute(); err != nil {
		t.Fatalf("root --version 执行失败: %v", err)
	}
	if got := outA.String(); got != "token-usage v0.1.0\n" {
		t.Errorf("root --version 输出应为 token-usage v0.1.0，实际 %q", got)
	}

	// update --version=<tag> 不被 root 解析。
	stub := &stubUpdateService{
		checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: false},
	}
	withStubUpdateService(t, stub)
	rootB := newRootCmd(fixedInfo)
	var outB bytes.Buffer
	rootB.SetOut(&outB)
	rootB.SetErr(&bytes.Buffer{})
	rootB.SetArgs([]string{"update", "--check", "--version", "v0.2.0"})
	if err := rootB.Execute(); err != nil {
		t.Fatalf("update --version=v0.2.0 执行失败: %v", err)
	}
	if stub.lastCheck.TargetTag != "v0.2.0" {
		t.Errorf("update --version=v0.2.0 应把 v0.2.0 传给服务，实际 %q", stub.lastCheck.TargetTag)
	}
}

// ---- --check 不创建 control.Manager（不创建配置目录）----

// TestUpdateCmd_CheckOnlyNeverCreatesControlManager --check 路径的工厂入参 checkOnly=true，
// 工厂据此构造不含 control.Manager 的服务；断言工厂收到 checkOnly=true 标记。
func TestUpdateCmd_CheckOnlyNeverCreatesControlManager(t *testing.T) {
	orig := updateServiceFactory
	var seenCheckOnly bool
	updateServiceFactory = func(info buildinfo.Info, checkOnly bool) (UpdateService, error) {
		seenCheckOnly = checkOnly
		return &stubUpdateService{
			checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0"},
		}, nil
	}
	t.Cleanup(func() { updateServiceFactory = orig })

	root := newRootCmd(fixedInfo)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update", "--check"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--check 应退出 0，got %v", err)
	}
	if !seenCheckOnly {
		t.Error("--check 路径应向工厂传入 checkOnly=true，使工厂不构造 control.Manager（避免创建配置目录）")
	}
}

// ---- 帮助与未知 flag ----

// TestUpdateCmd_HelpListsCommand update --help 能正常执行。
func TestUpdateCmd_HelpListsCommand(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"update", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("update --help 不应失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "update") {
		t.Errorf("update --help 应提及 update 用法，实际 %q", got)
	}
	if !strings.Contains(got, "--check") {
		t.Errorf("update --help 应列出 --check flag，实际 %q", got)
	}
}

// TestUpdateCmd_UnknownFlagFails 未知 flag 应被拒绝。
func TestUpdateCmd_UnknownFlagFails(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--no-such-flag"})

	if err := root.Execute(); err == nil {
		t.Fatal("未知 flag 应被拒绝并返回错误")
	}
}

// ---- Deferred 与未完成安装分支 ----

// TestUpdateCmd_ApplyDeferredNotInstalled Windows helper 已接管替换时是非错误的「已排队」状态。
// stdout 必须提示用户稍后验证最终版本，绝不谎称已完成；stderr 应为空，退出 0。
func TestUpdateCmd_ApplyDeferredNotInstalled(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: true,
			ReadyToInstall:    true,
			Installed:         false,
			Deferred:          true,
			BinaryPath:        "/usr/local/bin/token-usage",
			TargetAsset:       "token-usage-darwin-arm64",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Deferred 是非错误的排队状态，应退出 0，got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "后台替换已排队") {
		t.Errorf("stdout 应明确说明后台替换已排队，实际 %q", got)
	}
	if !strings.Contains(got, "确认最终版本") {
		t.Errorf("stdout 应提示用户稍后确认最终版本，实际 %q", got)
	}
	// 排队状态不是错误：不应写 stderr。
	if errOut.Len() != 0 {
		t.Errorf("排队状态不应写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_ApplyReadyButNotCompletedFails ReadyToInstall 但既未安装也未确认 helper
// 接管时，不能被当作成功。
func TestUpdateCmd_ApplyReadyButNotCompletedFails(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: true,
			ReadyToInstall:    true,
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("未完成安装应返回错误")
	}
	if !errors.Is(err, errUpdateIncomplete) {
		t.Errorf("err=%v，应保留未完成安装根因", err)
	}
	if !strings.Contains(out.String(), "尚未完成") {
		t.Errorf("stdout 应说明自动更新未完成，实际 %q", out.String())
	}
}

// TestUpdateCmd_ApplyVerificationFailureFails 校验或 stage 探针拒绝时返回非 0，
// 同时把底层原因写 stderr。
func TestUpdateCmd_ApplyVerificationFailureFails(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			Reason:      "SHA256 不匹配",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("校验拒绝应返回错误")
	}
	if !errors.Is(err, errUpdateVerificationFailed) {
		t.Errorf("err=%v，应保留校验失败根因", err)
	}
	if strings.Contains(out.String(), "SHA256 不匹配") {
		t.Errorf("校验详情应写 stderr，stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "SHA256 不匹配") {
		t.Errorf("校验详情应写 stderr，实际 %q", errOut.String())
	}
}

// ---- --check 路径绝不创建 control.Manager（不创建配置目录）----

// TestUpdateCmd_CheckOnlyRealFactorySkipsControlManager 对真实 defaultUpdateServiceFactory
// 调用 checkOnly=true，断言不创建 ~/.token-usage 配置目录。
// control.NewManager(home) 在构造时会 MkdirAll(<home>/.token-usage)，
// 若 --check 路径误调 buildUpdateControlManager，目录会被创建，本测试立即失败。
// 用临时 HOME 隔离，不触真实用户环境；不替换工厂，直接验证生产装配路径的安全属性。
func TestUpdateCmd_CheckOnlyRealFactorySkipsControlManager(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // Windows 上 os.UserHomeDir 读 USERPROFILE

	// 直接调用真实工厂（不替换 updateServiceFactory），验证 checkOnly=true 时
	// 不构造 control.Manager。factory 仅构造 Service struct，不应触网或创建目录。
	_, err := defaultUpdateServiceFactory(fixedInfo, true)
	if err != nil {
		t.Fatalf("checkOnly=true 工厂应成功构造 Service（不触网），got %v", err)
	}

	// 核心安全断言：~/.token-usage 目录必须不存在。
	// 若 buildUpdateControlManager 被调用，control.NewManager 会 MkdirAll 该目录。
	configDir := filepath.Join(tmpHome, ".token-usage")
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Errorf("--check 路径不应创建配置目录 %q（不应构造 control.Manager），stat err=%v", configDir, err)
	}
}

// TestUpdateCmd_ApplyRealFactoryCreatesControlManager 对照测试：checkOnly=false（Apply 路径）
// 的真实工厂确实构造 control.Manager，从而创建 ~/.token-usage 目录。
// 与上一用例共同确认「只有 Apply 路径才触碰 control.Manager」的边界正确。
func TestUpdateCmd_ApplyRealFactoryCreatesControlManager(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	_, err := defaultUpdateServiceFactory(fixedInfo, false)
	if err != nil {
		t.Fatalf("checkOnly=false 工厂应成功构造完整 Service，got %v", err)
	}

	configDir := filepath.Join(tmpHome, ".token-usage")
	if info, err := os.Stat(configDir); err != nil {
		t.Errorf("Apply 路径应构造 control.Manager 并创建配置目录 %q，stat err=%v", configDir, err)
	} else if !info.IsDir() {
		t.Errorf("配置路径 %q 应为目录，实际 mode %s", configDir, info.Mode())
	}
}

// TestUpdateCmd_ApplyRealFactoryWiresVersionProbe 真实 Apply 工厂必须装配生产
// stage --version 探针；否则 Service 会静默跳过发布物版本二次校验。
func TestUpdateCmd_ApplyRealFactoryWiresVersionProbe(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	service, err := defaultUpdateServiceFactory(fixedInfo, false)
	if err != nil {
		t.Fatalf("defaultUpdateServiceFactory: %v", err)
	}
	svc, ok := service.(*update.Service)
	if !ok {
		t.Fatalf("生产工厂返回 %T，want *update.Service", service)
	}
	if svc.VersionProbe == nil {
		t.Fatal("Apply 路径必须注入生产 VersionProbe")
	}
}

// ---- --force 受控出口（渲染分流 / sentinel / flag 合同）----

// TestUpdateCmd_ForceFlagDefined update 必须定义 --force bool flag。
func TestUpdateCmd_ForceFlagDefined(t *testing.T) {
	root := newRootCmd(fixedInfo)
	sub := findUpdateCmd(root)
	if sub == nil {
		t.Fatal("未找到 update 子命令")
	}
	forceFlag := sub.Flag("force")
	if forceFlag == nil {
		t.Fatal("update 必须定义 --force flag")
	}
	if forceFlag.Value.Type() != "bool" {
		t.Errorf("--force 应为 bool 类型，实际 %q", forceFlag.Value.Type())
	}
}

// TestUpdateCmd_ForcePassedToApply --force 被解析并传入 ApplyOptions.Force。
func TestUpdateCmd_ForcePassedToApply(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "dev", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ProvenanceForced:  true,
			Installed:         true,
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update", "--force"})

	if err := root.Execute(); err != nil {
		t.Fatalf("err: %v", err)
	}
	if stub.lastApply.Force != true {
		t.Errorf("ApplyOptions.Force 应为 true，实际 %+v", stub.lastApply)
	}
}

// TestUpdateCmd_NoForceByDefault 默认不带 --force 时 ApplyOptions.Force=false（默认行为不变）。
func TestUpdateCmd_NoForceByDefault(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0", UpdateAvailable: false},
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("err: %v", err)
	}
	if stub.lastApply.Force != false {
		t.Errorf("默认 ApplyOptions.Force 应为 false，实际 %+v", stub.lastApply)
	}
}

// TestUpdateCmd_CheckForceComboRejected --check --force 组合显式拒绝（双语 sentinel 错误），
// 不触发任何服务调用（Check 与 Apply 均不调用）。
func TestUpdateCmd_CheckForceComboRejected(t *testing.T) {
	stub := &stubUpdateService{}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update", "--check", "--force"})

	err := root.Execute()
	if err == nil {
		t.Fatal("--check --force 组合应显式拒绝")
	}
	if !errors.Is(err, errCheckForceCombination) {
		t.Errorf("err=%v，应保留组合拒绝 sentinel", err)
	}
	if stub.checkCalls != 0 || stub.applyCalls != 0 {
		t.Errorf("组合拒绝不应触发服务调用，check=%d apply=%d", stub.checkCalls, stub.applyCalls)
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Errorf("领域错误不应输出 usage，stdout=%q", out.String())
	}
}

// TestUpdateCmd_ApplyForceEligibleUntrustedPromptsForce 来源不可信但可 force（hash 失配）
// 且未 force：输出含 --force 出口的新标题，返回 errUpdateForceRequired（非 0 语义不变），
// 不再出现「无法安全覆盖」的表述。
func TestUpdateCmd_ApplyForceEligibleUntrustedPromptsForce(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ForceEligible:     true,
			Reason:            "当前二进制 hash 与官方资产不一致",
			BinaryPath:        "/usr/local/bin/token-usage",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("拒绝更新应返回非 0")
	}
	if !errors.Is(err, errUpdateForceRequired) {
		t.Errorf("err=%v，应保留 errUpdateForceRequired 根因", err)
	}
	if errors.Is(err, errUpdateSourceUntrusted) {
		t.Error("两类 sentinel 不得互相混淆")
	}
	got := out.String()
	if !strings.Contains(got, "--force") {
		t.Errorf("stdout 应提示 --force 出口，实际 %q", got)
	}
	if strings.Contains(got, "无法安全覆盖") {
		t.Errorf("可 force 场景不得使用「无法安全覆盖」标题，stdout=%q", got)
	}
	if !strings.Contains(errOut.String(), "当前二进制 hash") {
		t.Errorf("Reason 行应照旧写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_ApplyNotForceEligibleKeepsSentinel 不可 force 的 untrusted（如 symlink）
// 维持现行标题与 errUpdateSourceUntrusted。
func TestUpdateCmd_ApplyNotForceEligibleKeepsSentinel(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ForceEligible:     false,
			Reason:            "当前可执行文件是符号链接",
			BinaryPath:        "/usr/local/bin/token-usage",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("来源不可信应返回非 0")
	}
	if !errors.Is(err, errUpdateSourceUntrusted) {
		t.Errorf("err=%v，应保留 errUpdateSourceUntrusted 根因", err)
	}
	if errors.Is(err, errUpdateForceRequired) {
		t.Error("两类 sentinel 不得互相混淆")
	}
	got := out.String()
	if !strings.Contains(got, "无法安全覆盖") {
		t.Errorf("不可 force 场景应维持现行标题，实际 %q", got)
	}
	if strings.Contains(got, "--force") {
		t.Errorf("不可 force 场景不应提示 --force 出口，stdout=%q", got)
	}
}

// TestUpdateCmd_SentinelsNotContained 两个来源分流 sentinel 的错误文本互不包含，
// 防止脚本侧用 strings.Contains 区分时误判。
func TestUpdateCmd_SentinelsNotContained(t *testing.T) {
	a := errUpdateForceRequired.Error()
	b := errUpdateSourceUntrusted.Error()
	if strings.Contains(a, b) || strings.Contains(b, a) {
		t.Errorf("sentinel 错误文本不得互相包含：%q vs %q", a, b)
	}
}

// TestUpdateCmd_ApplyForcedInstalledSucceeds force 安装成功（POSIX Installed）：
// 输出注明 --force 强制覆盖的成功提示，退出 0——不得落入「来源不可信」非零分支（P0 回归）。
func TestUpdateCmd_ApplyForcedInstalledSucceeds(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "dev", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ProvenanceForced:  true,
			ReadyToInstall:    true,
			Installed:         true,
			ForceEligible:     true,
			BinaryPath:        "/usr/local/bin/token-usage",
			TargetAsset:       "token-usage-darwin-arm64",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("force 安装成功应退出 0，got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "已更新") || !strings.Contains(got, "--force") {
		t.Errorf("stdout 应注明 --force 强制覆盖的成功提示，实际 %q", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("成功状态不应写 stderr，实际 %q", errOut.String())
	}
}

// TestUpdateCmd_ApplyForcedDeferredSucceeds force 下 Windows helper 排队：
// 输出注明 --force 的排队提示，退出 0。
func TestUpdateCmd_ApplyForcedDeferredSucceeds(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ProvenanceForced:  true,
			ReadyToInstall:    true,
			Deferred:          true,
			ForceEligible:     true,
			TargetAsset:       "token-usage-windows-amd64.exe",
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("force 排队状态应退出 0，got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "后台替换已排队") || !strings.Contains(got, "--force") {
		t.Errorf("stdout 应注明 --force 的排队提示，实际 %q", got)
	}
}

// TestUpdateCmd_ApplyForcedIncompleteFails forced 但安装未完成：掉入既有失败分支，
// 返回非 0，不再以「来源不可信」拒绝。
func TestUpdateCmd_ApplyForcedIncompleteFails(t *testing.T) {
	stub := &stubUpdateService{
		applyResult: update.ApplyResult{
			CheckResult:       update.CheckResult{CurrentTag: "dev", TargetTag: "v0.2.0", UpdateAvailable: true},
			ProvenanceChecked: true,
			ProvenanceTrusted: false,
			ProvenanceForced:  true,
			ReadyToInstall:    true,
			ForceEligible:     true,
		},
	}
	withStubUpdateService(t, stub)

	root := newRootCmd(fixedInfo)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"update"})

	err := root.Execute()
	if err == nil {
		t.Fatal("安装未完成应返回非 0")
	}
	if !errors.Is(err, errUpdateIncomplete) {
		t.Errorf("err=%v，应掉入未完成分支而非来源不可信", err)
	}
}

// TestUpdateCmd_LongHelpDocumentsForce Long 帮助应包含 --force 用法行与豁免边界说明。
func TestUpdateCmd_LongHelpDocumentsForce(t *testing.T) {
	root := newRootCmd(fixedInfo)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"update", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("update --help 不应失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "update --force") {
		t.Errorf("帮助应含 --force 用法行，实际 %q", got)
	}
	for _, want := range []string{"re-signed", "dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("帮助应说明豁免边界（%s），实际 %q", want, got)
		}
	}
	if !strings.Contains(got, "符号链接") && !strings.Contains(got, "symlink") {
		t.Errorf("帮助应说明软链不可被 force，实际 %q", got)
	}
}
