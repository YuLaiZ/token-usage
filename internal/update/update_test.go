package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update_test.go 校验更新判定与编排骨架（Service.Check / Service.Apply）：
//   - Check 只做判定第 1/2 步（解析当前版本 + 目标 Release 比较），不写本地文件；
//   - Apply 在目标确有更新时做来源验证；不可信则只返回人工安装指引，绝不下载/覆盖。
//
// 全部注入 fake；网络由 fakeReleaseClient 承担，二进制与 manifest 由 fakes 提供。

// makeService 构造一组「当前 v0.1.0 已安装且可信」的 Service。
// 调用方可覆盖返回的字段以构造不同场景（如换版本、设 untrusted）。
func makeService(t *testing.T) *Service {
	t.Helper()
	bin := []byte("current-official-bin")
	deps, binPath, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", bin)
	withMatchingManifest(&deps, bin)
	// ReleaseClient 需能同时返回「当前 v0.1.0」与「目标（按用例覆盖）」两条查询。
	// 用 byTag 固定当前版本；目标版本默认指向 v0.2.0（latest / 显式 tag）。
	rc.byTag = map[string]*Release{
		"v0.1.0": makeCurrentRelease("v0.1.0"),
		"v0.2.0": makeCurrentRelease("v0.2.0"),
		"":       makeCurrentRelease("v0.2.0"), // latest
	}
	rc.release = makeCurrentRelease("v0.2.0")
	return &Service{
		CurrentVersion:    "v0.1.0",
		ReleaseClient:     rc,
		ProvenanceDeps:    deps,
		Goos:              deps.Goos,
		Goarch:            deps.Goarch,
		DownloadBase:      "https://example.invalid/download",
		ControlManager:    &fakeControlManager{},
		binPathForTest:    binPath,
		binContentForTest: bin,
	}
}

// setTarget 设置 Service 的目标 Release（覆盖 latest 与显式 tag）。
// 保留 byTag 中固定的当前版本，使 VerifyProvenance 仍能查到当前版本。
func setTarget(svc *Service, tag string) {
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	rel := makeCurrentRelease(tag)
	rc.release = rel
	if rc.byTag == nil {
		rc.byTag = map[string]*Release{}
	}
	rc.byTag[""] = rel
	rc.byTag[tag] = rel
}

// setTargetFetchErr 设置目标 Release 查询错误，仅影响指定 targetTag（或 latest ""）的查询。
// 当前版本查询仍返回 byTag[v0.1.0]，不被该错误影响。
func setTargetFetchErr(svc *Service, targetTag string, err error) {
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	if rc.errByTag == nil {
		rc.errByTag = map[string]error{}
	}
	rc.errByTag[targetTag] = err
}

// ---- Check 测试 ----

// TestCheck_LatestHigher：latest 严格高于当前 → updateAvailable=true。
func TestCheck_LatestHigher(t *testing.T) {
	svc := makeService(t) // 默认目标 v0.2.0

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("应报告 updateAvailable=true")
	}
	if got.TargetTag != "v0.2.0" {
		t.Fatalf("TargetTag = %q, want v0.2.0", got.TargetTag)
	}
	if got.CurrentTag != "v0.1.0" {
		t.Fatalf("CurrentTag = %q, want v0.1.0", got.CurrentTag)
	}
}

// TestCheck_TargetEqualCurrent：目标等于当前 → no update。
func TestCheck_TargetEqualCurrent(t *testing.T) {
	svc := makeService(t)
	setTarget(svc, "v0.1.0")

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("版本相同应报告无更新")
	}
}

// TestCheck_TargetLowerCurrent：目标低于当前 → no update。
func TestCheck_TargetLowerCurrent(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "v0.2.0"
	// 当前版本改为 v0.2.0 时，byTag 需补 v0.2.0 当前版本；目标设为 v0.1.0。
	svc.ReleaseClient.(*fakeReleaseClient).byTag["v0.2.0"] = makeCurrentRelease("v0.2.0")
	setTarget(svc, "v0.1.0")

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("目标低于当前应报告无更新")
	}
}

