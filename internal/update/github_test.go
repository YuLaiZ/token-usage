package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// github_test.go 校验 GitHub Release 查询客户端（githubReleaseClient）：
//   - latest 与 tags/<tag> 端点正确路由；
//   - 404 / 500 等状态码映射为对应错误；
//   - 畸形 JSON、draft、错误 prerelease、错误资产集合被拒；
//   - 响应体超过上限被拒；
//   - 请求携带固定 User-Agent；
//   - latest 端点把错误标成 prerelease 的 Release 拒绝（mis-tag 检查）。
//
// 全部测试基于 httptest.Server，绝不访问真实 GitHub。

// releaseJSON 构造一份合法/可控的 GitHub Release API JSON。
// assets 为 nil 时使用四项冻结资产名；draft/prerelease/tag 可覆盖默认值。
func releaseJSON(tag string, draft, prerelease bool, assets []string) string {
	if assets == nil {
		assets = []string{
			"token-usage-darwin-arm64",
			"token-usage-darwin-amd64",
			"token-usage-windows-amd64.exe",
			"SHA256SUMS",
		}
	}
	var b strings.Builder
	b.WriteString(`{"tag_name":` + jsonStr(tag) + `,"draft":` + jsonBool(draft) + `,"prerelease":` + jsonBool(prerelease) + `,"assets":[`)
	for i, a := range assets {
		if i > 0 {
			b.WriteByte(',')
		}
		// 故意写入 browser_download_url 字段，验证客户端不信任它（只用 name）。
		b.WriteString(`{"name":` + jsonStr(a) + `,"browser_download_url":"https://evil.example/x"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func jsonStr(s string) string {
	return `"` + s + `"`
}

func jsonBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// newGithubClient 用 httptest.Server 构造一个指向 server.URL 的 githubReleaseClient，
// 便于在测试中观察请求路径与状态码。maxBody 设为较小值以测试超限。
// paths 在 handler goroutine 写、测试 goroutine 读，用 mu 保护避免 data race。
func newGithubClient(t *testing.T, status int, body string) (*githubReleaseClient, *httptest.Server, *pathRecorder) {
	t.Helper()
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := &githubReleaseClient{
		http:      srv.Client(),
		baseURL:   srv.URL + "/repos/YuLaiZ/token-usage",
		userAgent: "token-usage-self-update",
		maxBody:   defaultMaxReleaseBody,
	}
	return c, srv, rec
}

// pathRecorder 线程安全地记录请求路径，供测试断言路由。
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) add(path string) {
	p.mu.Lock()
	p.paths = append(p.paths, path)
	p.mu.Unlock()
}

func (p *pathRecorder) slice() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.paths))
	copy(out, p.paths)
	return out
}

// TestGithubClient_LatestRouting 校验 tag=="" 时请求固定 latest 端点并解析 Release。
func TestGithubClient_LatestRouting(t *testing.T) {
	body := releaseJSON("v0.2.0", false, false, nil)
	c, _, rec := newGithubClient(t, http.StatusOK, body)

	r, err := c.FetchRelease(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchRelease(\"\") = err %v, want nil", err)
	}
	if r.Tag != "v0.2.0" {
		t.Fatalf("Tag = %q, want v0.2.0", r.Tag)
	}
	if r.Draft || r.Prerelease {
		t.Fatalf("Draft=%v Prerelease=%v, want both false", r.Draft, r.Prerelease)
	}
	if len(r.Assets) != 4 {
		t.Fatalf("len(Assets) = %d, want 4", len(r.Assets))
	}
	paths := rec.slice()
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/releases/latest") {
		t.Fatalf("path = %v, want suffix /releases/latest", paths)
	}
}

// TestGithubClient_TagRouting 校验非空 tag 请求 tags/<tag> 端点。
func TestGithubClient_TagRouting(t *testing.T) {
	body := releaseJSON("v0.3.0-rc.1", false, true, nil)
	c, _, rec := newGithubClient(t, http.StatusOK, body)

	r, err := c.FetchRelease(context.Background(), "v0.3.0-rc.1")
	if err != nil {
		t.Fatalf("FetchRelease err = %v, want nil", err)
	}
	if r.Tag != "v0.3.0-rc.1" {
		t.Fatalf("Tag = %q, want v0.3.0-rc.1", r.Tag)
	}
	if !r.Prerelease {
		t.Fatalf("Prerelease = false, want true")
	}
	want := "/releases/tags/v0.3.0-rc.1"
	paths := rec.slice()
	if len(paths) != 1 || !strings.HasSuffix(paths[0], want) {
		t.Fatalf("path = %v, want suffix %s", paths, want)
	}
}

// TestGithubClient_Latest404 校验 latest 端点 404 返回 ErrNoStableRelease（领域结果）。
func TestGithubClient_Latest404(t *testing.T) {
	c, _, _ := newGithubClient(t, http.StatusNotFound, `{"message":"Not Found"}`)

	_, err := c.FetchRelease(context.Background(), "")
	if !errors.Is(err, ErrNoStableRelease) {
		t.Fatalf("FetchRelease(latest 404) err = %v, want ErrNoStableRelease", err)
	}
}

// TestGithubClient_Tag404 校验显式 tag 端点 404 返回 ErrVersionNotFound（用户错误）。
func TestGithubClient_Tag404(t *testing.T) {
	c, _, _ := newGithubClient(t, http.StatusNotFound, `{"message":"Not Found"}`)

	_, err := c.FetchRelease(context.Background(), "v9.9.9")
	if !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("FetchRelease(tag 404) err = %v, want ErrVersionNotFound", err)
	}
}

// TestGithubClient_500 校验 5xx 返回瞬时错误（既非 no-stable 也非 not-found）。
func TestGithubClient_500(t *testing.T) {
	for _, tag := range []string{"", "v1.0.0"} {
		c, _, _ := newGithubClient(t, http.StatusInternalServerError, `{"message":"boom"}`)
		_, err := c.FetchRelease(context.Background(), tag)
		if err == nil {
			t.Fatalf("FetchRelease(tag=%q 500) err = nil, want error", tag)
		}
		if errors.Is(err, ErrNoStableRelease) || errors.Is(err, ErrVersionNotFound) {
			t.Fatalf("FetchRelease(tag=%q 500) err = %v, want transient (not sentinel)", tag, err)
		}
	}
}

// TestGithubClient_MalformedJSON 校验畸形 JSON 返回错误而非半解析结果。
func TestGithubClient_MalformedJSON(t *testing.T) {
	c, _, _ := newGithubClient(t, http.StatusOK, `{not json`)

	_, err := c.FetchRelease(context.Background(), "")
	if err == nil {
		t.Fatal("FetchRelease(malformed) err = nil, want error")
	}
	if errors.Is(err, ErrNoStableRelease) {
		t.Fatal("malformed JSON must not map to ErrNoStableRelease")
	}
}

// TestGithubClient_DraftRejected 校验 draft=true 被拒。
func TestGithubClient_DraftRejected(t *testing.T) {
	body := releaseJSON("v0.2.0", true, false, nil)
	c, _, _ := newGithubClient(t, http.StatusOK, body)

	_, err := c.FetchRelease(context.Background(), "v0.2.0")
	if err == nil {
		t.Fatal("FetchRelease(draft) err = nil, want error")
	}
}

// TestGithubClient_WrongPrereleaseOnLatest 校验 latest 端点返回稳定 tag 但标 prerelease=true 被拒
// （防止 GitHub 端 mis-tag 把候选版冒充 latest stable）。
func TestGithubClient_WrongPrereleaseOnLatest(t *testing.T) {
	body := releaseJSON("v0.2.0", false, true, nil)
	c, _, _ := newGithubClient(t, http.StatusOK, body)

	_, err := c.FetchRelease(context.Background(), "")
	if err == nil {
		t.Fatal("FetchRelease(latest mis-tagged prerelease) err = nil, want error")
	}
	if errors.Is(err, ErrNoStableRelease) {
		t.Fatal("mis-tagged prerelease must surface as validation error, not ErrNoStableRelease")
	}
}

// TestGithubClient_RcMisTaggedNotPrerelease 校验 rc 版本 tag 但 prerelease=false 被拒
// （版本与元数据矛盾）。
func TestGithubClient_RcMisTaggedNotPrerelease(t *testing.T) {
	body := releaseJSON("v0.2.0-rc.1", false, false, nil)
	c, _, _ := newGithubClient(t, http.StatusOK, body)

	_, err := c.FetchRelease(context.Background(), "v0.2.0-rc.1")
	if err == nil {
		t.Fatal("FetchRelease(rc with prerelease=false) err = nil, want error")
	}
}

// TestGithubClient_RcAllowedOnExplicitTag 校验显式请求 rc tag 且 prerelease=true 时通过。
func TestGithubClient_RcAllowedOnExplicitTag(t *testing.T) {
	body := releaseJSON("v0.2.0-rc.1", false, true, nil)
	c, _, _ := newGithubClient(t, http.StatusOK, body)

	r, err := c.FetchRelease(context.Background(), "v0.2.0-rc.1")
	if err != nil {
		t.Fatalf("FetchRelease(rc explicit) err = %v, want nil", err)
	}
	if !r.Prerelease {
		t.Fatal("Prerelease = false, want true")
	}
}

// TestGithubClient_TagEchoMismatch 校验显式请求 tag 端点时，JSON 回显的 tag_name 与
// 请求 tag 不一致被拒（防 mis-tag / MITM：请求 v0.3.0-rc.1 但响应换成 v0.5.0 的 Release）。
// 错误信息应体现 tag 不匹配，便于定位。
func TestGithubClient_TagEchoMismatch(t *testing.T) {
	// JSON tag_name 故意与请求 tag 不同：请求 v0.3.0-rc.1，响应 v0.5.0。
	body := releaseJSON("v0.5.0", false, false, nil)
	c, _, _ := newGithubClient(t, http.StatusOK, body)

	_, err := c.FetchRelease(context.Background(), "v0.3.0-rc.1")
	if err == nil {
		t.Fatal("FetchRelease(tag echo mismatch) err = nil, want error")
	}
	if !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("err = %v, want error mentioning 不匹配", err)
	}
}

// TestGithubClient_WrongAssetsRejected 校验资产集合错误（缺项/多项）被拒。
func TestGithubClient_WrongAssetsRejected(t *testing.T) {
	cases := []struct {
		name   string
		assets []string
	}{
		{"missing sums", []string{"token-usage-darwin-arm64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe"}},
		{"extra asset", []string{"token-usage-darwin-arm64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe", "SHA256SUMS", "README.md"}},
		{"empty", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := releaseJSON("v0.2.0", false, false, c.assets)
			cli, _, _ := newGithubClient(t, http.StatusOK, body)
			_, err := cli.FetchRelease(context.Background(), "v0.2.0")
			if err == nil {
				t.Fatalf("FetchRelease(wrong assets %s) err = nil, want error", c.name)
			}
		})
	}
}

// TestGithubClient_OversizedBodyRejected 校验响应体超过 maxBody 上限被拒。
func TestGithubClient_OversizedBodyRejected(t *testing.T) {
	body := releaseJSON("v0.2.0", false, false, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := &githubReleaseClient{
		http:      srv.Client(),
		baseURL:   srv.URL + "/repos/YuLaiZ/token-usage",
		userAgent: "token-usage-self-update",
		maxBody:   10, // 远小于合法 body
	}
	_, err := c.FetchRelease(context.Background(), "v0.2.0")
	if err == nil {
		t.Fatal("FetchRelease(oversized) err = nil, want error")
	}
}

// TestGithubClient_UserAgentHeader 校验请求携带固定 User-Agent。
func TestGithubClient_UserAgentHeader(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, releaseJSON("v0.2.0", false, false, nil))
	}))
	t.Cleanup(srv.Close)
	c := &githubReleaseClient{
		http:      srv.Client(),
		baseURL:   srv.URL + "/repos/YuLaiZ/token-usage",
		userAgent: "token-usage-self-update",
		maxBody:   defaultMaxReleaseBody,
	}
	if _, err := c.FetchRelease(context.Background(), ""); err != nil {
		t.Fatalf("FetchRelease err = %v", err)
	}
	if gotUA != "token-usage-self-update" {
		t.Fatalf("User-Agent = %q, want token-usage-self-update", gotUA)
	}
}

// TestGithubClient_TimeoutApplied 校验请求超时生效（用一个故意慢的 handler
// 配合很短的 ctx 超时触发失败）。超时由注入的 HTTPDoer 与 ctx 协同承担。
func TestGithubClient_TimeoutApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不写 header，阻塞到客户端超时关闭连接。
		select {
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	c := &githubReleaseClient{
		http:      srv.Client(),
		baseURL:   srv.URL + "/repos/YuLaiZ/token-usage",
		userAgent: "token-usage-self-update",
		maxBody:   defaultMaxReleaseBody,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	_, err := c.FetchRelease(ctx, "")
	if err == nil {
		t.Fatal("FetchRelease(timeout) err = nil, want error")
	}
}

// TestGithubClient_NoTagFieldRejected 校验 Release JSON 缺少 tag_name 被拒。
func TestGithubClient_NoTagFieldRejected(t *testing.T) {
	body := `{"draft":false,"prerelease":false,"assets":[{"name":"SHA256SUMS"}]}`
	c, _, _ := newGithubClient(t, http.StatusOK, body)
	_, err := c.FetchRelease(context.Background(), "")
	if err == nil {
		t.Fatal("FetchRelease(no tag) err = nil, want error")
	}
}

// TestGithubClient_NewDefaultUsesFixedBase 校验 NewGithubReleaseClient 构造的客户端
// 使用冻结的官方 baseURL 与默认 User-Agent。
func TestGithubClient_NewDefaultUsesFixedBase(t *testing.T) {
	c := NewGithubReleaseClient(nil)
	if c.baseURL != githubAPIBase {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, githubAPIBase)
	}
	if c.userAgent != defaultUserAgent {
		t.Fatalf("userAgent = %q, want %q", c.userAgent, defaultUserAgent)
	}
	if c.maxBody != defaultMaxReleaseBody {
		t.Fatalf("maxBody = %d, want %d", c.maxBody, defaultMaxReleaseBody)
	}
	if c.http == nil {
		t.Fatal("http client must not be nil")
	}
}
