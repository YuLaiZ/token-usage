package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// provenance_test.go 校验「当前安装来源验证」安全门（VerifyProvenance）。
//
// VerifyProvenance 实施判定顺序的第 1/3/4/5 步（第 2 步目标比较属于 update.go）：
//  1. 解析当前版本；dev 或非正式 tag → untrusted；
//  3. 解析当前可执行文件路径；Lstat 确认是普通文件且非 symlink，路径须绝对；
//  4. 读取当前版本 Release 的 SHA256SUMS，计算当前二进制 SHA256；
//  5. 仅当本地 hash == 该平台官方资产 hash 才视为 trusted。
//
// 全部测试不访问真实 GitHub，注入 fakeReleaseClient / fakeExecutableResolver /
// fakeLstat / fakeFileReader；当前二进制内容用 t.TempDir() 下真实小文件承载，
// 以便 fakeFileReader 与 sha256 计算路径一致。

// ---- fake Lstat（可模拟 symlink/dir/regular/不存在）----

// fakeLstat 按 path → FileInfo 预置返回；未命中返回 os.ErrNotExist。
// 支持用真实 os.Lstat 结果预置（方便构造 regular / dir / symlink 三类）。
type fakeLstat struct {
	infos map[string]fs.FileInfo
	errs  map[string]error
}

func newFakeLstat() *fakeLstat {
	return &fakeLstat{infos: map[string]fs.FileInfo{}, errs: map[string]error{}}
}

func (f *fakeLstat) Lstat(name string) (fs.FileInfo, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	if info, ok := f.infos[name]; ok {
		return info, nil
	}
	return nil, os.ErrNotExist
}

// fixtureFileInfo 在临时目录构造指定类型（regular/dir/symlink）的文件，
// 返回其 os.Lstat 结果，用于在 fakeLstat 中预置可信 FileInfo。
func fixtureFileInfo(t *testing.T, kind string) fs.FileInfo {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "fixture")
	switch kind {
	case "regular":
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	case "dir":
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("mkdir fixture: %v", err)
		}
	case "symlink":
		linkTarget := filepath.Join(dir, "real")
		if err := os.WriteFile(linkTarget, []byte("x"), 0o600); err != nil {
			t.Fatalf("write link target: %v", err)
		}
		if err := os.Symlink(linkTarget, target); err != nil {
			t.Fatalf("symlink fixture: %v", err)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat fixture: %v", err)
	}
	return info
}

// ---- 辅助：构造合法 Release 与 SHA256SUMS manifest ----

// sumHex 把字节数据转 64 位小写 hex。
func sumHex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// buildSumsBody 构造一份合法（ASCII 升序三行）SHA256SUMS 文本，
// assetName 对应的 hash 用 given，其它两项用固定占位。
func buildSumsBody(assetName, given string) string {
	hashes := map[string]string{
		"token-usage-darwin-amd64":      "1111111111111111111111111111111111111111111111111111111111111111",
		"token-usage-darwin-arm64":      "2222222222222222222222222222222222222222222222222222222222222222",
		"token-usage-windows-amd64":     "3333333333333333333333333333333333333333333333333333333333333333",
		"token-usage-windows-amd64.exe": "3333333333333333333333333331111111111111111111111111111111111111",
	}
	hashes[assetName] = given
	var b []byte
	for _, n := range manifestBinaryAssets { // 已是 ASCII 升序
		h := hashes[n]
		if h == "" {
			h = "0000000000000000000000000000000000000000000000000000000000000000"
		}
		b = append(b, []byte(h+"  "+n+"\n")...)
	}
	return string(b)
}

// makeCurrentRelease 构造一个 tag 对应的合法 Release（四项冻结资产，Prerelease 与 tag 一致）。
func makeCurrentRelease(tag string) *Release {
	ver, err := ParseVersion(tag)
	if err != nil {
		panic(err)
	}
	assets := map[string]Asset{}
	for _, n := range platformAssetNamesSorted {
		assets[n] = Asset{Name: n}
	}
	return &Release{
		Tag:        tag,
		Version:    ver,
		Prerelease: ver.IsPrerelease(),
		Assets:     assets,
	}
}

