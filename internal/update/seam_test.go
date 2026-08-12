package update

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// 本文件是 update 包的测试骨架：为每个 seam 提供确定性 fake 实现，
// 并以一个 trivial 占位测试证明「internal/update 包 + 全部 seam 接口可编译通过」。
//
// 设计原则（与 internal/control/process_test.go 一致）：
//   - 不访问真实 GitHub / 网络 / daemon / 真实 HOME；
//   - 所有临时路径来自 t.TempDir()；
//   - 所有 fake 线程安全（mu + 计数器 / 切片），避免并发 update 测试竞态；
//   - fake 不引入真实 sleep，状态切换由调用触发，便于确定性断言。
//
// 本骨架不写业务逻辑测试；具体行为测试由同包其它 *_test.go 补充。

// ---- HTTP / Release fakes ----

// fakeHTTPDoer 记录每次请求的 URL / User-Agent，按预置响应表返回。
// responses: URL 后缀 → (status, body)。未命中返回 404。
type fakeHTTPDoer struct {
	mu        sync.Mutex
	requests  []recordedReq
	responses map[string]fakeResponse
}

type recordedReq struct {
	URL         string
	UserAgent   string
	Method      string
	BodySnippet string
}

type fakeResponse struct {
	status int
	body   []byte
}

func newFakeHTTPDoer() *fakeHTTPDoer {
	return &fakeHTTPDoer{responses: map[string]fakeResponse{}}
}

func (f *fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snippet := ""
	if req.Body != nil {
		buf := make([]byte, 64)
		n, _ := req.Body.Read(buf)
		snippet = string(buf[:n])
	}
	f.requests = append(f.requests, recordedReq{
		URL:         req.URL.String(),
		UserAgent:   req.UserAgent(),
		Method:      req.Method,
		BodySnippet: snippet,
	})
	for suffix, resp := range f.responses {
		if suffix == "" || endsWith(req.URL.String(), suffix) {
			return &http.Response{
				StatusCode: resp.status,
				Body:       ioNopCloser(bytes.NewReader(resp.body)),
				Header:     make(http.Header),
			}, nil
		}
	}
	return &http.Response{StatusCode: http.StatusNotFound, Body: ioNopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
}

// fakeReleaseClient 实现 ReleaseClient，返回预置 Release，不触达网络。
// byTag 非空时按精确 tag 查找；命中返回对应 Release，未命中回退到 release 字段。
// errByTag 按 tag 注入查询错误（优先于 fetchErr）；fetchErr 对所有 tag 生效。
// 这样可在一次测试中同时模拟「当前版本 Release」与「目标版本 Release」两条查询，
// 并让目标查询失败而当前版本查询成功（或反之）。
type fakeReleaseClient struct {
	mu       sync.Mutex
	fetches  []string
	release  *Release
	byTag    map[string]*Release
	errByTag map[string]error
	fetchErr error
}

func (f *fakeReleaseClient) FetchRelease(ctx context.Context, tag string) (*Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, tag)
	if f.errByTag != nil {
		if err, ok := f.errByTag[tag]; ok {
			return nil, err
		}
	}
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if f.byTag != nil {
		if r, ok := f.byTag[tag]; ok {
			return r, nil
		}
	}
	return f.release, nil
}

// ---- 文件系统 fakes ----

// fakeExecutableResolver 返回固定路径，不依赖 os.Executable。
type fakeExecutableResolver struct {
	path string
	err  error
}

func (f *fakeExecutableResolver) Executable() (string, error) {
	return f.path, f.err
}

// realLstat 是 Lstat seam 的生产默认实现，直接转 os.Lstat。
// 测试若需隔离文件系统，应注入包装 t.TempDir 的自定义 Lstat。
type realLstat struct{}

func (realLstat) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

// fakeFileReader 用内存 map 模拟文件内容；未命中返回 os.ErrNotExist 包装错误。
type fakeFileReader struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeFileReader() *fakeFileReader {
	return &fakeFileReader{files: map[string][]byte{}}
}

func (f *fakeFileReader) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.files[name]; ok {
		cp := make([]byte, len(b))
		copy(cp, b)
		return cp, nil
	}
	return nil, os.ErrNotExist
}

// tempFileCreator 在指定目录创建真实 temp 文件（与生产 os.CreateTemp 同语义），
// 保证 temp 与 target 同目录同卷（fileutil.tempPattern 要求），便于后续原子 rename。
// 测试调用方传入 t.TempDir() 派生的目录，自动随测试结束清理。
type tempFileCreator struct{}

func (tempFileCreator) CreateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// ---- clock / nonce fakes ----

// fakeClock 维护单调递增的虚拟时间，杜绝真实 wall clock。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance 推进虚拟时间（测试驱动轮询/超时用）。
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// fakeNonceGenerator 返回固定值序列，便于断言生成的 temp 后缀与幂等键。
type fakeNonceGenerator struct {
	mu     sync.Mutex
	values []string
	idx    int
}