// TestCheck_ExplicitTagHigher：显式目标 tag 严格更高 → updateAvailable。
func TestCheck_ExplicitTagHigher(t *testing.T) {
	svc := makeService(t)

	got, err := svc.Check(context.Background(), CheckOptions{TargetTag: "v0.2.0"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("显式更高 tag 应报告更新")
	}
}

// TestCheck_StableHigherThanRC：稳定版严格高于同三元组 rc 版 → updateAvailable（稳定 > rc）。
// 说明 Compare 把稳定版（RC==0）排在任意候选版之上，故 rc.1 → 稳定 v0.1.0 视为更新。
func TestCheck_StableHigherThanRC(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "v0.1.0-rc.1"
	svc.ReleaseClient.(*fakeReleaseClient).byTag["v0.1.0-rc.1"] = makeCurrentRelease("v0.1.0-rc.1")
	setTarget(svc, "v0.1.0")

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("稳定 v0.1.0 严格高于 rc.1，应报告更新")
	}
}

// TestCheck_CurrentDevRejected：当前为 dev → Check 应返回 error（非法当前版本）。
// Check 阶段就要拒绝 dev，不进行目标查询。
func TestCheck_CurrentDevRejected(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "dev"

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err == nil {
		t.Fatalf("dev 版本应返回错误，got=%+v", got)
	}
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	if len(rc.fetches) != 0 {
		t.Fatalf("dev 短路不应查询目标 Release，fetches=%v", rc.fetches)
	}
}

// TestCheck_NoStableRelease：latest 端点无稳定版 → 结果携带 NoStableRelease 标记。
func TestCheck_NoStableRelease(t *testing.T) {
	svc := makeService(t)
	setTargetFetchErr(svc, "", ErrNoStableRelease) // 仅影响 latest("") 查询

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.NoStableRelease {
		t.Fatal("应设置 NoStableRelease=true")
	}
	if got.UpdateAvailable {
		t.Fatal("无稳定版时不应报告更新")
	}
}

