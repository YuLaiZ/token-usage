package db

import (
	"context"
	"fmt"
	"strings"
)

// DeleteMessagesByClientAndIDs 按 client + 可选日期范围 + id 集合删除消息。
// dates 为空表示不限日期；返回实际删除行数。
func DeleteMessagesByClientAndIDs(ctx context.Context, q dbtx, client string, dates []string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	for _, chunk := range chunkStrings(ids, upsertMaxSQLParams/2) {
		query := "DELETE FROM messages WHERE client = ?"
		args := []any{client}
		if len(dates) > 0 {
			query += " AND date IN (" + sqlPlaceholders(len(dates)) + ")"
			for _, date := range dates {
				args = append(args, date)
			}
		}
		query += " AND id IN (" + sqlPlaceholders(len(chunk)) + ")"
		for _, id := range chunk {
			args = append(args, id)
		}
		result, err := q.ExecContext(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("delete messages: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("delete messages rows: %w", err)
		}
		total += affected
	}
	return total, nil
}

// FindMessageIDsByClient 返回给定 id 集合中仍存在于该 client 下的行（id -> date）。
// 查询不加日期条件：调用方用它判断「删除后是否还有日期不符的残留」。
func FindMessageIDsByClient(ctx context.Context, q dbtx, client string, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(ids, upsertMaxSQLParams/2) {
		query := "SELECT id, date FROM messages WHERE client = ? AND id IN (" +
			sqlPlaceholders(len(chunk)) + ")"
		args := []any{client}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return out, fmt.Errorf("find messages: %w", err)
		}
		for rows.Next() {
			var id, date string
			if err := rows.Scan(&id, &date); err != nil {
				rows.Close()
				return out, fmt.Errorf("scan message: %w", err)
			}
			out[id] = date
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, fmt.Errorf("iterate messages: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

func chunkStrings(items []string, size int) [][]string {
	if size <= 0 {
		size = 1
	}
	var chunks [][]string
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
