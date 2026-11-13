package cli

// daemon status 与 errors 的采集异常可见性测试：退出证据（observation +
// 只读库）→ 表格/JSON 的中断展示；errors 表格的类型与发生时刻分流提示。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

func seedExitEvidence(t *testing.T, dataDir, kind string) {
	t.Helper()
	obs := runmeta.Observation{Version: runmeta.ObservationVersion}
	now := time.Now()
	if kind != "" {
		obs.LastExit = &runmeta.ExitRecord{
			InstanceID: "inst-prev",
			Kind:       kind,
			OccurredAt: now.Add(-time.Minute).Format(time.RFC3339),
			DetectedAt: now.Add(-time.Minute).Format(time.RFC3339),
			Source:     "claude",
			Summary:    "Claude 目录监听失败",
		}
	}
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dataDir), obs); err != nil {
		t.Fatal(err)
	}
}

// runStatusTable 在隔离 home 下真实执行 daemon status（判活与阶段都走只读
// 三态探测 + PID/runtime-state 文件，不再注入 control stub）。
func runStatusTable(t *testing.T, dataDir string) string {
	t.Helper()
	setupHomeConfig(t, `data_dir = "`+dataDir+`"
[daemon]
poll_interval = 30
`)
	cmd := newDaemonStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("status 不应报错: %v", err)
	}
	return out.String()
}

// 已记录的监控致命退出：表格展示中断时间与原因；JSON 的 collection 与
// last_exit 与 Web 同源（state=interrupted、kind=failed、来源 claude）。
func TestDaemonStatus_InterruptedEvidence(t *testing.T) {
	dataDir := t.TempDir()
	seedExitEvidence(t, dataDir, runmeta.ExitKindFailed)
	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "采集已中断") || !strings.Contains(out, "Claude 目录监听失败") {
		t.Fatalf("表格应展示中断与原因: %q", out)
	}
	if !strings.Contains(out, "daemon status") && !strings.Contains(out, "errors") {
		t.Fatalf("表格应给出定位入口: %q", out)
	}

	// JSON：collection 与 last_exit 分开输出。
	cmd := newDaemonStatusCmd()
	cmd.Flags().Set("format", "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Running    bool `json:"running"`
		Collection struct {
			State        string  `json:"state"`
			CatchUp      *string `json:"catch_up"`
			Interruption *struct {
				Kind          string  `json:"kind"`
				Source        *string `json:"source"`
				SourceDisplay *string `json:"source_display"`
				Summary       string  `json:"summary"`
			} `json:"interruption"`
		} `json:"collection"`
		LastExit *struct {
			Kind     string `json:"kind"`
			Resolved bool   `json:"resolved"`
		} `json:"last_exit"`
	}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("decode json: %v\n%s", err, buf.String())
	}
	if report.Collection.State != "interrupted" || report.Collection.Interruption == nil {
		t.Fatalf("collection = %+v", report.Collection)
	}
	if report.Collection.Interruption.Kind != "failed" || report.Collection.Interruption.Source == nil || *report.Collection.Interruption.Source != "claude" {
		t.Fatalf("interruption = %+v", report.Collection.Interruption)
	}
	if report.Collection.Interruption.SourceDisplay == nil || *report.Collection.Interruption.SourceDisplay != "Claude" {
		t.Fatalf("source_display = %v, want Claude", report.Collection.Interruption.SourceDisplay)
	}
	if report.LastExit == nil || report.LastExit.Kind != "failed" || report.LastExit.Resolved {
		t.Fatalf("last_exit = %+v", report.LastExit)
	}
}

// 上一代 active 残留、无退出记录：原因未知的中断（不虚构时间与来源）。
func TestDaemonStatus_ActiveLeftoverInterrupted(t *testing.T) {
	dataDir := t.TempDir()
	obs := runmeta.Observation{
		Version: runmeta.ObservationVersion,
		Active:  &runmeta.ActiveRun{InstanceID: "inst-dead", StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)},
	}
	if err := runmeta.WriteObservation(runmeta.ObservationPath(dataDir), obs); err != nil {
		t.Fatal(err)
	}
	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "采集已中断") || !strings.Contains(out, "退出原因未知") {
		t.Fatalf("残留 active 应显示原因未知中断: %q", out)
	}
}

