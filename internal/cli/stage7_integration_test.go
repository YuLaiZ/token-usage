package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/engine"
)

// stage7 本地测试桩（与 engine 包的同名桩不同包，不冲突）。
type fixedCollector struct {
	name   string
	err    error
	result collector.CollectResult
}

func (c *fixedCollector) Name() string { return c.name }
func (c *fixedCollector) SyncSources() []string {
	return []string{"test_source"}
}
func (c *fixedCollector) Collect(_ context.Context, _ collector.CollectRequest, _ *slog.Logger) (collector.CollectResult, error) {
	return c.result, c.err
}

func collectTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStage7_ErrorLifecycleIntegration(t *testing.T) {
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	date := "2026-06-23"

	// 1. 首次失败：采集报错并记录到 DB
	failedDeps := engine.NewDepsWithCollectors(
		&config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}},
		[]collector.Collector{&fixedCollector{name: "claude", err: errors.New("temporary failure")}},
		nil,
	)
	failed := engine.RunCollect(context.Background(), failedDeps, usageDB,
		collectTestLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{date}}, true, false)
	if failed.Complete() || failed.Err == nil {
		t.Fatalf("first collection must fail: %+v", failed)
	}

	// 2. errors/query 可见：DB 中有未解决错误，query 警告和 errors 命令都输出错误信息
	unresolved, err := db.GetErrors(usageDB, db.ErrorFilter{
		Dates: []string{date}, Source: "claude", Unresolved: true,
	})
	if err != nil || len(unresolved) != 1 {
		t.Fatalf("unresolved=%+v err=%v", unresolved, err)
	}
	var queryWarning bytes.Buffer
	if err := showErrorWarnings(&queryWarning, usageDB, []string{date}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queryWarning.String(), "temporary failure") {
		t.Fatalf("query warning=%q", queryWarning.String())
	}
	var errorsOutput bytes.Buffer
	if err := runErrors(usageDB, &errorsOutput, db.ErrorFilter{Unresolved: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errorsOutput.String(), "temporary failure") {
		t.Fatalf("errors output=%q", errorsOutput.String())
	}

	// 3. 手动重试成功：错误自动恢复
	successDeps := engine.NewDepsWithCollectors(
		&config.Config{Clients: map[string]config.Client{"claude": {Enabled: true}}},
		[]collector.Collector{&fixedCollector{name: "claude"}},
		nil,
	)
	if err := engine.RunRetryWithDeps(successDeps, usageDB, "claude",
		collectTestLogger(), io.Discard); err != nil {
		t.Fatal(err)
	}

	// 4. 恢复后：errors/query 清空
	unresolved, err = db.GetErrors(usageDB, db.ErrorFilter{Unresolved: true})
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("errors not resolved: %+v err=%v", unresolved, err)
	}
	queryWarning.Reset()
	if err := showErrorWarnings(&queryWarning, usageDB, []string{date}); err != nil {
		t.Fatal(err)
	}
	if queryWarning.Len() != 0 {
		t.Fatalf("stale query warning=%q", queryWarning.String())
	}
	errorsOutput.Reset()
	if err := runErrors(usageDB, &errorsOutput, db.ErrorFilter{Unresolved: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errorsOutput.String(), "暂无异常记录") {
		t.Fatalf("errors output after recovery=%q", errorsOutput.String())
	}
}
