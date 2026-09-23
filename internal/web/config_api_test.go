package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
)

// ---- handler 合同(fake store) ----

// fakeConfigStore 记录 handler 传入的参数并按预设返回,用于锁定 HTTP 层合同
// 而不触碰真实 configapp。
type fakeConfigStore struct {
	view        ConfigView
	viewErr     error
	applyResult ConfigApplyResult
	applyErr    error

	called      bool
	gotRevision string
	gotDraft    ConfigDraft
}

func (f *fakeConfigStore) Current(ctx context.Context) (ConfigView, error) {
	if f.viewErr != nil {
		return ConfigView{}, f.viewErr
	}
	return f.view, nil
}

func (f *fakeConfigStore) CurrentDefaults(ctx context.Context) (ConfigDraft, error) {
	return sampleConfigView().Config, nil
}

func (f *fakeConfigStore) Apply(ctx context.Context, revision string, draft ConfigDraft) (ConfigApplyResult, error) {
	f.called = true
	f.gotRevision = revision
	f.gotDraft = draft
	if f.applyErr != nil {
		return ConfigApplyResult{}, f.applyErr
	}
	return f.applyResult, nil
}

// sampleConfigView 构造一份覆盖全部区块的编辑模型。
func sampleConfigView() ConfigView {
	return ConfigView{
		Revision: "abc123",
		Config: ConfigDraft{
			Daemon:  DaemonDraft{PollInterval: 3, AutoStart: true},
			Log:     LogDraft{Level: "debug", Dir: "/tmp/logs", MaxDays: 30},
			Clients: []ClientDraft{{Name: "claude", Enabled: true, Router: "cc_switch", Paths: map[string]string{"projects_dir": "/tmp/p"}}},
			Routers: []RouterDraft{{Name: "cc_switch", DBPath: "/tmp/cc.db"}},
			ProviderAliases: []AliasDraft{
				{Key: "deepseek", Value: "DeepSeek"},
			},
			Query: &QueryDraft{
				Default:       "client",
				Subqueries:    map[string]string{"mpc": "model,provider"},
				Groups:        map[string]string{"combo": "client,model"},
				OutputColumns: []string{"requests", "total"},
			},
		},
	}
}

// doRequest 执行一次带 JSON body 的请求。
func doRequest(h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	h.ServeHTTP(rec, req)
	return rec
}

