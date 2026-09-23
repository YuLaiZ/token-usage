package web

// webconfig_app.go 是 ConfigStore 的生产实现:绑定 configapp.Application,
// 把网页编辑模型与用户配置模型互转。写路径的全部校验与持久化最终由
// configapp.Application.ApplyConfig 在 control lock 内完成(revision 校验、
// 原子整替、自启同步、data_dir 前置检查);本文件只负责草稿→配置模型的
// 白名单转换、query 定义的提前校验(给前端可理解的 400)与错误归类。

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/querydef"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// configAppStore 是 ConfigStore 的 configapp 绑定实现。
type configAppStore struct {
	home string
	app  *configapp.Application
}

// NewProductionConfigStore 以生产依赖集构造 store:标准运行环境解析、
// 进程控制管理器与自启管理器,与 cli 侧 config set / config TUI 的装配
// 完全同构。serve 与 devdashboard 共用本入口。
func NewProductionConfigStore(home string) (ConfigStore, error) {
	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         runtime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	mgr, err := control.NewManager(home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create process control manager", "创建进程控制管理器失败"), err)
	}
	return newConfigAppStore(home, env, mgr, service.NewAutoStartManager())
}

// newConfigAppStore 构造生产 store。依赖注入形态与 cli 侧 config set /
// config TUI 的装配完全一致(home、运行环境解析、进程控制管理器、自启管理器)。
func newConfigAppStore(home string, env runtimecfg.ResolveEnv, mgr *control.Manager, autoStart service.AutoStartManager) (ConfigStore, error) {
	app, err := configapp.NewApplication(home, env, mgr, autoStart)
	if err != nil {
		return nil, err
	}
	return &configAppStore{home: home, app: app}, nil
}

// Current 读取磁盘配置快照并转换为编辑模型。文件不存在返回 sentinel
// revision 与全默认草稿(configapp.SnapshotRevision 与 ApplyConfig 锁内
// 重读同口径,保证首次保存的 revision 校验可通过);文件存在但解析失败
// 返回错误(损坏配置由人工修复,网页不猜测内容)。
func (s *configAppStore) Current(ctx context.Context) (ConfigView, error) {
	snap, err := runtimecfg.LoadUserConfigSnapshot(runtimecfg.ConfigPath(s.home))
	if err != nil {
		return ConfigView{}, err
	}
	draft := draftFromConfig(snap)
	return ConfigView{
		Revision: hex.EncodeToString(configapp.SnapshotRevision(snap)),
		Config:   draft,
	}, nil
}

// CurrentDefaults 返回「全部默认值」的编辑模型:空用户配置经与 Current
// 同一展开逻辑(注册表全量展开、query 回退默认)。前端「恢复全部默认值」
// 以此形成待保存草稿——默认值定义只存在于后端,不在前端复制第二套。
func (s *configAppStore) CurrentDefaults(ctx context.Context) (ConfigDraft, error) {
	return draftFromConfig(runtimecfg.UserSnapshot{}), nil
}

// Apply 应用配置草稿:hex revision 解码→磁盘快照→草稿合并与提前校验→
// configapp.ApplyConfig。错误归类:冲突透传 ErrConfigRevisionConflict,
// 校验失败包成 *ConfigInvalidError,其余原样返回(handler 映射 500)。
func (s *configAppStore) Apply(ctx context.Context, revision string, draft ConfigDraft) (ConfigApplyResult, error) {
	revBytes, err := hex.DecodeString(strings.TrimSpace(revision))
	if err != nil {
		return ConfigApplyResult{}, &ConfigInvalidError{Message: ui.Bi(
			"invalid revision: reload /api/config and retry",
			"revision 非法：请重新读取 /api/config 后再保存",
		)}
	}
	snap, err := runtimecfg.LoadUserConfigSnapshot(runtimecfg.ConfigPath(s.home))
	if err != nil {
		return ConfigApplyResult{}, err
	}
	if snap.Exists && snap.Config == nil {
		return ConfigApplyResult{}, &ConfigInvalidError{Message: ui.Bi(
			"config file exists but cannot be parsed; fix it manually first",
			"配置文件存在但无法解析，请先手工修复",
		)}
	}
	base := snap.Config
	if base == nil {
		base = &config.Config{}
	}
	merged, err := draftToConfig(base, draft)
	if err != nil {
		return ConfigApplyResult{}, err
	}
	res, err := s.app.ApplyConfig(ctx, revBytes, merged, false)
	if err != nil {
		if errors.Is(err, configapp.ErrConfigChangedExternally) {
			return ConfigApplyResult{}, ErrConfigRevisionConflict
		}
		// 锁内写入前校验失败归 400(类型化错误,不依赖文案);其余(锁、
		// 磁盘 I/O)由 handler 按内部错误处理。
		if errors.Is(err, configapp.ErrConfigValidation) {
			return ConfigApplyResult{}, &ConfigInvalidError{Message: err.Error()}
		}
		// 「已保存但副作用部分失败」不是保存失败:配置已落盘(ConfigApplied
		// 为 true、error 是 PartialErrors 的 errors.Join),按成功路径返回
		// 200,副作用问题以 warnings 表达,绝不把已保存结果报成 500。
		if !res.ConfigApplied {
			return ConfigApplyResult{}, err
		}
	}
	// 规范化后的编辑模型重新从磁盘读取:响应中的 config 是落盘事实,
	// 与请求草稿可能有细微归一差异(如 "default" 级别归一为空串)。
	saved, cerr := s.Current(ctx)
	if cerr != nil {
		return ConfigApplyResult{}, cerr
	}
	warnings := make([]string, 0, len(res.PartialErrors))
	for _, perr := range res.PartialErrors {
		warnings = append(warnings, perr.Error())
	}
	return ConfigApplyResult{
		Changed:        res.Changed,
		Revision:       hex.EncodeToString(res.NewRevision),
		Config:         saved.Config,
		Warnings:       warnings,
		SuggestedSteps: append([]string(nil), res.SuggestedSteps...),
	}, nil
}

