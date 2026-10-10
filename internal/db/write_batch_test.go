package db

// 写路径批量化（P1/P2）的语义等价锁定测试：
//   - 每个改造 DAO 用「逐行参照实现 vs 生产实现」背靠背双跑 fixture，
//     两个独立库全列 diff 全等（幂等/冲突/重复键/空值保留等类别）；
//   - P2 唯一键守卫：交错/相邻重复键整批回退逐项（A→B→A 终值 A、计数 3）；
//   - 失败合同：任一批语句失败立即返回 → 调用方事务整体回滚；ctx 取消同样。
// 参照实现按改造前生产逐行逻辑原样内嵌（含错误信息形态），不随批量实现演进。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// ===== 逐行参照实现（改造前生产逻辑原样内嵌） =====

func refUpsertMessages(ctx context.Context, q dbtx, messages []model.Message) error {
	for _, m := range messages {
		if _, err := q.ExecContext(ctx, upsertMessageSQL,
			m.ID, m.SessionID, m.Client, m.Date, m.TS, m.Model, m.Provider,
			m.RouterProvider, m.RouterModel, m.RouterName, m.Directory, m.Project,
			m.InputTokens, m.FreshInputTokens, m.OutputTokens, m.CacheReadTokens,
			m.CacheCreateTokens, m.ReasoningTokens, m.TotalTokens, m.DurationMS,
		); err != nil {
			return fmt.Errorf("upsert message %q/%q 失败: %w", m.Client, m.ID, err)
		}
	}
	return nil
}

func refUpsertSessionMeta(ctx context.Context, q dbtx, sessions []model.Session) error {
	for _, s := range sessions {
		var stmt string
		var args []interface{}
		if isCodexDisplayClient(s.Client) {
			stmt = upsertCodexSessionMetaSQL
			args = []interface{}{s.ID, s.Client, s.Directory, s.Project, s.Title, s.ParentID, s.FirstTS, s.LastTS, s.TitleSource, s.TitleIndexTS}
		} else {
			stmt = upsertSessionMetaSQL
			args = []interface{}{s.ID, s.Client, s.Directory, s.Project, s.Title, s.ParentID, s.FirstTS, s.LastTS}
		}
		if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("upsert session meta %q/%q 失败: %w", s.Client, s.ID, err)
		}
	}
	return nil
}

func refUpsertRawRouterLogs(ctx context.Context, q dbtx, logs []model.RouterLog) error {
	for _, l := range logs {
		if _, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO raw_router_logs (
			request_id, message_id, router_name, session_id, app_type, model,
			provider_id, provider_name, input_tokens, output_tokens,
			cache_read_tokens, cache_create_tokens, created_at, data_source, raw_data
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.RequestID, l.MessageID, l.RouterName, l.SessionID, l.AppType, l.Model,
			l.ProviderID, l.ProviderName, l.InputTokens, l.OutputTokens,
			l.CacheReadTokens, l.CacheCreateTokens, l.CreatedAt, l.DataSource, l.RawData,
		); err != nil {
			return fmt.Errorf("插入 raw_router_log %q 失败: %w", l.RequestID, err)
		}
	}
	return nil
}

