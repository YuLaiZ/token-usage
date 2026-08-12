package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// download_test.go 校验资产下载器（downloader）：
//   - 下载 URL 由固定前缀 + tag + 资产名重构（断言请求路径）；
//   - 非 HTTPS 重定向被拒；
//   - 流式写入 stage 文件并边写边算 SHA256；
//   - hash 不匹配删除 stage 并失败；
//   - 响应超过 256 MiB 上限删除 stage 并失败；
//   - 只接受成功 HTTP 状态；
//   - 写完 Sync+Close+chmod（Unix 可执行位）；
//   - FetchManifest 下载并解析 SHA256SUMS。
//
// 全部基于 httptest.Server，绝不访问真实 GitHub。

// sha256Hex 计算输入的 64 位小写 hex sha256。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newDownloader 构造一个指向 httptest.Server 的 downloader（HTTPS-only 客户端）。
// 返回 server、路径记录、状态与 body 控制器（用闭包动态调整响应）。
// paths 在 handler goroutine 写、测试 goroutine 读，用 mu 保护避免 data race。
type dlServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	paths    []string
	status   int
	body     func() string
	redirect *redirectCfg
}

func (ds *dlServer) addPath(p string) {
	ds.mu.Lock()
	ds.paths = append(ds.paths, p)
	ds.mu.Unlock()
}

func (ds *dlServer) pathsSnapshot() []string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]string, len(ds.paths))
	copy(out, ds.paths)
	return out
}

type redirectCfg struct {
	to string // 重定向目标 URL
}

// atomicCounter 是用于 handler 内计数的线程安全计数器，避免 data race。
type atomicCounter struct{ n int64 }

func (a *atomicCounter) inc() int64 { return atomic.AddInt64(&a.n, 1) }

func newDownloader(t *testing.T) (*downloader, *dlServer) {
	t.Helper()
	ds := &dlServer{status: http.StatusOK}
	ds.body = func() string { return "" }
	ds.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds.addPath(r.URL.Path)
		ds.mu.Lock()
		redirect := ds.redirect
		status := ds.status
		body := ds.body()
		ds.mu.Unlock()
		if redirect != nil {
			http.Redirect(w, r, redirect.to, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ds.srv.Close)

	d := &downloader{
		http:         ds.srv.Client(),
		maxBytes:     defaultMaxBinaryBytes,
		temp:         tempFileCreator{},
		userAgent:    defaultUserAgent,
		downloadBase: ds.srv.URL, // 注入测试下载前缀，覆盖冻结默认
	}
	return d, ds
}

// TestDownloadAsset_FixedURLConstruction 校验下载 URL 由固定前缀+tag+资产名重构。
func TestDownloadAsset_FixedURLConstruction(t *testing.T) {
	payload := []byte("hello-binary")
	d, ds := newDownloader(t)
	ds.body = func() string { return string(payload) }

	wantHash := sha256Hex(payload)
	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", wantHash, t.TempDir(), "")
	if err != nil {
		t.Fatalf("DownloadAsset err = %v", err)
	}
	paths := ds.pathsSnapshot()
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want exactly 1 request", paths)
	}
	want := "/v0.2.0/token-usage-darwin-arm64"
	if paths[0] != want {
		t.Fatalf("path = %q, want %q", paths[0], want)
	}
	// stage 文件内容应与 payload 一致。
	got, err := os.ReadFile(stage)
	if err != nil {
		t.Fatalf("read stage: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stage content mismatch")
	}
}

