package db

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// 监控中断事件的幂等写入、按实例结案、类型隔离与历史非 JSON 明细容错。
func TestMonitorErrorsLifecycle(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()

	detail := MonitorErrorDetail{
		InstanceID: "inst-a",
		Kind:       "failed",
		OccurredAt: "2026-11-11T15:35:00+08:00",
		Source:     "claude",
	}
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatalf("RecordMonitorError: %v", err)
	}
	// 同实例重复记录不新增行（幂等）。
	if err := RecordMonitorError(ctx, d, "2026-11-12", "other", "重复摘要", detail); err != nil {
		t.Fatalf("RecordMonitorError 幂等写入: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同实例监控事件行数 = %d, want 1", n)
	}

	// 不同实例不合并。
	detail2 := detail
	detail2.InstanceID = "inst-b"
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "另一次中断", detail2); err != nil {
		t.Fatal(err)
	}
	unresolved, err := GetUnresolvedMonitorErrors(ctx, d)
	if err != nil || len(unresolved) != 2 {
		t.Fatalf("未结案监控事件 = %d (%v), want 2", len(unresolved), err)
	}

	// 记录存在性（任意 resolved 状态）：控制端覆盖观察文件证据前的判定依据。
	if recorded, err := HasMonitorInstanceRecord(ctx, d, "inst-a"); err != nil || !recorded {
		t.Fatalf("inst-a 应有监控中断记录: recorded=%v err=%v", recorded, err)
	}
	if recorded, err := HasMonitorInstanceRecord(ctx, d, "inst-b"); err != nil || !recorded {
		t.Fatalf("未结案的 inst-b 同样应有记录: recorded=%v err=%v", recorded, err)
	}
	if recorded, err := HasMonitorInstanceRecord(ctx, d, "inst-missing"); err != nil || recorded {
		t.Fatalf("未入库实例不应报告记录: recorded=%v err=%v", recorded, err)
	}
	if recorded, err := HasMonitorInstanceRecord(ctx, d, ""); err != nil || recorded {
		t.Fatalf("空实例键不应报告记录: recorded=%v err=%v", recorded, err)
	}

	// 按实例结案只作用于对应实例。
	if _, err := ResolveMonitorErrorsByInstance(ctx, d, "inst-a"); err != nil {
		t.Fatalf("ResolveMonitorErrorsByInstance: %v", err)
	}
	if resolved, err := IsMonitorInstanceResolved(ctx, d, "inst-a"); err != nil || !resolved {
		t.Fatalf("inst-a 应已结案: resolved=%v err=%v", resolved, err)
	}
	// 记录存在性覆盖任意 resolved 状态：已结案记录同样是"已持久化"。
	if recorded, err := HasMonitorInstanceRecord(ctx, d, "inst-a"); err != nil || !recorded {
		t.Fatalf("已结案的 inst-a 仍应有记录: recorded=%v err=%v", recorded, err)
	}
	// 未结案记录详情：返回明细与摘要；已结案/不存在的实例返回 false。
	if detail, message, found, ferr := FindUnresolvedMonitorInstance(ctx, d, "inst-b"); ferr != nil || !found || detail.InstanceID != "inst-b" || detail.Source != "claude" || message == "" {
		t.Fatalf("应返回 inst-b 的未结案详情: detail=%+v message=%q found=%v err=%v", detail, message, found, ferr)
	}
	if _, _, found, ferr := FindUnresolvedMonitorInstance(ctx, d, "inst-a"); ferr != nil || found {
		t.Fatalf("已结案实例不应返回未结案详情: found=%v err=%v", found, ferr)
	}
	if _, _, found, ferr := FindUnresolvedMonitorInstance(ctx, d, "inst-missing"); ferr != nil || found {
		t.Fatalf("不存在实例不应返回未结案详情: found=%v err=%v", found, ferr)
	}
	if resolved, err := IsMonitorInstanceResolved(ctx, d, "inst-b"); err != nil || resolved {
		t.Fatalf("inst-b 不应被结案: resolved=%v err=%v", resolved, err)
	}

	// 历史非 JSON 明细不参与匹配、不影响写入或结案。
	if _, err := d.ExecContext(ctx, `INSERT INTO collection_errors (date, source, error_type, message, detail)
		VALUES ('2026-11-10','claude','monitor','历史坏行','not-json')`); err != nil {
		t.Fatal(err)
	}
	if resolved, err := IsMonitorInstanceResolved(ctx, d, "inst-a"); err != nil || !resolved {
		t.Fatalf("历史坏行不得影响结案查询: resolved=%v err=%v", resolved, err)
	}
	if _, err := ResolveMonitorErrorsByInstance(ctx, d, "not-json"); err != nil {
		t.Fatalf("历史坏行不得影响结案写入: %v", err)
	}
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "第三次", MonitorErrorDetail{InstanceID: "inst-c", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00"}); err != nil {
		t.Fatalf("历史坏行不得影响幂等写入: %v", err)
	}
}

