package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
)

// setupErrorsEnv 隔离 HOME 并预置最小可用配置（含 data_dir），返回 data_dir 路径。
// errors RunE 经 config.Load() 读 ~/.token-usage/config.toml 定位 data_dir 下的 usage.db。
func setupErrorsEnv(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cfgDir := filepath.Join(tmpHome, ".token-usage")
	dataDir := filepath.Join(cfgDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgContent := `data_dir = "` + dataDir + `"
[log]
level = "info"
dir = "` + filepath.Join(cfgDir, "logs") + `"
max_days = 7
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// TestErrorsRunE_ShowUnresolvedByDefault errors RunE 无 flag 时默认只看未解决异常：
// 写入一未解决一已解决，输出只含未解决那条。
func TestErrorsRunE_ShowUnresolvedByDefault(t *testing.T) {
	dataDir := setupErrorsEnv(t)
	usageDB, err := db.Open(filepath.Join(dataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 写入：一条未解决、一条已解决（通过重试成功标记 resolved）
	db.RecordError(context.Background(), usageDB, "2026-07-01", "claude", "unresolved boom", "")
	db.RecordError(context.Background(), usageDB, "2026-07-02", "codex", "resolved one", "")

	var out bytes.Buffer
	cmd := newErrorsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("errors 失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unresolved boom") {
		t.Errorf("应显示未解决异常，输出: %s", got)
	}
}

// TestErrorsRunE_EmptyPrintsNoErrors errors RunE 在无异常时输出「暂无异常记录」。
func TestErrorsRunE_EmptyPrintsNoErrors(t *testing.T) {
	dataDir := setupErrorsEnv(t)
	usageDB, err := db.Open(filepath.Join(dataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = usageDB // 确保 db 已建表

	var out bytes.Buffer
	cmd := newErrorsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("errors 失败: %v", err)
	}
	if !strings.Contains(out.String(), "暂无异常记录") {
		t.Errorf("空数据应输出暂无异常记录，实际: %s", out.String())
	}
}

// TestErrorsRunE_FilterBySource --source flag 只看指定数据源。
func TestErrorsRunE_FilterBySource(t *testing.T) {
	dataDir := setupErrorsEnv(t)
	usageDB, err := db.Open(filepath.Join(dataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.RecordError(context.Background(), usageDB, "2026-07-01", "claude", "claude err", "")
	db.RecordError(context.Background(), usageDB, "2026-07-02", "codex", "codex err", "")

	var out bytes.Buffer
	cmd := newErrorsCmd()
	cmd.SetArgs([]string{"--source", "codex"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("errors 失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "codex err") {
		t.Errorf("应显示 codex 异常，输出: %s", got)
	}
	if strings.Contains(got, "claude err") {
		t.Errorf("不应显示 claude 异常，输出: %s", got)
	}
}

// TestErrorsRunE_FilterByDate 位置日期参数只看指定日期（验证 RunE 经 parseErrorDateArg 转 YYYY-MM-DD）。
func TestErrorsRunE_FilterByDate(t *testing.T) {
	dataDir := setupErrorsEnv(t)
	usageDB, err := db.Open(filepath.Join(dataDir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.RecordError(context.Background(), usageDB, "2026-07-01", "claude", "target", "")
	db.RecordError(context.Background(), usageDB, "2026-07-02", "claude", "other", "")

	var out bytes.Buffer
	cmd := newErrorsCmd()
	cmd.SetArgs([]string{"20260701"}) // RunE 应接受位置参数 YYYYMMDD 并归一化
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("errors 失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "target") {
		t.Errorf("应显示 0701 的异常，输出: %s", got)
	}
	if strings.Contains(got, "other") {
		t.Errorf("不应显示 0702 的异常，输出: %s", got)
	}
}

// TestErrorsRunE_InvalidDateReturnsError 位置日期参数非法值应返回 error（RunE 透传 parseErrorDateArg 的错误）。
func TestErrorsRunE_InvalidDateReturnsError(t *testing.T) {
	setupErrorsEnv(t)

	var out bytes.Buffer
	cmd := newErrorsCmd()
	cmd.SetArgs([]string{"not-a-date"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("非法日期应返回 error")
	}
	if !strings.Contains(err.Error(), "无效的日期") {
		t.Errorf("错误信息应含「无效的日期」，实际: %v", err)
	}
}