// TestDownloadAsset_ChecksumMismatchDeletesStage 校验 hash 不匹配时删除 stage 文件。
func TestDownloadAsset_ChecksumMismatchDeletesStage(t *testing.T) {
	payload := []byte("tampered-content")
	d, ds := newDownloader(t)
	ds.body = func() string { return string(payload) }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", "deadbeef", dir, "")
	if err == nil {
		t.Fatal("DownloadAsset(bad hash) err = nil, want error")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	// stage 文件必须已被删除。
	if _, statErr := os.Stat(stage); !os.IsNotExist(statErr) {
		t.Fatalf("stage should be removed, stat err = %v", statErr)
	}
}

// TestDownloadAsset_OversizedDeletesStage 校验响应超过 maxBytes 时删除 stage 并失败。
func TestDownloadAsset_OversizedDeletesStage(t *testing.T) {
	// 用 1KB payload + 256 字节上限触发超限。
	payload := strings.Repeat("x", 1024)
	d, ds := newDownloader(t)
	d.maxBytes = 256
	ds.body = func() string { return payload }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", sha256Hex([]byte(payload)), dir, "")
	if err == nil {
		t.Fatal("DownloadAsset(oversized) err = nil, want error")
	}
	if !errors.Is(err, ErrBinaryTooLarge) {
		t.Fatalf("err = %v, want ErrBinaryTooLarge", err)
	}
	if _, statErr := os.Stat(stage); !os.IsNotExist(statErr) {
		t.Fatalf("stage should be removed on oversized, stat err = %v", statErr)
	}
}

// TestDownloadAsset_NonHTTPSRedirectRejected 校验重定向到非 HTTPS 被拒。
func TestDownloadAsset_NonHTTPSRedirectRejected(t *testing.T) {
	d, ds := newDownloader(t)
	// httptest TLS server 重定向到 http:// 目标。
	ds.redirect = &redirectCfg{to: "http://evil.example/x"}
	dir := t.TempDir()

	_, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", "any", dir, "")
	if err == nil {
		t.Fatal("DownloadAsset(http redirect) err = nil, want error")
	}
}

// TestDownloadAsset_HTTPSRedirectAccepted 校验同一 host 的 HTTPS 重定向被接受。
func TestDownloadAsset_HTTPSRedirectAccepted(t *testing.T) {
	payload := []byte("redir-ok")
	d, ds := newDownloader(t)
	wantHash := sha256Hex(payload)
	dir := t.TempDir()

	// 第一次请求 302 到同 TLS server 的 /real；第二次返回真实内容。
	var hits atomicCounter
	ds.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds.addPath(r.URL.Path)
		if hits.inc() == 1 {
			http.Redirect(w, r, ds.srv.URL+"/real", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", wantHash, dir, "")
	if err != nil {
		t.Fatalf("DownloadAsset(https redirect) err = %v, want nil", err)
	}
	got, err := os.ReadFile(stage)
	if err != nil {
		t.Fatalf("read stage: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stage content mismatch after redirect")
	}
}

// TestDownloadAsset_NonSuccessStatusRejected 校验非 2xx 状态被拒。
func TestDownloadAsset_NonSuccessStatusRejected(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			d, ds := newDownloader(t)
			ds.status = code
			dir := t.TempDir()
			_, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", "any", dir, "")
			if err == nil {
				t.Fatalf("DownloadAsset(%d) err = nil, want error", code)
			}
		})
	}
}

