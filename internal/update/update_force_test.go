package update

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// update_force_test.go 校验 update --force 受控出口（Service.Check / Service.Apply 联动）：
//   - Check 联动：dev + force 不再拒绝（CurrentTag='dev'、有目标即 UpdateAvailable）；
//     dev 非 force 判定不变，仅错误文本携带 `token-usage update --force` 提示；
//   - Apply 联动：hash 失配 + 非 force 仅传递 ForceEligible；force 且来源具备豁免资格
//     （hash-mismatch / dev-build 白名单）才继续安装（ProvenanceForced=true、
//     ProvenanceTrusted=false、consume/sweep 不执行）；白名单外与编程错误 force 不可救；
//   - Windows deferred 路径在 force 下同样成立（Deferred=true && ProvenanceForced=true）。
//
// 全部注入 fake，复用 makeService / makeInstallService / fixtureFileInfo 等既有夹具。

// TestCheck_DevForceAcceptsAnyTarget：Check 对 dev + force 放行——CurrentTag='dev'，
// 查询到合法目标即 UpdateAvailable=true（dev 无版本语义，不做版本序比较）。
func TestCheck_DevForceAcceptsAnyTarget(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "dev"

	got, err := svc.Check(context.Background(), CheckOptions{Force: true})
	if err != nil {
		t.Fatalf("dev + force 的 Check 应放行，err: %v", err)
	}
	if got.CurrentTag != "dev" {
		t.Fatalf("CurrentTag = %q, want dev", got.CurrentTag)
	}
	if got.TargetTag != "v0.2.0" {
		t.Fatalf("TargetTag = %q, want v0.2.0", got.TargetTag)
	}
	if !got.UpdateAvailable {
		t.Fatal("dev 无版本语义：查询到合法目标即应 UpdateAvailable=true")
	}
}

// TestCheck_DevNoForceErrorMentionsForce：dev 非 force 判定不变（Check 仍拒绝），
// 但错误文本更新为携带 `token-usage update --force` 出口提示。
func TestCheck_DevNoForceErrorMentionsForce(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "dev"

	_, err := svc.Check(context.Background(), CheckOptions{Force: false})
	if err == nil {
		t.Fatal("dev 非 force 应被 Check 拒绝")
	}
	if !strings.Contains(err.Error(), "token-usage update --force") {
		t.Fatalf("错误文本应携带 token-usage update --force 提示，got %v", err)
	}
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	if len(rc.fetches) != 0 {
		t.Fatalf("dev 拒绝发生在目标查询之前，fetches=%v", rc.fetches)
	}
}

