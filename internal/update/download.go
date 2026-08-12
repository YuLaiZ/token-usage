package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// download.go 实现资产下载器：从固定 GitHub 下载前缀拉取 SHA256SUMS 与平台二进制，
// 流式写入目标同目录的私有 stage 文件，边写边算 SHA256，写完校验 hash。
//
// 下载 URL 始终由固定前缀 + 校验过的 tag + 资产名重构，绝不采用 Release JSON 提供的
// 任意 URL。只接受成功 HTTP 状态，只跟随从该固定初始请求产生的标准 HTTPS 重定向链。

// githubDownloadBase 是冻结的官方下载前缀。
// 资产下载 URL = githubDownloadBase + "/<tag>/<asset-name>"。
const githubDownloadBase = "https://github.com/YuLaiZ/token-usage/releases/download"

// defaultMaxBinaryBytes 是二进制 stage 文件的字节上限（256 MiB）。
// 超过即删除 stage 并失败，防止恶意超大响应撑爆磁盘。
const defaultMaxBinaryBytes int64 = 256 << 20

// defaultDownloadTimeout 是下载器 HTTP 客户端的全局超时上限。
// 清单查询（SHA256SUMS，<1KB）与二进制下载（~24MB）共用同一客户端；
// 清单由服务端快速返回，实际超时只影响大文件慢网络下载。
// 30s 在慢网络（VM/代理）上不够下载 24MB（实测 149KB/s 需 ~3min）。
// 20 分钟覆盖到 ~20KB/s 的极慢网络，同时不让断开连接挂太久。
const defaultDownloadTimeout = 20 * time.Minute

// stageFilePattern 是临时 stage 文件的默认命名模式，前缀固定以便清理。
// Windows 上加 .exe 扩展名且避开 "update" 字样：版本探针需 exec 执行 stage 文件，
// 实测 Windows 安全策略会拦截文件名含 "update" 的可执行文件（返回
// ERROR_ELEVATION_REQUIRED，复制/重命名后即正常，"token-usage-stage-*" 通过）。
var stageFilePattern = func() string {
	if runtime.GOOS == "windows" {
		return "token-usage-stage-*.exe"
	}
	return ".token-usage-update-*"
}()

// ErrNonHTTPSRedirect 表示下载/查询链路中出现非 HTTPS 重定向目标，一律拒绝。
var ErrNonHTTPSRedirect = errors.New("拒绝非 HTTPS 重定向")

// ErrChecksumMismatch 表示下载内容 SHA256 与预期清单 hash 不一致。
var ErrChecksumMismatch = errors.New("下载内容校验和不匹配")

// ErrBinaryTooLarge 表示下载响应超过 maxBytes 上限。
var ErrBinaryTooLarge = errors.New("下载内容超过大小上限")

// newHTTPSOnlyClient 构造一个仅跟随 HTTPS 重定向的 *http.Client。
// CheckRedirect 拒绝任何 scheme != https 的目标，确保下载链路全程加密；
// 同时对重定向次数施加常规上限（沿用 http.Client 默认行为）。
func newHTTPSOnlyClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("%w: %s", ErrNonHTTPSRedirect, req.URL.String())
			}
			return nil
		},
	}
}

// AssetDownloader 在可信分支下载目标 Release 资产并校验 SHA256，返回 stage 绝对路径。
// 生产实现用 *downloader（NewDownloader 返回），与 ManifestFetcher 复用同一对象——
// 一个 NewDownloader(nil) 既是 ManifestFetcher 又是 AssetDownloader。
// 测试可注入基于 httptest.Server 的真实 *downloader（验证下载集成），或内存 fake。
// Service.Apply 在来源校验通过后调用本接口下载目标资产；未注入时保持向后兼容，
// 只到 ReadyToInstall，不下载（stagePath 为空，由注入的 Installer 自行处理或测试注入 fake）。
type AssetDownloader interface {
	DownloadAsset(ctx context.Context, tag, assetName, expectedHash, targetDir, stagePrefix string) (string, error)
}