// TestDownloadAsset_UnixExecModeOnBinary 校验下载二进制后 stage 文件获得可执行位。
func TestDownloadAsset_UnixExecModeOnBinary(t *testing.T) {
	payload := []byte("exec-me")
	d, ds := newDownloader(t)
	ds.body = func() string { return string(payload) }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", sha256Hex(payload), dir, "")
	if err != nil {
		t.Fatalf("DownloadAsset err = %v", err)
	}
	info, err := os.Stat(stage)
	if err != nil {
		t.Fatalf("stat stage: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Fatalf("stage mode = %v, want owner-exec bit set (0o100)", info.Mode())
	}
}

// TestDownloadAsset_StageInTargetDir 校验 stage 文件落在目标目录内（同卷原子 rename 前提）。
func TestDownloadAsset_StageInTargetDir(t *testing.T) {
	payload := []byte("place")
	d, ds := newDownloader(t)
	ds.body = func() string { return string(payload) }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", sha256Hex(payload), dir, "")
	if err != nil {
		t.Fatalf("DownloadAsset err = %v", err)
	}
	if filepath.Dir(stage) != dir {
		t.Fatalf("stage dir = %q, want %q", filepath.Dir(stage), dir)
	}
}

// TestDownloadAsset_StagePrefixUsed 校验 stage 文件名使用指定前缀模式。
func TestDownloadAsset_StagePrefixUsed(t *testing.T) {
	payload := []byte("prefix")
	d, ds := newDownloader(t)
	ds.body = func() string { return string(payload) }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", sha256Hex(payload), dir, ".myupdate-*")
	if err != nil {
		t.Fatalf("DownloadAsset err = %v", err)
	}
	base := filepath.Base(stage)
	if !strings.HasPrefix(base, ".myupdate-") {
		t.Fatalf("stage basename = %q, want prefix .myupdate-", base)
	}
}

// TestDownloadAsset_ShortWriteDeletesStage 校验服务器提前断流（短写）导致 hash 不匹配时，
// stage 文件被删除并返回 ErrChecksumMismatch。短写场景下要么 io.Copy 报错、要么
// 实际写出内容比预期短，最终都体现为校验和不匹配。
func TestDownloadAsset_ShortWriteDeletesStage(t *testing.T) {
	full := []byte("full-payload-not-served")
	d, ds := newDownloader(t)
	// 服务器只返回部分内容后关闭连接，模拟短写。
	ds.body = func() string { return string(full[:5]) }
	dir := t.TempDir()

	stage, err := d.DownloadAsset(context.Background(), "v0.2.0", "token-usage-darwin-arm64", sha256Hex(full), dir, "")
	if err == nil {
		t.Fatal("DownloadAsset(short write) err = nil, want error")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if _, statErr := os.Stat(stage); !os.IsNotExist(statErr) {
		t.Fatalf("stage should be removed on short write, stat err = %v", statErr)
	}
}

// TestFetchManifest_DownloadsAndParses 校验 FetchManifest 下载 SHA256SUMS 并解析为 Manifest。
func TestFetchManifest_DownloadsAndParses(t *testing.T) {
	const body = "0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n" +
		"1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-arm64\n" +
		"2222222222222222222222222222222222222222222222222222222222222222  token-usage-windows-amd64.exe\n"
	d, ds := newDownloader(t)
	ds.body = func() string { return body }

	m, err := d.FetchManifest(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("FetchManifest err = %v", err)
	}
	h, ok := m.HashFor("token-usage-darwin-arm64")
	if !ok {
		t.Fatal("HashFor missing darwin-arm64")
	}
	if h != "1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("hash = %q, want 1111...", h)
	}
	// 请求路径应为 SHA256SUMS 资产。
	want := "/v0.2.0/SHA256SUMS"
	paths := ds.pathsSnapshot()
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("path = %v, want %q", paths, want)
	}
}

// TestFetchManifest_MalformedErrors 校验畸形 SHA256SUMS 返回错误。
func TestFetchManifest_MalformedErrors(t *testing.T) {
	d, ds := newDownloader(t)
	ds.body = func() string { return "not a manifest" }

	_, err := d.FetchManifest(context.Background(), "v0.2.0")
	if err == nil {
		t.Fatal("FetchManifest(malformed) err = nil, want error")
	}
}

// TestFetchManifest_NonSuccessErrors 校验非 2xx 下载失败返回错误。
func TestFetchManifest_NonSuccessErrors(t *testing.T) {
	d, ds := newDownloader(t)
	ds.status = http.StatusNotFound

	_, err := d.FetchManifest(context.Background(), "v0.2.0")
	if err == nil {
		t.Fatal("FetchManifest(404) err = nil, want error")
	}
}

// TestNewDownloader_Defaults 校验 NewDownloader 构造的下载器使用默认配置与内置 HTTPS 客户端。
func TestNewDownloader_Defaults(t *testing.T) {
	d := NewDownloader(nil)
	if d.maxBytes != defaultMaxBinaryBytes {
		t.Fatalf("maxBytes = %d, want %d", d.maxBytes, defaultMaxBinaryBytes)
	}
	if d.userAgent != defaultUserAgent {
		t.Fatalf("userAgent = %q, want %q", d.userAgent, defaultUserAgent)
	}
	if d.downloadBase != githubDownloadBase {
		t.Fatalf("downloadBase = %q, want %q", d.downloadBase, githubDownloadBase)
	}
	if d.http == nil {
		t.Fatal("http must not be nil")
	}
	if d.temp == nil {
		t.Fatal("temp must not be nil")
	}
}