// draftFromConfig 把磁盘快照转换为编辑模型:clients/routers 按注册表全量
// 展开(未配置项为零值),路径与级别下发用户层原值(不含推导默认,保证
// GET→PUT 往返不把默认值固化成显式配置);query 段经 querydef 解析,
// 解析失败时回退默认值并附带诊断。
func draftFromConfig(snap runtimecfg.UserSnapshot) ConfigDraft {
	d := ConfigDraft{
		Clients:         make([]ClientDraft, 0, len(runtimecfg.RegisteredClients())),
		Routers:         make([]RouterDraft, 0, len(runtimecfg.RegisteredRouters())),
		ProviderAliases: []AliasDraft{},
	}
	cfg := snap.Config
	if cfg != nil {
		d.Daemon = DaemonDraft{PollInterval: cfg.Daemon.PollInterval, AutoStart: cfg.Daemon.AutoStart}
		d.Log = LogDraft{Level: cfg.Log.Level, Dir: cfg.Log.Dir, MaxDays: cfg.Log.MaxDays}
		for _, a := range sortedAliasKeys(cfg.ProviderAliases) {
			d.ProviderAliases = append(d.ProviderAliases, AliasDraft{Key: a, Value: cfg.ProviderAliases[a]})
		}
	}
	for _, name := range runtimecfg.RegisteredClients() {
		c := ClientDraft{Name: name, Paths: map[string]string{}}
		if cfg != nil {
			if cl, ok := cfg.Clients[name]; ok {
				c.Enabled = cl.Enabled
				c.Router = cl.Router
				// 全量透传磁盘上的 path 键(含当前版本注册表之外的键):
				// 静默丢弃会让下一次保存无声删除磁盘数据。未知键在保存时
				// 会被白名单校验以 400 明确拒绝(与 config set 同约束),
				// 数据可见且不丢失,由用户决定去留。
				for k, v := range cl.Paths {
					c.Paths[k] = v
				}
			}
		}
		d.Clients = append(d.Clients, c)
	}
	for _, name := range runtimecfg.RegisteredRouters() {
		r := RouterDraft{Name: name}
		if cfg != nil {
			if rc, ok := cfg.Routers[name]; ok {
				r.DBPath = rc.DBPath
			}
		}
		d.Routers = append(d.Routers, r)
	}
	q := queryDraftFromRaw(snap)
	d.Query = &q
	return d
}