func newFakeNonceGenerator(values ...string) *fakeNonceGenerator {
	return &fakeNonceGenerator{values: values}
}

func (f *fakeNonceGenerator) Nonce() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx < len(f.values) {
		v := f.values[f.idx]
		f.idx++
		return v
	}
	return "nonce-exhausted"
}

// ---- 进程 starter fake ----

// fakeProcess 实现 Process，仅记录 Release 调用，不持有真实句柄。
type fakeProcess struct {
	pid      int
	released bool
}

func (p *fakeProcess) PID() int { return p.pid }
func (p *fakeProcess) Release() error {
	p.released = true
	return nil
}

// fakeProcessStarter 记录每次启动参数并返回 fakeProcess，不 fork 子进程。
type fakeProcessStarter struct {
	mu      sync.Mutex
	starts  []recordedStart
	nextPID int
	err     error
}

type recordedStart struct {
	binPath string
	args    []string
}

func newFakeProcessStarter() *fakeProcessStarter {
	return &fakeProcessStarter{nextPID: 1000}
}

func (f *fakeProcessStarter) Start(ctx context.Context, binPath string, args []string) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, recordedStart{binPath: binPath, args: append([]string(nil), args...)})
	if f.err != nil {
		return nil, f.err
	}
	f.nextPID++
	return &fakeProcess{pid: f.nextPID}, nil
}

// ---- platform installer fake ----

// fakePlatformInstaller 仅记录其声明的平台，供后续安装步骤测试注入。
type fakePlatformInstaller struct {
	platform string
}

func (f *fakePlatformInstaller) Platform() string { return f.platform }

// ---- control fakes ----

// fakeControlSession 实现 ControlSession，记录调用并按预置返回值响应。
// 不持有真实 control.Manager / daemon lock / 文件锁。
type fakeControlSession struct {
	mu               sync.Mutex
	inspectCalls     int
	stopCalls        int
	startCalls       int
	lastStartBinPath string
	state            control.RuntimeState
	inspectErr       error
	stopErr          error
	startErr         error
	startErrs        []error
}

func (s *fakeControlSession) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspectCalls++
	return s.state, s.inspectErr
}

func (s *fakeControlSession) Stop(ctx context.Context, cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	return s.stopErr
}

func (s *fakeControlSession) StartWithExecutable(ctx context.Context, cfg *config.Config, binPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	s.lastStartBinPath = binPath
	if index := s.startCalls - 1; index < len(s.startErrs) {
		return s.startErrs[index]
	}
	return s.startErr
}

// fakeControlManager 实现 ControlManager：WithLock 直接以注入的 fakeControlSession
// 调用 fn，不获取任何真实锁。lockErr 非空时原样返回（模拟抢锁失败/超时）。
type fakeControlManager struct {
	mu      sync.Mutex
	session ControlSession
	lockErr error
	calls   int
}

func (m *fakeControlManager) WithLock(ctx context.Context, fn func(ControlSession) error) error {
	m.mu.Lock()
	m.calls++
	lockErr := m.lockErr
	session := m.session
	m.mu.Unlock()
	if lockErr != nil {
		return lockErr
	}
	if fn == nil {
		return errors.New("进程控制锁回调不能为空")
	}
	return fn(session)
}

// ---- Windows helper fakes ----

// fakeParentWaiter 立即返回（模拟父进程已退出），或返回预置错误。记录收到的 identity。
type fakeParentWaiter struct {
	mu           sync.Mutex
	err          error
	calls        int
	lastIdentity ProcessIdentity
}

func (w *fakeParentWaiter) WaitParentExit(ctx context.Context, identity ProcessIdentity) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.lastIdentity = identity
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.err
}

// fakeFileMover 在内存 map 中模拟原子替换，记录 from/to，不触碰真实 FS。
type fakeFileMover struct {
	mu     sync.Mutex
	moves  []recordedMove
	layout map[string][]byte // 简化的内存文件布局：path → content
}

type recordedMove struct{ from, to string }

func newFakeFileMover() *fakeFileMover {
	return &fakeFileMover{layout: map[string][]byte{}}
}

func (m *fakeFileMover) MoveReplace(from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moves = append(m.moves, recordedMove{from: from, to: to})
	content, ok := m.layout[from]
	if !ok {
		return os.ErrNotExist
	}
	m.layout[to] = content
	delete(m.layout, from)
	return nil
}

// fakeResultWriter 把结果写入内存 buffer，便于断言内容；可选地也写真实文件。
type fakeResultWriter struct {
	mu      sync.Mutex
	written map[string][]byte
}

func newFakeResultWriter() *fakeResultWriter {
	return &fakeResultWriter{written: map[string][]byte{}}
}