// TestConfigGet:/api/config 返回编辑模型与 revision;键集合精确;handler
// 不自行加工 store 的数据。
func TestConfigGet(t *testing.T) {
	store := &fakeConfigStore{view: sampleConfigView()}
	h, _ := newTestServer(t, WithConfigStore(store))

	rec := doGet(h, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	assertKeys(t, "config 顶层", m, "revision", "config")
	if m["revision"] != "abc123" {
		t.Errorf("revision 应原样透出,实际 %v", m["revision"])
	}
	cfg := m["config"].(map[string]any)
	assertKeys(t, "config.config", cfg, "daemon", "log", "clients", "routers", "provider_aliases", "query")
	assertKeys(t, "config.daemon", cfg["daemon"].(map[string]any), "poll_interval", "autostart")
	assertKeys(t, "config.log", cfg["log"].(map[string]any), "level", "dir", "max_days")
	client := cfg["clients"].([]any)[0].(map[string]any)
	assertKeys(t, "config.clients[0]", client, "name", "enabled", "router", "paths")
	router := cfg["routers"].([]any)[0].(map[string]any)
	assertKeys(t, "config.routers[0]", router, "name", "db_path")
	alias := cfg["provider_aliases"].([]any)[0].(map[string]any)
	assertKeys(t, "config.provider_aliases[0]", alias, "key", "value")
	// 敏感内部状态不下发:data_dir 不出现在任何键中。
	if strings.Contains(rec.Body.String(), "data_dir") {
		t.Errorf("配置载荷不得包含 data_dir:\n%s", rec.Body.String())
	}
}

// TestConfigGet_StoreError:store 读取失败映射 500 与双语错误。
func TestConfigGet_StoreError(t *testing.T) {
	store := &fakeConfigStore{viewErr: errors.New("disk gone")}
	h, _ := newTestServer(t, WithConfigStore(store))
	rec := doGet(h, "/api/config")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("应 500,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
}

// TestConfigPut_RoundTrip:保存草稿把 revision 与完整草稿原样交给 store,
// 响应返回新 revision 与规范化配置。
func TestConfigPut_RoundTrip(t *testing.T) {
	store := &fakeConfigStore{
		view: sampleConfigView(),
		applyResult: ConfigApplyResult{
			Changed: true, Revision: "def456", Config: sampleConfigView().Config,
			SuggestedSteps: []string{"restart"},
		},
	}
	h, _ := newTestServer(t, WithConfigStore(store))

	draft := sampleConfigView().Config
	rec := doRequest(h, http.MethodPut, "/api/config", map[string]any{
		"revision": "abc123",
		"config":   draft,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	if !store.called {
		t.Fatal("保存必须经过 ConfigStore.Apply")
	}
	if store.gotRevision != "abc123" {
		t.Errorf("store 收到的 revision 应为 abc123,实际 %q", store.gotRevision)
	}
	if !reflect.DeepEqual(store.gotDraft, draft) {
		t.Errorf("store 收到的草稿应与请求一致:\n期望 %+v\n实际 %+v", draft, store.gotDraft)
	}
	m := decodeJSON(t, rec)
	assertKeys(t, "config 保存响应", m, "revision", "config", "changed", "warnings", "suggested_steps")
	if m["revision"] != "def456" || m["changed"] != true {
		t.Errorf("响应应含新 revision 与 changed=true,实际 %v / %v", m["revision"], m["changed"])
	}
}

// TestConfigPut_PartialFailure:配置已写入但副作用部分失败仍为 200,
// warnings 非空传达「已保存但需要处理」。
func TestConfigPut_PartialFailure(t *testing.T) {
	store := &fakeConfigStore{
		view: sampleConfigView(),
		applyResult: ConfigApplyResult{
			Changed: true, Revision: "def456", Config: sampleConfigView().Config,
			Warnings: []string{"autostart sync failed / 自启同步失败"},
		},
	}
	h, _ := newTestServer(t, WithConfigStore(store))
	rec := doRequest(h, http.MethodPut, "/api/config", map[string]any{
		"revision": "abc123", "config": sampleConfigView().Config,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("部分失败应 200(已保存),实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	warns, _ := m["warnings"].([]any)
	if len(warns) == 0 {
		t.Errorf("部分失败响应应含非空 warnings:\n%s", rec.Body.String())
	}
}

// TestConfigPut_Conflict:revision 冲突映射 409,响应携带当前 revision
// 供前端重载,不自动覆盖他处修改。
func TestConfigPut_Conflict(t *testing.T) {
	store := &fakeConfigStore{view: sampleConfigView(), applyErr: ErrConfigRevisionConflict}
	h, _ := newTestServer(t, WithConfigStore(store))
	rec := doRequest(h, http.MethodPut, "/api/config", map[string]any{
		"revision": "stale", "config": sampleConfigView().Config,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("应 409,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	errObj, ok := m["error"].(map[string]any)
	if !ok || strings.TrimSpace(errObj["message"].(string)) == "" {
		t.Fatalf("冲突响应应含 error.message:\n%s", rec.Body.String())
	}
	if m["revision"] != "abc123" {
		t.Errorf("冲突响应应携带 store 当前 revision 供重载,实际 %v", m["revision"])
	}
}

// TestConfigPut_Invalid:校验失败映射 400,错误 message 非空。
func TestConfigPut_Invalid(t *testing.T) {
	store := &fakeConfigStore{
		view:     sampleConfigView(),
		applyErr: &ConfigInvalidError{Message: "invalid log level / 无效的日志级别"},
	}
	h, _ := newTestServer(t, WithConfigStore(store))
	rec := doRequest(h, http.MethodPut, "/api/config", map[string]any{
		"revision": "abc123", "config": sampleConfigView().Config,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应 400,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	msg := decodeJSON(t, rec)["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "invalid log level") {
		t.Errorf("错误 message 应含校验详情,实际 %q", msg)
	}
}

// TestConfigPut_OmitsQuery:PUT 载荷省略 query 时 store 收到 Query==nil
// (保持磁盘原样语义),不按缺字段拒绝。
func TestConfigPut_OmitsQuery(t *testing.T) {
	store := &fakeConfigStore{view: sampleConfigView()}
	h, _ := newTestServer(t, WithConfigStore(store))
	body := map[string]any{
		"revision": "abc123",
		"config": map[string]any{
			"daemon":           map[string]any{"poll_interval": 9, "autostart": false},
			"log":              map[string]any{"level": "", "dir": "", "max_days": 0},
			"clients":          []any{},
			"routers":          []any{},
			"provider_aliases": []any{},
		},
	}
	rec := doRequest(h, http.MethodPut, "/api/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("省略 query 应 200,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
	if store.gotDraft.Query != nil {
		t.Errorf("store 应收到 Query==nil(保持原样),实际 %+v", store.gotDraft.Query)
	}
}

// TestConfigPut_InternalError:非冲突非校验错误映射 500。
func TestConfigPut_InternalError(t *testing.T) {
	store := &fakeConfigStore{view: sampleConfigView(), applyErr: errors.New("disk on fire")}
	h, _ := newTestServer(t, WithConfigStore(store))
	rec := doRequest(h, http.MethodPut, "/api/config", map[string]any{
		"revision": "abc123", "config": sampleConfigView().Config,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("应 500,实际 %d:\n%s", rec.Code, rec.Body.String())
	}
}

// TestConfigPut_BadRequest:非法 JSON、缺 revision、缺 config 均 400,
// 且不触碰 store。
func TestConfigPut_BadRequest(t *testing.T) {
	store := &fakeConfigStore{view: sampleConfigView()}
	h, _ := newTestServer(t, WithConfigStore(store))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader("{not json"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400,实际 %d", rec.Code)
	}

	for _, body := range []map[string]any{
		{"config": sampleConfigView().Config},
		{"revision": "abc123"},
	} {
		rec := doRequest(h, http.MethodPut, "/api/config", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("缺字段请求应 400,实际 %d:\n%s", rec.Code, rec.Body.String())
		}
	}
	if store.called {
		t.Error("非法请求不得调用 store.Apply")
	}
}

// ---- 真适配层(configapp 绑定) ----

// setupConfigHome 把 HOME 指向临时目录并写入初始 config.toml,返回 home 路径。
// control.NewManager 会在该 HOME 下建 .token-usage/(与 cli 集成测试同法)。
func setupConfigHome(t *testing.T, initial string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if initial != "" {
		if err := os.MkdirAll(filepath.Join(home, ".token-usage"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".token-usage", "config.toml"), []byte(initial), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// newRealConfigStore 构造绑定真实 configapp.Application 的 store。
func newRealConfigStore(t *testing.T, home string) ConfigStore {
	t.Helper()
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: home, GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	app, err := newConfigAppStore(home, env, mgr, service.NewAutoStartManager())
	if err != nil {
		t.Fatalf("newConfigAppStore: %v", err)
	}
	return app
}

// newRealConfigStoreWithAutoStart 同 newRealConfigStore,但自启管理器可注入
// (触发「配置已保存但副作用部分失败」路径)。
func newRealConfigStoreWithAutoStart(t *testing.T, home string, asm service.AutoStartManager) ConfigStore {
	t.Helper()
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: home, GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	app, err := newConfigAppStore(home, env, mgr, asm)
	if err != nil {
		t.Fatalf("newConfigAppStore: %v", err)
	}
	return app
}

// TestConfigAppStore_PartialFailure:自启同步失败时配置仍已落盘——响应
// 200、warnings 非空、revision 更新(「已保存但需要处理」,绝不报成保存失败)。
func TestConfigAppStore_PartialFailure(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStoreWithAutoStart(t, home, &failingAutoStart{})
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Daemon.AutoStart = true // 触发自启定义同步(将失败)

	res, err := store.Apply(ctx, view.Revision, draft)
	if err != nil {
		t.Fatalf("部分失败仍应成功返回(配置已落盘),实际错误: %v", err)
	}
	if !res.Changed || res.Revision == "" {
		t.Fatalf("应 changed 且 revision 更新,实际 %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("副作用失败应产生非空 warnings,实际 %+v", res)
	}
	// 落盘事实:autostart=true 已写入。
	view2, _ := store.Current(ctx)
	if !view2.Config.Daemon.AutoStart {
		t.Error("autostart 应已落盘")
	}
}

// failingAutoStart 是自启定义同步恒失败的 service.AutoStartManager 替身,
// 用于触发 ApplyConfig 的「配置已保存但副作用部分失败」路径。
type failingAutoStart struct{}

func (f *failingAutoStart) Enable(service.Options) error  { return errors.New("sync failed") }
func (f *failingAutoStart) Disable(service.Options) error { return nil }
func (f *failingAutoStart) Status(service.Options) (service.AutoStartStatus, error) {
	return service.AutoStartStatus{}, nil
}
func (f *failingAutoStart) Platform() string { return "test" }

// TestConfigAppStore_BrokenQueryKept:磁盘 query 段处于问题态(非表根值)
// 时,一次不含 query 的草稿(用户只改了其他区块)保存后 query 段原样保留
// ——既有合法值与坏值都不被回退草稿静默改写,diagnostics 持续可见。
func TestConfigAppStore_BrokenQueryKept(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n"+
		"[daemon]\npoll_interval = 8\n"+
		"[\"query\"]\n\"default\" = \"model\"\n[query.groups]\nbad = \"nope\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("问题态配置应可读: %v", err)
	}
	if len(view.Config.Query.Diagnostics) == 0 {
		t.Fatalf("问题态配置应下发 diagnostics,实际 %+v", view.Config.Query)
	}
	// 草稿只改 log.max_days;query 保持 GET 回退草稿但不回传(nil)。
	draft := view.Config
	draft.Log.MaxDays = 21
	draft.Query = nil
	res, err := store.Apply(ctx, view.Revision, draft)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"default" = "model"`) || !strings.Contains(string(raw), `"bad" = "nope"`) {
		t.Errorf("query 段应原样保留(default=model 与坏值都在):\n%s", raw)
	}
	if !strings.Contains(string(raw), "max_days = 21") {
		t.Errorf("非 query 区块应正常落盘:\n%s", raw)
	}
	if !res.Changed {
		t.Error("其他区块变更应 changed=true")
	}
	view2, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if len(view2.Config.Query.Diagnostics) == 0 {
		t.Error("问题态未修复时 diagnostics 应持续可见")
	}
	if view2.Config.Daemon.PollInterval != 8 {
		t.Errorf("daemon 应原样保留,实际 %d", view2.Config.Daemon.PollInterval)
	}
}

// TestConfigAppStore_BrokenQueryHeals:磁盘 query 段处于问题态时,明确
// 携带 query 的草稿保存后问题态清除、落盘文件不再含坏值,再 GET 无
// diagnostics(用户经 diagnostics 知情后的修复路径)。
func TestConfigAppStore_BrokenQueryHeals(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\nquery = \"broken\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("问题态配置应可读: %v", err)
	}
	if len(view.Config.Query.Diagnostics) == 0 {
		t.Fatalf("问题态配置应下发 diagnostics,实际 %+v", view.Config.Query)
	}
	draft := view.Config
	draft.Daemon.PollInterval = 5
	// Query 为 GET 下发的回退草稿(明确选择修复):指针非 nil 即表示覆盖。
	if _, err := store.Apply(ctx, view.Revision, draft); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "broken") {
		t.Errorf("落盘文件不应再含问题态坏值:\n%s", raw)
	}
	view2, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if len(view2.Config.Query.Diagnostics) != 0 {
		t.Errorf("修复后不应再有 diagnostics:%v", view2.Config.Query.Diagnostics)
	}
}

// TestConfigAppStore_Defaults:CurrentDefaults 返回全默认编辑模型,与缺失
// 配置文件时 Current 的草稿一致(默认值定义只存在于后端一份)。
func TestConfigAppStore_Defaults(t *testing.T) {
	home := setupConfigHome(t, "")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	def, err := store.CurrentDefaults(ctx)
	if err != nil {
		t.Fatalf("defaults 构造失败: %v", err)
	}
	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("缺失文件 Current 失败: %v", err)
	}
	if !reflect.DeepEqual(def, view.Config) {
		t.Errorf("defaults 应与缺失文件时的编辑模型一致:\n默认 %+v\n实际 %+v", def, view.Config)
	}
}

// TestConfigAppStore_DuplicateAlias:草稿中出现重复别名 key 被 400 拒绝。
func TestConfigAppStore_DuplicateAlias(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.ProviderAliases = []AliasDraft{
		{Key: "dup", Value: "A"},
		{Key: "dup", Value: "B"},
	}
	_, err = store.Apply(ctx, view.Revision, draft)
	var inv *ConfigInvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("重复别名应返回校验错误,实际 %v", err)
	}
	if !strings.Contains(inv.Message, "dup") {
		t.Errorf("错误应定位到重复别名,实际 %q", inv.Message)
	}
}

// TestConfigAppStore_GetMissingFile:配置文件不存在时 GET 返回默认编辑模型
// 与 sentinel revision(与 ApplyConfig 锁内重读同口径),不报错。
func TestConfigAppStore_GetMissingFile(t *testing.T) {
	home := setupConfigHome(t, "")
	store := newRealConfigStore(t, home)

	view, err := store.Current(context.Background())
	if err != nil {
		t.Fatalf("缺失配置文件应可读: %v", err)
	}
	if view.Revision == "" {
		t.Error("缺失文件也应有 sentinel revision")
	}
	// 全部注册客户端都出现在编辑模型中(前端看到完整可编辑面)。
	names := map[string]bool{}
	for _, c := range view.Config.Clients {
		names[c.Name] = true
	}
	for _, want := range runtimecfg.RegisteredClients() {
		if !names[want] {
			t.Errorf("编辑模型缺注册客户端 %q", want)
		}
	}
	if len(view.Config.Routers) == 0 {
		t.Error("编辑模型应含注册路由")
	}
	// query 输出列回退默认七列。
	if want := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"}; !reflect.DeepEqual(view.Config.Query.OutputColumns, want) {
		t.Errorf("无 query 配置时输出列应回退默认七列,实际 %v", view.Config.Query.OutputColumns)
	}
	if view.Config.Query.Default != "client" {
		t.Errorf("无 query 配置时默认视图应回退 client,实际 %q", view.Config.Query.Default)
	}
}

// TestConfigAppStore_RoundTrip:GET→改值→PUT→再 GET 的完整持久化链路:
// 新值落盘、revision 更新、副作用建议返回;改回原值后 changed=false。
func TestConfigAppStore_RoundTrip(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Daemon.PollInterval = 9
	draft.Log.Level = "debug"
	draft.ProviderAliases = append(draft.ProviderAliases, AliasDraft{Key: "test-key", Value: "Test Value"})
	draft.Query.Default = "model"

	res, err := store.Apply(ctx, view.Revision, draft)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if !res.Changed || res.Revision == "" || res.Revision == view.Revision {
		t.Fatalf("保存后应 changed 且 revision 更新,实际 changed=%v rev=%q", res.Changed, res.Revision)
	}

	view2, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if view2.Revision != res.Revision {
		t.Errorf("回读 revision 应与保存结果一致 %q,实际 %q", res.Revision, view2.Revision)
	}
	if view2.Config.Daemon.PollInterval != 9 || view2.Config.Log.Level != "debug" ||
		view2.Config.Query.Default != "model" {
		t.Errorf("保存值未落盘:%+v", view2.Config)
	}
	found := false
	for _, a := range view2.Config.ProviderAliases {
		if a.Key == "test-key" && a.Value == "Test Value" {
			found = true
		}
	}
	if !found {
		t.Errorf("新增别名未落盘:%+v", view2.Config.ProviderAliases)
	}

	// 原样重存(值改回):changed=false 且不产生新写入。
	view3, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	res2, err := store.Apply(ctx, view3.Revision, view3.Config)
	if err != nil {
		t.Fatalf("原样保存失败: %v", err)
	}
	if res2.Changed {
		t.Errorf("原样保存应 changed=false,实际 %+v", res2)
	}
}

// TestConfigAppStore_Conflict:用过期 revision 保存返回 ErrConfigRevisionConflict,
// 磁盘不被写入。
func TestConfigAppStore_Conflict(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Daemon.PollInterval = 42
	_, err = store.Apply(ctx, "00000000", draft)
	if !errors.Is(err, ErrConfigRevisionConflict) {
		t.Fatalf("过期 revision 应返回冲突错误,实际 %v", err)
	}
	view2, _ := store.Current(ctx)
	if view2.Config.Daemon.PollInterval == 42 {
		t.Error("冲突保存不得落盘")
	}
}

// TestConfigAppStore_InvalidQuery:query 定义不合法(自定义视图少于两个维度)
// 返回 *ConfigInvalidError,磁盘不被写入。
func TestConfigAppStore_InvalidQuery(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Query.Subqueries = map[string]string{"bad": "model"}
	_, err = store.Apply(ctx, view.Revision, draft)
	var inv *ConfigInvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("非法 query 应返回校验错误,实际 %v", err)
	}
	if !strings.Contains(inv.Message, "bad") {
		t.Errorf("校验错误应定位到非法定义,实际 %q", inv.Message)
	}
}

// TestConfigAppStore_InvalidLog:非法日志级别被 400 拒绝。
func TestConfigAppStore_InvalidLog(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Log.Level = "nope"
	_, err = store.Apply(ctx, view.Revision, draft)
	var inv *ConfigInvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("非法日志级别应返回校验错误,实际 %v", err)
	}
}

// TestConfigAppStore_MixedBrokenKeepsOutputColumns:视图定义无效但
// query.output.columns 有效时,GET 的输出列仍为配置列(与 /api/dashboard
// 的 columns 及 CLI 静态表同口径),default/视图回退并附 diagnostics。
func TestConfigAppStore_MixedBrokenKeepsOutputColumns(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n"+
		"[\"query\"]\n\"default\" = \"bogus-view\"\n"+
		"[query.groups]\nbroken = \"nope\"\n"+
		"[query.output]\n\"columns\" = [\"total\"]\n")
	store := newRealConfigStore(t, home)

	view, err := store.Current(context.Background())
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(view.Config.Query.Diagnostics) == 0 {
		t.Fatal("无效视图定义应下发 diagnostics")
	}
	if got := view.Config.Query.OutputColumns; len(got) != 1 || got[0] != "total" {
		t.Errorf("有效单列配置应原样下发,实际 %v", got)
	}
	if view.Config.Query.Default != "client" {
		t.Errorf("无效 default 应回退 client,实际 %q", view.Config.Query.Default)
	}
}

// TestConfigAppStore_RejectsUnknownClient:草稿中出现未注册客户端名被 400 拒绝
// (白名单门控,不走静默忽略)。
func TestConfigAppStore_RejectsUnknownClient(t *testing.T) {
	home := setupConfigHome(t, "data_dir = \""+t.TempDir()+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Clients = append(draft.Clients, ClientDraft{Name: "ghost"})
	_, err = store.Apply(ctx, view.Revision, draft)
	var inv *ConfigInvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("未注册客户端应返回校验错误,实际 %v", err)
	}
}

// TestConfigAppStore_KeepsDataDir:草稿不含 data_dir,保存后磁盘 data_dir
// 保持不变(不触发迁移确认链路)。
func TestConfigAppStore_KeepsDataDir(t *testing.T) {
	dataDir := t.TempDir()
	home := setupConfigHome(t, "data_dir = \""+dataDir+"\"\n")
	store := newRealConfigStore(t, home)
	ctx := context.Background()

	view, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	draft := view.Config
	draft.Daemon.PollInterval = 7
	if _, err := store.Apply(ctx, view.Revision, draft); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// 序列化形态不绑定引号风格(literal/basic string 均合法),只断言路径值在盘。
	if !strings.Contains(string(raw), dataDir) {
		t.Errorf("data_dir 应原样保留:\n%s", raw)
	}
}
