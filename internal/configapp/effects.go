// Package configapp 承载配置应用层：把「用户保存了一份新 config」翻译成结构化动作与说明。
//
// 本文件只实现纯函数部分（影响矩阵），不访问 FS/DB/lock。
// AnalyzeConfigEffects 比较两份已解析（effective）config，按配置变化影响矩阵
// 输出 collect 动作、运行时是否变化、warning 与 data_dir 迁移项。
// 写盘、加锁、自启同步、stale metadata 清理由 ApplyConfig 实现。
package configapp

import (
	"fmt"
	"sort"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// DataDirMigration 描述一次 data_dir 变更需手工迁移的产物。
// Items 说明需要手工搬运什么；PID/lock/runtime-state 不在迁移范围（由 stale metadata 清理接管）。
type DataDirMigration struct {
	From  string
	To    string
	Items []string
}

// ConfigEffects 是 AnalyzeConfigEffects 的输出，描述 effective config 变化触发的动作集合。
//
//   - RuntimeChanged：当前运行中的 daemon 需要重载/重启才能生效（autostart 单独变化除外）；
//   - FullCollectClients：需执行 `collect all --client X` 的 client（新版 collect all 已含 router 阶段，
//     因此同一 client 进入此列表后不再出现在 RouterBackfillClients）；
//   - RouterBackfillClients：只需 `collect router --client X`（client 已启用、router 归因层变化）；
//   - Warnings：既有数据不自动迁移/删除等需要用户知晓的事项；
//   - DataDirMigration：data_dir 变化时非 nil。
//
// 所有 client 列表已排序并去重。
type ConfigEffects struct {
	RuntimeChanged        bool
	FullCollectClients    []string
	RouterBackfillClients []string
	Warnings              []string
	DataDirMigration      *DataDirMigration
}

// AnalyzeConfigEffects 比较 previous/current 两份 effective config，返回变化影响。
//
// 纯函数：不读取文件/数据库，不依赖 home/GOOS（入参应是已 resolved 的有效配置）。
// 受影响 client 的计算只遍历 current client map，复杂度 O(n)，当前最多六个已注册 client。
// 同一 client 同时命中 full collect 与 router backfill 时只保留 full collect。
func AnalyzeConfigEffects(previous, current *config.Config) ConfigEffects {
	prev := normalize(previous)
	curr := normalize(current)

	full := map[string]struct{}{}
	router := map[string]struct{}{}
	warns := map[string]struct{}{}
	var migration *DataDirMigration

	// --- data_dir 变化（最先判定，决定日志迁移 warning 的措辞来源）---
	if prev.DataDir != curr.DataDir {
		migration = &DataDirMigration{
			From: prev.DataDir,
			To:   curr.DataDir,
			Items: []string{
				"usage.db",
				"logs",
			},
		}
		warns[warningDataDirManualMigration] = struct{}{}
	}

	// --- daemon / log 全局字段 ---
	if prev.Daemon.PollInterval != curr.Daemon.PollInterval {
		// 运行中需重载，无 collect
	}
	if prev.Log.Level != curr.Log.Level || prev.Log.MaxDays != curr.Log.MaxDays {
		// 运行中需重载，无 collect
	}
	if prev.Log.Dir != curr.Log.Dir {
		warns[warningLogDirNotMigrated] = struct{}{}
	}

	// --- provider alias 变化：backfill 所有已启用且配置 router 的 client ---
	aliasChanged := !stringMapEqual(prev.ProviderAliases, curr.ProviderAliases)
	if aliasChanged {
		for name, c := range curr.Clients {
			if c.Enabled && c.Router != "" {
				router[name] = struct{}{}
			}
		}
		warns[warningAliasAttribution] = struct{}{}
	}

	// --- router db_path 变化：backfill 绑定该 router 的已启用 client ---
	routerDBChanged := map[string]struct{}{}
	for name, r := range curr.Routers {
		if prevR, ok := prev.Routers[name]; !ok || prevR.DBPath != r.DBPath {
			routerDBChanged[name] = struct{}{}
		}
	}
	for name := range prev.Routers {
		if _, ok := curr.Routers[name]; !ok {
			// router 在 current 被整体移除：视为该 router 的 db_path 语义消失，
			// 绑定它的 client 会被下方 client router R→空 规则单独处理，这里不重复。
			routerDBChanged[name] = struct{}{}
		}
	}
	for cname, c := range curr.Clients {
		if !c.Enabled || c.Router == "" {
			continue
		}
		if _, changed := routerDBChanged[c.Router]; changed {
			router[cname] = struct{}{}
		}
	}
	for name := range routerDBChanged {
		// 只对仍存在于 current 的 router 给归因 warning（被移除的 router 走 client router→空 提示）。
		if _, ok := curr.Routers[name]; ok && hasRouterDBPathDiff(prev, curr, name) {
			warns[warningRouterDBPathAttribution(name)] = struct{}{}
		}
	}

	// --- 逐 client 比较（按 current client map 遍历，O(n)）---
	for name, c := range curr.Clients {
		pc, hadPrev := prev.Clients[name]
		prevOn := hadPrev && pc.Enabled
		currOn := c.Enabled

		pathChanged := !stringMapEqual(safePaths(pc.Paths), safePaths(c.Paths))
		routerChanged := clientRouterOf(pc) != clientRouterOf(c)

		switch {
		case !prevOn && currOn:
			// disabled→enabled：全量采集该 client。
			// 若同时配了 router，新版 collect all 已含 router 阶段，由 full 去重覆盖。
			full[name] = struct{}{}
		case prevOn && !currOn:
			// enabled→disabled：无采集，warning 已有历史不自动删除。
			warns[warningClientDisabledHistoryKept(name)] = struct{}{}
		case prevOn && currOn:
			// 两侧均启用。
			if pathChanged {
				full[name] = struct{}{}
				warns[warningOldPathHistoryNotDeleted(name)] = struct{}{}
			}
			if routerChanged {
				if c.Router != "" {
					// 已启用 client router 变化且新值非空（空→R 或 R1→R2）：router backfill。
					router[name] = struct{}{}
				}
				if pc.Router != "" && c.Router != "" {
					// R1→R2：旧关联不自动清理。
					warns[warningRouterRebindOldAssoc(name)] = struct{}{}
				}
				if pc.Router != "" && c.Router == "" {
					// R→空：无 backfill（无 router 可采）；已有 provider/model 关联不自动删除。
					warns[warningRouterRemovedAssocKept(name)] = struct{}{}
				}
			}
		default:
			// 两侧均禁用。
			if pathChanged {
				warns[warningDisabledClientPathChanged(name)] = struct{}{}
			}
			if routerChanged {
				warns[warningDisabledClientRouterChanged(name)] = struct{}{}
			}
		}
	}

	// client 出现在 previous 但 current 缺失：视为 enabled→disabled（如果之前启用）。
	for name, pc := range prev.Clients {
		if _, ok := curr.Clients[name]; ok {
			continue
		}
		if pc.Enabled {
			warns[warningClientDisabledHistoryKept(name)] = struct{}{}
		}
	}

	// --- 同一 client 同时命中 full 与 router：只保留 full ---
	for name := range full {
		delete(router, name)
	}

	// --- RuntimeChanged：除「仅 autostart」外任何 effective 变化都为 true ---
	runtimeChanged := runtimeAffectingChange(prev, curr, aliasChanged, len(full) > 0, len(router) > 0, migration != nil)

	return ConfigEffects{
		RuntimeChanged:        runtimeChanged,
		FullCollectClients:    sortedKeys(full),
		RouterBackfillClients: sortedKeys(router),
		Warnings:              sortedKeys(warns),
		DataDirMigration:      migration,
	}
}

// runtimeAffectingChange 判定运行中 daemon 是否需要重载/重启。
// 「仅 autostart 变化」不算 runtime changed（只影响下次登录/开机的定义）。
func runtimeAffectingChange(prev, curr normalizedCfg, aliasChanged bool, hasFull, hasRouter bool, hasMigration bool) bool {
	if prev.DataDir != curr.DataDir {
		return true
	}
	if prev.Daemon.PollInterval != curr.Daemon.PollInterval {
		return true
	}
	if prev.Log.Level != curr.Log.Level || prev.Log.Dir != curr.Log.Dir || prev.Log.MaxDays != curr.Log.MaxDays {
		return true
	}
	if aliasChanged || hasFull || hasRouter || hasMigration {
		return true
	}
	// client/router path 变化若未触发 full/router（如两侧均禁用仅 warning），仍属运行时配置变化。
	if clientRouterRuntimeDiff(prev, curr) {
		return true
	}
	return false
}

// clientRouterRuntimeDiff 检测 client 的 enabled/router/paths 变化（不含纯 autostart）。
func clientRouterRuntimeDiff(prev, curr normalizedCfg) bool {
	names := map[string]struct{}{}
	for n := range prev.Clients {
		names[n] = struct{}{}
	}
	for n := range curr.Clients {
		names[n] = struct{}{}
	}
	for n := range names {
		pc := prev.Clients[n]
		cc := curr.Clients[n]
		if pc.Enabled != cc.Enabled {
			return true
		}
		if clientRouterOf(pc) != clientRouterOf(cc) {
			return true
		}
		if !stringMapEqual(safePaths(pc.Paths), safePaths(cc.Paths)) {
			return true
		}
	}
	// router db_path 变化
	for name, r := range curr.Routers {
		if pr, ok := prev.Routers[name]; !ok || pr.DBPath != r.DBPath {
			return true
		}
	}
	for name := range prev.Routers {
		if _, ok := curr.Routers[name]; !ok {
			return true
		}
	}
	return false
}

// hasRouterDBPathDiff 报告 router name 在 prev/curr 间 db_path 是否真正不同（且 current 存在）。
func hasRouterDBPathDiff(prev, curr normalizedCfg, name string) bool {
	cr, ok := curr.Routers[name]
	if !ok {
		return false
	}
	pr, had := prev.Routers[name]
	if !had {
		return true
	}
	return pr.DBPath != cr.DBPath
}

// ---- 辅助类型与函数 ----

// normalizedCfg 把 *config.Config 归一化为非 nil、含非 nil map 的等价结构，
// 使后续比较不必到处判 nil。AnalyzeConfigEffects 的比较都基于归一化结果。
type normalizedCfg struct {
	DataDir         string
	Daemon          config.DaemonConfig
	Log             config.LogConfig
	Clients         map[string]config.Client
	Routers         map[string]config.RouterConfig
	ProviderAliases map[string]string
}

func normalize(c *config.Config) normalizedCfg {
	n := normalizedCfg{}
	if c == nil {
		n.Clients = map[string]config.Client{}
		n.Routers = map[string]config.RouterConfig{}
		n.ProviderAliases = map[string]string{}
		return n
	}
	n.DataDir = c.DataDir
	n.Daemon = c.Daemon
	n.Log = c.Log
	n.Clients = copyClients(c.Clients)
	n.Routers = copyRouters(c.Routers)
	n.ProviderAliases = copyStringMap(c.ProviderAliases)
	return n
}

func copyClients(in map[string]config.Client) map[string]config.Client {
	out := make(map[string]config.Client, len(in))
	for k, v := range in {
		v.Paths = copyStringMap(v.Paths)
		out[k] = v
	}
	return out
}

func copyRouters(in map[string]config.RouterConfig) map[string]config.RouterConfig {
	out := make(map[string]config.RouterConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func safePaths(p map[string]string) map[string]string {
	if p == nil {
		return map[string]string{}
	}
	return p
}

// clientRouterOf 取 client 的 router 名（空字符串表示未配）。
func clientRouterOf(c config.Client) string {
	return c.Router
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- 稳定 warning 文案（测试与 CLI 共享，避免散落字符串）----
//
// 这些函数返回稳定 warning 文案，供 AnalyzeConfigEffects 写入与 ApplyConfig/CLI 展示复用。

func warningOldPathHistoryNotDeleted(client string) string {
	return ui.Bi(
		fmt.Sprintf("history under the old paths of client %q is not deleted automatically", client),
		fmt.Sprintf("client %q 旧路径历史不会自动删除", client),
	)
}
func warningDisabledClientPathChanged(client string) string {
	return ui.Bi(
		fmt.Sprintf("client %q is disabled; path changes are not collected, run a full collection after enabling it, and existing history is not deleted automatically", client),
		fmt.Sprintf("client %q 已禁用，路径变化不采集；启用后再全量采集，已有历史不会自动删除", client),
	)
}
func warningClientDisabledHistoryKept(client string) string {
	return ui.Bi(
		fmt.Sprintf("client %q is disabled; existing history is not deleted automatically", client),
		fmt.Sprintf("client %q 已禁用，已有历史不会自动删除", client),
	)
}
func warningRouterRebindOldAssoc(client string) string {
	return ui.Bi(
		fmt.Sprintf("old router associations of client %q are not cleaned up automatically", client),
		fmt.Sprintf("client %q 旧的 router 关联不会自动清理", client),
	)
}
func warningDisabledClientRouterChanged(client string) string {
	return ui.Bi(
		fmt.Sprintf("client %q is disabled; router changes are not collected, run the corresponding full or router collection after enabling it", client),
		fmt.Sprintf("client %q 已禁用，router 变化不采集；启用后再执行对应全量或 router 采集", client),
	)
}
func warningRouterRemovedAssocKept(client string) string {
	return ui.Bi(
		fmt.Sprintf("client %q no longer has a router; existing provider/model associations are not deleted automatically", client),
		fmt.Sprintf("client %q 已移除 router，既有 provider/model 关联不会自动删除", client),
	)
}
func warningRouterDBPathAttribution(router string) string {
	return ui.Bi(
		fmt.Sprintf("router %q db_path changed; existing attribution is not rewritten automatically", router),
		fmt.Sprintf("router %q db_path 变化，既有归因不会自动改写", router),
	)
}

// 字符串型 warning 用 const 直接定义更合适。
const (
	warningDataDirManualMigration = "data_dir change requires manually migrating usage.db/logs / data_dir 变化需手工迁移 usage.db/logs，PID/lock/runtime-state 不迁移"
	warningAliasAttribution       = "provider_aliases changed; existing provider/model attribution needs to be re-run / provider_aliases 变化，既有 provider/model 需重新归因"
	warningLogDirNotMigrated      = "log.dir changed; existing logs are not migrated automatically / log.dir 变化，既有日志不会自动迁移"
)
