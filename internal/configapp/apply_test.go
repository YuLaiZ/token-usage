// internal/configapp/apply_test.go
// package configapp（白盒）：用 fake controlPort 注入 ApplyConfig 全流程，无真实进程/锁/文件 IO。
//
// fakeControlPort 实现包内私有 controlPort 接口，在 WithLock 回调内直接执行 fn，
// Inspect/CleanupStaleMetadata 返回可控行为并记录调用次数；可用锁状态标志断言调用顺序。
package configapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
)

// ---- fake controlPort ----

// fakeControlPort 实现包内 controlPort，无真实锁。
// inspectErr / inspectState 控制 Inspect 行为；cleanupErr 控制 CleanupStaleMetadata 行为。
// callOrder 记录方法调用顺序（Acquire=进入WithLock, Inspect, Cleanup, Release=离开WithLock）。
type fakeControlPort struct {
	mu sync.Mutex

	inspectState control.RuntimeState
	inspectErr   error
	cleanupErr   error

	inspectCalls      int
	cleanupCalls      []string // 记录每次 CleanupStaleMetadata 的 dataDir
	cleanupDataDirArg string

	inLock bool // WithLock 内置 true，供并发断言

	// WithLock 行为：lockErr 非 nil 时直接返回（模拟 timeout），不执行 fn。
	lockErr error
}

func (f *fakeControlPort) WithLock(ctx context.Context, fn func() error) error {
	if f.lockErr != nil {
		return f.lockErr
	}
	f.mu.Lock()
	f.inLock = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inLock = false
		f.mu.Unlock()
	}()
	return fn()
}

func (f *fakeControlPort) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectCalls++
	return f.inspectState, f.inspectErr
}

func (f *fakeControlPort) CleanupStaleMetadata(ctx context.Context, dataDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls = append(f.cleanupCalls, dataDir)
	f.cleanupDataDirArg = dataDir
	return f.cleanupErr
}

// 编译期保证 fakeControlPort 实现 controlPort。
var _ controlPort = (*fakeControlPort)(nil)

// ---- fake AutoStartManager ----

// fakeAutoStart 是可注入的 service.AutoStartManager，记录调用并返回可控行为。
type fakeAutoStart struct {
	platform   string
	status     service.AutoStartStatus
	statusErr  error
	enableErr  error
	disableErr error

	enableCalls  int
	disableCalls int
	enableOpts   service.Options
	disableOpts  service.Options
	statusOpts   service.Options
}

func (f *fakeAutoStart) Enable(opts service.Options) error {
	f.enableCalls++
	f.enableOpts = opts
	return f.enableErr
}
func (f *fakeAutoStart) Disable(opts service.Options) error {
	f.disableCalls++
	f.disableOpts = opts
	return f.disableErr
}
func (f *fakeAutoStart) Status(opts service.Options) (service.AutoStartStatus, error) {
	f.statusOpts = opts
	return f.status, f.statusErr
}
func (f *fakeAutoStart) Platform() string { return f.platform }

var _ service.AutoStartManager = (*fakeAutoStart)(nil)

// ---- 测试 helper ----