// makeProvenanceDeps 组装一组「默认可信」依赖：当前二进制内容为 binContent，
// 平台 darwin/arm64。返回的 deps 尚未注入 ManifestFetcher（各用例按需注入）。
func makeProvenanceDeps(t *testing.T, currentTag string, binContent []byte) (
	deps ProvenanceDeps,
	binPath string,
	rc *fakeReleaseClient,
	fr *fakeFileReader,
	ls *fakeLstat,
	exe *fakeExecutableResolver,
) {
	t.Helper()
	binPath = filepath.Join(t.TempDir(), "token-usage")
	if err := os.WriteFile(binPath, binContent, 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	info, err := os.Lstat(binPath)
	if err != nil {
		t.Fatalf("lstat fake binary: %v", err)
	}
	ls = newFakeLstat()
	ls.infos[binPath] = info
	fr = newFakeFileReader()
	fr.files[binPath] = binContent
	exe = &fakeExecutableResolver{path: binPath}
	rc = &fakeReleaseClient{release: makeCurrentRelease(currentTag)}
	deps = ProvenanceDeps{
		Executable: exe,
		Lstat:      ls,
		FileReader: fr,
		Goos:       "darwin",
		Goarch:     "arm64",
	}
	return
}

// withMatchingManifest 给 deps 注入一份 manifest，arm64 hash = binContent 实际 hash。
func withMatchingManifest(deps *ProvenanceDeps, binContent []byte) {
	deps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", sumHex(binContent)))
}

// ---- manifestFetcher fake（实现 ManifestFetcher seam）----

// staticManifestFetcher 返回固定 body 解析的 Manifest，无视 tag。
func staticManifestFetcher(body string) ManifestFetcher {
	return manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
		return ParseManifest([]byte(body))
	})
}

// errorManifestFetcher 始终返回 err。
func errorManifestFetcher(err error) ManifestFetcher {
	return manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
		return nil, err
	})
}

// manifestFetcherFunc 让普通函数实现 ManifestFetcher。
type manifestFetcherFunc func(ctx context.Context, tag string) (*Manifest, error)

func (f manifestFetcherFunc) FetchManifest(ctx context.Context, tag string) (*Manifest, error) {
	return f(ctx, tag)
}

// ---- 来源验证边界测试 ----

// TestVerifyProvenance_DevRejected：当前版本为 dev → untrusted，且短路不触网。
func TestVerifyProvenance_DevRejected(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))

	res, err := VerifyProvenance(context.Background(), deps, "dev", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("dev 版本应判定为 untrusted")
	}
	if res.Reason == "" {
		t.Fatal("untrusted 应携带 reason")
	}
	if len(rc.fetches) != 0 {
		t.Fatalf("dev 短路前不应查询 Release，fetches=%v", rc.fetches)
	}
}

// TestVerifyProvenance_InvalidVersionRejected：当前版本非正式 tag → untrusted。
func TestVerifyProvenance_InvalidVersionRejected(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))

	res, err := VerifyProvenance(context.Background(), deps, "v0.1", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("非法版本号应判定为 untrusted")
	}
	if len(rc.fetches) != 0 {
		t.Fatalf("非法版本短路前不应查询 Release，fetches=%v", rc.fetches)
	}
}

// TestVerifyProvenance_SymlinkRejected：当前可执行文件是 symlink → untrusted。
func TestVerifyProvenance_SymlinkRejected(t *testing.T) {
	deps, binPath, rc, _, ls, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	ls.infos[binPath] = fixtureFileInfo(t, "symlink")

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("symlink 应判定为 untrusted")
	}
	if len(rc.fetches) != 0 {
		t.Fatalf("symlink 短路前不应查询 Release，fetches=%v", rc.fetches)
	}
}

// TestVerifyProvenance_DirectoryRejected：当前路径是目录 → untrusted。
func TestVerifyProvenance_DirectoryRejected(t *testing.T) {
	deps, binPath, rc, _, ls, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	ls.infos[binPath] = fixtureFileInfo(t, "dir")

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("目录应判定为 untrusted")
	}
	if len(rc.fetches) != 0 {
		t.Fatalf("目录短路前不应查询 Release，fetches=%v", rc.fetches)
	}
}

