package querier

import (
	"context"
	"fmt"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// sqliteDateTimeLayout 是 SQLite datetime('now') 生成的 UTC 文本格式。
const sqliteDateTimeLayout = "2006-01-02 15:04:05"

// Freshness 承载 query 统一信息区的两项数据新鲜度指标。
// 零值字段由展示层渲染为 em dash,分别对应"范围内无消息事件"与"尚无成功采集记录"。
type Freshness struct {
	// MaxMessageTS 是统计日期范围内 messages.ts 的最大非零值(Unix 毫秒)。
	// 全部采集器写入的 ts 均为毫秒;0 表示范围内不存在消息事件时间。
	MaxMessageTS int64
	// LastCollection 是全库 collection_log.collected_at 最大值对应的本机时区时刻,
	// 即最近一次成功采集完成的时间;零值表示库中没有任何成功采集记录。
	LastCollection time.Time
}

// Freshness 查询统计范围的数据截至与全库最近成功采集时间。
// 数据截至随本次 dates 过滤;最近成功采集是全库口径,不随日期参数变化。
func (q *Querier) Freshness(ctx context.Context, dates []string) (Freshness, error) {
	ctx, err := q.readyContext(ctx)
	if err != nil {
		return Freshness{}, err
	}

	var fresh Freshness
	if len(dates) > 0 {
		placeholders, args := buildPlaceholders(dates)
		query := fmt.Sprintf(
			"SELECT COALESCE(MAX(ts), 0) FROM messages WHERE ts > 0 AND date IN (%s)",
			placeholders,
		)
		if err := q.db.QueryRowContext(ctx, query, args...).Scan(&fresh.MaxMessageTS); err != nil {
			return Freshness{}, fmt.Errorf("%s: %w", ui.Bi("query failed", "查询失败"), err)
		}
	}

	var collected string
	if err := q.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(collected_at), '') FROM collection_log",
	).Scan(&collected); err != nil {
		return Freshness{}, fmt.Errorf("%s: %w", ui.Bi("failed to query collection log", "查询采集记录失败"), err)
	}
	if collected == "" {
		return fresh, nil
	}
	t, err := time.Parse(sqliteDateTimeLayout, collected)
	if err != nil {
		return Freshness{}, fmt.Errorf("%s %q: %w",
			ui.Bi("invalid collected_at timestamp in collection_log:", "collection_log 中存在无法解析的采集时间:"), collected, err)
	}
	fresh.LastCollection = t.Local()
	return fresh, nil
}

// DateBounds 返回日期切片的最小/最大日期。dates 为 "YYYY-MM-DD",
// 字典序即时间序;调用方保证切片非空。
func DateBounds(dates []string) (string, string) {
	first, last := dates[0], dates[0]
	for _, date := range dates[1:] {
		if date < first {
			first = date
		}
		if date > last {
			last = date
		}
	}
	return first, last
}
