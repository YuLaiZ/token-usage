package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// applyDSHPersistPlan 在写事务内执行 DSH 的零值撤销：
// 先按请求日期范围删除，再只读检查是否仍有同 ID 行（日期不符的残留），
// 有则告警；最后交给随后的 UpsertMessages 写入本轮消息。
func applyDSHPersistPlan(
	ctx context.Context,
	tx *sql.Tx,
	plan []collector.DSHObsoleteMessage,
	dates []string,
	log *slog.Logger,
) error {
	// 落库名是显示名，不是配置 key（engine 传入的 client 是 dsh）。
	client := model.ClientDSH
	if len(plan) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(plan))
	ids := make([]string, 0, len(plan))
	for _, item := range plan {
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if _, err := db.DeleteMessagesByClientAndIDs(ctx, tx, client, dates, ids); err != nil {
		return fmt.Errorf("delete dsh obsolete messages: %w", err)
	}
	remaining, err := db.FindMessageIDsByClient(ctx, tx, client, ids)
	if err != nil {
		return fmt.Errorf("check dsh obsolete residual: %w", err)
	}
	if log != nil {
		for id, date := range remaining {
			log.Warn("dsh obsolete message still present after delete",
				"id", id, "db_date", date, "request_dates", dates)
		}
	}
	return nil
}