// newApp 用 fake 依赖构造 Application（白盒：直接调 newApplicationWithDeps）。
// 生产 control.NewManager 会创建 .token-usage/ 目录；fake controlPort 不创建，
// 故此处预先创建（模拟 control.Manager 已初始化）。
func newApp(t *testing.T, home string, ctrl controlPort, autoStart service.AutoStartManager) *Application {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".token-usage"), 0o755); err != nil {
		t.Fatalf("MkdirAll .token-usage: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: home, GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	app, err := newApplicationWithDeps(home, env, ctrl, autoStart)
	if err != nil {
		t.Fatalf("newApplicationWithDeps: %v", err)
	}
	return app
}

// writeFile 在 home/.token-usage/config.toml 写入 content（创建目录）。
func writeFile(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// basicConfig 返回一个最小合法 currentUser（含 data_dir + 一个 enabled client）。
func basicConfig(dataDir string) *config.Config {
	return &config.Config{
		DataDir: dataDir,
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
}

// revision 计算 bytes 的 sha256（与生产 Revision 一致，供测试断言）。
func testRevision(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// ---- Revision 函数 ----

func TestRevision_Sha256(t *testing.T) {
	raw := []byte("hello")
	got := Revision(raw)
	want := testRevision(raw)
	if !bytes.Equal(got, want) {
		t.Errorf("Revision 不等于 sha256: %x != %x", got, want)
	}
}

func TestRevision_SentinelDiffersFromEmptyFileHash(t *testing.T) {
	emptyHash := Revision(nil) // 空文件
	// sentinel 必须与空文件 hash 不同（用内部 sentinel 常量）。
	if bytes.Equal(missingFileSentinel, emptyHash) {
		t.Fatal("missingFileSentinel 不能等于空文件 hash")
	}
}

// ---- NewApplication 校验 ----

func TestNewApplication_RejectsHomeMismatch(t *testing.T) {
	mgr, err := control.NewManager(os.TempDir())
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: "/other/home", GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	_, err = NewApplication("/other/home", env, mgr, &fakeAutoStart{})
	if err == nil {
		t.Fatal("home != manager.ConfigHome() 父目录应被拒绝")
	}
}

func TestNewApplication_RejectsNilManager(t *testing.T) {
	env := runtimecfg.ResolveEnv{Home: os.TempDir(), GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	_, err := NewApplication(os.TempDir(), env, nil, &fakeAutoStart{})
	if err == nil {
		t.Fatal("nil manager 应被拒绝")
	}
}

func TestNewApplication_RejectsNilAutoStart(t *testing.T) {
	mgr, err := control.NewManager(os.TempDir())
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: os.TempDir(), GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	_, err = NewApplication(os.TempDir(), env, mgr, nil)
	if err == nil {
		t.Fatal("nil autoStart 应被拒绝")
	}
}

func TestNewApplicationWithDeps_RejectsRelativeHomeAndEmptyGOOS(t *testing.T) {
	ctrl := &fakeControlPort{}
	as := &fakeAutoStart{}
	provider := runtimecfg.NewStandardProvider()

	if _, err := newApplicationWithDeps("relative", runtimecfg.ResolveEnv{
		Home: "relative", GOOS: "darwin", DefaultPaths: provider,
	}, ctrl, as); err == nil {
		t.Fatal("相对 home 必须拒绝")
	}
	home := t.TempDir()
	if _, err := newApplicationWithDeps(home, runtimecfg.ResolveEnv{
		Home: home, GOOS: "", DefaultPaths: provider,
	}, ctrl, as); err == nil {
		t.Fatal("空 GOOS 必须拒绝")
	}
}

func TestNewApplication_AcceptsValidHome(t *testing.T) {
	mgr, err := control.NewManager(os.TempDir())
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: os.TempDir(), GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	app, err := NewApplication(os.TempDir(), env, mgr, &fakeAutoStart{})
	if err != nil {
		t.Fatalf("合法 home 应通过，err=%v", err)
	}
	if app == nil {
		t.Fatal("app 不应为 nil")
	}
}

// ---- exact trace：Acquire → Cleanup temp → Read raw → Revision → Validate/Resolve → Inspect → Replace → Sync → data_dir Cleanup → Release ----

func TestApplyConfig_ExactTrace_NoOp(t *testing.T) {
	home := t.TempDir()
	// 写入与 currentUser marshal 结果一致的配置 → no-op（Saved=false）。
	marshaled, err := config.MarshalUserConfig(basicConfig("/d"))
	if err != nil {
		t.Fatalf("MarshalUserConfig: %v", err)
	}
	writeFile(t, home, string(marshaled))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	ctx := context.Background()
	res, err := app.ApplyConfig(ctx, testRevision(marshaled), basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	// no-op：Saved=false，但 ConfigApplied=true（磁盘与 currentUser 一致）
	if res.Saved {
		t.Error("no-op 应 Saved=false")
	}
	if !res.ConfigApplied {
		t.Error("no-op 应 ConfigApplied=true（磁盘与 currentUser 对应）")
	}
	if !bytes.Equal(res.NewRevision, testRevision(marshaled)) {
		t.Errorf("NewRevision 应为磁盘 raw revision，got %x want %x", res.NewRevision, testRevision(marshaled))
	}
	if res.AutoStart.Err != nil {
		t.Errorf("SyncWith 不应失败（平台支持），err=%v", res.AutoStart.Err)
	}
}

// ---- revision 冲突：replace/sync 调用 0 ----

func TestApplyConfig_RevisionConflict_NoWriteNoSync(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "data_dir = \"/d\"\n[[clients]]\n") // 损坏/不同内容

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}}
	app := newApp(t, home, ctrl, as)

	// expectedRevision 是一个完全不同的值，应触发冲突。
	res, err := app.ApplyConfig(context.Background(), bytes.Repeat([]byte{0xff}, 32), basicConfig("/d"), false)
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("应返回 ErrConfigChangedExternally，got %v", err)
	}
	if res.ConfigApplied {
		t.Error("revision 冲突时 ConfigApplied 应为 false")
	}
	if as.enableCalls != 0 || as.disableCalls != 0 {
		t.Errorf("冲突时不应 sync 自启，enable=%d disable=%d", as.enableCalls, as.disableCalls)
	}
}

// ---- data_dir 迁移：Inspect 旧 data_dir 返回错误 → 传播，不写盘不同步 ----
// 覆盖 apply.go:257-260 的 inspErr != nil 分支（迁移前置检查）。

func TestApplyConfig_DataDirMigration_InspectError_Propagates(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	inspectErr := errors.New("inspect boom")
	// Inspect 返回错误（inspErr != nil 分支）；首次 Inspect(prevEff) 即失败。
	ctrl := &fakeControlPort{inspectErr: inspectErr}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/new"}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, true)
	if err == nil {
		t.Fatal("Inspect 返回错误应被传播")
	}
	if !errors.Is(err, inspectErr) {
		t.Errorf("err 应 wrap inspectErr，got %v", err)
	}
	// 写入前失败 → 未写盘，ConfigApplied=false。
	if res.Saved {
		t.Error("Inspect 错误不应写盘 Saved=false")
	}
	if res.ConfigApplied {
		t.Error("Inspect 错误 ConfigApplied 应为 false")
	}
	// 不应 sync 自启（写入前失败）。
	if as.enableCalls != 0 || as.disableCalls != 0 {
		t.Errorf("Inspect 错误不应 sync，enable=%d disable=%d", as.enableCalls, as.disableCalls)
	}
	// 磁盘应保持旧内容。
	got, _ := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if !bytes.Equal(got, oldRaw) {
		t.Errorf("磁盘应保持旧内容，got %q", got)
	}
}

// ---- control lock timeout：read/replace/service 调用 0 ----

func TestApplyConfig_LockTimeout_NoCalls(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "data_dir = \"/d\"\n")

	ctrl := &fakeControlPort{
		lockErr:      control.ErrControlLockTimeout,
		inspectState: control.RuntimeState{Running: false},
	}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}}
	app := newApp(t, home, ctrl, as)

	_, err := app.ApplyConfig(context.Background(), nil, basicConfig("/d"), false)
	if !errors.Is(err, control.ErrControlLockTimeout) {
		t.Fatalf("应返回 ErrControlLockTimeout，got %v", err)
	}
	if ctrl.inspectCalls != 0 {
		t.Errorf("lock timeout 时不应 Inspect，实际 %d", ctrl.inspectCalls)
	}
	if as.enableCalls != 0 || as.disableCalls != 0 {
		t.Errorf("lock timeout 时不应 sync，enable=%d disable=%d", as.enableCalls, as.disableCalls)
	}
}