// 观测文件损坏：只显示未运行，不构造故障。
func TestDaemonStatus_CorruptObservation(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(runmeta.ObservationPath(dataDir), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "未运行") {
		t.Fatalf("应显示未运行: %q", out)
	}
	if strings.Contains(out, "采集已中断") {
		t.Fatalf("损坏证据不得构造故障记录: %q", out)
	}
}

// 运行中仍有上一代未结案中断：提示"尚未确认补齐"，且 JSON 的
// collection.catch_up 与旧 startup_phase.catch_up 同源相等。判活使用只读
// 三态探测，测试需真实持有 daemon lock。
func TestDaemonStatus_RunningKeepsInterruption(t *testing.T) {
	dataDir := t.TempDir()
	// 未结案的监控中断事件（DB 证据优先）。
	usageDB, err := db.Open(filepath.Join(dataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(context.Background(), usageDB, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-prev", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	usageDB.Close()
	seedExitEvidence(t, dataDir, runmeta.ExitKindFailed)

	// 判活与阶段都从只读三态探测 + PID/runtime-state 匹配读取（与 Web 同源）：
	// 持有真实锁并写入匹配的 PID/阶段文件模拟运行中实例。
	fl, ok := daemon.AcquireLock(filepath.Join(dataDir, "token-usage.lock"))
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	defer daemon.ReleaseLock(fl)
	if err := runmeta.WritePIDFile(runmeta.PIDPath(dataDir), os.Getpid(), "inst-status"); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteRuntimeState(runmeta.StatePath(dataDir), runmeta.RuntimeState{
		PID: os.Getpid(), InstanceID: "inst-status", MonitorReady: true, CatchUp: "running",
	}); err != nil {
		t.Fatal(err)
	}

	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "上次采集中断尚未确认补齐") {
		t.Fatalf("运行中应提示上一代未确认补齐的中断: %q", out)
	}

	cmd := newDaemonStatusCmd()
	cmd.Flags().Set("format", "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var report struct {
		StartupPhase *struct {
			CatchUp string `json:"catch_up"`
		} `json:"startup_phase"`
		Collection struct {
			State   string  `json:"state"`
			CatchUp *string `json:"catch_up"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Collection.State != "running" {
		t.Fatalf("collection.state = %q", report.Collection.State)
	}
	if report.StartupPhase == nil || report.Collection.CatchUp == nil || *report.Collection.CatchUp != report.StartupPhase.CatchUp {
		t.Fatalf("collection.catch_up 必须与 startup_phase.catch_up 同源: %+v", report)
	}
}

// errors 表格：类型列、发生时刻列与分类提示；监控中断不引导重试。
// 时刻断言按本地时区动态构造，跨时区机器不敏感。
func TestErrors_TableTypeAndOccurredAt(t *testing.T) {
	occurred := time.Date(2026, 11, 11, 15, 35, 0, 0, time.Local)
	usageDB := seedMonitorAndCollectionErrors(t, occurred)
	var out bytes.Buffer
	if err := runErrors(usageDB, &out, db.ErrorFilter{Unresolved: true}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "监控") || !strings.Contains(text, "采集失败") {
		t.Fatalf("表格应展示类型列: %q", text)
	}
	if !strings.Contains(text, "发生时刻") {
		t.Fatalf("表格应包含发生时刻列: %q", text)
	}
	if !strings.Contains(text, "不要重试该记录") {
		t.Fatalf("监控中断应提示不要重试: %q", text)
	}
	if !strings.Contains(text, "collect retry") {
		t.Fatalf("普通失败应保留重试提示: %q", text)
	}
	// 监控行优先展示实际观测的 occurred_at（本地可读格式，按本地时区构造期望值）。
	wantTime := occurred.Format("2006-01-02 15:04")[5:]
	if !strings.Contains(text, wantTime) {
		t.Fatalf("监控中断的发生时刻应取 occurred_at（%s）: %q", wantTime, text)
	}
}

// seedMonitorAndCollectionErrors 建库并写入一条普通失败与一条监控中断。
func seedMonitorAndCollectionErrors(t *testing.T, occurred time.Time) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.RecordError(ctx, d, "2026-11-11", "claude", "claude read / 读取数据源失败: boom", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-x", Kind: "failed", OccurredAt: occurred.Format(time.RFC3339), Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	return d
}

// query 的采集异常警告按类型分流：普通失败引导 collect retry，监控中断引导
// daemon status 与日志（同一段输出里两个分组，顺序固定）。
func TestShowErrorWarnings_SeparatesMonitor(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := db.RecordError(ctx, d, "2026-11-11", "claude", "claude boom", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMonitorError(ctx, d, "2026-11-11", "claude", "Claude 目录监听失败",
		db.MonitorErrorDetail{InstanceID: "inst-warn", Kind: "failed", OccurredAt: "2026-11-11T15:35:00+08:00", Source: "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := showErrorWarnings(&buf, d, []string{"2026-11-11"}); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !strings.Contains(text, "采集异常（1 条）") || !strings.Contains(text, "监控中断（1 条）") {
		t.Fatalf("警告应分两组: %q", text)
	}
	collectionIdx := strings.Index(text, "采集异常（1 条）")
	monitorIdx := strings.Index(text, "监控中断（1 条）")
	if collectionIdx < 0 || monitorIdx < 0 || collectionIdx > monitorIdx {
		t.Fatalf("分组顺序应为普通失败在前: %q", text)
	}
	if !strings.Contains(text, "collect retry") {
		t.Fatalf("普通失败组应保留重试引导: %q", text)
	}
	if !strings.Contains(text, "daemon status") {
		t.Fatalf("监控中断组应引导 daemon status: %q", text)
	}
}

// daemon status 必须真正只读：空数据目录下不得创建 token-usage.lock（旧实现
// 经 Inspect 会创建锁文件，且与后续独立探测叠加可能拼出矛盾快照）。
func TestDaemonStatus_DoesNotCreateLockFile(t *testing.T) {
	dataDir := t.TempDir()
	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "未运行") {
		t.Fatalf("空数据目录应显示未运行: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "token-usage.lock")); !os.IsNotExist(err) {
		t.Fatalf("status 不得创建锁文件: %v", err)
	}
}

// daemon status 必须原样保留 runtime-state 的补采失败数：JSON 的
// startup_phase.catch_up_failures 与表格的"补采部分失败（N）"此前因只读
// 快照漏填 CatchUpFailures 而恒为 0。
func TestDaemonStatus_PreservesCatchUpFailures(t *testing.T) {
	dataDir := t.TempDir()
	fl, ok := daemon.AcquireLock(filepath.Join(dataDir, "token-usage.lock"))
	if !ok {
		t.Fatal("AcquireLock 失败")
	}
	defer daemon.ReleaseLock(fl)
	if err := runmeta.WritePIDFile(runmeta.PIDPath(dataDir), os.Getpid(), "inst-fail-count"); err != nil {
		t.Fatal(err)
	}
	if err := runmeta.WriteRuntimeState(runmeta.StatePath(dataDir), runmeta.RuntimeState{
		PID: os.Getpid(), InstanceID: "inst-fail-count", MonitorReady: true, CatchUp: "failed", CatchUpFailures: 3,
	}); err != nil {
		t.Fatal(err)
	}

	out := runStatusTable(t, dataDir)
	if !strings.Contains(out, "补采部分失败") || !strings.Contains(out, "3") {
		t.Fatalf("表格应显示补采失败数 3: %q", out)
	}

	cmd := newDaemonStatusCmd()
	cmd.Flags().Set("format", "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var report struct {
		StartupPhase *struct {
			CatchUp         string `json:"catch_up"`
			CatchUpFailures int    `json:"catch_up_failures"`
		} `json:"startup_phase"`
		Collection struct {
			CatchUp *string `json:"catch_up"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.StartupPhase == nil || report.StartupPhase.CatchUp != "failed" || report.StartupPhase.CatchUpFailures != 3 {
		t.Fatalf("JSON startup_phase 应保留失败数 3: %+v", report.StartupPhase)
	}
	if report.Collection.CatchUp == nil || *report.Collection.CatchUp != "failed" {
		t.Fatalf("collection.catch_up = %v", report.Collection.CatchUp)
	}
}
