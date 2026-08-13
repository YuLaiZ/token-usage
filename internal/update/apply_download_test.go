package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// apply_download_test.go 校验 AssetDownloader 已接入 Apply 可信分支：
// 来源校验通过后，Apply 用注入的 AssetDownloader 下载目标资产到真实 stage 文件，
// 再把 stagePath 喂给 VersionProbe 与 installUnderLock。
//
// 全部基于 httptest.Server，绝不访问真实 GitHub。

// newApplyDownloadServer 构造一个按 (tag, asset) 提供内容的 httptest TLS 服务器：
//   - /<tag>/SHA256SUMS         → 指定 tag 的清单
//   - /<tag>/token-usage-darwin-arm64 → 指定 tag 的二进制
//
// manifests 与 binaries 按 tag 索引；未命中返回 404。
func newApplyDownloadServer(t *testing.T, manifests, binaries map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(r.URL.Path) > len("/SHA256SUMS") && r.URL.Path[len(r.URL.Path)-len("/SHA256SUMS"):] == "/SHA256SUMS":
			tag := r.URL.Path[:len(r.URL.Path)-len("/SHA256SUMS")]
			if body, ok := manifests[tag]; ok {
				_, _ = w.Write(body)
				return
			}
		case r.URL.Path == "/v0.2.0/token-usage-darwin-arm64":
			if body, ok := binaries["v0.2.0"]; ok {
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestDownloader 构造指向 httptest 服务器的下载器（与 ManifestFetcher 同一对象）。
func newTestDownloader(t *testing.T, srv *httptest.Server) *downloader {
	t.Helper()
	return &downloader{
		http:         srv.Client(),
		maxBytes:     defaultMaxBinaryBytes,
		temp:         tempFileCreator{},
		userAgent:    defaultUserAgent,
		downloadBase: srv.URL,
	}
}

// TestApply_TrustedDownloadsStageViaAssetDownloader 来源可信 + 注入 AssetDownloader：
// Apply 下载目标资产到真实 stage，stagePath 喂给 installUnderLock（fakeInstaller 记录）。
// stage 内容为目标二进制，且落在 target 同目录（保证同卷 rename）。
func TestApply_TrustedDownloadsStageViaAssetDownloader(t *testing.T) {
	currentBin := []byte("current-official-bin")
	targetBin := []byte("target-official-bin-v0.2.0")

	manifests := map[string][]byte{
		"/v0.1.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))),
		"/v0.2.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex(targetBin))),
	}
	binaries := map[string][]byte{"v0.2.0": targetBin}
	srv := newApplyDownloadServer(t, manifests, binaries)
	d := newTestDownloader(t, srv)

	svc := makeService(t)
	// 一个下载器对象两用：ManifestFetcher（来源校验当前版本清单 + 目标版本清单）与 AssetDownloader。
	svc.ProvenanceDeps.Manifest = d
	svc.AssetDownloader = d
	// 注入 control 编排 + 记录型 installer，验证真实 stagePath 流入 installUnderLock。
	sess := &fakeControlSession{}
	svc.ControlManager = &fakeControlManager{session: sess}
	installer := newFakeInstaller()
	svc.Installer = installer
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall {
		t.Fatalf("应 ReadyToInstall=true，reason=%q", got.Reason)
	}
	if !got.Installed {
		t.Fatal("锁内编排成功后应 Installed=true")
	}
	if len(installer.calls) != 1 {
		t.Fatalf("应 Install 一次，calls=%d", len(installer.calls))
	}
	stagePath := installer.calls[0].stagePath
	if stagePath == "" {
		t.Fatal("stagePath 不应为空（已注入 AssetDownloader，应下载到真实 stage）")
	}
	// stage 内容须在 Install 内捕获：Apply 成功后外部 stage 已删（removeRegularFile）。
	if len(installer.stageContents) != 1 || installer.stageContents[0] == nil {
		t.Fatalf("应在 Install 内捕获 stage 内容，stageContents=%v", installer.stageContents)
	}
	content := installer.stageContents[0]
	if string(content) != string(targetBin) {
		t.Fatalf("stage 内容不匹配，got %q want %q", string(content), string(targetBin))
	}
	if filepath.Dir(stagePath) != filepath.Dir(got.BinaryPath) {
		t.Errorf("stage 目录 %q 应与 target 同目录 %q", filepath.Dir(stagePath), filepath.Dir(got.BinaryPath))
	}
	// 外部 stage 文件应在 Apply 后被删除。
	if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
		t.Errorf("外部 stage 应在 Apply 后删除: %s", stagePath)
	}
}

// TestApply_DownloadFailureRejectsInstall 目标资产下载失败（500）：
// Apply 保守拒绝安装（ReadyToInstall=false），Reason 写明下载失败，不进入锁内编排。
func TestApply_DownloadFailureRejectsInstall(t *testing.T) {
	currentBin := []byte("current-official-bin")
	targetBin := []byte("target-official-bin-v0.2.0")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 清单正常提供，二进制一律 500。
		if r.URL.Path == "/v0.1.0/SHA256SUMS" {
			_, _ = w.Write([]byte(buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))))
			return
		}
		if r.URL.Path == "/v0.2.0/SHA256SUMS" {
			_, _ = w.Write([]byte(buildSumsBody("token-usage-darwin-arm64", sumHex(targetBin))))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	d := newTestDownloader(t, srv)

	svc := makeService(t)
	svc.ProvenanceDeps.Manifest = d
	svc.AssetDownloader = d
	installer := newFakeInstaller()
	svc.Installer = installer
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply 不应返回 error（下载失败是领域结果），err=%v", err)
	}
	if got.ReadyToInstall {
		t.Fatal("下载失败应 ReadyToInstall=false")
	}
	if got.Installed {
		t.Fatal("下载失败应 Installed=false")
	}
	if got.Reason == "" {
		t.Fatal("下载失败应携带 Reason")
	}
	if len(installer.calls) != 0 {
		t.Errorf("下载失败不应进入 Install，calls=%d", len(installer.calls))
	}
}

