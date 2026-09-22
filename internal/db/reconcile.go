package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// 本文件实现 mimocode Desktop 拆分 reconciliation 的数据库原语。
//
// 层次说明（与 schema 冻结的边界）：reconciliation 是运行时产品逻辑，其历史行
// 合并语义与当前 DAO upsert 同源（由 upsertSQLParts 从 DAO 常量切割列清单与
// ON CONFLICT 子句，DAO 演进时合并语义同步跟随）；schema migration（v4/v5）
// 不使用本函数，其合同为 migration-local 冻结字面量。行为由 schema_v5_test 与
// engine 侧 reconciliation 测试锁定。
//
// pending 协议（generation）：sync_state(client='mimocode', source=V5ReconcileSource)
// 行存在即未完成；cursor_value 兼作 generation，cursor_id 为分批游标。旧名
// 写入（旧版二进制回滚）经 trigger UPSERT 使 generation+1 并清空游标；批次
// 推进与最终清除按 generation CAS——generation 变化时当前 reconciliation
// 停止并按新 generation 从空游标重跑，清除未命中不得宣布完成。

// upsertSQLParts 从 DAO 的参数化 upsert 语句（? 占位符形态）中切割出表名、
// 有序列列清单与 "ON CONFLICT ..." 冲突更新子句，供 re-key 合并语句机械构造。
func upsertSQLParts(upsertSQL string) (table string, cols []string, conflictClause string, err error) {
	const insertPrefix = "INSERT INTO "
	open := strings.Index(upsertSQL, insertPrefix)
	if open < 0 {
		return "", nil, "", fmt.Errorf("upsert 语句缺少 INSERT INTO 前缀: %s", upsertSQL)
	}
	rest := upsertSQL[open+len(insertPrefix):]
	openParen := strings.IndexByte(rest, '(')
	if openParen < 0 {
		return "", nil, "", fmt.Errorf("upsert 语句缺少列清单括号: %s", upsertSQL)
	}
	// 列清单为平铺列名（无嵌套括号），第一个 ')' 即清单结束；其后（跨任意
	// 空白/换行）应紧跟 VALUES，兼容 ") VALUES" 与 ")\nVALUES" 两种排版。
	closeParen := strings.IndexByte(rest[openParen+1:], ')')
	if closeParen < 0 {
		return "", nil, "", fmt.Errorf("upsert 语句列清单未闭合: %s", upsertSQL)
	}
	closeParen += openParen + 1
	if after := strings.TrimSpace(rest[closeParen+1:]); !strings.HasPrefix(after, "VALUES") {
		return "", nil, "", fmt.Errorf("upsert 语句列清单后应为 VALUES: %s", upsertSQL)
	}
	table = strings.TrimSpace(rest[:openParen])
	for _, c := range strings.FieldsFunc(rest[openParen+1:closeParen], func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	}) {
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return "", nil, "", fmt.Errorf("upsert 语句列清单为空: %s", upsertSQL)
	}
	conflictIdx := strings.Index(upsertSQL, "ON CONFLICT")
	if conflictIdx < 0 {
		return "", nil, "", fmt.Errorf("upsert 语句缺少 ON CONFLICT 子句: %s", upsertSQL)
	}
	return table, cols, upsertSQL[conflictIdx:], nil
}

// MimoReconcilePending 返回 pending 状态、generation（cursor_value）与游标
// （cursor_id）。行不存在即无待处理。
func MimoReconcilePending(ctx context.Context, q dbtx) (pending bool, generation int64, cursorID string, err error) {
	var gen int64
	var cur string
	err = q.QueryRowContext(ctx,
		`SELECT cursor_value, cursor_id FROM sync_state WHERE client='mimocode' AND source=?`, V5ReconcileSource).Scan(&gen, &cur)
	if err == nil {
		return true, gen, cur, nil
	}
	if err == sql.ErrNoRows {
		return false, 0, "", nil
	}
	return false, 0, "", fmt.Errorf("查询 mimocode 拆分 pending 失败: %w", err)
}

// MimoReconcileRearm 重新置位 reconciliation：不存在时创建 generation=1，
// 已存在时 generation+1 并清空游标。显式 mimocode 全量采集把自己定义为一次
// 完整归属复核，因此即使此前 pending 已清除，也必须先 re-arm 再从源库全部
// assignments 重跑；这同时覆盖 session.version 或未来分类规则改变后、源消息
// 已被清理但 token-usage 仍保留历史行的会话。
func MimoReconcileRearm(ctx context.Context, q dbtx) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO sync_state(client,source,cursor_value,cursor_id,updated_at)
		VALUES('mimocode',?,1,'',datetime('now'))
		ON CONFLICT(client,source) DO UPDATE SET
		 cursor_value = cursor_value + 1,
		 cursor_id = '',
		 updated_at = datetime('now')`, V5ReconcileSource); err != nil {
		return fmt.Errorf("重新置位 mimocode 拆分 pending 失败: %w", err)
	}
	return nil
}

// MimoReconcileAdvanceCursor 按 generation CAS 推进游标（批事务内调用）：
// 仅当 pending 行的 cursor_value 仍等于本次 reconciliation 启动时的
// generation 才更新 cursor_id；影响行数为 0 表示期间发生旧名写入（generation
// 已变），调用方须整批回滚并按新 generation 从空游标重跑。
func MimoReconcileAdvanceCursor(ctx context.Context, q dbtx, generation int64, newCursorID string) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE sync_state SET cursor_id=?, updated_at=datetime('now')
		 WHERE client='mimocode' AND source=? AND cursor_value=?`,
		newCursorID, V5ReconcileSource, generation)
	if err != nil {
		return false, fmt.Errorf("推进 mimocode 拆分游标失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取推进影响行数失败: %w", err)
	}
	return n > 0, nil
}