// sortedAliasKeys 返回别名字典序键列表(map 遍历无序,输出需稳定)。
func sortedAliasKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// queryDraftFromRaw 解析 raw query 段为编辑模型:成功取规范值;失败回退
// 默认(default=client、空视图、默认七列)并把诊断行放进 Diagnostics。
func queryDraftFromRaw(snap runtimecfg.UserSnapshot) QueryDraft {
	in := querydef.Input{RawQuery: rawQueryOf(snap.Config)}
	if snap.Config != nil {
		in.RawQueryTopLevelIssues = convertTopLevelIssues(snap.Config.RawQueryTopLevelIssues)
	}
	d := QueryDraft{
		Default:       string(querydef.DimensionClient),
		Subqueries:    map[string]string{},
		Groups:        map[string]string{},
		OutputColumns: ui.DefaultOutputColumns(),
	}
	defs, err := querydef.Parse(in)
	if err != nil {
		var ve *querydef.ValidationError
		if errors.As(err, &ve) {
			diag := make([]string, 0, len(ve.Issues))
			for _, issue := range ve.Issues {
				diag = append(diag, issue.Path+": "+issue.Message)
			}
			d.Diagnostics = diag
		}
		// 输出列与视图定义解耦:非顶层问题时单独解析 query.output.columns,
		// 与 /api/dashboard 的 columns 及 CLI 静态表同口径——否则「有效单列
		// 配置+无效视图定义」会让页面首开画默认七列而 API 返回配置列。
		if len(in.RawQueryTopLevelIssues) == 0 {
			if cols, oerr := querydef.ParseOutputLayout(in); oerr == nil {
				d.OutputColumns = cols
			}
		}
		return d
	}
	d.Default = defs.Default.Name
	for _, sq := range defs.Subqueries {
		d.Subqueries[sq.Name] = strings.Join(dimensionNames(sq.Dimensions), ",")
	}
	for _, g := range defs.Groups {
		names := make([]string, 0, len(g.Items))
		for _, item := range g.Items {
			names = append(names, item.Name)
		}
		d.Groups[g.Name] = strings.Join(names, ",")
	}
	if len(defs.OutputColumns) > 0 {
		d.OutputColumns = append([]string(nil), defs.OutputColumns...)
	}
	return d
}

// rawQueryOf 在 nil 配置上安全返回 nil。
func rawQueryOf(c *config.Config) map[string]any {
	if c == nil {
		return nil
	}
	return c.RawQuery
}

// convertTopLevelIssues 把 config 层的 raw query 顶层问题映射为 querydef
// 输入形态(两者 Kind 均为稳定字符串)。
func convertTopLevelIssues(in map[string]config.RawQueryTopLevelIssue) map[string]querydef.TopLevelIssue {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]querydef.TopLevelIssue, len(in))
	for k, v := range in {
		out[k] = querydef.TopLevelIssue{Name: v.Name, Kind: string(v.Kind)}
	}
	return out
}

// dimensionNames 把内置维度切片转为名字切片。
func dimensionNames(dims []querydef.BuiltinDimension) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, string(d))
	}
	return out
}