// TestApply_TargetManifestFetchFailureRejectsInstall 目标版本清单获取失败（404）：
// 无法取得 expectedHash，保守拒绝安装。
func TestApply_TargetManifestFetchFailureRejectsInstall(t *testing.T) {
	currentBin := []byte("current-official-bin")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅提供当前版本清单；目标版本清单与二进制均 404。
		if r.URL.Path == "/v0.1.0/SHA256SUMS" {
			_, _ = w.Write([]byte(buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	d := newTestDownloader(t, srv)

	svc := makeService(t)
	svc.ProvenanceDeps.Manifest = d
	svc.AssetDownloader = d
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	svc.Installer = newFakeInstaller()
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply 不应返回 error，err=%v", err)
	}
	if got.ReadyToInstall {
		t.Fatal("目标清单获取失败应 ReadyToInstall=false")
	}
	if got.Reason == "" {
		t.Fatal("应携带 Reason")
	}
}

// TestApply_NoAssetDownloaderBackwardCompatible 未注入 AssetDownloader：
// 向后兼容——stagePath 为空，仍到达 ReadyToInstall，锁内编排用空 stagePath（由 Installer 自行处理）。
func TestApply_NoAssetDownloaderBackwardCompatible(t *testing.T) {
	svc := makeService(t)
	// makeService 不注入 AssetDownloader；保持空。
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	installer := newFakeInstaller()
	svc.Installer = installer
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall {
		t.Fatal("未注入 AssetDownloader 应仍 ReadyToInstall=true（向后兼容）")
	}
	if len(installer.calls) != 1 {
		t.Fatalf("应 Install 一次，calls=%d", len(installer.calls))
	}
	if installer.calls[0].stagePath != "" {
		t.Errorf("未注入 AssetDownloader 时 stagePath 应为空，got %q", installer.calls[0].stagePath)
	}
}