// TestApply_HashMismatchNoForceEligiblePropagated：hash 失配 + 非 force → 现行为
// （不下载、返回人工指引路径），但 ApplyResult.ForceEligible=true 从豁免枚举传递，
// 渲染层据此输出 --force 出口标题；ProvenanceForced 保持 false。
func TestApply_HashMismatchNoForceEligiblePropagated(t *testing.T) {
	svc := makeService(t)
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(
		buildSumsBody("token-usage-darwin-arm64", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
	)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ProvenanceTrusted {
		t.Fatal("来源应不可信")
	}
	if !got.ForceEligible {
		t.Fatal("hash 失配应传递 ForceEligible=true（赋值与 force 无关）")
	}
	if got.ProvenanceForced {
		t.Fatal("未携带 force 不应置 ProvenanceForced")
	}
	if svc.downloaderUsed() {
		t.Fatal("非 force 的 untrusted 路径绝不下载")
	}
}

// TestApply_HashMismatchForceInstalls：hash 失配 + force → 走完整安装路径：
// ProvenanceForced=true 且 ProvenanceTrusted=false，日志按 hash-mismatch 类别记录，
// consume/sweep 不执行（trusted 专属），daemon 编排照常。
func TestApply_HashMismatchForceInstalls(t *testing.T) {
	svc, sess, _, installer, _ := makeInstallService(t, true)
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(
		buildSumsBody("token-usage-darwin-arm64", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
	)

	// 造一个遗留 result 文件：force 生效路径 trusted=false，consume 不得触碰。
	const nonce = "aabbccddaabbccddaabbccddaabbccdd"
	resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte(`{"success":true}`))
	t.Cleanup(func() { _ = removeRegularFile(resultPath) })

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("force 生效应完成安装，err: %v", err)
	}
	if !got.ProvenanceForced {
		t.Fatal("force 生效应置 ProvenanceForced=true")
	}
	if got.ProvenanceTrusted {
		t.Fatal("force 生效不谎报 trusted，ProvenanceTrusted 应保持 false")
	}
	if !got.Installed {
		t.Fatal("force 生效应走完整安装至 Installed=true")
	}
	if !got.ForceEligible {
		t.Fatal("hash 失配来源应 ForceEligible=true")
	}
	if !strings.Contains(logBuf.String(), "provenance hash mismatch (forced by --force); continuing install") {
		t.Errorf("日志应按 hash-mismatch 类别记录\n实际:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "dev build") {
		t.Errorf("hash 失配日志不得误报 dev 类别\n实际:\n%s", logBuf.String())
	}
	if _, err := os.Stat(resultPath); os.IsNotExist(err) {
		t.Error("force 生效路径 trusted=false，consume/sweep 不应执行（result 文件应保留）")
	}
	if sess.stopCalls != 1 || sess.startCalls != 1 {
		t.Errorf("daemon 编排照常：stop=%d start=%d, want 1/1", sess.stopCalls, sess.startCalls)
	}
	if len(installer.calls) != 1 {
		t.Errorf("应 Install 一次，calls=%d", len(installer.calls))
	}
}

// TestApply_DevForceFullChain：dev + force 全链——Check 放行（CurrentTag='dev'），
// provenance 置 dev-build 豁免，Apply 走完整安装至 Installed，日志按 dev 类别记录。
func TestApply_DevForceFullChain(t *testing.T) {
	svc, _, _, installer, _ := makeInstallService(t, true)
	svc.CurrentVersion = "dev"

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("dev + force 全链应完成安装，err: %v", err)
	}
	if got.CurrentTag != "dev" {
		t.Fatalf("CurrentTag = %q, want dev", got.CurrentTag)
	}
	if !got.UpdateAvailable {
		t.Fatal("dev + force 查询到合法目标即应有更新")
	}
	if !got.ProvenanceForced {
		t.Fatal("dev + force 应置 ProvenanceForced=true")
	}
	if got.ProvenanceTrusted {
		t.Fatal("dev + force 不谎报 trusted")
	}
	if !got.ForceEligible {
		t.Fatal("dev-build 应在白名单内，ForceEligible=true")
	}
	if !got.Installed {
		t.Fatal("dev + force 应走完整安装至 Installed=true")
	}
	if len(installer.calls) != 1 || installer.calls[0].oldBinPath == "" {
		t.Fatalf("应 Install 一次且 oldBinPath 为 provenance 解析出的当前二进制路径，calls=%+v", installer.calls)
	}
	if !strings.Contains(logBuf.String(), "provenance source unverifiable: dev build (forced by --force); continuing install") {
		t.Errorf("日志应按 dev 类别记录（不得误报 hash 语义）\n实际:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "hash mismatch") {
		t.Errorf("dev 从未发生 hash 比较，日志不得使用 hash 表述\n实际:\n%s", logBuf.String())
	}
}

// TestApply_DevNoForceCheckRejects：dev 非 force → Apply 在 Check 即返回 error，
// 错误文本含 `token-usage update --force` 提示（渲染分流之前唯一的 force 提示落点）。
func TestApply_DevNoForceCheckRejects(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "dev"

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatalf("dev 非 force 应被 Check 拒绝，got=%+v", got)
	}
	if !strings.Contains(err.Error(), "token-usage update --force") {
		t.Fatalf("错误文本应携带 token-usage update --force 提示，got %v", err)
	}
}

// TestApply_SymlinkForceStillRejected：白名单外（symlink）的 untrusted + force →
// force 不可救，仍返回人工指引路径（无 error 的领域结果，不下载、不安装）。
func TestApply_SymlinkForceStillRejected(t *testing.T) {
	svc := makeService(t)
	svc.ProvenanceDeps.Lstat.(*fakeLstat).infos[svc.binPathForTest] = fixtureFileInfo(t, "symlink")

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ProvenanceForced {
		t.Fatal("白名单外来源 force 不可救，不应置 ProvenanceForced")
	}
	if got.ForceEligible {
		t.Fatal("symlink 不应具豁免资格")
	}
	if svc.downloaderUsed() {
		t.Fatal("force 不可救的来源绝不下载")
	}
	if got.Installed {
		t.Fatal("force 不可救的来源绝不安装")
	}
}

// TestApply_ProvenanceErrorForceNotExempted：provenance 编程错误（deps 不完整）+ force
// → 仍返回 error（perr 不可被 force 豁免）。
func TestApply_ProvenanceErrorForceNotExempted(t *testing.T) {
	svc := makeService(t)
	svc.ProvenanceDeps.Executable = nil

	if _, err := svc.Apply(context.Background(), ApplyOptions{Force: true}); err == nil {
		t.Fatal("编程错误不可被 force 豁免，应返回 error")
	}
}