// TestCheck_VersionNotFound：显式 tag 不存在 → 结果携带 VersionNotFound 标记。
func TestCheck_VersionNotFound(t *testing.T) {
	svc := makeService(t)
	setTargetFetchErr(svc, "v9.9.9", ErrVersionNotFound) // 仅影响 v9.9.9 查询

	got, err := svc.Check(context.Background(), CheckOptions{TargetTag: "v9.9.9"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.VersionNotFound {
		t.Fatal("应设置 VersionNotFound=true")
	}
}

// TestCheck_TransientError：目标查询返回瞬时错误 → 透传。
func TestCheck_TransientError(t *testing.T) {
	svc := makeService(t)
	setTargetFetchErr(svc, "", errors.New("503 service unavailable"))

	if _, err := svc.Check(context.Background(), CheckOptions{}); err == nil {
		t.Fatal("瞬时错误应透传")
	}
}

// TestCheck_DoesNotVerifyProvenance：Check 不做来源验证、不创建本地文件。
// 用一个会 fail 的 ProvenanceDeps 证明 Check 完全绕过来源校验。
func TestCheck_DoesNotVerifyProvenance(t *testing.T) {
	svc := makeService(t)
	// 故意把来源校验依赖破坏掉：Manifest 设为 nil（VerifyProvenance 会判 untrusted）。
	svc.ProvenanceDeps.Manifest = nil

	got, err := svc.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("Check 不应因来源不可信而改变更新判定")
	}
}

// TestCheck_DoesNotCreateFiles：Check 不创建任何本地文件。
func TestCheck_DoesNotCreateFiles(t *testing.T) {
	svc := makeService(t)

	if _, err := svc.Check(context.Background(), CheckOptions{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	// 通过断言 downloader 未被触发来保证 Check 不下载、不写文件。
	if svc.downloaderUsed() {
		t.Fatal("Check 不应触发下载")
	}
}

// ---- Apply 测试 ----

// TestApply_NoUpdate：无更新 → 直接返回，不做来源验证。
func TestApply_NoUpdate(t *testing.T) {
	svc := makeService(t)
	setTarget(svc, "v0.1.0")
	// 破坏来源依赖，证明 Apply 在「无更新」分支短路、不触发来源校验。
	svc.ProvenanceDeps.Manifest = nil

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("无更新时不应报告更新")
	}
	if got.ProvenanceChecked {
		t.Fatal("无更新时不应执行来源校验")
	}
}

// TestApply_UntrustedNoDownload：来源不可信 → 只返回人工安装指引，不下载。
func TestApply_UntrustedNoDownload(t *testing.T) {
	svc := makeService(t)
	// 破坏来源：manifest hash 与本地不符。
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(
		buildSumsBody("token-usage-darwin-arm64", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
	)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("应报告有更新")
	}
	if !got.ProvenanceChecked {
		t.Fatal("应执行来源校验")
	}
	if got.ProvenanceTrusted {
		t.Fatal("来源应判定为不可信")
	}
	if got.Reason == "" {
		t.Fatal("不可信应携带 reason")
	}
	if svc.downloaderUsed() {
		t.Fatal("来源不可信时绝不下载")
	}
	if got.Installed {
		t.Fatal("来源不可信时绝不安装")
	}
}

// TestApply_DevCurrentRejected：当前为 dev → Apply 应返回 error。
func TestApply_DevCurrentRejected(t *testing.T) {
	svc := makeService(t)
	svc.CurrentVersion = "dev"

	if _, err := svc.Apply(context.Background(), ApplyOptions{}); err == nil {
		t.Fatal("dev 当前版本应返回错误")
	}
}

// TestApply_TrustedReadyToInstall：来源可信 → 到达「准备安装」集成点。
// 本测试只断言判定与 provenance 链路到 ReadyToInstall 集成点，不执行下载与锁内编排。
func TestApply_TrustedReadyToInstall(t *testing.T) {
	svc := makeService(t)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.UpdateAvailable {
		t.Fatal("应报告有更新")
	}
	if !got.ProvenanceChecked {
		t.Fatal("应执行来源校验")
	}
	if !got.ProvenanceTrusted {
		t.Fatalf("来源应可信，reason=%q", got.Reason)
	}
	if !got.ReadyToInstall {
		t.Fatal("可信来源应到达「准备安装」状态")
	}
	if got.TargetTag != "v0.2.0" {
		t.Fatalf("TargetTag = %q, want v0.2.0", got.TargetTag)
	}
	if got.TargetAsset != "token-usage-darwin-arm64" {
		t.Fatalf("TargetAsset = %q, want token-usage-darwin-arm64", got.TargetAsset)
	}
}

// TestApply_SymlinkCurrentRejected：当前二进制是 symlink → 不可信，不下载。
func TestApply_SymlinkCurrentRejected(t *testing.T) {
	svc := makeService(t)
	svc.ProvenanceDeps.Lstat.(*fakeLstat).infos[svc.binPathForTest] = fixtureFileInfo(t, "symlink")

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ProvenanceTrusted {
		t.Fatal("symlink 来源应不可信")
	}
	if svc.downloaderUsed() {
		t.Fatal("symlink 来源不可信时绝不下载")
	}
}

// TestApply_TargetEqualNoProvenance：目标等于当前 → 无更新，不做来源校验。
func TestApply_TargetEqualNoProvenance(t *testing.T) {
	svc := makeService(t)
	setTarget(svc, "v0.1.0")

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ProvenanceChecked {
		t.Fatal("目标等于当前时不应做来源校验")
	}
}

// TestApply_StageVersionMismatchRejected：可选 stage --version 探针返回不一致版本 → 拒绝。
// 本任务定义 VersionProbe seam；通过注入 fake probe 验证 Apply 在可信来源通过后，
// 若 probe 报告的版本 != 目标 tag，则拒绝安装（ReadyToInstall=false）。
func TestApply_StageVersionMismatchRejected(t *testing.T) {
	svc := makeService(t)
	// stage 探针返回一个与目标不一致的版本。
	svc.VersionProbe = staticVersionProbe("v0.1.0", nil)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ProvenanceTrusted {
		t.Fatalf("来源应可信，reason=%q", got.Reason)
	}
	if got.ReadyToInstall {
		t.Fatal("stage 版本不一致应拒绝安装")
	}
	if got.StageVersion != "v0.1.0" {
		t.Fatalf("StageVersion = %q, want v0.1.0", got.StageVersion)
	}
	if got.Reason == "" {
		t.Fatal("stage 失败应携带 reason")
	}
}

// TestApply_StageVersionMatchProceeds：stage --version 与目标一致 → ReadyToInstall=true。
func TestApply_StageVersionMatchProceeds(t *testing.T) {
	svc := makeService(t)
	svc.VersionProbe = staticVersionProbe("v0.2.0", nil)

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ReadyToInstall {
		t.Fatalf("stage 一致应 ReadyToInstall=true，reason=%q", got.Reason)
	}
}

// TestApply_StageProbeDisabledSkipsCheck：未注入 VersionProbe → 跳过 stage 检查，
// 仅依赖 SHA256 provenance（stage 检查是额外防线，非必需）。
func TestApply_StageProbeDisabledSkipsCheck(t *testing.T) {
	svc := makeService(t)
	svc.VersionProbe = nil // 未注入 → 跳过

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.ReadyToInstall {
		t.Fatal("未注入 probe 时，SHA256 通过即应 ReadyToInstall=true")
	}
	if got.StageVersion != "" {
		t.Fatalf("未注入 probe 时 StageVersion 应为空，got %q", got.StageVersion)
	}
}

// TestApply_StageProbeErrorRejected：probe 返回错误 → 拒绝安装（保守）。
func TestApply_StageProbeErrorRejected(t *testing.T) {
	svc := makeService(t)
	svc.VersionProbe = staticVersionProbe("", errors.New("exec failed"))

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ReadyToInstall {
		t.Fatal("probe 错误应拒绝安装")
	}
}

// ---- VersionProbe fake ----

// staticVersionProbe 返回固定版本/错误，无视 stage 路径。
func staticVersionProbe(ver string, err error) VersionProbe {
	return versionProbeFunc(func(ctx context.Context, stagePath string) (string, error) {
		return ver, err
	})
}

type versionProbeFunc func(ctx context.Context, stagePath string) (string, error)

func (f versionProbeFunc) ProbeVersion(ctx context.Context, stagePath string) (string, error) {
	return f(ctx, stagePath)
}

// ---- consume / sweep 时序与安全测试 ----

// nonceFilePath 构造 target 同目录的 nonce 命名事务文件路径（辅助测试）。
func nonceFilePath(binPath, suffix, nonce, ext string) string {
	base := filepath.Base(binPath)
	name := "." + base + suffix + nonce + ext
	return filepath.Join(filepath.Dir(binPath), name)
}

// writeNonceFile 在 binPath 同目录创建一个 nonce 命名文件，返回路径。
func writeNonceFile(t *testing.T, binPath, suffix, nonce, ext string, content []byte) string {
	t.Helper()
	p := nonceFilePath(binPath, suffix, nonce, ext)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestApply_UntrustedNoConsumeSweep ：非官方安装（Trusted=false）不被新增的
// consume/sweep 触碰。造 result + stage + backup 文件，Apply 后验证全部保留。
func TestApply_UntrustedNoConsumeSweep(t *testing.T) {
	svc := makeService(t)
	const nonce = "aabbccddaabbccddaabbccddaabbccdd"

	paths := []string{
		writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte(`{"success":true}`)),
		writeNonceFile(t, svc.binPathForTest, updateStageSuffix, nonce, "", []byte("stale")),
		writeNonceFile(t, svc.binPathForTest, updateBackupSuffix, nonce, "", []byte("stale")),
	}
	t.Cleanup(func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	})

	// 破坏来源：manifest hash 不匹配本地二进制。
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(
		buildSumsBody("token-usage-darwin-arm64", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
	)

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.ProvenanceTrusted {
		t.Fatal("来源应不可信")
	}

	// 不可信 → 新增 consume/sweep 零执行 → 文件全部保留。
	for _, p := range paths {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("不可信来源不应触碰事务文件: %s", p)
		}
	}
	if strings.Contains(logBuf.String(), "consumed") {
		t.Error("不可信来源不应 consume（日志不应含 consumed）")
	}
}

// TestApply_WindowsGoosSweepSkippedConsumeRuns ：注入 Goos="windows" 模拟 Windows
// 平台。第二次 Apply 的 consume 只读删 result，不删 plan/stage/backup（sweep 按 Goos 跳过）。
func TestApply_WindowsGoosSweepSkippedConsumeRuns(t *testing.T) {
	svc := makeService(t)
	svc.Goos = "windows"
	setTarget(svc, "v0.1.0") // 无更新：只走 consume/sweep，不进入 AssetName/download

	const nonce = "11223344556677881122334455667788"
	resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte(`{"success":true}`))
	planPath := writeNonceFile(t, svc.binPathForTest, planSuffix, nonce, "", []byte("plan"))
	stagePath := writeNonceFile(t, svc.binPathForTest, updateStageSuffix, nonce, "", []byte("stage"))
	backupPath := writeNonceFile(t, svc.binPathForTest, updateBackupSuffix, nonce, "", []byte("backup"))
	t.Cleanup(func() {
		for _, p := range []string{resultPath, planPath, stagePath, backupPath} {
			_ = os.Remove(p)
		}
	})

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("应无更新")
	}

	// result 被 consume 删除（consume 双平台执行）。
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Error("result 应被 consume 删除")
	}
	// plan/stage/backup 保留（Windows sweep 被跳过，验证 sweep 按 Goos 跳过）。
	for _, p := range []string{planPath, stagePath, backupPath} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("Windows 平台 sweep 应跳过，文件应保留: %s", p)
		}
	}
}