// draftToConfig 把编辑模型合并到磁盘当前配置上,产出完整的用户配置:
// data_dir 等草稿外字段原样保留;clients/routers 按注册表白名单重建;
// query 段重组为 raw map。任何白名单外条目返回 *ConfigInvalidError。
func draftToConfig(base *config.Config, d ConfigDraft) (*config.Config, error) {
	cfg := *base
	cfg.Daemon = config.DaemonConfig{PollInterval: d.Daemon.PollInterval, AutoStart: d.Daemon.AutoStart}
	cfg.Log = config.LogConfig{Level: normalizeLogLevel(d.Log.Level), Dir: d.Log.Dir, MaxDays: d.Log.MaxDays}

	clients := make(map[string]config.Client, len(d.Clients))
	seen := map[string]bool{}
	for _, c := range d.Clients {
		if seen[c.Name] {
			return nil, &ConfigInvalidError{Message: ui.Bi(
				fmt.Sprintf("duplicate client %q in draft", c.Name),
				fmt.Sprintf("草稿中客户端 %q 重复", c.Name),
			)}
		}
		seen[c.Name] = true
		if !containsStr(runtimecfg.RegisteredClients(), c.Name) {
			return nil, &ConfigInvalidError{Message: ui.Bi(
				fmt.Sprintf("unknown client %q", c.Name),
				fmt.Sprintf("未注册的客户端 %q", c.Name),
			)}
		}
		// 空路径表保持 nil:与 TOML 读回的零值形态一致,否则 effective
		// 比较会把「无路径配置」误判为变化,原样保存永远 changed=true。
		var paths map[string]string
		if len(c.Paths) > 0 {
			paths = make(map[string]string, len(c.Paths))
			for k, v := range c.Paths {
				if !containsStr(runtimecfg.RegisteredClientPathKeys(c.Name), k) {
					return nil, &ConfigInvalidError{Message: ui.Bi(
						fmt.Sprintf("client %q does not support path key %q", c.Name, k),
						fmt.Sprintf("客户端 %q 不支持数据来源键 %q", c.Name, k),
					)}
				}
				paths[k] = v
			}
		}
		if c.Router != "" && !containsStr(runtimecfg.RegisteredRouters(), c.Router) {
			return nil, &ConfigInvalidError{Message: ui.Bi(
				fmt.Sprintf("unknown router %q for client %q", c.Router, c.Name),
				fmt.Sprintf("客户端 %q 引用了未注册的路由 %q", c.Name, c.Router),
			)}
		}
		clients[c.Name] = config.Client{Enabled: c.Enabled, Router: c.Router, Paths: paths}
	}
	cfg.Clients = clients

	routers := make(map[string]config.RouterConfig, len(d.Routers))
	seenRouter := map[string]bool{}
	for _, r := range d.Routers {
		if !containsStr(runtimecfg.RegisteredRouters(), r.Name) {
			return nil, &ConfigInvalidError{Message: ui.Bi(
				fmt.Sprintf("unknown router %q", r.Name),
				fmt.Sprintf("未注册的路由 %q", r.Name),
			)}
		}
		// 空配置的 router 不落盘:空 TOML 子表读回即消失,落盘会让
		// 「原样保存」在 effective 比较中永远视为变化(幽灵 diff)。
		if r.DBPath == "" {
			continue
		}
		if seenRouter[r.Name] {
			return nil, &ConfigInvalidError{Message: ui.Bi(
				fmt.Sprintf("duplicate router %q in draft", r.Name),
				fmt.Sprintf("草稿中路由 %q 重复", r.Name),
			)}
		}
		seenRouter[r.Name] = true
		routers[r.Name] = config.RouterConfig{DBPath: r.DBPath}
	}
	cfg.Routers = routers

	var aliases map[string]string
	if len(d.ProviderAliases) > 0 {
		aliases = make(map[string]string, len(d.ProviderAliases))
		seenAlias := map[string]bool{}
		for _, a := range d.ProviderAliases {
			if a.Key == "" {
				return nil, &ConfigInvalidError{Message: ui.Bi(
					"provider alias key must not be empty",
					"供应商别名的原始标签不能为空",
				)}
			}
			if seenAlias[a.Key] {
				return nil, &ConfigInvalidError{Message: ui.Bi(
					fmt.Sprintf("duplicate provider alias %q in draft", a.Key),
					fmt.Sprintf("草稿中供应商别名 %q 重复", a.Key),
				)}
			}
			seenAlias[a.Key] = true
			aliases[a.Key] = a.Value
		}
	}
	cfg.ProviderAliases = aliases

	// query 段仅在草稿明确携带时重建:nil=保持磁盘原样(含问题态遗留),
	// 避免一次无关区块的保存把用户既有查询定义整体替换为回退默认。
	if d.Query != nil {
		cfg.RawQuery = rawQueryFromDraft(*d.Query)
		cfg.RawQueryTopLevelIssues = nil
		// query 段提前校验:给出可理解的 400,而不是让锁内校验失败落进
		// 500。ApplyConfig 锁内会再次校验(权威)。
		if _, err := querydef.Parse(querydef.Input{RawQuery: cfg.RawQuery}); err != nil {
			return nil, &ConfigInvalidError{Message: err.Error()}
		}
	}
	if err := runtimecfg.ValidateUserConfigForWrite(&cfg); err != nil {
		return nil, &ConfigInvalidError{Message: err.Error()}
	}
	return &cfg, nil
}

// normalizeLogLevel 把 "default" 归一为空串(用户层空值,与 config set 同语义)。
func normalizeLogLevel(level string) string {
	if level == "default" {
		return ""
	}
	return level
}

// rawQueryFromDraft 把 query 编辑模型重组为 raw map:值形态与 TOML 序列化
// 器接受的集合一致(string / map[string]any / []any)。空区块不写入,
// 保持配置文件最小。
func rawQueryFromDraft(q QueryDraft) map[string]any {
	m := map[string]any{}
	if strings.TrimSpace(q.Default) != "" {
		m["default"] = strings.TrimSpace(q.Default)
	}
	if len(q.Subqueries) > 0 {
		sub := make(map[string]any, len(q.Subqueries))
		for name, dims := range q.Subqueries {
			sub[name] = dims
		}
		m["subqueries"] = sub
	}
	if len(q.Groups) > 0 {
		groups := make(map[string]any, len(q.Groups))
		for name, items := range q.Groups {
			groups[name] = items
		}
		m["groups"] = groups
	}
	if len(q.OutputColumns) > 0 {
		cols := make([]any, 0, len(q.OutputColumns))
		for _, c := range q.OutputColumns {
			cols = append(cols, c)
		}
		m["output"] = map[string]any{"columns": cols}
	}
	return m
}

// containsStr 报告 slice 是否含 s。
func containsStr(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}