// 普通采集的按日结案与 WorkBuddy 按消息结案都不得误结监控中断（同源同日或
// 摘要命中同一模式也不行）。
func TestMonitorErrorsIsolatedFromCollectionResolve(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()

	// 同一天同一来源：一条普通失败 + 一条监控中断。
	if err := RecordError(ctx, d, "2026-11-11", "claude", "claude read / 读取数据源失败: boom", ""); err != nil {
		t.Fatal(err)
	}
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		MonitorErrorDetail{InstanceID: "inst-x", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveErrorsByDateSource(ctx, d, "2026-11-11", "claude"); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := IsMonitorInstanceResolved(ctx, d, "inst-x"); resolved {
		t.Fatal("按日成功不得结案同源的监控中断")
	}

	// WorkBuddy 摘要恰好命中监听中断的模式，也不得被按消息模式误结案。
	if err := RecordMonitorError(ctx, d, "2026-11-11", "workbuddy", "workbuddy metadata failed: watcher",
		MonitorErrorDetail{InstanceID: "inst-wb", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "workbuddy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorkBuddyErrorsByMessagePattern(ctx, d); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := IsMonitorInstanceResolved(ctx, d, "inst-wb"); resolved {
		t.Fatal("WorkBuddy 按消息结案不得误结监控中断")
	}
}

// 监控事件的主键幂等与来源归一：source 为空落 daemon。
func TestRecordMonitorErrorDefaults(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	if err := RecordMonitorError(ctx, d, "2026-11-11", "", "daemon exited",
		MonitorErrorDetail{InstanceID: "inst-d", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00"}); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := d.QueryRow(`SELECT source FROM collection_errors WHERE error_type='monitor'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "daemon" {
		t.Fatalf("source = %q, want daemon（无法定位来源时归一）", source)
	}
	// 缺 instance_id 的明细拒绝写入。
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "x", MonitorErrorDetail{Kind: "failed"}); err == nil {
		t.Fatal("缺 instance_id 必须拒绝")
	}
}

// parse 明细：非 JSON/缺字段返回错误。
func TestParseMonitorErrorDetail(t *testing.T) {
	if _, err := ParseMonitorErrorDetail("not-json"); err == nil {
		t.Fatal("非 JSON 应失败")
	}
	if _, err := ParseMonitorErrorDetail(`{"kind":"failed"}`); err == nil {
		t.Fatal("缺 instance_id 应失败")
	}
	got, err := ParseMonitorErrorDetail(`{"instance_id":"i","kind":"failed","occurred_at":"2026-11-11T15:35:00+08:00","source":"claude"}`)
	if err != nil || got.InstanceID != "i" || got.Source != "claude" {
		t.Fatalf("解析失败: %+v err=%v", got, err)
	}
}

// 只读打开：既有库可读、缺失文件返回错误且不创建文件。
func TestOpenReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("缺失库应返回错误")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("缺失库不得被只读打开创建: %v", err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordError(context.Background(), d, "2026-11-11", "claude", "claude boom", ""); err != nil {
		t.Fatal(err)
	}
	d.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if errs, err := GetErrorsContext(context.Background(), ro, ErrorFilter{Unresolved: true}); err != nil || len(errs) != 1 {
		t.Fatalf("只读查询失败: errs=%d err=%v", len(errs), err)
	}
	// 只读连接不允许写入。
	if err := RecordError(context.Background(), ro, "2026-11-11", "claude", "x", ""); err == nil {
		t.Fatal("只读连接写库必须失败")
	}
}

// ConfirmMonitorInterruption：已有未结案事件时结案；库中无记录（退出时写库
// 失败、证据仅存在于观察文件）时补记一条已结案确认行；重复调用幂等。
func TestConfirmMonitorInterruption(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	detail := MonitorErrorDetail{InstanceID: "inst-a", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}

	// 场景 1：已有未结案行 → 结案，行数不变。
	if err := RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmMonitorInterruption(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", detail); err != nil {
		t.Fatalf("ConfirmMonitorInterruption: %v", err)
	}
	if resolved, _ := IsMonitorInstanceResolved(ctx, d, "inst-a"); !resolved {
		t.Fatal("已有未结案事件应被结案")
	}

	// 场景 2：库中无该实例记录（写库失败）→ 补记 resolved=1 的确认行。
	confirmOnly := MonitorErrorDetail{InstanceID: "inst-b", Kind: "failed", OccurredAt: "2026-11-11T16:00:00+08:00", Source: "claude"}
	if err := ConfirmMonitorInterruption(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", confirmOnly); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := IsMonitorInstanceResolved(ctx, d, "inst-b"); !resolved {
		t.Fatal("写库失败场景应补记已结案确认行")
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("monitor 行数 = %d, want 2（结案不新增，确认行恰好一条）", count)
	}
	// 幂等：重复确认不新增行、不复活。
	if err := ConfirmMonitorInterruption(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败", confirmOnly); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM collection_errors WHERE error_type='monitor'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("重复确认后行数 = %d, want 2", count)
	}
}
