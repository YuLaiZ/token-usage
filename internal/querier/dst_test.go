package querier

// dst_test.go 覆盖夏令时转换点的 hour/weekday 分桶与 day×hour 聚合。
// time.Local 在进程启动时初始化，测试内无法通过 t.Setenv 改变；且 Windows
// 从系统 API 读取时区并把 Location 命名为 "Local"，不响应 TZ 环境变量。
// 因此父测试以隔离子进程重新执行自身，子进程加载纽约时区后显式赋值
// time.Local（不修改系统时区），再断言春季跳变（2026-03-08 02:00 不存在）
// 与秋季回拨（2026-11-01 01:00-02:00 重复）。

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

func TestDSTTransitions(t *testing.T) {
	if os.Getenv("TU_DST_CHILD") == "1" {
		runDSTAssertions(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestDSTTransitions", "-test.v")
	cmd.Env = append(os.Environ(), "TU_DST_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DST child failed: %v\n%s", err, out)
	}
}

func runDSTAssertions(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 显式赋值而不是依赖 TZ：Windows 上 time.Local 来自系统 API（名称为
	// "Local"），TZ 不会改变它；赋值只影响本测试进程，不修改系统时区。
	time.Local = loc
	if time.Local.String() != "America/New_York" {
		t.Fatalf("time.Local = %q, want America/New_York", time.Local)
	}

	// 春季跳变：01:59 EST → 03:00 EDT，02:00-02:59 不存在。
	springBefore := time.Date(2026, 3, 8, 1, 59, 0, 0, loc)
	springAfter := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
	springGap := time.Date(2026, 3, 8, 2, 30, 0, 0, loc)
	if got := bucketKeyOf("hour", springBefore.UnixMilli()); got != "01" {
		t.Fatalf("spring 01:59 hour = %q, want 01", got)
	}
	if got := bucketKeyOf("hour", springAfter.UnixMilli()); got != "03" {
		t.Fatalf("spring 03:00 hour = %q, want 03", got)
	}
	if got := bucketKeyOf("hour", springGap.UnixMilli()); got != "01" {
		t.Fatalf("spring gap 02:30 normalized hour = %q, want 01（Go 把不存在的本地时间回退到 gap 前）", got)
	}

	// 秋季回拨：01:30 EDT 与 01:30 EST 是同一本地 hour，但 UTC 相差一小时。
	fallFirst := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	fallSecond := fallFirst.Add(time.Hour)
	if fallFirst.UTC().Hour() != 5 || fallSecond.UTC().Hour() != 6 {
		t.Fatalf("fall UTC hours = %d/%d, want 5/6", fallFirst.UTC().Hour(), fallSecond.UTC().Hour())
	}
	if got := bucketKeyOf("hour", fallFirst.UnixMilli()); got != "01" {
		t.Fatalf("fall first hour = %q, want 01", got)
	}
	if got := bucketKeyOf("hour", fallSecond.UnixMilli()); got != "01" {
		t.Fatalf("fall second hour = %q, want 01", got)
	}

	// 2026-03-08 与 2026-11-01 都是周日，ISO 周序 0..6（周一=0）下为 6。
	if got := bucketKeyOf("weekday", springAfter.UnixMilli()); got != "6" {
		t.Fatalf("spring weekday = %q, want 6", got)
	}
	if got := bucketKeyOf("weekday", fallFirst.UnixMilli()); got != "6" {
		t.Fatalf("fall weekday = %q, want 6", got)
	}

	// day×hour 聚合：春季 01 与 03 各有值、02 为空；秋季两个 01:30 累加到同一格。
	testDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	ctx := context.Background()
	dates := []string{"2026-03-08", "2026-11-01"}
	msgs := []model.Message{
		{ID: "s1", SessionID: "x", Client: "c", Date: "2026-03-08", TS: springBefore.UnixMilli(), TotalTokens: 10},
		{ID: "s2", SessionID: "x", Client: "c", Date: "2026-03-08", TS: springAfter.UnixMilli(), TotalTokens: 20},
		{ID: "f1", SessionID: "y", Client: "c", Date: "2026-11-01", TS: fallFirst.UnixMilli(), TotalTokens: 30},
		{ID: "f2", SessionID: "y", Client: "c", Date: "2026-11-01", TS: fallSecond.UnixMilli(), TotalTokens: 40},
	}
	if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
		t.Fatalf("UpsertMessages failed: %v", err)
	}

	q := New(testDB)
	matrix, err := q.DayHourMatrix(ctx, dates)
	if err != nil {
		t.Fatalf("DayHourMatrix failed: %v", err)
	}
	assertDSTDayHourValues(t, "DayHourMatrix", matrix)

	// 同一 fixture 再走 ScanRange 的 AggDayHour：dashboard 已切到该路径，
	// 旧矩阵与扫描路径必须给出相同结果。
	snap, err := q.ScanRange(ctx, dates, []AggSpec{{Kind: AggDayHour}})
	if err != nil {
		t.Fatalf("ScanRange dayhour failed: %v", err)
	}
	scanMatrix, ok := snap.DayHour()
	if !ok {
		t.Fatal("ScanRange DayHour() not available")
	}
	assertDSTDayHourValues(t, "ScanRange", scanMatrix)
	if !reflect.DeepEqual(scanMatrix.Values, matrix.Values) {
		t.Fatalf("ScanRange dayhour mismatch:\n scan %v\n old  %v", scanMatrix.Values, matrix.Values)
	}
	if !reflect.DeepEqual(scanMatrix.Hours, matrix.Hours) || !reflect.DeepEqual(scanMatrix.Days, matrix.Days) {
		t.Fatalf("ScanRange dayhour axes mismatch: scan=%v/%v old=%v/%v",
			scanMatrix.Days, scanMatrix.Hours, matrix.Days, matrix.Hours)
	}
}

func assertDSTDayHourValues(t *testing.T, label string, matrix *DayHourMatrix) {
	t.Helper()
	if got := matrix.Values[0][1]; got != 10 {
		t.Fatalf("%s spring hour 01 = %d, want 10", label, got)
	}
	if got := matrix.Values[0][2]; got != 0 {
		t.Fatalf("%s spring hour 02 = %d, want 0（该本地小时不存在）", label, got)
	}
	if got := matrix.Values[0][3]; got != 20 {
		t.Fatalf("%s spring hour 03 = %d, want 20", label, got)
	}
	if got := matrix.Values[1][1]; got != 70 {
		t.Fatalf("%s fall hour 01 = %d, want 70（两个 01:30 累加）", label, got)
	}
}
