// internal/engine/workbuddy_persist.go
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// WorkBuddy 写事务合并（与 mimocode 的 mimoPersistRekeyPlan 同层，但合并规则
// 按本需求的确定性合同执行）：collector 产出的 WorkBuddySessionPlan 是会话级
// 目标归属（client/project/title/directory）；engine 在 persistClientBatch 的
// 同一用量库事务内把该会话 WorkBuddy 家族全部历史行收敛到目标 client。
//
// 固定顺序：读家族行并严格校验 client 编码 → 同 ID 身份冲突检测（覆盖家族
// 历史行与本轮新消息，含本轮两个新会话互相冲突）→ 按确定性规则合并出每条
// 逻辑消息/会话的最终行（历史目录兜底参与 project 计算）→ 与现状逐列比较，
// 有差异的会话删除家族行并先写回合并结果（本轮消息随后经现有 UpsertMessages
// 覆盖 token/model，即「本轮消息优先于迁移阶段的旧副本」）→ 提交前校验家族
// 内无非目标残留、project 收敛、会话-消息关联完整。
//
// 合并不改变逻辑消息的请求数与各 token 列（总量守恒）；存在历史双份行时以
// 去重后的逻辑账本为基线。幂等：无差异的会话不删不插，周期复核反复执行不
// 产生额外写入。该政策是会话级分类，不声称还原每条消息发生时的 expert。

// workBuddyMergeBatchSize 是单批处理的计划会话数上限（DAO IN 分块同量级）。
const workBuddyMergeBatchSize = 100

// workBuddyRekeyErrPrefix 是 engine 侧 WorkBuddy 重归属错误的固定可识别片段，
// 与 collector 的 metadata 前缀一同进入 collection_errors.message，供完整复核
// 成功后按片段解决（db.ResolveWorkBuddyErrorsByMessagePattern）。
const workBuddyRekeyErrPrefix = "workbuddy rekey failed:"

// workBuddyStepHook 仅供测试注入：在 applyBatch 的每个步骤（"delete"/
// "insert-messages"/"insert-sessions"）执行成功之后调用，返回错误时本事务以
// 该错误中止（验证任一步失败整轮回滚——不留半迁移）。生产恒为 nil。
var workBuddyStepHook func(step string) error

// workBuddyPersistPlan 是一次 WorkBuddy 采集批的事务合并计划。
type workBuddyPersistPlan struct {
	plans []collector.WorkBuddySessionPlan // 按 sessionID 升序
	byID  map[string]collector.WorkBuddySessionPlan
	// touchedSessions 是本轮有 JSONL 消息或 Session 元数据的会话（消息走现有
	// upsert 路径，家族历史行由本计划收敛）。
	touchedSessions map[string]bool
	// currentIDs 是本轮消息的逻辑 id → 归属会话（身份冲突检测必须覆盖本轮
	// 新消息：历史行与本轮消息同 id 不同会话时整轮失败，不吞并不改挂）。
	currentIDs map[string]string
}

// buildWorkBuddyPersistPlan 校验采集结果中的 WorkBuddy 计划并构造事务合并计划。
// 非 workbuddy client 或无计划会话时返回 nil（无操作）。计划内 session 必须唯一、
// 目标 client 必须是严格合法的 WorkBuddy 家族成员；本轮消息同 id 不同会话直接
// 拒绝（身份冲突）。
func buildWorkBuddyPersistPlan(client string, collected collector.CollectResult) (*workBuddyPersistPlan, error) {
	if client != "workbuddy" || len(collected.WorkBuddyPlans) == 0 {
		return nil, nil
	}
	plans := append([]collector.WorkBuddySessionPlan(nil), collected.WorkBuddyPlans...)
	sort.Slice(plans, func(i, j int) bool { return plans[i].SessionID < plans[j].SessionID })
	byID := make(map[string]collector.WorkBuddySessionPlan, len(plans))
	for _, p := range plans {
		if p.SessionID == "" {
			return nil, fmt.Errorf("计划包含空 session id")
		}
		if !model.IsWorkBuddyFamilyClient(p.Client) {
			return nil, fmt.Errorf("会话 %s 的目标 client 非法: %q", p.SessionID, p.Client)
		}
		if _, dup := byID[p.SessionID]; dup {
			return nil, fmt.Errorf("计划包含重复 session id: %s", p.SessionID)
		}
		byID[p.SessionID] = p
	}
	touched := make(map[string]bool)
	currentIDs := make(map[string]string, len(collected.Messages))
	for _, m := range collected.Messages {
		touched[m.SessionID] = true
		if prev, dup := currentIDs[m.ID]; dup {
			if prev != m.SessionID {
				return nil, fmt.Errorf("本轮消息 %s 同批出现于会话 %s 与 %s，身份冲突", m.ID, prev, m.SessionID)
			}
			continue
		}
		currentIDs[m.ID] = m.SessionID
	}
	for _, s := range collected.Sessions {
		touched[s.ID] = true
	}
	return &workBuddyPersistPlan{plans: plans, byID: byID, touchedSessions: touched, currentIDs: currentIDs}, nil
}