func (w *fakeResultWriter) WriteResult(path string, data []byte, mode fs.FileMode) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	w.written[path] = cp
	return nil
}

// fakeWindowsHelper 组合 Windows 相关 seam 的 fake 实现，Platform 固定 "windows"。
type fakeWindowsHelper struct {
	platform string
	*fakeParentWaiter
	*fakeFileMover
	*fakeResultWriter
}

func newFakeWindowsHelper() *fakeWindowsHelper {
	return &fakeWindowsHelper{
		platform:         "windows",
		fakeParentWaiter: &fakeParentWaiter{},
		fakeFileMover:    newFakeFileMover(),
		fakeResultWriter: newFakeResultWriter(),
	}
}

func (f *fakeWindowsHelper) Platform() string { return f.platform }

// ---- 小工具 ----

// endsWith 报告 s 是否以 suffix 结尾（避免引入 strings 仅为此一处）。
func endsWith(s, suffix string) bool {
	if len(suffix) > len(s) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

// ioNopCloser 包装一个 Reader 使其实现 io.Closer，供 fakeHTTPDoer 构造 *http.Response.Body。
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func ioNopCloser(r *bytes.Reader) nopCloser { return nopCloser{r} }

// ---- 占位测试：证明包 + 全部 seam 接口可编译并通过 ----

// TestSeamsCompileAndFakesSatisfyInterfaces 验证：
//   - internal/update 包可编译；
//   - 每个 seam 接口都被对应 fake 实现，确保接口形状合法；
//   - 各 fake 的基本行为符合预期（不触达真实网络/HOME/daemon）。
func TestSeamsCompileAndFakesSatisfyInterfaces(t *testing.T) {
	// 用临时目录承接 temp 创建，绝不触碰真实 HOME。
	tmp := t.TempDir()

	// 编译期断言：fake 实现各自 seam 接口。
	var (
		_ HTTPDoer           = newFakeHTTPDoer()
		_ ReleaseClient      = &fakeReleaseClient{}
		_ ExecutableResolver = &fakeExecutableResolver{}
		_ Lstat              = realLstat{}
		_ FileReader         = newFakeFileReader()
		_ TempFileCreator    = tempFileCreator{}
		_ Clock              = newFakeClock(time.Unix(1_700_000_000, 0))
		_ NonceGenerator     = newFakeNonceGenerator("n1", "n2")
		_ ProcessStarter     = newFakeProcessStarter()
		_ Process            = &fakeProcess{pid: 42}
		_ PlatformInstaller  = &fakePlatformInstaller{platform: "darwin"}
		_ ControlSession     = &fakeControlSession{}
		_ ControlManager     = &fakeControlManager{}
		_ ParentWaiter       = &fakeParentWaiter{}
		_ FileMover          = newFakeFileMover()
		_ ResultWriter       = newFakeResultWriter()
		_ WindowsHelper      = newFakeWindowsHelper()
	)

	// 基本行为校验：temp 创建器在 t.TempDir() 下正常工作。
	f, err := tempFileCreator{}.CreateTemp(tmp, ".token-usage.update.tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	// temp 必须落在 t.TempDir() 派生目录内。
	if dir := filepath.Dir(f.Name()); dir != tmp {
		t.Fatalf("temp created outside t.TempDir(): got %q want %q", dir, tmp)
	}

	// Nonce 顺序消费。
	ng := newFakeNonceGenerator("a", "b")
	if got := ng.Nonce(); got != "a" {
		t.Fatalf("first nonce = %q, want a", got)
	}
	if got := ng.Nonce(); got != "b" {
		t.Fatalf("second nonce = %q, want b", got)
	}

	// ControlManager.WithLock 把 fakeControlSession 透传给回调。
	sess := &fakeControlSession{}
	mgr := &fakeControlManager{session: sess}
	called := false
	if err := mgr.WithLock(context.Background(), func(s ControlSession) error {
		called = true
		if s != sess {
			return errors.New("ControlSession 透传不匹配")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !called {
		t.Fatal("WithLock 未调用回调")
	}

	// WindowsHelper 组合 seam 各方法可调用且不触达真实 FS / 进程。
	wh := newFakeWindowsHelper()
	if err := wh.WaitParentExit(context.Background(), ProcessIdentity{PID: 1, CreationTime: 1}); err != nil {
		t.Fatalf("WaitParentExit: %v", err)
	}
	wh.fakeFileMover.layout[filepath.Join(tmp, "new.exe")] = []byte("binary")
	if err := wh.MoveReplace(filepath.Join(tmp, "new.exe"), filepath.Join(tmp, "dst.exe")); err != nil {
		t.Fatalf("MoveReplace: %v", err)
	}
	if err := wh.WriteResult(filepath.Join(tmp, "result.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
}