// TestApply_ConsumePreservesCleanupPlan ：consume 删 result 后 plan 仍完好可读，
// 验证 cleanup 路径派生（读 plan 取 nonce/targetBasename）不受 result 删除影响。
func TestApply_ConsumePreservesCleanupPlan(t *testing.T) {
	svc := makeService(t)
	svc.Goos = "windows"
	setTarget(svc, "v0.1.0")

	const nonce = "aabbccddaabbccddaabbccddaabbccdd"
	resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte(`{"success":true}`))
	planPath := nonceFilePath(svc.binPathForTest, planSuffix, nonce, "")
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: filepath.Base(svc.binPathForTest),
		OldSHA256:      "1111111111111111111111111111111111111111111111111111111111111111",
		NewSHA256:      "2222222222222222222222222222222222222222222222222222222222222222",
		Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
	}
	if err := writeHelperPlan(planPath, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(resultPath)
		_ = os.Remove(planPath)
	})

	if _, err := svc.Apply(context.Background(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply err=%v", err)
	}

	// result 被消费删除。
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Error("result 应被 consume 删除")
	}
	// plan 完好可读，cleanup 路径派生不受 result 删除影响。
	gotPlan, err := readHelperPlan(planPath)
	if err != nil {
		t.Fatalf("plan 应仍可读: %v", err)
	}
	if gotPlan.Nonce != nonce {
		t.Errorf("plan nonce 不匹配: got %q want %q", gotPlan.Nonce, nonce)
	}
	if gotPlan.TargetBasename != filepath.Base(svc.binPathForTest) {
		t.Errorf("plan target basename 不匹配: got %q", gotPlan.TargetBasename)
	}
}