// downloader 是资产下载的生产实现，所有外部依赖通过字段注入：
//   - http：HTTPS-only HTTPDoer（生产用内置 *http.Client）；
//   - temp：stage 文件创建 seam（生产用 os.CreateTemp）；
//   - downloadBase：下载前缀，生产为冻结 githubDownloadBase，测试可注入 httptest.Server.URL；
//   - userAgent / maxBytes：固定可调参数。
//
// 请求超时由注入的 HTTPDoer（生产用 *http.Client.Timeout）承担，不在本结构体重复。
type downloader struct {
	http         HTTPDoer
	temp         TempFileCreator
	downloadBase string
	userAgent    string
	maxBytes     int64
}

// NewDownloader 构造默认下载器：doer 为 nil 时用内置 HTTPS-only 客户端（携带默认超时），
// temp 用 os.CreateTemp，downloadBase 为冻结的官方下载前缀。
func NewDownloader(doer HTTPDoer) *downloader {
	if doer == nil {
		doer = newHTTPSOnlyClient(defaultDownloadTimeout)
	}
	return &downloader{
		http:         doer,
		temp:         osTempFileCreator{},
		downloadBase: githubDownloadBase,
		userAgent:    defaultUserAgent,
		maxBytes:     defaultMaxBinaryBytes,
	}
}

// osTempFileCreator 是 TempFileCreator 的生产实现，直接转 os.CreateTemp。
type osTempFileCreator struct{}

func (osTempFileCreator) CreateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// DownloadAsset 下载指定资产到目标目录的私有 stage 文件，校验 SHA256 后返回 stage 路径。
//
// 流程：
//  1. 由固定 downloadBase + tag + assetName 构造 URL（绝不使用 Release JSON 提供的 URL）；
//  2. 在 targetDir 创建 stage 临时文件（保证与目标同目录同卷，便于后续原子 rename）；
//  3. 流式写入 stage，边写边算 SHA256，限制总字节数 <= maxBytes；
//  4. 只接受 2xx 状态；超过 maxBytes 删除 stage 并返回 ErrBinaryTooLarge；
//  5. 写完 Sync + Close，比对 SHA256 == expectedHash；
//  6. hash 不匹配删除 stage 并返回 ErrChecksumMismatch；
//  7. 二进制类资产（NeedsUnixExecMode）为 stage 设置 owner-exec 权限位。
//
// stagePrefix 为空时使用默认 stageFilePattern。
func (d *downloader) DownloadAsset(ctx context.Context, tag, assetName, expectedHash, targetDir, stagePrefix string) (string, error) {
	if stagePrefix == "" {
		stagePrefix = stageFilePattern
	}
	pattern := normalizeStagePattern(stagePrefix)

	stage, err := d.temp.CreateTemp(targetDir, pattern)
	if err != nil {
		return "", fmt.Errorf("创建 stage 文件失败: %w", err)
	}
	stagePath := stage.Name()

	// 任何失败路径都保证删除已创建的 stage，杜绝残留半成品。
	cleanup := func() { _ = os.Remove(stagePath) }

	url := buildDownloadURLBase(d.downloadBase, tag, assetName)
	written, err := d.streamToStage(ctx, url, stage)
	if err != nil {
		_ = stage.Close()
		cleanup()
		return "", err
	}
	// 关闭 stage 前先 Sync，确保数据落盘。
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		cleanup()
		return "", fmt.Errorf("stage 同步失败: %w", err)
	}
	if err := stage.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("stage 关闭失败: %w", err)
	}

	// hash 校验：边写边算的 sum 与清单预期 hash 比对。
	if err := verifyHash(written, expectedHash); err != nil {
		cleanup()
		return "", err
	}

	// 二进制类资产（POSIX）设置 owner-exec 权限位，与目标可执行文件兼容。
	if NeedsUnixExecMode(goosForAsset(assetName)) {
		if err := setExecMode(stagePath); err != nil {
			cleanup()
			return "", fmt.Errorf("设置 stage 权限失败: %w", err)
		}
	}
	return stagePath, nil
}