// ---- 连续两次：第一次 revision 可用于第二次 ----

func TestApplyConfig_SequentialRevisionReuse(t *testing.T) {
	home := t.TempDir()

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// 第一次：文件不存在，用 sentinel。
	res1, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d1"), false)
	if err != nil {
		t.Fatalf("第一次 ApplyConfig: %v", err)
	}
	if !res1.Saved {
		t.Error("第一次应 Saved=true")
	}

	// 第二次：用第一次返回的 NewRevision，且 currentUser 不变 → no-op。
	res2, err := app.ApplyConfig(context.Background(), res1.NewRevision, basicConfig("/d1"), false)
	if err != nil {
		t.Fatalf("第二次 ApplyConfig: %v", err)
	}
	if res2.Saved {
		t.Error("第二次应 Saved=false（no-op）")
	}
	if !bytes.Equal(res2.NewRevision, res1.NewRevision) {
		t.Error("第二次 NewRevision 应等于第一次（无变化）")
	}
}

// ---- no-op 返回真实磁盘 revision（即使 Saved=false）----

func TestApplyConfig_NoOpReturnsDiskRevision(t *testing.T) {
	home := t.TempDir()
	marshaled, _ := config.MarshalUserConfig(basicConfig("/d"))
	writeFile(t, home, string(marshaled))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}}
	app := newApp(t, home, ctrl, as)

	res, err := app.ApplyConfig(context.Background(), testRevision(marshaled), basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !bytes.Equal(res.NewRevision, testRevision(marshaled)) {
		t.Errorf("no-op NewRevision 应为磁盘 raw revision")
	}
}

// ---- ConfigApplied 四路径 ----

func TestApplyConfig_ConfigApplied_NoOp(t *testing.T) {
	// no-op：磁盘与 currentUser 一致 → ConfigApplied=true
	home := t.TempDir()
	marshaled, _ := config.MarshalUserConfig(basicConfig("/d"))
	writeFile(t, home, string(marshaled))
	app := newApp(t, home, &fakeControlPort{inspectState: control.RuntimeState{Running: false}}, &fakeAutoStart{status: service.AutoStartStatus{Exists: false}})
	res, err := app.ApplyConfig(context.Background(), testRevision(marshaled), basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.ConfigApplied {
		t.Error("no-op ConfigApplied 应为 true")
	}
}

func TestApplyConfig_ConfigApplied_WriteSuccess(t *testing.T) {
	// 写入成功 → ConfigApplied=true
	home := t.TempDir()
	app := newApp(t, home, &fakeControlPort{inspectState: control.RuntimeState{Running: false}}, &fakeAutoStart{status: service.AutoStartStatus{Exists: false}})
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.Saved || !res.ConfigApplied {
		t.Error("写入成功应 Saved=true + ConfigApplied=true")
	}
}

func TestApplyConfig_ConfigApplied_PreWriteFailure(t *testing.T) {
	// 写入前失败（validation）→ ConfigApplied=false
	home := t.TempDir()
	app := newApp(t, home, &fakeControlPort{inspectState: control.RuntimeState{Running: false}}, &fakeAutoStart{status: service.AutoStartStatus{Exists: false}})
	// currentUser 含未注册 client → ValidateUserConfig 失败
	bad := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"nonexistent": {Enabled: true, Paths: map[string]string{}},
		},
	}
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, bad, false)
	if err == nil {
		t.Fatal("非法 config 应返回错误")
	}
	if res.ConfigApplied {
		t.Error("写入前失败 ConfigApplied 应为 false")
	}
}

