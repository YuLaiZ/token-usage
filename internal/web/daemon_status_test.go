package web

// GET /api/daemon-status 契约测试：全局 collection、区间摘要（只统计未解决
// error 类型）、非法区间 400、provider 缺失/失败降级、只传单端不返回区间摘要。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
)

func getDaemonStatus(t *testing.T, h http.Handler, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v\n%s", target, err, rec.Body.String())
	}
	return rec, body
}

func TestDaemonStatusGlobal(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// provider 缺失：collection 降级 unknown，range_errors=null。
	h := NewServer(querier.New(d), "test")
	rec, body := getDaemonStatus(t, h, "/api/daemon-status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	col := body["collection"].(map[string]any)
	if col["state"] != "unknown" {
		t.Fatalf("provider 缺失时 state = %v, want unknown", col["state"])
	}
	if col["catch_up"] != nil || col["interruption"] != nil {
		t.Fatalf("provider 缺失时 catch_up/interruption 应为 null: %v", col)
	}
	if body["range_errors"] != nil {
		t.Fatalf("未请求区间时 range_errors 应为 null: %v", body["range_errors"])
	}

	// 注入 provider：collection 原样输出，且与 CLI 共用同一判定对象。
	interruption := &daemonstatus.Interruption{Kind: "failed", Summary: "Claude 目录监听失败"}
	h = NewServer(querier.New(d), "test", WithDaemonStatusProvider(func(ctx context.Context) daemonstatus.Collection {
		cu := "running"
		return daemonstatus.Collection{State: daemonstatus.StateRunning, CatchUp: &cu, Interruption: interruption}
	}))
	_, body = getDaemonStatus(t, h, "/api/daemon-status")
	col = body["collection"].(map[string]any)
	if col["state"] != "running" || col["catch_up"] != "running" {
		t.Fatalf("collection = %v", col)
	}
	it := col["interruption"].(map[string]any)
	if it["kind"] != "failed" || it["summary"] != "Claude 目录监听失败" {
		t.Fatalf("interruption = %v", it)
	}
	if it["occurred_at"] != nil || it["source"] != nil {
		t.Fatalf("不可得字段应为 null: %v", it)
	}
}

func TestDaemonStatusRangeErrors(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordError(ctx, d, "2026-11-11", "claude", "claude boom", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordError(ctx, d, "2026-11-11", "codex", "codex boom", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveErrorsByDateSource(ctx, d, "2026-11-11", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-x", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00"}); err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(d), "test", WithDaemonStatusProvider(func(ctx context.Context) daemonstatus.Collection {
		return daemonstatus.Collection{State: daemonstatus.StateRunning}
	}))

	_, body := getDaemonStatus(t, h, "/api/daemon-status?from=2026-11-11&to=2026-11-11")
	re := body["range_errors"].(map[string]any)
	if re["count"] != float64(1) {
		t.Fatalf("range_errors.count = %v, want 1（只统计未解决 error，不含 monitor/已解决）", re["count"])
	}
	latest := re["latest"].(map[string]any)
	if latest["source"] != "claude" || latest["created_at"] == "" {
		t.Fatalf("range_errors.latest = %v", latest)
	}
	if latest["source_display"] != "Claude" {
		t.Fatalf("range_errors.latest.source_display = %v, want Claude（展示名由后端统一计算）", latest["source_display"])
	}
	if summary, _ := latest["summary"].(string); !strings.Contains(summary, "采集失败") {
		t.Fatalf("latest.summary 应为安全简述: %q", summary)
	}

	// 空区间：count=0、latest=null（不是整体 null）。
	_, body = getDaemonStatus(t, h, "/api/daemon-status?from=2026-01-01&to=2026-01-01")
	re = body["range_errors"].(map[string]any)
	if re["count"] != float64(0) || re["latest"] != nil {
		t.Fatalf("空区间 range_errors = %v", re)
	}

	// 单端参数：不请求区间摘要（range_errors=null）。
	_, body = getDaemonStatus(t, h, "/api/daemon-status?from=2026-11-11")
	if body["range_errors"] != nil {
		t.Fatalf("只传 from 时 range_errors 应为 null: %v", body["range_errors"])
	}

	// 非法区间：按用量页同规则 400。
	rec, _ := getDaemonStatus(t, h, "/api/daemon-status?from=bad&to=2026-11-11")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法区间 status = %d, want 400", rec.Code)
	}
	rec, _ = getDaemonStatus(t, h, "/api/daemon-status?from=2026-11-11&to=2026-01-01")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("倒置区间 status = %d, want 400", rec.Code)
	}
}

// 区间记录不可读（库已关闭）：range_errors=null，collection 照常返回，不
// 影响成功状态码；不可解释为零错误。
func TestDaemonStatusRangeErrorsUnreadable(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	h := NewServer(querier.New(d), "test", WithDaemonStatusProvider(func(ctx context.Context) daemonstatus.Collection {
		return daemonstatus.Collection{State: daemonstatus.StateStopped}
	}))
	d.Close()
	rec, body := getDaemonStatus(t, h, "/api/daemon-status?from=2026-11-11&to=2026-11-11")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if body["range_errors"] != nil {
		t.Fatalf("记录不可读时 range_errors 应为 null: %v", body["range_errors"])
	}
	if body["collection"].(map[string]any)["state"] != "stopped" {
		t.Fatalf("collection 不受区间查询失败影响: %v", body["collection"])
	}
}
