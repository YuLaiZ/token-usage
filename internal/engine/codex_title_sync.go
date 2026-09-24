package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// runCodexTitleSync 读 Codex 标题索引（session_index.jsonl）与 state DB threads
// 元数据，对 usageDB.sessions 的既有 Codex 行做两步同步（同一事务）：
//
//  1. 索引命中：经 db.ApplyCodexIndexTitles 写入（UPDATE-only，写入门是单一的
//     时间门：记录时间严格晚于已应用时间即写入——含同名时间推进，索引截断/
//     重建回旧快照不倒退）；
//  2. 存量空标题子线程兜底：库内仍空标题且 state threads.thread_source 属子线程
//     （subagent / agent_created_thread）且 state 标题也为空的行，经
//     db.ApplyCodexFallbackTitles 写入稳定兜底。该步闭合「增量游标不会重触达
//     旧线程、rollout 全扫可能被跳过门略过」的存量缺口；thread_source 为空的
//     空标题行不判子线程，保持空（数据边界）。
//
// 这是「已有会话与改名」的独立同步步骤：不读 state 增量游标、不受 rollout
// 跳过门阻挡，覆盖「App 改名只写索引、state DB 与 rollout 均不变」的场景。
// 索引文件不存在时不做索引写入（无索引属正常形态，不清空、不降级）；state DB
// 不可达时跳过兜底步（Warn），其余步骤不受影响。同步自身无观察游标：每轮全量
// 读索引 + 条件 UPDATE 幂等收敛。失败后的重试不依赖文件再次变化——daemon 的
// 索引轮询器按固定周期提交本同步（analyzer 的 alwaysSubmit 模式），上一周期
// 失败在下一周期自然重试；codex 的常规采集轮末也会追加本同步。
//
// 触发点（全部经 RunCollect，与常规采集共用 analyzer 的串行化锁，不存在
// 索引更新后并发写入把标题改回 prompt 的竞态窗口）：
//   - 常规采集轮末：RunCollect 对 codex 的每次尝试后追加（CLI 手动采集、
//     daemon catch-up 与全部运行期触发自动覆盖）；
//   - SyncTitles 纯请求：daemon 的索引周期轮询器（RunCollect 前置分派）。
func runCodexTitleSync(ctx context.Context, deps *Deps, usageDB *db.DB, log *slog.Logger) error {
	if deps == nil || deps.cfg == nil {
		return nil
	}
	clientCfg, ok := deps.cfg.ClientConfig("codex")
	if !ok || !clientCfg.Enabled {
		return nil
	}
	stateDir := clientCfg.Paths["state_dir"]
	if stateDir == "" {
		return nil
	}
	titles, err := collector.ReadCodexTitleIndex(ctx, stateDir, log)
	if err != nil {
		return err
	}
	fallbacks, err := codexChildThreadFallbacks(ctx, usageDB, stateDir, log)
	if err != nil {
		return err
	}
	if len(titles) == 0 && len(fallbacks) == 0 {
		return nil
	}
	tx, err := usageDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("open write transaction", "开启写事务"), err)
	}
	defer func() { _ = tx.Rollback() }()
	var indexUpdated, fallbackUpdated int64
	if len(titles) > 0 {
		if indexUpdated, err = db.ApplyCodexIndexTitles(ctx, tx, titles); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("sync session titles", "同步会话标题"), err)
		}
	}
	if len(fallbacks) > 0 {
		if fallbackUpdated, err = db.ApplyCodexFallbackTitles(ctx, tx, fallbacks); err != nil {
			return fmt.Errorf("%s: %w", ui.Bi("backfill child-thread titles", "回填子线程标题"), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("commit write transaction", "提交写事务"), err)
	}
	// 同步命中才记 Debug 排查轨迹（与采集完成心跳同级），0 条不打。
	if indexUpdated > 0 || fallbackUpdated > 0 {
		log.Debug("codex title index synced", "client", "codex",
			"index_updated", indexUpdated, "fallback_updated", fallbackUpdated)
	}
	return nil
}

// codexChildThreadFallbacks 汇总需要兜底回填的 (id → 兜底标题)：库内空标题的
// Codex 行 ∩ state threads 的子线程判定（thread_source ∈ {subagent,
// agent_created_thread}）∩ state 标题为空（state 有标题的空库行留给采集路径
// 回填 native）。state DB 不可达时降级为空集（Warn），不影响索引同步。
func codexChildThreadFallbacks(ctx context.Context, usageDB *db.DB, stateDir string, log *slog.Logger) (map[string]string, error) {
	rows, err := usageDB.QueryContext(ctx,
		`SELECT id FROM sessions WHERE client IN (?,?) AND COALESCE(title,'')=''`,
		model.ClientCodexApp, model.ClientCodexCLI)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("query empty-title sessions", "查询空标题会话失败"), err)
	}
	var emptyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: %w", ui.Bi("scan empty-title sessions", "扫描空标题会话失败"), err)
		}
		if id != "" {
			emptyIDs = append(emptyIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("%s: %w", ui.Bi("scan empty-title sessions", "扫描空标题会话失败"), err)
	}
	rows.Close()
	if len(emptyIDs) == 0 {
		return nil, nil
	}
	metas, metaErr := collector.ReadCodexStateThreadMeta(ctx, stateDir, log)
	if metaErr != nil && len(metas) == 0 {
		// state DB 全部不可达：无子线程判定依据，本周期跳过兜底（下一周期重试）。
		log.Warn("Codex state thread meta unavailable, skipping fallback backfill", "error", metaErr)
		return nil, nil
	}
	fallbacks := make(map[string]string, len(emptyIDs))
	for _, id := range emptyIDs {
		meta, ok := metas[id]
		if !ok || !meta.ChildThread || meta.Title != "" {
			continue
		}
		fallbacks[id] = collector.CodexChildThreadFallbackTitle(id)
	}
	return fallbacks, nil
}