func TestApplyConfig_ConfigApplied_PostWritePartialFailure(t *testing.T) {
	// 写入后部分失败（sync 失败）→ ConfigApplied=true + 非空 PartialErrors
	home := t.TempDir()
	syncErr := errors.New("install failed")
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	// autostart=true + 定义缺失 → SyncWith 调 Enable；Enable 失败。
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, enableErr: syncErr, platform: "launchd"}
	app := newApp(t, home, ctrl, as)
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{AutoStart: true},
	}, false)
	if err == nil {
		t.Fatal("部分失败应返回 errors.Join 非 nil")
	}
	if !res.ConfigApplied {
		t.Error("写入后部分失败 ConfigApplied 应为 true")
	}
	if !res.Saved {
		t.Error("应 Saved=true")
	}
	if len(res.PartialErrors) == 0 {
		t.Error("应有 PartialErrors")
	}
	if !errors.Is(err, syncErr) {
		t.Errorf("err 应 wrap syncErr，got %v", err)
	}
}

// ---- raw changed / effective same vs raw no-op ----

func TestApplyConfig_RawChangedEffectiveSame_Message(t *testing.T) {
	home := t.TempDir()
	// 初始磁盘：一个有效配置（用 marshal 写入）。
	initial := &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{PollInterval: 0, AutoStart: false}, // poll=0 会 resolve 到 30
	}
	initialRaw, _ := config.MarshalUserConfig(initial)
	writeFile(t, home, string(initialRaw))

	// currentUser：poll 显式设为 30（与 resolve 后的 effective 一致，但 raw 字节不同）。
	current := &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{PollInterval: 30, AutoStart: false},
	}
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	res, err := app.ApplyConfig(context.Background(), testRevision(initialRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.Saved {
		t.Error("raw 变化应 Saved=true")
	}
	// effective 相同 → 不生成 restart/collect
	if len(res.SuggestedSteps) != 0 {
		t.Errorf("effective 相同不应生成动作步骤: %v", res.SuggestedSteps)
	}
	// 应有"有效配置未变化"说明
	found := false
	for _, n := range res.ExplanatoryNotes {
		if strings.Contains(n, "有效配置未变化") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("应含'有效配置未变化'说明, notes=%v", res.ExplanatoryNotes)
	}
}

func TestApplyConfig_AutoStartOnlyChangeIsEffectiveChange(t *testing.T) {
	home := t.TempDir()
	initial := &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{PollInterval: 30, AutoStart: false},
	}
	initialRaw, err := config.MarshalUserConfig(initial)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, home, string(initialRaw))

	current := &config.Config{
		DataDir: "/d",
		Daemon:  config.DaemonConfig{PollInterval: 30, AutoStart: true},
	}
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	res, err := app.ApplyConfig(context.Background(), testRevision(initialRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.Changed || res.SuccessMessage != "config saved / 配置已保存" {
		t.Errorf("仅 autostart 变化也是有效配置变化，got Changed=%v message=%q", res.Changed, res.SuccessMessage)
	}
	if len(res.SuggestedSteps) != 0 {
		t.Errorf("仅 autostart 变化不应要求 restart/collect: %v", res.SuggestedSteps)
	}
	for _, note := range res.ExplanatoryNotes {
		if strings.Contains(note, "仅写法变化") {
			t.Errorf("仅 autostart 变化不应被描述为仅写法变化: %v", res.ExplanatoryNotes)
		}
	}
}

func TestApplyConfig_AutoStartUsesEffectiveDataDir(t *testing.T) {
	home := t.TempDir()
	current := &config.Config{
		DataDir: "~/custom-data",
		Daemon:  config.DaemonConfig{AutoStart: true},
	}
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	if _, err := app.ApplyConfig(context.Background(), missingFileSentinel, current, false); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	want := filepath.Join(home, "custom-data")
	if as.statusOpts.DataDir != want || as.enableOpts.DataDir != want {
		t.Errorf("自启定义必须使用 effective data_dir，status=%q enable=%q want=%q",
			as.statusOpts.DataDir, as.enableOpts.DataDir, want)
	}
	if current.DataDir != "~/custom-data" {
		t.Errorf("解析自启参数不应修改用户层配置，got %q", current.DataDir)
	}
}

func TestApplyConfig_RawNoOp_OnlyDriftRepair(t *testing.T) {
	home := t.TempDir()
	current := basicConfig("/d")
	current.Daemon.AutoStart = true
	marshaled, _ := config.MarshalUserConfig(current)
	writeFile(t, home, string(marshaled))

	// 配置不变，但 autostart 定义漂移（Exists=true SpecMatches=false）→ 修复。
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{
		status:   service.AutoStartStatus{Exists: true, SpecMatches: false},
		platform: "launchd",
	}
	app := newApp(t, home, ctrl, as)

	// currentUser 同磁盘（raw no-op）。
	res, err := app.ApplyConfig(context.Background(), testRevision(marshaled), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if res.Saved {
		t.Error("raw no-op 应 Saved=false")
	}
	if !res.AutoStart.DriftRepaired {
		t.Error("漂移应被修复 DriftRepaired=true")
	}
}

// ---- sentinel 与空文件 hash 不同 ----

func TestApplyConfig_MissingFileUsesSentinel(t *testing.T) {
	home := t.TempDir()
	// 不写 config 文件 → 不存在 → sentinel
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.Saved {
		t.Error("不存在文件首次写入应 Saved=true")
	}
}

func TestApplyConfig_EmptyFileNotTreatedAsMissing(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "") // 空文件（Exists=true，但 LoadUserConfigSnapshot 视为损坏）
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// 空文件 Exists=true 但解析失败（snapshot 返回错误），不应被当 sentinel 跳过：
	// ApplyConfig 应返回读取错误（非 ErrConfigChangedExternally），且 ConfigApplied=false。
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d"), false)
	if err == nil {
		t.Fatal("空文件应返回读取错误")
	}
	if errors.Is(err, ErrConfigChangedExternally) {
		t.Error("空文件不应被当作 missing（sentinel 不匹配），应走读取错误分支")
	}
	if res.ConfigApplied {
		t.Error("空文件 ConfigApplied 应为 false")
	}
}

// ---- 保存成功+sync 失败仍返回新 revision ----

func TestApplyConfig_SyncFailureReturnsNewRevision(t *testing.T) {
	home := t.TempDir()
	syncErr := errors.New("enable failed")
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, enableErr: syncErr, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{AutoStart: true}}
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, current, false)
	if err == nil {
		t.Fatal("应有错误")
	}
	if !res.Saved {
		t.Error("应 Saved=true")
	}
	// NewRevision 必须对应实际写入字节
	expected, _ := config.MarshalUserConfig(current)
	if !bytes.Equal(res.NewRevision, testRevision(expected)) {
		t.Errorf("NewRevision 应为写入字节 revision")
	}
}

