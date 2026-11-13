package querier

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
)

// 区间普通失败摘要：只统计未解决 error 类型，不含 monitor/warning。
func TestQueryUnresolvedErrorSummary(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordError(ctx, d, "2026-11-11", "claude", "claude read / 读取数据源失败: boom", ""); err != nil {
		t.Fatal(err)
	}
	// 已解决的不计入。
	if err := db.RecordError(ctx, d, "2026-11-11", "codex", "codex boom", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveErrorsByDateSource(ctx, d, "2026-11-11", "codex"); err != nil {
		t.Fatal(err)
	}
	// 监控中断不计入。
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-x", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	// 区间外的不计入。
	if err := db.RecordError(ctx, d, "2026-10-01", "claude", "out of range", ""); err != nil {
		t.Fatal(err)
	}

	q := New(d)
	sum, err := q.QueryUnresolvedErrorSummary(ctx, []string{"2026-11-11"})
	if err != nil {
		t.Fatalf("QueryUnresolvedErrorSummary: %v", err)
	}
	if sum.Count != 1 {
		t.Fatalf("count = %d, want 1（只统计区间内未解决的 error 类型）", sum.Count)
	}
	if !sum.HasLatest || sum.LatestSource != "claude" || sum.LatestAt == "" {
		t.Fatalf("latest = %+v", sum)
	}

	// 无记录区间：零值、无 latest。
	empty, err := q.QueryUnresolvedErrorSummary(ctx, []string{"2026-01-01"})
	if err != nil || empty.Count != 0 || empty.HasLatest {
		t.Fatalf("空区间 = %+v err=%v", empty, err)
	}
	// 空日期集合：零值。
	if zero, err := q.QueryUnresolvedErrorSummary(ctx, nil); err != nil || zero.Count != 0 {
		t.Fatalf("nil dates = %+v err=%v", zero, err)
	}
}