// workBuddySessionMerge 是单个会话的合并结果。routerConflicts 收集存在多个
// 不同非空 router 组合的逻辑消息 id（诊断用，不构成失败）。
type workBuddySessionMerge struct {
	plan            collector.WorkBuddySessionPlan
	messages        []model.Message
	session         model.Session
	hasSessionRow   bool // 家族内已有任一 sessions 行
	hasMessages     bool // 家族内已有任一 messages 行
	routerConflicts []string
}

// apply 在 persistClientBatch 事务内执行家族合并（先于本轮消息 upsert）。
func (p *workBuddyPersistPlan) apply(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	if p == nil {
		return nil
	}
	for start := 0; start < len(p.plans); start += workBuddyMergeBatchSize {
		end := start + workBuddyMergeBatchSize
		if end > len(p.plans) {
			end = len(p.plans)
		}
		if err := p.applyBatch(ctx, tx, log, p.plans[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *workBuddyPersistPlan) applyBatch(ctx context.Context, tx *sql.Tx, log *slog.Logger, batch []collector.WorkBuddySessionPlan) error {
	sessionIDs := make([]string, len(batch))
	for i, pl := range batch {
		sessionIDs[i] = pl.SessionID
	}

	familyMsgs, err := db.WorkBuddyFamilyMessages(ctx, tx, sessionIDs)
	if err != nil {
		return err
	}
	familySess, err := db.WorkBuddyFamilySessions(ctx, tx, sessionIDs)
	if err != nil {
		return err
	}

	msgsBySession := make(map[string][]model.Message)
	for _, m := range familyMsgs {
		msgsBySession[m.SessionID] = append(msgsBySession[m.SessionID], m)
	}
	sessBySession := make(map[string][]model.Session)
	for _, s := range familySess {
		sessBySession[s.ID] = append(sessBySession[s.ID], s)
	}

	// 同 ID 身份冲突检测：家族历史行与本轮新消息合并视角下，任一消息 id 的
	// 归属必须唯一。
	if err := detectWorkBuddyIdentityConflicts(ctx, tx, msgsBySession, p.currentIDs); err != nil {
		return err
	}

	var changedIDs []string
	var changedMsgs []model.Message
	var changedSess []model.Session
	var routerConflicts []string
	for _, plan := range batch {
		rows := msgsBySession[plan.SessionID]
		sessRows := sessBySession[plan.SessionID]
		if len(rows) == 0 && len(sessRows) == 0 && !p.touchedSessions[plan.SessionID] {
			// 完全无数据的源库行：不创建空会话，忽略。
			continue
		}
		merge := mergeWorkBuddySession(plan, rows, sessRows)
		routerConflicts = append(routerConflicts, merge.routerConflicts...)
		if !workBuddyMergeChanged(plan, merge, rows, sessRows) {
			continue
		}
		changedIDs = append(changedIDs, plan.SessionID)
		changedMsgs = append(changedMsgs, merge.messages...)
		changedSess = append(changedSess, merge.session)
	}

	if len(routerConflicts) > 0 && log != nil {
		log.Warn("workbuddy family merge found conflicting non-empty router values",
			"messages", len(routerConflicts), "first", routerConflicts[0])
	}
	if len(changedIDs) == 0 {
		return nil
	}
	if log != nil {
		log.Debug("workbuddy family rekey", "sessions", len(changedIDs), "messages", len(changedMsgs))
	}
	// 先删除家族旧行，再写回合并结果；本轮消息随后经现有 upsert 覆盖
	// token/model（本轮优先），删除与写回都在本事务内。
	if err := db.DeleteWorkBuddyFamilyRows(ctx, tx, changedIDs); err != nil {
		return err
	}
	if err := workBuddyStepHookChecked("delete"); err != nil {
		return err
	}
	if _, err := db.UpsertMessages(ctx, tx, changedMsgs); err != nil {
		return err
	}
	if err := workBuddyStepHookChecked("insert-messages"); err != nil {
		return err
	}
	if _, err := db.UpsertSessionMeta(ctx, tx, changedSess); err != nil {
		return err
	}
	if err := workBuddyStepHookChecked("insert-sessions"); err != nil {
		return err
	}
	return nil
}

// workBuddyStepHookChecked 执行测试注入钩子（生产恒 nil）。
func workBuddyStepHookChecked(step string) error {
	if workBuddyStepHook == nil {
		return nil
	}
	return workBuddyStepHook(step)
}

// detectWorkBuddyIdentityConflicts 检查家族历史行（msgsBySession）与本轮新消息
// （currentIDs，id → 归属会话）合并视角下的同 ID 归属一致性，并对照用量库中
// 该 id 的全部家族归属。任何不一致（含本轮两个新会话互相冲突、本轮消息与
// 历史行冲突）都返回错误：本轮迁移失败并回滚，不删除任一方，由后续数据调查
// 解决，不在实现中猜测。非家族 client 的同 id 行不读取、不修改、不判冲突。
func detectWorkBuddyIdentityConflicts(ctx context.Context, tx *sql.Tx, msgsBySession map[string][]model.Message, currentIDs map[string]string) error {
	idOwner := make(map[string]string) // message id -> 家族历史 + 本轮消息的合并归属
	mergeOwner := func(id, sessionID string) error {
		if prev, ok := idOwner[id]; ok && prev != sessionID {
			return fmt.Errorf("消息 %s 同批出现于会话 %s 与 %s，身份冲突", id, prev, sessionID)
		}
		idOwner[id] = sessionID
		return nil
	}
	for sessionID, rows := range msgsBySession {
		for _, m := range rows {
			if err := mergeOwner(m.ID, sessionID); err != nil {
				return err
			}
		}
	}
	for id, sessionID := range currentIDs {
		if err := mergeOwner(id, sessionID); err != nil {
			return err
		}
	}

	ids := make([]string, 0, len(idOwner))
	for id := range idOwner {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	const chunk = 100
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		owners, err := db.QueryWorkBuddyMessageOwners(ctx, tx, ids[start:end])
		if err != nil {
			return err
		}
		for _, owner := range owners {
			if owner.SessionID != idOwner[owner.MessageID] {
				return fmt.Errorf("消息 %s 已属于会话 %s（本批归属 %s），身份冲突，不吞并不改挂",
					owner.MessageID, owner.SessionID, idOwner[owner.MessageID])
			}
		}
	}
	return nil
}

// mergeWorkBuddySession 按确定性规则把会话的家族历史行合并到目标 client：
//
//   - 每条逻辑消息：归因四列（ts/date/session_id/directory）取较早 ts 副本，
//     同 ts 优先目标侧、再按 client 键字节序；token/model/provider 取较大 ts
//     副本整组取值，同 ts 同优先序（目标侧不存在时取 client 键最小副本）；
//     router_* 优先保留 token 选定副本的非空值，空值按同一优先顺序从其他
//     副本补齐，多个不同非空值只记诊断不失败；project 一律取本轮有效元数据
//     校正后的目标值（见下方目录兜底）。
//   - 会话行：first/last_ts 取全部有效历史消息、历史会话行与本轮消息的
//     最早/最晚；title 以当前有效非空标题优先，否则目标侧非空标题，再按
//     client 存储键字节序选择非空历史标题；directory 以计划非空值优先，为空
//     时保留目标侧已有 directory，仍为空时取家族其他行中 client 键最小者的
//     directory（仅历史刷新兜底，不凭文件夹转写反推路径）；非 playground 的
//     project 在有效 directory 确定后按 projectBase 重算——不以空目录把历史
//     分类清空，playground 恒为空串。
//
// 该政策是会话级分类，不声称还原每条消息发生时的 expert。
func mergeWorkBuddySession(plan collector.WorkBuddySessionPlan, msgRows []model.Message, sessRows []model.Session) workBuddySessionMerge {
	target := plan.Client
	sort.Slice(msgRows, func(i, j int) bool {
		if msgRows[i].ID != msgRows[j].ID {
			return msgRows[i].ID < msgRows[j].ID
		}
		if msgRows[i].TS != msgRows[j].TS {
			return msgRows[i].TS < msgRows[j].TS
		}
		ti, tj := msgRows[i].Client == target, msgRows[j].Client == target
		if ti != tj {
			return ti // 同 ts 目标侧优先
		}
		return msgRows[i].Client < msgRows[j].Client
	})

	// 家族会话行按 client 键字节序排序：目录/标题兜底与 ParentID 取值共用。
	sortedSess := append([]model.Session(nil), sessRows...)
	sort.Slice(sortedSess, func(i, j int) bool { return sortedSess[i].Client < sortedSess[j].Client })

	// 有效 directory：计划非空值优先。历史兜底只用于无 JSONL 的仅历史刷新
	//（HasJSONL=false）：有 JSONL 触达时目录来源只有源库 cwd 与文件首条非空
	// cwd，两者皆空则 directory/project 允许为空——不做历史兜底，保证合并
	// 目标与 collector 产出的 session 行同值，不被后续 UPSERT 冲突覆盖。
	directory := plan.Directory
	if directory == "" && !plan.HasJSONL {
		for _, s := range sortedSess {
			if s.Directory != "" {
				directory = s.Directory
				break
			}
		}
	}
	var targetParent string
	for _, s := range sortedSess {
		if s.Client == target {
			if targetParent == "" {
				targetParent = s.ParentID
			}
		}
	}
	// 有效 project：playground 恒空；非 playground 按有效 directory 重算
	//（与 collector 计划自洽；目录历史兜底时以兜底目录为准）。
	project := plan.Project
	if !plan.Playground {
		project = collector.ProjectBase(directory)
	}

	var merged []model.Message
	var routerConflicts []string
	for i := 0; i < len(msgRows); {
		j := i
		for j < len(msgRows) && msgRows[j].ID == msgRows[i].ID {
			j++
		}
		group := msgRows[i:j]
		// 归因副本：组内已按（id、ts 升序、目标优先、字节序）排序，首元素即
		// 较早 ts 副本（同 ts 目标侧优先、字节序最小）。
		attr := group[0]
		// token 副本：最大 ts；同 ts 目标侧优先、否则 client 键字节序最小。
		// 从尾段向前持续覆盖：遇目标侧即停（段内目标侧唯一），扫到段首则
		// 取 client 键最小的副本。
		maxTS := group[len(group)-1].TS
		token := group[len(group)-1]
		for k := len(group) - 1; k >= 0 && group[k].TS == maxTS; k-- {
			token = group[k]
			if group[k].Client == target {
				break
			}
		}
		if countDistinctRouterValues(group) > 1 {
			routerConflicts = append(routerConflicts, attr.ID)
		}
		merged = append(merged, model.Message{
			ID:                attr.ID,
			SessionID:         plan.SessionID,
			Client:            target,
			Date:              attr.Date,
			TS:                attr.TS,
			Model:             token.Model,
			Provider:          token.Provider,
			RouterProvider:    pickWorkBuddyRouterValue(group, token, target, func(m model.Message) string { return m.RouterProvider }),
			RouterModel:       pickWorkBuddyRouterValue(group, token, target, func(m model.Message) string { return m.RouterModel }),
			RouterName:        pickWorkBuddyRouterValue(group, token, target, func(m model.Message) string { return m.RouterName }),
			Directory:         attr.Directory,
			Project:           project,
			InputTokens:       token.InputTokens,
			FreshInputTokens:  token.FreshInputTokens,
			OutputTokens:      token.OutputTokens,
			CacheReadTokens:   token.CacheReadTokens,
			CacheCreateTokens: token.CacheCreateTokens,
			ReasoningTokens:   token.ReasoningTokens,
			TotalTokens:       token.TotalTokens,
		})
		i = j
	}

	// 会话行合并：时间区间覆盖全部有效历史消息、历史会话行与本轮消息。
	firstTS, lastTS := plan.FirstTS, plan.LastTS
	expand := func(ts int64) {
		if ts > 0 {
			if firstTS == 0 || ts < firstTS {
				firstTS = ts
			}
			if ts > lastTS {
				lastTS = ts
			}
		}
	}
	for _, m := range msgRows {
		expand(m.TS)
	}
	for _, s := range sessRows {
		expand(s.FirstTS)
		expand(s.LastTS)
	}
	// title：计划非空优先；否则目标侧非空；否则按 client 键字节序最小的
	// 非空历史标题。
	title := plan.Title
	if title == "" {
		for _, s := range sortedSess {
			if s.Client == target && s.Title != "" {
				title = s.Title
				break
			}
		}
	}
	if title == "" {
		for _, s := range sortedSess {
			if s.Title != "" {
				title = s.Title
				break
			}
		}
	}
	return workBuddySessionMerge{
		plan:     plan,
		messages: merged,
		session: model.Session{
			ID:        plan.SessionID,
			Client:    target,
			Directory: directory,
			Project:   project,
			Title:     title,
			ParentID:  targetParent,
			FirstTS:   firstTS,
			LastTS:    lastTS,
		},
		hasSessionRow:   len(sessRows) > 0,
		hasMessages:     len(msgRows) > 0,
		routerConflicts: routerConflicts,
	}
}

// pickWorkBuddyRouterValue 取 router 字段的合并值：token 选定副本的值非空即用；
// 否则按（ts 降序、目标侧优先、client 键字节序升序）从其他副本取首个非空值。
func pickWorkBuddyRouterValue(group []model.Message, tokenCopy model.Message, targetClient string, field func(model.Message) string) string {
	if v := field(tokenCopy); v != "" {
		return v
	}
	ordered := append([]model.Message(nil), group...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TS != ordered[j].TS {
			return ordered[i].TS > ordered[j].TS
		}
		ti, tj := ordered[i].Client == targetClient, ordered[j].Client == targetClient
		if ti != tj {
			return ti
		}
		return ordered[i].Client < ordered[j].Client
	})
	for _, m := range ordered {
		if v := field(m); v != "" {
			return v
		}
	}
	return ""
}

// countDistinctRouterValues 统计组内三个 router 字段的 distinct 非空组合数，
// 大于 1 表示存在不同非空值冲突（调用方记录诊断，不构成失败）。
func countDistinctRouterValues(group []model.Message) int {
	seen := make(map[string]struct{})
	for _, m := range group {
		if m.RouterProvider == "" && m.RouterModel == "" && m.RouterName == "" {
			continue
		}
		key := m.RouterProvider + "\x00" + m.RouterModel + "\x00" + m.RouterName
		seen[key] = struct{}{}
	}
	return len(seen)
}

// workBuddyMergeChanged 判断合并结果是否与现状存在差异（目标侧行集与源侧
// 残留）。无差异的会话跳过删除/重插，保证周期复核幂等且不产生额外写入。
// 家族或合并存在消息数据而目标侧缺会话行时视为有差异（补会话修复关联）。
func workBuddyMergeChanged(plan collector.WorkBuddySessionPlan, merge workBuddySessionMerge, msgRows []model.Message, sessRows []model.Session) bool {
	// 家族内存在任何非目标侧副本 → 需要收敛。
	for _, m := range msgRows {
		if m.Client != plan.Client {
			return true
		}
	}
	for _, s := range sessRows {
		if s.Client != plan.Client {
			return true
		}
	}
	// 目标侧消息行必须与合并结果逐列一致。
	targetMsgs := make([]model.Message, 0, len(msgRows))
	for _, m := range msgRows {
		if m.Client == plan.Client {
			targetMsgs = append(targetMsgs, m)
		}
	}
	if len(targetMsgs) != len(merge.messages) {
		return true
	}
	for i := range targetMsgs {
		if !workBuddyMessageEqual(targetMsgs[i], merge.messages[i]) {
			return true
		}
	}
	// 会话行：家族有会话行、家族或合并有消息数据时，目标侧必须存在且逐列
	// 一致（只有 messages 没有 sessions 的历史数据在此补齐关联）。
	var targetSess *model.Session
	for i := range sessRows {
		if sessRows[i].Client == plan.Client {
			s := sessRows[i]
			targetSess = &s
			break
		}
	}
	if merge.hasSessionRow || merge.hasMessages || len(merge.messages) > 0 || targetSess != nil {
		if targetSess == nil || !workBuddySessionEqual(*targetSess, merge.session) {
			return true
		}
	}
	return false
}

func workBuddyMessageEqual(a, b model.Message) bool {
	return a.ID == b.ID && a.SessionID == b.SessionID && a.Client == b.Client &&
		a.Date == b.Date && a.TS == b.TS && a.Model == b.Model && a.Provider == b.Provider &&
		a.RouterProvider == b.RouterProvider && a.RouterModel == b.RouterModel && a.RouterName == b.RouterName &&
		a.Directory == b.Directory && a.Project == b.Project &&
		a.InputTokens == b.InputTokens && a.FreshInputTokens == b.FreshInputTokens &&
		a.OutputTokens == b.OutputTokens && a.CacheReadTokens == b.CacheReadTokens &&
		a.CacheCreateTokens == b.CacheCreateTokens && a.ReasoningTokens == b.ReasoningTokens &&
		a.TotalTokens == b.TotalTokens
}

func workBuddySessionEqual(a, b model.Session) bool {
	return a.ID == b.ID && a.Client == b.Client && a.Directory == b.Directory &&
		a.Project == b.Project && a.Title == b.Title && a.ParentID == b.ParentID &&
		a.FirstTS == b.FirstTS && a.LastTS == b.LastTS
}

// verify 在提交前校验：计划会话的家族内不存在目标 client 之外的残留行
// （messages 与 sessions）、消息与会话行的 project 已收敛到计划语义（playground
// 恒空、非 playground 为 projectBase(会话 directory)）、且存在目标消息行的
// 会话必有目标 client 的会话行（关联完整）。
func (p *workBuddyPersistPlan) verify(ctx context.Context, tx *sql.Tx) error {
	if p == nil {
		return nil
	}
	for start := 0; start < len(p.plans); start += workBuddyMergeBatchSize {
		end := start + workBuddyMergeBatchSize
		if end > len(p.plans) {
			end = len(p.plans)
		}
		batch := p.plans[start:end]
		sessionIDs := make([]string, len(batch))
		for i, pl := range batch {
			sessionIDs[i] = pl.SessionID
		}
		msgs, err := db.WorkBuddyFamilyMessages(ctx, tx, sessionIDs)
		if err != nil {
			return err
		}
		sess, err := db.WorkBuddyFamilySessions(ctx, tx, sessionIDs)
		if err != nil {
			return err
		}
		targetSessDirs := make(map[string]string, len(batch))
		hasTargetSess := make(map[string]bool, len(batch))
		for _, s := range sess {
			plan := p.byID[s.ID]
			if s.Client != plan.Client {
				return fmt.Errorf("会话 %s 存在家族残留会话行 client=%q（目标 %q）", s.ID, s.Client, plan.Client)
			}
			hasTargetSess[s.ID] = true
			targetSessDirs[s.ID] = s.Directory
		}
		hasTargetMsgs := make(map[string]bool)
		for _, m := range msgs {
			plan := p.byID[m.SessionID]
			if m.Client != plan.Client {
				return fmt.Errorf("会话 %s 存在家族残留消息行 client=%q（目标 %q）", m.SessionID, m.Client, plan.Client)
			}
			if m.Client == plan.Client {
				hasTargetMsgs[m.SessionID] = true
			}
			// project 收敛断言：playground 恒空；非 playground 为
			// projectBase(目标会话行 directory)——directory 已先经合并收敛。
			wantProject := plan.Project
			if !plan.Playground {
				wantProject = collector.ProjectBase(targetSessDirs[m.SessionID])
			}
			if m.Project != wantProject {
				return fmt.Errorf("会话 %s 消息 %s project=%q 未收敛到目标 %q", m.SessionID, m.ID, m.Project, wantProject)
			}
		}
		// 关联完整性：有目标消息行的会话必须有目标会话行。
		for id := range hasTargetMsgs {
			if !hasTargetSess[id] {
				return fmt.Errorf("会话 %s 存在目标消息行但缺少目标会话行，关联不完整", id)
			}
		}
	}
	return nil
}

// deferWorkBuddyExcludedHistory 处理完整无日期复核中被排除的会话（源库元
// 数据存在但无效、未产出计划）：若任一被排除会话在用量库已有家族行
// （messages 或 sessions），该会话本轮暂缓——保留原数据不动，并把暂缓错误
// 追加进 PartialErr。这接入现有部分失败编排：其余有效会话（计划不受影响）
// 仍在 persistClientBatch 内正常落库，请求返回失败、不写完成标记、不清除
// 历史错误，下一轮在源库修复后自然收敛。无历史行的被排除会话无数据可暂缓，
// 跳过。返回错误仅表示用量库查询失败（属写事务级故障，调用方整轮失败）。
func deferWorkBuddyExcludedHistory(ctx context.Context, usageDB *db.DB, collected *collector.CollectResult) error {
	if len(collected.WorkBuddyExcluded) == 0 {
		return nil
	}
	ids, err := db.WorkBuddyFamilySessionIDsWithRows(ctx, usageDB)
	if err != nil {
		return err
	}
	withRows := make(map[string]bool, len(ids))
	for _, id := range ids {
		withRows[id] = true
	}
	excludedWithRows := make(map[string]bool, len(collected.WorkBuddyExcluded))
	var deferredErr error
	for _, ex := range collected.WorkBuddyExcluded {
		if withRows[ex.SessionID] {
			excludedWithRows[ex.SessionID] = true
			deferredErr = errors.Join(deferredErr,
				fmt.Errorf("%s会话 %s 已有用量历史但源库元数据无效（%v），本轮暂缓",
					collector.WorkBuddyMetadataErrPrefix, ex.SessionID, ex.Reason))
		}
	}
	if deferredErr == nil {
		return nil
	}
	// 防御性剔除计划中的同名会话（collector 不应把无效会话放入计划）。
	kept := collected.WorkBuddyPlans[:0]
	for _, plan := range collected.WorkBuddyPlans {
		if !excludedWithRows[plan.SessionID] {
			kept = append(kept, plan)
		}
	}
	collected.WorkBuddyPlans = kept
	collected.PartialErr = errors.Join(collected.PartialErr, deferredErr)
	return nil
}

// isFullWorkBuddyReview 报告请求是否构成一次完整的 WorkBuddy 无日期全量复核
// （CLI collect all、daemon 启动 catch-up 与周期复核请求；Incremental/
// ChangedFile/Dates 请求均不是）。仅此类请求成功后按固定片段解决历史
// WorkBuddy 元数据/重归属错误，不受登记日期限制。
func isFullWorkBuddyReview(req collector.CollectRequest) bool {
	return len(req.Dates) == 0 && !req.Incremental && req.ChangedFile == ""
}