// ---- 保存成功+cleanup 失败仍返回新 revision ----

func TestApplyConfig_CleanupFailureReturnsNewRevision(t *testing.T) {
	home := t.TempDir()
	// 初始：data_dir=/old
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	cleanupErr := errors.New("cleanup failed")
	ctrl := &fakeControlPort{
		inspectState: control.RuntimeState{Running: false},
		cleanupErr:   cleanupErr,
	}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// currentUser 改 data_dir，确认迁移，daemon 未运行 → cleanup 被调（失败）。
	current := &config.Config{DataDir: "/new"}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, true)
	if err == nil {
		t.Fatal("cleanup 失败应有错误")
	}
	if !res.Saved {
		t.Error("应 Saved=true")
	}
	if len(ctrl.cleanupCalls) == 0 {
		t.Error("应调用 CleanupStaleMetadata")
	}
	if len(res.PartialErrors) == 0 {
		t.Error("应有 PartialErrors")
	}
	expected, _ := config.MarshalUserConfig(current)
	if !bytes.Equal(res.NewRevision, testRevision(expected)) {
		t.Errorf("NewRevision 应为写入字节 revision")
	}
}

// ---- 返回后外部修改不被旧 revision 覆盖 ----

func TestApplyConfig_ExternalModificationNotOverwritten(t *testing.T) {
	home := t.TempDir()
	marshaled, _ := config.MarshalUserConfig(basicConfig("/d"))
	writeFile(t, home, string(marshaled))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// 第一次写入新内容（保持 data_dir=/d 不变，仅加一个 client 触发 raw 变化），返回 revision A。
	current1 := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/p"}},
			"codex":  {Enabled: false, Paths: map[string]string{"state_dir": "/c"}},
		},
	}
	res1, err := app.ApplyConfig(context.Background(), testRevision(marshaled), current1, false)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}

	// 模拟外部修改：写入完全不同内容（保持 data_dir=/d 避免迁移保护干扰本测试焦点）。
	external := []byte("data_dir = \"/d\"\n[[clients]]\n")
	if err := os.WriteFile(filepath.Join(home, ".token-usage", "config.toml"), external, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 第二次用旧 revision A（不再匹配磁盘）→ 冲突，不写盘。
	_, err = app.ApplyConfig(context.Background(), res1.NewRevision, current1, false)
	if !errors.Is(err, ErrConfigChangedExternally) {
		t.Fatalf("外部修改后用旧 revision 应冲突，got %v", err)
	}
	// 磁盘内容应保持外部写入。
	got, _ := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if !bytes.Equal(got, external) {
		t.Errorf("磁盘应保持外部写入，got %q", got)
	}
}