// TestApply_NoUpdateTrustedConsumesResult (P1 核心场景)：升级成功后再次 update
// （无更新 + 可信），Apply 返回"已是最新"前 consume 删除 result 并记录"上次升级成功"。
func TestApply_NoUpdateTrustedConsumesResult(t *testing.T) {
	svc := makeService(t)
	setTarget(svc, "v0.1.0") // 无更新

	const nonce = "aabbccddaabbccddaabbccddaabbccdd"
	resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte(`{"success":true}`))
	t.Cleanup(func() { _ = os.Remove(resultPath) })

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("应无更新")
	}
	// result 被消费删除（证明无更新路径也执行 consume）。
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Error("result 应被 consume 删除（无更新也消费）")
	}
	// 日志记录上次升级成功。
	if !strings.Contains(logBuf.String(), "consumed previous result: success") {
		t.Errorf("日志应记 consumed previous result: success\n实际:\n%s", logBuf.String())
	}
}

// TestApply_ConsumeSafeScenarios ：consume 在 result 未写（无命中）或已原子写
// （完整 JSON / 损坏 JSON）时均正确：不崩、不误删其他文件、损坏的也删除。
func TestApply_ConsumeSafeScenarios(t *testing.T) {
	const nonce = "aabbccddaabbccddaabbccddaabbccdd"

	t.Run("no result file", func(t *testing.T) {
		svc := makeService(t)
		setTarget(svc, "v0.1.0")

		var logBuf bytes.Buffer
		svc.LogSink = &logBuf
		_, err := svc.Apply(context.Background(), ApplyOptions{})
		if err != nil {
			t.Fatalf("Apply err=%v", err)
		}
		if strings.Contains(logBuf.String(), "consumed") {
			t.Error("无 result 时不应 consume")
		}
	})

	t.Run("corrupt result deleted", func(t *testing.T) {
		svc := makeService(t)
		setTarget(svc, "v0.1.0")

		resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt, []byte("not-json"))
		t.Cleanup(func() { _ = os.Remove(resultPath) })

		var logBuf bytes.Buffer
		svc.LogSink = &logBuf
		_, err := svc.Apply(context.Background(), ApplyOptions{})
		if err != nil {
			t.Fatalf("Apply err=%v", err)
		}
		// 损坏 result 被删除（防止永久残留）。
		if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
			t.Error("损坏 result 应被删除")
		}
		if !strings.Contains(logBuf.String(), "parse error") {
			t.Errorf("日志应记 parse error\n实际:\n%s", logBuf.String())
		}
	})

	t.Run("failed result logged with error", func(t *testing.T) {
		svc := makeService(t)
		setTarget(svc, "v0.1.0")

		resultPath := writeNonceFile(t, svc.binPathForTest, resultSuffix, nonce, resultExt,
			[]byte(`{"success":false,"error":"MoveFileEx failed"}`))
		t.Cleanup(func() { _ = os.Remove(resultPath) })

		var logBuf bytes.Buffer
		svc.LogSink = &logBuf
		_, err := svc.Apply(context.Background(), ApplyOptions{})
		if err != nil {
			t.Fatalf("Apply err=%v", err)
		}
		if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
			t.Error("失败 result 也应被消费删除")
		}
		if !strings.Contains(logBuf.String(), "consumed previous result: failed") {
			t.Errorf("日志应记 consumed previous result: failed\n实际:\n%s", logBuf.String())
		}
	})
}