func refBackfillRouterFields(ctx context.Context, q dbtx, infos []model.RouterAttribution) (int, error) {
	count := 0
	for _, info := range infos {
		res, err := q.ExecContext(ctx, backfillRouterSQL,
			info.Provider, info.Provider,
			info.Model, info.Model,
			info.RouterName, info.RouterName,
			info.Client, info.MessageID,
		)
		if err != nil {
			return count, fmt.Errorf("回填 router 字段 %q/%q 失败: %w", info.Client, info.MessageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return count, fmt.Errorf("读取回填结果 %q/%q 失败: %w", info.Client, info.MessageID, err)
		}
		count += int(n)
	}
	return count, nil
}

// ===== 双跑比较脚手架 =====

// dualWrite 在两个全新等价库上分别执行 ref 与 prod 写路径（各自单事务提交），
// 返回两库供全列 diff。
func dualWrite(t *testing.T, name string,
	ref func(context.Context, dbtx) error,
	prod func(context.Context, dbtx) error,
) (refDB, prodDB *DB) {
	t.Helper()
	open := func(tag string) *DB {
		d, err := Open(filepath.Join(t.TempDir(), tag+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	refDB, prodDB = open(name+"-ref"), open(name+"-prod")
	for _, c := range []struct {
		d   *DB
		fn  func(context.Context, dbtx) error
		tag string
	}{
		{refDB, ref, "ref"}, {prodDB, prod, "prod"},
	} {
		tx, err := c.d.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.fn(context.Background(), tx); err != nil {
			t.Fatalf("%s write: %v", c.tag, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return refDB, prodDB
}

// assertTablesEqual 全列 diff 两库的指定表（按主键列排序——物理行序
// 受写入分块分组影响，不属语义差异）。pk 为主键列表达式（如 "client,id"）；
// exclude 逗号分隔需要排除的列（如 raw_router_logs 的 collected_at——其
// DEFAULT datetime('now') 随语句求值时刻变化，双跑两事务跨秒即不同，
// 不属写路径语义）。
func assertTablesEqual(t *testing.T, what, table, pk, exclude string, a, b *DB) {
	t.Helper()
	colsExpr := "*"
	if exclude != "" {
		var kept []string
		all := tableColumns[table]
		if all == "" {
			t.Fatalf("assertTablesEqual: 未登记表 %s 的列清单", table)
		}
		skip := map[string]bool{}
		for _, c := range strings.Split(exclude, ",") {
			skip[c] = true
		}
		for _, c := range strings.Split(all, ",") {
			if !skip[c] {
				kept = append(kept, c)
			}
		}
		colsExpr = strings.Join(kept, ",")
	}
	dump := func(d *DB) string {
		rows, err := d.Query("SELECT " + colsExpr + " FROM " + table + " ORDER BY " + pk)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if i > 0 {
					sb.WriteByte('|')
				}
				switch tv := v.(type) {
				case nil:
					sb.WriteString("<NULL>")
				case []byte:
					sb.Write(tv)
				default:
					fmt.Fprintf(&sb, "%v", tv)
				}
			}
			sb.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	da, db_ := dump(a), dump(b)
	if da != db_ {
		t.Fatalf("%s: %s 表不一致\n--- ref ---\n%s--- prod ---\n%s", what, table, da, db_)
	}
}

// ===== P1 UpsertMessages 等价 =====

func batchMessagesFixture() []model.Message {
	msgs := []model.Message{
		{ID: "m1", SessionID: "s1", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 1000,
			Model: "m", Provider: "p", Directory: "/d", Project: "pr",
			InputTokens: 1, FreshInputTokens: 1, OutputTokens: 2, TotalTokens: 3, DurationMS: 1500},
		// 幂等重放（同键新值：token 覆盖、归因取较早 ts）。
		{ID: "m1", SessionID: "s1", Client: model.ClientClaudeCode, Date: "2026-10-10", TS: 500,
			Model: "m2", Provider: "p2", Directory: "/d2", Project: "pr2",
			InputTokens: 10, FreshInputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		// router 空值保留（先写非空再写空）。
		{ID: "m2", SessionID: "s1", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 1100,
			RouterProvider: "rp", RouterModel: "rm", RouterName: "rn", TotalTokens: 1},
		{ID: "m2", SessionID: "s1", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 1200,
			RouterProvider: "", RouterModel: "", RouterName: "", TotalTokens: 2},
	}
	// 跨批重复键（>49 行触发分块，m-extra-7 与首块行同键）。
	for i := 0; i < 120; i++ {
		msgs = append(msgs, model.Message{
			ID: fmt.Sprintf("m-extra-%d", i), SessionID: "s1", Client: model.ClientClaudeCode,
			Date: "2026-10-09", TS: int64(2000 + i), TotalTokens: int64(i),
		})
	}
	msgs = append(msgs, model.Message{
		ID: "m-extra-7", SessionID: "s1", Client: model.ClientClaudeCode,
		Date: "2026-10-09", TS: 9999, TotalTokens: 777,
	})
	return msgs
}

func TestUpsertMessages_BatchEquivalent(t *testing.T) {
	msgs := batchMessagesFixture()
	refDB, prodDB := dualWrite(t, "msgs",
		func(ctx context.Context, q dbtx) error { return refUpsertMessages(ctx, q, msgs) },
		func(ctx context.Context, q dbtx) error {
			_, err := UpsertMessages(ctx, q, msgs)
			return err
		})
	assertTablesEqual(t, "UpsertMessages", "messages", "client,id", "", refDB, prodDB)
	// 返回计数 = 行数（含重复键重放）。
	tx, err := prodDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := UpsertMessages(context.Background(), tx, msgs)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if n != len(msgs) {
		t.Fatalf("count = %d, want %d", n, len(msgs))
	}
}

// 批中段失败：构造 trigger 使指定 id 触发 RAISE，位于批中段 → 整批失败、
// 调用方事务回滚（消息零提交）。
func TestUpsertMessages_MidBatchFailureRollsBack(t *testing.T) {
	msgs := batchMessagesFixture()
	d, err := Open(filepath.Join(t.TempDir(), "midfail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TRIGGER fail_mid BEFORE INSERT ON messages
		WHEN NEW.id='m-extra-60'
		BEGIN SELECT RAISE(ABORT, 'mid-batch fail'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertMessages(context.Background(), tx, msgs); err == nil {
		_ = tx.Rollback()
		t.Fatal("批中段失败必须返回错误")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("失败批不得提交任何行: n=%d err=%v", n, err)
	}
}

// ctx 取消：同样整批失败不提交。
func TestUpsertMessages_ContextCancelled(t *testing.T) {
	msgs := batchMessagesFixture()
	d, err := Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := d.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := UpsertMessages(ctx, tx, msgs); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的 ctx 必须使写入失败: %v", err)
	}
}

// 分块参数上限：行×列 ≤ 999（SQLite 变量上限），具名常量与各 DAO 的
// 推导口径（messages 20 列 / sessions 8、10 列 / raw_router_logs 15 列 /
// backfill 7+ids）全部锁定。
func TestUpsertChunkConstants(t *testing.T) {
	if upsertMaxSQLParams != 999 {
		t.Fatalf("upsertMaxSQLParams = %d, want 999", upsertMaxSQLParams)
	}
	for _, c := range []struct {
		name     string
		cols     int
		chunkRow int
	}{
		{"messages", 20, upsertMessagesChunkRows},
		{"sessions-generic", 8, upsertSessionsGenericRows},
		{"sessions-codex", 10, upsertSessionsCodexRows},
		{"raw_router_logs", 15, upsertRawRouterLogsChunkRow},
	} {
		if c.chunkRow < 1 || c.chunkRow*c.cols > upsertMaxSQLParams || (c.chunkRow+1)*c.cols <= upsertMaxSQLParams {
			t.Fatalf("%s chunkRows=%d（cols=%d）：须为不超上限的最大行数", c.name, c.chunkRow, c.cols)
		}
	}
	// backfill 批量 UPDATE：前缀占位符按 SQL 文本实数（SET 6 + WHERE 1），
	// 满块参数恰为 upsertMaxSQLParams——推导基数与前缀文本任何一侧漂移
	//（如多加/漏加一个 ? 或改 -6）都会使本断言红。
	if n := strings.Count(backfillBatchSQLPrefix, "?"); n != backfillPrefixParams {
		t.Fatalf("backfill 前缀占位符 = %d, 推导基数 = %d（须一致）", n, backfillPrefixParams)
	}
	if backfillPrefixParams+maxBackfillIDsPerStmt > upsertMaxSQLParams {
		t.Fatalf("backfill 满块参数超限: %d+%d > %d", backfillPrefixParams, maxBackfillIDsPerStmt, upsertMaxSQLParams)
	}
}

// ===== P1 UpsertSessionMeta 等价（混合 client 批次） =====

func TestUpsertSessionMeta_BatchEquivalent(t *testing.T) {
	var sessions []model.Session
	// 混合 codex/普通交替（跨语句分组），组内含重复键与 title 优先级形态。
	for i := 0; i < 30; i++ {
		sessions = append(sessions,
			model.Session{ID: fmt.Sprintf("cx-%d", i), Client: model.ClientCodexApp,
				Directory: "/d", Project: "p", Title: fmt.Sprintf("t%d", i), FirstTS: int64(i), LastTS: int64(i + 1),
				TitleSource: model.TitleSourceIndex, TitleIndexTS: int64(100 + i)},
			model.Session{ID: fmt.Sprintf("cl-%d", i), Client: model.ClientClaudeCode,
				Directory: "/d", Project: "p", Title: fmt.Sprintf("t%d", i), FirstTS: int64(i), LastTS: int64(i + 1)},
		)
	}
	// 重复键：codex index 时间门（先低后高、高后低两形态）与空标题保留。
	sessions = append(sessions,
		model.Session{ID: "cx-gate", Client: model.ClientCodexApp, Title: "new", TitleSource: model.TitleSourceIndex, TitleIndexTS: 500, FirstTS: 1, LastTS: 2},
		model.Session{ID: "cx-gate", Client: model.ClientCodexApp, Title: "", TitleIndexTS: 0, FirstTS: 1, LastTS: 9},
		model.Session{ID: "cx-gate", Client: model.ClientCodexApp, Title: "older", TitleSource: model.TitleSourceIndex, TitleIndexTS: 300, FirstTS: 1, LastTS: 2},
	)
	refDB, prodDB := dualWrite(t, "sess",
		func(ctx context.Context, q dbtx) error { return refUpsertSessionMeta(ctx, q, sessions) },
		func(ctx context.Context, q dbtx) error {
			_, err := UpsertSessionMeta(ctx, q, sessions)
			return err
		})
	assertTablesEqual(t, "UpsertSessionMeta", "sessions", "id,client", "", refDB, prodDB)
}

// ===== P1 UpsertRawRouterLogs 等价（REPLACE 语义） =====

func TestUpsertRawRouterLogs_BatchEquivalent(t *testing.T) {
	var logs []model.RouterLog
	for i := 0; i < 80; i++ {
		logs = append(logs, model.RouterLog{
			RequestID: fmt.Sprintf("req-%d", i), MessageID: fmt.Sprintf("msg-%d", i),
			RouterName: "cc_switch", Model: fmt.Sprintf("m%d", i),
			InputTokens: int64(i), OutputTokens: int64(i * 2),
			CreatedAt: int64(i), DataSource: "proxy",
		})
	}
	// REPLACE 语义：同 request_id 重写（字段更新）+ 批内重复。
	logs = append(logs,
		model.RouterLog{RequestID: "req-1", MessageID: "msg-1x", RouterName: "cc_switch",
			InputTokens: 111, CreatedAt: 999, DataSource: "proxy"},
		model.RouterLog{RequestID: "req-1", MessageID: "msg-1y", RouterName: "cc_switch",
			InputTokens: 222, CreatedAt: 1000, DataSource: "proxy"},
	)
	refDB, prodDB := dualWrite(t, "rlogs",
		func(ctx context.Context, q dbtx) error { return refUpsertRawRouterLogs(ctx, q, logs) },
		func(ctx context.Context, q dbtx) error {
			_, err := UpsertRawRouterLogs(ctx, q, logs)
			return err
		})
	assertTablesEqual(t, "UpsertRawRouterLogs", "raw_router_logs", "request_id,router_name", "collected_at", refDB, prodDB)
}

// ===== P2 BackfillRouterFields =====

// 唯一键输入：批量路径。五形态计数与终值与逐行参照全等。
func TestBackfillRouterFields_BatchUniqueKeys(t *testing.T) {
	seed := func(d *DB) {
		ctx := context.Background()
		var msgs []model.Message
		for i := 0; i < 60; i++ {
			msgs = append(msgs, model.Message{ID: fmt.Sprintf("bm-%d", i), SessionID: "s",
				Client: model.ClientClaudeCode, Date: "2026-10-09", TS: int64(i), TotalTokens: 1})
		}
		// 目标消息不存在：不计数不报错。
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UpsertMessages(ctx, tx, msgs); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var infos []model.RouterAttribution
	for i := 0; i < 60; i++ {
		info := model.RouterAttribution{
			Client: model.ClientClaudeCode, MessageID: fmt.Sprintf("bm-%d", i),
			Provider: fmt.Sprintf("prov-%d", i%3), Model: fmt.Sprintf("model-%d", i%3),
			RouterName: "cc_switch",
		}
		if i%5 == 0 {
			info.Provider = "" // 空值保留：不覆盖
		}
		infos = append(infos, info)
	}
	infos = append(infos, model.RouterAttribution{
		Client: model.ClientClaudeCode, MessageID: "missing-target", // 不存在
		Provider: "x", Model: "y", RouterName: "z",
	})

	run := func() {
		refDB, prodDB := dualWrite(t, "bf",
			func(ctx context.Context, q dbtx) error { _, err := refBackfillRouterFields(ctx, q, infos); return err },
			func(ctx context.Context, q dbtx) error { _, err := BackfillRouterFields(ctx, q, infos); return err })
		seed(refDB)
		seed(prodDB)
		// 对种子库各自跑回填（独立事务）。
		var refN, prodN int
		for _, c := range []struct {
			d    *DB
			fn   func(context.Context, dbtx, []model.RouterAttribution) (int, error)
			nPtr *int
		}{{refDB, refBackfillRouterFields, &refN}, {prodDB, BackfillRouterFields, &prodN}} {
			tx, err := c.d.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			n, err := c.fn(context.Background(), tx, infos)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			*c.nPtr = n
		}
		if refN != prodN {
			t.Fatalf("计数不一致: ref=%d prod=%d", refN, prodN)
		}
		if prodN != 60 {
			t.Fatalf("计数 = %d, want 60（60 行命中；空值仍计数=UPDATE 命中；missing 不计数）", prodN)
		}
		assertTablesEqual(t, "BackfillRouterFields", "messages", "client,id", "", refDB, prodDB)
		// 空值保留直证：bm-5 的 info.Provider 为空（i%5==0）→ 库值不被覆盖；
		// bm-1（i%3=1）→ prov-1。
		var rp string
		if err := prodDB.QueryRow(`SELECT router_provider FROM messages WHERE id='bm-5'`).Scan(&rp); err != nil {
			t.Fatal(err)
		}
		if rp != "" {
			t.Fatalf("bm-5 router_provider = %q, want 空（空值不覆盖）", rp)
		}
		if err := prodDB.QueryRow(`SELECT router_provider FROM messages WHERE id='bm-1'`).Scan(&rp); err != nil {
			t.Fatal(err)
		}
		if rp != "prov-1" {
			t.Fatalf("bm-1 router_provider = %q, want prov-1", rp)
		}
	}
	run()
}

// 重复键守卫：交错重复 A→B→A 终值 A、计数 3（整批回退逐项）。
func TestBackfillRouterFields_DuplicateKeysFallBack(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "dup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertMessages(ctx, tx, []model.Message{
		{ID: "dup-1", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 1, TotalTokens: 1},
		{ID: "adj-1", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 2, TotalTokens: 1},
		{ID: "adj-2", SessionID: "s", Client: model.ClientClaudeCode, Date: "2026-10-09", TS: 3, TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	infos := []model.RouterAttribution{
		{Client: model.ClientClaudeCode, MessageID: "dup-1", Provider: "A", Model: "A", RouterName: "A"},
		{Client: model.ClientClaudeCode, MessageID: "dup-1", Provider: "B", Model: "B", RouterName: "B"},
		{Client: model.ClientClaudeCode, MessageID: "dup-1", Provider: "A", Model: "A", RouterName: "A"}, // 交错 A→B→A
		{Client: model.ClientClaudeCode, MessageID: "adj-1", Provider: "C", Model: "C", RouterName: "C"},
		{Client: model.ClientClaudeCode, MessageID: "adj-2", Provider: "D", Model: "D", RouterName: "D"},
		{Client: model.ClientClaudeCode, MessageID: "adj-2", Provider: "D", Model: "D", RouterName: "D"}, // 相邻重复
	}
	// 直接在 d 上跑生产实现断言终值/计数（重复键路径=逐项回退，即旧实现行为）。
	tx2, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := BackfillRouterFields(ctx, tx2, infos)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	// A→B→A 逐项执行：终值 A，dup-1 被命中 3 次；相邻重复 adj-2 命中 2 次；
	// 合计计数 = 3+1+2 = 6（全部命中行计数，字段相同仍计数）。
	if n != 6 {
		t.Fatalf("计数 = %d, want 6（A→B→A=3 + adj-1=1 + adj-2 相邻重复=2）", n)
	}
	var provider string
	if err := d.QueryRow(`SELECT router_provider FROM messages WHERE id='dup-1'`).Scan(&provider); err != nil {
		t.Fatal(err)
	}
	if provider != "A" {
		t.Fatalf("交错 A→B→A 终值 = %q, want A（重复键整批回退逐项）", provider)
	}
}

// 批中段失败：批量回填任一批失败 → 整批回滚（零行提交）。
func TestBackfillRouterFields_MidBatchFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "bf-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var msgs []model.Message
	var infos []model.RouterAttribution
	for i := 0; i < 40; i++ {
		msgs = append(msgs, model.Message{ID: fmt.Sprintf("bf-%d", i), SessionID: "s",
			Client: model.ClientClaudeCode, Date: "2026-10-09", TS: int64(i), TotalTokens: 1})
		infos = append(infos, model.RouterAttribution{
			Client: model.ClientClaudeCode, MessageID: fmt.Sprintf("bf-%d", i),
			Provider: "p", Model: "m", RouterName: "r",
		})
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertMessages(ctx, tx, msgs); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER fail_bf BEFORE UPDATE ON messages
		WHEN NEW.id='bf-20'
		BEGIN SELECT RAISE(ABORT, 'backfill fail'); END`); err != nil {
		t.Fatal(err)
	}
	tx2, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillRouterFields(ctx, tx2, infos); err == nil {
		_ = tx2.Rollback()
		t.Fatal("批中段失败必须返回错误")
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	var filled int
	if err := d.QueryRow(`SELECT COUNT(*) FROM messages WHERE router_provider!=''`).Scan(&filled); err != nil || filled != 0 {
		t.Fatalf("失败批不得提交: filled=%d err=%v", filled, err)
	}
}

// P2 边界：单一四元组组内 ≥992 个 id（跨块）等价与守恒；组参数恰不超 999。
func TestBackfillRouterFields_ChunkBoundarySingleGroup(t *testing.T) {
	ctx := context.Background()
	var msgs []model.Message
	var infos []model.RouterAttribution
	const n = 2100 // > 2×992：跨三块
	for i := 0; i < n; i++ {
		msgs = append(msgs, model.Message{ID: fmt.Sprintf("cb-%d", i), SessionID: "s",
			Client: model.ClientClaudeCode, Date: "2026-10-09", TS: int64(i), TotalTokens: 1})
		infos = append(infos, model.RouterAttribution{
			Client: model.ClientClaudeCode, MessageID: fmt.Sprintf("cb-%d", i),
			Provider: "same-prov", Model: "same-model", RouterName: "same-router",
		})
	}
	refDB, prodDB := dualWrite(t, "bf-chunk",
		func(ctx context.Context, q dbtx) error { _, err := refBackfillRouterFields(ctx, q, infos); return err },
		func(ctx context.Context, q dbtx) error { _, err := BackfillRouterFields(ctx, q, infos); return err })
	for _, d := range []*DB{refDB, prodDB} {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UpsertMessages(ctx, tx, msgs); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var refN, prodN int
	for _, c := range []struct {
		d    *DB
		fn   func(context.Context, dbtx, []model.RouterAttribution) (int, error)
		nPtr *int
	}{{refDB, refBackfillRouterFields, &refN}, {prodDB, BackfillRouterFields, &prodN}} {
		tx, err := c.d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		nn, err := c.fn(ctx, tx, infos)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		*c.nPtr = nn
	}
	if refN != prodN || prodN != n {
		t.Fatalf("计数 ref=%d prod=%d, want %d（跨块全命中）", refN, prodN, n)
	}
	assertTablesEqual(t, "BackfillRouterFields chunk", "messages", "client,id", "", refDB, prodDB)
}

// SessionMeta 跨块路径：generic 组 >124 行触发多块，含跨块重复键。
func TestUpsertSessionMeta_MultiChunk(t *testing.T) {
	var sessions []model.Session
	for i := 0; i < 300; i++ {
		sessions = append(sessions, model.Session{
			ID: fmt.Sprintf("mc-%d", i), Client: model.ClientClaudeCode,
			Directory: "/d", Project: "p", FirstTS: int64(i), LastTS: int64(i),
		})
	}
	// 跨块重复键：末块重写首块行。
	sessions = append(sessions, model.Session{
		ID: "mc-0", Client: model.ClientClaudeCode,
		Directory: "/d2", Project: "p2", Title: "rewritten", FirstTS: 0, LastTS: 999,
	})
	refDB, prodDB := dualWrite(t, "sess-mc",
		func(ctx context.Context, q dbtx) error { return refUpsertSessionMeta(ctx, q, sessions) },
		func(ctx context.Context, q dbtx) error { _, err := UpsertSessionMeta(ctx, q, sessions); return err })
	assertTablesEqual(t, "UpsertSessionMeta multi-chunk", "sessions", "id,client", "", refDB, prodDB)
	var title string
	if err := prodDB.QueryRow(`SELECT title FROM sessions WHERE id='mc-0'`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "rewritten" {
		t.Fatalf("跨块重复键终值 = %q, want rewritten", title)
	}
}

// tableColumns 登记等价测试覆盖表的全部列（exclude 列剔除用）。
var tableColumns = map[string]string{
	"messages":        "id,session_id,client,date,ts,model,provider,router_provider,router_model,router_name,directory,project,input_tokens,fresh_input_tokens,output_tokens,cache_read_tokens,cache_create_tokens,reasoning_tokens,total_tokens,duration_ms",
	"sessions":        "id,client,directory,project,title,parent_id,first_ts,last_ts,title_source,title_index_ts",
	"raw_router_logs": "request_id,message_id,router_name,session_id,app_type,model,provider_id,provider_name,input_tokens,output_tokens,cache_read_tokens,cache_create_tokens,created_at,data_source,raw_data,collected_at",
}

// codex 形态（10 列）跨块：150 行 codex 会话 > 99 行/块，含跨块重复键与
// index 时间门形态。
func TestUpsertSessionMeta_CodexMultiChunk(t *testing.T) {
	var sessions []model.Session
	for i := 0; i < 150; i++ {
		sessions = append(sessions, model.Session{
			ID: fmt.Sprintf("cxm-%d", i), Client: model.ClientCodexApp,
			Directory: "/d", Project: "p", Title: fmt.Sprintf("t%d", i),
			FirstTS: int64(i), LastTS: int64(i + 1),
			TitleSource: model.TitleSourceIndex, TitleIndexTS: int64(i),
		})
	}
	sessions = append(sessions, model.Session{
		ID: "cxm-0", Client: model.ClientCodexApp,
		Directory: "/d2", Project: "p2", Title: "rewritten", FirstTS: 0, LastTS: 999,
		TitleSource: model.TitleSourceIndex, TitleIndexTS: 5000,
	})
	refDB, prodDB := dualWrite(t, "sess-cxmc",
		func(ctx context.Context, q dbtx) error { return refUpsertSessionMeta(ctx, q, sessions) },
		func(ctx context.Context, q dbtx) error { _, err := UpsertSessionMeta(ctx, q, sessions); return err })
	assertTablesEqual(t, "UpsertSessionMeta codex multi-chunk", "sessions", "id,client", "", refDB, prodDB)
	var title string
	var ts int64
	if err := prodDB.QueryRow(`SELECT title,title_index_ts FROM sessions WHERE id='cxm-0'`).Scan(&title, &ts); err != nil {
		t.Fatal(err)
	}
	if title != "rewritten" || ts != 5000 {
		t.Fatalf("跨块重复键终值 = %q/%d, want rewritten/5000", title, ts)
	}
}