// ---- data_dir 三路径 ----

func TestApplyConfig_DataDirMigration_NotConfirmed_Rejected(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/new"}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, false) // confirm=false
	if err == nil {
		t.Fatal("未确认迁移应被拒绝")
	}
	// 前置条件在写入前校验 → 未写盘，ConfigApplied=false。
	if res.Saved {
		t.Error("未确认迁移不应写盘 Saved=false")
	}
	if res.ConfigApplied {
		t.Error("未确认迁移 ConfigApplied 应为 false（写入前拒绝）")
	}
	// 磁盘应保持旧内容。
	got, _ := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if !bytes.Equal(got, oldRaw) {
		t.Errorf("磁盘应保持旧内容，got %q", got)
	}
}

func TestApplyConfig_DataDirMigration_DaemonRunning_Rejected(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	// 第一次 Inspect 旧 data_dir：Running=true → 拒绝（即使 confirm=true）。
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: true, PID: 123}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/new"}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, true)
	if err == nil {
		t.Fatal("旧 daemon 运行中即使确认也应拒绝")
	}
	// 写入前拒绝 → 未写盘。
	if res.Saved {
		t.Error("旧 daemon 运行中拒绝应未写盘 Saved=false")
	}
	if res.ConfigApplied {
		t.Error("写入前拒绝 ConfigApplied 应为 false")
	}
}

func TestApplyConfig_DataDirMigration_DaemonStopped_CleanupCalled(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/new"}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, true)
	if err != nil {
		t.Fatalf("迁移成功（daemon 停）不应错: %v", err)
	}
	// CleanupStaleMetadata 应以 /old 调用。
	if len(ctrl.cleanupCalls) == 0 || ctrl.cleanupCalls[0] != "/old" {
		t.Errorf("应 CleanupStaleMetadata(/old), calls=%v", ctrl.cleanupCalls)
	}
	// 应有迁移说明步骤（含手工迁移）。
	if res.Effects.DataDirMigration == nil {
		t.Error("应有 DataDirMigration")
	}
	if !reflect.DeepEqual(res.SuggestedSteps, []string{"token-usage start"}) {
		t.Errorf("迁移完成后应建议显式启动，got %v", res.SuggestedSteps)
	}
}

// ---- temp 清理失败传播 ----

func TestApplyConfig_TempCleanupFailurePropagates(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".token-usage")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 放一个文件但不给目录读权限 → CleanupKnownTempFiles 失败。
	// 用一个不可读的子目录模拟：创建只读目录会让 ReadDir 失败。
	// 更简单：把 .token-usage 改为不可读（仅 Unix）。
	if err := os.Chmod(configDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(configDir, 0o755)

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	_, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d"), false)
	if err == nil {
		t.Fatal("temp 清理失败应传播错误")
	}
}

// ---- marshal/resolve/replace 失败传播 ----

func TestApplyConfig_ValidateFailurePropagates(t *testing.T) {
	home := t.TempDir()
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// currentUser 含未注册 router。
	bad := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "nonexistent_router"},
		},
	}
	_, err := app.ApplyConfig(context.Background(), missingFileSentinel, bad, false)
	if err == nil {
		t.Fatal("validate 失败应传播")
	}
}

// ---- 自启四类说明 + 动作合并 ----

func TestApplyConfig_AutoStart_PlatformUnsupported_NonFatal(t *testing.T) {
	home := t.TempDir()
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{statusErr: service.ErrPlatformUnsupported, platform: "unsupported"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{AutoStart: true}}
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, current, false)
	if err != nil {
		t.Fatalf("平台不支持不应致命错: %v", err)
	}
	if len(res.PartialErrors) != 0 {
		t.Errorf("平台不支持不进 PartialErrors: %v", res.PartialErrors)
	}
	// 应有非致命说明。
	found := false
	for _, n := range res.ExplanatoryNotes {
		if strings.Contains(n, "不支持") {
			found = true
		}
	}
	if !found {
		t.Errorf("应有平台不支持说明: %v", res.ExplanatoryNotes)
	}
}

func TestApplyConfig_AutoStart_TrueDaemonStopped_SuggestsStart(t *testing.T) {
	home := t.TempDir()
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	// autostart=true + 定义缺失 → Enable。
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{AutoStart: true}}
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.AutoStart.Requested || !res.AutoStart.DefinitionNow {
		t.Errorf("Requested=true DefinitionNow(Enable 后)=true, got %+v", res.AutoStart)
	}
	if res.AutoStart.DefinitionWas || res.AutoStart.DriftRepaired {
		t.Errorf("首次启用前定义不存在，且不属于漂移修复，got %+v", res.AutoStart)
	}
}

