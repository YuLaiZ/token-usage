package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// github.go 实现 GitHub Release 查询客户端。
//
// 仅信任冻结的官方仓库 YuLaiZ/token-usage 与固定 API 根：
//
//	https://api.github.com/repos/YuLaiZ/token-usage
//
// 查询规则：
//   - tag 为空字符串 → 请求固定 /releases/latest（稳定版入口）；
//     该端点 404 视为「没有稳定 Release」的领域结果（ErrNoStableRelease），
//     不当作通用网络错误冒泡。
//   - tag 非空 → 请求固定 /releases/tags/<exact tag>；
//     该端点 404 视为「指定版本不存在」的用户错误（ErrVersionNotFound）。
//   - 其它非 2xx 视为瞬时错误，原样返回。
//
// 响应处理：
//   - 仅解码 tag_name / draft / prerelease / assets[].name 四类字段，
//     绝不信任 Release JSON 中任何 browser_download_url（下载 URL 始终由
//     固定 GitHub 下载前缀 + 校验过的 tag + 资产名重构，见 download.go）。
//   - 限制响应体大小，超过 maxBody 一律拒绝。
//   - 通过 ValidateRelease 校验 tag/draft/prerelease/资产集合一致性：
//     latest 端点把稳定 tag 错标成 prerelease 也会被拒（防止 mis-tag 冒充稳定版）。

// githubAPIBase 是冻结的官方 Release API 根。
// 下载与查询只允许访问该根下的子路径，不接受 Release JSON 提供的任意域名。
const githubAPIBase = "https://api.github.com/repos/YuLaiZ/token-usage"

// defaultUserAgent 是自更新流程对 GitHub API 的固定 User-Agent。
// GitHub API 要求请求带 User-Agent，否则可能被拒；此处使用应用标识而非默认 Go 客户端。
const defaultUserAgent = "token-usage-self-update"

// defaultHTTPTimeout 是单次请求的固定超时上限，杜绝无限等待挂死自更新。
const defaultHTTPTimeout = 30 * time.Second

// defaultMaxReleaseBody 是 Release JSON 响应体大小上限。
// Release 元数据远小于该值；超过即视为异常（滥用或攻击），拒绝解析。
const defaultMaxReleaseBody = 4 << 20 // 4 MiB

// ErrNoStableRelease 表示 latest 端点无稳定 Release（404）。
// 这是面向用户的领域结果，调用方应给出「没有稳定 Release」的明确提示，
// 而非当作网络故障重试。
var ErrNoStableRelease = errors.New(ui.Bi("no stable release available", "没有可用的稳定 Release"))

// ErrVersionNotFound 表示请求的精确 tag 不存在（tags 端点 404）。
// 这是用户错误（输入了不存在的版本号），调用方应提示「指定版本不存在」。
var ErrVersionNotFound = errors.New(ui.Bi("specified version not found", "指定的版本不存在"))

// githubReleaseClient 是 ReleaseClient 的生产实现：经 HTTPDoer 访问冻结的 GitHub API。
// baseURL / userAgent / maxBody 均为可注入字段，便于测试用 httptest.Server 覆盖。
// 请求超时由注入的 HTTPDoer（生产用 *http.Client.Timeout）承担，不在本结构体重复。
type githubReleaseClient struct {
	http      HTTPDoer
	baseURL   string
	userAgent string
	maxBody   int64
}

// NewGithubReleaseClient 构造默认的 ReleaseClient：使用注入的 HTTPDoer（nil 则用内置
// HTTPS-only 客户端，携带默认超时）、冻结的官方 baseURL 与固定 User-Agent。
func NewGithubReleaseClient(doer HTTPDoer) *githubReleaseClient {
	if doer == nil {
		doer = newHTTPSOnlyClient(defaultHTTPTimeout)
	}
	return &githubReleaseClient{
		http:      doer,
		baseURL:   githubAPIBase,
		userAgent: defaultUserAgent,
		maxBody:   defaultMaxReleaseBody,
	}
}