// ---- Apply 分支回归测试 ----

// TestApply_NilServiceNoPanic：nil Service 调用 Apply 返回错误，不 panic。
func TestApply_NilServiceNoPanic(t *testing.T) {
	var svc *Service
	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("nil Service 应返回错误")
	}
}

// TestApply_NoUpdateProvenanceErrorBestEffort：无更新 + provenance 查询失败时，
// 记 best-effort 日志并返回成功（"已是最新"），不作为致命错误。
func TestApply_NoUpdateProvenanceErrorBestEffort(t *testing.T) {
	svc := makeService(t)
	setTarget(svc, "v0.1.0") // 无更新
	// 破坏 provenance 依赖使其返回 error（Executable 为 nil → validate 失败）。
	svc.ProvenanceDeps.Executable = nil

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("无更新 + provenance error 不应返回 error: %v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("应无更新")
	}
	logStr := logBuf.String()
	if !strings.Contains(logStr, "provenance check failed (best-effort)") {
		t.Errorf("应记 best-effort provenance failure\n实际:\n%s", logStr)
	}
	if strings.Contains(logStr, "provenance error") {
		t.Error("无更新路径不应记致命 provenance error")
	}
}

// TestApply_UpdateAvailableProvenanceErrorFatal：有更新 + provenance 查询失败时，
// 记致命日志并返回 error（不记 best-effort）。
func TestApply_UpdateAvailableProvenanceErrorFatal(t *testing.T) {
	svc := makeService(t)
	// 默认目标 v0.2.0 > 当前 v0.1.0，有更新。
	// 破坏 provenance 依赖使其返回 error。
	svc.ProvenanceDeps.Executable = nil

	var logBuf bytes.Buffer
	svc.LogSink = &logBuf

	_, err := svc.Apply(context.Background(), ApplyOptions{})
	if err == nil {
		t.Fatal("有更新 + provenance error 应返回 error")
	}
	logStr := logBuf.String()
	if !strings.Contains(logStr, "provenance error") {
		t.Errorf("应记致命 provenance error\n实际:\n%s", logStr)
	}
	if strings.Contains(logStr, "best-effort") {
		t.Error("有更新路径不应记 best-effort")
	}
}