func TestApplyConfig_ActionMerging_DaemonRunningOnlyPollLog_Restart(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: 30}}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: true, PID: 9}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// poll 变化（运行时配置变化），无 collect。
	current := &config.Config{DataDir: "/d", Daemon: config.DaemonConfig{PollInterval: 60}}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if len(res.SuggestedSteps) != 1 || res.SuggestedSteps[0] != "token-usage restart" {
		t.Errorf("daemon 运行+仅 poll/log 变化应只生成 restart, got %v", res.SuggestedSteps)
	}
}

func TestApplyConfig_ActionMerging_DaemonRunningWithCollect_StopCollectStart(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: false, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: true, PID: 9}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// claude disabled→enabled → full collect，daemon 运行 → stop/collect/start。
	current := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if len(res.SuggestedSteps) < 3 {
		t.Fatalf("应至少 3 步(stop/collect/start), got %v", res.SuggestedSteps)
	}
	wantSteps := []string{
		"token-usage stop",
		"token-usage collect all --client claude",
		"token-usage start",
	}
	if !reflect.DeepEqual(res.SuggestedSteps, wantSteps) {
		t.Errorf("动作应是可直接执行的完整命令，got %v want %v", res.SuggestedSteps, wantSteps)
	}
}

func TestApplyConfig_ActionMerging_DaemonStoppedWithCollect_NoStop(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: false, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	for _, s := range res.SuggestedSteps {
		if s == "token-usage stop" || s == "token-usage restart" {
			t.Errorf("daemon 未运行不应 stop/restart: %v", res.SuggestedSteps)
			break
		}
	}
}

// ---- 变化后的 path 不存在只产生 warning，不阻止保存 ----

func TestApplyConfig_NonexistentPathOnlyWarning(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/p"}},
		},
	}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	// 新路径 /nonexistent（不存在），但应只 warning 不阻止。
	current := &config.Config{
		DataDir: "/d",
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/totally/nonexistent"}},
		},
	}
	res, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, false)
	if err != nil {
		t.Fatalf("path 不存在不应阻止保存: %v", err)
	}
	if !res.Saved {
		t.Error("应 Saved=true")
	}
	if len(res.Effects.FullCollectClients) == 0 {
		t.Error("path 变化应触发 full collect")
	}
	foundWarning := false
	for _, warning := range res.Effects.Warnings {
		if strings.Contains(warning, "clients.claude.paths.projects_dir") &&
			strings.Contains(warning, "/totally/nonexistent") &&
			strings.Contains(warning, "不存在") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("变化后的不存在路径应产生 warning，got %v", res.Effects.Warnings)
	}
}

// ---- 辅助：确认 reflect.DeepEqual 在 ExplanatoryNotes 上可用 ----

func TestApplyConfig_ExplanatoryNotesNotNilWhenSaved(t *testing.T) {
	home := t.TempDir()
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)
	res, err := app.ApplyConfig(context.Background(), missingFileSentinel, basicConfig("/d"), false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	// Saved 时应有 SuccessMessage 或 Notes。
	if res.SuccessMessage == "" && len(res.ExplanatoryNotes) == 0 {
		t.Error("保存成功应有某种用户反馈")
	}
}

// ---- exact trace：锁内调用顺序（data_dir 迁移场景）----
// 验证 Acquire → Cleanup temp → Read raw → Revision → Validate/Resolve →
// Inspect(prevEff, 旧 data_dir 运行检查) → Inspect(currEff) → Replace → Sync → Cleanup → Release。
//
// 用 traceControlPort 记录方法调用顺序，与 data_dir 迁移成功路径交叉验证。
type traceControlPort struct {
	fakeControlPort
	trace []string
	mu    sync.Mutex
}

func (t *traceControlPort) WithLock(ctx context.Context, fn func() error) error {
	t.mu.Lock()
	t.trace = append(t.trace, "Acquire")
	t.mu.Unlock()
	if t.fakeControlPort.lockErr != nil {
		return t.fakeControlPort.lockErr
	}
	err := fn()
	t.mu.Lock()
	t.trace = append(t.trace, "Release")
	t.mu.Unlock()
	return err
}

func (t *traceControlPort) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	t.mu.Lock()
	t.trace = append(t.trace, "Inspect:"+cfg.DataDir)
	t.mu.Unlock()
	return t.fakeControlPort.inspectState, t.fakeControlPort.inspectErr
}

func (t *traceControlPort) CleanupStaleMetadata(ctx context.Context, dataDir string) error {
	t.mu.Lock()
	t.trace = append(t.trace, "Cleanup:"+dataDir)
	t.mu.Unlock()
	return t.fakeControlPort.CleanupStaleMetadata(ctx, dataDir)
}