// TestVerifyProvenance_RelativePathRejected：Executable 返回相对路径 → untrusted。
func TestVerifyProvenance_RelativePathRejected(t *testing.T) {
	deps, binPath, rc, _, _, exe := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	exe.path = filepath.Base(binPath)

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("相对路径应判定为 untrusted")
	}
}

// TestVerifyProvenance_HashMismatchRejected：本地 hash != 官方 hash → untrusted。
func TestVerifyProvenance_HashMismatchRejected(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	// 64 位合法小写 hex，但与本地二进制实际 hash 不同。
	wrong := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	deps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", wrong))

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("hash 失配应判定为 untrusted")
	}
	if res.OfficialHash != wrong {
		t.Fatalf("OfficialHash = %q, want %q", res.OfficialHash, wrong)
	}
	if res.LocalHash != sumHex([]byte("official-bin")) {
		t.Fatalf("LocalHash 应为本地实际 hash，got %q", res.LocalHash)
	}
}

// TestVerifyProvenance_CurrentReleaseMissingAssets：当前版本 Release 查询返回错误 → untrusted。
func TestVerifyProvenance_CurrentReleaseMissingAssets(t *testing.T) {
	deps, _, _, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	rc := &fakeReleaseClient{fetchErr: ErrVersionNotFound}
	deps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", sumHex([]byte("official-bin"))))

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("当前版本 Release 查询失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_ManifestFetchFails：当前版本 manifest 获取失败 → untrusted。
func TestVerifyProvenance_ManifestFetchFails(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	deps.Manifest = errorManifestFetcher(errors.New("网络不可达"))

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("manifest 获取失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_UnsupportedPlatform：当前平台不在冻结资产集合 → untrusted。
func TestVerifyProvenance_UnsupportedPlatform(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	deps.Goos = "linux" // 不受支持
	deps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", sumHex([]byte("official-bin"))))

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("不受支持平台应判定为 untrusted")
	}
}

// TestVerifyProvenance_ManifestMissingPlatformHash：manifest 不含当前平台资产 hash → untrusted。
func TestVerifyProvenance_ManifestMissingPlatformHash(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	// 构造一份 manifest，其 arm64 hash 指向不存在的占位（与本地不符且 manifest 行存在），
	// 但改 platform 为 amd64，并让 manifest amd64 行缺失 → 用一份「amd64 缺失」的坏 manifest：
	// 直接复用合法 manifest（三行齐全），但 manifestFetcher 返回的 manifest 经 HashFor 找不到 amd64。
	// 由于 ParseManifest 要求三行齐全，这里改用一个把 amd64 hash 写为合法但与本地不同的值，
	// 实际上仍属 hash 失配（已被上一用例覆盖）。为制造「HashFor 未命中」，直接注入自定义 fetcher：
	deps.Goarch = "amd64"
	deps.Manifest = manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
		// 返回一个只含 arm64 的 Manifest（绕过 ParseManifest 的三行约束）。
		return &Manifest{hashes: map[string]string{
			"token-usage-darwin-arm64": sumHex([]byte("official-bin")),
		}}, nil
	})

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("manifest 缺当前平台 hash 应判定为 untrusted")
	}
}

// TestVerifyProvenance_ManualOfficialCopyMatches：手工复制的官方裸二进制 hash 匹配 → trusted。
func TestVerifyProvenance_ManualOfficialCopyMatches(t *testing.T) {
	bin := []byte("this-is-the-exact-official-bytes")
	deps, binPath, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", bin)
	withMatchingManifest(&deps, bin)

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Trusted {
		t.Fatalf("hash 匹配应判定为 trusted，reason=%q", res.Reason)
	}
	if res.CurrentTag != "v0.1.0" {
		t.Fatalf("CurrentTag = %q, want v0.1.0", res.CurrentTag)
	}
	if res.BinaryPath != binPath {
		t.Fatalf("BinaryPath = %q, want %q", res.BinaryPath, binPath)
	}
	wantHash := sumHex(bin)
	if res.LocalHash != wantHash {
		t.Fatalf("LocalHash = %q, want %q", res.LocalHash, wantHash)
	}
	if res.OfficialHash != wantHash {
		t.Fatalf("OfficialHash = %q, want %q", res.OfficialHash, wantHash)
	}
	if res.Asset != "token-usage-darwin-arm64" {
		t.Fatalf("Asset = %q, want token-usage-darwin-arm64", res.Asset)
	}
}

