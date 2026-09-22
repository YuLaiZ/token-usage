package engine

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// mimocode Desktop 拆分 reconciliation（schema v5 pending 驱动的自动无损重归属）。
//
// 触发边界：RunCollect 仅在显式 mimocode 采集或全客户端（client=="mimocode"
// 或 ""）时检查 pending——其他客户端的采集与 watcher 事件绝不被 mimocode 的
// reconciliation 故障拖累；router-only 路径不触发。daemon catch-up 逐 client
// 调 RunCollect，故每个 startup 只在 mimocode 的那次触发一次。
//
// pending 协议（generation，见 db/reconcile.go）：旧名写入使 generation+1 并
// 清空游标；每批推进按 generation CAS，最终清除按 generation compare-and-
// delete——期间被重新置位时当前轮停止并按新 generation 从空游标重跑，清除
// 未命中不得宣布完成。失败保留 pending 并记录 collection_errors（error_type
// 沿用现有 errors 模型的 'error'，以 message 前缀区分），下次采集自动重试。

// mimoReconcileBatchSize 是每个 re-key 批次的会话数上限。批内七步在同一
// 事务（见 runMimoReconcileBatch）；跨批以 generation CAS 游标推进，可中断
// 续跑、可被新 generation 重置重跑。
const mimoReconcileBatchSize = 100

// mimoPersistRekeyPlan 是普通 mimocode 采集写事务内的双向归属计划。pending
// reconciliation 负责 schema 升级、显式全量复核与源消息已删除的历史会话；
// 本计划负责 pending 已清除后 session.version 发生变化的日常路径：在写入当前
// 消息前先把同 session 的另一 client 历史行合并到目标侧，写入当前值后再删除
// 源侧，整个过程与本轮采集同事务，避免 (client,id) 不同导致双侧行和 token 双计。
type mimoPersistRekeyPlan struct {
	codeIDs    []string
	desktopIDs []string
}