// streamToStage 流式下载 URL 到 stage 文件，边写边算 SHA256，返回写出的 sum。
// 超过 maxBytes 时立即中断并返回 ErrBinaryTooLarge。
func (d *downloader) streamToStage(ctx context.Context, requestURL string, stage *os.File) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", d.userAgent)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发起下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("下载返回非成功状态 %d", resp.StatusCode)
	}

	h := sha256.New()
	// 限流读取：每次最多拷贝 maxBytes+1 字节，超限即拒绝。
	limited := io.LimitReader(resp.Body, d.maxBytes+1)
	// 同时写入 stage 文件与 hash 累加器，边落盘边计算 SHA256。
	sink := io.MultiWriter(stage, h)
	n, err := io.Copy(sink, limited)
	if err != nil {
		return nil, fmt.Errorf("写入 stage 失败: %w", err)
	}
	if n > d.maxBytes {
		return nil, fmt.Errorf("%w: 已写 %d 字节", ErrBinaryTooLarge, n)
	}
	return h.Sum(nil), nil
}

// FetchManifest 下载并解析目标 Release 的 SHA256SUMS 清单，返回 Manifest。
// URL 仍由固定前缀 + tag + SHA256SUMS 资产名重构。
func (d *downloader) FetchManifest(ctx context.Context, tag string) (*Manifest, error) {
	url := buildDownloadURLBase(d.downloadBase, tag, SumsAssetName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造清单请求失败: %w", err)
	}
	req.Header.Set("User-Agent", d.userAgent)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载清单失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("清单下载返回非成功状态 %d", resp.StatusCode)
	}
	// 清单远小于二进制，复用二进制上限即可。
	lr := io.LimitReader(resp.Body, d.maxBytes+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("读取清单失败: %w", err)
	}
	if int64(len(body)) > d.maxBytes {
		return nil, fmt.Errorf("%w: 清单超过上限", ErrBinaryTooLarge)
	}
	m, err := ParseManifest(body)
	if err != nil {
		return nil, fmt.Errorf("解析清单失败: %w", err)
	}
	return m, nil
}

// buildDownloadURLBase 由给定前缀、校验过的 tag 与资产名构造下载 URL。
// tag 与 assetName 经 url.PathEscape 安全转义，杜绝路径注入。
// 用 base 参数而非全局常量，便于测试注入 httptest.Server.URL。
func buildDownloadURLBase(base, tag, assetName string) string {
	return base + "/" + url.PathEscape(tag) + "/" + url.PathEscape(assetName)
}

// verifyHash 比对写出的 sum（裸字节）与预期 hash（64 位小写 hex 字符串）。
func verifyHash(sum []byte, expectedHex string) error {
	got := hex.EncodeToString(sum)
	if got != expectedHex {
		return fmt.Errorf("%w: got %s want %s", ErrChecksumMismatch, got, expectedHex)
	}
	return nil
}

// goosForAsset 从资产名反推 GOOS，仅用于决定是否设置可执行位。
// 资产名已被 ValidateRelease 严格冻结，此处映射是确定的：Windows 资产名含
// "-windows-"，其余为 POSIX 二进制（需可执行位）。
func goosForAsset(assetName string) string {
	if strings.Contains(assetName, "-windows-") {
		return "windows"
	}
	return "posix" // 非 Windows 资产一律需要可执行位（NeedsUnixExecMode 返回 true）
}

// setExecMode 为 stage 文件补上 owner-exec 权限位（与目标可执行文件兼容）。
// 保留原有权限，仅置位 0o100。
func setExecMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := info.Mode() | 0o100
	return os.Chmod(path, mode)
}

// normalizeStagePattern 确保传入的 stage 命名模式非空且含通配。
// 调用方传入的模式已含 *；为空时回退到默认 stageFilePattern。
func normalizeStagePattern(pattern string) string {
	if pattern == "" {
		return stageFilePattern
	}
	return pattern
}