// FetchRelease 获取指定 tag（空字符串表示 latest）的 Release 元数据与资产清单。
//
// latest 端点 404 → ErrNoStableRelease；显式 tag 端点 404 → ErrVersionNotFound；
// 草稿、prerelease 与 tag 不一致、资产集合不合规等返回 ValidateRelease 的错误；
// 畸形 JSON、超时、响应过大、5xx 等返回瞬时错误。
//
// wantTag 选取按端点区分：latest 端点无请求 tag，以 JSON 回显的 tag_name 为准；
// 显式 tag 端点要求 JSON tag_name 与请求 tag 严格一致（防 mis-tag / MITM 把
// 请求 v0.3.0-rc.1 的响应换成 v0.5.0 的 Release）。
func (c *githubReleaseClient) FetchRelease(ctx context.Context, tag string) (*Release, error) {
	path := "/releases/latest"
	if tag != "" {
		path = "/releases/tags/" + escapeTag(tag)
	}
	url := c.baseURL + path

	data, err := c.fetch(ctx, url)
	if err != nil {
		return nil, err
	}

	rel, err := decodeRelease(data)
	if err != nil {
		return nil, err
	}
	// 解析 tag 为结构化版本，供 prerelease 一致性校验。
	ver, verr := ParseVersion(rel.Tag)
	if verr != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("failed to parse release tag %q", rel.Tag),
			fmt.Sprintf("release tag %q 解析失败", rel.Tag),
		), verr)
	}
	rel.Version = ver
	// 显式 tag 端点要求回显与请求一致；latest 端点以 JSON tag_name 为权威。
	wantTag := rel.Tag
	if tag != "" {
		wantTag = tag
	}
	// allowPrerelease 由版本本身是否为候选版决定：候选版必须 Prerelease=true，
	// 稳定版必须 Prerelease=false（latest 端点尤其要防 mis-tag）。
	if err := ValidateRelease(rel, wantTag, ver.IsPrerelease()); err != nil {
		return nil, err
	}
	return rel, nil
}

// fetch 执行单次 GET，返回限流后的响应体。把 404 按 endpoint 语义翻译为 sentinel 错误。
func (c *githubReleaseClient) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to build request", "构造请求失败"), err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to request GitHub release", "请求 GitHub Release 失败"), err)
	}
	defer resp.Body.Close()

	// 404 按 endpoint 语义翻译：latest → 无稳定版；tags → 版本不存在。
	if resp.StatusCode == http.StatusNotFound {
		if strings.HasSuffix(url, "/releases/latest") {
			return nil, ErrNoStableRelease
		}
		return nil, ErrVersionNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("GitHub release query returned non-success status %d", resp.StatusCode),
			fmt.Sprintf("GitHub Release 查询返回非成功状态 %d", resp.StatusCode),
		))
	}

	// 限制响应体大小：超过 maxBody+1 即拒绝（+1 让 LimitReader 能探测到超限）。
	lr := io.LimitReader(resp.Body, c.maxBody+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to read response body", "读取响应体失败"), err)
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("response body exceeds the limit of %d bytes", c.maxBody),
			fmt.Sprintf("响应体超过上限 %d 字节", c.maxBody),
		))
	}
	return body, nil
}

// escapeTag 对 tag 做最小 URL path 段转义，防止 tag 含非法字符破坏固定 URL。
// tag 已被 ParseVersion 严格校验（v + 数字 + . + 可选 rc.N），此处仅做防御性转义。
func escapeTag(tag string) string {
	for i := 0; i < len(tag); i++ {
		// 仅 ASCII 字母数字、点、连字符、下划线无需转义；其余按字节转义。
		c := tag[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_' {
			continue
		}
		return url.PathEscape(tag)
	}
	return tag
}

// githubReleaseJSON 是 GitHub Release API 响应的最小子集，仅解码自更新需要的字段。
// browser_download_url 等字段被显式忽略（下载 URL 始终由固定前缀重构）。
type githubReleaseJSON struct {
	TagName    string            `json:"tag_name"`
	Draft      bool              `json:"draft"`
	Prerelease bool              `json:"prerelease"`
	Assets     []githubAssetJSON `json:"assets"`
}

// githubAssetJSON 是 Release 中单个资产的解码结构，仅取 name。
type githubAssetJSON struct {
	Name string `json:"name"`
}

// decodeRelease 把 GitHub Release API 的 JSON 解码为内部 Release 值对象。
// 解码后不保留任何下载 URL；Assets 以资产名为键构造 map。
func decodeRelease(data []byte) (*Release, error) {
	var raw githubReleaseJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to parse release JSON", "解析 Release JSON 失败"), err)
	}
	if raw.TagName == "" {
		return nil, errors.New(ui.Bi("release is missing the tag_name field", "release 缺少 tag_name 字段"))
	}
	assets := make(map[string]Asset, len(raw.Assets))
	for _, a := range raw.Assets {
		if a.Name == "" {
			continue
		}
		assets[a.Name] = Asset{Name: a.Name}
	}
	return &Release{
		Tag:        raw.TagName,
		Draft:      raw.Draft,
		Prerelease: raw.Prerelease,
		Assets:     assets,
	}, nil
}