func buildMimoPersistRekeyPlan(client string, collected collector.CollectResult) (*mimoPersistRekeyPlan, error) {
	if client != "mimocode" {
		return nil, nil
	}
	targets := make(map[string]string, len(collected.Sessions)+len(collected.Messages))
	add := func(sessionID, target string) error {
		if sessionID == "" {
			return fmt.Errorf("mimocode 采集结果缺少 session id")
		}
		if target != model.ClientMiMoCode && target != model.ClientMiMoDesktop {
			return fmt.Errorf("mimocode 会话 %s 的 client 非法: %q", sessionID, target)
		}
		if old, ok := targets[sessionID]; ok && old != target {
			return fmt.Errorf("mimocode 会话 %s 同批出现冲突 client: %q / %q", sessionID, old, target)
		}
		targets[sessionID] = target
		return nil
	}
	for _, s := range collected.Sessions {
		if err := add(s.ID, s.Client); err != nil {
			return nil, err
		}
	}
	for _, m := range collected.Messages {
		if err := add(m.SessionID, m.Client); err != nil {
			return nil, err
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	plan := &mimoPersistRekeyPlan{}
	for id, target := range targets {
		if target == model.ClientMiMoDesktop {
			plan.desktopIDs = append(plan.desktopIDs, id)
		} else {
			plan.codeIDs = append(plan.codeIDs, id)
		}
	}
	sort.Strings(plan.codeIDs)
	sort.Strings(plan.desktopIDs)
	return plan, nil
}

func (p *mimoPersistRekeyPlan) forEachGroup(fn func(ids []string, target, source string) error) error {
	if p == nil {
		return nil
	}
	groups := []struct {
		ids            []string
		target, source string
	}{
		{p.codeIDs, model.ClientMiMoCode, model.ClientMiMoDesktop},
		{p.desktopIDs, model.ClientMiMoDesktop, model.ClientMiMoCode},
	}
	for _, group := range groups {
		for start := 0; start < len(group.ids); start += mimoReconcileBatchSize {
			end := start + mimoReconcileBatchSize
			if end > len(group.ids) {
				end = len(group.ids)
			}
			if err := fn(group.ids[start:end], group.target, group.source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *mimoPersistRekeyPlan) begin(ctx context.Context, tx *sql.Tx) error {
	if p == nil {
		return nil
	}
	if err := db.MimoReconcileBypassOn(ctx, tx); err != nil {
		return err
	}
	return p.forEachGroup(func(ids []string, target, source string) error {
		return db.MimoRekeySessions(ctx, tx, ids, target, source)
	})
}

func (p *mimoPersistRekeyPlan) finish(ctx context.Context, tx *sql.Tx) error {
	if p == nil {
		return nil
	}
	if err := p.forEachGroup(func(ids []string, _, source string) error {
		return db.DeleteMimoSourceRows(ctx, tx, ids, source)
	}); err != nil {
		return err
	}
	return db.MimoReconcileBypassOff(ctx, tx)
}

// mimoAssignmentSource 由 mimocode collector 实现：返回源库全部会话的
// id+version 归属清单（含无消息会话，供历史行 re-key 判定）。
type mimoAssignmentSource interface {
	Assignments(ctx context.Context) ([]collector.MimoSessionAssignment, error)
}

// shouldRearmMimoFullReconcile 判断本次是否为显式全量归属复核。collect all 的
// 合同是 Dates=nil、非 Incremental、非 ChangedFile/ScanExistingJSONL；对单独
// mimocode 或全客户端入口都先 re-arm pending，使已经完成过 v5 reconciliation
// 的数据库也重新读取全部 assignments。普通日期采集、daemon 增量与 watcher
// 事件不走本分支，由写事务内的 mimoPersistRekeyPlan 处理实际触达的会话。
func shouldRearmMimoFullReconcile(deps *Deps, client string, req collector.CollectRequest) bool {
	if client != "" && client != "mimocode" {
		return false
	}
	if req.Dates != nil || req.Incremental || req.ChangedFile != "" || req.ScanExistingJSONL {
		return false
	}
	cc, ok := deps.cfg.ClientConfig("mimocode")
	if !ok || !cc.Enabled {
		return false
	}
	for _, c := range deps.collectors {
		if c.Name() == "mimocode" {
			_, ok := c.(mimoAssignmentSource)
			return ok
		}
	}
	return false
}

// runMimoReconciliationIfPending：pending 存在时执行（或续跑）全量
// reconciliation。mimocode 未启用（配置禁用或 collector 不可用）时 pending
// 悬置，静默返回 nil。返回错误仅表示本轮 reconciliation 失败（pending 已
// 保留，常规采集可继续）。
func runMimoReconciliationIfPending(ctx context.Context, deps *Deps, usageDB *db.DB, log *slog.Logger, out io.Writer) error {
	pending, _, _, err := db.MimoReconcilePending(ctx, usageDB)
	if err != nil || !pending {
		return err
	}
	// mimocode 未启用时无法读源库：pending 悬置，静默跳过（启用后首次采集
	// 自动补跑）。
	if cc, ok := deps.cfg.ClientConfig("mimocode"); !ok || !cc.Enabled {
		log.Debug("mimocode split reconcile pending but client disabled", "client", "mimocode")
		return nil
	}
	var mc mimoAssignmentSource
	for _, c := range deps.collectors {
		if c.Name() != "mimocode" {
			continue
		}
		if src, ok := c.(mimoAssignmentSource); ok {
			mc = src
		}
	}
	if mc == nil {
		log.Debug("mimocode split reconcile pending but collector unavailable", "client", "mimocode")
		return nil
	}
	_, err = runMimoReconciliation(ctx, mc, usageDB, log, out)
	return err
}

// runMimoReconciliation 执行（或从游标续跑）全量无损重归属，generation
// 外层循环：任一轮内 generation 被旧名写入改变（批 CAS 失败或最终清除未
// 命中）时，按新 generation 的空游标重跑；只有 CAS 清除命中才输出完成。
func runMimoReconciliation(ctx context.Context, mc mimoAssignmentSource, usageDB *db.DB, log *slog.Logger, out io.Writer) (int, error) {
	for round := 0; ; round++ {
		pending, generation, cursorID, err := db.MimoReconcilePending(ctx, usageDB)
		if err != nil {
			return 0, err
		}
		if !pending {
			return 0, nil
		}
		processed, desktopCount, genChanged, err := runMimoReconcileRound(ctx, mc, usageDB, log, generation, cursorID)
		if err != nil {
			return processed, err
		}
		if genChanged {
			log.Info("mimocode split reconcile generation changed, restarting from empty cursor",
				"round", round, "processed_before_restart", processed)
			continue
		}
		// 全部批次成功 → 按 generation compare-and-delete 清除 pending；
		// 未命中（最后批提交后又被旧名写入重新置位）不得宣布完成，按新
		// generation 重跑。
		cleared, err := db.MimoReconcileClearPendingCAS(ctx, usageDB, generation)
		if err != nil {
			return processed, err
		}
		if !cleared {
			log.Info("mimocode split reconcile pending re-armed during completion, restarting",
				"round", round)
			continue
		}
		log.Info("mimocode split reconcile completed", "sessions", processed, "desktop", desktopCount)
		if out != nil {
			fmt.Fprintf(out, "%s: %d sessions (desktop %d)\n",
				"mimocode split reconcile", processed, desktopCount)
		}
		return processed, nil
	}
}

// runMimoReconcileRound 跑完当前 generation 的全部批次：
//  1. 读取源库全量会话归属清单（不依赖会话当前是否有可采消息）；
//  2. 全量采集当前值（非增量）；
//  3. 从游标之后按 id 字典序分批执行 runMimoReconcileBatch；任一批因
//     generation 变化回滚时返回 genChanged=true（非故障）。
//
// 源库已消失、不在归属清单中的历史会话：不处理、不删除，保持现有 client
// 身份（无判别依据时不猜）。当前源值与历史值不同时，本批 upsert 在合并之后
// 执行，按 DAO upsert 语义胜出或补全。
func runMimoReconcileRound(ctx context.Context, mc mimoAssignmentSource, usageDB *db.DB, log *slog.Logger, generation int64, cursorID string) (processed, desktopCount int, genChanged bool, err error) {
	assignments, err := mc.Assignments(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	coll, ok := mc.(collector.Collector)
	if !ok {
		return 0, 0, false, fmt.Errorf("mimocode collector 不满足采集接口")
	}
	collected, err := coll.Collect(ctx, collector.CollectRequest{}, log)
	if err != nil {
		return 0, 0, false, err
	}
	msgsBySession := make(map[string][]model.Message, len(collected.Messages))
	for _, m := range collected.Messages {
		msgsBySession[m.SessionID] = append(msgsBySession[m.SessionID], m)
	}
	sessMetaByID := make(map[string]model.Session, len(collected.Sessions))
	for _, s := range collected.Sessions {
		sessMetaByID[s.ID] = s
	}

	rest := assignments[:0:0]
	for _, a := range assignments {
		if a.ID > cursorID {
			rest = append(rest, a)
		}
	}
	for start := 0; start < len(rest); start += mimoReconcileBatchSize {
		end := start + mimoReconcileBatchSize
		if end > len(rest) {
			end = len(rest)
		}
		batch := rest[start:end]
		var batchMsgs []model.Message
		var batchSessions []model.Session
		for _, a := range batch {
			target := model.MiMoSessionClient(a.Version)
			for _, m := range msgsBySession[a.ID] {
				// Assignments 是本轮 reconciliation 的归属快照；Collect 与其为
				// 两次源库读取，version 若恰在中间变化，不能让同一批出现相反
				// client。统一按 assignment target 落库，下一次普通采集再按更新
				// 后的 version 通过事务内 re-key 收敛。
				m.Client = target
				batchMsgs = append(batchMsgs, m)
			}
			if s, ok := sessMetaByID[a.ID]; ok {
				s.Client = target
				batchSessions = append(batchSessions, s)
			}
			if target == model.ClientMiMoDesktop {
				desktopCount++
			}
		}
		genChanged, err := runMimoReconcileBatch(ctx, usageDB, batch, batchMsgs, batchSessions, generation)
		if err != nil {
			return processed, desktopCount, false, err
		}
		if genChanged {
			return processed, desktopCount, true, nil
		}
		processed += len(batch)
	}
	return processed, desktopCount, false, nil
}

// mimoReconcileStepHook 仅供测试注入：在 runMimoReconcileBatch 的每个步骤
// （"bypass"/"rekey"/"messages"/"sessions"/"delete"/"cursor"）执行成功之后
// 调用，返回错误时本批事务以该错误中止（验证任一步失败整批回滚——不留
// 双侧并存、游标不越位、bypass 标志随回滚消失、pending 不清除）。
// 生产恒为 nil。
var mimoReconcileStepHook func(step string) error

// runMimoReconcileBatch 是单个 reconciliation 批次的事务边界（七步合同）：
// ① 写入 split bypass 标志（使受控 re-key 的 'MiMo Code' 写入不被 split
// trigger 改写吞掉——target=Code 方向执行时源侧 Desktop 行仍在库）；
// ② 按目标归属把另一侧历史行无损合并到目标 client（MimoRekeySessions，
// DAO upsert 冲突语义，不裸 UPDATE）；③ upsert 本批当前 messages；
// ④ upsert 本批 sessions 元数据；⑤ 删除已合并的另一侧行
// （DeleteMimoSourceRows）并删除 bypass 标志；⑥ 按 generation CAS 推进游标
// （未命中=期间旧名写入改变了 generation，整批回滚返回 genChanged）；
// ⑦ 提交。任一步失败整批回滚——不留双侧并存、不提前删旧、游标不越位、
// bypass 标志不残留、不输出成功统计。重入=游标之后重跑，批内先合并后删
// 保证幂等。
func runMimoReconcileBatch(ctx context.Context, usageDB *db.DB, batch []collector.MimoSessionAssignment, msgs []model.Message, sessions []model.Session, generation int64) (genChanged bool, err error) {
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启重归属事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	step := func(name string) error {
		if mimoReconcileStepHook != nil {
			if err := mimoReconcileStepHook(name); err != nil {
				return err
			}
		}
		return nil
	}

	if err := db.MimoReconcileBypassOn(ctx, tx); err != nil {
		return false, err
	}
	if err := step("bypass"); err != nil {
		return false, err
	}

	// 按目标方向分组：target 与另一侧（source）互为 mimocode 两 client。
	byTarget := map[string][]string{}
	for _, a := range batch {
		byTarget[model.MiMoSessionClient(a.Version)] = append(byTarget[model.MiMoSessionClient(a.Version)], a.ID)
	}
	for target, ids := range byTarget {
		source := model.ClientMiMoCode
		if target == model.ClientMiMoCode {
			source = model.ClientMiMoDesktop
		}
		if err := db.MimoRekeySessions(ctx, tx, ids, target, source); err != nil {
			return false, err
		}
	}
	if err := step("rekey"); err != nil {
		return false, err
	}
	if _, err := db.UpsertMessages(ctx, tx, msgs); err != nil {
		return false, err
	}
	if err := step("messages"); err != nil {
		return false, err
	}
	if _, err := db.UpsertSessionMeta(ctx, tx, sessions); err != nil {
		return false, err
	}
	if err := step("sessions"); err != nil {
		return false, err
	}
	for target, ids := range byTarget {
		source := model.ClientMiMoCode
		if target == model.ClientMiMoCode {
			source = model.ClientMiMoDesktop
		}
		if err := db.DeleteMimoSourceRows(ctx, tx, ids, source); err != nil {
			return false, err
		}
	}
	if err := db.MimoReconcileBypassOff(ctx, tx); err != nil {
		return false, err
	}
	if err := step("delete"); err != nil {
		return false, err
	}
	ok, err := db.MimoReconcileAdvanceCursor(ctx, tx, generation, batch[len(batch)-1].ID)
	if err != nil {
		return false, err
	}
	if !ok {
		// generation 已被旧名写入改变：整批回滚，按新 generation 从空游标
		// 重跑（非故障）。
		return true, nil
	}
	if err := step("cursor"); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// recordMimoReconcileFailure 把 reconciliation 失败记入 collection_errors
// （按当天日期、source=mimocode、error_type 沿用现有 errors 模型的 'error'，
// 以 message 前缀区分），使既有 retry 机制与 errors 视图可观察、可恢复；
// pending 行保留由事务语义保证（失败批次已回滚，游标未越位）。
func recordMimoReconcileFailure(ctx context.Context, usageDB *db.DB, log *slog.Logger, cause error) {
	today := time.Now().Format("2006-01-02")
	if err := db.RecordErrorsByDate(ctx, usageDB, []string{today}, "mimocode",
		"mimocode split reconcile failed: "+cause.Error(), ""); err != nil {
		log.Error("record mimocode split reconcile failure", "error", err)
	}
}