// TestApply_TrustedForceNormal：来源可信 + force → 正常路径不受影响
// （无豁免语义，ProvenanceForced=false、ForceEligible=false）。
func TestApply_TrustedForceNormal(t *testing.T) {
	svc, _, _, _, _ := makeInstallService(t, true)

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ProvenanceTrusted {
		t.Fatal("可信来源应保持 trusted")
	}
	if got.ProvenanceForced {
		t.Fatal("trusted + force 不涉及豁免，ProvenanceForced 应为 false")
	}
	if got.ForceEligible {
		t.Fatal("trusted 来源 ForceEligible 应为 false")
	}
	if !got.Installed {
		t.Fatal("可信路径应正常安装")
	}
}

// TestApply_WindowsGoosForceDeferred：goos=windows + hash 失配 + force + helper 排队
// → 完整走 Windows deferred 路径，Deferred=true && ProvenanceForced=true（评审 P0 回归）。
func TestApply_WindowsGoosForceDeferred(t *testing.T) {
	svc, _, _, installer, _ := makeInstallService(t, true)
	installer.deferred = true
	// 平台切换到 windows/amd64：provenance 与目标资产名都随之变化，
	// manifest 用 windows 资产的错误 hash 构造 hash 失配。
	svc.Goos = "windows"
	svc.Goarch = "amd64"
	svc.ProvenanceDeps.Goos = "windows"
	svc.ProvenanceDeps.Goarch = "amd64"
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(
		buildSumsBody("token-usage-windows-amd64.exe", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
	)

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ProvenanceForced {
		t.Fatal("force 生效应置 ProvenanceForced=true")
	}
	if !got.Deferred {
		t.Fatal("Windows helper 排队路径应 Deferred=true")
	}
	if got.Installed {
		t.Fatal("deferred 时 Installed 应为 false")
	}
	if !got.ForceEligible {
		t.Fatal("hash 失配应 ForceEligible=true")
	}
}

// TestApply_DevForceWindowsDeferred：dev + force 在 Windows 平台同样走 helper 排队
// （dev-build 豁免 × deferred 交叉场景）。
func TestApply_DevForceWindowsDeferred(t *testing.T) {
	svc, _, _, installer, _ := makeInstallService(t, true)
	installer.deferred = true
	svc.CurrentVersion = "dev"
	svc.Goos = "windows"
	svc.Goarch = "amd64"
	svc.ProvenanceDeps.Goos = "windows"
	svc.ProvenanceDeps.Goarch = "amd64"
	// dev + force 不查询 manifest；注入与否不影响结果，保持默认即可。

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ProvenanceForced || !got.Deferred {
		t.Fatalf("dev + force + windows 应 Deferred && ProvenanceForced，got %+v", got)
	}
}

// TestVerifyProvenance_PseudoVersionForceNotExempted：伪版本形态（非 dev、非正式 tag）
// + force → 仍被 ParseVersion 判 untrusted，无豁免资格（dev-build 仅限字面量 "dev"；
// 直接构建的伪版本经 buildinfo 归一为 dev 后才落进 dev 分支）。
func TestVerifyProvenance_PseudoVersionForceNotExempted(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	const pseudo = "v0.0.0-20260101000000-abcdefabcdef" // prerelease 段非 rc.N，ParseVersion 拒绝

	res, err := VerifyProvenance(context.Background(), deps, pseudo, rc, ProvenanceOptions{Force: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("伪版本 + force 应仍 untrusted")
	}
	if res.Exemption != "" {
		t.Fatalf("伪版本不可豁免，Exemption = %q", res.Exemption)
	}
	if len(rc.fetches) != 0 {
		t.Fatalf("ParseVersion 短路不应查询 Release，fetches=%v", rc.fetches)
	}
}

// TestApply_PseudoVersionForceCheckRejects：伪版本 + force → Apply 在 Check 即拒绝
// （非 dev 的非法 tag 不因 force 放行），不进入安装路径。
func TestApply_PseudoVersionForceCheckRejects(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "v0.0.0-20260101000000-abcdefabcdef"

	got, err := svc.Apply(context.Background(), ApplyOptions{Force: true})
	if err == nil {
		t.Fatalf("伪版本 + force 应被 Check 拒绝，got=%+v", got)
	}
	if got.ProvenanceForced {
		t.Fatal("拒绝路径不应置 ProvenanceForced")
	}
	if svc.downloaderUsed() {
		t.Fatal("拒绝路径不应下载")
	}
}