// TestVerifyProvenance_ExecutableError：Executable 返回错误 → untrusted。
func TestVerifyProvenance_ExecutableError(t *testing.T) {
	deps, _, rc, _, _, exe := makeProvenanceDeps(t, "v0.1.0", []byte("x"))
	exe.err = errors.New("readlink failed")
	exe.path = ""

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("Executable 解析失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_LstatError：Lstat 返回错误 → untrusted。
func TestVerifyProvenance_LstatError(t *testing.T) {
	deps, binPath, rc, _, ls, _ := makeProvenanceDeps(t, "v0.1.0", []byte("x"))
	ls.errs[binPath] = errors.New("permission denied")

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("Lstat 失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_ReadFileError：读取当前二进制失败 → untrusted。
func TestVerifyProvenance_ReadFileError(t *testing.T) {
	deps, binPath, rc, fr, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("x"))
	delete(fr.files, binPath)

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("读取二进制失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_ManifestParseError：manifest 解析失败 → untrusted。
func TestVerifyProvenance_ManifestParseError(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("x"))
	deps.Manifest = staticManifestFetcher("garbage-not-a-manifest")

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("manifest 解析失败应判定为 untrusted")
	}
}

// TestVerifyProvenance_PrereleaseCurrentAllowed：当前是 rc 版本但 hash 匹配 → trusted。
// 说明手工安装的候选版二进制只要 hash 匹配，provenance 仍可信。
func TestVerifyProvenance_PrereleaseCurrentAllowed(t *testing.T) {
	bin := []byte("rc-bytes")
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.2.0-rc.1", bin)
	deps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", sumHex(bin)))

	res, err := VerifyProvenance(context.Background(), deps, "v0.2.0-rc.1", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Trusted {
		t.Fatalf("rc 版本 hash 匹配应 trusted，reason=%q", res.Reason)
	}
}

// TestVerifyProvenance_DoesNotCheckTarget：VerifyProvenance 只校验当前来源，
// 不做目标比较；且应查询一次 currentVersion 的 Release 与 manifest。
func TestVerifyProvenance_DoesNotCheckTarget(t *testing.T) {
	bin := []byte("bin")
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", bin)
	withMatchingManifest(&deps, bin)
	var manifestTags []string
	wrapped := deps.Manifest
	deps.Manifest = manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
		manifestTags = append(manifestTags, tag)
		return wrapped.FetchManifest(ctx, tag)
	})

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Trusted {
		t.Fatalf("应 trusted，reason=%q", res.Reason)
	}
	// 查询的是当前版本 tag。
	foundRelease := false
	for _, tag := range rc.fetches {
		if tag == "v0.1.0" {
			foundRelease = true
		}
	}
	if !foundRelease {
		t.Fatalf("应查询当前版本 Release，fetches=%v", rc.fetches)
	}
	foundManifest := false
	for _, tag := range manifestTags {
		if tag == "v0.1.0" {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Fatalf("应查询当前版本 manifest，tags=%v", manifestTags)
	}
}

// TestVerifyProvenance_NilManifestFetcher：未注入 ManifestFetcher → untrusted。
// 防御性：deps 不完整时绝不放行。
func TestVerifyProvenance_NilManifestFetcher(t *testing.T) {
	deps, _, rc, _, _, _ := makeProvenanceDeps(t, "v0.1.0", []byte("official-bin"))
	deps.Manifest = nil

	res, err := VerifyProvenance(context.Background(), deps, "v0.1.0", rc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Trusted {
		t.Fatal("未注入 ManifestFetcher 应判定为 untrusted")
	}
}