func TestApplyConfig_ExactTrace_DataDirMigration(t *testing.T) {
	home := t.TempDir()
	oldCfg := &config.Config{DataDir: "/old"}
	oldRaw, _ := config.MarshalUserConfig(oldCfg)
	writeFile(t, home, string(oldRaw))

	// 预置一个残留 temp 文件，验证步骤2清理。
	configDir := filepath.Join(home, ".token-usage")
	if err := os.WriteFile(filepath.Join(configDir, ".config.toml.tmp-stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tc := &traceControlPort{
		fakeControlPort: fakeControlPort{inspectState: control.RuntimeState{Running: false}},
	}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, tc, as)

	current := &config.Config{DataDir: "/new"}
	_, err := app.ApplyConfig(context.Background(), testRevision(oldRaw), current, true)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// temp 文件应已被清理。
	if _, statErr := os.Stat(filepath.Join(configDir, ".config.toml.tmp-stale")); !os.IsNotExist(statErr) {
		t.Errorf("残留 temp 应被清理: statErr=%v", statErr)
	}

	// 验证关键调用顺序：两次 Inspect（prevEff DataDir=/old 先于 currEff /new），Cleanup 在末尾。
	tc.mu.Lock()
	defer tc.mu.Unlock()
	// 找 Inspect:/old 与 Inspect:/new 的位置。
	var oldIdx, newIdx, cleanupIdx int = -1, -1, -1
	for i, e := range tc.trace {
		if e == "Inspect:/old" && oldIdx < 0 {
			oldIdx = i
		}
		if e == "Inspect:/new" && newIdx < 0 {
			newIdx = i
		}
		if e == "Cleanup:/old" {
			cleanupIdx = i
		}
	}
	if oldIdx < 0 || newIdx < 0 {
		t.Fatalf("应含两次 Inspect（/old 与 /new），trace=%v", tc.trace)
	}
	if oldIdx >= newIdx {
		t.Errorf("Inspect:/old 应在 Inspect:/new 之前（迁移前置检查），trace=%v", tc.trace)
	}
	if cleanupIdx < 0 {
		t.Fatalf("应含 Cleanup:/old，trace=%v", tc.trace)
	}
	if cleanupIdx < newIdx {
		t.Errorf("Cleanup:/old 应在两次 Inspect 之后，trace=%v", tc.trace)
	}
}

// raw query 变化:写盘保存并保留问题态;不触发 restart/collect;
// effective 等价判断含 raw,不得误报「有效配置未变化(仅写法规范化)」。
func TestApplyConfig_RawQueryChangeSavedAndPreserved(t *testing.T) {
	home := t.TempDir()
	initialRaw := []byte("data_dir = \"/d\"\n[query.subqueries]\nmpc = \"model,provider\"\n")
	writeFile(t, home, string(initialRaw))
	ctrl := &fakeControlPort{inspectState: control.RuntimeState{Running: false}}
	as := &fakeAutoStart{status: service.AutoStartStatus{Exists: false}, platform: "launchd"}
	app := newApp(t, home, ctrl, as)

	current := &config.Config{
		DataDir:  "/d",
		RawQuery: map[string]any{"subqueries": map[string]any{"mpc": "model,provider,client"}},
	}
	res, err := app.ApplyConfig(context.Background(), testRevision(initialRaw), current, false)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.Saved {
		t.Error("raw query 变化应写盘")
	}
	if res.Effects.RuntimeChanged || len(res.Effects.FullCollectClients) != 0 || len(res.Effects.RouterBackfillClients) != 0 {
		t.Errorf("query 变化不得触发运行时动作: %+v", res.Effects)
	}
	if !strings.Contains(res.SuccessMessage, "配置已保存") {
		t.Errorf("raw query 变化不得误报「有效配置未变化」: %q", res.SuccessMessage)
	}
	got, err := config.LoadUserConfig(filepath.Join(home, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.RawQuery == nil || got.RawQueryTopLevelIssues != nil {
		t.Errorf("磁盘应保留合法 query 段: %#v / %#v", got.RawQuery, got.RawQueryTopLevelIssues)
	}

	// 问题态写盘:含 issues 的 current 保存后磁盘仍是问题态,ApplyConfig 不被阻塞。
	home2 := t.TempDir()
	initial2 := []byte("data_dir = \"/d2\"\n")
	writeFile(t, home2, string(initial2))
	app2 := newApp(t, home2, ctrl, as)
	cur2 := &config.Config{
		DataDir: "/d2",
		RawQueryTopLevelIssues: map[string]config.RawQueryTopLevelIssue{
			"query": {Name: "query", Value: "x", Kind: config.RawQueryIssueRootNotTable},
		},
	}
	res2, err := app2.ApplyConfig(context.Background(), testRevision(initial2), cur2, false)
	if err != nil {
		t.Fatalf("问题态不得阻塞 ApplyConfig: %v", err)
	}
	if !res2.Saved {
		t.Error("问题态 raw 变化应写盘")
	}
	got2, err := config.LoadUserConfig(filepath.Join(home2, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatalf("reload2: %v", err)
	}
	if got2.RawQuery != nil || got2.RawQueryTopLevelIssues == nil {
		t.Errorf("磁盘应保留问题态: %#v / %#v", got2.RawQuery, got2.RawQueryTopLevelIssues)
	}
	if got2.RawQueryTopLevelIssues["query"].Value != "x" {
		t.Errorf("问题项内容失真: %#v", got2.RawQueryTopLevelIssues["query"])
	}
}