// MimoReconcileClearPendingCAS 按 generation compare-and-delete 清除 pending
// （仅删除本次 reconciliation 确认过的 generation；未命中表示期间被旧名写入
// 重新置位，调用方不得宣布完成，须按新 generation 重跑）。
func MimoReconcileClearPendingCAS(ctx context.Context, d *DB, generation int64) (bool, error) {
	res, err := d.ExecContext(ctx,
		`DELETE FROM sync_state WHERE client='mimocode' AND source=? AND cursor_value=?`,
		V5ReconcileSource, generation)
	if err != nil {
		return false, fmt.Errorf("清除 mimocode 拆分 pending 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取清除影响行数失败: %w", err)
	}
	return n > 0, nil
}

// MimoReconcileBypassOn/Off 在 reconciliation 批事务内写入/删除 split
// trigger 的 bypass 标志行：使受控 re-key 的 'MiMo Code' 写入（target=Code
// 方向，此时源侧 Desktop 行仍在库）不被 split trigger 改写吞掉。行仅在事务
// 内存在（回滚即消失）；SQLite 写事务串行化保证外部写入不会越过保护窗口；
// 批外 split trigger 照常拦截。
func MimoReconcileBypassOn(ctx context.Context, q dbtx) error {
	if _, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO sync_state(client,source,cursor_value,cursor_id,updated_at)
		 VALUES('mimocode',?,0,'',datetime('now'))`, V5ReconcileBypassSource); err != nil {
		return fmt.Errorf("写入 split bypass 标志失败: %w", err)
	}
	return nil
}

func MimoReconcileBypassOff(ctx context.Context, q dbtx) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM sync_state WHERE client='mimocode' AND source=?`, V5ReconcileBypassSource); err != nil {
		return fmt.Errorf("删除 split bypass 标志失败: %w", err)
	}
	return nil
}

// MimoRekeySessions 把 sourceClient 侧指定会话的历史行无损合并到 targetClient
// （messages 按 session_id、sessions 按 id 关联）。messages 与 sessions 各一条
// INSERT ... SELECT ... ON CONFLICT 语句：冲突合并语义切割自当前 DAO upsert
// （早 ts 归因、token/model 新值覆盖、router_* 非空才覆盖、title 非空保留、
// first/last_ts 区间收窄），不做裸 UPDATE（主键含 client，同 id 并存时确定性
// 合并为一条，token 不双计）。调用方必须在事务内先开启 split bypass（见
// MimoReconcileBypassOn），否则 target=MiMoCode 方向会被 split trigger 改写
// 回 MiMo Desktop 后吞掉。sourceClient 侧行的删除由调用方在确认合并成功
// 后同事务执行（DeleteMimoSourceRows）。
func MimoRekeySessions(ctx context.Context, q dbtx, sessionIDs []string, targetClient, sourceClient string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	for _, upsert := range []string{upsertMessageSQL, upsertSessionMetaSQL} {
		table, cols, conflict, err := upsertSQLParts(upsert)
		if err != nil {
			return fmt.Errorf("构造 %s re-key 语句失败: %w", table, err)
		}
		keyCol := "session_id"
		if table == "sessions" {
			keyCol = "id"
		}
		sels := make([]string, 0, len(cols)+1)
		args := make([]interface{}, 0, len(sessionIDs)+2)
		for _, c := range cols {
			if c == "client" {
				sels = append(sels, "?")
				args = append(args, targetClient)
			} else {
				sels = append(sels, c)
			}
		}
		placeholders := make([]string, 0, len(sessionIDs))
		for _, id := range sessionIDs {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		args = append(args, sourceClient)
		stmt := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s) AND client = ? %s",
			table, strings.Join(cols, ", "), strings.Join(sels, ", "), table, keyCol,
			strings.Join(placeholders, ","), conflict)
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s\nargs: %v", err, stmt, args)
		}
	}
	return nil
}

// DeleteMimoSourceRows 删除已成功合并到目标侧的 sourceClient 侧行（messages 按
// session_id、sessions 按 id）。必须在 MimoRekeySessions 同一事务内、合并成功
// 之后执行；与合并一起构成无损 re-key（目标侧先完整接收，源侧才删除）。
func DeleteMimoSourceRows(ctx context.Context, q dbtx, sessionIDs []string, sourceClient string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(sessionIDs))
	args := make([]interface{}, 0, len(sessionIDs)+1)
	for _, id := range sessionIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	args = append(args, sourceClient)
	in := "(" + strings.Join(placeholders, ",") + ")"
	for _, stmt := range []string{
		"DELETE FROM messages WHERE session_id IN " + in + " AND client = ?",
		"DELETE FROM sessions WHERE id IN " + in + " AND client = ?",
	} {
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}
